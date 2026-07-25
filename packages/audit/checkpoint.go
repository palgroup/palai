package audit

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// CheckpointVersion is the on-disk envelope version. Bumping it invalidates old checkpoints loudly
// (Compare raises AlertMalformed) rather than comparing across two different meanings of "head".
const CheckpointVersion = 1

// Checkpoint is the signed anchor kept OUTSIDE the database. Sessions are sorted by session_id so the
// file's bytes — and therefore its signature — depend only on the journal, not on row arrival order.
type Checkpoint struct {
	Version     int             `json:"version"`
	Algorithm   string          `json:"algorithm"`
	GeneratedAt string          `json:"generated_at"`
	Head        string          `json:"head"`
	Count       int             `json:"count"`
	Sessions    []SessionAnchor `json:"sessions"`
}

// Checkpoint file names. The envelope is the E14 T5 shape: the signed file, its sha256 manifest, a
// detached openssl signature, and the public key written BESIDE them for convenience — the verifier
// refuses to trust that copy (see ResolvePubkey).
const (
	CheckpointFile = "audit-checkpoint.json"
	ManifestFile   = CheckpointFile + ".sha256"
	SignatureFile  = CheckpointFile + ".sig"
	PubkeyFile     = "palai-audit-checkpoint.pub"
)

// AllowBundledPubkeyEnv opts in to trusting a public key that sits beside the checkpoint. It exists
// only for a same-session local proof; see ResolvePubkey.
const AllowBundledPubkeyEnv = "PALAI_AUDIT_ALLOW_BUNDLED_PUBKEY"

// NewCheckpoint summarizes rows into a checkpoint. now is injected so a test can pin the timestamp.
func NewCheckpoint(rows []Row, now time.Time) Checkpoint {
	head, sessions, count := Summarize(rows)
	if sessions == nil {
		sessions = []SessionAnchor{}
	}
	return Checkpoint{
		Version:     CheckpointVersion,
		Algorithm:   Algorithm,
		GeneratedAt: now.UTC().Format(time.RFC3339Nano),
		Head:        head,
		Count:       count,
		Sessions:    sessions,
	}
}

// WriteSigned writes the checkpoint envelope into dir and signs it with signingKey.
//
// SIGNING TOOL: `openssl dgst -sha256 -sign` over an ECDSA P-256 key — the E14 T5 signer's command
// set VERBATIM (scripts/package/runner/build.sh). This adds no second signing tool and no second
// keyring: it is the same binary, the same command, and the same key format the release signer uses,
// and scripts/release/runner-verify.sh verifies these bytes unchanged.
func WriteSigned(dir string, cp Checkpoint, signingKey string) error {
	if signingKey == "" {
		return errors.New("audit: a signing key is required to cut a checkpoint (an unsigned anchor is not an anchor)")
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	body, err := json.MarshalIndent(cp, "", "  ")
	if err != nil {
		return err
	}
	body = append(body, '\n')
	path := filepath.Join(dir, CheckpointFile)
	if err := os.WriteFile(path, body, 0o644); err != nil {
		return err
	}
	sum := sha256.Sum256(body)
	manifest := fmt.Sprintf("%s  %s\n", hex.EncodeToString(sum[:]), CheckpointFile)
	if err := os.WriteFile(filepath.Join(dir, ManifestFile), []byte(manifest), 0o644); err != nil {
		return err
	}
	if out, err := exec.Command("openssl", "dgst", "-sha256", "-sign", signingKey,
		"-out", filepath.Join(dir, SignatureFile), path).CombinedOutput(); err != nil {
		return fmt.Errorf("audit: openssl sign failed: %v: %s", err, strings.TrimSpace(string(out)))
	}
	if out, err := exec.Command("openssl", "pkey", "-in", signingKey, "-pubout",
		"-out", filepath.Join(dir, PubkeyFile)).CombinedOutput(); err != nil {
		return fmt.Errorf("audit: openssl pubout failed: %v: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

// ResolvePubkey applies the fail-closed trust posture E18 T3 hardened
// (scripts/release/provenance-verify.sh): the public key must arrive OUT OF BAND. A key that resolves
// INSIDE the checkpoint's own directory is refused, because whoever rewrote the journal and re-cut the
// checkpoint could re-cut the key beside it too — then the signature is just a second checksum they
// control end to end. PALAI_AUDIT_ALLOW_BUNDLED_PUBKEY=1 says out loud that this is a same-session
// local proof.
func ResolvePubkey(checkpointPath, pubkey string) (string, string, error) {
	if pubkey == "" {
		return "", "", errors.New("audit: a trusted public key is REQUIRED (--pubkey or PALAI_AUDIT_PUBKEY); obtain it out of band, never from beside the checkpoint")
	}
	abs, err := filepath.Abs(pubkey)
	if err != nil {
		return "", "", err
	}
	dir, err := filepath.Abs(filepath.Dir(checkpointPath))
	if err != nil {
		return "", "", err
	}
	if filepath.Dir(abs) == dir {
		if os.Getenv(AllowBundledPubkeyEnv) != "1" {
			return "", "", fmt.Errorf("audit: REFUSING — the public key %s sits beside the checkpoint it would verify, so it is not a trust anchor; pass an out-of-band key or set %s=1 for a same-session local proof", abs, AllowBundledPubkeyEnv)
		}
		return abs, "WARNING: the public key came from beside the checkpoint (" + AllowBundledPubkeyEnv + "=1) — the signature proves only self-consistency; same-session local proof only", nil
	}
	return abs, "", nil
}

// LoadSigned verifies the checkpoint envelope and returns the parsed checkpoint. Signature FIRST:
// nothing in the file is parsed as truth before the bytes are shown to be the signed ones. Any
// failure returns a typed AlertSignature so the caller reports it like every other integrity alert.
func LoadSigned(checkpointPath, pubkey string) (Checkpoint, string, *Alert) {
	resolved, warn, err := ResolvePubkey(checkpointPath, pubkey)
	if err != nil {
		return Checkpoint{}, "", &Alert{Kind: AlertSignature, Detail: err.Error()}
	}
	body, err := os.ReadFile(checkpointPath)
	if err != nil {
		return Checkpoint{}, "", &Alert{Kind: AlertSignature, Detail: "read checkpoint: " + err.Error()}
	}
	// The sha256 manifest is a corruption check, not a security control (an attacker rewrites both);
	// the signature below is the security control. Checking it first gives the honest error for a
	// truncated file rather than an opaque signature failure.
	manifest, err := os.ReadFile(checkpointPath + ".sha256")
	if err == nil {
		sum := sha256.Sum256(body)
		if !strings.HasPrefix(strings.TrimSpace(string(manifest)), hex.EncodeToString(sum[:])) {
			return Checkpoint{}, "", &Alert{Kind: AlertSignature, Detail: "the checkpoint bytes do not match their sha256 manifest"}
		}
	}
	sig := checkpointPath + ".sig"
	out, verr := exec.Command("openssl", "dgst", "-sha256", "-verify", resolved,
		"-signature", sig, checkpointPath).CombinedOutput()
	if verr != nil {
		return Checkpoint{}, warn, &Alert{
			Kind: AlertSignature,
			Detail: fmt.Sprintf("the checkpoint signature does not verify against %s: %s",
				resolved, strings.TrimSpace(string(out))),
		}
	}
	var cp Checkpoint
	if err := json.Unmarshal(body, &cp); err != nil {
		return Checkpoint{}, warn, &Alert{Kind: AlertMalformed, Detail: "parse checkpoint: " + err.Error()}
	}
	return cp, warn, nil
}
