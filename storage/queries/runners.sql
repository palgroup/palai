-- Runner registry statements (E24 T1, migration 000045). The registry is an INVENTORY, not a health
-- source: nothing here polls or expires a row, and last_seen_at only ever records a moment a machine
-- authenticated. Heartbeat and the reaper are T5.
--
-- Every statement carries its tenant predicate explicitly even though RLS (000029) already confines
-- the rows. That is the tree's rule and it is not belt-and-braces here: ResolveRunnerPool and
-- RecordRunnerSeen run under the SYSTEM scope, because the runner plane carries no tenant on the wire
-- yet (§3.6 D8) — under that scope the policy admits everything, so the predicate in the statement is
-- the ONLY thing narrowing the read.

-- name: ResolveRunnerPool
-- The pool a machine is enrolling into, and the tenant that pool belongs to. Run system-scoped: the
-- enrolment request carries no org/project, so the POOL is what resolves the tenant. LIMIT is absent
-- deliberately — id is the primary key, so this returns at most one row by construction and there is
-- no ordering ambiguity to resolve (the LIMIT-1-without-ORDER-BY trap this tree has paid for twice).
--
-- isolation_mode is 000007's column and EMPTY IS THE SHIPPED VALUE: a pool that requires no particular
-- session-isolation mechanism admits every machine, which is every pool that exists on the day 000007
-- applies. Only a pool an operator sets to 'user' / 'accounts' / 'container' refuses a machine whose
-- MEASURED modes do not include it.
SELECT id, project_id, posture, os, arch, strict_enrollment, isolation_mode
  FROM runner_pools
 WHERE id = $1;

-- name: ResolveRunnerByFingerprint
-- The row this DEVICE KEY already owns in this pool, or none (plan §3.4, migration 000007).
--
-- THIS STATEMENT IS WHAT MAKES A RESTART A RESTART. Before it, every process start generated a fresh
-- keypair and took a fresh `rnr_` id, so a rebooted Mac was a machine the registry had never seen — a new
-- row, and in a strict pool a second approval for a box a human had already approved.
--
-- ORDER BY IS NOT DECORATION HERE. 000007 adds a UNIQUE index on (pool_id, public_key_sha256) for a
-- non-empty fingerprint, so this returns at most one row on any database that has applied it — but the
-- statement has to be correct on a database mid-upgrade too, and a LIMIT without an ORDER BY is a
-- non-deterministic answer this tree has already had decide three outcomes, two of them security ones.
-- Newest wins, and the tiebreak on id makes even that total.
SELECT id, state, runner_dns
  FROM runners
 WHERE pool_id = $1 AND public_key_sha256 = $2 AND public_key_sha256 <> ''
 ORDER BY enrolled_at DESC, id DESC
 LIMIT 1;

-- name: RecoverRunner
-- Re-enrol a machine the registry already holds, keeping its id.
--
-- WHAT IS REFRESHED AND WHAT IS NOT is the whole content of this statement. The label, the reported
-- shape, the version, the measured isolation modes and the key that admitted it are all refreshed —
-- they are facts about the machine as it is NOW, and a Mac upgraded to a new OS or moved to a new pool
-- key would otherwise carry last year's inventory forever.
--
-- `state` IS NOT TOUCHED, AND THAT IS THE SECURITY HALF. A `pending` machine stays in the strict pool's
-- waiting room across a restart rather than being promoted by the restart itself, and a `cordoned` one
-- stays cordoned. A `revoked` one never reaches this statement at all — fleet.Store.Register refuses it
-- before here (ErrRunnerRevoked), because re-presenting a live pool key must not undo a decommissioning.
--
-- `capacity` IS NOT TOUCHED EITHER, for a narrower reason: it is the ADMIN plane's number since plan §3.6,
-- and letting a re-enrolment write the machine's own declaration back over it would let a box widen a
-- ceiling an operator set by restarting.
UPDATE runners
   SET label = $2,
       os = coalesce(nullif($3, ''), os),
       arch = coalesce(nullif($4, ''), arch),
       agent_version = $5,
       isolation_modes = $6,
       enrolled_via_key_id = coalesce(nullif($7, ''), enrolled_via_key_id),
       enrolled_at = clock_timestamp(),
       last_seen_at = clock_timestamp()
 WHERE id = $1
RETURNING id, project_id, pool_id, label, runner_dns, public_key_sha256,
          state, os, arch, posture, capacity, cert_not_after, enrolled_at, last_seen_at,
       config_revision, config_applied, config_reported_at, agent_version, isolation_modes;

-- name: InsertRunner
-- Record an enrolled machine. The id is minted by the CALLER (server-side, `rnr_`), never by the
-- enrolling party: that is the whole property this table exists to hold. state is 'active' for a
-- non-strict pool, which admits on presentation of a valid credential, and 'pending' when the pool
-- carries `strict_enrollment` (E24 T6) — the caller passes it in $7 and the pool row is what decides.
--
-- $13 is enrolled_via_key_id (E24 T3): WHICH credential admitted this machine. NULL means the file
-- bootstrap token, which is not a row and cannot be revoked individually — the honest representation
-- of the pre-pool-key path rather than a sentinel id pointing at nothing.
--
-- $14/$15 are 000007's agent_version and isolation_modes: the build the machine reported and the
-- session-isolation mechanisms it MEASURED it can provide, comma-joined. Both default to '' for a runner
-- too old to send them, which is every runner on the day 000007 applies.
INSERT INTO runners (
    id, project_id, pool_id, label, runner_dns, public_key_sha256,
    state, os, arch, posture, capacity, cert_not_after, enrolled_via_key_id,
    agent_version, isolation_modes, enrolled_at, last_seen_at
) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, nullif($13, ''), $14, $15, clock_timestamp(), clock_timestamp())
RETURNING created_at, enrolled_at, last_seen_at;

-- name: AppendRunnerEnrollment
-- One entry of the append-only issuance journal. entry_seq is computed from the journal itself so two
-- concurrent writers collide on UNIQUE (runner_id, entry_seq) rather than both winning — the
-- capability_jobs fence, verbatim. detail is structured context and NEVER a credential: §2 says the
-- journal writes key_id and never a key value, and the only way to keep that true is for no caller to
-- have a column to put one in.
INSERT INTO runner_enrollments (
    id, project_id, runner_id, pool_id, key_id, entry_kind, entry_seq, detail
)
SELECT $1, $2, $3, $4, $5, $6,
       coalesce((SELECT max(entry_seq) FROM runner_enrollments WHERE runner_id = $3), 0) + 1,
       $7::jsonb;

-- name: RecordRunnerSeen
-- Advance the liveness stamp for the runner holding this certificate DNS. Keyed on runner_dns and not
-- on id because a renew presents a CERTIFICATE — there is no id on that wire — and the DNS is the
-- identity the certificate carries. cert_not_after is only overwritten when the caller has a fresh one
-- (connect presents a certificate it already holds; renew has just issued a newer one).
UPDATE runners
   SET last_seen_at = $2,
       cert_not_after = coalesce($3, cert_not_after)
 WHERE runner_dns = $1
RETURNING id, project_id, pool_id, label, runner_dns, public_key_sha256,
          state, os, arch, posture, capacity, cert_not_after, enrolled_at, last_seen_at,
       config_revision, config_applied, config_reported_at, agent_version, isolation_modes;

-- name: SetRunnerState
-- Cordon, resume or revoke ONE machine, durably (E24 T5). Before this the three were in-memory
-- whole-gateway booleans, so "decommission that Mac" took every Mac out of service AND a restart erased
-- it (§3.6 D15).
--
-- REVOKE IS IRREVERSIBLE AND THAT IS THIS PREDICATE, not a rule in Go. `state <> 'revoked'` refuses to
-- move a decommissioned machine, and the `OR $3 = 'revoked'` clause is what keeps a REVOKE idempotent:
-- revoking twice returns the row rather than a not-found an operator would read as "the id was wrong".
-- The pairing matters — an idempotency that also let a resume through would make "irreversible" a comment.
--
-- Tenant-scoped, because this is reached through the public API where the verified bearer scope is the
-- only tenant authority. Another tenant's runner id simply returns no row, so the handler answers 404
-- without ever learning whether the id exists elsewhere.
--
-- THERE IS NO 'unhealthy' HERE and there cannot be: migration 000045's CHECK admits
-- ('pending','active','cordoned','revoked') and E24 owns exactly one migration, which is T1's. Health is
-- derived from last_seen_at instead — see RunnerGateway.Heartbeat for why that is the better answer
-- rather than the available one.
--
-- A `pending` MACHINE IS NOT REACHABLE FROM THESE THREE VERBS, AND THAT CLAUSE IS T6'S WAITING ROOM
-- (E24 T6). Without it `resume` — an operator verb with no approver check on it — would move a machine
-- out of `pending` into `active`, which is the whole of strict enrolment, bypassed by a different route.
-- A `cordon` would be worse than a bypass: it would ERASE the fact that the machine was never admitted,
-- and the resume after it would then be legitimate. A REVOKE is deliberately still allowed, because
-- refusing an enrolment is exactly what an operator does with a machine they did not order.
UPDATE runners
   SET state = $3
 WHERE id = $1 AND ($2 = '' OR project_id = $2)
   AND (state <> 'revoked' OR $3 = 'revoked')
   AND (state <> 'pending' OR $3 = 'revoked')
RETURNING id, project_id, pool_id, label, runner_dns, public_key_sha256,
          state, os, arch, posture, capacity, cert_not_after, enrolled_at, last_seen_at,
       config_revision, config_applied, config_reported_at, agent_version, isolation_modes, created_at;

-- name: ApproveRunner
-- Admit ONE machine that is waiting in a strict pool's waiting room (E24 T6). It is a SEPARATE statement
-- from SetRunnerState rather than a fourth verb on it, and the separation is the control: the three
-- lifecycle verbs are gated on `provision` and this one is gated on `approve` and on the project's
-- approver list, so collapsing them would make the waiting room openable by the capability that can
-- rewrite the list it is checked against.
--
-- `state IN ('pending','active')` IS THE IDEMPOTENCY, and it is doing two jobs. The operator-facing one:
-- an approve that answered 404 on its second call would read as "the id was wrong" to whoever retried a
-- request that timed out. The structural one: the API writes the ROW and then tells the live gateway, so
-- an approve that raced a machine's own connect can leave the row admitted and that session still
-- waiting — and the retry is what reaches the session, which a 404 would refuse to do.
--
-- What it does NOT admit is a cordoned or revoked machine: `active` is the only other state here, so an
-- approval can never resurrect a decommissioned identity or silently un-cordon a Mac an operator took
-- out of service.
UPDATE runners
   SET state = 'active'
 WHERE id = $1 AND ($2 = '' OR project_id = $2)
   AND state IN ('pending', 'active')
RETURNING id, project_id, pool_id, label, runner_dns, public_key_sha256,
          state, os, arch, posture, capacity, cert_not_after, enrolled_at, last_seen_at,
       config_revision, config_applied, config_reported_at, agent_version, isolation_modes, created_at;

-- name: AppendRunnerDecision
-- The journal entry for a decision about ONE MACHINE — a decommission (E24 T5, `revoked`) or an
-- admission out of the waiting room (E24 T6, `approved`) — appended AT MOST ONCE per machine per kind.
--
-- The `NOT EXISTS` is what makes it once, and it is here rather than in Go because the alternative is a
-- read-then-write: both writers are idempotent by design (an operator must be able to repeat a revoke or
-- an approve and see the same answer), so a second call arrives with the row already in its target state
-- and must record nothing. A Go-side "was it already decided?" would need its own locking read, and two
-- concurrent ones would both see the pre-state.
--
-- Concurrency beyond that is handled by the journal's own spine: `UNIQUE (runner_id, entry_seq)` means two
-- writers computing the same next seq collide (23505) and exactly one lands — the capability_jobs fence,
-- and the same one AppendRunnerEnrollment relies on.
--
-- ONE STATEMENT FOR TWO KINDS rather than a near-copy per verb: the at-most-once reasoning above is the
-- part a reader has to get right, and two copies of it are two places for it to drift. $5 is the kind and
-- it is a literal at each Go call site, never caller input — `entry_kind` is CHECK-constrained by 000045
-- R4 to ('requested','approved','refused','issued','revoked','renewed') either way.
--
-- key_id is deliberately empty: a decision like this is about the MACHINE, and the key that admitted it is
-- already recorded on the `issued` entry. Writing it again here would suggest the key was the subject.
INSERT INTO runner_enrollments (
    id, project_id, runner_id, pool_id, key_id, entry_kind, entry_seq, detail
)
SELECT $1, $2, $3, $4, '', $5,
       coalesce((SELECT max(entry_seq) FROM runner_enrollments WHERE runner_id = $3), 0) + 1,
       $6::jsonb
 WHERE NOT EXISTS (SELECT 1
                     FROM runner_enrollments
                    WHERE runner_id = $3 AND entry_kind = $5);

-- name: GetRunner
-- One runner inside the caller's tenant. A row belonging to another tenant returns NO row, so the
-- handler answers 404 without ever learning whether the id exists elsewhere.
SELECT id, project_id, pool_id, label, runner_dns, public_key_sha256,
       state, os, arch, posture, capacity, cert_not_after, enrolled_at, last_seen_at,
       config_revision, config_applied, config_reported_at, agent_version, isolation_modes, created_at
  FROM runners
 WHERE id = $1 AND ($2 = '' OR project_id = $2);

-- name: ListRunners
-- The tenant-scoped keyset page: (created_at, id) DESC, the ordering api/pagination.go mints its
-- cursor from. $5 is the over-fetch (page size + 1) so the handler detects a further page without a
-- second round trip.
SELECT id, project_id, pool_id, label, runner_dns, public_key_sha256,
       state, os, arch, posture, capacity, cert_not_after, enrolled_at, last_seen_at,
       config_revision, config_applied, config_reported_at, agent_version, isolation_modes, created_at
  FROM runners
 WHERE ($1 = '' OR project_id = $1)
   AND ($2::timestamptz IS NULL OR created_at >= $2)
   AND ($3::timestamptz IS NULL OR created_at <= $3)
   AND ($4::timestamptz IS NULL OR (created_at, id) < ($4, $5))
 ORDER BY created_at DESC, id DESC
 LIMIT $6;

-- name: InsertDefaultRunnerPool
-- The tenant's default pool, seeded in the SAME transaction as the four identity rows a tenant is
-- born with (identity.Store.provision). Idempotent by both keys the row can already exist under: the
-- id (a re-boot against a retained volume) and the (org, project, name) uniqueness 000045 R1 adds
-- (migration 000045 R6 having already seeded it on an upgrading install).
INSERT INTO runner_pools (id, project_id, name, posture, strict_enrollment)
VALUES ($1, $2, 'default', 'sandboxed-linux', false)
    ON CONFLICT DO NOTHING;

-- name: InsertRunnerPool
-- A pool an OPERATOR created (E28 T1), and the statement above is why this one exists rather than being a
-- parameterised version of it: `InsertDefaultRunnerPool` writes the row every tenant is BORN with, and
-- every installation alive today has it, so its three literals stay literal and a second pool is a second
-- statement. Two statements, two subjects.
--
-- NO `ON CONFLICT`: the UNIQUE (project_id, name) index on runner_pools is what makes a duplicate
-- name a 409 an operator can act on, and swallowing the conflict here would answer 201 for a pool that was
-- not created.
--
-- project_id is NOT NULLABLE HERE even though the column allows it: fleet.Store.Register refuses to enrol a
-- machine into a project-less pool (000045's tenant policy for `runners` narrows on a project), so a pool
-- created without one would be a row nothing could ever join. The route refuses it before this runs.
INSERT INTO runner_pools (id, project_id, name, posture, os, arch, strict_enrollment)
VALUES ($1, $2, $3, $4, $5, $6, $7)
RETURNING created_at;

-- name: SetRunnerPoolStrictEnrollment
-- The waiting room's switch (E28 T1). ONE column, and the reason the statement takes no other is a
-- correctness requirement rather than a scope decision: a machine INHERITS its pool's posture at enrolment
-- (internal/fleet/store.go, "the machine inherits the pool's posture"), so an UPDATE that could move a
-- populated pool's posture would retroactively change what the machines already in it ARE.
--
-- The predicate carries the tenant as well as the id, so a pool that is not the caller's returns no row and
-- the handler answers 404 without learning whether it exists elsewhere.
UPDATE runner_pools
   SET strict_enrollment = $3
 WHERE ($1 = '' OR project_id = $1)
   AND id = $2
RETURNING id, project_id, name, posture, os, arch, strict_enrollment, created_at;

-- name: ListRunnerPools
-- The tenant-scoped keyset page of pools: (created_at, id) DESC, the ordering api/pagination.go mints
-- its cursor from. $6 is the over-fetch (page size + 1) so the handler detects a further page without a
-- second round trip.
--
-- E28 T1 CORRECTED THE COMMENT THAT USED TO STAND HERE. It read "creating and deleting a pool is T5/T6's",
-- and the same sentence stood in api/router.go; T5 and T6 both shipped and neither opened either. Creating
-- a pool is now `POST /v1/runner-pools` (InsertRunnerPool above) and its waiting room is switched by
-- `PATCH /v1/runner-pools/{pool_id}`. DELETING one is still absent, deliberately: `runner_pool_keys` has
-- ON DELETE CASCADE on this table (000045 R2), so removing a pool would silently remove its enrolment
-- keys, and what happens to the machines whose `pool_id` points here is a separate decision nobody has
-- asked for. This comment names no future owner, because a comment cannot schedule work.
SELECT id, project_id, name, posture, os, arch, strict_enrollment, created_at
  FROM runner_pools
 WHERE ($1 = '' OR project_id = $1)
   AND ($2::timestamptz IS NULL OR created_at >= $2)
   AND ($3::timestamptz IS NULL OR created_at <= $3)
   AND ($4::timestamptz IS NULL OR (created_at, id) < ($4, $5))
 ORDER BY created_at DESC, id DESC
 LIMIT $6;

-- ---------------------------------------------------------------------------------------------------
-- POOL ENROLMENT KEYS (E24 T3). The credential a machine presents to enrol into ONE pool.
--
-- WHAT IS NEW HERE IS NOT REUSABILITY. The tree's only enrolment credential was already reusable —
-- FileEnrollmentTokens' own heading says so at length — and what it lacked was scope, an expiry, a
-- revocation and a RECORD. These statements are those four.
--
-- The VALUE never appears in any statement below: the mint takes a digest, the redemption takes a
-- digest, and the listing returns a PREFIX. There is no parameter here a caller could pass a
-- credential to except as the sha256 it is compared by.
-- ---------------------------------------------------------------------------------------------------

-- name: InsertRunnerPoolKey
-- Mint a key for one pool inside the caller's verified tenant. The pool is re-asserted against the
-- tenant in the SELECT rather than trusted from the caller: a key minted against another tenant's pool
-- would be a credential that admits a machine into a fleet its owner never opened. No row means the
-- pool is not this tenant's (or does not exist), which the caller renders as a 404.
INSERT INTO runner_pool_keys (id, project_id, pool_id, key_sha256, key_prefix, expires_at)
SELECT $1, p.project_id, p.id, $4, $5, $6
  FROM runner_pools p
 WHERE p.id = $3 AND ($2 = '' OR p.project_id = $2)
RETURNING id, pool_id, key_prefix, created_at, expires_at;

-- name: ResolveRunnerPoolKey
-- Resolve a PRESENTED key by its digest. Run SYSTEM-SCOPED and keyed by the digest, for the reason
-- VerifyAPIKey is: this read is what establishes the tenant, so there is no tenant to publish yet, and
-- a caller can only reach the row whose secret they already hold.
--
-- NO ORDER BY AND NO LIMIT, deliberately: key_sha256 is UNIQUE across the whole installation (000045
-- R2 chose global-UNIQUE over per-tenant precisely so this lookup cannot be ambiguous), so this
-- returns at most one row by construction. The tree has decided a security outcome on an unordered
-- LIMIT 1 twice; there is no ordering here to get wrong.
--
-- The row is returned WITH its revoked/expired state rather than filtered by it, because a refused
-- presentation has to be journalled with the reason it was refused — a statement that returned no row
-- for a revoked key would leave the caller unable to tell "revoked" from "never existed".
-- The stored digest comes back with the row even though it WAS the lookup key: the caller compares it
-- in constant time (api/tool_callbacks.go:98 takes the same position after its own keyed read), so a
-- credential comparison in this tree never depends on a per-site argument about what leaks.
SELECT k.id, k.project_id, k.pool_id, k.key_sha256, k.revoked_at, k.expires_at,
       p.posture, p.strict_enrollment
  FROM runner_pool_keys k
  JOIN runner_pools p ON p.id = k.pool_id
 WHERE k.key_sha256 = $1;

-- name: TouchRunnerPoolKey
-- Record that a key was successfully redeemed. It is an OBSERVATION, not a rate limit: a pool key is a
-- FLEET credential by design (many machines, one key), so bounding redemptions per interval — the rule
-- FileEnrollmentTokens applies to its single-runner token — would make a ten-Mac pool unenrollable.
-- What it buys is that the fact survives a restart, which the in-memory map behind minInterval does not
-- (§3.6 D6).
UPDATE runner_pool_keys SET last_used_at = $2 WHERE id = $1;

-- name: ListRunnerPoolKeys
-- The operator's view: metadata only, never the digest and never the value. The PREFIX is here because
-- telling two keys apart in a list is the whole job of a listing, and a prefix of a credential is not
-- one — the same bargain `palai apikey list` already makes.
SELECT k.id, k.pool_id, k.key_prefix, k.created_at, k.expires_at, k.revoked_at, k.last_used_at
  FROM runner_pool_keys k
 WHERE ($1 = '' OR k.project_id = $1)
   AND ($2 = '' OR k.pool_id = $2)
 ORDER BY k.created_at DESC, k.id DESC;

-- name: RevokeRunnerPoolKey
-- Close the door. Idempotent (the first revoked_at is kept, the api_keys precedent) and tenant-scoped,
-- so another tenant's key id is simply not found. It touches NOTHING about the machines this key
-- already admitted: that is the property the whole task rests on, and it is a property of what this
-- statement does not say.
UPDATE runner_pool_keys
   SET revoked_at = coalesce(revoked_at, $3)
 WHERE id = $1 AND ($2 = '' OR project_id = $2)
RETURNING id, pool_id, key_prefix, created_at, expires_at, revoked_at, last_used_at;

-- name: ListRunnersEnrolledViaKey
-- The machines a revocation did NOT stop. An operator who revokes a key has to be shown them, or
-- "revoked" reads as "removed" and they believe one call decommissioned a fleet.
SELECT id, label, runner_dns, state, pool_id, enrolled_at, last_seen_at
  FROM runners
 WHERE enrolled_via_key_id = $1 AND ($2 = '' OR project_id = $2)
 ORDER BY enrolled_at DESC, id DESC;

-- ---------------------------------------------------------------------------------------------------
-- PLACEMENT AND THE CAPACITY PARK (E24 T4). These statements are about `runs` and `attempts` rather
-- than about the registry, and they live here because WHERE a run goes is a fleet question — the
-- alternative was scattering four statements through responses.sql where nothing names them as a set.
--
-- No migration: `runs.pool_id` is T1's 000045 R5 and `attempts.state` is 000001's, and 000045 is the
-- epic's only migration.
-- ---------------------------------------------------------------------------------------------------

-- name: RunPlacementInputs
-- The facts one placement decision reads, in ONE round trip: the pool the run was already placed in
-- (NULL until a decision is recorded — every run before E24), the moment the RUN entered the queue
-- (what orders a pool's waiting attempts, and deliberately not the attempt's arrival), and the id of
-- THIS TENANT's own default pool.
--
-- The third column exists because fleet.DefaultPoolID is a CONSTANT: it is the bootstrap tenant's pool
-- id, so before this every other tenant that had configured nothing resolved to a pool belonging to
-- somebody else. The subselect is the tenant's own 'default' pool — the row identity.Store.provision
-- seeds with every organization — and '' when it has none, which leaves the constant as the last
-- resort exactly as it was.
SELECT r.pool_id, r.created_at,
       coalesce((SELECT p.id
                   FROM runner_pools p
                  WHERE p.project_id = $2 AND p.name = 'default'), '')
  FROM runs r
 WHERE r.id = $1 AND r.project_id = $2;

-- name: RecordRunPool
-- Write the placement decision ONCE (000045 R5). `pool_id IS NULL` is what makes it write-once, so a
-- resume returns to the SAME pool and cannot be re-placed into another posture.
--
-- THE EXISTS IS NOT BELT-AND-BRACES. runs.pool_id has a foreign key, so writing a pool that does not
-- exist would abort the statement and FAIL the run — meaning one typo in a project's config_policy
-- would kill every run in that project. Excluding the row instead records NO decision, which is the
-- honest answer for a pool nobody created, and the run then parks (a pool with no machine) rather than
-- dying. The tenant predicate is the other half: recording another tenant's pool would claim a
-- placement into a fleet this tenant does not own.
-- RETURNING is what lets the caller tell "recorded" from "recorded nothing". Without it the two were
-- one answer at the site that decides whether to dial, and a run whose tenant owns no pool parked with
-- pool_id NULL — unreachable by OldestRunAwaitingCapacity, which matches on that exact column.
UPDATE runs
   SET pool_id = $3, updated_at = clock_timestamp()
 WHERE id = $1 AND project_id = $2
   AND pool_id IS NULL
   AND EXISTS (SELECT 1
                 FROM runner_pools p
                WHERE p.id = $3
                  AND (p.project_id IS NULL OR p.project_id = $2))
RETURNING pool_id;

-- name: ExpiredCapacityParks
-- The runs that have been parked for want of a machine longer than the operator's park TTL (E24 T5).
--
-- IT CLOSES T4'S SECOND CEILING (`FLT-P7`): a run parked in a pool that will never have a machine — a
-- deleted pool, a `config_policy.pool` naming one that does not exist, a Mac that was never ordered —
-- waits FOREVER, because the only wake fires when a machine connects. There is no default TTL and there
-- must not be: AWS documents a Mac host taking "approximately 6 minutes to 20 minutes" to start, so any
-- number this file chose would be a guess about somebody else's fleet.
--
-- The predicate is the park's OWN marker, positively: `attempts.state = 'awaiting_capacity'` (T4's, and
-- the reason it is on the attempt is that `waiting` is four different conditions with four different
-- wakers). The age is the ATTEMPT's `updated_at`, which is the moment the park was written — the run's
-- created_at would time out a run that has been running for hours and parked for one second.
--
-- $1 is the TTL in seconds. Bounded by $2 so one pass cannot hold a connection open over a large backlog;
-- the next tick takes the rest.
--
-- ponytail: THE SAME MISSING INDEX OldestRunAwaitingCapacity NAMES, and for the same reason — E24 owns one
-- migration and it is T1's. This runs once per reconcile tick (30s) rather than once per runner connect,
-- so it is the cheaper of the two, and the upgrade is the same one partial index.
SELECT r.project_id, r.id, r.response_id
  FROM runs r
  JOIN attempts a ON a.run_id = r.id
                 AND a.project_id = r.project_id
 WHERE r.state = 'waiting'
   AND a.state = 'awaiting_capacity'
   AND a.updated_at < clock_timestamp() - make_interval(secs => $1)
   AND r.response_id IS NOT NULL
 ORDER BY a.updated_at, r.id
 LIMIT $2;

-- name: MarkAttemptAwaitingCapacity
-- The POSITIVE marker for the one waiting reason a machine's arrival may wake. `waiting` is four
-- different conditions (a human's pause, an approval, a detached child, no capacity) and each has its
-- own waker; a capacity wake fires on every connect, which for a runner is after every lease, so a
-- predicate of `state = 'waiting'` alone would resume paused runs against their user's decision.
--
-- It needs no migration and no new column: attempts.state has only ever held 'assigned' and
-- 'preempted', nothing reads it for a decision, and the attempt IS the thing that found no machine.
-- It is cleared by the supersede the next attempt already performs (SupersedeActiveAttempts), so there
-- is no second write to forget.
UPDATE attempts
   SET state = 'awaiting_capacity', updated_at = clock_timestamp()
 WHERE id = $1 AND project_id = $2;

-- name: OldestRunAwaitingCapacity
-- The pool's oldest run parked for want of a machine, locked for the wake.
--
-- ORDER BY (created_at, id) is a TOTAL order and it is here rather than left to LIMIT: an unordered
-- LIMIT 1 has decided a security outcome in this tree twice. SKIP LOCKED is what makes two machines
-- arriving at the same moment wake two DIFFERENT runs instead of contending for one.
--
-- ponytail: THERE IS NO INDEX FOR THIS PREDICATE AND THIS TASK CANNOT ADD ONE. `runs` carries exactly
-- two indexes (both on session_id, from 000006/000007) and E24's only migration is T1's 000045, so the
-- planner filters. It runs once per runner CONNECT — which for a runner is once per lease — so on a
-- self-host install whose `runs` table is thousands of rows it is a cheap filtered scan, and on a table
-- of millions it is not. The upgrade is one line in the next epic's migration:
-- `CREATE INDEX ... ON runs (project_id, pool_id) WHERE state = 'waiting'` — a partial
-- index, because the rows this asks about are a vanishing fraction of the table.
SELECT r.id
  FROM runs r
 WHERE r.project_id = $1 AND r.pool_id = $2 AND r.state = 'waiting'
   AND EXISTS (SELECT 1
                 FROM attempts a
                WHERE a.run_id = r.id AND a.project_id = $1
                  AND a.state = 'awaiting_capacity')
 ORDER BY r.created_at, r.id
 LIMIT 1
 FOR UPDATE SKIP LOCKED;

-- name: RunnerExistsForConfig
-- Does ANY machine carry this id? The existence check behind a `runner_machine` desired document, so a
-- write naming a machine that has never enrolled is refused with the id in the sentence rather than
-- landing in an append-only journal nothing will ever resolve.
--
-- IT IS SYSTEM-SCOPED AND CROSSES TENANTS ON PURPOSE, which is the one thing about it worth arguing.
-- deployment_desired carries no tenant column — that absence IS its security property (000052) — so a
-- per-tenant existence check would be asking a question the document it guards cannot express. The
-- authority is the same one the rest of this surface runs under: the `provision` capability, checked in the
-- handler before the store is reached. The leak is bounded to one bit about an id the caller already typed,
-- and the caller is a deployment operator rather than a tenant.
--
-- A FOREIGN KEY WOULD HAVE BEEN THE OTHER PLACE TO PUT THIS and 000060's header records why it is not:
-- an operator configures a machine they are ABOUT to enrol as readily as one already enrolled, and a
-- constraint can only refuse — it cannot offer.
SELECT EXISTS (SELECT 1 FROM runners WHERE id = $1);

-- name: RecordRunnerConfigReport
-- The machine's own answer about the configuration it holds: which revision it resolved, and its verdict
-- per setting (`applied` / `pending_restart`).
--
-- KEYED ON runner_dns, not on id, for the reason RecordRunnerSeen is: the machine authenticates this write
-- with a CERTIFICATE and the wire carries no id — the DNS is the identity the certificate proves. A body
-- field naming the runner would be a machine telling the control plane which row to write, which is the
-- one thing this key makes unnecessary.
--
-- IT RETURNS THE ROW IT WROTE so a caller can tell "recorded" from "matched nothing". A silent zero-row
-- UPDATE is the defect RecordRunPool was fixed for: the write reports success, the panel reports the
-- machine has taken the value, and the row that would have said otherwise was never touched. The one
-- legitimate no-match — a runner enrolled before the registry existed, which has no row at all — is
-- distinguished by the CALLER rather than swallowed here.
UPDATE runners
   SET config_revision = $2,
       config_applied = $3::jsonb,
       config_reported_at = $4
 WHERE runner_dns = $1
RETURNING id, pool_id;
