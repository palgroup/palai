#!/bin/sh
set -eu
# E14 T1 — production posture guard. This runs as the control-plane's entrypoint ONLY under
# the production overlay (deploy/compose/production.yml overrides `entrypoint:` to it); the
# local profile keeps the plain bridge entrypoint untouched. It refuses to boot the stack
# unless the DB-backed secret store's master key is a REAL, operator-provided key — never
# unset, never a dev-default placeholder. A stack booted on the dev key would seal every
# tenant secret under a key the whole repo knows, so this FAILS CLOSED before the
# control-plane binary starts. Hex/length validity is enforced downstream by
# identity.ParseMasterKey (cmd/palai-control-plane/main.go); this guard owns "present and not
# a dev default". On success it execs its arguments — the overlay passes the plain bridge
# entrypoint as "$@", so a passing production boot is the local boot plus exactly this gate.

# The dev-default values this guard rejects. They MUST stay in lock-step with the placeholders
# production.env.example / docs/operations/install.md ship and production_guard_test.go asserts,
# so "refuse the dev default" is a tested contract, not a comment.
DEV_MASTER_KEY_PLACEHOLDER="REPLACE_WITH_OPENSSL_RAND_HEX_32"
DEV_MASTER_KEY_ZERO="0000000000000000000000000000000000000000000000000000000000000000"
DEV_BOOTSTRAP_KEY_PLACEHOLDER="REPLACE_WITH_A_REAL_BOOTSTRAP_KEY"

fail() { echo "palai production: $1" >&2; exit 1; }

: "${PALAI_SECRET_MASTER_KEY_FILE:?PALAI_SECRET_MASTER_KEY_FILE is required under the production profile}"
[ -s "$PALAI_SECRET_MASTER_KEY_FILE" ] || fail "master key file $PALAI_SECRET_MASTER_KEY_FILE is missing or empty"

# Command substitution strips the trailing newline a hex key file usually carries; a real key
# has no internal whitespace, so no further trimming is needed.
master="$(cat "$PALAI_SECRET_MASTER_KEY_FILE")"
# A whitespace-only file passes the -s size test; catch it here so the operator gets this
# actionable message instead of the binary's downstream "not valid hex".
case "$master" in
	*[![:space:]]*) : ;; # has a non-whitespace char — good
	*) fail "master key file $PALAI_SECRET_MASTER_KEY_FILE contains only whitespace" ;;
esac
case "$master" in
	"$DEV_MASTER_KEY_PLACEHOLDER" | "$DEV_MASTER_KEY_ZERO")
		fail "refusing to boot on the dev-default master key — generate a real one with 'openssl rand -hex 32' (docs/operations/install.md)" ;;
esac

# The bootstrap admin key the production profile provisions over must likewise not be the
# shipped placeholder. It is optional (a stack may seed none), so it is only checked when set
# and non-empty.
if [ -n "${PALAI_BOOTSTRAP_API_KEY_FILE:-}" ] && [ -s "$PALAI_BOOTSTRAP_API_KEY_FILE" ]; then
	bootstrap="$(cat "$PALAI_BOOTSTRAP_API_KEY_FILE")"
	[ "$bootstrap" != "$DEV_BOOTSTRAP_KEY_PLACEHOLDER" ] || fail "refusing to boot on the dev-default bootstrap API key — mint a real one (docs/operations/install.md)"
fi

exec "$@"
