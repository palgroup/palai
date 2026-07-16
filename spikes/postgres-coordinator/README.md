# PostgreSQL coordinator spike

This spike proves the minimum crash-safe coordination model against a real PostgreSQL 16 process. It uses the locally available official image by immutable digest:

```text
postgres@sha256:17e67d7b9890c99b055ba1e0d5c5be4ec27c9d3a72bda32db24a5e5d8a85af0c
```

The script uses `--pull=never`, an OS-assigned loopback port, a per-run labeled container and a per-run labeled volume. Cleanup runs on success and failure, and the test fails if labeled container or volume counts do not return to their starting values.

## Coordination invariants

- Claim eligibility uses PostgreSQL `clock_timestamp()` rather than a worker clock.
- The claim row is locked with `FOR UPDATE SKIP LOCKED`.
- Claim commits before the worker emits its machine-readable receipt.
- Every claim increments a monotonic `fence` and `attempt_count`.
- Completion requires the exact job ID, fence, lease owner and `running` status.
- Result state and the unique `(job_id, fence, event_type)` outbox row commit in one transaction.
- A stale callback affects zero rows and cannot create result or outbox state.
- Killing a process after staging result and outbox writes rolls back both.

## Commands

Run one iteration of every real process-kill scenario:

```bash
scripts/spikes/postgres-coordinator quick
```

Run the same profile with Go's race detector:

```bash
PALAI_SPIKE_RACE=1 scripts/spikes/postgres-coordinator test
```

Generate the 20-iteration evidence report from a clean source commit:

```bash
PALAI_SPIKE_REPORT_OUT=spikes/.evidence/postgres-coordinator.json \
  scripts/spikes/postgres-coordinator evidence
```

The report contains only aggregate iteration and cleanup counts, tool versions and the immutable image digest. It does not retain database URLs, credentials, ports, process IDs, row data or Docker object IDs.
