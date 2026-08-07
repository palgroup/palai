# Running several Palai sessions on one Mac

A Mac is where Palai runs when the work needs Apple's tools — Xcode, `xcodebuild`, the iOS
Simulator. This page is about the part that is not obvious: **how to run more than one session on
one machine without them stepping on each other**, and exactly how strong that separation is.

The short version, and the rest of the page is why:

> **Same customer → one Mac is fine.** Per-session directories plus a per-session `simctl --set`,
> no accounts, no root. It stops sessions clobbering each other, which is the real failure.
>
> **Different customers → different Macs.** Not different accounts, different *Macs*. Nothing
> weaker survived measurement, including Apple's own supported sandbox.

Everything here is driven by `scripts/ops/mac-sessions.sh`. Run `plan` first; it changes nothing
and it will tell you what this particular box can hold.

---

## 1. Before you start

- **One Xcode version per Mac.** `CoreSimulatorService` knows about one Xcode at a time, and
  switching kills every booted simulator. A Mac serving sessions is a Mac on one toolchain.
- **RAM is the ceiling, not CPU.** A booted device measured 640 MB RSS and ~179 processes, and RSS
  is a floor rather than a budget. `plan` computes the ceiling for your box; believe it.
- **`accounts` mode needs root.** `dirs` mode does not.

```sh
bash scripts/ops/mac-sessions.sh plan --count 4
```

## 2. Pick a mode, and the script will not pick for you

| Mode | What separates sessions | Needs root | Boundary between customers? |
|---|---|---|---|
| `dirs` | a directory per session + `simctl --set` per session | no | **no** |
| `accounts` | one non-admin macOS account (uid) per session | yes | **no** — see §5 |

`dirs` is the right default when every session belongs to one customer. It is **accident
prevention**: two sessions cannot boot, wipe or rename each other's simulators, and they do not
write into each other's working directories. It is not security — any process running as your uid
can point `--set` at another session's device set, and nothing stops it.

`accounts` is the only mechanism measured to survive the escape that defeats everything else (§5).
Choose it when you want density for one customer and are willing to run a login session per slot.

## 3. Bring sessions up

```sh
# no root, no accounts — same-customer separation
bash scripts/ops/mac-sessions.sh up --mode dirs --count 4 --apply

# one non-admin account per session
sudo bash scripts/ops/mac-sessions.sh up --mode accounts --count 4 --apply
```

Both are idempotent: running the same command twice does not create eight of anything. Nothing is
destructive without `--apply`, and `--apply` is a flag rather than a prompt because these scripts
run unattended.

## 3b. Tell the control plane it may use them — the step that was missing from this page

**Sections 3 and 4 create the accounts and prove they are isolated from each other. Neither makes the
control plane USE one.** Until the two steps below are done, `mac-sessions.sh verify` passes, four accounts
exist, and every session's commands still run as the uid the control plane itself runs as — which §7 is
explicit is *not* a boundary. This page omitted them until 2026-08-07; the omission is written down rather
than quietly filled because the shape it produces — a green verify over an unused mechanism — is exactly
what §4's own title warns about.

**One. Grant the wrapper, and read the grant first.** The privileged surface is
`scripts/ops/palai-session-account`: two verbs, one two-digit index, no pass-through. It PRINTS the sudoers
line rather than writing it, because a file that grants passwordless root is one an operator reads before
it exists.

```sh
scripts/ops/palai-session-account install-sudoers "$(id -un)"
# read the line it prints, then:
sudo tee /etc/sudoers.d/palai-session-account   # paste it, then Ctrl-D
sudo chmod 0440 /etc/sudoers.d/palai-session-account
sudo visudo -cf /etc/sudoers.d/palai-session-account   # must say "parsed OK"
```

**Two. Point the control plane at it.** `PALAI_SESSION_ACCOUNT_HELPER` is an absolute path, and its default
is unset — which the deployment surface reports in those words: *"no privileged path exists; every session's
commands run as the agent's own uid, which is same-customer accident isolation and NOT a boundary"*.

```sh
PALAI_SESSION_ACCOUNT_HELPER=/abs/path/to/scripts/ops/palai-session-account   # then restart the control plane
```

**‼️ SET IT ONLY AFTER STEP ONE.** The variable is what makes the control plane REQUIRE an account per
session; with it set and the sudoers entry missing, `sudo -n` fails immediately (that `-n` is deliberate — a
prompt would hang a process with no terminal) and every shell command fails closed. That is the correct
direction to fail in, and it is still an outage if the two steps are done out of order.

**Three. Prove it end to end, on a real run.** The accounts existing and the plane using them are two
different claims, and this is the one that measures the second:

```sh
make test-live-ios-demo ARGS=--with-accounts
```

‼️ **AND READ WHAT THAT LEG ACTUALLY ASSERTS, BECAUSE IT IS NARROWER THAN ITS HEADING.** It counts
`palai-s*` accounts before the run, after it and after the close: the count must RISE when a session starts
and FALL when it ends. That proves the control plane reached the wrapper and released what it took — which
is the half that is unreachable without step two, and the half that fails loudly when the sudoers entry is
missing (`is PALAI_SESSION_ACCOUNT_HELPER set on the control plane?`).

**What it does NOT yet assert is that the session's commands RAN as that account.** "An account was created"
and "the command ran as it" are two claims, and only the second one is the boundary. Until a leg drives
`id -un` through the session's own shell tool and requires `palai-s`, this page claims the first. Without
`--with-accounts` at all, the same smoke passes on a deployment with no boundary whatsoever, which is why
the flag exists.

## 4. Verify — because a boundary you did not test is a boundary you do not have

```sh
sudo bash scripts/ops/mac-sessions.sh verify --mode accounts --simulator
```

`verify` does not read configuration and report it back. It attempts the things that would break
the separation and requires them to fail: a cross-session read, a cross-session write, and (with
`--simulator`) whether a simulator boots and can be driven in that session at all.

**`--simulator` answers the one question the fleet design depends on**, and it is genuinely open:
whether a simulator boots and accepts input in a **non-console** (Screen Sharing) login session, and
whether two accounts can do it at the same time. Recording does not need an Aqua window; driving
does. Until you run this on your own hardware, treat "8 sessions per Mac" as a hope and "1 session
per Mac" as the number you can defend.

### 4.1 Checking a torn-down session left nothing — and the tool that lies about it

`down` deletes the account and its home. If you want to confirm a tenant's name is gone from this
machine, **the one thing you must not use is a bare `grep`.**

```sh
# WRONG — on a shell where `grep` is ugrep (Claude Code's snapshot wraps it with -I), this skips
# binaries silently and reports a clean machine while the tenant's compiled binary sits right there.
grep -rl 'AcmeCorpApp' ~/Library

# RIGHT
/usr/bin/grep -ral 'AcmeCorpApp' ~/Library/Developer ~/Library/Logs ~/Library/Caches
```

Measured 2026-08-05 (`palai-cloud docs/measurements/faz-a5-residue.md` §6 T3): the first form
answered "0 files carry the tenant string" while the tenant's source line was inside the installed
simulator binary. `-a` is what makes `grep` read a binary as text; `strings` is the other honest form.

Two more rules from the same measurement, both of which produce a **false clean**:

- **Decode before you scan.** A raw byte scan over compressed or encoded output cannot tell you what
  is in it. Measured 2026-08-05 with a tenant marker:
  `/usr/bin/grep -ral <marker> /private/var/db/diagnostics` (2.1 GB of `.tracev3`) answered **3
  files** — which files hold the bytes, and nothing about what was logged — while
  `/usr/bin/log show --start … --predicate 'eventMessage CONTAINS "<marker>"'` over a fifteen-minute
  window answered **4 messages**: `kernel` and `WindowManager` lines naming the app, its device UDID
  and the full path of its installed binary. `.gz` and binary plists are the same shape (`gunzip`,
  `plutil -p`). **And use `/usr/bin/log`, not `log`:** `log` is a zsh builtin and answers
  `too many arguments` without ever reaching the unified log.
- **`timeout` does not exist on macOS, and a missing tool returns NOTHING rather than failing.** The
  raw figure above read `0` until it was re-run: the command had been written
  `timeout 120 /usr/bin/grep …`, the shell answered `127`, no scan ran, and the empty output read as
  a clean machine. Check the exit code of the scan itself, not of the pipeline it was in — and on
  zsh `${PIPESTATUS[0]}` is empty, so do not put a scan you intend to trust behind a pipe.
- **The system unified log is a surface no teardown reaches.** It is root-owned and machine-wide, so
  deleting an account does not touch it, and the only eraser macOS offers (`log erase`) takes every
  tenant's history at once. Treat what a run writes there as bounded by rollover, not by teardown.
- **`find -newermt` reads LOCAL time on the BSD `find` that `sudo` puts on your PATH.** Passing
  `date -u` output silently widens the window by your UTC offset. Use
  `T0=$(date -v-1S +'%Y-%m-%d %H:%M:%S')` — no `-u` — and probe the format with two files, one touched
  hours ago and one now, before trusting a count.

**And the ceiling, which is a limit and not an omission: 131 paths under a home directory cannot be
scanned at all** — `Library/Containers`, `Group Containers`, `Mail`, `Messages`, `Safari`, `Desktop`,
`Mobile Documents`, `Photos Library`, `Daemon Containers`. They are TCC-protected and refuse **root**
without Full Disk Access. A scan that comes back empty has not shown those are clean; it has shown it
could not look. Source: same measurement, §1.

## 5. What is guaranteed, and where each claim comes from

| Claim | Holds? | source |
|---|---|---|
| A separate uid stops another session reading your files | yes | `docs/research/macos-isolation-without-accounts.md` §2 T19 (same-uid read/write of another app's container succeeded; separate uid is the mechanism that does not) |
| `simctl --set` keeps device sets apart | yes, against accidents | `macos-isolation-without-accounts.md` §6 T21 — a device in an alternate set is invisible to the default set |
| `simctl --set` is a security boundary | **no** | same doc, T22 — any same-uid process can point `--set` elsewhere |
| A hidden or encrypted volume separates sessions | **no** | `macos-isolation-without-accounts.md` §2 T4 (`chflags hidden` still lists), T5/T8 (a mounted sparsebundle is readable by every same-uid process) |
| A deny-ACL or `uchg` flag separates sessions | **no** | same doc, T2/T3 — the owning uid removes either with one command |
| Apple's App Sandbox contains a session | **no** | same doc, T14 — a signed, entitled App Sandbox was escaped via `simctl spawn`; T15 shows why (the spawned process's parent is `launchd_sim`, so the agent's process tree is severed) |
| TCC protects one session's files from another | **no** | same doc, T17 — a binary denied `opendir("~/Desktop")` listed real filenames through `simctl spawn` |
| A macOS VM per session | yes, but capped | `docs/research/macos-session-isolation.md` — two concurrent guests per host, by kernel quota **and** licence, and the licence excludes service-bureau use |
| Different customers can share a Mac | **no** | `macos-session-isolation.md` §4 — `sudo` and any local-root escalation defeat a uid boundary; three such escalations were patched in 2026 alone |
| A simulator can be driven in a non-console login session | **MEASURED 2026-08-02 — yes, and with NO login session at all** | `verify --mode accounts --simulator` on this host (M2 Pro, macOS 26.3, Xcode 26.6). `palai-s01` had **no Aqua session** — the `gui/701` row reported UNVERIFIED "none" in the same run — and its simulator booted, launched a UIKit app (`com.apple.Preferences` pid 20277) and was **driven**: an appearance flip changed the rendered frame (29,635 bytes, sha `d2696dd6` vs `c0835d0f`). This row previously read "driving is known to need one"; that belief did not survive being run. **Re-measure on different hardware before carrying it** — this is one box, and a ceiling is dated |
| Two accounts can drive simulators concurrently | **UNVERIFIED** | same. `simdiskimaged` is the one shared root process and is an untested single point of failure |

## 6. When it doesn't work

**`plan` says the ceiling is 1 and I asked for 4.**
Believe it. The number is RAM ÷ per-session budget, and a booted simulator's 640 MB is a floor. Past
the ceiling you get swap, then a machine that stops responding — which is how Docker Desktop was
wedged on this very box on 2026-07-28.

**A session's `xcodebuild` cannot find the SDK, or `simctl` returns `Invalid device`.**
Almost always the one-Xcode rule. Check `xcode-select -p` in the session and compare it to the
host's. Switching Xcode versions kills booted devices in every session, not just yours.

**Simulator boots but does not respond to taps over Screen Sharing.**
This is the unverified row in §5, not a misconfiguration. Recording works without an Aqua window;
driving needs one. Run `verify --simulator` and record what you get — that result is what decides
whether this Mac holds one session or several.

**`up --mode accounts` fails with a permission error.**
It needs root. `dirs` mode does not — if you are only separating one customer's sessions, use it and
skip the account machinery entirely.

**`down` refuses to remove an account.**
By design, and the refusal is the feature. Tear down with:

```sh
sudo bash scripts/ops/mac-sessions.sh down --mode accounts --apply
```

It removes only accounts this script created and recorded, never one that merely matches the name
pattern, and never the console user — a prefix is not proof of ownership, and an operator can have
a `palai-` account of their own. That ownership check is itself tested against a table of tricky
cases, and you can run that test without touching a single account:

```sh
bash scripts/ops/mac-sessions.sh selftest
```

If `down` refuses an account you did create by hand, remove it by hand. That is the correct
outcome: this script deletes only what it can prove it made.

**Two sessions are fighting over the same simulator.**
They are not using separate device sets. Check that each session's `SIMCTL_CHILD_*` environment and
`--set` path differ; `verify` reports this.

## 7. What this is not

It is not multi-tenancy on one Mac. Palai's control plane is multi-tenant at the database layer
(row-level security, migration 000029, `tests/security/tenancy`), but a Mac is not: the separation
here is a uid, and a uid falls to `sudo`. **Different customers need different Macs.** Accounts buy
density for one customer, and that is the whole of what they buy.

Both research documents are worth reading before you design a fleet on top of this:
`docs/research/macos-session-isolation.md` for why VMs and `sandbox-exec` are out, and
`docs/research/macos-isolation-without-accounts.md` for the 22 measurements behind the table in §5.
