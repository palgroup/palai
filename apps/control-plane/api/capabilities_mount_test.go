package api

import (
	"context"
	"net/http"
	"sort"
	"testing"

	"github.com/palgroup/palai/apps/control-plane/api/middleware"
	"github.com/palgroup/palai/tests/uat"
)

// This is the MECHANICAL form of the §2 discovery invariant (plan §3.5 row D14): discovery advertises what
// this binary can SERVE, and nothing else. The per-capability tests beside it
// (TestA2ACapabilityAdvertisedOnlyWhenMounted, TestCapabilityWorkersAdvertisedOnlyWhenMounted,
// TestSlackCapabilityAdvertisedOnlyWhenMounted, TestQueuesCapabilityAdvertisedOnlyWhenMounted,
// TestKnowledgeCapabilityAdvertisedOnlyWhenMounted) each pin ONE capability; this one sweeps the canonical
// uat.CapabilityTierOrder, so a capability added to the tier table with a hand-typed string in capabilities.go
// fails here without anybody remembering to write a sixth test.
//
// The rule it enforces: a router that mounts NOTHING may advertise a governed capability only when the
// advertised word claims no servable surface, or when the capability has no mount to derive from and the
// exemption is written down below. That is what "static string" meant in D14 — `"capability-workers": "stable"`
// served by a binary that did not so much as import internal/workers.

// mountExemptCapabilities names every governed capability discovery advertises WITHOUT a mount to derive from,
// with the reason the advertisement is not a §2 lie. Adding a name here is the auditable act; the DEFAULT is
// that a governed capability disappears from discovery when its surface is not mounted.
var mountExemptCapabilities = map[string]string{
	// A "disabled" tier is a NEGATIVE claim: it tells a client this deployment does NOT serve the surface, so
	// there is no surface to derive it from and nothing for a caller to be misled into calling. `apple-build`
	// has no signing material anywhere (§6 leg 3, WRK-006 proves it is ABSENT from the worker catalog) and
	// `knowledge-vector` has no vector store wired (§6 leg 4) — both are honest exactly BECAUSE they are static.
	"apple-build":      `"disabled" is a negative claim; no surface exists to derive it from (§6 leg 3)`,
	"knowledge-vector": `"disabled" is a negative claim; no vector store is wired to derive it from (§6 leg 4)`,
	// `console` is the one exemption that is NOT a negative claim, and it is stated rather than rounded off:
	// apps/web-console is a SEPARATE deployable that talks to /v1 as a client — this binary never serves it, so
	// there is no mount in this process for discovery to observe. The claim is therefore about the PRODUCT
	// (the tier table governs it and the E17 T11 / E18 T10 recomputes write the word), not about a route on
	// this router. If the console is ever served from this process, this line stops being true and the
	// derivation must follow the mount like everything else.
	"console": `apps/web-console is a separate deployable; this binary serves no console route to derive it from`,
}

// bareRouter is a router with NO optional surface mounted — the shape a caller of the exported NewRouter gets
// when it wires nothing. Every governed capability that is mount-gated must be ABSENT from its discovery body.
func bareRouter() http.Handler {
	return NewRouter(fakeVerifier{}, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, SSEConfig{}, nil, nil)
}

// TestBareRouterAdvertisesOnlyWhatItCanServe is the sweep. It fails for any governed capability a
// mounts-nothing router still names, unless that capability is in mountExemptCapabilities with a reason.
func TestBareRouterAdvertisesOnlyWhatItCanServe(t *testing.T) {
	got := servedCapabilities(t, bareRouter())

	var lies []string
	for _, capability := range uat.CapabilityTierOrder {
		tier, advertised := got[capability]
		if !advertised || mountExemptCapabilities[capability] != "" {
			continue
		}
		lies = append(lies, capability+"="+tier)
	}
	sort.Strings(lies)
	if len(lies) > 0 {
		t.Errorf("a router that mounts NOTHING advertises %v — every one of those surfaces 404s on this binary. "+
			"Derive the claim from the mount (the `cfg.a2a != nil` / workspacesCapability pattern in capabilities.go), "+
			"or add the capability to mountExemptCapabilities with the reason it has no mount to derive from (plan §2, §3.5 D14)", lies)
	}
}

// TestMountExemptionsAreGoverned keeps the exemption list from rotting into a place to hide a capability: a
// name here must be a capability the tier table actually governs, so a typo or a stale entry is a failure
// rather than a silent widening of the rule.
func TestMountExemptionsAreGoverned(t *testing.T) {
	governed := map[string]bool{}
	for _, c := range uat.CapabilityTierOrder {
		governed[c] = true
	}
	for capability := range mountExemptCapabilities {
		if !governed[capability] {
			t.Errorf("mountExemptCapabilities names %q, which uat.CapabilityTierOrder does not govern — an exemption for a capability nobody recomputes is a hole, not a decision", capability)
		}
	}
}

// TestKnowledgeCapabilityAdvertisedOnlyWhenMounted pins the HEAVIEST of the two static strings E19 T8 found
// still standing after T8a: `knowledge` was advertised "stable" — the strongest word the surface can express —
// by any router built without WithKnowledge, on which every /v1/knowledge-bases route 404s. That is the exact
// shape of plan §3.5 D14's `"capability-workers": "stable"`, one epic later and one seam over.
//
// Production mounts the spine unconditionally (main.go), so no DEPLOYMENT ever served the lie; the exported
// NewRouter did, and §2 is a property of the binary's discovery body, not of one wiring of it.
func TestKnowledgeCapabilityAdvertisedOnlyWhenMounted(t *testing.T) {
	if got := capabilityValue(t, bareRouter(), "knowledge"); got != "" {
		t.Fatalf("without a mounted knowledge spine, knowledge = %q, want absent — a binary whose /v1/knowledge-bases routes 404 must not claim the capability at ANY tier, least of all stable (plan §2, §3.5 D14)", got)
	}

	mounted := NewRouter(fakeVerifier{}, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, SSEConfig{}, nil, nil,
		WithKnowledge(stubKnowledge{}))
	want := recomputedTier(t, "knowledge")
	if got := capabilityValue(t, mounted, "knowledge"); got != want {
		t.Fatalf("with the knowledge spine mounted, knowledge = %q, want the recomputed tier %q — the mount makes the claim advertisABLE, the evidence decides the tier", got, want)
	}
}

// TestQueuesCapabilityAdvertisedOnlyWhenMounted is the queue half. E19 T6 is what gave this claim a mount to
// derive from at all (before it, `queues` named a store no production router exposed); the derivation is what
// stops the claim outliving that mount.
//
// The mounted half asserts the RECOMPUTED tier: `queues` stays preview because no broker PRODUCT exists in
// this tree (E17 §6 leg 5), and no amount of local wiring moves an operator leg.
func TestQueuesCapabilityAdvertisedOnlyWhenMounted(t *testing.T) {
	if got := capabilityValue(t, bareRouter(), "queues"); got != "" {
		t.Fatalf("without a mounted queue surface, queues = %q, want absent — a binary whose /v1/queue-connections 404s must not claim the capability (plan §2, §3.5 D14)", got)
	}

	mounted := NewRouter(fakeVerifier{}, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, SSEConfig{}, nil, nil,
		WithQueueConnections(&fakeQueueAPI{}, nil))
	want := recomputedTier(t, "queues")
	if got := capabilityValue(t, mounted, "queues"); got != want {
		t.Fatalf("with the queue surface mounted, queues = %q, want the recomputed tier %q — the mount makes the claim advertisABLE, the evidence decides the tier", got, want)
	}
	if want != "preview" {
		t.Fatalf("the recomputed queues tier is %q; E19 wiring must not advance it past preview (§6 leg 5 — a real broker PRODUCT — is an OPERATOR leg, not a code change)", want)
	}
}

// stubKnowledge is a KnowledgeAPI that answers nothing. The knowledge derivation is about whether the ROUTES
// are mounted, not about what they return, so the seam only has to be non-nil.
type stubKnowledge struct{}

func (stubKnowledge) CreateKnowledgeBase(context.Context, middleware.Scope, []byte) (ProvisionResult, error) {
	return ProvisionResult{}, nil
}
func (stubKnowledge) ListKnowledgeBases(context.Context, middleware.Scope) (ProvisionResult, error) {
	return ProvisionResult{}, nil
}
func (stubKnowledge) CreateSource(context.Context, middleware.Scope, string, []byte) (ProvisionResult, error) {
	return ProvisionResult{}, nil
}
func (stubKnowledge) ListSources(context.Context, middleware.Scope, string) (ProvisionResult, error) {
	return ProvisionResult{}, nil
}
func (stubKnowledge) DeleteSource(context.Context, middleware.Scope, string) (ProvisionResult, error) {
	return ProvisionResult{}, nil
}
func (stubKnowledge) Ingest(context.Context, middleware.Scope, string, string, []byte) (ProvisionResult, error) {
	return ProvisionResult{}, nil
}
func (stubKnowledge) Retrieve(context.Context, middleware.Scope, string, []byte) (ProvisionResult, error) {
	return ProvisionResult{}, nil
}
func (stubKnowledge) ListIndexRevisions(context.Context, middleware.Scope, string) (ProvisionResult, error) {
	return ProvisionResult{}, nil
}
