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

reject_traversal() {
  local path="$1"
  case "$path" in
    ..|../*|*/../*|*/..) fail "input path contains parent traversal: $path" ;;
  esac
}

require_regular_file() {
  local path="$1"
  local label="$2"
  test -f "$path" && test ! -L "$path" || fail "$label is not a regular file: $path"
}

for input_path in "$adr_dir" "$report_dir" "$index"; do
  reject_traversal "$input_path"
done

test -d "$report_dir" && test ! -L "$report_dir" || \
  fail "report directory is missing or symlinked: $report_dir"
test -d "$adr_dir" && test ! -L "$adr_dir" || \
  fail "ADR directory is missing or symlinked: $adr_dir"
require_regular_file "$index" "report index"
test -s "$index" || fail "empty report index: $index"

actual_reports="$(
  find "$report_dir" -mindepth 1 -maxdepth 1 -name '*.json' ! -name index.json \
    -exec basename {} \; | LC_ALL=C sort
)"
expected_report_list="$(printf '%s\n' "${expected_reports[@]}" | LC_ALL=C sort)"
test "$actual_reports" = "$expected_report_list" || fail "report set differs from the six E01 reports"

actual_adrs="$(
  find "$adr_dir" -mindepth 1 -maxdepth 1 \
    -name '[0-9][0-9][0-9][0-9]-*.md' ! -name '0000-template.md' \
    -exec basename {} \; | LC_ALL=C sort
)"
expected_adr_list="$(printf '%s\n' "${expected_adrs[@]}" | LC_ALL=C sort)"
test "$actual_adrs" = "$expected_adr_list" || fail "ADR set differs from ADR-0001..0005"

for report in "${expected_reports[@]}"; do
  require_regular_file "$report_dir/$report" "E01 report"
done
for adr in "${expected_adrs[@]}"; do
  require_regular_file "$adr_dir/$adr" "E01 ADR"
done

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
    (.path | test("^spikes/reports/[a-z0-9-]+\\.json$")) and
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

credential_assignment_pattern='(^|[^[:alnum:]_])"?([[:alpha:]][[:alnum:]_]*(API_?KEY|SECRET(_ACCESS)?_KEY|ACCESS_?KEY(_ID)?|TOKEN|PASSWORD|CREDENTIALS?)|DATABASE_URL)"?[[:space:]]*[:=][[:space:]]*"?[^[:space:]",}]+'
# whsec_ is the outbound-webhook signing-secret prefix (E11 Task 4, §21.5): a leaked webhook secret in
# any scanned artifact is caught here alongside sk-*, bearer tokens, and private keys.
secret_marker_pattern='(Authorization:[[:space:]]*Bearer|BEGIN[[:space:]]+PRIVATE[[:space:]]+KEY|(^|[^[:alnum:]])sk-[[:alnum:]]|(^|[^[:alnum:]])whsec_[[:alnum:]]|/Users/|/home/)'
scan_files=("$index")
for report in "${expected_reports[@]}"; do
  scan_files+=("$report_dir/$report")
done
for adr in "${expected_adrs[@]}"; do
  scan_files+=("$adr_dir/$adr")
done
if LC_ALL=C grep -Eiq "$credential_assignment_pattern|$secret_marker_pattern" "${scan_files[@]}"; then
  fail "report index, report, or ADR contains a sensitive value"
fi

require_adr_field() {
  local number="$1"
  local file="$2"
  local field="$3"
  local value="$4"
  local count

  count="$(grep -Ec "^- ${field}:" "$file" || true)"
  test "$count" -eq 1 || fail "ADR-$number must contain exactly one $field field"
  grep -Fqx -- "- $field: $value" "$file" || fail "ADR-$number has an invalid $field value"
}

check_adr() {
  local number="$1"
  local file="$2"
  shift 2
  local expected_links actual_links

  grep -q "^# ADR-$number:" "$file" || fail "ADR-$number title is invalid"
  require_adr_field "$number" "$file" Status accepted
  require_adr_field "$number" "$file" 'Hard-gate exceptions' none
  require_adr_field "$number" "$file" 'Production readiness' 'not established'
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

PALAI_SPIKE_REPORT_DIR="$report_dir" scripts/spikes/check-reports >/dev/null || \
  fail "report content, pass state, or source provenance is invalid"

echo "e01_verification=PASS reports=6 adrs=5 hard_gate_exceptions=0"
