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
SELECT id, organization_id, project_id, posture, os, arch, strict_enrollment
  FROM runner_pools
 WHERE id = $1;

-- name: InsertRunner
-- Record an enrolled machine. The id is minted by the CALLER (server-side, `rnr_`), never by the
-- enrolling party: that is the whole property this table exists to hold. state starts 'active'
-- because a non-strict pool admits on presentation of a valid credential; T6's strict mode is what
-- makes 'pending' reachable.
--
-- $14 is enrolled_via_key_id (E24 T3): WHICH credential admitted this machine. NULL means the file
-- bootstrap token, which is not a row and cannot be revoked individually — the honest representation
-- of the pre-pool-key path rather than a sentinel id pointing at nothing.
INSERT INTO runners (
    id, organization_id, project_id, pool_id, label, runner_dns, public_key_sha256,
    state, os, arch, posture, capacity, cert_not_after, enrolled_via_key_id, enrolled_at, last_seen_at
) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, nullif($14, ''), clock_timestamp(), clock_timestamp())
RETURNING created_at, enrolled_at, last_seen_at;

-- name: AppendRunnerEnrollment
-- One entry of the append-only issuance journal. entry_seq is computed from the journal itself so two
-- concurrent writers collide on UNIQUE (runner_id, entry_seq) rather than both winning — the
-- capability_jobs fence, verbatim. detail is structured context and NEVER a credential: §2 says the
-- journal writes key_id and never a key value, and the only way to keep that true is for no caller to
-- have a column to put one in.
INSERT INTO runner_enrollments (
    id, organization_id, project_id, runner_id, pool_id, key_id, entry_kind, entry_seq, detail
)
SELECT $1, $2, $3, $4, $5, $6, $7,
       coalesce((SELECT max(entry_seq) FROM runner_enrollments WHERE runner_id = $4), 0) + 1,
       $8::jsonb;

-- name: RecordRunnerSeen
-- Advance the liveness stamp for the runner holding this certificate DNS. Keyed on runner_dns and not
-- on id because a renew presents a CERTIFICATE — there is no id on that wire — and the DNS is the
-- identity the certificate carries. cert_not_after is only overwritten when the caller has a fresh one
-- (connect presents a certificate it already holds; renew has just issued a newer one).
UPDATE runners
   SET last_seen_at = $2,
       cert_not_after = coalesce($3, cert_not_after)
 WHERE runner_dns = $1
RETURNING id, organization_id, project_id, pool_id, label, runner_dns, public_key_sha256,
          state, os, arch, posture, capacity, cert_not_after, enrolled_at, last_seen_at;

-- name: SetRunnerState
-- Cordon, resume or revoke ONE machine, durably (E24 T5). Before this the three were in-memory
-- whole-gateway booleans, so "decommission that Mac" took every Mac out of service AND a restart erased
-- it (§3.6 D15).
--
-- REVOKE IS IRREVERSIBLE AND THAT IS THIS PREDICATE, not a rule in Go. `state <> 'revoked'` refuses to
-- move a decommissioned machine, and the `OR $4 = 'revoked'` clause is what keeps a REVOKE idempotent:
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
UPDATE runners
   SET state = $4
 WHERE id = $1 AND organization_id = $2 AND ($3 = '' OR project_id = $3)
   AND (state <> 'revoked' OR $4 = 'revoked')
RETURNING id, organization_id, project_id, pool_id, label, runner_dns, public_key_sha256,
          state, os, arch, posture, capacity, cert_not_after, enrolled_at, last_seen_at, created_at;

-- name: AppendRunnerRevocation
-- The journal entry for a decommission (E24 T5), appended AT MOST ONCE per machine.
--
-- The `NOT EXISTS` is what makes it once, and it is here rather than in Go because the alternative is a
-- read-then-write: SetRunnerState is idempotent by design (an operator must be able to repeat a revoke
-- and see the same answer), so a second call arrives with the row already revoked and must record
-- nothing. A Go-side "was it already revoked?" would need its own locking read, and two concurrent ones
-- would both see `active`.
--
-- Concurrency beyond that is handled by the journal's own spine: `UNIQUE (runner_id, entry_seq)` means two
-- writers computing the same next seq collide (23505) and exactly one lands — the capability_jobs fence,
-- and the same one AppendRunnerEnrollment relies on.
--
-- key_id is deliberately empty: a revocation is a decision about the MACHINE, and the key that admitted
-- it is already recorded on the `issued` entry. Writing it again here would suggest the key was revoked.
INSERT INTO runner_enrollments (
    id, organization_id, project_id, runner_id, pool_id, key_id, entry_kind, entry_seq, detail
)
SELECT $1, $2, $3, $4, $5, '', 'revoked',
       coalesce((SELECT max(entry_seq) FROM runner_enrollments WHERE runner_id = $4), 0) + 1,
       $6::jsonb
 WHERE NOT EXISTS (SELECT 1
                     FROM runner_enrollments
                    WHERE runner_id = $4 AND entry_kind = 'revoked');

-- name: GetRunner
-- One runner inside the caller's tenant. A row belonging to another tenant returns NO row, so the
-- handler answers 404 without ever learning whether the id exists elsewhere.
SELECT id, organization_id, project_id, pool_id, label, runner_dns, public_key_sha256,
       state, os, arch, posture, capacity, cert_not_after, enrolled_at, last_seen_at, created_at
  FROM runners
 WHERE id = $1 AND organization_id = $2 AND ($3 = '' OR project_id = $3);

-- name: ListRunners
-- The tenant-scoped keyset page: (created_at, id) DESC, the ordering api/pagination.go mints its
-- cursor from. $6 is the over-fetch (page size + 1) so the handler detects a further page without a
-- second round trip.
SELECT id, organization_id, project_id, pool_id, label, runner_dns, public_key_sha256,
       state, os, arch, posture, capacity, cert_not_after, enrolled_at, last_seen_at, created_at
  FROM runners
 WHERE organization_id = $1
   AND ($2 = '' OR project_id = $2)
   AND ($3::timestamptz IS NULL OR created_at >= $3)
   AND ($4::timestamptz IS NULL OR created_at <= $4)
   AND ($5::timestamptz IS NULL OR (created_at, id) < ($5, $6))
 ORDER BY created_at DESC, id DESC
 LIMIT $7;

-- name: InsertDefaultRunnerPool
-- The tenant's default pool, seeded in the SAME transaction as the four identity rows a tenant is
-- born with (identity.Store.provision). Idempotent by both keys the row can already exist under: the
-- id (a re-boot against a retained volume) and the (org, project, name) uniqueness 000045 R1 adds
-- (migration 000045 R6 having already seeded it on an upgrading install).
INSERT INTO runner_pools (id, organization_id, project_id, name, posture, strict_enrollment)
VALUES ($1, $2, $3, 'default', 'sandboxed-linux', false)
    ON CONFLICT DO NOTHING;

-- name: ListRunnerPools
-- The tenant-scoped keyset page of pools: (created_at, id) DESC, the ordering api/pagination.go mints
-- its cursor from. $7 is the over-fetch (page size + 1) so the handler detects a further page without a
-- second round trip. Read-only surface (E24 T2) — creating and deleting a pool is T5/T6's.
SELECT id, organization_id, project_id, name, posture, os, arch, strict_enrollment, created_at
  FROM runner_pools
 WHERE organization_id = $1
   AND ($2 = '' OR project_id = $2)
   AND ($3::timestamptz IS NULL OR created_at >= $3)
   AND ($4::timestamptz IS NULL OR created_at <= $4)
   AND ($5::timestamptz IS NULL OR (created_at, id) < ($5, $6))
 ORDER BY created_at DESC, id DESC
 LIMIT $7;

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
INSERT INTO runner_pool_keys (id, organization_id, project_id, pool_id, key_sha256, key_prefix, expires_at)
SELECT $1, p.organization_id, p.project_id, p.id, $5, $6, $7
  FROM runner_pools p
 WHERE p.id = $4 AND p.organization_id = $2 AND ($3 = '' OR p.project_id = $3)
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
SELECT k.id, k.organization_id, k.project_id, k.pool_id, k.key_sha256, k.revoked_at, k.expires_at,
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
 WHERE k.organization_id = $1
   AND ($2 = '' OR k.project_id = $2)
   AND ($3 = '' OR k.pool_id = $3)
 ORDER BY k.created_at DESC, k.id DESC;

-- name: RevokeRunnerPoolKey
-- Close the door. Idempotent (the first revoked_at is kept, the api_keys precedent) and tenant-scoped,
-- so another tenant's key id is simply not found. It touches NOTHING about the machines this key
-- already admitted: that is the property the whole task rests on, and it is a property of what this
-- statement does not say.
UPDATE runner_pool_keys
   SET revoked_at = coalesce(revoked_at, $4)
 WHERE id = $1 AND organization_id = $2 AND ($3 = '' OR project_id = $3)
RETURNING id, pool_id, key_prefix, created_at, expires_at, revoked_at, last_used_at;

-- name: ListRunnersEnrolledViaKey
-- The machines a revocation did NOT stop. An operator who revokes a key has to be shown them, or
-- "revoked" reads as "removed" and they believe one call decommissioned a fleet.
SELECT id, label, runner_dns, state, pool_id, enrolled_at, last_seen_at
  FROM runners
 WHERE enrolled_via_key_id = $1 AND organization_id = $2 AND ($3 = '' OR project_id = $3)
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
                  WHERE p.organization_id = $2 AND p.project_id = $3 AND p.name = 'default'), '')
  FROM runs r
 WHERE r.id = $1 AND r.organization_id = $2 AND r.project_id = $3;

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
UPDATE runs
   SET pool_id = $4, updated_at = clock_timestamp()
 WHERE id = $1 AND organization_id = $2 AND project_id = $3
   AND pool_id IS NULL
   AND EXISTS (SELECT 1
                 FROM runner_pools p
                WHERE p.id = $4 AND p.organization_id = $2
                  AND (p.project_id IS NULL OR p.project_id = $3));

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
SELECT r.organization_id, r.project_id, r.id, r.response_id
  FROM runs r
  JOIN attempts a ON a.run_id = r.id
                 AND a.organization_id = r.organization_id
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
 WHERE id = $1 AND organization_id = $2 AND project_id = $3;

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
-- `CREATE INDEX ... ON runs (organization_id, project_id, pool_id) WHERE state = 'waiting'` — a partial
-- index, because the rows this asks about are a vanishing fraction of the table.
SELECT r.id
  FROM runs r
 WHERE r.organization_id = $1 AND r.project_id = $2 AND r.pool_id = $3 AND r.state = 'waiting'
   AND EXISTS (SELECT 1
                 FROM attempts a
                WHERE a.run_id = r.id AND a.organization_id = $1 AND a.project_id = $2
                  AND a.state = 'awaiting_capacity')
 ORDER BY r.created_at, r.id
 LIMIT 1
 FOR UPDATE SKIP LOCKED;
