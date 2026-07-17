#!/usr/bin/env bash
# Shared provisioning for the runner Docker tiers (fault and security). Both cross-build
# the same fixture engine into a digest-pinned `FROM scratch` image, run their tagged
# suite with the immutable image ID exported, and require zero leaked sandbox
# containers afterward. Sourced by scripts/test/fault and scripts/test/security; not
# executed directly.

# run_runner_suite <build_tag> <test_dir> builds the fixture engine image for the Docker
# daemon architecture and runs `go test -tags=<build_tag> ./<test_dir>` against it,
# repeated COUNT times (default 1), then verifies no sandbox container leaked.
run_runner_suite() {
  local build_tag="$1"
  local test_dir="$2"
  local label="io.palai.sandbox=engine"
  local image_tag="palai-runner-engine:${build_tag}"
  local build_dir="tests/sandboxes/engine/.build"

  local daemon_os daemon_arch go_arch
  daemon_os="$(docker version --format '{{.Server.Os}}')"
  daemon_arch="$(docker version --format '{{.Server.Arch}}')"
  if test "$daemon_os" != linux; then
    echo "runner ${build_tag} suite requires a Linux Docker daemon, got $daemon_os" >&2
    exit 1
  fi
  case "$daemon_arch" in
    amd64 | x86_64) go_arch=amd64 ;;
    arm64 | aarch64) go_arch=arm64 ;;
    *)
      echo "unsupported Docker daemon architecture: $daemon_arch" >&2
      exit 1
      ;;
  esac

  count_containers() { docker ps -aq --filter "label=$label" | sed '/^$/d' | wc -l | tr -d ' '; }
  cleanup() {
    local ids
    ids="$(docker ps -aq --filter "label=$label")"
    if test -n "$ids"; then
      docker rm -f $ids >/dev/null 2>&1 || true
    fi
    docker image rm -f "$image_tag" >/dev/null 2>&1 || true
    rm -rf "$build_dir"
  }
  trap cleanup EXIT

  rm -rf "$build_dir"
  mkdir -p "$build_dir"
  CGO_ENABLED=0 GOOS=linux GOARCH="$go_arch" \
    go build -trimpath -buildvcs=false -ldflags='-s -w' \
    -o "$build_dir/palai-runner-engine" ./tests/sandboxes/engine
  docker build \
    --platform "linux/$go_arch" \
    --tag "$image_tag" \
    --file tests/sandboxes/engine/Dockerfile \
    "$build_dir" >/dev/null
  local image_id
  image_id="$(docker image inspect "$image_tag" --format '{{.Id}}')"
  if [[ ! "$image_id" =~ ^sha256:[0-9a-f]{64}$ ]]; then
    echo "Docker returned a non-immutable fixture image ID" >&2
    exit 1
  fi

  local containers_before
  containers_before="$(count_containers)"

  set +e
  PALAI_RUNNER_ENGINE_IMAGE_ID="$image_id" \
    go test -tags="$build_tag" -count="${COUNT:-1}" -v "$test_dir"
  local status=$?
  set -e

  local containers_after
  containers_after="$(count_containers)"
  cleanup
  trap - EXIT
  if test "$containers_after" -ne "$containers_before"; then
    echo "runner ${build_tag} suite leaked a sandbox container" >&2
    exit 1
  fi
  if test "$status" -ne 0; then
    exit "$status"
  fi
  echo "${build_tag}_runner=PASS containers=$containers_after"
}
