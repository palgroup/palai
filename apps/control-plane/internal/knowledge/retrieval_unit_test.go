package knowledge

import (
	"testing"
	"time"

	"github.com/palgroup/palai/apps/control-plane/api/middleware"
)

// These are the Docker-free, deterministic checks for the T5 retrieval helpers: server-derived grant
// extraction (KNO-003), the vector-hit re-resolution guard that keeps a leaky store from widening a result
// (§25.15.3), reciprocal rank fusion (KNO-005), the restricted-classification embedding guard (KNO-007), and
// the freshness verdict (KNO-008). They run in `make test-unit` (no build tag).

func TestDerivePrincipalGrantsFromKeyScopesOnly(t *testing.T) {
	scope := middleware.Scope{Scopes: []string{"provision", ACLGrantScope("finance"), "reporting", ACLGrantScope("hr")}}
	got := derivePrincipalGrants(scope)
	want := map[string]bool{"finance": true, "hr": true}
	if len(got) != len(want) {
		t.Fatalf("derivePrincipalGrants = %v, want the two kb-acl labels only", got)
	}
	for _, g := range got {
		if !want[g] {
			t.Fatalf("derivePrincipalGrants leaked a non-ACL scope as a grant: %q", g)
		}
	}
	// An admin key with NO scopes holds no ACL labels (fail-closed — admin status never implies a grant).
	if g := derivePrincipalGrants(middleware.Scope{}); len(g) != 0 {
		t.Fatalf("empty-scope key derived %v grants, want none (fail-closed)", g)
	}
}

func TestACLAdmits(t *testing.T) {
	cases := []struct {
		acl    string
		grants []string
		want   bool
	}{
		{"", nil, true},                          // KB-wide always admitted
		{"", []string{"x"}, true},                // KB-wide admitted regardless of grants
		{"restricted", []string{"restricted"}, true}, // held grant admits
		{"restricted", nil, false},               // no grant -> denied
		{"restricted", []string{"other"}, false}, // wrong grant -> denied
	}
	for _, c := range cases {
		if got := aclAdmits(c.acl, c.grants); got != c.want {
			t.Fatalf("aclAdmits(%q, %v) = %v, want %v", c.acl, c.grants, got, c.want)
		}
	}
}

// TestResolveVectorHitsDropsLeakyRecords proves §25.15.3: the vector store is never trusted. Even if Search
// returns a foreign-scope record or one for an unauthorized chunk, re-resolution against the ACL-first
// admitted set drops it — only the authorized in-scope record survives.
func TestResolveVectorHitsDropsLeakyRecords(t *testing.T) {
	org, project, kbID, idxID := "org_1", "proj_1", "kb_1", "kidx_1"
	admitted := map[string]RetrievedChunk{
		"kchk_ok": {ChunkID: "kchk_ok", Content: "authorized", DocumentRevisionID: "kdoc_1"},
	}
	recs := []VectorRecord{
		{Organization: org, Project: project, KnowledgeBaseID: kbID, IndexRevisionID: idxID, ChunkID: "kchk_ok"},       // authorized, in-scope
		{Organization: "org_other", Project: project, KnowledgeBaseID: kbID, IndexRevisionID: idxID, ChunkID: "kchk_ok"}, // foreign org -> dropped
		{Organization: org, Project: "proj_other", KnowledgeBaseID: kbID, IndexRevisionID: idxID, ChunkID: "kchk_ok"},    // foreign project -> dropped
		{Organization: org, Project: project, KnowledgeBaseID: "kb_other", IndexRevisionID: idxID, ChunkID: "kchk_ok"},   // foreign KB -> dropped
		{Organization: org, Project: project, KnowledgeBaseID: kbID, IndexRevisionID: "kidx_old", ChunkID: "kchk_ok"},    // foreign revision -> dropped
		{Organization: org, Project: project, KnowledgeBaseID: kbID, IndexRevisionID: idxID, ChunkID: "kchk_secret"},     // not in admitted set -> dropped
	}
	got := resolveVectorHits(recs, org, project, kbID, idxID, admitted)
	if len(got) != 1 || got[0].ChunkID != "kchk_ok" {
		t.Fatalf("resolveVectorHits let a leaky record through: %+v", got)
	}
	if got[0].Strategy != strategyVector {
		t.Fatalf("resolved hit not stamped vector strategy: %q", got[0].Strategy)
	}
}

// TestReciprocalRankFusionRewardsBothStrategies proves KNO-005 hybrid combination: a chunk both strategies
// surface outranks one found in only a single list.
func TestReciprocalRankFusionRewardsBothStrategies(t *testing.T) {
	keyword := []RetrievedChunk{{ChunkID: "A"}, {ChunkID: "B"}}
	vector := []RetrievedChunk{{ChunkID: "B"}, {ChunkID: "C"}}
	fused := reciprocalRankFusion(keyword, vector, 10)
	if len(fused) != 3 {
		t.Fatalf("fusion returned %d hits, want 3 distinct chunks", len(fused))
	}
	if fused[0].ChunkID != "B" {
		t.Fatalf("fusion did not rank the doubly-surfaced chunk first: %+v", fused)
	}
	if fused[0].Strategy != strategyHybrid {
		t.Fatalf("fused hit not stamped hybrid strategy: %q", fused[0].Strategy)
	}
}

// TestEmbeddingPolicyBlocksRestrictedToDisallowedTarget proves KNO-007: restricted content is refused for a
// route/region outside the allowlist; non-restricted content is unconstrained; an empty allowlist is fail-
// closed for restricted content.
func TestEmbeddingPolicyBlocksRestrictedToDisallowedTarget(t *testing.T) {
	inRegion := EmbeddingRoute{Provider: "acme", Region: "eu-west-1"}
	outRegion := EmbeddingRoute{Provider: "acme", Region: "us-east-1"}
	policy := EmbeddingPolicy{AllowedRegions: []string{"eu-west-1"}}

	if err := policy.allows("", outRegion); err != nil {
		t.Fatalf("non-restricted content wrongly blocked: %v", err)
	}
	if err := policy.allows(classificationRestricted, inRegion); err != nil {
		t.Fatalf("restricted content wrongly blocked for an allowed region: %v", err)
	}
	if err := policy.allows(classificationRestricted, outRegion); err == nil {
		t.Fatal("restricted content NOT blocked for a disallowed region (KNO-007 breach)")
	}
	if err := (EmbeddingPolicy{}).allows(classificationRestricted, inRegion); err == nil {
		t.Fatal("empty allowlist admitted restricted content (must be fail-closed)")
	}
	// Provider narrowing: region allowed but provider not.
	provPolicy := EmbeddingPolicy{AllowedRegions: []string{"eu-west-1"}, AllowedProviders: []string{"trusted"}}
	if err := provPolicy.allows(classificationRestricted, inRegion); err == nil {
		t.Fatal("restricted content NOT blocked for a disallowed provider (KNO-007 breach)")
	}
}

// TestEvaluateFreshness proves KNO-008: no deadline is always fresh; a met deadline is fresh; a missed
// deadline is stale + warned + flagged too-stale (the caller's policy decides fatality).
func TestEvaluateFreshness(t *testing.T) {
	if f, w, stale := evaluateFreshness(time.Now().Add(-time.Hour), 0); f != "fresh" || len(w) != 0 || stale {
		t.Fatalf("no deadline should be fresh: %q %v %v", f, w, stale)
	}
	if f, _, stale := evaluateFreshness(time.Now(), 60_000); f != "fresh" || stale {
		t.Fatalf("a just-built index within a 60s deadline should be fresh: %q %v", f, stale)
	}
	f, w, stale := evaluateFreshness(time.Now().Add(-2*time.Hour), 1000)
	if f != "stale" || len(w) == 0 || !stale {
		t.Fatalf("a 2h-old index against a 1s deadline should be stale+warned: %q %v %v", f, w, stale)
	}
}

func TestDeterministicEmbedIsPureAndOverlapSensitive(t *testing.T) {
	a := deterministicEmbed("full text search ranking")
	b := deterministicEmbed("full text search ranking")
	for i := range a {
		if a[i] != b[i] {
			t.Fatal("deterministicEmbed is not pure: same text yielded different vectors")
		}
	}
	related := cosine(deterministicEmbed("kubernetes networking secrets"), deterministicEmbed("kubernetes networking"))
	unrelated := cosine(deterministicEmbed("kubernetes networking secrets"), deterministicEmbed("weather and rain"))
	if related <= unrelated {
		t.Fatalf("embedding not overlap-sensitive: related=%v unrelated=%v", related, unrelated)
	}
}

func TestCitationRefStableSpan(t *testing.T) {
	got := citationRef(RetrievedChunk{DocumentRevisionID: "kdoc_x", ByteStart: 10, ByteEnd: 20})
	if got != "kdoc_x:10-20" {
		t.Fatalf("citationRef = %q, want kdoc_x:10-20", got)
	}
}
