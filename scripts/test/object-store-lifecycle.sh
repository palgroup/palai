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
  mock_container_inspect_failures=0
  mock_container_inspect_always_fails=0
  mock_container_rm_preserves=0
  mock_container_rm_breaks_inspect=0
  mock_container_list_failures=0
  mock_volume_exists=1
  mock_volume_spike_label=object-store
  mock_volume_run_label="$run_id"
  mock_volume_rm_calls=0
  mock_volume_rm_failures=0
  mock_volume_inspect_failures=0
  mock_volume_inspect_always_fails=0
  mock_volume_rm_preserves=0
  mock_volume_rm_breaks_inspect=0
  mock_volume_list_failures=0
  mock_image_tag_exists=1
  mock_image_list_failures=0
  mock_image_rm_calls=0
  mock_image_rm_failures=0
  mock_image_rm_preserves=0
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
      if test "$mock_container_inspect_always_fails" -eq 1; then
        return 1
      fi
      if test "$mock_container_inspect_failures" -gt 0; then
        mock_container_inspect_failures=$((mock_container_inspect_failures - 1))
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
      if test "$mock_container_rm_breaks_inspect" -eq 1; then
        mock_container_inspect_always_fails=1
      fi
      if test "$mock_container_rm_preserves" -ne 1; then
        mock_container_exists=0
      fi
      ;;
    container:ls)
      if test "$mock_container_list_failures" -gt 0; then
        mock_container_list_failures=$((mock_container_list_failures - 1))
        return 1
      fi
      if test "$mock_container_exists" -eq 1; then
        echo owned-container
      fi
      ;;
    volume:inspect)
      if test "$mock_volume_inspect_always_fails" -eq 1; then
        return 1
      fi
      if test "$mock_volume_inspect_failures" -gt 0; then
        mock_volume_inspect_failures=$((mock_volume_inspect_failures - 1))
        return 1
      fi
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
      if test "$mock_volume_rm_breaks_inspect" -eq 1; then
        mock_volume_inspect_always_fails=1
      fi
      if test "$mock_volume_rm_preserves" -ne 1; then
        mock_volume_exists=0
      fi
      ;;
    volume:ls)
      if test "$mock_volume_list_failures" -gt 0; then
        mock_volume_list_failures=$((mock_volume_list_failures - 1))
        return 1
      fi
      if test "$mock_volume_exists" -eq 1; then
        echo owned-volume
      fi
      ;;
    image:ls)
      if test "$mock_image_list_failures" -gt 0; then
        mock_image_list_failures=$((mock_image_list_failures - 1))
        return 1
      fi
      if test "$mock_image_tag_exists" -eq 1; then
        echo palai-object-store-test-run:local-archive-roundtrip
      fi
      ;;
    image:rm)
      mock_image_rm_calls=$((mock_image_rm_calls + 1))
      if test "$mock_image_rm_failures" -gt 0; then
        mock_image_rm_failures=$((mock_image_rm_failures - 1))
        return 1
      fi
      if test "$mock_image_rm_preserves" -ne 1; then
        mock_image_tag_exists=0
      fi
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
mock_container_inspect_always_fails=1
if object_store_remove_current_container >/dev/null 2>&1; then
  fail "reachable-daemon inspect failure with exact container listing must remain ambiguous"
fi
assert_equal owned-container "$current_container" "pre-remove inspect ambiguity must retain container name"
assert_equal 0 "$mock_container_rm_calls" "pre-remove inspect ambiguity must not call container rm"

mock_reset
current_container=owned-container
mock_container_rm_preserves=1
mock_container_rm_breaks_inspect=1
if object_store_remove_current_container >/dev/null 2>&1; then
  fail "post-remove inspect failure with exact container listing must not confirm absence"
fi
assert_equal owned-container "$current_container" "post-remove ambiguity must retain container name"
assert_equal 1 "$mock_container_rm_calls" "container removal must stop until labels can be inspected again"

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

mock_reset
current_container=""
current_volume=owned-volume
mock_volume_inspect_always_fails=1
if object_store_remove_current_volume >/dev/null 2>&1; then
  fail "reachable-daemon inspect failure with exact volume listing must remain ambiguous"
fi
assert_equal owned-volume "$current_volume" "pre-remove inspect ambiguity must retain volume name"
assert_equal 0 "$mock_volume_rm_calls" "pre-remove inspect ambiguity must not call volume rm"

mock_reset
current_container=""
current_volume=owned-volume
mock_volume_rm_preserves=1
mock_volume_rm_breaks_inspect=1
if object_store_remove_current_volume >/dev/null 2>&1; then
  fail "post-remove inspect failure with exact volume listing must not confirm absence"
fi
assert_equal owned-volume "$current_volume" "post-remove ambiguity must retain volume name"
assert_equal 1 "$mock_volume_rm_calls" "volume removal must stop until labels can be inspected again"

archive_tag=palai-object-store-test-run:local-archive-roundtrip
mock_reset
archive_tag_owned=1
mock_image_rm_failures=1
object_store_remove_owned_archive_tag >/dev/null
assert_equal 2 "$mock_image_rm_calls" "tag removal should retry"
assert_equal 0 "$mock_image_tag_exists" "tag should be absent after verified retry"
assert_equal 0 "$archive_tag_owned" "verified tag absence should clear ownership tracking"

mock_reset
archive_tag_owned=1
mock_image_list_failures=3
if object_store_remove_owned_archive_tag >/dev/null 2>&1; then
  fail "tag cleanup must fail closed when exact image listing is unavailable"
fi
assert_equal 1 "$archive_tag_owned" "unknown pre-remove tag state must retain ownership tracking"
assert_equal 0 "$mock_image_rm_calls" "unknown pre-remove tag state must not call image rm"

mock_reset
archive_tag_owned=1
mock_image_rm_preserves=1
if object_store_remove_owned_archive_tag >/dev/null 2>&1; then
  fail "tag cleanup must fail when exact post-remove listing still contains the tag"
fi
assert_equal 1 "$archive_tag_owned" "failed post-remove verification must retain tag ownership"
assert_equal 3 "$mock_image_rm_calls" "tag cleanup should retry a still-present exact tag"

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
