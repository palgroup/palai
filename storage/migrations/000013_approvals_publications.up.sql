-- Publications + approvals (spec §30.8-30.12, §22.4-22.5; E09 Task 8). A Publication is one DECOMPOSED
-- side-effect operation on the external repository — push_branch or open_pull_request — each with its
-- own capability, credential, idempotency key, and remote receipt (§30.8). There is no atomic
-- "push + PR" unit: "branch pushed but PR not opened" is a legitimate intermediate state (two rows at
-- different states), not a partial failure. An Approval is the one-shot gate a side-effect tool
-- produces (§22.4): bound to the request hash it authorizes, so an edited argument set or a moved head
-- is a new tool call needing a fresh approval (REP-009). The push/PR tool records a pending publication
-- (it never pushes); an approve command transitions it to a DURABLE `approved` state that survives run
-- termination; the approval-pump publishes it at the next live-run boundary (E10 executes a still-
-- approved row after termination — the honest E09 ceiling).
--
-- CREATE ... IF NOT EXISTS keeps the migration idempotent (Migrate re-runs per boot; the
-- 000010/000012 pattern). Tenant scope is the composite (organization_id, project_id) FK to projects
-- every execution row carries (§39.2); the new tables are granted to palai_app explicitly.

CREATE TABLE IF NOT EXISTS publications (
    id TEXT PRIMARY KEY,
    organization_id TEXT NOT NULL,
    project_id TEXT NOT NULL,
    -- The session this publication belongs to: the command spine looks up a session's PENDING approval
    -- to decide whether an approve/deny has a target (§22.4). run_id is the authoring run whose approved
    -- publication the pump publishes; response_id scopes the journal events (nullable — a session-only op).
    session_id TEXT NOT NULL,
    run_id TEXT NOT NULL REFERENCES runs (id),
    response_id TEXT,
    -- The decomposed operation (spec §30.8). Merge/release/protected-branch push are excluded from the
    -- ordinary coding set and never recorded here by default (policy-gated in the tool/adapter).
    operation TEXT NOT NULL CHECK (operation IN ('push_branch', 'open_pull_request')),
    -- The exact destination the operation targets. head_sha is the approved content for a push (a push
    -- publishes this sha, not "current HEAD"); base is the PR base branch (empty for a push).
    remote TEXT NOT NULL DEFAULT '',
    branch TEXT NOT NULL DEFAULT '',
    base TEXT NOT NULL DEFAULT '',
    head_sha TEXT NOT NULL DEFAULT '',
    -- The operation-specific dedupe identity (decision (b), repositories.IdempotencyKey): a push
    -- includes the head SHA (a new head is a new push, never a silent force); a PR excludes it (a PR
    -- tracks the branch → one PR, REP-008). UNIQUE per tenant, so a duplicate request/callback resolves
    -- to the ORIGINAL row rather than a second pending approval or a duplicate push/PR.
    idempotency_key TEXT NOT NULL,
    -- The exact operation display shown for approval (spec §22.4): the repo diff / command / destination.
    -- The model's prose never replaces this — it is computed by infrastructure. args is the redacted
    -- argument set (title/body for a PR); a credential never enters either.
    display TEXT NOT NULL DEFAULT '',
    args JSONB NOT NULL DEFAULT '{}',
    -- The publication lifecycle (§30.8-30.12): pending_approval -> approved (approve) -> published
    -- (push.completed / pull_request.opened); or -> denied / -> expired. `approved` is NON-terminal and
    -- retry-safe: a failed publish leaves it approved so the next drive re-reconciles (REP-007) — the
    -- E10 detached-execution re-drive needs zero rework. There is no `failed` terminal by design.
    state TEXT NOT NULL DEFAULT 'pending_approval'
        CHECK (state IN ('pending_approval', 'approved', 'published', 'denied', 'expired')),
    -- The external receipt once published (spec §30.9-30.10): the remote SHA for a push, the PR id/URL
    -- for a pull request. Nullable until the publish lands.
    receipt JSONB,
    created_at TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp(),
    FOREIGN KEY (organization_id, project_id) REFERENCES projects (organization_id, id),
    UNIQUE (organization_id, project_id, idempotency_key)
);

-- The session's pending-approval lookup (command spine) and the run's approved-publication drain
-- (approval pump) are the two hot reads; index the state-scoped paths.
CREATE INDEX IF NOT EXISTS publications_session_pending
    ON publications (organization_id, project_id, session_id) WHERE state = 'pending_approval';
CREATE INDEX IF NOT EXISTS publications_run_approved
    ON publications (organization_id, project_id, run_id) WHERE state = 'approved';

CREATE TABLE IF NOT EXISTS approvals (
    id TEXT PRIMARY KEY,
    -- One approval gate per publication (the side-effect operation it authorizes). The publication holds
    -- the lifecycle state; this row is the durable one-shot BINDING + decision audit, not a mirrored
    -- state machine. ponytail: folded 1:1 with publications for E09's only approval producer; a second
    -- producer (E12 shell protected-path approvals) is when this grows an operation-agnostic target.
    publication_id TEXT NOT NULL REFERENCES publications (id),
    organization_id TEXT NOT NULL,
    project_id TEXT NOT NULL,
    -- The request hash the approval is bound to (spec §22.4, REP-009): an approve command must carry a
    -- hash equal to this, so a stale approve (the head moved → a new pending approval with a new hash)
    -- authorizes nothing. Always head-bound (repositories.RequestHash), even for a PR.
    request_hash TEXT NOT NULL,
    -- Who may approve (empty = any principal in tenant scope; policy narrows it later) and who decided.
    allowed_approver TEXT NOT NULL DEFAULT '',
    decided_by TEXT NOT NULL DEFAULT '',
    -- Optional minutes-scale expiry (spec §22.4 "expiry"); NULL = no explicit expiry in E09.
    expires_at TIMESTAMPTZ,
    created_at TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp(),
    FOREIGN KEY (organization_id, project_id) REFERENCES projects (organization_id, id),
    UNIQUE (publication_id)
);

GRANT SELECT, INSERT, UPDATE, DELETE ON publications, approvals TO palai_app;

INSERT INTO schema_migrations (version) VALUES (13) ON CONFLICT DO NOTHING;
