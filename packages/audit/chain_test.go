package audit_test

import (
	"os"
	"os/exec"
	"path/filepath"
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
// alert naming the session and the missing seq — and NOT reported as a tamper (a missing row already
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
	if err := audit.WriteSigned(dir, cp, right); err != nil {
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
