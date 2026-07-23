#!/usr/bin/env bash
# E15 T1 live interruption/resume drill (claim OPS-006): a REAL control-plane binary is killed mid
# migration chain, then restarted, and the migration journal is proven to resume to the correct head with
# data intact. Unlike the component test (which injects the fault in-process), this runs the shipped
# binary against a throwaway Postgres, so the crash and the restart are two real process lifetimes.
#
# The only credential is a throwaway local Postgres password, generated per run and passed to the binary
# ONLY via PALAI_DATABASE_URL in the environment — never on argv, never echoed, and `set -x` is never used.
set -euo pipefail

root="$(git rev-parse --show-toplevel)"
cd "$root"

# Immutable pinned image (matches scripts/test/component / ADR-0001).
image="postgres@sha256:17e67d7b9890c99b055ba1e0d5c5be4ec27c9d3a72bda32db24a5e5d8a85af0c"
label="io.palai.e15t1-resume=1"
run_id="$$-${RANDOM}"
container="palai-e15t1-resume-$run_id"   # unique prefix so a sibling task's leak-guard never counts it
volume="palai-e15t1-resume-$run_id"
password="palai-e15t1-${RANDOM}${RANDOM}"  # throwaway; only ever placed in PALAI_DATABASE_URL
bindir="$(mktemp -d)"

cleanup() {
  [ -n "${resume_pid:-}" ] && kill "$resume_pid" >/dev/null 2>&1 || true
  docker rm -f "$container" >/dev/null 2>&1 || true
  docker volume rm -f "$volume" >/dev/null 2>&1 || true
  rm -rf "$bindir" >/dev/null 2>&1 || true
}
trap cleanup EXIT

fail() { echo "migration_resume_drill=FAIL: $*" >&2; exit 1; }

docker volume create --label "$label" "$volume" >/dev/null
docker run --detach --pull=missing \
  --name "$container" --label "$label" \
  -e POSTGRES_PASSWORD="$password" -e POSTGRES_DB=palai \
  -P -v "$volume":/var/lib/postgresql/data \
  "$image" >/dev/null

ready=0
for _ in $(seq 1 60); do
  if docker exec "$container" pg_isready -U postgres -d palai >/dev/null 2>&1; then ready=1; break; fi
  sleep 0.25
done
test "$ready" -eq 1 || { docker logs "$container" >&2 || true; fail "postgres did not become ready"; }

binding="$(docker port "$container" 5432/tcp)"
port="${binding##*:}"
url="postgres://postgres:$password@127.0.0.1:$port/palai?sslmode=disable"

# Query helper: runs psql INSIDE the container, so the password rides PGPASSWORD in the container env, not
# this shell's argv. Output is trimmed to a bare scalar.
q() { docker exec -e PGPASSWORD="$password" "$container" psql -U postgres -d palai -tAc "$1" | tr -d '[:space:]'; }

# Build the shipped control-plane binary. `go build` in a git repo embeds the VCS revision, so the
# journal's applied_by stamp is a real commit sha here (not the "dev" a `go test` binary carries).
go build -o "$bindir/palai-control-plane" ./apps/control-plane/cmd/palai-control-plane

# --- 1. First boot: crash right after migration 000033 -------------------------------------------------
set +e
PALAI_DATABASE_URL="$url" PALAI_MIGRATE_FAULT_AFTER=33 PALAI_DISPATCH_WORKERS=0 \
  PALAI_LISTEN_ADDR=127.0.0.1:0 "$bindir/palai-control-plane" >"$bindir/boot1.log" 2>&1
rc=$?
set -e
test "$rc" -ne 0 || { cat "$bindir/boot1.log" >&2; fail "first boot exited 0, expected the injected crash"; }

head="$(q "SELECT coalesce(max(version),0) FROM schema_revisions")"
test "$head" = "33" || fail "journal head after crash = $head, want 33 (partial)"
mig="$(q "SELECT coalesce(max(version),0) FROM schema_migrations")"
test "$mig" = "33" || fail "schema_migrations head after crash = $mig, want 33"
ue="$(q "SELECT to_regclass('public.usage_events') IS NOT NULL")"
test "$ue" = "t" || fail "usage_events absent after crash-at-33, but 000034 should not have run"

# Seed rows between the crash and the restart; their digest must survive the resumed chain (which includes
# the destructive 000034 contract) unchanged.
q "INSERT INTO organizations (id) VALUES ('drill_org_a'),('drill_org_b')" >/dev/null
before="$(q "SELECT coalesce(md5(string_agg(id,'|' ORDER BY id)),'') FROM organizations")"

# --- 2. Restart with no fault: the chain resumes to the head -------------------------------------------
PALAI_DATABASE_URL="$url" PALAI_DISPATCH_WORKERS=0 PALAI_LISTEN_ADDR=127.0.0.1:0 \
  "$bindir/palai-control-plane" >"$bindir/boot2.log" 2>&1 &
resume_pid=$!

resumed=0
for _ in $(seq 1 100); do
  if ! kill -0 "$resume_pid" >/dev/null 2>&1; then cat "$bindir/boot2.log" >&2; fail "restart process died before completing the chain"; fi
  if [ "$(q "SELECT coalesce(max(version),0) FROM schema_revisions")" = "34" ]; then resumed=1; break; fi
  sleep 0.2
done
kill "$resume_pid" >/dev/null 2>&1 || true
wait "$resume_pid" 2>/dev/null || true
resume_pid=""
test "$resumed" -eq 1 || fail "chain did not resume to head 34"

head="$(q "SELECT coalesce(max(version),0) FROM schema_revisions")"
test "$head" = "34" || fail "journal head after resume = $head, want 34"
ue="$(q "SELECT to_regclass('public.usage_events') IS NULL")"
test "$ue" = "t" || fail "usage_events survived the resumed chain, but 000034 should have dropped it"
after="$(q "SELECT coalesce(md5(string_agg(id,'|' ORDER BY id)),'') FROM organizations")"
test "$before" = "$after" || fail "seeded rows changed across the resumed migration (data not intact)"

applied_by="$(q "SELECT applied_by FROM schema_revisions WHERE version=34")"
test -n "$applied_by" || fail "journal row 34 has an empty applied_by version stamp"

echo "migration_resume_drill=PASS journal_head=$head applied_by=$applied_by data_intact=yes"
