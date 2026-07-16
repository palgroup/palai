#!/usr/bin/env bash
set -euo pipefail

root="$(git rev-parse --show-toplevel)"
verifier="$root/scripts/verify/repository-settings.sh"
fake_gh="$root/scripts/test/fixtures/fake-gh"

output="$(PALAI_GH_BIN="$fake_gh" bash "$verifier")"
test "$output" = 'repository_settings=PASS'

for mode in private missing-check force-push no-pull-request nonlinear; do
  if PALAI_FAKE_REPOSITORY_MODE="$mode" PALAI_GH_BIN="$fake_gh" bash "$verifier" >/dev/null 2>&1; then
    echo "repository settings verifier accepted $mode" >&2
    exit 1
  fi
done
