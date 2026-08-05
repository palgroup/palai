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
2. A device needs exactly these bootstrap inputs:
   - `PALAI_CONTROLLER_URL`;
   - `PALAI_ENROLLMENT_TOKEN_FILE`, containing a pool-scoped `rpk_...` key;
   - `PALAI_CONTROLLER_CA` only for a private/self-signed deployment. Public TLS uses system roots.
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
10. The Fleet screen shows, per machine: live connection state, last seen, agent version, measured shape,
    isolation mode, desired/applied configuration, current occupancy and historical sessions.
11. A real session created through the packed TypeScript SDK runs `hostname`, file operations and
    `xcodebuild -version` on a remote Mac rather than on the control-plane host.
12. A clean Mac installs from the Homebrew tap and a clean Linux user installs from a signed release
    archive without a source checkout.
13. `npm install @palai/sdk` installs compiled JavaScript plus declarations; a clean consumer can create,
    stream and inspect a session against the self-host.
14. Two Macs on different networks pass the live fleet gate simultaneously. No device opens an inbound
    port.
15. Autoscaling, when implemented, starts from the same immutable machine image and injects only the URL,
    key file and optional private CA. It does not invent a second enrolment protocol.
16. **There is no whole-stack device path.** The admin plane and the device agent are two products that
    meet over one URL. No device command starts a control plane, a database, an object store or a Compose
    project, and no device command builds a binary from a source checkout.
17. **The device reads exactly three `PALAI_` names**, and the count is the gate. Measured 2026-08-05,
    `cmd/runner` + `packages/runner` read **26** (`grep -rhoE 'PALAI_[A-Z_]+' cmd/runner/ packages/runner/
    | sort -u | wc -l`). Everything except `PALAI_CONTROLLER_URL`, `PALAI_ENROLLMENT_TOKEN_FILE` and the
    optional `PALAI_CONTROLLER_CA` is either derived by the binary, delivered by the admin plane after
    enrolment, or deleted. A per-run value passed to a session's shell is not a bootstrap variable and is
    out of this count.
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

Foreground contract:

```sh
PALAI_CONTROLLER_URL=https://runner.example.com:8443 \
PALAI_ENROLLMENT_TOKEN_FILE=/secure/path/pool-key \
palai
```

For a public server certificate this is the complete contract. A private PKI adds
`PALAI_CONTROLLER_CA=/secure/path/ca.pem`.

For a background service, the package reads the same values from a user-owned config file and points at a
separate mode-0600 key file. Environment variables override non-secret config for automation; a key value
is accepted from `PALAI_ENROLLMENT_TOKEN` for ephemeral CI only, immediately cleared as it is today, and is
never accepted as an argv flag.

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
| The CLI compiling and launching the agent (`cmd/cli/internal/stack/native_runner.go` runs `go build -o … ./cmd/runner`) | It makes a source checkout and a Go toolchain part of the device contract. A packaged device has neither, so a fallback here means the packaged path is never the one exercised |
| `palai up` / `palai local up` as anything a device runs | It starts a control plane, a database and an object store beside the agent. A machine that runs its own control plane is not a member of a fleet |
| Every admin/server command in the device artifact — pool, provider, secret, environment, backup, restore, doctor-for-the-stack | DoD 4 and 5 already require their absence from the package; this row makes the absence a deletion rather than a packaging filter, because a filtered binary still carries the code |
| 23 of the 26 device-side `PALAI_` variables | Each one is an operator input that the key, the binary or the admin plane already knows. Keeping a reader "for one release" keeps the machine configurable from the machine, which is the thing being removed |
| Per-device identity inputs: `PALAI_RUNNER_ID`, `PALAI_RUNNER_DNS`, `PALAI_RUNNER_PRIVATE_KEY` | Identity becomes the device key (§3.4). A client-supplied id is exactly the trust this design removes |
| Pre-enrolment placement inputs: `PALAI_RUNNER_POOL`, `PALAI_RUNNER_POSTURE`, `PALAI_RUNNER_CAPACITY` | The key chooses the pool, the binary measures the shape, the admin plane sets concurrency (§3.3, §3.6) |
| Four derived URLs: `PALAI_ENROLLMENT_URL`, `PALAI_SESSION_URL`, `PALAI_RENEW_URL`, `PALAI_CONTROLLER_DNS` | Already derived from one address today; keeping the overrides keeps four ways to point a device at four different planes |

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

### T2 — Build zero-touch bootstrap and durable device identity

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
- Keep `palai-agentd` in the package as the optional accounts-mode helper; do not install it during the
  default Homebrew flow.
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

### T6 — Add authoritative fleet presence and machine/session inspection

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

### T7 — Cut the real device distributions

**Goal:** Install from release artifacts, not from the repository.

**RED first**

- Release index must contain `palai` for Darwin/Linux × amd64/arm64 and must identify `palai-selfhost`
  separately.
- Extracting the device package must reveal no server/admin command implementation.
- Homebrew formula install/test/service runs against an immutable URL and SHA-256.
- A package built from a dirty tree or carrying an unstamped version is refused by the release gate.

**Implementation**

- Extend the existing deterministic/signature/SBOM release spine rather than adding a second packager.
- Produce macOS archives containing `palai`, optional `palai-agentd`, license/readme and service metadata;
  produce equivalent Linux archives and user unit.
- Prepare immutable GitHub-release asset names and generate a tap formula with architecture-specific
  checksums. Exercise it through a temporary/local tap in this task; T10 performs the external publish.
- Define a Homebrew `service` that runs as the current user. `brew services start palai` therefore uses a
  LaunchAgent, not a root LaunchDaemon.
- Keep installation and enrolment separate: the package may install with no secret, and the service starts
  only after automation has placed valid config/key files.
- Add upgrade/rollback tests that preserve config and device identity.

Official publication contracts checked 2026-08-05:

- Homebrew taps/formulae: <https://docs.brew.sh/How-to-Create-and-Maintain-a-Tap>
- Formula/service and audit requirements: <https://docs.brew.sh/Formula-Cookbook> and
  <https://docs.brew.sh/Adding-Software-to-Homebrew>

**Acceptance:** On a clean Mac with no Palai checkout, install through the generated temporary tap, place
the two bootstrap inputs, then `brew services start palai`; Fleet shows the same machine after a service
restart and a formula upgrade. T10 repeats this from the public `palai/tap` location.

### T8 — Turn `@palai/sdk` into a real npm package

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

**Acceptance:** Install the packed tarball into an empty directory and run a real response/session against
the self-host. T10 repeats the same consumer test against the registry version and verifies provenance.

### T9 — Prove the static remote fleet on real machines

**Goal:** Close the gap between local wire tests and the actual product.

**Topology**

- one cloud-hosted self-host control plane with public API and runner gateway DNS/TLS;
- Mac A and Mac B on different networks, neither with an inbound Palai port;
- one Linux VM on a third network;
- device artifacts installed from the signed release/Homebrew path;
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
- package checksums and Homebrew formula commit;
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

- Publication refuses an existing GitHub asset, npm name/version or Homebrew formula version.
- Publication refuses without the protected-environment approval and required OIDC permissions.
- Upgrade keeps device id/config and running leases; rollback remains protocol-compatible within the
  declared support window.

**Implementation**

- Give the protected job only the minimum `contents`/`id-token` authority needed by the actual registries.
- Upload signed GitHub release assets, publish/stage npm, then update the tap from the immutable asset
  digests. Never publish from a workstation.
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

**This milestone exists because every other one is blocked on an owner input, and that is a sequencing
defect rather than a fact about the work.** T1's live gate needs a public DNS name and certificates
(owner input 1); T9 needs three machines on three networks; T7/T8 need a tap and an npm scope. None of
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
3. GitHub release environment/two-maintainer policy and the separate Homebrew tap repository (T10).
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
