package knowledge

import (
	"context"
	"fmt"
	"hash/fnv"
	"math"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/palgroup/palai/apps/control-plane/api/middleware"
	"github.com/palgroup/palai/storage"
)

// The DETERMINISTIC fake vector strategy (§25.15.3/§25.15.4, KNO-005). The compose Postgres has no pgvector,
// so a real vector store is an operator leg (§6 leg 4) and `knowledge-vector` stays disabled. This adapter
// proves the INTERFACE + the hybrid combination + the §25.15.3 invariant that a vector store is NEVER the
// source of truth: every vector hit is re-resolved against the RLS + ACL-first authorized chunk set, so a
// leaky store cannot widen a result. The embedding is a deterministic bag-of-words hash — no real provider,
// no floats from a model — so the strategy is reproducible in a Docker-free unit test.

const (
	strategyVector = "vector"
	strategyHybrid = "hybrid"

	// embedDim is the fixed pseudo-embedding width. A token hashes into one bucket; the vector is L2-
	// normalized so cosine similarity is a plain dot product. Small and deterministic on purpose.
	embedDim = 64

	// rrfK is the reciprocal-rank-fusion constant for the hybrid combination (the standard 60). It damps
	// the contribution of a low-ranked hit so a chunk both strategies surface outranks one only in a single
	// list.
	rrfK = 60.0
)

// deterministicEmbed maps text to a fixed-width, L2-normalized pseudo-embedding by hashing each token into a
// bucket. It is a PURE function — the same text always yields the same vector — which is what makes the fake
// vector strategy reproducible without a model. It is NOT semantic; it captures lexical overlap, enough to
// exercise the interface + hybrid fusion honestly.
func deterministicEmbed(text string) []float32 {
	v := make([]float32, embedDim)
	for _, tok := range strings.FieldsFunc(strings.ToLower(text), func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsNumber(r)
	}) {
		h := fnv.New32a()
		_, _ = h.Write([]byte(tok))
		v[h.Sum32()%embedDim]++
	}
	var norm float64
	for _, x := range v {
		norm += float64(x) * float64(x)
	}
	if norm == 0 {
		return v
	}
	inv := float32(1 / math.Sqrt(norm))
	for i := range v {
		v[i] *= inv
	}
	return v
}

func cosine(a, b []float32) float64 {
	if len(a) != len(b) {
		return 0
	}
	var dot float64
	for i := range a {
		dot += float64(a[i]) * float64(b[i])
	}
	return dot
}

// storedVec is one indexed embedding + its fully-scoped record id.
type storedVec struct {
	rec VectorRecord
	emb []float32
}

// DeterministicVectorAdapter is the in-memory deterministic vector store (the fake). It honors VectorScope by
// pre-scoping Search to one tenant/kb/index-revision, but the retrieval layer re-resolves every hit anyway —
// the store is a coordinate index, never trusted. Safe for concurrent use.
type DeterministicVectorAdapter struct {
	mu   sync.RWMutex
	recs map[string]storedVec
}

// NewDeterministicVectorAdapter returns an empty deterministic adapter (Enabled). main.go still wires the
// DisabledVectorAdapter — this is the test/fixture adapter that proves the vector/hybrid path.
func NewDeterministicVectorAdapter() *DeterministicVectorAdapter {
	return &DeterministicVectorAdapter{recs: map[string]storedVec{}}
}

func recordKey(r VectorRecord) string {
	return strings.Join([]string{r.Organization, r.Project, r.KnowledgeBaseID, r.IndexRevisionID, r.ChunkID}, "\x00")
}

func (a *DeterministicVectorAdapter) Upsert(_ context.Context, rec VectorRecord, embedding []float32) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.recs[recordKey(rec)] = storedVec{rec: rec, emb: embedding}
	return nil
}

// Search returns the nearest record ids for a query embedding, PRE-SCOPED to the given tenant/kb/index-
// revision. A well-behaved store scopes here; the caller re-authorizes every hit regardless, so correctness
// never depends on this pre-scope being trustworthy.
func (a *DeterministicVectorAdapter) Search(_ context.Context, scope VectorScope, embedding []float32, k int) ([]VectorRecord, error) {
	a.mu.RLock()
	defer a.mu.RUnlock()
	type scored struct {
		rec   VectorRecord
		score float64
	}
	var hits []scored
	for _, sv := range a.recs {
		if sv.rec.Organization != scope.Organization || sv.rec.Project != scope.Project ||
			sv.rec.KnowledgeBaseID != scope.KnowledgeBaseID || sv.rec.IndexRevisionID != scope.IndexRevisionID {
			continue
		}
		hits = append(hits, scored{rec: sv.rec, score: cosine(embedding, sv.emb)})
	}
	sort.SliceStable(hits, func(i, j int) bool {
		if hits[i].score != hits[j].score {
			return hits[i].score > hits[j].score
		}
		return hits[i].rec.ChunkID < hits[j].rec.ChunkID
	})
	if k > 0 && len(hits) > k {
		hits = hits[:k]
	}
	out := make([]VectorRecord, 0, len(hits))
	for _, h := range hits {
		if h.score <= 0 {
			continue
		}
		out = append(out, h.rec)
	}
	return out, nil
}

func (a *DeterministicVectorAdapter) Enabled() bool { return true }

// SetVectorAdapter swaps the store's vector adapter (fixture/test wiring). Production leaves the disabled
// default; a real pgvector/external backend would be wired here behind the same interface.
func (s *Store) SetVectorAdapter(a VectorAdapter) { s.vector = a }

// IndexKBIntoVector is the deterministic stand-in for the §25.15.2 step-7 embedding pass: it reads the active
// (or pinned) index revision's chunks and upserts a deterministic embedding for each, under a fully-scoped
// record id. It enforces KNO-007 BEFORE any restricted content is embedded: a restricted-classification
// source bound for a route/region outside the policy is REFUSED (a hard error naming the source), never
// silently embedded. Non-restricted sources embed normally. This exists so the vector/hybrid strategy has an
// index to search in a test without a real store.
func (s *Store) IndexKBIntoVector(ctx context.Context, scope middleware.Scope, kbID string, route EmbeddingRoute, policy EmbeddingPolicy) error {
	ctx = tenantScope(ctx, scope)
	idxID, _, ok, err := s.resolveIndexRevision(ctx, kbID, "")
	if err != nil {
		return err
	}
	if !ok {
		return nil // never built; nothing to embed
	}
	rows, err := s.pool.Query(ctx, storage.Query("ChunksForEmbedding"), kbID, idxID)
	if err != nil {
		return fmt.Errorf("chunks for embedding: %w", err)
	}
	defer rows.Close()
	type row struct {
		chunkID, docRev, acl, classification, content string
	}
	var chunks []row
	for rows.Next() {
		var r row
		if err := rows.Scan(&r.chunkID, &r.docRev, &r.acl, &r.classification, &r.content); err != nil {
			return fmt.Errorf("scan embedding row: %w", err)
		}
		chunks = append(chunks, r)
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("iterate embedding rows: %w", err)
	}
	for _, c := range chunks {
		if err := policy.allows(c.classification, route); err != nil {
			return err // KNO-007: refuse before a restricted source's bytes leave for a disallowed target
		}
	}
	for _, c := range chunks {
		rec := VectorRecord{
			Organization: scope.Organization, Project: scope.Project, KnowledgeBaseID: kbID,
			DocumentRevision: c.docRev, ChunkID: c.chunkID, IndexRevisionID: idxID,
		}
		if err := s.vector.Upsert(ctx, rec, deterministicEmbed(c.content)); err != nil {
			return fmt.Errorf("upsert embedding: %w", err)
		}
	}
	return nil
}

// runVectorStrategy runs the vector (or hybrid) strategy. It loads the ACL-first authorized chunk set (the
// source of truth), asks the vector store for candidates, then RE-RESOLVES every candidate against that set
// — a record whose scope does not match or whose chunk is not authorized is dropped (§25.15.3: the store is
// never trusted). Hybrid additionally runs the keyword strategy and fuses the two rankings.
func (s *Store) runVectorStrategy(ctx context.Context, scope middleware.Scope, strategy, kbID, idxID, query string, grants []string, limit int) ([]RetrievedChunk, RetrievalCost, error) {
	if !s.vector.Enabled() {
		return nil, RetrievalCost{Strategy: strategy}, ErrVectorDisabled
	}
	admitted, err := s.admittedChunks(ctx, kbID, idxID, grants)
	if err != nil {
		return nil, RetrievalCost{}, err
	}
	recs, err := s.vector.Search(ctx, VectorScope{
		Organization: scope.Organization, Project: scope.Project, KnowledgeBaseID: kbID, IndexRevisionID: idxID,
	}, deterministicEmbed(query), limit)
	if err != nil {
		return nil, RetrievalCost{}, fmt.Errorf("vector search: %w", err)
	}
	vhits := resolveVectorHits(recs, kbID, idxID, admitted)

	cost := RetrievalCost{Strategy: strategy, VectorHits: len(vhits), EmbeddingTokens: len(strings.Fields(query))}
	if strategy == strategyVector {
		if len(vhits) > limit {
			vhits = vhits[:limit]
		}
		return vhits, cost, nil
	}

	// hybrid: fuse the keyword ranking with the vector ranking (reciprocal rank fusion).
	khits, err := s.keywordSearch(ctx, kbID, idxID, query, grants, limit)
	if err != nil {
		return nil, RetrievalCost{}, err
	}
	cost.KeywordHits = len(khits)
	fused := reciprocalRankFusion(khits, vhits, limit)
	return fused, cost, nil
}

// admittedChunks loads the ACL-first authorized chunk set for a pinned index revision, keyed by chunk id —
// the source of truth a vector hit is re-resolved against. It carries the SAME authorization predicate as
// the keyword query (grants applied in the WHERE), so an unauthorized chunk is never in the map.
func (s *Store) admittedChunks(ctx context.Context, kbID, idxID string, grants []string) (map[string]RetrievedChunk, error) {
	if grants == nil {
		grants = []string{}
	}
	rows, err := s.pool.Query(ctx, storage.Query("ChunksForVectorScope"), kbID, idxID, grants)
	if err != nil {
		return nil, fmt.Errorf("admitted chunks: %w", err)
	}
	defer rows.Close()
	out := map[string]RetrievedChunk{}
	for rows.Next() {
		var h RetrievedChunk
		var createdAt time.Time
		h.Object = "knowledge_chunk"
		if err := rows.Scan(&h.ChunkID, &h.SourceID, &h.DocumentRevisionID, &h.Ordinal,
			&h.ByteStart, &h.ByteEnd, &h.Checksum, &h.ACL, &h.Content, &createdAt); err != nil {
			return nil, fmt.Errorf("scan admitted chunk: %w", err)
		}
		h.CreatedAt = &createdAt
		out[h.ChunkID] = h
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate admitted chunks: %w", err)
	}
	return out, nil
}

// resolveVectorHits re-resolves vector-store candidates against the authorized chunk set (§25.15.3). It is
// the guard that makes the vector store untrusted: a record whose kb/index-revision does not match the query
// scope, or whose chunk is not in the ACL-first authorized set, is DROPPED — the store can never widen a
// result. Order is preserved (the store's rank), scores are the rank position rendered as a descending
// pseudo-score so a hybrid fusion has a stable ordering signal.
func resolveVectorHits(recs []VectorRecord, kbID, idxID string, admitted map[string]RetrievedChunk) []RetrievedChunk {
	out := make([]RetrievedChunk, 0, len(recs))
	for i, rec := range recs {
		if rec.KnowledgeBaseID != kbID || rec.IndexRevisionID != idxID {
			continue // scope mismatch: a leaky store cannot inject a foreign-scope record
		}
		h, ok := admitted[rec.ChunkID]
		if !ok {
			continue // not in the ACL-first authorized set: unauthorized or not in the index
		}
		h.Strategy = strategyVector
		h.Score = 1.0 / float64(i+1)
		out = append(out, h)
	}
	return out
}

// reciprocalRankFusion merges the keyword and vector rankings into one hybrid ranking (KNO-005). Each hit
// scores sum(1/(rrfK+rank)) across the lists it appears in, so a chunk both strategies surface outranks one
// found in a single list. Citation coordinates are preserved from whichever list first carried the chunk.
func reciprocalRankFusion(keyword, vector []RetrievedChunk, limit int) []RetrievedChunk {
	type acc struct {
		hit   RetrievedChunk
		score float64
	}
	merged := map[string]*acc{}
	add := func(list []RetrievedChunk) {
		for rank, h := range list {
			a, ok := merged[h.ChunkID]
			if !ok {
				copyHit := h
				a = &acc{hit: copyHit}
				merged[h.ChunkID] = a
			}
			a.score += 1.0 / (rrfK + float64(rank+1))
		}
	}
	add(keyword)
	add(vector)
	out := make([]RetrievedChunk, 0, len(merged))
	for _, a := range merged {
		a.hit.Strategy = strategyHybrid
		a.hit.Score = a.score
		out = append(out, a.hit)
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Score != out[j].Score {
			return out[i].Score > out[j].Score
		}
		return out[i].ChunkID < out[j].ChunkID
	})
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out
}
