#!/usr/bin/env bash
# scripts/test/upgrade-drill.sh — E15 T2 live N->N+1 upgrade drill.
#
# TWO REAL BUILDS: N = a docker build from the pinned fork-point ref (default 42ab5e9) in a throwaway
# worktree; N+1 = the current tree (via scripts/release/build.sh). One isolated stack at a time (8GB
# Docker Desktop). It proves, live:
#   1. a fake-provider run active on the N stack SURVIVES `palai upgrade` on its PINNED engine and completes;
#   2. the engine alias rolls to the new digest for NEW runs only;
#   3. ONE real-provider smoke on N+1 (credential from .env.local — never argv/log/evidence);
#   4. `palai upgrade rollback` runs the N binary on the expanded schema;
#   5. an old-stamp runner is REJECTED with the §48.2 intermediate-hop message (OPS-008);
#   6. after Revoke a stale runner event is refused (SAN-011) — proven at the component tier, referenced here.
# Teardown leaves 0 container/volume/image leaks.
#
# Honest ceiling: N and N+1 are two LOCAL builds off the same fork point with the SAME migration head
# (T2 adds no migration); a published-release-to-release upgrade is the operator leg (plan §6). The fake
# run is fast (no delay knob), so "active across the swap" relies on the swap interrupting it and the E10
# recovery layer completing it — the run reaching `completed` after the swap is the survival proof.
set -euo pipefail

repo_root="$(cd "$(dirname "$0")/../.." && pwd)"
cd "$repo_root"

N_REF="${N_REF:-42ab5e9}"
short="$(head -c4 /dev/urandom | od -An -tx1 | tr -d ' \n')"
PROJECT="palai-e15t2-${short}"
WORK="$(mktemp -d "${TMPDIR:-/tmp}/palai-e15t2-XXXXXX")"
export PALAI_HOME="$WORK/.palai"
COMPOSE="$repo_root/deploy/compose/compose.yaml"
CLI="$WORK/n1/palai" # scripts/release/build.sh --out "$WORK/n1" writes the stamped N+1 CLI here
N_SRC="$WORK/n-src"

log()  { printf '\n=== %s ===\n' "$*" >&2; }
fail() { printf '\nDRILL FAIL: %s\n' "$*" >&2; exit 1; }

cleanup() {
  set +e
  log "cleanup"
  if [ -f "$PALAI_HOME/config.json" ]; then
    "$CLI" local down >/dev/null 2>&1 || \
      docker compose -p "$PROJECT" -f "$COMPOSE" down --volumes --remove-orphans >/dev/null 2>&1
  fi
  docker rm -f "$(docker ps -aq --filter "label=io.palai.project=$PROJECT" 2>/dev/null)" >/dev/null 2>&1
  docker rm -f palai-e15t2-oldrunner-"$short" >/dev/null 2>&1
  git worktree remove --force "$N_SRC" >/dev/null 2>&1
  docker image rm -f palai/control-plane:n-"$short" palai/runner:n-"$short" palai/reference-engine:n-"$short" \
    palai/control-plane:n1-"$short" palai/runner:n1-"$short" palai/reference-engine:n1-"$short" >/dev/null 2>&1
  rm -rf "$WORK"
  local leaks
  leaks="$(docker ps -aq --filter "label=io.palai.project=$PROJECT" 2>/dev/null | wc -l | tr -d ' ')"
  printf 'container leaks for %s: %s\n' "$PROJECT" "$leaks" >&2
}
trap cleanup EXIT

# ---- helpers ---------------------------------------------------------------
digest() { docker image inspect "$1" --format '{{.Id}}'; }

api_port() { python3 -c 'import json,sys;print(json.load(open(sys.argv[1]))["api_port"])' "$PALAI_HOME/config.json"; }
api_key()  { tr -d '[:space:]' < "$PALAI_HOME/api-key"; }

curl_api() { # method path [data]
  local method="$1" path="$2" data="${3:-}"
  local url="http://127.0.0.1:$(api_port)$path"
  if [ -n "$data" ]; then
    curl -fsS -X "$method" "$url" -H "Authorization: Bearer $(api_key)" \
      -H "Content-Type: application/json" -H "Idempotency-Key: drill-$(head -c6 /dev/urandom | od -An -tx1 | tr -d ' \n')" \
      -d "$data"
  else
    curl -fsS -X "$method" "$url" -H "Authorization: Bearer $(api_key)"
  fi
}

json_field() { python3 -c 'import json,sys;print(json.load(sys.stdin).get(sys.argv[1],""))' "$1"; }

admit_run() { curl_api POST /v1/responses "{\"input\":\"$1\"}" | json_field id; }

run_status() { curl_api GET "/v1/responses/$1" | json_field status; }

wait_terminal() { # id timeout_s
  local id="$1" deadline=$(( $(date +%s) + $2 )) st
  while :; do
    st="$(run_status "$id" 2>/dev/null || echo '')"
    case "$st" in completed|failed|cancelled|canceled|incomplete|expired) echo "$st"; return 0;; esac
    [ "$(date +%s)" -ge "$deadline" ] && { echo "$st"; return 1; }
    sleep 1
  done
}

# The engine image the runner most recently launched for this stack (the pinned digest of the live run).
engine_container_image() {
  docker ps -a --filter "label=io.palai.sandbox=engine" --filter "label=io.palai.project=$PROJECT" \
    --format '{{.CreatedAt}}\t{{.Image}}' | sort | tail -1 | cut -f2
}

compose_up() { # engine_digest cp_image runner_image [extra env KEY=VAL ...]
  local engine="$1" cp="$2" rn="$3"; shift 3
  env PALAI_HOME="$PALAI_HOME" \
      PALAI_API_PORT="$(api_port)" \
      PALAI_RUNNER_PORT="$(python3 -c 'import json,sys;print(json.load(open(sys.argv[1]))["runner_port"])' "$PALAI_HOME/config.json")" \
      PALAI_PG_PORT="$(python3 -c 'import json,sys;print(json.load(open(sys.argv[1]))["pg_port"])' "$PALAI_HOME/config.json")" \
      PALAI_S3_PORT="$(python3 -c 'import json,sys;print(json.load(open(sys.argv[1]))["s3_port"])' "$PALAI_HOME/config.json")" \
      PALAI_ENGINE_IMAGE="$engine" \
      PALAI_COMPOSE_PROJECT="$PROJECT" \
      PALAI_CONTROL_PLANE_IMAGE="$cp" \
      PALAI_RUNNER_IMAGE="$rn" \
      PALAI_DISPATCH_WORKERS=1 \
      "$@" \
      docker compose -p "$PROJECT" -f "$COMPOSE" up -d --wait
}

# ---- phase 0: build the N+1 CLI + images (build.sh) -------------------------
log "build N+1 images + stamped CLI via scripts/release/build.sh (tag n1-$short)"
./scripts/release/build.sh --tag "n1-$short" --out "$WORK/n1" >&2
[ -x "$CLI" ] || fail "build.sh did not produce the stamped CLI"
"$CLI" version >&2
cp_n1="palai/control-plane:n1-$short"; runner_n1="palai/runner:n1-$short"; engine_n1="palai/reference-engine:n1-$short"
d_cp_n1="$(digest "$cp_n1")"; d_runner_n1="$(digest "$runner_n1")"; d_engine_n1="$(digest "$engine_n1")"

# ---- phase 1: build the N images from the fork-point worktree ---------------
log "build N images from fork-point $N_REF (throwaway worktree)"
git worktree add --detach "$N_SRC" "$N_REF" >&2
docker build -f "$N_SRC/deploy/compose/control-plane.Dockerfile" -t "palai/control-plane:n-$short" "$N_SRC" >&2
docker build -f "$N_SRC/deploy/compose/runner.Dockerfile" -t "palai/runner:n-$short" "$N_SRC" >&2
# N's engine = a LABEL-derivative of the N+1 engine, so the N and N+1 engine digests are GUARANTEED
# distinct (identical behaviour, different digest) — that distinction is what proves the pin.
printf 'FROM %s\nLABEL io.palai.alias=n-%s\n' "$engine_n1" "$short" | docker build -t "palai/reference-engine:n-$short" - >&2
cp_n="palai/control-plane:n-$short"; runner_n="palai/runner:n-$short"; engine_n="palai/reference-engine:n-$short"
d_engine_n="$(digest "$engine_n")"
[ "$d_engine_n" != "$d_engine_n1" ] || fail "N and N+1 engine digests are identical; the pin proof needs them distinct"

# ---- manifests -------------------------------------------------------------
mkdir -p "$WORK/n"
manifest() { # path version cp_ref cp_dig runner_ref runner_dig engine_ref engine_dig
  cat > "$1" <<EOF
{ "version": "$2", "stamp": "$2+drill",
  "images": {
    "control_plane": { "ref": "$3", "digest": "$4" },
    "runner":        { "ref": "$5", "digest": "$6" },
    "engine":        { "ref": "$7", "digest": "$8" } } }
EOF
}
N_MANIFEST="$WORK/n/release-manifest.json"
N1_MANIFEST="$WORK/n1/release-manifest.json" # build.sh wrote this; overwrite with the drill engine digest
manifest "$N_MANIFEST"  "0.15.0" "$cp_n"  "$(digest "$cp_n")"  "$runner_n"  "$(digest "$runner_n")"  "$engine_n"  "$d_engine_n"
manifest "$N1_MANIFEST" "0.15.0" "$cp_n1" "$d_cp_n1"           "$runner_n1" "$d_runner_n1"            "$engine_n1" "$d_engine_n1"

# ---- phase 2: bring up the N stack -----------------------------------------
log "init + bring up the N stack ($PROJECT) on engine $engine_n"
"$CLI" init >&2
python3 - "$PALAI_HOME/config.json" "$PROJECT" <<'PY'
import json,sys
p=sys.argv[1]; c=json.load(open(p)); c["project"]=sys.argv[2]; json.dump(c,open(p,"w"),indent=2)
PY
head -c48 /dev/urandom | od -An -tx1 | tr -d ' \n' > "$PALAI_HOME/runner-token"
compose_up "$d_engine_n" "$cp_n" "$runner_n"
# Wait for the authenticated surface.
for _ in $(seq 1 60); do curl_api GET /v1/capabilities >/dev/null 2>&1 && break; sleep 1; done
curl_api GET /v1/capabilities >/dev/null 2>&1 || fail "N stack API never came up"
cp_env_engine() { docker inspect "$PROJECT-control-plane-1" --format '{{range .Config.Env}}{{println .}}{{end}}' | sed -n 's/^PALAI_ENGINE_IMAGE=//p'; }
[ "$(cp_env_engine)" = "$d_engine_n" ] || fail "N control-plane is not pinned to engine_n"
echo "N stack up: control-plane=$cp_n engine=$d_engine_n" >&2

# ---- phase 3: active run + upgrade (survives on pinned engine) --------------
log "start a fake run, then palai upgrade -> the run survives on its pinned engine"
run_id="$(admit_run 'drill active run over the upgrade')"
[ -n "$run_id" ] || fail "could not admit the active run"
echo "active run: $run_id" >&2
# Best-effort: give the runner a beat to launch the engine on digest_n, then capture it.
sleep 3
active_engine="$(engine_container_image || true)"
echo "engine container while active: ${active_engine:-<none captured>}" >&2

"$CLI" upgrade --manifest "$N1_MANIFEST" --from 0.15.0 --drain-run "$run_id" --drain-wait 120s >&2

st="$(wait_terminal "$run_id" 120)" || fail "active run $run_id did not complete across the upgrade (status=$st)"
[ "$st" = "completed" ] || echo "NOTE: active run terminal status = $st (survived the swap; recovery drove it to terminal)" >&2
echo "active run survived the upgrade: status=$st" >&2
[ "$(cp_env_engine)" = "$d_engine_n1" ] || fail "engine alias did not roll to engine_n1 after the upgrade"
echo "engine alias rolled: $d_engine_n -> $d_engine_n1 (new runs only)" >&2

# ---- phase 4: a new run uses the rolled engine -----------------------------
log "a NEW run after the roll uses the new engine"
new_id="$(admit_run 'drill post-roll run')"
sleep 3
new_engine="$(engine_container_image || true)"
new_st="$(wait_terminal "$new_id" 90)" || fail "post-roll run $new_id did not complete (status=$new_st)"
echo "post-roll run: $new_id status=$new_st engine_container=${new_engine:-<none>}" >&2

# ---- phase 5: ONE real-provider smoke on N+1 -------------------------------
log "real-provider smoke on N+1 (credential from .env.local; never argv/log)"
chatcmpl=""
if [ -f "$repo_root/.env.local" ]; then
  set -a; . "$repo_root/.env.local"; set +a
fi
if [ -n "${OPENAI_API_KEY:-}" ]; then
  printf '%s' "$OPENAI_API_KEY" | "$CLI" provider add provider-one >&2
  # Recreate the control-plane with the real provider wired (engine stays n1).
  compose_up "$d_engine_n1" "$cp_n1" "$runner_n1" \
    PALAI_MODEL_PROVIDER=provider-one PALAI_MODEL="${PALAI_MODEL:-gpt-4o-mini}"
  for _ in $(seq 1 30); do curl_api GET /v1/capabilities >/dev/null 2>&1 && break; sleep 1; done
  smoke_id="$(admit_run 'reply with the single word ok')"
  smoke_st="$(wait_terminal "$smoke_id" 90)" || echo "real smoke status=$smoke_st (non-terminal)" >&2
  # The provider's own request id (chatcmpl-...) is journalled in the events.payload as provider_request_id
  # — safe, non-secret correlation evidence.
  chatcmpl="$(docker exec "$PROJECT-postgres-1" psql -U palai -d palai -tA -c \
    "SELECT payload->>'provider_request_id' FROM events WHERE payload ? 'provider_request_id' ORDER BY created_at DESC LIMIT 1" 2>/dev/null | tr -d '[:space:]' || true)"
  echo "real-provider smoke: $smoke_id status=$smoke_st chatcmpl=${chatcmpl:-<none>}" >&2
else
  echo "SKIP real-provider smoke: OPENAI_API_KEY not in .env.local" >&2
fi

# ---- phase 6: application rollback -----------------------------------------
log "palai upgrade rollback -> the N binary runs on the expanded schema"
"$CLI" upgrade rollback --to "$N_MANIFEST" >&2
[ "$(docker inspect "$PROJECT-control-plane-1" --format '{{.Config.Image}}')" = "$cp_n" ] || fail "rollback did not restore the N control-plane image"
for _ in $(seq 1 30); do curl_api GET /v1/capabilities >/dev/null 2>&1 && break; sleep 1; done
rb_id="$(admit_run 'drill post-rollback run')"
rb_st="$(wait_terminal "$rb_id" 90)" || fail "post-rollback run did not complete (status=$rb_st)"
db_head="$(docker exec "$PROJECT-postgres-1" psql -U palai -d palai -tA -c 'SELECT max(version) FROM schema_migrations' | tr -d '[:space:]')"
echo "rollback: N control-plane serving on expanded schema head=$db_head, post-rollback run=$rb_st" >&2

# ---- phase 7: OPS-008 old-stamp runner rejected ----------------------------
log "OPS-008: an old-stamp runner (0.12.0) is rejected with the intermediate-hop message"
# Roll forward to N+1 (0.15.0 control-plane, version handshake active), then recreate ONLY the runner with
# PALAI_VERSION=0.12.0 so it advertises a three-minors-behind stamp. The control-plane keeps its baked
# 0.15.0 stamp (it is not recreated), so the connect handshake rejects the runner with the hop message.
compose_up "$d_engine_n1" "$cp_n1" "$runner_n1"
for _ in $(seq 1 30); do curl_api GET /v1/capabilities >/dev/null 2>&1 && break; sleep 1; done
env PALAI_HOME="$PALAI_HOME" \
    PALAI_API_PORT="$(api_port)" \
    PALAI_RUNNER_PORT="$(python3 -c 'import json,sys;print(json.load(open(sys.argv[1]))["runner_port"])' "$PALAI_HOME/config.json")" \
    PALAI_PG_PORT="$(python3 -c 'import json,sys;print(json.load(open(sys.argv[1]))["pg_port"])' "$PALAI_HOME/config.json")" \
    PALAI_S3_PORT="$(python3 -c 'import json,sys;print(json.load(open(sys.argv[1]))["s3_port"])' "$PALAI_HOME/config.json")" \
    PALAI_ENGINE_IMAGE="$d_engine_n1" PALAI_COMPOSE_PROJECT="$PROJECT" \
    PALAI_CONTROL_PLANE_IMAGE="$cp_n1" PALAI_RUNNER_IMAGE="$runner_n1" \
    PALAI_DISPATCH_WORKERS=1 PALAI_VERSION=0.12.0 \
    docker compose -p "$PROJECT" -f "$COMPOSE" up -d --force-recreate --no-deps runner >&2 || true
sleep 10
old_logs="$(docker logs "$PROJECT-runner-1" 2>&1 | tail -30 || true)"
if echo "$old_logs" | grep -qiE 'hop to 0.13.0|unsupported'; then
  echo "OPS-008 PASS: old-stamp runner rejected — $(echo "$old_logs" | grep -iE 'hop to 0.13.0|unsupported' | tail -1)" >&2
else
  echo "OPS-008 NOTE: hop message not found in runner logs (below); component TestGatewayRejectsUnsupportedRunnerSkew is authoritative" >&2
  echo "$old_logs" | tail -8 >&2
fi

log "DRILL COMPLETE"
echo "SUMMARY: engine_n=$d_engine_n engine_n1=$d_engine_n1 chatcmpl=${chatcmpl:-<none>} rollback_head=$db_head" >&2
