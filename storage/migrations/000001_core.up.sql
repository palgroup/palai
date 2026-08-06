-- 000001 is the WHOLE schema: every table, sequence, constraint, index, grant and the one trigger this
-- installation runs on. It is a BASELINE, not a step -- the chain was squashed from sixty-seven files to
-- two on 2026-08-04, and this file is the first of the two.
--
-- WHY THE SQUASH. Fifty of the old chain's sixty-seven files built a tenant boundary ABOVE the project --
-- a table for it, an id column on eighty-six others, and every row-level-security policy keyed on it -- and
-- the last six files took all of it back out again (plan A.2). A fresh install spent its entire boot
-- constructing a boundary this product does not have and then dismantling it. What survives here is only
-- where that chain ARRIVED: the project is the tenant, and it always was the one being enforced.
--
-- IT IS DERIVED, NOT AUTHORED. The source is a schema dump of a database the shipped control-plane binary
-- migrated through the old sixty-seven-link chain, so "today's schema" is a measurement. The derivation is
-- one command and is meant to be re-run rather than trusted:
--
--     TWICE=1 chain-apply.sh canonical.sql && gen-baseline.py canonical.sql 000001_core.up.sql 000002_row_level_security.up.sql
--
-- EVERY STATEMENT IS IDEMPOTENT, and that is load-bearing rather than tidy: the runner re-applies the FULL
-- chain on every boot, so a statement that cannot run twice is a control-plane that cannot restart. Tables
-- and indexes carry IF NOT EXISTS; constraints and the two identity columns cannot, so each is wrapped in a
-- catalogue check. This tree has broken a second boot once before (90876ab8) -- tests/component/postgres
-- drives a second boot rather than trusting the shape of these statements.
--
-- THE GRANTS ARE THE END PRIVILEGE, NOT A BLANKET FOLLOWED BY TAKE-BACKS. The old chain granted
-- palai_app everything on every table and then had each append-only table REVOKE the writes back, on every
-- boot, forever. Here each table is granted exactly what it should hold: audit_events, capability_jobs,
-- queue_effect_receipts, runner_enrollments, schema_revisions and usage_ledger get SELECT and INSERT and
-- nothing else, which is the same end state the revokes produced and one fewer moving part.
--
-- ROW-LEVEL SECURITY IS NOT HERE. It is 000002, and the split is the chain's own seam: policies can only be
-- applied to tables that exist, which is why the old chain put its sweep at 000029 and not at 000001. The
-- split also keeps an interrupted boot observable -- a chain of one migration has no "partway" for the
-- resume drill (OPS-006) to be about.


-- palai_app is the non-owner role every application connection runs as (storage.RuntimeRole); 000002's
-- policies are inert for an owner or a superuser, so the role is what makes them apply at all. It is a
-- CLUSTER object rather than a database one, which is why it is authored here instead of derived: a schema
-- dump does not carry it. NOLOGIN and no password -- the pool reaches it with SET ROLE, never by dialling in.
DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'palai_app') THEN
        CREATE ROLE palai_app;
    END IF;
END
$$;

-- ---------------------------------------------------------------- tables

CREATE TABLE IF NOT EXISTS a2a_interfaces (
    id text NOT NULL,
    project_id text NOT NULL,
    name text NOT NULL,
    description text DEFAULT ''::text NOT NULL,
    version text DEFAULT '1'::text NOT NULL,
    agent_profile_id text NOT NULL,
    agent_revision_id text NOT NULL,
    streaming boolean DEFAULT true NOT NULL,
    push_notifications boolean DEFAULT true NOT NULL,
    extended_card boolean DEFAULT true NOT NULL,
    input_modes text[] DEFAULT ARRAY['text/plain'::text] NOT NULL,
    output_modes text[] DEFAULT ARRAY['application/json'::text] NOT NULL,
    skills jsonb DEFAULT '[]'::jsonb NOT NULL,
    auth_scheme text DEFAULT 'bearer'::text NOT NULL,
    published boolean DEFAULT true NOT NULL,
    etag text DEFAULT ''::text NOT NULL,
    created_at timestamp with time zone DEFAULT clock_timestamp() NOT NULL
);

CREATE TABLE IF NOT EXISTS a2a_remote_agents (
    id text NOT NULL,
    project_id text NOT NULL,
    name text NOT NULL,
    card_url text NOT NULL,
    endpoint_url text NOT NULL,
    protocol_version text DEFAULT '1.0'::text NOT NULL,
    auth_connection_ref text DEFAULT ''::text NOT NULL,
    allowed_input_modes text[] DEFAULT ARRAY['text/plain'::text] NOT NULL,
    allowed_output_modes text[] DEFAULT ARRAY['text/plain'::text] NOT NULL,
    allowed_extension_uris text[] DEFAULT ARRAY[]::text[] NOT NULL,
    data_policy text DEFAULT 'minimum'::text NOT NULL,
    max_cost_cents integer DEFAULT 0 NOT NULL,
    timeout_ms integer DEFAULT 30000 NOT NULL,
    max_output_bytes integer DEFAULT 1048576 NOT NULL,
    enabled boolean DEFAULT true NOT NULL,
    created_at timestamp with time zone DEFAULT clock_timestamp() NOT NULL,
    updated_at timestamp with time zone DEFAULT clock_timestamp() NOT NULL
);

CREATE TABLE IF NOT EXISTS a2a_task_refs (
    id text NOT NULL,
    project_id text NOT NULL,
    interface_id text NOT NULL,
    a2a_task_id text NOT NULL,
    a2a_context_id text NOT NULL,
    run_id text NOT NULL,
    session_id text NOT NULL,
    push_configs jsonb DEFAULT '[]'::jsonb NOT NULL,
    created_at timestamp with time zone DEFAULT clock_timestamp() NOT NULL,
    updated_at timestamp with time zone DEFAULT clock_timestamp() NOT NULL
);

CREATE TABLE IF NOT EXISTS agent_profiles (
    id text NOT NULL,
    project_id text NOT NULL,
    name text NOT NULL,
    created_at timestamp with time zone DEFAULT clock_timestamp() NOT NULL
);

CREATE TABLE IF NOT EXISTS agent_revisions (
    id text NOT NULL,
    project_id text NOT NULL,
    profile_id text NOT NULL,
    revision_number integer NOT NULL,
    model text DEFAULT ''::text NOT NULL,
    tools jsonb,
    instructions text DEFAULT ''::text NOT NULL,
    published_at timestamp with time zone,
    created_at timestamp with time zone DEFAULT clock_timestamp() NOT NULL,
    tool_sets jsonb,
    mcp_connections jsonb,
    skills jsonb,
    hooks jsonb,
    environment text DEFAULT ''::text NOT NULL
);

CREATE TABLE IF NOT EXISTS api_keys (
    id text NOT NULL,
    project_id text,
    principal_id text NOT NULL,
    key_hash text NOT NULL,
    created_at timestamp with time zone DEFAULT clock_timestamp() NOT NULL,
    revoked_at timestamp with time zone,
    scopes text[] DEFAULT '{}'::text[] NOT NULL,
    expires_at timestamp with time zone
);

CREATE TABLE IF NOT EXISTS approvals (
    id text NOT NULL,
    publication_id text,
    project_id text NOT NULL,
    request_hash text NOT NULL,
    allowed_approver text DEFAULT ''::text NOT NULL,
    decided_by text DEFAULT ''::text NOT NULL,
    expires_at timestamp with time zone,
    created_at timestamp with time zone DEFAULT clock_timestamp() NOT NULL,
    updated_at timestamp with time zone DEFAULT clock_timestamp() NOT NULL,
    tool_call_id text,
    CONSTRAINT approvals_one_target CHECK (((publication_id IS NULL) <> (tool_call_id IS NULL)))
);

CREATE TABLE IF NOT EXISTS artifacts (
    id text NOT NULL,
    project_id text NOT NULL,
    run_id text,
    object_key text NOT NULL,
    size_bytes bigint DEFAULT 0 NOT NULL,
    checksum text DEFAULT ''::text NOT NULL,
    created_at timestamp with time zone DEFAULT clock_timestamp() NOT NULL,
    media_type text DEFAULT ''::text NOT NULL,
    logical_type text DEFAULT ''::text NOT NULL,
    malware_scan_status text DEFAULT 'not_scanned'::text NOT NULL,
    provenance jsonb DEFAULT '{}'::jsonb NOT NULL,
    CONSTRAINT artifacts_size_bytes_check CHECK ((size_bytes >= 0))
);

CREATE TABLE IF NOT EXISTS attempts (
    id text NOT NULL,
    project_id text NOT NULL,
    run_id text NOT NULL,
    fence bigint NOT NULL,
    state text DEFAULT 'assigned'::text NOT NULL,
    created_at timestamp with time zone DEFAULT clock_timestamp() NOT NULL,
    updated_at timestamp with time zone DEFAULT clock_timestamp() NOT NULL,
    CONSTRAINT attempts_fence_check CHECK ((fence >= 1))
);

CREATE TABLE IF NOT EXISTS audit_events (
    id bigint NOT NULL,
    project_id text,
    actor text NOT NULL,
    action text NOT NULL,
    outcome text NOT NULL,
    resource text DEFAULT ''::text NOT NULL,
    detail jsonb DEFAULT '{}'::jsonb NOT NULL,
    created_at timestamp with time zone DEFAULT clock_timestamp() NOT NULL
);

CREATE TABLE IF NOT EXISTS background_tasks (
    id text NOT NULL,
    project_id text NOT NULL,
    run_id text NOT NULL,
    session_id text NOT NULL,
    response_id text NOT NULL,
    tool_call_id text NOT NULL,
    attempt_fence bigint DEFAULT 0 NOT NULL,
    posture text NOT NULL,
    handle text NOT NULL,
    state text NOT NULL,
    exit_code integer,
    output_path text NOT NULL,
    env_keys text[] DEFAULT '{}'::text[] NOT NULL,
    deadline_at timestamp with time zone,
    started_at timestamp with time zone DEFAULT clock_timestamp() NOT NULL,
    finished_at timestamp with time zone,
    notified_at timestamp with time zone,
    machine_id text DEFAULT ''::text NOT NULL,
    CONSTRAINT background_tasks_attempt_fence_check CHECK ((attempt_fence >= 0)),
    CONSTRAINT background_tasks_posture_check CHECK ((posture = ANY (ARRAY['sandboxed-linux'::text, 'unsandboxed-host'::text]))),
    CONSTRAINT background_tasks_state_check CHECK ((state = ANY (ARRAY['running'::text, 'exited'::text, 'killed'::text, 'expired'::text, 'lost'::text])))
);

CREATE TABLE IF NOT EXISTS budgets (
    id text NOT NULL,
    project_id text DEFAULT ''::text NOT NULL,
    meter_prefix text NOT NULL,
    limit_quantity numeric NOT NULL,
    period_start timestamp with time zone DEFAULT clock_timestamp() NOT NULL,
    created_at timestamp with time zone DEFAULT clock_timestamp() NOT NULL,
    updated_at timestamp with time zone DEFAULT clock_timestamp() NOT NULL,
    CONSTRAINT budgets_limit_quantity_check CHECK ((limit_quantity > (0)::numeric))
);

CREATE TABLE IF NOT EXISTS capability_jobs (
    id text NOT NULL,
    project_id text NOT NULL,
    job_id text NOT NULL,
    entry_seq integer NOT NULL,
    entry_kind text NOT NULL,
    idempotency_key text DEFAULT ''::text NOT NULL,
    run_id text DEFAULT ''::text NOT NULL,
    attempt_id text DEFAULT ''::text NOT NULL,
    worker_id text DEFAULT ''::text NOT NULL,
    capability text DEFAULT ''::text NOT NULL,
    operation text DEFAULT ''::text NOT NULL,
    input_refs jsonb DEFAULT '[]'::jsonb NOT NULL,
    secret_handle_refs jsonb DEFAULT '[]'::jsonb NOT NULL,
    deadline_at timestamp with time zone,
    resource_limits jsonb DEFAULT '{}'::jsonb NOT NULL,
    output_schema jsonb DEFAULT '{}'::jsonb NOT NULL,
    network_policy jsonb DEFAULT '{}'::jsonb NOT NULL,
    side_effect_key text DEFAULT ''::text NOT NULL,
    fence_token bigint DEFAULT 1 NOT NULL,
    receipt jsonb DEFAULT '{}'::jsonb NOT NULL,
    created_at timestamp with time zone DEFAULT clock_timestamp() NOT NULL,
    CONSTRAINT capability_jobs_entry_kind_check CHECK ((entry_kind = ANY (ARRAY['dispatched'::text, 'leased'::text, 'progress'::text, 'completed'::text, 'failed'::text, 'quarantined'::text]))),
    CONSTRAINT capability_jobs_entry_seq_check CHECK ((entry_seq > 0)),
    CONSTRAINT capability_jobs_fence_token_check CHECK ((fence_token > 0))
);

CREATE TABLE IF NOT EXISTS capability_workers (
    id text NOT NULL,
    project_id text NOT NULL,
    capability text NOT NULL,
    capability_version text DEFAULT '0.1.0'::text NOT NULL,
    os text DEFAULT ''::text NOT NULL,
    arch text DEFAULT ''::text NOT NULL,
    toolchain_digests jsonb DEFAULT '{}'::jsonb NOT NULL,
    capacity integer DEFAULT 1 NOT NULL,
    pool_label text DEFAULT ''::text NOT NULL,
    trust_label text DEFAULT 'sandbox'::text NOT NULL,
    health text DEFAULT 'healthy'::text NOT NULL,
    lease_fence bigint DEFAULT 1 NOT NULL,
    enrolled_at timestamp with time zone DEFAULT clock_timestamp() NOT NULL,
    last_heartbeat_at timestamp with time zone DEFAULT clock_timestamp() NOT NULL,
    CONSTRAINT capability_workers_capacity_check CHECK ((capacity > 0)),
    CONSTRAINT capability_workers_health_check CHECK ((health = ANY (ARRAY['healthy'::text, 'draining'::text, 'unhealthy'::text]))),
    CONSTRAINT capability_workers_lease_fence_check CHECK ((lease_fence > 0))
);

CREATE TABLE IF NOT EXISTS changeset_findings (
    id text NOT NULL,
    changeset_id text NOT NULL,
    project_id text NOT NULL,
    kind text DEFAULT 'secret'::text NOT NULL,
    path text DEFAULT ''::text NOT NULL,
    rule text DEFAULT ''::text NOT NULL,
    created_at timestamp with time zone DEFAULT clock_timestamp() NOT NULL
);

CREATE TABLE IF NOT EXISTS changesets (
    id text NOT NULL,
    project_id text NOT NULL,
    run_id text NOT NULL,
    base_commit text DEFAULT ''::text NOT NULL,
    final_commit text DEFAULT ''::text NOT NULL,
    final_tree text DEFAULT ''::text NOT NULL,
    files jsonb DEFAULT '[]'::jsonb NOT NULL,
    patch_artifact_id text,
    test_log_artifact_id text,
    patch_truncated boolean DEFAULT false NOT NULL,
    content_hash text NOT NULL,
    created_at timestamp with time zone DEFAULT clock_timestamp() NOT NULL,
    ignored_file_count integer DEFAULT 0 NOT NULL
);

CREATE TABLE IF NOT EXISTS checkpoints (
    id text NOT NULL,
    run_id text NOT NULL,
    attempt_id text NOT NULL,
    boundary_id text NOT NULL,
    project_id text NOT NULL,
    engine_digest text DEFAULT ''::text NOT NULL,
    engine_version text DEFAULT ''::text NOT NULL,
    protocol_version text DEFAULT ''::text NOT NULL,
    format text NOT NULL,
    format_version integer NOT NULL,
    config_snapshot_hash text DEFAULT ''::text NOT NULL,
    transcript_sequence bigint NOT NULL,
    workspace_snapshot_id text,
    pending_operations jsonb DEFAULT '[]'::jsonb NOT NULL,
    content_checksum text NOT NULL,
    object_key text NOT NULL,
    size_bytes bigint NOT NULL,
    created_at timestamp with time zone DEFAULT clock_timestamp() NOT NULL
);

CREATE TABLE IF NOT EXISTS chunk_revisions (
    id text NOT NULL,
    project_id text NOT NULL,
    knowledge_base_id text NOT NULL,
    source_id text NOT NULL,
    document_revision_id text NOT NULL,
    ordinal integer NOT NULL,
    byte_start bigint NOT NULL,
    byte_end bigint NOT NULL,
    checksum text NOT NULL,
    acl text DEFAULT ''::text NOT NULL,
    content text NOT NULL,
    fts tsvector GENERATED ALWAYS AS (to_tsvector('english'::regconfig, content)) STORED,
    created_at timestamp with time zone DEFAULT clock_timestamp() NOT NULL,
    CONSTRAINT chunk_revisions_byte_start_check CHECK ((byte_start >= 0)),
    CONSTRAINT chunk_revisions_check CHECK ((byte_end >= byte_start))
);

CREATE TABLE IF NOT EXISTS commands (
    id text NOT NULL,
    project_id text NOT NULL,
    session_id text NOT NULL,
    run_id text,
    kind text NOT NULL,
    delivery text,
    payload jsonb DEFAULT '{}'::jsonb NOT NULL,
    state text DEFAULT 'queued'::text NOT NULL,
    applied_sequence bigint,
    result jsonb,
    created_at timestamp with time zone DEFAULT clock_timestamp() NOT NULL,
    updated_at timestamp with time zone DEFAULT clock_timestamp() NOT NULL
);

CREATE TABLE IF NOT EXISTS config_revisions (
    id text NOT NULL,
    project_id text NOT NULL,
    session_id text NOT NULL,
    command_id text,
    sequence bigint NOT NULL,
    model text DEFAULT ''::text NOT NULL,
    tools jsonb,
    snapshot_hash text NOT NULL,
    immediate boolean DEFAULT false NOT NULL,
    created_at timestamp with time zone DEFAULT clock_timestamp() NOT NULL
);

CREATE TABLE IF NOT EXISTS delivered_messages (
    command_id text NOT NULL,
    project_id text NOT NULL,
    run_id text NOT NULL,
    boundary_request_id text,
    applied_sequence bigint NOT NULL,
    fold_state text DEFAULT 'delivered'::text NOT NULL,
    created_at timestamp with time zone DEFAULT clock_timestamp() NOT NULL,
    updated_at timestamp with time zone DEFAULT clock_timestamp() NOT NULL
);

CREATE TABLE IF NOT EXISTS delivery_attempts (
    id bigint NOT NULL,
    delivery_id text NOT NULL,
    attempt_number integer NOT NULL,
    status_code integer DEFAULT 0 NOT NULL,
    duration_ms bigint DEFAULT 0 NOT NULL,
    response_excerpt text DEFAULT ''::text NOT NULL,
    error text DEFAULT ''::text NOT NULL,
    created_at timestamp with time zone DEFAULT clock_timestamp() NOT NULL,
    CONSTRAINT delivery_attempts_attempt_number_check CHECK ((attempt_number >= 1))
);

CREATE TABLE IF NOT EXISTS deployment_desired (
    revision bigint NOT NULL,
    plane text NOT NULL,
    scope_id text DEFAULT ''::text NOT NULL,
    document jsonb NOT NULL,
    written_at timestamp with time zone DEFAULT clock_timestamp() NOT NULL,
    written_by text NOT NULL,
    CONSTRAINT deployment_desired_document_is_object CHECK ((jsonb_typeof(document) = 'object'::text)),
    CONSTRAINT deployment_desired_plane_check CHECK ((plane = ANY (ARRAY['control_plane'::text, 'runner_pool'::text, 'runner_machine'::text]))),
    CONSTRAINT deployment_desired_scope_shape_check CHECK ((((plane = 'control_plane'::text) AND (scope_id = ''::text)) OR ((plane = 'runner_pool'::text) AND (scope_id <> ''::text)) OR ((plane = 'runner_machine'::text) AND (scope_id <> ''::text))))
);

CREATE TABLE IF NOT EXISTS document_revisions (
    id text NOT NULL,
    project_id text NOT NULL,
    knowledge_base_id text NOT NULL,
    source_id text NOT NULL,
    version integer NOT NULL,
    checksum text NOT NULL,
    byte_size bigint NOT NULL,
    object_key text NOT NULL,
    content text NOT NULL,
    parser text NOT NULL,
    provenance jsonb DEFAULT '{}'::jsonb NOT NULL,
    created_at timestamp with time zone DEFAULT clock_timestamp() NOT NULL,
    CONSTRAINT document_revisions_byte_size_check CHECK ((byte_size >= 0))
);

CREATE TABLE IF NOT EXISTS durable_jobs (
    id text NOT NULL,
    project_id text NOT NULL,
    kind text DEFAULT ''::text NOT NULL,
    status text DEFAULT 'queued'::text NOT NULL,
    lease_owner text,
    lease_expires_at timestamp with time zone,
    fence bigint DEFAULT 0 NOT NULL,
    attempt_count integer DEFAULT 0 NOT NULL,
    ready_at timestamp with time zone DEFAULT clock_timestamp() NOT NULL,
    payload jsonb DEFAULT '{}'::jsonb NOT NULL,
    result_hash text,
    created_at timestamp with time zone DEFAULT clock_timestamp() NOT NULL,
    updated_at timestamp with time zone DEFAULT clock_timestamp() NOT NULL,
    CONSTRAINT durable_jobs_attempt_count_check CHECK ((attempt_count >= 0)),
    CONSTRAINT durable_jobs_fence_check CHECK ((fence >= 0)),
    CONSTRAINT durable_jobs_status_check CHECK ((status = ANY (ARRAY['queued'::text, 'running'::text, 'completed'::text, 'failed'::text, 'dead'::text])))
);

CREATE TABLE IF NOT EXISTS environment_values (
    environment_id text NOT NULL,
    key text NOT NULL,
    created_at timestamp with time zone DEFAULT clock_timestamp() NOT NULL
);

CREATE TABLE IF NOT EXISTS environments (
    id text NOT NULL,
    name text NOT NULL,
    description text DEFAULT ''::text NOT NULL,
    created_at timestamp with time zone DEFAULT clock_timestamp() NOT NULL
);

CREATE TABLE IF NOT EXISTS events (
    id text NOT NULL,
    project_id text NOT NULL,
    session_id text NOT NULL,
    seq bigint NOT NULL,
    type text NOT NULL,
    payload jsonb DEFAULT '{}'::jsonb NOT NULL,
    created_at timestamp with time zone DEFAULT clock_timestamp() NOT NULL,
    response_id text,
    journal_id bigint NOT NULL,
    CONSTRAINT events_seq_check CHECK ((seq >= 1))
);

CREATE TABLE IF NOT EXISTS hooks (
    id text NOT NULL,
    project_id text NOT NULL,
    name text NOT NULL,
    hook_point text NOT NULL,
    category text NOT NULL,
    executor text NOT NULL,
    config jsonb NOT NULL,
    secret_ref text,
    timeout_ms integer,
    disabled_at timestamp with time zone,
    created_at timestamp with time zone DEFAULT clock_timestamp() NOT NULL
);

CREATE TABLE IF NOT EXISTS host_quarantine (
    host_id text NOT NULL,
    reason text DEFAULT ''::text NOT NULL,
    quarantined_at timestamp with time zone DEFAULT clock_timestamp() NOT NULL
);

CREATE TABLE IF NOT EXISTS idempotency_records (
    id bigint NOT NULL,
    project_id text NOT NULL,
    principal_id text NOT NULL,
    method text NOT NULL,
    route text NOT NULL,
    idempotency_key text NOT NULL,
    request_hash text NOT NULL,
    status text NOT NULL,
    response_body jsonb,
    created_at timestamp with time zone DEFAULT clock_timestamp() NOT NULL,
    result_purged_at timestamp with time zone,
    resource_tombstone text,
    outcome_fingerprint text
);

CREATE TABLE IF NOT EXISTS inbox (
    id bigint NOT NULL,
    project_id text NOT NULL,
    source text NOT NULL,
    operation_id text NOT NULL,
    created_at timestamp with time zone DEFAULT clock_timestamp() NOT NULL
);

CREATE TABLE IF NOT EXISTS index_revisions (
    id text NOT NULL,
    project_id text NOT NULL,
    knowledge_base_id text NOT NULL,
    version integer NOT NULL,
    state text DEFAULT 'active'::text NOT NULL,
    document_revision_ids text[] DEFAULT '{}'::text[] NOT NULL,
    chunk_count integer DEFAULT 0 NOT NULL,
    created_at timestamp with time zone DEFAULT clock_timestamp() NOT NULL,
    CONSTRAINT index_revisions_state_check CHECK ((state = 'active'::text))
);

CREATE TABLE IF NOT EXISTS ingestion_jobs (
    id text NOT NULL,
    project_id text NOT NULL,
    knowledge_base_id text NOT NULL,
    source_id text NOT NULL,
    state text DEFAULT 'pending'::text NOT NULL,
    document_revision_id text,
    index_revision_id text,
    error text DEFAULT ''::text NOT NULL,
    created_at timestamp with time zone DEFAULT clock_timestamp() NOT NULL,
    updated_at timestamp with time zone DEFAULT clock_timestamp() NOT NULL,
    CONSTRAINT ingestion_jobs_state_check CHECK ((state = ANY (ARRAY['pending'::text, 'running'::text, 'succeeded'::text, 'failed'::text])))
);

CREATE TABLE IF NOT EXISTS integration_bots (
    id text NOT NULL,
    project_id text NOT NULL,
    name text NOT NULL,
    kind text NOT NULL,
    agent_revision_id text DEFAULT ''::text NOT NULL,
    repository_binding_id text DEFAULT ''::text NOT NULL,
    principal_id text DEFAULT ''::text NOT NULL,
    config json DEFAULT '{}'::json NOT NULL,
    disabled boolean DEFAULT false NOT NULL,
    created_at timestamp with time zone DEFAULT clock_timestamp() NOT NULL
);

CREATE TABLE IF NOT EXISTS job_attempts (
    id bigint NOT NULL,
    job_id text NOT NULL,
    fence bigint NOT NULL,
    owner text NOT NULL,
    started_at timestamp with time zone DEFAULT clock_timestamp() NOT NULL,
    outcome text,
    error text
);

CREATE TABLE IF NOT EXISTS knowledge_bases (
    id text NOT NULL,
    project_id text NOT NULL,
    name text NOT NULL,
    embedding_route text DEFAULT ''::text NOT NULL,
    active_index_revision_id text,
    created_at timestamp with time zone DEFAULT clock_timestamp() NOT NULL,
    updated_at timestamp with time zone DEFAULT clock_timestamp() NOT NULL
);

CREATE TABLE IF NOT EXISTS knowledge_sources (
    id text NOT NULL,
    project_id text NOT NULL,
    knowledge_base_id text NOT NULL,
    kind text NOT NULL,
    uri text NOT NULL,
    acl text DEFAULT ''::text NOT NULL,
    classification text DEFAULT ''::text NOT NULL,
    parser text DEFAULT 'text'::text NOT NULL,
    created_at timestamp with time zone DEFAULT clock_timestamp() NOT NULL,
    CONSTRAINT knowledge_sources_kind_check CHECK ((kind = ANY (ARRAY['artifact'::text, 'repository'::text]))),
    CONSTRAINT knowledge_sources_parser_check CHECK ((parser = ANY (ARRAY['text'::text, 'markdown'::text, 'code'::text])))
);

CREATE TABLE IF NOT EXISTS mcp_connections (
    id text NOT NULL,
    project_id text NOT NULL,
    name text NOT NULL,
    transport text NOT NULL,
    config jsonb NOT NULL,
    secret_ref text,
    trust_level text DEFAULT 'untrusted'::text NOT NULL,
    disabled_at timestamp with time zone,
    created_at timestamp with time zone DEFAULT clock_timestamp() NOT NULL
);

CREATE TABLE IF NOT EXISTS merge_records (
    id text NOT NULL,
    project_id text NOT NULL,
    parent_run_id text NOT NULL,
    source_child_run_id text NOT NULL,
    child_branch text NOT NULL,
    merged boolean NOT NULL,
    merge_commit text DEFAULT ''::text NOT NULL,
    conflict_paths jsonb DEFAULT '[]'::jsonb NOT NULL,
    created_at timestamp with time zone DEFAULT clock_timestamp() NOT NULL
);

CREATE TABLE IF NOT EXISTS messages (
    id text NOT NULL,
    project_id text NOT NULL,
    session_id text NOT NULL,
    role text NOT NULL,
    content jsonb DEFAULT '{}'::jsonb NOT NULL,
    created_at timestamp with time zone DEFAULT clock_timestamp() NOT NULL
);

CREATE TABLE IF NOT EXISTS model_connections (
    id text NOT NULL,
    project_id text,
    provider text NOT NULL,
    secret_ref text NOT NULL,
    created_at timestamp with time zone DEFAULT clock_timestamp() NOT NULL,
    base_url text DEFAULT ''::text NOT NULL,
    verified_at timestamp with time zone,
    verification_outcome text DEFAULT ''::text NOT NULL
);

CREATE TABLE IF NOT EXISTS model_requests (
    id text NOT NULL,
    project_id text NOT NULL,
    run_id text NOT NULL,
    state text DEFAULT 'requested'::text NOT NULL,
    result jsonb,
    created_at timestamp with time zone DEFAULT clock_timestamp() NOT NULL,
    updated_at timestamp with time zone DEFAULT clock_timestamp() NOT NULL
);

CREATE TABLE IF NOT EXISTS model_route_revisions (
    id text NOT NULL,
    route_id text NOT NULL,
    revision integer NOT NULL,
    config jsonb DEFAULT '{}'::jsonb NOT NULL,
    created_at timestamp with time zone DEFAULT clock_timestamp() NOT NULL,
    CONSTRAINT model_route_revisions_revision_check CHECK ((revision >= 1))
);

CREATE TABLE IF NOT EXISTS model_routes (
    id text NOT NULL,
    project_id text NOT NULL,
    name text NOT NULL,
    created_at timestamp with time zone DEFAULT clock_timestamp() NOT NULL
);

CREATE TABLE IF NOT EXISTS outbox (
    id bigint NOT NULL,
    project_id text NOT NULL,
    topic text NOT NULL,
    dedupe_key text NOT NULL,
    payload jsonb DEFAULT '{}'::jsonb NOT NULL,
    dispatched_at timestamp with time zone,
    created_at timestamp with time zone DEFAULT clock_timestamp() NOT NULL
);

CREATE TABLE IF NOT EXISTS preparation_receipts (
    id text NOT NULL,
    repository_binding_id text NOT NULL,
    project_id text NOT NULL,
    run_id text,
    requested_ref text DEFAULT ''::text NOT NULL,
    base_commit text NOT NULL,
    tree_hash text NOT NULL,
    branch text DEFAULT ''::text NOT NULL,
    prepared_at timestamp with time zone DEFAULT clock_timestamp() NOT NULL,
    created_at timestamp with time zone DEFAULT clock_timestamp() NOT NULL
);

CREATE TABLE IF NOT EXISTS principals (
    id text NOT NULL,
    project_id text,
    kind text NOT NULL,
    created_at timestamp with time zone DEFAULT clock_timestamp() NOT NULL
);

CREATE TABLE IF NOT EXISTS projects (
    id text NOT NULL,
    display_name text DEFAULT ''::text NOT NULL,
    created_at timestamp with time zone DEFAULT clock_timestamp() NOT NULL,
    updated_at timestamp with time zone DEFAULT clock_timestamp() NOT NULL,
    config_policy jsonb
);

CREATE TABLE IF NOT EXISTS publications (
    id text NOT NULL,
    project_id text NOT NULL,
    session_id text NOT NULL,
    run_id text NOT NULL,
    response_id text,
    operation text NOT NULL,
    remote text DEFAULT ''::text NOT NULL,
    branch text DEFAULT ''::text NOT NULL,
    base text DEFAULT ''::text NOT NULL,
    head_sha text DEFAULT ''::text NOT NULL,
    idempotency_key text NOT NULL,
    display text DEFAULT ''::text NOT NULL,
    args jsonb DEFAULT '{}'::jsonb NOT NULL,
    state text DEFAULT 'pending_approval'::text NOT NULL,
    receipt jsonb,
    created_at timestamp with time zone DEFAULT clock_timestamp() NOT NULL,
    updated_at timestamp with time zone DEFAULT clock_timestamp() NOT NULL,
    CONSTRAINT publications_operation_check CHECK ((operation = ANY (ARRAY['push_branch'::text, 'open_pull_request'::text, 'merge_pull_request'::text]))),
    CONSTRAINT publications_state_check CHECK ((state = ANY (ARRAY['pending_approval'::text, 'approved'::text, 'published'::text, 'denied'::text, 'expired'::text])))
);

CREATE TABLE IF NOT EXISTS queue_connections (
    id text NOT NULL,
    project_id text NOT NULL,
    name text NOT NULL,
    kind text DEFAULT 'local'::text NOT NULL,
    direction text DEFAULT 'inbound'::text NOT NULL,
    capacity integer DEFAULT 1024 NOT NULL,
    visibility_seconds integer DEFAULT 30 NOT NULL,
    max_deliveries integer DEFAULT 20 NOT NULL,
    enabled boolean DEFAULT true NOT NULL,
    config jsonb DEFAULT '{}'::jsonb NOT NULL,
    created_at timestamp with time zone DEFAULT clock_timestamp() NOT NULL,
    CONSTRAINT queue_connections_capacity_check CHECK ((capacity > 0)),
    CONSTRAINT queue_connections_direction_check CHECK ((direction = ANY (ARRAY['inbound'::text, 'outbound'::text]))),
    CONSTRAINT queue_connections_max_deliveries_check CHECK ((max_deliveries > 0)),
    CONSTRAINT queue_connections_visibility_seconds_check CHECK ((visibility_seconds > 0))
);

CREATE TABLE IF NOT EXISTS queue_deliveries (
    id text NOT NULL,
    project_id text NOT NULL,
    queue_connection_id text NOT NULL,
    destination_key text NOT NULL,
    payload bytea NOT NULL,
    state text DEFAULT 'pending'::text NOT NULL,
    attempt_count integer DEFAULT 0 NOT NULL,
    max_attempts integer DEFAULT 20 NOT NULL,
    next_attempt_at timestamp with time zone DEFAULT clock_timestamp() NOT NULL,
    first_attempt_at timestamp with time zone,
    created_at timestamp with time zone DEFAULT clock_timestamp() NOT NULL,
    updated_at timestamp with time zone DEFAULT clock_timestamp() NOT NULL,
    CONSTRAINT queue_deliveries_attempt_count_check CHECK ((attempt_count >= 0)),
    CONSTRAINT queue_deliveries_max_attempts_check CHECK ((max_attempts > 0)),
    CONSTRAINT queue_deliveries_state_check CHECK ((state = ANY (ARRAY['pending'::text, 'delivered'::text, 'dead'::text])))
);

CREATE TABLE IF NOT EXISTS queue_effect_receipts (
    id text NOT NULL,
    project_id text NOT NULL,
    queue_connection_id text NOT NULL,
    idempotency_key text NOT NULL,
    committed_at timestamp with time zone DEFAULT clock_timestamp() NOT NULL
);

CREATE TABLE IF NOT EXISTS queue_messages (
    id text NOT NULL,
    project_id text NOT NULL,
    queue_connection_id text NOT NULL,
    idempotency_key text NOT NULL,
    body bytea NOT NULL,
    state text DEFAULT 'ready'::text NOT NULL,
    attempt_count integer DEFAULT 0 NOT NULL,
    lease_expires_at timestamp with time zone,
    enqueued_at timestamp with time zone DEFAULT clock_timestamp() NOT NULL,
    updated_at timestamp with time zone DEFAULT clock_timestamp() NOT NULL,
    CONSTRAINT queue_messages_attempt_count_check CHECK ((attempt_count >= 0)),
    CONSTRAINT queue_messages_state_check CHECK ((state = ANY (ARRAY['ready'::text, 'leased'::text, 'acked'::text, 'dead'::text])))
);

CREATE TABLE IF NOT EXISTS quotas (
    id text NOT NULL,
    project_id text DEFAULT ''::text NOT NULL,
    meter_prefix text NOT NULL,
    limit_quantity numeric NOT NULL,
    window_seconds bigint NOT NULL,
    created_at timestamp with time zone DEFAULT clock_timestamp() NOT NULL,
    updated_at timestamp with time zone DEFAULT clock_timestamp() NOT NULL,
    CONSTRAINT quotas_limit_quantity_check CHECK ((limit_quantity > (0)::numeric)),
    CONSTRAINT quotas_window_seconds_check CHECK ((window_seconds > 0))
);

CREATE TABLE IF NOT EXISTS remote_tool_operations (
    id text NOT NULL,
    project_id text NOT NULL,
    tool_call_id text NOT NULL,
    secret_ref text DEFAULT ''::text NOT NULL,
    callback_token_hash text NOT NULL,
    deadline timestamp with time zone NOT NULL,
    state text DEFAULT 'pending'::text NOT NULL,
    external_operation_id text DEFAULT ''::text NOT NULL,
    result jsonb,
    result_hash text DEFAULT ''::text NOT NULL,
    fence bigint DEFAULT 0 NOT NULL,
    created_at timestamp with time zone DEFAULT clock_timestamp() NOT NULL,
    completed_at timestamp with time zone
);

CREATE TABLE IF NOT EXISTS repository_bindings (
    id text NOT NULL,
    project_id text NOT NULL,
    provider text NOT NULL,
    repository_identity text NOT NULL,
    clone_url text NOT NULL,
    default_branch text DEFAULT 'main'::text NOT NULL,
    connection_ref text DEFAULT ''::text NOT NULL,
    allowed_operations jsonb DEFAULT '[]'::jsonb NOT NULL,
    policy jsonb DEFAULT '{}'::jsonb NOT NULL,
    data_classification text DEFAULT ''::text NOT NULL,
    region_constraint text DEFAULT ''::text NOT NULL,
    created_at timestamp with time zone DEFAULT clock_timestamp() NOT NULL,
    archived_at timestamp with time zone
);

CREATE TABLE IF NOT EXISTS responses (
    id text NOT NULL,
    project_id text NOT NULL,
    session_id text NOT NULL,
    state text DEFAULT 'queued'::text NOT NULL,
    input jsonb DEFAULT '{}'::jsonb NOT NULL,
    output jsonb,
    created_at timestamp with time zone DEFAULT clock_timestamp() NOT NULL,
    updated_at timestamp with time zone DEFAULT clock_timestamp() NOT NULL,
    store boolean DEFAULT true NOT NULL,
    purged_at timestamp with time zone,
    retracted_at timestamp with time zone
);

CREATE TABLE IF NOT EXISTS run_template_revisions (
    id text NOT NULL,
    project_id text NOT NULL,
    template_name text NOT NULL,
    revision_number integer NOT NULL,
    model text DEFAULT ''::text NOT NULL,
    tools jsonb,
    instructions text DEFAULT ''::text NOT NULL,
    published_at timestamp with time zone,
    created_at timestamp with time zone DEFAULT clock_timestamp() NOT NULL,
    tool_sets jsonb,
    mcp_connections jsonb,
    skills jsonb,
    hooks jsonb
);

CREATE TABLE IF NOT EXISTS runner_enrollments (
    id text NOT NULL,
    project_id text,
    runner_id text NOT NULL,
    pool_id text NOT NULL,
    key_id text DEFAULT ''::text NOT NULL,
    entry_kind text NOT NULL,
    entry_seq bigint NOT NULL,
    detail jsonb DEFAULT '{}'::jsonb NOT NULL,
    created_at timestamp with time zone DEFAULT clock_timestamp() NOT NULL,
    CONSTRAINT runner_enrollments_entry_kind_check CHECK ((entry_kind = ANY (ARRAY['requested'::text, 'approved'::text, 'refused'::text, 'issued'::text, 'revoked'::text, 'renewed'::text]))),
    CONSTRAINT runner_enrollments_entry_seq_check CHECK ((entry_seq > 0))
);

CREATE TABLE IF NOT EXISTS runner_leases (
    id text NOT NULL,
    project_id text NOT NULL,
    runner_id text NOT NULL,
    run_id text,
    fence bigint DEFAULT 0 NOT NULL,
    expires_at timestamp with time zone,
    created_at timestamp with time zone DEFAULT clock_timestamp() NOT NULL,
    CONSTRAINT runner_leases_fence_check CHECK ((fence >= 0))
);

CREATE TABLE IF NOT EXISTS runner_pool_keys (
    id text NOT NULL,
    project_id text,
    pool_id text NOT NULL,
    key_sha256 text NOT NULL,
    key_prefix text DEFAULT ''::text NOT NULL,
    created_at timestamp with time zone DEFAULT clock_timestamp() NOT NULL,
    expires_at timestamp with time zone,
    revoked_at timestamp with time zone,
    last_used_at timestamp with time zone
);

CREATE TABLE IF NOT EXISTS runner_pools (
    id text NOT NULL,
    project_id text,
    name text DEFAULT ''::text NOT NULL,
    created_at timestamp with time zone DEFAULT clock_timestamp() NOT NULL,
    posture text DEFAULT 'sandboxed-linux'::text NOT NULL,
    os text DEFAULT ''::text NOT NULL,
    arch text DEFAULT ''::text NOT NULL,
    strict_enrollment boolean DEFAULT false NOT NULL,
    CONSTRAINT runner_pools_posture_check CHECK ((posture = ANY (ARRAY['sandboxed-linux'::text, 'unsandboxed-host'::text])))
);

CREATE TABLE IF NOT EXISTS runners (
    id text NOT NULL,
    pool_id text NOT NULL,
    created_at timestamp with time zone DEFAULT clock_timestamp() NOT NULL,
    project_id text,
    label text DEFAULT ''::text NOT NULL,
    runner_dns text DEFAULT ''::text NOT NULL,
    public_key_sha256 text DEFAULT ''::text NOT NULL,
    enrolled_via_key_id text,
    os text DEFAULT ''::text NOT NULL,
    arch text DEFAULT ''::text NOT NULL,
    posture text DEFAULT ''::text NOT NULL,
    capacity integer DEFAULT 1 NOT NULL,
    cert_not_after timestamp with time zone,
    enrolled_at timestamp with time zone DEFAULT clock_timestamp() NOT NULL,
    last_seen_at timestamp with time zone,
    state text DEFAULT 'pending'::text NOT NULL,
    config_revision bigint,
    config_applied jsonb,
    config_reported_at timestamp with time zone,
    CONSTRAINT runners_capacity_check CHECK ((capacity > 0)),
    CONSTRAINT runners_config_applied_is_object CHECK (((config_applied IS NULL) OR (jsonb_typeof(config_applied) = 'object'::text))),
    CONSTRAINT runners_state_check CHECK ((state = ANY (ARRAY['pending'::text, 'active'::text, 'cordoned'::text, 'revoked'::text])))
);

CREATE TABLE IF NOT EXISTS runs (
    id text NOT NULL,
    project_id text NOT NULL,
    session_id text NOT NULL,
    response_id text,
    state text DEFAULT 'queued'::text NOT NULL,
    created_at timestamp with time zone DEFAULT clock_timestamp() NOT NULL,
    updated_at timestamp with time zone DEFAULT clock_timestamp() NOT NULL,
    parent_run_id text,
    depth integer DEFAULT 0 NOT NULL,
    delegation jsonb,
    agent_revision_id text,
    run_template_revision_id text,
    skill_pins jsonb,
    pool_id text,
    output_contract jsonb,
    instructions text
);

CREATE TABLE IF NOT EXISTS schedule_occurrences (
    occurrence_id text NOT NULL,
    schedule_id text NOT NULL,
    schedule_revision integer NOT NULL,
    planned_at timestamp with time zone NOT NULL,
    admitted_at timestamp with time zone,
    state text DEFAULT 'pending'::text NOT NULL,
    delivery_id text DEFAULT ''::text NOT NULL,
    reason text DEFAULT ''::text NOT NULL,
    created_at timestamp with time zone DEFAULT clock_timestamp() NOT NULL,
    CONSTRAINT schedule_occurrences_state_check CHECK ((state = ANY (ARRAY['pending'::text, 'admitted'::text, 'skipped'::text, 'failed'::text])))
);

CREATE TABLE IF NOT EXISTS schedules (
    id text NOT NULL,
    project_id text NOT NULL,
    name text NOT NULL,
    trigger_id text NOT NULL,
    created_by text DEFAULT ''::text NOT NULL,
    kind text DEFAULT 'cron'::text NOT NULL,
    cron_expr text DEFAULT ''::text NOT NULL,
    timezone text NOT NULL,
    one_time_at timestamp with time zone,
    misfire_policy text DEFAULT 'fire_once_now'::text NOT NULL,
    misfire_grace_seconds integer DEFAULT 300 NOT NULL,
    max_catch_up integer DEFAULT 0 NOT NULL,
    jitter_seconds integer DEFAULT 0 NOT NULL,
    starts_at timestamp with time zone,
    ends_at timestamp with time zone,
    status text DEFAULT 'active'::text NOT NULL,
    status_reason text DEFAULT ''::text NOT NULL,
    revision integer DEFAULT 1 NOT NULL,
    next_fire_at timestamp with time zone,
    deleted_at timestamp with time zone,
    created_at timestamp with time zone DEFAULT clock_timestamp() NOT NULL,
    updated_at timestamp with time zone DEFAULT clock_timestamp() NOT NULL,
    CONSTRAINT schedules_jitter_seconds_check CHECK (((jitter_seconds >= 0) AND (jitter_seconds <= 3600))),
    CONSTRAINT schedules_kind_check CHECK ((kind = ANY (ARRAY['cron'::text, 'one_time'::text]))),
    CONSTRAINT schedules_max_catch_up_check CHECK (((max_catch_up >= 0) AND (max_catch_up <= 100))),
    CONSTRAINT schedules_misfire_policy_check CHECK ((misfire_policy = ANY (ARRAY['skip'::text, 'fire_once_now'::text, 'catch_up'::text, 'fail'::text]))),
    CONSTRAINT schedules_status_check CHECK ((status = ANY (ARRAY['active'::text, 'paused'::text, 'failed'::text])))
);

CREATE TABLE IF NOT EXISTS schema_migrations (
    version bigint NOT NULL,
    applied_at timestamp with time zone DEFAULT clock_timestamp() NOT NULL
);

CREATE TABLE IF NOT EXISTS schema_revisions (
    version integer NOT NULL,
    checksum text NOT NULL,
    applied_at timestamp with time zone DEFAULT clock_timestamp() NOT NULL,
    applied_by text NOT NULL
);

CREATE TABLE IF NOT EXISTS secret_refs (
    id text NOT NULL,
    name text NOT NULL,
    version integer NOT NULL,
    ciphertext bytea NOT NULL,
    created_at timestamp with time zone DEFAULT clock_timestamp() NOT NULL
);

CREATE TABLE IF NOT EXISTS session_sequences (
    session_id text NOT NULL,
    last_seq bigint DEFAULT 0 NOT NULL,
    CONSTRAINT session_sequences_last_seq_check CHECK ((last_seq >= 0))
);

CREATE TABLE IF NOT EXISTS sessions (
    id text NOT NULL,
    project_id text NOT NULL,
    state text DEFAULT 'active'::text NOT NULL,
    metadata jsonb DEFAULT '{}'::jsonb NOT NULL,
    created_at timestamp with time zone DEFAULT clock_timestamp() NOT NULL,
    updated_at timestamp with time zone DEFAULT clock_timestamp() NOT NULL,
    active_root_run_id text,
    name text DEFAULT ''::text NOT NULL,
    auto_approve_tools boolean DEFAULT false NOT NULL,
    auto_approve_publications boolean DEFAULT false NOT NULL,
    auto_approve_set_by text DEFAULT ''::text NOT NULL,
    auto_approve_set_at timestamp with time zone
);

CREATE TABLE IF NOT EXISTS skill_revisions (
    id text NOT NULL,
    project_id text NOT NULL,
    skill_id text NOT NULL,
    revision_number integer NOT NULL,
    digest text NOT NULL,
    state text DEFAULT 'quarantined'::text NOT NULL,
    scan_findings jsonb,
    metadata jsonb,
    archive bytea NOT NULL,
    source_url text,
    created_at timestamp with time zone DEFAULT clock_timestamp() NOT NULL,
    CONSTRAINT skill_revisions_state_check CHECK ((state = ANY (ARRAY['quarantined'::text, 'approved'::text, 'enabled'::text])))
);

CREATE TABLE IF NOT EXISTS skills (
    id text NOT NULL,
    project_id text NOT NULL,
    name text NOT NULL,
    created_at timestamp with time zone DEFAULT clock_timestamp() NOT NULL
);

CREATE TABLE IF NOT EXISTS slack_approval_deliveries (
    id text NOT NULL,
    project_id text NOT NULL,
    connection_id text NOT NULL,
    approval_id text NOT NULL,
    run_id text NOT NULL,
    response_id text DEFAULT ''::text NOT NULL,
    channel_id text NOT NULL,
    thread_ts text NOT NULL,
    state text DEFAULT 'pending'::text NOT NULL,
    attempt_count integer DEFAULT 0 NOT NULL,
    max_attempts integer DEFAULT 8 NOT NULL,
    next_attempt_at timestamp with time zone DEFAULT clock_timestamp() NOT NULL,
    message_ts text DEFAULT ''::text NOT NULL,
    created_at timestamp with time zone DEFAULT clock_timestamp() NOT NULL,
    updated_at timestamp with time zone DEFAULT clock_timestamp() NOT NULL,
    CONSTRAINT slack_approval_deliveries_attempt_count_check CHECK ((attempt_count >= 0)),
    CONSTRAINT slack_approval_deliveries_max_attempts_check CHECK ((max_attempts > 0)),
    CONSTRAINT slack_approval_deliveries_state_check CHECK ((state = ANY (ARRAY['pending'::text, 'delivered'::text, 'dead'::text])))
);

CREATE TABLE IF NOT EXISTS slack_connections (
    id text NOT NULL,
    project_id text NOT NULL,
    team_id text NOT NULL,
    enterprise_id text DEFAULT ''::text NOT NULL,
    bot_user_id text DEFAULT ''::text NOT NULL,
    signing_secret_ref text NOT NULL,
    bot_token_ref text DEFAULT ''::text NOT NULL,
    app_token_ref text DEFAULT ''::text NOT NULL,
    scopes text DEFAULT ''::text NOT NULL,
    allowed_channels jsonb DEFAULT '[]'::jsonb NOT NULL,
    allowed_users jsonb DEFAULT '[]'::jsonb NOT NULL,
    default_policy jsonb DEFAULT '{}'::jsonb NOT NULL,
    disabled boolean DEFAULT false NOT NULL,
    created_at timestamp with time zone DEFAULT clock_timestamp() NOT NULL
);

CREATE TABLE IF NOT EXISTS slack_message_turns (
    id text NOT NULL,
    project_id text NOT NULL,
    connection_id text NOT NULL,
    team_id text NOT NULL,
    channel_id text NOT NULL,
    message_ts text NOT NULL,
    response_id text NOT NULL,
    session_id text NOT NULL,
    created_at timestamp with time zone DEFAULT clock_timestamp() NOT NULL,
    requester_user_id text DEFAULT ''::text NOT NULL
);

CREATE TABLE IF NOT EXISTS slack_reply_deliveries (
    id text NOT NULL,
    project_id text NOT NULL,
    connection_id text NOT NULL,
    run_id text NOT NULL,
    response_id text DEFAULT ''::text NOT NULL,
    channel_id text NOT NULL,
    thread_ts text NOT NULL,
    run_state text NOT NULL,
    state text DEFAULT 'pending'::text NOT NULL,
    attempt_count integer DEFAULT 0 NOT NULL,
    max_attempts integer DEFAULT 8 NOT NULL,
    next_attempt_at timestamp with time zone DEFAULT clock_timestamp() NOT NULL,
    message_ts text DEFAULT ''::text NOT NULL,
    created_at timestamp with time zone DEFAULT clock_timestamp() NOT NULL,
    updated_at timestamp with time zone DEFAULT clock_timestamp() NOT NULL,
    requester_user_id text DEFAULT ''::text NOT NULL,
    CONSTRAINT slack_reply_deliveries_attempt_count_check CHECK ((attempt_count >= 0)),
    CONSTRAINT slack_reply_deliveries_max_attempts_check CHECK ((max_attempts > 0)),
    CONSTRAINT slack_reply_deliveries_state_check CHECK ((state = ANY (ARRAY['pending'::text, 'delivered'::text, 'dead'::text])))
);

CREATE TABLE IF NOT EXISTS slack_thread_sessions (
    id text NOT NULL,
    project_id text NOT NULL,
    connection_id text NOT NULL,
    team_id text NOT NULL,
    channel_id text NOT NULL,
    thread_ts text NOT NULL,
    session_id text NOT NULL,
    last_bot_message_ts text DEFAULT ''::text NOT NULL,
    created_at timestamp with time zone DEFAULT clock_timestamp() NOT NULL
);

CREATE TABLE IF NOT EXISTS tasks (
    id text NOT NULL,
    project_id text NOT NULL,
    session_id text NOT NULL,
    task_key text NOT NULL,
    kind text DEFAULT 'task'::text NOT NULL,
    title text DEFAULT ''::text NOT NULL,
    status text DEFAULT 'open'::text NOT NULL,
    detail jsonb DEFAULT '{}'::jsonb NOT NULL,
    created_at timestamp with time zone DEFAULT clock_timestamp() NOT NULL,
    updated_at timestamp with time zone DEFAULT clock_timestamp() NOT NULL
);

CREATE TABLE IF NOT EXISTS tool_calls (
    id text NOT NULL,
    project_id text NOT NULL,
    run_id text NOT NULL,
    fence bigint DEFAULT 0 NOT NULL,
    state text DEFAULT 'pending'::text NOT NULL,
    name text NOT NULL,
    arguments jsonb DEFAULT '{}'::jsonb NOT NULL,
    result jsonb,
    created_at timestamp with time zone DEFAULT clock_timestamp() NOT NULL,
    updated_at timestamp with time zone DEFAULT clock_timestamp() NOT NULL,
    replay_class text DEFAULT 'pure'::text NOT NULL,
    request_hash text DEFAULT ''::text NOT NULL,
    external_idempotency_key text DEFAULT ''::text NOT NULL,
    lease_owner text DEFAULT ''::text NOT NULL,
    reconciliation_state text DEFAULT ''::text NOT NULL,
    commit_boundary text DEFAULT ''::text NOT NULL,
    CONSTRAINT tool_calls_fence_check CHECK ((fence >= 0))
);

CREATE TABLE IF NOT EXISTS tool_revisions (
    id text NOT NULL,
    project_id text NOT NULL,
    tool_id text NOT NULL,
    revision_number integer NOT NULL,
    executor text NOT NULL,
    description text DEFAULT ''::text NOT NULL,
    input_schema jsonb NOT NULL,
    output_schema jsonb,
    replay_class text DEFAULT 'pure'::text NOT NULL,
    timeout_ms integer,
    limits jsonb,
    executor_config jsonb,
    secret_ref text,
    digest text NOT NULL,
    published_at timestamp with time zone,
    created_at timestamp with time zone DEFAULT clock_timestamp() NOT NULL,
    approval_required boolean DEFAULT false NOT NULL,
    approval_label text DEFAULT ''::text NOT NULL
);

CREATE TABLE IF NOT EXISTS tool_set_revisions (
    id text NOT NULL,
    project_id text NOT NULL,
    set_name text NOT NULL,
    revision_number integer NOT NULL,
    tool_pins jsonb NOT NULL,
    digest text NOT NULL,
    published_at timestamp with time zone,
    created_at timestamp with time zone DEFAULT clock_timestamp() NOT NULL
);

CREATE TABLE IF NOT EXISTS tools (
    id text NOT NULL,
    project_id text NOT NULL,
    canonical_name text NOT NULL,
    model_visible_name text NOT NULL,
    created_at timestamp with time zone DEFAULT clock_timestamp() NOT NULL
);

CREATE TABLE IF NOT EXISTS transcript_boundaries (
    id text NOT NULL,
    run_id text NOT NULL,
    attempt_id text NOT NULL,
    project_id text NOT NULL,
    transcript_sequence bigint NOT NULL,
    created_at timestamp with time zone DEFAULT clock_timestamp() NOT NULL
);

CREATE TABLE IF NOT EXISTS trigger_deliveries (
    id text NOT NULL,
    project_id text NOT NULL,
    trigger_id text NOT NULL,
    trigger_revision_id text NOT NULL,
    principal_id text DEFAULT ''::text NOT NULL,
    state text DEFAULT 'received'::text NOT NULL,
    dedupe_key text DEFAULT ''::text NOT NULL,
    duplicate_of text,
    correlation_key_hash text DEFAULT ''::text NOT NULL,
    mapped_input jsonb,
    response_id text DEFAULT ''::text NOT NULL,
    run_id text DEFAULT ''::text NOT NULL,
    session_id text DEFAULT ''::text NOT NULL,
    reason text DEFAULT ''::text NOT NULL,
    source text DEFAULT ''::text NOT NULL,
    source_tenant text DEFAULT ''::text NOT NULL,
    source_event_id text DEFAULT ''::text NOT NULL,
    raw_payload jsonb,
    callback_state text DEFAULT ''::text NOT NULL,
    received_at timestamp with time zone DEFAULT clock_timestamp() NOT NULL,
    updated_at timestamp with time zone DEFAULT clock_timestamp() NOT NULL,
    CONSTRAINT trigger_deliveries_callback_state_check CHECK ((callback_state = ANY (ARRAY[''::text, 'pending'::text, 'delivered'::text, 'dead'::text]))),
    CONSTRAINT trigger_deliveries_state_check CHECK ((state = ANY (ARRAY['received'::text, 'authenticated'::text, 'deduplicated'::text, 'mapped'::text, 'admitted'::text, 'run_created'::text, 'rejected'::text, 'duplicate'::text, 'failed'::text, 'deferred'::text, 'skipped'::text])))
);

CREATE TABLE IF NOT EXISTS trigger_revisions (
    id text NOT NULL,
    project_id text NOT NULL,
    trigger_id text NOT NULL,
    revision_number integer NOT NULL,
    agent_revision_id text,
    run_template_revision_id text,
    input_mapping jsonb DEFAULT '{}'::jsonb NOT NULL,
    dedupe_key_expr text DEFAULT ''::text NOT NULL,
    correlation_mode text DEFAULT 'per_event'::text NOT NULL,
    correlation_key_expr text DEFAULT ''::text NOT NULL,
    concurrency_policy text DEFAULT 'allow'::text NOT NULL,
    output_mapping jsonb DEFAULT '{}'::jsonb NOT NULL,
    callback_endpoint_id text,
    created_at timestamp with time zone DEFAULT clock_timestamp() NOT NULL,
    CONSTRAINT trigger_revisions_check CHECK (((agent_revision_id IS NULL) OR (run_template_revision_id IS NULL))),
    CONSTRAINT trigger_revisions_concurrency_policy_check CHECK ((concurrency_policy = ANY (ARRAY['allow'::text, 'queue'::text, 'replace'::text, 'drop_if_running'::text, 'coalesce'::text, 'singleton'::text]))),
    CONSTRAINT trigger_revisions_correlation_mode_check CHECK ((correlation_mode = ANY (ARRAY['per_event'::text, 'bounded_key_reuse'::text, 'named_session'::text, 'reject_if_active'::text])))
);

CREATE TABLE IF NOT EXISTS triggers (
    id text NOT NULL,
    project_id text NOT NULL,
    name text NOT NULL,
    type text DEFAULT 'manual_api'::text NOT NULL,
    enabled boolean DEFAULT true NOT NULL,
    created_at timestamp with time zone DEFAULT clock_timestamp() NOT NULL,
    created_by text DEFAULT ''::text NOT NULL,
    inbound_secret_ref text DEFAULT ''::text NOT NULL,
    inbound_secret_ref_next text DEFAULT ''::text NOT NULL,
    CONSTRAINT triggers_type_check CHECK ((type = ANY (ARRAY['manual_api'::text, 'webhook'::text, 'cron'::text, 'queue'::text, 'integration_event'::text])))
);

CREATE TABLE IF NOT EXISTS usage_ledger (
    id text NOT NULL,
    schema_version integer DEFAULT 1 NOT NULL,
    project_id text NOT NULL,
    session_id text,
    run_id text,
    meter text NOT NULL,
    quantity numeric NOT NULL,
    unit text NOT NULL,
    dedupe_key text NOT NULL,
    occurred_at timestamp with time zone DEFAULT clock_timestamp() NOT NULL,
    model_request_id text,
    CONSTRAINT usage_ledger_quantity_check CHECK ((quantity >= (0)::numeric))
);

CREATE TABLE IF NOT EXISTS webhook_deliveries (
    id text NOT NULL,
    project_id text NOT NULL,
    endpoint_id text NOT NULL,
    session_id text NOT NULL,
    event_id text NOT NULL,
    event_type text NOT NULL,
    payload jsonb DEFAULT '{}'::jsonb NOT NULL,
    state text DEFAULT 'pending'::text NOT NULL,
    attempt_count integer DEFAULT 0 NOT NULL,
    next_attempt_at timestamp with time zone DEFAULT clock_timestamp() NOT NULL,
    first_attempt_at timestamp with time zone,
    created_at timestamp with time zone DEFAULT clock_timestamp() NOT NULL,
    updated_at timestamp with time zone DEFAULT clock_timestamp() NOT NULL,
    CONSTRAINT webhook_deliveries_attempt_count_check CHECK ((attempt_count >= 0)),
    CONSTRAINT webhook_deliveries_state_check CHECK ((state = ANY (ARRAY['pending'::text, 'delivered'::text, 'dead'::text])))
);

CREATE TABLE IF NOT EXISTS webhook_endpoints (
    id text NOT NULL,
    project_id text NOT NULL,
    url text NOT NULL,
    enabled boolean DEFAULT true NOT NULL,
    event_filter text[] DEFAULT '{}'::text[] NOT NULL,
    api_revision text DEFAULT ''::text NOT NULL,
    signing_secret_ref text DEFAULT ''::text NOT NULL,
    signing_secret_ref_next text DEFAULT ''::text NOT NULL,
    fixed_headers jsonb DEFAULT '{}'::jsonb NOT NULL,
    timeout_ms integer DEFAULT 10000 NOT NULL,
    max_attempts integer DEFAULT 20 NOT NULL,
    retry_window_seconds integer DEFAULT 259200 NOT NULL,
    allow_private_destination boolean DEFAULT false NOT NULL,
    cursor_journal_id bigint DEFAULT 0 NOT NULL,
    created_at timestamp with time zone DEFAULT clock_timestamp() NOT NULL,
    CONSTRAINT webhook_endpoints_max_attempts_check CHECK ((max_attempts > 0)),
    CONSTRAINT webhook_endpoints_retry_window_seconds_check CHECK ((retry_window_seconds > 0)),
    CONSTRAINT webhook_endpoints_timeout_ms_check CHECK ((timeout_ms > 0))
);

CREATE TABLE IF NOT EXISTS workspace_allocations (
    id text NOT NULL,
    workspace_id text NOT NULL,
    project_id text NOT NULL,
    fence bigint NOT NULL,
    host_path text DEFAULT ''::text NOT NULL,
    state text DEFAULT 'active'::text NOT NULL,
    created_at timestamp with time zone DEFAULT clock_timestamp() NOT NULL
);

CREATE TABLE IF NOT EXISTS workspace_leases (
    id text NOT NULL,
    workspace_id text NOT NULL,
    allocation_id text NOT NULL,
    project_id text NOT NULL,
    run_id text NOT NULL,
    state text DEFAULT 'active'::text NOT NULL,
    fence bigint NOT NULL,
    created_at timestamp with time zone DEFAULT clock_timestamp() NOT NULL
);

CREATE TABLE IF NOT EXISTS workspace_snapshots (
    id text NOT NULL,
    workspace_id text NOT NULL,
    allocation_id text NOT NULL,
    project_id text NOT NULL,
    fencing_token bigint NOT NULL,
    tree_checksum text NOT NULL,
    index_checksum text DEFAULT ''::text NOT NULL,
    file_checksums jsonb DEFAULT '{}'::jsonb NOT NULL,
    exclusions jsonb DEFAULT '[]'::jsonb NOT NULL,
    reason text DEFAULT ''::text NOT NULL,
    created_at timestamp with time zone DEFAULT clock_timestamp() NOT NULL,
    boundary_id text,
    object_key text DEFAULT ''::text NOT NULL,
    archive_checksum text DEFAULT ''::text NOT NULL,
    size_bytes bigint DEFAULT 0 NOT NULL
);

CREATE TABLE IF NOT EXISTS workspaces (
    id text NOT NULL,
    project_id text NOT NULL,
    session_id text NOT NULL,
    run_id text,
    state text DEFAULT 'requested'::text NOT NULL,
    unsafe_bind boolean DEFAULT false NOT NULL,
    unsafe_host_path text DEFAULT ''::text NOT NULL,
    publication_disabled boolean DEFAULT false NOT NULL,
    created_at timestamp with time zone DEFAULT clock_timestamp() NOT NULL,
    repository_binding_id text DEFAULT ''::text NOT NULL,
    requested_ref text DEFAULT ''::text NOT NULL
);

-- ------------------------------------------------------------- sequences

CREATE SEQUENCE IF NOT EXISTS audit_events_id_seq
    START WITH 1
    INCREMENT BY 1
    NO MINVALUE
    NO MAXVALUE
    CACHE 1;

CREATE SEQUENCE IF NOT EXISTS delivery_attempts_id_seq
    START WITH 1
    INCREMENT BY 1
    NO MINVALUE
    NO MAXVALUE
    CACHE 1;

DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_attribute a
                     JOIN pg_class c ON c.oid = a.attrelid
                     JOIN pg_namespace n ON n.oid = c.relnamespace
                    WHERE n.nspname = 'public' AND c.relname = 'deployment_desired'
                      AND a.attname = 'revision' AND a.attidentity <> '') THEN
        ALTER TABLE deployment_desired ALTER COLUMN revision ADD GENERATED ALWAYS AS IDENTITY (
            SEQUENCE NAME deployment_desired_revision_seq
            START WITH 1
            INCREMENT BY 1
            NO MINVALUE
            NO MAXVALUE
            CACHE 1
        );
    END IF;
END
$$;

DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_attribute a
                     JOIN pg_class c ON c.oid = a.attrelid
                     JOIN pg_namespace n ON n.oid = c.relnamespace
                    WHERE n.nspname = 'public' AND c.relname = 'events'
                      AND a.attname = 'journal_id' AND a.attidentity <> '') THEN
        ALTER TABLE events ALTER COLUMN journal_id ADD GENERATED BY DEFAULT AS IDENTITY (
            SEQUENCE NAME events_journal_id_seq
            START WITH 1
            INCREMENT BY 1
            NO MINVALUE
            NO MAXVALUE
            CACHE 1
        );
    END IF;
END
$$;

CREATE SEQUENCE IF NOT EXISTS idempotency_records_id_seq
    START WITH 1
    INCREMENT BY 1
    NO MINVALUE
    NO MAXVALUE
    CACHE 1;

CREATE SEQUENCE IF NOT EXISTS inbox_id_seq
    START WITH 1
    INCREMENT BY 1
    NO MINVALUE
    NO MAXVALUE
    CACHE 1;

CREATE SEQUENCE IF NOT EXISTS job_attempts_id_seq
    START WITH 1
    INCREMENT BY 1
    NO MINVALUE
    NO MAXVALUE
    CACHE 1;

CREATE SEQUENCE IF NOT EXISTS outbox_id_seq
    START WITH 1
    INCREMENT BY 1
    NO MINVALUE
    NO MAXVALUE
    CACHE 1;

ALTER SEQUENCE audit_events_id_seq OWNED BY audit_events.id;

ALTER SEQUENCE delivery_attempts_id_seq OWNED BY delivery_attempts.id;

ALTER SEQUENCE idempotency_records_id_seq OWNED BY idempotency_records.id;

ALTER SEQUENCE inbox_id_seq OWNED BY inbox.id;

ALTER SEQUENCE job_attempts_id_seq OWNED BY job_attempts.id;

ALTER SEQUENCE outbox_id_seq OWNED BY outbox.id;

ALTER TABLE ONLY audit_events ALTER COLUMN id SET DEFAULT nextval('audit_events_id_seq'::regclass);

ALTER TABLE ONLY delivery_attempts ALTER COLUMN id SET DEFAULT nextval('delivery_attempts_id_seq'::regclass);

ALTER TABLE ONLY idempotency_records ALTER COLUMN id SET DEFAULT nextval('idempotency_records_id_seq'::regclass);

ALTER TABLE ONLY inbox ALTER COLUMN id SET DEFAULT nextval('inbox_id_seq'::regclass);

ALTER TABLE ONLY job_attempts ALTER COLUMN id SET DEFAULT nextval('job_attempts_id_seq'::regclass);

ALTER TABLE ONLY outbox ALTER COLUMN id SET DEFAULT nextval('outbox_id_seq'::regclass);

-- ----------------------------------------------------------- constraints

DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_constraint
                    WHERE conname = 'a2a_interfaces_pkey' AND connamespace = 'public'::regnamespace) THEN
        ALTER TABLE ONLY a2a_interfaces
            ADD CONSTRAINT a2a_interfaces_pkey PRIMARY KEY (id);
    END IF;
END
$$;

DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_constraint
                    WHERE conname = 'a2a_remote_agents_pkey' AND connamespace = 'public'::regnamespace) THEN
        ALTER TABLE ONLY a2a_remote_agents
            ADD CONSTRAINT a2a_remote_agents_pkey PRIMARY KEY (id);
    END IF;
END
$$;

DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_constraint
                    WHERE conname = 'a2a_task_refs_interface_id_a2a_task_id_key' AND connamespace = 'public'::regnamespace) THEN
        ALTER TABLE ONLY a2a_task_refs
            ADD CONSTRAINT a2a_task_refs_interface_id_a2a_task_id_key UNIQUE (interface_id, a2a_task_id);
    END IF;
END
$$;

DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_constraint
                    WHERE conname = 'a2a_task_refs_pkey' AND connamespace = 'public'::regnamespace) THEN
        ALTER TABLE ONLY a2a_task_refs
            ADD CONSTRAINT a2a_task_refs_pkey PRIMARY KEY (id);
    END IF;
END
$$;

DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_constraint
                    WHERE conname = 'agent_profiles_pkey' AND connamespace = 'public'::regnamespace) THEN
        ALTER TABLE ONLY agent_profiles
            ADD CONSTRAINT agent_profiles_pkey PRIMARY KEY (id);
    END IF;
END
$$;

DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_constraint
                    WHERE conname = 'agent_profiles_project_id_name_key' AND connamespace = 'public'::regnamespace) THEN
        ALTER TABLE ONLY agent_profiles
            ADD CONSTRAINT agent_profiles_project_id_name_key UNIQUE (project_id, name);
    END IF;
END
$$;

DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_constraint
                    WHERE conname = 'agent_revisions_pkey' AND connamespace = 'public'::regnamespace) THEN
        ALTER TABLE ONLY agent_revisions
            ADD CONSTRAINT agent_revisions_pkey PRIMARY KEY (id);
    END IF;
END
$$;

DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_constraint
                    WHERE conname = 'agent_revisions_profile_id_revision_number_key' AND connamespace = 'public'::regnamespace) THEN
        ALTER TABLE ONLY agent_revisions
            ADD CONSTRAINT agent_revisions_profile_id_revision_number_key UNIQUE (profile_id, revision_number);
    END IF;
END
$$;

DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_constraint
                    WHERE conname = 'api_keys_pkey' AND connamespace = 'public'::regnamespace) THEN
        ALTER TABLE ONLY api_keys
            ADD CONSTRAINT api_keys_pkey PRIMARY KEY (id);
    END IF;
END
$$;

DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_constraint
                    WHERE conname = 'approvals_pkey' AND connamespace = 'public'::regnamespace) THEN
        ALTER TABLE ONLY approvals
            ADD CONSTRAINT approvals_pkey PRIMARY KEY (id);
    END IF;
END
$$;

DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_constraint
                    WHERE conname = 'approvals_publication_id_key' AND connamespace = 'public'::regnamespace) THEN
        ALTER TABLE ONLY approvals
            ADD CONSTRAINT approvals_publication_id_key UNIQUE (publication_id);
    END IF;
END
$$;

DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_constraint
                    WHERE conname = 'artifacts_pkey' AND connamespace = 'public'::regnamespace) THEN
        ALTER TABLE ONLY artifacts
            ADD CONSTRAINT artifacts_pkey PRIMARY KEY (id);
    END IF;
END
$$;

DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_constraint
                    WHERE conname = 'attempts_pkey' AND connamespace = 'public'::regnamespace) THEN
        ALTER TABLE ONLY attempts
            ADD CONSTRAINT attempts_pkey PRIMARY KEY (id);
    END IF;
END
$$;

DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_constraint
                    WHERE conname = 'attempts_run_id_fence_key' AND connamespace = 'public'::regnamespace) THEN
        ALTER TABLE ONLY attempts
            ADD CONSTRAINT attempts_run_id_fence_key UNIQUE (run_id, fence);
    END IF;
END
$$;

DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_constraint
                    WHERE conname = 'audit_events_pkey' AND connamespace = 'public'::regnamespace) THEN
        ALTER TABLE ONLY audit_events
            ADD CONSTRAINT audit_events_pkey PRIMARY KEY (id);
    END IF;
END
$$;

DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_constraint
                    WHERE conname = 'background_tasks_pkey' AND connamespace = 'public'::regnamespace) THEN
        ALTER TABLE ONLY background_tasks
            ADD CONSTRAINT background_tasks_pkey PRIMARY KEY (id);
    END IF;
END
$$;

DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_constraint
                    WHERE conname = 'background_tasks_tool_call_id_key' AND connamespace = 'public'::regnamespace) THEN
        ALTER TABLE ONLY background_tasks
            ADD CONSTRAINT background_tasks_tool_call_id_key UNIQUE (tool_call_id);
    END IF;
END
$$;

DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_constraint
                    WHERE conname = 'budgets_pkey' AND connamespace = 'public'::regnamespace) THEN
        ALTER TABLE ONLY budgets
            ADD CONSTRAINT budgets_pkey PRIMARY KEY (id);
    END IF;
END
$$;

DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_constraint
                    WHERE conname = 'budgets_project_id_meter_prefix_key' AND connamespace = 'public'::regnamespace) THEN
        ALTER TABLE ONLY budgets
            ADD CONSTRAINT budgets_project_id_meter_prefix_key UNIQUE (project_id, meter_prefix);
    END IF;
END
$$;

DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_constraint
                    WHERE conname = 'capability_jobs_job_id_entry_seq_key' AND connamespace = 'public'::regnamespace) THEN
        ALTER TABLE ONLY capability_jobs
            ADD CONSTRAINT capability_jobs_job_id_entry_seq_key UNIQUE (job_id, entry_seq);
    END IF;
END
$$;

DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_constraint
                    WHERE conname = 'capability_jobs_pkey' AND connamespace = 'public'::regnamespace) THEN
        ALTER TABLE ONLY capability_jobs
            ADD CONSTRAINT capability_jobs_pkey PRIMARY KEY (id);
    END IF;
END
$$;

DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_constraint
                    WHERE conname = 'capability_workers_pkey' AND connamespace = 'public'::regnamespace) THEN
        ALTER TABLE ONLY capability_workers
            ADD CONSTRAINT capability_workers_pkey PRIMARY KEY (id);
    END IF;
END
$$;

DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_constraint
                    WHERE conname = 'changeset_findings_pkey' AND connamespace = 'public'::regnamespace) THEN
        ALTER TABLE ONLY changeset_findings
            ADD CONSTRAINT changeset_findings_pkey PRIMARY KEY (id);
    END IF;
END
$$;

DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_constraint
                    WHERE conname = 'changesets_pkey' AND connamespace = 'public'::regnamespace) THEN
        ALTER TABLE ONLY changesets
            ADD CONSTRAINT changesets_pkey PRIMARY KEY (id);
    END IF;
END
$$;

DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_constraint
                    WHERE conname = 'checkpoints_pkey' AND connamespace = 'public'::regnamespace) THEN
        ALTER TABLE ONLY checkpoints
            ADD CONSTRAINT checkpoints_pkey PRIMARY KEY (id);
    END IF;
END
$$;

DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_constraint
                    WHERE conname = 'chunk_revisions_pkey' AND connamespace = 'public'::regnamespace) THEN
        ALTER TABLE ONLY chunk_revisions
            ADD CONSTRAINT chunk_revisions_pkey PRIMARY KEY (id);
    END IF;
END
$$;

DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_constraint
                    WHERE conname = 'commands_pkey' AND connamespace = 'public'::regnamespace) THEN
        ALTER TABLE ONLY commands
            ADD CONSTRAINT commands_pkey PRIMARY KEY (project_id, id);
    END IF;
END
$$;

DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_constraint
                    WHERE conname = 'config_revisions_pkey' AND connamespace = 'public'::regnamespace) THEN
        ALTER TABLE ONLY config_revisions
            ADD CONSTRAINT config_revisions_pkey PRIMARY KEY (project_id, id);
    END IF;
END
$$;

DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_constraint
                    WHERE conname = 'delivered_messages_pkey' AND connamespace = 'public'::regnamespace) THEN
        ALTER TABLE ONLY delivered_messages
            ADD CONSTRAINT delivered_messages_pkey PRIMARY KEY (project_id, command_id);
    END IF;
END
$$;

DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_constraint
                    WHERE conname = 'delivery_attempts_delivery_id_attempt_number_key' AND connamespace = 'public'::regnamespace) THEN
        ALTER TABLE ONLY delivery_attempts
            ADD CONSTRAINT delivery_attempts_delivery_id_attempt_number_key UNIQUE (delivery_id, attempt_number);
    END IF;
END
$$;

DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_constraint
                    WHERE conname = 'delivery_attempts_pkey' AND connamespace = 'public'::regnamespace) THEN
        ALTER TABLE ONLY delivery_attempts
            ADD CONSTRAINT delivery_attempts_pkey PRIMARY KEY (id);
    END IF;
END
$$;

DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_constraint
                    WHERE conname = 'deployment_desired_pkey' AND connamespace = 'public'::regnamespace) THEN
        ALTER TABLE ONLY deployment_desired
            ADD CONSTRAINT deployment_desired_pkey PRIMARY KEY (revision);
    END IF;
END
$$;

DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_constraint
                    WHERE conname = 'document_revisions_pkey' AND connamespace = 'public'::regnamespace) THEN
        ALTER TABLE ONLY document_revisions
            ADD CONSTRAINT document_revisions_pkey PRIMARY KEY (id);
    END IF;
END
$$;

DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_constraint
                    WHERE conname = 'document_revisions_project_id_source_id_ver_key' AND connamespace = 'public'::regnamespace) THEN
        ALTER TABLE ONLY document_revisions
            ADD CONSTRAINT document_revisions_project_id_source_id_ver_key UNIQUE (project_id, source_id, version);
    END IF;
END
$$;

DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_constraint
                    WHERE conname = 'durable_jobs_pkey' AND connamespace = 'public'::regnamespace) THEN
        ALTER TABLE ONLY durable_jobs
            ADD CONSTRAINT durable_jobs_pkey PRIMARY KEY (id);
    END IF;
END
$$;

DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_constraint
                    WHERE conname = 'environment_values_pkey' AND connamespace = 'public'::regnamespace) THEN
        ALTER TABLE ONLY environment_values
            ADD CONSTRAINT environment_values_pkey PRIMARY KEY (environment_id, key);
    END IF;
END
$$;

DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_constraint
                    WHERE conname = 'environments_name_key' AND connamespace = 'public'::regnamespace) THEN
        ALTER TABLE ONLY environments
            ADD CONSTRAINT environments_name_key UNIQUE (name);
    END IF;
END
$$;

DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_constraint
                    WHERE conname = 'environments_pkey' AND connamespace = 'public'::regnamespace) THEN
        ALTER TABLE ONLY environments
            ADD CONSTRAINT environments_pkey PRIMARY KEY (id);
    END IF;
END
$$;

DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_constraint
                    WHERE conname = 'events_pkey' AND connamespace = 'public'::regnamespace) THEN
        ALTER TABLE ONLY events
            ADD CONSTRAINT events_pkey PRIMARY KEY (id);
    END IF;
END
$$;

DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_constraint
                    WHERE conname = 'events_session_id_seq_key' AND connamespace = 'public'::regnamespace) THEN
        ALTER TABLE ONLY events
            ADD CONSTRAINT events_session_id_seq_key UNIQUE (session_id, seq);
    END IF;
END
$$;

DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_constraint
                    WHERE conname = 'hooks_pkey' AND connamespace = 'public'::regnamespace) THEN
        ALTER TABLE ONLY hooks
            ADD CONSTRAINT hooks_pkey PRIMARY KEY (id);
    END IF;
END
$$;

DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_constraint
                    WHERE conname = 'hooks_project_id_name_key' AND connamespace = 'public'::regnamespace) THEN
        ALTER TABLE ONLY hooks
            ADD CONSTRAINT hooks_project_id_name_key UNIQUE (project_id, name);
    END IF;
END
$$;

DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_constraint
                    WHERE conname = 'host_quarantine_pkey' AND connamespace = 'public'::regnamespace) THEN
        ALTER TABLE ONLY host_quarantine
            ADD CONSTRAINT host_quarantine_pkey PRIMARY KEY (host_id);
    END IF;
END
$$;

DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_constraint
                    WHERE conname = 'idempotency_records_pkey' AND connamespace = 'public'::regnamespace) THEN
        ALTER TABLE ONLY idempotency_records
            ADD CONSTRAINT idempotency_records_pkey PRIMARY KEY (id);
    END IF;
END
$$;

DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_constraint
                    WHERE conname = 'idempotency_records_project_id_principal_id_key' AND connamespace = 'public'::regnamespace) THEN
        ALTER TABLE ONLY idempotency_records
            ADD CONSTRAINT idempotency_records_project_id_principal_id_key UNIQUE (project_id, principal_id, method, route, idempotency_key);
    END IF;
END
$$;

DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_constraint
                    WHERE conname = 'inbox_pkey' AND connamespace = 'public'::regnamespace) THEN
        ALTER TABLE ONLY inbox
            ADD CONSTRAINT inbox_pkey PRIMARY KEY (id);
    END IF;
END
$$;

DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_constraint
                    WHERE conname = 'inbox_project_id_source_operation_id_key' AND connamespace = 'public'::regnamespace) THEN
        ALTER TABLE ONLY inbox
            ADD CONSTRAINT inbox_project_id_source_operation_id_key UNIQUE (project_id, source, operation_id);
    END IF;
END
$$;

DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_constraint
                    WHERE conname = 'index_revisions_pkey' AND connamespace = 'public'::regnamespace) THEN
        ALTER TABLE ONLY index_revisions
            ADD CONSTRAINT index_revisions_pkey PRIMARY KEY (id);
    END IF;
END
$$;

DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_constraint
                    WHERE conname = 'index_revisions_project_id_knowledge_base_i_key' AND connamespace = 'public'::regnamespace) THEN
        ALTER TABLE ONLY index_revisions
            ADD CONSTRAINT index_revisions_project_id_knowledge_base_i_key UNIQUE (project_id, knowledge_base_id, version);
    END IF;
END
$$;

DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_constraint
                    WHERE conname = 'ingestion_jobs_pkey' AND connamespace = 'public'::regnamespace) THEN
        ALTER TABLE ONLY ingestion_jobs
            ADD CONSTRAINT ingestion_jobs_pkey PRIMARY KEY (id);
    END IF;
END
$$;

DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_constraint
                    WHERE conname = 'integration_bots_pkey' AND connamespace = 'public'::regnamespace) THEN
        ALTER TABLE ONLY integration_bots
            ADD CONSTRAINT integration_bots_pkey PRIMARY KEY (id);
    END IF;
END
$$;

DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_constraint
                    WHERE conname = 'integration_bots_project_id_name_key' AND connamespace = 'public'::regnamespace) THEN
        ALTER TABLE ONLY integration_bots
            ADD CONSTRAINT integration_bots_project_id_name_key UNIQUE (project_id, name);
    END IF;
END
$$;

DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_constraint
                    WHERE conname = 'job_attempts_job_id_fence_key' AND connamespace = 'public'::regnamespace) THEN
        ALTER TABLE ONLY job_attempts
            ADD CONSTRAINT job_attempts_job_id_fence_key UNIQUE (job_id, fence);
    END IF;
END
$$;

DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_constraint
                    WHERE conname = 'job_attempts_pkey' AND connamespace = 'public'::regnamespace) THEN
        ALTER TABLE ONLY job_attempts
            ADD CONSTRAINT job_attempts_pkey PRIMARY KEY (id);
    END IF;
END
$$;

DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_constraint
                    WHERE conname = 'knowledge_bases_pkey' AND connamespace = 'public'::regnamespace) THEN
        ALTER TABLE ONLY knowledge_bases
            ADD CONSTRAINT knowledge_bases_pkey PRIMARY KEY (id);
    END IF;
END
$$;

DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_constraint
                    WHERE conname = 'knowledge_bases_project_id_name_key' AND connamespace = 'public'::regnamespace) THEN
        ALTER TABLE ONLY knowledge_bases
            ADD CONSTRAINT knowledge_bases_project_id_name_key UNIQUE (project_id, name);
    END IF;
END
$$;

DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_constraint
                    WHERE conname = 'knowledge_sources_pkey' AND connamespace = 'public'::regnamespace) THEN
        ALTER TABLE ONLY knowledge_sources
            ADD CONSTRAINT knowledge_sources_pkey PRIMARY KEY (id);
    END IF;
END
$$;

DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_constraint
                    WHERE conname = 'mcp_connections_pkey' AND connamespace = 'public'::regnamespace) THEN
        ALTER TABLE ONLY mcp_connections
            ADD CONSTRAINT mcp_connections_pkey PRIMARY KEY (id);
    END IF;
END
$$;

DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_constraint
                    WHERE conname = 'mcp_connections_project_id_name_key' AND connamespace = 'public'::regnamespace) THEN
        ALTER TABLE ONLY mcp_connections
            ADD CONSTRAINT mcp_connections_project_id_name_key UNIQUE (project_id, name);
    END IF;
END
$$;

DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_constraint
                    WHERE conname = 'merge_records_pkey' AND connamespace = 'public'::regnamespace) THEN
        ALTER TABLE ONLY merge_records
            ADD CONSTRAINT merge_records_pkey PRIMARY KEY (id);
    END IF;
END
$$;

DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_constraint
                    WHERE conname = 'messages_pkey' AND connamespace = 'public'::regnamespace) THEN
        ALTER TABLE ONLY messages
            ADD CONSTRAINT messages_pkey PRIMARY KEY (id);
    END IF;
END
$$;

DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_constraint
                    WHERE conname = 'model_connections_pkey' AND connamespace = 'public'::regnamespace) THEN
        ALTER TABLE ONLY model_connections
            ADD CONSTRAINT model_connections_pkey PRIMARY KEY (id);
    END IF;
END
$$;

DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_constraint
                    WHERE conname = 'model_requests_pkey' AND connamespace = 'public'::regnamespace) THEN
        ALTER TABLE ONLY model_requests
            ADD CONSTRAINT model_requests_pkey PRIMARY KEY (id);
    END IF;
END
$$;

DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_constraint
                    WHERE conname = 'model_route_revisions_pkey' AND connamespace = 'public'::regnamespace) THEN
        ALTER TABLE ONLY model_route_revisions
            ADD CONSTRAINT model_route_revisions_pkey PRIMARY KEY (id);
    END IF;
END
$$;

DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_constraint
                    WHERE conname = 'model_route_revisions_route_id_revision_key' AND connamespace = 'public'::regnamespace) THEN
        ALTER TABLE ONLY model_route_revisions
            ADD CONSTRAINT model_route_revisions_route_id_revision_key UNIQUE (route_id, revision);
    END IF;
END
$$;

DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_constraint
                    WHERE conname = 'model_routes_pkey' AND connamespace = 'public'::regnamespace) THEN
        ALTER TABLE ONLY model_routes
            ADD CONSTRAINT model_routes_pkey PRIMARY KEY (id);
    END IF;
END
$$;

DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_constraint
                    WHERE conname = 'outbox_pkey' AND connamespace = 'public'::regnamespace) THEN
        ALTER TABLE ONLY outbox
            ADD CONSTRAINT outbox_pkey PRIMARY KEY (id);
    END IF;
END
$$;

DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_constraint
                    WHERE conname = 'outbox_project_id_dedupe_key_key' AND connamespace = 'public'::regnamespace) THEN
        ALTER TABLE ONLY outbox
            ADD CONSTRAINT outbox_project_id_dedupe_key_key UNIQUE (project_id, dedupe_key);
    END IF;
END
$$;

DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_constraint
                    WHERE conname = 'preparation_receipts_pkey' AND connamespace = 'public'::regnamespace) THEN
        ALTER TABLE ONLY preparation_receipts
            ADD CONSTRAINT preparation_receipts_pkey PRIMARY KEY (id);
    END IF;
END
$$;

DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_constraint
                    WHERE conname = 'principals_pkey' AND connamespace = 'public'::regnamespace) THEN
        ALTER TABLE ONLY principals
            ADD CONSTRAINT principals_pkey PRIMARY KEY (id);
    END IF;
END
$$;

DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_constraint
                    WHERE conname = 'projects_pkey' AND connamespace = 'public'::regnamespace) THEN
        ALTER TABLE ONLY projects
            ADD CONSTRAINT projects_pkey PRIMARY KEY (id);
    END IF;
END
$$;

DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_constraint
                    WHERE conname = 'publications_pkey' AND connamespace = 'public'::regnamespace) THEN
        ALTER TABLE ONLY publications
            ADD CONSTRAINT publications_pkey PRIMARY KEY (id);
    END IF;
END
$$;

DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_constraint
                    WHERE conname = 'publications_project_id_idempotency_key_key' AND connamespace = 'public'::regnamespace) THEN
        ALTER TABLE ONLY publications
            ADD CONSTRAINT publications_project_id_idempotency_key_key UNIQUE (project_id, idempotency_key);
    END IF;
END
$$;

DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_constraint
                    WHERE conname = 'queue_connections_pkey' AND connamespace = 'public'::regnamespace) THEN
        ALTER TABLE ONLY queue_connections
            ADD CONSTRAINT queue_connections_pkey PRIMARY KEY (id);
    END IF;
END
$$;

DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_constraint
                    WHERE conname = 'queue_deliveries_pkey' AND connamespace = 'public'::regnamespace) THEN
        ALTER TABLE ONLY queue_deliveries
            ADD CONSTRAINT queue_deliveries_pkey PRIMARY KEY (id);
    END IF;
END
$$;

DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_constraint
                    WHERE conname = 'queue_deliveries_queue_connection_id_destination_key_key' AND connamespace = 'public'::regnamespace) THEN
        ALTER TABLE ONLY queue_deliveries
            ADD CONSTRAINT queue_deliveries_queue_connection_id_destination_key_key UNIQUE (queue_connection_id, destination_key);
    END IF;
END
$$;

DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_constraint
                    WHERE conname = 'queue_effect_receipts_pkey' AND connamespace = 'public'::regnamespace) THEN
        ALTER TABLE ONLY queue_effect_receipts
            ADD CONSTRAINT queue_effect_receipts_pkey PRIMARY KEY (id);
    END IF;
END
$$;

DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_constraint
                    WHERE conname = 'queue_effect_receipts_queue_connection_id_idempotency_key_key' AND connamespace = 'public'::regnamespace) THEN
        ALTER TABLE ONLY queue_effect_receipts
            ADD CONSTRAINT queue_effect_receipts_queue_connection_id_idempotency_key_key UNIQUE (queue_connection_id, idempotency_key);
    END IF;
END
$$;

DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_constraint
                    WHERE conname = 'queue_messages_pkey' AND connamespace = 'public'::regnamespace) THEN
        ALTER TABLE ONLY queue_messages
            ADD CONSTRAINT queue_messages_pkey PRIMARY KEY (id);
    END IF;
END
$$;

DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_constraint
                    WHERE conname = 'queue_messages_queue_connection_id_idempotency_key_key' AND connamespace = 'public'::regnamespace) THEN
        ALTER TABLE ONLY queue_messages
            ADD CONSTRAINT queue_messages_queue_connection_id_idempotency_key_key UNIQUE (queue_connection_id, idempotency_key);
    END IF;
END
$$;

DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_constraint
                    WHERE conname = 'quotas_pkey' AND connamespace = 'public'::regnamespace) THEN
        ALTER TABLE ONLY quotas
            ADD CONSTRAINT quotas_pkey PRIMARY KEY (id);
    END IF;
END
$$;

DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_constraint
                    WHERE conname = 'quotas_project_id_meter_prefix_key' AND connamespace = 'public'::regnamespace) THEN
        ALTER TABLE ONLY quotas
            ADD CONSTRAINT quotas_project_id_meter_prefix_key UNIQUE (project_id, meter_prefix);
    END IF;
END
$$;

DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_constraint
                    WHERE conname = 'remote_tool_operations_pkey' AND connamespace = 'public'::regnamespace) THEN
        ALTER TABLE ONLY remote_tool_operations
            ADD CONSTRAINT remote_tool_operations_pkey PRIMARY KEY (id);
    END IF;
END
$$;

DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_constraint
                    WHERE conname = 'repository_bindings_pkey' AND connamespace = 'public'::regnamespace) THEN
        ALTER TABLE ONLY repository_bindings
            ADD CONSTRAINT repository_bindings_pkey PRIMARY KEY (id);
    END IF;
END
$$;

DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_constraint
                    WHERE conname = 'responses_pkey' AND connamespace = 'public'::regnamespace) THEN
        ALTER TABLE ONLY responses
            ADD CONSTRAINT responses_pkey PRIMARY KEY (id);
    END IF;
END
$$;

DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_constraint
                    WHERE conname = 'run_template_revisions_pkey' AND connamespace = 'public'::regnamespace) THEN
        ALTER TABLE ONLY run_template_revisions
            ADD CONSTRAINT run_template_revisions_pkey PRIMARY KEY (id);
    END IF;
END
$$;

DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_constraint
                    WHERE conname = 'run_template_revisions_project_id_template__key' AND connamespace = 'public'::regnamespace) THEN
        ALTER TABLE ONLY run_template_revisions
            ADD CONSTRAINT run_template_revisions_project_id_template__key UNIQUE (project_id, template_name, revision_number);
    END IF;
END
$$;

DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_constraint
                    WHERE conname = 'runner_enrollments_pkey' AND connamespace = 'public'::regnamespace) THEN
        ALTER TABLE ONLY runner_enrollments
            ADD CONSTRAINT runner_enrollments_pkey PRIMARY KEY (id);
    END IF;
END
$$;

DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_constraint
                    WHERE conname = 'runner_enrollments_runner_id_entry_seq_key' AND connamespace = 'public'::regnamespace) THEN
        ALTER TABLE ONLY runner_enrollments
            ADD CONSTRAINT runner_enrollments_runner_id_entry_seq_key UNIQUE (runner_id, entry_seq);
    END IF;
END
$$;

DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_constraint
                    WHERE conname = 'runner_leases_pkey' AND connamespace = 'public'::regnamespace) THEN
        ALTER TABLE ONLY runner_leases
            ADD CONSTRAINT runner_leases_pkey PRIMARY KEY (id);
    END IF;
END
$$;

DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_constraint
                    WHERE conname = 'runner_pool_keys_key_sha256_key' AND connamespace = 'public'::regnamespace) THEN
        ALTER TABLE ONLY runner_pool_keys
            ADD CONSTRAINT runner_pool_keys_key_sha256_key UNIQUE (key_sha256);
    END IF;
END
$$;

DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_constraint
                    WHERE conname = 'runner_pool_keys_pkey' AND connamespace = 'public'::regnamespace) THEN
        ALTER TABLE ONLY runner_pool_keys
            ADD CONSTRAINT runner_pool_keys_pkey PRIMARY KEY (id);
    END IF;
END
$$;

DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_constraint
                    WHERE conname = 'runner_pools_pkey' AND connamespace = 'public'::regnamespace) THEN
        ALTER TABLE ONLY runner_pools
            ADD CONSTRAINT runner_pools_pkey PRIMARY KEY (id);
    END IF;
END
$$;

DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_constraint
                    WHERE conname = 'runners_pkey' AND connamespace = 'public'::regnamespace) THEN
        ALTER TABLE ONLY runners
            ADD CONSTRAINT runners_pkey PRIMARY KEY (id);
    END IF;
END
$$;

DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_constraint
                    WHERE conname = 'runs_pkey' AND connamespace = 'public'::regnamespace) THEN
        ALTER TABLE ONLY runs
            ADD CONSTRAINT runs_pkey PRIMARY KEY (id);
    END IF;
END
$$;

DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_constraint
                    WHERE conname = 'schedule_occurrences_pkey' AND connamespace = 'public'::regnamespace) THEN
        ALTER TABLE ONLY schedule_occurrences
            ADD CONSTRAINT schedule_occurrences_pkey PRIMARY KEY (occurrence_id);
    END IF;
END
$$;

DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_constraint
                    WHERE conname = 'schedule_occurrences_schedule_id_schedule_revision_planned__key' AND connamespace = 'public'::regnamespace) THEN
        ALTER TABLE ONLY schedule_occurrences
            ADD CONSTRAINT schedule_occurrences_schedule_id_schedule_revision_planned__key UNIQUE (schedule_id, schedule_revision, planned_at);
    END IF;
END
$$;

DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_constraint
                    WHERE conname = 'schedules_pkey' AND connamespace = 'public'::regnamespace) THEN
        ALTER TABLE ONLY schedules
            ADD CONSTRAINT schedules_pkey PRIMARY KEY (id);
    END IF;
END
$$;

DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_constraint
                    WHERE conname = 'schedules_project_id_name_key' AND connamespace = 'public'::regnamespace) THEN
        ALTER TABLE ONLY schedules
            ADD CONSTRAINT schedules_project_id_name_key UNIQUE (project_id, name);
    END IF;
END
$$;

DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_constraint
                    WHERE conname = 'schema_migrations_pkey' AND connamespace = 'public'::regnamespace) THEN
        ALTER TABLE ONLY schema_migrations
            ADD CONSTRAINT schema_migrations_pkey PRIMARY KEY (version);
    END IF;
END
$$;

DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_constraint
                    WHERE conname = 'schema_revisions_pkey' AND connamespace = 'public'::regnamespace) THEN
        ALTER TABLE ONLY schema_revisions
            ADD CONSTRAINT schema_revisions_pkey PRIMARY KEY (version);
    END IF;
END
$$;

DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_constraint
                    WHERE conname = 'secret_refs_name_version_key' AND connamespace = 'public'::regnamespace) THEN
        ALTER TABLE ONLY secret_refs
            ADD CONSTRAINT secret_refs_name_version_key UNIQUE (name, version);
    END IF;
END
$$;

DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_constraint
                    WHERE conname = 'secret_refs_pkey' AND connamespace = 'public'::regnamespace) THEN
        ALTER TABLE ONLY secret_refs
            ADD CONSTRAINT secret_refs_pkey PRIMARY KEY (id);
    END IF;
END
$$;

DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_constraint
                    WHERE conname = 'session_sequences_pkey' AND connamespace = 'public'::regnamespace) THEN
        ALTER TABLE ONLY session_sequences
            ADD CONSTRAINT session_sequences_pkey PRIMARY KEY (session_id);
    END IF;
END
$$;

DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_constraint
                    WHERE conname = 'sessions_pkey' AND connamespace = 'public'::regnamespace) THEN
        ALTER TABLE ONLY sessions
            ADD CONSTRAINT sessions_pkey PRIMARY KEY (id);
    END IF;
END
$$;

DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_constraint
                    WHERE conname = 'skill_revisions_pkey' AND connamespace = 'public'::regnamespace) THEN
        ALTER TABLE ONLY skill_revisions
            ADD CONSTRAINT skill_revisions_pkey PRIMARY KEY (id);
    END IF;
END
$$;

DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_constraint
                    WHERE conname = 'skill_revisions_skill_id_revision_number_key' AND connamespace = 'public'::regnamespace) THEN
        ALTER TABLE ONLY skill_revisions
            ADD CONSTRAINT skill_revisions_skill_id_revision_number_key UNIQUE (skill_id, revision_number);
    END IF;
END
$$;

DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_constraint
                    WHERE conname = 'skills_pkey' AND connamespace = 'public'::regnamespace) THEN
        ALTER TABLE ONLY skills
            ADD CONSTRAINT skills_pkey PRIMARY KEY (id);
    END IF;
END
$$;

DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_constraint
                    WHERE conname = 'skills_project_id_name_key' AND connamespace = 'public'::regnamespace) THEN
        ALTER TABLE ONLY skills
            ADD CONSTRAINT skills_project_id_name_key UNIQUE (project_id, name);
    END IF;
END
$$;

DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_constraint
                    WHERE conname = 'slack_approval_deliveries_approval_id_key' AND connamespace = 'public'::regnamespace) THEN
        ALTER TABLE ONLY slack_approval_deliveries
            ADD CONSTRAINT slack_approval_deliveries_approval_id_key UNIQUE (approval_id);
    END IF;
END
$$;

DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_constraint
                    WHERE conname = 'slack_approval_deliveries_pkey' AND connamespace = 'public'::regnamespace) THEN
        ALTER TABLE ONLY slack_approval_deliveries
            ADD CONSTRAINT slack_approval_deliveries_pkey PRIMARY KEY (id);
    END IF;
END
$$;

DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_constraint
                    WHERE conname = 'slack_connections_pkey' AND connamespace = 'public'::regnamespace) THEN
        ALTER TABLE ONLY slack_connections
            ADD CONSTRAINT slack_connections_pkey PRIMARY KEY (id);
    END IF;
END
$$;

DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_constraint
                    WHERE conname = 'slack_connections_project_id_team_id_enterp_key' AND connamespace = 'public'::regnamespace) THEN
        ALTER TABLE ONLY slack_connections
            ADD CONSTRAINT slack_connections_project_id_team_id_enterp_key UNIQUE (project_id, team_id, enterprise_id);
    END IF;
END
$$;

DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_constraint
                    WHERE conname = 'slack_message_turns_pkey' AND connamespace = 'public'::regnamespace) THEN
        ALTER TABLE ONLY slack_message_turns
            ADD CONSTRAINT slack_message_turns_pkey PRIMARY KEY (id);
    END IF;
END
$$;

DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_constraint
                    WHERE conname = 'slack_message_turns_project_id_team_id_chan_key' AND connamespace = 'public'::regnamespace) THEN
        ALTER TABLE ONLY slack_message_turns
            ADD CONSTRAINT slack_message_turns_project_id_team_id_chan_key UNIQUE (project_id, team_id, channel_id, message_ts);
    END IF;
END
$$;

DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_constraint
                    WHERE conname = 'slack_reply_deliveries_pkey' AND connamespace = 'public'::regnamespace) THEN
        ALTER TABLE ONLY slack_reply_deliveries
            ADD CONSTRAINT slack_reply_deliveries_pkey PRIMARY KEY (id);
    END IF;
END
$$;

DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_constraint
                    WHERE conname = 'slack_reply_deliveries_run_id_key' AND connamespace = 'public'::regnamespace) THEN
        ALTER TABLE ONLY slack_reply_deliveries
            ADD CONSTRAINT slack_reply_deliveries_run_id_key UNIQUE (run_id);
    END IF;
END
$$;

DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_constraint
                    WHERE conname = 'slack_thread_sessions_pkey' AND connamespace = 'public'::regnamespace) THEN
        ALTER TABLE ONLY slack_thread_sessions
            ADD CONSTRAINT slack_thread_sessions_pkey PRIMARY KEY (id);
    END IF;
END
$$;

DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_constraint
                    WHERE conname = 'slack_thread_sessions_project_id_team_id_ch_key' AND connamespace = 'public'::regnamespace) THEN
        ALTER TABLE ONLY slack_thread_sessions
            ADD CONSTRAINT slack_thread_sessions_project_id_team_id_ch_key UNIQUE (project_id, team_id, channel_id, thread_ts);
    END IF;
END
$$;

DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_constraint
                    WHERE conname = 'tasks_pkey' AND connamespace = 'public'::regnamespace) THEN
        ALTER TABLE ONLY tasks
            ADD CONSTRAINT tasks_pkey PRIMARY KEY (id);
    END IF;
END
$$;

DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_constraint
                    WHERE conname = 'tasks_session_id_task_key_key' AND connamespace = 'public'::regnamespace) THEN
        ALTER TABLE ONLY tasks
            ADD CONSTRAINT tasks_session_id_task_key_key UNIQUE (session_id, task_key);
    END IF;
END
$$;

DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_constraint
                    WHERE conname = 'tool_calls_pkey' AND connamespace = 'public'::regnamespace) THEN
        ALTER TABLE ONLY tool_calls
            ADD CONSTRAINT tool_calls_pkey PRIMARY KEY (id);
    END IF;
END
$$;

DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_constraint
                    WHERE conname = 'tool_revisions_pkey' AND connamespace = 'public'::regnamespace) THEN
        ALTER TABLE ONLY tool_revisions
            ADD CONSTRAINT tool_revisions_pkey PRIMARY KEY (id);
    END IF;
END
$$;

DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_constraint
                    WHERE conname = 'tool_revisions_tool_id_revision_number_key' AND connamespace = 'public'::regnamespace) THEN
        ALTER TABLE ONLY tool_revisions
            ADD CONSTRAINT tool_revisions_tool_id_revision_number_key UNIQUE (tool_id, revision_number);
    END IF;
END
$$;

DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_constraint
                    WHERE conname = 'tool_set_revisions_pkey' AND connamespace = 'public'::regnamespace) THEN
        ALTER TABLE ONLY tool_set_revisions
            ADD CONSTRAINT tool_set_revisions_pkey PRIMARY KEY (id);
    END IF;
END
$$;

DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_constraint
                    WHERE conname = 'tool_set_revisions_project_id_set_name_revi_key' AND connamespace = 'public'::regnamespace) THEN
        ALTER TABLE ONLY tool_set_revisions
            ADD CONSTRAINT tool_set_revisions_project_id_set_name_revi_key UNIQUE (project_id, set_name, revision_number);
    END IF;
END
$$;

DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_constraint
                    WHERE conname = 'tools_pkey' AND connamespace = 'public'::regnamespace) THEN
        ALTER TABLE ONLY tools
            ADD CONSTRAINT tools_pkey PRIMARY KEY (id);
    END IF;
END
$$;

DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_constraint
                    WHERE conname = 'tools_project_id_canonical_name_key' AND connamespace = 'public'::regnamespace) THEN
        ALTER TABLE ONLY tools
            ADD CONSTRAINT tools_project_id_canonical_name_key UNIQUE (project_id, canonical_name);
    END IF;
END
$$;

DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_constraint
                    WHERE conname = 'tools_project_id_model_visible_name_key' AND connamespace = 'public'::regnamespace) THEN
        ALTER TABLE ONLY tools
            ADD CONSTRAINT tools_project_id_model_visible_name_key UNIQUE (project_id, model_visible_name);
    END IF;
END
$$;

DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_constraint
                    WHERE conname = 'transcript_boundaries_pkey' AND connamespace = 'public'::regnamespace) THEN
        ALTER TABLE ONLY transcript_boundaries
            ADD CONSTRAINT transcript_boundaries_pkey PRIMARY KEY (id);
    END IF;
END
$$;

DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_constraint
                    WHERE conname = 'trigger_deliveries_pkey' AND connamespace = 'public'::regnamespace) THEN
        ALTER TABLE ONLY trigger_deliveries
            ADD CONSTRAINT trigger_deliveries_pkey PRIMARY KEY (id);
    END IF;
END
$$;

DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_constraint
                    WHERE conname = 'trigger_revisions_pkey' AND connamespace = 'public'::regnamespace) THEN
        ALTER TABLE ONLY trigger_revisions
            ADD CONSTRAINT trigger_revisions_pkey PRIMARY KEY (id);
    END IF;
END
$$;

DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_constraint
                    WHERE conname = 'trigger_revisions_trigger_id_revision_number_key' AND connamespace = 'public'::regnamespace) THEN
        ALTER TABLE ONLY trigger_revisions
            ADD CONSTRAINT trigger_revisions_trigger_id_revision_number_key UNIQUE (trigger_id, revision_number);
    END IF;
END
$$;

DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_constraint
                    WHERE conname = 'triggers_pkey' AND connamespace = 'public'::regnamespace) THEN
        ALTER TABLE ONLY triggers
            ADD CONSTRAINT triggers_pkey PRIMARY KEY (id);
    END IF;
END
$$;

DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_constraint
                    WHERE conname = 'triggers_project_id_name_key' AND connamespace = 'public'::regnamespace) THEN
        ALTER TABLE ONLY triggers
            ADD CONSTRAINT triggers_project_id_name_key UNIQUE (project_id, name);
    END IF;
END
$$;

DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_constraint
                    WHERE conname = 'usage_ledger_pkey' AND connamespace = 'public'::regnamespace) THEN
        ALTER TABLE ONLY usage_ledger
            ADD CONSTRAINT usage_ledger_pkey PRIMARY KEY (id);
    END IF;
END
$$;

DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_constraint
                    WHERE conname = 'usage_ledger_project_id_dedupe_key_key' AND connamespace = 'public'::regnamespace) THEN
        ALTER TABLE ONLY usage_ledger
            ADD CONSTRAINT usage_ledger_project_id_dedupe_key_key UNIQUE (project_id, dedupe_key);
    END IF;
END
$$;

DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_constraint
                    WHERE conname = 'webhook_deliveries_endpoint_id_event_id_key' AND connamespace = 'public'::regnamespace) THEN
        ALTER TABLE ONLY webhook_deliveries
            ADD CONSTRAINT webhook_deliveries_endpoint_id_event_id_key UNIQUE (endpoint_id, event_id);
    END IF;
END
$$;

DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_constraint
                    WHERE conname = 'webhook_deliveries_pkey' AND connamespace = 'public'::regnamespace) THEN
        ALTER TABLE ONLY webhook_deliveries
            ADD CONSTRAINT webhook_deliveries_pkey PRIMARY KEY (id);
    END IF;
END
$$;

DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_constraint
                    WHERE conname = 'webhook_endpoints_pkey' AND connamespace = 'public'::regnamespace) THEN
        ALTER TABLE ONLY webhook_endpoints
            ADD CONSTRAINT webhook_endpoints_pkey PRIMARY KEY (id);
    END IF;
END
$$;

DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_constraint
                    WHERE conname = 'workspace_allocations_pkey' AND connamespace = 'public'::regnamespace) THEN
        ALTER TABLE ONLY workspace_allocations
            ADD CONSTRAINT workspace_allocations_pkey PRIMARY KEY (id);
    END IF;
END
$$;

DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_constraint
                    WHERE conname = 'workspace_allocations_workspace_id_fence_key' AND connamespace = 'public'::regnamespace) THEN
        ALTER TABLE ONLY workspace_allocations
            ADD CONSTRAINT workspace_allocations_workspace_id_fence_key UNIQUE (workspace_id, fence);
    END IF;
END
$$;

DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_constraint
                    WHERE conname = 'workspace_leases_pkey' AND connamespace = 'public'::regnamespace) THEN
        ALTER TABLE ONLY workspace_leases
            ADD CONSTRAINT workspace_leases_pkey PRIMARY KEY (id);
    END IF;
END
$$;

DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_constraint
                    WHERE conname = 'workspace_snapshots_pkey' AND connamespace = 'public'::regnamespace) THEN
        ALTER TABLE ONLY workspace_snapshots
            ADD CONSTRAINT workspace_snapshots_pkey PRIMARY KEY (id);
    END IF;
END
$$;

DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_constraint
                    WHERE conname = 'workspaces_pkey' AND connamespace = 'public'::regnamespace) THEN
        ALTER TABLE ONLY workspaces
            ADD CONSTRAINT workspaces_pkey PRIMARY KEY (id);
    END IF;
END
$$;

DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_constraint
                    WHERE conname = 'a2a_interfaces_project_id_fkey' AND connamespace = 'public'::regnamespace) THEN
        ALTER TABLE ONLY a2a_interfaces
            ADD CONSTRAINT a2a_interfaces_project_id_fkey FOREIGN KEY (project_id) REFERENCES projects(id);
    END IF;
END
$$;

DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_constraint
                    WHERE conname = 'a2a_remote_agents_project_id_fkey' AND connamespace = 'public'::regnamespace) THEN
        ALTER TABLE ONLY a2a_remote_agents
            ADD CONSTRAINT a2a_remote_agents_project_id_fkey FOREIGN KEY (project_id) REFERENCES projects(id);
    END IF;
END
$$;

DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_constraint
                    WHERE conname = 'a2a_task_refs_interface_id_fkey' AND connamespace = 'public'::regnamespace) THEN
        ALTER TABLE ONLY a2a_task_refs
            ADD CONSTRAINT a2a_task_refs_interface_id_fkey FOREIGN KEY (interface_id) REFERENCES a2a_interfaces(id) ON DELETE CASCADE;
    END IF;
END
$$;

DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_constraint
                    WHERE conname = 'a2a_task_refs_project_id_fkey' AND connamespace = 'public'::regnamespace) THEN
        ALTER TABLE ONLY a2a_task_refs
            ADD CONSTRAINT a2a_task_refs_project_id_fkey FOREIGN KEY (project_id) REFERENCES projects(id);
    END IF;
END
$$;

DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_constraint
                    WHERE conname = 'agent_profiles_project_id_fkey' AND connamespace = 'public'::regnamespace) THEN
        ALTER TABLE ONLY agent_profiles
            ADD CONSTRAINT agent_profiles_project_id_fkey FOREIGN KEY (project_id) REFERENCES projects(id);
    END IF;
END
$$;

DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_constraint
                    WHERE conname = 'agent_revisions_profile_id_fkey' AND connamespace = 'public'::regnamespace) THEN
        ALTER TABLE ONLY agent_revisions
            ADD CONSTRAINT agent_revisions_profile_id_fkey FOREIGN KEY (profile_id) REFERENCES agent_profiles(id);
    END IF;
END
$$;

DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_constraint
                    WHERE conname = 'agent_revisions_project_id_fkey' AND connamespace = 'public'::regnamespace) THEN
        ALTER TABLE ONLY agent_revisions
            ADD CONSTRAINT agent_revisions_project_id_fkey FOREIGN KEY (project_id) REFERENCES projects(id);
    END IF;
END
$$;

DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_constraint
                    WHERE conname = 'api_keys_principal_id_fkey' AND connamespace = 'public'::regnamespace) THEN
        ALTER TABLE ONLY api_keys
            ADD CONSTRAINT api_keys_principal_id_fkey FOREIGN KEY (principal_id) REFERENCES principals(id);
    END IF;
END
$$;

DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_constraint
                    WHERE conname = 'api_keys_project_id_fkey' AND connamespace = 'public'::regnamespace) THEN
        ALTER TABLE ONLY api_keys
            ADD CONSTRAINT api_keys_project_id_fkey FOREIGN KEY (project_id) REFERENCES projects(id);
    END IF;
END
$$;

DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_constraint
                    WHERE conname = 'approvals_project_id_fkey' AND connamespace = 'public'::regnamespace) THEN
        ALTER TABLE ONLY approvals
            ADD CONSTRAINT approvals_project_id_fkey FOREIGN KEY (project_id) REFERENCES projects(id);
    END IF;
END
$$;

DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_constraint
                    WHERE conname = 'approvals_publication_id_fkey' AND connamespace = 'public'::regnamespace) THEN
        ALTER TABLE ONLY approvals
            ADD CONSTRAINT approvals_publication_id_fkey FOREIGN KEY (publication_id) REFERENCES publications(id);
    END IF;
END
$$;

DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_constraint
                    WHERE conname = 'approvals_tool_call_id_fkey' AND connamespace = 'public'::regnamespace) THEN
        ALTER TABLE ONLY approvals
            ADD CONSTRAINT approvals_tool_call_id_fkey FOREIGN KEY (tool_call_id) REFERENCES tool_calls(id);
    END IF;
END
$$;

DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_constraint
                    WHERE conname = 'artifacts_project_id_fkey' AND connamespace = 'public'::regnamespace) THEN
        ALTER TABLE ONLY artifacts
            ADD CONSTRAINT artifacts_project_id_fkey FOREIGN KEY (project_id) REFERENCES projects(id);
    END IF;
END
$$;

DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_constraint
                    WHERE conname = 'artifacts_run_id_fkey' AND connamespace = 'public'::regnamespace) THEN
        ALTER TABLE ONLY artifacts
            ADD CONSTRAINT artifacts_run_id_fkey FOREIGN KEY (run_id) REFERENCES runs(id);
    END IF;
END
$$;

DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_constraint
                    WHERE conname = 'attempts_project_id_fkey' AND connamespace = 'public'::regnamespace) THEN
        ALTER TABLE ONLY attempts
            ADD CONSTRAINT attempts_project_id_fkey FOREIGN KEY (project_id) REFERENCES projects(id);
    END IF;
END
$$;

DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_constraint
                    WHERE conname = 'attempts_run_id_fkey' AND connamespace = 'public'::regnamespace) THEN
        ALTER TABLE ONLY attempts
            ADD CONSTRAINT attempts_run_id_fkey FOREIGN KEY (run_id) REFERENCES runs(id);
    END IF;
END
$$;

DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_constraint
                    WHERE conname = 'audit_events_project_id_fkey' AND connamespace = 'public'::regnamespace) THEN
        ALTER TABLE ONLY audit_events
            ADD CONSTRAINT audit_events_project_id_fkey FOREIGN KEY (project_id) REFERENCES projects(id);
    END IF;
END
$$;

DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_constraint
                    WHERE conname = 'background_tasks_project_id_fkey' AND connamespace = 'public'::regnamespace) THEN
        ALTER TABLE ONLY background_tasks
            ADD CONSTRAINT background_tasks_project_id_fkey FOREIGN KEY (project_id) REFERENCES projects(id);
    END IF;
END
$$;

DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_constraint
                    WHERE conname = 'background_tasks_response_id_fkey' AND connamespace = 'public'::regnamespace) THEN
        ALTER TABLE ONLY background_tasks
            ADD CONSTRAINT background_tasks_response_id_fkey FOREIGN KEY (response_id) REFERENCES responses(id);
    END IF;
END
$$;

DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_constraint
                    WHERE conname = 'background_tasks_run_id_fkey' AND connamespace = 'public'::regnamespace) THEN
        ALTER TABLE ONLY background_tasks
            ADD CONSTRAINT background_tasks_run_id_fkey FOREIGN KEY (run_id) REFERENCES runs(id);
    END IF;
END
$$;

DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_constraint
                    WHERE conname = 'background_tasks_session_id_fkey' AND connamespace = 'public'::regnamespace) THEN
        ALTER TABLE ONLY background_tasks
            ADD CONSTRAINT background_tasks_session_id_fkey FOREIGN KEY (session_id) REFERENCES sessions(id);
    END IF;
END
$$;

DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_constraint
                    WHERE conname = 'background_tasks_tool_call_id_fkey' AND connamespace = 'public'::regnamespace) THEN
        ALTER TABLE ONLY background_tasks
            ADD CONSTRAINT background_tasks_tool_call_id_fkey FOREIGN KEY (tool_call_id) REFERENCES tool_calls(id);
    END IF;
END
$$;

DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_constraint
                    WHERE conname = 'capability_jobs_project_id_fkey' AND connamespace = 'public'::regnamespace) THEN
        ALTER TABLE ONLY capability_jobs
            ADD CONSTRAINT capability_jobs_project_id_fkey FOREIGN KEY (project_id) REFERENCES projects(id);
    END IF;
END
$$;

DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_constraint
                    WHERE conname = 'capability_workers_project_id_fkey' AND connamespace = 'public'::regnamespace) THEN
        ALTER TABLE ONLY capability_workers
            ADD CONSTRAINT capability_workers_project_id_fkey FOREIGN KEY (project_id) REFERENCES projects(id);
    END IF;
END
$$;

DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_constraint
                    WHERE conname = 'changeset_findings_changeset_id_fkey' AND connamespace = 'public'::regnamespace) THEN
        ALTER TABLE ONLY changeset_findings
            ADD CONSTRAINT changeset_findings_changeset_id_fkey FOREIGN KEY (changeset_id) REFERENCES changesets(id);
    END IF;
END
$$;

DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_constraint
                    WHERE conname = 'changeset_findings_project_id_fkey' AND connamespace = 'public'::regnamespace) THEN
        ALTER TABLE ONLY changeset_findings
            ADD CONSTRAINT changeset_findings_project_id_fkey FOREIGN KEY (project_id) REFERENCES projects(id);
    END IF;
END
$$;

DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_constraint
                    WHERE conname = 'changesets_patch_artifact_id_fkey' AND connamespace = 'public'::regnamespace) THEN
        ALTER TABLE ONLY changesets
            ADD CONSTRAINT changesets_patch_artifact_id_fkey FOREIGN KEY (patch_artifact_id) REFERENCES artifacts(id);
    END IF;
END
$$;

DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_constraint
                    WHERE conname = 'changesets_project_id_fkey' AND connamespace = 'public'::regnamespace) THEN
        ALTER TABLE ONLY changesets
            ADD CONSTRAINT changesets_project_id_fkey FOREIGN KEY (project_id) REFERENCES projects(id);
    END IF;
END
$$;

DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_constraint
                    WHERE conname = 'changesets_run_id_fkey' AND connamespace = 'public'::regnamespace) THEN
        ALTER TABLE ONLY changesets
            ADD CONSTRAINT changesets_run_id_fkey FOREIGN KEY (run_id) REFERENCES runs(id);
    END IF;
END
$$;

DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_constraint
                    WHERE conname = 'changesets_test_log_artifact_id_fkey' AND connamespace = 'public'::regnamespace) THEN
        ALTER TABLE ONLY changesets
            ADD CONSTRAINT changesets_test_log_artifact_id_fkey FOREIGN KEY (test_log_artifact_id) REFERENCES artifacts(id);
    END IF;
END
$$;

DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_constraint
                    WHERE conname = 'checkpoints_attempt_id_fkey' AND connamespace = 'public'::regnamespace) THEN
        ALTER TABLE ONLY checkpoints
            ADD CONSTRAINT checkpoints_attempt_id_fkey FOREIGN KEY (attempt_id) REFERENCES attempts(id);
    END IF;
END
$$;

DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_constraint
                    WHERE conname = 'checkpoints_boundary_id_fkey' AND connamespace = 'public'::regnamespace) THEN
        ALTER TABLE ONLY checkpoints
            ADD CONSTRAINT checkpoints_boundary_id_fkey FOREIGN KEY (boundary_id) REFERENCES transcript_boundaries(id);
    END IF;
END
$$;

DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_constraint
                    WHERE conname = 'checkpoints_project_id_fkey' AND connamespace = 'public'::regnamespace) THEN
        ALTER TABLE ONLY checkpoints
            ADD CONSTRAINT checkpoints_project_id_fkey FOREIGN KEY (project_id) REFERENCES projects(id);
    END IF;
END
$$;

DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_constraint
                    WHERE conname = 'checkpoints_run_id_fkey' AND connamespace = 'public'::regnamespace) THEN
        ALTER TABLE ONLY checkpoints
            ADD CONSTRAINT checkpoints_run_id_fkey FOREIGN KEY (run_id) REFERENCES runs(id);
    END IF;
END
$$;

DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_constraint
                    WHERE conname = 'checkpoints_workspace_snapshot_id_fkey' AND connamespace = 'public'::regnamespace) THEN
        ALTER TABLE ONLY checkpoints
            ADD CONSTRAINT checkpoints_workspace_snapshot_id_fkey FOREIGN KEY (workspace_snapshot_id) REFERENCES workspace_snapshots(id);
    END IF;
END
$$;

DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_constraint
                    WHERE conname = 'chunk_revisions_document_revision_id_fkey' AND connamespace = 'public'::regnamespace) THEN
        ALTER TABLE ONLY chunk_revisions
            ADD CONSTRAINT chunk_revisions_document_revision_id_fkey FOREIGN KEY (document_revision_id) REFERENCES document_revisions(id) ON DELETE CASCADE;
    END IF;
END
$$;

DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_constraint
                    WHERE conname = 'chunk_revisions_knowledge_base_id_fkey' AND connamespace = 'public'::regnamespace) THEN
        ALTER TABLE ONLY chunk_revisions
            ADD CONSTRAINT chunk_revisions_knowledge_base_id_fkey FOREIGN KEY (knowledge_base_id) REFERENCES knowledge_bases(id) ON DELETE CASCADE;
    END IF;
END
$$;

DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_constraint
                    WHERE conname = 'commands_project_id_fkey' AND connamespace = 'public'::regnamespace) THEN
        ALTER TABLE ONLY commands
            ADD CONSTRAINT commands_project_id_fkey FOREIGN KEY (project_id) REFERENCES projects(id);
    END IF;
END
$$;

DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_constraint
                    WHERE conname = 'commands_run_id_fkey' AND connamespace = 'public'::regnamespace) THEN
        ALTER TABLE ONLY commands
            ADD CONSTRAINT commands_run_id_fkey FOREIGN KEY (run_id) REFERENCES runs(id);
    END IF;
END
$$;

DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_constraint
                    WHERE conname = 'commands_session_id_fkey' AND connamespace = 'public'::regnamespace) THEN
        ALTER TABLE ONLY commands
            ADD CONSTRAINT commands_session_id_fkey FOREIGN KEY (session_id) REFERENCES sessions(id);
    END IF;
END
$$;

DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_constraint
                    WHERE conname = 'config_revisions_project_id_fkey' AND connamespace = 'public'::regnamespace) THEN
        ALTER TABLE ONLY config_revisions
            ADD CONSTRAINT config_revisions_project_id_fkey FOREIGN KEY (project_id) REFERENCES projects(id);
    END IF;
END
$$;

DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_constraint
                    WHERE conname = 'config_revisions_session_id_fkey' AND connamespace = 'public'::regnamespace) THEN
        ALTER TABLE ONLY config_revisions
            ADD CONSTRAINT config_revisions_session_id_fkey FOREIGN KEY (session_id) REFERENCES sessions(id);
    END IF;
END
$$;

DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_constraint
                    WHERE conname = 'delivered_messages_project_id_command_id_fkey' AND connamespace = 'public'::regnamespace) THEN
        ALTER TABLE ONLY delivered_messages
            ADD CONSTRAINT delivered_messages_project_id_command_id_fkey FOREIGN KEY (project_id, command_id) REFERENCES commands(project_id, id);
    END IF;
END
$$;

DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_constraint
                    WHERE conname = 'delivered_messages_run_id_fkey' AND connamespace = 'public'::regnamespace) THEN
        ALTER TABLE ONLY delivered_messages
            ADD CONSTRAINT delivered_messages_run_id_fkey FOREIGN KEY (run_id) REFERENCES runs(id);
    END IF;
END
$$;

DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_constraint
                    WHERE conname = 'delivery_attempts_delivery_id_fkey' AND connamespace = 'public'::regnamespace) THEN
        ALTER TABLE ONLY delivery_attempts
            ADD CONSTRAINT delivery_attempts_delivery_id_fkey FOREIGN KEY (delivery_id) REFERENCES webhook_deliveries(id) ON DELETE CASCADE;
    END IF;
END
$$;

DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_constraint
                    WHERE conname = 'document_revisions_knowledge_base_id_fkey' AND connamespace = 'public'::regnamespace) THEN
        ALTER TABLE ONLY document_revisions
            ADD CONSTRAINT document_revisions_knowledge_base_id_fkey FOREIGN KEY (knowledge_base_id) REFERENCES knowledge_bases(id) ON DELETE CASCADE;
    END IF;
END
$$;

DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_constraint
                    WHERE conname = 'document_revisions_project_id_fkey' AND connamespace = 'public'::regnamespace) THEN
        ALTER TABLE ONLY document_revisions
            ADD CONSTRAINT document_revisions_project_id_fkey FOREIGN KEY (project_id) REFERENCES projects(id);
    END IF;
END
$$;

DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_constraint
                    WHERE conname = 'durable_jobs_project_id_fkey' AND connamespace = 'public'::regnamespace) THEN
        ALTER TABLE ONLY durable_jobs
            ADD CONSTRAINT durable_jobs_project_id_fkey FOREIGN KEY (project_id) REFERENCES projects(id);
    END IF;
END
$$;

DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_constraint
                    WHERE conname = 'environment_values_environment_id_fkey' AND connamespace = 'public'::regnamespace) THEN
        ALTER TABLE ONLY environment_values
            ADD CONSTRAINT environment_values_environment_id_fkey FOREIGN KEY (environment_id) REFERENCES environments(id) ON DELETE CASCADE;
    END IF;
END
$$;

DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_constraint
                    WHERE conname = 'events_project_id_fkey' AND connamespace = 'public'::regnamespace) THEN
        ALTER TABLE ONLY events
            ADD CONSTRAINT events_project_id_fkey FOREIGN KEY (project_id) REFERENCES projects(id);
    END IF;
END
$$;

DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_constraint
                    WHERE conname = 'events_session_id_fkey' AND connamespace = 'public'::regnamespace) THEN
        ALTER TABLE ONLY events
            ADD CONSTRAINT events_session_id_fkey FOREIGN KEY (session_id) REFERENCES sessions(id);
    END IF;
END
$$;

DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_constraint
                    WHERE conname = 'hooks_project_id_fkey' AND connamespace = 'public'::regnamespace) THEN
        ALTER TABLE ONLY hooks
            ADD CONSTRAINT hooks_project_id_fkey FOREIGN KEY (project_id) REFERENCES projects(id);
    END IF;
END
$$;

DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_constraint
                    WHERE conname = 'idempotency_records_principal_id_fkey' AND connamespace = 'public'::regnamespace) THEN
        ALTER TABLE ONLY idempotency_records
            ADD CONSTRAINT idempotency_records_principal_id_fkey FOREIGN KEY (principal_id) REFERENCES principals(id);
    END IF;
END
$$;

DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_constraint
                    WHERE conname = 'idempotency_records_project_id_fkey' AND connamespace = 'public'::regnamespace) THEN
        ALTER TABLE ONLY idempotency_records
            ADD CONSTRAINT idempotency_records_project_id_fkey FOREIGN KEY (project_id) REFERENCES projects(id);
    END IF;
END
$$;

DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_constraint
                    WHERE conname = 'index_revisions_knowledge_base_id_fkey' AND connamespace = 'public'::regnamespace) THEN
        ALTER TABLE ONLY index_revisions
            ADD CONSTRAINT index_revisions_knowledge_base_id_fkey FOREIGN KEY (knowledge_base_id) REFERENCES knowledge_bases(id) ON DELETE CASCADE;
    END IF;
END
$$;

DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_constraint
                    WHERE conname = 'index_revisions_project_id_fkey' AND connamespace = 'public'::regnamespace) THEN
        ALTER TABLE ONLY index_revisions
            ADD CONSTRAINT index_revisions_project_id_fkey FOREIGN KEY (project_id) REFERENCES projects(id);
    END IF;
END
$$;

DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_constraint
                    WHERE conname = 'ingestion_jobs_knowledge_base_id_fkey' AND connamespace = 'public'::regnamespace) THEN
        ALTER TABLE ONLY ingestion_jobs
            ADD CONSTRAINT ingestion_jobs_knowledge_base_id_fkey FOREIGN KEY (knowledge_base_id) REFERENCES knowledge_bases(id) ON DELETE CASCADE;
    END IF;
END
$$;

DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_constraint
                    WHERE conname = 'ingestion_jobs_project_id_fkey' AND connamespace = 'public'::regnamespace) THEN
        ALTER TABLE ONLY ingestion_jobs
            ADD CONSTRAINT ingestion_jobs_project_id_fkey FOREIGN KEY (project_id) REFERENCES projects(id);
    END IF;
END
$$;

DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_constraint
                    WHERE conname = 'integration_bots_project_id_fkey' AND connamespace = 'public'::regnamespace) THEN
        ALTER TABLE ONLY integration_bots
            ADD CONSTRAINT integration_bots_project_id_fkey FOREIGN KEY (project_id) REFERENCES projects(id);
    END IF;
END
$$;

DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_constraint
                    WHERE conname = 'job_attempts_job_id_fkey' AND connamespace = 'public'::regnamespace) THEN
        ALTER TABLE ONLY job_attempts
            ADD CONSTRAINT job_attempts_job_id_fkey FOREIGN KEY (job_id) REFERENCES durable_jobs(id) ON DELETE CASCADE;
    END IF;
END
$$;

DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_constraint
                    WHERE conname = 'knowledge_bases_project_id_fkey' AND connamespace = 'public'::regnamespace) THEN
        ALTER TABLE ONLY knowledge_bases
            ADD CONSTRAINT knowledge_bases_project_id_fkey FOREIGN KEY (project_id) REFERENCES projects(id);
    END IF;
END
$$;

DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_constraint
                    WHERE conname = 'knowledge_sources_knowledge_base_id_fkey' AND connamespace = 'public'::regnamespace) THEN
        ALTER TABLE ONLY knowledge_sources
            ADD CONSTRAINT knowledge_sources_knowledge_base_id_fkey FOREIGN KEY (knowledge_base_id) REFERENCES knowledge_bases(id) ON DELETE CASCADE;
    END IF;
END
$$;

DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_constraint
                    WHERE conname = 'knowledge_sources_project_id_fkey' AND connamespace = 'public'::regnamespace) THEN
        ALTER TABLE ONLY knowledge_sources
            ADD CONSTRAINT knowledge_sources_project_id_fkey FOREIGN KEY (project_id) REFERENCES projects(id);
    END IF;
END
$$;

DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_constraint
                    WHERE conname = 'mcp_connections_project_id_fkey' AND connamespace = 'public'::regnamespace) THEN
        ALTER TABLE ONLY mcp_connections
            ADD CONSTRAINT mcp_connections_project_id_fkey FOREIGN KEY (project_id) REFERENCES projects(id);
    END IF;
END
$$;

DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_constraint
                    WHERE conname = 'merge_records_parent_run_id_fkey' AND connamespace = 'public'::regnamespace) THEN
        ALTER TABLE ONLY merge_records
            ADD CONSTRAINT merge_records_parent_run_id_fkey FOREIGN KEY (parent_run_id) REFERENCES runs(id);
    END IF;
END
$$;

DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_constraint
                    WHERE conname = 'merge_records_project_id_fkey' AND connamespace = 'public'::regnamespace) THEN
        ALTER TABLE ONLY merge_records
            ADD CONSTRAINT merge_records_project_id_fkey FOREIGN KEY (project_id) REFERENCES projects(id);
    END IF;
END
$$;

DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_constraint
                    WHERE conname = 'merge_records_source_child_run_id_fkey' AND connamespace = 'public'::regnamespace) THEN
        ALTER TABLE ONLY merge_records
            ADD CONSTRAINT merge_records_source_child_run_id_fkey FOREIGN KEY (source_child_run_id) REFERENCES runs(id);
    END IF;
END
$$;

DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_constraint
                    WHERE conname = 'messages_project_id_fkey' AND connamespace = 'public'::regnamespace) THEN
        ALTER TABLE ONLY messages
            ADD CONSTRAINT messages_project_id_fkey FOREIGN KEY (project_id) REFERENCES projects(id);
    END IF;
END
$$;

DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_constraint
                    WHERE conname = 'messages_session_id_fkey' AND connamespace = 'public'::regnamespace) THEN
        ALTER TABLE ONLY messages
            ADD CONSTRAINT messages_session_id_fkey FOREIGN KEY (session_id) REFERENCES sessions(id);
    END IF;
END
$$;

DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_constraint
                    WHERE conname = 'model_connections_project_id_fkey' AND connamespace = 'public'::regnamespace) THEN
        ALTER TABLE ONLY model_connections
            ADD CONSTRAINT model_connections_project_id_fkey FOREIGN KEY (project_id) REFERENCES projects(id);
    END IF;
END
$$;

DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_constraint
                    WHERE conname = 'model_requests_project_id_fkey' AND connamespace = 'public'::regnamespace) THEN
        ALTER TABLE ONLY model_requests
            ADD CONSTRAINT model_requests_project_id_fkey FOREIGN KEY (project_id) REFERENCES projects(id);
    END IF;
END
$$;

DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_constraint
                    WHERE conname = 'model_requests_run_id_fkey' AND connamespace = 'public'::regnamespace) THEN
        ALTER TABLE ONLY model_requests
            ADD CONSTRAINT model_requests_run_id_fkey FOREIGN KEY (run_id) REFERENCES runs(id);
    END IF;
END
$$;

DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_constraint
                    WHERE conname = 'model_route_revisions_route_id_fkey' AND connamespace = 'public'::regnamespace) THEN
        ALTER TABLE ONLY model_route_revisions
            ADD CONSTRAINT model_route_revisions_route_id_fkey FOREIGN KEY (route_id) REFERENCES model_routes(id);
    END IF;
END
$$;

DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_constraint
                    WHERE conname = 'model_routes_project_id_fkey' AND connamespace = 'public'::regnamespace) THEN
        ALTER TABLE ONLY model_routes
            ADD CONSTRAINT model_routes_project_id_fkey FOREIGN KEY (project_id) REFERENCES projects(id);
    END IF;
END
$$;

DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_constraint
                    WHERE conname = 'preparation_receipts_project_id_fkey' AND connamespace = 'public'::regnamespace) THEN
        ALTER TABLE ONLY preparation_receipts
            ADD CONSTRAINT preparation_receipts_project_id_fkey FOREIGN KEY (project_id) REFERENCES projects(id);
    END IF;
END
$$;

DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_constraint
                    WHERE conname = 'preparation_receipts_repository_binding_id_fkey' AND connamespace = 'public'::regnamespace) THEN
        ALTER TABLE ONLY preparation_receipts
            ADD CONSTRAINT preparation_receipts_repository_binding_id_fkey FOREIGN KEY (repository_binding_id) REFERENCES repository_bindings(id);
    END IF;
END
$$;

DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_constraint
                    WHERE conname = 'preparation_receipts_run_id_fkey' AND connamespace = 'public'::regnamespace) THEN
        ALTER TABLE ONLY preparation_receipts
            ADD CONSTRAINT preparation_receipts_run_id_fkey FOREIGN KEY (run_id) REFERENCES runs(id);
    END IF;
END
$$;

DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_constraint
                    WHERE conname = 'principals_project_id_fkey' AND connamespace = 'public'::regnamespace) THEN
        ALTER TABLE ONLY principals
            ADD CONSTRAINT principals_project_id_fkey FOREIGN KEY (project_id) REFERENCES projects(id);
    END IF;
END
$$;

DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_constraint
                    WHERE conname = 'publications_project_id_fkey' AND connamespace = 'public'::regnamespace) THEN
        ALTER TABLE ONLY publications
            ADD CONSTRAINT publications_project_id_fkey FOREIGN KEY (project_id) REFERENCES projects(id);
    END IF;
END
$$;

DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_constraint
                    WHERE conname = 'publications_run_id_fkey' AND connamespace = 'public'::regnamespace) THEN
        ALTER TABLE ONLY publications
            ADD CONSTRAINT publications_run_id_fkey FOREIGN KEY (run_id) REFERENCES runs(id);
    END IF;
END
$$;

DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_constraint
                    WHERE conname = 'queue_connections_project_id_fkey' AND connamespace = 'public'::regnamespace) THEN
        ALTER TABLE ONLY queue_connections
            ADD CONSTRAINT queue_connections_project_id_fkey FOREIGN KEY (project_id) REFERENCES projects(id);
    END IF;
END
$$;

DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_constraint
                    WHERE conname = 'queue_deliveries_project_id_fkey' AND connamespace = 'public'::regnamespace) THEN
        ALTER TABLE ONLY queue_deliveries
            ADD CONSTRAINT queue_deliveries_project_id_fkey FOREIGN KEY (project_id) REFERENCES projects(id);
    END IF;
END
$$;

DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_constraint
                    WHERE conname = 'queue_deliveries_queue_connection_id_fkey' AND connamespace = 'public'::regnamespace) THEN
        ALTER TABLE ONLY queue_deliveries
            ADD CONSTRAINT queue_deliveries_queue_connection_id_fkey FOREIGN KEY (queue_connection_id) REFERENCES queue_connections(id) ON DELETE CASCADE;
    END IF;
END
$$;

DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_constraint
                    WHERE conname = 'queue_effect_receipts_project_id_fkey' AND connamespace = 'public'::regnamespace) THEN
        ALTER TABLE ONLY queue_effect_receipts
            ADD CONSTRAINT queue_effect_receipts_project_id_fkey FOREIGN KEY (project_id) REFERENCES projects(id);
    END IF;
END
$$;

DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_constraint
                    WHERE conname = 'queue_effect_receipts_queue_connection_id_fkey' AND connamespace = 'public'::regnamespace) THEN
        ALTER TABLE ONLY queue_effect_receipts
            ADD CONSTRAINT queue_effect_receipts_queue_connection_id_fkey FOREIGN KEY (queue_connection_id) REFERENCES queue_connections(id) ON DELETE CASCADE;
    END IF;
END
$$;

DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_constraint
                    WHERE conname = 'queue_messages_project_id_fkey' AND connamespace = 'public'::regnamespace) THEN
        ALTER TABLE ONLY queue_messages
            ADD CONSTRAINT queue_messages_project_id_fkey FOREIGN KEY (project_id) REFERENCES projects(id);
    END IF;
END
$$;

DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_constraint
                    WHERE conname = 'queue_messages_queue_connection_id_fkey' AND connamespace = 'public'::regnamespace) THEN
        ALTER TABLE ONLY queue_messages
            ADD CONSTRAINT queue_messages_queue_connection_id_fkey FOREIGN KEY (queue_connection_id) REFERENCES queue_connections(id) ON DELETE CASCADE;
    END IF;
END
$$;

DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_constraint
                    WHERE conname = 'remote_tool_operations_project_id_fkey' AND connamespace = 'public'::regnamespace) THEN
        ALTER TABLE ONLY remote_tool_operations
            ADD CONSTRAINT remote_tool_operations_project_id_fkey FOREIGN KEY (project_id) REFERENCES projects(id);
    END IF;
END
$$;

DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_constraint
                    WHERE conname = 'repository_bindings_project_id_fkey' AND connamespace = 'public'::regnamespace) THEN
        ALTER TABLE ONLY repository_bindings
            ADD CONSTRAINT repository_bindings_project_id_fkey FOREIGN KEY (project_id) REFERENCES projects(id);
    END IF;
END
$$;

DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_constraint
                    WHERE conname = 'responses_project_id_fkey' AND connamespace = 'public'::regnamespace) THEN
        ALTER TABLE ONLY responses
            ADD CONSTRAINT responses_project_id_fkey FOREIGN KEY (project_id) REFERENCES projects(id);
    END IF;
END
$$;

DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_constraint
                    WHERE conname = 'responses_session_id_fkey' AND connamespace = 'public'::regnamespace) THEN
        ALTER TABLE ONLY responses
            ADD CONSTRAINT responses_session_id_fkey FOREIGN KEY (session_id) REFERENCES sessions(id);
    END IF;
END
$$;

DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_constraint
                    WHERE conname = 'run_template_revisions_project_id_fkey' AND connamespace = 'public'::regnamespace) THEN
        ALTER TABLE ONLY run_template_revisions
            ADD CONSTRAINT run_template_revisions_project_id_fkey FOREIGN KEY (project_id) REFERENCES projects(id);
    END IF;
END
$$;

DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_constraint
                    WHERE conname = 'runner_enrollments_project_id_fkey' AND connamespace = 'public'::regnamespace) THEN
        ALTER TABLE ONLY runner_enrollments
            ADD CONSTRAINT runner_enrollments_project_id_fkey FOREIGN KEY (project_id) REFERENCES projects(id);
    END IF;
END
$$;

DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_constraint
                    WHERE conname = 'runner_leases_project_id_fkey' AND connamespace = 'public'::regnamespace) THEN
        ALTER TABLE ONLY runner_leases
            ADD CONSTRAINT runner_leases_project_id_fkey FOREIGN KEY (project_id) REFERENCES projects(id);
    END IF;
END
$$;

DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_constraint
                    WHERE conname = 'runner_leases_run_id_fkey' AND connamespace = 'public'::regnamespace) THEN
        ALTER TABLE ONLY runner_leases
            ADD CONSTRAINT runner_leases_run_id_fkey FOREIGN KEY (run_id) REFERENCES runs(id);
    END IF;
END
$$;

DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_constraint
                    WHERE conname = 'runner_leases_runner_id_fkey' AND connamespace = 'public'::regnamespace) THEN
        ALTER TABLE ONLY runner_leases
            ADD CONSTRAINT runner_leases_runner_id_fkey FOREIGN KEY (runner_id) REFERENCES runners(id);
    END IF;
END
$$;

DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_constraint
                    WHERE conname = 'runner_pool_keys_pool_id_fkey' AND connamespace = 'public'::regnamespace) THEN
        ALTER TABLE ONLY runner_pool_keys
            ADD CONSTRAINT runner_pool_keys_pool_id_fkey FOREIGN KEY (pool_id) REFERENCES runner_pools(id) ON DELETE CASCADE;
    END IF;
END
$$;

DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_constraint
                    WHERE conname = 'runner_pool_keys_project_id_fkey' AND connamespace = 'public'::regnamespace) THEN
        ALTER TABLE ONLY runner_pool_keys
            ADD CONSTRAINT runner_pool_keys_project_id_fkey FOREIGN KEY (project_id) REFERENCES projects(id);
    END IF;
END
$$;

DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_constraint
                    WHERE conname = 'runner_pools_project_id_fkey' AND connamespace = 'public'::regnamespace) THEN
        ALTER TABLE ONLY runner_pools
            ADD CONSTRAINT runner_pools_project_id_fkey FOREIGN KEY (project_id) REFERENCES projects(id);
    END IF;
END
$$;

DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_constraint
                    WHERE conname = 'runners_enrolled_via_key_id_fkey' AND connamespace = 'public'::regnamespace) THEN
        ALTER TABLE ONLY runners
            ADD CONSTRAINT runners_enrolled_via_key_id_fkey FOREIGN KEY (enrolled_via_key_id) REFERENCES runner_pool_keys(id);
    END IF;
END
$$;

DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_constraint
                    WHERE conname = 'runners_pool_id_fkey' AND connamespace = 'public'::regnamespace) THEN
        ALTER TABLE ONLY runners
            ADD CONSTRAINT runners_pool_id_fkey FOREIGN KEY (pool_id) REFERENCES runner_pools(id);
    END IF;
END
$$;

DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_constraint
                    WHERE conname = 'runners_project_id_fkey' AND connamespace = 'public'::regnamespace) THEN
        ALTER TABLE ONLY runners
            ADD CONSTRAINT runners_project_id_fkey FOREIGN KEY (project_id) REFERENCES projects(id);
    END IF;
END
$$;

DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_constraint
                    WHERE conname = 'runs_agent_revision_id_fkey' AND connamespace = 'public'::regnamespace) THEN
        ALTER TABLE ONLY runs
            ADD CONSTRAINT runs_agent_revision_id_fkey FOREIGN KEY (agent_revision_id) REFERENCES agent_revisions(id);
    END IF;
END
$$;

DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_constraint
                    WHERE conname = 'runs_parent_run_id_fkey' AND connamespace = 'public'::regnamespace) THEN
        ALTER TABLE ONLY runs
            ADD CONSTRAINT runs_parent_run_id_fkey FOREIGN KEY (parent_run_id) REFERENCES runs(id);
    END IF;
END
$$;

DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_constraint
                    WHERE conname = 'runs_pool_id_fkey' AND connamespace = 'public'::regnamespace) THEN
        ALTER TABLE ONLY runs
            ADD CONSTRAINT runs_pool_id_fkey FOREIGN KEY (pool_id) REFERENCES runner_pools(id);
    END IF;
END
$$;

DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_constraint
                    WHERE conname = 'runs_project_id_fkey' AND connamespace = 'public'::regnamespace) THEN
        ALTER TABLE ONLY runs
            ADD CONSTRAINT runs_project_id_fkey FOREIGN KEY (project_id) REFERENCES projects(id);
    END IF;
END
$$;

DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_constraint
                    WHERE conname = 'runs_response_id_fkey' AND connamespace = 'public'::regnamespace) THEN
        ALTER TABLE ONLY runs
            ADD CONSTRAINT runs_response_id_fkey FOREIGN KEY (response_id) REFERENCES responses(id);
    END IF;
END
$$;

DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_constraint
                    WHERE conname = 'runs_run_template_revision_id_fkey' AND connamespace = 'public'::regnamespace) THEN
        ALTER TABLE ONLY runs
            ADD CONSTRAINT runs_run_template_revision_id_fkey FOREIGN KEY (run_template_revision_id) REFERENCES run_template_revisions(id);
    END IF;
END
$$;

DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_constraint
                    WHERE conname = 'runs_session_id_fkey' AND connamespace = 'public'::regnamespace) THEN
        ALTER TABLE ONLY runs
            ADD CONSTRAINT runs_session_id_fkey FOREIGN KEY (session_id) REFERENCES sessions(id);
    END IF;
END
$$;

DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_constraint
                    WHERE conname = 'schedule_occurrences_schedule_id_fkey' AND connamespace = 'public'::regnamespace) THEN
        ALTER TABLE ONLY schedule_occurrences
            ADD CONSTRAINT schedule_occurrences_schedule_id_fkey FOREIGN KEY (schedule_id) REFERENCES schedules(id);
    END IF;
END
$$;

DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_constraint
                    WHERE conname = 'schedules_project_id_fkey' AND connamespace = 'public'::regnamespace) THEN
        ALTER TABLE ONLY schedules
            ADD CONSTRAINT schedules_project_id_fkey FOREIGN KEY (project_id) REFERENCES projects(id);
    END IF;
END
$$;

DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_constraint
                    WHERE conname = 'schedules_trigger_id_fkey' AND connamespace = 'public'::regnamespace) THEN
        ALTER TABLE ONLY schedules
            ADD CONSTRAINT schedules_trigger_id_fkey FOREIGN KEY (trigger_id) REFERENCES triggers(id);
    END IF;
END
$$;

DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_constraint
                    WHERE conname = 'session_sequences_session_id_fkey' AND connamespace = 'public'::regnamespace) THEN
        ALTER TABLE ONLY session_sequences
            ADD CONSTRAINT session_sequences_session_id_fkey FOREIGN KEY (session_id) REFERENCES sessions(id);
    END IF;
END
$$;

DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_constraint
                    WHERE conname = 'sessions_project_id_fkey' AND connamespace = 'public'::regnamespace) THEN
        ALTER TABLE ONLY sessions
            ADD CONSTRAINT sessions_project_id_fkey FOREIGN KEY (project_id) REFERENCES projects(id);
    END IF;
END
$$;

DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_constraint
                    WHERE conname = 'skill_revisions_skill_id_fkey' AND connamespace = 'public'::regnamespace) THEN
        ALTER TABLE ONLY skill_revisions
            ADD CONSTRAINT skill_revisions_skill_id_fkey FOREIGN KEY (skill_id) REFERENCES skills(id);
    END IF;
END
$$;

DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_constraint
                    WHERE conname = 'slack_approval_deliveries_approval_id_fkey' AND connamespace = 'public'::regnamespace) THEN
        ALTER TABLE ONLY slack_approval_deliveries
            ADD CONSTRAINT slack_approval_deliveries_approval_id_fkey FOREIGN KEY (approval_id) REFERENCES approvals(id) ON DELETE CASCADE;
    END IF;
END
$$;

DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_constraint
                    WHERE conname = 'slack_approval_deliveries_connection_id_fkey' AND connamespace = 'public'::regnamespace) THEN
        ALTER TABLE ONLY slack_approval_deliveries
            ADD CONSTRAINT slack_approval_deliveries_connection_id_fkey FOREIGN KEY (connection_id) REFERENCES slack_connections(id) ON DELETE CASCADE;
    END IF;
END
$$;

DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_constraint
                    WHERE conname = 'slack_approval_deliveries_project_id_fkey' AND connamespace = 'public'::regnamespace) THEN
        ALTER TABLE ONLY slack_approval_deliveries
            ADD CONSTRAINT slack_approval_deliveries_project_id_fkey FOREIGN KEY (project_id) REFERENCES projects(id);
    END IF;
END
$$;

DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_constraint
                    WHERE conname = 'slack_approval_deliveries_run_id_fkey' AND connamespace = 'public'::regnamespace) THEN
        ALTER TABLE ONLY slack_approval_deliveries
            ADD CONSTRAINT slack_approval_deliveries_run_id_fkey FOREIGN KEY (run_id) REFERENCES runs(id) ON DELETE CASCADE;
    END IF;
END
$$;

DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_constraint
                    WHERE conname = 'slack_connections_project_id_fkey' AND connamespace = 'public'::regnamespace) THEN
        ALTER TABLE ONLY slack_connections
            ADD CONSTRAINT slack_connections_project_id_fkey FOREIGN KEY (project_id) REFERENCES projects(id);
    END IF;
END
$$;

DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_constraint
                    WHERE conname = 'slack_message_turns_connection_id_fkey' AND connamespace = 'public'::regnamespace) THEN
        ALTER TABLE ONLY slack_message_turns
            ADD CONSTRAINT slack_message_turns_connection_id_fkey FOREIGN KEY (connection_id) REFERENCES slack_connections(id) ON DELETE CASCADE;
    END IF;
END
$$;

DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_constraint
                    WHERE conname = 'slack_message_turns_project_id_fkey' AND connamespace = 'public'::regnamespace) THEN
        ALTER TABLE ONLY slack_message_turns
            ADD CONSTRAINT slack_message_turns_project_id_fkey FOREIGN KEY (project_id) REFERENCES projects(id);
    END IF;
END
$$;

DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_constraint
                    WHERE conname = 'slack_message_turns_response_id_fkey' AND connamespace = 'public'::regnamespace) THEN
        ALTER TABLE ONLY slack_message_turns
            ADD CONSTRAINT slack_message_turns_response_id_fkey FOREIGN KEY (response_id) REFERENCES responses(id) ON DELETE CASCADE;
    END IF;
END
$$;

DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_constraint
                    WHERE conname = 'slack_message_turns_session_id_fkey' AND connamespace = 'public'::regnamespace) THEN
        ALTER TABLE ONLY slack_message_turns
            ADD CONSTRAINT slack_message_turns_session_id_fkey FOREIGN KEY (session_id) REFERENCES sessions(id);
    END IF;
END
$$;

DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_constraint
                    WHERE conname = 'slack_reply_deliveries_connection_id_fkey' AND connamespace = 'public'::regnamespace) THEN
        ALTER TABLE ONLY slack_reply_deliveries
            ADD CONSTRAINT slack_reply_deliveries_connection_id_fkey FOREIGN KEY (connection_id) REFERENCES slack_connections(id) ON DELETE CASCADE;
    END IF;
END
$$;

DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_constraint
                    WHERE conname = 'slack_reply_deliveries_project_id_fkey' AND connamespace = 'public'::regnamespace) THEN
        ALTER TABLE ONLY slack_reply_deliveries
            ADD CONSTRAINT slack_reply_deliveries_project_id_fkey FOREIGN KEY (project_id) REFERENCES projects(id);
    END IF;
END
$$;

DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_constraint
                    WHERE conname = 'slack_reply_deliveries_run_id_fkey' AND connamespace = 'public'::regnamespace) THEN
        ALTER TABLE ONLY slack_reply_deliveries
            ADD CONSTRAINT slack_reply_deliveries_run_id_fkey FOREIGN KEY (run_id) REFERENCES runs(id) ON DELETE CASCADE;
    END IF;
END
$$;

DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_constraint
                    WHERE conname = 'slack_thread_sessions_connection_id_fkey' AND connamespace = 'public'::regnamespace) THEN
        ALTER TABLE ONLY slack_thread_sessions
            ADD CONSTRAINT slack_thread_sessions_connection_id_fkey FOREIGN KEY (connection_id) REFERENCES slack_connections(id);
    END IF;
END
$$;

DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_constraint
                    WHERE conname = 'slack_thread_sessions_project_id_fkey' AND connamespace = 'public'::regnamespace) THEN
        ALTER TABLE ONLY slack_thread_sessions
            ADD CONSTRAINT slack_thread_sessions_project_id_fkey FOREIGN KEY (project_id) REFERENCES projects(id);
    END IF;
END
$$;

DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_constraint
                    WHERE conname = 'slack_thread_sessions_session_id_fkey' AND connamespace = 'public'::regnamespace) THEN
        ALTER TABLE ONLY slack_thread_sessions
            ADD CONSTRAINT slack_thread_sessions_session_id_fkey FOREIGN KEY (session_id) REFERENCES sessions(id);
    END IF;
END
$$;

DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_constraint
                    WHERE conname = 'tasks_project_id_fkey' AND connamespace = 'public'::regnamespace) THEN
        ALTER TABLE ONLY tasks
            ADD CONSTRAINT tasks_project_id_fkey FOREIGN KEY (project_id) REFERENCES projects(id);
    END IF;
END
$$;

DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_constraint
                    WHERE conname = 'tasks_session_id_fkey' AND connamespace = 'public'::regnamespace) THEN
        ALTER TABLE ONLY tasks
            ADD CONSTRAINT tasks_session_id_fkey FOREIGN KEY (session_id) REFERENCES sessions(id);
    END IF;
END
$$;

DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_constraint
                    WHERE conname = 'tool_calls_project_id_fkey' AND connamespace = 'public'::regnamespace) THEN
        ALTER TABLE ONLY tool_calls
            ADD CONSTRAINT tool_calls_project_id_fkey FOREIGN KEY (project_id) REFERENCES projects(id);
    END IF;
END
$$;

DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_constraint
                    WHERE conname = 'tool_calls_run_id_fkey' AND connamespace = 'public'::regnamespace) THEN
        ALTER TABLE ONLY tool_calls
            ADD CONSTRAINT tool_calls_run_id_fkey FOREIGN KEY (run_id) REFERENCES runs(id);
    END IF;
END
$$;

DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_constraint
                    WHERE conname = 'tool_revisions_project_id_fkey' AND connamespace = 'public'::regnamespace) THEN
        ALTER TABLE ONLY tool_revisions
            ADD CONSTRAINT tool_revisions_project_id_fkey FOREIGN KEY (project_id) REFERENCES projects(id);
    END IF;
END
$$;

DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_constraint
                    WHERE conname = 'tool_revisions_tool_id_fkey' AND connamespace = 'public'::regnamespace) THEN
        ALTER TABLE ONLY tool_revisions
            ADD CONSTRAINT tool_revisions_tool_id_fkey FOREIGN KEY (tool_id) REFERENCES tools(id);
    END IF;
END
$$;

DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_constraint
                    WHERE conname = 'tool_set_revisions_project_id_fkey' AND connamespace = 'public'::regnamespace) THEN
        ALTER TABLE ONLY tool_set_revisions
            ADD CONSTRAINT tool_set_revisions_project_id_fkey FOREIGN KEY (project_id) REFERENCES projects(id);
    END IF;
END
$$;

DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_constraint
                    WHERE conname = 'tools_project_id_fkey' AND connamespace = 'public'::regnamespace) THEN
        ALTER TABLE ONLY tools
            ADD CONSTRAINT tools_project_id_fkey FOREIGN KEY (project_id) REFERENCES projects(id);
    END IF;
END
$$;

DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_constraint
                    WHERE conname = 'transcript_boundaries_attempt_id_fkey' AND connamespace = 'public'::regnamespace) THEN
        ALTER TABLE ONLY transcript_boundaries
            ADD CONSTRAINT transcript_boundaries_attempt_id_fkey FOREIGN KEY (attempt_id) REFERENCES attempts(id);
    END IF;
END
$$;

DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_constraint
                    WHERE conname = 'transcript_boundaries_project_id_fkey' AND connamespace = 'public'::regnamespace) THEN
        ALTER TABLE ONLY transcript_boundaries
            ADD CONSTRAINT transcript_boundaries_project_id_fkey FOREIGN KEY (project_id) REFERENCES projects(id);
    END IF;
END
$$;

DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_constraint
                    WHERE conname = 'transcript_boundaries_run_id_fkey' AND connamespace = 'public'::regnamespace) THEN
        ALTER TABLE ONLY transcript_boundaries
            ADD CONSTRAINT transcript_boundaries_run_id_fkey FOREIGN KEY (run_id) REFERENCES runs(id);
    END IF;
END
$$;

DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_constraint
                    WHERE conname = 'trigger_deliveries_duplicate_of_fkey' AND connamespace = 'public'::regnamespace) THEN
        ALTER TABLE ONLY trigger_deliveries
            ADD CONSTRAINT trigger_deliveries_duplicate_of_fkey FOREIGN KEY (duplicate_of) REFERENCES trigger_deliveries(id);
    END IF;
END
$$;

DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_constraint
                    WHERE conname = 'trigger_deliveries_project_id_fkey' AND connamespace = 'public'::regnamespace) THEN
        ALTER TABLE ONLY trigger_deliveries
            ADD CONSTRAINT trigger_deliveries_project_id_fkey FOREIGN KEY (project_id) REFERENCES projects(id);
    END IF;
END
$$;

DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_constraint
                    WHERE conname = 'trigger_deliveries_trigger_id_fkey' AND connamespace = 'public'::regnamespace) THEN
        ALTER TABLE ONLY trigger_deliveries
            ADD CONSTRAINT trigger_deliveries_trigger_id_fkey FOREIGN KEY (trigger_id) REFERENCES triggers(id) ON DELETE CASCADE;
    END IF;
END
$$;

DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_constraint
                    WHERE conname = 'trigger_deliveries_trigger_revision_id_fkey' AND connamespace = 'public'::regnamespace) THEN
        ALTER TABLE ONLY trigger_deliveries
            ADD CONSTRAINT trigger_deliveries_trigger_revision_id_fkey FOREIGN KEY (trigger_revision_id) REFERENCES trigger_revisions(id);
    END IF;
END
$$;

DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_constraint
                    WHERE conname = 'trigger_revisions_agent_revision_id_fkey' AND connamespace = 'public'::regnamespace) THEN
        ALTER TABLE ONLY trigger_revisions
            ADD CONSTRAINT trigger_revisions_agent_revision_id_fkey FOREIGN KEY (agent_revision_id) REFERENCES agent_revisions(id);
    END IF;
END
$$;

DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_constraint
                    WHERE conname = 'trigger_revisions_callback_endpoint_id_fkey' AND connamespace = 'public'::regnamespace) THEN
        ALTER TABLE ONLY trigger_revisions
            ADD CONSTRAINT trigger_revisions_callback_endpoint_id_fkey FOREIGN KEY (callback_endpoint_id) REFERENCES webhook_endpoints(id);
    END IF;
END
$$;

DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_constraint
                    WHERE conname = 'trigger_revisions_project_id_fkey' AND connamespace = 'public'::regnamespace) THEN
        ALTER TABLE ONLY trigger_revisions
            ADD CONSTRAINT trigger_revisions_project_id_fkey FOREIGN KEY (project_id) REFERENCES projects(id);
    END IF;
END
$$;

DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_constraint
                    WHERE conname = 'trigger_revisions_run_template_revision_id_fkey' AND connamespace = 'public'::regnamespace) THEN
        ALTER TABLE ONLY trigger_revisions
            ADD CONSTRAINT trigger_revisions_run_template_revision_id_fkey FOREIGN KEY (run_template_revision_id) REFERENCES run_template_revisions(id);
    END IF;
END
$$;

DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_constraint
                    WHERE conname = 'trigger_revisions_trigger_id_fkey' AND connamespace = 'public'::regnamespace) THEN
        ALTER TABLE ONLY trigger_revisions
            ADD CONSTRAINT trigger_revisions_trigger_id_fkey FOREIGN KEY (trigger_id) REFERENCES triggers(id) ON DELETE CASCADE;
    END IF;
END
$$;

DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_constraint
                    WHERE conname = 'triggers_project_id_fkey' AND connamespace = 'public'::regnamespace) THEN
        ALTER TABLE ONLY triggers
            ADD CONSTRAINT triggers_project_id_fkey FOREIGN KEY (project_id) REFERENCES projects(id);
    END IF;
END
$$;

DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_constraint
                    WHERE conname = 'usage_ledger_project_id_fkey' AND connamespace = 'public'::regnamespace) THEN
        ALTER TABLE ONLY usage_ledger
            ADD CONSTRAINT usage_ledger_project_id_fkey FOREIGN KEY (project_id) REFERENCES projects(id);
    END IF;
END
$$;

DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_constraint
                    WHERE conname = 'webhook_deliveries_project_id_fkey' AND connamespace = 'public'::regnamespace) THEN
        ALTER TABLE ONLY webhook_deliveries
            ADD CONSTRAINT webhook_deliveries_project_id_fkey FOREIGN KEY (project_id) REFERENCES projects(id);
    END IF;
END
$$;

DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_constraint
                    WHERE conname = 'webhook_endpoints_project_id_fkey' AND connamespace = 'public'::regnamespace) THEN
        ALTER TABLE ONLY webhook_endpoints
            ADD CONSTRAINT webhook_endpoints_project_id_fkey FOREIGN KEY (project_id) REFERENCES projects(id);
    END IF;
END
$$;

DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_constraint
                    WHERE conname = 'workspace_allocations_project_id_fkey' AND connamespace = 'public'::regnamespace) THEN
        ALTER TABLE ONLY workspace_allocations
            ADD CONSTRAINT workspace_allocations_project_id_fkey FOREIGN KEY (project_id) REFERENCES projects(id);
    END IF;
END
$$;

DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_constraint
                    WHERE conname = 'workspace_allocations_workspace_id_fkey' AND connamespace = 'public'::regnamespace) THEN
        ALTER TABLE ONLY workspace_allocations
            ADD CONSTRAINT workspace_allocations_workspace_id_fkey FOREIGN KEY (workspace_id) REFERENCES workspaces(id);
    END IF;
END
$$;

DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_constraint
                    WHERE conname = 'workspace_leases_allocation_id_fkey' AND connamespace = 'public'::regnamespace) THEN
        ALTER TABLE ONLY workspace_leases
            ADD CONSTRAINT workspace_leases_allocation_id_fkey FOREIGN KEY (allocation_id) REFERENCES workspace_allocations(id);
    END IF;
END
$$;

DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_constraint
                    WHERE conname = 'workspace_leases_project_id_fkey' AND connamespace = 'public'::regnamespace) THEN
        ALTER TABLE ONLY workspace_leases
            ADD CONSTRAINT workspace_leases_project_id_fkey FOREIGN KEY (project_id) REFERENCES projects(id);
    END IF;
END
$$;

DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_constraint
                    WHERE conname = 'workspace_leases_run_id_fkey' AND connamespace = 'public'::regnamespace) THEN
        ALTER TABLE ONLY workspace_leases
            ADD CONSTRAINT workspace_leases_run_id_fkey FOREIGN KEY (run_id) REFERENCES runs(id);
    END IF;
END
$$;

DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_constraint
                    WHERE conname = 'workspace_leases_workspace_id_fkey' AND connamespace = 'public'::regnamespace) THEN
        ALTER TABLE ONLY workspace_leases
            ADD CONSTRAINT workspace_leases_workspace_id_fkey FOREIGN KEY (workspace_id) REFERENCES workspaces(id);
    END IF;
END
$$;

DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_constraint
                    WHERE conname = 'workspace_snapshots_allocation_id_fkey' AND connamespace = 'public'::regnamespace) THEN
        ALTER TABLE ONLY workspace_snapshots
            ADD CONSTRAINT workspace_snapshots_allocation_id_fkey FOREIGN KEY (allocation_id) REFERENCES workspace_allocations(id);
    END IF;
END
$$;

DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_constraint
                    WHERE conname = 'workspace_snapshots_boundary_id_fkey' AND connamespace = 'public'::regnamespace) THEN
        ALTER TABLE ONLY workspace_snapshots
            ADD CONSTRAINT workspace_snapshots_boundary_id_fkey FOREIGN KEY (boundary_id) REFERENCES transcript_boundaries(id);
    END IF;
END
$$;

DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_constraint
                    WHERE conname = 'workspace_snapshots_project_id_fkey' AND connamespace = 'public'::regnamespace) THEN
        ALTER TABLE ONLY workspace_snapshots
            ADD CONSTRAINT workspace_snapshots_project_id_fkey FOREIGN KEY (project_id) REFERENCES projects(id);
    END IF;
END
$$;

DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_constraint
                    WHERE conname = 'workspace_snapshots_workspace_id_fkey' AND connamespace = 'public'::regnamespace) THEN
        ALTER TABLE ONLY workspace_snapshots
            ADD CONSTRAINT workspace_snapshots_workspace_id_fkey FOREIGN KEY (workspace_id) REFERENCES workspaces(id);
    END IF;
END
$$;

DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_constraint
                    WHERE conname = 'workspaces_project_id_fkey' AND connamespace = 'public'::regnamespace) THEN
        ALTER TABLE ONLY workspaces
            ADD CONSTRAINT workspaces_project_id_fkey FOREIGN KEY (project_id) REFERENCES projects(id);
    END IF;
END
$$;

DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_constraint
                    WHERE conname = 'workspaces_run_id_fkey' AND connamespace = 'public'::regnamespace) THEN
        ALTER TABLE ONLY workspaces
            ADD CONSTRAINT workspaces_run_id_fkey FOREIGN KEY (run_id) REFERENCES runs(id);
    END IF;
END
$$;

DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_constraint
                    WHERE conname = 'workspaces_session_id_fkey' AND connamespace = 'public'::regnamespace) THEN
        ALTER TABLE ONLY workspaces
            ADD CONSTRAINT workspaces_session_id_fkey FOREIGN KEY (session_id) REFERENCES sessions(id);
    END IF;
END
$$;

-- --------------------------------------------------------------- indexes

CREATE INDEX IF NOT EXISTS a2a_remote_agents_project_idx ON a2a_remote_agents USING btree (project_id, created_at DESC);

CREATE INDEX IF NOT EXISTS a2a_task_refs_interface_idx ON a2a_task_refs USING btree (interface_id, created_at DESC);

CREATE INDEX IF NOT EXISTS agent_revisions_by_profile ON agent_revisions USING btree (profile_id, revision_number DESC);

CREATE INDEX IF NOT EXISTS approvals_tool_call_expiry ON approvals USING btree (expires_at) WHERE (tool_call_id IS NOT NULL);

CREATE UNIQUE INDEX IF NOT EXISTS approvals_tool_call_key ON approvals USING btree (tool_call_id);

CREATE UNIQUE INDEX IF NOT EXISTS attempts_one_active_per_run ON attempts USING btree (run_id) WHERE (state = ANY (ARRAY['assigned'::text, 'starting'::text, 'active'::text, 'draining'::text]));

CREATE INDEX IF NOT EXISTS background_tasks_running_idx ON background_tasks USING btree (state) WHERE (state = 'running'::text);

CREATE INDEX IF NOT EXISTS background_tasks_running_machine_idx ON background_tasks USING btree (machine_id) WHERE (state = 'running'::text);

CREATE UNIQUE INDEX IF NOT EXISTS capability_jobs_idempotency_idx ON capability_jobs USING btree (project_id, idempotency_key) WHERE ((entry_seq = 1) AND (idempotency_key <> ''::text));

CREATE INDEX IF NOT EXISTS capability_jobs_job_seq_idx ON capability_jobs USING btree (job_id, entry_seq DESC);

CREATE INDEX IF NOT EXISTS capability_workers_capability_idx ON capability_workers USING btree (project_id, capability) WHERE (health = 'healthy'::text);

CREATE INDEX IF NOT EXISTS checkpoints_by_run ON checkpoints USING btree (run_id, created_at DESC);

CREATE INDEX IF NOT EXISTS chunk_revisions_docrev_idx ON chunk_revisions USING btree (project_id, document_revision_id);

CREATE INDEX IF NOT EXISTS chunk_revisions_fts_idx ON chunk_revisions USING gin (fts);

CREATE INDEX IF NOT EXISTS commands_run_pending_idx ON commands USING btree (run_id, created_at) WHERE (state = 'queued'::text);

CREATE INDEX IF NOT EXISTS config_revisions_session_seq_idx ON config_revisions USING btree (session_id, sequence DESC);

CREATE INDEX IF NOT EXISTS delivered_messages_run_boundary_idx ON delivered_messages USING btree (run_id, boundary_request_id);

CREATE INDEX IF NOT EXISTS deployment_desired_scope_tip_idx ON deployment_desired USING btree (plane, scope_id, revision DESC);

CREATE INDEX IF NOT EXISTS durable_jobs_claimable_idx ON durable_jobs USING btree (status, ready_at, lease_expires_at);

CREATE INDEX IF NOT EXISTS events_journal_id_idx ON events USING btree (journal_id);

CREATE INDEX IF NOT EXISTS events_response_id_idx ON events USING btree (response_id);

CREATE INDEX IF NOT EXISTS hooks_point_order_idx ON hooks USING btree (project_id, hook_point, created_at, id);

CREATE INDEX IF NOT EXISTS knowledge_sources_kb_idx ON knowledge_sources USING btree (project_id, knowledge_base_id);

CREATE INDEX IF NOT EXISTS merge_records_by_parent_run ON merge_records USING btree (parent_run_id);

CREATE INDEX IF NOT EXISTS publications_run_approved ON publications USING btree (project_id, run_id) WHERE (state = 'approved'::text);

CREATE INDEX IF NOT EXISTS publications_session_pending ON publications USING btree (project_id, session_id) WHERE (state = 'pending_approval'::text);

CREATE INDEX IF NOT EXISTS queue_deliveries_due_idx ON queue_deliveries USING btree (next_attempt_at) WHERE (state = 'pending'::text);

CREATE INDEX IF NOT EXISTS queue_messages_deliverable_idx ON queue_messages USING btree (queue_connection_id, enqueued_at) WHERE (state = ANY (ARRAY['ready'::text, 'leased'::text]));

CREATE INDEX IF NOT EXISTS remote_tool_operations_by_call ON remote_tool_operations USING btree (tool_call_id);

CREATE UNIQUE INDEX IF NOT EXISTS remote_tool_operations_one_pending ON remote_tool_operations USING btree (tool_call_id) WHERE (state = 'pending'::text);

CREATE INDEX IF NOT EXISTS repository_bindings_live_idx ON repository_bindings USING btree (project_id, archived_at);

CREATE INDEX IF NOT EXISTS responses_session_created_idx ON responses USING btree (session_id, created_at, id);

CREATE INDEX IF NOT EXISTS runner_enrollments_runner_seq_idx ON runner_enrollments USING btree (runner_id, entry_seq DESC);

CREATE INDEX IF NOT EXISTS runner_pool_keys_pool_idx ON runner_pool_keys USING btree (pool_id);

-- Two partial indexes rather than one composite: a UNIQUE index treats NULLs as DISTINCT, so
-- (project_id, name) would stop refusing duplicates the moment project_id became optional -- every
-- free pool could take a name another free pool already had, and the refusal InsertRunnerPool
-- relies on (it carries NO `ON CONFLICT`) would simply never fire.
CREATE UNIQUE INDEX IF NOT EXISTS runner_pools_free_name_key ON runner_pools USING btree (name)
    WHERE (project_id IS NULL);
CREATE UNIQUE INDEX IF NOT EXISTS runner_pools_name_key ON runner_pools USING btree (project_id, name)
    WHERE (project_id IS NOT NULL);

CREATE UNIQUE INDEX IF NOT EXISTS runners_dns_key ON runners USING btree (runner_dns) WHERE (runner_dns <> ''::text);

CREATE INDEX IF NOT EXISTS runners_pool_idx ON runners USING btree (pool_id, state);

CREATE INDEX IF NOT EXISTS runners_tenant_page_idx ON runners USING btree (project_id, created_at DESC, id DESC);

CREATE UNIQUE INDEX IF NOT EXISTS runs_one_active_root_per_session ON runs USING btree (session_id) WHERE ((state <> ALL (ARRAY['completed'::text, 'failed'::text, 'canceled'::text, 'timed_out'::text, 'budget_exceeded'::text])) AND (parent_run_id IS NULL));

CREATE INDEX IF NOT EXISTS runs_session_created_idx ON runs USING btree (session_id, created_at);

CREATE INDEX IF NOT EXISTS schedule_occurrences_pending_idx ON schedule_occurrences USING btree (planned_at) WHERE (state = 'pending'::text);

CREATE INDEX IF NOT EXISTS schedules_due_scan_idx ON schedules USING btree (next_fire_at) WHERE ((status = 'active'::text) AND (deleted_at IS NULL) AND (next_fire_at IS NOT NULL));

CREATE INDEX IF NOT EXISTS sessions_tenant_keyset_idx ON sessions USING btree (project_id, created_at DESC, id DESC);

CREATE INDEX IF NOT EXISTS slack_approval_deliveries_due_idx ON slack_approval_deliveries USING btree (next_attempt_at) WHERE (state = 'pending'::text);

CREATE INDEX IF NOT EXISTS slack_reply_deliveries_due_idx ON slack_reply_deliveries USING btree (next_attempt_at) WHERE (state = 'pending'::text);

CREATE INDEX IF NOT EXISTS slack_thread_sessions_session_idx ON slack_thread_sessions USING btree (session_id);

CREATE INDEX IF NOT EXISTS tasks_session_created_idx ON tasks USING btree (session_id, created_at);

CREATE INDEX IF NOT EXISTS tool_revisions_by_tool ON tool_revisions USING btree (tool_id, revision_number DESC);

CREATE UNIQUE INDEX IF NOT EXISTS trigger_deliveries_dedupe_canonical_idx ON trigger_deliveries USING btree (trigger_id, dedupe_key) WHERE ((dedupe_key <> ''::text) AND (duplicate_of IS NULL));

CREATE INDEX IF NOT EXISTS trigger_deliveries_deferred_fifo_idx ON trigger_deliveries USING btree (trigger_id, correlation_key_hash, received_at) WHERE (state = 'deferred'::text);

CREATE UNIQUE INDEX IF NOT EXISTS trigger_deliveries_source_dedupe_idx ON trigger_deliveries USING btree (trigger_id, source, source_tenant, source_event_id) WHERE ((source_event_id <> ''::text) AND (duplicate_of IS NULL));

CREATE INDEX IF NOT EXISTS usage_ledger_session_idx ON usage_ledger USING btree (project_id, session_id, meter);

CREATE INDEX IF NOT EXISTS usage_ledger_tenant_keyset_idx ON usage_ledger USING btree (occurred_at DESC, id DESC);

CREATE INDEX IF NOT EXISTS usage_ledger_tenant_meter_idx ON usage_ledger USING btree (project_id, meter, occurred_at DESC);

CREATE INDEX IF NOT EXISTS usage_ledger_tenant_series_idx ON usage_ledger USING btree (project_id, occurred_at) INCLUDE (meter, unit, quantity);

CREATE INDEX IF NOT EXISTS webhook_deliveries_due_idx ON webhook_deliveries USING btree (next_attempt_at) WHERE (state = 'pending'::text);

CREATE INDEX IF NOT EXISTS webhook_endpoints_project_idx ON webhook_endpoints USING btree (project_id) WHERE enabled;

CREATE UNIQUE INDEX IF NOT EXISTS workspace_leases_one_active_writer ON workspace_leases USING btree (workspace_id) WHERE (state = 'active'::text);

-- ------------------------------------------------- the run-terminal trigger

CREATE OR REPLACE FUNCTION enforce_run_terminal_final() RETURNS trigger
    LANGUAGE plpgsql
    AS $$
BEGIN
    IF OLD.state IN ('completed', 'failed', 'canceled', 'timed_out', 'budget_exceeded')
        AND NEW.state IS DISTINCT FROM OLD.state THEN
        RAISE EXCEPTION 'run % is terminal (%): cannot transition to %',
            OLD.id, OLD.state, NEW.state
            USING ERRCODE = 'check_violation';
    END IF;
    RETURN NEW;
END;
$$;

CREATE OR REPLACE TRIGGER runs_terminal_final BEFORE UPDATE ON runs FOR EACH ROW EXECUTE FUNCTION enforce_run_terminal_final();

-- --------------------------------------------------------------- comments

COMMENT ON COLUMN usage_ledger.model_request_id IS 'The model request (turn) this settlement is attributed to. NULL means the meter does not describe a model call — run.admitted is the admission reservation and has no step. Not a foreign key: the ledger outlives retention of the rows it prices.';

-- ---------------------------------------------------------------- grants

GRANT SELECT,INSERT,DELETE,UPDATE ON TABLE a2a_interfaces TO palai_app;

GRANT SELECT,INSERT,DELETE,UPDATE ON TABLE a2a_remote_agents TO palai_app;

GRANT SELECT,INSERT,DELETE,UPDATE ON TABLE a2a_task_refs TO palai_app;

GRANT SELECT,INSERT,DELETE,UPDATE ON TABLE agent_profiles TO palai_app;

GRANT SELECT,INSERT,DELETE,UPDATE ON TABLE agent_revisions TO palai_app;

GRANT SELECT,INSERT,DELETE,UPDATE ON TABLE api_keys TO palai_app;

GRANT SELECT,INSERT,DELETE,UPDATE ON TABLE approvals TO palai_app;

GRANT SELECT,INSERT,DELETE,UPDATE ON TABLE artifacts TO palai_app;

GRANT SELECT,INSERT,DELETE,UPDATE ON TABLE attempts TO palai_app;

GRANT SELECT,INSERT ON TABLE audit_events TO palai_app;

GRANT SELECT,USAGE ON SEQUENCE audit_events_id_seq TO palai_app;

GRANT SELECT,INSERT,UPDATE ON TABLE background_tasks TO palai_app;

GRANT SELECT,INSERT,DELETE,UPDATE ON TABLE budgets TO palai_app;

GRANT SELECT,INSERT ON TABLE capability_jobs TO palai_app;

GRANT SELECT,INSERT,DELETE,UPDATE ON TABLE capability_workers TO palai_app;

GRANT SELECT,INSERT,DELETE,UPDATE ON TABLE changeset_findings TO palai_app;

GRANT SELECT,INSERT,DELETE,UPDATE ON TABLE changesets TO palai_app;

GRANT SELECT,INSERT,DELETE ON TABLE checkpoints TO palai_app;

GRANT SELECT,INSERT ON TABLE chunk_revisions TO palai_app;

GRANT SELECT,INSERT,DELETE,UPDATE ON TABLE commands TO palai_app;

GRANT SELECT,INSERT,DELETE,UPDATE ON TABLE config_revisions TO palai_app;

GRANT SELECT,INSERT,DELETE,UPDATE ON TABLE delivered_messages TO palai_app;

GRANT SELECT,INSERT,DELETE,UPDATE ON TABLE delivery_attempts TO palai_app;

GRANT SELECT,USAGE ON SEQUENCE delivery_attempts_id_seq TO palai_app;

GRANT SELECT,INSERT ON TABLE deployment_desired TO palai_app;

GRANT SELECT,USAGE ON SEQUENCE deployment_desired_revision_seq TO palai_app;

GRANT SELECT,INSERT ON TABLE document_revisions TO palai_app;

GRANT SELECT,INSERT,DELETE,UPDATE ON TABLE durable_jobs TO palai_app;

GRANT SELECT,INSERT,DELETE ON TABLE environment_values TO palai_app;

GRANT SELECT,INSERT ON TABLE environments TO palai_app;

GRANT SELECT,INSERT,DELETE,UPDATE ON TABLE events TO palai_app;

GRANT SELECT,USAGE ON SEQUENCE events_journal_id_seq TO palai_app;

GRANT SELECT,INSERT,DELETE,UPDATE ON TABLE hooks TO palai_app;

GRANT SELECT,INSERT,DELETE,UPDATE ON TABLE host_quarantine TO palai_app;

GRANT SELECT,INSERT,DELETE,UPDATE ON TABLE idempotency_records TO palai_app;

GRANT SELECT,USAGE ON SEQUENCE idempotency_records_id_seq TO palai_app;

GRANT SELECT,INSERT,DELETE,UPDATE ON TABLE inbox TO palai_app;

GRANT SELECT,USAGE ON SEQUENCE inbox_id_seq TO palai_app;

GRANT SELECT,INSERT ON TABLE index_revisions TO palai_app;

GRANT SELECT,INSERT,DELETE,UPDATE ON TABLE ingestion_jobs TO palai_app;

GRANT SELECT,INSERT,DELETE,UPDATE ON TABLE integration_bots TO palai_app;

GRANT SELECT,INSERT,DELETE,UPDATE ON TABLE job_attempts TO palai_app;

GRANT SELECT,USAGE ON SEQUENCE job_attempts_id_seq TO palai_app;

GRANT SELECT,INSERT,DELETE,UPDATE ON TABLE knowledge_bases TO palai_app;

GRANT SELECT,INSERT,DELETE,UPDATE ON TABLE knowledge_sources TO palai_app;

GRANT SELECT,INSERT,DELETE,UPDATE ON TABLE mcp_connections TO palai_app;

GRANT SELECT,INSERT,DELETE,UPDATE ON TABLE merge_records TO palai_app;

GRANT SELECT,INSERT,DELETE,UPDATE ON TABLE messages TO palai_app;

GRANT SELECT,INSERT,DELETE,UPDATE ON TABLE model_connections TO palai_app;

GRANT SELECT,INSERT,DELETE,UPDATE ON TABLE model_requests TO palai_app;

GRANT SELECT,INSERT,DELETE,UPDATE ON TABLE model_route_revisions TO palai_app;

GRANT SELECT,INSERT,DELETE,UPDATE ON TABLE model_routes TO palai_app;

GRANT SELECT,INSERT,DELETE,UPDATE ON TABLE outbox TO palai_app;

GRANT SELECT,USAGE ON SEQUENCE outbox_id_seq TO palai_app;

GRANT SELECT,INSERT,DELETE,UPDATE ON TABLE preparation_receipts TO palai_app;

GRANT SELECT,INSERT,DELETE,UPDATE ON TABLE principals TO palai_app;

GRANT SELECT,INSERT,DELETE,UPDATE ON TABLE projects TO palai_app;

GRANT SELECT,INSERT,DELETE,UPDATE ON TABLE publications TO palai_app;

GRANT SELECT,INSERT,DELETE,UPDATE ON TABLE queue_connections TO palai_app;

GRANT SELECT,INSERT,DELETE,UPDATE ON TABLE queue_deliveries TO palai_app;

GRANT SELECT,INSERT ON TABLE queue_effect_receipts TO palai_app;

GRANT SELECT,INSERT,DELETE,UPDATE ON TABLE queue_messages TO palai_app;

GRANT SELECT,INSERT,DELETE,UPDATE ON TABLE quotas TO palai_app;

GRANT SELECT,INSERT,DELETE,UPDATE ON TABLE remote_tool_operations TO palai_app;

GRANT SELECT,INSERT,DELETE,UPDATE ON TABLE repository_bindings TO palai_app;

GRANT SELECT,INSERT,DELETE,UPDATE ON TABLE responses TO palai_app;

GRANT SELECT,INSERT,DELETE,UPDATE ON TABLE run_template_revisions TO palai_app;

GRANT SELECT,INSERT ON TABLE runner_enrollments TO palai_app;

GRANT SELECT,INSERT,DELETE,UPDATE ON TABLE runner_leases TO palai_app;

GRANT SELECT,INSERT,DELETE,UPDATE ON TABLE runner_pool_keys TO palai_app;

GRANT SELECT,INSERT,DELETE,UPDATE ON TABLE runner_pools TO palai_app;

GRANT SELECT,INSERT,DELETE,UPDATE ON TABLE runners TO palai_app;

GRANT SELECT,INSERT,DELETE,UPDATE ON TABLE runs TO palai_app;

GRANT SELECT,INSERT,DELETE,UPDATE ON TABLE schedule_occurrences TO palai_app;

GRANT SELECT,INSERT,DELETE,UPDATE ON TABLE schedules TO palai_app;

GRANT SELECT ON TABLE schema_migrations TO palai_app;

GRANT SELECT,INSERT ON TABLE schema_revisions TO palai_app;

GRANT SELECT,INSERT ON TABLE secret_refs TO palai_app;

GRANT SELECT,INSERT,DELETE,UPDATE ON TABLE session_sequences TO palai_app;

GRANT SELECT,INSERT,DELETE,UPDATE ON TABLE sessions TO palai_app;

GRANT SELECT,INSERT,DELETE,UPDATE ON TABLE skill_revisions TO palai_app;

GRANT SELECT,INSERT,DELETE,UPDATE ON TABLE skills TO palai_app;

GRANT SELECT,INSERT,DELETE,UPDATE ON TABLE slack_approval_deliveries TO palai_app;

GRANT SELECT,INSERT,DELETE,UPDATE ON TABLE slack_connections TO palai_app;

GRANT SELECT,INSERT,DELETE,UPDATE ON TABLE slack_message_turns TO palai_app;

GRANT SELECT,INSERT,DELETE,UPDATE ON TABLE slack_reply_deliveries TO palai_app;

GRANT SELECT,INSERT,DELETE,UPDATE ON TABLE slack_thread_sessions TO palai_app;

GRANT SELECT,INSERT,DELETE,UPDATE ON TABLE tasks TO palai_app;

GRANT SELECT,INSERT,DELETE,UPDATE ON TABLE tool_calls TO palai_app;

GRANT SELECT,INSERT,DELETE,UPDATE ON TABLE tool_revisions TO palai_app;

GRANT SELECT,INSERT,DELETE,UPDATE ON TABLE tool_set_revisions TO palai_app;

GRANT SELECT,INSERT,DELETE,UPDATE ON TABLE tools TO palai_app;

GRANT SELECT,INSERT,DELETE ON TABLE transcript_boundaries TO palai_app;

GRANT SELECT,INSERT,DELETE,UPDATE ON TABLE trigger_deliveries TO palai_app;

GRANT SELECT,INSERT,DELETE,UPDATE ON TABLE trigger_revisions TO palai_app;

GRANT SELECT,INSERT,DELETE,UPDATE ON TABLE triggers TO palai_app;

GRANT SELECT,INSERT ON TABLE usage_ledger TO palai_app;

GRANT SELECT,INSERT,DELETE,UPDATE ON TABLE webhook_deliveries TO palai_app;

GRANT SELECT,INSERT,DELETE,UPDATE ON TABLE webhook_endpoints TO palai_app;

GRANT SELECT,INSERT,DELETE,UPDATE ON TABLE workspace_allocations TO palai_app;

GRANT SELECT,INSERT,DELETE,UPDATE ON TABLE workspace_leases TO palai_app;

GRANT SELECT,INSERT,DELETE,UPDATE ON TABLE workspace_snapshots TO palai_app;

GRANT SELECT,INSERT,DELETE,UPDATE ON TABLE workspaces TO palai_app;

-- The version marker. storage.OrderedMigrations reads the file name for its ordering and the boot runner
-- writes schema_revisions itself; this row is what the preflight reads as the database's schema head.
INSERT INTO schema_migrations (version) VALUES (1) ON CONFLICT DO NOTHING;
