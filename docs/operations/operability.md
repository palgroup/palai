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
| `cert_pair` | the edge TLS pair the overlay **actually mounts** is present and readable — `PALAI_EDGE_CERT`/`PALAI_EDGE_KEY` when set, `ca/server.{crt,key}` otherwise. It resolves the same `${PALAI_EDGE_CERT:-…}` default the overlay does, so the validator cannot end up checking a different file than compose mounts. |
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

## `palai local doctor` — live health (15 checks)

`doctor` probes the running stack over the ports `palai init` published. It reports 11 core checks
(api, migration, object_store, runner, image_digests, provider, clock, retention_ttl,
runner_tls_reject, supervisor, host_quarantine) plus the three E14 T3 additions and
`runner_identity`:

| Check | Signal | Fails when |
|---|---|---|
| `disk` | free space on the data dir (`statfs`) | free/total `< 10%` (matches `PalaiDiskLow`) |
| `queue` | claimable-backlog depth + age of the oldest ready job | oldest ready `> 300s` (matches `PalaiQueueBacklog`) |
| `callback` | outbound-webhook delivery states | `pending > 50` (matches `PalaiWebhookDeliveryBacklog`) |
| `runner_identity` | the runner's client-certificate lifetime, from `/healthz/runner` | it has expired **and** no runner session is connected |

`runner_identity` closes a blind spot the other fourteen share. The runner renews its client
certificate over mTLS *with that certificate*, so a runner that misses its renewal window — a
sleeping laptop, a stalled Docker Desktop, a loaded host — holds an identity that can neither renew
nor connect. `runner` and `runner_tls_reject` stay green through that (the gateway is listening and
still enforcing mTLS); the only symptom is that every run fails. The runner recovers itself by
re-presenting its bootstrap token, so an expired identity with a session still parked is a runner
mid-recovery — named in the detail, not failed. Expired **with nothing connected** is the fault.

The first three read the **same signals** `/metrics` exposes (§52.9/§52.10): `queue` and `callback` reuse
the `MetricQueueReady` / `MetricWebhookDeliveryStates` statements in
`storage/queries/metrics.sql`, and each fails on the **same boundary** as its Prometheus alert in
`deploy/observability/alerts.yml` — so doctor is the operator's on-demand version of the alert set.
Dead-lettered webhook deliveries are *named* in the `callback` detail but do not fail a point-in-time
check (their alert is a delta over a window).

```sh
palai local doctor            # human table; non-zero exit on any non-green check
palai local doctor --json     # the Report contract the UAT harness parses
```

### `palai doctor` — the same questions, against a PRODUCTION stack

`local doctor` reaches the control-plane, Postgres, and the object store over **host-published**
ports. The production overlay publishes none of them (`ports: !reset []`; only the edge is published),
so against a production stack it reported 13 of its 15 checks red for one reason that had nothing to
do with the stack's health — measured 2026-07-29 (`docs/operations/cloud-smoke-report.md`, Bulgu 5).

```sh
palai doctor --env-file production.env         # 18 checks; --json for the Report
```

Same questions, different transports — and both transports were already proven in this CLI:

- **`docker exec` by container name** for Postgres and for the internal `/healthz*` probes, which is
  exactly what `palai backup`/`restore`/`support-bundle` do and why *they* work against an edge-only
  stack. The `/healthz*` reads use the busybox `wget` the control-plane's own compose healthcheck
  uses, so no extra image is pulled and nothing has to be exposed;
- **the TLS edge** for the public API, which is what install.md step 6 already has you curl.

Every verdict is the shared function `local doctor` applies — `migrationCheck`, `clockCheck`,
`queueCheck`, `callbackCheck`, `quarantineCheck`, `supervisorCheck`, `runnerIdentityFromBody`,
`diskCheck`. **No check is weakened to make production green.** Two are stronger, and three are new:

| Check | Difference from `local doctor` |
|---|---|
| `api` | proves TLS termination, CA trust and the edge's `/v1/*` path match, not a plaintext localhost port. It verifies against the name **your** certificate carries (system roots + the local CA), never with verification relaxed |
| `runner`, `runner_tls_reject` | one `openssl s_client` probe from a one-off container on the stack's own network, in the digest-pinned postgres image the stack already has — a verified handshake, then a certificate-less request that must be answered `401` |
| `edge_cert` | **new** — compares the certificate the edge is **serving** with the file on disk. `docker compose up -d edge` does not reload replaced certificate *contents*; the edge keeps serving the old one with no error anywhere (Bulgu 4). Also fails on an expired certificate |
| `backup_target` | **new** — `backup`/`restore`/`support-bundle` derive container names from `config.json`'s `project`, not `PALAI_COMPOSE_PROJECT`. Choose a different name and the stack is healthy while every disaster-recovery command fails with `No such container` (Bulgu 2) |
| `containers` | **new** — what `docker compose ps` was standing in for: every service running, and healthy where compose declares a healthcheck. Catches a crash-looping service under `restart: always`, which looks alive from outside |
| `object_store` | asks the **control-plane** whether it can reach `PALAI_S3_ENDPOINT` — the reachability the artifact path actually depends on |

A check that this posture genuinely cannot answer reports **`n/a`** with the reason: it is not
counted as green, the summary line names it, and `--json` carries the status — but it does not fail
the verdict, because a permanently-unmeasurable check that reddens every run trains an operator to
ignore the command. In practice the only `n/a` seen is `runner_tls_reject` when the handshake itself
failed, and then `runner` carries the real failure.

> Honest ceiling: this was measured against a production-overlay stack on Docker Desktop
> (`darwin/arm64`), not a real cloud VM or `linux/amd64`.

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
