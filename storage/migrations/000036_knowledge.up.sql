-- 000036 opens the knowledge spine (E17 Task 4, 17b — KNO-001/KNO-002/KNO-004): an IMMUTABLE
-- ingestion -> index -> retrieval spine on PostgreSQL full-text search (tsvector/tsquery + GIN). It is
-- the FTS core the E17 exit gate promotes as a `knowledge` STABLE candidate; the vector strategy is a
-- DEFINED-but-DISABLED adapter interface (pgvector not wired — the compose image is plain), advertised
-- as `knowledge-vector`=disabled, so nothing here claims a capability the deployment cannot serve.
--
-- MERGE NOTE (fixed order §1): the plan ASSIGNS this migration 000036 and merges T1's slack 000035 FIRST.
-- It is built here as 000035 — the next contiguous number off the 000034 chain head — so this isolated
-- worktree's TestOrderedMigrationsIsContiguousVersionOrder (strict, no gaps) stays green, exactly as the
-- sibling wave-1 tasks (T1 slack, T7 queue) each build at 000035 off the same base. At integration the
-- fixed order applies: T1's slack lands as 000035, and the integrator renumbers THIS file to 000036 —
-- rename the up/down files, bump the embed var (migrationUp35 -> migrationUp36) and the concat, update the
-- migrations_test head/penultimate assertions, and change the two `schema_migrations` markers below to (36).
-- The table/policy/grant content is number-independent.
--
-- SHAPE (§25.15.2/§25.15.3). Six tables, all org+project tenant-scoped:
--   knowledge_bases    mutable config; carries the active_index_revision_id pointer flipped on activation
--   knowledge_sources  mutable; the ingest inputs (uploaded artifact | repository path — connector v0)
--   ingestion_jobs     mutable state machine (pending->running->succeeded/failed) recording each 9-step run
--   document_revisions APPEND-ONLY; one immutable, versioned snapshot per source ingest (checksum+bytes)
--   chunk_revisions    APPEND-ONLY; immutable deterministic chunks + a GENERATED tsvector (the FTS index)
--   index_revisions    APPEND-ONLY; one immutable KB-wide build snapshot (its member document_revisions)
--
-- IMMUTABILITY: a re-ingest is a NEW document_revision version, never an in-place edit; a rebuild is a NEW
-- index_revision. The three *_revisions tables therefore carry a self-re-asserting REVOKE UPDATE,DELETE (the
-- 000031/000032 precedent) so the writing role can only append. Activation is a single UPDATE on the mutable
-- knowledge_bases pointer, so a FAILED refresh never disturbs the prior active index (KNO-002).
--
-- RLS: every table carries organization_id + project_id, so per the M3 born-secured rule (000030) each
-- asserts its OWN project-aware policy in THIS migration rather than leaning on 000029's boot sweep. This
-- runs LAST in the chain, so the CALLs and the REVOKEs re-assert after 000001/000029's blanket grants every
-- boot. Retrieval filters by RLS (tenant) AND an ACL predicate AT THE QUERY LEVEL (never post-fetch) — the
-- ACL-first hook T5 hardens against the cross-ACL leak (KNO-003).

-- knowledge_bases: the retrieval unit. active_index_revision_id points at the index_revision retrieval reads
-- against; it is a plain column (NO foreign key) on purpose — index_revisions references knowledge_bases, so
-- a reciprocal FK would be a cycle. The app flips it inside the activation transaction under RLS.
CREATE TABLE IF NOT EXISTS knowledge_bases (
    id TEXT PRIMARY KEY,
    organization_id TEXT NOT NULL,
    project_id TEXT NOT NULL,
    name TEXT NOT NULL,
    -- The pinned embedding route ref for the (disabled) vector strategy. Empty = FTS-only. A REF, never a
    -- secret value: the value resolves through the E13 secret_refs chain, never a column here.
    embedding_route TEXT NOT NULL DEFAULT '',
    active_index_revision_id TEXT,
    created_at TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp(),
    FOREIGN KEY (organization_id, project_id) REFERENCES projects (organization_id, id),
    UNIQUE (organization_id, project_id, name)
);

-- knowledge_sources: an ingest input. kind is the connector-v0 vocabulary (artifact|repository — §25.15.2;
-- web/DB connectors are OUT, §5). acl is the source's authorization label denormalized onto every chunk for
-- the ACL-first retrieval predicate; classification pins restricted content away from disallowed embedding
-- providers (KNO-007, T5). A source is deletable (KNO-004 propagation), so it keeps full DML.
CREATE TABLE IF NOT EXISTS knowledge_sources (
    id TEXT PRIMARY KEY,
    organization_id TEXT NOT NULL,
    project_id TEXT NOT NULL,
    knowledge_base_id TEXT NOT NULL REFERENCES knowledge_bases (id) ON DELETE CASCADE,
    kind TEXT NOT NULL CHECK (kind IN ('artifact', 'repository')),
    uri TEXT NOT NULL,
    acl TEXT NOT NULL DEFAULT '',
    classification TEXT NOT NULL DEFAULT '',
    parser TEXT NOT NULL DEFAULT 'text' CHECK (parser IN ('text', 'markdown', 'code')),
    created_at TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp(),
    FOREIGN KEY (organization_id, project_id) REFERENCES projects (organization_id, id)
);
CREATE INDEX IF NOT EXISTS knowledge_sources_kb_idx
    ON knowledge_sources (organization_id, project_id, knowledge_base_id);

-- ingestion_jobs: the durable record of one 9-step ingestion run. Mutable state machine — a failed refresh
-- lands 'failed' with an error and leaves the KB's prior active pointer untouched (KNO-002); a success
-- records the document + index revision it produced.
CREATE TABLE IF NOT EXISTS ingestion_jobs (
    id TEXT PRIMARY KEY,
    organization_id TEXT NOT NULL,
    project_id TEXT NOT NULL,
    knowledge_base_id TEXT NOT NULL REFERENCES knowledge_bases (id) ON DELETE CASCADE,
    source_id TEXT NOT NULL,
    state TEXT NOT NULL DEFAULT 'pending' CHECK (state IN ('pending', 'running', 'succeeded', 'failed')),
    document_revision_id TEXT,
    index_revision_id TEXT,
    error TEXT NOT NULL DEFAULT '',
    created_at TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp(),
    FOREIGN KEY (organization_id, project_id) REFERENCES projects (organization_id, id)
);

-- document_revisions: APPEND-ONLY. One immutable, per-source versioned snapshot. checksum is sha256 of the
-- canonical bytes (provenance source->document); content holds the parsed text so a chunk's byte offsets are
-- verifiable against it (the citation-offset proof recomputes chunk bytes as content[byte_start:byte_end]).
-- object_key names where the canonical bytes live in the object store (§25.15.3).
-- ponytail: content is stored in Postgres for the FTS spine, duplicating the object-store canonical bytes;
-- the object-store upload path is the E09 seam — object_key is recorded, the byte copy lands when wired.
CREATE TABLE IF NOT EXISTS document_revisions (
    id TEXT PRIMARY KEY,
    organization_id TEXT NOT NULL,
    project_id TEXT NOT NULL,
    knowledge_base_id TEXT NOT NULL REFERENCES knowledge_bases (id) ON DELETE CASCADE,
    source_id TEXT NOT NULL,
    version INTEGER NOT NULL,
    checksum TEXT NOT NULL,
    byte_size BIGINT NOT NULL CHECK (byte_size >= 0),
    object_key TEXT NOT NULL,
    content TEXT NOT NULL,
    parser TEXT NOT NULL,
    provenance JSONB NOT NULL DEFAULT '{}'::jsonb,
    created_at TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp(),
    FOREIGN KEY (organization_id, project_id) REFERENCES projects (organization_id, id),
    UNIQUE (organization_id, project_id, source_id, version)
);

-- chunk_revisions: APPEND-ONLY. The deterministic chunk projection of a document_revision. fts is a STORED
-- GENERATED tsvector — the FTS index is a column, so it can never drift from content. acl is denormalized
-- from the source so the ACL-first predicate filters WITHOUT a join. byte_start/byte_end index into the
-- parent document_revision's content (stable citation offsets); checksum is sha256 of this chunk's bytes.
CREATE TABLE IF NOT EXISTS chunk_revisions (
    id TEXT PRIMARY KEY,
    organization_id TEXT NOT NULL,
    project_id TEXT NOT NULL,
    knowledge_base_id TEXT NOT NULL REFERENCES knowledge_bases (id) ON DELETE CASCADE,
    source_id TEXT NOT NULL,
    document_revision_id TEXT NOT NULL REFERENCES document_revisions (id) ON DELETE CASCADE,
    ordinal INTEGER NOT NULL,
    byte_start BIGINT NOT NULL CHECK (byte_start >= 0),
    byte_end BIGINT NOT NULL CHECK (byte_end >= byte_start),
    checksum TEXT NOT NULL,
    acl TEXT NOT NULL DEFAULT '',
    content TEXT NOT NULL,
    fts tsvector GENERATED ALWAYS AS (to_tsvector('english', content)) STORED,
    created_at TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp()
);
-- The GIN index the FTS @@ match and ts_rank read. Scoped by tenant/kb at query time via RLS + the WHERE.
CREATE INDEX IF NOT EXISTS chunk_revisions_fts_idx ON chunk_revisions USING GIN (fts);
CREATE INDEX IF NOT EXISTS chunk_revisions_docrev_idx
    ON chunk_revisions (organization_id, project_id, document_revision_id);

-- index_revisions: APPEND-ONLY. One immutable KB-wide build snapshot. document_revision_ids is the member
-- set retrieval intersects against (each source's active document_revision at build time), so a rebuild that
-- omits a deleted source's document naturally excludes it from the active index (KNO-004) without mutating
-- any chunk. state is terminal at INSERT: the completeness check runs BEFORE the row is written and a failed
-- build rolls back ENTIRELY (its failure is recorded on ingestion_jobs, never here), so an index_revision row
-- only ever exists as 'active' — a successfully built, activated snapshot.
CREATE TABLE IF NOT EXISTS index_revisions (
    id TEXT PRIMARY KEY,
    organization_id TEXT NOT NULL,
    project_id TEXT NOT NULL,
    knowledge_base_id TEXT NOT NULL REFERENCES knowledge_bases (id) ON DELETE CASCADE,
    version INTEGER NOT NULL,
    state TEXT NOT NULL DEFAULT 'active' CHECK (state = 'active'),
    document_revision_ids TEXT[] NOT NULL DEFAULT '{}',
    chunk_count INTEGER NOT NULL DEFAULT 0,
    created_at TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp(),
    FOREIGN KEY (organization_id, project_id) REFERENCES projects (organization_id, id),
    UNIQUE (organization_id, project_id, knowledge_base_id, version)
);

-- RLS: every table is project-aware (org+project). Born secured here (M3), re-asserted every boot because
-- this migration runs last in the chain.
CALL palai_apply_tenant_policy('knowledge_bases', 'organization_id', true);
CALL palai_apply_tenant_policy('knowledge_sources', 'organization_id', true);
CALL palai_apply_tenant_policy('ingestion_jobs', 'organization_id', true);
CALL palai_apply_tenant_policy('document_revisions', 'organization_id', true);
CALL palai_apply_tenant_policy('chunk_revisions', 'organization_id', true);
CALL palai_apply_tenant_policy('index_revisions', 'organization_id', true);

-- These tables are created after 000029's blanket `GRANT ... ON ALL TABLES`, so that sweep never saw them:
-- without an explicit grant the runtime role fails closed with "permission denied for table" instead of the
-- row-scoped policy. The mutable config tables keep full DML; the three *_revisions tables are append-only.
GRANT SELECT, INSERT, UPDATE, DELETE ON knowledge_bases, knowledge_sources, ingestion_jobs TO palai_app;
GRANT SELECT, INSERT ON document_revisions, chunk_revisions, index_revisions TO palai_app;

-- The load-bearing REVOKE (000031/000032 precedent): main.go re-runs the WHOLE chain every boot, so on boot
-- #2 000001's and 000029's blanket grants re-hand palai_app UPDATE+DELETE on the now-existing revision
-- tables (silent revision-history rewrite/erase). This migration runs LAST, so the REVOKE re-asserts after
-- them every boot and keeps the ingested corpus append-only. A re-ingest appends a version; it never edits.
REVOKE UPDATE, DELETE ON document_revisions, chunk_revisions, index_revisions FROM palai_app;

INSERT INTO schema_migrations (version) VALUES (35) ON CONFLICT DO NOTHING;
