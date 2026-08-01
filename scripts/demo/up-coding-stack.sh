#!/usr/bin/env bash
# Bring up a Palai stack that can CLONE A REPOSITORY, RUN A SHELL, AND PROPOSE A PULL REQUEST —
# and grant the tools that make those reachable, which is the half a bring-up does not do for you.
#
# WHY THIS FILE EXISTS RATHER THAN A PARAGRAPH IN A DOC. Every value below was measured against a
# live stack on 2026-08-01, and three of them are things a careful operator gets wrong because the
# tree's own naming points the other way. They are marked MEASURED where they appear.
#
# ---------------------------------------------------------------------------------------------
# WHAT IT NEEDS FROM YOU, AND WHAT IT CANNOT DO FOR YOU
# ---------------------------------------------------------------------------------------------
#
# It cannot create your GitHub App. That is the one step with no automation, and burying it is how
# an operator discovers at the end that the PR half was never going to work. Do this FIRST:
#
#   1. github.com/settings/apps -> "New GitHub App". Any name, any homepage URL.
#      UNCHECK "Active" under Webhook — nothing here consumes webhooks.
#   2. Repository permissions — set exactly these:
#          Contents        Read and write
#          Pull requests   Read and write
#          Checks          Read and write     (only if you want check runs)
#      MEASURED (adapters/repositories/broker_github.go permissionsForScope): the control plane
#      asks for `contents:write` to push, and `pull_requests:write` + `contents:read` to open a PR.
#      GitHub only ever grants a SUBSET of what the installation already holds, so under-granting
#      here yields a token quietly missing the permission rather than an error you can read.
#   3. "Where can this GitHub App be installed?" -> Only on this account. Create.
#   4. Copy the App ID from the App page.
#   5. "Generate a private key" -> downloads a .pem. Save it OUTSIDE this repository
#      (e.g. ~/.palai-secrets/github-app.pem, chmod 600). `palai up` copies its BYTES into the
#      0600 $PALAI_HOME secrets slot and the control plane only ever reads a PATH, so the key
#      appears in no `docker inspect`, no `compose config`, no argv and no log.
#   6. "Install App" -> pick ONE repository. The browser URL then ends
#      /settings/installations/<NUMBER> — that number is the installation id.
#
# Then set these in the env file you pass as $ENV_FILE (default ./.env.local):
#
#   OPENAI_API_KEY=...                      the live provider credential
#   PALAI_WORKSPACE_ROOT=/abs/path          where workspaces are allocated (absolute, not /tmp)
#   PALAI_SHELL_NATIVE=unsandboxed-host     READ THE WARNING BELOW BEFORE SETTING THIS
#   PALAI_GITHUB_APP_ID=...                 from step 4
#   PALAI_GITHUB_APP_INSTALLATION_ID=...    from step 6
#   PALAI_GITHUB_APP_PRIVATE_KEY_FILE=...   from step 5 (a path, outside the tree)
#   PALAI_GIT_REPO=owner/repo               MEASURED: without it a push publishes but every pull
#                                           request answers "no pull-request client wired"
#                                           (cmd/cli/internal/stack/up.go:524-528)
#   PALAI_GIT_CLONE_URL=https://github.com/owner/repo.git
#   PALAI_GIT_BASE_BRANCH=main
#
# ALL THREE APP VARIABLES OR NONE. up.go:491 refuses a half-configuration, because a nil publisher
# makes the approval pump a no-op: you would grant the publish tools, watch the agent propose a
# push, press Approve, and NOTHING would happen — no error, no log line, an approved row sitting in
# the database forever with a human believing they authorised something.
#
# ---------------------------------------------------------------------------------------------
# THE PRICE OF THE NATIVE POSTURE, STATED PLAINLY
# ---------------------------------------------------------------------------------------------
#
# MEASURED: `grep -c PALAI_WORKSPACE_ROOT deploy/compose/compose.yaml` -> 0. The containerised
# compose file sets no workspace root on any service, and apps/control-plane/api/capabilities.go
# gates the whole `workspaces` capability on that one variable. So `palai up --native` is the ONLY
# shipped posture in which a coding run works at all.
#
# And the native posture means PALAI_SHELL_NATIVE=unsandboxed-host, whose own declaration reads:
# "commands run as this uid with no container boundary, no network denial and no resource bound;
# different customers MUST use different Macs". PALAI_SANDBOX_IMAGE and PALAI_SHELL_NATIVE are
# mutually exclusive. Use a throwaway repository and a machine you are willing to hand to a model.
#
# ---------------------------------------------------------------------------------------------
# Usage:
#   scripts/demo/up-coding-stack.sh [--env-file PATH] [--home PATH] [--project-id prj_local]
#
# It is IDEMPOTENT: re-running re-applies the grants and re-reports, and `palai init` is a no-op on
# an initialised home.
set -euo pipefail

ENV_FILE="./.env.local"
PROJECT_ID="prj_local"
PALAI_HOME_DIR="${PALAI_HOME:-./.palai}"

while [ $# -gt 0 ]; do
  case "$1" in
    --env-file)   ENV_FILE="$2"; shift 2 ;;
    --home)       PALAI_HOME_DIR="$2"; shift 2 ;;
    --project-id) PROJECT_ID="$2"; shift 2 ;;
    -h|--help)    sed -n '1,80p' "$0"; exit 0 ;;
    *) echo "unknown argument: $1" >&2; exit 2 ;;
  esac
done

if [ ! -f "$ENV_FILE" ]; then
  echo "no env file at $ENV_FILE — see the header of this script for what it must contain" >&2
  exit 1
fi

# The env file is SOURCED, never echoed, and `set -x` is never enabled: it carries the provider
# credential and the path to a GitHub App private key.
set -a
# shellcheck disable=SC1090
. "$ENV_FILE"
set +a

export PALAI_HOME="$PALAI_HOME_DIR"

require() {
  # shellcheck disable=SC2086
  if [ -z "${!1:-}" ]; then
    echo "MISSING: $1 — $2" >&2
    return 1
  fi
}

echo "==> checking what this env file actually carries"
missing=0
require PALAI_WORKSPACE_ROOT "the host directory workspaces are allocated under; its presence is what turns the workspaces capability on" || missing=1
require PALAI_SHELL_NATIVE   "must be exactly 'unsandboxed-host'; without it the control plane wires NO shell runner and every shell call refuses" || missing=1
[ "$missing" -eq 0 ] || exit 1

if [ "${PALAI_SHELL_NATIVE}" != "unsandboxed-host" ]; then
  echo "PALAI_SHELL_NATIVE=${PALAI_SHELL_NATIVE} is not the exact string the control plane accepts" >&2
  echo "it is a sentence rather than a 1 because it DELETES the sandbox boundary" >&2
  exit 1
fi

# The App is optional for clone+shell+commit and REQUIRED for push/pull_request. Say which stack
# this is going to be, up front, rather than letting it be discovered at the approval step.
if [ -n "${PALAI_GITHUB_APP_ID:-}" ] && [ -n "${PALAI_GITHUB_APP_INSTALLATION_ID:-}" ] && [ -n "${PALAI_GITHUB_APP_PRIVATE_KEY_FILE:-}" ]; then
  echo "    GitHub App: configured (push and pull_request can publish)"
  [ -n "${PALAI_GIT_REPO:-}" ] || echo "    WARNING: PALAI_GIT_REPO is unset — a push will publish, every pull request will answer 'no pull-request client wired'"
  [ -r "${PALAI_GITHUB_APP_PRIVATE_KEY_FILE}" ] || { echo "    ERROR: cannot read ${PALAI_GITHUB_APP_PRIVATE_KEY_FILE}" >&2; exit 1; }
else
  echo "    GitHub App: NOT configured — clone, shell and commit will work; push and pull_request will NOT publish"
fi

echo "==> palai init (no-op if this home is already initialised)"
palai init

# CLEARING THE GRANT BEFORE THE BRING-UP, AND THIS IS NOT TIDINESS — IT IS THE ONLY WAY THIS SCRIPT
# CAN BE RUN TWICE.
#
# MEASURED 2026-08-01, the hard way: `palai up`'s step [5/6] runs one real single-step proof run.
# That run has NO repository and therefore NO workspace. Once `default_tools` carries the workspace
# tools, the model is offered them on that proof run too, it calls one
#   palai.workspace.file {"op":"write","path":"response.txt","content":"ok"}
# and file.go:45 answers `no workspace bound for this run` — a Go ERROR rather than a tool result.
# A tool error aborts the attempt (tool_dispatch.go:249-251 returns it; orchestrator.go:726 calls
# abortIfTerminal), the ledger row is left unresolved, recovery escalates it to manual_resolution,
# and nothing ever resolves it. The run sits `running` forever and the bring-up reports
#   NOT PROVEN: response ... was still "in_progress" after 3m0s
# on a stack that is in fact completely healthy.
#
# So the grant and the bring-up proof cannot both hold at once. The script owns the ordering: clear,
# bring up, re-grant. A first run on a fresh stack has nothing to clear and this is a no-op.
if [ -f "${PALAI_HOME}/config.json" ]; then
  PRIOR_URL="$(python3 -c "import json;print(json.load(open('${PALAI_HOME}/config.json'))['base_url'])" 2>/dev/null || true)"
  PRIOR_KEY="$(cat "${PALAI_HOME}/api-key" 2>/dev/null || true)"
  if [ -n "$PRIOR_URL" ] && [ -n "$PRIOR_KEY" ] && curl -fsS -m 5 -o /dev/null -H "Authorization: Bearer ${PRIOR_KEY}" "${PRIOR_URL}/v1/capabilities" 2>/dev/null; then
    echo "==> clearing project default_tools so the bring-up's own proof run can complete"
    curl -fsS -X PATCH \
      -H "Authorization: Bearer ${PRIOR_KEY}" -H "Content-Type: application/json" \
      -d '{"config_policy":{"default_tools":[]}}' \
      "${PRIOR_URL}/v1/projects/${PROJECT_ID}" > /dev/null || true
  fi
fi


echo "==> palai up --native"
# THE PROOF STEP CAN FAIL FOR A REASON THAT IS NOT A BROKEN STACK, so its exit code is captured rather
# than trusted, and this script then PROVES LIVENESS ITSELF below.
#
# The clear above is a fast path and it only fires when the API is already reachable — on a COLD start
# there is nothing to clear it through, the grant is still in the database from last time, and step [5/6]
# wedges exactly as described. Measured both ways on 2026-08-01: warm start -> proof PASSES (46 in / 1 out);
# cold start -> `NOT PROVEN ... still "in_progress" after 3m0s` on a stack whose /v1/capabilities answers
# 200 the moment it is asked.
#
# Tolerating a non-zero exit is only honest if something else checks the same thing, so nothing below is
# skipped: the capability probe, the runner-boundary probe and the grant all run, and a stack that is
# genuinely down fails at the probe with the bring-up's own output already on screen.
set +e
palai up --env-file "$ENV_FILE" --native
UP_STATUS=$?
set -e
if [ $UP_STATUS -ne 0 ]; then
  echo "    NOTE: \`palai up\` exited $UP_STATUS. If that was step [5/6] NOT PROVEN, it is the known"
  echo "          workspace-tool wedge on a project that already grants them, not a broken stack —"
  echo "          the probes below decide which it was."
fi

BASE_URL="$(python3 -c "import json;print(json.load(open('${PALAI_HOME}/config.json'))['base_url'])")"
API_KEY="$(cat "${PALAI_HOME}/api-key")"

echo "==> confirming the workspaces capability is actually ON (not merely configured)"
CAPS="$(curl -fsS -H "Authorization: Bearer ${API_KEY}" "${BASE_URL}/v1/capabilities")"
echo "$CAPS" | python3 -c "import sys,json;c=json.load(sys.stdin)['capabilities'];print('    workspaces =',c['workspaces']);sys.exit(0 if c['workspaces']=='available' else 1)"

# THE ROUND TRIP THIS SCRIPT OWNS, replacing the one it just tolerated failing.
#
# It runs in the ONE window where it cannot hit the wedge: the API is up (so the grant can be cleared
# through it, which a cold start could not do earlier) and the workspace tools are NOT yet granted, so the
# model has none to call and no tool can error. The grant goes on afterwards.
echo "==> clearing project default_tools (now that the API is reachable) and proving a live round trip"
curl -fsS -X PATCH -H "Authorization: Bearer ${API_KEY}" -H "Content-Type: application/json" \
  -d '{"config_policy":{"default_tools":[]}}' "${BASE_URL}/v1/projects/${PROJECT_ID}" > /dev/null

PROOF_ID="$(curl -fsS -X POST -H "Authorization: Bearer ${API_KEY}" -H "Content-Type: application/json" \
  -H "Idempotency-Key: up-proof-$(date +%s)-$$" \
  -d '{"input":"Reply with exactly: PALAI-UP-OK"}' "${BASE_URL}/v1/responses" \
  | python3 -c "import sys,json;print(json.load(sys.stdin)['id'])")"

PROOF_STATE=""
for _ in $(seq 1 60); do
  PROOF_JSON="$(curl -fsS -H "Authorization: Bearer ${API_KEY}" "${BASE_URL}/v1/responses/${PROOF_ID}")"
  PROOF_STATE="$(printf '%s' "$PROOF_JSON" | python3 -c "import sys,json;print(json.load(sys.stdin).get('status',''))")"
  case "$PROOF_STATE" in completed|failed|cancelled|incomplete) break ;; esac
  sleep 2
done
if [ "$PROOF_STATE" != "completed" ]; then
  echo "    ROUND TRIP NOT PROVEN: ${PROOF_ID} ended '${PROOF_STATE}' — this stack is not serving" >&2
  exit 1
fi
printf '%s' "$PROOF_JSON" | python3 -c "
import sys,json
d=json.load(sys.stdin)
txt=(d.get('output') or [{}])[0].get('content','')
u=d.get('usage') or {}
print(f\"    round trip  {d['id']} -> {d['status']}  model={d.get('model')}  {u.get('input_tokens')} in / {u.get('output_tokens')} out\")
print(f\"    said        {txt!r}\")
"

echo "==> confirming the §24 trust boundary is armed on the RUNNER too, not just the control plane"
# MEASURED 2026-08-01: not one shipped compose file used to put PALAI_WORKSPACE_ROOT in a runner's
# environment block, so `workspaceUnderRoot` saw an empty root — and an empty root meant "admit any
# host path". The native overlay now carries it. This check is here because the control plane
# reporting `workspaces = available` says NOTHING about the plane that does the bind-mounting.
RUNNER="$(docker ps --filter "name=runner" --format '{{.Names}}' | head -1)"
if [ -n "$RUNNER" ]; then
  if docker inspect "$RUNNER" --format '{{range .Config.Env}}{{println .}}{{end}}' | grep -q "^PALAI_WORKSPACE_ROOT="; then
    echo "    runner ${RUNNER}: PALAI_WORKSPACE_ROOT present (boundary armed)"
  else
    echo "    WARNING runner ${RUNNER}: PALAI_WORKSPACE_ROOT ABSENT — the runner will admit ANY host path the control plane names" >&2
  fi
fi

# -------------------------------------------------------------------------------------------
# THE GRANT, WHICH IS THE HALF A BRING-UP DOES NOT DO AND THE HALF THAT SILENTLY DOES NOTHING
# -------------------------------------------------------------------------------------------
#
# MEASURED (apps/control-plane/internal/execution/config.go Resolve): the effective tool set is
#   intersect( project default_tools , pinned agent revision's tools )
# The PROJECT layer is the BASELINE. The agent revision's tool list is a capability CEILING that is
# intersected LAST — it narrows, it never grants. So an agent published with five tools against a
# project whose default_tools is empty resolves to the EMPTY SET, the model is offered no tools at
# all, and the run completes green with the model politely answering "I can't run shell commands".
# Nothing on any surface says the set was empty. That is why this grant is here and not left to a
# console click on the agent screen, which would grant nothing.
#
# NOTE: the config_policy write REPLACES the whole document (it is an assignment, not a merge), so
# a policy that also carried `approvers` or `pool` must re-send them here.
echo "==> granting the workspace + publish tools at the PROJECT layer (the baseline that actually grants)"
curl -fsS -X PATCH \
  -H "Authorization: Bearer ${API_KEY}" \
  -H "Content-Type: application/json" \
  -d '{"config_policy":{"default_tools":[
        "palai.workspace.file",
        "palai.workspace.shell",
        "palai.workspace.commit",
        "palai.publish.push",
        "palai.publish.pull_request"
      ]}}' \
  "${BASE_URL}/v1/projects/${PROJECT_ID}" \
  | python3 -c "import sys,json;p=json.load(sys.stdin);print('    default_tools =',p['config_policy']['default_tools'])"

# -------------------------------------------------------------------------------------------
# THE BINDING
# -------------------------------------------------------------------------------------------
#
# MEASURED: `allowed_operations` is NOT a permission gate. Every reference to it in the tree is
# marshal / unmarshal / store / project — there is no membership test anywhere, so writing
# ["clone"] or ["clone","push"] enforces exactly nothing. What bounds the agent is the tool list
# granted above. It is left unset here rather than written as a security-shaped string that
# enforces nothing.
if [ -n "${PALAI_GIT_CLONE_URL:-}" ] && [ -n "${PALAI_GIT_BASE_BRANCH:-}" ]; then
  echo "==> registering the repository binding"
  IDENTITY="${PALAI_GIT_REPO:-$(basename "${PALAI_GIT_CLONE_URL}" .git)}"
  curl -fsS -X POST \
    -H "Authorization: Bearer ${API_KEY}" \
    -H "Content-Type: application/json" \
    -d "$(python3 - "$IDENTITY" "$PALAI_GIT_CLONE_URL" "$PALAI_GIT_BASE_BRANCH" <<'PY'
import json,sys
print(json.dumps({"provider":"github","repository_identity":sys.argv[1],
                  "clone_url":sys.argv[2],"default_branch":sys.argv[3]}))
PY
)" \
    "${BASE_URL}/v1/repository-bindings" \
  | python3 -c "import sys,json;b=json.load(sys.stdin);print('    binding',b['id'],'->',b['repository_identity'],'@',b['default_branch'])"
else
  echo "==> no repository binding (PALAI_GIT_CLONE_URL / PALAI_GIT_BASE_BRANCH unset)"
fi

cat <<EOF

STACK READY
  api            ${BASE_URL}
  workspaces     available
  shell posture  UNSANDBOXED HOST — commands run as this uid, no container, no network denial

WHAT IS PROVEN TO WORK on this configuration (measured 2026-08-01):
  clone into an allocated workspace, palai.workspace.shell on the host, palai.workspace.file,
  palai.workspace.commit, and palai.publish.push recording a PENDING approval.

ONE STANDING CONSEQUENCE OF THE GRANT, so it is not discovered later: while default_tools carries
the workspace tools, \`palai up\` on this stack will report NOT PROVEN at step [5/6]. Its proof run
has no workspace, it calls the file tool anyway, and a tool error wedges the run. This script
clears the grant before the bring-up and restores it after; running \`palai up\` BY HAND on this
project will hang for three minutes and then lie about the stack's health.

APPROVING THAT PUBLICATION NOW WORKS FROM HTTP AND FROM THE CONSOLE, and it did not until
2026-08-01. GET /v1/approvals returns the row (with the remote and branch the write is going to) and
POST /v1/approvals/{id}/approve applies it — publication pending_approval -> approved, the durable
command -> applied, and the parked run waiting -> completed.

What it used to do, so an older stack is recognisable: the list answered {"data":[]} while the row
existed, the decide route 404'd on a publication id even with the correct hash, and an approve posted
to POST /v1/sessions/{id}/commands was accepted (202) and never applied — the only thing that drains
that queue runs inside a live attempt, and a run parked on an approval has none. It then EXPIRED and
released the run half an hour later with nobody having decided. If you see that, the control plane
predates the fix.

ONE THING THE SCREEN STILL DOES NOT SAY: which CREDENTIAL the write will be made under. The
publication row carries the destination and nothing about the identity, so an operator can see WHERE,
not AS WHOM.
EOF
