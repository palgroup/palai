#!/usr/bin/env bash
set -euo pipefail

root="$(git rev-parse --show-toplevel)"
cd "$root"

adr_dir="${PALAI_E01_ADR_DIR:-docs/adr}"
report_dir="${PALAI_E01_REPORT_DIR:-spikes/reports}"
index="${PALAI_E01_INDEX:-spikes/reports/index.json}"

expected_reports=(
  contract-toolchain.json
  control-plane-runtime.json
  nextjs-streaming.json
  object-store.json
  postgres-coordinator.json
  runner-supervisor.json
)
expected_adrs=(
  0001-language-runtime.md
  0002-contract-toolchain.md
  0003-runner-transport.md
  0004-local-object-store.md
  0005-build-orchestration.md
)

fail() {
  echo "E01 verification failed: $1" >&2
  exit 1
}

test -s "$index" || fail "missing report index: $index"
test -d "$report_dir" || fail "missing report directory: $report_dir"
test -d "$adr_dir" || fail "missing ADR directory: $adr_dir"

actual_reports="$(
  find "$report_dir" -maxdepth 1 -type f -name '*.json' ! -name index.json \
    -exec basename {} \; | LC_ALL=C sort
)"
expected_report_list="$(printf '%s\n' "${expected_reports[@]}" | LC_ALL=C sort)"
test "$actual_reports" = "$expected_report_list" || fail "report set differs from the six E01 reports"

actual_adrs="$(
  find "$adr_dir" -maxdepth 1 -type f -name '000[1-9]-*.md' \
    -exec basename {} \; | LC_ALL=C sort
)"
expected_adr_list="$(printf '%s\n' "${expected_adrs[@]}" | LC_ALL=C sort)"
test "$actual_adrs" = "$expected_adr_list" || fail "ADR set differs from ADR-0001..0005"

expected_index='[
  {"path":"spikes/reports/contract-toolchain.json","spike":"contract-toolchain","passed":true,"owning_adr":"ADR-0002"},
  {"path":"spikes/reports/control-plane-runtime.json","spike":"control-plane-runtime","passed":true,"owning_adr":"ADR-0001"},
  {"path":"spikes/reports/nextjs-streaming.json","spike":"nextjs-streaming","passed":true,"owning_adr":"ADR-0005"},
  {"path":"spikes/reports/object-store.json","spike":"object-store","passed":true,"owning_adr":"ADR-0004"},
  {"path":"spikes/reports/postgres-coordinator.json","spike":"postgres-coordinator","passed":true,"owning_adr":"ADR-0001"},
  {"path":"spikes/reports/runner-supervisor.json","spike":"runner-supervisor","passed":true,"owning_adr":"ADR-0003"}
]'
jq -e --argjson expected "$expected_index" '
  (keys == ["reports", "schema_version"]) and
  .schema_version == 1 and
  (.reports | type == "array" and length == 6) and
  ([.reports[].path] | length == (unique | length)) and
  ([.reports[].spike] | length == (unique | length)) and
  (all(.reports[];
    (keys == ["owning_adr", "passed", "path", "sha256", "spike"]) and
    (.sha256 | test("^[0-9a-f]{64}$")))) and
  ((.reports | map(del(.sha256)) | sort_by(.path)) == ($expected | sort_by(.path)))
' "$index" >/dev/null || fail "report index schema, ownership, or pass state is invalid"

for report in "${expected_reports[@]}"; do
  path="spikes/reports/$report"
  indexed_checksum="$(
    jq -er --arg path "$path" '.reports[] | select(.path == $path) | .sha256' "$index"
  )" || fail "report is not indexed: $path"
  actual_checksum="$(shasum -a 256 "$report_dir/$report" | awk '{print $1}')"
  test "$actual_checksum" = "$indexed_checksum" || fail "report checksum mismatch: $path"
done

PALAI_SPIKE_REPORT_DIR="$report_dir" scripts/spikes/check-reports >/dev/null || \
  fail "report content, pass state, or source provenance is invalid"

secret_pattern='(OPENAI_API_KEY|ANTHROPIC_API_KEY|ANTROPHIC_API_KEY|Authorization:[[:space:]]*Bearer|BEGIN[[:space:]]+PRIVATE[[:space:]]+KEY|(^|[^[:alnum:]])sk-[[:alnum:]]|/Users/|/home/)'
scan_files=("$index")
for report in "${expected_reports[@]}"; do
  scan_files+=("$report_dir/$report")
done
for adr in "${expected_adrs[@]}"; do
  scan_files+=("$adr_dir/$adr")
done
if LC_ALL=C grep -Eiq "$secret_pattern" "${scan_files[@]}"; then
  fail "report index, report, or ADR contains a sensitive value"
fi

check_adr() {
  local number="$1"
  local file="$2"
  shift 2
  local expected_links actual_links

  grep -q "^# ADR-$number:" "$file" || fail "ADR-$number title is invalid"
  grep -qx -- '- Status: accepted' "$file" || fail "ADR-$number is not accepted"
  grep -qx -- '- Hard-gate exceptions: none' "$file" || fail "ADR-$number has a hard-gate exception"
  grep -qx -- '- Readiness scope: E01 technology baseline only' "$file" || \
    fail "ADR-$number overstates the E01 readiness scope"
  for heading in \
    '## Context' \
    '## Evidence and options' \
    '## Decision' \
    '## Scope' \
    '## Version and digest policy' \
    '## Consequences' \
    '## Verification' \
    '## Revisit triggers'; do
    grep -qx "$heading" "$file" || fail "ADR-$number is missing $heading"
  done

  expected_links="$(printf '%s\n' "$@" | LC_ALL=C sort)"
  actual_links="$(
    { grep -Eo 'spikes/reports/[a-z0-9-]+\.json' "$file" || true; } | LC_ALL=C sort
  )"
  test "$actual_links" = "$expected_links" || fail "ADR-$number report links are not exact"
}

check_adr 0001 "$adr_dir/0001-language-runtime.md" \
  spikes/reports/control-plane-runtime.json \
  spikes/reports/postgres-coordinator.json
check_adr 0002 "$adr_dir/0002-contract-toolchain.md" \
  spikes/reports/contract-toolchain.json
check_adr 0003 "$adr_dir/0003-runner-transport.md" \
  spikes/reports/runner-supervisor.json
check_adr 0004 "$adr_dir/0004-local-object-store.md" \
  spikes/reports/object-store.json
check_adr 0005 "$adr_dir/0005-build-orchestration.md" \
  spikes/reports/contract-toolchain.json \
  spikes/reports/control-plane-runtime.json \
  spikes/reports/nextjs-streaming.json \
  spikes/reports/object-store.json \
  spikes/reports/postgres-coordinator.json \
  spikes/reports/runner-supervisor.json

echo "e01_verification=PASS reports=6 adrs=5 hard_gate_exceptions=0"
