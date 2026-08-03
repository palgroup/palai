-- job_attempts.error — WHY a claimed job's attempt failed (spec §24.4).
--
-- THE ATTEMPT LEDGER RECORDED THAT EVERY ATTEMPT FAILED AND NOWHERE WHY, and that is not a gap in
-- coverage, it is the reason four separate defects took a night to find on 2026-08-02. `Worker.process`
-- called `store.Fail(ctx, claim, retry)` — the handler's error was a local variable that went out of
-- scope. `FailJob` writes a status and a backoff; `RecordJobOutcome` writes the word 'failed'; nothing
-- logs. So a live stack carried 11 dead response.run jobs and 208 preempted attempts, and the actual
-- cause — an engine exiting at its first model step — was only readable in a CONTAINER's stdout, which
-- no operator has after a restart and no support case ever includes.
--
-- IT GOES ON THE ATTEMPT AND NOT ON THE JOB, and that is the whole point rather than a normalisation
-- preference. Attempt 1 failing to dial and attempt 5 failing to reach a provider are two different
-- facts about one job; a single column on durable_jobs would keep the last one and silently overwrite
-- the four that explain the pattern. Reading the run whose eight jobs each burned five attempts is only
-- possible if each of those forty rows kept its own sentence.
--
-- NULLABLE, and the NULL means something specific: this attempt did not fail. A row whose `outcome` is
-- 'failed' with a NULL error is an attempt that failed BEFORE this column existed, which is every row
-- already in every deployment — deliberately distinguishable from an attempt that failed with an empty
-- message, which would be a bug in the writer rather than a fact about history.
--
-- The text is bounded at the writer (jobAttemptErrorLimit), not here: a CHECK would abort the failure
-- path itself, which would turn one failing job into a failing worker.
ALTER TABLE job_attempts
  ADD COLUMN IF NOT EXISTS error TEXT;

INSERT INTO schema_migrations (version) VALUES (58) ON CONFLICT DO NOTHING;
