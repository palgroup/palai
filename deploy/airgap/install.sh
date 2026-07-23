#!/usr/bin/env bash
# E15 T4 — air-gap install: load the bundle's OCI images, MIRROR them into a private
# digest-pinned registry (registry:2 on 127.0.0.1), and bring the packaged stack up FROM
# that mirror onto an `internal: true` network where egress is topologically impossible.
#
# Run verify.sh FIRST — this script assumes the bundle's signature + digest chain already
# checked out. The registry mirror is the operator's distribution point: images are pushed
# to it BY DIGEST, the local build tags are removed, and the stack is pulled back FROM the
# mirror by that same digest — so the running images are provably the mirror's, not a cache.
#
# The 127.0.0.1 mirror is insecure-by-default in Docker (loopback), so no TLS ceremony here;
# a real operator fronts the mirror with their own registry + trust root (plan §6).
#
# Env (required): PALAI_HOME (an initialised .palai dir), PALAI_AIRGAP_PROJECT (compose project),
#   PALAI_AIRGAP_REGISTRY_PORT (host port for the mirror). Optional: VERSION (0.1.0).
# Usage: install.sh <bundle-dir>
set -euo pipefail

bundle="${1:?usage: install.sh <bundle-dir>}"
bundle="$(cd "$bundle" && pwd)"
: "${PALAI_HOME:?PALAI_HOME must point at an initialised .palai dir (run: palai init)}"
: "${PALAI_AIRGAP_PROJECT:?PALAI_AIRGAP_PROJECT (compose project name) must be set}"
: "${PALAI_AIRGAP_REGISTRY_PORT:?PALAI_AIRGAP_REGISTRY_PORT (mirror host port) must be set}"
VERSION="${VERSION:-0.1.0}"
mirror="127.0.0.1:${PALAI_AIRGAP_REGISTRY_PORT}"
reg_ctr="${PALAI_AIRGAP_PROJECT}-registry"
compose_root="$bundle/compose"

step() { echo "airgap-install: $*" >&2; }

# manifest_field <image-name> <id|ref> — read a per-image field from manifest.json (host python3).
manifest_field() {
	python3 - "$bundle/manifest.json" "$1" "$2" <<'PY'
import json, sys
m = json.load(open(sys.argv[1]))
for img in m["images"]:
    if img["name"] == sys.argv[2]:
        print(img[sys.argv[3]]); break
PY
}

# --- 1. load every image; verify each loaded ID matches the (signed) manifest --------------
step "docker load images from the bundle"
for f in "$bundle"/images/*.tar; do docker load -q -i "$f" >/dev/null; done

for name in control-plane runner reference-engine postgres object-store registry; do
	id="$(manifest_field "$name" id)"
	[ -n "$id" ] || { echo "install: manifest has no image $name" >&2; exit 1; }
	docker image inspect "$id" >/dev/null 2>&1 || { echo "install: loaded image $name ($id) not present — bundle/manifest mismatch" >&2; exit 1; }
	# Retag the loaded content (by its signed config ID) into the mirror namespace.
	docker tag "$id" "$mirror/palai/$name:$VERSION"
done

# --- 2. stand up the private registry mirror -----------------------------------------------
step "start the private registry mirror ($mirror)"
docker rm -f "$reg_ctr" >/dev/null 2>&1 || true
docker run -d --name "$reg_ctr" -p "127.0.0.1:${PALAI_AIRGAP_REGISTRY_PORT}:5000" \
	"$mirror/palai/registry:$VERSION" >/dev/null
for _ in $(seq 1 30); do
	if curl -fsS "http://$mirror/v2/" >/dev/null 2>&1; then break; fi
	sleep 1
done
curl -fsS "http://$mirror/v2/" >/dev/null || { echo "install: registry mirror did not come up" >&2; exit 1; }

# --- 3. push each stack image to the mirror BY DIGEST; re-pull FROM the mirror --------------
# bash-3.2 safe (no associative arrays): each pinned digest lands in digest_<sanitized-name>.
for name in control-plane runner reference-engine postgres object-store; do
	step "push $name to the mirror, re-pull by digest"
	docker push "$mirror/palai/$name:$VERSION" >/dev/null
	# Pick the RepoDigest for THIS mirror, not `index 0` — a reused image accumulates repo
	# digests from earlier mirror ports, and index 0 can be a stale one pointing at a dead port.
	rd="$(docker inspect --format '{{range .RepoDigests}}{{println .}}{{end}}' "$mirror/palai/$name:$VERSION" | grep "^$mirror/palai/$name@" | head -1)"
	[ -n "$rd" ] || { echo "install: no $mirror digest for $name after push" >&2; exit 1; }
	# Drop the local tag so the ref is carried by the mirror digest, then pull it back.
	docker rmi "$mirror/palai/$name:$VERSION" >/dev/null 2>&1 || true
	docker pull -q "$rd" >/dev/null
	got="$(docker image inspect "$rd" --format '{{.Id}}')"
	want="$(manifest_field "$name" id)"
	[ "$got" = "$want" ] || { echo "install: mirror digest mismatch for $name ($got != $want)" >&2; exit 1; }
	eval "digest_$(printf '%s' "$name" | tr '-' '_')=\$rd"
done

# --- 4. runner enrollment token (one-use; the in-stack runner presents it) ------------------
if [ ! -s "$PALAI_HOME/runner-token" ]; then
	step "mint a one-use runner enrollment token"
	openssl rand -hex 24 > "$PALAI_HOME/runner-token"
	chmod 600 "$PALAI_HOME/runner-token"
fi

# --- 5. bring the stack up FROM the mirror on an internal:true network ----------------------
step "compose up (from the mirror, internal network, --no-build)"
# Ports are reset by airgap.yml, but the base file still interpolates them — read init's values.
eval "$(python3 - "$PALAI_HOME/config.json" <<'PY'
import json, sys
c = json.load(open(sys.argv[1]))
for k in ("api_port", "runner_port", "pg_port", "s3_port"):
    print(f'{k.upper()}={c[k]}')
PY
)"
export PALAI_HOME
export PALAI_API_PORT="$API_PORT" PALAI_RUNNER_PORT="$RUNNER_PORT" PALAI_PG_PORT="$PG_PORT" PALAI_S3_PORT="$S3_PORT"
export PALAI_COMPOSE_PROJECT="$PALAI_AIRGAP_PROJECT"
# shellcheck disable=SC2154  # digest_* are assigned dynamically above via `eval "digest_$name=..."`
export PALAI_CP_IMAGE="$digest_control_plane"
export PALAI_RUNNER_IMAGE="$digest_runner"
export PALAI_PG_IMAGE="$digest_postgres"
export PALAI_S3_IMAGE="$digest_object_store"
# The engine sandbox is launched by the runner via the host socket (not a compose service); it
# must be a locally-present ref. Use the mirror digest we just pulled.
export PALAI_ENGINE_IMAGE="$digest_reference_engine"

docker compose -p "$PALAI_AIRGAP_PROJECT" \
	-f "$compose_root/compose.yaml" -f "$bundle/airgap.yml" \
	up -d --no-build --wait postgres object-store control-plane runner >&2

step "stack is up on the internal network (project $PALAI_AIRGAP_PROJECT); engine=$PALAI_ENGINE_IMAGE"
