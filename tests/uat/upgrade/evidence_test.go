// Package upgrade holds the E15 T6 SH-2 RC EXIT gate: the Docker-free evidence anchor (this file) + the
// host-agnostic live journey (journey_test.go, //go:build uat). The E15 T6 UAT cases (OPS-003..006, DR-001,
// SAN-011) are materialized in the shared self-host catalog (tests/uat/self-host/catalog_test.go — the
// T2-established pattern), not in this package.
//
// This file is the ANTI-FABRICATION ANCHOR (plan §2, the crown jewel) for the self-host-0.2.0 RC bundle. It
// verifies the committed bundle clean through the shared verifier AND re-derives every anchored value from its
// canonical source — the upgrade journey spine, the event-continuity digest, the helm policy-assert digest, and
// (the measurement anchor) the DR RPO/RTO recomputed from RAW timestamps with the SAME dr.ComputeRPO/RTO the T5
// harness uses. A committed digest/checksum/measurement that does not reproduce is a fabricated value the
// shape-checked verifier cannot see — exactly the E13/E14 managed-cloud/self-host MUST-FIX-1 defect this catches.
//
// The committed bundle is authored deterministic data; the separate live journey (make uat-sh2
// PROVIDER=provider-one) captures the REAL upgrade/DR/air-gap/helm evidence independently. This is the
// deterministic mirror of `make evidence-verify RELEASE=self-host-0.2.0`.
package upgrade

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/palgroup/palai/tests/uat"
	"github.com/palgroup/palai/tests/uat/dr"
)

// hashParts reproduces the tests/uat hashParts construction (sha256 of each part followed by a NUL, hex,
// sha256:-prefixed) so this gate can recompute the committed journey/continuity/asserts digests and per-case
// checksums. ponytail: a 4-line copy, not a shared export (the self-host anchor makes the same copy).
func hashParts(parts ...string) string {
	h := sha256.New()
	for _, p := range parts {
		h.Write([]byte(p))
		h.Write([]byte{0})
	}
	return "sha256:" + hex.EncodeToString(h.Sum(nil))
}

// upgradeSpineAnchored reports whether an UpgradeProof's step_ids + journey_digest are the CANONICAL upgrade
// spine: the list must equal uat.UpgradeStepIDs exactly AND the digest must be hashParts of that canonical list.
// Anchoring to the canonical set (not the manifest's own step_ids) closes the fabrication hole — a bundle
// carrying invented step ids + a self-consistent digest is NOT the real spine.
func upgradeSpineAnchored(stepIDs []string, digest string) bool {
	return slices.Equal(stepIDs, uat.UpgradeStepIDs) && digest == hashParts(uat.UpgradeStepIDs...)
}

// helmAssertsAnchored reports whether a HelmRenderProof's policy_asserts + asserts_digest are the CANONICAL
// restricted set: the list must equal uat.HelmPolicyAsserts exactly AND the digest must be hashParts of it —
// so a bundle that quietly drops no-cluster-role cannot keep a matching digest.
func helmAssertsAnchored(asserts []string, digest string) bool {
	return slices.Equal(asserts, uat.HelmPolicyAsserts) && digest == hashParts(uat.HelmPolicyAsserts...)
}

// continuityAnchored reports whether an event list is a real run stream: >= 2 events that START at
// response.created and END at the run's own terminal type. The EventContinuityDigest alone is self-referential
// (an arbitrary list + a consistent digest passes), so this canons the endpoints against the run's lifecycle
// (MF-2a) — a fabricated list cannot both look like a real stream AND carry the case's terminal.
func continuityAnchored(events []string, terminal string) bool {
	return len(events) >= 2 && events[0] == "response.created" && events[len(events)-1] == terminal
}

// measureDerivable recomputes RPO/RTO from a DrillProof measure's RAW timestamps with the SAME dr.ComputeRPO/RTO
// the T5 harness uses, and reports whether the stored seconds reproduce — the measurement anti-fabrication
// anchor. A hand-edited rpo_seconds/rto_seconds the timestamps do not support is caught here.
func measureDerivable(m *dr.Measure) bool {
	if m == nil {
		return true // detection-only drill: no measurement to re-derive
	}
	lw, e1 := time.Parse(time.RFC3339Nano, m.LastMarkerWrittenAt)
	lb, e2 := time.Parse(time.RFC3339Nano, m.LastMarkerInBackupAt)
	da, e3 := time.Parse(time.RFC3339Nano, m.DisasterAt)
	ra, e4 := time.Parse(time.RFC3339Nano, m.RecoveredAt)
	if e1 != nil || e2 != nil || e3 != nil || e4 != nil {
		return false
	}
	const eps = 1e-6
	return math.Abs(dr.ComputeRPO(lw, lb)-m.RPOSeconds) <= eps && math.Abs(dr.ComputeRTO(da, ra)-m.RTOSeconds) <= eps
}

// selfHost02BackupParts is the declared canonical install-backup identity the 0.2.0 DR-002 BackupProof records
// its manifest_digest over — hashParts(kind, migration, org ids, sample response). Migration is "34" (the T1
// journal + contract head), the only field that moved from the self-host-0.1.0 identity. The anchor recomputes
// this and fails if the committed manifest_digest does not reproduce (SF-7 — the E14 self-host anchor pinned
// only 0.1.0's; the 0.2.0 digest was unrecomputed).
var selfHost02BackupParts = []string{
	"palai-install-backup", "34", "org_local", "org_self_host_journey", "resp_self_host_journey",
}

// sh2Case is the subset of the manifest a case carries for the anchored re-derivation.
type sh2Case struct {
	ID       string `json:"id"`
	RunID    string `json:"run_id"`
	Checksum string `json:"checksum"`
	Terminal struct {
		Type string `json:"type"`
	} `json:"terminal"`
	InstallClaim string `json:"install_claim"`
	InstallProof *struct {
		StepIDs       []string `json:"step_ids"`
		JourneyDigest string   `json:"journey_digest"`
	} `json:"install_proof"`
	BackupClaim string `json:"backup_claim"`
	BackupProof *struct {
		ManifestDigest string `json:"manifest_digest"`
	} `json:"backup_proof"`
	RestoreVerifyClaim string          `json:"restore_verify_claim"`
	RestoreVerifyProof json.RawMessage `json:"restore_verify_proof"`
	UpgradeClaim       string          `json:"upgrade_claim"`
	UpgradeProof       *struct {
		ContinuityEventIDs    []string `json:"continuity_event_ids"`
		EventContinuityDigest string   `json:"event_continuity_digest"`
		StepIDs               []string `json:"step_ids"`
		JourneyDigest         string   `json:"journey_digest"`
	} `json:"upgrade_proof"`
	MigrationJournalClaim string          `json:"migration_journal_claim"`
	MigrationJournalProof json.RawMessage `json:"migration_journal_proof"`
	DrillClaim            string          `json:"drill_claim"`
	DrillProof            *struct {
		Measure *dr.Measure `json:"measure"`
	} `json:"drill_proof"`
	AirgapClaim     string          `json:"airgap_claim"`
	AirgapProof     json.RawMessage `json:"airgap_proof"`
	HelmRenderClaim string          `json:"helm_render_claim"`
	HelmRenderProof *struct {
		PolicyAsserts []string `json:"policy_asserts"`
		AssertsDigest string   `json:"asserts_digest"`
	} `json:"helm_render_proof"`
}

// TestSH2ReleaseVerifiesClean wires the self-host-0.2.0 RC bundle into the shared evidence verifier: it must
// verify clean (0 failed, 0 missing, 0 secret) with every E15 rule ACTIVE on real data — an active run survived
// an N->N+1 upgrade with both rollbacks draining it (upgrade), a migration chain resumed after interruption
// (migration-journal), DR drills produced measured RPO/RTO (drill), a signed air-gap bundle re-verified offline
// (airgap), and the restricted Helm chart rendered with no ClusterRole (helm-render) — AND the E14 self-host
// track (install/backup/restore-verify) carried forward, because 0.2.0 EXTENDS that track. It fails (bundle
// absent) until the bundle is committed.
func TestSH2ReleaseVerifiesClean(t *testing.T) {
	root := repoRoot(t)
	dir := filepath.Join(root, "evidence", "releases", "self-host-0.2.0")

	summary, err := uat.VerifyRelease(dir, nil)
	if err != nil {
		t.Fatalf("verify self-host-0.2.0: %v", err)
	}
	if !summary.OK() {
		t.Fatalf("self-host-0.2.0 did not verify clean: %s\n%v", summary.String(), summary.Findings)
	}
	if summary.Passed == 0 {
		t.Fatalf("self-host-0.2.0 verified zero cases (%s)", summary.String())
	}

	raw, err := os.ReadFile(filepath.Join(dir, "manifest.json"))
	if err != nil {
		t.Fatalf("read self-host-0.2.0 manifest: %v", err)
	}
	var parsed struct {
		Maturity string    `json:"maturity"`
		Cases    []sh2Case `json:"cases"`
	}
	if err := json.Unmarshal(raw, &parsed); err != nil {
		t.Fatalf("decode self-host-0.2.0 manifest: %v", err)
	}

	// The 0.2.0 bundle EXTENDS the self-host track and is a release CANDIDATE, not a stable claim.
	if parsed.Maturity != "rc" {
		t.Fatalf("self-host-0.2.0 maturity = %q, want \"rc\" (an RC bundle, not a stable sign-off)", parsed.Maturity)
	}

	// The per-case checksum anchor for this release: hashParts(id, run_id, canonical upgrade journey digest).
	upgradeJourneyDigest := hashParts(uat.UpgradeStepIDs...)

	var install, backup, restoreVerify, upgrade, migration, drill, airgap, helm int
	for _, c := range parsed.Cases {
		// Per-case checksum is re-derivable: a checksum that does not reproduce from the case's own id + run +
		// the canonical spine digest is fabricated.
		if want := hashParts(c.ID, c.RunID, upgradeJourneyDigest); c.Checksum != want {
			t.Fatalf("%s checksum %q is not hashParts(id, run_id, upgrade journey digest) %q — an authored checksum must be re-derivable", c.ID, c.Checksum, want)
		}
		if c.InstallClaim != "" && c.InstallProof != nil {
			install++
			// The install spine is still anchored to the E14 canonical spine (0.2.0 carries it forward).
			if !slices.Equal(c.InstallProof.StepIDs, uat.SelfHostStepIDs) || c.InstallProof.JourneyDigest != hashParts(uat.SelfHostStepIDs...) {
				t.Fatalf("%s install_proof is not anchored to the canonical self-host spine", c.ID)
			}
		}
		if c.BackupClaim != "" && c.BackupProof != nil {
			backup++
			// SF-7: the backup manifest_digest must be hashParts of the declared canonical backup identity —
			// a committed digest that does not reproduce is fabricated (the E14 self-host backup anchor, now
			// pinned for 0.2.0's migration-34 identity too).
			if want := hashParts(selfHost02BackupParts...); c.BackupProof.ManifestDigest != want {
				t.Fatalf("%s backup manifest_digest %q is not hashParts(canonical backup identity) %q", c.ID, c.BackupProof.ManifestDigest, want)
			}
		}
		if c.RestoreVerifyClaim != "" && len(c.RestoreVerifyProof) > 0 {
			restoreVerify++
		}
		if c.UpgradeClaim != "" && c.UpgradeProof != nil {
			upgrade++
			// ANCHORED: the step_ids must be the canonical upgrade spine and the journey_digest hashParts of it.
			if !upgradeSpineAnchored(c.UpgradeProof.StepIDs, c.UpgradeProof.JourneyDigest) {
				t.Fatalf("%s upgrade_proof is not anchored to the canonical upgrade spine: step_ids=%v digest=%q, want %v / %q",
					c.ID, c.UpgradeProof.StepIDs, c.UpgradeProof.JourneyDigest, uat.UpgradeStepIDs, upgradeJourneyDigest)
			}
			// The event-continuity digest must be hashParts of the case's own ordered event list — a fabricated
			// digest over an unstated event stream is caught.
			if want := hashParts(c.UpgradeProof.ContinuityEventIDs...); c.UpgradeProof.EventContinuityDigest != want {
				t.Fatalf("%s event_continuity_digest %q is not hashParts(continuity_event_ids) %q — a fabricated continuity digest", c.ID, c.UpgradeProof.EventContinuityDigest, want)
			}
			// MF-2a: canon the endpoints against the run's own lifecycle (the digest alone is self-referential).
			// The gaplessness across the recreate is proven live by the journey's DB check; here we pin the shape.
			if !continuityAnchored(c.UpgradeProof.ContinuityEventIDs, c.Terminal.Type) {
				t.Fatalf("%s continuity_event_ids %v are not a real run stream: must start at response.created and end at the case terminal %q", c.ID, c.UpgradeProof.ContinuityEventIDs, c.Terminal.Type)
			}
		}
		if c.MigrationJournalClaim != "" && len(c.MigrationJournalProof) > 0 {
			migration++
		}
		if c.DrillClaim != "" && c.DrillProof != nil {
			drill++
			// THE MEASUREMENT ANCHOR: a timed drill's RPO/RTO must be DERIVABLE from its raw timestamps.
			if !measureDerivable(c.DrillProof.Measure) {
				t.Fatalf("%s drill_proof measurement is not derivable from its raw timestamps — a fabricated RPO/RTO", c.ID)
			}
		}
		if c.AirgapClaim != "" && len(c.AirgapProof) > 0 {
			airgap++
		}
		if c.HelmRenderClaim != "" && c.HelmRenderProof != nil {
			helm++
			// ANCHORED: the policy asserts must be the canonical restricted set and the digest hashParts of it.
			if !helmAssertsAnchored(c.HelmRenderProof.PolicyAsserts, c.HelmRenderProof.AssertsDigest) {
				t.Fatalf("%s helm_render_proof is not anchored to the canonical policy asserts: asserts=%v digest=%q",
					c.ID, c.HelmRenderProof.PolicyAsserts, c.HelmRenderProof.AssertsDigest)
			}
		}
	}
	// The RC bundle must exercise EVERY E15 rule AND carry the E14 track forward — a bundle missing any rule
	// would silently not test that invariant (the self-host all-rules-exercised loop, extended).
	if install == 0 || backup == 0 || restoreVerify == 0 || upgrade == 0 || migration == 0 || drill == 0 || airgap == 0 || helm == 0 {
		t.Fatalf("self-host-0.2.0 does not exercise all SH-2 rules: install=%d backup=%d restore_verify=%d upgrade=%d migration=%d drill=%d airgap=%d helm=%d",
			install, backup, restoreVerify, upgrade, migration, drill, airgap, helm)
	}
}

// TestSH2AnchorsRejectFabrication pins the anti-fabrication anchors: each rejects a self-consistent-but-invented
// value the shape-checked verifier would pass. Without these, invented step ids / dropped policy asserts / a
// hand-edited RPO with a matching-looking digest would verify green while claiming a bogus fact (the
// managed-cloud/self-host MUST-FIX-1 precedent, extended to the measurement).
func TestSH2AnchorsRejectFabrication(t *testing.T) {
	// The canonical upgrade spine + its real digest anchors; an invented list with a SELF-CONSISTENT digest does not.
	if !upgradeSpineAnchored(uat.UpgradeStepIDs, hashParts(uat.UpgradeStepIDs...)) {
		t.Fatal("the canonical upgrade spine must anchor")
	}
	fabSteps := []string{"s1", "s2", "s3", "s4", "s5", "s6", "s7", "s8", "s9", "s10", "s11", "s12", "s13"}
	if upgradeSpineAnchored(fabSteps, hashParts(fabSteps...)) {
		t.Fatal("a fabricated upgrade step list with a self-consistent digest must NOT anchor (the fabrication hole)")
	}

	// The canonical helm asserts anchor; a DROPPED assert (e.g. removing no-cluster-role) with a self-consistent
	// digest does not — the whole point is that silently loosening the chart cannot keep a matching digest.
	if !helmAssertsAnchored(uat.HelmPolicyAsserts, hashParts(uat.HelmPolicyAsserts...)) {
		t.Fatal("the canonical helm policy asserts must anchor")
	}
	dropped := slices.Clone(uat.HelmPolicyAsserts)[1:] // drop no-cluster-role
	if helmAssertsAnchored(dropped, hashParts(dropped...)) {
		t.Fatal("a dropped-assert list with a self-consistent digest must NOT anchor")
	}

	// MF-2a: the event-continuity endpoints canon a real run stream — an arbitrary list (even with a
	// self-consistent digest) must NOT anchor; only created→…→terminal does.
	if !continuityAnchored([]string{"response.created", "response.in_progress", "response.completed"}, "response.completed") {
		t.Fatal("a real created→…→completed stream must anchor")
	}
	if continuityAnchored([]string{"foo", "bar", "baz"}, "response.completed") {
		t.Fatal("an arbitrary event list must NOT anchor (the self-referential-digest hole)")
	}
	if continuityAnchored([]string{"response.created", "response.failed"}, "response.completed") {
		t.Fatal("a stream not ending at the case terminal must NOT anchor")
	}

	// THE MEASUREMENT ANCHOR: a hand-edited RPO the raw timestamps do not support must be rejected.
	good := &dr.Measure{
		LastMarkerWrittenAt: "2026-07-24T02:00:05Z", LastMarkerInBackupAt: "2026-07-24T02:00:03.5Z", RPOSeconds: 1.5,
		DisasterAt: "2026-07-24T02:00:10Z", RecoveredAt: "2026-07-24T02:00:47.25Z", RTOSeconds: 37.25,
	}
	if !measureDerivable(good) {
		t.Fatal("a measurement whose seconds match its raw timestamps must be derivable")
	}
	bad := *good
	bad.RPOSeconds = 0.1 // fabricated; the timestamps recompute to 1.5s
	if measureDerivable(&bad) {
		t.Fatal("a fabricated rpo_seconds the raw timestamps do not reproduce must NOT be derivable (the measurement anti-fabrication anchor)")
	}
}

// repoRoot walks up to the module root (the dir holding go.mod).
func repoRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("go.mod not found")
		}
		dir = parent
	}
}
