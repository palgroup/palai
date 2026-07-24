-- Knowledge spine (E17 Task 4, 17b — KNO-001/KNO-002/KNO-004). The read/write half of migration 000035's
-- six tables: the IMMUTABLE ingestion -> index -> retrieval spine on PostgreSQL FTS. Every statement runs
-- under the caller's org+project scope (internal/knowledge), so RLS isolates one tenant's corpus from
-- another's — a query names only its own ids, and organization_id/project_id are enforced by the tenant
-- policy, never a WHERE clause here. Retrieval adds an ACL predicate AT THE QUERY LEVEL (never post-fetch):
-- the ACL-first hook T5 hardens against the cross-ACL ranking/existence leak (KNO-003).

-- name: InsertKnowledgeBase
INSERT INTO knowledge_bases (id, organization_id, project_id, name, embedding_route)
VALUES ($1, $2, $3, $4, $5)
RETURNING created_at;

-- name: ListKnowledgeBases
SELECT id, name, embedding_route, active_index_revision_id, created_at
FROM knowledge_bases
ORDER BY created_at DESC, id;

-- name: GetKnowledgeBase
SELECT id, name, embedding_route, active_index_revision_id, created_at
FROM knowledge_bases
WHERE id = $1;

-- InsertSource records an ingest input. acl/classification/parser are the source's authorization + parsing
-- pins; acl is denormalized onto every chunk this source produces for the ACL-first retrieval predicate.
-- name: InsertSource
INSERT INTO knowledge_sources (id, organization_id, project_id, knowledge_base_id, kind, uri, acl, classification, parser)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
RETURNING created_at;

-- name: ListSources
SELECT id, kind, uri, acl, classification, parser, created_at
FROM knowledge_sources
WHERE knowledge_base_id = $1
ORDER BY created_at DESC, id;

-- name: GetSource
SELECT id, knowledge_base_id, kind, uri, acl, classification, parser
FROM knowledge_sources
WHERE id = $1;

-- DeleteSource removes a source (KNO-004). Its document/chunk revisions are NOT cascade-deleted (they belong
-- to the KB, and are retained as immutable history); the next rebuild EXCLUDES them because ActiveDocumentRevisions
-- inner-joins knowledge_sources, so an orphaned source_id drops out of the active index membership.
-- name: DeleteSource
DELETE FROM knowledge_sources WHERE id = $1;

-- name: InsertIngestionJob
INSERT INTO ingestion_jobs (id, organization_id, project_id, knowledge_base_id, source_id, state)
VALUES ($1, $2, $3, $4, $5, 'running')
RETURNING created_at;

-- name: FinishIngestionJob
UPDATE ingestion_jobs
SET state = $2, document_revision_id = $3, index_revision_id = $4, error = $5, updated_at = clock_timestamp()
WHERE id = $1;

-- LockKnowledgeBaseForBuild takes a per-KB row lock as the FIRST statement of a build transaction, so
-- concurrent same-KB ingests SERIALIZE (KNO-002). Without it, job B could snapshot membership before job A
-- commits its index, then commit a later version whose member set OMITS A's just-committed doc (A's content
-- silently drops from the active index until the next rebuild) — or the two collide on UNIQUE(kb, version).
-- name: LockKnowledgeBaseForBuild
SELECT id FROM knowledge_bases WHERE id = $1 FOR UPDATE;

-- NextDocumentVersion computes the next immutable version for a source (org/project enforced by RLS): 1 for
-- a first ingest, MAX(version)+1 for a re-ingest. A re-ingest is a new version, never an in-place edit.
-- name: NextDocumentVersion
SELECT coalesce(max(version), 0) + 1 FROM document_revisions WHERE source_id = $1;

-- name: InsertDocumentRevision
INSERT INTO document_revisions (id, organization_id, project_id, knowledge_base_id, source_id, version, checksum, byte_size, object_key, content, parser, provenance)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12)
RETURNING created_at;

-- name: GetDocumentRevision
SELECT id, source_id, version, checksum, byte_size, object_key, content, parser, provenance, created_at
FROM document_revisions
WHERE id = $1;

-- name: InsertChunkRevision
INSERT INTO chunk_revisions (id, organization_id, project_id, knowledge_base_id, source_id, document_revision_id, ordinal, byte_start, byte_end, checksum, acl, content)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12);

-- CountChunksInRevisions counts the chunks belonging to a set of document revisions — the index revision's
-- chunk_count over its member set (the just-ingested doc plus the unchanged sibling sources' latest docs).
-- name: CountChunksInRevisions
SELECT count(*) FROM chunk_revisions WHERE document_revision_id = ANY ($1);

-- name: NextIndexVersion
SELECT coalesce(max(version), 0) + 1 FROM index_revisions WHERE knowledge_base_id = $1;

-- name: InsertIndexRevision
INSERT INTO index_revisions (id, organization_id, project_id, knowledge_base_id, version, state, document_revision_ids, chunk_count)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
RETURNING created_at;

-- name: ListIndexRevisions
SELECT id, version, state, document_revision_ids, chunk_count, created_at
FROM index_revisions
WHERE knowledge_base_id = $1
ORDER BY version DESC;

-- ActivateIndex flips the KB's active pointer to a freshly built index revision. It is the atomic activation
-- of §25.15.2 completeness: it runs only after the index_revision row (state='active') is committed, so a
-- failed refresh (no activation call) leaves the prior active pointer intact (KNO-002).
-- name: ActivateIndex
UPDATE knowledge_bases SET active_index_revision_id = $2, updated_at = clock_timestamp() WHERE id = $1;

-- ActiveDocumentRevisions returns the member set for a rebuild: the latest document_revision per source that
-- STILL EXISTS in the KB. The inner join on knowledge_sources is what makes a source-delete propagate — a
-- deleted source's documents drop out of the next index membership (KNO-004).
-- name: ActiveDocumentRevisions
SELECT DISTINCT ON (dr.source_id) dr.id
FROM document_revisions dr
JOIN knowledge_sources s ON s.id = dr.source_id
WHERE dr.knowledge_base_id = $1
ORDER BY dr.source_id, dr.version DESC;

-- RetrieveChunks is the ranked FTS retrieval, ACL-FIRST. It intersects the KB's ACTIVE index_revision member
-- set ($1 kb) with the FTS match on $2, applies the principal's ACL grants ($3) AT THE QUERY LEVEL (a source
-- with a non-empty acl is invisible unless the principal holds it — never post-filtered), and ranks by
-- ts_rank. Tenant isolation is one layer down (RLS). $4 caps the result count. T5 hardens this into the full
-- KNO-003 (second filter before return, existence-leak proof); T4 pins the query-level predicate here.
-- name: RetrieveChunks
SELECT c.id, c.source_id, c.document_revision_id, c.ordinal, c.byte_start, c.byte_end, c.checksum, c.acl, c.content,
       ts_rank(c.fts, plainto_tsquery('english', $2)) AS rank
FROM chunk_revisions c
JOIN knowledge_bases kb ON kb.id = c.knowledge_base_id
JOIN index_revisions ir ON ir.id = kb.active_index_revision_id
WHERE c.knowledge_base_id = $1
  AND c.document_revision_id = ANY (ir.document_revision_ids)
  AND c.fts @@ plainto_tsquery('english', $2)
  AND (c.acl = '' OR c.acl = ANY ($3))
ORDER BY rank DESC, c.id
LIMIT $4;
