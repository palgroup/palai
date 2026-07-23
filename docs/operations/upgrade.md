# Upgrade — schema migrations

This page covers the **migration half** of a Palai upgrade: how the schema changes are applied, the
expand/migrate/contract discipline that keeps a change reversible across a release, the rollback window,
and how to read the migration journal to confirm an upgrade landed. The N→N+1 control-plane swap, runner
drain, and engine-alias rollback sequence are a separate half (`palai upgrade`, added in E15 T2).

## How migrations apply at boot

The control-plane applies its schema at startup, before it serves any request. Since E15 T1 the runner
applies the chain **one migration at a time**, each in its own transaction:

1. A **preflight** runs once (see below).
2. Each migration file (`storage/migrations/NNNNNN_name.up.sql`, in ascending version order) is applied
   under the owning role, bounded by `lock_timeout` and `statement_timeout`.
3. From `000033` on, each applied migration records a row in the **`schema_revisions` journal** in the
   **same transaction** as the migration itself, so the journal head can never run ahead of the schema.

Every migration file is **idempotent** (`CREATE TABLE IF NOT EXISTS`, `ADD COLUMN IF NOT EXISTS`,
`ON CONFLICT DO NOTHING`, guarded `DO` blocks), so the whole chain is safe to re-run on every boot. That
is what makes an interrupted upgrade **resumable**: if the process dies mid-chain, the committed
migrations stay committed and a restart re-runs the chain from the top, skipping what is already applied
and continuing to the head.

The whole chain is **advisory-locked** (`pg_advisory_lock`, a single fixed key), so only **one migrator
runs at a time**. Two control-planes booting simultaneously (a multi-replica Kubernetes rollout)
serialize: the second waits for the first to finish, then re-runs the idempotent chain as a no-op. The
lock is session-scoped, so it releases automatically if a migrator crashes — a dead migrator never wedges
the next boot.

### Preflight

The preflight is a boot gate, before the first migration:

| Check | Default | What it does |
|---|---|---|
| **Version window** | always on | Refuses to run when the database's recorded head is **newer** than the binary's chain head — an older control-plane must not migrate a database a newer one already advanced. Upgrade the binary first. |
| **Disk headroom** | off (`PALAI_MIGRATE_MIN_DISK_BYTES=0`) | When a floor is set, `statfs` the data path (`PALAI_MIGRATE_DISK_PATH`, default `.`) and refuse below it. **Ceiling:** this measures the *control-plane host's* path, not necessarily an external/managed DB volume. |
| **Backup status** | off (`PALAI_MIGRATE_REQUIRE_BACKUP` unset) | When set truthy, a backup marker file (`PALAI_MIGRATE_BACKUP_MARKER`) must exist or the migration is refused. **Ceiling:** it trusts the marker's presence; it does not verify backup integrity — that is `palai backup verify`. |

Enable the disk and backup gates for a **risky upgrade** (one that includes a contract, below):

```sh
export PALAI_MIGRATE_MIN_DISK_BYTES=$((2*1024*1024*1024))   # 2 GiB
export PALAI_MIGRATE_REQUIRE_BACKUP=1
export PALAI_MIGRATE_BACKUP_MARKER=/var/lib/palai/backups/latest.manifest
```

### Bounded locks

Each migration runs with `SET LOCAL lock_timeout` / `statement_timeout` (defaults 15 s / 5 min,
overridable with `PALAI_MIGRATE_LOCK_TIMEOUT_MS` / `PALAI_MIGRATE_STATEMENT_TIMEOUT_MS`). A migration that
cannot get its lock, or a statement that never returns, **aborts the boot** rather than hanging forever
holding a lock. `SET LOCAL` is transaction-scoped, so no timeout leaks onto pooled application
connections.

## Expand / migrate / contract

A schema change that would break a running older binary is split across **three tranches, in three
separate releases**, so that at every moment the schema is compatible with the binary versions inside the
rollback window:

1. **Expand** — add the new shape (a new column/table/index) *additively*. The old binary ignores it; the
   new binary can use it. Nothing is removed.
2. **Migrate** — point writers/readers at the new shape and backfill. Both shapes still exist; either
   binary works.
3. **Contract** — once the new shape has shipped and drained and **no in-rollback-window binary still
   reads the old shape**, drop the old shape.

Contract is the only destructive tranche, and it is only safe a **full release after** its expand shipped.

### The rollback window

Application rollback (going from binary N+1 back to N while the schema stays expanded — E15 T2's
`palai upgrade rollback`) must always land on a schema the N binary can run. That is why a contract waits:
dropping a table/column that release N still reads would make a rollback to N crash. The rule:

> **Never contract a shape any binary within the supported rollback window still reads.** Verify with a
> tree-wide grep for readers *and* confirm the successor shipped in a prior release.

### Worked example — `000034` drops `usage_events`

`000034_contract_usage_events` is the real contract in this codebase:

- **Expand + migrate already shipped:** `usage_ledger` (`000032`) superseded `usage_events` in **E13**, a
  prior release. Writers moved to the ledger; the successor drained.
- **Zero readers:** a tree-wide grep finds no non-test, non-migration reference to `usage_events`; the
  writer LP-0 once sketched never landed (see `000032`'s own comment).
- **Rollback target is safe:** the N binary (E13/E14) reads `usage_ledger` and never touched
  `usage_events`, so dropping it cannot break a rollback inside the window.

Only with all three true does `000034` run `DROP TABLE usage_events`. A contract is **one-way**: its
`down.sql` does not re-create the table (there was no data and no reader). A real downgrade past a contract
restores from a **pre-upgrade backup**, not from the down migration.

## The migration journal — `schema_revisions`

`schema_revisions` is an **append-only** journal the runner writes one row to per applied migration, from
`000033` (its own introduction) forward:

| Column | Meaning |
|---|---|
| `version` | the migration number (matches `schema_migrations.version`) |
| `checksum` | sha256 of the migration file's `up.sql` — detects a file whose bytes drifted from the one that first applied it |
| `applied_at` | when it applied |
| `applied_by` | the binary's version stamp (VCS revision, or an `-ldflags` override) |

It is append-only by grant: the runtime role may `SELECT` it but the self-re-asserting
`REVOKE UPDATE, DELETE ... FROM palai_app` (re-run every boot) means the process cannot restate or erase
history. Migrations older than `000033` predate the journal and are recorded in `schema_migrations` only;
the journal's **head is always the true latest version**.

### Reading it

Confirm an upgrade reached its expected head:

```sql
SELECT max(version) FROM schema_revisions;                 -- the chain head
SELECT version, applied_at, applied_by, left(checksum, 12)
  FROM schema_revisions ORDER BY version;                  -- the applied-migration audit trail
```

After an **interrupted** upgrade that resumed, the head equals the binary's chain head and every migration
from `000033` on carries a row. A head *below* the binary's chain head means the chain has not finished —
restart the control-plane and it resumes.

The interruption/resume behaviour has a live drill against the **shipped binary** (a real crash + restart,
not an in-process fault): `make migration-resume-drill` (`scripts/test/migration-resume-drill.sh`). It
spins a throwaway Postgres, kills the control-plane right after `000033`, restarts it, and asserts the
journal resumed to the head with seeded rows intact.

## Honest ceiling (migrations)

The **background data-migration** pattern (a long backfill that runs resumably behind the schema change)
is **not yet proven** — the live chain has no big-data backfill case, and the component tests exercise
only a bounded-lock probe, not a real resumable backfill. The **first real** backfill migration must adopt
this resumable, journaled, bounded-lock, advisory-locked path rather than a one-shot `UPDATE`.

---

# Upgrade — the N→N+1 sequence (`palai upgrade`)

This is the **sequence half** (E15 T2): the control-plane swap, runner drain, engine-alias roll, and the
two distinct rollbacks. The migration half above is the schema story; this half is the binary/image story.

## Version stamp

Every release build carries one version stamp, injected by `scripts/release/build.sh` via
`-ldflags -X …/packages/version.Stamp=<stamp>` into the control-plane, runner, and CLI. The stamp is
`<VERSION>+g<git-describe>` — `VERSION` (repo root) gives the semantic `major.minor.patch` the support
window compares; the git-describe suffix makes each build a distinct id. It is **build metadata only** —
"same binaries, different version stamp"; it never forks behaviour. `palai version` prints it, and the
migrator records it in `schema_revisions.applied_by`. `PALAI_VERSION` (env) overrides the baked stamp for
an operator pin or a drill; it is a compatibility identifier, never a secret.

The build also emits a **release manifest** (`release-manifest.json`): the target version and the
control-plane / runner / engine image digests. `palai upgrade` reads it to know what to pin.

## The §48.2 support window (OPS-008)

A control-plane serves its **current minor and the previous two** (current + previous two minors). The
runner advertises its stamp in the enroll/connect handshake; the control-plane checks it at **connect**
(not enroll, so an already-enrolled runner that has fallen too far behind after a control-plane upgrade is
caught **every session** — it never re-enrolls). A runner more than two minors behind is **rejected** with
the required intermediate-hop message, e.g.:

> `runner 0.12.0 unsupported: hop to 0.13.0 first, then to control-plane 0.15.0 (window: current+prev 2 minors)`

Two **unstamped** dev/from-source builds compare equal and skip the check, so a plain `palai local up` is
unchanged. `palai upgrade` runs the same window check against the target manifest **before** the swap
(`incompatible upgrade: …`), so a downgrade or too-wide jump is refused up front, not after the swap.

## The sequence

`palai upgrade --manifest <n+1-release-manifest.json>` runs, in order:

1. **backup + restore-status** — `palai backup` captures a Postgres dump + object-store copy + manifest
   BEFORE the swap, so a failed migration can be rolled back to it. The boot-time **require-backup
   preflight** (`PALAI_MIGRATE_REQUIRE_BACKUP` + a marker) is *not* auto-wired by `palai upgrade`: its
   marker must be readable **inside** the control-plane container, and the compose profile mounts no
   backup volume — so that gate is the **operator / Kubernetes-migration-Job** option (a marker on a
   mounted path), not the single-node compose default.
2. **signature / compat verify** — the target manifest parses, its images carry digests, and the target
   version supports the current version (§48.2). (Detached-signature verification of the manifest is the
   air-gap bundle's job, T4 — the same `openssl` tool.)
3. **expand** — the schema is expanded. On the single-node compose profile this is **folded into the
   control-plane swap**: the swapped control-plane applies the idempotent, advisory-locked migration chain
   at boot before it serves. The separate **pre-swap migration Job** is the Kubernetes path (Helm chart,
   T3) — there, N pods do not each migrate at boot; the Job migrates once.
4. **control-plane swap** — the control-plane image is pinned to N+1 and recreated. Compose sends the old
   container `SIGTERM`; it **drains its runner gateway** (stops offering new leases, waits up to
   `PALAI_DRAIN_TIMEOUT`, default 20 s, for the in-flight lease) before exiting — this is the graceful
   half of the drain. The `PALAI_ENGINE_IMAGE` alias is left on the **old** engine so a run interrupted by
   the swap re-pins the **same** engine on retry.
5. **runner drain** — the runner image is pinned to N+1 and recreated. A run interrupted by either
   recreate is reclaimed and completed by the **E10 §26.3 recovery layer** (coordinator reconcile +
   `WorkspaceRecovery`) on the new control-plane — the drain **reuses** that layer, it does not
   re-implement run migration. `palai upgrade` then **waits for the active run to reach a terminal
   status** before step 6, so it completes on its pinned engine.
6. **new-run engine-alias roll** — the control-plane is recreated once more with `PALAI_ENGINE_IMAGE`
   pointed at the **new** engine digest, so runs started **from now** pin the new engine. Because this
   happens **only after** the drain, any run that was active kept the old engine to completion — that is
   what "the active run stays on its **pinned engine**" means: the alias is rolled only once no active run
   can be disrupted by it.
7. **smoke** — one fake response admits and reaches a terminal status end to end. A **real-provider**
   smoke is the drill's job (credential from `.env.local`), never wired into the CLI.

## The two rollbacks — do not conflate them

E15 uses "rollback" for **two different things**. `palai upgrade rollback` does both for new runs but they
are distinct mechanisms:

- **Application rollback** (OPS-007, §48.5) — return the control-plane **binary/image from N+1 back to N**
  while the **schema stays expanded**. The N binary boots on the expanded schema because a **contract**
  only ever drops a shape that **no in-rollback-window binary reads** (see the migration half), so N's
  chain head equals the database head and the boot preflight passes. A real downgrade **past** a contract
  is **not** this — it restores from the **pre-upgrade backup** (the contract's `down.sql` does not
  re-create dropped data).
- **Engine-alias rollback** (§48.5) — return the **engine activation pointer** (`PALAI_ENGINE_IMAGE`) to
  N's engine **for new runs**. An **active run stays on its pinned engine** — exactly as in the forward
  roll, the alias change touches only runs started after it.

`palai upgrade rollback --to <n-release-manifest.json>` swaps the control-plane (and runner) image back to
N and rolls the engine alias back, then smokes. It does **not** run the migration chain backward.

## Runner cordon / drain / revoke (SAN-011)

The runner gateway carries three lifecycle primitives, proven at the component level and used by the
graceful shutdown above:

- **Cordon** — stop offering **new** leases; a waiting attempt requeues. An in-flight lease is untouched.
- **Drain** — cordon, then wait for the **in-flight lease** (not a parked-idle runner) to finish, bounded
  by a deadline; whatever does not finish is completed by the E10 recovery layer.
- **Revoke** — the hard stop: reject a decommissioned runner's **new connects** and drop its **in-flight
  session frames** (stale events refused).

## Honest ceiling (sequence)

- **Two local builds, no published prior release.** N→N+1 here is between **two local builds** off the same
  fork point — there is no published previous release yet, so a real published-release-to-release upgrade
  is the **operator leg** (plan §6). N and N+1 carry the **same migration head** (T2 adds no migration;
  T1's expand/contract tranche already applied at the fork point), so this drill proves the **binary swap
  + rollback-boots-on-expanded-schema**; the fresh expand→contract interruption/resume is T1's own OPS-006
  drill.
- **Single-runner drain.** Drain is a single-runner batch — there is no multi-runner fleet in SH-0, so the
  cordon/drain/revoke primitives are whole-gateway (the gateway *is* the runner). A multi-runner fleet
  would key them per runner id; that is the SaaS/post-SH-0 path.
- **Pinned engine by sequencing.** The active run stays on its engine because the alias rolls only after
  the drain — not because of a per-run durable engine column (T2 adds no migration). A per-run durable pin
  would be the upgrade path if concurrent alias rolls during active runs ever became a requirement.
