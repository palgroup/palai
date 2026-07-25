#!/usr/bin/env bash
# scripts/release/publish.sh — E18 T5. THE PUBLICATION GATE: the act release-policy.md's "Two-person
# promotion" section governs, as opposed to the local evidence gate scripts/release/promote.sh has always
# been. The difference is one rule: a publication ALWAYS presents an approval, and this script refuses
# without one.
#
#   "Stable and release-candidate promotion is a two-person operation. One maintainer starts the protected
#    release workflow and a different authorized maintainer reviews the release index, conformance
#    evidence, dependency and vulnerability results, then approves the protected environment. The builder
#    cannot bypass this gate, including as a repository administrator."
#
# TWO HALVES, and this script is the second one. GitHub's protected environment (required reviewers,
# prevent-self-review, no admin bypass) is the first; it is configuration, and configuration drifts. So the
# same rule is re-judged here MECHANICALLY against the canonical maintainer set in .github/CODEOWNERS
# (uat.ApprovalGate, unit-pinned by tests/uat/approval_test.go). A misconfigured environment cannot pass a
# single-person promotion through this script.
#
# IT DELEGATES, it does not re-derive: scripts/release/promote.sh runs the WHOLE existing chain — the
# release artifacts verified offline when PALAI_RELEASE_DIR names them (SEC-101's promotion arm), the
# evidence bundle's receipts and checksums re-verified, the release-family exit proofs, and then the
# two-person approval. The tag rehearsal and the registry dry runs are scripts/release/publish-dryrun.sh,
# a separate step so each is runnable and testable on its own.
#
# HONEST CEILING — this script has never run in CI (plan §T5, §6 leg 1). There is no protected environment,
# no second maintainer and no OIDC workflow identity. In THIS repository .github/CODEOWNERS names one
# maintainer, so the gate below refuses every approval, which is release-policy.md's own sentence holding
# mechanically: "Until two maintainers and a protected release environment exist, Palai may publish
# development snapshots but must not publish an RC or stable release."
#
# Env:
#   PALAI_RELEASE_VERSION    the version being published                      (required)
#   PALAI_RELEASE_EVIDENCE   the evidence bundle under evidence/releases/      (required)
#   PALAI_RELEASE_TARGET     rc | stable                                       (default rc)
#   PALAI_RELEASE_APPROVAL   a palai.release-approval/v1 record. When unset and this is a GitHub run with
#                            a token, it is ASSEMBLED from the run's own protected-deployment approvals.
#   PALAI_RELEASE_DIR        the built + attested release directory, verified offline before the gate
#   PALAI_RELEASE_PUBKEY     the OUT-OF-BAND trust root for it
set -euo pipefail

root="$(git rev-parse --show-toplevel)"
cd "$root"

step() { echo "publish: $*" >&2; }
fail() { echo "publish: REFUSED: $*" >&2; exit 1; }

version="${PALAI_RELEASE_VERSION:?publish: PALAI_RELEASE_VERSION is required}"
release="${PALAI_RELEASE_EVIDENCE:?publish: PALAI_RELEASE_EVIDENCE (the evidence bundle) is required}"
target="${PALAI_RELEASE_TARGET:-rc}"
approval="${PALAI_RELEASE_APPROVAL:-}"

# --- who approved this run -------------------------------------------------------------------------
# On a real GitHub run the approval is not something the job can assert about itself: it is asked of the
# API, which knows who actually approved the protected deployment. UNTESTED LOCALLY (there is no run to
# ask — plan §6 leg 1); it fails CLOSED, because an approval that cannot be fetched is not an approval.
if [ -z "$approval" ] && [ -n "${GH_TOKEN:-}" ] && [ -n "${GITHUB_RUN_ID:-}" ]; then
	step "assembling the approval record from run $GITHUB_RUN_ID's protected-environment approvals"
	tmp="$(mktemp -d)"
	trap 'rm -rf "$tmp"' EXIT
	gh api "repos/${GITHUB_REPOSITORY}/actions/runs/${GITHUB_RUN_ID}/approvals" > "$tmp/approvals.json" ||
		fail "could not read this run's approvals from the API — an approval that cannot be verified is not an approval"
	RELEASE="$release" TARGET="$target" BUILDER="${GITHUB_TRIGGERING_ACTOR:-${GITHUB_ACTOR:-}}" \
		RUN_URL="${GITHUB_SERVER_URL:-https://github.com}/${GITHUB_REPOSITORY}/actions/runs/${GITHUB_RUN_ID}" \
		python3 - "$tmp/approvals.json" "$tmp/approval.json" <<'PY'
import json, os, sys

raw = json.load(open(sys.argv[1], encoding="utf-8"))
approvers = []
for entry in raw if isinstance(raw, list) else []:
    # Only an ACTUAL approval of the protected release environment counts; a rejection or a pending
    # review is not one, and neither is an approval of some other environment in the same run.
    if entry.get("state") != "approved":
        continue
    if not any(e.get("name") == "release" for e in entry.get("environments") or []):
        continue
    login = ((entry.get("user") or {}).get("login") or "").strip()
    if login and login not in approvers:
        approvers.append(login)

json.dump({
    "schema": "palai.release-approval/v1",
    "release": os.environ["RELEASE"],
    "target": os.environ["TARGET"],
    "environment": "release",
    "workflow_run": os.environ["RUN_URL"],
    "builder": os.environ["BUILDER"],
    "approvers": approvers,
    # The API reports who approved; it cannot report that nobody was allowed to skip the review. An
    # environment configured WITHOUT required reviewers yields an empty approver list, which the gate
    # refuses as a single-person promotion — the same outcome an admin bypass must have.
    "admin_bypass": False,
}, open(sys.argv[2], "w", encoding="utf-8"), indent=2)
PY
	approval="$tmp/approval.json"
fi

if [ -z "$approval" ]; then
	fail "no two-person approval was presented (PALAI_RELEASE_APPROVAL). A publication is a two-person
	 operation: one maintainer starts the protected release workflow and a DIFFERENT authorized maintainer
	 approves it (release-policy.md). scripts/release/promote.sh without an approval is the evidence gate
	 only — it does not publish anything."
fi

# --- the gate ----------------------------------------------------------------------------------------
step "gating the $target publication of $version: artifacts (when named) -> evidence bundle $release -> two-person approval"
PALAI_RELEASE_APPROVAL="$approval" RELEASE="$release" scripts/release/promote.sh "$target"

step "PASS: $version may be tagged as $target — the evidence bundle verifies clean, the release-family exit"
step "proofs are present, and an authorized maintainer OTHER than the builder approved it in the protected"
step "environment. NOTHING has been published: scripts/release/publish-dryrun.sh rehearses the tags and the"
step "registry uploads, and a real publication is plan §6 leg 2."
