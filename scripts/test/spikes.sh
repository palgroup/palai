#!/usr/bin/env bash
set -euo pipefail

root="$(git rev-parse --show-toplevel)"
cd "$root"

test -x scripts/spikes/run
test -x scripts/spikes/check-reports
test -s spikes/manifest.json

quick_output="$(scripts/spikes/run quick)"
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

if evidence_output="$(scripts/spikes/run evidence 2>&1)"; then
  echo "evidence profile accepted planned spikes" >&2
  exit 1
fi
grep -q 'spike_not_implemented' <<<"$evidence_output"
grep -q 'planned=6' <<<"$evidence_output"

scripts/spikes/check-reports

tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT
mkdir -p "$tmp/reports"
implemented_manifest="$tmp/implemented.json"
jq --arg quick "$root/scripts/test/fixtures/spike-pass" \
  --arg evidence "$root/scripts/test/fixtures/spike-pass" '
  .spikes[0].status = "implemented" |
  .spikes[0].quick_command = $quick |
  .spikes[0].evidence_command = $evidence
' spikes/manifest.json >"$implemented_manifest"
cp spikes/internal/report/testdata/valid.json "$tmp/reports/control-plane-runtime.json"

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
cp spikes/internal/report/testdata/valid.json "$tmp/reports/control-plane-runtime.json"

jq '.assertions[0].passed = false | .passed = false' \
  spikes/internal/report/testdata/valid.json >"$tmp/reports/control-plane-runtime.json"
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
if PALAI_SPIKE_REPORT_DIR="$tmp/reports" scripts/spikes/check-reports >/dev/null 2>&1; then
  echo "report checker accepted a report for a planned spike" >&2
  exit 1
fi
rm "$tmp/reports/postgres-coordinator.json"

for mutation in duplicate-id invalid-status; do
  invalid_manifest="$tmp/$mutation.json"
  case "$mutation" in
    duplicate-id) jq '.spikes[1].id = .spikes[0].id' spikes/manifest.json >"$invalid_manifest" ;;
    invalid-status) jq '.spikes[0].status = "unknown"' spikes/manifest.json >"$invalid_manifest" ;;
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
  spikes/manifest.json >"$missing_command_manifest"
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
' spikes/manifest.json >"$failing_command_manifest"
if PALAI_SPIKE_MANIFEST="$failing_command_manifest" \
  PALAI_SPIKE_REPORT_DIR="$tmp/reports" \
  scripts/spikes/run quick >/dev/null 2>&1; then
  echo "spike runner accepted a failing command" >&2
  exit 1
fi

for target in test-spikes evidence-spikes check-spike-reports; do
  make -n "$target" >/dev/null
done
