#!/usr/bin/env bash
set -euo pipefail

root="$(git rev-parse --show-toplevel)"
source "$root/scripts/spikes/object-store-lifecycle"

fail() {
  echo "object-store lifecycle test failed: $1" >&2
  exit 1
}

assert_equal() {
  local want="$1"
  local got="$2"
  local message="$3"
  test "$got" = "$want" || fail "$message (want=$want got=$got)"
}

mock_reset() {
  mock_daemon_up=1
  mock_container_exists=1
  mock_container_spike_label=object-store
  mock_container_run_label="$run_id"
  mock_container_rm_calls=0
  mock_container_rm_failures=0
  mock_drop_daemon_on_container_rm=0
  mock_drop_daemon_on_container_inspect=0
  mock_volume_exists=1
  mock_volume_spike_label=object-store
  mock_volume_run_label="$run_id"
  mock_volume_rm_calls=0
  mock_volume_rm_failures=0
}

docker() {
  if test "$1" = version; then
    test "$mock_daemon_up" -eq 1 || return 1
    echo 24.0.2
    return 0
  fi

  test "$mock_daemon_up" -eq 1 || return 1
  case "$1:$2" in
    container:inspect)
      if test "$mock_drop_daemon_on_container_inspect" -eq 1; then
        mock_drop_daemon_on_container_inspect=0
        mock_daemon_up=0
        return 1
      fi
      test "$mock_container_exists" -eq 1 || return 1
      if test "${4:-}" = --format; then
        printf '%s|%s\n' "$mock_container_spike_label" "$mock_container_run_label"
      else
        echo '{}'
      fi
      ;;
    container:rm)
      mock_container_rm_calls=$((mock_container_rm_calls + 1))
      if test "$mock_drop_daemon_on_container_rm" -eq 1; then
        mock_drop_daemon_on_container_rm=0
        mock_daemon_up=0
        return 1
      fi
      if test "$mock_container_rm_failures" -gt 0; then
        mock_container_rm_failures=$((mock_container_rm_failures - 1))
        return 1
      fi
      mock_container_exists=0
      ;;
    volume:inspect)
      test "$mock_volume_exists" -eq 1 || return 1
      if test "${4:-}" = --format; then
        printf '%s|%s\n' "$mock_volume_spike_label" "$mock_volume_run_label"
      else
        echo '{}'
      fi
      ;;
    volume:rm)
      mock_volume_rm_calls=$((mock_volume_rm_calls + 1))
      if test "$mock_volume_rm_failures" -gt 0; then
        mock_volume_rm_failures=$((mock_volume_rm_failures - 1))
        return 1
      fi
      mock_volume_exists=0
      ;;
    *)
      fail "unexpected docker command: $*"
      ;;
  esac
}

run_id=test-run
cleanup_attempts=3
cleanup_retry_delay=0
current_container=""
current_volume=""

mock_reset
current_container=owned-container
mock_container_rm_failures=1
object_store_remove_current_container >/dev/null
assert_equal 2 "$mock_container_rm_calls" "container removal should retry"
assert_equal 0 "$mock_container_exists" "container should be removed after retry"
assert_equal "" "$current_container" "verified container removal should clear tracked name"

mock_reset
current_container=owned-container
mock_drop_daemon_on_container_rm=1
if object_store_remove_current_container >/dev/null 2>&1; then
  fail "daemon loss must make verified container cleanup fail"
fi
assert_equal owned-container "$current_container" "unverified container removal must retain tracked name"
assert_equal 1 "$mock_container_rm_calls" "cleanup must not retry removal while daemon is unavailable"

mock_reset
current_container=owned-container
mock_drop_daemon_on_container_inspect=1
if object_store_remove_current_container >/dev/null 2>&1; then
  fail "inspect failure during daemon loss must not be treated as verified absence"
fi
assert_equal owned-container "$current_container" "daemon-loss inspect failure must retain tracked name"
assert_equal 0 "$mock_container_rm_calls" "cleanup must not remove an uninspected container"

mock_reset
current_container=owned-container
mock_container_run_label=another-run
if object_store_remove_current_container >/dev/null 2>&1; then
  fail "mismatched ownership labels must refuse container removal"
fi
assert_equal 0 "$mock_container_rm_calls" "mismatched labels must prevent docker rm"
assert_equal owned-container "$current_container" "refused container must remain tracked"

mock_reset
current_container=""
current_volume=owned-volume
mock_volume_rm_failures=1
object_store_remove_current_volume >/dev/null
assert_equal 2 "$mock_volume_rm_calls" "volume removal should retry"
assert_equal 0 "$mock_volume_exists" "volume should be removed after retry"
assert_equal "" "$current_volume" "verified volume removal should clear tracked name"

failure_log="$(mktemp "${TMPDIR:-/tmp}/palai-object-store-lifecycle.XXXXXXXX")"
trap 'rm -f "$failure_log"' EXIT
printf '%s\n' \
  'request failed with arbitrary details' \
  '    object-store live proof failed at checksum.put_head_get: hidden details' \
  'object-store live proof failed at later.stage' >"$failure_log"
reason="$(object_store_public_failure_reason "$failure_log")"
assert_equal 'object-store live proof failed at checksum.put_head_get' "$reason" \
  "failure reason should expose only the first bounded public stage"

printf '%s\n' 'request failed with arbitrary details' >"$failure_log"
reason="$(object_store_public_failure_reason "$failure_log")"
assert_equal "" "$reason" "arbitrary failure output must not be surfaced"

echo "object_store_lifecycle=PASS"
