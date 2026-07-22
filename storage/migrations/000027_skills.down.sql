-- Reverse 000027: drop the runs rider first, then the revision table (its FK references skills), then
-- the skill lineage table. IF EXISTS keeps the down idempotent.
ALTER TABLE runs DROP COLUMN IF EXISTS skill_pins;
DROP TABLE IF EXISTS skill_revisions;
DROP TABLE IF EXISTS skills;
