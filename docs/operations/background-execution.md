# Background execution — running a build without waiting for it

**Read this first, because it decides whether the feature works on your stack at all: on a deployment
where `PALAI_DISPATCH_WORKERS` is `0`, **no** background exit notification ever lands.** The reconciler
that notices a finished task is built inside `startDispatch`, below its `if workers <= 0 { return }`, so
a stack at zero starts no reconciler — and the shipped `deploy/compose/compose.yaml` defaults that
variable to `0`. Background tasks still start, still run and still write their output; nothing tells the
model they finished, and a run parked on one stays parked. `deploy/compose/production.yml` defaults it to
`1`, so a production overlay is fine. If you run the base compose file and want notifications, set
`PALAI_DISPATCH_WORKERS=1`.

## What background execution is

A model can start a long command and keep working instead of waiting for it:

```
palai.workspace.shell { "argv": ["xcodebuild", "-scheme", "App", "test"], "background": true }
→ { "task_id": "bgt_…", "output_path": ".palai-session/bg/bgt_….log", "status": "running" }
```

The call returns **while the command is still running** and carries **no output**. That is the point:
a model backgrounds a five-minute build precisely so it does not have to hold that build's output. To
see how it is going, it reads the file with `palai.workspace.file`; to stop it, it calls
`palai.workspace.background_kill` with the task id.

When the command exits, the control plane delivers a message into the model's next turn naming the
task, its exit code, the output path and the last 2 KiB of that output.

## The moving parts, and which one is yours to operate

| Part | Where it lives | What an operator does about it |
|---|---|---|
| The process | a process group (native host) or a labelled container (`io.palai.bg=<task_id>`) | nothing, until you are hunting an orphan |
| The row | `background_tasks`, one per started process | read it; it is the audit record and it is never deleted |
| The output | `<allocation>/.palai-session/bg/<task_id>.log` | it is excluded from snapshots and changesets by design |
| The notification | the reconciler's sweep, every 30s | **`PALAI_DISPATCH_WORKERS` must be > 0** |

## Reading the state of things

```sql
-- what is running right now, across every tenant
SELECT id, run_id, posture, handle, started_at FROM background_tasks WHERE state = 'running';

-- what finished and was never announced to anybody (see "orphaned notices" below)
SELECT id, run_id, state, exit_code, finished_at FROM background_tasks
WHERE notified_at IS NOT NULL AND finished_at IS NOT NULL ORDER BY finished_at DESC LIMIT 50;

-- runs parked on a task: waiting, with a live task, and nothing wrong
SELECT r.id, r.state, b.id AS task, b.state FROM runs r
JOIN background_tasks b ON b.run_id = r.id WHERE r.state = 'waiting' AND b.state = 'running';
```

`state` is what the operating system said, never what the control plane hoped:

- `running` — the kernel or the container daemon still lists it.
- `exited` — it is gone. `exit_code` is `NULL` when this control plane did not observe the exit (it
  restarted since the spawn). **`NULL` is not `-1` and is not `0`**: nobody knows, and no number is
  invented to hide that.
- `lost` — a process may be at that pgid and **we cannot prove it is ours**. A `lost` task is never
  signalled, ever. The cost of that rule is our own orphan; the cost of the other rule is somebody
  else's process.
- `killed`, `expired` — a reaper's classifications.

## Exactly once, and what that costs

The notification is claimed by a single-winner `UPDATE` on `background_tasks.notified_at`. Two
reconciler ticks, two control planes and a crash-restart between them produce **one** notification,
because the winner is a row and not a flag in anybody's memory. There is nothing to reset, and running
two control planes against one database is safe.

## Orphaned notices

If a task finishes after its run has already reached a terminal state — a run that failed or was
cancelled while its build kept compiling — there is no model turn left for the notification to fold
into. It is **not dropped silently**. The row is stamped and the session journal gets a
`warning.raised.v1` with `code = background_notice_orphaned`, carrying the task id, the exit code and
the output path:

```sql
SELECT payload FROM events
WHERE type = 'warning.raised.v1' AND payload->>'code' = 'background_notice_orphaned'
ORDER BY seq DESC LIMIT 20;
```

The output file is still on the allocation. If these appear often, something is terminating runs while
their work is still going — look at cancellations and at run timeouts before you look at this feature.

## A ceiling worth knowing before you debug a prompt

The notification reaches the engine as a **user turn**. The model cannot tell it apart from something a
person typed; the text begins with `[palai:background]`, and that prefix is a **convention a prompt can
be written against, not a contract the protocol enforces**. If a model treats an exit notification as an
instruction from the user, that is this ceiling and not a bug in your prompt. Closing it means a
`role`/`source` field on the `message.deliver` frame, which is a protocol version.

Four more, named rather than implied:

- **The output IS redacted on the way out, and the file on disk is not.** Since E26 T6 both places a
  task's output reaches the model — a `palai.workspace.file` read and the exit notification's 2 KiB
  excerpt — mask it from the same code, applying both redactors: the shape-based one (API-key and
  bearer-token patterns) and the value-based one, which **re-resolves this run's environment values at
  read time** from `background_tasks.env_keys` (key names, never values). So a build that echoes its own
  environment lands `***` in `commands.payload`, in `delivered_messages` and in `tool_calls.result`.
  **The bytes on the allocation are untouched**, which is what an operator wants when debugging and is
  the reason the file is `0600` under `.palai-session`. Redaction is literal substring matching: a
  command that base64s a value or prints it one character per line defeats it, and nothing can prevent
  that — giving a build a credential is the build having it.
- **A background process holds its environment for its whole life, which is why a task that carries one
  cannot be unbounded.** `PALAI_BACKGROUND_MAX_WALL_TIME=0` is a legitimate choice for a task with no
  credential; a spawn on a run whose revision names an environment is **refused** under it. A same-uid
  process can reach the value — which is **equally true of a synchronous command**, same executor and
  same uid, and is governed by `MAC-P6`. Background does not widen that exposure, it lengthens it, and
  the length's ceiling is the wall time. **One correction to the usual wording, measured on macOS 26.3
  rather than assumed:** `ps -E` / `ps e` does **not** print another process's environment on a Mac, not
  even your own, and there is no `/proc`. On the **container** posture `/proc/<pid>/environ` inside the
  sandbox does. So on the native Mac the reachable route is a debugger or the process's own files, not
  a `ps` flag — the risk direction is unchanged, the usual demonstration of it does not work here.
  **The narrowing path has a name and is not this epic's:** hand the value as a short-lived file handle
  instead of an environment variable, which is already how the broker passes the push credential.
- **There is no live progress stream.** Nobody tells the model that ten more lines arrived; it reads the
  file, or it waits for the exit.
- **A control-plane restart does not stop a task**, and it is not supposed to: the process belongs to
  the RUN. Since E26 T5 it does not lose the handle either — there is no in-memory registry any more, and
  a restarted plane resolves a task id straight from the row. See "the restart, and the machine it does
  not survive" below for the part that genuinely does not survive.

## The four ceilings, and what each one refuses

Every one of them is read from the environment **at the moment it is used** — three at spawn, one on the
sweep — so a change takes effect on the next call rather than on the next restart.

| Variable | Default | What happens at the limit |
|---|---|---|
| `PALAI_BACKGROUND_MAX_WALL_TIME` | **`60m`** | the reaper **kills** the task, records `expired` and the model is told |
| `PALAI_BACKGROUND_MAX_PER_RUN` | **`5`** | the next spawn of that run is **refused**, not queued |
| `PALAI_BACKGROUND_MAX_PER_HOST` | **`20`** | the next spawn on the machine is **refused**, not queued |
| `PALAI_BACKGROUND_LOG_TTL` | **`24h`** | the output file of a settled task is deleted |

Four things about that table are decisions rather than defaults, and each is likely to matter to you
before the numbers do.

**Unset means bounded, and unbounded has to be written.** `PALAI_BACKGROUND_MAX_WALL_TIME=0` is the only
way to say "no ceiling", and a value this binary cannot parse falls back to 60 minutes rather than to
infinity. That is the opposite of `PALAI_FLEET_PARK_TTL`, where unset means never — and deliberately so:
there is no honest default for how long a rented Mac takes to arrive, and there is one for how long a
build may hold a machine.

**The enforcer is the reaper, not a timeout.** The deadline is a column, read on the reconciler's 30s
sweep, so enforcement is accurate to a tick and **does not happen at all on a stack with
`PALAI_DISPATCH_WORKERS=0`** — the same first paragraph as the notification. A `context` would have been
accurate to the millisecond and would also have died with the process the task exists to outlive.

**A ceiling refuses; it never queues.** A queue would be a delay the model cannot see: it would believe
five builds were running while a sixth waited. A refusal is an answer — wait, kill one, or run it
synchronously. The machine ceiling is counted **across all tenants**, because the machine is shared; a
per-tenant twenty would give the Mac twenty times as many tenants as you have.

**The log collector walks directories, not rows.** It looks under each active allocation's
`.palai-session/bg/`, deletes files whose mtime is past the TTL, and **skips any file whose task is still
`running`** — a build printing nothing for a day is not a finished build. The `background_tasks` row is
never deleted; it is the audit record that a process existed, and you need the `lost` ones to stay.

```sql
-- tasks the reaper ended for outliving their ceiling
SELECT id, run_id, started_at, finished_at FROM background_tasks
WHERE state = 'expired' ORDER BY finished_at DESC LIMIT 50;
```

## Cancelling a run

Cancelling a run now **kills every live background task it owns** — `palai runs cancel`, the API's
cancel, and anything else that routes through `CancelRunReconciled`. Before E26 T5 it did not: the run
went `canceled`, its children were cancelled, its response was finalized, and the build kept compiling.

Two consequences worth knowing:

- The rows go to `killed` and are **stamped as settled without a notification**, because a cancelled run
  has no turn left to read one and whoever cancelled it already knows.
- A task whose handle cannot be **proven** to be ours goes to `lost` and **receives no signal**, on a
  cancellation exactly as everywhere else. The cancel still succeeds. So does a cancel whose kill failed
  for a transient reason — the reaper meets that task again on the next tick, rather than the whole
  cancellation failing and leaving the run itself running.

## The restart, and the machine it does not survive

A restarted control plane **adopts** what the rows describe: it probes each `running` task (a
`ContainerInspect` in the container posture, a pgid plus start-time comparison on the host), leaves the
live ones alone, settles the dead ones — with `exit_code` **NULL** where it cannot know — and marks the
unprovable ones `lost`. It can kill an adopted task by id, and a run adopted with a live task still
parks on it.

**Adoption only works on the same machine, and that is a consequence rather than an omission.** If you
move the control plane to another host, the host posture's pgids mean nothing there and every task
becomes `lost`; in the container posture the containers stay on the old daemon and this one cannot see
them. Both follow from there being exactly one machine: the execution relay that would let a run's shell
live somewhere other than the control plane's own host was planned for E24 and never shipped. Nothing
closes this before that relay does.

Practically: **drain before you move a control plane to a different host**, and before you do, look at
what is running.

```sql
SELECT count(*) FROM background_tasks WHERE state = 'running';
```

## The tracked ceilings, by id

Everything this page describes as a limit is also a row in
[`known-gaps-1.0.md`](known-gaps-1.0.md), so a ceiling you hit here has a place to be read about and a
place to be closed. None of the four is an RC-blocker.

| Row | What it is, in one line | Who closes it |
|---|---|---|
| `BGT-P1` | No live progress stream — reading the log is a PULL, and nothing pushes | E27 (a progress channel on the shell seam + `task_update` chunks) |
| `BGT-P2` | PID reuse on the host: a handle we cannot prove is ours is `lost` and never signalled, which leaves us the orphan on purpose | nothing at this layer; the ambiguity is the operating system's |
| `BGT-P3` | The exit notice folds in with a **user-turn role**; the `[palai:background]` prefix is a convention, not a contract | a protocol version adding `role`/`source` to `message.deliver` |
| `BGT-P4` | Adoption works on the same machine only | E27's execution relay |

`CAS-P2` is the same subject from the other side and it was **narrowed rather than closed** by background
execution: the silence of a long build can now be broken by reading a file, and what remains open is that
nobody pushes.
