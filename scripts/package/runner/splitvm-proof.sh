#!/usr/bin/env bash
# E14 T5 — split-VM local proof (Docker-network seam). It proves the runner extracted from the
# SIGNED host package enrolls OUTBOUND-ONLY from OUTSIDE the stack's Docker network and runs a
# REAL run:
#
#   1. palai init (fresh temp PALAI_HOME) + mint a one-use runner token.
#   2. Build the control-plane + reference-engine images.
#   3. Bring up postgres + object-store + control-plane WITHOUT any in-stack runner, with the
#      runner gateway published (splitvm.yml), dispatch on, fake provider.
#   4. Build + VERIFY the signed runner package; extract the runner from it.
#   5. Run the packaged runner as a container on a SEPARATE Docker network, dialing the gateway
#      via the Docker host gateway — outbound-only, mounting the host Docker socket to supervise
#      the engine sandbox.
#   6. Create a response over the API and poll it to `completed` — only the EXTERNAL packaged
#      runner could have run it (there is no in-stack runner).
#
# Operator ceiling (plan §6): a real `systemctl enable --now`, boot persistence, and two physical
# VMs. Here the split-VM network is the Docker-network seam; the runner is the packaged binary.
set -euo pipefail

root="$(git rev-parse --show-toplevel)"
arch="$(docker version --format '{{.Server.Arch}}')"
run_id="splitvm-$(date +%s)"
work="$(mktemp -d)"
export PALAI_HOME="$work/home"
project="palai-$run_id"
net="palai-$run_id-net"
runner_ctr="palai-$run_id-runner"
palai="$work/palai"
pkgout="$work/pkg"
extract="$work/extract"

cleanup() {
	set +e
	echo "--- cleanup ---" >&2
	docker rm -f "$runner_ctr" >/dev/null 2>&1
	docker compose -p "$project" -f "$root/deploy/compose/compose.yaml" -f "$root/scripts/package/runner/splitvm.yml" down -v --remove-orphans >/dev/null 2>&1
	docker ps -aq --filter "label=io.palai.project=$project" | xargs -r docker rm -f >/dev/null 2>&1
	docker network rm "$net" >/dev/null 2>&1
	rm -rf "$work"
}
trap cleanup EXIT

step() { echo "==> $*" >&2; }

step "build palai CLI"
( cd "$root" && go build -o "$palai" ./cmd/cli )

step "palai init (PALAI_HOME=$PALAI_HOME)"
"$palai" init >&2

# The control-plane validates the enrollment token against this file; the external runner
# presents the same one-use token. No in-stack runner consumes it first.
openssl rand -hex 24 > "$PALAI_HOME/runner-token"
chmod 600 "$PALAI_HOME/runner-token"

# Resolve the ports/project palai init minted.
eval "$(python3 - "$PALAI_HOME/config.json" <<'PY'
import json, sys
c = json.load(open(sys.argv[1]))
for k in ("api_port", "runner_port", "pg_port", "s3_port"):
    print(f'{k.upper()}={c[k]}')
PY
)"

step "build control-plane + reference-engine images"
docker build -q -t palai/control-plane:local -f "$root/deploy/compose/control-plane.Dockerfile" "$root" >&2
docker build -q -t palai/reference-engine:local "$root/engines/reference" >&2
engine_digest="$(docker image inspect palai/reference-engine:local --format '{{.Id}}')"

step "bring up stack WITHOUT an in-stack runner (gateway published, dispatch on, fake provider)"
export PALAI_API_PORT="$API_PORT" PALAI_RUNNER_PORT="$RUNNER_PORT" PALAI_PG_PORT="$PG_PORT" PALAI_S3_PORT="$S3_PORT"
export PALAI_ENGINE_IMAGE="$engine_digest" PALAI_COMPOSE_PROJECT="$project"
export PALAI_DISPATCH_WORKERS=1 PALAI_MODEL_PROVIDER=fake
docker compose -p "$project" \
	-f "$root/deploy/compose/compose.yaml" \
	-f "$root/scripts/package/runner/splitvm.yml" \
	up -d --wait postgres object-store control-plane >&2

step "build + VERIFY the signed runner host package"
OUT="$pkgout" ARCH="$arch" bash "$root/scripts/package/runner/build.sh" >/dev/null
tarball="$(cd "$pkgout" && ls palai-runner-host-*.tar.gz)"
( cd "$pkgout" && ./verify.sh "$tarball" ) >&2
mkdir -p "$extract"
tar -xzf "$pkgout/$tarball" -C "$extract"

step "run the PACKAGED runner on a SEPARATE network ($net), outbound-only to the published gateway"
docker network create "$net" >/dev/null
docker run -d --name "$runner_ctr" --network "$net" \
	--add-host host.docker.internal:host-gateway \
	-v /var/run/docker.sock:/var/run/docker.sock \
	-v "$extract:/opt/palai-runner:ro" \
	-v "$PALAI_HOME/ca/ca.crt:/palai/ca.crt:ro" \
	-v "$PALAI_HOME/runner-token:/palai/runner-token:ro" \
	-e PALAI_CONTROLLER_URL="https://host.docker.internal:$RUNNER_PORT" \
	-e PALAI_CONTROLLER_DNS=control-plane \
	-e PALAI_RUNNER_ID=runner-splitvm \
	-e PALAI_RUNNER_DNS=runner-splitvm.runners.palai.internal \
	-e PALAI_RUNNER_CA_CERT=/palai/ca.crt \
	-e PALAI_ENROLLMENT_TOKEN_FILE=/palai/runner-token \
	-e PALAI_ENGINE_IMAGE="$engine_digest" \
	--entrypoint /opt/palai-runner/palai-runner.sh \
	alpine:3.21 >/dev/null

step "wait for the packaged runner to enroll (outbound)"
enrolled=false
for _ in $(seq 1 60); do
	if docker logs "$runner_ctr" 2>&1 | grep -q "enrolled runner"; then
		enrolled=true
		break
	fi
	if [ "$(docker inspect -f '{{.State.Running}}' "$runner_ctr" 2>/dev/null)" != "true" ]; then
		echo "runner container exited early:" >&2
		docker logs "$runner_ctr" >&2 || true
		exit 1
	fi
	sleep 2
done
$enrolled || { echo "runner did not enroll in time:" >&2; docker logs "$runner_ctr" >&2; exit 1; }
docker logs "$runner_ctr" 2>&1 | grep "enrolled runner" >&2

step "create a response over the API and await terminal"
base="http://127.0.0.1:$API_PORT"
key="$(cat "$PALAI_HOME/api-key")"
created="$("$palai" response create --input "hello from split-vm")"
id="$(printf '%s\n' "$created" | python3 -c 'import json,sys; print(json.loads(sys.stdin.read().strip().splitlines()[-1])["id"])')"
echo "response id=$id" >&2

status=""
for _ in $(seq 1 120); do
	body="$(curl -sS "$base/v1/responses/$id" -H "Authorization: Bearer $key")"
	status="$(printf '%s' "$body" | python3 -c 'import json,sys; print(json.load(sys.stdin).get("status",""))')"
	case "$status" in
		completed | failed | canceled) break ;;
	esac
	sleep 1
done

echo "==> terminal status: $status" >&2
if [ "$status" != "completed" ]; then
	echo "SPLIT-VM PROOF FAILED: response did not complete (status=$status)" >&2
	docker logs "$runner_ctr" 2>&1 | tail -30 >&2
	exit 1
fi

# Positive evidence the EXTERNAL packaged runner did the work: it received the lease and
# supervised a real engine to completion (the sandbox itself is reaped on completion, so a
# post-hoc container count would read 0 — the runner log is the durable proof).
step "runner lease/engine evidence"
if ! docker logs "$runner_ctr" 2>&1 | grep -E "received lease|engine completed for run" >&2; then
	echo "SPLIT-VM PROOF FAILED: no lease/engine activity in the packaged runner's log" >&2
	exit 1
fi

echo "SPLIT-VM PROOF PASSED: the signed-package runner enrolled outbound-only from network $net, received the lease, supervised the engine, and completed response $id"
