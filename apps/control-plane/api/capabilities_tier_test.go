package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"testing"

	"github.com/palgroup/palai/adapters/integrations/a2a"
	"github.com/palgroup/palai/tests/uat"
)

// This is the shipped end of the E17 T11 anti-fabrication anchor (plan §T11, §2): a capability's maturity tier
// is a FUNCTION of its UAT claim outcomes, and the SERVED /v1/capabilities map must be BIT-EQUAL to that
// function. The evidence verifier owns the function (uat.RecomputeCapabilityTiers over the canonical code
// tables); this test drives the REAL router over real HTTP and asserts discovery agrees with it.
//
// Which means: nobody edits a tier in capabilities.go and gets away with it. Raising `slack` to "stable" here
// fails this test (its §6 leg 1 caps it at preview), and lowering `knowledge` fails it too (all eight KNO
// claims are green in the bundle). The flip happens by EARNING it in the evidence, never by typing it.
//
// The dependency direction is deliberate: production code imports nothing new — only this test reads the
// verifier's table, so the shipped surface is checked against the gate rather than the gate against the
// surface.

// servedCapabilities fetches GET /v1/capabilities from the real router under a verified bearer and returns the
// whole capability map.
func servedCapabilities(t *testing.T, router http.Handler) map[string]string {
	t.Helper()
	ts := httptest.NewServer(router)
	defer ts.Close()
	req, _ := http.NewRequest(http.MethodGet, ts.URL+"/v1/capabilities", nil)
	req.Header.Set("Authorization", "Bearer any")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET /v1/capabilities: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("capabilities = %d, want 200", resp.StatusCode)
	}
	var body struct {
		Capabilities map[string]string `json:"capabilities"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode capabilities: %v", err)
	}
	return body.Capabilities
}

// repoRootFromTest walks up to the module root so the test can read the committed evidence bundle.
func repoRootFromTest(t *testing.T) string {
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

// fullyMountedRouter builds the router with EVERY optional surface a governed capability derives from
// mounted — A2A (E17 T2) and the capability-worker gateway (E17 T9, E19 T8a) — so the served map contains
// every governed capability and the bit-equality assert below has something to compare for each.
//
// This is the honest reconciliation of mount-gated discovery with the tier recompute, and the two halves are
// tested separately so neither can hide the other: TestA2ACapabilityAdvertisedOnlyWhenMounted and
// TestCapabilityWorkersAdvertisedOnlyWhenMounted prove the capability DISAPPEARS when its surface is not
// mounted, and this file proves that when everything IS mounted the tiers equal the recompute exactly. What
// stays forbidden is the shortcut: exempting a capability from the assert because a router did not mount it.
func fullyMountedRouter() http.Handler {
	srv := &a2a.Server{Interfaces: stubIfaceStore{iface: a2a.PublishedInterface{ID: "if_tier"}}, ScopeFunc: a2aScopeFunc}
	return NewRouter(fakeVerifier{}, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil,
		SSEConfig{}, nil, nil, WithA2A(srv, srv.PublicCardHandler()), WithCapabilityWorkers())
}

// TestServedCapabilityTiersEqualTheRecompute is the bit-equality assert. It builds the router with every
// optional surface MOUNTED (so every governed capability is advertised), serves the real discovery body, and
// compares it to uat.RecomputeCapabilityTiers over the committed extensions-0.1.0 bundle's per-case outcomes.
func TestServedCapabilityTiersEqualTheRecompute(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join(repoRootFromTest(t), "evidence", "releases", "extensions-0.1.0", "manifest.json"))
	if err != nil {
		t.Fatalf("read the extensions-0.1.0 bundle (the canonical source of the tier recompute): %v", err)
	}
	want, err := uat.CapabilityTiersFromBundle(raw)
	if err != nil {
		t.Fatalf("recompute the tier table: %v", err)
	}

	got := servedCapabilities(t, fullyMountedRouter())

	for _, capability := range uat.CapabilityTierOrder {
		served, advertised := got[capability]
		if !advertised {
			t.Errorf("capability %q is GOVERNED by the E17 tier table but /v1/capabilities does not advertise it — discovery must publish every governed capability so the recompute has something to compare", capability)
			continue
		}
		if served != want[capability] {
			t.Errorf("capability %q: /v1/capabilities serves %q but the verifier's recompute over the extensions-0.1.0 per-case outcomes is %q — discovery must be BIT-EQUAL to the recomputed tier table; earn the tier in the evidence, never type it here (plan §2, §T11)",
				capability, served, want[capability])
		}
	}
}

// TestUngovernedCapabilitiesAreNotSilentlyGoverned guards the other direction: a capability advertised with a
// maturity word the tier table does not govern is a tier nobody recomputes. `responses`/`sessions` predate E17
// and `workspaces` is derived from deployment configuration at request time, so those three are the ONLY
// legitimate exemptions — a new E17-era capability must join uat.CapabilityTierOrder (and own claims) rather
// than appear here with a hand-written tier.
func TestUngovernedCapabilitiesAreNotSilentlyGoverned(t *testing.T) {
	exempt := map[string]bool{"responses": true, "sessions": true, "workspaces": true}
	governed := map[string]bool{}
	for _, c := range uat.CapabilityTierOrder {
		governed[c] = true
	}

	var ungoverned []string
	for name := range servedCapabilities(t, fullyMountedRouter()) {
		if !governed[name] && !exempt[name] {
			ungoverned = append(ungoverned, name)
		}
	}
	sort.Strings(ungoverned)
	if len(ungoverned) > 0 {
		t.Errorf("capabilities %v are advertised with a tier NOTHING recomputes — add them to uat.CapabilityTierOrder + uat.CapabilityClaims so the exit gate governs them (plan §2: a tier is a function, not a declaration)", ungoverned)
	}
}
