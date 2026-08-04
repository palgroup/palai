-- MACHINE OCCUPANCY (Faz A.4 T1, migration 000003_lease_occupancy). One row of runner_leases is one
-- interval in which one session held one machine, and these five statements are its whole life: take,
-- keep alive, give back, read one, read a session's.
--
-- WHY THE ROW IS PER OCCUPANCY AND NOT PER SESSION. A session is closed for idleness, its machine account
-- is destroyed, and the next approval resumes it on whichever machine is free — so one session holds N
-- machines over its life and the bill is the SUM of those holds. A per-session row would carry one
-- started_at and one released_at and would charge the customer for the gaps between holds, in which they
-- were holding nothing.
--
-- Every statement carries its tenant predicate explicitly even though 000002's row-level security already
-- confines the rows to the acquiring scope. That is this tree's rule, and here it is also what makes the
-- statements readable as the boundary they claim.

-- name: AcquireLease
-- Take a machine for a session. The row IS the occupancy: it opens now and stays open until released.
--
-- IT IS AN `INSERT ... SELECT` AND THE JOIN IS THE TENANCY CHECK, not decoration. The row's own project_id
-- is a parameter, so WITH CHECK on runner_leases would happily admit a lease this tenant owns that names
-- ANOTHER tenant's machine. Selecting the machine and the session out of their own tables instead means the
-- caller's project has to be able to SEE both — row-level security confines each read, and the predicates
-- below say the same thing in the statement — so a foreign machine or a foreign session writes no row at
-- all and the caller reads the empty result as a refusal.
--
-- THIS STATEMENT ENFORCES NO CAPACITY CEILING, and saying so is the point of the sentence: nothing here
-- refuses a second occupancy of a machine that is already held. `runners.capacity` exists (000001) and
-- nothing reads it. Placement — which machine a run goes to, and what happens when every machine is full —
-- is a later task in this phase; until it lands, the only thing stopping a double-booking is that exactly
-- one call site will exist.
INSERT INTO runner_leases (id, project_id, runner_id, session_id, started_at, last_activity_at)
SELECT $1, $2, r.id, s.id, clock_timestamp(), clock_timestamp()
  FROM runners r, sessions s
 WHERE r.id = $3 AND r.project_id = $2
   AND s.id = $4 AND s.project_id = $2
RETURNING id;

-- name: TouchLease
-- Move the occupancy's last activity to now. ONE WRITER, TWO READERS: the idle reaper reads this column to
-- know when to fire, and the bill reads it to know where to stop.
--
-- `released_at IS NULL` is what keeps the two readers honest. A touch that landed after the release would
-- change a settled bill — upward, and for a hold that had already ended.
UPDATE runner_leases
   SET last_activity_at = clock_timestamp()
 WHERE id = $1 AND project_id = $2
   AND released_at IS NULL;

-- name: ReleaseLease
-- Close the occupancy, with the reason it closed. The reason is not bookkeeping: 'idle' is the one word the
-- billed interval branches on (the CASE in GetOccupancy and SessionOccupancies below), and the CHECK
-- constraint 000003 adds is what stops a typo from quietly charging a customer for the idle tail.
--
-- WRITE-ONCE, via `released_at IS NULL`. A redelivered or repeated release must not move released_at
-- forward, because moving it forward extends the bill. The caller distinguishes "already released" from "no
-- such occupancy" by reading the row, rather than this statement guessing on its behalf.
UPDATE runner_leases
   SET released_at = clock_timestamp(),
       release_reason = $3
 WHERE id = $1 AND project_id = $2
   AND released_at IS NULL
RETURNING released_at;

-- name: GetOccupancy
-- One occupancy, with its billed interval derived rather than stored.
--
-- THE BILLING RULE IS THIS `CASE` AND IT LIVES IN EXACTLY ONE PLACE:
--
--     closed by idle : last_activity_at - started_at
--     any other way  : released_at      - started_at
--
-- WHAT IS SUBTRACTED FOR AN IDLE CLOSE IS THE COLUMN, NEVER A CONSTANT. `released_at - <ttl>` looks
-- equivalent and is not: a TTL is a setting, so it changes, and a reaper is a sweep, so it fires late by
-- however long its tick is. Both errors are paid by the customer.
--
-- An occupancy that is still OPEN has no final interval, so `coalesce(released_at, clock_timestamp())`
-- reports the elapsed time so far. That is an answer about a hold in progress, not a settled amount.
--
-- extract(epoch ...) is cast to double precision because extract returns numeric on PostgreSQL 14 and
-- later and a plain double on earlier ones; the cast makes the scan the same shape on both.
SELECT id, runner_id, session_id, started_at, last_activity_at, released_at,
       coalesce(release_reason, ''),
       extract(epoch FROM (
           CASE WHEN release_reason = 'idle' THEN last_activity_at
                ELSE coalesce(released_at, clock_timestamp()) END
           - started_at))::double precision
  FROM runner_leases
 WHERE id = $1 AND project_id = $2;

-- name: SessionOccupancies
-- Every machine this session has held, oldest first — which is what a bill for the session IS, and the
-- read the 000003 index (project_id, session_id, started_at) exists for.
--
-- ORDER BY IS TOTAL: (started_at, id). started_at alone ties when two occupancies open inside the same
-- clock tick, and an unordered result has decided the wrong thing in this tree more than once.
SELECT id, runner_id, session_id, started_at, last_activity_at, released_at,
       coalesce(release_reason, ''),
       extract(epoch FROM (
           CASE WHEN release_reason = 'idle' THEN last_activity_at
                ELSE coalesce(released_at, clock_timestamp()) END
           - started_at))::double precision
  FROM runner_leases
 WHERE project_id = $1 AND session_id = $2
 ORDER BY started_at, id;
