//go:build component

package execution

import (
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/palgroup/palai/apps/control-plane/internal/extensions"
)

// TWO CONTRACT DEFECTS ON THE TOOL REGISTRY'S WRITE SURFACE, each measured against the SHIPPED router and a
// real Postgres. Both are answers the API gives that are not true: one blames the server for an operator's
// typo, the other hands out an address nothing serves.

// TestAToolRevisionTimeoutAboveTheCeilingIsRefusedAsBadInput pins the status an out-of-range timeout_ms
// gets. extensions.DecodeToolRevisionInput has enforced MaxTimeoutMS since E12 T4 and returns the typed
// ErrTimeoutTooLarge — but store.toolReject never named it, so it fell past every mapped arm to the
// default and the handler rendered a bare error as 500.
//
// THE DIFFERENCE IS NOT COSMETIC. A 500 says "the server broke, retry"; the operator's value is the one
// thing that will never work no matter how often it is retried, and the ceiling that rejected it is not in
// the response either way. Every SIBLING validation on the same decode path — an unknown field, a bad
// replay_class, an over-long approval label, a widening override — already answered 400.
//
// WAS RED:
//
//	=== RUN   TestAToolRevisionTimeoutAboveTheCeilingIsRefusedAsBadInput
//	    tool_contract_component_test.go:60: POST /v1/tools/tool_.../revisions = 500, want 400
//	--- FAIL: TestAToolRevisionTimeoutAboveTheCeilingIsRefusedAsBadInput
func TestAToolRevisionTimeoutAboveTheCeilingIsRefusedAsBadInput(t *testing.T) {
	cs, tenant, _ := openPinnedSpine(t)
	repo := approverHTTP(t)
	caller := mintScopedKey(t, repo, cs, tenant, []string{"provision"})
	srv := httptest.NewServer(runbookRouter(repo))
	t.Cleanup(srv.Close)
	client := &consoleClient{t: t, base: srv.URL, token: caller.token}

	tool := client.post(201, "/v1/tools", map[string]any{"canonical_name": "acme.contracts.ceiling"})
	toolID, _ := tool["id"].(string)
	if toolID == "" {
		t.Fatalf("POST /v1/tools returned no id: %v", tool)
	}
	revisions := "/v1/tools/" + toolID + "/revisions"

	// The operator's mistake: a timeout past the ceiling. It is THEIR input that is wrong, and the answer
	// has to say so.
	client.post(400, revisions, map[string]any{
		"executor": "remote_http", "replay_class": "idempotent", "timeout_ms": 400000,
	})

	// THE CEILING ITSELF IS ACCEPTED, and this leg is what keeps the one above from passing vacuously: a
	// route that answered 400 to every revision body would satisfy the refusal and prove nothing.
	client.post(201, revisions, map[string]any{
		"executor": "remote_http", "replay_class": "idempotent", "timeout_ms": extensions.MaxTimeoutMS,
	})

	// The boundary is exactly MaxTimeoutMS — one millisecond over is already too much, so the comparison is
	// `>` and not `>=`. Asserted rather than assumed, because an off-by-one here silently narrows the
	// contract for every caller sitting on the documented maximum.
	client.post(400, revisions, map[string]any{
		"executor": "remote_http", "replay_class": "idempotent", "timeout_ms": extensions.MaxTimeoutMS + 1,
	})
}

// TestEveryToolRegistryCreateAdvertisesAnAddressThatResolves follows the Location header of all three
// registry creates instead of reading it. A 201's Location is the created resource's identity (RFC 9110
// §10.2.2), and a client that follows one is doing what the header exists for.
//
// TWO OF THE THREE NAMED A ROUTE THIS TREE HAS NEVER MOUNTED. `POST /v1/tools/{tool_id}/revisions`
// answered `/v1/tool-revisions/<id>` and `POST /v1/tool-sets/{set}/revisions` answered
// `/v1/tool-set-revisions/<id>`; neither prefix appears in router.go in any epic. The set's resource WAS
// readable the whole time — at `/v1/tool-sets/{set}/revisions/{revision_id}`, mounted by E25 T7 — so that
// header was pointing away from a live route rather than at a missing one.
//
// WAS RED:
//
//	=== RUN   TestEveryToolRegistryCreateAdvertisesAnAddressThatResolves
//	    tool_contract_component_test.go:118: POST /v1/tools/{tool_id}/revisions advertised
//	        Location: /v1/tool-revisions/toolrev_... — following it: GET /v1/tool-revisions/toolrev_... = 404
//	--- FAIL: TestEveryToolRegistryCreateAdvertisesAnAddressThatResolves
func TestEveryToolRegistryCreateAdvertisesAnAddressThatResolves(t *testing.T) {
	cs, tenant, _ := openPinnedSpine(t)
	repo := approverHTTP(t)
	caller := mintScopedKey(t, repo, cs, tenant, []string{"provision"})
	srv := httptest.NewServer(runbookRouter(repo))
	t.Cleanup(srv.Close)
	client := &consoleClient{t: t, base: srv.URL, token: caller.token}

	// (1) The lineage. This one has always resolved — GET /v1/tools/{tool_id} ships since E13 T4 — and it
	// is here as the CONTROL: it is what the two below are measured against.
	tool := client.post(201, "/v1/tools", map[string]any{"canonical_name": "acme.contracts.located"})
	toolID, _ := tool["id"].(string)
	followed, _ := followLocation(t, client, "POST /v1/tools")
	if followed["id"] != toolID {
		t.Fatalf("POST /v1/tools — following Location returned id %v, want the created %s", followed["id"], toolID)
	}

	// (2) The tool revision.
	rev := client.post(201, "/v1/tools/"+toolID+"/revisions", map[string]any{
		"executor": "remote_http", "replay_class": "idempotent",
		"description":  "a revision whose Location must resolve",
		"input_schema": map[string]any{"type": "object"},
	})
	revID, _ := rev["id"].(string)
	followedRev, _ := followLocation(t, client, "POST /v1/tools/{tool_id}/revisions")
	if followedRev["id"] != revID {
		t.Fatalf("POST /v1/tools/{tool_id}/revisions — following Location returned id %v, want the created %s", followedRev["id"], revID)
	}
	// WHAT THE FOLLOWED ADDRESS SERVES IS THE REVISION, not a near-miss that happens to answer 200: the
	// tool it belongs to and the untrusted description an admin approves both have to be on it.
	if followedRev["tool_id"] != toolID {
		t.Fatalf("the followed revision names tool_id=%v, want %s", followedRev["tool_id"], toolID)
	}
	if followedRev["description"] != "a revision whose Location must resolve" {
		t.Fatalf("the followed revision does not carry the description being approved: %v", followedRev["description"])
	}

	// (3) The set revision. It needs a PUBLISHED pin, so the revision above is published first.
	client.post(200, "/v1/tools/"+toolID+"/revisions/"+revID+"/publish", map[string]any{})
	set := client.post(201, "/v1/tool-sets/located/revisions", map[string]any{
		"tools": []any{map[string]any{"tool_revision_id": revID}},
	})
	setID, _ := set["id"].(string)
	followedSet, setLoc := followLocation(t, client, "POST /v1/tool-sets/{set}/revisions")
	if followedSet["id"] != setID {
		t.Fatalf("POST /v1/tool-sets/{set}/revisions — following Location returned id %v, want the created %s", followedSet["id"], setID)
	}
	// The set's Location must name the set it was created under, or it would resolve to a route that is
	// mounted but decorative — the same segment-is-part-of-the-identity property E25 T7 pinned.
	if !strings.HasPrefix(setLoc, "/v1/tool-sets/located/revisions/") {
		t.Fatalf("the set revision's Location does not name its set: %s", setLoc)
	}
	// And its pins are on the followed address, which is the field the LIST projection does not carry.
	if pins, _ := followedSet["tools"].([]any); len(pins) != 1 {
		t.Fatalf("the followed set revision carries %v pin(s), want the one that was pinned", followedSet["tools"])
	}
}

// followLocation takes the Location the previous create advertised and GETs it. An empty header, or one
// that does not resolve, fails naming the create that emitted it — the two things a client following the
// header would hit.
func followLocation(t *testing.T, c *consoleClient, create string) (map[string]any, string) {
	t.Helper()
	// Read it BEFORE the GET, which overwrites c.location with its own (absent) header.
	loc := c.location
	if loc == "" {
		t.Fatalf("%s emitted no Location header", create)
	}
	if !strings.HasPrefix(loc, "/v1/") {
		t.Fatalf("%s advertised a Location that is not a /v1 path: %q", create, loc)
	}
	return c.get(200, loc), loc
}
