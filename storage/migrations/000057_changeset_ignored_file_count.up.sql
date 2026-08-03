-- changesets.ignored_file_count — what the changed-set DELIBERATELY leaves out (spec §30.6, REP-005).
--
-- The changed set is now compiled from two sources: the file-tool write ledger, and a git observation of
-- the clone that catches everything the shell tool wrote (a heredoc, `git apply`, a code generator, a
-- compiler). The observation raises a question the ledger never had to answer, because build output is
-- the bulk of what a compiler writes: MEASURED on the live spine 2026-08-02, one `swift build` left 1284
-- files under repo/.build/ in a single run.
--
-- Those are excluded from the file list, for the same reason `Commit`'s `git add -A` excludes them and
-- the patch never contains them: a changed set buried under object files is not a changed set. But
-- excluding them SILENTLY would rebuild, one layer quieter, the exact defect this work exists to remove —
-- a record that says "this run changed nothing" about a run that wrote 1284 files. So the count is kept.
--
-- DEFAULT 0 AND NOT NULL: every changeset written before this column existed reads 0, which is the same
-- answer the compiler gives when it looked and found none. That collision is acceptable here and only
-- here — a changeset row with no ignored count is a row from before anything scanned, and the scan is
-- what the column reports on. Nothing branches on the difference; the number is read by humans.
ALTER TABLE changesets
  ADD COLUMN IF NOT EXISTS ignored_file_count INTEGER NOT NULL DEFAULT 0;

INSERT INTO schema_migrations (version) VALUES (57) ON CONFLICT DO NOTHING;
