# Session isolation, per-device configuration, and the live view

**Date:** 2026-08-02
**Status:** design, awaiting two decisions (§7)
**Origin:** palcore will be rewritten on top of palai. The requirement set below is the operator's,
stated verbatim in §1; everything in §2 is a measurement taken on this tree on 2026-08-02, with the
command that produced it, so a task's first step is to re-run them rather than trust this page.

---

## 1. The requirements, as stated

1. Configuration comes from the **admin panel** — not a bash export, not a dotenv file.
2. **No silent default.** (Read narrowly: no unannounced fallback. See §3.4 — a blanket removal of
   every catalogue default is not what this means and would delete safety values.)
3. An **isolated account path** must exist.
4. That account is **deleted when the session closes**, so a later session cannot read its data.
5. Session data lives under a **per-session directory** (`tmp/<session-id>/*` was the shape offered).
6. The agent reaches **nothing outside `tmp/*` and `repo/*`**.

And the visibility goal that motivated the whole exercise: the operator wants to watch the model
talk, watch every file read/write/edit, and watch the simulator being driven.

---

## 2. What was measured

Every row is a command. Re-run them; a changed number is the point of writing them down.

### 2.1 The surface

| Measurement | Command | Result (2026-08-02) |
|---|---|---|
| Mounted `/v1` operations | `grep -oE '"(GET\|POST\|PATCH\|PUT\|DELETE) /v1/[a-zA-Z0-9_/{}.-]*"' apps/control-plane/api/router.go \| sort -u \| wc -l` | **147** |
| Published contract operations | `python3 -c "import json;d=json.load(open('protocols/openapi/openapi-3.2.yaml'));print(sum(1 for p,o in d['paths'].items() for m in o if m in ('get','post','put','patch','delete')))"` | **10** |
| Registry event types | `python3 -c "import json;print(len(json.load(open('protocols/schemas/execution/event-types.json'))['events']))"` | **117** |
| Default tool names | `grep -oE 'var slack(Default\|Repository\|Publish)Tools = \[\]string\{[^}]*\}' cmd/cli/internal/stack/up.go \| grep -oE '"palai\.[a-z_.]+"' \| sort -u \| wc -l` | **10** |

The 10-of-147 gap is **not yet classified as a defect** — it may be a deliberate public/admin split.
That classification is work item §6.5 and must be settled before anyone calls it drift.

### 2.2 The event stream: what is declared vs what is written

Event type names are **never constructed dynamically**, so a literal grep is a sound search here —
this check exists because CLAUDE.md rule 1 is that the wrong number usually comes from the wrong
search:

```
grep -rnE 'Sprintf\([^)]*\.v1|"\.v1"|\+ "\.v1"' --include='*.go' apps packages   → 0 hit
```

With that established:

```
# registry types that appear nowhere in production Go/TS
→ 15, of which 5 are INITIAL STATES (no transition enters them, so nothing can emit them):
  session.created.v1  response.queued.v1  attempt.assigned.v1
  tool_call.proposed.v1  workspace.requested.v1
```

The remaining 10 are candidates with no writer: `model_step.failed.v1`,
`agent.revision.published.v1`, `tool.revision.published.v1`, `tool_set.revision.published.v1`,
`artifact.created.v1`, `trigger.delivery.received.v1`, and the four `schedule.occurrence.*`.

One is confirmed rather than a candidate:

```
grep -ciE 'event|emit' apps/control-plane/api/agents.go   → 0
```

`POST /v1/agents/{id}/revisions/{rev}/publish` exists and emits nothing.

### 2.3 The live view does not exist, and the transport is why

`apps/control-plane/internal/execution/model_dispatch.go:211-216` — the delta callback accumulates
into a local builder and does nothing else:

```go
var partial strings.Builder
onDelta := func(d modelbroker.Delta) {
    if d.Text != "" { partial.WriteString(d.Text) }
}
```

Its comment states the intent plainly: *"so an interrupt can record it as the explicit partial item
(spec §25.16)"*. So `model_step.delta.v1` is in the registry, in the AsyncAPI `x-event-types` list,
and in tests — with **no production writer**. A client sees `model_step.created.v1` then
`model_step.completed.v1`, and nothing in between.

**This is not an oversight, it is a transport consequence.** `apps/control-plane/api/events.go`:

```
:28   PollInterval time.Duration // journal tail poll cadence
:38   c.PollInterval = 500 * time.Millisecond
:67   "The journal is the source of truth"
```

The SSE endpoint replays the journal from a cursor and tails it by polling. There is no live
in-process fan-out. Emitting a delta per token through this path is one Postgres row per token,
surfaced at 500 ms granularity.

### 2.4 Confinement on macOS

`docs/research/macos-isolation-without-accounts.md` §1, 23 measurements on this host:

> **No. On a single Mac, there is no way to isolate concurrent agent sessions from each other
> without giving each session its own uid.**

The two that decide our design:

| # | Test | Result |
|---|---|---|
| T14 | `simctl spawn` writing outside the container, from inside Apple's App Sandbox | **ESCAPED** |
| T17 | `opendir("~/Desktop")` directly → DENIED ×3; same binary via `simctl spawn` → **ALLOWED** ×3 |

The mechanism that defeats Apple's supported sandbox **is `simctl`** — the binary this product
exists to drive. So requirement 6 is enforceable only as follows:

| Run's tool set | Confinement under one uid | Mechanism |
|---|---|---|
| `palai.workspace.file` only | **yes** | in-process: `WorkspaceFS.resolve` + `assertNoSymlinkEscape`, two independent guards |
| `palai.workspace.shell` also (required for xcodebuild/simctl) | **no** | argv is arbitrary; `simctl spawn` severs the process tree (T15: ppid = `launchd_sim` → `launchd`) |

`tmp/<session-id>/*` is a correct layout under either. Accounts do not change the layout; they
change who **owns** it — uid 501 shared, versus the session's own uid at mode 700.

### 2.5 Account lifecycle: the machinery exists, nothing drives it

```
grep -rn 'mac-sessions' --include='*.go' --include='*.ts' .   → 7 hits, ALL in tests/docs/mac_sessions_test.go
grep -rln 'sysadminctl|dscl \.'                               → scripts/ops/mac-sessions.sh + that test only
```

`scripts/ops/mac-sessions.sh` creates per-session non-admin accounts (marker
`dsAttrTypeNative:palai_mac_session`, uid base + index) and `down --apply` deletes them. But the
control plane never calls it: accounts are provisioned **up front** by an operator (`up --count N`)
and removed by an operator. There is **no per-session lifecycle today**, so requirement 4 is not a
fix — it is new machinery.

Capacity on this host, measured:

```
bash scripts/ops/mac-sessions.sh plan --count 4
→ M2 Pro / 16 GiB / Xcode 26.6 / console user salih (uid 501)
→ ceiling on this box: 1 concurrent session
```

### 2.6 Per-device configuration: the shape exists, one reader does not

`apps/control-plane/api/deployment_desired.go:107-136`. The desired document already carries
`plane` and `scope_id`, and a runner-plane write is refused with a sentence that names its own
missing piece:

> *"The storage is scoped for it and the catalogue can carry it, but the READER is a second binary:
> `cmd/runner` reads its environment at exec (main.go:117 for `PALAI_RUNNER_CONCURRENCY`, main.go:120
> for `PALAI_WORKSPACE_ROOT`) and nothing hands it a document. A row written here would be a setting
> no machine ever sees."*

The allow-list is structural, and this constrains requirement 1:

```go
if entry.Kind == kindPath || entry.DesiredValue == "" { continue }   // desiredWritable()
```

Path-kind settings are dropped **by kind**, deliberately — its comment notes that giving such an
entry a value grammar still yields a read-only setting. `PALAI_WORKSPACE_ROOT` is path-kind.

---

## 3. Design

### 3.1 Isolation is one design, not four requirements

Requirements 3, 4, 5 and 6 collapse into a single mechanism, because §2.4 leaves exactly one that
survives measurement:

- A session is allocated a **macOS account** of its own (`palai_mac_session` marker, non-admin).
- Its workspace is minted **inside that account's home** at mode 700 — `tmp/<session-id>/*` in the
  operator's vocabulary, owned by the session uid rather than by uid 501.
- What lives **under the repo** is a clone or worktree the session is given, not the operator's
  working tree. This resolves the tension in requirement 5 vs 4: bytes the next session must not see
  cannot sit in a directory the next session owns.
- The account is deleted at session close, which is what makes "a later session cannot read its
  data" true rather than aspirational.
- Requirement 6 is then a **property of the uid**, not a path filter. A path filter over
  `palai.workspace.shell` cannot hold: T14/T17.

### 3.2 The account lifecycle needs a privileged actor — DECISION REQUIRED

Creating and deleting local accounts needs root. The control plane does not run as root and should
not. The options are a privileged helper (a small setuid/launchd-managed binary with exactly two
verbs, `create-session-account` and `destroy-session-account`, taking a session id and nothing else)
or running the Mac deployment's control plane as root. **The helper is the recommendation**; the
decision is the operator's because it changes the machine's trust model. See §7.

### 3.3 The architecture hangs on one UNVERIFIED row

`docs/operations/mac-sessions.md` §5 carries two unverified claims, and one of them decides whether
this design is viable at all:

> *"A simulator can be driven in a non-console login session"* — **UNVERIFIED**
> *"Two accounts can drive simulators concurrently"* — **UNVERIFIED**

Recording is known not to need an Aqua window; **driving is known to need one**. Since §3.1 runs
each session in its own account, a session that cannot drive a simulator from a non-console login
makes the isolation design and the product goal mutually exclusive on this hardware.

This is measurable today and must be measured before any code lands:

```
sudo bash scripts/ops/mac-sessions.sh verify --simulator
```

The script's own contract: *"UNVERIFIED means it could not run — never that it passed."*

### 3.4 "No default" means no silent fallback

Read literally, requirement 2 would delete `PALAI_SANDBOX_WALL_TIME: 10m` — a safety value measured
rather than picked. The defensible reading is the one the tree already applies to
`PALAI_MODEL_PROVIDER`, where every unrecognised value silently selects the fake adapter and
`palai up` refuses rather than allow it: **a setting whose absence changes behaviour must be
declared, and its absence must be an error rather than an assumption.** Safety ceilings keep their
defaults; selectors and roots lose theirs.

### 3.5 Per-device configuration is the runner-plane reader

Requirement 1's "for device X, see it and change it" is precisely the refusal in §2.6. The work is:
teach `cmd/runner` to fetch and apply its scoped desired document at start, then turn the refusal at
`deployment_desired.go:131` into an accept, then surface the scope in the console. `PALAI_WORKSPACE_ROOT`
is already one of the two settings that binary reads, so per-device workspace roots fall out of it.

The path-kind allow-list (§2.6) still refuses it on the **control** plane. Whether the kind filter
should be evaluated per-plane is the second decision in §7.

### 3.6 The live view: coalesce into the journal, do not bypass it

Three transports were considered against §2.3:

| Option | Cost | Resumable | Latency |
|---|---|---|---|
| A — one journal row per token | one Postgres row per token | yes | ~500 ms |
| **B — coalesce deltas, flush on a window, one row per flush** | bounded | yes | ~500 ms + window |
| C — separate live channel, never persisted (the CMA shape) | none | **no** | true streaming |

**B is the recommendation for the first move.** It works with the transport that already exists,
keeps the journal as the single source of truth, keeps `Last-Event-ID` resume intact, and bounds
cost. C is where CMA landed (`event_start`/`event_delta` are explicitly *never persisted* there) and
remains the right end state for token-level streaming; it needs a fan-out path that does not exist
today and should not block the first visible improvement.

---

## 4. What this design does NOT claim

- It does not claim two sessions can drive simulators concurrently. §3.3 is unmeasured, and this
  box's ceiling is 1 session regardless.
- It does not claim accounts are a boundary between **different customers**. The research is
  explicit: `sudo` and local-root escalation defeat a uid, three such were patched in 2026 alone.
  Different customers still need different Macs. Accounts buy density for one customer.
- It does not classify the 10-of-147 contract gap as a defect. That is §6.5.

---

## 5. Verification

A claim here is only worth what re-running it proves.

- §2.2 counts: re-run the two greps; a changed count means an event gained or lost a writer.
- §2.3: the assertion is that no production writer exists. The perturbation that proves the test is
  honest: add a writer, confirm a client sees deltas; remove it, confirm the client sees none.
  A green result with the writer removed means the test measures something else.
- §2.4: the isolation rows are reproductions of T14/T17 on this host. Re-run them on any new
  hardware before assuming the verdict transfers — a ceiling inherited from another epic is dated
  (CLAUDE.md rule 4).
- §3.3: `verify --simulator` must print a PASS for the driving row, not an absence of failures.

---

## 6. Work items, in order

1. **Measure §3.3.** `sudo bash scripts/ops/mac-sessions.sh verify --simulator`. Nothing below is
   safe to build before this row is closed — it can invalidate §3.1.
2. **Live view (§3.6, option B).** Coalescing delta sink in `model_dispatch.go`, journalled as
   `model_step.delta.v1`, asserted end-to-end through the SSE endpoint.
3. **Runner-plane desired reader (§3.5).** `cmd/runner` fetches and applies its scoped document;
   `deployment_desired.go:131` becomes an accept; console surfaces the scope.
4. **Session account lifecycle (§3.1, §3.2).** Privileged helper with two verbs; create on session
   open, destroy on close; workspace minted in the session home at 700.
5. **Silent event writers (§2.2).** `agent.revision.published.v1` first — the endpoint exists and is
   silent — then the other nine candidates, each judged individually rather than as a batch.
6. **Classify the contract gap (§2.1).** Deliberate public/admin split, or drift. Settle before
   publishing anything.

---

## 7. Decisions required before item 4

1. **Privileged actor (§3.2).** A two-verb helper (recommended) or a root control plane on the Mac.
2. **Path-kind settings on the runner plane (§3.5).** The allow-list drops path-kind settings
   structurally and on purpose. Per-device `PALAI_WORKSPACE_ROOT` from a panel requires that rule to
   be evaluated per-plane. This weakens a refusal that was written deliberately, so it is the
   operator's call rather than the implementer's.
