# Installing the Palai production stack (single-node)

The production profile is the packaged local stack (`deploy/compose/compose.yaml`) with a
hardened posture layered on top by `deploy/compose/production.yml`:

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

## Prerequisites

- Docker Engine + Compose v2.24+ (for `!reset` and inline `configs`; verify: `docker compose version`).
- The `palai` CLI built from this repo (`go build -o palai ./cmd/cli`).
- `openssl` (to generate the master key).

## 1. Initialise the data dir

```sh
export PALAI_HOME=/srv/palai            # persistent host path for this install
PALAI_HOME=$PALAI_HOME palai init
```

`palai init` mints, under `${PALAI_HOME}`: the local CA + edge server certificate
(`ca/ca.crt`, `ca/server.crt`, `ca/server.key`), the bootstrap API key (`api-key`), the
Postgres password (`secrets/pg-password`), and an empty provider secret slot.

## 2. Generate the MANDATORY secret master key

The production control-plane will not boot without it:

```sh
openssl rand -hex 32 > "${PALAI_HOME}/secrets/master-key"
chmod 600 "${PALAI_HOME}/secrets/master-key"
```

The boot guard refuses an unset/empty file, an all-zero key, and the placeholder
`REPLACE_WITH_OPENSSL_RAND_HEX_32`.

Mint the one-use runner enrollment token as well — a hand-run compose (unlike
`palai local up`) does NOT create it, and its bind-mount source must exist as a file:

```sh
openssl rand -hex 24 > "${PALAI_HOME}/runner-token"
chmod 600 "${PALAI_HOME}/runner-token"
```

## 3. Configure the environment

```sh
cp deploy/compose/production.env.example production.env
# edit production.env: set PALAI_HOME (absolute), PALAI_EDGE_PORT (e.g. 443),
# PALAI_ENGINE_IMAGE, PALAI_COMPOSE_PROJECT.

# Load it into THIS shell so the steps below (which reference $PALAI_EDGE_PORT,
# $PALAI_COMPOSE_PROJECT) run copy-paste:
set -a; . ./production.env; set +a
```

## 4. Validate the compose overlay

```sh
docker compose --env-file production.env \
  -f deploy/compose/compose.yaml -f deploy/compose/production.yml config >/dev/null && echo OK
```

## 5. Bring the stack up

```sh
docker compose --env-file production.env -p "$PALAI_COMPOSE_PROJECT" \
  -f deploy/compose/compose.yaml -f deploy/compose/production.yml up -d --build --wait
```

## 6. Verify TLS termination through the edge

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

## 7. Swap in a real certificate (operator leg)

For a real domain, replace the self-minted pair the edge mounts —
`${PALAI_HOME}/ca/server.crt` and `${PALAI_HOME}/ca/server.key` — with a certificate valid for
your domain (from your CA or an ACME client), then restart the edge:

```sh
docker compose --env-file production.env -p "$PALAI_COMPOSE_PROJECT" \
  -f deploy/compose/compose.yaml -f deploy/compose/production.yml up -d edge
```

Clients then trust it via the public trust store (drop `--cacert`/`--resolve`). Real-domain
ACME automation on a dedicated VM is the operator leg (plan §6); the profile itself is
unchanged.

## Teardown

```sh
docker compose --env-file production.env -p "$PALAI_COMPOSE_PROJECT" \
  -f deploy/compose/compose.yaml -f deploy/compose/production.yml down    # keeps volumes
```
