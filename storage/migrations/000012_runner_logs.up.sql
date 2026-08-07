-- A machine's own log reaches the admin plane. Until this table there was no path at all: measured
-- 2026-08-07, the runner plane had four routes (`connect`, `enroll`, `renew`, `settings`) and none of
-- them carried a line the agent had written. An operator asking "what went wrong on that Mac" had to
-- ssh into it, which is exactly the answer a fleet of a hundred machines cannot give.
--
-- IT IS NOT TENANT DATA AND THE COLUMN SAYS SO. `project_id` is NULLABLE for the same reason
-- `runners.project_id` is: a machine can be plane-owned (a free-fleet Mac serving whoever the placement
-- gives it) and its boot log belongs to the operator of the installation, not to whichever customer
-- happened to be on it. A row written while a tenant's session held the machine carries that project so
-- the tenant can be shown their own; a row from an idle machine carries none.
CREATE TABLE IF NOT EXISTS runner_logs (
    id text NOT NULL,
    project_id text,
    runner_id text NOT NULL,
    -- `at` is the AGENT'S clock, and `received_at` is the plane's. Both, because they answer different
    -- questions and this tree has been burned by having only one: `at` orders the lines a machine wrote,
    -- `received_at` is when the plane could first have known — and the gap between them IS the outage
    -- signal when an agent reconnects after an hour offline and ships its backlog.
    at timestamp with time zone NOT NULL,
    received_at timestamp with time zone DEFAULT clock_timestamp() NOT NULL,
    -- level is free text rather than an enum: the agent's lines come from Go's log package and a schema
    -- that refused an unfamiliar word would drop the line that mattered. The reader filters; the writer
    -- never loses.
    level text NOT NULL DEFAULT '',
    -- session_id ties a line to the work it happened during, when the agent knows which. It is empty for
    -- everything a machine does between sessions — boot, enrolment, config, the idle sweep — and those
    -- are precisely the lines an infrastructure question is about.
    session_id text,
    message text NOT NULL,
    CONSTRAINT runner_logs_pkey PRIMARY KEY (id)
);

-- The read is always "this machine, newest first", so the index leads with the machine and orders by the
-- agent's own clock. A read that fell back to a sequential scan would be a per-page table scan on the
-- one table that grows fastest in the installation.
CREATE INDEX IF NOT EXISTS runner_logs_runner_at_idx ON runner_logs (runner_id, at DESC);

-- The session view is a different question with a different shape — "what did the machine say while my
-- session ran" — so it gets its own partial index rather than riding the one above.
CREATE INDEX IF NOT EXISTS runner_logs_session_idx ON runner_logs (session_id, at DESC) WHERE session_id IS NOT NULL;

GRANT SELECT, INSERT, DELETE ON TABLE runner_logs TO palai_app;

-- Row-level security, on the same terms as every other tenant-visible table: 000002's sweep ran before
-- this file existed, so this migration applies the policy itself or the table would be readable by every
-- scope until the next boot.
CALL palai_apply_tenant_policy('runner_logs', 'project_id');

INSERT INTO schema_migrations (version) VALUES (12) ON CONFLICT DO NOTHING;
