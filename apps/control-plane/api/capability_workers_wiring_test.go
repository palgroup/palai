package api

import (
	"testing"
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

}
