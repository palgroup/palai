// Package knowledge is the E17 Task 4 knowledge spine (17b): an IMMUTABLE ingestion -> index -> retrieval
// spine on PostgreSQL full-text search (tsvector/tsquery + GIN, migration 000035 in-tree — assigned 000036,
// renumbered at merge per the fixed order; see the migration's MERGE NOTE). Ingested documents are
// append-only — a re-ingest is a new document_revision version, a rebuild a new index_revision, never an
// in-place edit. Retrieval returns ranked results and applies tenant (RLS) AND ACL filters AT THE QUERY
// LEVEL (never post-fetch): the ACL-first hook T5 hardens against the cross-ACL leak (KNO-003).
//
// TIER: the FTS core is the `knowledge` STABLE candidate; the vector strategy is a DEFINED-but-DISABLED
// adapter (vector.go — pgvector not wired), advertised as `knowledge-vector`=disabled.
//
// HONEST CEILINGS: parser v0 is text/markdown/code only (office/PDF are §5); the connector fetch (from an
// uploaded artifact's bytes or a repository path) is the E09 seam — Ingest accepts the resolved content
// directly and records the source uri as provenance; the object-store canonical-byte copy is likewise the
// E09 seam (object_key is recorded, the chunk text lives in Postgres for the FTS spine).
package knowledge

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/palgroup/palai/apps/control-plane/api"
	"github.com/palgroup/palai/apps/control-plane/api/middleware"
	"github.com/palgroup/palai/storage"
)

// Store is the knowledge spine over the durable pool. Each method scopes itself to the caller's
// organization+project, so RLS (migration 000035) isolates one tenant's corpus from another's. The vector
// adapter is the disabled default until a real backend is provisioned (an operator leg).
type Store struct {
	pool   *pgxpool.Pool
	vector VectorAdapter
}

// New builds the store over the durable spine's pool with the disabled vector adapter.
func New(pool *pgxpool.Pool) *Store {
	return &Store{pool: pool, vector: DisabledVectorAdapter()}
}

// tenantScope binds the caller's verified org+project to ctx (project-aware — unlike the org-wide identity
// resources, a knowledge base belongs to one project). The values come from the verified key, never a body.
func tenantScope(ctx context.Context, scope middleware.Scope) context.Context {
	return storage.WithTenant(ctx, scope.Organization, scope.Project)
}

// --- knowledge bases -------------------------------------------------------------------------------------

// CreateKnowledgeBase opens a knowledge base in the caller's project. embedding_route is an optional pinned
// route ref for the (disabled) vector strategy; empty = FTS-only.
func (s *Store) CreateKnowledgeBase(ctx context.Context, scope middleware.Scope, body []byte) (api.ProvisionResult, error) {
	var in struct {
		Name           string `json:"name"`
		EmbeddingRoute string `json:"embedding_route"`
	}
	if err := strictDecode(body, &in); err != nil {
		return api.ProvisionResult{BadField: true}, nil
	}
	if in.Name == "" {
		return api.ProvisionResult{MissingField: "name"}, nil
	}
	ctx = tenantScope(ctx, scope)
	id := middleware.NewID("kb")
	var createdAt time.Time
	if err := s.pool.QueryRow(ctx, storage.Query("InsertKnowledgeBase"),
		id, scope.Organization, scope.Project, in.Name, in.EmbeddingRoute).Scan(&createdAt); err != nil {
		return api.ProvisionResult{}, fmt.Errorf("insert knowledge base: %w", err)
	}
	return api.ProvisionResult{Body: mustJSON(knowledgeBaseView{
		ID: id, Object: "knowledge_base", Name: in.Name, EmbeddingRoute: in.EmbeddingRoute, CreatedAt: &createdAt,
	})}, nil
}

// ListKnowledgeBases lists the caller's project's knowledge bases (newest first).
func (s *Store) ListKnowledgeBases(ctx context.Context, scope middleware.Scope) (api.ProvisionResult, error) {
	ctx = tenantScope(ctx, scope)
	rows, err := s.pool.Query(ctx, storage.Query("ListKnowledgeBases"))
	if err != nil {
		return api.ProvisionResult{}, fmt.Errorf("list knowledge bases: %w", err)
	}
	defer rows.Close()
	data := []knowledgeBaseView{}
	for rows.Next() {
		v, err := scanKnowledgeBase(rows)
		if err != nil {
			return api.ProvisionResult{}, err
		}
		data = append(data, v)
	}
	if err := rows.Err(); err != nil {
		return api.ProvisionResult{}, fmt.Errorf("iterate knowledge bases: %w", err)
	}
	return api.ProvisionResult{Body: mustJSON(listView{Object: "list", Data: data})}, nil
}

// --- sources ---------------------------------------------------------------------------------------------

// CreateSource registers an ingest input on a knowledge base. kind is the connector-v0 vocabulary
// (artifact|repository); acl/classification/parser are the source's authorization + parsing pins.
func (s *Store) CreateSource(ctx context.Context, scope middleware.Scope, kbID string, body []byte) (api.ProvisionResult, error) {
	var in struct {
		Kind           string `json:"kind"`
		URI            string `json:"uri"`
		ACL            string `json:"acl"`
		Classification string `json:"classification"`
		Parser         string `json:"parser"`
	}
	if err := strictDecode(body, &in); err != nil {
		return api.ProvisionResult{BadField: true}, nil
	}
	if in.Kind == "" {
		return api.ProvisionResult{MissingField: "kind"}, nil
	}
	if in.URI == "" {
		return api.ProvisionResult{MissingField: "uri"}, nil
	}
	if in.Parser == "" {
		in.Parser = "text"
	}
	ctx = tenantScope(ctx, scope)
	if _, ok, err := s.knowledgeBase(ctx, kbID); err != nil {
		return api.ProvisionResult{}, err
	} else if !ok {
		return api.ProvisionResult{NotFound: true}, nil
	}
	id := middleware.NewID("ksrc")
	var createdAt time.Time
	if err := s.pool.QueryRow(ctx, storage.Query("InsertSource"),
		id, scope.Organization, scope.Project, kbID, in.Kind, in.URI, in.ACL, in.Classification, in.Parser).Scan(&createdAt); err != nil {
		return api.ProvisionResult{BadField: true}, nil // a bad kind/parser trips the CHECK constraint
	}
	return api.ProvisionResult{Body: mustJSON(sourceView{
		ID: id, Object: "knowledge_source", Kind: in.Kind, URI: in.URI, ACL: in.ACL,
		Classification: in.Classification, Parser: in.Parser, CreatedAt: &createdAt,
	})}, nil
}

// ListSources lists a knowledge base's sources (newest first).
func (s *Store) ListSources(ctx context.Context, scope middleware.Scope, kbID string) (api.ProvisionResult, error) {
	ctx = tenantScope(ctx, scope)
	rows, err := s.pool.Query(ctx, storage.Query("ListSources"), kbID)
	if err != nil {
		return api.ProvisionResult{}, fmt.Errorf("list sources: %w", err)
	}
	defer rows.Close()
	data := []sourceView{}
	for rows.Next() {
		v := sourceView{Object: "knowledge_source"}
		var createdAt time.Time
		if err := rows.Scan(&v.ID, &v.Kind, &v.URI, &v.ACL, &v.Classification, &v.Parser, &createdAt); err != nil {
			return api.ProvisionResult{}, fmt.Errorf("scan source: %w", err)
		}
		v.CreatedAt = &createdAt
		data = append(data, v)
	}
	if err := rows.Err(); err != nil {
		return api.ProvisionResult{}, fmt.Errorf("iterate sources: %w", err)
	}
	return api.ProvisionResult{Body: mustJSON(listView{Object: "list", Data: data})}, nil
}

// DeleteSource removes a source. Its immutable document/chunk revisions are retained as history, but the
// next ingest's rebuild excludes them (ActiveDocumentRevisions inner-joins knowledge_sources), so the
// deleted content drops out of the active index (KNO-004). Callers should re-ingest a remaining source (or
// the KB is left with its prior active index until the next build) — deletion alone does not rebuild.
func (s *Store) DeleteSource(ctx context.Context, scope middleware.Scope, sourceID string) (api.ProvisionResult, error) {
	ctx = tenantScope(ctx, scope)
	tag, err := s.pool.Exec(ctx, storage.Query("DeleteSource"), sourceID)
	if err != nil {
		return api.ProvisionResult{}, fmt.Errorf("delete source: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return api.ProvisionResult{NotFound: true}, nil
	}
	return api.ProvisionResult{Body: mustJSON(map[string]any{"object": "knowledge_source", "id": sourceID, "deleted": true})}, nil
}

// --- ingestion -------------------------------------------------------------------------------------------

// Ingest runs the §25.15.2 pipeline for one source: an immutable versioned document_revision, deterministic
// chunks, a rebuilt KB-wide index_revision, and atomic activation. It is append-only (a re-ingest is a new
// version) and atomic (the build commits in one transaction; a failed build leaves the prior active index
// intact — KNO-002). content is the resolved source bytes (the connector fetch is the E09 seam).
//
// The ingestion_job is recorded in its OWN committed statement first, so the ATTEMPT is durable even when
// the build rolls back — a failed refresh is visible without having corrupted the corpus.
func (s *Store) Ingest(ctx context.Context, scope middleware.Scope, kbID, sourceID string, body []byte) (api.ProvisionResult, error) {
	var in struct {
		Content string `json:"content"`
	}
	if err := strictDecode(body, &in); err != nil {
		return api.ProvisionResult{BadField: true}, nil
	}
	if in.Content == "" {
		return api.ProvisionResult{MissingField: "content"}, nil
	}
	ctx = tenantScope(ctx, scope)

	kb, ok, err := s.knowledgeBase(ctx, kbID)
	if err != nil {
		return api.ProvisionResult{}, err
	}
	if !ok {
		return api.ProvisionResult{NotFound: true}, nil
	}
	src, ok, err := s.source(ctx, sourceID)
	if err != nil {
		return api.ProvisionResult{}, err
	}
	if !ok || src.knowledgeBaseID != kbID {
		return api.ProvisionResult{NotFound: true}, nil
	}

	// Step 0: record the running attempt durably (own statement — survives a build rollback).
	jobID := middleware.NewID("kjob")
	if _, err := s.pool.Exec(ctx, storage.Query("InsertIngestionJob"),
		jobID, scope.Organization, scope.Project, kbID, sourceID); err != nil {
		return api.ProvisionResult{}, fmt.Errorf("insert ingestion job: %w", err)
	}

	result, buildErr := s.runBuild(ctx, scope, kb, src, in.Content)
	if buildErr != nil {
		// KNO-002: the build rolled back (corpus + active pointer untouched); record the failure durably.
		if _, err := s.pool.Exec(ctx, storage.Query("FinishIngestionJob"),
			jobID, "failed", nil, nil, buildErr.Error()); err != nil {
			return api.ProvisionResult{}, fmt.Errorf("record failed ingestion job: %w", err)
		}
		return api.ProvisionResult{Body: mustJSON(ingestionView{
			Object: "ingestion_job", ID: jobID, State: "failed", Error: buildErr.Error(),
		})}, nil
	}
	if _, err := s.pool.Exec(ctx, storage.Query("FinishIngestionJob"),
		jobID, "succeeded", result.documentRevisionID, result.indexRevisionID, ""); err != nil {
		return api.ProvisionResult{}, fmt.Errorf("record succeeded ingestion job: %w", err)
	}
	return api.ProvisionResult{Body: mustJSON(ingestionView{
		Object: "ingestion_job", ID: jobID, State: "succeeded",
		DocumentRevisionID: result.documentRevisionID, IndexRevisionID: result.indexRevisionID,
		IndexVersion: result.indexVersion, ChunkCount: result.chunkCount,
	})}, nil
}

// buildResult carries the ids a successful build produced.
type buildResult struct {
	documentRevisionID string
	indexRevisionID    string
	indexVersion       int
	chunkCount         int
}

// runBuild performs the immutable build in ONE transaction: append the document_revision + its chunks,
// snapshot the KB's active document revisions into a new index_revision, and flip the KB's active pointer.
// Any error rolls the whole thing back, so a failed refresh never leaves a half-built or wrongly-activated
// index (KNO-002). It returns an error the caller records on the ingestion_job.
func (s *Store) runBuild(ctx context.Context, scope middleware.Scope, kb knowledgeBaseRow, src sourceRow, content string) (buildResult, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return buildResult{}, fmt.Errorf("begin build: %w", err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()

	// Steps 3-4: validate + immutable DocumentRevision. Deterministic chunking BEFORE the doc is written so
	// a document that parses to nothing indexable is rejected as an empty build (completeness), leaving the
	// prior active index intact.
	chunks := chunkDocument(content, defaultMaxChunkBytes)
	if len(chunks) == 0 {
		return buildResult{}, errors.New("validation: document parsed to no indexable content")
	}
	sum := sha256.Sum256([]byte(content))
	checksum := "sha256:" + hex.EncodeToString(sum[:])

	var docVersion int
	if err := tx.QueryRow(ctx, storage.Query("NextDocumentVersion"), src.id).Scan(&docVersion); err != nil {
		return buildResult{}, fmt.Errorf("next document version: %w", err)
	}
	docRevID := middleware.NewID("kdoc")
	objectKey := fmt.Sprintf("%s/%s/%s/%s/%d", scope.Organization, scope.Project, kb.id, src.id, docVersion)
	if _, err := tx.Exec(ctx, storage.Query("InsertDocumentRevision"),
		docRevID, scope.Organization, scope.Project, kb.id, src.id, docVersion, checksum,
		len(content), objectKey, content, src.parser, provenanceJSON(src, docVersion)); err != nil {
		return buildResult{}, fmt.Errorf("insert document revision: %w", err)
	}

	// Steps 5-6: deterministic chunks with ACL + checksum + byte offsets + provenance (source->document->chunk).
	for _, c := range chunks {
		chunkSum := sha256.Sum256([]byte(c.Content))
		if _, err := tx.Exec(ctx, storage.Query("InsertChunkRevision"),
			middleware.NewID("kchk"), scope.Organization, scope.Project, kb.id, src.id, docRevID,
			c.Ordinal, c.ByteStart, c.ByteEnd, "sha256:"+hex.EncodeToString(chunkSum[:]), src.acl, c.Content); err != nil {
			return buildResult{}, fmt.Errorf("insert chunk revision: %w", err)
		}
	}

	// Step 8: snapshot the KB's active document revisions (latest per still-existing source) into the new
	// index_revision. This is the member set retrieval intersects against.
	rows, err := tx.Query(ctx, storage.Query("ActiveDocumentRevisions"), kb.id)
	if err != nil {
		return buildResult{}, fmt.Errorf("active document revisions: %w", err)
	}
	var membership []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return buildResult{}, fmt.Errorf("scan membership: %w", err)
		}
		membership = append(membership, id)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return buildResult{}, fmt.Errorf("iterate membership: %w", err)
	}

	var chunkCount int
	if err := tx.QueryRow(ctx, storage.Query("CountChunksInRevisions"), membership).Scan(&chunkCount); err != nil {
		return buildResult{}, fmt.Errorf("count index chunks: %w", err)
	}

	var indexVersion int
	if err := tx.QueryRow(ctx, storage.Query("NextIndexVersion"), kb.id).Scan(&indexVersion); err != nil {
		return buildResult{}, fmt.Errorf("next index version: %w", err)
	}
	indexRevID := middleware.NewID("kidx")
	if _, err := tx.Exec(ctx, storage.Query("InsertIndexRevision"),
		indexRevID, scope.Organization, scope.Project, kb.id, indexVersion, "active", membership, chunkCount); err != nil {
		return buildResult{}, fmt.Errorf("insert index revision: %w", err)
	}

	// Step 9: atomic activation — flip the KB's active pointer to the completed index revision.
	if _, err := tx.Exec(ctx, storage.Query("ActivateIndex"), kb.id, indexRevID); err != nil {
		return buildResult{}, fmt.Errorf("activate index: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return buildResult{}, fmt.Errorf("commit build: %w", err)
	}
	return buildResult{documentRevisionID: docRevID, indexRevisionID: indexRevID, indexVersion: indexVersion, chunkCount: chunkCount}, nil
}

// --- retrieval -------------------------------------------------------------------------------------------

// defaultMaxResults / maxResultsCap bound the retrieval result count.
const (
	defaultMaxResults = 10
	maxResultsCap     = 50
)

// Retrieve runs a ranked FTS query against the KB's ACTIVE index revision, applying the principal's ACL
// grants AT THE QUERY LEVEL (ACL-first — a source with a non-empty acl is invisible unless the principal
// holds it). Tenant isolation is enforced one layer down by RLS. Results carry stable citation offsets, the
// chunk checksum, and the document_revision id, so a citation is verifiable against the document bytes.
func (s *Store) Retrieve(ctx context.Context, scope middleware.Scope, kbID string, body []byte) (api.ProvisionResult, error) {
	var in struct {
		Query      string   `json:"query"`
		ACLGrants  []string `json:"acl_grants"`
		MaxResults int      `json:"max_results"`
	}
	if err := strictDecode(body, &in); err != nil {
		return api.ProvisionResult{BadField: true}, nil
	}
	if in.Query == "" {
		return api.ProvisionResult{MissingField: "query"}, nil
	}
	limit := in.MaxResults
	if limit <= 0 {
		limit = defaultMaxResults
	}
	if limit > maxResultsCap {
		limit = maxResultsCap
	}
	ctx = tenantScope(ctx, scope)
	if _, ok, err := s.knowledgeBase(ctx, kbID); err != nil {
		return api.ProvisionResult{}, err
	} else if !ok {
		return api.ProvisionResult{NotFound: true}, nil
	}

	hits, err := s.retrieve(ctx, kbID, in.Query, in.ACLGrants, limit)
	if err != nil {
		return api.ProvisionResult{}, err
	}
	return api.ProvisionResult{Body: mustJSON(listView{Object: "list", Data: hits})}, nil
}

// retrieve is the typed retrieval used by both the API projection and the component tests (which assert
// rank order and verify citation offsets). ACL grants nil/empty means the principal holds no labels, so
// only KB-wide (acl=”) sources are visible.
func (s *Store) retrieve(ctx context.Context, kbID, query string, aclGrants []string, limit int) ([]RetrievedChunk, error) {
	if aclGrants == nil {
		aclGrants = []string{}
	}
	rows, err := s.pool.Query(ctx, storage.Query("RetrieveChunks"), kbID, query, aclGrants, limit)
	if err != nil {
		return nil, fmt.Errorf("retrieve chunks: %w", err)
	}
	defer rows.Close()
	hits := []RetrievedChunk{}
	for rows.Next() {
		var h RetrievedChunk
		h.Object = "knowledge_chunk"
		if err := rows.Scan(&h.ChunkID, &h.SourceID, &h.DocumentRevisionID, &h.Ordinal,
			&h.ByteStart, &h.ByteEnd, &h.Checksum, &h.ACL, &h.Content, &h.Score); err != nil {
			return nil, fmt.Errorf("scan retrieved chunk: %w", err)
		}
		hits = append(hits, h)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate retrieved chunks: %w", err)
	}
	return hits, nil
}

// --- index revisions -------------------------------------------------------------------------------------

// ListIndexRevisions lists a KB's index revisions (newest version first) — the append-only build history.
func (s *Store) ListIndexRevisions(ctx context.Context, scope middleware.Scope, kbID string) (api.ProvisionResult, error) {
	ctx = tenantScope(ctx, scope)
	rows, err := s.pool.Query(ctx, storage.Query("ListIndexRevisions"), kbID)
	if err != nil {
		return api.ProvisionResult{}, fmt.Errorf("list index revisions: %w", err)
	}
	defer rows.Close()
	data := []indexRevisionView{}
	for rows.Next() {
		v := indexRevisionView{Object: "index_revision"}
		var members []string
		var createdAt time.Time
		if err := rows.Scan(&v.ID, &v.Version, &v.State, &members, &v.ChunkCount, &createdAt); err != nil {
			return api.ProvisionResult{}, fmt.Errorf("scan index revision: %w", err)
		}
		v.DocumentRevisions = len(members)
		v.CreatedAt = &createdAt
		data = append(data, v)
	}
	if err := rows.Err(); err != nil {
		return api.ProvisionResult{}, fmt.Errorf("iterate index revisions: %w", err)
	}
	return api.ProvisionResult{Body: mustJSON(listView{Object: "list", Data: data})}, nil
}

// DocumentContent returns a document revision's stored content (org/project RLS-scoped). It is the anchor
// the citation-offset proof recomputes chunk bytes against: content[byte_start:byte_end] == the chunk.
func (s *Store) DocumentContent(ctx context.Context, scope middleware.Scope, docRevID string) (string, error) {
	ctx = tenantScope(ctx, scope)
	var (
		id, sourceID, checksum, objectKey, content, parser string
		version                                            int
		byteSize                                           int64
		provenance                                         []byte
		createdAt                                          time.Time
	)
	err := s.pool.QueryRow(ctx, storage.Query("GetDocumentRevision"), docRevID).Scan(
		&id, &sourceID, &version, &checksum, &byteSize, &objectKey, &content, &parser, &provenance, &createdAt)
	if err != nil {
		return "", fmt.Errorf("get document revision: %w", err)
	}
	return content, nil
}

// --- internal reads --------------------------------------------------------------------------------------

type knowledgeBaseRow struct {
	id                    string
	name                  string
	embeddingRoute        string
	activeIndexRevisionID string
}

func (s *Store) knowledgeBase(ctx context.Context, id string) (knowledgeBaseRow, bool, error) {
	var kb knowledgeBaseRow
	var active *string
	var route string
	var createdAt time.Time
	err := s.pool.QueryRow(ctx, storage.Query("GetKnowledgeBase"), id).Scan(&kb.id, &kb.name, &route, &active, &createdAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return knowledgeBaseRow{}, false, nil
	}
	if err != nil {
		return knowledgeBaseRow{}, false, fmt.Errorf("get knowledge base: %w", err)
	}
	kb.embeddingRoute = route
	if active != nil {
		kb.activeIndexRevisionID = *active
	}
	return kb, true, nil
}

type sourceRow struct {
	id              string
	knowledgeBaseID string
	kind            string
	uri             string
	acl             string
	classification  string
	parser          string
}

func (s *Store) source(ctx context.Context, id string) (sourceRow, bool, error) {
	var src sourceRow
	err := s.pool.QueryRow(ctx, storage.Query("GetSource"), id).Scan(
		&src.id, &src.knowledgeBaseID, &src.kind, &src.uri, &src.acl, &src.classification, &src.parser)
	if errors.Is(err, pgx.ErrNoRows) {
		return sourceRow{}, false, nil
	}
	if err != nil {
		return sourceRow{}, false, fmt.Errorf("get source: %w", err)
	}
	return src, true, nil
}

// provenanceJSON records the source->document provenance link pinned on the revision.
func provenanceJSON(src sourceRow, version int) []byte {
	return mustJSON(map[string]any{
		"source_id":      src.id,
		"source_kind":    src.kind,
		"source_uri":     src.uri,
		"parser":         src.parser,
		"chunker":        chunkerRevision,
		"source_version": version,
	})
}

// --- helpers (local copies of the identity package's unexported projection helpers) ----------------------

type listView struct {
	Object string `json:"object"`
	Data   any    `json:"data"`
}

func strictDecode(body []byte, v any) error {
	if len(bytes.TrimSpace(body)) == 0 {
		body = []byte("{}")
	}
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.DisallowUnknownFields()
	return dec.Decode(v)
}

func mustJSON(v any) []byte {
	b, err := json.Marshal(v)
	if err != nil {
		panic(fmt.Sprintf("knowledge: marshal projection: %v", err))
	}
	return b
}
