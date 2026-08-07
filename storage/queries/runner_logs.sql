-- name: AppendRunnerLog
-- One line a machine wrote. The id is minted by the caller so a redelivered batch collides on the
-- primary key rather than doubling a line — an agent that ships, loses the reply and ships again is the
-- ordinary case on a flaky link, not an error.
INSERT INTO runner_logs (id, project_id, runner_id, at, level, session_id, message)
VALUES ($1, nullif($2, ''), $3, $4, $5, nullif($6, ''), $7)
ON CONFLICT (id) DO NOTHING;

-- name: RunnerLogPage
-- One machine's lines, NEWEST FIRST, bounded.
--
-- ORDERED BY (at DESC, id DESC) AND NOT BY `at` ALONE: an agent ships a batch whose lines share a
-- millisecond, and an unordered tie is a page that can repeat or skip a line between two reads. The id
-- is the tiebreak because it is unique and stable, which is the property a cursor needs.
SELECT id, coalesce(project_id, ''), runner_id, at, received_at, level, coalesce(session_id, ''), message
FROM runner_logs
WHERE runner_id = $1
  AND ($2 = '' OR session_id = $2)
ORDER BY at DESC, id DESC
LIMIT $3;

-- name: TrimRunnerLog
-- Keeps the newest N lines for one machine and deletes the rest.
--
-- THE TABLE THAT GROWS FASTEST IN THE INSTALLATION IS THE ONE NOBODY WATCHES. A hundred machines
-- shipping their own logs is unbounded growth by construction, and "we will add retention later" is how
-- a disk fills on a Sunday. This runs on the write path, so the bound is enforced by the same call that
-- would breach it rather than by a sweeper somebody has to remember to wire.
DELETE FROM runner_logs
WHERE runner_id = $1
  AND id NOT IN (
      SELECT id FROM runner_logs
      WHERE runner_id = $1
      ORDER BY at DESC, id DESC
      LIMIT $2
  );
