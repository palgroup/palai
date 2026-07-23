# Operability: config validate, doctor, support-bundle (E14 T3)

Three commands close the operate loop around a Palai stack: a **static** pre-flight audit of a
production deploy, a **live** health surface, and a **redacted** diagnostics bundle. All three are
subcommands of the same `palai` binary the operator already runs (`go build -o palai ./cmd/cli`).

---

## `palai config validate` — static production posture

A stack-**less** audit: it reads files, never dials a running stack, so it runs before bring-up (or
in CI) and catches an unsafe production profile *before* it boots.

```sh
palai config validate --env-file deploy/compose/production.env \
                      --overlay  deploy/compose/production.yml
# --json for a machine-readable report; a non-zero exit means "do not bring this up".
```

It checks:

| Check | What it asserts |
|---|---|
| `env_contract` | every required key in `production.env` is present; no unknown (typo'd) key |
| `master_key` | `${PALAI_HOME}/secrets/master-key` is present and **not** a dev-default |
| `bootstrap_key` | `${PALAI_HOME}/api-key` is not the shipped placeholder |
| `cert_pair` | the edge TLS `ca/server.crt` + `ca/server.key` are present and readable |
| `dispatch_workers` | `PALAI_DISPATCH_WORKERS >= 1` (production runs the exec-path, not queued-only) |
| `edge_only_surface` | the TLS edge is the **only** host-published surface, and the Caddyfile proxies **only** `/v1/*` |

The dev-default literals it rejects (the master-key placeholder, the all-zero key, the bootstrap
placeholder) are **read from `production-entrypoint.sh`**, the same fail-closed boot guard the stack
enforces at start — config-validate never re-declares them, so the two cannot disagree on what
"dev-default" means. The master-key file is read only to *compare*; its contents are never printed.

`edge_only_surface` is the machine-check that the production overlay resets every internal service's
host ports (postgres, object-store, control-plane, runner) and that the edge's `reverse_proxy` is
path-matched to `/v1/*`. Because `/metrics` and `/healthz` live on the control-plane's top mux
*outside* `/v1/*`, a `/v1/*` match proves they are **not reachable through the edge** — a catch-all
`reverse_proxy` (no path matcher) fails the check.

---

## `palai local doctor` — live health (14 checks)

`doctor` probes the running stack over the ports `palai init` published. It reports 11 core checks
(api, migration, object_store, runner, image_digests, provider, clock, retention_ttl,
runner_tls_reject, supervisor, host_quarantine) plus the three E14 T3 additions:

| Check | Signal | Fails when |
|---|---|---|
| `disk` | free space on the data dir (`statfs`) | free/total `< 10%` (matches `PalaiDiskLow`) |
| `queue` | claimable-backlog depth + age of the oldest ready job | oldest ready `> 300s` (matches `PalaiQueueBacklog`) |
| `callback` | outbound-webhook delivery states | `pending > 50` (matches `PalaiWebhookDeliveryBacklog`) |

The three read the **same signals** `/metrics` exposes (§52.9/§52.10): `queue` and `callback` reuse
the `MetricQueueReady` / `MetricWebhookDeliveryStates` statements in
`storage/queries/metrics.sql`, and each fails on the **same boundary** as its Prometheus alert in
`deploy/observability/alerts.yml` — so doctor is the operator's on-demand version of the alert set.
Dead-lettered webhook deliveries are *named* in the `callback` detail but do not fail a point-in-time
check (their alert is a delta over a window).

```sh
palai local doctor            # human table; non-zero exit on any non-green check
palai local doctor --json     # the Report contract the UAT harness parses
```

> `doctor` reaches the control-plane, Postgres, and object store over host-published ports. Under the
> **production** overlay those ports are intentionally not published (only the edge is), so doctor is
> run against the stack it can reach — the same binaries, local posture. Watching a production stack
> from outside its network is the operator leg (plan §6).

---

## `palai support-bundle` — redacted diagnostics

One `tar.gz` to hand to support: the doctor verdict, `compose ps`, the compose config, the last N log
lines per service, and the secret-free stack config.

```sh
palai support-bundle --out palai-support-bundle.tar.gz --tail 200
```

**Credential hygiene is enforced, not assumed.** Every part passes through a redactor before it
reaches the tar. The redactor scrubs both the stack's **exact** secret values (the master key, the
bootstrap key, the Postgres password, the provider credential, read from `${PALAI_HOME}`) and generic
secret *shapes* (provider `sk-…` keys, HTTP `Bearer` tokens, `*_KEY`/`*_PASSWORD`/`*_TOKEN` env
assignments) — so even a secret the assembler never parsed, leaked into a log line, is caught by
shape. A test reads the produced tar back and asserts **zero** secrets survive
(`supportbundle_test.go`); the master-key file is compared, never emitted.

A compose command that fails (e.g. the stack is down) records its error text instead of aborting the
bundle, so an operator diagnosing a broken stack still gets the doctor report and the config.

> Like `doctor`, `support-bundle` targets the project and base compose file from `.palai`
> (`cfg.Project` + `deploy/compose/compose.yaml`), not a hand-run `-p <name> -f compose.yaml -f
> production.yml` production overlay. Point it at a production stack by exporting
> `PALAI_COMPOSE_FILE` and bringing the stack up under the project `.palai` recorded — otherwise the
> `compose ps/config/logs` parts will describe the base-profile project. Watching a production stack
> whose ports aren't host-published is the operator leg (plan §6).
