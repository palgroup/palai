package api

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/palgroup/palai/tests/uat"
)

// TestCapabilityWorkersAdvertisedOnlyWhenMounted is the capability-worker half of the §2 discovery invariant
// (E19 T8a, plan §3.5 row D14), and it mirrors TestA2ACapabilityAdvertisedOnlyWhenMounted exactly: a binary
// that serves NO capability-worker gateway must not advertise `capability-workers` at all.
//
// The defect it pins was the heaviest discovery lie in the tree: capabilities.go advertised
// "capability-workers": "stable" UNCONDITIONALLY while palai-control-plane did not even import
// internal/workers — the only production importer was the WORKER binary (the client side). Every deployment
// therefore claimed, at the strongest tier the surface can express, a contract it did not serve.
//
// The mounted half asserts the RECOMPUTED tier, never a literal: mounting a surface makes a capability
// advertisABLE, it never sets or raises its maturity (§2 — the T11 CapabilityTierProof recomputes the tier
// from the WRK claim outcomes).
func TestCapabilityWorkersAdvertisedOnlyWhenMounted(t *testing.T) {
	unmounted := NewRouter(fakeVerifier{}, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, SSEConfig{}, nil, nil)
	if got := capabilityValue(t, unmounted, "capability-workers"); got != "" {
		t.Fatalf("without a mounted capability-worker gateway, capability-workers = %q, want absent — a binary that serves no worker surface must not claim the capability (plan §2, §3.5 D14)", got)
	}

	mounted := NewRouter(fakeVerifier{}, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, SSEConfig{}, nil, nil,
		WithCapabilityWorkers())
	want := recomputedTier(t, "capability-workers")
	if got := capabilityValue(t, mounted, "capability-workers"); got != want {
		t.Fatalf("with the capability-worker gateway mounted, capability-workers = %q, want the recomputed tier %q — the mount makes the claim advertisABLE, the evidence decides the tier", got, want)
	}
}

// recomputedTier is the tier uat.RecomputeCapabilityTiers derives for one capability from the committed
// extensions-0.1.0 per-case outcomes — the same source TestServedCapabilityTiersEqualTheRecompute compares
// the whole map against, so this test cannot drift from the gate by hardcoding a maturity word.
func recomputedTier(t *testing.T, capability string) string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(repoRootFromTest(t), "evidence", "releases", "extensions-0.1.0", "manifest.json"))
	if err != nil {
		t.Fatalf("read the extensions-0.1.0 bundle (the canonical source of the tier recompute): %v", err)
	}
	tiers, err := uat.CapabilityTiersFromBundle(raw)
	if err != nil {
		t.Fatalf("recompute the tier table: %v", err)
	}
	return tiers[capability]
}
