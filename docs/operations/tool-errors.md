# When a tool says no

A tool call has three outcomes, not two, and the middle one is the one this page is about.

| Outcome | What the model sees | What the run does |
|---|---|---|
| It worked | the result | continues |
| **It refused** | **the refusal, in the same shape a result comes back in** | **continues** |
| The machinery broke | nothing | the attempt fails; the tool call is reconciled |

Until 2026-08-01 there was no middle row. **Every** error a tool returned killed the attempt, and for
any tool whose class takes a durable pre-write marker — the file tool is one — the run then wedged
permanently: the next attempt's ledger consult found the `executing` row, marked it `uncertain`, and the
reconciler escalated it to `manual_resolution`, which nothing resolves. The run sat `running` and the API
reported `in_progress` forever.

Measured on this machine, same agent revision, same repository binding, one variable changed:

```
file read "repo/README"            (exists)   -> completed,         run completed
file read "README"                 (missing)  -> manual_resolution, run WEDGED  (152s, still running)
file read "../../../../etc/passwd" (refused)  -> manual_resolution, run WEDGED
shell `false`                      (exit 1)   -> completed,         run completed
```

The last line is the whole diagnosis. **A non-zero exit code is a result FIELD** and always completed
cleanly; a Go error was not a result at all. And the third line is why it was urgent rather than tidy:
**a security control firing correctly took the run down with it.**

## What is a refusal and what is a fault

The distinction is drawn in code, in one place — `packages/tool-broker/answer.go` — and **an error nobody
classified is a FAULT**. The safe direction is the default; widening it is a deliberate edit to one call
site, never something that happens because an error message changed.

**Refusals** (the model is told, the run continues):

- a read, list, stat or checksum that failed — *a read that failed changed nothing*, so nothing about it
  can be uncertain;
- a refused traversal, an absolute path, an escaping symlink, a device/socket, a likely-secret read;
- a write refused by path-escape or non-regular-target, both decided before the temp file exists;
- arguments that fail their schema, and a tool name that resolves to nothing;
- `no workspace bound for this run`, `no sandbox shell runner wired`, `no durable task registry` — all
  pre-flight: **nothing ran**.

**Faults** (unchanged: the attempt fails and the tool call is reconciled):

- the shell runner could not say what the command did, or a background spawn errored — **a process may
  exist**;
- a tool's OUTPUT failed its schema — the tool already ran, so its effect is uncertain;
- a registry lookup that errored, an allocation root that will not resolve, a fence violation, and every
  ledger write.

A refusal reaches the model as:

```json
{"status":"error","error":{"code":"not_found","tool":"palai.workspace.file",
 "message":"read \"README\": open <workspace>/README: no such file or directory"}}
```

Codes: `not_found`, `refused`, `invalid_arguments`, `unknown_tool`, `unavailable`, `permission_denied`,
`failed`. The message is run through **both** redactors — the shape-based one and the value-based one
over the attempt's own environment values — because it lands in a durable `tool_calls` row and in the
model's context. The allocation's absolute host path is folded to `<workspace>`: every path the model may
name is workspace-relative anyway, and a refusal should not carry the operator's home directory to a
hosted provider.

The refusal is **committed to the tool ledger before it is delivered**, exactly as a success is, so
`tool_calls.result` is where you read what your agent was told:

```sql
SELECT name, arguments, result FROM tool_calls
WHERE run_id = '<run>' AND result->>'status' = 'error' ORDER BY created_at;
```

## What bounds a model that keeps asking for a missing file

Delivering the error removes an accidental stop, so the bound is explicit.

| Variable | Default | What happens at the limit |
|---|---|---|
| `PALAI_TOOL_ERROR_BUDGET` | **`16`** | the run reaches a **named terminal failure**, `tool_error_budget_exhausted` |

It counts **refusals, not tool calls**. A run that is making progress — reading files that exist, running
commands that work — is not bounded by it at all. A run whose model has been told "no" sixteen times is
not exploring. Sixteen is a judgement, not a measurement.

**Unset means bounded, and unbounded has to be written**: `PALAI_TOOL_ERROR_BUDGET=0` is the only way to
say "no ceiling", and a value the binary cannot parse falls back to 16 rather than to infinity — the same
rule `PALAI_BACKGROUND_MAX_WALL_TIME` follows (`docs/operations/background-execution.md`). It is read at
the moment it is used, so a change takes effect on the next call rather than on the next restart.

A replayed refusal counts like a fresh one, so an attempt reclaimed after a kill arrives at the same
number rather than starting over.

**Two honest ceilings:**

- **The count is per ATTEMPT, in memory.** A run whose attempts are reopened by something other than a
  transcript replay — a park, a pause, a resume — starts a fresh count. The refusals themselves are all
  durable rows, so making it per-run is a query and a column away; nothing needed it yet.
- **Nothing bounds a run that keeps making SUCCESSFUL tool calls.** There is no per-run step limit in the
  orchestrator or in the reference engine, the only wall clock is the §20.12 *admission* deadline (which
  stops applying once a run starts), and `max_tool_calls` is published in
  `protocols/schemas/execution/response-create.json` and typed in both SDKs with **no reader anywhere**:

  ```
  grep -rn 'MaxToolCalls' --include='*.go' . | wc -l   ->  2   (2026-08-01)
  ```

  Two declarations, zero uses. That is a real gap and it is not this page's budget — a caller who sets
  `max_tool_calls` today is setting nothing.

## Proof

| Claim | Proof |
|---|---|
| A missing file is answered, the row closes `completed`, and the same run then reads the file that IS there | `TestAMissingFileIsAnsweredToTheModelInsteadOfWedgingTheRun` |
| A refused traversal stays refused — an out-of-workspace canary appears in neither the delivered result nor the committed row — and the run stays alive | `TestARefusedTraversalIsAnsweredAndTheOutsideFileIsNeverRead` |
| An UNCLASSIFIED error still aborts the attempt and leaves its row for the reconciler | `TestAToolFaultStillAbortsTheAttemptAndLeavesItsRowUncommitted` |
| The budget ends the run with a named terminal; `0` is honoured; a replay is not free | `TestTheToolErrorBudgetEndsARunThatKeepsBeingRefused`, `TestAnExplicitlyUnboundedBudgetIsHonoured`, `TestAReplayedRefusalCountsAgainstTheSameBudget` |
| Which tool errors are refusals and which are faults | `TestEveryFailedWorkspaceReadIsAnAnswer`, `TestTheShellToolAnswersItsPreFlightRefusalsAndFaultsOnTheRest` |
| A non-zero exit code is still a result field | `TestANonZeroExitCodeIsStillAResultAndNotAnError` |
| A refusal carries no absolute host path, through the `/var` → `/private/var` alias | `TestARefusalDoesNotCarryTheHostPathOffThisMachine` |

The first six run under `make test-component TEST=postgres`; the rest ride `make verify`.

**And it was proven on a live stack**, not only in tests. A real agent on `gpt-4o-mini`, a real cloned
repository, a real missing filename — the model corrected itself on the next turn:

```
read  "README"        -> {"status":"error","error":{"code":"not_found", …}}
list  ""              -> artifacts, repo, scratch
list  "repo"          -> .git, README
read  "repo/README"   -> "Hello World!"
run completed
```

and, on the same stack, both traversals refused with the run alive:

```
read "../canary-outside.txt"       -> {"code":"refused","message":"path escapes the workspace: …"}
read "../../../../etc/passwd"      -> {"code":"refused","message":"path escapes the workspace: …"}
run completed; the canary's bytes appear in 0 tool_calls, 0 events, 0 responses
```
