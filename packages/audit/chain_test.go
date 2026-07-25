package audit_test

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/palgroup/palai/packages/audit"
)

// journal builds a deterministic three-session journal: seq 1..n per session.
func journal(sessions map[string]int) []audit.Row {
	var out []audit.Row
	var j int64
	for id, n := range sessions {
		for s := int64(1); s <= int64(n); s++ {
			j++
			out = append(out, audit.Row{
				ID: id + "-" + string(rune('a'+s)), OrganizationID: "org_1", ProjectID: "proj_1",
				SessionID: id, Seq: s, JournalID: j, Type: "run.step.v1",
				Payload:   `{"k": "v", "n": ` + string(rune('0'+s)) + `}`,
				CreatedAt: "2026-07-25T10:00:0" + string(rune('0'+s)) + ".000000Z",
			})
		}
	}
	return out
}

func fixture() []audit.Row { return journal(map[string]int{"sess_a": 4, "sess_b": 3}) }

// rowsN is a one-session journal of n rows, for the arms that need a journal to GROW.
func rowsN(session string, n int) []audit.Row {
	var out []audit.Row
	for s := int64(1); s <= int64(n); s++ {
		out = append(out, audit.Row{
			ID: fmt.Sprintf("%s-%03d", session, s), OrganizationID: "org_1", ProjectID: "proj_1",
			SessionID: session, Seq: s, JournalID: s, Type: "run.step.v1",
			Payload:   fmt.Sprintf(`{"step": %d}`, s),
			CreatedAt: fmt.Sprintf("2026-04-%02dT10:00:00.000000Z", 1+(s%27)),
		})
	}
	return out
}

// TestHeadIsOrderIndependentAndRowSensitive pins the two properties everything else rests on: the
// chain head depends on the row CONTENT and canonical order, not on the order rows were handed over.
func TestHeadIsOrderIndependentAndRowSensitive(t *testing.T) {
	rows := fixture()
	shuffled := append([]audit.Row{rows[3], rows[0]}, rows[1:3]...)
	shuffled = append(shuffled, rows[4:]...)
	if audit.Head(rows) != audit.Head(shuffled) {
		t.Fatalf("Head() depends on input order; it must chain in canonical (session_id, seq) order")
	}

	flipped := append([]audit.Row(nil), rows...)
	flipped[2].Payload = strings.Replace(flipped[2].Payload, `"v"`, `"w"`, 1)
	if audit.Head(flipped) == audit.Head(rows) {
		t.Fatalf("Head() is blind to a payload byte change — the chain would never see a tamper")
	}

	// Length-prefixing: moving a byte across a field boundary must change the digest. Without it,
	// ("ab","c") and ("a","bc") collide and an attacker can re-slice a row into a different one.
	a := []audit.Row{{SessionID: "s", Seq: 1, Type: "ab", Payload: "c"}}
	b := []audit.Row{{SessionID: "s", Seq: 1, Type: "a", Payload: "bc"}}
	if audit.Head(a) == audit.Head(b) {
		t.Fatalf("Head() collides across a field boundary — fields are not length-prefixed")
	}
}

// TestIntactJournalVerifiesGreen is the GREEN arm: an untouched journal against its own checkpoint
// raises zero alerts and recomputes the anchored head exactly.
func TestIntactJournalVerifiesGreen(t *testing.T) {
	rows := fixture()
	cp := audit.NewCheckpoint(rows, time.Now())
	rep := audit.Compare(cp, rows)
	if !rep.OK || len(rep.Alerts) != 0 {
		t.Fatalf("intact journal produced alerts %+v, want none", rep.Alerts)
	}
	if rep.RecomputedHead != cp.Head {
		t.Fatalf("recomputed head %s != checkpoint head %s", rep.RecomputedHead, cp.Head)
	}
	if rep.AnchoredRows != len(rows) || rep.UnanchoredRows != 0 {
		t.Fatalf("anchored=%d unanchored=%d, want %d/0", rep.AnchoredRows, rep.UnanchoredRows, len(rows))
	}
}

// TestDeletedRowRaisesGap is the GAP arm: a row removed from an anchored session is a typed "gap"
// alert naming the session — and NOT reported as a tamper (a missing row already
// explains the head difference; see Compare).
func TestDeletedRowRaisesGap(t *testing.T) {
	rows := fixture()
	cp := audit.NewCheckpoint(rows, time.Now())

	var kept []audit.Row
	for _, r := range rows {
		if r.SessionID == "sess_a" && r.Seq == 2 {
			continue
		}
		kept = append(kept, r)
	}
	rep := audit.Compare(cp, kept)
	if rep.OK {
		t.Fatalf("a deleted row verified OK — the gap is invisible")
	}
	if !rep.Has(audit.AlertGap) {
		t.Fatalf("alerts = %+v, want a %q alert", rep.Alerts, audit.AlertGap)
	}
	if rep.Has(audit.AlertTamper) {
		t.Fatalf("a gap also raised %q; the byte-level arm would then be vacuous", audit.AlertTamper)
	}
	for _, a := range rep.Alerts {
		if a.Kind == audit.AlertGap && a.SessionID != "sess_a" {
			t.Fatalf("gap alert names session %q, want sess_a", a.SessionID)
		}
	}

	// Truncation (the tail row removed, so no interior hole) is a gap too.
	var truncated []audit.Row
	for _, r := range rows {
		if r.SessionID == "sess_b" && r.Seq == 3 {
			continue
		}
		truncated = append(truncated, r)
	}
	if rep := audit.Compare(cp, truncated); !rep.Has(audit.AlertGap) {
		t.Fatalf("a truncated tail did not raise %q: %+v", audit.AlertGap, rep.Alerts)
	}
}

// TestFlippedPayloadByteRaisesTamper is the TAMPER arm: every anchored row is still present, so the
// seq set is intact, but one payload byte changed — a typed "tamper" alert, with no gap.
func TestFlippedPayloadByteRaisesTamper(t *testing.T) {
	rows := fixture()
	cp := audit.NewCheckpoint(rows, time.Now())

	edited := append([]audit.Row(nil), rows...)
	for i := range edited {
		if edited[i].SessionID == "sess_b" && edited[i].Seq == 2 {
			edited[i].Payload = strings.Replace(edited[i].Payload, `"v"`, `"V"`, 1)
		}
	}
	rep := audit.Compare(cp, edited)
	if rep.OK {
		t.Fatalf("a flipped payload byte verified OK — the tamper is invisible")
	}
	if !rep.Has(audit.AlertTamper) {
		t.Fatalf("alerts = %+v, want a %q alert", rep.Alerts, audit.AlertTamper)
	}
	// The alert must LOCALIZE the tamper to its session; a bare global head mismatch tells an
	// operator only that something moved, not where to look.
	localized := false
	for _, a := range rep.Alerts {
		if a.Kind == audit.AlertTamper && a.SessionID == "sess_b" {
			localized = true
		}
	}
	if !localized {
		t.Fatalf("tamper alerts %+v name no session; the per-session anchor is not being compared", rep.Alerts)
	}
	if rep.Has(audit.AlertGap) {
		t.Fatalf("a byte flip raised %q; gap and tamper are not being discriminated", audit.AlertGap)
	}
}

// TestRowsAppendedAfterTheCheckpointAreUnanchoredNotAlerts pins the honest ceiling: a checkpoint
// anchors a PREFIX, so later rows are counted and reported, never treated as corruption — and never
// silently blessed either.
func TestRowsAppendedAfterTheCheckpointAreUnanchoredNotAlerts(t *testing.T) {
	rows := fixture()
	cp := audit.NewCheckpoint(rows, time.Now())
	grown := append(append([]audit.Row(nil), rows...), audit.Row{
		ID: "later", OrganizationID: "org_1", ProjectID: "proj_1", SessionID: "sess_a",
		Seq: 5, JournalID: 99, Type: "run.step.v1", Payload: `{}`, CreatedAt: "2026-07-25T11:00:00.000000Z",
	}, audit.Row{
		ID: "newsess", OrganizationID: "org_1", ProjectID: "proj_1", SessionID: "sess_c",
		Seq: 1, JournalID: 100, Type: "run.step.v1", Payload: `{}`, CreatedAt: "2026-07-25T11:00:01.000000Z",
	})
	rep := audit.Compare(cp, grown)
	if !rep.OK {
		t.Fatalf("appended rows raised alerts %+v; a growing journal is not a tampered one", rep.Alerts)
	}
	if rep.UnanchoredRows != 2 {
		t.Fatalf("unanchored = %d, want 2 (one late row + one whole new session)", rep.UnanchoredRows)
	}
}

// TestWrongVersionOrAlgorithmIsRefusedNotCompared: a checkpoint cut with another construction is
// refused, not compared against a head that means something else.
func TestWrongVersionOrAlgorithmIsRefusedNotCompared(t *testing.T) {
	rows := fixture()
	cp := audit.NewCheckpoint(rows, time.Now())
	cp.Algorithm = "palai-audit-chain-blake3-v9"
	rep := audit.Compare(cp, rows)
	if !rep.Has(audit.AlertMalformed) || rep.OK {
		t.Fatalf("an unknown algorithm was compared anyway: %+v", rep.Alerts)
	}
}

// TestCheckpointSignatureFailClosed is the FAIL-CLOSED arm, three ways: a checkpoint signed with the
// WRONG key fails, a key taken from beside the checkpoint is REFUSED as a trust anchor unless the
// explicit same-session opt-in is set, and no key at all fails. Each is a typed AlertSignature.
func TestCheckpointSignatureFailClosed(t *testing.T) {
	if _, err := exec.LookPath("openssl"); err != nil {
		t.Skip("openssl not on PATH — the checkpoint signature is the E14 T5 openssl signer")
	}
	dir := t.TempDir()
	right := filepath.Join(t.TempDir(), "right.key")
	wrong := filepath.Join(t.TempDir(), "wrong.key")
	genKey(t, right)
	genKey(t, wrong)

	cp := audit.NewCheckpoint(fixture(), time.Now())
	if err := audit.WriteSigned(dir, cp, right, false); err != nil {
		t.Fatalf("WriteSigned() error = %v", err)
	}
	path := filepath.Join(dir, audit.CheckpointFile)

	rightPub := filepath.Join(t.TempDir(), "right.pub")
	wrongPub := filepath.Join(t.TempDir(), "wrong.pub")
	pubOf(t, right, rightPub)
	pubOf(t, wrong, wrongPub)

	// GREEN: the real out-of-band key.
	if _, _, alert := audit.LoadSigned(path, rightPub); alert != nil {
		t.Fatalf("a correctly signed checkpoint failed to load: %+v", alert)
	}

	// WRONG KEY: fail-closed with a typed signature alert.
	_, _, alert := audit.LoadSigned(path, wrongPub)
	if alert == nil || alert.Kind != audit.AlertSignature {
		t.Fatalf("a checkpoint signed with the WRONG key loaded anyway (alert=%+v)", alert)
	}

	// BUNDLED KEY: the copy written beside the checkpoint is not a trust anchor.
	bundled := filepath.Join(dir, audit.PubkeyFile)
	t.Setenv(audit.AllowBundledPubkeyEnv, "")
	_, _, alert = audit.LoadSigned(path, bundled)
	if alert == nil || alert.Kind != audit.AlertSignature {
		t.Fatalf("a public key from INSIDE the checkpoint dir was accepted as a trust anchor (alert=%+v)", alert)
	}
	if !strings.Contains(alert.Detail, "REFUSING") {
		t.Fatalf("bundled-key refusal detail = %q, want it to name the refusal", alert.Detail)
	}
	// ... and the explicit same-session opt-in lets it through, loudly.
	t.Setenv(audit.AllowBundledPubkeyEnv, "1")
	cpLoaded, warn, alert := audit.LoadSigned(path, bundled)
	if alert != nil {
		t.Fatalf("with %s=1 the bundled key still failed: %+v", audit.AllowBundledPubkeyEnv, alert)
	}
	if warn == "" {
		t.Fatalf("the bundled-key opt-in produced no warning; it must say out loud what it proves")
	}
	if cpLoaded.Head != cp.Head {
		t.Fatalf("loaded head %s != written head %s", cpLoaded.Head, cp.Head)
	}

	// NO KEY: refused before anything is read.
	_, _, alert = audit.LoadSigned(path, "")
	if alert == nil || alert.Kind != audit.AlertSignature {
		t.Fatalf("a checkpoint verified with NO key (alert=%+v)", alert)
	}

	// TAMPERED CHECKPOINT BYTES: the signature no longer covers them.
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(strings.Replace(string(body), `"count"`, `"COUNT"`, 1)), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, _, alert := audit.LoadSigned(path, rightPub); alert == nil || alert.Kind != audit.AlertSignature {
		t.Fatalf("an edited checkpoint verified anyway (alert=%+v)", alert)
	}
}

func genKey(t *testing.T, path string) {
	t.Helper()
	out, err := exec.Command("openssl", "genpkey", "-algorithm", "EC",
		"-pkeyopt", "ec_paramgen_curve:P-256", "-out", path).CombinedOutput()
	if err != nil {
		t.Fatalf("genpkey: %v: %s", err, out)
	}
}

func pubOf(t *testing.T, key, pub string) {
	t.Helper()
	out, err := exec.Command("openssl", "pkey", "-in", key, "-pubout", "-out", pub).CombinedOutput()
	if err != nil {
		t.Fatalf("pubout: %v: %s", err, out)
	}
}

// TestALegitimateHoleIsNotAGap: `events.seq` is assigned by the APPLICATION, so a rolled-back insert
// burns a number and leaves a permanent hole. A checkpoint cut over a journal that ALREADY has one
// must verify green — a gap rule built on "seq 1..max must be contiguous" would cry gap over an
// ordinary aborted transaction and train an operator to ignore the alert.
//
// This test used to justify itself with the §22.2 retention purge "deleting" events. It does not:
// scrub_events UPDATEs the payload (see TestARetentionPurgeIsIndistinguishableFromTamper below).
func TestALegitimateHoleIsNotAGap(t *testing.T) {
	var holed []audit.Row
	for _, r := range fixture() {
		if r.SessionID == "sess_a" && r.Seq == 2 {
			continue // purged before the checkpoint was ever cut
		}
		holed = append(holed, r)
	}
	cp := audit.NewCheckpoint(holed, time.Now())
	if rep := audit.Compare(cp, holed); !rep.OK {
		t.Fatalf("a journal with a pre-existing retention hole raised %+v, want no alerts", rep.Alerts)
	}
	// ... and losing a row from that already-holed journal is still a gap.
	if rep := audit.Compare(cp, holed[1:]); !rep.Has(audit.AlertGap) {
		t.Fatalf("removing a row from a holed journal did not raise %q: %+v", audit.AlertGap, rep.Alerts)
	}
}

// TestAnOldValidlySignedCheckpointIsStale is the ROLLBACK arm. An attacker — or a restore from a
// stale backup — swaps in an OLDER checkpoint. It is validly signed, its own tiny prefix is intact,
// so Compare says ok=true, the whole journal since becomes merely "unanchored", and 20x of growth
// containing a forged row raises absolutely nothing. A MISSING checkpoint was already distinguished
// (AlertSignature, exit != 0); OLD versus FRESH was not, and the report did not even carry the age.
func TestAnOldValidlySignedCheckpointIsStale(t *testing.T) {
	now := time.Date(2026, 7, 25, 12, 0, 0, 0, time.UTC)
	old := audit.NewCheckpoint(rowsN("sess_a", 2), now.Add(-90*24*time.Hour))

	grown := rowsN("sess_a", 40)
	grown[20].Payload = `{"step": 21, "forged": true}`

	rep := audit.Compare(old, grown)
	// This IS green by design — the anchored prefix really is intact. That is the point: green here
	// says nothing about the 38 rows the anchor never saw.
	if !rep.OK {
		t.Fatalf("the anchored prefix was intact but Compare alerted: %+v", rep.Alerts)
	}
	if rep.CheckpointGeneratedAt != old.GeneratedAt {
		t.Fatalf("the report carries generated_at %q, want %q — without it a 90-day-old anchor and one "+
			"cut a minute ago produce the same report", rep.CheckpointGeneratedAt, old.GeneratedAt)
	}

	stale := audit.Freshness(old, 7*24*time.Hour, 0, now)
	if len(stale) != 1 || stale[0].Kind != audit.AlertStale {
		t.Fatalf("a 90-day-old checkpoint against a 7-day window raised %+v, want one %q alert", stale, audit.AlertStale)
	}
	if !strings.Contains(stale[0].Detail, "rollback") {
		t.Fatalf("the stale alert does not name the rollback it exists for: %q", stale[0].Detail)
	}

	// A fresh checkpoint in the same window raises nothing.
	if a := audit.Freshness(audit.NewCheckpoint(grown, now.Add(-time.Hour)), 7*24*time.Hour, 0, now); len(a) != 0 {
		t.Fatalf("a one-hour-old checkpoint was called stale: %+v", a)
	}

	// ... and the coverage floor catches the same rollback WITHOUT trusting the clock, which matters
	// when whoever swapped the file also owns the host's time.
	if a := audit.Freshness(old, 0, 20, now); len(a) != 1 || a[0].Kind != audit.AlertStale {
		t.Fatalf("a checkpoint anchoring 2 rows against a floor of 20 raised %+v, want %q", a, audit.AlertStale)
	}
	if a := audit.Freshness(audit.NewCheckpoint(grown, now), 0, 20, now); len(a) != 0 {
		t.Fatalf("a checkpoint anchoring 40 rows against a floor of 20 was called stale: %+v", a)
	}

	// No declared policy means no staleness VERDICT — but the age is still reported, so the absence
	// of a policy is visible rather than silent.
	if a := audit.Freshness(old, 0, 0, now); len(a) != 0 {
		t.Fatalf("Freshness invented a window nobody declared: %+v", a)
	}
	if audit.Age(old, now) == "" {
		t.Fatalf("Age() rendered nothing for a parseable checkpoint")
	}
}

// TestAVacuousCheckpointIsRefused is the E14 T7 lesson applied to the anchor itself: a checkpoint cut
// from a read that returned ZERO rows anchors the empty prefix, so it verifies green against ANY
// journal forever — every row is merely "unanchored". Nothing has to be malicious to get there: every
// tenant table is FORCE ROW LEVEL SECURITY (000029), and a connection without palai.org_id returns no
// rows AND NO ERROR. A claim over a surface that can be empty can never fail.
func TestAVacuousCheckpointIsRefused(t *testing.T) {
	if _, err := exec.LookPath("openssl"); err != nil {
		t.Skip("openssl not on PATH — the checkpoint signature is the E14 T5 openssl signer")
	}
	empty := audit.NewCheckpoint(nil, time.Now())

	// The hole, stated: an empty anchor is green against half a thousand rows it never saw.
	if rep := audit.Compare(empty, rowsN("sess_a", 500)); !rep.OK || rep.AnchoredRows != 0 {
		t.Fatalf("precondition changed: empty-vs-500 gave ok=%v anchored=%d", rep.OK, rep.AnchoredRows)
	}

	key := filepath.Join(t.TempDir(), "k.key")
	genKey(t, key)
	err := audit.WriteSigned(t.TempDir(), empty, key, false)
	if err == nil {
		t.Fatalf("a ZERO-ROW checkpoint was signed; it would verify green against any journal forever")
	}
	if !strings.Contains(err.Error(), "REFUSING") {
		t.Fatalf("refusal = %q, want it to name the refusal", err)
	}

	// ... and the explicit opt-in lets a genuinely empty journal through.
	if err := audit.WriteSigned(t.TempDir(), empty, key, true); err != nil {
		t.Fatalf("--allow-empty still refused: %v", err)
	}
	// A non-empty checkpoint is unaffected.
	if err := audit.WriteSigned(t.TempDir(), audit.NewCheckpoint(fixture(), time.Now()), key, false); err != nil {
		t.Fatalf("a real checkpoint was refused: %v", err)
	}
}

// TestARetentionPurgeIsIndistinguishableFromTamper is the correction to this package's own comment.
// `scrub_events` (storage/queries/responses.sql) UPDATEs an anchored row's payload to
// {"purged": true} — it does not DELETE, and there is no `DELETE FROM events` in production code at
// all. So on a stack with PALAI_RETENTION_STORE_FALSE_TTL set, the reaper leaves the same rows at the
// same seq with different bytes: the exact tamper signature, raised by routine maintenance. The
// behaviour is correct and unavoidable at this design point; what is NOT acceptable is failing to say
// so where an operator meets the alert, because it is also perfect cover ("the reaper did it").
func TestARetentionPurgeIsIndistinguishableFromTamper(t *testing.T) {
	rows := rowsN("sess_a", 6)
	cp := audit.NewCheckpoint(rows, time.Now())

	purged := append([]audit.Row(nil), rows...)
	for i := range purged {
		if purged[i].Seq == 3 {
			purged[i].Payload = `{"purged": true}` // byte for byte what scrub_events writes
		}
	}
	rep := audit.Compare(cp, purged)
	if !rep.Has(audit.AlertTamper) {
		t.Fatalf("a retention purge raised %+v; the purge rewrites anchored bytes, so it MUST land as a tamper "+
			"(if this ever stops being true, the ceiling text is now a lie)", rep.Alerts)
	}
	if rep.Has(audit.AlertGap) {
		t.Fatalf("the purge raised %q — it removes no row", audit.AlertGap)
	}
	for _, a := range rep.Alerts {
		if a.Kind != audit.AlertTamper {
			continue
		}
		if !strings.Contains(a.Detail, "retention purge") || !strings.Contains(a.Detail, "re-cut") {
			t.Fatalf("the tamper alert does not warn that an authorised purge looks identical, nor name the "+
				"remedy: %q", a.Detail)
		}
	}
	// The same warning must reach the machine-readable ceiling, not only the alert prose.
	var named bool
	for _, c := range audit.Ceilings {
		if strings.Contains(c, "RETENTION PURGE") {
			named = true
		}
	}
	if !named {
		t.Fatalf("audit.Ceilings does not name the retention purge; %v", audit.Ceilings)
	}
}

// TestEveryRowFieldIsInTheDigest binds Row's FIELDS to digest(). The component guard already pins the
// column SET mechanically (information_schema vs ChainedColumns), but nothing stopped someone adding
// a column to the SQL, to Row, and to Scan while forgetting digest() — the column would then be
// chained on paper and editable without a tamper alert in practice. Mutating each field must move the
// head.
func TestEveryRowFieldIsInTheDigest(t *testing.T) {
	base := audit.Row{
		ID: "evt_1", OrganizationID: "org_1", ProjectID: "proj_1", SessionID: "sess_a",
		ResponseID: "resp_1", HasResponseID: true, Seq: 3, JournalID: 7, Type: "run.step.v1",
		Payload: `{"k":"v"}`, CreatedAt: "2026-07-25T10:00:00.000000Z",
	}
	want := audit.Head([]audit.Row{base})
	rt := reflect.TypeOf(base)
	for i := 0; i < rt.NumField(); i++ {
		mutated := base
		f := reflect.ValueOf(&mutated).Elem().Field(i)
		switch f.Kind() {
		case reflect.String:
			f.SetString(f.String() + "x")
		case reflect.Int64:
			f.SetInt(f.Int() + 1)
		case reflect.Bool:
			f.SetBool(!f.Bool())
		default:
			t.Fatalf("Row.%s is a %s this guard cannot mutate; teach it that kind rather than leaving the "+
				"field unchecked", rt.Field(i).Name, f.Kind())
		}
		if audit.Head([]audit.Row{mutated}) == want {
			t.Fatalf("changing Row.%s does not change the chain head — the field is carried but NOT digested, "+
				"so it can be edited with no tamper alert; add it to Row.digest()", rt.Field(i).Name)
		}
	}
}

// TestPubkeyContainmentSurvivesSymlinks: the prefix rule was a STRING comparison, and an ordinary
// symlink defeated it in both directions while the report went on printing "key supplied out of
// band" — asserting the exact property it did not have.
func TestPubkeyContainmentSurvivesSymlinks(t *testing.T) {
	if _, err := exec.LookPath("openssl"); err != nil {
		t.Skip("openssl not on PATH — the checkpoint signature is the E14 T5 openssl signer")
	}
	dir, keyDir := t.TempDir(), t.TempDir()
	key := filepath.Join(keyDir, "k.key")
	genKey(t, key)
	if err := audit.WriteSigned(dir, audit.NewCheckpoint(fixture(), time.Now()), key, false); err != nil {
		t.Fatalf("WriteSigned() error = %v", err)
	}
	cpPath := filepath.Join(dir, audit.CheckpointFile)
	bundled := filepath.Join(dir, audit.PubkeyFile)
	t.Setenv(audit.AllowBundledPubkeyEnv, "")

	// (a) A symlink OUTSIDE the checkpoint dir pointing AT the bundled key. Its textual path is
	// elsewhere; the bytes it verifies with are the attacker's.
	link := filepath.Join(t.TempDir(), "innocent.pub")
	if err := os.Symlink(bundled, link); err != nil {
		t.Fatalf("symlink: %v", err)
	}
	if _, _, alert := audit.LoadSigned(cpPath, link); alert == nil || alert.Kind != audit.AlertSignature {
		t.Fatalf("a symlink to the bundled key was accepted as an out-of-band trust anchor (alert=%+v)", alert)
	}

	// (b) The mirror image: reach the CHECKPOINT through a symlinked directory and hand over the key
	// by its real path. Now the dir string is elsewhere instead.
	dirLink := filepath.Join(t.TempDir(), "cp")
	if err := os.Symlink(dir, dirLink); err != nil {
		t.Fatalf("symlink dir: %v", err)
	}
	if _, _, alert := audit.LoadSigned(filepath.Join(dirLink, audit.CheckpointFile), bundled); alert == nil || alert.Kind != audit.AlertSignature {
		t.Fatalf("reaching the checkpoint through a symlinked directory laundered the bundled key (alert=%+v)", alert)
	}

	// A genuinely out-of-band key, and a SIBLING directory whose name merely shares a prefix, both
	// stay allowed — the fix must not turn into a blanket refusal.
	pub := filepath.Join(keyDir, "k.pub")
	pubOf(t, key, pub)
	if _, warn, alert := audit.LoadSigned(cpPath, pub); alert != nil || warn != "" {
		t.Fatalf("an out-of-band key was refused (alert=%+v warn=%q)", alert, warn)
	}
	sibling := dir + "-evil"
	if err := os.MkdirAll(sibling, 0o755); err != nil {
		t.Fatal(err)
	}
	siblingPub := filepath.Join(sibling, "k.pub")
	pubOf(t, key, siblingPub)
	if _, warn, alert := audit.LoadSigned(cpPath, siblingPub); alert != nil || warn != "" {
		t.Fatalf("a sibling directory sharing a name prefix was treated as inside (alert=%+v warn=%q)", alert, warn)
	}
}
