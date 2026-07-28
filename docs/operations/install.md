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
> that VM, with [step 7](#7-swap-in-a-real-certificate-operator-leg) swapping the certificate.
> This document does not claim a cloud deployment; it is verified against a local
> production-compose bring-up on Docker Desktop.

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

### 1. Initialise the data dir

```sh
export PALAI_HOME=/srv/palai            # persistent host path for this install
palai init
```

`palai init` mints, under `${PALAI_HOME}`: the local CA + edge server certificate
(`ca/ca.crt`, `ca/server.crt`, `ca/server.key`), the bootstrap API key (`api-key`), the
Postgres password (`secrets/pg-password`), a secret-store master key (`secrets/master-key`), an
empty provider secret slot — **and `compose/`**, holding `compose.yaml`, `production.yml`,
`production-entrypoint.sh` and the two `*.env.example` files the binary carries.

### 2. Regenerate the secret master key and mint the runner token

`init` mints a master key already. Regenerate it if you want one you chose yourself:

```sh
openssl rand -hex 32 > "${PALAI_HOME}/secrets/master-key"
chmod 600 "${PALAI_HOME}/secrets/master-key"
```

The boot guard refuses an unset/empty file, an all-zero key, and the placeholder
`REPLACE_WITH_OPENSSL_RAND_HEX_32`.

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
resolve that name to the edge:

```sh
curl --cacert "${PALAI_HOME}/ca/ca.crt" \
  --resolve control-plane:${PALAI_EDGE_PORT}:127.0.0.1 \
  https://control-plane:${PALAI_EDGE_PORT}/healthz
```

An authenticated call round-trips the real API through the edge (bootstrap key in
`${PALAI_HOME}/api-key`):

```sh
curl --cacert "${PALAI_HOME}/ca/ca.crt" \
  --resolve control-plane:${PALAI_EDGE_PORT}:127.0.0.1 \
  -H "Authorization: Bearer $(cat ${PALAI_HOME}/api-key)" \
  https://control-plane:${PALAI_EDGE_PORT}/v1/capabilities
```

Provisioning (org/project/api-key/secret) then goes through the `palai` admin CLI (E14 T2)
pointed at `https://control-plane:${PALAI_EDGE_PORT}` — there is no signup endpoint.

### 7. Swap in a real certificate (operator leg)

For a real domain, replace the self-minted pair the edge mounts —
`${PALAI_HOME}/ca/server.crt` and `${PALAI_HOME}/ca/server.key` — with a certificate valid for
your domain (from your CA or an ACME client), then restart the edge:

```sh
docker compose --env-file production.env -p "$PALAI_COMPOSE_PROJECT" \
  -f "${PALAI_HOME}/compose/compose.yaml" -f "${PALAI_HOME}/compose/production.yml" up -d edge
```

Clients then trust it via the public trust store (drop `--cacert`/`--resolve`). Real-domain
ACME automation on a dedicated VM is the operator leg (plan §6); the profile itself is
unchanged.

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
