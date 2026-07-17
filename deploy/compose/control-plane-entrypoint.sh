#!/bin/sh
set -eu
# ponytail: file-secret -> process env bridge (Option B; the E13 encrypted-at-rest
# backend removes this bridge — LP plan §7.1). Runtime-only: the resolved value lives in
# the process environment, never in the container's static .Config.Env, so it stays out
# of `docker inspect` / `compose config` (LP-011 secret scan).
if [ -s /run/secrets/provider_one_key ]; then
  PALAI_SECRET_PROVIDER_ONE="$(cat /run/secrets/provider_one_key)"
  export PALAI_SECRET_PROVIDER_ONE
fi

# The Postgres password is a file-secret too: assemble the connection URL here so the
# credential never rides a compose `environment:` value. `palai init` mints a hex
# password, so it needs no URL-escaping.
PALAI_DATABASE_URL="postgres://palai:$(cat /run/secrets/pg_password)@postgres:5432/palai?sslmode=disable"
export PALAI_DATABASE_URL

exec /usr/local/bin/palai-control-plane
