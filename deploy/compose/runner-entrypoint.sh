#!/bin/sh
set -eu
# Bridge the compose contract to the agent binary (cmd/runner). It does ONE thing the binary cannot:
# read the enrolment key out of its mounted FILE and hand it over in memory. The agent clears it after
# enrolling and never writes it back to disk, and re-reads the FILE (never a retained copy) if it ever
# has to recover an expired identity.
#
# ‼️ IT NO LONGER NAMES AN IDENTITY OR DERIVES A URL, and both deletions have a measured cost behind
# them. `PALAI_RUNNER_ID=runner-local` was hardcoded here, so `--scale runner=3` was three containers
# presenting ONE label — the registry now keys a machine by its device-key fingerprint, and each replica
# generates its own, so the label is the container's own hostname and three replicas are three machines.
# The enroll/session/renew addresses were derived here, in the host launcher, and in the CLI's env map —
# three copies and none in Go, which is how cmd/runner's own derivation shipped producing an `https://`
# session URL that the session refuses. The binary derives all four now, and an explicit value still
# wins, so a deployment that sets them is unchanged.
PALAI_ENROLLMENT_TOKEN="$(cat "$PALAI_ENROLLMENT_TOKEN_FILE")"
export PALAI_ENROLLMENT_TOKEN

exec /usr/local/bin/palai-runner
