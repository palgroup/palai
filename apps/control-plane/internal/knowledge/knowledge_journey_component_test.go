//go:build component

// The E17 T11 EXIT-gate KNOWLEDGE JOURNEY (plan §T11): ingest → ACL negative → retrieve + cite → source
// delete → propagation, chained as ONE sequence against a real PostgreSQL FTS index. The individual
// invariants each have their own component test (KNO-001..008); this journey proves they hold TOGETHER on one
// knowledge base and emits the uat.KnowledgeACLProof the extensions-0.1.0 bundle carries.
//
// The load-bearing anti-fabrication move lives in the proof, not here: every citation the journey records
// carries the chunk's EXACT bytes alongside its declared offsets, and uat.KnowledgeACLProof.Complete()
// recomputes ChunkBytes[start:end] and refuses any citation whose quote does not reproduce. So a bundle
// cannot carry an invented offset pair — the verifier re-derives them.
//
// HONEST CEILING: the retrieval strategy exercised here is the KEYWORD (PostgreSQL FTS) core. The vector
// adapter is an interface with a deterministic fake and NO backing store (the compose Postgres image is
// plain — no pgvector), which is why `knowledge-vector` is advertised DISABLED and is §6 leg 4.
package knowledge_test

import (
	"context"
	"strings"
	"testing"

	"github.com/palgroup/palai/apps/control-plane/internal/knowledge"
	"github.com/palgroup/palai/tests/uat"
)

// TestKnowledgeJourney is the E17 knowledge EXIT journey.
func TestKnowledgeJourney(t *testing.T) {
	cs, ks := openStore(t)
	ctx := context.Background()
	scope := provisionTenant(t, cs, "kno-journey")
	kb := createKB(t, ks, scope, "journey-kb")

	// ---- 1. INGEST: one KB-wide source and one ACL-restricted source -------------------------------
	open := createSource(t, ks, scope, kb, "")
	restricted := createSource(t, ks, scope, kb, "restricted")
	const openText = "Deployment runbook: the rollout gate blocks a release without a restore proof."
	const secretText = "Confidential rollout gate memo: the acquisition timeline blocks the release."
	openIngest := ingest(t, ks, scope, kb, open, openText)
	if openIngest.State != "succeeded" || openIngest.ChunkCount < 1 || openIngest.IndexRevisionID == "" {
		t.Fatalf("step 1: open ingest = %+v, want a SUCCEEDED ingest with an active index revision and chunks", openIngest)
	}
	if got := ingest(t, ks, scope, kb, restricted, secretText); got.State != "succeeded" {
		t.Fatalf("step 1: restricted ingest = %+v, want succeeded", got)
	}

	// ---- 2. ACL NEGATIVE: without the server-derived grant the restricted source is INVISIBLE -------
	// It is filtered in the QUERY (before ranking), so it neither returns nor perturbs the ranking. The
	// journey measures the ranking BOTH ways to make the ACL-first claim mechanical rather than asserted:
	// the authorized ranking must be IDENTICAL with and without the restricted document present.
	ungranted := retrieve(t, ks, scope, kb, "rollout gate blocks release", nil)
	if len(ungranted) < 1 {
		t.Fatal("step 2: the authorized source must be retrievable")
	}
	for _, h := range ungranted {
		if h.ACL != "" || strings.Contains(strings.ToLower(h.Content), "confidential") {
			t.Fatalf("step 2: ACL-FIRST BREACH — restricted content reached an ungranted caller: %q (acl=%q)", h.Content, h.ACL)
		}
	}
	ungrantedOrder := chunkOrder(ungranted)

	// With the grant the restricted source becomes visible — the grant, not the body, is what unlocks it.
	grantedHits := retrieve(t, ks, scope, kb, "rollout gate blocks release", []string{"restricted"})
	sawRestricted := false
	for _, h := range grantedHits {
		if h.ACL == "restricted" {
			sawRestricted = true
		}
	}
	if !sawRestricted {
		t.Fatal("step 2: the restricted source is not retrievable even WITH the server-derived grant")
	}
	// The authorized subsequence of the granted ranking must be exactly the ungranted ranking: the
	// restricted document was ADDED to the result set, it did not DISPLACE or REORDER an authorized hit —
	// which a post-filter top-K implementation could not achieve.
	rankingShifted := !equalStrings(ungrantedOrder, authorizedOrder(grantedHits))
	if rankingShifted {
		t.Fatalf("step 2: the authorized ranking SHIFTED when the restricted doc entered (%v -> %v) — that is post-filter behaviour, not ACL-first",
			ungrantedOrder, authorizedOrder(grantedHits))
	}

	// ---- 3. RETRIEVE + CITE with offsets the verifier can re-derive from the chunk bytes ------------
	var citations []uat.KnowledgeCitation
	for _, h := range ungranted {
		doc, err := ks.DocumentContent(ctx, scope, h.DocumentRevisionID)
		if err != nil {
			t.Fatalf("step 3: read document bytes for %s: %v", h.DocumentRevisionID, err)
		}
		if h.ByteEnd <= h.ByteStart || h.ByteEnd > len(doc) {
			t.Fatalf("step 3: chunk %s offsets [%d,%d) are not inside its %d-byte document", h.ChunkID, h.ByteStart, h.ByteEnd, len(doc))
		}
		// The load-bearing assert: the CITED content must equal the document slice its offsets name. A
		// citation whose offsets do not reproduce is unverifiable provenance.
		if doc[h.ByteStart:h.ByteEnd] != h.Content {
			t.Fatalf("step 3: chunk %s content does not equal document_bytes[%d:%d] — the citation offsets do not reproduce",
				h.ChunkID, h.ByteStart, h.ByteEnd)
		}
		if h.CitationRef == "" || h.Checksum == "" || h.TrustClass == "" {
			t.Fatalf("step 3: chunk %s is missing its stable citation ref / checksum / trust class: %+v", h.ChunkID, h)
		}
		citations = append(citations, uat.KnowledgeCitation{
			ChunkID: h.ChunkID, ChunkBytes: doc,
			StartOffset: h.ByteStart, EndOffset: h.ByteEnd, Quote: h.Content,
		})
	}
	if len(citations) == 0 {
		t.Fatal("step 3: the journey recorded no citation")
	}

	// ---- 4. SOURCE DELETE ---------------------------------------------------------------------------
	if _, err := ks.DeleteSource(ctx, scope, restricted); err != nil {
		t.Fatalf("step 4: delete source: %v", err)
	}

	// ---- 5. PROPAGATION: a rebuild excludes the deleted source from the ACTIVE index ----------------
	rebuilt := ingest(t, ks, scope, kb, open, openText+" Revised after the source delete.")
	if rebuilt.State != "succeeded" || rebuilt.IndexVersion <= openIngest.IndexVersion {
		t.Fatalf("step 5: rebuild = %+v, want a NEWER active index than %d", rebuilt, openIngest.IndexVersion)
	}
	// Even a caller HOLDING the grant sees nothing of the deleted source: the delete propagated into index
	// membership, it is not merely an authorization change.
	for _, h := range retrieve(t, ks, scope, kb, "confidential acquisition timeline", []string{"restricted"}) {
		if h.SourceID == restricted || strings.Contains(strings.ToLower(h.Content), "confidential") {
			t.Fatalf("step 5: deleted source content is STILL in the active index: %q", h.Content)
		}
	}

	// ---- the EXIT-gate proof ------------------------------------------------------------------------
	proof := uat.KnowledgeACLProof{
		AuthorizedResults:            len(ungranted),
		UnauthorizedResults:          0,
		RankingShiftedByUnauthorized: rankingShifted,
		PostFilterTopK:               false,
		Citations:                    citations,
		SourceDeletePropagated:       true,
	}
	if !proof.Complete() {
		t.Fatalf("the journey's KnowledgeACLProof is not COMPLETE: %+v", proof)
	}
	t.Logf("knowledge journey PASS: %d authorized hits, 0 unauthorized, ranking unshifted, %d citations whose offsets RE-DERIVE from the chunk bytes, source delete propagated. Real vector/hybrid retrieval in a real store is §6 leg 4 — NOT claimed (knowledge-vector stays disabled).",
		proof.AuthorizedResults, len(proof.Citations))
}

// chunkOrder is the ranked chunk-id sequence — the comparable shape of a ranking.
func chunkOrder(hits []knowledge.RetrievedChunk) []string {
	out := make([]string, 0, len(hits))
	for _, h := range hits {
		out = append(out, h.ChunkID)
	}
	return out
}

// authorizedOrder is the ranked chunk-id sequence with the ACL-gated hits removed — the subsequence that
// must be identical to the ungranted ranking if authorization ran BEFORE scoring.
func authorizedOrder(hits []knowledge.RetrievedChunk) []string {
	out := make([]string, 0, len(hits))
	for _, h := range hits {
		if h.ACL == "" {
			out = append(out, h.ChunkID)
		}
	}
	return out
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
