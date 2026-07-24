package knowledge

import (
	"context"
	"errors"
)

// VectorAdapter is the DEFINED-BUT-DISABLED vector retrieval strategy (§25.15.3/§25.15.4). The FTS core
// (keyword strategy) is the STABLE candidate; the vector strategy is a follow-up. The interface is fixed
// here so T5's hybrid retrieval and a real pgvector/external engine can slot in without reshaping the spine,
// but no real implementation is wired — the compose Postgres image is plain (no pgvector), so this
// capability is advertised as `knowledge-vector`=disabled (KnowledgeVectorCapability). Nothing claims a
// vector capability the deployment cannot serve.
//
// A record ID carries tenant+kb+doc+chunk+index-revision identity (VectorRecord) so a vector store is NEVER
// the source of truth (§25.15.3) — it is a coordinate index back into the RLS-scoped chunk_revisions rows,
// and cross-tenant/cross-kb search is prevented by scoping the record IDs, not by trusting the store.
type VectorAdapter interface {
	// Upsert indexes a chunk's embedding under its fully-scoped record ID.
	Upsert(ctx context.Context, rec VectorRecord, embedding []float32) error
	// Search returns the nearest record IDs for a query embedding, pre-scoped to one tenant/kb; the caller
	// re-resolves and re-authorizes each hit against chunk_revisions (ACL-first), so a leaky store cannot
	// widen the result.
	Search(ctx context.Context, scope VectorScope, embedding []float32, k int) ([]VectorRecord, error)
	// Enabled reports whether a real vector backend is wired. It is false for the disabled default.
	Enabled() bool
}

// VectorRecord is the fully-scoped coordinate of one chunk's embedding. Every field is load-bearing for
// isolation: a search result that does not resolve back to a chunk_revisions row under the caller's RLS
// scope is discarded.
type VectorRecord struct {
	Organization     string
	Project          string
	KnowledgeBaseID  string
	DocumentRevision string
	ChunkID          string
	IndexRevisionID  string
}

// VectorScope narrows a vector search to one tenant/kb/index-revision before the store is even consulted.
type VectorScope struct {
	Organization    string
	Project         string
	KnowledgeBaseID string
	IndexRevisionID string
}

// ErrVectorDisabled is returned by the disabled adapter for every operation. It is errors.Is-able so a
// caller that reaches for the vector strategy fails LOUDLY (with a clear "capability disabled") rather than
// silently degrading to an empty or, worse, cross-scope result.
var ErrVectorDisabled = errors.New("knowledge: vector retrieval is disabled (pgvector not wired — §6 operator leg)")

// disabledVectorAdapter is the default: every operation refuses. It is the honest stand-in that keeps the
// interface exercised and the spine compiling while no real vector backend exists.
type disabledVectorAdapter struct{}

// DisabledVectorAdapter returns the default disabled adapter. main.go wires this until a real backend
// (pgvector/external) is provisioned as an operator leg.
func DisabledVectorAdapter() VectorAdapter { return disabledVectorAdapter{} }

func (disabledVectorAdapter) Upsert(context.Context, VectorRecord, []float32) error {
	return ErrVectorDisabled
}

func (disabledVectorAdapter) Search(context.Context, VectorScope, []float32, int) ([]VectorRecord, error) {
	return nil, ErrVectorDisabled
}

func (disabledVectorAdapter) Enabled() bool { return false }
