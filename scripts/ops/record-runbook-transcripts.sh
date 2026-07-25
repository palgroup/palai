#!/usr/bin/env bash
# Record the pinned command transcripts for docs/operations/runbooks/.
#
# WHY THIS EXISTS. A runbook whose commands were never run is a paper runbook: the flags rot, the
# output an operator is told to expect is imagined, and nobody finds out at 3am. So every command a
# runbook tells you to type is EXECUTED here against a real stack, and its real output is pinned next
# to the runbook. TestRunbookCommandsWereExecuted (tests/docs/) then requires that every command line
# in every runbook appears in its transcript, so a command cannot be added to a runbook without being
# run.
#
# WHAT STACK. A `palai local up` stack — the packaged four-service compose stack. That is a REAL
# control plane, runner, Postgres and object store; it is NOT the production overlay, so two things
# are honestly absent and the runbooks say so where it matters:
#   - the secret-ref surface is unmounted (no master key ⇒ HTTP 404), because the master key is a
#     production-overlay concern (docs/operations/install.md step 2);
#   - `palai config validate` audits deploy/compose/production.env, which a local stack does not have.
#
# DESTRUCTIVE. The audit section deliberately corrupts the events journal to show what each alert
# looks like. Run this against a throwaway stack ONLY. It reseeds from `palai local reset --confirm`.
#
# Usage:  PALAI_HOME=<repo>/.palai bash scripts/ops/record-runbook-transcripts.sh [outdir]
set -euo pipefail

root="$(git rev-parse --show-toplevel)"
out="${1:-$root/docs/operations/runbooks/transcripts}"
export PALAI_HOME="${PALAI_HOME:-$root/.palai}"
work="$(mktemp -d)"
bin="$work/palai"

mkdir -p "$out"
go build -o "$bin" "$root/cmd/cli"

PROJECT="$(sed -n 's/.*"project": "\([^"]*\)".*/\1/p' "$PALAI_HOME/config.json")"
PG="${PROJECT}-postgres-1"
BASE="$(sed -n 's/.*"base_url": "\([^"]*\)".*/\1/p' "$PALAI_HOME/config.json")"
export PROJECT PG BASE

# `palai` on the transcript's PATH is the binary we just built; $WORK/$REPO keep machine-specific
# absolute paths out of the committed text without editing what actually ran.
export PATH="$work:$PATH"
export WORK="$work" REPO="$root"

current=""

# redact normalises the machine out of the transcript and strips anything credential-shaped. The API
# key VALUE is printed exactly once by `apikey create` and must never reach a committed file.
redact() {
	sed -e "s#$work#\$WORK#g" -e "s#$root#\$REPO#g" \
		-e 's#sk_[0-9a-f]\{16,\}#sk_<redacted>#g' \
		-e 's#\(password=\)[^ &"]*#\1<redacted>#g'
}

open_transcript() {
	current="$out/$1"
	{
		echo "# $2"
		echo "#"
		echo "# Recorded by scripts/ops/record-runbook-transcripts.sh against a live \`palai local up\`"
		echo "# stack (compose project \$PROJECT), NOT the production overlay."
		echo "#   commit:   $(git -C "$root" rev-parse --short HEAD)"
		echo "#   recorded: $(date -u +%Y-%m-%dT%H:%M:%SZ)"
		echo "#   platform: $(uname -sm), Docker $(docker version --format '{{.Server.Version}}')"
		echo "#"
		echo "# Absolute paths are rewritten to \$WORK (a scratch dir) and \$REPO (the worktree)."
		echo "# API key values are redacted; they are printed exactly once by design."
		echo
	} >"$current"
}

# run echoes the command exactly as a runbook prints it, then runs it and pins the output. A non-zero
# exit is RECORDED, not fatal: several of these commands are supposed to fail.
run() {
	{
		echo "\$ $1"
		set +e
		eval "$1" 2>&1 | redact
		rc="${PIPESTATUS[0]}"
		set -e
		test "$rc" -eq 0 || echo "[exit $rc]"
		echo
	} >>"$current"
}

note() { printf '%s\n\n' "$1" >>"$current"; }

# ---------------------------------------------------------------------------------------------------
# Seed: a clean stack with a real journal. Runs are dispatched (PALAI_DISPATCH_WORKERS=1) against the
# FAKE provider — the transcripts are about operator commands, and a fake engine keeps a real provider
# credential out of a committed file.
# ---------------------------------------------------------------------------------------------------
echo "recording against $PROJECT ($BASE) — this RESETS the stack's data volumes" >&2
"$bin" local reset --confirm >/dev/null 2>&1 || true
PALAI_DISPATCH_WORKERS=1 PALAI_MODEL_PROVIDER=fake "$bin" local up >/dev/null 2>&1
for i in 1 2 3; do "$bin" response create --input "runbook transcript anchor $i" >/dev/null 2>&1 || true; done
sleep 25

# ---------------------------------------------------------------------------------------------------
open_transcript incident-response.txt "Incident response — triage commands"
note "# Step 1: is the stack healthy, and which check is not?"
run 'palai local doctor'
note "# Step 2: what is this stack running?"
run 'palai version'
run 'docker exec "$PG" psql -U palai -d palai -c "SELECT max(version) AS chain_head FROM schema_revisions;"'
note "# Step 3: capture redacted diagnostics BEFORE changing anything."
run 'palai support-bundle --out "$WORK/support-bundle.tar.gz" --tail 200'
run 'tar tzf "$WORK/support-bundle.tar.gz"'
note "# Step 4: has anything touched the audit journal? (full drill: audit-integrity-alert.txt)"
run 'openssl ecparam -genkey -name prime256v1 -noout -out "$WORK/anchor.key"'
run 'palai audit checkpoint --out "$WORK/ir-anchor" --signing-key "$WORK/anchor.key"'
run 'mkdir -p "$WORK/oob" && cp "$WORK/ir-anchor/palai-audit-checkpoint.pub" "$WORK/oob/"'
run 'palai audit verify --checkpoint "$WORK/ir-anchor/audit-checkpoint.json" --pubkey "$WORK/oob/palai-audit-checkpoint.pub"'
note "# Step 5: production posture. On a local stack this AUDITS THE PRODUCTION OVERLAY and therefore
# fails — deploy/compose/production.env does not exist here. That is the honest output, not a defect;
# on a production install every row is green (docs/operations/install.md step 4)."
run 'palai config validate'

# ---------------------------------------------------------------------------------------------------
open_transcript key-compromise.txt "Key compromise — revoke, rotate, re-verify"
note "# A. API key: revoke bites immediately. Mint the replacement FIRST so the operator is not locked out."
run 'palai apikey list'
# `create` prints the key value exactly once, so it is captured HERE rather than re-run: the value is
# needed to prove revocation at the REQUEST level (200 -> revoke -> 401), which is the claim that
# matters. It never reaches the transcript — redact() strips it, and the echoed command shows $KEYVAL.
create_out="$("$bin" apikey create --project prj_local --scope provision 2>&1)"
{
	echo '$ palai apikey create --project prj_local --scope provision'
	printf '%s\n\n' "$create_out" | redact
} >>"$current"
NEWKEY="$(printf %s "$create_out" | python3 -c 'import json,sys; print(json.load(sys.stdin)["id"])')"
KEYVAL="$(printf %s "$create_out" | python3 -c 'import json,sys; print(json.load(sys.stdin)["key"])')"
export NEWKEY KEYVAL
note "# The new key works. This is the BEFORE half of the revocation proof."
run 'curl -s -o /dev/null -w "%{http_code}\n" "$BASE/v1/organizations" -H "Authorization: Bearer $KEYVAL"'
run 'palai apikey revoke "$NEWKEY"'
note "# The SAME key, immediately after. Revocation is enforced at the request, not merely recorded."
run 'curl -s -o /dev/null -w "%{http_code}\n" "$BASE/v1/organizations" -H "Authorization: Bearer $KEYVAL"'
run 'palai apikey get "$NEWKEY"'
note "# B. Runner enrollment credential: \`palai local up\` mints a FRESH one-use token every time, so
# re-running it rotates the runner's enrollment secret. The digests below are of the token file
# before and after."
run 'shasum -a 256 "$PALAI_HOME/runner-token"'
run 'palai local up >/dev/null 2>&1'
run 'shasum -a 256 "$PALAI_HOME/runner-token"'
note "# C. Provider/integration secrets. The secret-ref surface is mounted only where a master key is
# configured — i.e. the production overlay. On this local stack it is NOT mounted, and the CLI says so
# with a 404 rather than pretending to rotate something. On a production install, follow
# docs/operations/admin-cli.md (Secrets)."
run 'printf %s throwaway | palai secret create --name incident-demo'
note "# D. Signing key: verification is fail-closed against an out-of-band public key. A signature that
# does not verify against the key you were handed is an ALERT, never a warning."
run 'openssl ecparam -genkey -name prime256v1 -noout -out "$WORK/wrong.key"'
run 'openssl pkey -in "$WORK/wrong.key" -pubout -out "$WORK/oob/wrong.pub"'
run 'palai audit verify --checkpoint "$WORK/ir-anchor/audit-checkpoint.json" --pubkey "$WORK/oob/wrong.pub"'

# ---------------------------------------------------------------------------------------------------
open_transcript backup-restore.txt "Backup and restore-verify"
note "# Take the backup. It reaches the stack by docker-exec on the container NAME, so no host port has
# to be published — this is safe on a production install."
run 'palai backup --out "$WORK/palai-backup.tar.gz"'
run 'ls -l "$WORK/palai-backup.tar.gz"'
note "# Verify the archive WITHOUT restoring it. Six checks; every one is recomputed from the archive."
run 'palai restore verify --archive "$WORK/palai-backup.tar.gz"'
note "# The restore itself refuses a non-empty target — proven here against THIS stack, which has data."
run 'palai restore --archive "$WORK/palai-backup.tar.gz"'

# ---------------------------------------------------------------------------------------------------
open_transcript upgrade-rollback.txt "Upgrade and rollback — the read-and-refuse half"
note "# Before any swap: what is running, and where is the schema chain?"
run 'palai version'
run 'docker exec "$PG" psql -U palai -d palai -c "SELECT max(version) AS chain_head FROM schema_revisions;"'
run 'docker exec "$PG" psql -U palai -d palai -c "SELECT version, applied_at, applied_by, left(checksum,12) FROM schema_revisions ORDER BY version DESC LIMIT 5;"'
note "# The command refuses to guess. There is no default manifest and no default rollback target."
run 'palai upgrade'
run 'palai upgrade rollback'
note "# ... and it refuses a manifest that does not carry the images it would swap in."
printf '%s' '{"release":"n1","version":"deadbeef"}' >"$work/bogus.json"
run 'cat "$WORK/bogus.json"'
run 'palai upgrade --manifest "$WORK/bogus.json"'

# ---------------------------------------------------------------------------------------------------
open_transcript audit-integrity-alert.txt "Audit integrity — what each alert actually looks like"
note "# Cut an anchor over the current journal and keep the public key OUT of the checkpoint directory."
run 'palai audit checkpoint --out "$WORK/anchor" --signing-key "$WORK/anchor.key"'
run 'mkdir -p "$WORK/oob2" && cp "$WORK/anchor/palai-audit-checkpoint.pub" "$WORK/oob2/"'
note "# GREEN: an intact journal."
run 'palai audit verify --checkpoint "$WORK/anchor/audit-checkpoint.json" --pubkey "$WORK/oob2/palai-audit-checkpoint.pub"'
ROW="$(docker exec "$PG" psql -U palai -d palai -tAc "select id from events where type='model_step.completed.v1' order by journal_id limit 1;" | tr -d '[:space:]')"
# The three arms are held in variables so the transcript (and the runbook) show ONE readable psql
# invocation each instead of a wall of nested quoting.
EDIT_SQL="update events set payload = jsonb_set(payload, '{state}', '\"edited\"') where id = '$ROW'"
PURGE_SQL="update events set payload = '{\"purged\": true}'::jsonb where id = '$ROW'"
DELETE_SQL="delete from events where id = '$ROW'"
VERIFY='palai audit verify --checkpoint "$WORK/anchor/audit-checkpoint.json" --pubkey "$WORK/oob2/palai-audit-checkpoint.pub" 2>&1 | grep -E "ALERT|integrity alert"'
export ROW EDIT_SQL PURGE_SQL DELETE_SQL
note "# ---- DRILL (destructive; throwaway stack only) -------------------------------------------------
# The three arms below deliberately corrupt the journal so an operator has seen the alert BEFORE the
# night it fires for real."
note "# ARM 1 — an edited payload: [tamper]."
run 'docker exec "$PG" psql -U palai -d palai -c "$EDIT_SQL"'
run "$VERIFY"
note "# ARM 2 — an AUTHORISED §22.2 retention purge writes {\"purged\": true} over the same anchored row.
# The alert below is BYTE-FOR-BYTE the one above. This is the trap: on a stack with
# PALAI_RETENTION_STORE_FALSE_TTL set, routine maintenance raises the highest-severity alert, and an
# attacker gets \"the reaper did it\" for free."
run 'docker exec "$PG" psql -U palai -d palai -c "$PURGE_SQL"'
run "$VERIFY"
note "# ARM 3 — a removed row: [gap], a different alert with a different meaning."
run 'docker exec "$PG" psql -U palai -d palai -c "$DELETE_SQL"'
run "$VERIFY"

echo "recorded 5 transcripts into $out" >&2
