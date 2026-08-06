-- MACHINE OCCUPANCY (Faz A.4 T1, migration 000003_lease_occupancy). One row of runner_leases is one
-- interval in which one session held one machine, and these eight statements are its whole life: order the
-- contenders, take, keep alive, give back, read one, read a session's, say why a take was refused, and find
-- the holds whose closer never ran.
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

-- name: LockMachineForPlacement
-- Take the machine's row for the duration of an acquire, and report the capacity it declared. Returns no
-- row for a machine this tenant cannot see, which is the FIRST of AcquireLease's two refusals and the one
-- that can be told apart without asking anything else.
--
-- `FOR UPDATE` IS WHAT MAKES THE CEILING SINGLE-WINNER, AND WITHOUT IT THE PREDICATE IN AcquireLease IS
-- DECORATION. Under READ COMMITTED every statement takes its own snapshot at the moment it starts, so two
-- transactions acquiring the last slot at once both count the machine one short of full and both insert:
-- the machine ends up over its declared capacity and NEITHER caller is told, because both were told yes.
-- Serializing the contenders on the machine's own row is what turns that into one winner — the loser
-- blocks here, and the INSERT it runs afterwards takes a FRESH snapshot in which the winner's row is
-- committed and visible. That ordering is the whole reason this is a separate statement rather than a
-- `FOR UPDATE` inside AcquireLease's own CTE: a lock taken mid-statement does not re-take the snapshot the
-- statement's subqueries already read from, so the count would still be the stale one.
--
-- It locks `runners` rather than the lease rows because the thing being serialized is the machine's
-- OCCUPANCY as a whole, and the rows that would have to be locked to serialize it are the ones that do not
-- exist yet. The lock is held for two statements and no round trip to anything but the database.
SELECT capacity
  FROM runners
 WHERE id = $1 AND (project_id = $2 OR project_id IS NULL)
   FOR UPDATE;

-- name: AcquireLease
-- Take a machine for a session, IF the machine has a slot free. The row IS the occupancy: it opens now and
-- stays open until released.
--
-- IT IS AN `INSERT ... SELECT` AND THE JOIN IS THE TENANCY CHECK, not decoration. The row's own project_id
-- is a parameter, so WITH CHECK on runner_leases would happily admit a lease this tenant owns that names
-- ANOTHER tenant's machine. Selecting the machine and the session out of their own tables instead means the
-- caller's project has to be able to SEE both — row-level security confines each read, and the predicates
-- below say the same thing in the statement — so a foreign machine or a foreign session writes no row at
-- all and the caller reads the empty result as a refusal.
--
-- THE CAPACITY CEILING IS THE SUBQUERY, AND IT IS IN THE WRITE RATHER THAN IN FRONT OF IT (Faz A.4 T5).
-- `runners.capacity` has existed since 000001, and until this statement NO Go expression and no other
-- statement compared it to anything — it was written at enrolment, read back for display, and that was
-- all. A caller that counted the open holds and then inserted would be correct only until two callers did
-- it at once; here the count is evaluated by the same command that writes, so a machine that is full
-- matches no row and the caller reads the empty result as the refusal it is.
--
-- `r.capacity = 0` MEANS UNDECLARED AND ADMITS EVERYTHING, and getting that backwards is the one real trap
-- in this statement. Written as a bare `count(*) < r.capacity`, zero would compare as "no room at all" and
-- a machine that simply never said anything would refuse EVERY session — the exact inversion of what an
-- absent declaration means. Until 000005_capacity_declaration the column's CHECK refused zero and the
-- registry clamped every enrolment to 1, so nothing could reach this arm. The ceiling is opt-in, and this
-- is where the opt is read.
--
-- THE SENTENCE THAT STOOD HERE SAID "nothing BUT this arm is reached, because no machine in any deployment
-- declares a capacity yet", AND IT WAS TRUE UNTIL 05822d86. Counting which halves survived rather than
-- writing "now it works":
--   * "the ceiling is opt-in, and zero admits everything" — STILL TRUE, and it is the whole guard above.
--   * "no machine declares a capacity" — NO LONGER TRUE, and it was the operative half. The column was
--     declarable on the wire, read by the enrolment handler and written by the store, but cmd/runner's
--     loadConfig never filled it and no environment variable existed, so every machine in every deployment
--     enrolled at 0 and this arm was the only reachable one. Measured then: 47 machines, one distinct
--     capacity, 0. cmd/runner now reads PALAI_RUNNER_CAPACITY (compose interpolates it in 16e723da), and a
--     machine declaring 1 was read back from the database on a live stack — so the OTHER arm, the one this
--     subquery exists for, is REACHABLE. Reachable is the claim; whether a live run has yet been refused
--     by it is a separate measurement and is not asserted here.
--
-- WHAT IS COUNTED IS `released_at IS NULL` — the OPEN holds, not the holds ever taken. A machine whose
-- ceiling counted history would be placeable until its first few sessions ended and unplaceable forever
-- after, which on a fleet is every Mac going dark one by one.
--
-- ‼️ A MACHINE THE PLANE OWNS IS SERVABLE TO EVERY PROJECT, which is why the predicate is not a bare
-- equality. `runners.project_id IS NULL` is the free fleet (000002's palai_apply_fleet_policy), and
-- `NULL = $2` is NULL — so the equality alone silently refused every shared machine while the row sat
-- there, online, in the pool the run had already been placed into.
--
-- ‼️ AND THE COUNT LOST ITS PROJECT PREDICATE, which is the same change seen from the other side. It used
-- to read `l.project_id = r.project_id` under a paragraph arguing the predicate changed no row "because a
-- machine belongs to exactly one project". A machine on the free fleet belongs to NONE, and many projects
-- take turns on it — so that predicate stopped being a no-op and became a per-tenant ceiling: three
-- tenants each holding one session on a three-capacity Mac would each count 1, each be admitted, and the
-- Mac would carry double what it declared. palai_machine_open_occupancies (000003) counts the machine
-- across tenants, which is the property renting a Mac rests on.
INSERT INTO runner_leases (id, project_id, runner_id, session_id, started_at, last_activity_at)
SELECT $1, $2, r.id, s.id, clock_timestamp(), clock_timestamp()
  FROM runners r, sessions s
 WHERE r.id = $3 AND (r.project_id = $2 OR r.project_id IS NULL)
   AND s.id = $4 AND s.project_id = $2
   AND (r.capacity = 0 OR palai_machine_open_occupancies(r.id) < r.capacity)
RETURNING id;

-- name: MachineOpenOccupancies
-- How many holds are open on this machine right now. It is a DIAGNOSIS and never the decision: AcquireLease
-- above is what refuses, and this only names WHICH refusal that was.
--
-- The distinction matters because the two answers a caller can give are opposite. A machine at its ceiling
-- is a transient no that becomes yes the moment a hold settles; a machine (or a session) this tenant cannot
-- see is a no that will still be no on the next attempt. A zero-row INSERT cannot tell them apart on its
-- own, and guessing either way is how a caller ends up retrying forever or giving up on a machine that was
-- about to free.
--
-- It is read INSIDE the acquire's transaction, while LockMachineForPlacement still holds the machine's row,
-- so no other writer can move the number between the judgement and the explanation.
--
-- IT CARRIES THE SAME `released_at IS NULL` AS THE CEILING ABOVE, AND THAT IS A COUPLING RATHER THAN A
-- COINCIDENCE. These were the only two copies of "what counts as occupied" until PoolMachineOccupancies
-- below made a third, and if they ever disagree the refusal is still correct while its REASON becomes
-- wrong. Measured by perturbing exactly that on 2026-08-05: with the liveness predicate dropped from the
-- ceiling and left here, a machine that was genuinely full refused the acquire and the caller was told
-- "machine is not one this tenant can occupy" — an answer that sends the next reader to look at tenancy for
-- a capacity problem. Change one, change all three.
SELECT palai_machine_open_occupancies($1);

-- name: PoolMachineOccupancies
-- Every machine in one pool with the two numbers a PLACEMENT PREFERENCE needs: how many holds are open on
-- it right now, and the ceiling it declared. Read once per dial, and only when the gateway has more than
-- one machine actually free to choose between.
--
-- WHY IT EXISTS. Until Faz A.5 T6 the pool was chosen and the MACHINE was not: `fleet.ResolvePool` decided
-- which queue an attempt waited on, and the queue then handed over whichever machine happened to be at the
-- head of its parked list. That was invisible while every Mac pool held one machine. On a fleet it means a
-- Mac carrying three idle allocations and a Mac carrying none are the same candidate.
--
-- IT IS A PREFERENCE AND NEVER A REFUSAL, which is the whole reason it can be a plain read outside any
-- transaction while AcquireLease's own count has to be inside the write. Nothing here decides whether a
-- hold opens: by the time the caller acts on this answer it is already stale, and the statement that judges
-- the machine takes that machine's row `FOR UPDATE` first. What this buys is which machine is TRIED.
--
-- `capacity` IS RETURNED RAW, ZERO AND ALL, and the caller reads zero exactly as AcquireLease does —
-- declared nothing, therefore no ceiling, therefore always room. A statement that filtered full machines
-- out here would be a SECOND ceiling: weaker than the real one, disagreeing with it under contention, and
-- reading a machine that simply never declared anything as permanently full, which is the one real trap in
-- this column and is nailed down by a test.
--
-- NO STATE FILTER, AND THE OMISSION IS DELIBERATE. The caller ranks only the machines parked on its queue
-- at that moment, so a decommissioned machine is not a candidate to begin with. A filter here could
-- therefore only DROP a parked machine from the answer — and a machine missing from the answer is read as
-- "nothing is known about it", which ranks it as EMPTY. `state <> 'active'` would make a cordoned machine
-- that is still parked look like the emptiest Mac on the fleet.
--
-- THE OPEN COUNT IS THE THIRD COPY OF `released_at IS NULL` (see MachineOpenOccupancies above). If this one
-- drifts from the ceiling's, nothing is over-allocated — the ceiling still refuses — but every run is sent
-- to the machine most likely to refuse it, so the failure is a placement that reliably picks the wrong Mac
-- rather than a placement that overfills one.
--
-- ORDER BY r.id makes the result total. The caller folds these rows into a map keyed by machine, so this
-- order decides nothing today; it is here so a reader who turns them into a slice does not inherit an
-- arbitrary one.
SELECT r.id, r.capacity, palai_machine_open_occupancies(r.id)
  FROM runners r
 WHERE (r.project_id = $1 OR r.project_id IS NULL) AND r.pool_id = $2
 ORDER BY r.id;

-- name: MachinePool
-- Which pool does this machine belong to? Read when a hold on it SETTLES, because that is the moment a
-- slot frees and the runs waiting for one are keyed by POOL (Faz A.4 T6).
--
-- IT CARRIES THE PROJECT FOR THE REASON AcquireLease's OWN JOIN DOES, and the two predicates are
-- deliberately the same one: a hold can only have been opened on a machine this tenant could see under
-- `r.project_id = $2`, so asking the same question back is asking about the machine that was really held.
-- A read that leaned on row-level security alone would change meaning the day a maintenance sweep called
-- it under a system scope — which is exactly the caller settleStranded is.
--
-- NO ROW IS NOT AN ERROR AT THE CALLER: a machine that has been deleted since the hold opened frees a slot
-- on a pool nobody can name, and the honest answer is to announce nothing rather than to guess a pool.
-- ‼️ A MACHINE THE PLANE OWNS IS SERVABLE TO EVERY PROJECT, which is why the predicate is not a bare
-- equality. `runners.project_id IS NULL` is the free fleet (000002's palai_apply_fleet_policy), and
-- `NULL = $2` is NULL — so the equality alone silently refused every shared machine while the row sat
-- there, online, in the pool the run had already been placed into.
--
-- ‼️ THIS IS THE ONE WHOSE FAILURE WAS SILENT. A hold settling on a free machine resolved NO pool, and the
-- caller's documented answer to no row is to announce nothing — so the runs parked on that pool were never
-- told a slot had opened. Nothing errored and nothing was logged; the fleet simply stopped waking.
SELECT pool_id
  FROM runners
 WHERE id = $1 AND (project_id = $2 OR project_id IS NULL);

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

-- name: MachineOccupancies
-- ONE MACHINE'S SESSION HISTORY, newest first: what it is holding now and what it held before (device
-- plan T6, DoD 10). The panel's machine-detail view reads this to answer "what is this Mac doing, and
-- what did it do" — the question an operator asks before unplugging one.
--
-- IT IS TENANT-SCOPED IN THE WHERE, NOT IN THE HANDLER. A machine id belonging to another project
-- returns NO rows, so a caller cannot learn that one exists elsewhere by the shape of the answer.
--
-- THE ORDERING IS TOTAL AND THAT IS NOT DECORATION. `started_at DESC` alone ties whenever two holds
-- begin inside the same clock tick, and a LIMIT over a tie is a page that can drop or repeat a row
-- between requests — this tree has already had an unordered LIMIT decide a security outcome twice. The
-- id breaks it, and the keyset below reads the same pair.
--
-- The billed interval is the SAME `CASE` GetOccupancy derives, and it is duplicated here rather than
-- shared because the two are read in different transactions; if they ever disagree, the panel and the
-- ledger would answer a customer differently about one hold. Change one, change both.
SELECT id, runner_id, session_id, started_at, last_activity_at, released_at,
       coalesce(release_reason, ''),
       extract(epoch FROM (
           CASE WHEN release_reason = 'idle' THEN last_activity_at
                ELSE coalesce(released_at, clock_timestamp()) END
           - started_at))::double precision
  FROM runner_leases
 WHERE runner_id = $1 AND project_id = $2
   AND ($3::timestamptz IS NULL OR (started_at, id) < ($3, $4))
 ORDER BY started_at DESC, id DESC
 LIMIT $5;

-- name: StrandedOccupancies
-- The holds whose closer never ran: still open, on a machine that was demonstrably handed back. $1 bounds
-- one pass. A MAINTENANCE read spanning every tenant, like IdleWorkspacesForRelease, so it runs under the
-- system scope and every row carries its own project for the settle that follows.
--
-- WHY THIS EXISTS AT ALL — A ONE-WAY DOOR. release() hands the machine back BEFORE it settles: the
-- workspace moves to `paused` (idle_release.go:183) and the meter runs after (idle_release.go:224). That
-- order is right and stays, because a meter that could abort a release would leave an archived, deleted
-- workspace whose hold is still open. But it made the meter's failure PERMANENT: every route back to an
-- open hold ran through the workspace, and the pause had already moved it out of every one. The candidate
-- query above this one selects `state = 'ready'` (workspaces.sql), so a paused workspace is never an idle
-- candidate again; the writer-lease reclaim sweeps workspace_leases, a different table answering a
-- different question; and ReleaseLease has no production caller by design, so a hold cannot close unbilled.
-- One machine slot leaked per failed settle, for good.
--
-- `paused` IS THE WHOLE SIGNAL, AND IT IS TRUSTWORTHY BECAUSE IT HAS EXACTLY ONE WRITER:
--
--     grep -rn 'WorkspaceCmdPause' --include='*.go' . | grep -v node_modules | grep -v _test
--     -> 1   (2026-08-05: apps/control-plane/internal/execution/idle_release.go:183)
--
-- The state machine declares three arms into `paused` (preparing, ready, snapshotting) and only the
-- snapshotting one has a driver, so in this build a paused workspace means one thing and one thing only:
-- the idle releaser archived it and handed its machine back. If a second writer ever appears — a run that
-- pauses its own workspace while staying on the machine — this statement starts closing live holds, and
-- that is the reason the signal is named here rather than assumed.
--
-- `released_at IS NULL` IS NOT A FILTER, IT IS THE EXACTLY-ONCE PROPERTY. Selecting on the workspace alone
-- would re-settle every hold the release path already closed, because a released workspace stays `paused`
-- forever — the recovery would bill the customer a second time for the same occupancy of the same machine,
-- and it would do so on the ordinary path rather than on the failure one. Reclaimable and billed-twice are
-- one edit apart.
--
-- BOTH HALVES OF THE WORKSPACE TEST ARE LOAD-BEARING AND NEITHER IMPLIES THE OTHER. EXISTS(paused) alone
-- would close the hold of a session that has a paused workspace AND a live one, which is a machine it is
-- still on. NOT EXISTS(not-paused) alone is vacuously true for a session with NO workspace at all — a hold
-- opened while its workspace was still being planned — and closing that one settles an occupancy that has
-- barely begun. Together they say the only safe thing: every workspace this session has, it has handed back.
--
-- `destroyed` is deliberately NOT accepted here even though it means the machine went back too, because
-- DestroyAllocation has no production caller (workspace_recovery.go:219) — an arm for a state nothing can
-- reach would be an untested branch claiming a coverage it never has.
--
-- ORDER BY l.id is a total order over the primary key, so the LIMIT takes a determinate batch rather than
-- an arbitrary one. It is not a priority: a hold skipped this pass is a hold next pass.
SELECT l.id, l.project_id, l.session_id
  FROM runner_leases l
 WHERE l.released_at IS NULL
   AND EXISTS (
       SELECT 1 FROM workspaces w
        WHERE w.session_id = l.session_id AND w.project_id = l.project_id
          AND w.state = 'paused')
   AND NOT EXISTS (
       SELECT 1 FROM workspaces w
        WHERE w.session_id = l.session_id AND w.project_id = l.project_id
          AND w.state <> 'paused')
 ORDER BY l.id
 LIMIT $1;

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
