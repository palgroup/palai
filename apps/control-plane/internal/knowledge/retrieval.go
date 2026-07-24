package knowledge

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/palgroup/palai/apps/control-plane/api"
	"github.com/palgroup/palai/apps/control-plane/api/middleware"
	"github.com/palgroup/palai/storage"
)

// The E17 Task 5 retrieval surface (17b, SECURITY-CRITICAL): the typed §25.15.4 request / §25.15.5 result,
// ACL-first authorization (KNO-003), the keyword/vector/hybrid strategy dispatch (KNO-005), the untrusted
// tool-result trust class (KNO-006), the restricted-classification embedding guard (KNO-007), and the
// freshness deadline (KNO-008). The crown hardening over T4: the principal's ACL grants are DERIVED
// SERVER-SIDE from the verified key scopes — a request body can never carry authorization.

const (
	// defaultMaxResults / maxResultsCap bound the retrieval result count.
	defaultMaxResults = 10
	maxResultsCap     = 50

	// aclGrantPrefix namespaces a key scope that grants a knowledge ACL label. A source labelled
	// acl="finance" is retrievable only by a principal whose verified key carries the scope
	// "kb-acl:finance". The grant lives in api_keys.scopes (migration 000030), set at provisioning time by
	// an admin and resolved server-side by the auth middleware — never a request-body field. This is the
	// namespace that keeps an ACL grant distinct from a capability scope like "provision".
	aclGrantPrefix = "kb-acl:"

	// trustUntrusted is stamped on every retrieved chunk (KNO-006): source content is data, never an
	// instruction or a capability grant.
	trustUntrusted = "untrusted"

	// freshnessPolicyFail / freshnessPolicyWarn are the two honest responses to a missed freshness
	// deadline (KNO-008) — never a silent stale serve. Warn is the default.
	freshnessPolicyFail = "fail"
	freshnessPolicyWarn = "warn"

	// strategyKeyword is the real, local FTS strategy. Vector/hybrid (strategyVector/strategyHybrid) are
	// defined in vector_fake.go — a deterministic adapter, since the compose Postgres has no pgvector.
	strategyKeyword = "keyword"
)

// ACLGrantScope returns the verified-key scope string that grants a principal the given knowledge ACL label.
// It is the ONE canonical constructor for an ACL grant, so a provisioner (or the console/SDK) that wants a
// key to read acl="finance" sources adds ACLGrantScope("finance") to the key's scopes — the retrieval layer
// derives the held labels back out of the verified scopes, never from a request body.
func ACLGrantScope(label string) string { return aclGrantPrefix + label }

// derivePrincipalGrants extracts the ACL labels the authenticated principal holds from its VERIFIED key
// scopes (KNO-003). This is the T5 crown hardening: a caller cannot widen its authorization by claiming a
// grant in the request body — only the scopes bound to its key count. An empty result means the principal
// holds no labels, so only KB-wide (acl="") sources are visible (least privilege, fail-closed). Note the
// empty-scopes admin key (Scope.HasScope treats empty as unrestricted for capability gating) holds NO ACL
// labels here — ACL authorization is explicit and additive, never implied by admin status.
func derivePrincipalGrants(scope middleware.Scope) []string {
	grants := []string{}
	for _, s := range scope.Scopes {
		if strings.HasPrefix(s, aclGrantPrefix) {
			grants = append(grants, strings.TrimPrefix(s, aclGrantPrefix))
		}
	}
	return grants
}

// RetrieveRequest is the typed §25.15.4 retrieval request. It carries NO authorization field: the
// principal's grants are server-derived (derivePrincipalGrants). Strict decoding rejects any unknown field
// — including a forged acl_grants — so a caller cannot smuggle authorization through the body.
type RetrieveRequest struct {
	Query           string `json:"query"`
	Strategy        string `json:"strategy,omitempty"`         // keyword|vector|hybrid (default keyword)
	MaxResults      int    `json:"max_results,omitempty"`      // caps returned hits (<= maxResultsCap)
	IndexRevision   string `json:"index_revision,omitempty"`   // pin a reproducible revision (default active)
	RerankRoute     string `json:"rerank_route,omitempty"`     // optional pinned rerank model route (recorded, honest ceiling below)
	MaxStalenessMS  int64  `json:"max_staleness_ms,omitempty"` // freshness deadline; 0 = no constraint (KNO-008)
	FreshnessPolicy string `json:"freshness_policy,omitempty"` // fail|warn on a missed deadline (default warn)
	RequireCitation bool   `json:"require_citation,omitempty"` // reject a hit that cannot produce a citation ref
}

// RetrievalCost is the pinned strategy/route/score-provenance record (KNO-005): exactly which strategies ran,
// which pinned routes were consulted, and the token/rerank accounting. For the keyword strategy the embedding
// token count is 0 (no embedding); a real vector store's embedding cost is an operator leg (§6 leg 4).
type RetrievalCost struct {
	Strategy        string `json:"strategy"`
	KeywordHits     int    `json:"keyword_hits"`
	VectorHits      int    `json:"vector_hits"`
	EmbeddingTokens int    `json:"embedding_tokens"`
	RerankRoute     string `json:"rerank_route,omitempty"`
	Reranked        bool   `json:"reranked"`
}

// RetrievalResponse is the typed §25.15.5 result envelope. It pins the strategy and index revision the hits
// came from, the freshness verdict (KNO-008), any warnings, the cost record (KNO-005), and the ranked hits.
// Data keeps the "data" key so it renders as the list envelope existing callers decode.
type RetrievalResponse struct {
	Object          string           `json:"object"`
	Strategy        string           `json:"strategy"`
	IndexRevisionID string           `json:"index_revision_id"`
	Freshness       string           `json:"freshness"` // fresh|stale
	Warnings        []string         `json:"warnings,omitempty"`
	Cost            RetrievalCost    `json:"cost"`
	Data            []RetrievedChunk `json:"data"`
}

// Retrieve runs the typed §25.15.4/5 retrieval. It derives the principal's ACL grants SERVER-SIDE, resolves
// the target index revision (active or caller-pinned), enforces the freshness deadline (KNO-008), runs the
// requested strategy with the ACL predicate IN the query (KNO-003 — never post-filter top-K), re-checks the
// ACL a SECOND time in Go before returning (defense in depth), and stamps every hit untrusted (KNO-006).
func (s *Store) Retrieve(ctx context.Context, scope middleware.Scope, kbID string, body []byte) (api.ProvisionResult, error) {
	var in RetrieveRequest
	if err := strictDecode(body, &in); err != nil {
		return api.ProvisionResult{BadField: true}, nil
	}
	if in.Query == "" {
		return api.ProvisionResult{MissingField: "query"}, nil
	}
	strategy := in.Strategy
	if strategy == "" {
		strategy = strategyKeyword
	}
	if strategy != strategyKeyword && strategy != strategyVector && strategy != strategyHybrid {
		return api.ProvisionResult{BadField: true}, nil
	}
	limit := in.MaxResults
	if limit <= 0 {
		limit = defaultMaxResults
	}
	if limit > maxResultsCap {
		limit = maxResultsCap
	}
	policy := in.FreshnessPolicy
	if policy == "" {
		policy = freshnessPolicyWarn
	}
	if policy != freshnessPolicyFail && policy != freshnessPolicyWarn {
		return api.ProvisionResult{BadField: true}, nil
	}

	ctx = tenantScope(ctx, scope)
	if _, ok, err := s.knowledgeBase(ctx, kbID); err != nil {
		return api.ProvisionResult{}, err
	} else if !ok {
		return api.ProvisionResult{NotFound: true}, nil
	}

	// Resolve the target index revision (active or pinned) + its build time (the freshness anchor). A KB
	// that has never been built has no active revision — an empty, honest result rather than an error.
	idxID, builtAt, ok, err := s.resolveIndexRevision(ctx, kbID, in.IndexRevision)
	if err != nil {
		return api.ProvisionResult{}, err
	}
	if !ok {
		if in.IndexRevision != "" {
			return api.ProvisionResult{NotFound: true}, nil // a pinned-but-unknown revision is a 404
		}
		return api.ProvisionResult{Body: mustJSON(RetrievalResponse{
			Object: "retrieval_result", Strategy: strategy, Freshness: "fresh",
			Cost: RetrievalCost{Strategy: strategy}, Data: []RetrievedChunk{},
		})}, nil
	}

	// KNO-008: enforce the freshness deadline before serving — fail or warn, never silent stale.
	freshness, warnings, tooStale := evaluateFreshness(builtAt, in.MaxStalenessMS)
	if tooStale && policy == freshnessPolicyFail {
		return api.ProvisionResult{Conflict: true, Body: mustJSON(map[string]any{
			"object": "error", "code": "freshness_deadline_exceeded",
			"message": "the active index is staler than the requested freshness deadline",
		})}, nil
	}

	grants := derivePrincipalGrants(scope)
	hits, cost, err := s.runStrategy(ctx, scope, strategy, kbID, idxID, in.Query, grants, limit)
	if err != nil {
		if errors.Is(err, ErrVectorDisabled) {
			return api.ProvisionResult{Conflict: true, Body: mustJSON(map[string]any{
				"object": "error", "code": "capability_disabled",
				"message": "the vector retrieval strategy is disabled (pgvector not wired — §6 operator leg)",
			})}, nil
		}
		return api.ProvisionResult{}, err
	}
	cost.RerankRoute = in.RerankRoute
	// A pinned rerank route is RECORDED, not applied: cross-encoder rerank is an optional pinned model-route
	// score (honest ceiling — §6). Recording it keeps the request reproducible without claiming a rerank ran.

	// Second ACL filter before return (KNO-003 defense in depth) + untrusted stamp + citation ref.
	out := make([]RetrievedChunk, 0, len(hits))
	for _, h := range hits {
		if !aclAdmits(h.ACL, grants) {
			continue // belt-and-suspenders: the query predicate already excluded this; never leak on a regression.
		}
		h.IndexRevisionID = idxID
		h.TrustClass = trustUntrusted
		h.CitationRef = citationRef(h)
		if in.RequireCitation && h.CitationRef == "" {
			continue
		}
		out = append(out, h)
	}

	return api.ProvisionResult{Body: mustJSON(RetrievalResponse{
		Object: "retrieval_result", Strategy: strategy, IndexRevisionID: idxID,
		Freshness: freshness, Warnings: warnings, Cost: cost, Data: out,
	})}, nil
}

// runStrategy dispatches the requested retrieval strategy. keyword is the real, local FTS path; vector and
// hybrid route through the deterministic fake adapter (vector_fake.go) — the compose Postgres has no
// pgvector, so a real vector store is an operator leg (§6 leg 4). Each returns its hits + a cost record.
func (s *Store) runStrategy(ctx context.Context, scope middleware.Scope, strategy, kbID, idxID, query string, grants []string, limit int) ([]RetrievedChunk, RetrievalCost, error) {
	switch strategy {
	case strategyVector, strategyHybrid:
		return s.runVectorStrategy(ctx, scope, strategy, kbID, idxID, query, grants, limit)
	default:
		hits, err := s.keywordSearch(ctx, kbID, idxID, query, grants, limit)
		return hits, RetrievalCost{Strategy: strategyKeyword, KeywordHits: len(hits)}, err
	}
}

// keywordSearch runs the ACL-first FTS query against a PINNED index revision. Grants nil/empty means the
// principal holds no labels, so only KB-wide (acl="") sources are visible.
func (s *Store) keywordSearch(ctx context.Context, kbID, idxID, query string, grants []string, limit int) ([]RetrievedChunk, error) {
	if grants == nil {
		grants = []string{}
	}
	rows, err := s.pool.Query(ctx, storage.Query("RetrieveChunks"), kbID, query, grants, limit, idxID)
	if err != nil {
		return nil, fmt.Errorf("retrieve chunks: %w", err)
	}
	defer rows.Close()
	hits := []RetrievedChunk{}
	for rows.Next() {
		var h RetrievedChunk
		var createdAt time.Time
		h.Object = "knowledge_chunk"
		h.Strategy = strategyKeyword
		if err := rows.Scan(&h.ChunkID, &h.SourceID, &h.DocumentRevisionID, &h.Ordinal,
			&h.ByteStart, &h.ByteEnd, &h.Checksum, &h.ACL, &h.Content, &createdAt, &h.Score); err != nil {
			return nil, fmt.Errorf("scan retrieved chunk: %w", err)
		}
		h.CreatedAt = &createdAt
		hits = append(hits, h)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate retrieved chunks: %w", err)
	}
	return hits, nil
}

// resolveIndexRevision returns the target index revision id + its build time. An empty pinned id resolves the
// KB's active revision; a non-empty one resolves that specific revision within the KB (a foreign/unknown id
// or a KB that was never built -> ok=false).
func (s *Store) resolveIndexRevision(ctx context.Context, kbID, pinned string) (id string, builtAt time.Time, ok bool, err error) {
	query, args := storage.Query("GetActiveIndexRevision"), []any{kbID}
	if pinned != "" {
		query, args = storage.Query("GetIndexRevisionByID"), []any{pinned, kbID}
	}
	err = s.pool.QueryRow(ctx, query, args...).Scan(&id, &builtAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", time.Time{}, false, nil
	}
	if err != nil {
		return "", time.Time{}, false, fmt.Errorf("resolve index revision: %w", err)
	}
	return id, builtAt, true, nil
}

// evaluateFreshness compares the index build time against the requested max staleness (KNO-008). A zero
// deadline means no constraint (always fresh). A missed deadline yields freshness="stale" + a warning; the
// caller's policy decides whether that warning is fatal (Retrieve handles the fail policy before serving).
func evaluateFreshness(builtAt time.Time, maxStalenessMS int64) (freshness string, warnings []string, tooStale bool) {
	if maxStalenessMS <= 0 {
		return "fresh", nil, false
	}
	if time.Since(builtAt) > time.Duration(maxStalenessMS)*time.Millisecond {
		return "stale", []string{"freshness_deadline_exceeded"}, true
	}
	return "fresh", nil, false
}

// aclAdmits reports whether a chunk's ACL label is admitted by the principal's grants: a KB-wide (empty)
// label is always admitted; a restricted label needs the matching held grant. It is the SECOND ACL check
// (the query predicate is the first) — the same predicate, in Go, so a returned row is authorized twice.
func aclAdmits(acl string, grants []string) bool {
	if acl == "" {
		return true
	}
	for _, g := range grants {
		if g == acl {
			return true
		}
	}
	return false
}

// citationRef is the stable §25.15.5 citation reference: the document revision plus the exact byte span, so
// a citation resolves to the same bytes across rebuilds (the revision is immutable). The T11 verifier
// recomputes the chunk bytes from these offsets against the stored document.
func citationRef(h RetrievedChunk) string {
	return fmt.Sprintf("%s:%d-%d", h.DocumentRevisionID, h.ByteStart, h.ByteEnd)
}
