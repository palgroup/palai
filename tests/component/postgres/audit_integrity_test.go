//go:build component

package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	osexec "os/exec"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/palgroup/palai/packages/audit"
	"github.com/palgroup/palai/packages/coordinator"
	"github.com/palgroup/palai/storage"
)

// audit_integrity_test.go is SEC-103's real-journal half (E18 T7): the four arms the plan names, run
// against actual `events` rows in real PostgreSQL rather than synthetic structs.
//
//	delete a row                        -> gap    FAIL
//	flip one byte in a payload          -> tamper FAIL
//	intact journal + correct checkpoint -> GREEN
//	checkpoint signed with the WRONG key -> fail-closed
//
// Each asserts the TYPED alert, never a substring of prose (the E15 T6 lesson): a failed run and a
// passing run must not be distinguishable only by which words they happened to print.
//
// The checkpoint is written to a temp dir OUTSIDE the database, which is the whole point of the design
// (plan §1, and why this task ships no migration): these arms mutate the journal with ordinary SQL —
// exactly the capability an in-database anchor would hand the attacker as well.

// TestAuditChainCoversEveryEventColumn keeps the chain honest as the schema moves: a migration that
// adds an `events` column without adding it to the digest creates a field an attacker can edit with no
// tamper alert. This fails on that migration rather than silently in production.
func TestAuditChainCoversEveryEventColumn(t *testing.T) {
	cs := openHarness(t)
	rows, err := cs.Pool().Query(storage.WithSystemScope(context.Background()),
		`SELECT column_name FROM information_schema.columns WHERE table_name = 'events'`)
	if err != nil {
		t.Fatalf("read events columns: %v", err)
	}
	defer rows.Close()
	var actual []string
	for rows.Next() {
		var c string
		if err := rows.Scan(&c); err != nil {
			t.Fatal(err)
		}
		actual = append(actual, c)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	chained := append([]string(nil), audit.ChainedColumns...)
	sort.Strings(actual)
	sort.Strings(chained)
	if fmt.Sprint(actual) != fmt.Sprint(chained) {
		t.Fatalf("events columns %v != chained columns %v — a column outside the digest is a column an "+
			"attacker can edit without raising a tamper alert; add it to audit.ChainedColumns AND to the "+
			"AuditChainRows query, then re-cut every checkpoint", actual, chained)
	}
}

// TestAuditIntegrityFourArms drives all four arms over one seeded real journal.
func TestAuditIntegrityFourArms(t *testing.T) {
	if _, err := osexec.LookPath("openssl"); err != nil {
		t.Skip("openssl not on PATH — the checkpoint signature is the E14 T5 openssl signer")
	}
	cs := openHarness(t)
	pool := cs.Pool()
	ctx := storage.WithSystemScope(context.Background())
	tenant, sessionID, _ := seedRun(t, pool)

	// A small real journal: six events over one session, payloads distinguishable byte-for-byte.
	for seq := 1; seq <= 6; seq++ {
		exec(t, pool, `INSERT INTO events (id, organization_id, project_id, session_id, seq, type, payload)
		               VALUES ($1, $2, $3, $4, $5, $6, $7)`,
			newID("evt"), tenant.Project, sessionID, seq, "run.step.v1",
			fmt.Sprintf(`{"step": %d, "note": "payload-%d"}`, seq, seq))
	}

	// The key material lives OUTSIDE the checkpoint dir: it is the trust anchor, so a copy sitting
	// beside the thing it verifies would be worth nothing (see the bundled-key leg below).
	dir, keyDir := t.TempDir(), t.TempDir()
	signing := filepath.Join(keyDir, "release-signing.key")
	pubkey := filepath.Join(keyDir, "release-signing.pub")
	wrongKey := filepath.Join(keyDir, "attacker.key")
	wrongPub := filepath.Join(keyDir, "attacker.pub")
	mintKey(t, signing, pubkey)
	mintKey(t, wrongKey, wrongPub)

	cp := audit.NewCheckpoint(readJournal(t, ctx, pool), time.Now())
	if err := audit.WriteSigned(dir, cp, signing, false); err != nil {
		t.Fatalf("WriteSigned() error = %v", err)
	}
	cpPath := filepath.Join(dir, audit.CheckpointFile)

	// ---- ARM 3 (GREEN): intact journal + correct checkpoint, verified with the out-of-band key.
	anchor, warn, alert := audit.LoadSigned(cpPath, pubkey)
	if alert != nil {
		t.Fatalf("the freshly signed checkpoint failed to load: %+v", alert)
	}
	if warn != "" {
		t.Fatalf("an out-of-band key produced a bundled-key warning: %s", warn)
	}
	if rep := audit.Compare(anchor, readJournal(t, ctx, pool)); !rep.OK {
		t.Fatalf("an untouched journal raised alerts %+v", rep.Alerts)
	}

	// ---- ARM 4 (FAIL-CLOSED): a checkpoint signed with the WRONG key. The forged file's CONTENT is
	// byte-identical to the real one and internally consistent, so only the trust anchor can reject it.
	forged := t.TempDir()
	if err := audit.WriteSigned(forged, cp, wrongKey, false); err != nil {
		t.Fatalf("WriteSigned(wrong key) error = %v", err)
	}
	if _, _, alert := audit.LoadSigned(filepath.Join(forged, audit.CheckpointFile), pubkey); alert == nil || alert.Kind != audit.AlertSignature {
		t.Fatalf("a checkpoint signed with the WRONG key was accepted (alert=%+v)", alert)
	}
	// ... and the forged checkpoint is refused when the verifier is handed the key sitting BESIDE it —
	// there the fail-closed resolution, not the signature, is what catches it (an attacker who re-signs
	// also re-writes the neighbouring public key).
	if _, _, alert := audit.LoadSigned(filepath.Join(forged, audit.CheckpointFile), filepath.Join(forged, audit.PubkeyFile)); alert == nil || alert.Kind != audit.AlertSignature {
		t.Fatalf("a forged checkpoint verified against its OWN bundled key (alert=%+v)", alert)
	}

	// ---- ARM 2 (TAMPER): flip one byte inside a payload. Every anchored row is still present, so this
	// must be a tamper and NOT a gap.
	exec(t, pool, `UPDATE events SET payload = jsonb_set(payload, '{note}', '"payload-4x"')
	               WHERE session_id = $1 AND seq = 4`, sessionID)
	rep := audit.Compare(anchor, readJournal(t, ctx, pool))
	if rep.OK {
		t.Fatalf("a payload edit in real Postgres verified OK")
	}
	if !rep.Has(audit.AlertTamper) {
		t.Fatalf("payload edit alerts = %+v, want %q", rep.Alerts, audit.AlertTamper)
	}
	if rep.Has(audit.AlertGap) {
		t.Fatalf("payload edit also raised %q — gap and tamper are not discriminated", audit.AlertGap)
	}
	// Restore the byte: the chain must come back, which proves the alert tracked the BYTES and not
	// some incidental state the edit disturbed.
	exec(t, pool, `UPDATE events SET payload = jsonb_set(payload, '{note}', '"payload-4"')
	               WHERE session_id = $1 AND seq = 4`, sessionID)
	if rep := audit.Compare(anchor, readJournal(t, ctx, pool)); !rep.OK {
		t.Fatalf("restoring the byte did not restore the chain: %+v", rep.Alerts)
	}

	// ---- ARM 1 (GAP): delete a row outright.
	exec(t, pool, `DELETE FROM events WHERE session_id = $1 AND seq = 3`, sessionID)
	rep = audit.Compare(anchor, readJournal(t, ctx, pool))
	if rep.OK {
		t.Fatalf("a deleted journal row verified OK")
	}
	if !rep.Has(audit.AlertGap) {
		t.Fatalf("deleted-row alerts = %+v, want %q", rep.Alerts, audit.AlertGap)
	}
	named := false
	for _, a := range rep.Alerts {
		if a.Kind == audit.AlertGap && a.SessionID == sessionID {
			named = true
		}
	}
	if !named {
		t.Fatalf("the gap alert does not name the affected session: %+v", rep.Alerts)
	}
}

// readJournal re-reads the whole journal for a fresh recompute. Every verdict comes from the ROWS as
// they are right now — never from a cached summary, which is the property the whole design rests on.
func readJournal(t *testing.T, ctx context.Context, pool *pgxpool.Pool) []audit.Row {
	t.Helper()
	rows, err := audit.ReadRows(ctx, pool)
	if err != nil {
		t.Fatalf("audit.ReadRows() error = %v", err)
	}
	return rows
}

// mintKey generates an ECDSA P-256 keypair with the E14 T5 command set.
func mintKey(t *testing.T, key, pub string) {
	t.Helper()
	if out, err := osexec.Command("openssl", "genpkey", "-algorithm", "EC",
		"-pkeyopt", "ec_paramgen_curve:P-256", "-out", key).CombinedOutput(); err != nil {
		t.Fatalf("genpkey: %v: %s", err, out)
	}
	if out, err := osexec.Command("openssl", "pkey", "-in", key, "-pubout", "-out", pub).CombinedOutput(); err != nil {
		t.Fatalf("pubout: %v: %s", err, out)
	}
}

// TestAuditVerifyCommandExitsNonZeroOnTamper drives the REAL `palai audit verify` binary, because the
// requirement is not "a library returns a struct" but "an operator SEES the alert": exit code non-zero
// plus a typed report. A verifier that noticed a tamper and exited 0 would be worse than none at all.
func TestAuditVerifyCommandExitsNonZeroOnTamper(t *testing.T) {
	if _, err := osexec.LookPath("openssl"); err != nil {
		t.Skip("openssl not on PATH — the checkpoint signature is the E14 T5 openssl signer")
	}
	cs := openHarness(t)
	pool := cs.Pool()
	tenant, sessionID, _ := seedRun(t, pool)
	for seq := 1; seq <= 3; seq++ {
		exec(t, pool, `INSERT INTO events (id, organization_id, project_id, session_id, seq, type, payload)
		               VALUES ($1, $2, $3, $4, $5, $6, $7)`,
			newID("evt"), tenant.Project, sessionID, seq, "run.step.v1",
			fmt.Sprintf(`{"cli": %d}`, seq))
	}

	bin := filepath.Join(t.TempDir(), "palai")
	if out, err := osexec.Command("go", "build", "-o", bin, "github.com/palgroup/palai/cmd/cli").CombinedOutput(); err != nil {
		t.Fatalf("build palai: %v: %s", err, out)
	}
	dir, keyDir := t.TempDir(), t.TempDir()
	signing, pubkey := filepath.Join(keyDir, "k.key"), filepath.Join(keyDir, "k.pub")
	mintKey(t, signing, pubkey)

	env := append(os.Environ(), "PALAI_AUDIT_POSTGRES_URL="+componentURL(t))
	run := func(args ...string) (string, int) {
		cmd := osexec.Command(bin, args...)
		cmd.Env = env
		out, err := cmd.CombinedOutput()
		code := 0
		var ee *osexec.ExitError
		if errors.As(err, &ee) {
			code = ee.ExitCode()
		} else if err != nil {
			t.Fatalf("run %v: %v", args, err)
		}
		return string(out), code
	}

	if out, code := run("audit", "checkpoint", "--out", dir, "--signing-key", signing); code != 0 {
		t.Fatalf("audit checkpoint exited %d: %s", code, out)
	}
	cpPath := filepath.Join(dir, audit.CheckpointFile)

	// GREEN: exit 0 and ok:true.
	out, code := run("audit", "verify", "--checkpoint", cpPath, "--pubkey", pubkey, "--json")
	if code != 0 {
		t.Fatalf("audit verify on an intact journal exited %d: %s", code, out)
	}
	var rep audit.Report
	if err := json.Unmarshal([]byte(strings.TrimSpace(out)), &rep); err != nil {
		t.Fatalf("verify --json is not a typed report (%v): %s", err, out)
	}
	if !rep.OK || len(rep.Alerts) != 0 {
		t.Fatalf("intact journal reported %+v", rep)
	}

	// TAMPER: exit non-zero and a typed tamper alert in the report — not a log line.
	exec(t, pool, `UPDATE events SET payload = '{"cli": 99}'::jsonb WHERE session_id = $1 AND seq = 2`, sessionID)
	out, code = run("audit", "verify", "--checkpoint", cpPath, "--pubkey", pubkey, "--json")
	if code == 0 {
		t.Fatalf("audit verify exited 0 on a tampered journal: %s", out)
	}
	if err := json.Unmarshal([]byte(firstLine(out)), &rep); err != nil {
		t.Fatalf("tampered verify emitted no typed report (%v): %s", err, out)
	}
	if rep.OK || !rep.Has(audit.AlertTamper) {
		t.Fatalf("tampered verify report = %+v, want ok:false with a %q alert", rep, audit.AlertTamper)
	}

	// WRONG KEY: fail-closed at the CLI boundary too.
	wrongKey, wrongPub := filepath.Join(keyDir, "w.key"), filepath.Join(keyDir, "w.pub")
	mintKey(t, wrongKey, wrongPub)
	if out, code := run("audit", "verify", "--checkpoint", cpPath, "--pubkey", wrongPub, "--json"); code == 0 {
		t.Fatalf("audit verify accepted a checkpoint under the WRONG key: %s", out)
	}
	// ... and with NO key at all.
	if out, code := run("audit", "verify", "--checkpoint", cpPath, "--json"); code == 0 {
		t.Fatalf("audit verify accepted a checkpoint with NO trust anchor: %s", out)
	}
}

// firstLine returns the JSON report line, ignoring any warning the command wrote to stderr first.
func firstLine(out string) string {
	for _, line := range strings.Split(out, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "{") {
			return strings.TrimSpace(line)
		}
	}
	return strings.TrimSpace(out)
}

// TestAuditReadRefusesARowLevelScopedConnection is the VACUOUS-CHECKPOINT arm against real RLS.
//
// The commands SAY they connect as the stack's Postgres superuser; the documented second path
// (PALAI_AUDIT_POSTGRES_URL) carries no such guarantee. Every tenant table is FORCE ROW LEVEL
// SECURITY (000029), so a connection holding only the runtime role and no palai.org_id reads the
// journal as ZERO ROWS WITH NO ERROR — and a checkpoint cut from that anchors the empty prefix and
// verifies green against any journal forever. This drives a REAL restricted login against the REAL
// policies: first showing the silent-empty read is real, then that ReadRows now refuses it.
func TestAuditReadRefusesARowLevelScopedConnection(t *testing.T) {
	cs := openHarness(t)
	pool := cs.Pool()
	ctx := storage.WithSystemScope(context.Background())
	tenant, sessionID, _ := seedRun(t, pool)
	for seq := 1; seq <= 4; seq++ {
		exec(t, pool, `INSERT INTO events (id, organization_id, project_id, session_id, seq, type, payload)
		               VALUES ($1, $2, $3, $4, $5, $6, $7)`,
			newID("evt"), tenant.Project, sessionID, seq, "run.step.v1", `{"rls": true}`)
	}
	// The superuser read the commands are documented to use sees them.
	if full := readJournal(t, ctx, pool); len(full) == 0 {
		t.Fatalf("the superuser read saw no rows; the fixture never landed")
	}

	// A real restricted login: LOGIN + the runtime role's grants, nothing more. Dropped on cleanup.
	role, password := "palai_audit_scoped_test", "throwaway-"+newID("pw")
	execAsOwner(t, pool, fmt.Sprintf(`DROP ROLE IF EXISTS %s`, role))
	execAsOwner(t, pool, fmt.Sprintf(`CREATE ROLE %s LOGIN PASSWORD %s IN ROLE %s`,
		role, quoteLiteral(password), storage.RuntimeRole))
	t.Cleanup(func() { execAsOwner(t, pool, fmt.Sprintf(`DROP ROLE IF EXISTS %s`, role)) })

	scopedURL := withCredentials(t, componentURL(t), role, password)
	scoped, err := pgx.Connect(context.Background(), scopedURL)
	if err != nil {
		t.Fatalf("connect as %s: %v", role, err)
	}
	defer scoped.Close(context.Background())

	// THE HOLE, shown against real policies: the query itself succeeds and returns nothing at all.
	raw, err := scoped.Query(context.Background(), storage.Query("AuditChainRows"))
	if err != nil {
		t.Fatalf("the scoped query errored rather than returning empty (%v) — if Postgres now refuses "+
			"outright, the silent-empty premise below is stale", err)
	}
	var silent int
	for raw.Next() {
		silent++
	}
	raw.Close()
	if err := raw.Err(); err != nil {
		t.Fatalf("iterate scoped query: %v", err)
	}
	if silent != 0 {
		t.Fatalf("the row-level-security-scoped connection saw %d row(s); this arm's premise is that it "+
			"sees zero WITHOUT an error", silent)
	}

	// ... and ReadRows now refuses instead of handing that emptiness back as the journal.
	rows, err := audit.ReadRows(context.Background(), scoped)
	if err == nil {
		t.Fatalf("audit.ReadRows returned %d row(s) and no error over a row-level-security-scoped "+
			"connection; a checkpoint cut from it would anchor nothing and verify green forever", len(rows))
	}
	if !strings.Contains(err.Error(), "REFUSING") {
		t.Fatalf("ReadRows error = %q, want it to name the refusal", err)
	}
}

// quoteLiteral wraps a throwaway password for a role-creation statement. The value is generated in
// this test and dies with the container; it can carry no quote, but escaping is free.
func quoteLiteral(s string) string { return "'" + strings.ReplaceAll(s, "'", "''") + "'" }

// withCredentials re-points the component URL at another role.
func withCredentials(t *testing.T, raw, user, password string) string {
	t.Helper()
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatalf("parse component URL: %v", err)
	}
	u.User = url.UserPassword(user, password)
	return u.String()
}

// TestAuditVerifyReportsCheckpointAgeAndAlertsOnRollback is the ROLLBACK arm at the CLI boundary: the
// operator surface must SHOW the checkpoint's age and turn an out-of-window one into a typed alert
// and a non-zero exit. An older validly-signed checkpoint verifies its own prefix perfectly, so
// without this a swapped-in stale copy is indistinguishable from a fresh one.
func TestAuditVerifyReportsCheckpointAgeAndAlertsOnRollback(t *testing.T) {
	if _, err := osexec.LookPath("openssl"); err != nil {
		t.Skip("openssl not on PATH — the checkpoint signature is the E14 T5 openssl signer")
	}
	cs := openHarness(t)
	pool := cs.Pool()
	tenant, sessionID, _ := seedRun(t, pool)
	for seq := 1; seq <= 3; seq++ {
		exec(t, pool, `INSERT INTO events (id, organization_id, project_id, session_id, seq, type, payload)
		               VALUES ($1, $2, $3, $4, $5, $6, $7)`,
			newID("evt"), tenant.Project, sessionID, seq, "run.step.v1",
			fmt.Sprintf(`{"age": %d}`, seq))
	}

	bin := filepath.Join(t.TempDir(), "palai")
	if out, err := osexec.Command("go", "build", "-o", bin, "github.com/palgroup/palai/cmd/cli").CombinedOutput(); err != nil {
		t.Fatalf("build palai: %v: %s", err, out)
	}
	dir, keyDir := t.TempDir(), t.TempDir()
	signing, pubkey := filepath.Join(keyDir, "k.key"), filepath.Join(keyDir, "k.pub")
	mintKey(t, signing, pubkey)

	env := append(os.Environ(), "PALAI_AUDIT_POSTGRES_URL="+componentURL(t))
	run := func(args ...string) (string, int) {
		cmd := osexec.Command(bin, args...)
		cmd.Env = env
		out, err := cmd.CombinedOutput()
		code := 0
		var ee *osexec.ExitError
		if errors.As(err, &ee) {
			code = ee.ExitCode()
		} else if err != nil {
			t.Fatalf("run %v: %v", args, err)
		}
		return string(out), code
	}

	if out, code := run("audit", "checkpoint", "--out", dir, "--signing-key", signing); code != 0 {
		t.Fatalf("audit checkpoint exited %d: %s", code, out)
	}
	cpPath := filepath.Join(dir, audit.CheckpointFile)

	// The report CARRIES the age, and the ceiling, in --json (T10 reads --json, not the terminal).
	out, code := run("audit", "verify", "--checkpoint", cpPath, "--pubkey", pubkey, "--json")
	if code != 0 {
		t.Fatalf("verify exited %d: %s", code, out)
	}
	var rep audit.Report
	if err := json.Unmarshal([]byte(firstLine(out)), &rep); err != nil {
		t.Fatalf("verify --json is not a typed report (%v): %s", err, out)
	}
	if rep.CheckpointGeneratedAt == "" || rep.CheckpointAge == "" {
		t.Fatalf("the report carries no checkpoint age (%+v); an operator cannot tell a fresh anchor from "+
			"a 90-day-old one", rep)
	}
	if len(rep.Ceiling) == 0 {
		t.Fatalf("the --json report carries no ceiling; SEC-103's limits reached only the terminal")
	}

	// A window this checkpoint cannot possibly satisfy: it was cut seconds ago, so ask for nanoseconds.
	// This is the rollback detector, exercised where it will actually be used.
	out, code = run("audit", "verify", "--checkpoint", cpPath, "--pubkey", pubkey,
		"--not-older-than", "1ns", "--json")
	if code == 0 {
		t.Fatalf("an out-of-window checkpoint exited 0: %s", out)
	}
	if err := json.Unmarshal([]byte(firstLine(out)), &rep); err != nil {
		t.Fatalf("stale verify emitted no typed report (%v): %s", err, out)
	}
	if rep.OK || !rep.Has(audit.AlertStale) {
		t.Fatalf("stale verify report = %+v, want ok:false with a %q alert", rep, audit.AlertStale)
	}

	// The coverage floor is the clock-free half of the same detector.
	out, code = run("audit", "verify", "--checkpoint", cpPath, "--pubkey", pubkey,
		"--min-anchored", "100000", "--json")
	if code == 0 {
		t.Fatalf("a checkpoint far under the --min-anchored floor exited 0: %s", out)
	}
	if err := json.Unmarshal([]byte(firstLine(out)), &rep); err != nil || !rep.Has(audit.AlertStale) {
		t.Fatalf("under-floor verify report = %+v (err=%v), want a %q alert", rep, err, audit.AlertStale)
	}

	// A generous window is green again — the arm is not just "always alerts".
	if out, code := run("audit", "verify", "--checkpoint", cpPath, "--pubkey", pubkey,
		"--not-older-than", "24h", "--min-anchored", "1", "--json"); code != 0 {
		t.Fatalf("a fresh, adequately covered checkpoint exited %d: %s", code, out)
	}
}

// TestAuditCheckpointRefusesAVacuousJournal is the vacuous arm at the CLI boundary: `audit checkpoint`
// over a journal with no rows used to print "anchored 0 event(s)" and write a perfectly valid signed
// file. It must refuse, and --allow-empty must be the only way past.
func TestAuditCheckpointRefusesAVacuousJournal(t *testing.T) {
	if _, err := osexec.LookPath("openssl"); err != nil {
		t.Skip("openssl not on PATH — the checkpoint signature is the E14 T5 openssl signer")
	}
	// A database of its own, migrated but never written to (the E15 T1 helper). Emptying the shared
	// one would pull the journal out from under every other arm in this tier.
	emptyURL := freshDatabase(t)
	empty, err := coordinator.Open(context.Background(), emptyURL)
	if err != nil {
		t.Fatalf("open the empty database: %v", err)
	}
	defer empty.Close()
	if err := empty.Migrate(context.Background()); err != nil {
		t.Fatalf("migrate the empty database: %v", err)
	}

	bin := filepath.Join(t.TempDir(), "palai")
	if out, err := osexec.Command("go", "build", "-o", bin, "github.com/palgroup/palai/cmd/cli").CombinedOutput(); err != nil {
		t.Fatalf("build palai: %v: %s", err, out)
	}
	dir, keyDir := t.TempDir(), t.TempDir()
	signing := filepath.Join(keyDir, "k.key")
	mintKey(t, signing, filepath.Join(keyDir, "k.pub"))

	env := append(os.Environ(), "PALAI_AUDIT_POSTGRES_URL="+emptyURL)
	cmd := osexec.Command(bin, "audit", "checkpoint", "--out", dir, "--signing-key", signing)
	cmd.Env = env
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("audit checkpoint signed an anchor over ZERO rows: %s", out)
	}
	if !strings.Contains(string(out), "REFUSING") {
		t.Fatalf("the refusal does not name itself: %s", out)
	}
	if _, err := os.Stat(filepath.Join(dir, audit.CheckpointFile)); !os.IsNotExist(err) {
		t.Fatalf("a refused checkpoint still left a file behind (stat err = %v)", err)
	}

	// --allow-empty is the explicit opt-in for a journal that really is empty.
	cmd = osexec.Command(bin, "audit", "checkpoint", "--out", dir, "--signing-key", signing, "--allow-empty")
	cmd.Env = env
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("--allow-empty was still refused: %v: %s", err, out)
	}
}
