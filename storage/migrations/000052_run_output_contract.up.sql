-- The run's OUTPUT CONTRACT (spec §22.7): the JSON Schema a run's final answer must satisfy,
-- resolved at admission from the request's `output` field and stored on the run.
--
-- WHY IT IS ON THE RUN. Two readers need it and neither holds the request body: model dispatch,
-- which turns it into the provider's own decoding constraint (response_format / output_config), and
-- finalize, which checks the produced answer before the run may be called completed. Re-deriving it
-- from the stored request at each step would let the two disagree, and the whole point of the
-- contract is that the thing asked of the model and the thing checked afterwards are the SAME
-- document. It sits beside `delegation` (000007), which is on the run for exactly the same reason.
--
-- Shape: {"format":"json_schema","name":<string>,"schema":{...},"strict":<bool>}. NULL means the
-- request named no format, which is every run created before 2026-08-01 and every run that does not
-- opt in — so a text run's row is unchanged and its model request stays bit-identical.
--
-- Only ALTER TABLE ADD COLUMN IF NOT EXISTS on a table 000001 created, so no new grants are needed
-- and the chain stays safe to re-run (Migrate is idempotent).
ALTER TABLE runs ADD COLUMN IF NOT EXISTS output_contract JSONB;

INSERT INTO schema_migrations (version) VALUES (52) ON CONFLICT DO NOTHING;
