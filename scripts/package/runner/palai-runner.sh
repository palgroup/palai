#!/bin/sh
# Host launcher for the Palai agent (palai-runner.service ExecStart). It does ONE thing the binary
# cannot: read the enrolment key out of its FILE and hand it over in memory, so a secret never sits in
# an environment file an operator edits.
#
# ‼️ IT NO LONGER DERIVES URLs, AND THE DELETION IS THE POINT. It used to build the enroll, session and
# renew addresses from PALAI_CONTROLLER_URL — a swap that also lived in the compose entrypoint and in the
# native runner's env map, and in NO Go code. That is how the binary's own derivation shipped producing
# `https://…/v1/runner/connect` for the session, which the session refuses: nobody noticed, because
# every deployment was handed a pre-built value by one of the three scripts. cmd/runner derives all four
# now (loadConfig, one address instead of four), and an explicit value still wins, so a deployment that
# sets them is unchanged.
#
# The agent opens NO inbound port — it dials PALAI_CONTROLLER_URL outbound only.
set -eu

: "${PALAI_CONTROLLER_URL:?PALAI_CONTROLLER_URL is required (https://controller-host:8443)}"
: "${PALAI_ENROLLMENT_TOKEN_FILE:?PALAI_ENROLLMENT_TOKEN_FILE is required (the pool enrolment key file)}"

case "$PALAI_CONTROLLER_URL" in
	https://*) : ;;
	*) echo "palai-runner: PALAI_CONTROLLER_URL must start with https:// (got: $PALAI_CONTROLLER_URL)" >&2; exit 1 ;;
esac

# The key is handed to the process in memory only; the agent clears it after enrolling and never writes
# it back to disk.
PALAI_ENROLLMENT_TOKEN="$(cat "$PALAI_ENROLLMENT_TOKEN_FILE")"
export PALAI_ENROLLMENT_TOKEN

# PALAI_CONTROLLER_CA is OPTIONAL: unset means the host's own root store verifies the gateway, which is
# the normal case for a publicly trusted certificate rather than a degraded one. PALAI_RUNNER_CA_CERT is
# accepted for one release as the name the earlier host package used — a second name for one thing is a
# second thing to keep in step, so the example and the unit teach only PALAI_CONTROLLER_CA.
if [ -n "${PALAI_RUNNER_CA_CERT:-}" ] && [ -z "${PALAI_CONTROLLER_CA:-}" ]; then
	export PALAI_CONTROLLER_CA="$PALAI_RUNNER_CA_CERT"
fi

exec /opt/palai-runner/palai-runner
