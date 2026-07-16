#!/usr/bin/env bash
set -euo pipefail

root="$(git rev-parse --show-toplevel)"
cd "$root"

verifier="$root/scripts/verify/e01.sh"
test -x "$verifier" || {
  echo "missing E01 verifier: scripts/verify/e01.sh" >&2
  exit 1
}

tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT

fixture="$tmp/fixture"

reset_fixture() {
  rm -rf "$fixture"
  mkdir -p "$fixture/adrs" "$fixture/reports"
  cp docs/adr/000{1..5}-*.md "$fixture/adrs/"
  cp spikes/reports/*.json "$fixture/reports/"
  cp spikes/reports/index.json "$fixture/index.json"
}

verify_fixture() {
  PALAI_E01_ADR_DIR="$fixture/adrs" \
    PALAI_E01_REPORT_DIR="$fixture/reports" \
    PALAI_E01_INDEX="$fixture/index.json" \
    "$verifier"
}

expect_rejection() {
  local name="$1"
  if verify_fixture >"$tmp/$name.out" 2>&1; then
    echo "E01 verifier accepted $name fixture" >&2
    exit 1
  fi
}

expect_traversal_rejection() {
  if PALAI_E01_ADR_DIR="$fixture/adrs" \
    PALAI_E01_REPORT_DIR="$fixture/reports" \
    PALAI_E01_INDEX="$fixture/reports/../index.json" \
    "$verifier" >"$tmp/traversal.out" 2>&1; then
    echo "E01 verifier accepted a traversal-bearing input path" >&2
    exit 1
  fi
}

refresh_index_checksum() {
  local report="$1"
  local path="spikes/reports/$report"
  local checksum
  checksum="$(shasum -a 256 "$fixture/reports/$report" | awk '{print $1}')"
  jq --arg path "$path" --arg checksum "$checksum" '
    (.reports[] | select(.path == $path)).sha256 = $checksum
  ' "$fixture/index.json" >"$fixture/index.next"
  mv "$fixture/index.next" "$fixture/index.json"
}

reset_fixture
verify_fixture >/dev/null

reset_fixture
printf '%s\n' '- Status: proposed' >>"$fixture/adrs/0001-language-runtime.md"
expect_rejection duplicate-status-field

reset_fixture
printf '%s\n' '- Hard-gate exceptions: contract-loss' >>"$fixture/adrs/0001-language-runtime.md"
expect_rejection duplicate-hard-gate-field

reset_fixture
printf '%s\n' '- Production readiness: established' >>"$fixture/adrs/0001-language-runtime.md"
expect_rejection contradictory-production-readiness-field

reset_fixture
sed 's/^- Production readiness: not established$/- Production readiness: established/' \
  "$fixture/adrs/0001-language-runtime.md" >"$fixture/adr.next"
mv "$fixture/adr.next" "$fixture/adrs/0001-language-runtime.md"
expect_rejection production-readiness-overclaim

reset_fixture
rm "$fixture/index.json"
expect_rejection missing-index

reset_fixture
mv "$fixture/index.json" "$fixture/index-target.json"
ln -s "$fixture/index-target.json" "$fixture/index.json"
expect_rejection symlink-index

reset_fixture
rm "$fixture/reports/control-plane-runtime.json"
expect_rejection missing-report

reset_fixture
mv "$fixture/reports/control-plane-runtime.json" "$fixture/control-plane-runtime.data"
ln -s "$fixture/control-plane-runtime.data" "$fixture/reports/control-plane-runtime.json"
expect_rejection symlink-report

reset_fixture
mv "$fixture/adrs/0001-language-runtime.md" "$fixture/language-runtime.data"
ln -s "$fixture/language-runtime.data" "$fixture/adrs/0001-language-runtime.md"
expect_rejection symlink-adr

reset_fixture
ln -s "$fixture/reports/control-plane-runtime.json" "$fixture/reports/unreferenced.json"
expect_rejection unreferenced-symlink-report

reset_fixture
ln -s "$fixture/adrs/0001-language-runtime.md" "$fixture/adrs/0006-unreferenced.md"
expect_rejection unreferenced-symlink-adr

reset_fixture
expect_traversal_rejection

reset_fixture
jq '.assertions[0].passed = false | .passed = false' \
  "$fixture/reports/control-plane-runtime.json" >"$fixture/report.next"
mv "$fixture/report.next" "$fixture/reports/control-plane-runtime.json"
refresh_index_checksum control-plane-runtime.json
expect_rejection failed-report

reset_fixture
printf '\n' >>"$fixture/reports/control-plane-runtime.json"
expect_rejection tampered-report

reset_fixture
jq --arg commit "$(git rev-parse HEAD)" '.git_commit = $commit' \
  "$fixture/reports/control-plane-runtime.json" >"$fixture/report.next"
mv "$fixture/report.next" "$fixture/reports/control-plane-runtime.json"
refresh_index_checksum control-plane-runtime.json
expect_rejection wrong-commit-report

reset_fixture
jq '.assertions[0].detail = "PALAI_SPIKE_OBJECT_STORE_SECRET_KEY=fixture-secret"' \
  "$fixture/reports/control-plane-runtime.json" >"$fixture/report.next"
mv "$fixture/report.next" "$fixture/reports/control-plane-runtime.json"
refresh_index_checksum control-plane-runtime.json
expect_rejection palai-secret-assignment-report

reset_fixture
jq '.assertions[0].detail = "MY_PROJECT_API_KEY=fixture-secret"' \
  "$fixture/reports/control-plane-runtime.json" >"$fixture/report.next"
mv "$fixture/report.next" "$fixture/reports/control-plane-runtime.json"
refresh_index_checksum control-plane-runtime.json
expect_rejection project-secret-assignment-report

reset_fixture
printf '\nPALAI_SPIKE_OBJECT_STORE_SECRET_KEY=fixture-secret\n' >>"$fixture/adrs/0001-language-runtime.md"
expect_rejection palai-secret-assignment-adr

reset_fixture
printf '\nMY_PROJECT_API_KEY=fixture-secret\n' >>"$fixture/adrs/0001-language-runtime.md"
expect_rejection project-secret-assignment-adr

reset_fixture
jq '.assertions[0].detail = "PALAI_SPIKE_OBJECT_STORE_SECRET_KEY and MY_PROJECT_API_KEY are variable names"' \
  "$fixture/reports/control-plane-runtime.json" >"$fixture/report.next"
mv "$fixture/report.next" "$fixture/reports/control-plane-runtime.json"
refresh_index_checksum control-plane-runtime.json
printf '\nPALAI_SPIKE_OBJECT_STORE_SECRET_KEY and MY_PROJECT_API_KEY are discussed without values.\n' \
  >>"$fixture/adrs/0001-language-runtime.md"
verify_fixture >/dev/null || {
  echo "E01 verifier rejected benign credential variable-name discussion" >&2
  exit 1
}

reset_fixture
cp "$fixture/reports/control-plane-runtime.json" "$fixture/reports/unreferenced.json"
expect_rejection unreferenced-report

reset_fixture
sed '/spikes\/reports\/postgres-coordinator.json/d' \
  "$fixture/adrs/0001-language-runtime.md" >"$fixture/adr.next"
mv "$fixture/adr.next" "$fixture/adrs/0001-language-runtime.md"
expect_rejection unreferenced-adr

reset_fixture
rm "$fixture/adrs/0005-build-orchestration.md"
expect_rejection missing-adr

reset_fixture
sed 's/^- Status: accepted$/- Status: proposed/' \
  "$fixture/adrs/0001-language-runtime.md" >"$fixture/adr.next"
mv "$fixture/adr.next" "$fixture/adrs/0001-language-runtime.md"
expect_rejection unaccepted-adr

reset_fixture
sed 's/^- Hard-gate exceptions: none$/- Hard-gate exceptions: contract-loss/' \
  "$fixture/adrs/0001-language-runtime.md" >"$fixture/adr.next"
mv "$fixture/adr.next" "$fixture/adrs/0001-language-runtime.md"
expect_rejection hard-gate-exception

echo "e01_fixture_tests=PASS"
