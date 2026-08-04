#!/usr/bin/env bash
# E15 T1 live interruption/resume drill (claim OPS-006): a REAL control-plane binary is killed mid
# migration chain, then restarted, and the migration journal is proven to resume to the correct head with
# data intact. Unlike the component test (which injects the fault in-process), this runs the shipped
# binary against a throwaway Postgres, so the crash and the restart are two real process lifetimes.
#
# The only credential is a throwaway local Postgres password, generated per run and passed to the binary
# ONLY via PALAI_DATABASE_URL in the environment — never on argv, never echoed, and `set -x` is never used.
#
# WHY THIS ROTTED, AND WHERE THE NEXT ROT WILL COME FROM. On 2026-08-04 this drill could not pass: it
# waited for a journal head of 34 while the chain had grown to sixty-seven, and it seeded and checksummed
# a table that had been dropped. Both are the kind of break a compiler cannot see, and NOTHING ROUTINE
# RUNS THIS FILE — `make verify` does not (Makefile lists `migration-resume-drill` as its own target), CI
# runs only `make verify`, and the component tier has its own in-process version of this drill that keeps
# passing while this one is broken. So the only thing standing between this script and silent rot is
# somebody typing `make migration-resume-drill`, and OPS-006's case.yaml plus two runbooks point at it as
# the LIVE proof.
#
# The mitigation carried here is to derive every number from the tree instead of writing it down: the head
# is read from storage/migrations below, and the fault point is the FIRST migration, which exists in any
# chain of two or more. A future change to the chain should not need to edit this file at all — and if it
# does, that is the signal to run it.
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

# THE HEAD COMES FROM THE TREE, NEVER FROM A LITERAL. This drill sat red for an unknown stretch because
# it asserted `journal head = 34` while the chain had grown to sixty-seven, and then the chain was squashed
# to two — a hardcoded head is wrong after every change to the chain and right only by accident.
head_version="$(ls "$root"/storage/migrations/*.up.sql | sed 's/.*\/0*\([0-9]*\)_.*/\1/' | sort -n | tail -1)"
test -n "$head_version" || fail "could not read the chain head from storage/migrations"
test "$head_version" -ge 2 || fail "the chain has $head_version migration(s); an interruption needs at least two"

# --- 1. First boot: crash right after the FIRST migration ----------------------------------------------
# The fault stops the runner between the two links, which is the state worth resuming from rather than
# merely a convenient one: every table exists and NOT ONE of them is row-level secured yet.
set +e
PALAI_DATABASE_URL="$url" PALAI_MIGRATE_FAULT_AFTER=1 PALAI_DISPATCH_WORKERS=0 \
  PALAI_LISTEN_ADDR=127.0.0.1:0 "$bindir/palai-control-plane" >"$bindir/boot1.log" 2>&1
rc=$?
set -e
test "$rc" -ne 0 || { cat "$bindir/boot1.log" >&2; fail "first boot exited 0, expected the injected crash"; }

head="$(q "SELECT coalesce(max(version),0) FROM schema_revisions")"
test "$head" = "1" || fail "journal head after crash = $head, want 1 (partial)"
mig="$(q "SELECT coalesce(max(version),0) FROM schema_migrations")"
test "$mig" = "1" || fail "schema_migrations head after crash = $mig, want 1"
secured="$(q "SELECT coalesce((SELECT relrowsecurity FROM pg_class c JOIN pg_namespace n ON n.oid=c.relnamespace WHERE n.nspname='public' AND c.relname='projects'), false)")"
test "$secured" = "f" || fail "projects is already row-level secured after the crash at 1; the partial state this drill resumes from is not the one it claims"

# Seed rows between the crash and the restart; their digest must survive the resumed chain unchanged.
# THE TABLE HAS TO BE ONE THIS BUILD REALLY WRITES: an earlier revision seeded a table that had stopped
# being populated, and a digest over an empty table is '' on both sides — "intact" whatever the chain did.
q "INSERT INTO projects (id) VALUES ('drill_prj_a'),('drill_prj_b')" >/dev/null
before="$(q "SELECT coalesce(md5(string_agg(id,'|' ORDER BY id)),'') FROM projects")"
test -n "$before" || fail "the seed digest is empty; the comparison below could not fail"

# --- 2. Restart with no fault: the chain resumes to the head -------------------------------------------
PALAI_DATABASE_URL="$url" PALAI_DISPATCH_WORKERS=0 PALAI_LISTEN_ADDR=127.0.0.1:0 \
  "$bindir/palai-control-plane" >"$bindir/boot2.log" 2>&1 &
resume_pid=$!

resumed=0
for _ in $(seq 1 100); do
  if ! kill -0 "$resume_pid" >/dev/null 2>&1; then cat "$bindir/boot2.log" >&2; fail "restart process died before completing the chain"; fi
  if [ "$(q "SELECT coalesce(max(version),0) FROM schema_revisions")" = "$head_version" ]; then resumed=1; break; fi
  sleep 0.2
done
kill "$resume_pid" >/dev/null 2>&1 || true
wait "$resume_pid" 2>/dev/null || true
resume_pid=""
test "$resumed" -eq 1 || fail "chain did not resume to head $head_version"

head="$(q "SELECT coalesce(max(version),0) FROM schema_revisions")"
test "$head" = "$head_version" || fail "journal head after resume = $head, want $head_version"
secured="$(q "SELECT relrowsecurity AND relforcerowsecurity FROM pg_class c JOIN pg_namespace n ON n.oid=c.relnamespace WHERE n.nspname='public' AND c.relname='projects'")"
test "$secured" = "t" || fail "projects is still unsecured after the resume; the chain reported its head without the last link taking effect"
after="$(q "SELECT coalesce(md5(string_agg(id,'|' ORDER BY id)),'') FROM projects")"
test "$before" = "$after" || fail "seeded rows changed across the resumed migration (data not intact)"

applied_by="$(q "SELECT applied_by FROM schema_revisions WHERE version=$head_version")"
test -n "$applied_by" || fail "journal row $head_version has an empty applied_by version stamp"

echo "migration_resume_drill=PASS journal_head=$head applied_by=$applied_by data_intact=yes"
