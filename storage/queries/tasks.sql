-- name: GetTaskByKey
-- FOR UPDATE locks the row for the read-modify-write in UpsertTask, so two writers on the same
-- session under DIFFERENT run locks (E10's detached child) cannot lost-update each other. A new-task
-- create locks nothing here; the UNIQUE (session_id, task_key) index serializes concurrent creates.
SELECT id, kind, title, status, detail
FROM tasks
WHERE session_id = $1 AND task_key = $2 AND project_id = $3
FOR UPDATE;

-- name: InsertTask
INSERT INTO tasks (id, project_id, session_id, task_key, kind, title, status, detail)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8);

-- name: UpdateTaskByKey
UPDATE tasks
SET kind = $3, title = $4, status = $5, detail = $6, updated_at = clock_timestamp()
WHERE session_id = $1 AND task_key = $2;

-- name: ListTasksBySession
SELECT task_key, kind, title, status, detail
FROM tasks
WHERE session_id = $1 AND project_id = $2
ORDER BY created_at, task_key;
