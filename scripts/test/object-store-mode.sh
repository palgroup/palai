#!/usr/bin/env bash
set -euo pipefail

root="$(git rev-parse --show-toplevel)"
cd "$root"

tmp="$(mktemp -d "${TMPDIR:-/tmp}/palai-object-store-mode.XXXXXXXX")"
trap 'rm -rf "$tmp"' EXIT
report="$tmp/spikes/reports/object-store.json"

if output="$(PALAI_SPIKE_OBJECT_STORE_REPETITIONS=1 \
  scripts/spikes/object-store evidence "$report" 2>&1)"; then
  echo "one-run evidence mode unexpectedly succeeded" >&2
  exit 1
fi
grep -q 'object-store evidence requires exactly 5 repetitions' <<<"$output"
test ! -e "$report"

if output="$(scripts/spikes/object-store diagnostic "$report" 2>&1)"; then
  echo "diagnostic mode unexpectedly accepted a report path" >&2
  exit 1
fi
grep -q 'object-store diagnostic mode does not write reports' <<<"$output"
test ! -e "$report"

echo "object_store_mode=PASS"
