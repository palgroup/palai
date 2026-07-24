# External orchestrator kit (§35.1 / §35.2)

The orchestrator kit (`import { Orchestrator } from "@palai/sdk"`) lets an **external durable
orchestrator** — Temporal, Restate, a CI pipeline, or a plain retry-loop script — drive a Palai run
through the canonical five-step contract. It writes **no vendor adapter**: it composes the existing
client (`responses`, `sessions.commands`), and the contract it pins is the one *any* orchestrator
follows. A real Temporal/Restate integration is an operator concern (plan §6 leg 6); this kit + the
`tests/conformance/orchestrator` fixtures are the canonical contract those integrations conform to.

## The five steps

| Step | Contract | Kit surface |
|------|----------|-------------|
| 1 | create-with-workflow-ID-metadata + idempotency-key | `start(workflowId, request)` |
| 2 | wait — webhook **or** SSE **or** poll | `waitByPoll` / `waitByStream` / `start({callback})` + `result` |
| 3 | message / approval / cancel command | `sendMessage` / `cancel` |
| 4 | structured result + artifacts | the terminal `Response` a wait returns (`output`, `metadata`; artifacts via `client.artifacts.listForResponse`) |
| 5 | after-timeout reconcile-by-key | `reconcile(workflowId, request)` |

`runActivity(workflowId, request, { wait })` composes steps 1→2→4 into a single durable activity an
orchestrator schedules.

## Two identities, kept separate

The contract keeps two identities **distinct and never conflated**:

- **`workflowId`** — the external orchestrator's own durable workflow identity. The orchestrator owns
  it and may replay it from its own history any number of times.
- **`runId` / `responseId` / `sessionId`** — Palai's **canonical run identity**, minted by the server
  on the *first* admission. The workflow id never replaces it and is never inferred from it (it rides
  `metadata.workflow_id` as untrusted external correlation only).

The bridge is the **idempotency key**, derived deterministically from the workflow id
(`workflowIdempotencyKey`). It is the **single retry owner** (§35.2): every replay under the same
workflow id carries the same key, so the server settles **exactly one run**. A retry *storm* does not
multiply runs — proven executably by AUT-013 and `tests/conformance/orchestrator`.

An orchestrator that lost its state re-derives the same key from its own workflow id and reconciles
back to the one run it created — **without ever having persisted the run id**.

## Who owns which timeout and retry (§35.2 verbatim)

Retry multiplication is a correctness bug (rejected by AUT-013). There is exactly **one** owner for
each retry and timeout; the two systems never replay each other's retries.

**Palai owns — the run's *internal* durability:**
- the run's own execution deadline and budget (`max_output_tokens`, budget caps);
- per-step / per-tool-call retries inside the run;
- the durable event journal and its at-least-once callback/outbox delivery.

**The orchestrator owns — the *workflow-level* retry:**
- re-issuing `start` under the same derived key (this is workflow-level retry — it reconciles, it does
  not create a second run);
- its own activity timeouts and the `waitBy*` deadline (distinct from Palai's run deadline — a wait
  timeout throws **without** canceling the run; `reconcile`/`result` recover it later);
- deciding whether a settled-**failed** run is retried as a *new* workflow attempt (a new workflow id,
  hence a new run) or surfaced as a terminal failure.

**The idempotency key is the seam between them.** The orchestrator's retries flow through the key, so
they collapse to one run; Palai's internal retries never surface as orchestrator retries.

> **Workflow history is never a single copy.** The orchestrator persists its own workflow history; Palai
> persists the run. Neither is the authoritative copy of the other — they are **reconciled by key**, not
> merged. `reconcile()` is that reconciliation made explicit: re-derive the key, replay the admission,
> resolve to the same canonical run.

## Wait modes

All three observe the *same* terminal; pick by how your orchestrator prefers to block:

- **poll** — `waitByPoll(run)`: GETs the run until a terminal status. Simplest; no open connection.
- **SSE** — `waitByStream(run)`: drains the session event stream to its terminal event (reuses
  `ResponseStream`, so a transport drop reconnects with `Last-Event-ID`, loss-less).
- **webhook** — `start(workflowId, request, { callback })`: Palai POSTs the terminal result to the
  callback URL. The webhook body is **untrusted** — the receiver calls `result(run)` to read the
  canonical outcome back from the API rather than trusting the payload.

## Honest ceiling

- **No native Temporal/Restate adapter is written** (§35.2 MAY; plan §5). The deliverable is the
  canonical contract + this kit + the conformance fixtures.
- A run against a **real Temporal instance** is plan §6 operator leg 6.
- The interop claim of this task is limited to **AUT-013 green** + the conformance suite green across
  the three wait modes — nothing broader.
