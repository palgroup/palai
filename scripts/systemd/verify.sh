#!/usr/bin/env bash
# E14 T5 — run `systemd-analyze verify` on every Palai unit inside a Linux container (macOS has no
# systemd). Builds a cached verify image (Dockerfile.verify) then runs it. The real host runs
# `systemctl enable --now`; this is the static-verify half — the boot leg is the operator ceiling.
set -euo pipefail
root="$(git rev-parse --show-toplevel)"
img="palai-systemd-verify:local"
docker build -q -t "$img" -f "$root/scripts/systemd/Dockerfile.verify" "$root" >/dev/null
docker run --rm "$img"
