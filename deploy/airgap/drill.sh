#!/usr/bin/env bash
# E15 T4 — the air-gap LIVE proof (OPS-004, local seam). One script, the full chain:
#
#   1. build the signed bundle (scripts/release/airgap-build.sh).
#   2. OFFLINE verify in a `--network none` container — signature + digest chain (green).
#   3. TAMPER test — flip one byte in an image tar; the SAME offline verify must FAIL closed.
#   4. INSTALL — mirror the images into a private digest-pinned registry and bring the stack
#      up FROM the mirror onto an `internal: true` network (deploy/airgap/install.sh).
#   5. RUN — admit a real response over the admin key via `docker exec` (no host-published
#      port); it drives control-plane -> runner -> engine to `completed`, model = in-process
#      fake (zero model egress), engine sandbox = NetworkMode:none.
#   6. ZERO-EGRESS — assert the network is `internal: true` (topology) AND that a container on
#      it CANNOT reach the internet (an active egress attempt FAILS). Not a log line.
#   7. IN-NETWORK GIT — a git daemon fixture on the internal network is clone-able from another
#      in-network container: git works air-gapped, egress still impossible.
#
# HONEST CEILING (plan §T4/§6): the fake provider stands in for a real private model endpoint;
# a real air-gapped facility, the operator trust-root/mirror ceremony, and a real private model +
# Git server are the operator leg. SBOM/provenance production is E18.
#
# 8GB Docker Desktop: unique `palai-e15t4-<short>` names, full teardown (0 leaks) on exit.
set -euo pipefail

# Reuse already-built palai/<name>:local images (same real artifacts) rather than invoking
# BuildKit — on this 8GB Docker Desktop a concurrent build OOMs the frontend. A release/CI build
# of the bundle (scripts/release/airgap-build.sh with this unset) builds fresh from source.
export PALAI_AIRGAP_REUSE_LOCAL="${PALAI_AIRGAP_REUSE_LOCAL:-1}"

root="$(git rev-parse --show-toplevel)"
short="$(openssl rand -hex 3)"
proj="palai-e15t4-$short"
work="$(mktemp -d)"
export PALAI_HOME="$work/home"
reg_port="$(( 20000 + RANDOM % 20000 ))"
git_ctr="$proj-gitfixture"
git_client="$proj-gitclient"
git_vol="$proj-gitrepos"
tool_image="palai-e15t4-$short-tool:latest"
GIT_IMAGE="${PALAI_AIRGAP_GIT_IMAGE:-alpine/git}"

step() { echo; echo "==> $*" >&2; }
fail() { echo "AIR-GAP DRILL FAILED: $*" >&2; exit 1; }

cleanup() {
	set +e
	echo "--- cleanup ---" >&2
	docker rm -f "$git_ctr" "$git_client" "$proj-registry" >/dev/null 2>&1
	# Label-based teardown (NOT `docker compose down`): env-independent, so it works even though
	# the PALAI_*_IMAGE interpolation vars live only inside install.sh's process.
	docker ps -aq --filter "label=com.docker.compose.project=$proj" | xargs -r docker rm -f >/dev/null 2>&1
	docker ps -aq --filter "label=io.palai.project=$proj" | xargs -r docker rm -f >/dev/null 2>&1
	docker network rm "${proj}_airgap" >/dev/null 2>&1
	docker volume ls -q --filter "label=com.docker.compose.project=$proj" | xargs -r docker volume rm >/dev/null 2>&1
	docker volume rm -f "$git_vol" >/dev/null 2>&1
	# Rebuildable tags only (upstream bases stay cached).
	docker rmi -f "$tool_image" >/dev/null 2>&1
	for n in control-plane runner reference-engine postgres object-store registry; do
		docker rmi -f "palai/$n:0.1.0" >/dev/null 2>&1
	done
	rm -rf "$work"
}
trap cleanup EXIT

# ---------------------------------------------------------------------------------------------
step "1. build the signed air-gap bundle"
bundle="$(OUT="$work/bundle" bash "$root/scripts/release/airgap-build.sh")"
pub="$bundle/palai-airgap-signing.pub"   # local proof: emitted key == out-of-band key (same session)

step "2. OFFLINE verify in a --network none container (signature + digest chain)"
# Load ONLY the postgres image (safe — load runs nothing) to power the offline verify sandbox;
# the verify then re-confirms that exact postgres.tar against the signed sums.
pg_id="$(python3 -c 'import json,sys; print(next(i["id"] for i in json.load(open(sys.argv[1]))["images"] if i["name"]=="postgres"))' "$bundle/manifest.json")"
docker load -q -i "$bundle/images/postgres.tar" >/dev/null
docker tag "$pg_id" "$tool_image"
PALAI_AIRGAP_TOOL_IMAGE="$tool_image" bash "$bundle/verify.sh" --network-none "$bundle" "$pub" \
	|| fail "offline verify of a pristine bundle did not pass"
echo "OFFLINE VERIFY: green (ran with the container network fully removed)" >&2

step "3. TAMPER test — flip one byte in an image tar; offline verify must FAIL closed"
python3 - "$bundle/images/control-plane.tar" <<'PY'
import sys
p = sys.argv[1]
b = bytearray(open(p, "rb").read())
b[len(b)//2] ^= 0xff
open(p, "wb").write(b)
PY
if PALAI_AIRGAP_TOOL_IMAGE="$tool_image" bash "$bundle/verify.sh" --network-none "$bundle" "$pub" >/dev/null 2>&1; then
	fail "offline verify PASSED a tampered bundle — it must fail closed"
fi
echo "TAMPER TEST: verify correctly FAILED on a 1-byte flip" >&2
# Rebuild a pristine bundle for the install (the tamper corrupted images/control-plane.tar).
step "3b. rebuild a pristine bundle for install"
bundle="$(OUT="$work/bundle" bash "$root/scripts/release/airgap-build.sh")"
pub="$bundle/palai-airgap-signing.pub"

step "4. init + INSTALL — mirror to a private registry, stack up FROM the mirror (internal net)"
go build -o "$work/palai" "$root/cmd/cli"
"$work/palai" init >&2
PALAI_AIRGAP_PROJECT="$proj" PALAI_AIRGAP_REGISTRY_PORT="$reg_port" \
	bash "$bundle/install.sh" "$bundle"

# Resolve containers by compose label (env-independent — the PALAI_*_IMAGE interpolation vars
# live only inside install.sh's process, so a `compose ps` here would see blank images).
cp_ctr="$(docker ps -q --filter "label=com.docker.compose.project=$proj" --filter "label=com.docker.compose.service=control-plane")"
runner_ctr="$(docker ps -q --filter "label=com.docker.compose.project=$proj" --filter "label=com.docker.compose.service=runner")"
[ -n "$cp_ctr" ] && [ -n "$runner_ctr" ] || fail "control-plane/runner container not found after install"
netname="$(docker inspect -f '{{range $k,$v := .NetworkSettings.Networks}}{{$k}} {{end}}' "$cp_ctr" | awk '{print $1}')"

step "5. RUN — admit a real response over docker-exec (no host-published port)"
# Wait for the in-stack runner to enroll (outbound to the internal runner gateway).
enrolled=false
for _ in $(seq 1 60); do
	if docker logs "$runner_ctr" 2>&1 | grep -q "enrolled runner"; then enrolled=true; break; fi
	sleep 2
done
$enrolled || { docker logs "$runner_ctr" >&2; fail "runner did not enroll"; }

key="$(docker exec "$cp_ctr" cat /run/secrets/bootstrap_api_key)"
created="$(docker exec "$cp_ctr" wget -q -O- \
	--header="Authorization: Bearer $key" \
	--header="Content-Type: application/json" \
	--header="Idempotency-Key: airgap-$short" \
	--post-data='{"input":"hello from the air-gap"}' \
	http://127.0.0.1:8080/v1/responses)" || fail "response create failed (wget non-zero)"
id="$(printf '%s' "$created" | python3 -c 'import json,sys
try: print(json.load(sys.stdin).get("id",""))
except Exception: pass')"
[ -n "$id" ] || { echo "raw create response: $created" >&2; fail "response create returned no id"; }
echo "response id=$id" >&2

status=""
for _ in $(seq 1 120); do
	body="$(docker exec "$cp_ctr" wget -q -O- --header="Authorization: Bearer $key" "http://127.0.0.1:8080/v1/responses/$id")"
	status="$(printf '%s' "$body" | python3 -c 'import json,sys; print(json.load(sys.stdin).get("status",""))')"
	case "$status" in completed|failed|canceled) break ;; esac
	sleep 1
done
[ "$status" = "completed" ] || { docker logs "$runner_ctr" 2>&1 | tail -20 >&2; fail "run did not complete (status=$status)"; }
echo "RUN: response $id reached 'completed' with egress impossible" >&2

step "6. ZERO-EGRESS — topology (internal:true) AND an active egress attempt must FAIL"
internal="$(docker network inspect -f '{{.Internal}}' "$netname")"
[ "$internal" = "true" ] || fail "network $netname is NOT internal:true (topology claim broken)"
# 1.1.1.1 by IP (no DNS): on internal:true there is no gateway, so this cannot connect.
if docker exec "$cp_ctr" wget -q -T 4 -O- http://1.1.1.1/ >/dev/null 2>&1; then
	fail "control-plane reached the internet — egress is NOT impossible"
fi
echo "ZERO-EGRESS: network is internal:true and an egress attempt from the stack FAILED" >&2

step "7. IN-NETWORK GIT — a git daemon fixture is clone-able from an in-network container"
docker pull -q "$GIT_IMAGE" >/dev/null
docker volume create "$git_vol" >/dev/null
docker run --rm -v "$git_vol:/repos" --entrypoint /bin/sh "$GIT_IMAGE" -c '
	cd /repos && git init -q demo && cd demo &&
	git config user.email air@gap.local && git config user.name airgap &&
	echo "air-gapped repository" > README.md && git add -A && git commit -q -m init' \
	|| fail "seeding the git fixture failed"
docker run -d --name "$git_ctr" --network "$netname" -v "$git_vol:/repos" \
	--entrypoint git "$GIT_IMAGE" daemon --reuseaddr --export-all --base-path=/repos /repos >/dev/null
# Clone from ANOTHER in-network container (egress still impossible for both). Retry a few
# times to absorb the daemon's startup race (it was just backgrounded).
docker run --rm --name "$git_client" --network "$netname" --entrypoint /bin/sh "$GIT_IMAGE" -c "
	for i in 1 2 3 4 5 6 7 8; do
		git clone -q git://$git_ctr/demo /tmp/clone 2>/dev/null && break || sleep 1
	done
	grep -q 'air-gapped repository' /tmp/clone/README.md" \
	|| fail "in-network git clone failed"
echo "IN-NETWORK GIT: clone over git:// succeeded on the internal network" >&2

echo
echo "AIR-GAP DRILL PASSED (project $proj):"
echo "  - offline verify green in --network none; tamper -> FAIL"
echo "  - digest-pinned registry mirror install; stack up on internal:true network $netname"
echo "  - real run $id completed (fake provider in-process, engine NetworkMode:none)"
echo "  - zero egress: internal:true topology + active egress attempt FAILED"
echo "  - in-network git daemon clone succeeded"
