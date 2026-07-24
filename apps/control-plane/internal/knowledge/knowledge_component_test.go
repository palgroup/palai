//go:build component

// Package knowledge_test holds the real-PostgreSQL component tests for the E17 Task 4 knowledge spine
// (KNO-001/KNO-002/KNO-004 + the ACL-first/tenant-isolation hook T5 hardens as KNO-003). They run only
// under `make test-component TEST=postgres` (which starts a throwaway container and exports
// PALAI_COMPONENT_POSTGRES_URL); the build tag keeps them out of the credential-free, Docker-free unit
// tier. They prove the spine against a real FTS index: immutable versioned ingestion, ranked retrieval
// with verifiable citation offsets, atomic activation that survives a failed refresh, source-delete
// propagation, the append-only REVOKE, and query-level tenant/ACL filtering.
package knowledge_test

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/palgroup/palai/apps/control-plane/api/middleware"
	"github.com/palgroup/palai/apps/control-plane/internal/identity"
	"github.com/palgroup/palai/apps/control-plane/internal/knowledge"
	"github.com/palgroup/palai/packages/coordinator"
	"github.com/palgroup/palai/storage"
)

// openStore returns a migrated spine store + a knowledge store over the same pool.
func openStore(t *testing.T) (*coordinator.Store, *knowledge.Store) {
	t.Helper()
	url := envURL(t)
	cs, err := coordinator.Open(context.Background(), url)
	if err != nil {
		t.Fatalf("coordinator.Open() error = %v", err)
	}
	t.Cleanup(cs.Close)
	if err := cs.Migrate(context.Background()); err != nil {
		t.Fatalf("Migrate() error = %v", err)
	}
	return cs, knowledge.New(cs.Pool())
}

func envURL(t *testing.T) string {
	t.Helper()
	url := os.Getenv("PALAI_COMPONENT_POSTGRES_URL")
	if url == "" {
		t.Skip("PALAI_COMPONENT_POSTGRES_URL is required; run make test-component TEST=postgres")
	}
	return url
}

// provisionTenant opens a new org+default project through the identity store (the engine behind
// POST /v1/organizations) and returns the scope a knowledge caller uses.
func provisionTenant(t *testing.T, cs *coordinator.Store, name string) middleware.Scope {
	t.Helper()
	idstore := identity.New(cs.Pool())
	out, err := idstore.CreateOrganization(context.Background(), middleware.Scope{}, []byte(`{"display_name":"`+name+`"}`))
	if err != nil {
		t.Fatalf("CreateOrganization(%s) error = %v", name, err)
	}
	var r struct {
		ID               string `json:"id"`
		DefaultProjectID string `json:"default_project_id"`
	}
	if err := json.Unmarshal(out.Body, &r); err != nil {
		t.Fatalf("decode organization: %v", err)
	}
	if r.ID == "" || r.DefaultProjectID == "" {
		t.Fatalf("incomplete tenant: %s", out.Body)
	}
	return middleware.Scope{Organization: r.ID, Project: r.DefaultProjectID}
}

// createKB provisions a knowledge base and returns its id.
func createKB(t *testing.T, ks *knowledge.Store, scope middleware.Scope, name string) string {
	t.Helper()
	out, err := ks.CreateKnowledgeBase(context.Background(), scope, []byte(`{"name":"`+name+`"}`))
	if err != nil || out.Body == nil {
		t.Fatalf("CreateKnowledgeBase error = %v out = %+v", err, out)
	}
	return decodeID(t, out.Body)
}

// createSource provisions an artifact source with an optional acl and returns its id.
func createSource(t *testing.T, ks *knowledge.Store, scope middleware.Scope, kbID, acl string) string {
	t.Helper()
	body := `{"kind":"artifact","uri":"upload://` + newID("u") + `","acl":"` + acl + `"}`
	out, err := ks.CreateSource(context.Background(), scope, kbID, []byte(body))
	if err != nil || out.Body == nil {
		t.Fatalf("CreateSource error = %v out = %+v", err, out)
	}
	return decodeID(t, out.Body)
}

// ingest runs one ingestion and returns the parsed outcome.
func ingest(t *testing.T, ks *knowledge.Store, scope middleware.Scope, kbID, sourceID, content string) ingestOutcome {
	t.Helper()
	body, _ := json.Marshal(map[string]string{"content": content})
	out, err := ks.Ingest(context.Background(), scope, kbID, sourceID, body)
	if err != nil || out.Body == nil {
		t.Fatalf("Ingest error = %v out = %+v", err, out)
	}
	var o ingestOutcome
	if err := json.Unmarshal(out.Body, &o); err != nil {
		t.Fatalf("decode ingest outcome: %v", err)
	}
	return o
}

type ingestOutcome struct {
	State              string `json:"state"`
	DocumentRevisionID string `json:"document_revision_id"`
	IndexRevisionID    string `json:"index_revision_id"`
	IndexVersion       int    `json:"index_version"`
	ChunkCount         int    `json:"chunk_count"`
	Error              string `json:"error"`
}

// retrieve runs a query and returns the ranked chunks.
func retrieve(t *testing.T, ks *knowledge.Store, scope middleware.Scope, kbID, query string, grants []string) []knowledge.RetrievedChunk {
	t.Helper()
	body, _ := json.Marshal(map[string]any{"query": query, "acl_grants": grants})
	out, err := ks.Retrieve(context.Background(), scope, kbID, body)
	if err != nil {
		t.Fatalf("Retrieve error = %v", err)
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

// TestIngestIsImmutableAndVersioned proves KNO-001's immutability + provenance: a re-ingest is a NEW
// document_revision version (never an in-place edit), both builds are retained as index revisions, and the
// append-only REVOKE stops the runtime role from mutating a stored revision.
func TestIngestIsImmutableAndVersioned(t *testing.T) {
	cs, ks := openStore(t)
	scope := provisionTenant(t, cs, "kno-immutable")
	kb := createKB(t, ks, scope, "handbook")
	src := createSource(t, ks, scope, kb, "")

	first := ingest(t, ks, scope, kb, src, "The onboarding guide explains vacation policy.")
	if first.State != "succeeded" || first.IndexVersion != 1 {
		t.Fatalf("first ingest = %+v, want succeeded index v1", first)
	}
	second := ingest(t, ks, scope, kb, src, "The onboarding guide now explains remote work policy.")
	if second.State != "succeeded" || second.IndexVersion != 2 {
		t.Fatalf("re-ingest = %+v, want succeeded index v2", second)
	}
	if first.DocumentRevisionID == second.DocumentRevisionID {
		t.Fatal("re-ingest reused the document revision id; it must be a new immutable version")
	}

	// Both builds retained (append-only index history).
	out, err := ks.ListIndexRevisions(context.Background(), scope, kb)
	if err != nil {
		t.Fatalf("ListIndexRevisions error = %v", err)
	}
	var env struct {
		Data []struct {
			Version int    `json:"version"`
			State   string `json:"state"`
		} `json:"data"`
	}
	if err := json.Unmarshal(out.Body, &env); err != nil {
		t.Fatalf("decode index revisions: %v", err)
	}
	if len(env.Data) != 2 {
		t.Fatalf("index revisions = %d, want 2 (append-only history)", len(env.Data))
	}

	// The append-only REVOKE: the runtime role cannot UPDATE or DELETE a stored document revision.
	assertAppendOnly(t, cs, scope, "document_revisions", first.DocumentRevisionID)
}

// TestFTSRanksAndCitesWithVerifiableOffsets proves KNO-001 retrieval: FTS ranks a query, and every hit's
// byte offsets recompute against the stored document bytes (the citation-offset invariant the T11 verifier
// recomputes).
func TestFTSRanksAndCitesWithVerifiableOffsets(t *testing.T) {
	cs, ks := openStore(t)
	scope := provisionTenant(t, cs, "kno-fts")
	kb := createKB(t, ks, scope, "docs")
	src := createSource(t, ks, scope, kb, "")

	ingest(t, ks, scope, kb, src,
		"Postgres full text search ranks documents.\n\nUnrelated paragraph about weather and rain.\n\nFull text search uses tsvector and tsquery for ranking.")

	hits := retrieve(t, ks, scope, kb, "full text search ranking", nil)
	if len(hits) < 2 {
		t.Fatalf("retrieval returned %d hits, want >=2 for the FTS query", len(hits))
	}
	// Ranked: scores are non-increasing.
	for i := 1; i < len(hits); i++ {
		if hits[i].Score > hits[i-1].Score {
			t.Fatalf("results not rank-ordered: hit %d score %v > hit %d score %v", i, hits[i].Score, i-1, hits[i-1].Score)
		}
	}
	// The top hit should mention the query terms; the weather paragraph must not out-rank them.
	if !strings.Contains(strings.ToLower(hits[0].Content), "search") {
		t.Fatalf("top hit does not contain the query term: %q", hits[0].Content)
	}
	// Citation offsets verify against the document bytes.
	for _, h := range hits {
		content, err := ks.DocumentContent(context.Background(), scope, h.DocumentRevisionID)
		if err != nil {
			t.Fatalf("DocumentContent error = %v", err)
		}
		if h.ByteStart < 0 || h.ByteEnd > len(content) || h.ByteStart > h.ByteEnd {
			t.Fatalf("hit offsets [%d,%d) out of document bounds (len %d)", h.ByteStart, h.ByteEnd, len(content))
		}
		if got := content[h.ByteStart:h.ByteEnd]; got != h.Content {
			t.Fatalf("citation offsets recover %q, want chunk content %q", got, h.Content)
		}
	}
}

// TestFailedRefreshLeavesPriorActiveIntact proves KNO-002: a refresh that fails completeness (nothing
// indexable) records a failed job and leaves the prior active index untouched — retrieval still serves it.
func TestFailedRefreshLeavesPriorActiveIntact(t *testing.T) {
	cs, ks := openStore(t)
	scope := provisionTenant(t, cs, "kno-failedrefresh")
	kb := createKB(t, ks, scope, "kb")
	src := createSource(t, ks, scope, kb, "")

	good := ingest(t, ks, scope, kb, src, "Retrieval augmented generation grounds answers in sources.")
	if good.State != "succeeded" {
		t.Fatalf("good ingest = %+v", good)
	}

	// A whitespace-only document parses to no indexable content — the completeness check fails.
	bad := ingest(t, ks, scope, kb, src, "   \n\n   \n   ")
	if bad.State != "failed" {
		t.Fatalf("whitespace ingest = %+v, want failed (nothing indexable)", bad)
	}

	// The prior active index still serves the original content (KNO-002: failed refresh does not corrupt).
	hits := retrieve(t, ks, scope, kb, "retrieval augmented generation", nil)
	if len(hits) == 0 {
		t.Fatal("failed refresh corrupted the prior active index: retrieval returned nothing")
	}
}

// TestSourceDeletePropagates proves KNO-004: after a source is deleted and the KB rebuilt, the deleted
// source's content no longer appears in retrieval (its documents drop out of the active index membership).
func TestSourceDeletePropagates(t *testing.T) {
	cs, ks := openStore(t)
	scope := provisionTenant(t, cs, "kno-delete")
	kb := createKB(t, ks, scope, "kb")
	keep := createSource(t, ks, scope, kb, "")
	drop := createSource(t, ks, scope, kb, "")

	ingest(t, ks, scope, kb, keep, "Alpha document about kubernetes deployments.")
	ingest(t, ks, scope, kb, drop, "Bravo document about kubernetes networking secrets.")

	// Both present before deletion.
	if got := retrieve(t, ks, scope, kb, "kubernetes networking", nil); len(got) == 0 {
		t.Fatal("expected the bravo document before deletion")
	}

	// Delete the source, then rebuild by re-ingesting the surviving source.
	if _, err := ks.DeleteSource(context.Background(), scope, drop); err != nil {
		t.Fatalf("DeleteSource error = %v", err)
	}
	ingest(t, ks, scope, kb, keep, "Alpha document about kubernetes deployments, revised.")

	// The deleted source's content must be gone from the active index.
	for _, h := range retrieve(t, ks, scope, kb, "networking secrets", nil) {
		if strings.Contains(strings.ToLower(h.Content), "networking") {
			t.Fatalf("deleted source content still retrievable: %q", h.Content)
		}
	}
}

// TestRetrievalIsTenantAndACLScoped proves the query-level filtering the T5 KNO-003 hardening rests on: a
// second tenant never sees the first's knowledge base (RLS), and within one tenant an ACL-restricted
// source is invisible unless the principal holds the grant (ACL-first, applied in the WHERE clause).
func TestRetrievalIsTenantAndACLScoped(t *testing.T) {
	cs, ks := openStore(t)
	orgA := provisionTenant(t, cs, "kno-tenant-a")
	orgB := provisionTenant(t, cs, "kno-tenant-b")

	kbA := createKB(t, ks, orgA, "a-kb")
	openSrc := createSource(t, ks, orgA, kbA, "")             // KB-wide
	secretSrc := createSource(t, ks, orgA, kbA, "restricted") // ACL-gated
	ingest(t, ks, orgA, kbA, openSrc, "Public roadmap discusses launch timeline widgets.")
	ingest(t, ks, orgA, kbA, secretSrc, "Confidential roadmap discusses acquisition timeline widgets.")

	// Cross-tenant: org B cannot even see org A's KB (RLS -> NotFound -> nil).
	if got := retrieve(t, ks, orgB, kbA, "roadmap timeline widgets", []string{"restricted"}); got != nil {
		t.Fatalf("cross-tenant retrieval leaked %d rows from another org", len(got))
	}

	// Within org A, no ACL grant -> only the KB-wide source is visible; the restricted one is filtered at
	// the query level (never returned, never ranked).
	noGrant := retrieve(t, ks, orgA, kbA, "roadmap timeline widgets", nil)
	if len(noGrant) == 0 {
		t.Fatal("expected the KB-wide source to be retrievable without a grant")
	}
	for _, h := range noGrant {
		if h.ACL == "restricted" || strings.Contains(strings.ToLower(h.Content), "confidential") {
			t.Fatalf("ACL-first breach: restricted content returned without the grant: %q", h.Content)
		}
	}

	// With the grant, the restricted source becomes visible.
	withGrant := retrieve(t, ks, orgA, kbA, "confidential acquisition", []string{"restricted"})
	found := false
	for _, h := range withGrant {
		if h.ACL == "restricted" {
			found = true
		}
	}
	if !found {
		t.Fatal("restricted source not retrievable even with the matching grant")
	}
}

// assertAppendOnly proves the migration's REVOKE UPDATE,DELETE: the runtime role (palai_app) cannot mutate
// a stored revision row. Both statements must be refused by a privilege error, not merely affect 0 rows.
func assertAppendOnly(t *testing.T, cs *coordinator.Store, scope middleware.Scope, table, id string) {
	t.Helper()
	ctx := storage.WithTenant(context.Background(), scope.Organization, scope.Project)
	_, updErr := cs.Pool().Exec(ctx, "UPDATE "+table+" SET checksum = 'tampered' WHERE id = $1", id)
	if !isPrivilegeError(updErr) {
		t.Fatalf("UPDATE %s was not refused by the append-only REVOKE: err = %v", table, updErr)
	}
	_, delErr := cs.Pool().Exec(ctx, "DELETE FROM "+table+" WHERE id = $1", id)
	if !isPrivilegeError(delErr) {
		t.Fatalf("DELETE %s was not refused by the append-only REVOKE: err = %v", table, delErr)
	}
}

func isPrivilegeError(err error) bool {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		return pgErr.Code == "42501" // insufficient_privilege
	}
	return false
}

func decodeID(t *testing.T, body []byte) string {
	t.Helper()
	var r struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(body, &r); err != nil {
		t.Fatalf("decode id: %v", err)
	}
	if r.ID == "" {
		t.Fatalf("missing id in %s", body)
	}
	return r.ID
}

func newID(prefix string) string {
	var raw [6]byte
	_, _ = rand.Read(raw[:])
	return prefix + "_" + hex.EncodeToString(raw[:])
}
