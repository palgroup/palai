package stack

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/palgroup/palai/packages/audit"
)

// audit.go is the SEC-103 operator surface (E18 T7): `palai audit checkpoint` cuts a signed anchor
// over the `events` journal and `palai audit verify` RECOMPUTES the chain from the rows and measures
// it against that anchor.
//
// The anchor is a FILE, deliberately outside the database (plan §1, and why this task ships no
// migration): whoever can rewrite a payload row can rewrite an anchor row in the same transaction, so
// an in-database anchor proves nothing. The signature is the E14 T5 openssl P-256 signer's command
// set, and `verify` needs the public key OUT OF BAND — the fail-closed posture E18 T3 hardened.
//
// An alert is never just a log line: `verify` prints a typed report and exits non-zero.

// PgURLForAudit is the connection the audit commands read the journal over. They connect as the
// stack's Postgres superuser like `doctor` does, so the chain covers the WHOLE installation's journal
// rather than one tenant's row-level-security view — an integrity check that could not see a row
// could not notice its deletion.
// PALAI_AUDIT_POSTGRES_URL points the audit commands at a journal that is not this host's .palai
// stack — a restored backup being checked before it is trusted, or a throwaway database in the
// component proof. Without it the commands read the local stack exactly like `doctor`.
func pgURLForAudit() (string, error) {
	if url := os.Getenv("PALAI_AUDIT_POSTGRES_URL"); url != "" {
		return url, nil
	}
	cfg, p, err := loadConfig()
	if err != nil {
		return "", err
	}
	return pgURLFor(cfg, p), nil
}

// AuditCheckpoint reads the journal, folds it into a checkpoint, and writes the signed envelope into
// dir. signingKey is the release signing key (PALAI_AUDIT_SIGNING_KEY when the flag is empty); there
// is no unsigned mode, because an unsigned anchor anyone can regenerate is not an anchor.
func AuditCheckpoint(dir, signingKey string) error {
	if signingKey == "" {
		signingKey = os.Getenv("PALAI_AUDIT_SIGNING_KEY")
	}
	if signingKey == "" {
		return errors.New("audit checkpoint: --signing-key (or PALAI_AUDIT_SIGNING_KEY) is required — the release signing key that anchors the journal")
	}
	url, err := pgURLForAudit()
	if err != nil {
		return err
	}
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, url)
	if err != nil {
		return fmt.Errorf("audit checkpoint: connect Postgres: %w", err)
	}
	defer conn.Close(ctx)

	rows, err := audit.ReadRows(ctx, conn)
	if err != nil {
		return err
	}
	cp := audit.NewCheckpoint(rows, time.Now().UTC())
	if err := audit.WriteSigned(dir, cp, signingKey); err != nil {
		return err
	}
	fmt.Printf("audit checkpoint: anchored %d event(s) across %d session(s) at %s\n", cp.Count, len(cp.Sessions), cp.Head)
	fmt.Printf("audit checkpoint: wrote %s (+ .sha256, .sig) and the public key %s\n",
		filepath.Join(dir, audit.CheckpointFile), filepath.Join(dir, audit.PubkeyFile))
	fmt.Printf("audit checkpoint: KEEP THIS OUTSIDE THE DATABASE and deliver the public key OUT OF BAND — verifying against a key stored beside the checkpoint proves only self-consistency.\n")
	return nil
}

// AuditVerify recomputes the chain from the journal rows and compares it to the signed checkpoint.
// It returns a non-nil error — and therefore a non-zero exit — on any alert.
func AuditVerify(checkpointPath, pubkey string, jsonOut bool) error {
	if checkpointPath == "" {
		return errors.New("audit verify: --checkpoint <path to " + audit.CheckpointFile + "> is required")
	}
	if pubkey == "" {
		pubkey = os.Getenv("PALAI_AUDIT_PUBKEY")
	}

	report := audit.Report{CheckpointPath: checkpointPath, Algorithm: audit.Algorithm}

	// The anchor is authenticated BEFORE a single row is read: an unsigned or wrongly-signed
	// checkpoint has nothing to say about the journal, so there is nothing to compare it against.
	cp, warn, alert := audit.LoadSigned(checkpointPath, pubkey)
	if alert != nil {
		report.Alerts = append(report.Alerts, *alert)
		return emitAuditReport(report, warn, jsonOut)
	}
	report.Signature = "verified (openssl ECDSA P-256, E14 T5 signer; key supplied out of band)"
	if warn != "" {
		report.Signature = "verified against a bundled key — " + warn
	}

	url, err := pgURLForAudit()
	if err != nil {
		return err
	}
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, url)
	if err != nil {
		return fmt.Errorf("audit verify: connect Postgres: %w", err)
	}
	defer conn.Close(ctx)

	rows, err := audit.ReadRows(ctx, conn)
	if err != nil {
		return err
	}
	compared := audit.Compare(cp, rows)
	compared.CheckpointPath = checkpointPath
	compared.Signature = report.Signature
	return emitAuditReport(compared, warn, jsonOut)
}

// emitAuditReport prints the typed report and returns a non-nil error when it carries any alert, so
// an integrity failure is a non-zero exit and not something an operator has to spot in a log.
func emitAuditReport(rep audit.Report, warn string, jsonOut bool) error {
	rep.OK = len(rep.Alerts) == 0
	if jsonOut {
		raw, err := json.Marshal(rep)
		if err != nil {
			return err
		}
		fmt.Println(string(raw))
	} else {
		if warn != "" {
			fmt.Fprintln(os.Stderr, "audit verify: "+warn)
		}
		fmt.Printf("audit verify: checkpoint %s\n", rep.CheckpointPath)
		fmt.Printf("audit verify: signature   %s\n", orNone(rep.Signature))
		fmt.Printf("audit verify: anchored    %d row(s) recomputed to %s\n", rep.AnchoredRows, orNone(rep.RecomputedHead))
		fmt.Printf("audit verify: checkpoint  head %s\n", orNone(rep.CheckpointHead))
		fmt.Printf("audit verify: unanchored  %d row(s) written after this checkpoint (cadence is operator policy — they are reported, not vouched for)\n", rep.UnanchoredRows)
		for _, a := range rep.Alerts {
			fmt.Printf("audit verify: ALERT [%s] %s %s\n", a.Kind, a.SessionID, a.Detail)
			if a.Want != "" {
				fmt.Printf("audit verify:            want %s\n                         got  %s\n", a.Want, a.Got)
			}
		}
	}
	if !rep.OK {
		return fmt.Errorf("audit verify: %d integrity alert(s) — the journal does not match its signed checkpoint", len(rep.Alerts))
	}
	return nil
}

func orNone(s string) string {
	if s == "" {
		return "(none)"
	}
	return s
}
