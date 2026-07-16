#!/usr/bin/env bash
set -euo pipefail

repository="${PALAI_GITHUB_REPOSITORY:-palgroup/palai}"
branch="${PALAI_DEFAULT_BRANCH:-main}"
gh_bin="${PALAI_GH_BIN:-gh}"

metadata="$($gh_bin api "repos/$repository")"
jq -e --arg branch "$branch" '
  .visibility == "public" and .default_branch == $branch
' <<<"$metadata" >/dev/null

protection="$($gh_bin api "repos/$repository/branches/$branch/protection")"
jq -e '
  .required_status_checks.strict == true and
  (.required_status_checks.contexts | index("Foundation") != null) and
  .enforce_admins.enabled == true and
  .required_pull_request_reviews.required_approving_review_count == 0 and
  .required_linear_history.enabled == true and
  .required_conversation_resolution.enabled == true and
  .allow_force_pushes.enabled == false and
  .allow_deletions.enabled == false
' <<<"$protection" >/dev/null

echo "repository_settings=PASS"
