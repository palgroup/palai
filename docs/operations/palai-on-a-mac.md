# Palai on a Mac

A Mac is not a Palai feature. It is a **deployment**. The control-plane binary compiles for
`darwin/arm64` (25.6 MB, no `//go:build linux` anywhere under `apps/control-plane` or `packages`), so
the thing that goes to a Mac is **the stack itself** — not a protocol, not a worker, not a typed
`xcode-simulator` capability.

What follows from that is the whole idea, and it is worth stating flatly:

> **The agent's capabilities are whatever the machine it runs on has.** `xcodebuild`, `xcrun simctl`
> and `axe` are binaries on a `PATH`. Palai runs an argv. It knows nothing about iOS, and adding an
> iOS operation to it would be adding a thing that already works.

This page has two readers. An **operator** needs §1–§4: how to run it, what posture it declares, and
what that posture costs. A **model** needs §5: the host facts that decide whether a session works,
every one of them measured on this machine rather than recalled.

---

## 1. The posture, and the boundary that is gone

The shell tool runs in one of two postures, and a deployment declares which:

| Variable | Posture | Boundary |
|---|---|---|
| `PALAI_SANDBOX_IMAGE=<pinned digest>` | Container | unprivileged uid, **no network**, read-only rootfs, all capabilities dropped, cgroup memory/pid/cpu bounds, process-group destroy |
| `PALAI_SHELL_NATIVE=unsandboxed-host` | **Host** | **the uid.** Nothing else. |

In the host posture **the boundary is the uid**, and there is nothing behind it.

Setting **both is fatal at boot**. There is no "sometimes sandboxed" state, because in that state
nobody can read off a deployment where a given call ran
(`TestShellPostureRefusesBothSandboxImageAndNativeHost`).

`PALAI_SHELL_NATIVE=1` is **refused**. Only the exact string `unsandboxed-host` is accepted, and it
is a sentence rather than a boolean on purpose: switching off a security boundary should not be
reachable by the reflex that switches on a feature, and `ps`, `docker inspect` and an env dump should
say what the posture *is* (`TestShellPostureAcceptsOnlyTheStringThatSaysWhatItIs`).

A stack in the host posture prints **one line** at boot, before anything else:

```
shell posture: UNSANDBOXED HOST — commands run as this uid with no container boundary, no network
denial and no resource bound; different customers MUST use different Macs
(docs/research/macos-isolation-without-accounts.md §6, docs/operations/palai-on-a-mac.md)
```

**What is actually gone**, stated without softening:

- **No filesystem boundary.** The command runs as the control plane's own uid, in the control plane's
  own filesystem. `docs/research/macos-isolation-without-accounts.md` measured this on this machine
  (§2, 23 measurements): under one uid nothing weaker is a boundary — Apple's **supported** App
  Sandbox was escaped with `simctl spawn`.
- **No egress denial.** In the container the sandbox denied all network traffic and
  `ClassifyEgress` was an *audit record on top of* that denial. On the host the finding remains and
  **the denial does not**: a `curl` in an argv really leaves the machine. The finding is kept
  precisely so the audit trail does not quietly get shorter.
- **No resource bound.** No memory ceiling, no pid ceiling, no CPU share. `OOMKilled` is therefore
  always `false` in this posture — reporting an OOM nobody observed would be worse than reporting
  none.

**What survives, because it is a property of the result rather than of the container:** bounded
output (1 MiB stdout / 64 KiB stderr) with a truncation flag, secret redaction over the captured
bytes, wall-time expiry classified as `TimedOut`, and a **process-group kill** so a reaped
`xcodebuild` leaves no compiler running.

**And one thing is refused rather than faked:** a `ReadOnly` attempt. That was a read-only bind
mount; a host has no equivalent, so the runner refuses the call instead of running it writable under
a read-only name (`TestHostShellRefusesAReadOnlyAttempt`).

**The wall time is the only bound this posture has, so it is the only one worth configuring.**

| Variable | Default | What it does |
|---|---|---|
| `PALAI_SANDBOX_WALL_TIME` | **`10m`** | Kills one shell call — and its whole process group — after this long. |

Ten minutes is **measured, not picked.** On an M-series Mac with Xcode 26.6 (2026-07-30):
`xcodebuild -version` takes **3.15s**; merely *listing* the schemes of a real iOS project with its
packages already cached takes **12.3s**; a clean `swift build` of one trivial SwiftPM target takes
**59s**; and a clean `xcodebuild build` of a single iOS framework scheme with SPM dependencies takes
**3m32s** — at 19% CPU, so contention rather than compute dominates and a busy Mac is slower still.
Anything in the tens of seconds would kill a command that only prints a version number.

It is **a backstop against a hang, not a schedule** — a build wedged on a stuck
`CoreSimulatorService` is what it exists to reap. If your real builds run longer, set the variable;
it is deliberately far above the tree's other timeouts (`PALAI_STACK_READY_TIMEOUT` 90s,
`PALAI_DRAIN_TIMEOUT` 20s) because those bound work *Palai* does and this one bounds whatever
toolchain your argv names.

**Before this default existed the variable was set in no shipped file, and unset meant this posture
ran shell commands with no wall time at all** — a hung `xcodebuild` held the attempt forever. The
model is told when the cap is hit: the tool result carries `timed_out: true` and `signal: "KILL"`,
never an indistinguishable failure (`TestAWallTimeExpiryTellsTheModelWhatHappened`).

### 1.1 The environment is an allow-list

This is the sharpest edge of the swap. In a container the agent's shell inherited **nothing**. On the
host it is a child of the control-plane process, so without an explicit list it would inherit the
operator's own environment: `SLACK_BOT_TOKEN`, `PALAI_GITHUB_APP_*`, the master key, cloud
credentials.

The command receives exactly these six, and **nothing else** — three **inherited** from the control
plane, three **derived** from the run's own workspace allocation:

```
inherited:  PATH  LANG  DEVELOPER_DIR
derived:    HOME  TMPDIR  PALAI_SIMCTL_SET
```

The inherited half is built *from* that list rather than filtered *against* a deny-list, so a
variable nobody thought of cannot arrive (`TestHostShellDropsTheOperatorsEnvironment` runs `env` and
fails on any name outside the list). A variable unset on the control plane is unset for the command
too — never defaulted.

The derived half is §2's per-session separation, and it points at
`<allocation>/.palai-session/{home,tmp,simulators}`. Two runs at once get two of each
(`TestHostShellGivesConcurrentAllocationsDisjointSessionDirectories`,
`TestNativeShellPostureSeparatesConcurrentSessionsOnOneMac`). They are **created by the runner**, so
a variable never points at a directory that does not exist; a directory that cannot be created is an
error rather than a fallback to the operator's own.

Two consequences an operator should expect, both deliberate:

- **The control plane's own `PATH` is the agent's `PATH`.** A LaunchAgent gets a minimal one, so
  Homebrew tools (`axe`) are missing unless you set it. See §3.
- **The agent's `git` has no `~/.gitconfig`**, because `HOME` is no longer yours. `git commit` in a
  workspace therefore fails with *"Author identity unknown"* unless the argv sets one
  (`git -c user.name=… -c user.email=…`). That is the correct outcome — a run should not commit as
  the operator — and publishing does not go through this path anyway: `push`/`pull_request` are the
  repositories adapter, which carries its own identity.

## 2. The operating rule

Not code. An operating rule, and it is the only thing between two customers on one machine:

> **Different customers → different Macs (or different uids). Same customer → one Mac, per-session directories plus `simctl --set`.**

Source: `docs/research/macos-isolation-without-accounts.md` §6 (measured 2026-07-27/28). The
per-session half is accident-prevention, **not a security boundary** — any process under the same uid
can point `--set` at another session's device set (research T22). `docs/operations/mac-sessions.md`
is the operator page for the density half of it, and `docs/operations/known-gaps-1.0.md` carries the
same sentence as row `MAC-P6`.

### 2.1 What the runner separates, and what it cannot — MEASURED 2026-07-28

The second half of that rule is now partly mechanical. Every run already had its own workspace
allocation and ran there as its working directory; what it *also* had, until E22 T2, was the control
plane's own `HOME` and `TMPDIR` — so two concurrent runs shared one home directory, one DerivedData,
one set of toolchain caches, and the operator's `~/.ssh` and `~/.gitconfig` sat in it. Now each run
gets `<allocation>/.palai-session/{home,tmp,simulators}` (§1.1).

**The question that decided the design (E22 X20) was measured before anything was built, and the
answer was not the convenient one.**

> **Does `CoreSimulatorService` resolve the device set from the calling process's `HOME`?**
> **No.** Measured on this machine on **2026-07-28** (macOS 26.3 / Xcode 26.6): a device created from
> a shell whose `HOME` was a scratch directory landed in the **default** set
> (`~/Library/Developer/CoreSimulator/Devices`), was listed identically from both `HOME`s, and left
> **nothing at all** under the scratch directory (`find` → 0 entries).

The mechanism is why it will not change: `simctl` is a thin client. The device set belongs to
`com.apple.CoreSimulator.CoreSimulatorService`, a **launchd-managed per-user XPC service** that is
already running under the login session's own `HOME` — the calling process's `HOME` never reaches
it.

So the free version of this isolation does not exist, and the fallback is the one the research
measured (T21): `simctl --set <dir>` partitions device sets cleanly, in both directions. **But
`--set` is an argv flag, and argv belongs to the model.** The runner rewrites no argv, so it can
*offer* a per-session device set and can never *enforce* one. That is stated in the name of the
proof rather than in a comment: **`TestSimctlSetIsAdvisoryNotEnforced`**, which re-runs the X20
measurement rather than recalling it, so the day `HOME` starts partitioning device sets the test
fails and this section is what gets fixed.

**What an agent must therefore do** — this is §5's rule, restated here because it is the operator's
concern too:

```
xcrun simctl --set "$PALAI_SIMCTL_SET" create "run-device" "iPhone 16"
xcrun simctl --set "$PALAI_SIMCTL_SET" bootstatus <udid> -b
```

Omit `--set` and the device goes into the machine-wide default set, where another run can boot, wipe
or rename it. Nothing stops that; nothing can.

Two ceilings beyond the boundary one, both unmeasured rather than solved:

- **Concurrency is subject to whatever the driving constraint turns out to be.** Whether two runs can
  `axe tap` at the same time on one Mac **has not been measured**, and this epic does not measure it.
  Until it is, treat "one driving run per Mac" as the number you can defend.
- **One Xcode per Mac** (§5). `CoreSimulatorService` knows one active Xcode; switching it kills
  booted simulators in every session at once, `--set` or not.

## 3. Launch context — MEASURED 2026-07-28, and the answer was not the expected one

The open question (E22 X21) was whether the control plane must run in a **logged-in user's GUI
session** to drive a simulator, with a LaunchAgent named in advance as the fallback. The probe ran the
same three checks in two launch contexts on this machine (macOS 26.3 / Xcode 26.6 / AXe 1.7.0), with
no `sudo`:

| | (a) Terminal, `launchctl managername = Aqua` | (b) cron job, `launchctl managername = Background` |
|---|---|---|
| `xcrun simctl bootstatus <udid> -b` | ✅ | ✅ |
| `xcrun simctl io <udid> screenshot` | ✅ | ✅ |
| `open -a Simulator --args -CurrentDeviceUDID` | ✅ (app appeared) | ✅ (app appeared) |
| `axe describe-ui` / `axe tap` after that | ✅ | ✅ |

Context (b) is a **cron job**: cron runs under a LaunchDaemon in the system bootstrap namespace, so
its children have **no Aqua session** — the same situation a LaunchDaemon or an `ssh` login gives.
`ssh` itself was not usable here (Remote Login is off and enabling it needs `sudo`), and
`launchctl bootstrap user/$UID` refuses without root (`Bootstrap failed: 5: Input/output error`), so
cron is what supplied the Aqua-less context.

**Result: the launch context is not the discriminator.** Both contexts drove the simulator
identically. What *is* the discriminator is **time**, and finding that corrected the belief this task
started from:

- `xcrun simctl bootstatus <udid> -b` returned after **8 s**.
- `axe describe-ui` **immediately** after that: `Error: No translation object returned for simulator`.
- **7 s later** the same command returned the tree — **with no Simulator.app window open at all**.

So E22 §3.5 **X5 is wrong as stated**: driving does not need an Aqua window. It needs the device's
accessibility translation service, which comes up *after* `bootstatus` reports the device booted. The
original X5 experiment opened a window and waited 12 s; the wait was doing the work.

**What is therefore still untested:** a Mac with **nobody logged in graphically at all**. Both
contexts above ran while a user was logged in. `open -a Simulator` in context (b) launched the app
into that logged-in session — with no GUI login there is no session to launch into. Since no window
is needed, this is likely moot; it is recorded as unmeasured rather than assumed.

**Recommendation, unchanged in shape and now cheaper:** run the control plane as a **LaunchAgent** of
a logged-in user. Not because a daemon cannot drive a simulator — it can — but because a LaunchAgent
inherits a sane per-user environment and keeps `HOME` pointing at the user whose
`~/Library/Developer/CoreSimulator` holds the devices.

```xml
<!-- ~/Library/LaunchAgents/net.example.palai-control-plane.plist -->
<key>ProgramArguments</key>
<array><string>/usr/local/bin/palai-control-plane</string></array>
<key>EnvironmentVariables</key>
<dict>
  <!-- The agent's PATH is this PATH. A LaunchAgent's default has no /opt/homebrew/bin, so `axe`
       would be missing and every drive call would honestly fail with 127. -->
  <key>PATH</key><string>/usr/bin:/bin:/usr/sbin:/sbin:/usr/local/bin:/opt/homebrew/bin</string>
  <key>PALAI_SHELL_NATIVE</key><string>unsandboxed-host</string>
  <key>PALAI_WORKSPACE_ROOT</key><string>/Users/&lt;user&gt;/palai/workspaces</string>
</dict>
<key>RunAtLoad</key><true/>
```

`launchctl bootstrap gui/$(id -u) ~/Library/LaunchAgents/net.example.palai-control-plane.plist`.

## 4. Bringing it up — one command

Postgres, the object store and the **runner** stay in containers; only the control plane goes native.

```sh
PALAI_WORKSPACE_ROOT=/absolute/host/path \
PALAI_SHELL_NATIVE=unsandboxed-host \
palai up --native
```

`--native` is the whole posture selector, and it selects **where the control plane runs** — nothing
else. `PALAI_SHELL_NATIVE` is separate and stays the operator's own sentence: switching off a
security boundary must not be reachable by the reflex that switches on a feature (§1). A
`--native` bring-up with no posture declared comes up fine, prints a WARNING, and every
`xcodebuild` call fails cleanly for want of a runner — which is honest, and would be baffling if
nobody said it.

The compose half of that command is the overlay, if you would rather drive it by hand:

```sh
docker compose -f deploy/compose/compose.yaml -f deploy/compose/native-control-plane.yml up -d
```

The overlay does three things, and each is a fact you would otherwise rediscover the hard way —
`palai up --native` now **checks all three before it touches Docker** and refuses by the name of the
variable that is wrong (`nativeRunnerListen`, `nativeWorkspaceRoot` in
`cmd/cli/internal/stack/native.go`), because each one used to surface twenty seconds later as a TLS
or DNS error inside a container nobody was reading:

- **The in-compose control plane moves into a profile**, so it is not started. It is still there
  behind `--profile container-control-plane`, which is the A/B you want when something breaks.
- **The runner reaches the native control plane by the name its certificate already carries.** The
  stack CA mints exactly one SAN — `control-plane` — and the runner pins exactly one, so the fix is
  DNS rather than certificates: the overlay aliases `control-plane` to `host-gateway`. Two
  consequences for you: the native control plane must bind its runner listener on a **routable**
  interface (`PALAI_RUNNER_LISTEN_ADDR=":8443"`, not `127.0.0.1:8443`), and the URL's port must be
  the port it binds (`PALAI_RUNNER_PORT`).
- **The workspace is bound at the same absolute path on both sides.** `PALAI_WORKSPACE_ROOT` is a
  host absolute path; the control plane hands the runner that path, and the runner hands it to the
  Docker daemon as a bind source the daemon resolves on the host. Because the control plane is
  native, its own path *is* the host path — the trap the split deployment had (a named volume the
  daemon cannot resolve) does not arise, but source and target must still be identical.

**One refusal belongs to the posture rather than to the overlay: `PALAI_WORKSPACE_ROOT` may not sit
under a world-writable sticky directory.** `/private/tmp` and `/Users/Shared` are both `drwxrwxrwt`,
and in the native posture every run's workspace, `HOME`, `TMPDIR` and CoreSimulator device set live
under this root — there, **any local account**, not merely this uid, can create and replace paths
beside them, which is a rung below the only boundary this posture has (§1). The bring-up refuses it
and names the offending directory. The check walks the **resolved** path, because `/tmp` is a symlink
to `/private/tmp` and a check that reads the name it was handed is one an everyday alias walks
straight through (`TestNativeWorkspaceRootRefusesAWorldWritableParent`,
`docs/research/macos-isolation-without-accounts.md` §6).

A fourth fact belongs to the bring-up rather than the overlay, and it is the one that bit hardest:
**the runner must start LAST.** The overlay resets its `depends_on`, so compose has nothing left to
make it wait, and `cmd/runner/main.go` `log.Fatalf`s on a failed enroll with no restart policy behind
it — a runner started before the control plane listens is a *dead container*, not a retrying one.
`UpNative` therefore starts Postgres and the object store, then the control plane, then the runner.

### 4.1 It was brought up, on this machine, on 2026-07-28

The paragraph that used to sit here said a full bring-up "has not been run". It has now.

```
$ palai up --native --env-file …
[1/6] env       …
[2/6] provider  selector provider-one — credential from OPENAI_API_KEY, written to the 0600 file secret
[3/6] stack     NATIVE: postgres + object-store + runner in docker, control plane on this machine
 Container palai-304e4be4-postgres-1      Healthy
 Container palai-304e4be4-object-store-1  Healthy
 runner  Built
 Container palai-304e4be4-runner-1        Healthy
stack up: api http://127.0.0.1:54787 (native control plane, pid 91593), runner :54788
[4/6] health    doctor: 14/15 green — NOT green: disk: data dir 6.7% free … (PalaiDiskLow)
[5/6] proof     one real single-step run...

PROVEN LIVE
  round-trip   resp_54dd8e34c1ff6c3fcaa3596a19a6354f -> completed
  model        gpt-4o-mini-2024-07-18   (selector provider-one — NOT the fake adapter)
  usage        46 in / 1 out / 47 total tokens
  api          http://127.0.0.1:54787
  posture      NATIVE control plane, pid 91593, log …/.palai/control-plane.log — postgres/object-store/runner in Docker (native-control-plane.yml)
```

The one red check is the host's own disk, not the posture (it is red for the container stack on this
machine too). The control plane's first log line was the §1 declaration, and `lsof` says the runner
listener is on the wildcard rather than loopback — fact 1, in the only form that counts:

```
$ lsof -nP -iTCP:54788 -sTCP:LISTEN
palai-con 91593 salih 6u IPv6 … TCP *:54788 (LISTEN)
$ palai local doctor | grep runner_identity
runner_identity  ok  runner runner-local.runners.palai.internal identity valid for another 3m32s, 1 session(s) connected
```

That line is a **container** that enrolled with a **native** process by the one name its certificate
carries, through `control-plane:host-gateway`.

**Then the point of the whole thing.** A run bound to a repository, on an agent revision granting
`palai.workspace.shell`, asked to run `xcodebuild -version`. The model chose the argv; the durable
tool ledger recorded what the machine answered:

```
name      | palai.workspace.shell
state     | completed
arguments | {"argv": ["xcodebuild", "-version"]}
result    | {"stdout": "Xcode 26.6\nBuild version 17F113\n", "stderr": "", "exit_code": 0,
             "timed_out": false, "truncated": false, "oom_killed": false, "duration_ms": 1374}
```

and the response the API served was that stdout, verbatim. The same argv inside this same stack's
runner container:

```
$ docker exec palai-304e4be4-runner-1 sh -c 'xcodebuild -version'
sh: xcodebuild: not found
```

`oom_killed: false` is not a claim of good behaviour: in this posture there is no memory ceiling to
be killed by, which §1 says out loud.

**Teardown left nothing.** `palai local down` printed `stopped the native control plane`, and
afterwards: 0 containers, the pid record gone, the process gone, `:54788` free. A second
`palai up --native` over a running one printed `stopped the control plane a previous bring-up left
running` and there was exactly one control-plane process afterwards. `palai local reset --confirm`
removed both volumes.

**Two things this bring-up measured that nobody had:**

- **`image_digests` is green here for a reason that will not hold on a clean Mac.** In the native
  posture compose never builds `palai/control-plane:local` — the service is behind a profile — and
  doctor's check requires that image to exist. It passed only because a container stack on this
  machine had built it. On a Mac that has only ever run natively, expect that check red; it is a
  doctor check written for the container topology, not a broken stack.
- **A run offered the shell tool with no workspace HANGS rather than failing — FIXED 2026-08-01, and it
  was never about the shell tool.** With `config_policy.default_tools` set to `palai.workspace.shell`,
  `palai up`'s own trivial proof run (which binds no repository) had the model call
  `{"argv": ["echo","ok"]}`; the tool refused with "no workspace bound for this run", and because the
  shell tool is `ClassIrreversible` the call went `uncertain` → `manual_resolution` and the run never
  reached a terminal state. `palai up` then blamed dispatch ("PALAI_DISPATCH_WORKERS must be >= 1"),
  which was not the cause.

  **What this measurement had actually found was every tool error, not this one.** Three days later the
  same shape was reproduced with the FILE tool on a missing filename and on a correctly-refused
  traversal, and the cause was one line in the dispatcher: an `Exec` error was returned up rather than
  turned into a tool result. A tool's refusal is now delivered to the model as a result and the run
  carries on; a genuine fault still fails the attempt. **`docs/operations/tool-errors.md` is the page**,
  and it carries the bound that came with it (`PALAI_TOOL_ERROR_BUDGET`, default 16). The Slack path
  still binds the workspace tools only when a repository exists, which remains the right default for a
  different reason: a tool that cannot work should not be advertised.

---

### 4.2 More than one session at a time — MEASURED 2026-07-29

Renting a Mac only pays if it runs **more than one session at once**. It does, and it takes two
environment variables. With their shipped defaults it does **not**, silently.

**A stack is only as parallel as the smaller of two numbers**, and both default to at most 1:

| variable | what it is | shipped default |
|---|---|---|
| `PALAI_DISPATCH_WORKERS` | how many runs the control plane dispatches at once | `0` in `compose.yaml`, `1` in `production.yml` |
| `PALAI_RUNNER_CONCURRENCY` | how many engine leases **one runner** parks at once | `1` |

Set both, and a native bring-up runs N sessions concurrently:

```sh
PALAI_DISPATCH_WORKERS=2 PALAI_RUNNER_CONCURRENCY=2 \
PALAI_WORKSPACE_ROOT=/absolute/host/path \
palai up --native
```

`palai up` exports the runner count for you when only the dispatch count is set — they answer the same
question, and letting them disagree silently was a real defect. `scripts/ops/mac-sessions.sh plan
--count N` prints this block with N filled in.

**The measurement, on this machine** (M2 Pro, 12 core, 16 GiB). Two sessions submitted at the same
instant, intervals taken from the durable run ledger (`job_attempts.owner` and `.started_at`,
`durable_jobs.updated_at` — Postgres timestamps, not log lines):

```
DISPATCH=1, RUNNER=1 — SERIAL
 owner                 | started      | ended        | secs
 control-plane-63655-0 | 15:43:17.456 | 15:43:31.293 | 13.84
 control-plane-63655-0 | 15:43:31.336 | 15:43:35.893 |  4.56
 overlapping pairs: 0     distinct dispatch workers: 1     peak concurrent runs: 1
```

The second run started **43 ms after the first one finished**. That is the default posture.

```
DISPATCH=2, RUNNER=2 — CONCURRENT
 owner                 | started      | ended        | secs
 control-plane-38340-1 | 15:47:27.651 | 15:47:39.593 | 11.94
 control-plane-38340-0 | 15:47:27.671 | 15:47:39.719 | 12.05
 overlapping pairs: 1     distinct dispatch workers: 2     peak concurrent runs: 2
 overlap: 15:47:27.671 -> 15:47:39.593  (11.92s)
```

Two runs, two different dispatch workers, in flight together for 11.92 s — and `docker ps` showed
**two engine containers alive at once**.

**Prove it on your own box**, against the running stack:

```sh
make test-live-concurrency
```

It reads the ledger and **fails** unless two runs were in flight at the same instant on two different
dispatch workers. It does not skip a serial stack: a stack that runs sessions one at a time is the
regression this fences, so it goes red. It skips only when there is no stack at all.

**Two things this measurement settles that were previously assumed:**

- **`runner_leases` is a dead table.** It has no writer anywhere in the tree and is always empty, so
  "N live leases" cannot be read from it. The observable lease is the dispatch worker holding a job
  (`durable_jobs.lease_owner`) and, physically, the engine container the runner started.
- **Nothing serialises the path in code.** The gateway hands out engines over an unbuffered channel
  with no lock (`RunnerGateway.Dial`), and the runner parks N independent lease loops on one enrolled
  identity (`packages/runner/serve.go`). Concurrency here was **configuration only** — no code change
  was needed to make sessions run in parallel.

**What caps N is the machine, not the stack.** On this 16 GiB box the ceiling is low and it is RAM:
`mac-sessions.sh plan` budgets 8 GiB per *Xcode-class* session, giving a ceiling of 1. Light sessions
(one model round trip, no simulator) are far cheaper — 2 ran comfortably. A later repeat of the same
2-session run, taken while the host was thrashing at load 250 with 157 MB free, **failed**: Postgres
dropped into recovery mode and engines missed their startup handshake. The runs were still dispatched
concurrently (25.5 s overlap on two workers); the box simply could not finish the work. Size the Mac
for the sessions, and read §4.3 before assuming a number.

### 4.3 A bring-up on a loaded Mac needs longer than 30 seconds

`palai up` waits for the control-plane API and used to give it a fixed 30 s. On this box under load the
native control plane needed **34 s** to serve `/v1/capabilities` — migrations against a containerised
Postgres, plus Slack socket init — so the bring-up failed and, critically, **never started the runner**.
The stack looked broken; it was merely slow.

The default is now 90 s and it is tunable:

```sh
PALAI_STACK_READY_TIMEOUT=3m palai up --native
```

This matters more here than anywhere else: a Mac hosting concurrent sessions is, by definition, a
loaded Mac.

---

## 5. Host facts an agent needs

Everything below was measured on this machine on **2026-07-28** (macOS 26.3 / 25D125, Xcode 26.6 /
17F113, AXe 1.7.0). It is written for a model reading a run's context, so it is phrased as rules.

**Boot deterministically, never with a fixed sleep.**

```
xcrun simctl bootstatus <udid> -b
```

`simctl boot` returns long before the device is usable (25 s after boot the screen was still the Apple
logo). `bootstatus -b` boots if needed and waits.

**`xcrun simctl help` does NOT list `bootstatus`.** It is a real subcommand with its own help, and it
is missing from the subcommand list — a model that enumerates capabilities from `help` will never
find the one command that makes a boot deterministic. Also: **`simctl help <subcommand>` prints to
stderr**, so reading only stdout makes a working subcommand look absent. Both are pinned by
`TestNativeShellPostureRunsTheHostsOwnToolchain`.

**After `bootstatus`, the device is booted but not yet drivable.** The accessibility translation
service comes up seconds later (measured: 7 s after an 8 s `bootstatus`). Until it does, `axe`
answers `Error: No translation object returned for simulator`. **Poll `axe describe-ui` until it
returns a JSON array; do not sleep a guessed number of seconds.**

**A Simulator.app window is NOT required** to boot, screenshot, record, describe or tap (§3). Opening
one costs ~12–20 s and is only useful for a human watching.

**`simctl` has no input verbs.** Its 40 subcommands are device management — `boot`, `install`,
`launch`, `io`, `spawn`, `privacy`, `status_bar`, `ui`, … There is **no** `tap`, `swipe`, `scroll`,
`press` or `drag`. Driving a UI is `axe`.

**`axe` is the driving tool** (`/opt/homebrew/bin/axe`, MIT, https://github.com/cameroncooke/AXe):

| Need | Command |
|---|---|
| read the UI | `axe describe-ui --udid <udid>` |
| tap a point / an element | `axe tap -x 100 -y 200 --udid <udid>` · `axe tap --label "Sign in" --udid <udid>` |
| scroll | `axe gesture scroll-down --udid <udid>` (presets: `scroll-up/down/left/right`) |
| swipe / drag | `axe swipe …` · `axe drag …` |
| type | `axe type "hello" --udid <udid>` · `axe key`, `axe key-combo` |
| hardware button | `axe button home --udid <udid>` |
| several steps, one session | `axe batch …` |

**Ceiling:** `axe` is a third-party tool over Apple's **private** accessibility and HID APIs. An OS
update can break it; when it breaks, the shell call fails honestly rather than silently doing
nothing. **Do not use `idb`**: `idb_companion` on this machine is a 2022 build and collides with
macOS 26's `FrontBoard` on every call, and Meta's WebDriverAgent is archived.

**Build once, test many.** `xcodebuild test` = build + run every time. Prefer:

```
xcodebuild build-for-testing   -project X.xcodeproj -scheme S -destination 'platform=iOS Simulator,id=<udid>' -derivedDataPath DD CODE_SIGNING_ALLOWED=NO
xcodebuild test-without-building -xctestrun DD/Build/Products/*.xctestrun -destination 'platform=iOS Simulator,id=<udid>'
```

`-destination` requires `platform`; `name`/`id` and `OS` narrow it. A **simulator build needs no
signing identity** — measured: `** BUILD SUCCEEDED **` with `CODE_SIGNING_ALLOWED=NO`, and the
product is `Signature=adhoc`, `linker-signed`, `TeamIdentifier=not set`.

**Recording is stopped by a signal, and the file is not what you named it.**

```
xcrun simctl io <udid> recordVideo --codec=h264 --force out.mov & REC=$!; sleep 10; kill -INT $REC; wait $REC
```

The shell tool returns **one** result and has no signal channel, so the stop belongs in your argv.
And **`--codec=h264` still writes a QuickTime container**: `file(1)` reports *"ISO Media, Apple
QuickTime movie"*. Name it `.mov`. Calling it `.mp4` publishes a lie about bytes you produced.

**One Xcode per Mac.** `CoreSimulatorService` knows one active Xcode; switching it kills booted
simulators.

**Always pass `--set "$PALAI_SIMCTL_SET"` to every `simctl` call.** It is your run's own device set,
it already exists, and it is the only thing keeping another run from booting, wiping or renaming your
simulators. Omit it and your device goes into the machine-wide default set with everybody else's.

```
xcrun simctl --set "$PALAI_SIMCTL_SET" create "run-device" "iPhone 16"
xcrun simctl --set "$PALAI_SIMCTL_SET" bootstatus <udid> -b
xcrun simctl --set "$PALAI_SIMCTL_SET" io <udid> screenshot shot.png
```

`--set` must be passed to **every** call in the chain, including the `create` that made the device —
a device created without it is not in your set and no later `--set` will find it. It is **advice, not
a fence**: nothing in Palai rewrites your argv, and `HOME` does not do this for you (§2.1, measured).
`xcodebuild -destination` takes the plain `id=<udid>`; it has no `--set` of its own, which is fine
because a udid is unambiguous once the device exists.

**Delete what you create.** The set lives inside your allocation and goes away with it, but a device
still *booted* when that happens leaves a `CoreSimulatorService` process holding a directory that no
longer exists. End with `xcrun simctl --set "$PALAI_SIMCTL_SET" shutdown all`.

**Your own environment is six variables** (§1.1) — `PATH`, `LANG`, `DEVELOPER_DIR` inherited, and
`HOME`, `TMPDIR`, `PALAI_SIMCTL_SET` derived from your own workspace allocation. Anything else you
expect to inherit is not there, and that is deliberate.

**`HOME` is yours, not the operator's**, so nothing you write there touches another run — and nothing
you *expect* to be there is. There is no `~/.gitconfig`, so `git commit` fails with *"Author identity
unknown"* until your argv supplies one (`git -c user.name=… -c user.email=…`). There is no `~/.ssh`
either. Publishing a branch is `push`/`pull_request`, not `git push` — those go through the
repositories adapter and carry their own credentials.

**Nothing here is a Palai operation.** Every line above is an argv for `palai.workspace.shell`. If a
command exists on the machine, you can run it; if it does not, you get exit 127 and the machine's own
message.

---

## 6. Evidence

| Claim | Proof |
|---|---|
| Environment is an allow-list, and `HOME` is **not** the operator's | `TestHostShellDropsTheOperatorsEnvironment` |
| Two concurrent runs get disjoint `HOME`/`TMPDIR`/`PALAI_SIMCTL_SET`, and neither sees the other's files | `TestHostShellGivesConcurrentAllocationsDisjointSessionDirectories`, `TestNativeShellPostureSeparatesConcurrentSessionsOnOneMac` (`make test-component TEST=native-shell`) |
| **`simctl --set` is advice this runner cannot enforce**, and `HOME` does not select the device set (X20) | `TestSimctlSetIsAdvisoryNotEnforced` — the ceiling is in the NAME |
| The X20 and T21 measurements, re-run against real devices rather than recalled | `TestLiveMacHostHomeDoesNotSelectTheSimulatorDeviceSet` (`make test-live-mac`) |
| A run's session directory never enters a workspace snapshot | `TestSnapshotSkipsThePerSessionDirectoryAsASubtree` |
| A workspace root under `/private/tmp` or `/Users/Shared` is refused at bring-up, on the RESOLVED path | `TestNativeWorkspaceRootRefusesAWorldWritableParent` |
| The operating rule is word-for-word in both operator pages, with its source and date | `TestTheMacOperatingRuleIsVerbatimInBothOperatorPages` |
| `ReadOnly` refuses rather than running writable | `TestHostShellRefusesAReadOnlyAttempt` |
| Output bounded, secrets redacted, workspace is the cwd | `TestHostShellBoundsOutput`, `TestHostShellRunsInTheWorkspaceRootAndRedactsSecrets` |
| Wall time kills the process **group** | `TestHostShellWallTimeKillsTheWholeProcessGroup` |
| There IS a wall time with nothing configured (it used to be unbounded) | `TestTheNativePostureBoundsAShellCallWithNoEnvironmentSet` |
| The model is told a cap was hit, not handed an indistinguishable failure | `TestAWallTimeExpiryTellsTheModelWhatHappened` |
| Missing command is exit 127, not an infrastructure error | `TestHostShellReportsAMissingCommandAsExit127` |
| Both postures at once is fatal; `=1` refused | `TestShellPostureRefusesBothSandboxImageAndNativeHost`, `TestShellPostureAcceptsOnlyTheStringThatSaysWhatItIs` |
| No posture ⇒ nil runner ⇒ clean tool failure (unchanged) | `TestShellRunnerFromEnvKeepsItsNilDiscipline`, `TestShellToolStillFailsCleanlyWithNoPostureConfigured` |
| Boot line names the operating rule and cites the measurement | `TestNativeShellPostureDeclarationNamesTheOperatingRule` |
| The host's own Xcode answers through the shell tool | `TestNativeShellPostureRunsTheHostsOwnToolchain` (`make test-component TEST=native-shell`) |
| A real simulator is booted, read, tapped, shot and recorded through argv | `TestLiveMacHostDrivesASimulatorThroughShellCalls` (`make test-live-mac`) |
| `build-for-testing` / `test-without-building` against a real project | `TestLiveMacHostBuildsAnXcodeProjectThroughShellCalls` (skips without `PALAI_IOS_PROJECT`) |
| The native overlay starts no second control plane, keeps the certificate's name, binds one path | `TestNativeOverlayDoesNotStartTheContainerControlPlane`, `TestNativeOverlayReachesTheControlPlaneByTheNameOnItsCertificate`, `TestNativeOverlayBindsTheWorkspaceAtTheSameAbsolutePath` |
| A loopback runner listener, a port the container does not dial, and an unset/relative workspace root are refused **by name**, before Docker | `TestNativeRunnerListenerRefusesALoopbackAddress`, `TestNativeRunnerListenerRefusesAPortTheRunnerContainerDoesNotDial`, `TestNativeWorkspaceRootRefusesUnsetAndRelativePaths` (`make test-component TEST=native-shell`) |
| The native process reaches the containers by their published ports, and no override is lost behind an inherited value | `TestNativeEnvironmentReachesTheContainersByTheirPublishedPorts`, `TestNativeEnvironmentHasNoDuplicateKeys` |
| `palai local down` kills the process **group** and refuses a pid running something else | `TestNativeStopKillsTheProcessGroupAndClearsTheRecord`, `TestNativeStopRefusesAPidThatIsNotTheControlPlane` |
| A full native bring-up, a live round-trip, and the host's Xcode through the shell tool | §4.1 — run on this machine 2026-07-28, output pasted there |
