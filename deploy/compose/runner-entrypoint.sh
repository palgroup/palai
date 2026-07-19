#!/bin/sh
set -eu
# Bridge the compose contract to the runner binary's environment (cmd/runner). The
# one-use enrollment token is read from its mounted file and handed to the process in
# memory only; the runner clears it after enrolling and never writes it back to disk.
PALAI_ENROLLMENT_TOKEN="$(cat "$PALAI_ENROLLMENT_TOKEN_FILE")"
export PALAI_ENROLLMENT_TOKEN
export PALAI_CONTROLLER_CA="$PALAI_RUNNER_CA_CERT"
export PALAI_RUNNER_ID="runner-local"
export PALAI_RUNNER_DNS="runner-local.runners.palai.internal"
export PALAI_ENROLLMENT_URL="${PALAI_CONTROLLER_URL}/v1/runner/enroll"
# Renewal rolls the client certificate forward over the runner's existing identity as it
# nears expiry — the one-use bootstrap token is never presented again.
export PALAI_RENEW_URL="${PALAI_CONTROLLER_URL}/v1/runner/renew"
# Enrollment is https; the leasing session is wss over the same host:port.
export PALAI_SESSION_URL="wss://${PALAI_CONTROLLER_URL#https://}/v1/runner/connect"

exec /usr/local/bin/palai-runner
