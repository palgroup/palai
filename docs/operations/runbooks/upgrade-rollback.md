# Runbook — upgrade and rollback

**Fires when:** an N→N+1 upgrade is planned, or one has gone wrong and you need N back.

**Pinned transcript:** [`transcripts/upgrade-rollback.txt`](./transcripts/upgrade-rollback.txt)

**Reference (not repeated here):** [`../upgrade.md`](../upgrade.md) — the migration model
(expand/migrate/contract, the rollback window, the `schema_revisions` journal), the full `palai
upgrade` sequence, the §48.2 support window, and the cordon/drain/revoke primitives.

## Read this first: there are two different "rollbacks"

[`../upgrade.md`](../upgrade.md) §"The two rollbacks — do not conflate them" is the section that saves
you here. In one line each:

- **Application rollback** returns the *binary* to N while the *schema stays expanded*. This works,
  and it is what `palai upgrade rollback` does.
- **Restoring from the pre-upgrade backup** is what you need if you must go back *past a contract*.
  A contract's `down.sql` does not re-create dropped data. Different operation, different runbook
  ([`backup-restore.md`](./backup-restore.md)).

## 1. Before any swap: know where you are

```sh
palai version
docker exec "$PG" psql -U palai -d palai -c "SELECT max(version) AS chain_head FROM schema_revisions;"
docker exec "$PG" psql -U palai -d palai -c "SELECT version, applied_at, applied_by, left(checksum,12) FROM schema_revisions ORDER BY version DESC LIMIT 5;"
```

The build stamp plus the chain head is the deployment's identity. A chain head **below** the binary's
head means a migration chain did not finish — restart the control plane and it resumes from the
journal; the live proof of that is `make migration-resume-drill` (`OPS-006`).

Then take a backup ([`backup-restore.md`](./backup-restore.md) §1). `palai upgrade` does it for you
unless you pass `--skip-backup`; do not pass `--skip-backup`.

## 2. The commands refuse to guess

```sh
palai upgrade
palai upgrade rollback
```

Neither has a default. There is no "upgrade to latest" and no "roll back to whatever was there before"
— an upgrade names the exact N+1 release manifest and a rollback names the exact N one. The transcript
pins both refusals.

```sh
cat "$WORK/bogus.json"
palai upgrade --manifest "$WORK/bogus.json"
```

And a manifest that does not carry the images it would swap in is rejected before anything moves.
Manifests come from `scripts/release/build.sh`; a hand-written one will not pass.

## 3. Run the upgrade

<!-- drill: upgrade-drill -->
```sh
palai upgrade --manifest <n+1-release-manifest.json>
```

The sequence — backup, compatibility verify, control-plane swap, **runner drain**, engine-alias roll,
smoke — is documented step by step in [`../upgrade.md`](../upgrade.md) §"The sequence". Two properties
worth knowing before you press it:

- **An active run survives on its pinned engine.** The alias rolls only *after* the drain, so a run in
  flight finishes on the engine it started with (`OPS-005`).
- **A runner whose version skew is outside the §48.2 window is rejected**, with the intermediate hop
  named in the message (`OPS-008`). Check the window before you plan a multi-version jump.

## 4. Roll back

<!-- drill: upgrade-drill -->
```sh
palai upgrade rollback --to <n-release-manifest.json>
```

Swaps the control-plane (and runner) image back to N, rolls the engine alias back, then smokes. It does
**not** run the migration chain backward — N boots on the expanded schema because a contract only ever
drops a shape no in-window binary reads (`OPS-007`).

If N will not boot on the expanded schema, you are past the rollback window: restore the pre-upgrade
backup ([`backup-restore.md`](./backup-restore.md) §3).

## 5. Verify the outcome

```sh
palai local doctor
```

`migration` green at the expected version, `supervisor` green, `runner` green. Then re-cut the audit
anchor — an upgrade is exactly the kind of maintenance that leaves the previous checkpoint behind
([`audit-integrity-alert.md`](./audit-integrity-alert.md) §4).

<!-- drill: upgrade-drill -->
```sh
make upgrade-drill
```

The **executed** proof of §3 and §4 end to end: two real builds off the same fork point, an active run
surviving the swap on its pinned engine, one real-provider smoke, then a rollback that runs N on the
expanded schema, and an old-stamp runner rejected with the hop message. It is not re-run in this
runbook's transcript because it builds two full stacks; it runs the same commands this runbook names.

## Honest ceiling

- **No published release-to-release upgrade has been performed.** The drill upgrades between two local
  builds off one fork point. A real published N→N+1 is an operator leg (E15 §6 leg 4).
- **Drain is whole-gateway.** There is one runner in this topology, so cordon/drain/revoke are not
  keyed per runner id; a multi-runner fleet would need that ([`../upgrade.md`](../upgrade.md) honest
  ceiling).
- **The engine pin is by sequencing, not by a durable per-run column.** Concurrent alias rolls during
  active runs are outside what is proven.
- **Background data migration is unproven** — no long resumable backfill exists in the chain yet
  ([`../upgrade.md`](../upgrade.md) honest ceiling, migrations).
