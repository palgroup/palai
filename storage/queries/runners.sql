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
INSERT INTO runners (
    id, organization_id, project_id, pool_id, label, runner_dns, public_key_sha256,
    state, os, arch, posture, capacity, cert_not_after, enrolled_at, last_seen_at
) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, clock_timestamp(), clock_timestamp())
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
