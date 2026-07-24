//go:build component

// E17 Task 5 strategy + security component proofs against a real PostgreSQL FTS index: the hybrid
// keyword+vector combination with verifiable citations (KNO-005), the disabled default vector strategy,
// the restricted-classification embedding guard (KNO-007), and the end-to-end freshness deadline (KNO-008).
// The vector strategy uses the DETERMINISTIC fake adapter (no pgvector, no real model) so the proof is
// reproducible; a real vector store is an operator leg (§6 leg 4).
package knowledge_test

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/palgroup/palai/apps/control-plane/api"
	"github.com/palgroup/palai/apps/control-plane/api/middleware"
	"github.com/palgroup/palai/apps/control-plane/internal/knowledge"
)

// retrieveTyped runs a retrieval with an explicit request map and returns the raw result + typed response.
func retrieveTyped(t *testing.T, ks *knowledge.Store, scope middleware.Scope, kbID string, req map[string]any) (api.ProvisionResult, knowledge.RetrievalResponse) {
	t.Helper()
	body, _ := json.Marshal(req)
	out, err := ks.Retrieve(context.Background(), scope, kbID, body)
	if err != nil {
		t.Fatalf("Retrieve error = %v", err)
	}
	var resp knowledge.RetrievalResponse
	if out.Body != nil {
		_ = json.Unmarshal(out.Body, &resp)
	}
	return out, resp
}

// createRestrictedSource provisions an artifact source pinned classification="restricted" (the KNO-007 gate).
func createRestrictedSource(t *testing.T, ks *knowledge.Store, scope middleware.Scope, kbID string) string {
	t.Helper()
	body := `{"kind":"artifact","uri":"upload://` + newID("u") + `","classification":"restricted"}`
	out, err := ks.CreateSource(context.Background(), scope, kbID, []byte(body))
	if err != nil || out.Body == nil {
		t.Fatalf("CreateSource(restricted) error = %v out = %+v", err, out)
	}
	return decodeID(t, out.Body)
}

// TestHybridStrategyFusesKeywordAndVectorWithCitations proves KNO-005: the hybrid strategy combines the real
// FTS ranking with the deterministic vector ranking, records the pinned strategy/scores/cost, and every hit
// carries a citation whose byte offsets recompute against the stored document bytes.
func TestHybridStrategyFusesKeywordAndVectorWithCitations(t *testing.T) {
	cs, ks := openStore(t)
	ks.SetVectorAdapter(knowledge.NewDeterministicVectorAdapter())
	scope := provisionTenant(t, cs, "kno-hybrid")
	kb := createKB(t, ks, scope, "kb")
	src := createSource(t, ks, scope, kb, "")
	ingest(t, ks, scope, kb, src,
		"Postgres full text search ranks documents.\n\nUnrelated weather and rain paragraph.\n\nFull text search uses tsvector and tsquery for ranking.")

	// Seed the deterministic vector index (the §25.15.2 step-7 embedding pass; non-restricted content).
	route := knowledge.EmbeddingRoute{Provider: "fake", Region: "local"}
	policy := knowledge.EmbeddingPolicy{AllowedRegions: []string{"local"}}
	if err := ks.IndexKBIntoVector(context.Background(), scope, kb, route, policy); err != nil {
		t.Fatalf("IndexKBIntoVector error = %v", err)
	}

	view, resp := retrieveTyped(t, ks, scope, kb, map[string]any{"query": "full text search ranking", "strategy": "hybrid"})
	if view.BadField || view.NotFound || view.Conflict {
		t.Fatalf("hybrid retrieve rejected: %+v", view)
	}
	if resp.Strategy != "hybrid" || resp.IndexRevisionID == "" {
		t.Fatalf("response not pinned to hybrid strategy + index revision: %+v", resp)
	}
	if len(resp.Data) < 2 {
		t.Fatalf("hybrid returned %d hits, want >=2", len(resp.Data))
	}
	if resp.Cost.KeywordHits == 0 || resp.Cost.VectorHits == 0 {
		t.Fatalf("hybrid cost record missing a strategy contribution: %+v", resp.Cost)
	}
	for _, h := range resp.Data {
		if h.Strategy != "hybrid" {
			t.Fatalf("hit not stamped hybrid: %q", h.Strategy)
		}
		if h.TrustClass != "untrusted" {
			t.Fatalf("hit not stamped untrusted: %q", h.TrustClass)
		}
		if h.CitationRef == "" {
			t.Fatal("hybrid hit has no citation ref")
		}
		content, err := ks.DocumentContent(context.Background(), scope, h.DocumentRevisionID)
		if err != nil {
			t.Fatalf("DocumentContent error = %v", err)
		}
		if got := content[h.ByteStart:h.ByteEnd]; got != h.Content {
			t.Fatalf("citation offsets recover %q, want %q", got, h.Content)
		}
	}

	// The pure vector strategy also resolves (deterministic overlap), scoped + authorized.
	_, vresp := retrieveTyped(t, ks, scope, kb, map[string]any{"query": "tsvector tsquery ranking", "strategy": "vector"})
	if vresp.Strategy != "vector" || len(vresp.Data) == 0 {
		t.Fatalf("vector strategy returned nothing: %+v", vresp)
	}
}

// TestVectorStrategyDisabledByDefault proves the honest ceiling: with no vector backend wired (production
// default), the vector strategy fails LOUDLY as a disabled capability — never a silent empty result.
func TestVectorStrategyDisabledByDefault(t *testing.T) {
	cs, ks := openStore(t) // default store: disabled vector adapter
	scope := provisionTenant(t, cs, "kno-vecdisabled")
	kb := createKB(t, ks, scope, "kb")
	src := createSource(t, ks, scope, kb, "")
	ingest(t, ks, scope, kb, src, "Anything indexable here for the active index.")

	view, _ := retrieveTyped(t, ks, scope, kb, map[string]any{"query": "anything", "strategy": "vector"})
	if !view.Conflict {
		t.Fatalf("disabled vector strategy must be a 409 conflict, got %+v", view)
	}
}

// TestRestrictedSourceNotEmbeddedToDisallowedRegion proves KNO-007: the embedding pass REFUSES to embed a
// restricted-classification source bound for a disallowed region; the same source embeds fine for an allowed
// region.
func TestRestrictedSourceNotEmbeddedToDisallowedRegion(t *testing.T) {
	cs, ks := openStore(t)
	ks.SetVectorAdapter(knowledge.NewDeterministicVectorAdapter())
	scope := provisionTenant(t, cs, "kno-region")
	kb := createKB(t, ks, scope, "kb")
	src := createRestrictedSource(t, ks, scope, kb)
	ingest(t, ks, scope, kb, src, "Restricted acquisition figures for internal use only.")

	disallowed := knowledge.EmbeddingRoute{Provider: "acme", Region: "us-east-1"}
	policy := knowledge.EmbeddingPolicy{AllowedRegions: []string{"eu-west-1"}}
	if err := ks.IndexKBIntoVector(context.Background(), scope, kb, disallowed, policy); err == nil {
		t.Fatal("restricted source embedded to a DISALLOWED region (KNO-007 breach)")
	}

	allowed := knowledge.EmbeddingRoute{Provider: "acme", Region: "eu-west-1"}
	if err := ks.IndexKBIntoVector(context.Background(), scope, kb, allowed, policy); err != nil {
		t.Fatalf("restricted source blocked for an ALLOWED region: %v", err)
	}
}

// TestFreshnessDeadlineFailsWarnsNeverSilentStale proves KNO-008: a missed freshness deadline fails under
// the fail policy and warns (but still serves) under the warn policy — never a silent stale serve; no
// deadline is fresh.
func TestFreshnessDeadlineFailsWarnsNeverSilentStale(t *testing.T) {
	cs, ks := openStore(t)
	scope := provisionTenant(t, cs, "kno-fresh")
	kb := createKB(t, ks, scope, "kb")
	src := createSource(t, ks, scope, kb, "")
	ingest(t, ks, scope, kb, src, "Retrieval augmented generation grounds answers in sources.")

	// Let the active index age past a 1ms deadline (robust to modest container clock skew).
	time.Sleep(120 * time.Millisecond)

	// fail policy: the retrieval is refused (409), no data served.
	view, _ := retrieveTyped(t, ks, scope, kb, map[string]any{
		"query": "retrieval augmented generation", "max_staleness_ms": 1, "freshness_policy": "fail"})
	if !view.Conflict {
		t.Fatalf("stale index under the fail policy must be a 409, got %+v", view)
	}

	// warn policy: results still served, but flagged stale with a warning (never silent).
	view, resp := retrieveTyped(t, ks, scope, kb, map[string]any{
		"query": "retrieval augmented generation", "max_staleness_ms": 1, "freshness_policy": "warn"})
	if view.Conflict {
		t.Fatalf("warn policy must serve, got conflict")
	}
	if resp.Freshness != "stale" || len(resp.Warnings) == 0 {
		t.Fatalf("warn policy did not flag staleness: %+v", resp)
	}
	if len(resp.Data) == 0 {
		t.Fatal("warn policy returned no data (should still serve the stale-but-present index)")
	}

	// no deadline: fresh.
	_, fresh := retrieveTyped(t, ks, scope, kb, map[string]any{"query": "retrieval augmented generation"})
	if fresh.Freshness != "fresh" {
		t.Fatalf("no deadline should be fresh, got %q", fresh.Freshness)
	}
}
