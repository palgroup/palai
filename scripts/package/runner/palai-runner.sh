#!/bin/sh
# E14 T5 — host launcher for the Palai runner (palai-runner.service ExecStart). It bridges the
# operator's runner.env to the runner binary's environment, mirroring the compose
# runner-entrypoint: it reads the one-use enrollment token from its FILE and derives the
# enroll/session/renew URLs from PALAI_CONTROLLER_URL. The runner opens NO inbound port — it
# dials PALAI_CONTROLLER_URL outbound only.
set -eu

: "${PALAI_CONTROLLER_URL:?PALAI_CONTROLLER_URL is required (https://controller-host:8443)}"
: "${PALAI_ENROLLMENT_TOKEN_FILE:?PALAI_ENROLLMENT_TOKEN_FILE is required (one-use token file)}"
: "${PALAI_RUNNER_CA_CERT:?PALAI_RUNNER_CA_CERT is required (the controller CA the runner pins)}"

# PALAI_SESSION_URL below strips the https:// prefix to build wss://…; a non-https URL would
# yield a broken "wss://http://…", so require https up front rather than fail obscurely later.
case "$PALAI_CONTROLLER_URL" in
	https://*) : ;;
	*) echo "palai-runner: PALAI_CONTROLLER_URL must start with https:// (got: $PALAI_CONTROLLER_URL)" >&2; exit 1 ;;
esac

# The one-use token is handed to the process in memory only; the runner clears it after
# enrolling (cmd/runner unsets it) and never writes it back to disk.
PALAI_ENROLLMENT_TOKEN="$(cat "$PALAI_ENROLLMENT_TOKEN_FILE")"
export PALAI_ENROLLMENT_TOKEN
export PALAI_CONTROLLER_CA="$PALAI_RUNNER_CA_CERT"
export PALAI_ENROLLMENT_URL="${PALAI_CONTROLLER_URL}/v1/runner/enroll"
# Renewal rolls the client certificate forward over the runner's identity as it nears expiry;
# the one-use bootstrap token is never presented again.
export PALAI_RENEW_URL="${PALAI_CONTROLLER_URL}/v1/runner/renew"
# Enrollment is https; the leasing session is wss over the same host:port.
export PALAI_SESSION_URL="wss://${PALAI_CONTROLLER_URL#https://}/v1/runner/connect"

# PALAI_CONTROLLER_DNS, PALAI_RUNNER_ID, PALAI_RUNNER_DNS, PALAI_ENGINE_IMAGE (and optional
# PALAI_RUNNER_CONCURRENCY) come straight from runner.env — no derivation needed.
exec /opt/palai-runner/palai-runner
