-- name: GetTaskByKey
SELECT id, kind, title, status, detail
FROM tasks
WHERE session_id = $1 AND task_key = $2 AND organization_id = $3 AND project_id = $4;

-- name: InsertTask
INSERT INTO tasks (id, organization_id, project_id, session_id, task_key, kind, title, status, detail)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9);

-- name: UpdateTaskByKey
UPDATE tasks
SET kind = $3, title = $4, status = $5, detail = $6, updated_at = clock_timestamp()
WHERE session_id = $1 AND task_key = $2;

-- name: ListTasksBySession
SELECT task_key, kind, title, status, detail
FROM tasks
WHERE session_id = $1 AND organization_id = $2 AND project_id = $3
ORDER BY created_at, task_key;
