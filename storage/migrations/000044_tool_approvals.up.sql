-- E23 T1: APPROVAL STOPS BEING A PUBLICATION FEATURE AND BECOMES A TOOL-CALL PROPERTY.
--
-- The tree ordered this migration by name three years ago. 000013's own comment on the approvals table
-- reads: "ponytail: folded 1:1 with publications for E09's only approval producer; a second producer
-- (E12 shell protected-path approvals) is when this grows an operation-agnostic target." The second
-- producer is here, and it is not the one that comment guessed: it is every side-effecting tool call.
--
-- FOUR RIDERS, and the reason each one CANNOT be code:
--
--   R1  approvals.publication_id is NOT NULL REFERENCES publications(id). A tool call has no publication,
--       so a generic approval is not representable without altering the column. The CHECK makes the two
--       targets mutually exclusive rather than merely both-nullable: an approval with neither target
--       authorizes nothing and an approval with both is ambiguous about what a click means.
--   R2  publications.operation is a CHECK, so merging a pull request — the third operation the owner
--       actually asked for — is a migration and not a switch case. T6 writes the switch; this writes the
--       permission.
--   R3  a registry/MCP tool's gate has to survive a restart, so it is a column. A built-in declares it in
--       Go (toolbroker.Tool.RequiresApproval) and stores nothing at all.
--   R4  the Slack delivery of an approval message needs its own exactly-once row. It cannot reuse
--       slack_reply_deliveries, whose claim is UNIQUE (run_id): a run can produce several approvals, and
--       loosening a shipped exactly-once constraint to fit a new caller would weaken a guarantee that is
--       already relied on. A second table keyed UNIQUE (approval_id) leaves the first one's promise intact.
--
-- WHAT NEEDS NO MIGRATION, written down so the next reader does not add columns for it: the call's
-- awaiting state (tool_calls.state is TEXT with NO CHECK, and `approval_pending` is already in the §26.7
-- machine), the one-shot binding (tool_calls.request_hash already IS toolbroker.RequestHash(name, args)),
-- the run's parking (RunWaiting + RunCmdWait exist and guardRunActive does not treat waiting as terminal),
-- the approver list (config_policy JSONB, T2), and the expiry deadline (approvals.expires_at, 000013,
-- which until now nothing ever set).

-- ---------------------------------------------------------------------------------------------------
-- R1 — approvals gains an operation-agnostic target.
-- ---------------------------------------------------------------------------------------------------

ALTER TABLE approvals ALTER COLUMN publication_id DROP NOT NULL;

-- The gated call. No ON DELETE rule, matching publication_id: nothing in this tree deletes a tool_calls
-- row, and an approval that outlived its call would be a dangling authorization — better a loud FK error
-- than a silent one.
ALTER TABLE approvals ADD COLUMN IF NOT EXISTS tool_call_id TEXT REFERENCES tool_calls (id);

-- EXACTLY ONE target. `<>` on two IS NULL tests is XOR: neither and both are rejected. Dropped first so
-- the migration stays idempotent under the boot runner, which re-applies the whole chain every start.
ALTER TABLE approvals DROP CONSTRAINT IF EXISTS approvals_one_target;
ALTER TABLE approvals ADD CONSTRAINT approvals_one_target
    CHECK ((publication_id IS NULL) <> (tool_call_id IS NULL));

-- One approval per gated call, the tool-call twin of UNIQUE (publication_id). A unique INDEX rather than
-- a constraint because it must tolerate the many NULLs every publication-shaped row carries (Postgres
-- treats NULLs as distinct in both forms; the index form is the one that takes IF NOT EXISTS).
CREATE UNIQUE INDEX IF NOT EXISTS approvals_tool_call_key ON approvals (tool_call_id);

-- The reaper's read: still-open approvals past their deadline. Partial on a non-null tool_call_id so it
-- indexes only the generic half and leaves the publication sweep's plans untouched.
CREATE INDEX IF NOT EXISTS approvals_tool_call_expiry
    ON approvals (expires_at) WHERE tool_call_id IS NOT NULL;

-- ---------------------------------------------------------------------------------------------------
-- R2 — merging a pull request becomes a legal publication operation (T6 writes the executor).
-- ---------------------------------------------------------------------------------------------------

ALTER TABLE publications DROP CONSTRAINT IF EXISTS publications_operation_check;
ALTER TABLE publications ADD CONSTRAINT publications_operation_check
    CHECK (operation IN ('push_branch', 'open_pull_request', 'merge_pull_request'));

-- ---------------------------------------------------------------------------------------------------
-- R3 — a registered tool declares its gate, and the operator writes the one sentence a human reads.
-- ---------------------------------------------------------------------------------------------------

-- Default false is the bit-unchanged posture: every revision published before this migration keeps
-- running exactly as it did, and a gate appears only where an operator asked for one. Palai does NOT
-- classify tools as dangerous and does not try — the vendor forbids trusting a server's own hints
-- ("clients MUST consider tool annotations to be untrusted", MCP 2025-11-25 server/tools, fetched
-- 2026-07-29), so the declaration is the operator's, made beside the per-tool publish decision they
-- already make today.
ALTER TABLE tool_revisions ADD COLUMN IF NOT EXISTS approval_required BOOLEAN NOT NULL DEFAULT false;

-- The operator's label: the ONLY human sentence on the approval screen. It is a separate column rather
-- than a reuse of `description` because a discovered MCP revision's description is written by the SERVER,
-- and putting a server's sentence on the screen a human reads before authorizing that server's side
-- effect is precisely the failure the screen exists to prevent. Empty renders as `(no operator label)`.
ALTER TABLE tool_revisions ADD COLUMN IF NOT EXISTS approval_label TEXT NOT NULL DEFAULT '';

-- ---------------------------------------------------------------------------------------------------
-- R4 — the order to POST an approval message, following 000041's shape deliberately.
-- ---------------------------------------------------------------------------------------------------

-- Same shape as slack_reply_deliveries on purpose: an operator should read ONE delivery model, not two.
-- The row is written in the SAME transaction as the approval it announces, so "a human owes an answer"
-- and "the question is recorded for delivery" commit together; a single-winner pump posts it. T3 wires
-- the pump — this creates the durable half so the migration chain stops at one file for this epic.
CREATE TABLE IF NOT EXISTS slack_approval_deliveries (
    id TEXT PRIMARY KEY,
    organization_id TEXT NOT NULL REFERENCES organizations (id),
    project_id TEXT NOT NULL REFERENCES projects (id),
    connection_id TEXT NOT NULL REFERENCES slack_connections (id) ON DELETE CASCADE,
    -- The approval this announces. UNIQUE, and it is the whole exactly-once claim — the enqueue is
    -- ON CONFLICT DO NOTHING, so however many times the requesting transaction runs, one message is posted.
    -- Keyed on the APPROVAL and not the run, because one run can owe a human several separate answers.
    approval_id TEXT NOT NULL REFERENCES approvals (id) ON DELETE CASCADE,
    run_id TEXT NOT NULL REFERENCES runs (id) ON DELETE CASCADE,
    response_id TEXT NOT NULL DEFAULT '',
    -- FROZEN at enqueue, for 000041's reason: a thread correlation can legitimately be repaired away, and
    -- a question that lost its address is a question nobody answers and a run that waits forever.
    channel_id TEXT NOT NULL,
    thread_ts TEXT NOT NULL,
    state TEXT NOT NULL DEFAULT 'pending'
        CHECK (state IN ('pending', 'delivered', 'dead')),
    attempt_count INTEGER NOT NULL DEFAULT 0 CHECK (attempt_count >= 0),
    max_attempts INTEGER NOT NULL DEFAULT 8 CHECK (max_attempts > 0),
    next_attempt_at TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp(),
    -- The ts Slack assigned, so a later decision can REPAIR this exact message in place.
    message_ts TEXT NOT NULL DEFAULT '',
    created_at TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp(),
    FOREIGN KEY (organization_id, project_id) REFERENCES projects (organization_id, id),
    UNIQUE (approval_id)
);

CREATE INDEX IF NOT EXISTS slack_approval_deliveries_due_idx
    ON slack_approval_deliveries (next_attempt_at) WHERE state = 'pending';

CALL palai_apply_tenant_policy('slack_approval_deliveries', 'organization_id', true);
-- Created after 000029's blanket GRANT ... ON ALL TABLES, so this table needs its own grant or the runtime
-- role fails closed with "permission denied" rather than with the row-scoped policy.
GRANT SELECT, INSERT, UPDATE, DELETE ON slack_approval_deliveries TO palai_app;

INSERT INTO schema_migrations (version) VALUES (44) ON CONFLICT DO NOTHING;
