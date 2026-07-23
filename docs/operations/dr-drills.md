# Palai Self-Host DR Drills — Runbook

This runbook covers the self-host disaster-recovery drills (E15 T5) and the operator recovery
procedures they exercise. The drills prove, on a live stack, that a Palai self-host install can
recover from database loss, object corruption, and master-key loss — and they **measure** the
recovery-point (RPO) and recovery-time (RTO) so the published targets are grounded in real numbers,
not aspiration.

The machine-generated results live in [`dr-report.md`](./dr-report.md); the raw evidence (with the
timestamps every RPO/RTO is computed from) lives in `evidence/dr/drill-evidence.json`.

> **REUSE, not reimplementation.** The drills drive the shipped `palai backup` / `palai restore` /
> `palai restore verify` (the E14 install-backup tooling) exactly as an operator would. The harness
> adds the drill choreography and the measurement — it does not fork the backup/restore code.

## Honest ceiling (read this first)

These drills run on the **local same-host two-stack** (two isolated production-compose stacks on one
Docker Desktop host). That means:

- **"Primary loss" is container + volume destruction on ONE host.** The database comes back on the
  same machine. A real instance/zone loss, and a restore onto a **separate physical host / cloud VM**,
  are the **operator leg** (plan §6, incl. E14 operator leg 2) — the harness is parametric on
  `PALAI_HOME` + the compose files, so an operator points it at a second host unchanged.
- **DR-003 cross-region failover is the SaaS plan**, not the self-host tier.
- **The master key is a FILE**, kept in escrow. A KMS-backed key + lease ceremony (SEC-001/003) is the
  **E13-H** hardening tranche, out of scope here. DR-005 proves the file seam is fail-closed.

The report and evidence name these ceilings; the drills never claim to have proven a second-site DR.

## The five drills

| Drill | Scenario | What it proves | Measured |
|---|---|---|---|
| **DR-001** | Primary (database) loss | destroy the pg container + volume → fresh pg + `palai restore` from the last backup → healthy + run-capable | **RPO + RTO** |
| **DR-002** | Restore into a separate clean stack | a backup captured from stack A restores into a SEPARATE clean stack B (no-clobber empty-target gate) | consistency |
| **DR-006** | `restore verify` six checks | archive checksum, migration, tenant-ids, run-retrieval, RLS isolation, secret canary all green on the restore | — |
| **DR-004** | Object corruption | byte-flip an object → the backup manifest's per-file sha256 detects EXACTLY which object; the backup holds the intact bytes; a tampered archive is fail-closed | exact detection |
| **DR-005** | Master-key recovery | wrong/absent master-key file → `restore verify` secret canary fails CLOSED; an escrow copy restores usability | fail-closed |

DR-002 and DR-006 are the same restore run measured together (a restore that verifies clean).

## Running the drills

Prerequisites: Docker Desktop, the Go toolchain, and the three stack images (built on demand by the
harness; a blocked builder reuses pre-built `palai/*:local` tags with the ceiling logged).

```bash
# Run the five drills on two throwaway production-compose stacks (measure + recompute-verify only):
go test -tags=uat -count=1 -timeout 20m -run TestDRDrills ./tests/uat/dr -v

# (Re)generate the committed machine-generated report + evidence artifact from a live run:
PALAI_DR_WRITE_REPORT=1 go test -tags=uat -count=1 -timeout 20m -run TestDRDrills ./tests/uat/dr -v
```

The drills use the **fake provider** — DR is about data recovery, not the model, so no credential is
needed and nothing secret rides argv/env/log/evidence. The stacks use unique compose project prefixes
(`palai-e15t5-a`, `palai-e15t5-b`) and every container/volume is torn down on exit (`down --volumes`,
both stacks) — a clean run leaves zero `palai-*` leaks.

### Measurement is fabrication-guarded

Every measured RPO/RTO lies in the evidence artifact **beside the raw timestamps it was computed
from**. `dr.Verify` (and, downstream, the E15 T6 `DrillProof` verifier) recomputes:

- `RPO = last_marker_written_at − last_marker_in_backup_at`
- `RTO = recovered_at − disaster_at`

and FAILs if a stored value does not reproduce. A hand-written or hard-coded number cannot survive the
recompute — do not edit `dr-report.md` by hand.

## Operator recovery procedures (what the drills script)

### Database primary loss (DR-001)

The drill destroys the pg container + its data volume; the runner and object store stay alive (this is
database loss, not total loss). Recovery:

1. **Bring up a fresh Postgres.** With Compose: `docker compose … up -d --wait postgres` recreates the
   container with a fresh volume.
2. **Re-run the migrations to the backup's version.** Recreate the control-plane so it boots against
   the empty database and runs the migration chain to head:
   `docker compose … up -d --wait --force-recreate control-plane`.
3. **Restore the last backup.** `palai restore --archive <last-backup>`. The empty-target gate passes
   on a fresh install; `restore` loads the consistent pg dump + the object store and waits for the
   control-plane healthcheck (the restore swapped in the backup's identity, so it waits on the
   container health, not the old bootstrap key).
4. **Confirm run-capability.** Create a run and confirm it reaches a terminal state.

**RPO** is the window of writes between the last backup and the failure (in the drill, the markers
committed after the backup). **RTO** is the wall-clock from destruction to healthy-and-run-capable.
In production, cap RPO by shortening the `deploy/systemd/palai-backup.timer` interval; to drive it
toward zero, add WAL archiving or a streaming replica (operator/E18 leg).

### Object corruption (DR-004)

Every `palai backup` records a **per-file sha256** of the object store in its manifest. To check an
object store against a backup, compare each file's current sha256 to the manifest — a mismatch names
EXACTLY which object corrupted. The backup archive still holds the intact bytes, so the object is
recoverable by restoring from the backup. A **tampered backup archive** is refused by `palai restore
verify` (the archive's checksum chain), so a corrupted backup never silently restores.

### Master-key recovery (DR-005)

Secrets are AES-256-GCM sealed under the install's master key (`${PALAI_HOME}/secrets/master-key`). If
the target boots with the **wrong or absent** key, `palai restore verify`'s **secret canary** fails
CLOSED (`secret_decrypt FAIL`) — the restored secrets are undecryptable, caught here rather than at the
first provider call. Recovery is the **escrow copy** of the source master key: restore the correct
key file and re-run `restore verify` — the canary goes green.

> **Keep the master key in escrow** (a sealed, offsite copy). Without it, a restore's secrets are
> dead. This is the file-key seam; the KMS ceremony is E13-H.

### Restore into a separate clean stack (DR-002 / DR-006)

`palai backup` on the source, then on a SEPARATE clean install: `palai restore --archive <backup>`
followed by `palai restore verify --archive <backup>`. Verify's six checks must all be green:
archive checksum, migration version, tenant ids, sample-run retrieval, RLS isolation (FORCE row-level
security + the tenant_isolation policies survived), and the secret canary. The no-clobber empty-target
gate refuses a restore into a stack that already holds tenant data.

## Operator legs (deferred, parametric — not run here)

The harness is designed so these run the SAME code against real infrastructure the operator provides:

1. **Separate physical host / cloud VM restore** — point the harness at a second host; a real instance
   loss (E14 operator leg 2).
2. **Cross-region failover (DR-003)** — the managed SaaS plan.
3. **KMS-backed master key** — the E13-H hardening tranche (SEC-001/003).

Until the operator legs run, the published targets in `dr-report.md` are the **single-node** achievable
posture, explicitly NOT the managed-SaaS targets.
