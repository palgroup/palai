-- 000064 (background task machine, A.3 Task 6): a background task records WHICH MACHINE started it.
--
-- THE COLUMN EXISTS BECAUSE A PROBE ANSWERS WRONGLY RATHER THAN FAILING. 000047 gave this table
-- `posture` for exactly that reason and said so: "probing the wrong object is not an error, it is a
-- WRONG ANSWER". `machine_id` is the same argument one level out — posture says WHAT KIND of object the
-- handle names, and this says WHOSE KERNEL it names.
--
-- Measured before this migration (2026-08-03):
--
--   grep -n "type Handle" -A4 packages/tool-broker/background.go     -> Posture + Value, no machine
--   background_tasks columns                                          -> 13, none of them a machine
--   grep -rn 'o\.background|b\.orch\.background|r\.background' --include='*.go' \
--     apps/control-plane/internal/execution/ | grep -v _test | wc -l  -> 22
--
-- packages/tool-broker/background.go:91 already stated the ceiling in prose: for the unsandboxed-host
-- posture "liveness is a process probe on THIS MACHINE. The start time is the half that makes the pgid
-- trustworthy." That start time is a reading of one machine's clock, so the pair is unique WITHIN a
-- machine and says nothing across them. A control plane that never started a task finds no such process
-- group and cannot tell that from one that exited — kill(pgid, 0) returns ESRCH for both.
--
-- Survivable while there is exactly one control plane, which is what §3.6 D4 assumes and what
-- background.go's own spawn lock is written against. The sweep, however, is system-scoped
-- (RunningBackgroundTasks runs under WithSystemScope across every tenant), so a SECOND control plane
-- probes every still-running row against its own kernel — and the tree already models that second plane
-- for crash-restart (background_wake_component_test.go secondPlane).
--
-- '' IS "WHICH MACHINE IS UNKNOWN", AND UNKNOWN IS `lost` RATHER THAN `exited`. Every row written before
-- this migration carries the empty string, and there is no way to recover the machine after the fact:
-- the row never held it. `lost` already means precisely this — the process may still be running and this
-- control plane cannot prove it is ours — and it carries the safety property that matters, because a
-- lost handle is never signalled. Reading '' as `exited` would be the wrong answer this column exists to
-- stop, applied to every row that predates it.
--
-- DEFAULT '' rather than NULL so no reader has to spell the difference twice: the Go zero value, the
-- column default and "unknown" are one thing. NOT NULL for the same reason.
ALTER TABLE background_tasks ADD COLUMN IF NOT EXISTS machine_id TEXT NOT NULL DEFAULT '';

-- The reaper reads the running set on every tick and now routes by machine, so the partial index that
-- already serves that predicate carries the discriminator too. It is not a uniqueness claim: two
-- machines may legitimately hold the same handle value, which is the whole point.
CREATE INDEX IF NOT EXISTS background_tasks_running_machine_idx
    ON background_tasks (machine_id)
    WHERE state = 'running';

INSERT INTO schema_migrations (version) VALUES (64) ON CONFLICT DO NOTHING;
