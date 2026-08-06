# Palai zero-touch device agent and remote fleet plan

**Date:** 2026-08-05  
**Measured baseline:** `main@a8e93214`  
**Owner outcome:** A self-hosted Palai control plane runs in the cloud. A clean Mac or Linux machine,
on any network, receives one Palai package plus a control-plane URL and a pool enrolment key, starts
without prompts, joins the pool over an outbound connection, survives restarts as the same machine, and
accepts sessions. The admin console owns pools, bootstrap keys, machine configuration, health and session
history. The TypeScript SDK is installable from npm. The same bootstrap contract is usable by a later
autoscaler.

This plan supersedes the proposed invite/device-code flow. There is no interactive device login in the
target design.

---

## 1. Definition of done

The work is complete only when all of the following are true at once:

1. A production self-host exposes two authenticated surfaces:
   - the existing HTTPS API/admin edge;
   - a public TLS runner gateway used only by agents.
2. A device is set up by exactly one command, run exactly once, taking exactly these inputs:
   - `palai enroll --url <controller>`;
   - `--key-file <path>`, a file holding a pool-scoped `rpk_...` key — never `--key`, because a secret
     on argv is readable by every process on the machine (§3.2);
   - `--ca-file <path>` only for a private/self-signed deployment. Public TLS uses system roots.

   These are arguments to a **one-time install**, not a runtime contract; see item 17.
3. `PALAI_RUNNER_POOL`, `PALAI_RUNNER_POSTURE`, runner id, DNS name, OS, architecture and capacity are not
   required operator inputs. The key chooses the pool, the binary measures the machine, and the admin
   plane chooses concurrency.
4. The public device artifact is the runner, installed as `palai`. The local-stack/admin CLI is a separate
   `palai-selfhost` artifact and is not present in the device package.
5. Running `palai` in the foreground has no prompt, browser, REPL or admin command. A service manager may
   run the same binary with the same config.
6. A restart reconnects as the same runner id. It does not add another machine row or require another
   strict-pool approval.
7. macOS has two explicit isolation modes:
   - `user`: current login account, per-session directory and simulator set, no root; supported only for
     one customer on that Mac;
   - `accounts`: one macOS account per slot through `palai-agentd`; one-time administrator installation is
     required and the agent fails closed without it.
8. Linux runs the existing sandboxed posture as an unprivileged agent with access to a supported Docker
   daemon. Docker access is a declared prerequisite, not something the Palai installer silently obtains.
9. Before enrolment, the agent measures its engine driver, workspace, platform and supported isolation
   modes. The gateway checks those facts against the key's pool before issuing an identity. A machine that
   cannot execute never appears as ready capacity.
10. The Fleet screen shows, per machine: live connection state, last seen, **running agent version and
    the version it should be running**, measured shape, isolation mode, desired/applied configuration,
    current occupancy and historical sessions. A newer version can be rolled out from the panel, and the
    device updates itself without a human on it (T6b).
11. A real session created through the packed TypeScript SDK runs `hostname`, file operations and
    `xcodebuild -version` on a remote Mac rather than on the control-plane host.
12. A clean Mac and a clean Linux user each install from one signed release archive through one install
    script, with no source checkout and no package manager in the path.
13. `npm install @palai/sdk` installs compiled JavaScript plus declarations; a clean consumer can create,
    stream and inspect a session against the self-host.
14. Two Macs on different networks pass the live fleet gate simultaneously. No device opens an inbound
    port.
15. Autoscaling, when implemented, starts from the same immutable machine image and injects only the URL,
    key file and optional private CA. It does not invent a second enrolment protocol.
16. **There is no whole-stack device path.** The admin plane and the device agent are two products that
    meet over one URL. No device command starts a control plane, a database, an object store or a Compose
    project, and no device command builds a binary from a source checkout.
17. **An INSTALLED agent reads ZERO `PALAI_` names**, and the count is the gate. The URL, the key file
    and the optional CA are **arguments to `enroll`**, which runs once; everything else is derived by the
    binary, delivered by the admin plane, or deleted.

    **THE FIRST VERSION OF THIS ITEM COUNTED THE WRONG THING, and correcting it is worth more than the
    number was.** It said 26, from `grep -rhoE 'PALAI_[A-Z_]+' … | sort -u | wc -l` — which counts
    comments, doc references, and a DENYLIST of names the runner refuses to pass into an engine. Counting
    a name the code exists to REJECT as a name the code reads is the same defect this plan keeps finding
    elsewhere. Re-measured 2026-08-06 against the readers rather than the spellings:

    ```bash
    grep -rhoE '(os\.Getenv|os\.LookupEnv|envDurationDefault|envIntDefault|planeIntDefault|derivedEnv|defaultEnv|mustEnv)\("PALAI_[A-Z_]+"' \
      cmd/runner/ packages/runner/ packages/device/ | grep -oE 'PALAI_[A-Z_]+' | sort -u | wc -l
    ```
    → **20** (2026-08-06), of which **15 are on the environment branch alone**: `loadConfig` returns
    `installedBootstrap` before reaching any of them, so a device that was enrolled never reads the URLs,
    the ids, the pool, the posture or the capacity. Those 15 belong to compose, Helm and the systemd unit,
    which is the compatibility window §3.7 grants them.

    **FIVE REMAINED ON THE INSTALLED PATH; FOUR ARE CLOSED (2026-08-06).** `PALAI_SHELL_NATIVE` and
    `PALAI_SANDBOX_IMAGE` are gone from it: a darwin device is native by construction — `user` and
    `accounts` both mean commands run on this host — so `posture.RunnerForInstalledDevice` answers from
    what the machine measured. `PALAI_SETTINGS_INTERVAL` is gone: a device takes the package default,
    because the value could only reach a device as a per-machine variable and a fleet whose poll interval
    differs per box for unrecorded reasons is one whose configuration lag cannot be reasoned about.
    `PALAI_WORKSPACE_UNSAFE_BIND` is gone and that one is a NARROWING: an installed device never opts into
    honouring a lease's unsafe local bind, because the opt-in exists so a control plane alone cannot make
    a runner mount an arbitrary host path — and on a device the machine's half of that yes could only be
    typed as an environment variable on the box.

    **ONE REMAINS AND IT IS NOT A DEVICE INPUT:** `PALAI_COMPOSE_PROJECT`, read by the supervisor to label
    a container so `palai local down` can find it. On a device it reads empty and nothing depends on it;
    deleting it would break compose, and keeping it imposes nothing on a machine. Verified by inspecting
    the `installed != nil` branch of every device-facing helper — `settingsInterval`, `allowUnsafeBind`,
    `workspaceRoot`, `shellRunner` — none reads the environment.
18. **A session's tree follows the session, not the machine.** When a machine is stopped, drained,
    terminated or simply loses the placement, the next attempt restores that session's workspace from the
    object store on whichever machine takes it, or the run refuses. A resumed session that continues over
    an empty tree is the failure this item exists to forbid — measured 2026-08-05, that is today's
    behaviour and it is **silent**: `reuseAllocation` short-circuits on the `(bindingID, runID)` receipt,
    the new machine CREATES the path, and the run proceeds with no error, no log and no terminal.
19. **Multi-tenant pools are `accounts`-only.** `isolation_mode=user` is a single-customer posture and the
    admin plane refuses to place two projects on a `user`-mode machine. A hosted fleet serving many
    customers runs `accounts` mode, where a session holds its own macOS account and the account is deleted
    when the session's workspace has been archived.
20. **A power cycle is a non-event.** After `enroll`, the machine is rebooted with nobody touching it and
    returns to the fleet as the same runner id, ready, within a bounded time that the gate names. This is
    asserted by rebooting, not by reading a plist.

    **AND THE ONE UNMEASURED CASE IS NAMED RATHER THAN ASSUMED.** Measured 2026-07-28 on macOS 26.3 /
    Xcode 26.6 (`docs/operations/palai-on-a-mac.md` §3): the **launch context is not the discriminator** —
    a cron job in the system bootstrap namespace with no Aqua session drove `simctl bootstatus`,
    `simctl io screenshot`, `open -a Simulator` and `axe tap` **identically** to a Terminal session. The
    real discriminator was **time**: the accessibility translation service answers roughly 7 s after
    `bootstatus` returns, and an earlier experiment mistook its own 12 s wait for a requirement that a
    window be open.

    What both of those contexts shared is that **a user was logged in graphically**. A Mac with nobody
    logged in at all — precisely the state a rebooted headless fleet machine is in — was recorded as
    **not measured**, and it stays not measured until this gate runs. Two outcomes, both acceptable, and
    the gate must say which one it got: either a LaunchDaemon serves sessions with no GUI login (and
    `user` mode gets a LaunchAgent purely for a sane `HOME`), or a graphical auto-login is a documented
    prerequisite of a Mac fleet machine. What is not acceptable is shipping either sentence unmeasured.

---

## 2. Measured starting point

| Area | What exists | What prevents the owner outcome |
|---|---|---|
| Pool bootstrap | `runner_pool_keys`; `rpk_...` values are pool-scoped, hashed, reveal-once, reusable, expirable and revocable | Packaging and deployment still teach the legacy default-pool file token |
| Runner wire | `cmd/runner` derives enroll/connect/renew/settings from one controller URL and keeps outbound mTLS/WebSocket sessions | It requires a CA file, enrols on every process start, omits OS/arch and has no stable on-disk device identity |
| Remote execution | `exec.*`, `ws.*` and `bg.*` reach the lease-holding machine; workspace affinity and least-loaded placement are present | Live proof is one host/two processes; stale operator docs still claim the relay is absent; no remote Mac has run `xcodebuild` through it |
| macOS isolation | Per-session directories are measured; `palai-agentd` and account workers exist | Native Darwin admission always requires the privileged daemon, so the documented rootless `dirs` mode is not reachable from the runner |
| Production network | API is behind the TLS edge; runner gateway listens internally on `:8443` | `production.yml` deliberately removes every control-plane host port, including `:8443`; a remote device cannot enrol |
| Linux package | Signed deterministic Linux runner tarballs and a systemd system unit exist | The package is build-only, manual, root-oriented and not published; there is no macOS runner package |
| Release | Matrix build, SBOM, signatures, SDK tarballs and registry dry-runs exist | Workflow explicitly publishes nothing; the GitHub release, Homebrew tap and npm registry writes do not exist |
| TypeScript SDK | `@palai/sdk` source, tests and `npm pack` are present | `private: true`; exports point at `.ts` source; no compiled `dist`, consumer install test or real publish |
| Fleet console | Pools, pool keys, lifecycle and desired-config reports exist | No authoritative per-runner online field; only `last_seen_at`; no runner occupancy/session-history view |
| Desired config | Pool + machine documents are polled; runner applies changes live | Only `PALAI_RUNNER_CONCURRENCY` is applied; capacity is a separate pre-enrolment local input |
| Autoscale | A run parks while capacity is absent and pool waiting depth exists | No scaler loop and no cloud-provider spawn/terminate implementation exist |

Important source anchors:

- `cmd/runner/main.go` — current agent composition and bootstrap.
- `packages/runner/{enrollment,serve,toolserver,workspaceserver}.go` — identity and remote execution.
- `apps/control-plane/internal/execution/runner_gateway.go` — runner TLS gateway and live sessions.
- `apps/control-plane/internal/fleet/keys.go` — reusable pool key contract.
- `apps/control-plane/api/runners.go` and `apps/web-console/app/fleet/page.tsx` — current admin surface.
- `deploy/compose/production.yml` — runner gateway is currently unreachable from another network.
- `scripts/package/runner/` and `scripts/release/` — existing package and release spine.
- `sdks/typescript/package.json` — current non-publishable package metadata.

Historical evidence bundles are immutable records. Correct stale operational docs and current known-gap
projections; do not rewrite an old bundle to pretend it measured the new topology.

---

## 3. Product and security decisions

### 3.1 Four credential classes stay separate

| Credential | Holder | Authority |
|---|---|---|
| Admin/API key | Admin console relay, SDK or self-host operator | Authenticated `/v1` operations |
| Pool enrolment key (`rpk_...`) | Device bootstrap file or provisioner secret | Enrol one or more machines into exactly one pool |
| Runner certificate/private key | One installed device | Renew, poll settings and accept leases as that runner |
| Environment values | Control-plane secret store, resolved for an agent revision | Variables given to session shell commands |

An Environment value cannot bootstrap a runner: values arrive only after placement, and `PALAI_` names are
reserved. The device never receives an admin API key.

### 3.2 One device contract, no invitation protocol

**Enrolment is a one-time INSTALL, not a runtime contract.** One command runs once per machine:

```sh
palai enroll --url https://runner.example.com:8443 --key-file /secure/path/pool-key
```

It writes config and device identity, **installs and starts the service**, and returns. Nothing else is
typed on that machine again. The running agent reads **zero** `PALAI_` variables: after enrolment its
inputs are its own on-disk identity and the configuration the admin plane sends it.

`--key-file`, never `--key`. **A secret on argv is readable by every process on the machine**, measured
on this tree 2026-08-04: `ps -E -p <pid>` listed 62 environment variables **with their values**, and
`os.Unsetenv` did not hide them because macOS serves `ps` from the kernel's start-time copy
(`KERN_PROCARGS2`). argv is at least as exposed. The key file is read once, consumed, and never copied
into the config.

A private PKI adds `--ca-file /secure/path/ca.pem`; a publicly trusted server needs nothing.

**The service is what makes a reboot a non-event.** `enroll` installs it, so a machine that is powered
off and back on returns to the fleet with no human on it — that is the property the whole device design
exists for, and it is a gate rather than a hope (DoD 20).

| Platform | Mode | Service installed by `enroll` |
|---|---|---|
| macOS, `user` | one customer, no root | LaunchAgent in `~/Library/LaunchAgents` |
| macOS, `accounts` | one macOS account per slot | LaunchDaemon; `palai-agentd` is already exactly this shape (`RunAtLoad` + `KeepAlive`), and enabling the mode is the one administrator action in the product |
| Linux | container posture | systemd **user** unit, plus `loginctl enable-linger` so it survives logout |

For ephemeral CI, `PALAI_ENROLLMENT_TOKEN` remains accepted by `enroll` only, cleared immediately as it
is today. It is not a way to run the agent; it is a way to script `enroll`.

Suggested default paths:

| Platform | Config | Secret/state | Logs |
|---|---|---|---|
| macOS | `~/Library/Application Support/Palai/agent.json` | same directory, mode 0700; key and device identity mode 0600 | `~/Library/Logs/Palai/agent.log` |
| Linux | `${XDG_CONFIG_HOME:-~/.config}/palai/agent.json` | `${XDG_STATE_HOME:-~/.local/state}/palai`, mode 0700/0600 | journal or `${XDG_STATE_HOME}/palai/agent.log` |

The config stores the path to the pool key, never a copied plaintext key.

### 3.3 Pool key is authoritative

- The key determines project and pool.
- An optional legacy `PALAI_RUNNER_POOL` remains a mismatch guard during a compatibility window, but the
  public install path never asks for it.
- The agent reports `runtime.GOOS`, `runtime.GOARCH`, version and measured capabilities. The server refuses
  a mismatch with the pool's expected shape.
- Strict enrolment remains optional. Autoscaled pools must use non-strict enrolment; static high-trust
  pools may use strict mode.

### 3.4 Stable identity is a local device key

The hostname is a label, not identity. On first start the agent generates one device keypair and persists
it atomically. Enrolment carries a standard CSR whose signature proves possession. The registry keys a
machine by project + pool + public-key fingerprint:

- first CSR + live pool key creates the runner id;
- restart loads the current certificate and renews/reconnects as that id;
- expired-certificate recovery presents the same CSR/key plus the pool key and reissues the same id;
- a revoked runner fingerprint cannot recover, even while another pool key is live;
- re-imaging creates a new key and therefore a new machine/approval subject.

This closes the current “new row on every restart” behaviour without trusting a client-supplied id.

### 3.5 Sudo-free does not mean pretending OS privileges do not exist

- Default self-hosted Mac pool: `isolation_mode=user`. No `palai-agentd`; Palai runs as the installing
  login account with per-session workspaces and simulator sets. This is same-customer accident isolation,
  not a cross-customer security boundary.
- Optional dense Mac pool: `isolation_mode=accounts`. The same package carries `palai-agentd`, but enabling
  this mode requires one administrator action. Enrolment fails closed if the daemon cannot prove readiness.
- Different customers never share a Mac in either mode.
- Linux uses its current container posture. Membership in a Docker group is root-equivalent and must be
  stated as a prerequisite. The Palai package does not silently modify that group.
- The current engine still requires Docker, including on a native Mac. Removing Docker requires a separate
  trusted engine-driver design and is not smuggled into packaging work.

### 3.6 Capacity has one owner

`PALAI_RUNNER_CONCURRENCY` becomes the one admin-owned “sessions per machine” value:

- its effective pool/machine desired value sets the server-side occupancy ceiling;
- the runner applies the same number to its parked lease loops;
- a live change updates both through the existing settings/report path;
- shrinking is cooperative: current sessions finish and no new session starts above the new limit;
- `PALAI_RUNNER_CAPACITY` remains read for one compatibility release but is not part of public setup and
  cannot widen the admin limit.

This removes an impossible UX in which the panel controls concurrency but a hidden pre-enrolment variable
controls a different ceiling.

---

### 3.7 What is deleted, by name

A deprecation that keeps both paths alive is the failure mode this repository has paid for repeatedly: the
old path stays reachable, nothing forces the new one, and the "migration" is a second way to be wrong. The
device path is a **cut-over**, and these are the things that stop existing rather than becoming legacy.

| Deleted | Why it cannot survive as a fallback |
|---|---|
| The CLI compiling and launching the agent (`cmd/cli/internal/stack/native_runner.go` runs `go build -o … ./cmd/runner`) | It makes a source checkout and a Go toolchain part of the device contract. A packaged device has neither, so a fallback here means the packaged path is never the one exercised — **measured 2026-08-06: it hid a real defect.** The Milestone A0 session was served by this conjured runner while the enrolled device sat parked beside it, so nobody noticed the device had no shell executor at all |
| `palai up` / `palai local up` as anything a device runs | It starts a control plane, a database and an object store beside the agent. A machine that runs its own control plane is not a member of a fleet |
| Every admin/server command in the device artifact — pool, provider, secret, environment, backup, restore, doctor-for-the-stack | DoD 4 and 5 already require their absence from the package; this row makes the absence a deletion rather than a packaging filter, because a filtered binary still carries the code |
| 23 of the 26 device-side `PALAI_` variables | Each one is an operator input that the key, the binary or the admin plane already knows. Keeping a reader "for one release" keeps the machine configurable from the machine, which is the thing being removed |
| Per-device identity inputs: `PALAI_RUNNER_ID`, `PALAI_RUNNER_DNS`, `PALAI_RUNNER_PRIVATE_KEY` | Identity becomes the device key (§3.4). A client-supplied id is exactly the trust this design removes |
| Pre-enrolment placement inputs: `PALAI_RUNNER_POOL`, `PALAI_RUNNER_POSTURE`, `PALAI_RUNNER_CAPACITY` | The key chooses the pool, the binary measures the shape, the admin plane sets concurrency (§3.3, §3.6) |
| Four derived URLs: `PALAI_ENROLLMENT_URL`, `PALAI_SESSION_URL`, `PALAI_RENEW_URL`, `PALAI_CONTROLLER_DNS` | Already derived from one address today; keeping the overrides keeps four ways to point a device at four different planes |

**BLAST RADIUS OF THE FIRST ROW, MEASURED BEFORE CUTTING (2026-08-06), because it is not a one-file
deletion and a half-done cascade is worse than an un-started one:**

```bash
grep -rln 'palai-runner\|native runner' --include='*_test.go' --include='*.sh' cmd/cli tests scripts
```
→ `cmd/cli/internal/stack/native_runner_test.go`, `tests/uat/tool-execution/bundle_test.go`,
`tests/uat/stable-release/bundle_test.go`, `tests/uat/kubernetes/kind-smoke.sh`,
`scripts/test/runner-engine.sh`, `scripts/package/runner/splitvm-proof.sh`.

**AND THAT LIST WAS WRONG — THE GREP MATCHED SUBSTRINGS OF UNRELATED NAMES (corrected 2026-08-06).**
Read one by one: the tool-execution bundle mentions *"the native runner builds"* in **prose**, inside a
claim string; the stable-release bundle checksums `palai-runner-host-*.tar.gz`, which is the host
**package** from `scripts/package/runner`; `runner-engine.sh` builds `palai-runner-engine` from
`tests/sandboxes/engine`; `splitvm-proof.sh` and `kind-smoke.sh` use that host package too. **None of
them depends on this build path.** The UAT cascade priced above does not exist, and the deferral it
justified was a deferral bought with a number that did not measure what it claimed — the same defect
this plan corrected in DoD 17 an hour earlier, committed again while writing about it.

**DONE (`8f24e234`, 2026-08-06).** `palai up --native` no longer builds an agent. A stack with no agent
beside it is a CORRECT stack and prints the two commands that give it capacity — `install.sh`, then
`palai enroll --url … --server-name …`. `PALAI_RUNNER_BIN` remains the only way a bring-up starts one,
which is a checkout or CI naming a build deliberately rather than a path conjuring one.

**The compatibility window applies to Compose deployments that exist today, not to the device path.** T3
may keep a reader for an existing self-host stack for one release; it does not keep one for the packaged
agent, and no documentation, example, formula or unit file names a deleted variable.

---

## 4. Delivery sequence

Each task uses one implementation writer, one fresh consolidated reviewer, one fix/re-review loop when a
reproduced Critical/Important issue exists, one source commit, and one final `make verify`. Expensive live
evidence runs once after the source commit. Evidence-only changes receive their own commit.

### T0 — Freeze the truthful contract and delete stale current claims

**Goal:** Make the repository agree on what is already built and what this plan is adding.

**RED first**

- A source test proves every coding tool resolves a machine executor and refuses when that machine is
  withheld; it must fail if process-local fallback returns.
- A documentation consistency test rejects current operator pages that still say remote execution is
  absent while `ToolServer`/`WorkspaceServer` are wired.
- A source-level surface test requires the device composition root to import/dispatch no `init`, `up`,
  `provider`, `poolkey`, `admin`, backup or restore command. T7 repeats the assertion on the built artifact.

**Implementation**

- Mark `FLT-P15` closed by the shipped `exec/ws/bg` relay in current operational docs and known-gaps.
- Preserve old UAT/evidence text as historical evidence.
- Add this plan and a short architecture page naming API edge versus runner gateway.
- Define artifact names: `palai` (device), `palai-selfhost` (server operator), `@palai/sdk` (client SDK).

**Acceptance:** A new reader cannot conclude either that a remote Mac already works end-to-end or that the
execution relay is still absent.

### T1 — Publish a real runner-plane endpoint from self-host

**Goal:** A machine on another network can reach the gateway without exposing Postgres, object storage,
metrics or an unauthenticated admin surface.

**RED first**

- Rendered production Compose must expose the API edge and one runner gateway port, and no other service
  port.
- A remote TLS client using the public hostname completes enrolment; a wrong hostname, wrong CA, expired
  pool key and admin API key are each refused.
- A client certificate from another CA cannot open `/v1/runner/connect`.

**Implementation**

- Add a production runner-gateway bind/port contract instead of `ports: !reset []` hiding `:8443`.
- Separate the runner gateway's public server certificate/key from the private runner client CA/key.
  The server certificate may be publicly trusted; client certificates remain signed by Palai's runner CA.
- Add `PALAI_RUNNER_PUBLIC_URL` to self-host config and surface it read-only in Fleet setup help.
- Add equivalent Helm `LoadBalancer`/documented TCP exposure without putting a Docker-socket runner in the
  cluster.
- Extend production doctor to test the served runner certificate and enrolment route from the public side.
- Keep devices outbound-only; no reverse connection or device listener is added.

**Likely files:** `deploy/compose/{production.yml,production.env.example}`, Helm service/values,
`cmd/cli/internal/stack/doctor_production.go`, deployment catalogue, install/runner-host docs.

**Acceptance:** A throwaway client outside the Compose network enrols through the public DNS name and an
internal-only service remains unreachable.

### T2 — Build zero-touch bootstrap and durable device identity ✅ DONE (`0cfdd4b7`, 2026-08-06)

> `palai enroll --url --key-file [--ca-file]` ships; `PALAI_CONTROLLER_CA` is no longer `mustEnv` and
> `mustEnv` itself is deleted; identity is a device keypair + signed CSR keyed by fingerprint;
> migration `000007_device_identity_and_pool_isolation` follows `000006`. Six component tests against
> real Postgres, each `--- PASS` observed on `TEST=postgres scripts/test/component` (607 PASS / 0 FAIL).
> **NOT done here:** the running agent still reads 26 `PALAI_` names (DoD 17 wants zero) — that is T3.

**Goal:** URL + pool key starts a ready agent and a reboot returns the same machine.

**This is the only task that writes the next schema migration.** The migration adds the minimum registry
constraints/fields needed for device-key recovery and pool isolation mode; later tasks consume them.

**RED first**

- No CA env + a publicly trusted test server succeeds; private CA still succeeds when explicitly given.
- First start mints an id; process restart, certificate renewal and expired-certificate recovery all keep
  that id and produce no second runner row.
- A CSR with an invalid signature, a recovery request whose persisted id and fingerprint disagree, a
  revoked fingerprint or a pool mismatch is refused. A genuinely new install with a new key becomes a new
  machine instead of stealing an existing identity.
- A key/config/identity file with unsafe permissions is refused before network enrolment.
- Decode logs, argv, process environment after bootstrap, registry rows and journal payloads; no pool-key
  value may occur.

**Implementation**

- Extract a testable agent config loader with system-root default and optional additional private CA.
- Introduce standard config/state paths and atomic mode-0600 identity persistence.
- Replace the raw public-key enrolment body with a signed CSR and measured machine facts.
- Reuse an existing active/pending runner row by public-key fingerprint; refuse recovery of a revoked row.
- Persist every renewed/re-enrolled certificate through a callback from `packages/runner.Serve`.
- Move engine/workspace/platform probing before enrolment; report supported isolation modes in the signed
  request and let the gateway validate them against the key's pool before issuing an identity.
- Send OS, architecture and agent version; keep hostname only as the label.
- Continue accepting the existing env contract for Compose compatibility.

**Likely files:** `cmd/runner`, `packages/runner/{enrollment,renewal,serve}.go`, runner gateway, fleet store
and queries, one `000007_*` migration, security tests.

**Acceptance:** Kill and restart the agent three times, including across certificate expiry; Fleet still
contains one machine id and a strict pool does not ask for a second approval.

### T3 — Make pool policy the machine's automatic configuration

**Goal:** Nothing except URL/key/optional CA is edited per device.

**RED first**

- A Darwin binary holding a Darwin pool key joins without pool id, posture, hostname, DNS or capacity envs.
- The same binary/key shape mismatch is refused with a named reason.
- Changing pool concurrency from 1 to 4 produces four lease loops and a server occupancy ceiling of four;
  changing back to 1 admits no new occupancy until the excess finishes.
- A machine override wins over its pool document and its applied report is shown only after the agent says
  it applied it.

**Implementation**

- Add explicit pool `isolation_mode` to API/store/UI; enforce coherent combinations with posture/OS.
- Validate the request's measured platform/capabilities against the resolved pool before issuing the
  identity, then return the selected bootstrap profile with enrolment.
- Make effective `PALAI_RUNNER_CONCURRENCY` update both runner loops and durable capacity.
- Auto-derive native/container shell posture, workspace root and platform metadata from pool + measured OS.
- Deprecate public use of `PALAI_RUNNER_POOL`, `PALAI_RUNNER_POSTURE` and `PALAI_RUNNER_CAPACITY`; retain
  compatibility readers with warnings for one release.
- Keep Agent Environments entirely outside this path.

**Acceptance:** A machine with only the public three bootstrap variables becomes configuration revision N,
serves the configured number of sessions, and reports exactly what it applied.

### T4 — Make the macOS agent genuinely sudo-free in `user` mode

**Goal:** A normal logged-in Mac user can install and run Palai without `sudo`, while privileged account
mode remains fail-closed and explicit.

**RED first**

- `isolation_mode=user` on Darwin must not dial `palai-agentd`, invoke `sudo`, `sysadminctl`, `dscl` or
  write outside user-owned directories.
- `isolation_mode=accounts` must be absent from the measured supported-mode set when the daemon socket is
  absent, causing the gateway to refuse enrolment into an accounts pool.
- Two user-mode sessions get distinct workspace, DerivedData and simulator-set paths.
- A missing Docker daemon, Xcode toolchain, writable workspace or named user produces a preflight failure
  and no ready capacity.

**Implementation**

- Replace the unconditional `admitMachine` gate with a pre-enrolment capability probe; after the gateway
  selects the pool profile, bind only the mode that the probe proved available.
- Wire the measured `dirs` machinery into runner workspace/session setup.
- Keep `palai-agentd` in the package as the optional accounts-mode helper; the default install never
  places or loads it, and enabling accounts mode stays one explicit administrator action.
- Add preflight output that is useful in service logs but carries no secret.
- State the security ceiling in Fleet and docs: user mode is one customer/one uid; accounts mode is still
  not a cross-customer Mac boundary.

**Narrow tests:** `cmd/runner`, `packages/macagent`, host workspace/shell packages, live-tagged Mac tests.

**Acceptance:** On a clean logged-in Mac with Xcode and Docker already available, install/start/run/stop
uses no privilege elevation and a remote session returns that Mac's `whoami`, `sw_vers` and
`xcodebuild -version`.

### T5 — Make the Linux user agent a first-class package target

**Goal:** The same `palai` executable and bootstrap config work on Linux amd64 and arm64.

**RED first**

- Cross-built binaries start and print their baked version on both architectures.
- A user without Docker access is refused before enrolment; a user with Docker access runs the engine and
  sandboxed shell without receiving the pool key in the engine container.
- The systemd user unit opens no listening socket and restarts the same identity.

**Implementation**

- Add a user-level systemd unit beside the existing root/system unit; foreground mode stays supported.
- Replace manual identity/DNS fields in `runner.env.example` with the public URL/key contract.
- Preserve the existing hardened engine sandbox and Docker-socket boundary.
- Update the support matrix from build-only only after a real host test passes.

**Acceptance:** A clean Linux VM installs the signed archive under the user's prefix, enables the user
service, survives reboot/login and executes a real SDK-created session.

### T6 — Add authoritative fleet presence and machine/session inspection ⏳ PARTLY DONE (2026-08-06)

> **Shipped, each with a guard rather than a live measurement:**
> - `connection_state` + `connections`, from the gateway and not from `last_seen_at` (`9001ac10`). A
>   durable timestamp says when a machine last spoke; presence says whether it is there, and on a Mac
>   unplugged four minutes ago the two disagree in the direction that decides whether work is sent to it.
> - The Fleet **listing** carries it too (`40ddb068`). It did not at first: `RegistryAPI` embeds `Store`
>   and defined only `GetRunner`, so a single read showed presence and the SCREEN showed none.
> - `agent_version` and `isolation_modes` reach the projection (`7e88e86c`) — two columns 000007 added,
>   enrolment wrote, and every projection dropped.
> - `GET /v1/runners/{id}/occupancies`, keyset-paginated on `(started_at, id)` (`80c24e8c`), with its
>   own behaviour guards (`d7acefb4`) and the fixture that makes the tie case reachable at all.
> - `admin.fleet.runners.occupancies()` on the SDK's admin entrypoint, cursor typed as one object so it
>   cannot be halved (`0ca2b56c`), proven against the PACKED tarball with `tsc --noEmit`.
> - The gateway's own bookkeeping (`d7e1c4ea`), added after measuring that deleting the counter increment
>   reddened ZERO tests: the projection guards drive a stub, so they say nothing about whether the
>   gateway counts.
>
> **The six "actionable failure states" were re-measured one by one on 2026-08-06, because the list was
> written as a list rather than as six claims about writers — and three of the six were already surfaced,
> one had no producer at all, and one reached the panel in a form nothing could read.**
>
> | State | Writer | Surfaced |
> |---|---|---|
> | incompatible version | `runner_gateway.go:1374` — the ONE production caller of `refuse()` | ✅ `connection_refusal` |
> | cordoned | `CordonRunner` → the `state` column | ✅ `state` |
> | revoked | `RevokeRunner` / `ErrRunnerRevoked` at enrolment → `state` | ✅ `state` |
> | at capacity | placement parks the run (`8f69dd2b`) | ⚠️ DERIVABLE, not distinct: `capacity` and `connections` are both on the view, and an operator compares them. Named here so it is not read as a field |
> | config refused | `serve.go:359`, the machine's own verdict | ✅ **fixed 2026-08-06** — see below |
> | preflight refused | **NONE.** `grep -rn 'preflight' --include='*.go' apps packages cmd adapters` finds only `coordinator/migrate.go`, the boot MIGRATION check. There is no device preflight in this tree | ❌ a state with no producer; surfacing it would be a field nothing writes |
>
> **`config refused` reached the panel and nothing could classify it.** `"refused: not a positive integer"`
> was an ad-hoc string literal at one call site, beside two declared constants (`applied`, `not_read`). So
> the reason travelled and "show me the machines that rejected a setting" had no answer — the panel is
> TypeScript and cannot ask Go what a refusal looks like, and a second refusal arm would have written
> "rejected" or "invalid" and reached the panel as a verdict nothing grouped with the others. The prefix
> is now the contract (`VerdictRefused`, `RefusedVerdict`, `IsRefused`), and the guard drives the REAL apply
> path over a setting this build reads, one it does not, and a value it will not take.
> The existing refusal test asserted `!= VerdictApplied`, which passes on `not_read`, on `""` and on any
> word at all; it now asserts the machine says refused AND says why. Perturbed both ways: RED, RED.
>
> **And the comment that would have been read while adding a verdict was wrong.** It sent its reader to
> "settingApplier below" — a symbol that exists nowhere in the tree; the dispatch is a `switch` in another
> file. Corrected to name `ServeConfig.applySettings`.
>
> **NOT done, and none of it is claimed:** the machine-detail VIEW (the owner supplies designs — no UI is
> invented here); the bounded live refresh (a fleet event stream or one documented poll); and reading a
> FINISHED session's transcript from the panel.

**Goal:** The admin screen answers whether a device is connected, what it is doing and what it did.

**RED first**

- An open authenticated runner WebSocket makes only that runner `online`; closing it makes it `offline`.
  `last_seen_at` remains a separate durable timestamp.
- A runner with two concurrency connections is one online machine, not two machines.
- Runner A's occupancy history cannot be read by another project.
- Current occupancy links to a live session; a released occupancy remains in history with timestamps and
  reason.
- **A finished session is readable from the panel without shell access to the device.** Its transcript,
  tool calls and terminal reason are served by the control plane, which already holds them, and the
  assertion is that a machine whose account has been deleted still answers the question "what did that
  session do" — because the record was never on the machine. A view that reads a file on the device is
  the failure this leg refuses: that file is gone by design, and on a `user`-mode machine it is worse
  than gone, because it is the operator's.
- **A machine's own health output is bounded and carries no session content**: version, preflight result,
  applied configuration revision, connection history. Session output travels the run's own path, not the
  agent's log.

**Implementation**

- Index gateway sessions by runner id and expose `connection_state` plus connection count in the registry
  projection.
- Add keyset-paginated `GET /v1/runners/{id}/occupancies` backed by existing `runner_leases`.
- Add a machine detail view: identity/version/shape, connectivity, desired-versus-applied config, current
  sessions and history.
- Refresh bounded live fields through a fleet event stream or one documented bounded poll; do not infer
  health from browser time alone.
- Surface actionable failure states: incompatible version, preflight refused, config refused, at capacity,
  cordoned and revoked.
- Add the read methods to the admin entry point of the TypeScript SDK, not the browser-safe public client.

**Acceptance:** Pull a network cable, reconnect it, start/end a session and alter concurrency; every state
transition appears on Fleet with the correct runner/session identity.

#### T6b — Remote update: one more field, drained first, and reversible

**Goal:** The panel shows what version each machine runs, offers the newer one, and a click updates the
device without a human on it and without killing a session.

**IT IS NOT A NEW CHANNEL, AND THAT IS THE WHOLE DESIGN.** T3 already makes the admin plane the
configuration authority: a desired document is polled, the agent applies it, and the applied value is
shown only after the agent says it applied it. `desired_agent_version` is one more field in that
document. A separate update channel would be a second way to instruct a machine, with its own auth, its
own retry semantics and its own failure states — and this plan has already paid once for removing a
second path (T7's two service installers).

**THE PLANE NAMES A VERSION; IT NEVER SERVES BYTES.** The agent fetches the artifact from the release
location and verifies it against the **separately served checksum manifest** — the same rule as
`install.sh` (T7b), for the same reason. A control plane that hands out binaries is a CDN with a
database attached, and every deployment's trust story then includes "whatever my plane served me".

**DRAIN FIRST, AND REUSE THE PRIMITIVE THAT ALREADY EXISTS.** An agent holding leases must not swap its
own binary underneath a running session. The sequence is cordon → wait for occupancies to reach zero →
swap → restart, which is exactly T11's scale-down sequence minus the terminate. If T6b invents a second
drain, one of the two will be the one that is wrong when a session is lost.

**REVERSIBLE, BECAUSE THIS IS REMOTE CODE EXECUTION OVER THE WHOLE FLEET.** A bad release reaching every
Mac at once is the worst failure a fleet-update feature has, and it is the one it must be designed
against rather than tested for:

- the new binary is written beside the old one and swapped by atomic rename; the previous binary is kept;
- after restart the agent must re-connect and report ready within a bounded window, or it **rolls back to
  the kept binary by itself** and reports why — a machine that cannot phone home cannot be told to
  roll back;
- an update is refused if the resolved version is absent from the signed manifest, if the digest
  disagrees, or if it is **below the pool's minimum-version floor**, so a rollback cannot be used to
  reintroduce a version with a known hole;
- a pool-wide update applies in **batches with a configured size**, so a version that fails everywhere
  takes down one batch rather than the fleet;
- the plane keeps serving agents on version N while N+1 rolls out; the support window is T10's.

**RED first**

- A machine running an occupancy does not swap; it cordons, finishes, then swaps. The session's output
  is unaffected and its lease is never cut.
- A manifest-absent version, a digest mismatch and a below-floor downgrade are each refused **by name**
  and leave the running binary untouched.
- An update whose new binary fails to reconnect within the window rolls back automatically, and Fleet
  shows the machine online on the OLD version with the failure recorded — not `offline`, and not
  silently on the new one.
- A pool-wide update with batch size 2 across 5 machines never has 3 draining at once.
- The applied version is read from the **running agent's report**, never from the desired document —
  a panel that shows the version it asked for is a panel that cannot show a failed update.

**Implementation**

- Add `desired_agent_version` and a pool `minimum_agent_version` to the existing desired-config
  documents and their API/UI, with the same override precedence T3 defines.
- Add the agent-side updater: resolve, fetch, verify against the separate manifest, stage, cordon, drain,
  atomic swap, restart through the service manager, health-gate, roll back on failure.
- Extend the machine detail view with running version, desired version, latest available, and update
  state (`idle`, `draining`, `downloading`, `verifying`, `restarting`, `rolled_back`, `failed`).
- Add a per-pool update action with batch size and a per-machine one; both write the same field.
- Surface the refusal reasons from the RED list as actionable states, not as a generic error.

**Acceptance:** With three machines enrolled and one running a session, set the pool's desired version;
the idle two update and return online on the new version, the busy one waits for its session to finish
and then updates, and none of the three loses a lease. Then publish a deliberately broken build, set it,
and watch every machine roll back by itself and say so on Fleet.

**Version drift self-heals, which is why `install.sh` needs no version logic.** A machine installed
today and one installed next month converge on whatever the plane's desired version says, at their first
settings poll. Pinning stays available for image bakes (`PALAI_VERSION`, T7b) and is not the mechanism
that keeps a fleet consistent.

### T7 — Cut the real device distributions ⚠️ MOSTLY DONE (2026-08-06)

**Goal:** Install from release artifacts, not from the repository.

**RED first**

- ✅ Release index must contain `palai` for Darwin/Linux × amd64/arm64 — `scripts/release/build.sh` took
  `--runner-archs` (arch only, **linux hardcoded**), so no release could serve a Mac fleet. It now takes
  `--agent-targets <os>/<arch>` defaulting to all four, writes them into ONE `device/` directory so they
  share the `checksums.txt` install.sh fetches, and indexes them as kind `device-agent`.
  `TestAReleaseServesEveryDeviceTheInstallerCanResolve` builds with the DEFAULT targets and drives the
  REAL `install.sh` once per uname pair it accepts, over HTTP, asserting the binary that lands is the one
  from THAT triple's archive. **Neither side's platform list is written in the test.** Perturbed by
  restoring the linux-only default: both darwin legs RED, both linux legs PASS.
- ✅ …and must identify `palai-selfhost` separately — the release wrote the operator's CLI to `$out/palai`
  while every device archive carries the agent as `palai`. Two unrelated programs, one name: `./palai
  enroll` in a release directory answered "unknown command". The CLI matrix is now
  `cli/palai-selfhost-<os>-<arch>` with the host copy at `palai-selfhost`, and
  `TestTheNamePalaiMeansExactlyOneThingInARelease` walks the WHOLE release tree rather than the two paths
  that collide today — a third producer adding another `palai` is the same defect.
  **The operator COMMAND is deliberately unchanged.** Renaming it would touch 54 documents, 21 UAT cases
  and 14 scripts, and — the part that decides it — 5 runbook transcripts that are RECORDINGS of commands
  run against a live stack. `TestRunbookCommandsWereExecuted` requires every runbook command to appear in
  one, so the rename's real price is re-recording them on a running stack, not editing them. Editing them
  would be fabricating evidence. `install.md` states the artifact name and that a machine is an operator
  workstation or a fleet device, never both.
- ✅ Extracting the device package must reveal no server/admin command implementation —
  `cmd/runner/surface_test.go` asserts the LINKED package set (a grep of cmd/runner/*.go cannot see an
  admin verb reached through a helper). Perturbed with a non-test import of the control-plane API: RED.
  The rule names `apps/` only; `cmd/cli` is a `package main` and its `internal/` is closed by the
  compiler, so a second denied root would have been a branch that can never fire.
- ✅ The install script resolves an immutable URL and verifies a SHA-256 it fetched separately (T7b).
- ✅ A package built from a dirty tree or carrying an unstamped version is refused by the release gate —
  **both halves were open, and the second one silently disarmed a shipped guard.** The packager compiled
  `cmd/runner` with no `-X version.Stamp` and `-buildvcs=false`, so every installed agent reported `dev`;
  `version.Supported` is FAIL-OPEN for an unstamped build, so the §48.2 window that `cmd/runner`'s own
  comment says the version is advertised for could not fire on ANY packaged machine, the panel read one
  version for the whole fleet, and a desired-version rollout had nothing to compare. The packager now
  stamps and REFUSES a non-release stamp (`stamp_test.go` binds its shell rule to `version.IsReleaseStamp`
  over one table, so the two cannot drift); `scripts/release/build.sh` refuses a dirty tree, because
  `<version>+g<commit>-dirty` is the same string for any two trees on that commit — two binaries, one
  identity. Measured live in both directions on 2026-08-06.
  **Ceiling:** no test manufactures dirt in this shared checkout on purpose, so the dirty refusal is
  exercised by construction (a developer's tree is dirty, CI's is clean) rather than by a gate.

**THERE IS NO HOMEBREW TAP, AND DROPPING IT IS A CORRECTION RATHER THAN A SCOPE CUT.** An earlier draft
of this plan had Homebrew as the human path and `install.sh` as the fleet path. That is **two installers
for one binary, and worse, two SERVICE installers**: `brew services start palai` writes its own
LaunchAgent, while §3.2 makes writing and loading the service `palai enroll`'s job. Whichever ran second
would silently own the machine, and a fleet where two mechanisms can each claim the agent is a fleet
where "why is this Mac not connected" has two answers. One installer, one service owner.

What is given up is discoverability on a developer's laptop, and it is worth giving up: this is a fleet
agent that a provisioner installs, not a developer tool someone reaches for. If a tap is ever wanted, it
is added later as a **thin wrapper that calls the same script and does not define a service** — that
constraint is the whole reason this paragraph exists.

Consequences elsewhere in this plan: T10 no longer updates a tap, the owner no longer needs to create a
tap repository (§6.3), and Milestone B's gate is the install script rather than a formula.

**Implementation**

- Extend the existing deterministic/signature/SBOM release spine rather than adding a second packager.
- Produce macOS archives containing `palai`, optional `palai-agentd`, license/readme and service metadata;
  produce equivalent Linux archives and user unit.
- Publish immutable GitHub-release asset names plus a **separately served checksum manifest**, which is
  what `install.sh` verifies against.
- Keep installation and enrolment separate: the package installs with no secret, and the service is
  written and started by `enroll`, never by the installer.
- ✅ Add upgrade/rollback tests that preserve config and device identity —
  `TestAnUpgradeLeavesTheDEVICEIdentityUntouched` installs one version over another (two versions, so
  the installer takes its REPLACE path rather than its no-op path) with one HOME across both, against
  the real paths `packages/device` resolves for this platform. It holds by construction today —
  install.sh writes one path and knows nothing about the state directory — which is precisely why it
  needed a guard: perturbed with a plausible `rm -rf` "clean up an old install" line, RED, and every
  upgraded machine would have come back as a stranger holding a key the registry has never seen.
  **Rollback is not covered:** `install.sh` installs any version it is given, so a downgrade is the
  same code path with a smaller number; what is untested is a downgrade whose agent then meets a
  NEWER control plane, which is the §48.2 window's job and not the installer's.

**Acceptance:** On a clean Mac with no Palai checkout and no package manager, `install.sh` places the
binary, `palai enroll` writes identity and loads the service, and Fleet shows the same machine after a
service restart and an in-place upgrade. T10 repeats this against the public release assets.

#### T7b — `install.sh`, because a provisioner is not a person ✅ DONE (`7db770bd`, 2026-08-05)

> `scripts/install/install.sh` + `install_test.go`, which **execs the real script** rather than
> reimplementing it. Six cases, four of them refusals, each asserting the binary is ABSENT afterwards:
> tampered archive, manifest missing the artifact, truncated download, unsupported architecture, plus
> the happy path and idempotence. Perturbed twice (digest comparison, idempotence branch): both RED.
> Driven live 2026-08-06 against a local file server — installed `palai 0.1.0-local (darwin-arm64)`,
> digest `39a15a41…` verified.
> **NOT done here:** the release does not yet publish `palai-<v>-<os>-<arch>.tar.gz`; that is T7.

**One script is the only install path, for a person and for a provisioner alike.** A freshly booted
cloud Mac may have no Homebrew at all, so installing it first would be a second unattended installer
with its own prompts, prefix and update schedule, and an autoscaler that waits for `brew` has put a
package manager in its critical path. The deciding argument is narrower than convenience though, and it
is in T7: `brew services` would be a **second service installer** competing with `palai enroll`.

**Contract**

```sh
curl -fsSL https://<host>/install.sh | sh          # latest stable
PALAI_VERSION=1.4.2 curl -fsSL … | sh              # pinned, which is what an image bake uses
```

It installs the **binary only**. It never enrols, never asks for a key, never writes a service unit —
those are `palai enroll`'s job (§3.2), and keeping them apart is what lets an image be baked with no
secret in it and enrolled later from provider user-data.

**Requirements, each one a defence against a measured failure rather than a style preference**

- **The whole script is one `main` function invoked on the last line.** A download truncated mid-flight
  otherwise executes the half it received. This is the single most valuable line in the pattern and it
  is why Tailscale's installer is written that way.
- **It verifies a checksum it did not get from the artifact.** The manifest is fetched separately from
  the archive, and T10's release job already produces digests; the script refuses on mismatch rather
  than warning. A checksum served beside the file it protects, by the same credential, protects nothing.
- **It is not interactive and does not elevate.** No prompt, no `sudo`. `accounts` mode's one
  administrator action stays explicit and separate (§3.5).
- **It pins and reports what it installed** — version, digest, install path, target triple — so a fleet
  of a hundred machines can be asked what they are running without logging into any of them.
- **It is idempotent**: running it twice leaves one binary and does not disturb an enrolled identity or
  a running service.
- **It refuses an unsupported OS/arch by name**, rather than installing a binary that cannot run.

**Two consumers, and the plan owes both**

| Consumer | Path |
|---|---|
| A person with a Mac | The same `install.sh`. There is no second path to maintain, and no package manager to wait for |
| A provisioner / autoscaler | Bake `install.sh --version <pinned>` into the image, **or** run it from user-data. The pool key never enters the image; it arrives at boot through the provider's secret mechanism and `enroll` consumes it (T11) |

**Likely files:** `scripts/install/install.sh`, the release job that publishes it beside the manifest,
a test that runs it against a local file server with a corrupted archive, a truncated download and an
unsupported arch.

**Acceptance:** In a clean container/VM with no Palai anything, the script installs a pinned version
whose digest matches the manifest; a corrupted archive and a truncated script both refuse; a second run
changes nothing. Note that `deploy/airgap/install.sh` already exists and serves the air-gapped **server**
bundle — this is a different artifact for a different consumer and T0 must say so in both files, or the
next reader will assume one supersedes the other.

### T8 — Turn `@palai/sdk` into a real npm package ✅ PACKAGING DONE (`8be90b95`, 2026-08-06); PUBLISH DEFERRED

> One export map pointing at `dist` (the `publishConfig` dual-map was written and then removed — two
> maps means the thing developed against and the thing shipped are different objects). Build needs
> `rewriteRelativeImportExtensions`, since all 76 relative imports end in `.ts`; `noEmitOnError`, since
> the first build produced 51 errors AND a full `dist/`. The guard rides `make verify`, packs the real
> package, walks the export map, and installs the tarball into an empty directory with `--offline`.
> Perturbed by pointing exports back at src: 2 of 3 RED.
> **Owner decision recorded:** the SDK gets its own PUBLIC repository and npm publication happens from
> there; until then the reference stays local (`workspace:*`).

**Goal:** A normal TypeScript/JavaScript consumer can install and use the SDK without repository tooling.

**RED first**

- A packed-package content test refuses `.ts` source as a runtime export and requires compiled `.js`,
  `.d.ts`, README, license and package metadata.
- Clean ESM JavaScript and TypeScript fixture projects install the tarball with network disabled, import
  public/browser/admin entry points, typecheck and run.
- Browser exports cannot import the admin/system-key entry point.
- Package version must equal the release version, and the publication script must carry an explicit
  remote-version immutability check for T10.

**Implementation**

- Remove `private: true`; add `files`, `repository`, `license`, `engines`, `sideEffects` and public
  `publishConfig` metadata.
- Compile to `dist` and point conditional exports/types at built files.
- Make `sdk-package.sh` pack only the compiled package and verify the tarball contents.
- Add a clean consumer smoke that creates a client, streams a response and reads a session against the
  live self-host.
- Wire the protected release job to publish from a GitHub-hosted runner through npm trusted
  publishing/OIDC, with provenance and no long-lived write token. Keep it behind the external gate; T10 is
  the task that actually executes publication. Staged publication/manual approval may enforce the
  repository's two-person release rule.

Official npm contracts checked 2026-08-05:

- Trusted publishers and required OIDC workflow permissions: <https://docs.npmjs.com/trusted-publishers/>
- Provenance and public scoped publication: <https://docs.npmjs.com/generating-provenance-statements/>

**External owner gate:** the `@palai` npm scope/package and trusted-publisher binding must exist. Ask for
this setup only when the dry-run, packed consumer and release approval gates are green.

**There is no npm token to store, and that is deliberate.** The obvious shape is an automation token in
a CI secret; this plan does not use one. npm trusted publishing exchanges the workflow's short-lived
OIDC identity for publish rights at the moment of publication, so the repository holds **no long-lived
credential that can be exfiltrated from a log, a fork or a compromised action**. The owner binds the
package to this repository and workflow once, in npm's settings, and never hands over a secret. It also
produces provenance, which is what lets a consumer verify the tarball came from this commit rather than
from someone's laptop — and "never publish from a workstation" (T10) is only enforceable because of it.

If the scope binding turns out to be unavailable for the chosen npm plan, the fallback is a granular
automation token scoped to that one package, stored in the protected release environment only, and the
plan records that as a **downgrade with a named reason** rather than swapping it in silently.

**Acceptance:** Install the packed tarball into an empty directory and run a real response/session against
the self-host. T10 repeats the same consumer test against the registry version and verifies provenance.

### T9 — Prove the static remote fleet on real machines

**Goal:** Close the gap between local wire tests and the actual product.

**Topology**

- one cloud-hosted self-host control plane with public API and runner gateway DNS/TLS;
- Mac A and Mac B on different networks, neither with an inbound Palai port;
- one Linux VM on a third network;
- device artifacts installed by `install.sh` from the signed release assets;
- SDK installed from the packed or staged npm artifact, never imported from the checkout.

**Live cases**

1. Both Macs automatically enrol into the same Darwin pool with one reusable pool key.
2. Linux enrols into its Linux pool; presenting either pool's key to the other shape is refused.
3. Three concurrent SDK sessions land according to pool and measured capacity.
4. Mac commands return `hostname`, `whoami`, `sw_vers`, `xcodebuild -version`; the control-plane host's
   values are different and asserted absent.
5. File write/read, background start/probe/kill and workspace handoff run across the real gateway.
6. Stop/restart/reboot Mac A: it becomes offline, new work goes to Mac B, then the same Mac A runner id
   returns without another approval.
7. Revoke the pool key: a new machine is refused while enrolled machines renew. Revoke Mac A: it is cut
   and cannot recover with the still-live device key.
8. Change concurrency in Fleet and observe placement/occupancy change without restarting agents.
9. Decode logs, evidence, process environments and database JSON for seeded secret sentinels; report a
   non-vacuous zero leak count.
10. Scan listening sockets on each device and require zero Palai listeners.

**Evidence identity**

- source commit and version;
- public API URL and runner DNS name, with secrets redacted;
- package checksums and the install script's own digest;
- npm tarball/version/provenance identity;
- each physical/VM machine's OS/arch and opaque runner id;
- timestamps and session/run ids;
- explicit unproven limits.

**Acceptance:** The evidence demonstrates remote execution on both physical Macs, not merely enrolment or
a machine row.

### T10 — Wire real publication, upgrade and rollback

**Goal:** Convert the existing release rehearsal into an immutable development release, then RC/stable only
when repository policy permits.

**RED first**

- Publication refuses an existing GitHub asset or npm name/version.
- Publication refuses without the protected-environment approval and required OIDC permissions.
- Upgrade keeps device id/config and running leases; rollback remains protocol-compatible within the
  declared support window.

**Implementation**

- Give the protected job only the minimum `contents`/`id-token` authority needed by the actual registries.
- Upload signed GitHub release assets and the checksum manifest, then publish/stage npm. Never publish
  from a workstation.
- Verify every public artifact by downloading it back from its public location into a clean consumer.
- Keep snapshot/RC/stable labels truthful. Current one-maintainer policy may publish development snapshots
  but must continue refusing RC/stable until the two-person environment exists.

**Acceptance:** A clean reinstall from public locations and an N to N+1 agent upgrade pass T9's identity
and session checks.

### T11 — Add autoscaling without changing enrolment

**Goal:** Waiting work can start and retire machines using the already-proven agent bootstrap.

This task begins only after T9 passes. Static remote fleets are the prerequisite and remain supported.

**RED first**

- With waiting depth and no free slots, the scaler requests the exact missing capacity once; repeated
  reconciles are idempotent.
- A provisioning request carries a URL and secret reference/key-file payload, never a plaintext key in a
  log, database event or image.
- Scale-down cordons first, waits for zero active occupancies, then terminates; it never kills an active
  session.
- Provider timeout/failure leaves the run parked and reports the provider reason without dead-lettering it.
- Cooldown and provider minimum-lifetime constraints are fake-clock tested.

**Implementation**

- Add one small provider interface around create/status/terminate and one durable operation row for
  idempotency. Do not put provider SDK types in placement or runner packages.
- Compute demand from pool waiting depth plus effective free slots; configure min/max machines, sessions
  per machine, scale-up batch and idle/cooldown in the admin plane.
- Inject bootstrap data at launch through the chosen provider's secret/user-data mechanism; never bake a
  pool key into an image.
- On ready enrolment, correlate provider instance to runner id. On scale-down: cordon, drain, terminate,
  revoke runner identity, record result.
- Implement exactly one real provider adapter selected by the owner; a fake provider proves the core but
  does not close the live gate.

**External owner gate:** choose the first Mac provider and provide its live credentials only for the final
credential-gated test. Provider licensing/minimum-host lifetime remains provider-specific and must be
verified from its current primary documentation before encoding a constant.

**Acceptance:** The 21st session in a configured 5×4 pool parks, the scaler creates one real Mac, its
preinstalled `palai` joins with the same URL/key contract, the session runs there, and the machine is
cordoned/drained/terminated according to the configured provider floor.

---

## 5. Milestones and stop/go gates

### Milestone A0 — The same contract, no DNS required

**Progress, 2026-08-06 (live, on this machine):**

| leg | state |
|---|---|
| a plane the device did not start | ✅ measured — `http://127.0.0.1:60351/healthz` → `ok` |
| the agent installed by `install.sh` | ✅ measured — `palai 0.1.0-local (darwin-arm64)`, digest verified |
| `palai enroll` with only URL + key file | ⛔ blocked, then FIXED: the gateway certificate carried `DNS:control-plane` and nothing else, so enrolment by address failed `x509: … doesn't contain any IP SANs`. `8e058415` adds `localhost`, `127.0.0.1`, `::1` to the SERVER certificate. Re-measured on a fresh stack home: SANs present. |
| the machine appears in Fleet | ⏳ pending a running plane on the fresh home |
| an SDK session runs on THAT Mac | ⏳ |
| three restarts, one machine row | ⏳ |
| power cycle with nobody touching it | ⏳ (DoD 20's unmeasured case) |

**A bring-up finding worth keeping:** `local up` builds the control-plane image and that build hangs on this machine (a recorded Docker-frontend hang). The NATIVE posture — control plane as a process, only Postgres/object-store in Docker — needs no such image and is also the posture a Mac fleet machine is in.


**This milestone exists because every other one is blocked on an owner input, and that is a sequencing
defect rather than a fact about the work.** T1's live gate needs a public DNS name and certificates
(owner input 1); T9 needs three machines on three networks; T8 needs an npm scope. None of
those are needed to demonstrate the thing this plan is actually about — **an agent that is handed a URL
and a key and joins a plane it did not start.**

Cloud, remote and local are the same topology with a different hostname. So prove it at the cheapest
hostname first:

- one admin plane running anywhere the device can reach — a laptop, a LAN box, a cloud VM; the plane does
  not know the difference;
- the agent binary on a Mac, started with **only** `PALAI_CONTROLLER_URL` and
  `PALAI_ENROLLMENT_TOKEN_FILE` (plus `PALAI_CONTROLLER_CA` while the plane still uses a private CA);
- no `palai up`, no checkout on the device, no admin command, no other `PALAI_` name;
- the Mac appears in Fleet, an SDK-created session runs there, and `whoami` / `sw_vers` /
  `xcodebuild -version` return **that Mac's** values and not the plane's;
- restart the agent three times: one machine row, one id;
- **power the Mac off and on with nobody touching it**: it returns online by itself, same id, and serves
  the next session — this is the leg that measures the case §1.20 records as unmeasured, and its result
  decides whether a Mac fleet machine needs graphical auto-login;
- change concurrency in the panel: the machine applies it and says it applied it.

**Go when that passes with T2 and T3 complete.** T1 then becomes what it should be — putting a public
name and certificate in front of a contract that is already proven — instead of the gate that blocks the
first demonstration. The same binary, key file and URL that pass here are what a cloud plane receives; the
only thing T1 changes is who can route to the port.

### Milestone A — Remote static fleet

T0–T6 complete. Go only when a source-built agent on a second network runs a real session and returns
machine-specific output. This validates architecture before publication work.

### Milestone B — Installable product

T7–T8 complete. Go only when clean consumer tests install the actual archives/tarball with no source
checkout and no admin CLI in the device package.

### Milestone C — Public self-host release

T9–T10 complete. Go only when two real Macs and one Linux host pass the live topology from released
artifacts. This is the first point at which docs may say “install a remote machine”.

### Milestone D — Autoscale

T11 complete. Go only when one real provider machine, not a fake, is created, enrolled, used, drained and
terminated. Until then the product truth is “autoscale-ready bootstrap and capacity parking”, not
“autoscaling”.

---

## 6. Required owner inputs, requested only at their gate

1. Public DNS names and certificate paths for the API edge and runner gateway (T1 live gate).
2. Two real Macs and one Linux VM reachable outbound to the runner gateway (T9).
3. GitHub release environment and two-maintainer policy (T10). No tap repository is needed.
4. Ownership of the `@palai/sdk` npm name/scope and its trusted-publisher binding (T8/T10).
5. First autoscale provider choice and live credentials (T11 only).

No task reads or prints `.env.local`. Every live gate names the variable it needs and skips/refuses without
it according to the repository's existing rules.

---

## 7. Final repository gate

After T0–T11 source work and task-specific live proofs:

1. Run one consolidated acceptance review over explicit requirements, security, cleanup, package contents,
   migrations, tests and evidence integrity.
2. Fix only reproduced Critical/Important findings; one re-review loop by default.
3. Create the final source commit.
4. Run `make verify` once from that commit.
5. Run T9 and T11 live evidence once where their external inputs exist.
6. Create evidence-only commits; do not regenerate evidence whose source behaviour did not change.
7. Require a clean worktree, verify public artifacts by digest, and push.

The release statement must separately name what was proven for static remote devices, package
installation, npm consumption and provider autoscaling. Passing one is not evidence for another.
