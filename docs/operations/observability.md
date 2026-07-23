# Observability — metrics, dashboards, alerts (E14 Task 6)

The self-host stack exposes a Prometheus `/metrics` endpoint on the control-plane and ships a ready
Grafana/Prometheus bundle under `deploy/observability/`. This page is the operator runbook.

## What `/metrics` is

`GET /metrics` on the control-plane serves Prometheus text exposition. It rides the same **internal,
unauthenticated** surface as `/healthz` (the top mux, ahead of the tenant-auth middleware): a
Prometheus on the stack's internal network scrapes it, and the production TLS edge proxies `/v1` only,
so it is never reachable off the internal network. It needs no tenant key.

Every series is **installation-aggregate** — grouped by a lifecycle enum (`state`, `status`) or a
background-loop name (`loop`), never by organization/project/secret. An unauthenticated scrape
therefore leaks no tenant identity. This is asserted two ways: a unit test (`collector_test.go`) checks
the rendered label set contains only the allowed keys, and a component test
(`collector_component_test.go`) seeds two tenants and asserts their real ids appear nowhere in the
output while the aggregate counts them both.

`/metrics` is mounted via the `api.WithMetrics` router option (the E13 trailing-option pattern), so a
tier that wires no collector simply mounts no `/metrics` — the same nil-seam every optional surface
uses. It is a separate concern from a dedicated listener because `/healthz` already establishes an
unauthenticated internal surface on this mux; a second listener would duplicate the bind for no
isolation gain (both are internal-only, both unauthenticated).

### Series

| Metric | Type | Source | Alert |
|---|---|---|---|
| `palai_db_up` | gauge | 1 when the scrape's aggregate queries succeeded | PalaiScrapeDatabaseDown |
| `palai_runs{state}` | gauge | `runs` grouped by lifecycle state | — |
| `palai_durable_jobs{status}` | gauge | `durable_jobs` grouped by status | — |
| `palai_queue_ready_depth` | gauge | claimable queued jobs (`ready_at` arrived) | — |
| `palai_queue_oldest_ready_seconds` | gauge | age of the oldest claimable job | PalaiQueueBacklog |
| `palai_job_inflight_oldest_seconds` | gauge | age of the oldest running job (dispatch-progress proxy) | — |
| `palai_webhook_deliveries{state}` | gauge | `webhook_deliveries` by state (pending=backlog, dead=failure) | PalaiWebhookDeliveryBacklog / PalaiWebhookDeadLetters |
| `palai_db_clock_skew_seconds` | gauge | database clock minus control-plane clock | PalaiClockSkew |
| `palai_runner_sessions` | gauge | runner sessions connected to the gateway (0 = no runner) | PalaiRunnerDown |
| `palai_supervisor_restarts_total{loop}` | counter | background-loop restart counters | — |
| `palai_disk_free_bytes` / `palai_disk_total_bytes` | gauge | `statfs(PALAI_METRICS_DISK_PATH)` | PalaiDiskLow |
| `palai_object_store_up` | gauge | a per-scrape HEAD probe of the object store | PalaiObjectStoreDown |
| `palai_provider_calls_total` / `palai_provider_errors_total` | counter | provider model calls / failures, counted at the two broker call sites | PalaiProviderErrors |

Plus `PalaiControlPlaneDown` on Prometheus's own `up{job="palai-control-plane"}`.

The run-state/queue/webhook/clock series are **queried fresh from the durable spine at scrape time**
(under the documented `WithSystemScope` cross-tenant escape hatch — a whole-installation count cannot be
scoped to one tenant), so they reflect the source of truth rather than a counter that resets on
restart. `palai_runner_sessions` reads the gateway's live connected-session count; `palai_object_store_up`
is a real reachability HEAD; the provider counters are incremented at the model broker's two call sites.

### Configuration

- `PALAI_METRICS_DISK_PATH` — filesystem to `statfs` for the disk series. Point it at the data-volume
  mount; unset defaults to `/` (the container rootfs).

## Running the bundle

The overlay adds Prometheus (scraping `control-plane:8080/metrics`) and Grafana (the provisioned
dashboard), both digest-pinned like the rest of the stack:

```sh
docker compose -f deploy/compose/compose.yaml -f deploy/compose/compose.observability.yml up -d
```

Prometheus is at `127.0.0.1:${PALAI_PROM_PORT:-9090}`, Grafana at `127.0.0.1:${PALAI_GRAFANA_PORT:-3000}`.
The overlay opens no new control-plane surface — `/metrics` already ships in the control-plane image;
the overlay only adds the two observers.

## Validating the bundle

```sh
# Alert-rule + scrape-config syntax (via the pinned Prometheus image's promtool):
docker run --rm --entrypoint promtool -v "$PWD/deploy/observability:/etc/prometheus:ro" \
  prom/prometheus:v2.54.1 check rules /etc/prometheus/alerts.yml
docker run --rm --entrypoint promtool -v "$PWD/deploy/observability:/etc/prometheus:ro" \
  prom/prometheus:v2.54.1 check config /etc/prometheus/prometheus.yml
```

The Grafana dashboard validates against the Grafana schema by provisioning cleanly into a real Grafana
(`/api/dashboards/uid/palai-self-host-overview` returns it with `provisioned: true`).

### Live alert-fire proof

The runner-down alert is proven live, not just by syntax: bring the stack up with the observability
overlay, confirm the Prometheus target is `up` and `palai_runner_sessions >= 1`, then stop the runner
(`docker stop <project>-runner-1`). `palai_runner_sessions` drops to 0 and `PalaiRunnerDown`
transitions `inactive -> pending -> firing` within `for: 30s` plus one scrape interval — observable on
Prometheus's `/api/v1/alerts`.

The gauge is honest even for an idle runner: the gateway's connect handler runs a single read-loop for
the connection's whole life, so a runner that dies while parked-and-idle (nothing else reads the
connection then) is noticed at once rather than only at the next lease dial.

## Honest ceiling

- Alert `for:` windows and long-horizon rules (e.g. a disk-**trend** rule) are validated here only by
  syntax and a synthetic trigger. Real-load tuning needs an operator's own production telemetry.
- `palai_disk_*` measures the control-plane container's view of `PALAI_METRICS_DISK_PATH`. A true
  multi-volume host wants one series per mount; single-node alpha has one data volume.
- The object-store probe is a per-scrape HEAD (fine at single-node scrape intervals); cache it if scrape
  load ever matters.
- Everything above runs on the local production-compose seam. Pointing the same bundle at a real
  cloud VM is an operator leg (see the phase-14 plan §6) — the mechanism is identical; only the scrape
  target address changes.
