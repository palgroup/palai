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

> **PRECONDITION — carry the master key (production).** Secret values (`secret_refs`) are
> AES-256-GCM-sealed under the **source** stack's `${PALAI_HOME}/secrets/master-key`. A restore does
> NOT move that key. Before restoring an install that has secrets, **copy the source
> `secrets/master-key` onto the target** (overwriting the target's own). Without it every restored
> provider/MCP/webhook secret is undecryptable and the install is silently non-functional —
> `restore verify`'s secret canary fails loudly if the key is missing or mismatched.

**Fail-closed (no-clobber):** restore refuses any target that holds tenant data — **any** row in an
org-bearing (tenant-scoped) table beyond a fresh install's baseline: not just extra organizations or
runs, but a project, api-key, `secret_ref`, model-route, schedule, tool, etc. created under **any**
org (including the seeded `org_local`). The gate enumerates the FORCE-RLS tables from the live
catalog, excludes the four boot-seed identity rows and the runner-enrollment tables, and counts the
rest; a non-empty result names the offending tables. A **freshly `init`ed, brought-up** stack passes;
a stack that has been provisioned or used does not — no live data is ever overwritten.

The writers are stopped **before** the gate runs (so a client write cannot slip in between the check
and the swap); if the gate refuses, the writers are restarted and the target is left as it was. It
then verifies the archive's member checksums, requires the **target's migration version to equal the
backup's** (a mismatch is refused — restoring across schema versions would rewind
`schema_migrations` into a boot crash-loop), and with the writers stopped for the swap:

1. `pg_restore --clean --if-exists --no-owner --exit-on-error` — replaces the fresh schema +
   loads the backup's data;
2. clears and re-extracts the object-store data volume;
3. restarts the object store and the writers, and waits for the control-plane healthcheck.

If a step past the migration/empty checks fails, the target is **half-restored**: the error says so
and to re-init it (`palai local reset --confirm`) before retrying.

> **Identity after restore.** The restore replaces the target's `key_local` row with the **source's**
> bootstrap-key hash. So after a restore the target's own `palai init`-printed bootstrap key stops
> authenticating, and the **source** stack's bootstrap key becomes the valid one against the target.
> Provision fresh keys with the admin CLI, or keep using the source's bootstrap key.

## `palai restore verify`

```sh
palai restore verify --archive palai-backup-<project>-<UTC>.tar.gz
```

Against the restored target it checks:

- **archive checksum** — every member re-hashes to its manifest value;
- **migration version** — live `max(schema_migrations.version)` == manifest;
- **tenant ids** — every manifest org id is present in the restored data (a concurrent org created
  during a live backup may add an extra one — that is not a failure; a *missing* backed-up org is);
- **run retrieval** — the manifest's sample response is retrievable from the restored database
  (proving the tenant data is queryable);
- **rls isolation** — the restored data still has FORCE ROW LEVEL SECURITY + a `tenant_isolation`
  policy on every org-bearing table (a restore that landed with RLS disabled is a silent
  cross-tenant breach the superuser queries above would never notice);
- **secret decrypt** — if `secret_refs` has rows, one is decrypted under the target's master key,
  so a missing/mismatched master key (see the restore precondition) is caught here, not at the
  first provider call.

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

> **The secret path is NOT exercised by that two-stack proof.** The base compose profile has no
> secret store (`PALAI_SECRET_MASTER_KEY_FILE` is unset), so the two-stack backup/restore never
> carries a `secret_ref`. The master-key round-trip — a secret sealed under key A fails closed under
> key B, and `restore verify`'s canary catches it — is proven at the **component tier against a real
> Postgres with two master keys**, not in the two-stack run.

The object store in the packaged stack holds **no wired S3 objects** — the control-plane sets no
`PALAI_S3_ENDPOINT`, so artifacts are not written to S3 today. The backup copies the object-store
data volume byte-for-byte and records a per-object sha256 of its contents.

> **Ceiling when the S3 write-path is wired.** The volume is tar'd from the **live** object store,
> which is crash-consistent-enough for today's empty/near-idle store but is **not** a consistent
> snapshot under concurrent writes, and it is a slightly different point in time than the Postgres
> dump. When artifacts are actually written to S3, take backups during a quiet window, or upgrade the
> object-store copy to quiesce the store (or enumerate + GET each S3 object) so it aligns with the DB
> snapshot. The per-object checksums are then the stored objects with no manifest change.
