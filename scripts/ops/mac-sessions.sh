#!/usr/bin/env bash
# Prepare ONE Mac to host N concurrent Palai agent sessions — and prove the separation it claims.
#
# WHY THIS EXISTS. On Linux we open ten containers on one box. macOS has no equivalent: Apple's
# Containerization framework runs LINUX guests, macOS VMs are capped at two per host (kernel quota
# plus the SLA), and every weaker mechanism was measured and defeated on this hardware —
# `sandbox-exec`, Apple's own App Sandbox, ACLs, `chflags`, encrypted disk images. They all fall to
# the same trick, because `xcrun simctl spawn` does not fork a child: it asks a per-user daemon to
# create a process, which arrives parented to `launchd` with none of the caller's restrictions.
# `CoreSimulatorService` is instantiated PER UID, so the uid is the only boundary on the same axis as
# the thing being isolated. That is the whole argument for this script, and it is measured, not
# assumed: docs/research/macos-session-isolation.md and macos-isolation-without-accounts.md.
#
# WHAT IT ASSUMES. macOS with Xcode installed and selected, and ONE Xcode version for the whole box
# (`CoreSimulatorService` is aware of a single Xcode at a time; switching it kills booted simulators).
# `up --mode accounts` and most of `verify` need root, so run them under `sudo`. `--mode dirs` needs
# no privilege at all.
#
# THE CEILING, AND IT IS NOT NEGOTIABLE. A uid boundary is defeated by `sudo` and by any local-root
# privilege escalation — three shipped for macOS in 2026 alone. So this script packs ONE customer's
# concurrent sessions densely onto one Mac. DIFFERENT CUSTOMERS STILL NEED DIFFERENT MACS. Nothing
# here changes that, and no output of this script should be read as claiming otherwise.
#
# DESTRUCTIVE. `up --mode accounts --apply` creates local user accounts. `down --apply` DELETES them
# and their home directories, irreversibly, along with every simulator device and every uncommitted
# file the session left behind. Both refuse to act without `--apply`; neither ever prompts, because
# these scripts run non-interactively. `verify --simulator` creates, boots and deletes simulator
# devices inside the sessions' own device sets.
#
# NO PASSWORDS. This script never puts credential material in argv or on disk. Accounts are created
# WITHOUT a password, which makes them usable by root (`sudo -u <user> ...`) and unusable for login.
# Turning one into a real Aqua login is an operator decision with a cost — see docs/operations/
# mac-sessions.md §5 — so the script prints the command and does not run it.
#
# Usage:
#   bash scripts/ops/mac-sessions.sh plan [--count N] [--mode dirs|accounts]
#   bash scripts/ops/mac-sessions.sh up --count N [--mode dirs|accounts] [--apply]
#   sudo bash scripts/ops/mac-sessions.sh verify [--mode dirs|accounts] [--simulator]
#   sudo bash scripts/ops/mac-sessions.sh down [--mode dirs|accounts] [--apply]
#   bash scripts/ops/mac-sessions.sh selftest
set -euo pipefail

SUBCOMMANDS="plan up verify down selftest"

# PREFIX is the account-name prefix this script owns. It is overridable for ONE reason and it is the same
# reason PALAI_MAC_SESSIONS_HOST_UUID is: a test that drives this script has to be able to scan a namespace
# no real machine uses.
#
# WITHOUT IT, THE DOC'S OWN INSTRUCTIONS BREAK THE DOC'S OWN TESTS. tests/docs runs the real script against
# real directory services with a pinned fake host UUID, so a genuine session account — the thing
# mac-sessions.md tells every operator to create — carries a marker minted on the REAL host UUID, collides
# by name, and turns a dry run into exit 2. Measured 2026-08-02: one `palai-s01` on the box made
# TestMacSessionsDestructiveSubcommandsNeedAnExplicitFlag red at HEAD. An operator who followed the page
# could not run `make verify` again.
#
# IT IS NOT AUTHORITY. Nothing may be deleted on the strength of a name: deletion_is_permitted also requires
# the uid this script would have allocated, the marker written into the record (root-only), and that
# marker's host UUID matching this Mac. Renaming the namespace changes what is SCANNED, never what is
# permitted — which is why this is a variable and the marker is not.
PREFIX="${PALAI_MAC_SESSIONS_PREFIX:-palai-s}"
# UID_BASE is overridable for the same reason PREFIX is, and it is the SECOND half of the same isolation:
# renaming the namespace alone was not enough. A test that scans `palaitest-sNN` still allocates uid 701 for
# index 01, which the operator's real `palai-s01` already holds — measured 2026-08-02, the dry run refused
# with "uid 701 already belongs to palai-s01". A namespace is a name AND a uid range or it is neither.
UID_BASE="${PALAI_MAC_SESSIONS_UID_BASE:-700}"
MAX_SESSIONS=99
MARKER_ATTR="dsAttrTypeNative:palai_mac_session"
MARKER_MAGIC="palai-mac-sessions"
MARKER_VERSION=1
DIRS_MARKER=".palai-mac-sessions"
# A session that is only allowed ~8 GiB is a session that can hold one booted device (~640 MB RSS
# measured, and RSS is a floor) plus a build. Reserve the same again for the OS and its caches.
GIB_PER_SESSION=8
GIB_FOR_HOST=8

MODE=""
COUNT=""
APPLY=0
SIMULATOR=0
DIRS_ROOT="${PALAI_SESSIONS_ROOT:-$HOME/palai-sessions}"

fail=0
unverified=0

die() {
	printf 'mac-sessions: %s\n' "$*" >&2
	exit 2
}
say() { printf '%s\n' "$*"; }
hdr() { printf '\n%s\n' "$*"; }

is_macos() { [ "$(uname -s)" = "Darwin" ]; }
is_root() { [ "$(id -u)" -eq 0 ]; }

# ---------------------------------------------------------------------------------------------------
# Identity. The host UUID binds a marker to THIS Mac, so a directory-services record restored from
# another machine — or a home directory copied off one — cannot authorise a deletion here.
# ---------------------------------------------------------------------------------------------------
host_uuid() {
	if [ -n "${PALAI_MAC_SESSIONS_HOST_UUID:-}" ]; then
		printf '%s' "$PALAI_MAC_SESSIONS_HOST_UUID"
		return 0
	fi
	is_macos || return 0
	ioreg -rd1 -c IOPlatformExpertDevice 2>/dev/null | awk -F'"' '/IOPlatformUUID/{print $4; exit}'
}

# session_seq is `seq FIRST LAST` that yields NOTHING when the range is empty, which BSD seq does not do.
#
# ON THIS MACHINE, MEASURED 2026-08-02: `seq 2 1` prints "2\n1" and `seq 1 0` prints "1\n0" — BSD seq
# counts DOWN when the first number exceeds the last, where GNU seq prints nothing. Every `for n in $(seq
# ...)` in this file was written expecting the GNU behaviour, and one of them shipped two defects because of
# it, both of them shapes this repository keeps finding:
#
#   `verify_device_sets` looped `seq 2 "$COUNT"` to compare every OTHER session against session 1. With one
#   session that became n=2 then n=1. n=2 is a session that does not exist, so `as_session` failed, the
#   output was empty, the case fell to its default arm and it reported **PASS — a green line about a machine
#   that is not there**. n=1 is session one itself, which of course sees its own device, and it reported
#   **FAIL "s01's set cannot see s01's device"** — an assertion that reads like a boundary violation and is
#   actually a session compared with itself.
#
# So the range is bounded here rather than at four call sites, because the next loop written in this file
# would have inherited it.
session_seq() {
	[ "$1" -le "$2" ] 2>/dev/null || return 0
	seq "$1" "$2"
}

session_name() { printf '%s%02d' "$PREFIX" "$1"; }
session_id() { printf 's%02d' "$1"; }
session_uid() { printf '%d' "$((UID_BASE + $1))"; }
marker_for() { printf '%s:%s:%s:%s:%s' "$MARKER_MAGIC" "$MARKER_VERSION" "$(host_uuid)" "$1" "$2"; }

# ---------------------------------------------------------------------------------------------------
# THE DELETION GUARD. `down` removes accounts, so every reason to refuse lives in one predicate and
# `selftest` runs it against a table. A name prefix on its own is NOT enough: an operator can have an
# account called palai-s01 that this script never created, a record can be renamed, and the account
# running the script is never a candidate. Returns 0 to permit; otherwise prints why and returns 1.
#
#   $1 name   $2 uid   $3 marker value   $4 in-admin (yes|no)   $5 protected uids (comma separated)
# ---------------------------------------------------------------------------------------------------
deletion_is_permitted() {
	local name="$1" uid="$2" marker="$3" in_admin="$4" protected="$5"
	local idx expect_uid huuid f_magic f_ver f_host f_name

	case "$name" in
	"$PREFIX"[0-9][0-9]) ;;
	*)
		printf 'name %q is not %sNN' "$name" "$PREFIX"
		return 1
		;;
	esac
	idx="${name#"$PREFIX"}"
	if [ "$((10#$idx))" -lt 1 ] || [ "$((10#$idx))" -gt "$MAX_SESSIONS" ]; then
		printf 'session index %s is outside 01..%02d' "$idx" "$MAX_SESSIONS"
		return 1
	fi

	# The uid must be the one this script would have allocated for that exact name. A record whose
	# name and uid disagree was edited after creation, and we do not know by whom.
	expect_uid="$((UID_BASE + 10#$idx))"
	if [ "$uid" != "$expect_uid" ]; then
		printf 'uid %s does not match the uid %s this script allocates for %s' "$uid" "$expect_uid" "$name"
		return 1
	fi

	# The marker is written into the directory-services record, which only root can write. It is the
	# difference between "an account named like ours" and "an account we created on this Mac".
	huuid="$(host_uuid)"
	if [ -z "$huuid" ]; then
		printf 'this host has no identity (no IOPlatformUUID) — refusing to match any marker'
		return 1
	fi
	IFS=':' read -r f_magic f_ver f_host f_name _ <<<"$marker"
	if [ "$f_magic" != "$MARKER_MAGIC" ] || [ "$f_ver" != "$MARKER_VERSION" ]; then
		printf 'record carries no %s marker (found %q)' "$MARKER_MAGIC" "$marker"
		return 1
	fi
	if [ "$f_host" != "$huuid" ]; then
		printf 'marker was minted on host %q, this host is %q' "$f_host" "$huuid"
		return 1
	fi
	if [ "$f_name" != "$name" ]; then
		printf 'marker names session %q but the record is %q' "$f_name" "$name"
		return 1
	fi

	if [ "$in_admin" != "no" ]; then
		printf 'account is a member of admin — this script never creates one, so it did not create this'
		return 1
	fi

	case ",$protected," in
	*",$uid,"*)
		printf 'uid %s is the operator, the console user, or root' "$uid"
		return 1
		;;
	esac
	return 0
}

# protected_uids lists everything `down` must never touch: root, whoever invoked sudo, whoever is at
# the console, and the uid this process is running as.
protected_uids() {
	local out="0,$(id -u)"
	if [ -n "${SUDO_UID:-}" ]; then out="$out,$SUDO_UID"; fi
	if is_macos; then
		local console
		console="$(stat -f '%u' /dev/console 2>/dev/null || true)"
		if [ -n "$console" ]; then out="$out,$console"; fi
	fi
	printf '%s' "$out"
}

# ---------------------------------------------------------------------------------------------------
# Directory-services reads. All of these are readable without privilege.
# ---------------------------------------------------------------------------------------------------
# The whole local user table comes from ONE call and is cached: `dscl -read` per name and
# `dscl -search` per uid each cost seconds, and 99 of either turned `down` into a minute-long command.
# create_account invalidates the cache, since it is the only thing here that changes the table.
# load_user_table must run in the TOP-LEVEL shell: every helper below is called from a command
# substitution, and a cache filled inside one dies with that subshell. That mistake cost 20 seconds
# per `plan`.
_user_table=""
load_user_table() {
	if is_macos; then _user_table="$(dscl . -list /Users UniqueID 2>/dev/null)"; fi
}
user_table() { printf '%s' "$_user_table"; }
ds_uid() { user_table | awk -v n="$1" '$1 == n {print $2; exit}'; }
ds_exists() { [ -n "$(ds_uid "$1")" ]; }
uid_owner() { user_table | awk -v u="$1" '$2 == u {print $1; exit}'; }
ds_home() { dscl . -read "/Users/$1" NFSHomeDirectory 2>/dev/null | awk '{print $2}'; }
ds_marker() { dscl . -read "/Users/$1" "$MARKER_ATTR" 2>/dev/null | awk '{print $2}'; }
ds_in_admin() { if dseditgroup -o checkmember -m "$1" admin >/dev/null 2>&1; then echo yes; else echo no; fi; }

# session_records prints "<name> <uid>" for every local account whose name matches ours.
session_records() {
	user_table | awk -v p="^$PREFIX[0-9][0-9]\$" '$1 ~ p {print $1, $2}'
}

# ---------------------------------------------------------------------------------------------------
# Check accounting. A check that could not be run is UNVERIFIED, never a pass — the point of this
# script is a boundary that was proven, and "we did not look" is not a proof.
# ---------------------------------------------------------------------------------------------------
check() { # check PASS|FAIL|UNVERIFIED <name> <detail>
	local status="$1" name="$2" detail="${3:-}"
	printf '  [%-10s] %-46s %s\n' "$status" "$name" "$detail"
	case "$status" in
	FAIL) fail=$((fail + 1)) ;;
	UNVERIFIED) unverified=$((unverified + 1)) ;;
	esac
}

# ---------------------------------------------------------------------------------------------------
# Session trees. Same relative layout in both modes, so an agent's environment does not change shape
# when an operator moves from directories to accounts.
# ---------------------------------------------------------------------------------------------------
session_root() { # session_root <index>
	if [ "$MODE" = "accounts" ]; then
		printf '/Users/%s/palai-session' "$(session_name "$1")"
	else
		printf '%s/%s' "$DIRS_ROOT" "$(session_id "$1")"
	fi
}
device_set() { printf '%s/devices' "$(session_root "$1")"; }

# as_session runs a command as the session's owner in accounts mode, or directly in dirs mode.
as_session() { # as_session <index> <cmd...>
	local n="$1"
	shift
	if [ "$MODE" = "accounts" ]; then
		sudo -u "$(session_name "$n")" -- "$@"
	else
		"$@"
	fi
}

write_session_tree() { # write_session_tree <index>
	local n="$1" root sid
	root="$(session_root "$n")"
	sid="$(session_id "$n")"
	as_session "$n" /bin/mkdir -p "$root/work" "$root/devices" "$root/derived"
	as_session "$n" /bin/chmod 700 "$root"
	# session.env is what an agent session sources. --set is the whole point: a device in an alternate
	# set is invisible to the default set, so two sessions cannot boot, wipe or rename each other's
	# devices by accident. It is namespacing, not security — under one uid another session can point
	# --set at this directory (measured: macos-isolation-without-accounts.md T22).
	as_session "$n" /usr/bin/tee "$root/session.env" >/dev/null <<-EOF
		# Palai agent session $sid on $(hostname -s). Written by scripts/ops/mac-sessions.sh.
		# Source this before running anything, and pass --set on EVERY simctl invocation.
		export PALAI_SESSION="$sid"
		export PALAI_SESSION_ROOT="$root"
		export PALAI_SIMCTL_SET="$root/devices"
		export PALAI_WORKDIR="$root/work"
		export PALAI_DERIVED_DATA="$root/derived"
		# e.g.  xcrun simctl --set "\$PALAI_SIMCTL_SET" list devices
		#       xcodebuild -derivedDataPath "\$PALAI_DERIVED_DATA" ...
	EOF
	as_session "$n" /bin/chmod 600 "$root/session.env"
}

# ---------------------------------------------------------------------------------------------------
# plan
# ---------------------------------------------------------------------------------------------------
cmd_plan() {
	local n name uid state gib ceiling
	say "palai mac-sessions — PLAN. This subcommand changes nothing."

	hdr "HOST"
	if is_macos; then
		printf '  %-18s %s (%s)\n' "macOS" "$(sw_vers -productVersion)" "$(sw_vers -buildVersion)"
		printf '  %-18s %s\n' "hardware" "$(sysctl -n machdep.cpu.brand_string 2>/dev/null || echo unknown)"
		gib=$(($(sysctl -n hw.memsize) / 1073741824))
		printf '  %-18s %s GiB\n' "memory" "$gib"
		printf '  %-18s %s\n' "Xcode" "$(xcodebuild -version 2>/dev/null | tr '\n' ' ' || echo 'NOT INSTALLED')"
		printf '  %-18s %s\n' "developer dir" "$(xcode-select -p 2>/dev/null || echo unset)"
		printf '  %-18s %s\n' "host uuid" "$(host_uuid)"
		printf '  %-18s %s (uid %s)\n' "console user" "$(stat -f '%Su' /dev/console)" "$(stat -f '%u' /dev/console)"
	else
		gib=0
		printf '  %-18s %s\n' "kernel" "$(uname -sm) — NOT macOS"
		say "  This plan describes what WOULD happen on a Mac. Every host fact above is unavailable"
		say "  here, and \`up\`, \`verify\` and \`down\` will refuse to run."
	fi

	hdr "PICK A MODE — this script does not pick for you"
	say "  --mode dirs       Per-session directories plus a per-session \`simctl --set\`. No accounts,"
	say "                    no root, no login. Sufficient when every session belongs to the SAME"
	say "                    customer: it stops sessions clobbering each other, which is the real"
	say "                    failure there. It is NOT a security boundary — any process of your uid"
	say "                    can point --set at another session's devices."
	say "  --mode accounts   One non-admin macOS account per session. The uid is the only mechanism"
	say "                    measured to survive \`simctl spawn\`, because CoreSimulatorService is"
	say "                    instantiated per user. Needs root to create, and an Aqua login per"
	say "                    session if the session must DRIVE a simulator (see §5 of the doc)."
	say ""
	say "  Neither is a boundary between DIFFERENT CUSTOMERS. sudo and any local-root escalation"
	say "  defeat a uid, so different customers still need different Macs. Accounts buy density for"
	say "  one customer, nothing more."

	if [ -n "$COUNT" ]; then
		hdr "CAPACITY"
		if [ "$gib" -gt 0 ]; then
			ceiling=$(((gib - GIB_FOR_HOST) / GIB_PER_SESSION))
			if [ "$ceiling" -lt 0 ]; then ceiling=0; fi
			printf '  %s GiB of RAM, %s GiB reserved for the host, %s GiB budgeted per session\n' \
				"$gib" "$GIB_FOR_HOST" "$GIB_PER_SESSION"
			printf '  ceiling on this box: %s concurrent session(s); you asked for %s\n' "$ceiling" "$COUNT"
			if [ "$COUNT" -gt "$ceiling" ]; then
				say "  WARNING: $COUNT exceeds what this box can hold. One booted device measured 640 MB"
				say "  RSS and 179 processes, and RSS is a floor. Expect swap, then instability."
			fi
		else
			say "  unknown — host RAM cannot be read here"
		fi

		# THE SECOND AXIS, AND IT IS THE ONE THAT ACTUALLY RUNS THE SESSIONS. Everything above is OS-level
		# isolation — uids, device sets, home directories. None of it makes the Palai stack execute two runs
		# at the same time: that is two environment variables on the bring-up, and with their shipped
		# defaults (dispatch 0 in compose.yaml, 1 in production.yml; runner 1) a Mac prepared for N sessions
		# still runs them ONE AT A TIME. Measured 2026-07-29 on this hardware: at the defaults two runs
		# submitted together ran back to back, the second starting 43ms after the first finished.
		hdr "PALAI CONCURRENCY (the stack knobs — isolation above does NOT provide this)"
		say "  A stack is only as parallel as the SMALLER of these two, so set both:"
		say ""
		printf '    PALAI_DISPATCH_WORKERS=%s   \\\n' "$COUNT"
		printf '    PALAI_RUNNER_CONCURRENCY=%s \\\n' "$COUNT"
		say "    PALAI_WORKSPACE_ROOT=/absolute/host/path \\"
		say "    palai up --native"
		say ""
		say "  PALAI_DISPATCH_WORKERS  how many runs the control plane dispatches at once."
		say "  PALAI_RUNNER_CONCURRENCY  how many engine leases ONE runner parks at once; at 1 every"
		say "                          dispatch worker but one blocks in Dial, which looks like a hang."
		say "  Unset, the runner count follows the dispatch count (cmd/cli/internal/stack/up.go)."
		say ""
		say "  Prove it actually ran concurrently, against the running stack:"
		say "    go test -tags=live -count=1 -v ./tests/live/concurrency/"
		say "  It reads the durable run ledger and FAILS if the runs never overlapped."

		hdr "WOULD ALLOCATE (mode=${MODE:-accounts}, count=$COUNT)"
		for n in $(session_seq 1 "$COUNT"); do
			name="$(session_name "$n")"
			uid="$(session_uid "$n")"
			if [ "${MODE:-accounts}" = "dirs" ]; then
				state="absent"
				if [ -d "$DIRS_ROOT/$(session_id "$n")" ]; then state="exists (would be reused)"; fi
				printf '  %-12s %-6s %-38s %s\n' "$(session_id "$n")" "" "$DIRS_ROOT/$(session_id "$n")" "$state"
			else
				state="absent — would be created"
				if ds_exists "$name"; then
					if [ -n "$(ds_marker "$name")" ]; then
						state="exists, ours — would be reused"
					else
						state="EXISTS AND IS NOT OURS — up would refuse"
					fi
				elif [ -n "$(uid_owner "$uid")" ]; then
					state="uid $uid is taken by $(uid_owner "$uid") — up would refuse"
				fi
				printf '  %-12s uid %-4s %-38s %s\n' "$name" "$uid" "/Users/$name" "$state"
			fi
		done
	fi

	hdr "NEXT"
	if [ "${MODE:-}" = "dirs" ]; then
		say "  bash scripts/ops/mac-sessions.sh up --mode dirs --count ${COUNT:-N} --apply"
		say "  bash scripts/ops/mac-sessions.sh verify --mode dirs --simulator"
	else
		say "  sudo bash scripts/ops/mac-sessions.sh up --mode accounts --count ${COUNT:-N} --apply"
		say "  sudo bash scripts/ops/mac-sessions.sh verify --mode accounts --simulator"
	fi
	say "  Read docs/operations/mac-sessions.md before the first one."
}

# ---------------------------------------------------------------------------------------------------
# up
# ---------------------------------------------------------------------------------------------------
create_account() { # create_account <index>
	local n="$1" name uid
	name="$(session_name "$n")"
	uid="$(session_uid "$n")"
	say "  creating $name (uid $uid, /Users/$name), no password, non-admin"
	# No -password: the account is created without one, so it is reachable by root (`sudo -u`) and
	# not by any login path. Passing one here would put it in argv, where every user on the box reads
	# it out of `ps`. Post-conditions are checked below rather than trusted.
	sysadminctl -addUser "$name" \
		-fullName "Palai session $(session_id "$n")" \
		-UID "$uid" -GID 20 -shell /bin/zsh -home "/Users/$name" 2>&1 | sed 's/^/    /'
	dscl . -create "/Users/$name" "$MARKER_ATTR" "$(marker_for "$name" "$(date -u +%Y-%m-%dT%H:%M:%SZ)")"
	# -home ASSIGNS a home directory; it does not CREATE one. sysadminctl says so in its own output —
	# "Home directory is assigned (not created!)" — and this script used to read straight past it into a
	# chmod that failed with "No such file or directory", leaving a real account with no home and the run
	# dead on set -e. Found on 2026-08-02 by running it; nothing else could have found it, because every
	# check in this file ran AFTER the step that aborted.
	#
	# createhomedir is the only supported way to populate one from the user template. -c is "this machine",
	# -u names the account.
	createhomedir -c -u "$name" 2>&1 | sed 's/^/    /'
	# AND THEN VERIFY IT, because createhomedir exits 0 on paths it did not create — a mobile-account
	# variant, a full disk, a /Users that is not writable. A directory this script is about to chmod 700
	# and hand a session is one it must have SEEN, not one it asked for.
	if [ ! -d "/Users/$name" ]; then
		die "created account $name but its home /Users/$name does not exist. createhomedir did not populate it, so this session has nowhere to run; remove the account with 'down --mode accounts --apply' before retrying"
	fi
	# The default is drwxr-x--- with group staff, which lets every other session traverse the top
	# level of this one. 700 is the first thing to fix on any shared Mac.
	chmod 700 "/Users/$name"
	load_user_table # the table just changed
}

cmd_up() {
	local n name uid marker refused=0
	[ -n "$COUNT" ] || die "up needs --count N"
	is_macos || die "up runs on macOS only"

	say "palai mac-sessions — UP (mode=$MODE, count=$COUNT)"
	if [ "$APPLY" -eq 0 ]; then
		say "DRY RUN: nothing was changed. Add --apply to act."
	fi

	if [ "$MODE" = "accounts" ]; then
		[ "$APPLY" -eq 0 ] || is_root || die "up --mode accounts --apply must run as root (use sudo)"
		# Refuse the whole run before creating anything if any name or uid is already spoken for by
		# something we did not create. Half a fleet is worse than none.
		for n in $(session_seq 1 "$COUNT"); do
			name="$(session_name "$n")"
			uid="$(session_uid "$n")"
			if ds_exists "$name"; then
				marker="$(ds_marker "$name")"
				if [ -z "$marker" ]; then
					say "  REFUSE $name: an account with this name exists and carries no $MARKER_MAGIC marker."
					refused=1
				elif ! deletion_is_permitted "$name" "$(ds_uid "$name")" "$marker" "$(ds_in_admin "$name")" "" >/dev/null; then
					say "  REFUSE $name: $(deletion_is_permitted "$name" "$(ds_uid "$name")" "$marker" "$(ds_in_admin "$name")" "" || true)"
					refused=1
				fi
			elif [ -n "$(uid_owner "$uid")" ]; then
				say "  REFUSE $name: uid $uid already belongs to $(uid_owner "$uid")."
				refused=1
			fi
		done
		[ "$refused" -eq 0 ] || die "refusing to continue — resolve the collisions above"
	fi

	for n in $(session_seq 1 "$COUNT"); do
		name="$(session_name "$n")"
		if [ "$APPLY" -eq 0 ]; then
			if [ "$MODE" = "accounts" ]; then
				if ds_exists "$name"; then
					say "  would reuse  $name (exists, ours)"
				else
					say "  would create $name uid $(session_uid "$n") /Users/$name, chmod 700, non-admin, no password"
				fi
			else
				say "  would create $(session_root "$n")/{work,devices,derived} chmod 700"
			fi
			continue
		fi
		if [ "$MODE" = "accounts" ] && ! ds_exists "$name"; then
			create_account "$n"
		elif [ "$MODE" = "accounts" ]; then
			say "  reusing $name (exists, ours)"
			chmod 700 "/Users/$name"
		fi
		write_session_tree "$n"
		say "  session $(session_id "$n") ready at $(session_root "$n")"
	done

	if [ "$APPLY" -eq 1 ] && [ "$MODE" = "dirs" ]; then
		chmod 700 "$DIRS_ROOT"
		marker_for "root" "$(date -u +%Y-%m-%dT%H:%M:%SZ)" >"$DIRS_ROOT/$DIRS_MARKER"
	fi

	if [ "$APPLY" -eq 0 ]; then
		say ""
		say "DRY RUN: nothing was changed. Re-run with --apply."
		return 0
	fi

	hdr "POST-CONDITIONS (checked, not assumed)"
	verify_posture
	hdr "NEXT"
	say "  Each session sources $(session_root 1)/session.env."
	if [ "$MODE" = "accounts" ]; then
		say "  These accounts have NO password: usable by root (\`sudo -u $(session_name 1) -i\`), not"
		say "  loginable. A session that must DRIVE a simulator needs an Aqua login — see §5 of"
		say "  docs/operations/mac-sessions.md, then, once per session and interactively:"
		say "      sudo sysadminctl -resetPasswordFor $(session_name 1) -newPassword -"
	fi
	say "  Then prove it: sudo bash scripts/ops/mac-sessions.sh verify --mode $MODE --simulator"
	[ "$fail" -eq 0 ] || exit 1
}

# ---------------------------------------------------------------------------------------------------
# verify
# ---------------------------------------------------------------------------------------------------

# verify_posture: what each session account IS. Every claim in the operator page about the account's
# shape is re-derived here from the live record rather than from the flags we passed at creation.
verify_posture() {
	local n name uid home mode marker
	if [ "$MODE" = "dirs" ]; then
		for n in $(session_seq 1 "${COUNT:-0}"); do
			mode="$(stat -f '%Lp' "$(session_root "$n")" 2>/dev/null || echo none)"
			if [ "$mode" = "700" ]; then
				check PASS "$(session_id "$n") directory is 700" "$(session_root "$n")"
			else
				check FAIL "$(session_id "$n") directory is 700" "mode is $mode"
			fi
		done
		return 0
	fi
	for n in $(session_seq 1 "${COUNT:-0}"); do
		name="$(session_name "$n")"
		if ! ds_exists "$name"; then
			check FAIL "$name exists" "no directory-services record"
			continue
		fi
		uid="$(ds_uid "$name")"
		home="$(ds_home "$name")"
		if [ "$uid" = "$(session_uid "$n")" ]; then
			check PASS "$name has the allocated uid" "$uid"
		else
			check FAIL "$name has the allocated uid" "record says $uid, expected $(session_uid "$n")"
		fi

		mode="$(stat -f '%Lp' "$home" 2>/dev/null || echo none)"
		if [ "$mode" = "700" ]; then
			check PASS "$name home is 700" "$home"
		else
			check FAIL "$name home is 700" "$home is $mode — the macOS default 755/750 lets siblings traverse it"
		fi

		# Verified from the group, not from the -admin flag we did not pass.
		if [ "$(ds_in_admin "$name")" = "no" ]; then
			check PASS "$name is NOT in admin" ""
		else
			check FAIL "$name is NOT in admin" "an admin session has sudo, which defeats every boundary here"
		fi

		marker="$(ds_marker "$name")"
		if deletion_is_permitted "$name" "$uid" "$marker" "$(ds_in_admin "$name")" "$(protected_uids)" >/dev/null 2>&1; then
			check PASS "$name is recognisably ours" "marker binds it to this host"
		else
			check FAIL "$name is recognisably ours" "$(deletion_is_permitted "$name" "$uid" "$marker" "$(ds_in_admin "$name")" "$(protected_uids)" || true)"
		fi

		# An account that authenticates with an empty password is reachable over Screen Sharing by
		# anyone who can see the box. `sysadminctl -addUser` without -password is not documented to
		# leave one, so this is measured every time rather than believed once.
		if dscl . -authonly "$name" "" >/dev/null 2>&1; then
			check FAIL "$name refuses an empty password" "it AUTHENTICATES with an empty password — set one: sudo sysadminctl -resetPasswordFor $name -newPassword -"
		else
			check PASS "$name refuses an empty password" ""
		fi
	done
}

# verify_cross_read is the check the whole design exists for: session B must not be able to read
# session A's files. It is done the only honest way — write a canary as A, try to read that exact
# byte string as B — and it carries its own positive control, because a read that fails for the wrong
# reason (no such path, no such binary) would otherwise look like a boundary.
verify_cross_read() {
	local a b na nb canary out
	if [ "$MODE" != "accounts" ]; then
		check UNVERIFIED "cross-session reads are refused" "mode=dirs shares one uid — there is no boundary to test, by construction"
		return 0
	fi
	if ! is_root; then
		check UNVERIFIED "cross-session reads are refused" "needs root to act as each session — re-run under sudo"
		return 0
	fi
	[ "${COUNT:-0}" -ge 2 ] || {
		check UNVERIFIED "cross-session reads are refused" "needs at least 2 sessions; --count is ${COUNT:-0}"
		return 0
	}

	for a in 1 2; do
		b=$((3 - a))
		na="$(session_name "$a")"
		nb="$(session_name "$b")"
		canary="palai-canary-$(session_id "$a")-$RANDOM$RANDOM"
		sudo -u "$na" -- /bin/sh -c "printf '%s' '$canary' > /Users/$na/.palai-canary && chmod 600 /Users/$na/.palai-canary"

		# Positive control FIRST. If the owner cannot read its own canary the negative result below
		# proves nothing at all.
		out="$(sudo -u "$na" -- /bin/cat "/Users/$na/.palai-canary" 2>/dev/null || true)"
		if [ "$out" != "$canary" ]; then
			check FAIL "$na can read its own canary (control)" "it read '$out' — the negative result below would prove nothing"
			continue
		fi
		check PASS "$na can read its own canary (control)" ""

		out="$(sudo -u "$nb" -- /bin/cat "/Users/$na/.palai-canary" 2>&1 || true)"
		case "$out" in
		*"$canary"*) check FAIL "$nb cannot read $na's home" "IT READ THE CANARY — there is no boundary" ;;
		*) check PASS "$nb cannot read $na's home" "$(printf '%s' "$out" | head -1)" ;;
		esac

		# And the directory listing, because a 700 home whose parent leaks names is still a leak.
		out="$(sudo -u "$nb" -- /bin/ls -a "/Users/$na" 2>&1 || true)"
		case "$out" in
		*".palai-canary"*) check FAIL "$nb cannot list $na's home" "IT LISTED THE HOME" ;;
		*) check PASS "$nb cannot list $na's home" "$(printf '%s' "$out" | head -1)" ;;
		esac
	done
}

# verify_device_sets re-measures the one property session.env depends on: a device created in an
# alternate set is invisible to the default set and to every other session's set.
verify_device_sets() {
	local n set_a udid others pair
	[ "${COUNT:-0}" -ge 1 ] || return 0
	is_macos || {
		check UNVERIFIED "device sets are separate" "not macOS"
		return 0
	}
	set_a="$(device_set 1)"
	pair="$(pick_pair)" || {
		check UNVERIFIED "device sets are separate" "no available iOS runtime with a supported iPhone"
		return 0
	}
	udid="$(as_session 1 xcrun simctl --set "$set_a" create "palai-verify-$(session_id 1)" $pair 2>&1)" || {
		check UNVERIFIED "device sets are separate" "could not create a device: $(printf '%s' "$udid" | head -1)"
		return 0
	}
	if xcrun simctl list devices 2>/dev/null | grep -q "$udid"; then
		check FAIL "the default device set cannot see session s01's device" "$udid is listed in the default set"
	else
		check PASS "the default device set cannot see session s01's device" "$udid"
	fi
	for n in $(session_seq 2 "${COUNT:-0}"); do
		others="$(as_session "$n" xcrun simctl --set "$(device_set "$n")" list devices 2>/dev/null || true)"
		# AN EMPTY LISTING IS NOT A CLEAN SEPARATION. `as_session` fails for a session that does not exist,
		# and simctl prints nothing when it cannot read a set at all — both land in the same default arm as a
		# genuine "not listed" and used to report PASS. A check that passes because it could not run is the
		# thing the header of this file says UNVERIFIED exists for.
		if [ -z "$(printf '%s' "$others" | tr -d '[:space:]')" ]; then
			check UNVERIFIED "$(session_id "$n")'s set cannot see s01's device" "$(session_id "$n") listed no devices at all, so nothing was compared"
			continue
		fi
		case "$others" in
		*"$udid"*) check FAIL "$(session_id "$n")'s set cannot see s01's device" "it is listed" ;;
		*) check PASS "$(session_id "$n")'s set cannot see s01's device" "" ;;
		esac
	done
	as_session 1 xcrun simctl --set "$set_a" delete "$udid" >/dev/null 2>&1 || true
}

# pick_pair prints "<device type id> <runtime id>" for the newest available iOS runtime and a device
# type that runtime actually supports. Do NOT pick these independently: `simctl list devicetypes` is
# not ordered, its last iPhone entry here is the iPhone 6s Plus, and pairing it with iOS 26.5 fails
# with SimError 403 "Incompatible device". The runtimes JSON is the only place the pairing is stated.
# (python3 comes with the Xcode command-line tools, which this script already requires.)
pick_pair() {
	xcrun simctl list runtimes -j 2>/dev/null | python3 -c '
import json, sys
runtimes = [r for r in json.load(sys.stdin)["runtimes"]
            if r.get("isAvailable") and r.get("platform") == "iOS"]
if not runtimes:
    sys.exit(1)
runtimes.sort(key=lambda r: [int(p) for p in r["version"].split(".") if p.isdigit()])
newest = runtimes[-1]
phones = [d for d in newest.get("supportedDeviceTypes", []) if d.get("productFamily") == "iPhone"]
if not phones:
    sys.exit(1)
print(phones[-1]["identifier"], newest["identifier"])
'
}

# verify_aqua answers half of the load-bearing question for free: does this session's uid have an
# Aqua GUI session at all? `launchctl print gui/<uid>` succeeds only when one exists — measured on
# this host: gui/501 (console user) prints, gui/503 and gui/504 (never logged in) return
# "Domain does not support specified action".
verify_aqua() {
	local n uid
	is_macos || return 0
	for n in $(session_seq 1 "${COUNT:-0}"); do
		uid="$(session_uid "$n")"
		[ "$MODE" = "dirs" ] && uid="$(id -u)"
		if launchctl print "gui/$uid" >/dev/null 2>&1; then
			check PASS "$(session_id "$n") has an Aqua session (gui/$uid)" "a simulator can be driven here"
		else
			check UNVERIFIED "$(session_id "$n") has an Aqua session (gui/$uid)" \
				"none — log in at the console or over Screen Sharing before driving a simulator"
		fi
	done
}

# verify_simulator is THE measurement. It is untested anywhere in the prior research whether a
# simulator boots and can be DRIVEN in a non-console (Screen Sharing) Aqua session, and whether two
# users can do it concurrently — and the fleet design (one session per Mac, or eight) turns on the
# answer. So: boot a device in each session's own set, launch a real UIKit app, then DRIVE it by
# flipping the appearance and comparing two screenshots. Two identical screenshots mean nothing
# rendered; a differing pair means the session issued a command and the display changed.
verify_simulator() {
	local n pid pids="" rc=0 log
	if [ "$SIMULATOR" -eq 0 ]; then
		check UNVERIFIED "a simulator boots and can be DRIVEN in this session" \
			"not measured — re-run with --simulator (boots a device per session, ~640 MB each)"
		return 0
	fi
	is_macos || {
		check UNVERIFIED "a simulator boots and can be DRIVEN in this session" "not macOS"
		return 0
	}
	[ "${COUNT:-0}" -ge 1 ] || {
		check UNVERIFIED "a simulator boots and can be DRIVEN in this session" "no sessions exist yet"
		return 0
	}
	say ""
	say "  measuring ${COUNT} session(s) CONCURRENTLY — this is the load-bearing unknown"
	for n in $(session_seq 1 "$COUNT"); do
		drive_one "$n" >"${TMPDIR:-/tmp}/palai-mac-sessions-$(session_id "$n").log" 2>&1 &
		pids="$pids $!"
	done
	for pid in $pids; do
		wait "$pid" || rc=1
	done
	# Re-tally from what the children printed. Their own $fail/$unverified died with their subshells,
	# and a concurrency measurement whose failures do not reach the summary is not a measurement.
	for n in $(session_seq 1 "$COUNT"); do
		log="${TMPDIR:-/tmp}/palai-mac-sessions-$(session_id "$n").log"
		cat "$log"
		fail=$((fail + $(grep -c '^  \[FAIL' "$log" || true)))
		unverified=$((unverified + $(grep -c '^  \[UNVERIFIED' "$log" || true)))
		rm -f "$log"
	done
	[ "$rc" -eq 0 ] || say "  (a session's measurement exited non-zero; see its lines above)"
}

drive_one() { # drive_one <index>
	local n="$1" set_d udid app pid shot_a shot_b sum_a sum_b bytes sid pair
	# `check` tallies into $fail, which is this subshell's copy and dies with it. The parent re-tallies
	# from the log this writes, so nothing here needs to survive except the printed lines.
	sid="$(session_id "$n")"
	set_d="$(device_set "$n")"
	shot_a="$set_d/verify-a.png"
	shot_b="$set_d/verify-b.png"

	pair="$(pick_pair)" || {
		check UNVERIFIED "$sid simulator can be driven" "no available iOS runtime with a supported iPhone"
		return 0
	}
	udid="$(as_session "$n" xcrun simctl --set "$set_d" create "palai-drive-$sid" $pair 2>&1)" || {
		check UNVERIFIED "$sid simulator can be driven" "create failed: $(printf '%s' "$udid" | head -1)"
		return 0
	}
	if ! as_session "$n" xcrun simctl --set "$set_d" bootstatus "$udid" -b >/dev/null 2>&1; then
		check FAIL "$sid simulator boots" "bootstatus did not reach booted"
		as_session "$n" xcrun simctl --set "$set_d" delete "$udid" >/dev/null 2>&1 || true
		return 1
	fi
	check PASS "$sid simulator boots" "$udid"

	app="com.apple.Preferences"
	pid="$(as_session "$n" xcrun simctl --set "$set_d" launch "$udid" "$app" 2>&1 | awk -F': ' '{print $2}')"
	if [ -z "$pid" ]; then
		check FAIL "$sid launches a UIKit app" "$app did not launch"
	else
		check PASS "$sid launches a UIKit app" "$app pid $pid"
	fi

	# Drive it: two commands from this session that must change what the display shows.
	as_session "$n" xcrun simctl --set "$set_d" ui "$udid" appearance dark >/dev/null 2>&1 || true
	sleep 2
	as_session "$n" xcrun simctl --set "$set_d" io "$udid" screenshot "$shot_a" >/dev/null 2>&1 || true
	as_session "$n" xcrun simctl --set "$set_d" ui "$udid" appearance light >/dev/null 2>&1 || true
	sleep 2
	as_session "$n" xcrun simctl --set "$set_d" io "$udid" screenshot "$shot_b" >/dev/null 2>&1 || true

	sum_a="$(shasum -a 256 "$shot_a" 2>/dev/null | awk '{print $1}')"
	sum_b="$(shasum -a 256 "$shot_b" 2>/dev/null | awk '{print $1}')"
	bytes="$(stat -f %z "$shot_a" 2>/dev/null || echo 0)"
	if [ -z "$sum_a" ] || [ -z "$sum_b" ]; then
		check FAIL "$sid simulator can be DRIVEN" "no screenshot came out — nothing is rendering"
	elif [ "$sum_a" = "$sum_b" ]; then
		check FAIL "$sid simulator can be DRIVEN" \
			"the display did not change between dark and light ($bytes bytes, identical) — it boots but does not render"
	else
		check PASS "$sid simulator can be DRIVEN" \
			"appearance flip changed the frame ($bytes bytes, sha ${sum_a:0:8} vs ${sum_b:0:8})"
	fi

	verify_spawn_confinement "$n" "$udid" "$set_d"

	as_session "$n" /bin/rm -f "$shot_a" "$shot_b" 2>/dev/null || true
	as_session "$n" xcrun simctl --set "$set_d" shutdown "$udid" >/dev/null 2>&1 || true
	as_session "$n" xcrun simctl --set "$set_d" delete "$udid" >/dev/null 2>&1 || true
}

# verify_spawn_confinement closes the loop on the argument this whole design rests on. `simctl spawn`
# walked through seatbelt, the App Sandbox and TCC, because the spawned process is parented to
# launchd rather than to the caller. It should NOT walk through a uid, because CoreSimulatorService
# is per user — but "should" is what the rest of that list also had.
verify_spawn_confinement() { # <index> <udid> <set>
	local n="$1" udid="$2" set_d="$3" other out sid
	sid="$(session_id "$n")"
	if [ "$MODE" != "accounts" ] || [ "${COUNT:-0}" -lt 2 ]; then
		check UNVERIFIED "$sid: simctl spawn does not cross the uid" "needs mode=accounts and >=2 sessions"
		return 0
	fi
	other="$(session_name "$((n == 1 ? 2 : 1))")"
	[ -f "/Users/$other/.palai-canary" ] || {
		check UNVERIFIED "$sid: simctl spawn does not cross the uid" "no canary in /Users/$other (run verify as root)"
		return 0
	}
	out="$(as_session "$n" xcrun simctl --set "$set_d" spawn "$udid" /bin/cat "/Users/$other/.palai-canary" 2>&1 || true)"
	case "$out" in
	*palai-canary-*) check FAIL "$sid: simctl spawn does not cross the uid" "IT READ $other's CANARY — the account boundary is not one" ;;
	*) check PASS "$sid: simctl spawn does not cross the uid" "$(printf '%s' "$out" | head -1)" ;;
	esac
}

cmd_verify() {
	is_macos || die "verify runs on macOS only"
	[ -n "$COUNT" ] || COUNT="$(discover_count)"
	say "palai mac-sessions — VERIFY (mode=$MODE, sessions=$COUNT)"
	say "Every line is a check that RAN. UNVERIFIED means it could not run — never that it passed."

	hdr "WHAT EACH SESSION IS"
	verify_posture
	hdr "WHETHER THE BOUNDARY HOLDS"
	verify_cross_read
	hdr "WHETHER THE SESSIONS CAN COLLIDE"
	verify_device_sets
	hdr "WHETHER A SIMULATOR RUNS HERE"
	verify_aqua
	verify_simulator

	hdr "RESULT"
	printf '  %s failed, %s unverified\n' "$fail" "$unverified"
	if [ "$unverified" -gt 0 ]; then
		say "  An UNVERIFIED row is an open question about THIS box, not a pass. The fleet shape"
		say "  (one session per Mac, or several) depends on the two simulator rows above."
	fi
	say ""
	say "  Not in scope of any check here: this is a boundary between SESSIONS OF ONE CUSTOMER."
	say "  sudo and any local-root escalation defeat it. Different customers need different Macs."
	[ "$fail" -eq 0 ] || exit 1
}

# discover_count is the highest session index that already exists, so `verify` does not need --count.
discover_count() {
	local n found=0 name
	if [ "$MODE" = "accounts" ]; then
		while read -r name _; do
			[ -n "$name" ] || continue
			n="${name#"$PREFIX"}"
			if [ "$((10#$n))" -gt "$found" ]; then found="$((10#$n))"; fi
		done <<-EOF
			$(session_records)
		EOF
	else
		for n in $(session_seq 1 "$MAX_SESSIONS"); do
			if [ -d "$DIRS_ROOT/$(session_id "$n")" ]; then found="$n"; fi
		done
	fi
	printf '%s' "$found"
}

# ---------------------------------------------------------------------------------------------------
# down
# ---------------------------------------------------------------------------------------------------
cmd_down() {
	# bash 3.2 is what macOS ships, and an empty array under `set -u` is an error there — so the
	# candidate list is a plain space-separated string of names.
	local n name uid marker why candidates=""
	is_macos || die "down runs on macOS only"
	say "palai mac-sessions — DOWN (mode=$MODE)"

	if [ "$MODE" = "dirs" ]; then
		if [ ! -d "$DIRS_ROOT" ]; then
			say "  nothing to delete: $DIRS_ROOT does not exist."
			say ""
			say "Nothing was changed, with or without --apply."
			return 0
		fi
		# A directory that exists but carries no marker is somebody else's. Refusing is the point:
		# --root takes a path, and a typo in a path is how an operator deletes the wrong tree.
		if [ ! -f "$DIRS_ROOT/$DIRS_MARKER" ]; then
			die "$DIRS_ROOT carries no $DIRS_MARKER — this script did not create it, so it will not delete it"
		fi
		marker="$(cat "$DIRS_ROOT/$DIRS_MARKER")"
		if ! why="$(deletion_is_permitted "$(session_name 1)" "$(session_uid 1)" \
			"${marker/:root:/:$(session_name 1):}" no "" 2>&1)"; then
			die "$DIRS_ROOT's marker does not belong to this host: $why"
		fi
		say "  would delete $DIRS_ROOT ($(du -sh "$DIRS_ROOT" 2>/dev/null | awk '{print $1}')), including every"
		say "  simulator device and every file the sessions left there."
		if [ "$APPLY" -eq 0 ]; then
			say ""
			say "DRY RUN: nothing was changed. Re-run with --apply."
			return 0
		fi
		for n in $(session_seq 1 "$MAX_SESSIONS"); do
			[ -d "$(device_set "$n")" ] || continue
			xcrun simctl --set "$(device_set "$n")" delete all >/dev/null 2>&1 || true
		done
		rm -rf "$DIRS_ROOT"
		say "  deleted $DIRS_ROOT"
		return 0
	fi

	while read -r name uid; do
		[ -n "$name" ] || continue
		marker="$(ds_marker "$name")"
		if ! why="$(deletion_is_permitted "$name" "$uid" "$marker" "$(ds_in_admin "$name")" "$(protected_uids)" 2>&1)"; then
			say "  SKIP   $name (uid $uid): $why"
			continue
		fi
		candidates="$candidates $name"
	done <<-EOF
		$(session_records)
	EOF

	if [ -z "$candidates" ]; then
		say "  nothing to delete: no account matched the marker this script writes."
		say ""
		say "Nothing was changed; --apply would have had nothing to do either."
		return 0
	fi
	say "  would DELETE these accounts and their home directories, irreversibly:"
	for name in $candidates; do
		printf '    %-12s uid %-5s %-24s (%s)\n' \
			"$name" "$(ds_uid "$name")" "$(ds_home "$name")" \
			"$(du -sh "$(ds_home "$name")" 2>/dev/null | awk '{print $1}')"
	done
	if [ "$APPLY" -eq 0 ]; then
		say ""
		say "DRY RUN: nothing was changed. Re-run with --apply."
		return 0
	fi
	is_root || die "down --apply must run as root (use sudo)"

	for name in $candidates; do
		say "  deleting $name"
		sysadminctl -deleteUser "$name" 2>&1 | sed 's/^/    /'
		if ds_exists "$name"; then
			check FAIL "$name is gone" "the record still exists"
		elif [ -d "/Users/$name" ]; then
			check FAIL "$name is gone" "the record went but /Users/$name did not"
		else
			check PASS "$name is gone" "record and home removed"
		fi
	done
	[ "$fail" -eq 0 ] || exit 1
}

# ---------------------------------------------------------------------------------------------------
# selftest — the deletion guard, against a table. This is the only part of the destructive path that
# can be exercised without a Mac and without root, which is exactly why it is where the guard lives.
# ---------------------------------------------------------------------------------------------------
cmd_selftest() {
	local rc=0 huuid good
	huuid="$(host_uuid)"
	[ -n "$huuid" ] || die "selftest needs a host identity; set PALAI_MAC_SESSIONS_HOST_UUID"
	good="$MARKER_MAGIC:$MARKER_VERSION:$huuid:${PREFIX}01:2026-07-28T00:00:00Z"

	say "selftest — the deletion guard, on host identity $huuid"

	case_is() { # case_is <expected PERMIT|REFUSE> <label> <name> <uid> <marker> <in_admin> <protected>
		local expect="$1" label="$2" why=""
		shift 2
		if why="$(deletion_is_permitted "$@" 2>&1)"; then
			if [ "$expect" = "PERMIT" ]; then
				printf '  [ok]   %-7s %s\n' "PERMIT" "$label"
			else
				printf '  [BUG]  %-7s %s — the guard PERMITTED it\n' "$expect" "$label"
				rc=1
			fi
		else
			if [ "$expect" = "REFUSE" ]; then
				printf '  [ok]   %-7s %-52s (%s)\n' "REFUSE" "$label" "$why"
			else
				printf '  [BUG]  %-7s %s — the guard refused it: %s\n' "$expect" "$label" "$why"
				rc=1
			fi
		fi
	}

	case_is PERMIT "an account this script created on this Mac" \
		"${PREFIX}01" $((UID_BASE + 1)) "$good" no "0,501"
	case_is REFUSE "the same name with no marker at all" \
		"${PREFIX}01" $((UID_BASE + 1)) "" no "0,501"
	case_is REFUSE "a marker minted on another Mac" \
		"${PREFIX}01" $((UID_BASE + 1)) "$MARKER_MAGIC:$MARKER_VERSION:SOME-OTHER-MAC:${PREFIX}01:2026-07-28T00:00:00Z" no "0,501"
	case_is REFUSE "a marker for a different session" \
		"${PREFIX}01" $((UID_BASE + 1)) "$MARKER_MAGIC:$MARKER_VERSION:$huuid:${PREFIX}07:2026-07-28T00:00:00Z" no "0,501"
	case_is REFUSE "a uid that does not match the name" \
		"${PREFIX}01" $((UID_BASE + 5)) "$good" no "0,501"
	case_is REFUSE "an account outside our uid range" \
		"${PREFIX}01" 501 "$good" no "0,501"
	case_is REFUSE "an admin account wearing our name" \
		"${PREFIX}01" $((UID_BASE + 1)) "$good" yes "0,501"
	case_is REFUSE "the operator's own account" \
		"${PREFIX}01" $((UID_BASE + 1)) "$good" no "0,501,$((UID_BASE + 1))"
	case_is REFUSE "someone else's account entirely" \
		"konuk" 503 "$good" no "0,501"
	case_is REFUSE "a name with a path in it" \
		"${PREFIX}01/../../etc" $((UID_BASE + 1)) "$good" no "0,501"
	case_is REFUSE "session index zero" \
		"${PREFIX}00" $((UID_BASE + 0)) "$MARKER_MAGIC:$MARKER_VERSION:$huuid:${PREFIX}00:2026-07-28T00:00:00Z" no "0,501"

	say ""
	if [ "$rc" -eq 0 ]; then
		say "selftest: the guard decided every case correctly."
	else
		say "selftest: the guard decided at least one case WRONGLY — do not run \`down\`."
	fi
	return "$rc"
}

# ---------------------------------------------------------------------------------------------------
# argument handling
# ---------------------------------------------------------------------------------------------------
sub="${1:-}"
[ -n "$sub" ] || die "usage: mac-sessions.sh <$(echo "$SUBCOMMANDS" | tr ' ' '|')> [flags]"
shift
case " $SUBCOMMANDS " in
*" $sub "*) ;;
*) die "unknown subcommand $(printf %q "$sub") — one of: $SUBCOMMANDS" ;;
esac

while [ "$#" -gt 0 ]; do
	case "$1" in
	--count)
		COUNT="${2:-}"
		shift 2 || die "--count needs a value"
		;;
	--mode)
		MODE="${2:-}"
		shift 2 || die "--mode needs a value"
		;;
	--root)
		DIRS_ROOT="${2:-}"
		shift 2 || die "--root needs a value"
		;;
	--apply)
		APPLY=1
		shift
		;;
	--simulator)
		SIMULATOR=1
		shift
		;;
	*) die "unknown flag $(printf %q "$1")" ;;
	esac
done

if [ -n "$COUNT" ]; then
	case "$COUNT" in
	'' | *[!0-9]*) die "--count must be a positive integer, got $(printf %q "$COUNT")" ;;
	esac
	if [ "$COUNT" -lt 1 ] || [ "$COUNT" -gt "$MAX_SESSIONS" ]; then
		die "--count must be between 1 and $MAX_SESSIONS, got $COUNT"
	fi
fi
case "${MODE:-}" in
"" | dirs | accounts) ;;
*) die "--mode must be dirs or accounts, got $(printf %q "$MODE")" ;;
esac

load_user_table

case "$sub" in
plan) cmd_plan ;;
selftest) cmd_selftest ;;
*)
	[ -n "$MODE" ] || die "$sub needs --mode dirs or --mode accounts (run \`plan\` first if unsure)"
	case "$sub" in
	up) cmd_up ;;
	verify) cmd_verify ;;
	down) cmd_down ;;
	esac
	;;
esac
