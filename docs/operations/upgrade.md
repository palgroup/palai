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

## Honest ceiling

The **background data-migration** pattern (a long backfill that runs resumably behind the schema change)
is proven here only with a *fixture* migration in the component tests — the live chain has no big-data
backfill case yet. The **first real** backfill migration must adopt this resumable, journaled,
bounded-lock path rather than a one-shot `UPDATE`.
