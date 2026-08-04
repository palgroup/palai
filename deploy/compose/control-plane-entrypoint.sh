#!/bin/sh
set -eu
# THE PROVIDER-KEY BRIDGE IS GONE, 2026-08-04. The comment that stood here said "the E13
# encrypted-at-rest backend removes this bridge — LP plan §7.1"; that backend shipped and the
# bridge stayed, so every deployment kept a provider credential in the control plane's
# environment while the encrypted path sat unused beside it.
#
# Keeping it out of `docker inspect` was never the whole risk: an environment value is readable
# by anything running as the same uid, and it cannot be unset afterwards (the kernel keeps the
# process's initial environment). The credential is sealed through POST /v1/secret-refs and named
# by a model connection instead; /run/secrets/provider_one_key is no longer read by anything.

# The Postgres password is a file-secret too: assemble the connection URL here so the
# credential never rides a compose `environment:` value. `palai init` mints a hex
# password, so it needs no URL-escaping.
PALAI_DATABASE_URL="postgres://palai:$(cat /run/secrets/pg_password)@postgres:5432/palai?sslmode=disable"
export PALAI_DATABASE_URL

exec /usr/local/bin/palai-control-plane
