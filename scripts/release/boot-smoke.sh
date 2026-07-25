#!/usr/bin/env bash
# scripts/release/boot-smoke.sh — E18 T1: boot one release control-plane image tar from the matrix
# and prove the container COMES UP and serves /healthz. For a foreign-arch image on this host that
# runs the binary under qemu (Docker Desktop's binfmt), which is exactly the point: the cross-
# compiled amd64 binary in the amd64 image is a real, runnable amd64 program — not just a file with
# the right ELF header.
#
# HONEST CEILING (plan §T1 / §6 leg 3): a boot-smoke is NOT a full UAT run on that architecture and
# is NOT accepted as a substitute for one. It proves boot + migration + liveness under emulation.
# The full UAT suite on real amd64 hardware is the operator leg.
#
# Usage: scripts/release/boot-smoke.sh <control-plane-image.tar> [platform]
#   e.g. scripts/release/boot-smoke.sh dist/release/images/control-plane-linux-amd64.tar linux/amd64
#
# Everything it creates (network, two containers, a temp secrets dir) is torn down on exit, including
# on failure: Docker is shared infrastructure.
set -euo pipefail

tar_path="${1:?usage: boot-smoke.sh <control-plane-image.tar> [platform]}"
platform="${2:-linux/amd64}"
arch="${platform##*/}"
case "$arch" in amd64) want_uname="x86_64";; arm64) want_uname="aarch64";; *) want_uname="";; esac

root="$(git rev-parse --show-toplevel)"
id="e18t1-smoke-$$"
net="$id-net"
tmp="$(mktemp -d)"
cp_cid=""
pg_cid=""

cleanup() {
  [ -z "$cp_cid" ] || docker rm -f "$cp_cid" >/dev/null 2>&1 || true
  [ -z "$pg_cid" ] || docker rm -f "$pg_cid" >/dev/null 2>&1 || true
  docker network rm "$net" >/dev/null 2>&1 || true
  rm -rf "$tmp"
}
trap cleanup EXIT

step() { echo "boot-smoke: $*" >&2; }
fail() { echo "boot-smoke: FAIL: $*" >&2; exit 1; }

# The image under test comes from the TAR (the shipped artifact), not from whatever the local daemon
# happens to have under that tag — a smoke test of a stale local tag proves nothing about a release.
step "loading $tar_path"
ref="$(docker load -i "$tar_path" | sed -n 's/^Loaded image: //p' | head -1)"
[ -n "$ref" ] || fail "docker load did not report a loaded image ref"
step "image ref $ref (platform $platform, digest sha256:$(sha256sum "$tar_path" | cut -d' ' -f1))"

# Postgres by the SAME digest the compose stack pins (single source of truth).
pg_ref="$(grep -oE 'postgres@sha256:[0-9a-f]+' "$root/deploy/compose/compose.yaml" | head -1)"
[ -n "$pg_ref" ] || fail "no digest-pinned postgres ref found in deploy/compose/compose.yaml"

pgpass="$(head -c 24 /dev/urandom | od -An -tx1 | tr -d ' \n')"
mkdir -p "$tmp/secrets"
printf '%s' "$pgpass" > "$tmp/secrets/pg_password"
chmod 0600 "$tmp/secrets/pg_password"

docker network create "$net" >/dev/null
step "starting $pg_ref"
pg_cid="$(docker run -d --name "$id-pg" --network "$net" --network-alias postgres \
  -e POSTGRES_USER=palai -e POSTGRES_DB=palai -e "POSTGRES_PASSWORD=$pgpass" \
  "$pg_ref")"
for _ in $(seq 1 60); do
  docker exec "$pg_cid" pg_isready -U palai -d palai >/dev/null 2>&1 && break
  sleep 1
done
docker exec "$pg_cid" pg_isready -U palai -d palai >/dev/null 2>&1 || fail "postgres never became ready"

# The image's OWN entrypoint runs (it assembles PALAI_DATABASE_URL from the file secret), so this
# exercises the shipped entrypoint too, not just the binary.
step "starting $ref under $platform"
cp_cid="$(docker run -d --name "$id-cp" --platform "$platform" --network "$net" \
  -v "$tmp/secrets:/run/secrets:ro" \
  -e PALAI_LISTEN_ADDR=":8080" \
  -p 127.0.0.1::8080 \
  "$ref")"
port="$(docker port "$cp_cid" 8080/tcp | head -1 | sed 's/.*://')"
[ -n "$port" ] || fail "no published port for the control-plane"

# Boot includes the whole migration chain, under emulation for a foreign arch — allow for that.
ok=0
for _ in $(seq 1 180); do
  if curl -fsS "http://127.0.0.1:$port/healthz" >/dev/null 2>&1; then ok=1; break; fi
  if [ -z "$(docker ps -q --filter "id=$cp_cid")" ]; then
    docker logs "$cp_cid" >&2 || true
    fail "the control-plane container EXITED before serving /healthz"
  fi
  sleep 1
done
[ "$ok" -eq 1 ] || { docker logs "$cp_cid" >&2 || true; fail "/healthz never answered"; }

body="$(curl -fsS "http://127.0.0.1:$port/healthz")"
step "GET /healthz -> $body"

# /healthz answering already IMPLIES the migration chain ran (main.go migrates at boot, then binds),
# but implication is not observation: read the E15 T1 journal head out of the database.
head="$(docker exec "$pg_cid" psql -U palai -d palai -tAc 'select max(version) from schema_revisions')"
case "$head" in
  ''|*[!0-9]*) fail "schema_revisions has no numeric head (got '$head') — the migration chain did not run";;
esac
step "schema_revisions head = $head (migration chain applied by the $arch binary)"

# Prove the foreign-arch binary really ran as that architecture (i.e. emulation was exercised) —
# otherwise a mislabelled image could pass the liveness check.
if [ -n "$want_uname" ]; then
  got="$(docker exec "$cp_cid" uname -m)"
  [ "$got" = "$want_uname" ] || fail "container reports uname -m=$got, want $want_uname for $platform"
  # Say which it was, honestly: only a foreign arch exercises emulation.
  if [ "$arch" = "$(docker info --format '{{.Architecture}}' | sed 's/x86_64/amd64/;s/aarch64/arm64/')" ]; then
    step "container uname -m = $got (NATIVE on this daemon — no emulation involved)"
  else
    step "container uname -m = $got (the $arch binary executed under qemu emulation)"
  fi
fi
img_arch="$(docker image inspect "$ref" --format '{{.Architecture}}')"
[ "$img_arch" = "$arch" ] || fail "image Architecture=$img_arch, want $arch"

step "PASS: $ref booted on $platform, ran the migration chain and served /healthz"
step "CEILING: a boot-smoke is not a full UAT run on $arch (plan §6 leg 3)"
