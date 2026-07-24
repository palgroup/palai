//go:build component

// The E17 Task 5 ACL-first negative corpus (KNO-003, SECURITY-CRITICAL). It hardens the T4 spine's
// query-level ACL predicate into the T5 invariant: the principal's authorization is SERVER-DERIVED (from
// the verified key scopes — api_keys.scopes, migration 000030), never a request-body field, and the ACL
// predicate is in the QUERY (before ranking + LIMIT), never a post-fetch top-K filter. Two tenants and two
// projects with deliberate-leak fixtures prove the DB-level scoping rejects a cross-scope read and a forged
// grant. Runs only under `make test-component TEST=postgres`.
package knowledge_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/palgroup/palai/apps/control-plane/api/middleware"
	"github.com/palgroup/palai/apps/control-plane/internal/identity"
	"github.com/palgroup/palai/apps/control-plane/internal/knowledge"
)

// granted returns a copy of scope carrying the SERVER-SIDE ACL grants for the given labels — the shape a
// provisioned key's scopes take (kb-acl:<label>). This is how a principal legitimately holds a grant; a
// caller can never add these by sending a body field.
func granted(scope middleware.Scope, labels ...string) middleware.Scope {
	for _, l := range labels {
		scope.Scopes = append(scope.Scopes, knowledge.ACLGrantScope(l))
	}
	return scope
}

// secondProject opens a second project in the same organization and returns its id.
func secondProject(t *testing.T, idstore *identity.Store, org string) string {
	t.Helper()
	out, err := idstore.CreateProject(context.Background(), middleware.Scope{Organization: org}, []byte(`{"display_name":"p2"}`))
	if err != nil || out.Body == nil {
		t.Fatalf("CreateProject error = %v out = %+v", err, out)
	}
	var r struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(out.Body, &r); err != nil || r.ID == "" {
		t.Fatalf("decode project: %v (%s)", err, out.Body)
	}
	return r.ID
}

// queryHits runs a retrieval and returns the ranked chunks (grants come from scope, never the body).
func queryHits(t *testing.T, ks *knowledge.Store, scope middleware.Scope, kbID, q string) []knowledge.RetrievedChunk {
	t.Helper()
	body, _ := json.Marshal(map[string]any{"query": q})
	out, err := ks.Retrieve(context.Background(), scope, kbID, body)
	if err != nil {
		t.Fatalf("Retrieve error = %v", err)
	}
	if out.BadField {
		t.Fatalf("Retrieve unexpectedly rejected a clean body as bad field")
	}
	if out.NotFound {
		return nil
	}
	var env struct {
		Data []knowledge.RetrievedChunk `json:"data"`
	}
	if err := json.Unmarshal(out.Body, &env); err != nil {
		t.Fatalf("decode retrieval: %v", err)
	}
	return env.Data
}

// TestForgedACLGrantInBodyIsRejectedAndGovernedByScope is the KNO-003 crown proof: a caller cannot widen
// its authorization by claiming an ACL grant in the request body. The body grant is REJECTED (strict decode
// — acl_grants is no longer a recognized field), and the same caller, whose key scope lacks the grant, sees
// NOTHING of the restricted source. Only the server-derived grant (a key scope) unlocks it.
func TestForgedACLGrantInBodyIsRejectedAndGovernedByScope(t *testing.T) {
	cs, ks := openStore(t)
	scope := provisionTenant(t, cs, "kno-forged")
	kb := createKB(t, ks, scope, "kb")
	openSrc := createSource(t, ks, scope, kb, "")
	secretSrc := createSource(t, ks, scope, kb, "restricted")
	ingest(t, ks, scope, kb, openSrc, "Public roadmap discusses launch timeline widgets.")
	ingest(t, ks, scope, kb, secretSrc, "Confidential roadmap discusses acquisition timeline widgets.")

	// A forged grant in the request body is refused outright (a body never carries authorization).
	forged, _ := json.Marshal(map[string]any{"query": "roadmap timeline widgets", "acl_grants": []string{"restricted"}})
	out, err := ks.Retrieve(context.Background(), scope, kb, forged)
	if err != nil {
		t.Fatalf("Retrieve error = %v", err)
	}
	if !out.BadField {
		t.Fatalf("a forged acl_grants body field must be rejected as a bad field; got %s", out.Body)
	}

	// The same caller, no server-side grant: only the KB-wide source is visible; the restricted one is
	// filtered at the QUERY level (never returned, never ranked).
	for _, h := range queryHits(t, ks, scope, kb, "roadmap timeline widgets") {
		if h.ACL == "restricted" || strings.Contains(strings.ToLower(h.Content), "confidential") {
			t.Fatalf("ACL-first breach: restricted content leaked without a server-derived grant: %q", h.Content)
		}
	}

	// With the server-derived grant (a key scope), the restricted source becomes visible.
	found := false
	for _, h := range queryHits(t, ks, granted(scope, "restricted"), kb, "confidential acquisition") {
		if h.ACL == "restricted" {
			found = true
		}
	}
	if !found {
		t.Fatal("restricted source not retrievable even with the matching server-derived grant")
	}
}

// TestPostFilterTopKIsForbidden pins the §25.15.4 rule: the ACL predicate is in the QUERY (before ranking +
// LIMIT), so an unauthorized document can never occupy a slot in the top-K window and displace an authorized
// one. A restricted document is engineered to out-rank the authorized document (more query-term repetition),
// and the caller (no grant) queries with LIMIT=1. A post-filter-top-K implementation would fetch the
// restricted doc into the single slot, then filter it out, and return NOTHING — the authorized doc displaced.
// ACL-first returns the authorized doc (the restricted one never entered the window).
func TestPostFilterTopKIsForbidden(t *testing.T) {
	cs, ks := openStore(t)
	scope := provisionTenant(t, cs, "kno-topk")
	kb := createKB(t, ks, scope, "kb")
	openSrc := createSource(t, ks, scope, kb, "")
	secretSrc := createSource(t, ks, scope, kb, "restricted")
	// The authorized doc mentions the term once; the restricted doc repeats it, so ts_rank scores the
	// restricted doc HIGHER — it would win a naive top-1 fetch.
	ingest(t, ks, scope, kb, openSrc, "Widgets are discussed here.")
	ingest(t, ks, scope, kb, secretSrc, "Widgets widgets widgets widgets dominate this confidential note.")

	body, _ := json.Marshal(map[string]any{"query": "widgets", "max_results": 1})
	out, err := ks.Retrieve(context.Background(), scope, kb, body)
	if err != nil {
		t.Fatalf("Retrieve error = %v", err)
	}
	var env struct {
		Data []knowledge.RetrievedChunk `json:"data"`
	}
	if err := json.Unmarshal(out.Body, &env); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(env.Data) != 1 {
		t.Fatalf("post-filter-top-K breach: LIMIT=1 returned %d hits — the restricted doc displaced the authorized one from the window", len(env.Data))
	}
	if env.Data[0].ACL == "restricted" {
		t.Fatalf("ACL-first breach: restricted doc returned to a caller without the grant: %q", env.Data[0].Content)
	}
	if !strings.Contains(strings.ToLower(env.Data[0].Content), "widgets are discussed") {
		t.Fatalf("expected the authorized doc in the K window, got %q", env.Data[0].Content)
	}
}

// TestCrossProjectACLNegative proves intra-tenant project isolation: two projects in ONE organization, each
// with its own restricted KB. A caller scoped to project 2 (even holding the restricted grant label) sees
// NOTHING of project 1's KB — the knowledge base belongs to a project, so RLS (org+project) rejects the
// cross-project read before the ACL predicate is even reached.
func TestCrossProjectACLNegative(t *testing.T) {
	cs, ks := openStore(t)
	p1 := provisionTenant(t, cs, "kno-xproj")
	idstore := identity.New(cs.Pool())
	p2ID := secondProject(t, idstore, p1.Organization)
	p2 := middleware.Scope{Organization: p1.Organization, Project: p2ID}

	kb1 := createKB(t, ks, p1, "p1-kb")
	src1 := createSource(t, ks, p1, kb1, "restricted")
	ingest(t, ks, p1, kb1, src1, "Project one confidential acquisition timeline widgets.")

	// Project 2, even carrying the "restricted" grant, cannot see project 1's KB (RLS -> NotFound -> nil).
	if got := queryHits(t, ks, granted(p2, "restricted"), kb1, "acquisition timeline widgets"); got != nil {
		t.Fatalf("cross-project retrieval leaked %d rows from another project's KB", len(got))
	}
}
