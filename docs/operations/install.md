# Installing the Palai production stack (single-node)

The production profile is the packaged local stack (`compose.yaml`) with a hardened posture layered
on top by `production.yml`:

- a **digest-pinned TLS-terminating reverse-proxy edge** (Caddy) is the *only* host-published
  surface; the control-plane API and the mutually-authenticated runner gateway stay on the
  internal network;
- **every persistent service restarts always**;
- the dispatch exec-path is **on by default** (1 worker);
- the control-plane **refuses to boot** on an unset or dev-default secret master key
  (fail-closed, `production-entrypoint.sh`);
- there is **no public self-registration endpoint** — provisioning is bootstrap-key + the
  `palai` admin CLI (E14 T2) only.

Same images, different posture: no new server surface is opened.

> **Honest ceiling (plan §6).** The steps below bring the stack up on a single host with a
> **self-minted** local-CA certificate on the edge. A real deployment — a dedicated cloud VM,
> a real domain, and an ACME/real certificate — is the **operator leg**: the same steps run on
> that VM. This document does not claim a cloud deployment; it is verified against a local
> production-compose bring-up on Docker Desktop.
>
> **What that leg costs is now measured, not assumed.** Steps 1–6 come up and serve real runs
> unchanged. But a real domain is **not** a matter of swapping the certificate:
> `${PALAI_HOME}/ca/server.crt` is shared with the runner gateway, which pins exactly one SAN, so
> replacing it breaks every run at the next control-plane restart — read
> [step 7](#7-swap-in-a-real-certificate-operator-leg) before you try it. Terminate your domain on
> a proxy in front of the edge instead. Full transcript:
> `docs/operations/cloud-smoke-report.md` (2026-07-29).

> **Honest ceiling (images).** **No Palai image has ever been published.** `release.yml`'s publish
> step is `scripts/release/publish-dryrun.sh`, which holds no credentials and runs every leg against
> a blackholed registry (plan §6 leg 2). So route A below requires you to name the images yourself
> via `PALAI_*_IMAGE` — from your own registry, an [air-gap bundle](../../deploy/airgap/), or
> images you built from a checkout. A `palai` binary with no image override **refuses** with an
> actionable message; it will never silently pull something that is not this release.

## Route A — the packaged `palai` binary (no source tree)

This is the route for a host that has the `palai` binary and nothing else. The binary **carries the
compose files inside it** and writes them to `${PALAI_HOME}/compose` at `init`, so there is no
`deploy/` directory to clone and no path relative to your shell's cwd.

### Prerequisites

- Docker Engine + Compose v2.24+ (for `!reset` and inline `configs`; verify: `docker compose version`).
- The `palai` binary for your platform (`scripts/release/build.sh` produces the CLI matrix;
  until publication, take it from a colleague's build or [route B](#route-b--from-a-checkout-development)).
- `openssl` (to generate the master key).
- The three stack images reachable by your Docker daemon — see the images ceiling above.

Since nothing is published, the usual way to get those three images is to build them once from a
checkout and push them to your own registry (or load them on the host). Restrict the matrix to the
architecture you actually deploy — `build.sh` defaults to **both** `linux/amd64` and `linux/arm64`:

```sh
scripts/release/build.sh --tag 0.15.0 --out ./out \
  --platforms linux/arm64 --cli-targets linux/arm64 --runner-archs arm64
```

That produces `palai/{control-plane,runner,reference-engine}:0.15.0`, the `palai` CLI, and
`release-index.json`. The platform you name must include the build host's own architecture (the
release manifest pins host-platform refs); pass `--no-images` if you only want the binaries.

### 1. Initialise the data dir

```sh
export PALAI_HOME=/srv/palai            # persistent host path for this install
palai init
```

`palai init` mints, under `${PALAI_HOME}`: the local CA + edge server certificate
(`ca/ca.crt`, `ca/server.crt`, `ca/server.key`), the bootstrap API key (`api-key`), the
Postgres password (`secrets/pg-password`), a secret-store master key (`secrets/master-key`), an
empty provider secret slot, an empty GitHub App key slot (`secrets/github-app-key`) — **and
`compose/`**, holding `compose.yaml`, `production.yml`, `production-entrypoint.sh` and the two
`*.env.example` files the binary carries.

### 2. Regenerate the secret master key and mint the runner token

`init` mints a master key already. Regenerate it if you want one you chose yourself:

```sh
openssl rand -hex 32 > "${PALAI_HOME}/secrets/master-key"
chmod 600 "${PALAI_HOME}/secrets/master-key"
```

The boot guard refuses an unset/empty file, an all-zero key, and the placeholder
`REPLACE_WITH_OPENSSL_RAND_HEX_32`.

A hand-run compose must also create the GitHub App key slot, because compose mounts it on every
stack and a missing mount source fails `compose up` outright. Empty is the correct value until you
configure an App — the control-plane never reads the file while `PALAI_GITHUB_APP_ID` is unset:

```sh
touch "${PALAI_HOME}/secrets/github-app-key"
chmod 600 "${PALAI_HOME}/secrets/github-app-key"
```

Mint the one-use runner enrollment token as well — a hand-run compose (unlike `palai local up`)
does NOT create it, and its bind-mount source must exist as a file:

```sh
openssl rand -hex 24 > "${PALAI_HOME}/runner-token"
chmod 600 "${PALAI_HOME}/runner-token"
```

### 3. Configure the environment

```sh
cp "${PALAI_HOME}/compose/production.env.example" production.env
# edit production.env: set PALAI_HOME (absolute), PALAI_EDGE_PORT (e.g. 443),
# PALAI_ENGINE_IMAGE, PALAI_COMPOSE_PROJECT.

# Load it into THIS shell so the steps below (which reference $PALAI_EDGE_PORT,
# $PALAI_COMPOSE_PROJECT) run copy-paste:
set -a; . ./production.env; set +a

# Name the two service images — there is no published default (see the images ceiling).
export PALAI_CONTROL_PLANE_IMAGE=...      # e.g. registry.example/palai/control-plane:0.15.0
export PALAI_RUNNER_IMAGE=...             # e.g. registry.example/palai/runner:0.15.0
```

`PALAI_COMPOSE_PROJECT` must be set to the **`project` value `palai init` wrote into
`${PALAI_HOME}/config.json`** — not a name you invent:

```sh
grep '"project"' "${PALAI_HOME}/config.json"     # e.g. "project": "palai-21e1258e"
```

`palai backup`, `palai restore`, `palai restore verify` and `palai support-bundle` reach the stack by
`docker exec` on container names they derive from *that* file (`<project>-postgres-1`, and the
`<project>_palai-objects` volume), never from this env file. Choose any other name here and the stack
comes up perfectly healthy while every one of those commands fails with
`No such container: <project>-postgres-1` — that is, the whole backup/restore path is dead on a stack
that otherwise looks fine. Verified 2026-07-29 (`docs/operations/cloud-smoke-report.md`, Bulgu 2).

`PALAI_ENGINE_IMAGE` must be a **bare `sha256:<64 hex>` config digest**, not a repository tag: the
runner's lease validator accepts nothing else, and a tag there produces a stack that comes up healthy
and then fails every run at lease time. Resolve one with
`docker image inspect <ref> --format '{{.Id}}'`.

### 4. Validate the posture

```sh
palai config validate --env-file production.env
```

With no `--overlay`, this reads the overlay this binary would actually bring up — the one under
`${PALAI_HOME}/compose` — and the guard script beside it, so the checks and the fail-closed boot
guard cannot disagree about what a dev-default is. Then check compose itself parses:

```sh
docker compose --env-file production.env \
  -f "${PALAI_HOME}/compose/compose.yaml" -f "${PALAI_HOME}/compose/production.yml" \
  config >/dev/null && echo OK
```

### 5. Bring the stack up

```sh
docker compose --env-file production.env -p "$PALAI_COMPOSE_PROJECT" \
  -f "${PALAI_HOME}/compose/compose.yaml" -f "${PALAI_HOME}/compose/production.yml" \
  up -d --no-build --wait
```

`--no-build` is not optional here: the packaged files declare a build context (`../..`, the repo
root) that does not exist on this host. `palai local up` passes it for you on this path, and refuses
before touching Docker if an image reference is missing.

### 6. Verify TLS termination through the edge

The edge presents the local-CA server certificate (SAN `control-plane`). Pin the CA and
resolve that name to the edge. **The probe must be a `/v1/*` path** — an authenticated call that
round-trips the real API (bootstrap key in `${PALAI_HOME}/api-key`):

```sh
curl --cacert "${PALAI_HOME}/ca/ca.crt" \
  --resolve control-plane:${PALAI_EDGE_PORT}:127.0.0.1 \
  -H "Authorization: Bearer $(cat ${PALAI_HOME}/api-key)" \
  https://control-plane:${PALAI_EDGE_PORT}/v1/capabilities
```

> **Do not health-check `/healthz` through the edge.** The Caddyfile proxies **only `/v1/*`**
> (deliberately — it is what keeps `/metrics` off the public listener), and Caddy answers every
> unmatched path with an empty **HTTP 200**. So `curl .../healthz` through the edge returns `200`
> and exit code `0` **whether or not the control-plane is running** — it is indistinguishable from
> `curl .../this-endpoint-never-existed`, and it stays green with the control-plane stopped, while
> `/v1/capabilities` correctly returns `502`. Measured 2026-07-29
> (`docs/operations/cloud-smoke-report.md`, Bulgu 1). `palai config validate` reports this same fact
> as `edge_only_surface`. The real `/healthz` is reachable on the internal compose network only,
> which is where the compose healthcheck reads it.

Provisioning (org/project/api-key/secret) then goes through the `palai` admin CLI (E14 T2)
pointed at `https://control-plane:${PALAI_EDGE_PORT}` — there is no signup endpoint. The CLI has no
`--resolve` equivalent, so **the host must be able to resolve the certificate's name itself**; until
[step 7](#7-swap-in-a-real-certificate-operator-leg) gives you a real domain, add a hosts entry:

```sh
echo "127.0.0.1 control-plane" | sudo tee -a /etc/hosts

export PALAI_BASE_URL="https://control-plane:${PALAI_EDGE_PORT}"
export PALAI_API_KEY="$(cat ${PALAI_HOME}/api-key)"
palai org create --display-name "Example Org" --ca "${PALAI_HOME}/ca/ca.crt"
```

Without it the CLI fails with `dial tcp: lookup control-plane: no such host`, and substituting
`https://127.0.0.1:${PALAI_EDGE_PORT}` fails differently — `x509: cannot validate certificate for
127.0.0.1 because it doesn't contain any IP SANs` — because the cert carries the DNS name only.

### 6a. Day-one operations

With `PALAI_COMPOSE_PROJECT` set correctly ([step 3](#3-configure-the-environment)) these work
against the production stack, because they go through `docker exec` rather than host ports:

```sh
palai backup --out ./palai-backup.tar.gz     # Postgres + object store + manifest
palai restore --archive ./palai-backup.tar.gz        # into an EMPTY target stack only
palai restore verify --archive ./palai-backup.tar.gz # checksum, migration, tenant ids, run retrieval
palai support-bundle --out ./bundle.tar.gz   # redacted diagnostics
```

`palai local doctor` is the exception: it probes over the **host-published** ports from
`${PALAI_HOME}/config.json`, which this profile deliberately does not publish, so against a
production stack it reports almost every check red for the wrong reason. That is a known ceiling, not
a fault in your install — see `docs/operations/operability.md`. There is **no production health
command today**; `/v1/capabilities` through the edge (step 6) and `docker compose ps` are what an
operator has.

### 7. Swap in a real certificate (operator leg)

> **STOP — this step does not work as it was previously written, and doing it breaks the stack.**
> Measured 2026-07-29 against a live production-overlay bring-up
> (`docs/operations/cloud-smoke-report.md`, Bulgu 3). Read this whole section before touching a
> certificate.

`${PALAI_HOME}/ca/server.crt` + `server.key` are **not the edge's certificate. They are the stack's
one server identity, shared by two services with incompatible requirements**:

| Consumer | Mounted at | Requires |
|---|---|---|
| edge (Caddy) | `production.yml:70-71` → `/etc/palai/edge/edge.{crt,key}` | a cert for **your domain** |
| control-plane runner gateway `:8443` | `compose.yaml:61-62` → `PALAI_RUNNER_SERVER_CERT` | **exactly one** DNS SAN, equal to `control-plane` |

The runner pins that identity exactly — `len(leaf.DNSNames) != 1 || leaf.DNSNames[0] != ControllerDNS`
(`packages/runner/enrollment.go:142`, and the same check in `session.go:302` and `renewal.go:101`).
So **every certificate that satisfies the edge fails the runner**:

- a cert for `palai.example.com` → `x509: certificate is valid for palai.example.com, not control-plane`;
- a cert carrying **both** SANs → `controller certificate DNS identity is not exact` (two SANs is not one).

The failure is also **delayed, which is what makes it dangerous**: the running control-plane holds the
old cert in memory, so right after the swap the runner still shows `sessions: 1` and everything looks
fine. It breaks at the **next control-plane restart** — which `restart: always` guarantees on the next
VM reboot. The symptom then is that **runs queue forever and never complete**, with nothing in the API
response mentioning a certificate.

(The previously documented `up -d edge` did not reload the cert either — compose sees no config change
and leaves the container alone, so the edge kept serving the old certificate while the new one sat on
disk. `restart edge` is what reloads it. This was masking the breakage above, not avoiding it.)

**What to do today.** Leave `${PALAI_HOME}/ca/server.{crt,key}` alone and terminate your real domain
**in front of** the Palai edge — a cloud load balancer, or your own nginx/Caddy — forwarding to the
edge and trusting `${PALAI_HOME}/ca/ca.crt` upstream. The edge's Caddyfile sets `auto_https off` with an
explicit `tls` pair, so it serves its certificate for any SNI and does not care what hostname the front
proxy used. This is also the normal shape of a cloud deployment (ACME terminates at the LB).

**The gap.** Giving the edge its own identity is a small overlay change — a `PALAI_EDGE_CERT` /
`PALAI_EDGE_KEY` pair defaulting to `${PALAI_HOME}/ca/server.{crt,key}`, so the runner keeps its exact
pin and the operator can point the edge somewhere else. It is **not** done, it is deliberately not being
done here (it touches a shipped production posture with its own guard tests), and until it is, a
single-certificate real-domain edge is not supported. Filed in
`docs/operations/cloud-smoke-report.md`, Bulgu 3.

### Teardown

```sh
docker compose --env-file production.env -p "$PALAI_COMPOSE_PROJECT" \
  -f "${PALAI_HOME}/compose/compose.yaml" -f "${PALAI_HOME}/compose/production.yml" down
```

(keeps volumes)

## Route B — from a checkout (development)

This is the **development** route: it builds the images from source, so it needs the repo, the
Dockerfiles and the engine sources. Everything above applies unchanged except that the compose files
come from the working tree rather than from `${PALAI_HOME}/compose`, and nothing is materialised.

```sh
go build -o palai ./cmd/cli

export PALAI_HOME=/srv/palai
./palai init                    # writes no compose/ dir: the committed tree wins

cp deploy/compose/production.env.example production.env
# edit it, then:
set -a; . ./production.env; set +a

docker compose --env-file production.env \
  -f deploy/compose/compose.yaml -f deploy/compose/production.yml config >/dev/null && echo OK

docker compose --env-file production.env -p "$PALAI_COMPOSE_PROJECT" \
  -f deploy/compose/compose.yaml -f deploy/compose/production.yml up -d --build --wait
```

`PALAI_COMPOSE_FILE` overrides the resolution entirely and still wins over both routes — it is how
the e2e harness and the UAT point the CLI at a specific file.

Which route a `palai` binary takes is decided by whether `deploy/compose/compose.yaml` exists
relative to its working directory: inside a checkout, route B; anywhere else, route A. The
outside-the-tree behaviour is proven by `cmd/cli/packaged_test.go`, whose cases run the built binary
from a directory with no `deploy/` at any level above it.
