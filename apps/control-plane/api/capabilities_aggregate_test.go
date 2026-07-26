package api

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/palgroup/palai/tests/uat"
)

// The shipped end of the E18 T10 anti-fabrication anchor (plan §T10). Its E17 sibling in
// capabilities_tier_test.go asserts the served map equals the recompute over ONE epic's bundle; this asserts
// it equals the recompute over EVERY committed bundle's claim outcomes, which is the cross-epic form the exit
// sentence names — and it is what breaks the circularity in the RC bundle. release-1.0.0-rc1's
// AggregateTierProof carries a `snapshot`; without this test that snapshot would be a map the generator
// derived from the same recompute it is compared against, i.e. a number checking itself. Here the snapshot is
// compared to what a REAL router really serves over real HTTP.
//
// EXT-1, ACKNOWLEDGED AT THE ASSERT: the router under test is fullyMountedRouter() — A2A, the capability-
// worker gateway and the Slack receiver all mounted. NO shipped deployment config sets
// PALAI_CAPABILITY_WORKER_LISTEN_ADDR, so no deployed binary serves this exact map, and the RC bundle's proof
// says so in its own `served_by_deployed_config: false` + `unmounted_reason` fields (which
// uat.AggregateTierProof.Complete() requires). This test does not, and must not, be read as evidence that a
// deployment serves it.

// TestServedCapabilityTiersEqualTheAggregateRecompute is the cross-epic bit-equality assert.
func TestServedCapabilityTiersEqualTheAggregateRecompute(t *testing.T) {
	want, err := uat.AggregateCapabilityTiers()
	if err != nil {
		t.Fatalf("recompute the product-wide capability posture from the committed bundles: %v", err)
	}
	got := servedCapabilities(t, fullyMountedRouter())

	for _, capability := range uat.CapabilityTierOrder {
		served, advertised := got[capability]
		if !advertised {
			t.Errorf("capability %q is GOVERNED by the product-wide posture but /v1/capabilities does not advertise it", capability)
			continue
		}
		if served != want[capability] {
			t.Errorf("capability %q: /v1/capabilities serves %q but the recompute over EVERY committed bundle's claim outcomes is %q — a fabricated cross-epic tier is a FAIL (plan §T10)",
				capability, served, want[capability])
		}
	}
}

// TestRCBundleSnapshotIsWhatTheRouterServes is the assert that makes release-1.0.0-rc1's
// AggregateTierProof.Snapshot evidence rather than an echo: the map committed in the bundle must be BYTE-FOR-
// BYTE what the fully-mounted router returns. Flip one tier in the bundle and this fails; flip one in
// capabilities.go and this fails.
func TestRCBundleSnapshotIsWhatTheRouterServes(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join(repoRootFromTest(t), "evidence", "releases", uat.StableReleaseBundle, "manifest.json"))
	if err != nil {
		t.Fatalf("read the %s bundle: %v", uat.StableReleaseBundle, err)
	}
	var m struct {
		Cases []struct {
			ID                 string `json:"id"`
			AggregateTierClaim string `json:"aggregate_tier_claim"`
			AggregateTierProof *struct {
				Snapshot               map[string]string `json:"snapshot"`
				ServedByDeployedConfig bool              `json:"served_by_deployed_config"`
			} `json:"aggregate_tier_proof"`
		} `json:"cases"`
	}
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("decode the bundle: %v", err)
	}

	served := servedCapabilities(t, fullyMountedRouter())
	checked := 0
	for _, c := range m.Cases {
		if c.AggregateTierClaim == "" || c.AggregateTierProof == nil {
			continue
		}
		checked++
		if c.AggregateTierProof.ServedByDeployedConfig {
			t.Errorf("%s claims a DEPLOYED config serves this map — no shipped deployment config sets PALAI_CAPABILITY_WORKER_LISTEN_ADDR (EXT-1)", c.ID)
		}
		for capability, tier := range c.AggregateTierProof.Snapshot {
			if got := served[capability]; got != tier {
				t.Errorf("%s: the bundle's snapshot says %q is %q but the fully-mounted router serves %q — the snapshot must be what the router REALLY returned",
					c.ID, capability, tier, got)
			}
		}
		for _, capability := range uat.CapabilityTierOrder {
			if _, ok := c.AggregateTierProof.Snapshot[capability]; !ok {
				t.Errorf("%s: the snapshot omits governed capability %q", c.ID, capability)
			}
		}
	}
	if checked == 0 {
		t.Fatalf("%s carries no aggregate_tier_proof — there is no snapshot to check against the router", uat.StableReleaseBundle)
	}
}
