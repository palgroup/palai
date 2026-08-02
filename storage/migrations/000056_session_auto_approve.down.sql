ALTER TABLE sessions
  DROP COLUMN IF EXISTS auto_approve_tools,
  DROP COLUMN IF EXISTS auto_approve_publications,
  DROP COLUMN IF EXISTS auto_approve_set_by,
  DROP COLUMN IF EXISTS auto_approve_set_at;
