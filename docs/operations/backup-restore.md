# Installation backup & restore

`palai backup` / `palai restore` / `palai restore verify` capture and revive a **whole Palai
install**: a consistent Postgres dump, the object-store data, and a manifest, packed into one
archive that restores into a **separate clean stack**.

> **This is a different layer from the run-level checkpoint restore.** The recovery ladder in
> `apps/control-plane/internal/execution/{snapshot,restore}.go` revives *one run* inside a *live*
> stack (spec §26.3). This is the **installation** layer: it moves the entire stack's data to a
> fresh install. The two share no code and no names.

## How it reaches the stack

Like `palai local doctor`, these commands reach the running stack's containers **by name through
the Docker socket** (`docker exec` / `docker run`), never through host-published ports. The
production profile keeps Postgres and the object store on the internal network
(`deploy/compose/production.yml` `ports: !reset []`), so backup works against the hardened
deployment where only the TLS edge is published. Run them on the Docker host, from the same
working directory whose `${PALAI_HOME}/.palai` identifies the stack (project name, ports).

## Credential hygiene

The archive holds real tenant **data** — that is its purpose. But:

- the **manifest carries only ids + checksums**, never a secret (a test scans it for
  secret-shaped tokens);
- the **Postgres password** is read from the in-container file-secret inside a `sh -c` wrapper,
  so it never appears in the host process's `argv` or in a log line.

Keep the archive itself protected at rest (it contains tenant data): `chmod 600`, encrypt it for
off-host storage, and restrict who can read the backup directory.

## `palai backup`

```sh
palai backup                       # -> palai-backup-<project>-<UTC>.tar.gz in cwd (path on stdout)
palai backup --out /backups/palai-2026-07-23.tar.gz
```

It:

1. reads the migration version, the org + project ids, and a sample response id (over the
   internal Postgres, as the superuser — RLS is bypassed so every tenant is captured);
2. runs `pg_dump -Fc` — a **consistent** custom-format dump (single snapshot);
3. copies the object-store **data volume** byte-for-byte;
4. writes a `manifest.json` (kind, migration version, org/project ids, sample response id,
   whole-member checksums, and a **per-object** sha256 for each object-store file) alongside
   `db.dump` and `object-store.tar` into one gzip'd tar.

## `palai restore` (into an EMPTY target only)

```sh
palai restore --archive palai-backup-<project>-<UTC>.tar.gz
```

**Fail-closed:** restore refuses any target that already holds tenant rows (organizations,
responses, or runs) — it never overwrites live data. Restore only into a **freshly `init`ed,
brought-up** stack (schema migrated, no tenant data).

It verifies the archive's member checksums, then, with the writers stopped for the swap:

1. `pg_restore --clean --if-exists --no-owner --exit-on-error` — replaces the fresh schema +
   loads the backup's data;
2. clears and re-extracts the object-store data volume;
3. restarts the object store and the writers, and waits for the API to answer.

## `palai restore verify`

```sh
palai restore verify --archive palai-backup-<project>-<UTC>.tar.gz
```

Against the restored target it checks:

- **archive checksum** — every member re-hashes to its manifest value;
- **migration version** — live `max(schema_migrations.version)` == manifest;
- **tenant ids** — live org ids == manifest (set equality);
- **run retrieval** — the manifest's sample response is retrievable from the restored database
  (proving the tenant data is queryable).

Exit 0 with `restore verify: all checks green`, or non-zero listing the failed checks.

## Retention / prune policy (example)

Backups are plain files — retain and prune them with `find`. A daily backup with a 14-day window:

```sh
# 1. take a backup into the retention directory
palai backup --out "/backups/palai-$(date -u +%Y%m%dT%H%M%SZ).tar.gz"

# 2. prune backups older than 14 days (dry-run first: drop -delete to preview)
find /backups -name 'palai-backup-*.tar.gz' -mtime +14 -print -delete
```

For a grandfather-father-son policy, keep the newest of each period instead of a flat window:

```sh
# keep: last 7 daily, last 4 weekly (Sundays), last 6 monthly (1st) — prune the rest.
# Wire it into the scheduled-backup timer (E14 T5, deploy/systemd/palai-backup.timer).
```

The scheduled-backup systemd timer (E14 T5) calls `palai backup` on a schedule; pair it with a
prune step like the above so the retention directory does not grow unbounded.

## Honest ceiling (plan §6)

Restore to a **separate physical host / cloud VM** is the **operator leg**: the same commands run
there unchanged (backup on host A, copy the archive to host B, `restore` + `restore verify` on
host B). This document is verified against **two isolated stacks on the same Docker Desktop**
(different `PALAI_HOME`, ports, and volume set) — the local-production-compose equivalent of
"restore to a separate clean install". It does **not** claim a real separate-host restore.

The object store in the packaged stack holds **no wired S3 objects** — the control-plane sets no
`PALAI_S3_ENDPOINT`, so artifacts are not written to S3 today. The backup still copies the
object-store data volume byte-for-byte and records a per-object sha256 of its contents; when the
S3 write-path is wired, those entries are the stored objects with no code change.
