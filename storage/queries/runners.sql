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
