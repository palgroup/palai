# Runbook — backup and restore

**Fires when:** you need a backup before a risky change, you need to know an existing backup is good,
or you are restoring after a loss.

**Pinned transcript:** [`transcripts/backup-restore.txt`](./transcripts/backup-restore.txt)

**Reference (not repeated here):** [`../backup-restore.md`](../backup-restore.md) for the archive
format, credential hygiene, and the retention/prune policy;
[`../dr-drills.md`](../dr-drills.md) for the five recovery drills and the measured RPO/RTO.

## 1. Take the backup

```sh
palai backup --out "$WORK/palai-backup.tar.gz"
ls -l "$WORK/palai-backup.tar.gz"
```

One archive: the Postgres dump (`pg_dump -Fc`), the object-store volume, and a manifest. It reaches the
stack by **docker-exec on the container name**, so it needs no published host port — that is what makes
it safe to run against a production install where only the edge is published.

Always back up **before** an upgrade; `palai upgrade` does it for you unless you pass `--skip-backup`
([`upgrade-rollback.md`](./upgrade-rollback.md)).

## 2. Verify the archive without restoring it

```sh
palai restore verify --archive "$WORK/palai-backup.tar.gz"
```

Six checks, each recomputed from the archive rather than read out of the manifest:

| Check | What it proves | UAT |
|---|---|---|
| `archive_checksum` | Every member matches the manifest, per file | `DR-004` |
| `migration_version` | The schema version in the dump is the one claimed | `DR-004` |
| `tenant_ids` | Every organization in the manifest is present in the data | `DR-004` |
| `run_retrieval` | A real response is retrievable from the restored data — not just that rows exist | `DR-004` |
| `rls_isolation` | Every org-bearing table still FORCEs RLS with a policy | `DR-005` |
| `secret_decrypt` | A secret canary decrypts under the target master key | `DR-006` |

"All checks green" is the only acceptable state before you trust a backup. A backup you have never
verified is a hope, not a recovery plan.

## 3. Restore — into an EMPTY target only

```sh
palai restore --archive "$WORK/palai-backup.tar.gz"
```

The transcript pins this command **refusing**, because it was run against the stack that produced the
backup. Read that refusal carefully: it stops the writers first, then enumerates exactly what it found
(`api_keys=1, events=18, responses=3, …`) and declines. Restoring over live data is the mistake this
guard exists to prevent, and the guard is not overridable.

So a real restore goes into a **fresh install**:

1. Stand up a clean stack ([`../install.md`](../install.md)) — do **not** provision tenants into it;
   the empty-target gate excludes only the bootstrap `org_local` seed.
2. Run `palai restore --archive <path>` against it.
3. Run `palai restore verify --archive <path>` again, now against the restored stack.
4. Wait on the healthcheck, not on the bootstrap key: the restore swaps the local key, so the key you
   had before the restore is not the key afterwards.

<!-- drill: uat-self-host -->
```sh
make uat-self-host
```

That drill is the **executed** proof of the whole path — backup on stack A, restore into a separate
clean stack B, restore-verify there, ending in a real provider run (`DR-002`, `DR-004`..`DR-006`). It
is not re-run in this runbook's transcript because it needs two isolated production stacks; it is the
same commands in the same order.

## 4. If the primary database is gone

Go to [`../dr-drills.md`](../dr-drills.md) §"Database primary loss (DR-001)". The measured RPO/RTO for
the single-node topology, recomputed from raw timestamps, is in
[`../dr-report.md`](../dr-report.md) — read its findings section before promising a recovery time.

## Honest ceiling

- **RPO is bounded by backup cadence, not by replication.** There is no WAL archiving and no streaming
  replica; the loss window is the time since the last `palai backup`. Set the cadence accordingly
  ([`../backup-restore.md`](../backup-restore.md) retention section, and the scheduled-backup timer).
- **RTO excludes human detection and decision time** — the number in
  [`../dr-report.md`](../dr-report.md) starts when someone begins the restore.
- **Same-host drill, not a second-site failover.** Restore into a separate host is an operator leg
  (E14 §6 leg 2); cross-region failover is `not claimed` in
  [`../../security/threat-model.md`](../../security/threat-model.md).
- The object store is backed up by **copying the data volume**, not through an S3 API, so an external
  S3 backend is outside what `palai backup` covers today.
