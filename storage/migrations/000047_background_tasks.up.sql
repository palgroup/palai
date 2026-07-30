-- 000047 (E26 T1): A BACKGROUND TASK — a process that belongs to the RUN, not to the call that
-- started it.
--
-- E26's ONE migration, and T1 owns it because the epic's other six tasks put no file under
-- storage/migrations/. That boundary is not tidiness: two tasks that each take "the next contiguous
-- number" in isolation both build green and collide at merge, and the tree has a written procedure for
-- repairing exactly that (parallel-migration-contiguous-build). One owner means the repair is never
-- needed. The number is carried in the FILE NAME, the HEADER, the `VALUES` marker below, the embed
-- variable in storage/embed.go and the test name that asserts the chain head, all in the same commit —
-- verified through `git show HEAD` rather than through the working tree, because `git mv` stages the
-- OLD content and a post-move edit that is never re-added ships a file nobody read.
--
-- WHY A TABLE AT ALL, WHEN THE PROCESS IS A LIVE OBJECT OF THIS OPERATING SYSTEM. Because the process
-- has to outlive the thing that knows about it. Ownership today is bound to the `ShellRunner.Run` CALL
-- in both postures — the OCI driver force-removes its container on a `defer` (adapters/sandboxes/oci/
-- docker.go), and the host executor kills the process GROUP through the attempt context
-- (adapters/sandboxes/host/exec.go) — so every in-memory record of a background task dies with the
-- attempt that made it, and a control-plane restart would leave a running `xcodebuild` that nothing in
-- this program remembers. A row survives both.
--
-- WHAT IS DELIBERATELY NOT HERE: A SECOND `tool_calls.state`. 000044:24-25 says verbatim that
-- `tool_calls.state` is TEXT with NO CHECK, so a new state would need no migration at all — and it
-- would still be the wrong shape. A background PROCESS is not a tool CALL: one call spawns it and the
-- process outlives that call's ledger row. Hence a separate table with a single FK back to the call,
-- and `UNIQUE (tool_call_id)` so the relationship stays one-to-one in the database rather than in a
-- comment.

CREATE TABLE IF NOT EXISTS background_tasks (
    id TEXT PRIMARY KEY,
    organization_id TEXT NOT NULL REFERENCES organizations (id),
    project_id TEXT NOT NULL,
    -- The RUN is the owner. Every other id here is context; this one is the answer to "whose process is
    -- this", and it is what makes "cancelling a run kills its background work" expressible at all.
    run_id TEXT NOT NULL REFERENCES runs (id),
    session_id TEXT NOT NULL REFERENCES sessions (id),
    response_id TEXT NOT NULL REFERENCES responses (id),
    -- The call that spawned it. UNIQUE because one call spawns one process: a retried dispatch that
    -- reached the operating system twice would otherwise leave two live processes under one ledger row,
    -- and only one of them would ever be reaped.
    tool_call_id TEXT NOT NULL REFERENCES tool_calls (id),
    -- attempt_fence IS NOT BOOKKEEPING. A run's attempt can be reclaimed at a higher fence while its
    -- background process is still alive; the old attempt does not know that and will happily write what
    -- it believes. Every write path compares this column and REFUSES a stale attempt's update, the same
    -- discipline tool_calls.fence carries (000001:262). Without it a superseded attempt could mark a
    -- live task `exited`, and the reaper would then never look at it again.
    attempt_fence BIGINT NOT NULL DEFAULT 0 CHECK (attempt_fence >= 0),
    -- posture IS CHECKED BECAUSE THE PROBE LOOKS AT A DIFFERENT OPERATING-SYSTEM OBJECT PER POSTURE:
    -- `sandboxed-linux` means `handle` is a container id and liveness is a ContainerInspect;
    -- `unsandboxed-host` means `handle` is `pgid:starttime` and liveness is a process probe. An
    -- unrecognised value would send the reaper to probe the wrong kind of object — and the failure mode
    -- of probing the wrong object is not an error, it is a WRONG ANSWER: "no such container" reads as
    -- "the task exited" for a process that is still compiling.
    posture TEXT NOT NULL CHECK (posture IN ('sandboxed-linux', 'unsandboxed-host')),
    handle TEXT NOT NULL,
    -- `lost` is a first-class state and it is the one this table exists to be able to say. It means: a
    -- process may still be running and WE CANNOT PROVE IT IS OURS. The rule built on top of it is
    -- absolute — a `lost` task is never signalled — because PID reuse is real and killing a stranger's
    -- process is a worse outcome than leaving our own orphan alive.
    state TEXT NOT NULL CHECK (state IN ('running', 'exited', 'killed', 'expired', 'lost')),
    -- NULL MEANS "NOT KNOWN YET", AND THERE IS NO SENTINEL. Not -1, not 0. A watcher goroutine learns
    -- the exit code of a process it started; a control plane that restarted learns only that the process
    -- is gone. Writing -1 for the second case would hand a model a number it would compare against zero.
    -- Every line of docs/operations/known-gaps-1.0.md exists to refuse exactly that trade.
    exit_code INTEGER,
    -- Allocation-RELATIVE, so the row stays true after the allocation root moves and so nothing here
    -- discloses a host path (spec §29.9). The absolute path is reconstructed by joining the run's
    -- allocation root, which is the only component that knows it.
    output_path TEXT NOT NULL,
    -- KEY NAMES, NEVER VALUES. The values live in the process's environ for the whole life of the task
    -- (E26 §0.4) and in secret_refs; this column exists so the READ path can re-resolve them and mask
    -- them out of the log file before a single byte reaches the model (T6). A value in this column would
    -- put a credential in a row that a read route will one day return.
    env_keys TEXT[] NOT NULL DEFAULT '{}',
    -- THE ONLY TIME THE REAPER READS. It is a column and not a `context` for one reason, and it is the
    -- same reason the table exists: a context dies with the process that made it, and the whole point of
    -- a background task is to outlive that process. NULL = no ceiling, which is a choice an operator can
    -- make for a credential-free task and which T6 refuses for a task carrying env_keys.
    deadline_at TIMESTAMPTZ,
    started_at TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp(),
    finished_at TIMESTAMPTZ,
    -- THE EXACTLY-ONCE KEY for the exit notification (T4). Two reconciler ticks, two control planes and
    -- a crash-restart must produce ONE notification, and the winner is decided by a conditional UPDATE
    -- on this column rather than by an in-memory flag that a restart erases.
    notified_at TIMESTAMPTZ,
    UNIQUE (tool_call_id),
    FOREIGN KEY (organization_id, project_id) REFERENCES projects (organization_id, id)
);

-- The reaper's read is `state = 'running'`, and it is a cross-tenant sweep over a table that mostly
-- holds finished tasks. A PARTIAL index keeps that read proportional to the live set rather than to the
-- history — the same shape the tree uses wherever a sweep reads one state out of an accumulating table.
CREATE INDEX IF NOT EXISTS background_tasks_running_idx ON background_tasks (state) WHERE state = 'running';

-- Tenancy. project_id is present, so has_project is true: a background task is scoped to the project
-- whose run owns it, exactly as tool_calls is.
CALL palai_apply_tenant_policy('background_tasks', 'organization_id', true);

-- Created AFTER 000001's and 000029's blanket `GRANT ... ON ALL TABLES` — both re-run on every boot but
-- ran BEFORE this file on the boot that creates the table — so it needs its own grant or the runtime
-- role fails closed with "permission denied for table background_tasks" instead of with the row-scoped
-- policy.
--
-- UPDATE IS GRANTED AND DELETE IS NOT, and the asymmetry is the table's whole life cycle: a row moves
-- running -> exited/killed/expired/lost and takes a `notified_at` stamp, and it is never removed. What
-- T5 deletes on a TTL is the LOG FILE, which is bytes in an allocation; the row is the audit record that
-- a process existed, and an operator hunting orphans needs the `lost` rows to still be there.
GRANT SELECT, INSERT, UPDATE ON background_tasks TO palai_app;
REVOKE DELETE ON background_tasks FROM palai_app;

INSERT INTO schema_migrations (version) VALUES (47) ON CONFLICT DO NOTHING;
