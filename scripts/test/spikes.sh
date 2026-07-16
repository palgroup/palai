#!/usr/bin/env bash
set -euo pipefail

root="$(git rev-parse --show-toplevel)"
cd "$root"

test -x scripts/spikes/run
test -x scripts/spikes/check-reports
test -s spikes/manifest.json

tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT
mkdir -p "$tmp/reports"
all_planned_manifest="$tmp/all-planned.json"
jq '
  .spikes |= map(
    .status = "planned" |
    .quick_command = null |
    .evidence_command = null
  )
' spikes/manifest.json >"$all_planned_manifest"

quick_output="$(
  PALAI_SPIKE_MANIFEST="$all_planned_manifest" \
    PALAI_SPIKE_REPORT_DIR="$tmp/reports" \
    scripts/spikes/run quick
)"
for spike in \
  control-plane-runtime \
  postgres-coordinator \
  contract-toolchain \
  runner-supervisor \
  nextjs-streaming \
  object-store; do
  grep -qx "spike=$spike status=planned" <<<"$quick_output"
done
grep -qx 'spikes_summary profile=quick implemented=0 planned=6 failed=0' <<<"$quick_output"

if evidence_output="$(
  PALAI_SPIKE_MANIFEST="$all_planned_manifest" \
    PALAI_SPIKE_REPORT_DIR="$tmp/reports" \
    scripts/spikes/run evidence 2>&1
)"; then
  echo "evidence profile accepted planned spikes" >&2
  exit 1
fi
grep -q 'spike_not_implemented' <<<"$evidence_output"
grep -q 'planned=6' <<<"$evidence_output"

scripts/spikes/check-reports

implemented_manifest="$tmp/implemented.json"
jq --arg quick "$root/scripts/test/fixtures/spike-pass" \
  --arg evidence "$root/scripts/test/fixtures/spike-pass" '
  .spikes[0].status = "implemented" |
  .spikes[0].quick_command = $quick |
  .spikes[0].evidence_command = $evidence
' "$all_planned_manifest" >"$implemented_manifest"
commit="$(git rev-parse HEAD)"
source_tree="$(git rev-parse HEAD^{tree})"
jq --arg commit "$commit" --arg source_tree "$source_tree" '
  .git_commit = $commit |
  .source_tree = $source_tree
' spikes/internal/report/testdata/valid.json >"$tmp/reports/control-plane-runtime.json"

PALAI_SPIKE_MANIFEST="$implemented_manifest" \
  PALAI_SPIKE_REPORT_DIR="$tmp/reports" \
  scripts/spikes/check-reports >/dev/null
implemented_output="$(
  PALAI_SPIKE_MANIFEST="$implemented_manifest" \
    PALAI_SPIKE_REPORT_DIR="$tmp/reports" \
    scripts/spikes/run quick
)"
grep -q 'fixture_spike=PASS' <<<"$implemented_output"
grep -qx 'spikes_summary profile=quick implemented=1 planned=5 failed=0' <<<"$implemented_output"

printf '{}\n' >"$tmp/reports/control-plane-runtime.json"
if PALAI_SPIKE_MANIFEST="$implemented_manifest" \
  PALAI_SPIKE_REPORT_DIR="$tmp/reports" \
  scripts/spikes/check-reports >/dev/null 2>&1; then
  echo "report checker accepted invalid report JSON" >&2
  exit 1
fi
jq --arg commit "$commit" --arg source_tree "$source_tree" '
  .git_commit = $commit |
  .source_tree = $source_tree
' spikes/internal/report/testdata/valid.json >"$tmp/reports/control-plane-runtime.json"

jq '.source_tree = "0000000000000000000000000000000000000000"' \
  "$tmp/reports/control-plane-runtime.json" >"$tmp/reports/unbound.json"
mv "$tmp/reports/unbound.json" "$tmp/reports/control-plane-runtime.json"
if PALAI_SPIKE_MANIFEST="$implemented_manifest" \
  PALAI_SPIKE_REPORT_DIR="$tmp/reports" \
  scripts/spikes/check-reports >/dev/null 2>&1; then
  echo "report checker accepted a source tree outside current history" >&2
  exit 1
fi

parent_tree="$(git rev-parse HEAD^1^{tree})"
jq --arg commit "$commit" --arg source_tree "$parent_tree" '
  .git_commit = $commit |
  .source_tree = $source_tree
' spikes/internal/report/testdata/valid.json >"$tmp/reports/control-plane-runtime.json"
if PALAI_SPIKE_MANIFEST="$implemented_manifest" \
  PALAI_SPIKE_REPORT_DIR="$tmp/reports" \
  scripts/spikes/check-reports >/dev/null 2>&1; then
  echo "report checker accepted a mismatched commit and source tree" >&2
  exit 1
fi

jq --arg commit "$commit" --arg source_tree "$source_tree" '
  .git_commit = $commit |
  .source_tree = $source_tree |
  .spike = "postgres-coordinator"
' spikes/internal/report/testdata/valid.json >"$tmp/reports/control-plane-runtime.json"
if PALAI_SPIKE_MANIFEST="$implemented_manifest" \
  PALAI_SPIKE_REPORT_DIR="$tmp/reports" \
  scripts/spikes/check-reports >/dev/null 2>&1; then
  echo "report checker accepted a report for the wrong spike" >&2
  exit 1
fi

jq '.assertions[0].passed = false | .passed = false' \
  spikes/internal/report/testdata/valid.json |
  jq --arg commit "$commit" --arg source_tree "$source_tree" '
    .git_commit = $commit |
    .source_tree = $source_tree
  ' >"$tmp/reports/control-plane-runtime.json"
if PALAI_SPIKE_MANIFEST="$implemented_manifest" \
  PALAI_SPIKE_REPORT_DIR="$tmp/reports" \
  scripts/spikes/check-reports >/dev/null 2>&1; then
  echo "report checker accepted a failed report" >&2
  exit 1
fi

rm "$tmp/reports/control-plane-runtime.json"
if PALAI_SPIKE_MANIFEST="$implemented_manifest" \
  PALAI_SPIKE_REPORT_DIR="$tmp/reports" \
  scripts/spikes/check-reports >/dev/null 2>&1; then
  echo "report checker accepted a missing implemented report" >&2
  exit 1
fi

printf '{}\n' >"$tmp/reports/postgres-coordinator.json"
if PALAI_SPIKE_MANIFEST="$all_planned_manifest" \
  PALAI_SPIKE_REPORT_DIR="$tmp/reports" \
  scripts/spikes/check-reports >/dev/null 2>&1; then
  echo "report checker accepted a report for a planned spike" >&2
  exit 1
fi
rm "$tmp/reports/postgres-coordinator.json"

for mutation in duplicate-id invalid-status; do
  invalid_manifest="$tmp/$mutation.json"
  case "$mutation" in
    duplicate-id) jq '.spikes[1].id = .spikes[0].id' "$all_planned_manifest" >"$invalid_manifest" ;;
    invalid-status) jq '.spikes[0].status = "unknown"' "$all_planned_manifest" >"$invalid_manifest" ;;
  esac
  if PALAI_SPIKE_MANIFEST="$invalid_manifest" \
    PALAI_SPIKE_REPORT_DIR="$tmp/reports" \
    scripts/spikes/check-reports >/dev/null 2>&1; then
    echo "report checker accepted $mutation" >&2
    exit 1
  fi
done

missing_command_manifest="$tmp/missing-command.json"
jq '.spikes[0].status = "implemented" | .spikes[0].quick_command = "/missing" | .spikes[0].evidence_command = "/missing"' \
  "$all_planned_manifest" >"$missing_command_manifest"
cp spikes/internal/report/testdata/valid.json "$tmp/reports/control-plane-runtime.json"
if PALAI_SPIKE_MANIFEST="$missing_command_manifest" \
  PALAI_SPIKE_REPORT_DIR="$tmp/reports" \
  scripts/spikes/run quick >/dev/null 2>&1; then
  echo "spike runner accepted a missing command" >&2
  exit 1
fi

failing_command_manifest="$tmp/failing-command.json"
jq --arg command "$root/scripts/test/fixtures/spike-fail" '
  .spikes[0].status = "implemented" |
  .spikes[0].quick_command = $command |
  .spikes[0].evidence_command = $command
' "$all_planned_manifest" >"$failing_command_manifest"
if PALAI_SPIKE_MANIFEST="$failing_command_manifest" \
  PALAI_SPIKE_REPORT_DIR="$tmp/reports" \
  scripts/spikes/run quick >/dev/null 2>&1; then
  echo "spike runner accepted a failing command" >&2
  exit 1
fi

for target in test-spikes evidence-spikes check-spike-reports; do
  make -n "$target" >/dev/null
done
