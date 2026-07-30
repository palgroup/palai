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

Three more, named rather than implied:

- **The output is not redacted, and the notification's excerpt is not either.** A synchronous shell
  result is masked before it is stored; a background task writes its own file, which nothing masks, and
  `palai.workspace.file` has never masked anything it reads. Since the exit notification quotes the last
  2 KiB of that file, those bytes also land in a durable row (`commands.payload`, then
  `delivered_messages`) — which the synchronous path does not do. **If your background commands receive
  credentials in their environment, assume anything they print is readable and stored.** Closing this is
  E26 T6's, and `background_tasks.env_keys` — key names, never values — exists so a read path can
  re-resolve and mask them.
- **There is no live progress stream.** Nobody tells the model that ten more lines arrived; it reads the
  file, or it waits for the exit.
- **A control-plane restart does not stop a task**, and it is not supposed to: the process belongs to
  the RUN. What it does lose is the in-memory handle registry, so `palai.workspace.background_kill` by
  id fails until the reaper adopts the row.
