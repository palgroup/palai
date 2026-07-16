# Standalone Agent Platform — Complete Product and Architecture Specification

> Status: Complete product and architecture draft for stakeholder review. This file is the single source of truth for product behavior, architecture, public contracts, security boundaries, deployment modes, and acceptance criteria. The implementation master plan is intentionally produced only after this specification is reviewed.
>
> Last updated: 2026-07-16
>
> Spec revision: 1.0-review

## Document map

- Sections 1–18: product decision, use cases, public surfaces, domain, configuration, and decision record.
- Sections 19–23: normative HTTP/event/webhook/resource/SDK/CLI contracts.
- Sections 24–31: standalone control plane, kernel, recovery, models, tools, sandboxes, repositories, and specialized workers.
- Sections 32–38: triggers, schedules, queues, external orchestration, Slack, investigation automation, and A2A.
- Sections 39–48: tenancy, identity, secrets, data governance, billing, packaging, self-host, SaaS, console, and upgrades.
- Sections 49–57: threat model, audit, supply chain, observability, reliability, SLO/DR, testing, and evaluations.
- Sections 58–69: build-versus-reuse, independent repository, licensing/governance, rejected alternatives, UAT, traceability, sources, and review boundary.

## 1. Executive decision

Build a standalone, provider-neutral agent execution platform that works through the same API and SDKs in local, self-hosted, and managed SaaS deployments.

The platform supports four public usage surfaces over one execution kernel:

1. **Responses** — profile-free, single-shot or continued calls comparable to modern model APIs.
2. **Sessions** — durable, attachable, interactive conversations with live steering and model/tool changes.
3. **Agents** — reusable, versioned configuration profiles for recurring work, triggers, and integrations.
4. **Tasks / A2A** — an interoperability facade for external agent systems, not the platform's internal persistence model.

These are not competing architectures and users do not have to choose one for the whole installation. “Session-first” describes the canonical internal model: every execution can be represented as a session plus one or more runs. The Responses, Agents, and A2A APIs are purpose-built facades mapped to that model.

There will be exactly one execution kernel. A single-shot request, a Slack conversation, a scheduled research job, and an autonomous coding task must share the same model routing, tool execution, sandbox, event, checkpoint, approval, accounting, and policy mechanisms. No facade may grow a second agent loop.

## 2. Product goal

Provide the reusable infrastructure needed to run remote agentic work safely and observably:

- accept work through API, SDK, webhook, schedule, queue adapter, chat integration, or A2A;
- create an isolated workspace and optionally clone a repository;
- run a configurable agent with any supported model provider;
- expose live text, reasoning summaries, tool activity, files, diffs, approvals, and status;
- allow a human or another system to attach, send a message, steer, pause, resume, or cancel;
- survive API, worker, process, container, and host failures without duplicating side effects;
- return text, structured data, files, patches, reports, or repository publication results;
- run locally, in a customer's cloud, or in the managed SaaS with the same public behavior.

The first product must be useful both as infrastructure embedded into another product and as a complete cloud-agent service.

## 3. Non-goals and boundaries

- Do not bind the public API to one model vendor, model naming scheme, agent SDK, workflow engine, sandbox implementation, cloud, Git host, or chat provider.
- Do not expose workflow-engine or container-orchestrator concepts as the public session model.
- Do not require a saved agent profile for an interactive or single-shot call.
- Do not make A2A the internal event log or workspace protocol.
- Do not allow model-visible tools to receive raw long-lived credentials.
- Do not promise that an in-flight provider request can change model. Configuration changes take effect at a safe model-step boundary; an immediate switch cancels the current step and resumes from durable context.
- Do not claim local and SaaS use identical infrastructure. They must have semantic and contract parity, while scale and isolation implementations may differ.
- Do not implement multiple agent loops for different entry points.

## 4. Product principles

### 4.1 One contract everywhere

Local, self-hosted, and SaaS clients differ only by base URL, authentication, enabled capabilities, quotas, and capacity. Resource shapes, error semantics, event ordering, SDK calls, and lifecycle states remain the same.

### 4.2 Profiles are optional; snapshots are mandatory

Users may call the platform without defining an agent. When a profile is used, a run pins an immutable profile revision. Every run also stores a redacted effective-configuration snapshot so later retries and audits do not depend on mutable defaults.

### 4.3 Durable state belongs to the control plane

Containers and hosts are replaceable compute. Sessions, messages, event journals, tool-call state, approvals, artifacts, usage, configuration snapshots, and portable checkpoints are control-plane records.

### 4.4 Capability-based security

Tools, network destinations, repositories, credentials, publication rights, device runtimes, and subagent creation are explicit capabilities. A lower configuration layer may further restrict a capability but may not expand beyond organization/project policy.

### 4.5 Open core without a crippled self-hosted edition

The functional execution core, API, SDKs, local deployment, standard model/tool adapters, basic multi-user operation, and observability are open source under Apache-2.0. Managed operations and enterprise governance are commercial surfaces; core execution semantics are not SaaS-only.

## 5. Validated external patterns

The design uses public systems as evidence, not as a product dependency or wire-contract clone.

| Source | Validated pattern | Decision taken here |
|---|---|---|
| [OpenAI Responses API](https://developers.openai.com/api/reference/resources/responses/methods/create) | A single create call can accept multimodal input, tools, structured output, streaming, background execution, storage, and continuation. | Provide a first-class `/v1/responses` facade with the same class of ergonomics, backed by the common kernel. |
| [Anthropic Messages API](https://platform.claude.com/docs/en/api/messages/create) | Single queries and stateless multi-turn calls use one message API; prior turns may be supplied by the caller. | Support profile-free stateless requests even when server-side storage is disabled. |
| [Anthropic streaming](https://platform.claude.com/docs/en/build-with-claude/streaming) | Server-sent events stream text and tool-use deltas. | SSE is the default server-to-client streaming transport. |
| [Cursor SDK release](https://cursor.com/changelog/sdk-release) | Remote agents expose durable runs, SSE streams, cancellation, stable errors, and resume from an event ID. | SDKs expose both high-level helpers and resumable event streams. |
| [Cursor subagents](https://cursor.com/changelog/2-4) | Specialized child agents can run in parallel with isolated context. | Subagents are optional child runs with explicit model, tools, budget, and parent linkage. |
| [Cursor cloud agent lessons](https://cursor.com/blog/cloud-agent-lessons) | Remote coding needs isolated workspaces and a different reliability model from local chat. | Workspace and sandbox lifecycles are first-class, independently recoverable resources. |
| [A2A specification](https://a2a-protocol.org/dev/specification/) | Agent interoperability centers on contexts, tasks, messages, artifacts, and streaming. | Implement A2A as a boundary adapter mapped to richer internal objects. |
| [CloudEvents](https://github.com/cloudevents/spec) | A standard event envelope improves interoperability. | Public webhooks use a CloudEvents-compatible envelope and versioned typed data. |
| [RFC 9457](https://www.rfc-editor.org/rfc/rfc9457.html) | HTTP APIs can return machine-readable problem details. | All non-stream HTTP errors use Problem Details plus stable platform error codes. |
| [OpenAPI](https://spec.openapis.org/oas/v3.2.0.html) and [AsyncAPI](https://www.asyncapi.com/docs/reference/specification/v3.1.0) | HTTP and event contracts can be generated and validated from standard schemas. | OpenAPI 3.2.0 and AsyncAPI 3.1.0 are release artifacts and SDK inputs. |
| [LiteLLM](https://github.com/BerriAI/litellm) | A gateway can normalize many providers and provide routing/accounting. | Support it as an optional model-gateway adapter, never as the canonical model contract. |
| [Kubernetes Agent Sandbox](https://github.com/kubernetes-sigs/agent-sandbox) | Kubernetes can expose agent-oriented isolated sandbox primitives. | Build a sandbox-driver interface and a Kubernetes implementation; retain Docker and stronger isolation options. |
| [Temporal](https://temporal.io/about) | External durable-workflow systems can deliver retries, timers, signals, and long waits. | Treat durable-workflow systems as optional ingress/orchestration adapters. The standalone product must boot and provide its complete core lifecycle without requiring Temporal or any equivalent external system. |

## 6. One kernel, four public surfaces

### 6.1 Canonical mapping

| Public surface | Caller thinks in | Internal mapping | Typical use |
|---|---|---|---|
| Responses | input → output | ephemeral or durable session + root run | extraction, generation, research call, synchronous SDK use |
| Sessions | conversation + turns | durable session + root/child runs | product chat, Slack thread, human-steered coding |
| Agents | saved behavior | immutable agent revision + session/run | scheduled jobs, event automation, reusable specialists |
| Tasks / A2A | context + task + artifact | adapter-generated session/run/messages/artifacts | external agent-to-agent delegation |

### 6.2 Why the core remains session-first

A session is the smallest model that naturally contains all of the following without forcing a saved profile:

- an ordered conversation;
- multiple model turns and model changes;
- queued human messages;
- one active root run and parallel child runs;
- workspace bindings and checkpoint lineage;
- reconnectable events;
- approvals and external tool results;
- artifacts and final results.

An agent profile is configuration, not identity or durable execution state. A task is an interoperability unit, not sufficiently detailed storage for tool replay and workspace recovery. A response is a convenient request/response projection, not a separate runtime.

## 7. Required usage scenarios

### 7.1 Embedded interactive product

1. The host product creates or retrieves a session through its backend.
2. It submits user messages with an idempotency key.
3. It streams session events to its UI.
4. It may change model, tool set, or budget for the next model step.
5. The agent uses a sandbox workspace when the task needs code or files.
6. The host may attach approvals, client-executed tool results, or further messages.

### 7.2 Slack conversation

- One Slack thread maps to one durable session by default.
- The integration acknowledges Slack events immediately and processes them asynchronously.
- Bot message updates show streamed progress without turning the Slack message itself into the source of truth.
- Commands or natural-language controls can switch model, select an agent profile, approve an action, cancel, or request status.
- Duplicate Slack deliveries map to one platform message via source event ID.
- OAuth credentials stay in the secret broker and never enter model context.

### 7.3 Event-triggered store-review investigation

1. An external product emits a signed webhook or queue event containing the rejection payload and correlation ID.
2. A trigger references a pinned research-agent revision and structured report schema.
3. The platform creates a background session/run, enabling web research and supplied product metadata but no repository write capability unless explicitly granted.
4. The run returns a structured report artifact containing findings, citations, confidence, suggested remediation, and unresolved questions.
5. The platform sends an idempotent completion webhook; the caller can also poll or stream the run.
6. A retry reuses prior tool results where safe and never publishes duplicate external changes.

### 7.4 Single-shot SDK call

The caller needs no profile and no visible session lifecycle:

```ts
const result = await client.responses.create({
  model: "fast",
  input: "Analyze this rejection payload and return the likely causes.",
  tools: [{ ref: "web.search" }],
  output: {
    format: {
      type: "json_schema",
      name: "rejection_report",
      schema: reportSchema,
      strict: true
    }
  },
  store: false
});
```

The server still creates a transient internal session and run so policy, tool idempotency, usage, events, and failure handling remain identical to every other mode.

## 8. Public Responses API

### 8.1 Endpoints

```text
POST   /v1/responses
GET    /v1/responses/{response_id}
GET    /v1/responses/{response_id}/events
POST   /v1/responses/{response_id}/cancel
DELETE /v1/responses/{response_id}
```

`POST /v1/responses` supports ordinary JSON and `text/event-stream`. Every mutating request accepts `Idempotency-Key`.

### 8.2 Request contract

```json
{
  "agent_revision_id": null,
  "engine": null,
  "model": "fast",
  "instructions": "Return evidence-backed findings.",
  "input": "Investigate the supplied issue.",
  "tools": [{ "ref": "web.search" }],
  "tool_sets": [],
  "skills": [],
  "tool_choice": "auto",
  "parallel_tool_calls": true,
  "context": {
    "knowledge_bases": [],
    "memory": "session",
    "attachments": []
  },
  "delegation": {
    "mode": "allowed",
    "max_depth": 1,
    "max_children": 4
  },
  "capabilities": [],
  "output": { "format": { "type": "text" } },
  "max_output_tokens": 4000,
  "max_tool_calls": 20,
  "budget": { "max_cost_usd": 2.0, "max_duration_seconds": 600 },
  "store": false,
  "background": false,
  "stream": false,
  "previous_response_id": null,
  "session_id": null,
  "workspace": null,
  "repository": null,
  "callback": null,
  "metadata": {}
}
```

Rules:

- `model` accepts a project-defined model alias or an allowed fully qualified provider model ID. Aliases are preferred for portability.
- `agent_revision_id` optionally imports a pinned immutable profile; inline fields then act only as allowed run/session overrides and do not mutate it.
- `engine` selects an allowed engine revision/digest when the caller is permitted; otherwise the project/profile default resolves it.
- `input` accepts text or typed content items including text, image, file, prior assistant output, and tool result.
- `capabilities` requests a subset; it cannot grant anything absent from policy/profile/tool requirements.
- `previous_response_id` and `session_id` are mutually exclusive.
- `store: false` prevents a retrievable transcript after the configured short operational window; minimum security, abuse, billing, and aggregate usage records may remain according to deployment policy.
- `store: true` creates retrievable response state and permits continuation by `previous_response_id`.
- `background: true` returns after acceptance. It may also be streamed through the event endpoint with reconnection.
- `stream: true` returns SSE. Disconnecting never cancels the run.
- `workspace` is optional. It can reference an existing workspace or request one from an environment template.
- Output schemas use JSON Schema 2020-12.

### 8.3 Response lifecycle

```text
queued → provisioning → in_progress
                         ├→ waiting_for_tool
                         ├→ waiting_for_approval
                         ├→ waiting_for_input
                         └→ completed | failed | canceled | timed_out | budget_exceeded
```

The response object is a projection over the internal root run. It includes status, output items, model actually used, tool calls, usage, cost, warnings, error, and links to retained artifacts/events.

### 8.4 Stateless and stateful continuation

Both vendor patterns are supported:

- **Caller-managed state:** send the full desired message/input history with `store: false`.
- **Response chaining:** set `previous_response_id` when the prior response is retained.
- **Durable conversation:** set `session_id` or use the Sessions API.

Instructions and configuration do not silently inherit through response chaining unless the API explicitly marks a field as inheritable. The SDK provides a helper that resolves effective continuation config so callers do not accidentally rely on provider-specific behavior.

## 9. Sessions and live chat API

### 9.1 Core endpoints

```text
POST /v1/sessions
GET  /v1/sessions/{session_id}
POST /v1/sessions/{session_id}/messages
POST /v1/sessions/{session_id}/runs
GET  /v1/sessions/{session_id}/events
POST /v1/sessions/{session_id}/commands
GET  /v1/sessions/{session_id}/artifacts
```

`commands` initially supports `steer`, `pause`, `resume`, `cancel`, `change_config`, and `approve`. Each command has a stable ID and idempotent result.

### 9.2 Message delivery modes

- `queue`: add the message after the current root run reaches an input boundary.
- `steer`: deliver at the next safe agent-loop boundary.
- `interrupt`: cancel the current provider/tool step if cancelable, checkpoint, and resume with the new message.

Messages never modify an already-issued provider request in place.

### 9.3 Dynamic model and tool changes

The user may change model and tools during a chat. The change creates a new session configuration revision and applies at the next model-step boundary.

- A normal change lets the current step finish and affects the next step.
- An immediate change interrupts the current step, records partial output, and resumes using the new effective configuration.
- A profile revision remains historically pinned; session overrides do not mutate the profile.
- Policy may deny a requested model or tool. The denial is a typed event and structured error, not a silent fallback.

## 10. Agent profiles and automation

An `AgentProfile` is a named lineage. An `AgentRevision` is immutable executable configuration containing:

- pinned engine revision and namespaced engine settings;
- instructions and context assembly policy;
- default model route and permitted fallbacks;
- tool set and capability restrictions;
- skill, hook, knowledge-base, memory, and retrieval revisions/policies;
- environment/workspace template;
- optional repository defaults and allowed overrides;
- approval rules;
- time, token, tool-call, concurrency, and cost budgets;
- subagent/delegation policy;
- output schema and artifact contract;
- checkpoint/retention policy;
- labels and metadata.

Draft edits produce a new revision. Schedules and production triggers pin a revision; an explicit rollout action changes the pin. Interactive users may choose “latest” only when reproducibility is not required.

Triggers are separate resources so profiles remain reusable:

```text
manual | api | webhook | cron | queue | integration_event
```

Each trigger declares input mapping, deduplication key, agent revision, optional session-correlation rule, callback, retry policy, and concurrency policy.

## 11. Subagents and model delegation

Subagents are first-class but never mandatory.

- A child agent is represented by a child run with `parent_run_id`, its own context, model, tools, budget, events, and terminal result.
- The parent receives an explicit child result/artifact rather than unrestricted access to hidden child context.
- Parallel children are allowed within project/session limits.
- A profile can disable delegation, allow it, or require it for selected task classes.
- A caller can explicitly request `delegation.required: true` with a role such as `research` and a cheaper model route.
- The kernel exposes a guarded `delegate` tool only when the effective capability set permits it.
- Delegation depth, fan-out, total cost, total tokens, and wall time are bounded.
- If delegation is optional, the model may decide not to use it. If the caller marks it required, successful completion requires at least one conforming child run or an explicit policy/capability error.

The default routing policy supports roles such as `primary`, `research`, `review`, and `fast`, but they are project-defined aliases rather than vendor model names.

## 12. Canonical domain model

```text
Organization
└── Project
    ├── ModelRoute / ToolSet / Connection / SecretRef
    ├── EnvironmentTemplate / RepositoryBinding
    ├── AgentProfile → AgentRevision
    ├── Trigger / Schedule
    └── Session
        ├── ConfigRevision
        ├── Message / InputItem
        ├── WorkspaceBinding
        ├── Run → Attempt
        │   ├── ModelStep
        │   ├── ToolCall
        │   ├── ChildRun
        │   └── Checkpoint
        ├── Approval
        ├── Artifact
        └── EventJournal
```

Invariants:

- At most one active root run executes in a session unless the caller explicitly forks the session.
- Child runs may execute in parallel and cannot mutate the parent workspace concurrently without an explicit merge strategy.
- `run_id` remains stable across recovery; `attempt_id` changes.
- Every run pins an effective config snapshot, environment image digest, tool definitions, and model routing policy.
- Session identity is independent of model, process, container, runner, and host.
- Durable tool-call completion is keyed by stable `tool_call_id` and side-effect semantics.

## 13. Event and streaming protocol

### 13.1 Public transport

- HTTP/JSON for resource operations.
- SSE for ordered server-to-client events and output deltas.
- WebSocket only for genuinely bidirectional low-latency streams such as an interactive terminal.
- Signed webhooks for asynchronous delivery.
- A2A streaming only at the interoperability boundary.

### 13.2 Event envelope

Public events use a versioned CloudEvents-compatible envelope:

```json
{
  "specversion": "1.0",
  "id": "evt_...",
  "source": "/v1/sessions/ses_...",
  "type": "run.tool_call.completed.v1",
  "subject": "run/run_...",
  "time": "2026-07-16T12:00:00Z",
  "datacontenttype": "application/json",
  "sequence": 184,
  "project_id": "prj_...",
  "session_id": "ses_...",
  "run_id": "run_...",
  "attempt_id": "att_...",
  "data": {}
}
```

Properties:

- sequence numbers are monotonic within a session, not globally;
- SSE clients reconnect with `Last-Event-ID` or `after` sequence;
- delivery is at least once; event IDs make consumption idempotent;
- unknown fields are ignored within a major API version;
- secrets and raw credentials are forbidden in event data;
- large output, logs, and files are artifact references rather than oversized events.

### 13.3 Event classes

- session and configuration changes;
- message accepted/queued/delivered;
- run and attempt lifecycle;
- model step created/delta/completed/failed;
- tool request/approval/result;
- child run created/progress/completed;
- workspace provisioned/snapshotted/recovered;
- checkpoint created/restored/rejected;
- artifact created;
- usage/cost updated;
- warning, policy denial, error, and terminal result.

## 14. Configuration resolution

Effective configuration is resolved in this order, with restrictive policy always winning:

```text
deployment capabilities
  → organization policy
  → project policy
  → pinned agent revision (optional)
  → session config revision
  → run override
  → child/model-step override
```

Resolution outputs a content-addressed, redacted `ConfigSnapshot`. Secret references remain references. The snapshot includes provenance for every effective value so the UI and API can explain why a model/tool was selected or denied.

### 14.1 Common ExecutionSpec

Responses, Session ConfigRevisions, RunTemplates, AgentRevisions, Triggers, and ChildRuns resolve through one logical ExecutionSpec:

| Field group | Contents |
|---|---|
| engine | engine revision/digest selector and namespaced engine configuration |
| instructions | trusted instruction layers and prompt/template revisions |
| models | primary route, role routes, capability requirements, fallback/cache controls |
| tools | tool-set revisions, inline allowed tool references, tool choice, parallelism |
| skills/hooks | pinned revisions and activation configuration |
| context | attachments, knowledge bases/index revisions, memory policy, compaction/retrieval policy |
| workspace | environment revision, isolation/resource/network requirements, persistence/TTL |
| repository | binding, ref, branch/worktree, submodule/LFS and publication policy |
| delegation | allowed/required/disabled, roles, route/tool/capability subsets, depth/fan-out |
| capabilities | requested resource/action constraints; always intersected with policy |
| approvals | side-effect classes, approver groups, expiry, conversational approval policy |
| output | modalities, JSON Schema, artifacts, citations, validation/repair |
| budgets | cost/token/tool/time/resource/concurrency limits and warning thresholds |
| retention | store mode, transcript/event/artifact/checkpoint/snapshot policy |
| delivery | stream/background, callback/webhook, result routing |
| observability | content-capture prohibition/allowance, customer export labels |

The public facades may expose ergonomic aliases such as model or max_output_tokens. Before admission they normalize into ExecutionSpec and the final ConfigSnapshot.

### 14.2 Omitted, null, and empty

- omitted means inherit from the next higher configuration layer;
- null means clear an inheritable optional value only when that field permits clearing;
- empty collection means intentionally select none and is subject to minimum policy requirements;
- explicit scalar replaces the inherited value if authorized;
- restriction objects intersect rather than replace broader policy;
- SDKs preserve omitted versus null.

### 14.3 Profile plus overrides

When agent_revision_id is present:

- the immutable revision supplies defaults and capability ceiling;
- permitted session/run fields may narrow tools/capabilities/budget or select an allowed model role;
- instructions may append only at the configured trust layer;
- engine/environment/repository changes require the profile to mark them overrideable;
- required tool/delegation/output constraints cannot be removed;
- the ConfigSnapshot records every override and denial.

### 14.4 Profile-free defaults

Without a profile:

- project defaults resolve engine, model route, environment, retention, and baseline tools;
- the caller may provide inline instructions and allowed references;
- project policy still supplies hard ceilings;
- the resulting ConfigSnapshot is as reproducible/auditable as a profile-based run.

### 14.5 Config validation

Validation occurs before admission and detects:

- missing/ambiguous references;
- incompatible engine/protocol/checkpoint;
- model capability mismatch;
- tools requiring unavailable capabilities/connections;
- contradictory residency/network/provider rules;
- impossible output/context limits;
- child budget exceeding parent;
- environment/isolation/runner unavailability;
- callback/retention conflict.

Errors include field provenance and remediation without revealing hidden policy or secret values.

## 15. Initial public compatibility policy

- The canonical API is the platform's own `/v1` contract.
- Official TypeScript, Python, and Go SDKs ship together for the first stable release. Their transport layers are generated and their ergonomic layers are hand-written.
- OpenAI-style Responses and Anthropic-style Messages compatibility endpoints may be offered as adapters, but neither defines internal objects.
- A2A is an optional adapter.
- MCP is supported for tools and resources, not used as the session/run persistence protocol.
- Breaking public changes require a new major API version; additive fields and event types are allowed within `/v1`.

## 16. Specification completion and review boundary

This document resolves the product and architecture decisions required before implementation planning:

- public surfaces and canonical domain model;
- versioned HTTP/event/webhook/engine contracts;
- three official SDKs and CLI behavior;
- provider-neutral kernel, model routing, tools, subagents, context, checkpoint, and replay;
- sandbox, runner, workspace, repository, and capability-worker boundaries;
- self-contained coordination plus optional external adapters;
- Slack, webhook, queue, schedule, and A2A integration;
- identity, tenancy, secrets, retention, billing, quotas, self-host, and SaaS;
- security, observability, evals, SLO, disaster recovery, supply chain, and UAT;
- independent repository, open-source/commercial boundary, and governance.

Product branding, concrete programming-language/toolchain selection, commercial price numbers, and implementation task sequencing do not change these contracts. They belong to the implementation/launch master plan produced after stakeholder review. Any later implementation choice that would change a normative behavior in this file requires an explicit spec revision or RFC rather than a silent plan change.

## 17. Decision log

| Date | Decision | Status |
|---|---|---|
| 2026-07-16 | Treat session-first, agent-first, and task-first as public views over one session/run kernel, not mutually exclusive products. | Accepted |
| 2026-07-16 | Add a first-class profile-free Responses API for single-shot, streaming, background, stateless, and continued calls. | Accepted |
| 2026-07-16 | Keep agent profiles optional and version them immutably when used for automation. | Accepted |
| 2026-07-16 | Make subagents optional child runs; allow callers to explicitly require delegation and choose a cheaper route. | Accepted |
| 2026-07-16 | Preserve the same public API semantics across local, self-hosted, and SaaS. | Accepted |
| 2026-07-16 | Use an Apache-2.0 functional core with commercial managed operations and enterprise governance. | Accepted |
| 2026-07-16 | Keep model gateways, workflow engines, sandbox systems, MCP, and A2A behind adapters. | Accepted |
| 2026-07-16 | Build a self-contained narrow PostgreSQL-backed coordinator; do not require an external workflow system. | Accepted |
| 2026-07-16 | Make PostgreSQL the durable product state and object storage the artifact/snapshot/checkpoint byte store. | Accepted |
| 2026-07-16 | Ship one real reference kernel while allowing conforming OCI-packaged alternative engines. | Accepted |
| 2026-07-16 | Use a versioned JSONL supervisor/engine protocol with stable run, attempt, model-request, and tool-call identities. | Accepted |
| 2026-07-16 | Recover through exact continuation, portable checkpoint, then transcript reconstruction with explicit evidence. | Accepted |
| 2026-07-16 | Broker model, tool, repository, and secret access outside the engine sandbox. | Accepted |
| 2026-07-16 | Keep direct model adapters first-class and LiteLLM optional. | Accepted |
| 2026-07-16 | Treat tools by replay/side-effect class and never claim universal exactly-once execution. | Accepted |
| 2026-07-16 | Support local OCI, hardened, microVM, and dedicated isolation tiers; managed hostile multi-tenancy requires microVM/dedicated. | Accepted |
| 2026-07-16 | Connect customer/private runners outbound with short-lived workload identity and fencing. | Accepted |
| 2026-07-16 | Make repository preparation deterministic infrastructure work and publication granular control-plane capabilities. | Accepted |
| 2026-07-16 | Publish TypeScript, Python, and Go SDKs together for stable v1. | Accepted |
| 2026-07-16 | Use A2A 1.0 only as an interoperability facade and MCP stable revision only for tools/resources. | Accepted |
| 2026-07-16 | Create a new independent public repository rather than forking an assessed product. | Accepted |
| 2026-07-16 | Do not remove a consumer's legacy path until consumer-specific kill/replay/secret/host-change UAT passes. | Accepted |

## 18. Research log

| Date | Topic | Result |
|---|---|---|
| 2026-07-16 | Single-shot and stateful model APIs | Official OpenAI contract validates stored/background/streaming/continued Responses; official Anthropic contract validates single-query and caller-managed stateless multi-turn Messages. The platform supports both semantics through one Responses facade. |
| 2026-07-16 | Remote cloud agents | Cursor's public SDK and cloud-agent material validate durable runs, resumable streams, cancellation, workspaces, and subagents as expected product primitives. |
| 2026-07-16 | Interoperability | A2A is useful at the edge but too coarse to own checkpoints, tool replay, workspace state, and durable internal events. |
| 2026-07-16 | Provider normalization | A gateway is valuable but must remain replaceable; aliases and platform-owned usage records are canonical. |
| 2026-07-16 | Public SDK ergonomics | Mature SDKs validate generated typed transports plus handwritten streaming/retry/idempotency helpers and raw-response escape hatches. |
| 2026-07-16 | Events and API lifecycle | CloudEvents, SSE Last-Event-ID, RFC 9457, OpenAPI 3.2, AsyncAPI 3.1, Deprecation, and Sunset provide interoperable patterns without defining agent state. |
| 2026-07-16 | Agent frameworks | OpenHands, Pydantic AI, LangGraph, and Letta validate useful engine/state patterns but do not cover the complete product contract; framework objects remain internal/optional. |
| 2026-07-16 | Sandbox products | Daytona, E2B, Kubernetes Agent Sandbox, gVisor, Kata, and Firecracker validate reusable execution layers; none replaces the agent/control/SaaS product. |
| 2026-07-16 | Durable execution | External workflow systems are useful adapters, but transactional state machines, leases, timers, outbox/inbox, and fencing are sufficient for a self-contained core. |
| 2026-07-16 | MCP and skills | Latest stable MCP at the spec date is 2025-11-25; tasks are experimental. Agent Skills is a portable instruction/resource package, never an authority grant. |
| 2026-07-16 | Identity and secrets | OAuth BCP, audience binding, short-lived workload identity, envelope encryption, and one-operation secret leases reduce confused-deputy and credential exposure. |
| 2026-07-16 | Multi-tenant SaaS | PostgreSQL RLS, tenant-scoped object/derived stores, regional cells, and pooled/bridge/silo tiers support self-host and SaaS with one API. |
| 2026-07-16 | Billing | An immutable internal usage ledger with downstream OpenMeter/Stripe adapters avoids gateway/provider billing lock-in and duplicate settlement. |
| 2026-07-16 | Agentic security | OWASP Agentic Top 10 and NIST AI RMF reinforce deterministic capabilities, supply-chain verification, bounded autonomy, and continuous eval/red-team gates. |
| 2026-07-16 | Evaluation | Task-specific datasets, deterministic graders, calibrated model/human graders, trace evaluation, and held-out release gates are required; aggregate “vibe” scores are insufficient. |

## 19. Normative language and conformance

### 19.1 Requirement keywords

The terms **MUST**, **MUST NOT**, **SHOULD**, **SHOULD NOT**, and **MAY** are normative in the sense of [BCP 14](https://www.rfc-editor.org/info/bcp14). A component is conforming only when every applicable MUST is satisfied.

### 19.2 Product conformance profiles

The project publishes one versioned conformance suite and four profiles:

| Profile | Required behavior |
|---|---|
| Core API | Responses, Sessions, Runs, Events, Agents, Tools, Artifacts, authentication, idempotency, quotas, and the built-in durable job lifecycle |
| Execution host | Engine protocol, sandbox lifecycle, workspace recovery, capability enforcement, artifact transfer, and runner health |
| Self-host | Core API plus at least one execution host, PostgreSQL, object storage, local identity bootstrap, backup/restore, upgrade, and operational diagnostics |
| Managed service | Self-host behavior plus multi-tenant isolation, billing, abuse controls, regional placement, customer support controls, and published SLOs |

An installation MAY omit optional integrations, providers, high-isolation sandbox drivers, A2A, and compatibility facades. It MUST advertise absent capabilities through discovery and MUST return a typed capability error rather than silently degrading behavior.

### 19.3 Semantic parity

Local, self-hosted, and managed deployments MUST pass the same contract tests for:

- resource and event schemas;
- lifecycle state transitions;
- idempotency and retry behavior;
- tool request/result correlation;
- checkpoint compatibility decisions;
- cancellation and queued-message semantics;
- SDK method behavior;
- structured errors.

Parity does not require equal capacity, latency, provider availability, isolation tier, retention, or authentication mechanism. These differences MUST be discoverable before a run starts.

### 19.4 Maturity labels

Every public capability is labeled:

- experimental: may change within a minor release and is disabled by default;
- preview: additive feedback-driven change is allowed; production use requires explicit opt-in;
- stable: follows the compatibility and deprecation policy;
- deprecated: still behaves as documented but has a published migration and sunset date.

Experimental fields MUST live under an explicitly experimental namespace or require a feature header. Stable SDK methods MUST NOT depend on experimental behavior.

## 20. Public API foundation

### 20.1 API styles

The canonical API consists of:

- resource-oriented HTTP/JSON under /v1;
- SSE for ordered resumable event delivery;
- signed HTTP webhooks for server-initiated delivery;
- WebSocket only for interactive terminal byte streams;
- an optional A2A facade;
- optional provider-compatibility facades that translate into canonical requests.

GraphQL is not a canonical control surface. gRPC MAY be used between trusted internal components but MUST NOT define product semantics unavailable through the public API.

### 20.2 Resource inventory

The stable /v1 surface is organized as follows:

| Area | Resources and operations |
|---|---|
| Execution | responses, sessions, messages, runs, attempts, commands, approvals, events |
| Reuse | agent profiles, immutable agent revisions, tool sets, environment templates |
| Models | model providers, connections, model routes, immutable route revisions, capability discovery |
| Tools | tool definitions, MCP connections, skills, hooks, client-tool registrations |
| Compute | workspaces, snapshots, runners, runner pools, environment images |
| Automation | triggers, schedules, inbound events, webhook endpoints, outbound webhook deliveries |
| Output | artifacts, artifact versions, structured results, repository changesets |
| Governance | organizations, projects, memberships, service accounts, API keys, policies, secret references |
| Operations | usage, budgets, quotas, audit events, eval suites, eval runs, health and capability discovery |

The first stable release MAY expose administration resources through a separate /v1/admin namespace. Administration and execution resources still use the same identity, error, pagination, audit, and compatibility rules.

### 20.2.1 Stable endpoint families

The OpenAPI document is authoritative for exact schemas. Stable endpoint families are:

~~~text
# Responses
POST   /v1/responses
GET    /v1/responses/{response_id}
GET    /v1/responses/{response_id}/events
POST   /v1/responses/{response_id}/cancel
DELETE /v1/responses/{response_id}

# Sessions and execution
POST   /v1/sessions
GET    /v1/sessions
GET    /v1/sessions/{session_id}
PATCH  /v1/sessions/{session_id}
DELETE /v1/sessions/{session_id}
POST   /v1/sessions/{session_id}/messages
GET    /v1/sessions/{session_id}/messages
POST   /v1/sessions/{session_id}/runs
GET    /v1/sessions/{session_id}/events
POST   /v1/sessions/{session_id}/commands
POST   /v1/sessions/{session_id}/fork
GET    /v1/sessions/{session_id}/artifacts
GET    /v1/runs/{run_id}
GET    /v1/runs/{run_id}/attempts
GET    /v1/runs/{run_id}/events
POST   /v1/runs/{run_id}/cancel
GET    /v1/approvals
GET    /v1/approvals/{approval_id}
POST   /v1/approvals/{approval_id}/decisions

# Agents, reusable configuration, and knowledge
POST   /v1/agents
GET    /v1/agents
GET    /v1/agents/{agent_id}
PATCH  /v1/agents/{agent_id}
POST   /v1/agents/{agent_id}/revisions
GET    /v1/agents/{agent_id}/revisions
GET    /v1/agent-revisions/{revision_id}
POST   /v1/agent-revisions/{revision_id}/publish
POST   /v1/run-templates
GET    /v1/run-templates
POST   /v1/knowledge-bases
GET    /v1/knowledge-bases/{knowledge_base_id}
POST   /v1/knowledge-bases/{knowledge_base_id}/sources
POST   /v1/knowledge-bases/{knowledge_base_id}/ingestions
POST   /v1/knowledge-bases/{knowledge_base_id}/query

# Models and connections
POST   /v1/connections
GET    /v1/connections
GET    /v1/connections/{connection_id}
PATCH  /v1/connections/{connection_id}
DELETE /v1/connections/{connection_id}
POST   /v1/connections/{connection_id}/verify
POST   /v1/model-routes
GET    /v1/model-routes
POST   /v1/model-routes/{route_id}/revisions
POST   /v1/model-route-revisions/{revision_id}/publish
GET    /v1/models/capabilities

# Tools, skills, hooks, and environments
POST   /v1/tools
GET    /v1/tools
POST   /v1/tools/{tool_id}/revisions
POST   /v1/tool-sets
POST   /v1/tool-sets/{tool_set_id}/revisions
POST   /v1/mcp-connections
POST   /v1/mcp-connections/{connection_id}/discover
POST   /v1/skills
POST   /v1/skills/{skill_id}/revisions
POST   /v1/hooks
POST   /v1/environment-templates
POST   /v1/environment-templates/{template_id}/revisions

# Workspaces, artifacts, and repositories
GET    /v1/workspaces/{workspace_id}
POST   /v1/workspaces/{workspace_id}/commands
POST   /v1/workspaces/{workspace_id}/snapshots
GET    /v1/workspace-snapshots/{snapshot_id}
POST   /v1/artifacts
POST   /v1/artifacts/{artifact_id}/uploads
POST   /v1/artifacts/{artifact_id}/finalize
GET    /v1/artifacts/{artifact_id}
GET    /v1/artifacts/{artifact_id}/content
DELETE /v1/artifacts/{artifact_id}
POST   /v1/repository-bindings
GET    /v1/repository-bindings
POST   /v1/repository-bindings/{binding_id}/verify

# Automation and delivery
POST   /v1/triggers
GET    /v1/triggers
PATCH  /v1/triggers/{trigger_id}
POST   /v1/triggers/{trigger_id}/deliveries
GET    /v1/trigger-deliveries/{delivery_id}
POST   /v1/schedules
GET    /v1/schedules
PATCH  /v1/schedules/{schedule_id}
GET    /v1/schedules/{schedule_id}/occurrences
POST   /v1/webhook-endpoints
GET    /v1/webhook-endpoints
GET    /v1/webhook-deliveries
POST   /v1/webhook-deliveries/{delivery_id}/redeliver

# Runners and specialized workers
POST   /v1/runner-pools
GET    /v1/runner-pools
POST   /v1/runner-pools/{pool_id}/enrollment-tokens
GET    /v1/runners
GET    /v1/runners/{runner_id}
POST   /v1/runners/{runner_id}/commands
POST   /v1/capability-worker-pools
GET    /v1/capability-workers

# Governance, operations, and evaluation
POST   /v1/organizations
GET    /v1/organizations
GET    /v1/organizations/{organization_id}
PATCH  /v1/organizations/{organization_id}
POST   /v1/organizations/{organization_id}/projects
GET    /v1/organizations/{organization_id}/projects
GET    /v1/projects/{project_id}
PATCH  /v1/projects/{project_id}
POST   /v1/organizations/{organization_id}/memberships
GET    /v1/organizations/{organization_id}/memberships
PATCH  /v1/memberships/{membership_id}
DELETE /v1/memberships/{membership_id}
POST   /v1/service-accounts
GET    /v1/service-accounts
POST   /v1/service-accounts/{service_account_id}/tokens
POST   /v1/api-keys
GET    /v1/api-keys
POST   /v1/api-keys/{api_key_id}/revoke
POST   /v1/policies
GET    /v1/policies
POST   /v1/policies/{policy_id}/revisions
POST   /v1/secret-refs
GET    /v1/secret-refs
POST   /v1/secret-refs/{secret_ref_id}/versions
POST   /v1/secret-refs/{secret_ref_id}/revoke
GET    /v1/capabilities
GET    /v1/usage
GET    /v1/audit-events
POST   /v1/budgets
GET    /v1/quotas
POST   /v1/eval-suites
POST   /v1/eval-suites/{suite_id}/runs
GET    /v1/eval-runs/{eval_run_id}
POST   /v1/exports
POST   /v1/deletion-jobs
~~~

Deployment-global operations that must not be tenant-accessible—migration state, cell maintenance, release trust roots, and emergency security controls—live under /v1/admin and require a separate deployment-operator identity. Tenant governance uses the routes above; it is not mixed with deployment administration.

### 20.2.2 Endpoint conventions

- POST creates or invokes a named action and requires/accepts idempotency according to the operation schema.
- PATCH applies JSON Merge Patch to allowed mutable fields and requires If-Match for protected resources.
- DELETE normally creates a DeletionJob and returns 202; it does not synchronously claim byte erasure.
- Named action endpoints are used only when the operation is not ordinary CRUD, such as publish, verify, cancel, fork, finalize, or redeliver.
- List/get operations enforce current authorization even when an idempotent create response is replayed.
- Bulk operations are separate explicit endpoints with item-level idempotency and results; arbitrary arrays on ordinary create are not treated as transactional bulk.
- No endpoint accepts a caller-selected organization/project body field to escape the route/credential scope.

### 20.3 Canonical identifiers

- IDs are opaque, globally unique, URL-safe strings with a human-readable resource prefix such as ses_, run_, tcall_, art_, or whd_.
- The implementation uses UUIDv7-compatible time-sortable randomness or an equivalent collision-resistant scheme.
- Clients MUST NOT parse timestamps, tenancy, region, or routing information from an ID.
- An ID is never reused after deletion.
- Caller-supplied correlation IDs live in metadata or source_ref and do not replace canonical IDs.
- All timestamps are RFC 3339 UTC timestamps with microsecond precision where available.

### 20.4 Common resource fields

Every durable resource includes:

~~~json
{
  "id": "res_...",
  "object": "resource_type",
  "organization_id": "org_...",
  "project_id": "prj_...",
  "created_at": "2026-07-16T12:00:00.000000Z",
  "updated_at": "2026-07-16T12:00:00.000000Z",
  "revision": 3,
  "metadata": {},
  "labels": {}
}
~~~

Rules:

- metadata is caller-controlled JSON with documented size and key limits; it is not sent to a model unless explicitly mapped into context;
- labels are indexed strings intended for policy, filtering, cost allocation, and placement;
- server-controlled fields cannot be shadowed by metadata;
- secret values and unrestricted personal data MUST NOT be placed in metadata or labels;
- mutable resources expose an ETag derived from revision.

### 20.5 Authentication and request context

Supported public authentication mechanisms are:

- short-lived user access tokens issued through OIDC;
- project-scoped API keys for server applications;
- service-account tokens with explicit roles and expiry;
- workload credentials for trusted runner and capability-worker connections.

Each request resolves exactly one principal, organization, project, policy revision, and request ID. Project selection comes from a path or credential scope, not an untrusted body field. SaaS browser clients MUST call the API through a backend or use short-lived, narrowly scoped tokens; long-lived project keys MUST NOT be embedded in mobile or browser code.

Every response includes:

~~~text
Request-Id: req_...
API-Version: 2026-07-16
RateLimit-Limit: ...
RateLimit-Remaining: ...
RateLimit-Reset: ...
~~~

The platform accepts W3C trace context but generates its own request ID. Untrusted callers cannot choose internal audit identity or tenant context through tracing headers.

### 20.6 API versioning

- /v1 is the major compatibility boundary.
- API-Version selects a dated schema revision within the major version. Official SDKs send the revision they were generated against.
- Omitting API-Version selects the deployment default and returns that selection in the response.
- Additive optional fields, enum values, and event types may appear within v1. Clients MUST ignore unknown object fields and preserve unknown open-enum values.
- Removing or changing stable behavior requires /v2.
- Webhook endpoints pin their own API revision so a server upgrade cannot silently change payloads.
- Engine-process protocol, checkpoint formats, plugin manifests, and SDK packages are versioned independently.
- Deprecation uses the standardized [Deprecation header](https://www.rfc-editor.org/rfc/rfc9745.html), a documentation Link, and when applicable the [Sunset header](https://www.rfc-editor.org/info/rfc8594/).
- Stable behavior receives at least twelve months between deprecation announcement and managed-service removal. A critical security issue may use an accelerated path with a written advisory and migration.

### 20.7 Pagination, filtering, and consistency

- Collection endpoints use cursor pagination with limit, after, and before; offset pagination is not exposed.
- Default limit is 50 and maximum is 200 unless an endpoint documents a smaller bound.
- Sort order is explicit and stable, normally created_at plus id.
- Filters use documented fields; arbitrary SQL-like expressions are forbidden.
- A page response includes data, has_more, next_cursor, and optional previous_cursor.
- Resource reads after a successful write are strongly consistent within the home region.
- Search, analytics, and aggregated usage MAY be eventually consistent and MUST report their freshness timestamp.
- List pagination is snapshot-consistent when an as_of cursor is supplied; otherwise new writes may appear on later pages but a resource MUST NOT repeat.

### 20.8 Optimistic concurrency

PATCH, destructive commands, profile publication, policy changes, and secret rotation accept If-Match. A stale revision returns 412 revision_conflict with the current ETag. Last-write-wins is forbidden for security policy, agent publication, schedules, model routes, and webhook endpoint changes.

### 20.9 Idempotency

The design follows the fault-tolerance semantics described by the work-in-progress [IETF Idempotency-Key draft](https://datatracker.ietf.org/doc/draft-ietf-httpapi-idempotency-key-header/) while treating it as guidance rather than a published standard.

All externally visible mutations MUST accept Idempotency-Key. SDKs generate one automatically unless the caller opts out.

Server behavior:

1. Scope the key to principal, project, HTTP method, and normalized route.
2. Hash the canonical semantic request after server defaults are resolved but before transient fields are added.
3. Atomically reserve the key before any side effect.
4. Reusing the key with the same request returns the original status, selected headers, body, and resource pointer.
5. Reusing it with a different semantic request returns 409 idempotency_mismatch.
6. A concurrent duplicate returns the completed response or 409 idempotency_in_progress with Retry-After.
7. Validation failures that execute no operation are not cached. Once execution begins, success and failure are cached.

API idempotency records remain for at least the greater of seven days or maximum run duration plus twenty-four hours. Source-event deduplication and tool-call records use their own longer retention. A server MUST publish configured retention through discovery.

Idempotency retention does not extend customer-content retention:

- while the result content exists, a duplicate receives the original response;
- after store:false or deletion purges content, the record keeps only request hash, operation/resource tombstone, outcome fingerprint, and purge time;
- a duplicate then returns 410 idempotency_result_expired with the original operation identity/tombstone and MUST NOT execute again;
- retrieving an idempotent response always re-checks current authorization;
- secrets are never stored in the idempotency response cache.

Idempotency prevents duplicate platform operations; it cannot manufacture exactly-once behavior in an external system. External side effects require the same stable operation key to reach a destination that supports idempotency, or they require reconciliation and human-visible uncertain status.

### 20.10 Errors

Non-stream failures use application/problem+json following [RFC 9457](https://www.rfc-editor.org/rfc/rfc9457.html):

~~~json
{
  "type": "https://docs.example.invalid/problems/capability-denied",
  "title": "Capability denied",
  "status": 403,
  "detail": "Repository publication is not permitted by project policy.",
  "instance": "/v1/runs/run_...",
  "code": "capability_denied",
  "request_id": "req_...",
  "retryable": false,
  "field_errors": [],
  "context": {
    "policy_decision_id": "pdec_..."
  }
}
~~~

Stable error families include:

| HTTP | Stable codes |
|---|---|
| 400 | invalid_request, invalid_state, unsupported_content, missing_idempotency_key |
| 401 | authentication_required, invalid_token, expired_token |
| 403 | permission_denied, capability_denied, policy_denied, region_denied |
| 404 | not_found |
| 409 | revision_conflict, idempotency_mismatch, idempotency_in_progress, active_run_conflict, lease_conflict |
| 410 | gone, idempotency_result_expired, retention_expired |
| 412 | precondition_failed |
| 413 | payload_too_large, context_too_large |
| 422 | schema_validation_failed, unsupported_model_capability |
| 429 | rate_limited, quota_exceeded, concurrency_exceeded |
| 500 | internal_error |
| 502 | provider_error, tool_transport_error, runner_error |
| 503 | capacity_unavailable, dependency_unavailable, maintenance |
| 504 | operation_timed_out |

Provider messages, stack traces, host paths, credentials, and internal policy data MUST NOT leak into public errors. A sanitized upstream code and request ID MAY be included.

### 20.11 Asynchronous operation pattern

Any operation that can exceed the HTTP request deadline returns 202 with:

- the accepted resource;
- Location pointing to its canonical status resource;
- Retry-After when polling is appropriate;
- links for events and cancellation.

Disconnecting an HTTP request or SSE stream does not cancel work. Cancellation is an explicit idempotent command.

### 20.12 Rate limiting and admission control

Rate limits are separate from durable budgets:

- request-rate limit;
- concurrent root runs;
- concurrent child runs;
- queued-run count;
- model token/cost rate;
- sandbox CPU/memory capacity;
- tool/provider-specific limits.

Admission evaluates authentication, policy, quota, budget reservation, region, model capability, runner capacity, and queue bounds before provisioning. A run may be queued when capacity is temporarily unavailable. If queue deadline expires, it terminates as timed_out without starting billable compute.

### 20.13 OpenAPI and AsyncAPI artifacts

- OpenAPI 3.2.0 is the primary HTTP description and uses JSON Schema 2020-12-compatible schemas.
- A mechanically verified OpenAPI 3.1.2 projection MAY also ship for generators that have not implemented 3.2. It cannot omit or change platform semantics; unsupported constructs are supplied through generated types and conformance fixtures.
- AsyncAPI 3.1.0 describes SSE event channels, webhooks, and optional queue adapters.
- Schemas are generated from one checked-in source model and are themselves checked into releases.
- Every pull request runs backward-compatibility diffing, examples validation, SDK generation checks, and conformance tests.
- Handwritten documentation may clarify behavior but cannot contradict the schemas.

## 21. Events, streams, and webhooks

### 21.1 Event journal guarantees

The session event journal is the canonical replayable history of observable execution, not a debug log.

- The append of a state transition and its public event occurs in one database transaction.
- sequence is strictly increasing per session.
- An event ID is globally unique.
- Delivery is at least once; journal insertion is exactly once.
- Event payloads are immutable.
- Corrections are new events, never in-place edits.
- Delta events may be compacted after their retention window only after a canonical completed item has been persisted.
- Audit records and session events are distinct streams with controlled cross-links.

Events are ordered within one session. No total ordering is promised across sessions. Child-run events carry both child and parent linkage; their interleaving is determined by the parent session sequence assigned at ingestion.

### 21.2 SSE contract

SSE follows the [WHATWG event stream format](https://html.spec.whatwg.org/dev/server-sent-events.html):

~~~text
id: evt_01...
event: response.output_text.delta.v1
data: {"sequence":42,"run_id":"run_...","delta":"hello"}

~~~

Rules:

- clients reconnect with Last-Event-ID or after_sequence;
- the server sends a heartbeat comment at least every fifteen seconds while idle;
- an expired cursor returns 409 event_cursor_expired with the earliest available sequence and a link to the canonical snapshot;
- slow consumers are disconnected after bounded buffering and can resume;
- terminal events contain the final canonical status;
- SDKs deduplicate event IDs and reconnect with bounded exponential backoff;
- clients MUST treat deltas as provisional until the corresponding item-completed event.

### 21.3 Event taxonomy

Stable event names use resource.action.phase.vN:

- session.created.v1, session.config_changed.v1, session.forked.v1;
- message.accepted.v1, message.queued.v1, message.delivered.v1;
- run.queued.v1, run.started.v1, run.waiting.v1, run.completed.v1, run.failed.v1, run.canceled.v1;
- attempt.started.v1, attempt.recovering.v1, attempt.ended.v1;
- model_step.started.v1, model_step.output_delta.v1, model_step.completed.v1;
- tool_call.requested.v1, tool_call.approval_required.v1, tool_call.started.v1, tool_call.completed.v1, tool_call.uncertain.v1;
- child_run.created.v1, child_run.completed.v1;
- checkpoint.created.v1, checkpoint.restored.v1, checkpoint.rejected.v1;
- workspace.provisioned.v1, workspace.snapshot_created.v1, workspace.recovered.v1;
- artifact.created.v1, artifact.finalized.v1;
- usage.updated.v1, budget.warning.v1, budget.exceeded.v1;
- policy.denied.v1, warning.created.v1.

New event types are additive. Consumers subscribe by exact name or documented prefix and MUST tolerate unknown events.

### 21.4 Outbound webhook endpoints

Each endpoint stores:

- URL and enabled state;
- subscribed event filters;
- pinned API revision;
- one or two active signing secrets during rotation;
- delivery timeout and retry policy within platform bounds;
- optional fixed headers whose values are secret references;
- data-region restrictions.

Private or loopback destinations are denied by default. Self-host administrators MAY allow private ranges through an explicit egress policy.

### 21.5 Webhook signing

Each attempt includes:

~~~text
Webhook-Id: whd_...
Webhook-Timestamp: 1784203200
Webhook-Signature: v1=<hex-hmac>
Webhook-Attempt: 3
~~~

The signed input is version, delivery ID, timestamp, and the exact raw body. HMAC-SHA-256 is the baseline; asymmetric signing MAY be added without removing HMAC. Receivers MUST verify the raw body, use constant-time comparison, enforce a configurable timestamp tolerance with a five-minute default, and deduplicate Webhook-Id. Secret rotation overlaps old and new signatures for a bounded period.

The design follows the validation, fast acknowledgement, unique delivery ID, and replay protections documented in [GitHub webhook best practices](https://docs.github.com/en/webhooks/using-webhooks/best-practices-for-using-webhooks).

### 21.6 Webhook delivery

- The receiver has ten seconds to return any 2xx response.
- Redirects are not followed.
- DNS is re-resolved through the egress policy for every attempt to prevent rebinding.
- Retry uses jittered exponential backoff for network errors, 408, 409 when documented retryable, 425, 429, and 5xx.
- 2xx completes delivery; most other 4xx are terminal.
- Default retry window is seventy-two hours with a maximum of twenty attempts.
- Every attempt is visible with sanitized response status, duration, and body excerpt.
- Operators and authorized users can request an idempotent redelivery with the same delivery ID and payload.
- Dead deliveries enter a dead-letter view and can produce an alert.

Webhook delivery never blocks run completion. A trigger may define workflow success as requiring acknowledged delivery, but that is a separate callback step with its own terminal state.

### 21.7 Inbound events

Inbound webhook and queue adapters normalize source events into:

~~~json
{
  "source": "slack",
  "source_tenant": "T123",
  "source_event_id": "Ev123",
  "type": "message.created",
  "occurred_at": "2026-07-16T12:00:00Z",
  "subject": "thread:...",
  "data": {},
  "verified_identity": {}
}
~~~

The adapter verifies source authentication before persistence, records the raw payload in encrypted short-retention storage when policy permits, and atomically deduplicates source plus source tenant plus source event ID. It acknowledges the source before starting agent work.

## 22. Detailed execution resources

### 22.1 Session object

A Session contains durable conversation identity and coordination state:

~~~json
{
  "id": "ses_...",
  "status": "active",
  "mode": "interactive",
  "agent_revision_id": null,
  "config_revision_id": "cfgrev_...",
  "workspace_binding_id": "wbind_...",
  "active_root_run_id": null,
  "next_sequence": 185,
  "retention_policy_id": "ret_...",
  "created_by": {"type": "user", "id": "usr_..."}
}
~~~

Session states are active, paused, closing, closed, and deleted. A terminal run does not close its session. Closing rejects new messages and waits or cancels active work according to the command. Deletion is asynchronous and follows retention/legal-hold policy.

### 22.2 Messages and content items

Messages have role, ordered content items, author identity, source reference, visibility, delivery mode, and immutable creation time.

Roles are user, assistant, tool, system_notice, and external_actor. Caller-created system instructions are not represented by a privileged role; they enter through an authorized config layer.

Content item types include:

- input_text and output_text;
- image_ref, audio_ref, file_ref, and artifact_ref;
- structured_json with schema identity;
- tool_request and tool_result;
- refusal, warning, and citation;
- compacted_context;
- redacted_content marker.

Binary data is uploaded as an artifact and referenced by ID. Inline base64 is allowed only below a small documented request limit. Every external URL is fetched by a controlled ingestion service, not by the API process.

### 22.3 Run and attempt

A Run is the caller-visible logical execution. An Attempt is one allocation of the run to an engine process and sandbox.

~~~text
Run:      queued → provisioning → running ↔ waiting
                    └───────────────┬──────────────┘
                                    └→ completed | failed | canceled | timed_out | budget_exceeded

Attempt:  assigned → starting → active → draining → succeeded | failed | lost | preempted
~~~

Rules:

- run_id is stable through retries and host movement;
- attempt_id changes after loss, incompatible checkpoint, or explicit re-execution;
- an attempt owns a fencing token; stale attempts cannot append authoritative state or complete tool calls;
- one active root attempt exists per run;
- provider and tool sub-attempts have their own counters and IDs;
- run terminality is monotonic;
- recovery does not erase failed attempt evidence.

### 22.4 Commands

Commands are durable resources with command_id, target, requested_by, reason, expected revision, state, and result. Stable command kinds:

- send_message with queue, steer, or interrupt delivery;
- pause and resume;
- cancel;
- change_config;
- approve or deny;
- retry_from_failure;
- fork_session;
- request_checkpoint;
- close_session.

Command acceptance means the command is durably queued, not necessarily applied. applied_sequence identifies the event boundary where it took effect. Duplicate command IDs return the original result.

### 22.5 Approvals

An approval request contains:

- exact tool and version;
- normalized arguments with secrets redacted;
- predicted capabilities and side-effect classification;
- repository diff, command, destination, or external object when applicable;
- requesting run/model step;
- policy reason;
- expiry and allowed approver roles;
- one-shot approval token bound to the exact request hash.

Approval decisions are approve_once, approve_for_run under an allowed argument constraint, deny, or expire. Broad approval cannot override organization policy. Editing arguments creates a new tool call and approval request. A model-generated prose summary is supplementary and never replaces the exact operation display.

### 22.6 Artifacts

Artifacts are immutable versioned outputs with:

- media type, byte size, checksum, and storage location;
- creator run/tool/user;
- logical type such as report, patch, diff, log, archive, image, test result, or structured output;
- security classification and malware-scan status;
- provenance links to inputs, tool calls, model steps, and configuration;
- retention and legal-hold status.

Upload is create → signed multipart upload → finalize. Finalize verifies size and checksum atomically. Download uses short-lived scoped URLs or authenticated streaming. Mutable “latest” names point to immutable versions.

### 22.7 Structured output

Structured output declares JSON Schema 2020-12, a schema name/version, strictness, and validation policy.

- strict output that fails validation triggers bounded repair using the same model route or a declared repair route;
- each repair is a visible model step and consumes budget;
- exhaustion terminates with schema_validation_failed while retaining the invalid output as a restricted diagnostic artifact;
- host-language SDK types do not replace server validation;
- schemas cannot request executable code, remote references, or unbounded recursive expansion.

### 22.8 Session fork

Forking creates a new session with:

- a parent session and fork sequence;
- copied immutable messages/config references up to that sequence;
- a new event journal;
- either a copy-on-write workspace snapshot or no workspace;
- no inherited pending approval or active tool lease;
- independent future retention and model choices.

Forking is the supported way to run competing continuations. Parallel root runs against one mutable workspace are forbidden.

### 22.9 Discovery

GET /v1/capabilities returns:

- API revisions;
- enabled public surfaces and maturity;
- configured content and artifact limits;
- supported sandbox isolation tiers;
- available model-route aliases and declared capabilities;
- installed tool/skill types without exposing secrets;
- retention/idempotency bounds;
- streaming transports;
- enabled integration adapters;
- deployment mode and region.

Discovery is filtered by caller permissions and does not reveal organization-wide inventory to project-scoped callers.

## 23. Official SDK and CLI contract

### 23.1 SDK design

TypeScript, Python, and Go are equally supported official SDKs at stable release. No language is a compatibility afterthought.

Each SDK has two layers:

1. a generated, checked-in transport layer derived from OpenAPI and event schemas;
2. a small handwritten ergonomic layer for streaming, polling, tool callbacks, file transfer, webhook verification, and local development.

Code generation is a build-time technique, not a runtime dependency on a commercial generator. Generated diffs are reviewed and released from the main repository. This combines the consistent types and forward-compatible enums promoted by modern SDK generators with a deliberately human-designed API surface.

### 23.2 Client construction

All SDKs accept the same logical configuration:

~~~json
{
  "base_url": "http://127.0.0.1:8080",
  "api_key": "secret reference or value",
  "project": "prj_...",
  "api_version": "2026-07-16",
  "timeout": {
    "connect_seconds": 10,
    "request_seconds": 60
  },
  "max_retries": 2,
  "user_agent_suffix": "my-product/1.4"
}
~~~

Precedence is explicit constructor argument → documented environment variable → local profile file. SDKs never discover credentials from unrelated provider environment variables. Configuration files contain secret references or OS-keychain handles by default, not plaintext keys.

### 23.3 Common object model

All languages expose:

- typed request builders and response objects;
- open enums that preserve unknown server values;
- discriminated content-item and event unions;
- pagination iterators plus access to raw pages;
- request ID, response headers, status, and raw body for diagnostics;
- per-request timeout, idempotency key, retry, and extra-header overrides;
- safe escape hatches for additive fields without accepting arbitrary replacement URLs;
- explicit nullable versus omitted values.

The SDKs do not hide resource IDs, state transitions, usage, actual model route, warnings, or fallback attempts.

### 23.4 Core ergonomics

The same conceptual calls exist in every language:

~~~text
client.responses.create(...)
client.responses.stream(...)
client.responses.retrieve(...)
client.responses.cancel(...)

client.sessions.create(...)
client.sessions.messages.create(...)
client.sessions.runs.create(...)
client.sessions.events.stream(...)
client.sessions.commands.send(...)

client.agents.create(...)
client.agents.revisions.publish(...)
client.triggers.create(...)
client.schedules.create(...)

client.artifacts.upload(...)
client.artifacts.download(...)
client.webhooks.verify(...)
~~~

Convenience helpers compose canonical calls; they do not use private endpoints.

### 23.5 Response helpers

High-level helpers include:

- create_and_wait with configurable terminal conditions and deadline;
- stream returning both typed events and a final accumulated Response;
- output_text that fails if non-text output makes flattening ambiguous;
- output_as(schema/type) with server and client validation;
- with_session for continued conversations;
- upload-aware input helpers for files and images.

The raw response object remains available so convenience does not erase tool calls, citations, partial output, warnings, or usage.

### 23.6 Streaming

- TypeScript uses AsyncIterable.
- Python provides both synchronous and asynchronous context managers/iterators.
- Go uses an iterator-style Stream with Next, Event, Err, Close and accepts context.Context.

The stream helper remembers the last confirmed event ID, reconnects only while the caller deadline permits, deduplicates events, and returns event_cursor_expired distinctly. Closing the local iterator closes transport only; cancel requires an explicit API call.

### 23.7 Retries and timeouts

The default retry policy is inspired by mature official SDK behavior such as [openai-node](https://github.com/openai/openai-node) and [stripe-go](https://github.com/stripe/stripe-go):

- retry connection failures, 408, 409 idempotency_in_progress, 425, 429, and retryable 5xx;
- honor Retry-After and rate-limit reset headers;
- use exponential backoff with full jitter and a total deadline;
- retry mutation only when an idempotency key is present;
- never retry a streamed request after an uncommitted caller-visible side effect without reconnecting by event ID;
- expose retry count and final request ID.

Connect, ordinary request, stream idle, and total operation timeouts are separate. Background jobs are not constrained by the initiating HTTP timeout.

### 23.8 SDK idempotency behavior

Official SDKs generate cryptographically random idempotency keys for mutation helpers and reuse the same key across transport retries. A caller can supply a business key for cross-process replay. The SDK must never generate a new key during an automatic retry.

### 23.9 Client-executed tools

An application may register a client tool handler against a live run:

1. the server emits a signed client-tool request;
2. the SDK verifies session, run, tool version, argument schema, and request expiry;
3. the application handler executes;
4. the SDK submits the result with the original tool_call_id and idempotency key;
5. a duplicate request returns the stored result rather than running the handler again when the local durable handler store is enabled.

Client tools are unsuitable for unattended background runs unless a durable client worker is registered. SDK docs MUST warn that an in-memory handler cannot survive process failure.

### 23.10 Webhook helper

Every SDK includes:

- verify_and_parse(raw_body, headers, secret);
- constant-time signature comparison;
- timestamp-window validation;
- support for overlapping rotation secrets;
- typed event decoding by pinned webhook API version;
- a framework-neutral result plus small adapters for common HTTP frameworks.

Verification requires raw request bytes. Helpers reject already-parsed and reserialized JSON unless the framework can prove byte preservation.

### 23.11 Language-specific quality bar

| Language | Stable-release requirement |
|---|---|
| TypeScript | ESM and CommonJS consumption, modern Node LTS, browser-safe types, no server credential use in browser bundles, AbortSignal |
| Python | Python 3.10+, sync and async clients, Pydantic-free public dependency surface where practical, pathlib/file-like uploads, typed overloads |
| Go | supported Go release policy, context on every network operation, io.Reader/io.Writer streaming, errors.Is/As-compatible typed errors |

All three publish provenance, checksums, changelogs, migration notes, and API-revision compatibility. SDK release versions need not equal server versions.

### 23.12 CLI

The CLI is a first-class client of the public API:

~~~text
platform init
platform local up|down|status|doctor|logs
platform auth login|status
platform project use
platform response create
platform session create|attach|send|cancel|fork
platform run get|events|cancel
platform agent validate|publish
platform tool validate
platform runner enroll|status
platform webhook listen
platform eval run
platform config export|validate
~~~

The eventual branded executable replaces platform; this placeholder does not enter wire schemas.

CLI requirements:

- machine-readable JSON output on every command;
- stable exit codes;
- no secret values in history, process arguments, or default logs;
- interactive session attachment with reconnect;
- a local webhook tunnel is optional and clearly marked development-only;
- doctor checks API, database migration state, object storage, runner capacity, image pull, provider connectivity, clock skew, and callback reachability.

### 23.13 Local/cloud parity

The following program must work unchanged after replacing only base URL and credentials:

~~~ts
const session = await client.sessions.create({ model: "primary" });
await client.sessions.messages.create(session.id, {
  content: "Inspect the repository and fix the failing test."
});
for await (const event of client.sessions.events.stream(session.id)) {
  render(event);
}
~~~

No cloud-only SDK namespace exists. Cloud-only commercial features appear as ordinary governed resources and capability flags.

### 23.14 Compatibility tests

Each SDK release runs against:

- the current server;
- every still-supported dated API revision;
- the local reference distribution;
- a fault proxy that injects disconnects, duplicate events, delayed responses, 429, and 5xx;
- generated unknown fields and enum values;
- large artifact streams;
- webhook signature vectors shared across languages.

## 24. Standalone system architecture

### 24.1 Architectural shape

The reference product starts as a modular control plane plus independently scalable execution hosts, not as dozens of mandatory microservices.

~~~text
Clients / SDKs / CLI / integrations
                  |
          Public API + SSE
                  |
  +---------------+----------------+
  | Standalone control plane       |
  |                                |
  | API and identity               |
  | session/run state machine      |
  | built-in durable coordinator   |
  | scheduler and trigger ingress  |
  | model router and broker        |
  | tool/capability broker         |
  | policy and approval service    |
  | artifact and secret brokers    |
  | usage ledger and audit         |
  +---------------+----------------+
          |                |
     PostgreSQL       object storage
          |
     outbound runner connections
          |
  +-------+------------------------+
  | Runner daemon on execution host|
  | sandbox driver                 |
  | workspace manager              |
  | engine supervisor              |
  +-------+------------------------+
          |
   isolated engine OCI container
~~~

Modules have explicit interfaces and queues so managed-service deployments can split them later. A self-host installation MUST NOT require that split.

### 24.2 Mandatory infrastructure

The complete baseline installation requires only:

- the control-plane distribution;
- PostgreSQL;
- S3-compatible object storage, with filesystem storage allowed for single-node development;
- one enrolled execution host with a supported local sandbox driver;
- at least one configured model connection.

Redis, Kafka, NATS, Kubernetes, Temporal, service mesh, external policy engine, and external secret manager are not mandatory. Adapters MAY use them without changing public semantics.

### 24.3 System of record

PostgreSQL is authoritative for:

- organizations, projects, identities, policies, and configuration revisions;
- sessions, messages, runs, attempts, tool calls, approvals, and commands;
- event journal metadata;
- durable jobs, timers, leases, deduplication, and transactional outbox;
- artifact metadata and checkpoint indexes;
- immutable usage ledger and audit index;
- migration state.

Object storage is authoritative for:

- artifact bytes;
- workspace snapshots;
- engine checkpoints;
- large raw integration payloads under retention policy;
- exported audit/evaluation datasets;
- release and plugin objects when an installation chooses to mirror them.

Search indexes, caches, analytics stores, and stream brokers are derived and rebuildable.

### 24.4 Built-in durable coordinator

The platform owns a deliberately narrow durable job engine sufficient for its own lifecycle:

- transactional insertion of state transition plus outbox event;
- ready-at timers stored in PostgreSQL;
- bounded priority queues;
- worker claims using leases and fencing tokens;
- heartbeat and lease expiry;
- retry with persisted attempt count and next-ready timestamp;
- cancellation and pause flags;
- workflow steps encoded as product state-machine transitions, not user-authored arbitrary code;
- dead-letter state with operator retry/reconcile actions.

The coordinator does not attempt to become a general workflow product. External workflow systems can start runs, wait through webhooks/SSE/polling, send commands, and correlate their own workflow IDs. An optional orchestration adapter MAY map native signals and timers, but core correctness never relies on its private history.

### 24.5 Transactional outbox and inbox

Any database change that requires asynchronous work writes an outbox row in the same transaction. Dispatchers claim rows with a lease, deliver at least once, and mark completion. Consumers use a stable operation ID and inbox record before applying a side effect.

This pattern covers:

- public event fanout;
- runner assignments;
- model/tool work;
- webhook delivery;
- usage aggregation;
- artifact scanning;
- search indexing.

The event journal remains caller-visible history; the outbox is an internal delivery mechanism and may be compacted independently.

### 24.6 Horizontal scaling

- API instances are stateless apart from bounded connection buffers.
- Coordinator workers share PostgreSQL leases.
- A session has a short-lived logical owner for command serialization; ownership is fenced and movable.
- SSE may reconnect to any instance and replay from the journal.
- Runner assignments survive control-plane instance loss.
- Object uploads use direct signed transfer where safe.
- Managed deployments MAY introduce a stream broker for fanout, but journal replay remains authoritative.

### 24.7 Control-plane failure boundaries

No model call, tool callback, runner message, or webhook is considered committed until its corresponding state is persisted. If the control plane fails:

- an accepted API mutation remains discoverable by idempotency key;
- a claimed job becomes eligible after lease expiry;
- a runner buffers only a bounded event window and reconnects;
- a completed external operation is reconciled by operation ID before replay;
- an SSE client resumes by event ID;
- the workspace is not destroyed until a durable terminal or recovery decision.

### 24.8 Runner connectivity

The runner daemon initiates an outbound mutually authenticated TLS connection to the control plane. This allows a runner on a private Linux VM or customer network without a public inbound port.

The runner protocol carries:

- enrollment and short-lived workload identity renewal;
- capabilities, labels, capacity, health, and supported sandbox drivers;
- lease offer, accept, renew, complete, and revoke;
- sandbox lifecycle commands;
- ordered engine protocol frames;
- artifact/checkpoint transfer grants;
- bounded logs and metrics.

WebSocket over TLS is the baseline firewall-friendly transport; HTTP long polling is a compatibility fallback. Managed deployments MAY add an HTTP/2 binary transport. All transports share the same versioned message schema and conformance tests.

### 24.9 Runner enrollment

1. An administrator creates a single-use enrollment token scoped to organization, project or pool, labels, expiry, and maximum hosts.
2. The daemon presents the token over TLS and generates a local key pair.
3. The control plane attests the request as far as the configured deployment supports, consumes the token, and issues a short-lived runner certificate.
4. Renewal uses the runner identity, not the enrollment token.
5. Revocation stops new leases immediately and terminates or drains existing leases according to policy.

Enrollment secrets are never reusable bootstrap API keys.

### 24.10 Scheduling and placement

Placement filters in this order:

1. tenant and data-region boundary;
2. required operating system, architecture, runtime, device, and sandbox isolation;
3. allowed runner pool and trust class;
4. environment image availability and signature;
5. resource minimums and accelerator needs;
6. network and repository reachability;
7. affinity to a warm workspace or recoverable snapshot;
8. capacity, queue age, priority, and cost.

A caller requests capabilities, not a hostname. Hard constraints never silently relax. Soft preferences and the selected placement reason are visible.

### 24.11 Optional external adapters

Adapters can integrate:

- durable workflow systems;
- Kafka, NATS, SQS, Pub/Sub, RabbitMQ, or Redis-based queues;
- Kubernetes or cloud batch schedulers;
- external identity/policy/secret systems;
- third-party model gateways;
- observability and billing systems.

Every adapter translates to canonical resources and IDs. Disabling an adapter cannot make retained sessions unreadable or destroy canonical event history.

## 25. Provider-neutral agent kernel

### 25.1 Kernel responsibility

The product ships a real, usable reference kernel for coding, research, and general tool-using work. It is not merely a container scheduler.

The kernel owns:

- the model/tool reasoning loop;
- context assembly and compaction;
- interpretation of model tool requests;
- subagent delegation decisions;
- progress and final-output production;
- an opaque portable checkpoint;
- deterministic reaction to supervisor commands at safe boundaries.

The kernel does not own:

- organizations, users, billing, schedules, or durable session state;
- hosts, slots, container placement, or global queues;
- long-lived model/tool credentials;
- repository publication authority;
- external secret storage;
- the canonical event journal;
- provider routing policy enforcement.

### 25.2 Pluggability without fragmentation

The platform defines a language-neutral engine protocol and ships one reference engine OCI image. Alternative engines MAY implement the protocol. A run chooses exactly one engine image and protocol version.

Pluggability MUST NOT create competing public session models:

- all engines receive the same canonical inputs;
- all emit the same event classes;
- model and external tool access remain brokered;
- checkpoints declare compatibility explicitly;
- conformance tests cover cancel, queued message, tool replay, checkpoint, and terminal behavior.

An engine-specific extension lives under a namespaced config object and cannot change core event or security semantics.

### 25.3 OCI packaging

Every engine execution pins:

- registry repository;
- immutable manifest digest;
- platform architecture;
- engine semantic version;
- protocol version range;
- checkpoint format ID/version;
- declared capabilities;
- signed provenance identity.

Mutable tags are permitted only for development resolution. The resolved digest is stored in ConfigSnapshot before admission. Production policy MAY require an allowlisted signature, SBOM, and provenance.

### 25.4 Supervisor boundary

The runner starts the engine as an unprivileged process in the sandbox and supervises:

- protocol handshake and version negotiation;
- CPU, memory, process, disk, and wall-time limits;
- stdin/stdout protocol transport;
- stderr log capture with redaction and bounds;
- liveness and progress deadlines;
- cancel, grace period, and forced termination;
- checkpoint requests and artifact upload;
- final exit classification.

The engine cannot talk directly to the control-plane database, container runtime socket, runner credentials, or host filesystem.

### 25.5 Versioned JSONL engine protocol

The baseline engine protocol uses UTF-8 JSON Lines over stdin/stdout. It is easy to implement in any language and observable without an SDK.

Each frame is one line:

~~~json
{
  "protocol": "engine.v1",
  "id": "frm_...",
  "type": "run.start",
  "run_id": "run_...",
  "attempt_id": "att_...",
  "sequence": 1,
  "reply_to": null,
  "time": "2026-07-16T12:00:00Z",
  "data": {}
}
~~~

Rules:

- controller and engine maintain independent monotonic outbound sequence numbers;
- id is unique and makes frame delivery idempotent;
- reply_to correlates request/result;
- unknown additive fields are ignored;
- unsupported frame types produce protocol.error without crashing;
- stdout contains protocol frames only; human logs go to stderr;
- maximum line size is one MiB by default; larger content uses artifact references;
- writes are flushed per frame;
- invalid JSON, duplicate sequence with different content, or run identity mismatch is a protocol violation;
- secrets are forbidden in protocol frames except opaque single-use capability handles explicitly typed as secret handles.

### 25.6 Handshake

The supervisor sends supervisor.hello with supported protocol versions, run identity, fencing token hash, limits, and feature flags. The engine responds engine.ready with:

- selected protocol;
- engine name and version;
- checkpoint formats accepted;
- supported input and output content types;
- supported commands;
- maximum frame size;
- engine instance nonce.

Failure to negotiate before the startup deadline ends the attempt as incompatible_engine. No run input is sent before a successful handshake.

### 25.7 Controller-to-engine frames

Stable input frames:

| Type | Purpose |
|---|---|
| run.start | effective redacted config, initial context references, budgets, and workspace descriptor |
| run.restore | compatible checkpoint reference and replay boundary |
| message.deliver | queued or steering message with delivery semantics |
| config.change | new effective model/tool/context revision at a safe boundary |
| model.result / model.delta | brokered provider response |
| tool.result | normalized tool outcome keyed by tool_call_id |
| approval.result | approval decision |
| child.result | terminal result from a child run |
| checkpoint.request | request portable state at a declared boundary |
| run.pause | reach a checkpointable paused state |
| run.cancel | cooperative cancellation with reason and deadline |
| protocol.ack | highest contiguous frame sequence durably processed |

### 25.8 Engine-to-controller frames

Stable output frames:

| Type | Purpose |
|---|---|
| engine.ready / engine.heartbeat | negotiation and liveness |
| progress | concise user-visible progress |
| output.delta / output.item | streaming and canonical output |
| model.request | brokered model call with route alias and capability requirements |
| tool.request | tool invocation with stable tool_call_id |
| child.request | guarded subagent creation |
| approval.request | explicit approval need if not already inferred by policy |
| checkpoint.offer | opaque checkpoint bytes or artifact reference plus compatibility metadata |
| context.compacted | compaction summary and lineage |
| warning / protocol.error | typed diagnostic |
| run.waiting | waiting reason and resumability |
| run.terminal | completed, failed, canceled, timed_out, or budget_exceeded |
| protocol.ack | highest contiguous input sequence durably processed |

The supervisor, not the engine, assigns canonical public event sequence numbers.

### 25.9 Model and tool correlation

- model_request_id and tool_call_id are stable logical IDs generated before dispatch;
- an engine retransmission with the same ID and request hash receives the stored result;
- reuse with a different hash is a protocol violation;
- partial provider deltas are not treated as a completed model result;
- the final result records provider attempt IDs and usage;
- tool requests declare side-effect and replay expectations, but policy independently classifies them.

### 25.10 Run loop

The reference kernel follows an explicit step loop:

1. apply pending safe-boundary commands;
2. assemble context under the current config revision;
3. request a model response through the broker;
4. validate output and tool requests;
5. execute permitted tool calls, request approval, or create bounded child runs;
6. append canonical results to context;
7. checkpoint at configured boundaries;
8. repeat until final output, wait state, cancellation, or budget limit.

The model never controls lifecycle state directly. It proposes tool/delegation actions; deterministic code and policy decide whether they execute.

### 25.11 Safe boundaries

A safe boundary exists:

- before a model request;
- after a provider response has been finalized;
- before dispatching a tool side effect;
- after a tool result is durably recorded;
- before and after child-run creation;
- after a portable checkpoint.

Model/tool configuration changes, steering messages, cooperative pause, and normal cancellation apply at these boundaries. Immediate cancellation may abort an in-flight provider request or cancelable tool, but its outcome is recorded as partial or uncertain.

### 25.12 Context layers

Context is assembled deterministically in this precedence:

1. kernel safety and protocol instructions;
2. deployment/organization/project policy-visible instructions;
3. pinned agent revision instructions;
4. session config instructions;
5. run-specific instructions;
6. selected durable conversation items;
7. retrieved memory/context items;
8. current workspace/repository summary;
9. tool definitions and capability notices;
10. current user/trigger input.

Higher-trust instructions are delimited and provenance-tagged. Retrieved web pages, repository text, tool output, issue text, and user attachments are untrusted data even when they contain instruction-like language.

### 25.13 Context budget

The context planner reserves space for:

- provider response;
- tool definitions;
- mandatory safety/config instructions;
- recent conversation;
- expected tool results.

Remaining capacity is allocated by explicit policies: recent turns, pinned items, retrieval score, summaries, and truncation. The planner records included item IDs, excluded item IDs, token estimates, and compaction lineage so a run is explainable.

### 25.14 Compaction

Compaction creates an immutable compacted_context item with:

- source sequence range and item hashes;
- compacting model route and prompt revision;
- factual summary;
- open tasks and commitments;
- decisions and constraints;
- artifact/tool references that must remain addressable;
- confidence/warnings;
- checksum and creation time.

Original retained messages remain canonical storage and can be re-expanded according to retention. A compacted item never silently overwrites source history. Automatic compaction is evaluation-gated and occurs before hard provider context failure.

### 25.15 Memory

There is no hidden cross-session memory.

Memory types are explicit:

- session memory, derived only from one session;
- project knowledge, curated or indexed project data;
- user memory, opt-in and access-controlled;
- agent-revision knowledge, immutable references;
- ephemeral retrieval cache.

Writing durable memory requires a dedicated capability and provenance. Users can inspect, correct, export, and delete memory subject to legal hold. Memory retrieval is policy-filtered before model context.

### 25.15.1 Knowledge bases and retrieval

A KnowledgeBase is project-scoped curated/retrieved context, separate from conversational memory. Its canonical resources are:

~~~text
KnowledgeBase
├── KnowledgeSource → SourceRevision
├── IngestionJob
├── DocumentRevision
│   └── ChunkRevision[]
└── IndexRevision
~~~

Sources can be uploaded artifacts, repository paths, authenticated connectors, databases through a controlled extractor, or approved web locations. Each source pins identity, authorization, sync mode, classification, region, parser, and retention.

### 25.15.2 Ingestion

Ingestion:

1. authenticates and snapshots the source revision;
2. fetches through a constrained connector;
3. scans content and validates type/size;
4. parses into immutable DocumentRevisions;
5. chunks deterministically with parser/chunker revision;
6. attaches source ACL, checksum, offsets, and provenance;
7. optionally embeds through a pinned embedding route;
8. builds a derived IndexRevision;
9. atomically activates only after completeness checks.

A failed refresh leaves the prior active IndexRevision intact. Connector credentials never enter documents/chunks or model context.

### 25.15.3 Index ownership

Document/chunk metadata is canonical in PostgreSQL and bytes are canonical in object storage. Search indexes are derived and rebuildable.

The reference distribution supports PostgreSQL full-text search and an optional compatible vector extension. External vector/search engines are adapters. Their record IDs include tenant, knowledge-base, document, chunk, and index revision; they do not become the source of truth.

### 25.15.4 Retrieval

A retrieval request specifies:

- query and modality;
- active or pinned IndexRevision;
- principal/run and policy context;
- filters and source trust classes;
- maximum documents/chunks/tokens;
- keyword/vector/hybrid strategy;
- optional pinned rerank route;
- freshness deadline;
- citation requirement.

Authorization filters are applied before scoring and again before returning content. Post-filtering a top-K set is insufficient because it can leak ranking/existence and reduce recall unpredictably.

### 25.15.5 Retrieval result

Each result includes:

- document/chunk revision and source identity;
- exact byte/character/page/line offsets where available;
- retrieval and rerank scores;
- source timestamp and ingestion time;
- trust/classification;
- stable citation reference;
- content checksum;
- redaction markers.

The context planner records which results entered the model and their token cost.

### 25.15.6 Knowledge security

- Source content remains untrusted and cannot enter privileged instruction layers.
- ACL changes invalidate affected derived authorization caches immediately.
- Deleting/disconnecting a source deactivates it and propagates to indexes, caches, exports, and future retrieval.
- Retrieval cannot use a model/provider disallowed for the source classification.
- Embeddings are treated as potentially sensitive derived data and follow the source region/retention.
- Cross-tenant or cross-project vector search is forbidden even when a shared index service is used.
- Connector sync cannot execute source-provided code/macros.
- Web sources use SSRF, license, robots/terms, and citation retention policy.

### 25.15.7 Freshness and synchronization

Sync modes are manual, scheduled, and source-event-driven. Each run can require:

- latest active revision;
- at least a given source revision;
- maximum staleness;
- reproducible pinned index.

If freshness cannot be met, retrieval fails or warns according to explicit run policy; it never silently presents stale data as current.

### 25.15.8 Retrieval evaluation

Knowledge release tests measure:

- ACL/tenant isolation;
- source/chunk recall and precision;
- citation offset validity;
- stale/deleted content absence;
- prompt-injection resistance;
- parser/chunker regression;
- embedding/rerank cost and latency;
- answer groundedness on held-out cases.

### 25.16 Dynamic model switching

A session model change creates a ConfigRevision and applies at the next model safe boundary.

The kernel translates canonical history into the next provider format. Provider-private reasoning tokens, encrypted continuation objects, cache handles, and unsupported content are not assumed portable. The switch result reports:

- requested and selected route revisions;
- first model step using the new route;
- any omitted or summarized provider-specific state;
- whether a new compaction was required;
- estimated cost/context effect.

Immediate switch cancels the current model attempt when possible. Partial text remains an explicit partial item and is not silently presented as final assistant output.

### 25.17 Model failure recovery

The kernel distinguishes:

- request definitely not accepted;
- request accepted but no output observed;
- partial stream observed;
- final response observed but usage acknowledgement missing;
- provider result durably committed.

Retry and fallback policy depends on this state. A possibly completed request is never blindly replayed when it could have produced a tool side effect through a provider-hosted mechanism. All platform tools remain platform-brokered to reduce this ambiguity.

### 25.18 Subagent execution

child.request includes role, objective, input artifacts, model-route alias, tool/capability subset, output contract, budget, deadline, and workspace mode.

Guardrails:

- child capabilities are an intersection of parent, profile, project, and requested capabilities;
- children never receive parent secrets by inheritance;
- default workspace mode is read-only snapshot;
- mutable work uses a separate branch/worktree and explicit merge;
- depth, fan-out, concurrent children, tokens, cost, and duration are bounded;
- recursive delegation is off by default;
- parent cancellation propagates unless a child was explicitly detached by policy;
- child output enters parent context as a typed result/artifact, not hidden transcript.

### 25.19 Required delegation

When delegation.required is true:

- admission verifies a conforming route and capacity;
- at least one child matching the declared role must reach a terminal result;
- inability to delegate is a typed failure, not silent parent-only execution;
- the final response identifies child runs and how their results were used.

### 25.20 Engine termination

The supervisor classifies termination as:

- clean terminal frame and zero exit;
- clean terminal frame followed by abnormal exit;
- cooperative cancellation;
- resource limit;
- protocol violation;
- liveness timeout;
- host/sandbox loss;
- operator termination;
- unknown.

A terminal frame is not authoritative until persisted by the control plane under the current fencing token. An exit without a terminal frame enters recovery rather than being interpreted as success.

## 26. Checkpoint, recovery, and replay

### 26.1 Separate recovery objects

Recovery keeps three concerns separate:

- engine checkpoint: opaque model/tool/context loop state;
- workspace snapshot: filesystem and repository state;
- canonical transcript/event state: control-plane history.

They have a shared recovery boundary ID but independent formats and retention. Restoring one never implies the others were restored.

### 26.2 Checkpoint metadata

Every checkpoint records:

~~~json
{
  "checkpoint_id": "chk_...",
  "run_id": "run_...",
  "attempt_id": "att_...",
  "boundary_id": "bnd_...",
  "engine_digest": "sha256:...",
  "engine_version": "1.4.0",
  "protocol_version": "engine.v1",
  "format": "reference-kernel",
  "format_version": 3,
  "config_snapshot_hash": "sha256:...",
  "transcript_sequence": 184,
  "workspace_snapshot_id": "wsnap_...",
  "pending_operations": [],
  "content_checksum": "sha256:...",
  "created_at": "2026-07-16T12:00:00Z"
}
~~~

Checkpoint bytes are encrypted, integrity-protected, size-bounded, malware-scanned where meaningful, and never interpreted by the control plane.

### 26.3 Recovery ladder

Recovery always attempts, in order:

1. **Exact continuation** — the original healthy sandbox/process still owns the current fenced lease and acknowledges the reconnect.
2. **Portable checkpoint** — a new process whose declared compatibility accepts the checkpoint restores it together with the matching workspace snapshot and transcript boundary.
3. **Transcript reconstruction** — a fresh engine receives canonical messages, completed tool results, config snapshot, artifact references, and reconstructed workspace state.
4. **Explicit failure** — when policy forbids reconstruction or required state no longer exists.

The chosen level is emitted and visible in the final run. The platform never calls transcript reconstruction an exact resume.

### 26.4 Compatibility decision

A checkpoint is accepted only if:

- format and version are declared compatible by the target engine;
- protocol major is compatible;
- checksum and encryption verification pass;
- config differences are allowed by the checkpoint contract;
- transcript boundary exists;
- workspace snapshot matches boundary or the checkpoint declares no workspace dependency;
- no pending uncertain side effect would be hidden.

Engine digest equality permits exact binary compatibility but is not alone sufficient. A newer engine may explicitly migrate a checkpoint in a sandboxed migration step that preserves the original and emits a new checkpoint.

### 26.5 Checkpoint frequency

The reference policy checkpoints:

- after each completed external side-effecting tool call;
- after repository publication state changes;
- after context compaction;
- before a long wait or approval;
- before planned host drain;
- periodically during long compute, subject to minimum interval and size budget;
- on explicit pause/request.

Checkpoint failure does not always fail the run, but a run that enters wait, pause, or drain without a required recoverable boundary MUST fail or remain on its current host according to policy.

### 26.6 Tool replay classes

Each tool operation is classified:

| Class | Replay behavior |
|---|---|
| pure | safe to execute again; cached result may be reused |
| idempotent | resend with stable destination idempotency key |
| read_with_time_variance | caller policy chooses cached result or refreshed call; both are labeled |
| reversible_side_effect | reconcile first; retry or compensate under policy |
| irreversible_side_effect | never automatic replay after uncertain completion |
| interactive | requires client/approval availability and cannot be silently replayed |

The platform stores request hash, lease owner, attempts, external idempotency key, sanitized result, reconciliation state, and commit boundary for every call.

### 26.7 Tool-call state machine

~~~text
proposed → policy_check → approval_pending → ready → leased → executing
                                               ├→ completed
                                               ├→ failed
                                               ├→ canceled
                                               └→ uncertain → reconciled_completed
                                                            → reconciled_not_applied
                                                            → manual_resolution
~~~

Only completed and reconciled_completed results enter context as successful tool results. uncertain blocks automatic continuation when the result could affect subsequent reasoning.

### 26.8 Process and host loss

On missed runner heartbeats:

1. stop accepting events from the stale fencing token;
2. mark the attempt lost after the configured grace period;
3. retain the workspace until its lease or host reality is reconciled;
4. inspect last durable checkpoint, workspace snapshot, and tool states;
5. select recovery level;
6. create a new attempt and emit attempt.recovering;
7. deliver messages queued during outage in canonical order.

If the old host returns, it may upload diagnostics but cannot resume authoritative execution after fencing has advanced.

### 26.9 Queued messages during recovery

Messages accepted while a run is disconnected remain durable and ordered:

- queue messages apply after recovery reaches an input boundary;
- steer messages apply at the first safe boundary;
- interrupt messages set cancellation intent before a new attempt begins;
- duplicate source messages are removed by source ID;
- no message is inserted inside a reconstructed historical model step.

### 26.10 Cancellation

Cancellation is monotonic intent:

1. persist cancel command and event;
2. stop admission of new model/tool/child work;
3. request cooperative engine cancellation;
4. cancel provider requests and cancelable tools;
5. propagate to children;
6. wait a bounded grace period;
7. force sandbox termination;
8. reconcile active external operations;
9. finalize as canceled or failed_with_uncertain_side_effect.

Repeated cancellation is idempotent. Cancel does not erase partial outputs, usage, artifacts, or audit evidence.

### 26.11 Pause and resume

Pause differs from cancel:

- it reaches a safe boundary;
- requires a valid checkpoint and workspace snapshot according to policy;
- releases billable compute after snapshot;
- retains session/run identity and pending messages;
- may retain storage charges;
- resume creates a new attempt if the process was released.

A pause request can time out if an irreversible tool is in progress. The API reports why.

### 26.12 Recovery proof

Every recovered run exposes:

- previous and new attempt IDs;
- recovery level;
- checkpoint and workspace snapshot IDs;
- transcript boundary;
- replayed versus reused tool calls;
- config/model changes;
- any semantic-loss warning;
- measured recovery duration.

This record is required for UAT and support; “resumed” without evidence is not accepted.

## 27. Model gateway and routing

### 27.1 Ownership

The platform owns a canonical model contract and usage record. A provider SDK, OpenAI-compatible endpoint, third-party gateway, or routing service is an adapter behind that contract.

The model broker is the only normal path from an engine to a provider. This ensures:

- provider credentials never enter the engine sandbox;
- policy, residency, budgets, rate limits, retries, and usage are consistently enforced;
- model changes do not require rebuilding the engine image;
- managed keys and bring-your-own keys behave through one audit path;
- provider-specific telemetry can be reconciled.

An administrator MAY explicitly enable direct egress for a custom engine, but such a run is marked unmetered_or_unverified and is ineligible for strong usage, secret-isolation, and provider-policy guarantees.

### 27.2 Model provider and connection

A ModelProvider describes an adapter implementation and supported API families. A Connection is a project or organization-scoped credential/configuration binding containing only secret references and non-secret endpoint settings.

Supported connection classes:

- direct first-party provider API;
- cloud-hosted provider through a cloud account;
- local or private inference endpoint;
- OpenAI-compatible endpoint with declared deviations;
- LiteLLM proxy;
- custom gateway implementing the platform adapter interface.

“OpenAI-compatible” means transport compatibility only. The adapter must probe and declare actual support for tools, structured output, streaming, usage, cancellation, multimodal content, reasoning controls, and context limits.

### 27.3 Canonical model request

A broker request contains:

- route revision or explicit allowed provider model;
- canonical ordered content;
- model-visible tools and tool-choice policy;
- desired response modalities;
- JSON Schema output contract;
- temperature/top-p/reasoning controls only when semantically supported;
- max output tokens and stop conditions;
- timeout, priority, and cancellation handle;
- run/model-step/idempotency identities;
- privacy, residency, retention, and training restrictions;
- cache policy;
- budget reservation.

Provider-specific options live in a namespaced provider_options object validated by the chosen adapter. They are excluded from portable fallback unless the destination adapter explicitly maps them.

### 27.4 Canonical model result

The broker normalizes:

- output content items and incremental deltas;
- tool requests;
- refusal and safety signals;
- finish reason;
- input, output, cached, reasoning, audio, and other provider-reported usage dimensions;
- actual provider, endpoint, region, account, and model ID;
- provider request ID;
- latency breakdown;
- cache status;
- retry/fallback attempt lineage;
- sanitized provider warnings/errors.

The raw provider response MAY be retained as an encrypted diagnostic artifact when policy permits. It is never the only canonical result.

### 27.5 Model capabilities

Every concrete provider model revision has a capability record:

- input/output modalities;
- streaming;
- tool calling and parallel calls;
- strict structured output;
- context and output limits;
- reasoning controls;
- prompt caching;
- provider-side storage controls;
- cancellation;
- deterministic seed if supported;
- region/residency choices;
- zero-data-retention or training policy flags;
- known compatibility constraints;
- price dimensions and currency;
- last validation time.

Capabilities come from adapter-maintained metadata plus active probes. A stale or unverified capability is labeled. Admission fails before the run if a hard requirement is unavailable.

### 27.6 Model routes

A ModelRoute is a stable alias such as primary, fast, research, review, vision, or embedding. A ModelRouteRevision is immutable and contains:

- ordered candidate targets;
- hard capability predicates;
- policy and residency predicates;
- quality tier;
- maximum price and latency preferences;
- per-target concurrency and rate limits;
- retry/fallback rules;
- cache and data handling policy;
- optional traffic weights for evaluated rollout;
- evaluation evidence and publication state.

Runs pin the route revision, not just the alias. Every model step records the actual selected target.

### 27.7 Selection algorithm

Selection is deterministic given health observations:

1. filter by authorization and data policy;
2. filter by required modality, tools, schema, context, output, and reasoning capabilities;
3. filter by region and connection availability;
4. filter by remaining run budget and target price ceiling;
5. exclude open circuit breakers and exhausted quotas;
6. apply route priority/weights;
7. reserve estimated cost and capacity;
8. record a routing decision ID and explanation;
9. dispatch.

A route cannot silently select a target that violates a hard predicate to avoid an error.

### 27.8 Direct adapters and LiteLLM

Direct provider adapters are first-class and remain available even when a gateway is configured. They preserve provider-specific features and provide an escape path from gateway limitations.

[LiteLLM](https://docs.litellm.ai/) is supported in two ways:

- as an OpenAI-compatible proxy target;
- through an enhanced adapter that imports health, routing, budgets, and usage metadata where available.

LiteLLM is optional. Its virtual keys, model aliases, or spend database do not become canonical platform identity, route, policy, or billing records. Gateway retries are coordinated or disabled to avoid multiplicative retries.

### 27.9 Retry layers

Only one layer owns retry for a model attempt. The broker configures provider SDK, HTTP client, and optional gateway so their hidden retries are disabled or reported.

Retry is allowed when:

- the request was definitely not accepted;
- the provider declares a safe idempotency key;
- a transient error occurred before output;
- policy explicitly permits a new billed attempt after ambiguous acceptance.

Once a content delta is exposed, a retry is a new model attempt and its partial predecessor remains visible. It cannot masquerade as continuation.

### 27.10 Fallback

Fallback is a new attempt under the same ModelStep and is allowed only if:

- the route revision names the fallback;
- hard capabilities and data policy still match;
- context can be represented safely;
- no provider-hosted action may have occurred;
- remaining budget covers it.

Fallback reason and semantic differences are public. When a provider-specific continuation, encrypted reasoning state, cache handle, or unsupported content prevents safe portability, the step fails or requests compaction according to policy.

### 27.11 Hedging and speculative requests

Parallel hedged model requests are disabled by default because they duplicate cost and data disclosure. A route MAY enable them for read-only latency-sensitive calls when:

- every target satisfies the same policy;
- budget reserves all hedges;
- cancellation behavior is known;
- only one final result commits;
- all attempts and charges are recorded.

Hedging is never used for a step that can invoke provider-hosted external tools.

### 27.12 Rate limits and circuit breakers

The broker maintains per connection, provider, model, organization, and project limiters. Provider headers update observed windows without being blindly trusted across tenants.

Circuit breakers distinguish:

- target model failure;
- account/credential failure;
- region endpoint failure;
- gateway failure;
- caller-specific invalid request.

Invalid user requests do not trip a shared circuit. A circuit transition emits operational telemetry and affects new selection, not already accepted results.

### 27.13 Budgets and reservations

Before dispatch, the broker estimates maximum incremental cost and reserves it against run/project budgets. On completion it settles against provider usage or a conservative estimate.

When exact price is unknown:

- a project-configured ceiling is required for managed billing;
- usage is marked estimated;
- final invoice reconciliation cannot exceed a published adjustment policy;
- a missing price never means free.

Budget warnings are emitted at configured percentages. Crossing a hard budget stops new model/tool/child work at the next safe boundary.

### 27.14 Prompt caching

Caching is provider-specific optimization, not durable context:

- route policy decides whether data is eligible;
- cache keys include provider/model, relevant request content, policy and tenant boundary;
- provider cache handles never cross tenants or routes;
- cache reads/writes and discounted tokens are recorded;
- switching provider never assumes cache portability;
- sensitive projects may disable provider-side caching.

The platform MAY cache exact deterministic non-streaming model results only with explicit caller opt-in, a content hash, tenant isolation, TTL, and disclosure that the result was reused. General semantic response caching is off by default.

### 27.15 Privacy and provider policy

Each request carries effective controls for:

- allowed providers/accounts;
- allowed regions;
- provider retention mode;
- training opt-out requirement;
- data classification;
- customer-managed key requirement where supported;
- whether raw prompts/responses may enter diagnostic storage;
- whether prompt caching is allowed.

Provider marketing names are not sufficient evidence. Connection administrators attach verified policy claims with review date and supporting documentation. Runs record which claims controlled selection.

### 27.16 Model route rollout

A new route revision moves through draft → validated → canary → active → retired:

- validation runs capability and regression suites;
- canary uses an explicit traffic slice or selected projects;
- quality, failure, latency, and cost gates compare against the current revision;
- rollback changes the alias pointer for new steps;
- already-started steps retain their pinned revision;
- retired revisions remain readable for audit and replay.

### 27.17 Embeddings, reranking, audio, and image generation

Non-chat inference uses the same connection, route, policy, budget, and usage mechanisms with separate route kinds:

- generation;
- embedding;
- rerank;
- transcription;
- speech;
- image generation.

Agent profiles declare required kinds. A generation route cannot be substituted for an embedding route merely because an endpoint accepts both.

### 27.18 Provider adapter conformance

An adapter is stable only after tests for:

- content conversion and round-trip;
- tool requests and parallel tools;
- structured output;
- streaming order and UTF-8 boundaries;
- cancellation;
- timeout and retry classification;
- every usage dimension;
- context/output limits;
- multimodal upload;
- provider safety/refusal;
- unknown finish reasons;
- redaction of auth and raw errors;
- fallback portability fixtures.

## 28. Tools, capabilities, MCP, skills, and extensions

### 28.1 Tool taxonomy

The common ToolDefinition supports these executors:

| Executor | Where it runs | Typical examples |
|---|---|---|
| sandbox | inside the run sandbox through the supervisor | files, shell, local test commands |
| control_plane | trusted platform service | artifact operations, session operations |
| remote_http | customer or integration service | business API |
| MCP | stdio process or Streamable HTTP server | third-party tool/resource server |
| client | attached SDK/client worker | UI action, local user machine capability |
| capability_worker | enrolled specialized host | macOS/iOS build, Android device, private network |
| child_run | platform delegation broker | research/review/coding subagent |

The model sees one normalized tool schema regardless of executor. Executor identity and network credentials remain hidden.

### 28.2 Tool definition and revision

A stable ToolRevision contains:

- globally unique namespaced name;
- semantic version and immutable digest;
- title and model-visible description;
- JSON Schema 2020-12 input and output;
- executor configuration with secret references;
- timeout and output-size limits;
- declared side-effect/replay class;
- required capabilities;
- approval hint;
- data classifications accepted/returned;
- network destinations;
- concurrency and rate-limit policy;
- provenance and publisher identity.

Tool descriptions and annotations are untrusted claims until an administrator or signed trusted publisher policy approves them.

### 28.3 Tool sets

A ToolSetRevision pins exact tool revisions plus:

- model-visible aliases;
- argument constraints;
- capability intersection;
- approval overrides that may only become stricter;
- per-tool budgets;
- availability conditions.

Agent and session config reference immutable tool-set revisions. Dynamic tool changes create a new ConfigRevision at a safe boundary.

### 28.4 Tool naming

Canonical names use publisher.namespace.tool and remain stable. Model-visible shortened names are deterministically generated per model step and collision-checked. Results always carry canonical identity.

Names are case-sensitive ASCII identifiers with length limits. Two MCP servers exposing search cannot collide because their canonical publisher/connection namespace differs.

### 28.5 Tool dispatch

1. Validate tool identity and argument schema.
2. Normalize arguments and compute request hash.
3. Evaluate policy/capabilities.
4. Resolve approval.
5. Atomically create or recover ToolCall by tool_call_id.
6. Acquire a fenced execution lease.
7. Resolve short-lived credentials only at the executor.
8. Execute with timeout, output, and network limits.
9. Persist outcome and usage before replying to the engine.
10. Reconcile ambiguous completion according to replay class.

The model cannot bypass dispatch by inventing a tool name or direct URL.

### 28.6 Tool results

A normalized result has:

- status: completed, failed, canceled, denied, timed_out, or uncertain;
- structured content validated against the output schema;
- bounded human/model-readable text;
- artifact references for large or binary output;
- citations/source references;
- sanitized error code;
- timing and metering;
- side-effect receipt and external object IDs;
- redaction markers.

The engine receives only the model-visible projection. Full audit details require separate permission.

### 28.7 Sandbox file tools

Built-in file operations:

- resolve every path relative to an assigned workspace root;
- reject traversal, host mounts, device files, sockets, and disallowed symlink escapes;
- support bounded read, write, patch, list, search, stat, and checksum;
- write atomically where possible;
- report before/after hash and changed paths;
- preserve binary data through artifacts rather than model text;
- honor read-only and protected path policies.

File writes are visible events and contribute to a changeset. Reads of likely secrets are blocked or redacted by policy.

### 28.8 Shell tool

Shell execution:

- uses an argv form by default, not a shell string;
- requires explicit shell mode for pipelines/redirection;
- runs as the sandbox user in the workspace;
- has timeout, process count, CPU, memory, disk, and output limits;
- streams bounded stdout/stderr while storing full allowed logs as artifacts;
- kills the entire process group on cancel;
- redacts secrets in command display and output;
- records executable resolution and exit/signal status;
- cannot invoke the container runtime or host control socket.

Interactive PTY is a distinct capability. Commands with external side effects or protected paths can require approval.

### 28.9 Network tool and egress

Network access is deny-by-default in high-trust profiles and policy-driven elsewhere.

Controls include:

- DNS and connect through an egress proxy;
- scheme, host, port, method, and path allowlists;
- private, loopback, link-local, metadata-service, and cluster-control ranges denied;
- resolution and connection IP checked to prevent DNS rebinding;
- TLS verification mandatory except explicit local development;
- request/response size, redirect, timeout, and rate limits;
- destination and byte metering;
- per-tool service identities instead of general internet credentials.

Browser automation runs in its own isolated process/container and cannot inherit the control-plane browser session unless a user explicitly connects an approved capability.

### 28.10 Capability model

Capabilities are typed grants, not booleans:

~~~json
{
  "kind": "repository.publish",
  "resource": "repo:installation/123/repository/456",
  "actions": ["create_branch", "push", "open_pull_request"],
  "constraints": {
    "branch_prefix": "agent/",
    "protected_branch": false
  },
  "expires_at": "2026-07-16T13:00:00Z"
}
~~~

Effective capability is the intersection of deployment, organization, project, connection, agent revision, session, run, and child constraints. A lower layer cannot broaden an upper layer.

### 28.11 Capability tokens

At execution time, the broker issues a short-lived, audience-bound, one-operation or narrowly scoped capability token:

- bound to organization, project, run, attempt fencing token, tool call, executor, action, and resource;
- expires within minutes;
- unusable by another tool or destination;
- contains no long-lived secret;
- can be revoked before dispatch;
- is exchanged by the executor for the minimum necessary credential when required.

The engine sees an opaque handle, not the token or underlying provider credential.

### 28.12 Built-in policy engine

The baseline distribution includes a deterministic policy evaluator supporting:

- role and relationship authorization;
- capability intersection;
- resource/branch/destination constraints;
- data classification and residency;
- model/provider restrictions;
- approval rules;
- time, cost, and concurrency budgets;
- extension trust and signature policy.

Policy inputs, result, policy revision, and decision ID are auditable with sensitive fields masked. An [OPA](https://www.openpolicyagent.org/docs/) adapter MAY evaluate signed policy bundles, but OPA is not required to boot or enforce the standard policy model.

Fail behavior:

- authorization, secret, network, publication, and irreversible actions fail closed;
- optional observability enrichment may fail open with a warning;
- policy timeout is distinct from denial.

### 28.13 MCP support

At the date of this specification, the supported stable MCP revision is [2025-11-25](https://modelcontextprotocol.io/specification/2025-11-25/basic). Future revisions require a separate adapter/conformance release.

Supported transports:

- stdio for a sandboxed local MCP server;
- Streamable HTTP for remote MCP servers.

Platform mapping:

- MCP tools become ToolRevisions namespaced by connection;
- resources become explicit context references subject to ingestion policy;
- prompts are user/agent-selectable templates, never privileged system policy;
- progress and cancellation map into tool events;
- elicitation maps to waiting_for_input or approval;
- server sampling requests are denied by default and, when enabled, route through the model broker with a separate budget and visible child/model step.

Experimental MCP Tasks are not used for internal run persistence. Platform runs remain canonical.

### 28.14 MCP authorization and security

The implementation follows MCP authorization guidance:

- OAuth tokens are audience/resource bound;
- PKCE and exact redirect matching are required where applicable;
- incremental scopes are requested only when needed;
- token passthrough to an upstream service is forbidden;
- inbound Origin is validated for HTTP transport;
- server metadata fetching uses SSRF protections;
- tool annotations are untrusted;
- connection credentials are secret references;
- each MCP server has an explicit trust level and capability ceiling.

Remote MCP servers cannot call back into arbitrary platform endpoints with the bearer token they received.

### 28.15 Agent Skills compatibility

The platform imports and exports the open [Agent Skills](https://github.com/agentskills/agentskills) folder convention:

- SKILL.md contains discoverable instructions and metadata;
- referenced scripts/resources remain in the skill directory;
- progressive loading avoids placing every skill body in context.

A platform SkillRevision additionally pins:

- source and publisher;
- immutable digest and semantic version;
- required tools/capabilities;
- environment/runtime dependencies;
- allowed network destinations;
- signature and provenance;
- scan result;
- compatibility range.

A skill grants no authority. Its instructions, scripts, and metadata are untrusted content constrained by the active tool set and policy.

### 28.16 Skill installation

- Installation by URL or repository is a control-plane/admin action, never an autonomous model action by default.
- Sources are fetched without credentials into a quarantine environment.
- Archives reject traversal, symlink escape, special files, and decompression bombs.
- Static secret, malware, dependency, and executable-content scans run.
- The exact content digest is presented for approval and pinned.
- Updates create new revisions and require re-evaluation when requested capabilities change.
- Runs never use “latest” without resolving and recording a digest.

### 28.17 Hooks

Hook points are:

- before_admission;
- before_context;
- before_model and after_model;
- before_tool and after_tool;
- before_artifact_finalize;
- before_repository_publish;
- on_checkpoint;
- on_terminal.

Hooks are typed, versioned, timeout-bounded extensions. They are categorized:

- policy hook: synchronous, deterministic, fail closed, cannot perform arbitrary network I/O;
- transform hook: returns a schema-validated patch to allowed fields;
- observer hook: asynchronous, cannot affect the operation.

Arbitrary tenant code never runs inside the API process. Custom hooks execute as signed WebAssembly modules with a constrained host API or as isolated remote workers. Hook output cannot grant capabilities.

### 28.18 Plugins and extensions

An extension bundle may include:

- tool definitions/executors;
- skills;
- provider adapters;
- sandbox drivers;
- integration adapters;
- hooks;
- UI metadata.

Its manifest declares API/protocol compatibility, permissions, entry points, configuration schema, migrations, publisher, digest, and signature. Extensions cannot patch database tables directly; they use namespaced storage or declared migrations reviewed by the platform.

Managed SaaS runs only allowlisted or reviewed extensions. Self-host administrators can relax this policy but receive an explicit trust warning.

### 28.19 Extension lifecycle

~~~text
discovered → quarantined → validated → approved → enabled → disabled → retired
~~~

Upgrade is side-by-side:

- old revisions remain available for pinned runs;
- new runs use the activated revision;
- rollback changes activation pointer;
- an extension cannot be removed while retained runs/checkpoints require it unless those runs become explicitly non-replayable;
- extension crashes are isolated and trip their own circuit breaker.

### 28.20 Tool and extension provenance

OCI executors and extension artifacts are digest-pinned and verified. Release policy supports [Sigstore Cosign verification](https://docs.sigstore.dev/cosign/verifying/verify/), SBOMs, and in-toto/SLSA provenance.

Trust policy may require:

- exact publisher identity;
- allowed source repository;
- CI workflow identity;
- transparency-log inclusion;
- vulnerability threshold;
- recent scan;
- reproducible build evidence.

### 28.21 Tool availability during a run

If a tool becomes unhealthy:

- existing completed results remain valid;
- new calls fail with tool_unavailable or route to an explicitly equivalent executor revision;
- replacement must satisfy the same schema, capabilities, data policy, and replay semantics;
- the actual executor is recorded;
- a mandatory tool outage can pause or fail the run;
- no unrelated tool is silently substituted based only on a similar name.

### 28.22 Human and client input

Tools can request typed human/client input:

- confirmation;
- secret connection authorization;
- selection;
- form data;
- file upload;
- free text.

Requests declare schema, audience, expiry, sensitivity, and whether the response may enter model context. Sensitive input can be routed directly to the secret broker and represented to the model only as “connection established.”

### 28.23 Tool developer SDK

The public extension SDK provides TypeScript, Python, and Go helpers to:

- define input/output JSON Schema from native types while preserving the emitted schema;
- declare capabilities, side-effect/replay class, timeout, destinations, and data classification;
- validate calls and produce normalized results/artifacts/progress;
- verify signed platform invocation and caller audience;
- store/recover tool_call_id idempotency;
- redeem a scoped Connection/capability handle;
- run a local conformance server;
- package/sign a remote, sandboxed, or capability-worker executor.

The SDK does not let a tool declare itself trusted; publisher/admin policy assigns trust.

### 28.24 Remote HTTP tool protocol

The broker invokes a remote tool endpoint with:

~~~json
{
  "protocol": "tool-http.v1",
  "tool_call_id": "tcall_...",
  "tool_revision": "publisher.namespace.tool@1.2.0",
  "run_id": "run_...",
  "attempt_id": "att_...",
  "request_hash": "sha256:...",
  "deadline": "2026-07-16T12:01:00Z",
  "arguments": {},
  "capability_handle": "opaque",
  "callback": {
    "url": "broker-controlled callback",
    "token": "one-use audience-bound handle"
  }
}
~~~

Transport requirements:

- HTTPS with mTLS, OAuth audience token, or signed request according to Connection;
- Idempotency-Key equals the stable tool_call_id;
- timestamp and body digest protect replay/tampering;
- redirects are off;
- exact endpoint and resolved network policy are pinned;
- response size/time is bounded.

The executor may return:

- 200 with terminal normalized result;
- 202 with operation ID and optional progress URL, then signed callback;
- 409 for request-hash mismatch;
- RFC 9457 error.

A callback is accepted once under the active tool lease/fence and same result hash. Polling/callback retries never create a second logical ToolCall.

### 28.25 Remote tool progress and cancellation

- Progress is advisory, ordered per operation, bounded, and cannot report terminal success.
- Cancellation sends the stable operation ID and deadline; lack of cancellation support is declared.
- After timeout/cancel, a late terminal callback enters reconciliation and is not silently discarded or applied to a newer call.
- A long-running remote tool must provide query/reconcile semantics before it can be classified idempotent or reversible.

## 29. Sandbox and execution-host architecture

### 29.1 Security premise

Repository content, model-generated commands, installed dependencies, web content, build scripts, tests, extensions, and tool output are untrusted. An ordinary Linux container is a packaging boundary, not a sufficient hostile multi-tenant security boundary.

The platform supports explicit isolation tiers and never labels them all “sandbox” without qualification.

### 29.2 Isolation tiers

| Tier | Intended use | Minimum boundary |
|---|---|---|
| development | one developer on a trusted machine | local container; security guarantees explicitly absent |
| trusted_single_tenant | customer-controlled trusted code | hardened rootless container with mandatory profiles |
| hardened | untrusted code with shared host risk accepted | userspace-kernel or equivalent hardened runtime plus host controls |
| microvm | managed multi-tenant untrusted workloads | hardware-virtualized microVM/Kata-class boundary |
| dedicated | regulated or highest assurance | dedicated node/VM/pool, optional dedicated control/data plane |

Managed SaaS MUST use microvm or dedicated isolation for arbitrary third-party code by stable release. Hardened tier MAY be offered for lower-risk workloads with explicit tenant policy and disclosure.

[gVisor](https://gvisor.dev/docs/) is a supported hardened-runtime candidate. [Firecracker](https://firecracker-microvm.github.io/) and Kata-class virtualization are supported microVM candidates. These implementations are drivers, not public contract dependencies.

### 29.3 Sandbox driver interface

Every driver implements:

- capabilities and isolation attestation;
- create from immutable EnvironmentRevision;
- start/stop/kill;
- exec through supervised channels;
- file transfer;
- network policy attachment;
- resource limit and usage reporting;
- health/liveness;
- snapshot and restore when supported;
- secure destroy and residual-state report;
- image digest verification;
- audit correlation.

Drivers expose unsupported behavior before placement. The control plane never assumes snapshot, pause, nested virtualization, GPU, or network-policy support.

### 29.4 Baseline drivers

The supported roadmap contains:

- local OCI driver for laptop and single-node self-host;
- hardened OCI driver using a userspace-kernel runtime where available;
- Kubernetes driver with RuntimeClass support;
- microVM driver;
- optional Kubernetes SIG [Agent Sandbox](https://github.com/kubernetes-sigs/agent-sandbox) adapter after its API is pinned and conformance-tested.

Kubernetes is optional. Kubernetes-specific objects are not exposed as public workspace resources.

### 29.5 Environment templates

An EnvironmentTemplate is a lineage; EnvironmentRevision is immutable and includes:

- operating system, architecture, and base image digest;
- package/runtime declarations and lockfiles;
- initialization command run without repository credentials;
- non-secret environment variables;
- resource minimum/default/maximum;
- workspace mount and filesystem policy;
- network policy;
- supported engine protocol and engine image constraints;
- sandbox isolation minimum;
- device/accelerator requirements;
- snapshot compatibility;
- provenance, SBOM, vulnerability state, and signature requirements.

Agent profiles reference an environment revision or an allowed selector. A run records the resolved revision and all image digests.

### 29.6 Image build pipeline

Environment and engine images are built outside run sandboxes:

1. validate declarative build definition;
2. resolve base images to digests;
3. build in an isolated builder with no production secrets;
4. generate SBOM and provenance;
5. scan dependencies and configuration;
6. sign the manifest digest;
7. run protocol and smoke tests;
8. publish immutable revision;
9. promote through development, canary, and stable channels.

The platform does not accept a Dockerfile from a model and automatically promote the result to a trusted production image.

### 29.7 Workspace model

A Workspace is a logical filesystem lineage independent of a process or host. A WorkspaceBinding connects it to one session or run.

States:

~~~text
requested → provisioning → preparing → ready → leased
                              |          ↕
                              |        snapshotting
                              |          ↕
                              +→ paused → restoring
                                         |
             failed ← recovering ← host_lost

ready|paused|failed → destroying → destroyed
~~~

The logical workspace ID remains stable across host movement. Each physical allocation has a separate allocation ID and fencing token.

### 29.8 Workspace ownership

- A mutable workspace has at most one active writer lease.
- The root run normally owns that lease.
- Child runs receive a read-only snapshot or isolated copy-on-write branch.
- Client terminal attachment shares the same serialized writer boundary; simultaneous writes require explicit collaborative locking.
- A stale host cannot upload a new authoritative snapshot after fencing advances.
- Workspace state does not confer session, repository, or publication permission.

### 29.9 Filesystem layout

The supervisor provides documented logical paths:

~~~text
/workspace/repo        checked-out repository
/workspace/scratch     run scratch data
/workspace/artifacts   staged outputs
/runtime               read-only engine/runtime support
/secrets               normally absent; narrowly mounted ephemeral handles only
~~~

Exact host paths are hidden. Root filesystem is read-only except declared ephemeral locations. Workspace, temp, and cache quotas are separate. Device nodes, host sockets, and container runtime sockets are absent.

### 29.10 Snapshot semantics

A WorkspaceSnapshot is immutable, content-addressed, encrypted, and tied to a boundary ID.

It includes:

- changed file tree and metadata required for reconstruction;
- repository HEAD/index/worktree state;
- environment revision and base image digests;
- excluded-path manifest;
- total/logical byte size and checksums;
- parent snapshot for incremental storage;
- creation reason and fencing token.

It excludes:

- secret mounts and credential helpers;
- process memory;
- network connections;
- OS keychains;
- package registry tokens;
- host-specific sockets/devices;
- caches declared non-portable.

Restore verifies all referenced objects and recreates exclusions as empty. Snapshot success is not assumed until the control plane verifies the manifest and object availability.

### 29.11 Snapshot retention

- latest recoverable snapshot is protected while a run/session can resume;
- incremental chains are periodically flattened;
- deletion follows session retention and legal hold;
- customers can configure maximum snapshot bytes and frequency;
- managed service may charge stored bytes and snapshot operations;
- “delete workspace” schedules both metadata and object deletion and produces a tombstone receipt.

### 29.12 Warm pools

Warm pools can reduce cold start but are never tenant-dirty reuse:

- pool members contain only a verified environment image;
- no repository, user data, credential, prior process, or writable layer is reused;
- allocation attaches a fresh writable workspace;
- return destroys rather than sanitizes the tenant layer;
- pool image digest and isolation tier must exactly match placement;
- random sampling verifies cleanup;
- security policy can disable pooling.

### 29.13 Runner host hardening

Execution hosts MUST:

- run only the runner and required runtime services;
- use minimal patched host images;
- separate control and workload networks;
- deny workload access to runner sockets and metadata endpoints;
- enforce cgroup/resource limits and mandatory access controls;
- use rootless execution where compatible;
- disable privileged containers, host PID/IPC/network, broad devices, and arbitrary host mounts;
- encrypt local workspace storage where policy requires;
- rotate short-lived workload identity;
- ship tamper-evident audit/health telemetry;
- support cordon, drain, and remote revocation.

A runner compromise is treated as a security incident, not ordinary sandbox failure.

### 29.14 Resource accounting

The runner reports:

- requested and actual CPU time;
- wall time;
- memory peak and time-weighted usage;
- local and snapshot storage;
- network ingress/egress;
- process count;
- accelerator/device time;
- sandbox start/restore/snapshot latency.

Hard limits terminate or throttle according to resource type. Resource-limit termination is distinct from agent failure.

### 29.15 Network modes

Environment policy selects:

- none;
- allowlisted internet through proxy;
- broad internet through filtered proxy;
- private network capability worker;
- explicitly attached customer network.

Direct host networking is forbidden. DNS, HTTP, and raw TCP policy use the same resolved destination controls. Private connectivity is scoped per project/run and cannot turn a shared SaaS sandbox into a general network bridge.

### 29.16 Inbound sandbox connectivity

Sandboxes have no public inbound port by default. Preview servers, terminals, browser VNC, or debug endpoints use an authenticated reverse proxy:

- random non-guessable route;
- caller authorization on every connection;
- short expiry;
- protocol/port allowlist;
- rate and byte limits;
- session/run/audit binding;
- no direct pod/container address exposure.

Public preview publication is a separate capability and produces an artifact/deployment resource.

### 29.17 Interactive terminal

Terminal attach:

- requires workspace and terminal capability;
- uses short-lived WebSocket authorization;
- records attach/detach identity and command stream policy;
- distinguishes user commands from model tool commands;
- supports read-only observation mode;
- can be disabled for sensitive workloads;
- does not allow a user to inherit hidden engine/provider credentials;
- respects the writer lease and pause state.

Terminal recording content is configurable because it may contain sensitive data; metadata is always audited.

### 29.18 Sandbox destruction

Destroy:

1. fences and stops execution;
2. revokes capability handles;
3. unmounts ephemeral secrets;
4. uploads required final diagnostics/snapshot under policy;
5. destroys writable storage and network identity;
6. verifies runtime object removal;
7. reports residual-state failure;
8. makes the allocation ID permanently unusable.

A failed destroy quarantines the host/pool member from new tenants until reconciled.

## 30. Repository lifecycle

### 30.1 Repository binding

A RepositoryBinding stores:

- provider and immutable repository identity;
- clone/fetch URL;
- default branch;
- authentication Connection reference;
- allowed read/write/publication operations;
- branch and path policy;
- fork/PR target policy;
- submodule and LFS policy;
- commit-signing policy;
- webhook source mapping;
- data classification and region constraints.

Display names and URLs are not trusted as identity. Provider installation/repository IDs are preferred.

### 30.2 Authentication

Repository access uses short-lived, repository-scoped credentials. For GitHub, a [GitHub App installation token](https://docs.github.com/en/apps/oauth-apps/building-oauth-apps/differences-between-github-apps-and-oauth-apps) is preferred over a user personal token.

Credentials:

- are minted just in time;
- are scoped separately for read, push, pull request, checks, and merge;
- enter only a credential helper or brokered Git operation;
- are excluded from command arguments, environment dumps, remote URLs, logs, snapshots, and model context;
- are revoked or expire after the operation.

The model never receives a raw Git credential.

### 30.3 Deterministic preparation

Repository preparation is infrastructure-owned:

1. resolve binding and requested ref;
2. mint read credential;
3. initialize a clean repository with safe Git configuration;
4. fetch the exact commit/ref with bounded history;
5. verify commit identity if policy requires;
6. check out a generated work branch or detached read-only state;
7. materialize allowed submodules and LFS objects;
8. disable repository hooks and unsafe filters by default;
9. remove credential material;
10. record base commit, tree hash, branch, and preparation receipt;
11. expose the prepared workspace to the engine.

The engine MAY run ordinary Git commands afterward, but initial provenance does not depend on model behavior.

### 30.4 Untrusted repository defenses

Before agent execution:

- .git configuration from the repository cannot override system credential helpers or execute arbitrary commands;
- hooks are disabled;
- submodule URLs are validated against policy and credentials are separately scoped;
- symlink and case-collision behavior is checked for the target filesystem;
- archive and LFS downloads are size-limited;
- package install scripts remain sandboxed and network-constrained;
- repository instructions are labeled untrusted context;
- likely committed secrets are detected and protected from outbound disclosure.

### 30.5 Branch strategy

Default mutable work uses a generated branch:

~~~text
agent/<session-short-id>/<run-short-id>
~~~

Policy controls prefix, source branch, force-push, protected branches, and fork use. Direct work on a protected/default branch is denied by default.

Child agents that edit code use isolated worktrees/branches. Merge into the parent worktree is an explicit tool call that detects conflicts and records source child run.

### 30.6 Changeset

A Changeset is a first-class immutable summary:

- base and final commit/tree;
- added, modified, deleted, renamed, and binary files;
- patch artifact and truncation markers;
- generated files and excluded paths;
- tests/checks and their evidence;
- commit list;
- authoring run/tool lineage;
- policy and approval state;
- possible secret or license findings.

The final model response does not substitute for the Changeset.

### 30.7 Commit creation

Commit is a sandbox tool with policy:

- deterministic configured author identity;
- generated message may be edited;
- no credential is required;
- optional SSH/Sigstore signing occurs through a brokered signing capability;
- commit hash and diff are persisted;
- signing key never enters the workspace;
- commit does not imply push permission.

### 30.8 Publication capabilities

Publication is decomposed:

- repository.push_branch;
- repository.open_pull_request;
- repository.update_pull_request;
- repository.comment;
- repository.set_status;
- repository.merge;
- repository.create_release.

Each has independent capability, approval, credential, idempotency key, and audit. The default stable policy allows push to an agent branch and opening a draft pull request after approval; merge, release, and protected-branch push are denied.

### 30.9 Push

Push execution:

1. refresh target branch and detect divergence;
2. re-evaluate branch policy;
3. show exact commits/diff for approval if required;
4. mint write-only credential;
5. push with force disabled;
6. persist provider receipt and remote commit;
7. remove credential;
8. reconcile ambiguous network completion by querying the remote ref before retry.

Force push is a separate high-risk capability and never inferred from ordinary push.

### 30.10 Pull requests

Open/update pull request uses the provider API through the control plane:

- head/base repository and branches are exact identities;
- title/body can be model-proposed but are policy-filtered;
- idempotency finds an existing PR for the run/branch before creating;
- changeset, test evidence, run link, and disclosure are attached according to template;
- duplicate callbacks do not create duplicate PRs;
- the PR ID/URL is a structured result.

The platform honors protected branch and required-review rules rather than attempting to bypass them. See [GitHub protected branches](https://docs.github.com/en/repositories/configuring-branches-and-merges-in-your-repository/managing-protected-branches/about-protected-branches).

### 30.11 Merge and release

Merge/release are excluded from ordinary coding-agent capability sets. If enabled:

- required checks/reviews are re-read immediately before action;
- exact head SHA is bound to approval;
- stale approval cannot merge a changed head;
- provider-side branch protection remains authoritative;
- ambiguous outcomes are reconciled;
- release artifacts require separate provenance and signing.

### 30.12 Repository race handling

- Base branch movement does not mutate a run's recorded base.
- Before publication, policy chooses fail, rebase, merge, or open PR with behind-base warning.
- Rebase/merge is a visible tool call and can require new tests/approval.
- Conflicts enter waiting_for_input or a bounded repair loop.
- The model cannot silently discard remote changes.
- A push receipt is tied to exact local and remote SHAs.

### 30.13 Other Git providers and local repositories

GitHub, GitLab, Bitbucket, and generic SSH/HTTPS use provider adapters. Generic Git supports clone/fetch/push but does not pretend to support pull requests or checks.

Local development can bind a local directory:

- default is a snapshot/copy, not a direct mutable bind mount;
- direct bind mode requires an explicit unsafe-development flag;
- direct mode displays that host files can be modified and cannot claim sandbox isolation;
- publication operations are disabled unless configured separately.

## 31. Specialized capability workers

### 31.1 Purpose

Some tasks cannot or should not run in the ordinary Linux sandbox:

- macOS and Xcode builds;
- iOS simulator/device tests;
- Android emulator/device farms;
- GPU workloads;
- browser/computer-use sessions;
- customer-private services;
- hardware signing or HSM operations.

These use enrolled capability workers rather than granting the Linux sandbox host or cloud credentials.

### 31.2 Worker contract

A CapabilityWorker advertises:

- typed capabilities and versions;
- operating system/architecture;
- installed runtime/toolchain digests;
- devices and health;
- tenant/pool/region/trust labels;
- concurrency and resource capacity;
- supported input/output artifact types;
- isolation and attestation claims.

It connects outbound with short-lived workload identity and uses the same lease/fencing semantics as a runner.

### 31.3 Capability job

The broker sends:

- job ID and idempotency key;
- run/attempt identity;
- exact capability and operation;
- input artifact references;
- secret/certificate handles scoped to the job;
- resource/deadline limits;
- output schema;
- network policy.

The worker returns progress, structured result, logs, artifacts, usage, and an attested execution receipt. It cannot request broader credentials from the model.

### 31.4 Apple build example

An iOS build/test flow:

1. snapshot or source archive is finalized in the Linux workspace;
2. a macOS worker lease is selected by Xcode/runtime requirement;
3. source transfers by signed artifact grant;
4. ephemeral keychain and signing assets are created only if capability permits;
5. build/test runs under a fresh worker workspace;
6. result bundle, logs, screenshots, and binary return as artifacts;
7. signing handles and workspace are destroyed;
8. publication to a store is a separate capability and approval.

The agent may analyze output, but raw signing certificates, private keys, and store credentials never enter model context or the Linux sandbox.

### 31.5 Private-network worker

A customer can enroll a worker inside its network. The run sends a typed operation to it; the ordinary sandbox does not receive arbitrary tunnel access.

If a full private sandbox is required, it is placed in a customer runner pool under the same policy. The product does not use a capability worker as an unrestricted SOCKS proxy.

### 31.6 Worker failure and idempotency

- read-only jobs may retry on another compatible worker;
- side-effecting jobs use destination idempotency and reconciliation;
- device state is quarantined after uncertain failure;
- output is accepted only under the active fencing token;
- worker health/capability changes prevent new leases;
- user-visible recovery records mirror ordinary runner recovery.

## 32. Automation and durable triggering

### 32.1 Trigger resource

A Trigger is a versioned mapping from an authenticated source event to a canonical action. It contains:

- type: webhook, schedule, queue, integration_event, or manual/API;
- source connection and event filters;
- input mapping;
- pinned AgentRevision or profile-free RunTemplateRevision;
- session-correlation rule;
- deduplication key expression;
- concurrency and ordering policy;
- retry/misfire policy;
- budget and capability ceiling;
- callback/output mapping;
- enabled state and activation revision.

Editing a trigger creates a new revision. An accepted source event records the exact revision before asynchronous processing.

### 32.2 Run template

A RunTemplateRevision provides reusable automation configuration without pretending to be an agent identity:

- instructions and input schema;
- model route;
- tool/environment/workspace settings;
- output schema;
- budgets;
- retry/callback policy.

An AgentRevision MAY reference the same components and add human-facing identity, skills, and delegation behavior. Simple webhooks do not need an AgentProfile.

### 32.3 Input mapping

Mappings use a bounded declarative expression language with:

- field selection and renaming;
- constants;
- simple conditional/default operations;
- schema validation;
- secret-reference lookup by allowed name;
- no arbitrary network, filesystem, or process access.

Mapping output is previewable and testable against saved redacted fixtures. A mapping error creates a failed TriggerDelivery without starting a billable run.

### 32.4 Trigger delivery state

~~~text
received → authenticated → deduplicated → mapped → admitted → run_created
              |                 |           |          |
            rejected         duplicate    failed     deferred
~~~

Every source delivery has its own identity and links to zero or one canonical action. A duplicate returns the original linkage.

### 32.5 Session correlation

A trigger may:

- create a new session per event;
- reuse by a bounded correlation key;
- append to a named durable session;
- reject when an active session already exists.

Correlation is scoped to project, trigger revision, and source tenant. Keys are length-bounded and hashed. Reuse does not bypass authorization or retention checks.

### 32.6 Concurrency policies

Supported policies:

- allow: independent sessions/runs;
- queue: FIFO per correlation key;
- replace: cancel older non-side-effecting work before starting new;
- drop_if_running: acknowledge and record skipped;
- coalesce: combine pending events with a deterministic reducer;
- singleton: only one active delivery for the entire trigger.

Replace and coalesce are prohibited after an irreversible side effect unless a trigger-specific reconciliation contract exists.

## 33. Scheduling

### 33.1 Schedule resource

A Schedule contains:

- cron expression or one-time RFC 3339 instant;
- IANA timezone;
- start/end boundaries;
- pinned trigger/action revision;
- misfire policy;
- overlap/concurrency policy;
- jitter window;
- enabled/paused state;
- next planned fire time.

The built-in coordinator provides scheduling; no external workflow system is required.

### 33.2 Cron semantics

- Cron uses an explicitly documented five-field minute/hour/day/month/weekday syntax for stable v1.
- Seconds and calendar intervals MAY be added as separate schedule kinds.
- Timezone is mandatory; UTC is the SDK/CLI default.
- Daylight-saving duplicate local times fire once by default, at the earlier instant.
- Nonexistent local times follow misfire policy.
- Each planned occurrence has a deterministic occurrence ID derived from schedule revision and planned UTC instant.
- Clock changes do not create duplicate occurrence IDs.

### 33.3 Misfire policies

- skip: discard missed occurrences and schedule the next;
- fire_once_now: create one catch-up occurrence;
- catch_up: create each missed occurrence up to max_catch_up;
- fail: pause schedule and alert.

Default is fire_once_now with a bounded grace window. Catch-up is never unbounded.

### 33.4 Scheduler correctness

- schedule revision and occurrence insertion are transactional;
- multiple scheduler instances may scan, but the occurrence ID is unique;
- an occurrence is durable before a run is created;
- retries reuse the same occurrence and idempotency key;
- pausing prevents future admission but does not silently cancel active runs;
- deleting a schedule preserves historical occurrences under retention.

### 33.5 Jitter

Jitter spreads load but is deterministic per occurrence, bounded by the declared window, and visible as planned_at versus admitted_at. It cannot move execution outside start/end bounds or violate a deadline.

## 34. Queue integration

### 34.1 Adapter model

Queue adapters consume from systems such as SQS, Pub/Sub, Kafka, NATS, RabbitMQ, or customer-defined brokers and normalize into InboundEvent.

The platform does not expose one queue's offset, partition, receipt handle, or delivery attempt as canonical run identity.

### 34.2 Acknowledgement

An adapter acknowledges/commits a source message only after:

- source authentication is accepted;
- raw/normalized event is durably recorded;
- deduplication record is committed;
- trigger mapping and action admission are either committed or terminally rejected under configured policy.

Long processing occurs after acknowledgement. If source semantics require delayed acknowledgement, the adapter extends visibility/lease and still relies on source event deduplication.

### 34.3 Ordering

Source partition/order is preserved only when the trigger requests queue or singleton concurrency for the corresponding correlation key. Global ordering is not promised.

An event that fails mapping can block an ordered key only according to explicit poison-message policy; default sends it to an adapter dead-letter view and advances.

### 34.4 Backpressure

- adapters pause consumption when durable queue or tenant concurrency exceeds thresholds;
- admission reports queue depth and oldest age;
- bounded buffers prevent broker floods from exhausting control-plane memory;
- priority is declared by trusted trigger configuration, never accepted directly from an untrusted payload;
- managed service applies per-tenant fairness.

### 34.5 Outbound queue delivery

Outbound result adapters use transactional outbox, stable delivery IDs, destination idempotency fields, retries, and dead-letter state. A run result is not lost because a queue publisher was temporarily unavailable.

## 35. External orchestrator integration

### 35.1 Integration contract

Any external workflow system can control the platform through:

1. create response/session/run with its workflow ID as metadata and an idempotency key;
2. wait through webhook, SSE, or polling;
3. send message/approval/cancel commands;
4. retrieve structured result and artifacts;
5. reconcile by idempotency key after timeout.

This is sufficient for Temporal, Restate, DBOS, cloud workflow services, CI systems, and custom scripts without embedding any of them into core state.

### 35.2 Optional native adapters

A native adapter MAY improve developer experience by providing activities/steps, heartbeats, and typed payloads. It MUST:

- call canonical APIs;
- keep external workflow ID separate from run ID;
- avoid retry multiplication;
- never treat workflow history as the only copy of agent state;
- support cancellation propagation;
- document which system owns each timeout/retry;
- pass the same conformance fixtures as ordinary SDK usage.

### 35.3 Exactly-once claim

The platform promises durable at-least-once delivery plus idempotent effects where supported. It does not advertise universal exactly-once execution across model providers, repositories, chat systems, and arbitrary tools.

Every integration document must distinguish:

- event deduplication;
- platform operation idempotency;
- external destination idempotency;
- uncertain side-effect reconciliation.

### 35.4 Custom agents and application flows

Customization has four deliberately separate levels:

1. **AgentRevision** for instructions, models, tools, skills, context, sandbox, budgets, and delegation while using the reference loop.
2. **Engine implementation** for a fundamentally different model/tool/control loop while retaining platform lifecycle/security.
3. **SDK-controlled application flow** for deterministic multi-session/multi-run business sequencing.
4. **External orchestrator adapter** when the customer already needs durable cross-system workflow semantics.

Tools, hooks, triggers, and integration adapters extend any of these levels.

The platform does not force custom flows into a proprietary DAG language. A flow written against the public SDK behaves the same locally and in cloud; if it needs to survive its own process loss, it uses the built-in trigger/run primitives for individual work units or the customer's chosen durable workflow system. No option creates a second hidden agent loop inside the control plane.

## 36. Slack integration

### 36.1 Product behavior

Slack is an integration adapter over Sessions, not a separate bot agent runtime.

- one Slack assistant thread or configured message thread maps to one Session;
- source workspace/team, channel, and thread timestamp form the default correlation key;
- every Slack event maps to a canonical Message or Command;
- streamed platform events are rendered as rate-limited Slack updates;
- the canonical transcript remains in the platform.

Slack's official [Events API](https://docs.slack.dev/apis/events-api/) expects a response within three seconds and retries failures. The adapter verifies, stores, deduplicates by event ID, acknowledges immediately, then schedules work.

### 36.2 Installation and identity

Each Slack installation is a Connection with:

- workspace/team and enterprise IDs;
- bot identity;
- granted OAuth scopes;
- signing secret reference;
- token references;
- allowed channels/users;
- mapping from Slack identities to platform identities or constrained external actors;
- default project/agent/route policy.

Slack user identity does not automatically become a platform administrator. Unmapped users receive only the capabilities granted to the integration actor and channel policy.

### 36.3 Transport modes

- Managed SaaS uses HTTPS Events API and interactive callbacks.
- Local/self-host MAY use Slack Socket Mode to avoid a public callback during development or private deployment.
- Both modes produce identical normalized events and session behavior.
- Switching transport does not change session correlation IDs.

### 36.4 Message ingestion

- bot and integration-originated messages are ignored unless explicitly configured, preventing loops;
- edits become new correction events and do not rewrite already-consumed transcript history;
- deletion becomes a tombstone and follows configured privacy behavior;
- files are fetched through scoped Slack authorization, scanned, stored as artifacts, and the token is discarded;
- channel history is not automatically imported; context expansion is a separate consented tool/capability;
- mentions and slash commands are parsed by deterministic integration code before model context.

### 36.5 Live output

The adapter:

- sets a visible working status;
- coalesces text deltas into bounded updates;
- renders progress/tool status without exposing hidden reasoning or secrets;
- posts large reports/files as artifacts or links;
- handles Slack rate limits and resumes from canonical events;
- posts exactly one terminal summary per run delivery ID;
- records Slack message timestamps for update reconciliation.

Slack delivery failure does not erase the platform result. A reconnect can replay and repair the visible message.

### 36.6 Commands

Deterministic commands support:

- new/reset session;
- status;
- cancel/pause/resume;
- select agent profile;
- select allowed model route;
- approve/deny;
- attach artifact;
- help and privacy controls.

Natural-language requests may propose the same actions, but sensitive commands still pass authorization and exact confirmation.

### 36.7 Approvals

Slack interactive approval displays:

- exact action, arguments/diff/destination;
- run and agent identity;
- expiry;
- risk and policy reason.

The signed interaction is bound to Slack user, workspace, approval request hash, and one-shot decision. Merely replying “yes” is insufficient for high-risk actions unless an explicit organization policy allows conversational confirmation.

### 36.8 Session attachment

A user can continue the same Session from the web console or another authorized client. Source attribution is preserved. Output routing policy decides whether responses go to Slack, web, or both.

Concurrent messages use normal queue/steer/interrupt semantics. Slack does not invent its own agent queue.

### 36.9 Privacy

- private-channel access requires installation scopes plus project policy;
- the session stores source channel classification;
- artifacts and links require platform authorization rather than relying on an unguessable URL;
- Slack message retention and platform retention are independent and disclosed;
- deletion/export requests can locate sessions through source mappings;
- sensitive tool output can be restricted to the web console even when the run started in Slack.

## 37. Generic investigation automation

### 37.1 Rejection-analysis scenario

The store-rejection use case is a reusable automation pattern, not a store-specific core primitive:

1. a signed external event supplies product/version, rejection text, attachments, locale, and correlation ID;
2. a trigger validates input and starts a pinned research AgentRevision;
3. the run uses web/retrieval tools and supplied artifacts under a read-only capability set;
4. it produces a strict InvestigationReport artifact;
5. callback delivery returns report ID, summary, confidence, and citations;
6. a human can attach to the Session for follow-up;
7. remediation code work can be forked into a separately authorized coding run.

### 37.2 Investigation report schema

The stable logical schema contains:

- subject and source-event identity;
- normalized issues;
- evidence for each issue;
- cited sources with retrieval time and excerpts within licensing policy;
- confidence and competing explanations;
- recommended remediation;
- verification steps;
- missing information and questions;
- model/tool/config provenance;
- explicit statement of whether private product data was used.

Unsupported claims fail quality validation or are marked unsupported. Citations refer to stored source records or durable URLs, not model-generated link text alone.

### 37.3 Safety boundary

Research access does not imply repository write, store-console access, or publication. Those are new capabilities, approvals, and often separate runs.

## 38. A2A interoperability

### 38.1 Supported role

The platform implements [A2A 1.0](https://github.com/a2aproject/A2A/blob/main/docs/specification.md) as an optional server and client adapter. A2A is for interoperability with opaque remote agents; it is not the internal engine, event journal, checkpoint, workspace, or billing protocol.

For each published A2A hostname/interface, the adapter implements the 1.0 HTTP binding:

~~~text
GET    /.well-known/agent-card.json
POST   /message:send
POST   /message:stream
GET    /tasks/{id}
GET    /tasks
POST   /tasks/{id}:cancel
POST   /tasks/{id}:subscribe
POST   /tasks/{id}/pushNotificationConfigs
GET    /tasks/{id}/pushNotificationConfigs
GET    /tasks/{id}/pushNotificationConfigs/{configId}
DELETE /tasks/{id}/pushNotificationConfigs/{configId}
GET    /extendedAgentCard
~~~

The Agent Card declares the actual interface base URL, protocol binding, and version. Multi-agent hosting uses separate hostnames or explicitly published interface/card URLs; it does not invent a non-standard tenant path and expect generic clients to discover it.

### 38.2 Inbound mapping

| A2A concept | Canonical mapping |
|---|---|
| Agent Card | published projection of an allowed AgentRevision and endpoint policy |
| Context | Session correlation |
| Task | root Run projection |
| Message | Message with A2A source metadata |
| Part | typed content item or artifact reference |
| Artifact | Artifact projection |
| status update | selected run/session events |
| push notification | signed outbound webhook endpoint |
| cancel | cancel Command |

The adapter stores A2A task/context IDs as external references and never replaces canonical IDs.

### 38.3 Agent Cards

Agent Cards:

- expose only intentionally published profiles/skills;
- declare exact supported A2A version, bindings, modalities, and auth;
- avoid provider model names, internal tools, private capabilities, or tenant inventory;
- are revisioned and cacheable;
- may require authenticated extended cards for sensitive capabilities;
- are signed with JWS/JCS when trust policy requires and support signing-key rotation;
- map to immutable agent revisions for each accepted task.

### 38.4 A2A task behavior

- send-message may return a direct Message only for a truly completed non-durable response; otherwise it returns a Task;
- streaming maps canonical ordered events into A2A status/artifact updates;
- input-required maps waiting_for_input;
- auth-required is handled by the adapter without leaking secret handles;
- task cancellation maps to platform cancellation and reports non-cancelable uncertain side effects;
- retained platform detail may exceed A2A projection and is available only through canonical authorized APIs.

### 38.5 Outbound remote agent

A remote A2A agent can be registered as:

- an external child-run executor; or
- a tool-like remote specialist.

Registration pins Agent Card identity/version, endpoint, authentication Connection, allowed modalities, data policy, cost policy, and timeout. Remote output is untrusted. It receives the minimum context/artifacts and cannot inherit parent credentials.

### 38.6 A2A security

- card and endpoint retrieval use SSRF protections;
- redirects and endpoint changes require revalidation;
- OAuth audience and scopes are bound to the remote agent;
- pushed files are ingested/scanned as artifacts;
- extension URIs require allowlisting;
- remote task IDs are scoped by connection;
- push callbacks use the platform webhook security model;
- A2A metadata cannot override organization/project identity.

### 38.7 Versioning

A2A protocol revision is negotiated independently of platform API revision. Unsupported versions fail explicitly. A future A2A release is added behind a tested adapter; it does not force a platform major-version change unless canonical public behavior also breaks.

## 39. Tenancy and resource hierarchy

### 39.1 Hierarchy

~~~text
Deployment
└── Organization
    ├── Memberships / groups / service accounts
    ├── billing account and organization policies
    ├── organization-scoped connections and runner pools
    └── Project
        ├── project policy, quotas, budgets, and API keys
        ├── models, tools, agents, environments, repositories
        ├── sessions, runs, workspaces, and artifacts
        └── integrations, schedules, usage, audit, and evals
~~~

Project is the default data and execution isolation boundary. Cross-project access is denied unless an organization-level resource has an explicit project grant.

### 39.2 Tenant invariants

- Every durable object has an organization and project scope unless explicitly deployment-scoped.
- Tenant scope comes from verified identity and route context, not request-body fields.
- Database queries include scope and use row-level controls as defense in depth.
- Object keys, cache keys, search documents, usage subjects, runner leases, and event channels include an opaque tenant partition.
- A tenant cannot select another tenant's runner, connection, route revision, checkpoint, artifact, or idempotency record by guessing an ID.
- Authorization occurs before existence disclosure; forbidden cross-tenant IDs normally return not_found.
- Support tooling cannot bypass tenant scope without a time-bound audited support grant.

### 39.3 Deployment models

The product supports:

- pooled: shared control plane and compute pools with logical and microVM isolation;
- bridge: shared control plane with dedicated runner pool, network, key, or region;
- silo: dedicated data plane or full deployment.

The public API is identical. Deployment choice changes placement, keys, capacity, SLO, upgrade control, and price—not session semantics.

### 39.4 Database isolation

PostgreSQL roles are separated for migration, application, read-only analytics, and backup. Application transactions set verified tenant context and query tenant-keyed tables. [PostgreSQL row security](https://www.postgresql.org/docs/current/ddl-rowsecurity.html) supplies defense in depth; table owners and privileged migration roles are not used for ordinary requests.

Tests attempt cross-tenant access through:

- direct resource ID;
- list filters and pagination;
- event cursors;
- idempotency keys;
- artifact URLs;
- search indexes;
- usage reports;
- support tooling;
- runner reconnect/replay.

### 39.5 Object storage isolation

- object paths use non-user-controlled organization/project prefixes and random object IDs;
- bucket policy or per-tenant credentials restrict access;
- signed URLs are short-lived, action-specific, checksum/size-bound where supported, and cannot list a prefix;
- managed high-assurance tiers use dedicated buckets/accounts/keys when required;
- object metadata does not contain secrets or unredacted user text;
- deletion and inventory jobs verify orphaned/cross-prefix objects.

### 39.6 Cache and derived-store isolation

Caches use tenant-scoped keys and never cache authorization decisions without principal, policy revision, resource revision, and expiry. Search and analytics ingestion carry immutable tenant scope; rebuilding from canonical storage preserves it. A derived store is excluded from release until cross-tenant conformance tests pass.

## 40. Identity, authorization, and support access

### 40.1 Human identity

Supported identity:

- local bootstrap account for development and initial self-host setup;
- OIDC for normal user sign-in;
- SAML through an identity broker or enterprise module;
- SCIM for enterprise provisioning/deprovisioning;
- identity-provider MFA and conditional access;
- recovery codes/admin recovery under explicit deployment policy.

The platform stores a stable internal user ID and linked external subject/issuer pairs. Email is not identity and an email change does not create a new principal.

### 40.2 Machine identity

Machine actors use:

- project API key;
- service account with short-lived token;
- OAuth client credentials where configured;
- runner/capability-worker workload certificate;
- integration installation identity;
- webhook source identity.

Every credential has owner, scope, roles, created/last-used time, expiry, status, and rotation lineage.

### 40.3 API keys

- Keys have public prefix plus at least 256 bits of random secret.
- Only a memory-hard or keyed cryptographic verifier is stored; the full key is shown once.
- Keys are project-scoped by default.
- Optional IP/network and endpoint restrictions are enforced.
- Expiry is required for managed SaaS service keys unless an enterprise policy explicitly allows otherwise.
- Rotation supports overlapping old/new keys and last-used visibility.
- Revocation propagates immediately to new requests.
- Keys are never accepted in query strings.

### 40.4 Roles

Baseline roles:

| Role | Scope |
|---|---|
| organization_owner | organization lifecycle and all governance |
| organization_admin | membership, projects, shared configuration |
| security_admin | policy, connections, audit, extension trust |
| billing_admin | plan, invoices, budgets, usage |
| project_admin | project resources and membership |
| developer | agents/tools/environments and development runs |
| operator | runners, runs, retries, schedules, incidents |
| agent_user | create/attach to allowed sessions and runs |
| approver | approve explicitly assigned capability classes |
| viewer | read permitted metadata/results |

Roles are templates over granular permissions. Sensitive separation of duties can prevent the same principal from creating and approving a publication policy.

### 40.5 Relationship authorization

Authorization also considers:

- session participant;
- run creator;
- artifact visibility;
- repository binding grant;
- connection use grant;
- assigned approver group;
- integration source actor;
- support grant.

The built-in evaluator stores these relationships in PostgreSQL. An OpenFGA-style external adapter MAY be used at scale, but authorization semantics and audit remain platform-owned.

### 40.6 End-user delegation for embedded products

An embedding backend can exchange its authenticated end-user identity for a short-lived platform token containing:

- issuer and external subject;
- target project/session;
- allowed operations;
- optional agent/tool/model constraints;
- expiry and nonce.

The exchange requires the backend's service identity and a signed subject assertion. A client cannot assert an arbitrary end_user body field and gain access. Resulting actions audit both embedding application and end user.

### 40.7 Session participants

Sessions have explicit participant grants: owner, contributor, observer, approver. A public share link is disabled by default and, when enabled, is a short-lived revocable participant token with no inherited project browse permission.

### 40.8 Support access

Managed support follows just-in-time access:

1. authorized support operator requests tenant, scope, reason, ticket, and duration;
2. policy requires customer approval for content access unless an emergency contract applies;
3. a separate system issues a narrow grant;
4. UI/API displays support mode;
5. all reads/actions are immutable audit events;
6. grant expires automatically;
7. exports and secret reads remain forbidden.

Break-glass use pages security, requires post-incident review, and cannot be hidden from tenant audit.

### 40.9 OAuth security

OAuth/OIDC implementations follow [RFC 9700 OAuth 2.0 Security Best Current Practice](https://www.rfc-editor.org/rfc/rfc9700.html):

- authorization code with PKCE;
- exact redirect URI matching;
- state/nonce validation;
- no implicit flow;
- audience-bound tokens;
- sender-constrained tokens where supported;
- refresh-token rotation for public clients;
- no token forwarding between unrelated resources.

## 41. Secrets and connections

### 41.1 SecretRef

A SecretRef is metadata, not a readable secret:

- ID, scope, type, backend, and backend locator;
- owner and permitted connections/tools;
- created/rotated/expires timestamps;
- current version identifier;
- status and last-used metadata;
- classification and region.

The ordinary GET API never returns secret value. Creation accepts a write-only value or external backend locator.

### 41.2 Secret backends

The baseline self-host backend uses envelope encryption:

- random per-secret data encryption key;
- authenticated encryption for value and sensitive metadata;
- data key wrapped by deployment master key/KMS;
- versioned key ID;
- rotation without rewriting unrelated application state.

Adapters support Vault and cloud secret managers. [Vault response wrapping](https://developer.hashicorp.com/vault/docs/concepts/response-wrapping) is a supported pattern for single-use handoff. No external secret manager is mandatory.

### 41.3 Managed-service keys

Managed SaaS separates:

- platform root/key-encryption keys;
- regional data keys;
- tenant/project keys where tier requires;
- signing keys;
- runner workload identity;
- billing and support systems.

KMS access is service-identity and region restricted. Key use is audited. Customer-managed keys MAY wrap tenant data keys; key disablement makes affected content unavailable and is surfaced as a distinct state.

### 41.4 Secret delivery

1. Tool/model/repository operation is authorized.
2. Broker creates a one-operation lease bound to tool call, executor audience, run/attempt fence, and expiry.
3. Executor authenticates and redeems the handle.
4. Broker obtains or mints the minimum credential.
5. Credential is injected through a non-logged channel such as an in-memory credential helper.
6. Operation completes.
7. Lease is consumed/revoked and ephemeral material destroyed.

The engine and model see only connection/tool availability and sanitized success/failure.

### 41.5 Secret constraints

- No secret in prompts, events, ConfigSnapshot, checkpoints, artifacts, command lines, process listings, Git remotes, or telemetry.
- Environment variable delivery is avoided; when a third-party CLI requires it, the variable exists only for the child process and is redacted.
- Secret files use memory-backed mounts with strict permissions and are excluded from snapshot.
- Copy/paste and artifact scanners detect common credential patterns.
- Logs undergo exact-value and pattern-based redaction, but redaction is defense in depth rather than permission to log secrets.
- Tools cannot enumerate secret names unless granted.

### 41.6 Rotation

Connections reference a logical secret while each operation records the secret version used without revealing it. Rotation:

- validates new credentials before activation where possible;
- atomically moves the active pointer;
- allows bounded overlap;
- revokes the old version after drain;
- does not mutate historical ConfigSnapshots;
- alerts on continued old-version use.

### 41.7 User authorization connections

For services requiring user OAuth:

- consent is initiated through a typed waiting_for_input request;
- PKCE/state and exact redirect handling occur outside the sandbox;
- refresh tokens live only in the secret backend;
- tool execution receives an audience/scoped access token;
- scope escalation requires new consent;
- disconnect revokes when supported and disables the Connection;
- the model receives no authorization callback URL containing a code.

### 41.8 Secret incident response

Suspected exposure can:

- revoke affected secret versions and capability leases;
- search redacted fingerprints across permitted logs/artifacts without exposing the value;
- quarantine artifacts and runs;
- rotate dependent connections;
- notify affected tenants;
- preserve encrypted forensic evidence under incident policy.

## 42. Data classification, privacy, and retention

### 42.1 Data classes

Every resource/content item is classified:

- public;
- internal;
- confidential;
- restricted;
- secret.

Classification can be inherited from project/repository/source and raised by scanners or users. It cannot be lowered automatically by a model. Policy uses classification for model/provider eligibility, logging, support access, egress, retention, and region.

### 42.2 Data categories

The inventory distinguishes:

- customer content: prompts, messages, files, repositories, outputs, checkpoints;
- customer configuration: agents, policies, tool definitions, connections;
- credentials/secrets;
- operational metadata: IDs, timestamps, status, resource usage;
- security/audit data;
- billing records;
- product analytics and diagnostics.

Terms, export, deletion, provider disclosure, and retention are documented separately per category.

### 42.3 Retention profiles

A RetentionPolicy defines content windows. Managed defaults:

| Data | Default |
|---|---|
| store:false response content after terminal | purge within 24 hours; shorter configurable |
| durable sessions/messages/canonical events | 30 days after last activity |
| transient streaming deltas after canonical item | 7 days |
| artifacts/workspace snapshots/checkpoints | 30 days after session terminal/close |
| inbound raw webhook payload | 7 days |
| webhook attempt diagnostics | 30 days |
| audit/security events | 365 days |
| itemized usage | 13 months |
| invoice/tax records | statutory period, content-free |
| deleted data in backups | expires with backup rotation, target 35 days |

These are product defaults, not universal legal requirements. Organization policy may shorten them, or extend them within plan/compliance bounds. The actual policy is pinned to each run/session and discoverable.

### 42.4 store:false

store:false means:

- content is retained only for the active operation, delivery, abuse/security handling required by deployment policy, and a published short operational TTL;
- it is not available for normal response retrieval or continuation after expiry;
- no durable cross-request memory is created;
- content-free usage, request ID, policy decision, and security metadata may remain;
- provider-side handling still follows the selected Connection's declared policy and is disclosed separately.

Background and tool-using execution can still use store:false until terminal. A caller requiring immediate zero persistence must use an eligible direct synchronous mode with tools/workspaces disabled and an advertised ephemeral-processing capability.

### 42.5 Deletion

Deletion is asynchronous and produces a DeletionJob:

1. authorize and tombstone resource;
2. block new use/share;
3. cancel or detach active work according to policy;
4. delete primary content and object-store bytes;
5. propagate to search/cache/analytics;
6. schedule backup expiry;
7. retain only a content-free deletion receipt and legally required records;
8. report partial failures and retry.

Legal hold prevents destructive steps and is visible to authorized compliance roles. Ordinary deletion cannot silently bypass hold.

### 42.6 Provider and integration deletion

The platform records external destinations and provider policy but cannot claim deletion from a provider that offers no delete/zero-retention contract. Where APIs exist, a deletion adapter attempts them and stores receipt. Otherwise the deletion result explicitly states the external retention boundary.

### 42.7 Export

Authorized export supports:

- sessions/messages/config revisions/events;
- artifacts and checksums;
- agents/tools/policies;
- audit records;
- usage and billing allocation;
- repository/source mappings;
- deletion/hold metadata.

Export uses a versioned manifest, JSONL/JSON plus original artifact formats, checksums, and signed provenance. Secret values and provider credentials are never exported through the ordinary data export.

### 42.8 Data residency

Each project has a home region. In-region:

- primary database partition;
- object storage;
- logs/traces containing customer content;
- model/provider route eligibility;
- runner placement;
- secret backend.

Global routing may see content-free tenant/health metadata. Cross-region processing requires explicit policy and is recorded. Failover to a disallowed region fails rather than silently moving data.

### 42.9 Encryption

- TLS 1.2+ with modern policy in transit; TLS 1.3 preferred.
- Database, object, snapshot, backup, and local runner storage encrypted at rest.
- Secret values and restricted checkpoint/artifact content have application-layer envelope encryption where required.
- Keys are versioned and rotated.
- Checksums provide integrity; encryption alone is not accepted as provenance.

### 42.10 Product analytics

Self-host analytics are off by default and opt-in. Managed product analytics:

- exclude prompt/output/repository content by default;
- use pseudonymous tenant/user identifiers;
- document fields and retention;
- honor organization opt-out where contract permits;
- remain separate from security/billing records;
- never train models on customer content without separate explicit opt-in.

### 42.11 Privacy controls

The platform provides primitives for:

- data subject lookup through verified external identity mappings;
- export and deletion;
- retention shortening;
- region selection;
- provider allow/deny;
- support-access approval;
- audit export;
- legal hold.

These primitives support customer compliance programs; the open-source project does not claim that installation alone creates compliance.

## 43. Usage metering, billing, budgets, and quotas

### 43.1 Canonical usage ledger

The platform owns an append-only UsageEvent ledger. Provider dashboards, LiteLLM, Stripe, OpenMeter, and cloud invoices are reconciliation inputs or outputs, not the source of truth.

Each event contains:

~~~json
{
  "id": "use_...",
  "subject": "project:prj_...",
  "organization_id": "org_...",
  "project_id": "prj_...",
  "session_id": "ses_...",
  "run_id": "run_...",
  "attempt_id": "att_...",
  "operation_id": "mstep_...",
  "meter": "model.output_tokens",
  "quantity": 812,
  "unit": "token",
  "price_revision_id": "price_...",
  "state": "settled",
  "occurred_at": "2026-07-16T12:00:00Z",
  "dedupe_key": "..."
}
~~~

Event IDs/dedupe keys are deterministic per provider/tool/runner attempt dimension. Adjustments append compensating entries; settled events are not edited.

### 43.2 Meter dimensions

The ledger supports:

- model input, output, cached input, cache write, reasoning, image, audio, and provider-specific units;
- model request and fallback attempt;
- sandbox vCPU-second, memory GiB-second, accelerator/device time;
- active and retained workspace/snapshot/artifact GiB-time;
- network egress;
- hosted tool call or external paid API unit;
- capability-worker minute/device minute;
- webhook/integration delivery where plan meters it;
- seats and dedicated capacity.

Meters use normalized units while preserving provider raw usage.

### 43.3 Meter lifecycle

~~~text
estimated → reserved → provisional → settled
                               └→ disputed → adjusted
~~~

- Estimated usage supports admission.
- Reservation prevents concurrent overspend.
- Provisional usage appears during streaming/long runs.
- Settlement uses authoritative provider/runner receipt or conservative policy.
- Adjustment has reason, evidence, and actor.

### 43.4 Price revisions

Prices are immutable revisions with:

- meter and unit;
- currency;
- provider/model/region/tier predicates;
- effective interval;
- included allowance behavior;
- markup/discount;
- tax handling boundary;
- source and verification time.

A run pins the effective pricing revision for disclosed estimates. Managed terms define how provider price changes during very long runs are handled.

### 43.5 Bring-your-own-key

BYOK runs still meter platform compute/storage/tools. Provider token usage is recorded for observability/budget when available but is not charged as platform-managed model spend unless commercial terms say so.

Usage from opaque direct engine egress cannot be trusted; such runs are labeled and excluded from strong cost enforcement.

### 43.6 Budget hierarchy

Budgets can exist at:

- organization billing period;
- project billing period;
- agent revision;
- trigger/schedule;
- session;
- run;
- child run;
- model/tool operation.

Child budgets are reservations within the parent, not new money. Effective hard limit is the smallest remaining applicable limit.

### 43.7 Budget behavior

- soft threshold emits warning/notification;
- hard threshold denies new reservations;
- an in-flight operation is canceled only if its semantics and provider support allow;
- final small overage from estimate variance is recorded;
- irreversible external operation requires full worst-case reservation;
- budget change is audited and affects future reservations, not already settled usage.

### 43.8 Quotas

Quotas limit non-price resources:

- API requests/rate;
- queued and concurrent runs;
- child depth/fan-out;
- workspace size/count;
- artifact/checkpoint storage;
- tool calls;
- model tokens per period;
- runner/capability capacity;
- schedules/triggers/webhooks;
- members/projects/connections.

Quota errors identify dimension, scope, current/limit, reset or remediation without leaking other tenants.

### 43.9 Fairness

Managed pooled capacity uses tenant-weighted fair queuing with:

- per-plan concurrency;
- aging to prevent starvation;
- trusted priority classes;
- per-tenant burst limits;
- separate interactive and background pools;
- dedicated-capacity reservations where purchased.

One project cannot fill every global queue slot with cheap pending work.

### 43.10 Billing integration

Adapters may export UsageEvents to [OpenMeter](https://openmeter.io/docs/metering/events/overview) and settled aggregate quantities to [Stripe meters](https://docs.stripe.com/billing/subscriptions/usage-based/how-it-works).

Requirements:

- deterministic external event ID;
- replayable export cursor;
- currency/quantity reconciliation;
- dead-letter and alerting;
- no customer content;
- invoice line trace back to aggregate ledger;
- correction via adjustment, not duplicate overwrite.

### 43.11 Credits and prepaid limits

Credit grants have amount/unit, applicable meters, priority, expiry, and source. Consumption is deterministic. Expired credit never deletes usage. Prepaid exhaustion is a budget denial, not an authentication failure.

### 43.12 Billing visibility

Users with permission can view:

- live run estimate/reservation/settlement;
- model/tool/sandbox breakdown;
- child-run allocation;
- fallback/retry cost;
- cached-token savings;
- project/label/agent/trigger allocation;
- price revision and BYOK distinction;
- adjustment history.

Model-generated cost claims are never authoritative.

## 44. Packaging and local development

### 44.1 Released artifacts

Every stable release publishes:

- signed multi-architecture control-plane OCI image;
- signed reference-engine OCI image;
- runner daemon binaries and packages for supported Linux architectures;
- CLI binaries/packages;
- TypeScript, Python, and Go SDK packages;
- Docker Compose local/self-host bundle;
- Helm chart for Kubernetes;
- OpenAPI, AsyncAPI, engine/runner protocol schemas;
- database migration bundle;
- default policy and conformance fixtures;
- SBOM, provenance, checksums, release notes, and upgrade guide;
- optional air-gap bundle manifest.

Source tags and all artifacts point to one release commit. A release is incomplete if only container tags, rather than immutable digests and checksums, are available.

### 44.2 Local quick start

The supported path is:

~~~text
platform init
platform local up
platform provider add
platform doctor
platform response create --input "hello"
~~~

local up starts:

- control plane;
- PostgreSQL;
- S3-compatible development object storage;
- a local runner;
- reference engine;
- optional web console.

Docker Engine and compatible Podman configurations are supported where conformance passes. The CLI generates local TLS/auth, random passwords, persistent volumes, and a development project. It never silently reuses unrelated cloud/provider credentials.

### 44.3 Local guarantees

Local mode supports the full core semantics:

- Responses, Sessions, Agents, Runs, SSE;
- model/provider configuration;
- tools, MCP, skills, approvals;
- repository clone/workspace/snapshot;
- queue, cancel, checkpoint, resume;
- schedule/webhook receiver;
- SDK conformance;
- usage and audit.

Local mode may have only development isolation, one runner, no HA, and no managed billing. The UI and discovery clearly display these limits.

### 44.4 Local data

- All persistent paths live under one user-selected project data directory or named container volumes.
- uninstall does not delete data by default;
- reset requires confirmation and produces a backup option;
- backup/export works before destructive reset;
- file permissions prevent other local users where the OS permits;
- local credentials can use the OS keychain.

### 44.5 Development bind mode

An explicit unsafe development option can mount a local repository directly. It:

- is off by default;
- is unavailable in managed SaaS;
- disables claims of filesystem isolation and reliable workspace snapshot;
- shows the exact host path;
- warns before model-initiated writes;
- never mounts the repository's parent or user home implicitly.

## 45. Self-hosted deployment

### 45.1 Supported topologies

| Topology | Intended use |
|---|---|
| single-node Compose | evaluation, team development, low-volume internal service |
| split VM | control plane/database/object storage separated from one or more runner VMs |
| Kubernetes | HA control plane and elastic runner pools |
| hybrid | self-host control plane with customer/private/specialized runner pools |
| air-gapped | offline registry, private model endpoints, no required telemetry |

Single-node and Kubernetes expose the same public API.

### 45.2 Single-node production boundary

A single-node installation can be production for a trusted internal tenant if:

- external TLS/reverse proxy is configured;
- persistent PostgreSQL/object storage are backed up;
- runner capacity and isolation match workload;
- monitoring and disk alerts are active;
- secrets use a non-development master key;
- public registration is disabled;
- restore is tested.

It cannot claim HA or hostile multi-tenant isolation merely because it uses containers.

### 45.3 Kubernetes deployment

The Helm chart separates:

- API/control-plane deployment;
- coordinator workers;
- optional integration workers;
- migrations job;
- runner gateway;
- web console;
- configured runner pools.

It supports external PostgreSQL/object storage/secret manager, NetworkPolicy, Pod Security restricted profile, RuntimeClass, topology spread, disruption budgets, resource requests, and ingress TLS. The chart does not require cluster-admin after installation.

### 45.4 Runner installation on a Linux VM

This is the standard way another product uses its own Linux VM with the platform:

1. Install the signed runner package on the VM.
2. Install/configure an approved sandbox driver such as rootless OCI, gVisor, Kata, or microVM runtime.
3. Create a runner pool and one-time enrollment token in the target project/organization.
4. Run platform runner enroll with SaaS/self-host control-plane URL, token, pool, labels, capacity, and isolation tier.
5. The runner generates its key, consumes the token, and obtains short-lived workload identity.
6. The runner opens only an outbound TLS connection.
7. Control-plane placement sends fenced work leases.
8. The VM creates the sandbox, prepares the repository, supervises the engine, and uploads events/snapshots/artifacts.
9. Model, Git, and integration credentials are delivered only as scoped operation leases.

No public inbound port, shared database credential, or permanent cloud API key is required on the VM.

### 45.5 Customer-cloud runner with managed SaaS

The SaaS control plane can schedule into a customer-owned runner pool:

- customer data/repository may remain in the selected network/region;
- control events and chosen artifacts still cross to the SaaS according to policy;
- route policy can require customer model endpoints;
- outbound domains are allowlisted;
- runner identity and version health are visible;
- customer controls drain/revoke;
- commercial terms distinguish SaaS control usage from customer compute.

“Data stays in customer cloud” may be claimed only if the configured transcript, artifact, model, telemetry, and backup paths actually remain there; runner location alone is insufficient.

### 45.6 Configuration

Deployment config is a versioned YAML/JSON schema:

- network/listen/public URLs;
- database and object-store references;
- identity providers;
- encryption/KMS;
- default regions/retention;
- runner gateway and trust;
- enabled adapters/features;
- limits and operational settings;
- telemetry destinations.

Sensitive values use secret references. Environment-variable overrides are documented and namespaced. Unknown keys fail validation. platform config validate and doctor run without mutating state.

### 45.7 Bootstrap

First startup:

- verifies database/object storage;
- creates or validates encryption root;
- runs only safe pending migrations under lock;
- creates a one-time local setup URL/token;
- requires creation of the first organization owner;
- invalidates bootstrap token;
- records bootstrap audit.

A deployment never ships with a default admin password.

### 45.8 External services

Self-host operators may replace:

- PostgreSQL with a compatible managed service;
- object storage with S3-compatible/cloud service;
- built-in identity with OIDC/SAML;
- secret backend with Vault/cloud manager;
- observability exporters;
- billing adapter;
- model gateway;
- sandbox scheduler.

Replacement cannot remove canonical product state or bypass conformance without the deployment advertising a non-conforming mode.

### 45.9 Air-gapped installation

An air-gap bundle contains a signed manifest of:

- OCI image digests and platform variants;
- runner/CLI binaries;
- charts/compose files;
- schema/migrations;
- SBOM/provenance/signatures;
- approved built-in skills/tools.

Operators mirror it into an offline registry and verify offline trust roots. The installation supports private/local model endpoints and internal Git/MCP services. License validation for open-core functionality never requires an internet heartbeat.

### 45.10 Telemetry

- Self-host telemetry is opt-in.
- Required license/entitlement checks, if any, are content-free and documented.
- No prompt, output, repository path/content, secret, or user email is sent by default.
- Operators can inspect a telemetry preview and route telemetry to their own collector.
- Disabling telemetry does not disable functional open-core behavior.

## 46. Managed SaaS architecture

### 46.1 Cell architecture

Managed service uses regional cells:

~~~text
Global edge and tenant directory
          |
     home-region cell
     ├── API/coordinator instances
     ├── tenant-partitioned PostgreSQL
     ├── regional object storage/KMS
     ├── model/tool brokers
     ├── runner gateway
     └── isolated runner pools
~~~

A cell failure limits blast radius. Tenant content routes to its home cell. Global systems hold only the minimum directory, entitlement, and health metadata needed to route.

### 46.2 Control and data planes

Control plane:

- identity, policy, configuration, session/run state, audit, usage;
- model/tool/capability brokers;
- scheduling and integrations.

Execution data plane:

- runner hosts;
- sandboxes/microVMs;
- workspace/snapshot transfer;
- specialized workers.

The control plane cannot enter a sandbox as root through ordinary support tools. Execution hosts do not have broad database access.

### 46.3 Tenant placement

Organizations select available home region. Enterprise options can provide:

- dedicated runner pool;
- dedicated keys;
- dedicated database/object partition;
- dedicated cell/cluster;
- customer cloud runners;
- private connectivity;
- controlled upgrade window.

The selected isolation mode is visible in contract/discovery and audit.

### 46.4 Capacity pools

Separate pools exist for:

- interactive low-latency sessions;
- background/batch tasks;
- hardened versus microVM isolation;
- CPU/memory sizes;
- GPU/device classes;
- region and compliance boundary;
- warm environment revisions;
- dedicated tenants.

Admission never places a hard-isolation request into a weaker pool to reduce queue time.

### 46.5 Cold start

Cold-start budget is measured as:

- queue wait;
- runner assignment;
- sandbox/microVM boot;
- image pull;
- workspace restore/clone;
- engine handshake;
- first model request.

Warm pools, pre-pulled signed images, incremental snapshots, and repository caches optimize individual phases. Metrics identify which phase regressed.

### 46.6 Repository cache

Managed runner pools MAY cache public or tenant-scoped Git objects:

- cache identity includes immutable repository and tenant scope;
- credentials/remotes/worktrees are not cached;
- object integrity is verified;
- private objects never cross tenant;
- cache is an optimization and failure falls back to clean fetch;
- purge follows repository disconnect/deletion policy.

### 46.7 SaaS organization lifecycle

~~~text
trial → active → past_due → restricted → suspended → closing → deleted
~~~

- past_due may block new costly runs but preserves retrieval/export for a grace period;
- suspension fences execution and revokes API keys/runners according to reason;
- security suspension is distinct from billing restriction;
- closing offers export and deletes according to retention/contract;
- organization ID is never reassigned.

### 46.8 Abuse controls

Managed SaaS includes:

- signup/payment risk checks;
- API and compute rate limits;
- outbound network and port abuse prevention;
- malware, cryptomining, credential theft, spam, phishing, and exploit detection;
- model/tool abuse policy;
- quarantine and appeal workflow;
- tenant notification where safe;
- content-minimizing detection and role-separated review.

Abuse systems cannot silently grant support access to customer content. Automated suspension is reason-coded and auditable.

### 46.9 Commercial feature boundary

Managed/enterprise features can include:

- hosted operations and SLO;
- elastic microVM pools;
- global regions;
- enterprise SSO/SCIM;
- advanced governance, audit export, support access controls;
- dedicated capacity/connectivity/keys;
- managed billing and marketplace;
- advanced abuse/security operations.

Core Responses/Sessions/Agents, model/tool adapters, local runner, repository work, basic auth/policy, events, checkpoints, and self-host operation remain functional open core.

## 47. Web console and operator experience

### 47.1 Developer console

The console provides:

- organizations/projects/API keys;
- model connections/routes and capability probes;
- agent/profile revision editor and diff;
- tool/MCP/skill/environment configuration;
- repository connections;
- live sessions with chat attach;
- run timeline and event stream;
- tool arguments/results and approvals;
- workspace files/diff/terminal/preview;
- artifacts and structured outputs;
- schedules/triggers/webhook deliveries;
- usage, budget, quota, and evals.

Every UI action uses public APIs or explicitly documented admin APIs.

### 47.2 Live session view

The view separates:

- canonical user/assistant messages;
- progress updates;
- model steps and selected model;
- tool and subagent activity;
- approvals/input requests;
- files/changeset/tests;
- usage/cost;
- recovery/attempt transitions;
- terminal result.

Hidden chain-of-thought is never required or displayed. Provider reasoning summaries are shown only when policy/provider permit and are labeled.

### 47.3 Configuration explainability

For model, tool, network, approval, runner placement, and denial, the console can display:

- requested value;
- effective value;
- contributing config layers;
- controlling policy revision;
- decision ID;
- remediation available to the current role.

Secret values and sensitive policy internals remain hidden.

### 47.4 Operator console

Operators can:

- view system/cell/pool health;
- cordon/drain runners;
- inspect queue age/capacity;
- reconcile lost attempts and uncertain tools;
- replay dead outbox/webhook deliveries;
- manage migrations/backups;
- view extension/provider circuits;
- initiate scoped support access;
- manage incidents and maintenance.

Operator actions are audited and cannot edit customer messages or mark a failed tool successful without a reconciliation record.

### 47.5 Accessibility and localization

- Keyboard navigation and screen-reader semantics are release requirements.
- Status is never color-only.
- Streaming updates respect reduced motion and announcement controls.
- Timestamps show timezone and exact UTC on demand.
- User-facing product text is localization-ready; identifiers, tool names, code, and protocol values are not translated.

### 47.6 API-first requirement

No core workflow requires clicking the console. Anything needed to configure, run, approve, inspect, export, or operate the platform has an API and CLI path, except identity-provider/browser consent steps that intrinsically require a user agent.

## 48. Upgrades, migrations, and compatibility

### 48.1 Independent versions

The release records:

- product/server semantic version;
- dated public API revision;
- database schema version;
- runner protocol version;
- engine protocol version;
- reference engine version/digest;
- SDK versions;
- extension manifest version;
- policy schema version.

These values are visible through health/capabilities and support bundles.

### 48.2 Compatibility window

- A stable control plane supports the current and previous two minor runner releases.
- A runner supports the current engine protocol major and advertised minor feature negotiation.
- Retained checkpoints remain restorable for their published retention/compatibility window or are migrated before support removal.
- Official SDKs support every non-sunset API revision.
- Extensions declare minimum/maximum API and protocol versions.
- The upgrade tool refuses an unsupported gap and prints the required intermediate path.

### 48.3 Database migrations

Migrations follow expand/migrate/contract:

1. preflight checks version, disk, locks, extension compatibility, and backup;
2. expand adds backward-compatible schema;
3. old/new application versions can run during rolling deployment;
4. background data migration is resumable and observable;
5. reads switch after verification;
6. contract/destructive removal occurs only in a later release after rollback window.

Migrations are transactional where safe, have bounded locks, and never silently drop customer content.

### 48.4 Upgrade sequence

Reference sequence:

1. back up and run restore verification status check;
2. verify signatures and compatibility;
3. apply expansion migration;
4. upgrade control plane/coordinators;
5. upgrade runner gateways;
6. drain and upgrade runners in batches;
7. roll reference engine for new runs by digest;
8. run smoke/conformance tests;
9. complete background migration;
10. activate new capabilities;
11. contract only after rollback window.

Active runs keep pinned engine/config. Host drain uses checkpoint recovery.

### 48.5 Rollback

- Application rollback is supported while schema remains expanded.
- A new engine activation can be rolled back for new runs by alias/digest pointer.
- Already-migrated checkpoints require declared backward compatibility or remain on the newer engine.
- Destructive contract migration has no automatic rollback and requires verified restore/new forward fix.
- Rollback itself is audited and emits operational events.

### 48.6 Backup before upgrade

The updater requires a recent successful backup or explicit override in development. A backup existence check is not enough: the system tracks last verified restore drill and warns/fails according to environment policy.

### 48.7 Extension migrations

Extensions use namespaced, versioned migrations:

- core migration cannot depend on an optional extension;
- extension failure disables the new extension revision rather than corrupting core startup;
- downgrade compatibility is declared;
- uninstall retains data until explicit purge;
- extension migration code runs with restricted database capability.

### 48.8 Maintenance mode

Maintenance can:

- reject new runs while allowing reads;
- drain active work to checkpoint;
- pause schedules and inbound consumption;
- continue critical callbacks;
- expose Retry-After and status;
- avoid reporting accepted work that cannot be persisted.

Maintenance does not bypass webhook/source deduplication when services resume.

## 49. Security architecture and threat model

### 49.1 Security objectives

The platform must preserve:

- tenant and project isolation;
- confidentiality of customer content and credentials;
- integrity of repository changes, artifacts, policy, and audit;
- availability under runaway or malicious workloads;
- human control over consequential actions;
- traceability from input through model/tool/subagent to side effect;
- recoverability without duplicate external actions;
- supply-chain identity of every executed component.

The security program uses the [OWASP Top 10 for Agentic Applications 2026](https://genai.owasp.org/resource/owasp-top-10-for-agentic-applications-for-2026/) as one risk taxonomy and [NIST AI RMF](https://www.nist.gov/itl/ai-risk-management-framework) for governance/evaluation structure. Neither replaces a product-specific threat model.

### 49.2 Trust boundaries

Explicit boundaries:

1. public caller ↔ API edge;
2. organization/project ↔ another tenant;
3. control plane ↔ execution runner;
4. runner host ↔ sandbox/microVM;
5. engine ↔ model/tool brokers;
6. platform ↔ model provider;
7. platform ↔ tool/MCP/integration;
8. workspace ↔ external repository;
9. parent run ↔ child/remote agent;
10. managed operator ↔ customer tenant;
11. release pipeline ↔ runtime artifact;
12. primary region ↔ backup/failover region.

Authentication and authorization are re-evaluated at each boundary; internal network location is not trust.

### 49.3 Threat actors

- unauthenticated internet attacker;
- malicious or compromised tenant user;
- tenant attempting cross-tenant access;
- malicious repository/website/document author;
- prompt-injected external content;
- compromised MCP/tool/provider/integration;
- malicious skill/plugin/image/package;
- escaped sandbox workload;
- compromised runner/worker;
- overprivileged or compromised operator;
- leaked API/runner/provider credential;
- faulty or adversarial model output;
- accidental administrator/user error;
- supply-chain attacker.

### 49.4 Asset inventory

Critical assets:

- identity and authorization state;
- model/repository/integration secrets;
- customer repository/files/messages/artifacts;
- workspace/checkpoint contents;
- policy/config/agent/tool revisions;
- runner and signing keys;
- audit and usage ledgers;
- publication/signing capabilities;
- billing/entitlement state;
- release artifacts and update channel.

Every asset has owner, classification, storage location, retention, encryption key, allowed processors, and deletion path.

### 49.5 Agentic threat-control matrix

| Threat | Required controls |
|---|---|
| goal/prompt hijack | trust-labeled context, instruction precedence, least capability, output/tool validation, injection evals |
| tool misuse | schema validation, deterministic policy, exact approval, side-effect class, scoped token, audit |
| identity/privilege abuse | short-lived audience-bound identity, no token passthrough, capability intersection, revocation |
| agentic supply chain | digest pinning, signatures, SBOM/provenance, quarantine, allowlists |
| unexpected code execution | hostile-code sandbox, no host sockets, hardened/microVM tier, resource/network controls |
| memory/context poisoning | explicit provenance, immutable sources, constrained memory writes, user inspection/deletion |
| inter-agent trust abuse | child capability subset, minimum context, remote output untrusted, bounded delegation |
| cascading failure/runaway | budgets, depth/fan-out limits, circuit breakers, cancel propagation, admission/fairness |
| human deception | exact operation UI, source/citation labels, no hidden authority, separation of duties |
| data exfiltration | egress policy, secret isolation, DLP/redaction, provider/region policy, artifact authorization |

### 49.6 Prompt injection

Prompt injection is treated as an authorization problem, not solved by a prompt alone.

- External/repository/tool content is explicitly delimited and provenance-tagged.
- Content cannot grant capabilities or change policy.
- Tool execution always passes deterministic validation.
- Retrieved instructions do not enter trusted instruction layers.
- Sensitive data is not placed in context unless necessary and allowed.
- High-risk actions require exact approval independent of model claims.
- Web/browser tools restrict navigation/download/exfiltration.
- Injection regression suites include direct, indirect, encoded, multilingual, and tool-output attacks.
- The final output can cite untrusted text but must not present it as platform policy.

### 49.7 Excessive agency

Every run has:

- finite cost/token/time/tool/subagent/resource budgets;
- explicit capability set;
- network policy;
- repository scope;
- side-effect approval policy;
- cancellation owner;
- terminal/output contract.

“Autonomous” never means unbounded. A model cannot extend its own budget, expiry, delegation depth, capabilities, or approval.

### 49.8 Secret exfiltration

Defense in depth:

- secret not supplied to model/engine;
- JIT executor-only credential;
- destination-scoped token;
- egress allowlist/proxy;
- output redaction and secret scanning;
- artifact classification/quarantine;
- model provider data policy;
- anomaly detection for encoded/high-entropy output;
- revocation and incident search by fingerprint.

DLP cannot guarantee detection of arbitrary transformed data; isolation and least privilege remain primary.

### 49.9 Data poisoning

- Knowledge/memory items retain source and checksum.
- Index ingestion authenticates source and records revision.
- Automated memory writes are reviewable and capability-limited.
- Retrieved sources can be filtered by trust class.
- Conflicting sources remain visible.
- Agent/profile/eval release does not automatically accept model-generated prompt changes.
- Poisoning tests cover repository instructions, issue comments, web pages, tool descriptions, and prior model output.

### 49.10 Confused deputy prevention

The platform never accepts an engine's assertion that a caller is authorized. Tool requests bind:

- original principal/delegation chain;
- run/attempt fence;
- capability;
- destination/resource;
- exact arguments;
- connection;
- approval decision.

MCP and OAuth tokens are audience-bound and not forwarded. Repository and SaaS integration actions use installation/resource identity, not display names.

### 49.11 SSRF and network attacks

All server-side URL retrieval—including webhooks, model endpoints, MCP metadata, A2A cards, artifacts, OAuth metadata, and repository URLs—uses:

- normalized URL parsing;
- allowed schemes;
- DNS and resolved-IP policy;
- private/link-local/metadata/control-plane denial;
- redirect revalidation;
- response size/time limits;
- TLS validation;
- proxy isolation;
- audit destination.

User-controlled Host, forwarding, and callback headers cannot change target policy.

### 49.12 Sandbox escape

Mitigations:

- hardware virtualization for managed hostile multi-tenancy;
- minimal patched host/runtime;
- no privileged mode or host namespaces;
- seccomp/mandatory access control;
- no raw devices/runtime sockets;
- workload network separation;
- short-lived host;
- immutable verified images;
- runtime and kernel vulnerability response;
- escape detection/quarantine;
- separate runner identity from workload.

Suspected escape immediately fences the runner, stops new placement, preserves minimal forensic state, revokes identities, and invokes incident response.

### 49.13 Denial of service and cost exhaustion

- size/depth/schema limits before expensive parsing;
- authentication and rate limit before model/sandbox allocation;
- bounded queues and per-tenant fairness;
- cost reservation;
- sandbox resource limits;
- tool/provider rate/circuit breakers;
- subagent fan-out limits;
- webhook loop detection;
- decompression/archive bombs blocked;
- slow consumer and connection limits;
- per-operation deadlines.

### 49.14 Model output handling

Model output is untrusted:

- HTML/Markdown rendered with sanitization;
- URLs are not auto-fetched;
- code is not executed outside sandbox;
- JSON is schema-validated;
- shell commands are parsed/displayed and policy checked;
- citations are verified against source records;
- file paths are normalized;
- user interfaces distinguish model claims from platform receipts.

### 49.15 Browser and computer use

- browser profile is fresh per tenant/run unless explicit durable profile capability;
- downloads/uploads pass artifact policy;
- clipboard is isolated;
- local network and metadata endpoints denied;
- credential autofill uses approved brokered steps;
- screenshots/video may contain sensitive data and inherit classification;
- destructive clicks/forms/payment/publication require policy/approval;
- visual prompt injection remains untrusted input.

### 49.16 Multi-agent security

- child/remote agent identity is explicit;
- capability never exceeds parent intersection;
- shared workspace write is off by default;
- remote agent cannot request parent secret;
- parent treats child output as untrusted;
- delegation chain and cost remain visible;
- colluding children cannot exceed aggregate limits;
- depth/fan-out are enforced outside model control.

### 49.17 Security defaults

Stable defaults:

- no public registration for self-host;
- no broad internet in high-trust profiles;
- no protected-branch push/merge/release;
- no recursive delegation;
- no arbitrary skill/plugin install;
- no support content access;
- no secret in model context;
- no direct Docker socket in workload;
- no mutable image tags;
- no provider/model fallback across data policy;
- approvals expire;
- audit enabled;
- managed hostile code uses microVM isolation.

Unsafe development overrides are visibly labeled, local-only where possible, and cannot be enabled by a model.

## 50. Audit and security operations

### 50.1 Audit events

Audited actions include:

- sign-in/token/key/service-account lifecycle;
- membership/role/policy changes;
- secret/connection create, rotate, use, revoke;
- agent/tool/skill/plugin/model/environment publication;
- run/session creation, attach, command, approval;
- capability issuance and side effect;
- repository push/PR/merge/release;
- runner enroll/revoke/drain;
- support/break-glass access;
- data export/delete/legal hold;
- billing/entitlement change;
- migration/backup/restore;
- security detection/quarantine.

### 50.2 Audit record

Each record contains:

- immutable ID and time;
- actor and authentication method;
- delegation/support chain;
- organization/project/resource;
- action and outcome;
- request/trace/policy decision IDs;
- source network/device metadata under privacy policy;
- before/after revision IDs, not necessarily content;
- reason/ticket where required;
- integrity linkage.

Secret values, full prompts, and unnecessary personal content are excluded.

### 50.3 Audit integrity

- append-only application permissions;
- monotonically ordered partitions;
- periodic hash-chain/Merkle checkpoints;
- signed export manifests;
- optional WORM/external SIEM export;
- separate operator from audit-deletion authority;
- gap/lag monitoring.

Audit immutability does not mean infinite retention. Expiry follows policy while preserving integrity receipts.

### 50.4 Security detections

Detections include:

- credential misuse/impossible access;
- cross-tenant authorization probes;
- abnormal secret/capability requests;
- suspicious egress/DNS/metadata access;
- malware/cryptomining/exploit behavior;
- prompt-injection success indicators;
- unusual model/tool/subagent cost fan-out;
- sandbox runtime alerts;
- runner identity/attestation change;
- extension signature/digest drift;
- large or encoded data exfiltration patterns;
- audit/telemetry gaps.

Detections produce a reason-coded SecurityFinding with evidence access controls and disposition workflow.

### 50.5 Incident actions

Policy can:

- deny one operation;
- pause/cancel one run;
- quarantine artifact/workspace;
- revoke connection/secret/capability;
- cordon runner/pool;
- suspend project/organization;
- block model/tool/extension revision;
- require reauthentication/approval;
- notify on-call/tenant.

High-impact automated actions have bounded scope and an appeal/manual review path.

### 50.6 Vulnerability handling

The project publishes:

- security policy and private reporting channel;
- supported-version policy;
- severity and response targets;
- coordinated disclosure process;
- signed advisories and patched artifacts;
- dependency/container scan status;
- CVE mapping to affected images/extensions;
- emergency upgrade/mitigation instructions.

### 50.7 Assurance and compliance readiness

Managed-service operations maintain:

- documented control ownership and evidence collection;
- annual independent penetration test plus continuous scoped testing;
- vendor/subprocessor inventory and security review;
- data-processing terms and subprocessor change process;
- security/privacy training and access review;
- business continuity and incident exercises;
- vulnerability disclosure and eventual public bug-bounty path;
- control mapping suitable for SOC 2 and ISO 27001 assessment;
- regional/privacy/legal review for offered markets.

Certification/compliance claims are made only after the relevant independent assessment and scoped report exists. Self-hosted software documentation explains which controls remain the operator's responsibility.

## 51. Software supply chain

### 51.1 Build requirements

- protected source branches and required review;
- isolated ephemeral CI runners for releases;
- pinned dependencies/actions/toolchains;
- no long-lived release secrets;
- reproducible or hermetic build targets where feasible;
- tests and scans before signing;
- source-to-artifact provenance;
- two-person control for release promotion;
- immutable registry/release assets.

### 51.2 SBOM and provenance

Every OCI image and distributed binary/package has:

- SPDX or CycloneDX SBOM;
- cryptographic digest;
- signed provenance identifying source commit and build workflow;
- dependency/vulnerability scan;
- license inventory;
- signature verifiable offline or through configured trust roots.

The project targets [SLSA 1.2](https://slsa.dev/spec/v1.2/) Build Level 2 for initial stable release and Level 3 for managed production artifacts.

### 51.3 Runtime verification

Before execution/promotion:

- digest matches configured revision;
- signature identity/issuer matches policy;
- provenance source/workflow is allowed;
- SBOM exists;
- critical vulnerability policy passes or has an explicit time-bound exception;
- architecture/protocol declaration matches;
- revocation/advisory list is checked.

Mutable tag movement does not alter a pinned run.

### 51.4 Dependencies

- lockfiles are committed;
- automated updates create reviewed pull requests;
- dependency confusion is mitigated by namespace/registry policy;
- package install scripts in builds are isolated;
- abandoned/high-risk dependencies have owners and replacement plans;
- SDK runtime dependencies are minimized;
- license policy is enforced.

### 51.5 Extension marketplace

If a public marketplace is offered:

- publisher identity verification;
- signed immutable releases;
- capability and data-use disclosure;
- automated/manual review tiers;
- malware/secret/vulnerability scan;
- user ratings do not replace security review;
- revocation and emergency disable;
- clear distinction between official, verified, and community;
- self-host import remains possible under administrator policy.

## 52. Observability

### 52.1 Principles

Observability must answer:

- what the caller requested;
- which configuration/policy/model/tool/runner was selected;
- where time and cost went;
- what failed and whether retry is safe;
- what external effects committed;
- how recovery proceeded;
- whether tenant/security boundaries held.

It must do so without default collection of customer content or secrets.

### 52.2 Signals

The platform emits OpenTelemetry-compatible:

- distributed traces;
- metrics;
- structured logs;
- canonical events;
- audit events;
- usage events.

These are related but not interchangeable. Public event journal is not a log backend; audit is not a debug trace.

### 52.3 Trace model

Typical hierarchy:

~~~text
API request / trigger delivery
└── session command / run
    ├── admission and queue
    ├── sandbox provision/restore
    ├── engine attempt
    │   ├── context assembly
    │   ├── model step / provider attempt
    │   ├── tool call / executor attempt
    │   ├── child run link
    │   └── checkpoint/snapshot
    ├── artifact finalize
    └── callback delivery
~~~

Trace links, rather than invalid parentage, connect parallel child runs and asynchronous callbacks. W3C trace context is propagated only to trusted destinations and stripped/rewritten across untrusted boundaries.

### 52.4 GenAI semantic conventions

Where stable, the implementation maps model and agent spans to [OpenTelemetry GenAI semantic conventions](https://opentelemetry.io/docs/specs/semconv/registry/attributes/gen-ai/). Platform-specific attributes remain namespaced. A changing external convention never breaks canonical resource/event schemas.

### 52.5 Content capture

Default production telemetry captures:

- IDs, revisions, sizes, counts, hashes, status, latency, usage, destination class;
- no prompt, output, tool argument/result, file content, secret, or raw provider body.

Debug content capture:

- requires organization policy and authorized role;
- is time-bound and scoped;
- redacts/scans;
- inherits data region/retention/classification;
- shows an audit/console indicator;
- is disabled for secret fields and certain restricted projects.

### 52.6 Core metrics

API:

- request rate, latency, errors by stable code;
- auth/rate/idempotency outcomes;
- SSE connections/reconnects/lag.

Runs:

- admitted/queued/active/terminal;
- queue and end-to-end latency;
- attempts/recoveries/cancellations;
- waiting reasons;
- budget/timeout/resource termination.

Models:

- request/first-token/total latency;
- tokens/usage/cost/cache;
- provider errors/retries/fallbacks/circuits;
- route selection and capability mismatch.

Tools:

- call latency/outcome/replay class;
- approvals/wait time;
- uncertain/reconciliation;
- executor health and output limit.

Sandboxes:

- provision/restore/snapshot/destroy;
- cold-start phases;
- resource/egress;
- escapes/policy denials/residual cleanup.

Coordination:

- job queue depth/age;
- lease expiry/fencing rejects;
- outbox/inbox lag;
- timer/schedule lag;
- webhook/queue dead letters.

Storage:

- database transactions/locks/replication;
- object errors/bytes/orphans;
- backup/PITR/restore verification.

### 52.7 Logs

Structured logs include severity, event name, request/trace/resource IDs, component, deployment/cell, stable error code, and redacted fields. They do not embed multiline provider dumps by default.

Log cardinality and label size are bounded. High-cardinality resource IDs belong in traces/logs, not unbounded metric labels.

### 52.8 Customer observability

Projects can:

- query canonical run/event/usage data;
- configure OpenTelemetry export of their own scoped telemetry;
- receive signed webhooks;
- set alerts for failures, budgets, queue age, policy denial, runner health;
- export audit.

Export credentials are tenant-scoped. A customer exporter cannot receive another tenant's data.

### 52.9 Operational dashboards

Required dashboards:

- API/SSE availability;
- regional cell health;
- run success and recovery;
- queue/capacity/cold start;
- provider/model/tool health;
- runner/sandbox isolation health;
- database/object store;
- webhook/integration delivery;
- usage/billing pipeline;
- security findings/audit lag;
- SLO/error budget;
- backup/restore.

### 52.10 Alert quality

Alerts have owner, severity, SLI or symptom, runbook, deduplication, and test. Page on user-impacting symptoms or imminent data/security risk, not every transient dependency error. Alerts are exercised in drills.

### 52.11 Support bundle

An operator-generated support bundle contains:

- versions and compatibility;
- redacted config;
- health/capability summaries;
- migration state;
- bounded metrics/logs;
- runner/provider/extension status;
- recent stable error IDs;
- no customer content/secrets by default.

Bundle creation previews content, has expiry, is encrypted, and is audited.

## 53. Reliability and failure semantics

### 53.1 Reliability principle

Accepted work must be durable before acknowledgement, and every ambiguous side effect must remain visible. The platform prefers a typed incomplete/uncertain state over a false success.

### 53.2 Dependency failure matrix

| Failure | Required behavior |
|---|---|
| PostgreSQL unavailable | reject mutations; do not buffer accepted work in process memory; serve only explicitly safe stale health/static data |
| object storage unavailable | queue or fail operations requiring artifacts/snapshots; simple responses may proceed only if policy permits loss of portable recovery |
| model target unavailable | retry/fallback under pinned route or wait/fail visibly |
| tool/MCP unavailable | retry by replay class, wait for input, or fail; never fabricate result |
| runner unavailable | retain queued run until queue deadline; expose capacity reason |
| runner/host lost | fence and execute recovery ladder |
| API instance lost | clients reconnect; accepted mutations recover by idempotency key |
| coordinator lost | leases expire and another worker claims |
| webhook destination down | run remains terminal; delivery retries/dead-letters |
| identity provider down | existing valid sessions follow token policy; new login fails; no fail-open auth |
| KMS/secret backend down | secret-dependent operations wait/fail closed |
| analytics/search down | canonical execution continues; derived views show stale/unavailable |

### 53.3 Timeout ownership

Timeout layers are named and non-overlapping:

- API request deadline;
- queue deadline;
- sandbox provisioning deadline;
- engine startup/heartbeat/progress deadline;
- model attempt deadline;
- tool execution deadline;
- approval/input expiry;
- run wall-time budget;
- callback delivery deadline.

The smallest active deadline wins. A timeout result identifies which layer expired and whether external completion is uncertain.

### 53.4 Retry ownership

Every operation records exactly one retry owner:

- SDK transport;
- control-plane job;
- model broker;
- tool broker;
- runner supervisor;
- integration adapter;
- external orchestrator.

Nested hidden retries are disabled or included in the reported attempt count. Retry policies have maximum attempts, total elapsed time, backoff/jitter, error classes, and idempotency precondition.

### 53.5 Fencing

Fencing tokens are monotonically increasing per leased resource:

- session execution ownership;
- run attempt;
- workspace writer;
- tool call executor;
- runner assignment.

Every state-changing callback includes its fence. A stale token is rejected even if its credential has not yet expired. This prevents a recovered old host from overwriting new state.

### 53.6 Reconciliation

For ambiguous external operations, a ReconciliationJob:

- queries destination by idempotency key or expected object/ref;
- compares exact intended request hash;
- determines completed, not applied, conflicting, or unknowable;
- stores evidence/receipt;
- only then retries or requests manual resolution.

Operators cannot choose “completed” without evidence and an audited override reason.

### 53.7 Graceful degradation

Allowed degradation is explicit:

- stale analytics with freshness label;
- delayed callbacks;
- queueing for capacity;
- disabled optional model target/tool/extension;
- transcript reconstruction instead of checkpoint with warning.

Forbidden degradation:

- weaker sandbox isolation;
- different data region/provider policy;
- skipped approval;
- broadened network/capability;
- unmetered platform-managed provider spend;
- silent data loss;
- fabricated tool/model result.

### 53.8 Load shedding

During overload:

1. reject unauthenticated/invalid work early;
2. limit expensive lists/search and new streams;
3. defer background/low-priority triggers;
4. protect active run heartbeats, cancellations, approvals, and terminal persistence;
5. preserve audit/usage/outbox;
6. reject new work with retryable capacity_unavailable and Retry-After.

Cancellation and security revocation paths have reserved capacity.

### 53.9 Clock and ordering

- Hosts use monitored time synchronization.
- Correctness uses database time/monotonic process clocks where appropriate, not runner wall clock alone.
- Event sequence, revision, fencing, and occurrence IDs establish order.
- Clock skew beyond threshold quarantines runner identity renewal and signed webhook generation.
- Human timestamps are informative; sequence/revision decide concurrency.

## 54. Managed-service SLOs

### 54.1 Scope

SLOs measure platform-controlled behavior. Provider, customer runner, repository, MCP/tool, and customer webhook downtime are separately reported and excluded only according to a published calculation—not silently removed from all user-visible metrics.

### 54.2 Initial SLO targets

| SLI | Standard target, monthly | Enterprise target |
|---|---:|---:|
| authenticated API availability excluding long-running work | 99.90% | 99.95% |
| accepted mutation durability/read-after-write | 99.99% | 99.995% |
| event journal retrieval/SSE reconnect availability | 99.90% | 99.95% |
| built-in scheduler occurrences created within 60s of planned time | 99.9% | 99.95% |
| terminal state persisted after platform-controlled completion within 10s | 99.9% | 99.95% |
| cancellation intent delivered to a connected healthy runner within 5s | 99.9% | 99.95% |
| outbound webhook first attempt begun within 30s of journal event | 99.5% | 99.9% |

Availability targets are product goals until commercial terms explicitly turn them into SLAs.

### 54.3 Latency objectives

Under admitted healthy capacity:

- API metadata read p95 below 300 ms;
- accepted mutation p95 below 500 ms;
- first SSE event p95 below 1 s;
- warm sandbox assignment-to-engine-ready p95 below 10 s;
- cold environment assignment-to-engine-ready p95 below 90 s excluding large user image pull;
- session message accepted-to-queued event p95 below 1 s;
- control-plane model broker overhead p95 below 100 ms excluding provider latency.

Latency is segmented by region, isolation tier, environment, and deployment. One blended percentile is insufficient.

### 54.4 Run success

No global “agent success SLO” is claimed. Run terminal outcomes are partitioned:

- platform failure;
- provider/tool/integration failure;
- customer input/config/policy failure;
- capacity/budget cancellation;
- user cancellation;
- task-quality failure measured by eval.

Platform reliability must not hide behind model-quality or invalid-request denominators.

### 54.5 Error budgets

Each SLO has:

- rolling and calendar views;
- burn-rate alerts;
- incident linkage;
- change-freeze/reliability-work policy after exhaustion;
- tenant/cell segmentation;
- public status impact criteria.

### 54.6 Incident communication

Managed service maintains:

- public status page by region/capability;
- authenticated tenant-specific incident notices where impact is narrower;
- severity, commander, timeline, mitigation, and customer-impact records;
- update cadence targets;
- post-incident review for material incidents;
- security notification path separated from ordinary availability notices.

Status never claims model/tool/provider health is platform-green when the default customer path is unusable; dependency impact is attributed but visible.

## 55. High availability and disaster recovery

### 55.1 Availability architecture

Within a managed region:

- API/coordinator replicas span failure zones;
- PostgreSQL uses synchronous/managed HA appropriate to target;
- object storage is zone redundant;
- runner pools span zones;
- gateways/load balancers health-route;
- no single in-memory owner is required for correctness;
- migration jobs are singleton-fenced.

Self-host HA depends on operator-provided database/object/storage/network architecture and is validated by doctor.

### 55.2 Backup scope

Backups cover:

- PostgreSQL full backup plus point-in-time logs;
- object storage versions/manifests;
- encryption-key references and documented key recovery;
- deployment configuration and trust roots;
- extension/release manifests;
- audit export where separately stored.

Runner local writable layers are not backup; required state must reach snapshots/artifacts.

### 55.3 Backup security

- encrypted with separate backup keys;
- region and tenant policy respected;
- immutable/object-lock copy for managed service;
- access role separated from ordinary application;
- deletion/retention tracked;
- malware/ransomware scenario considered;
- backup catalog integrity verified.

A backup without available keys/manifests is not successful.

### 55.4 Restore verification

- automated sample restores at least weekly for managed service;
- full regional restore drill at least quarterly;
- schema/content/object checksum and tenant isolation validation;
- replay of selected API/event/artifact reads;
- measured RPO/RTO;
- findings and remediation tracked.

Self-host tooling provides backup, restore into a separate target, and verification commands.

### 55.5 Recovery objectives

Managed standard targets:

- zonal/instance failure: RPO 0 for committed database state; RTO 15 minutes;
- regional disaster: RPO at most 5 minutes; RTO 4 hours;
- object corruption: restore to last verified consistent manifest within 4 hours for standard retained data.

Enterprise dedicated offerings MAY provide stronger targets. Self-host defaults publish achievable settings rather than inheriting SaaS claims.

### 55.6 Regional disaster behavior

1. declare incident and fence affected cell writers;
2. assess data-region/failover permission;
3. promote consistent database/object recovery point;
4. rotate/reissue workload identities;
5. restore control plane;
6. reconcile outbox, leases, attempts, and external side effects;
7. restore eligible runs via recovery ladder;
8. reopen traffic in stages;
9. report semantic loss and affected IDs.

If policy forbids cross-region failover, the platform remains unavailable rather than violating residency.

### 55.7 Runner disaster

Runner pools are disposable:

- loss triggers fencing/recovery;
- environment images are reproducible;
- workspace/checkpoints are externalized;
- replacement enrollment does not reuse identity;
- capacity plans include zone/host failure headroom.

### 55.8 Disaster exercises

Required game days:

- database primary loss;
- object-store outage/corruption;
- KMS unavailability;
- control-plane zone loss;
- runner-pool loss;
- stale runner return after recovery;
- provider/gateway outage;
- webhook flood;
- malicious extension/image revocation;
- region failover/failback;
- backup-key recovery.

## 56. Testing and chaos engineering

### 56.1 Test layers

- schema and static contract tests;
- unit tests for state machines/policy;
- property tests for idempotency/order/budget;
- component tests with real PostgreSQL/object store/runtime;
- SDK contract tests;
- provider/tool adapter recordings plus live canaries;
- sandbox escape/hardening tests;
- end-to-end workflows;
- failure/chaos tests;
- performance/soak tests;
- security/red-team tests;
- UAT.

Mock-only testing cannot prove recovery or isolation.

### 56.2 Deterministic state-machine tests

Generate valid/invalid transition sequences for:

- session/run/attempt;
- command;
- tool/approval/reconciliation;
- workspace/snapshot;
- trigger/schedule;
- webhook delivery;
- billing settlement/deletion.

Properties include monotonic terminal state, one active fence, no double settlement, no duplicate side effect, and event/state transaction parity.

### 56.3 Fault injection

Tests inject:

- process kill at every durable boundary;
- duplicate/reordered/lost runner frames;
- network timeout before/after external commit;
- PostgreSQL transaction abort/deadlock;
- object upload partial/corruption;
- provider stream disconnect;
- stale fencing token;
- clock skew;
- disk/memory/process limit;
- queue flood and slow consumer;
- extension/MCP crash;
- secret backend timeout.

Each expected outcome is asserted from canonical state, not logs alone.

### 56.4 Load tests

Scenarios:

- high-rate single-shot calls;
- many idle attached sessions;
- concurrent SSE reconnect storm;
- burst schedules/webhooks;
- large repository clone/snapshot;
- child-run fan-out at limits;
- multi-tenant fairness/noisy neighbor;
- warm/cold sandbox mix;
- usage/webhook outbox backlog;
- long-duration sessions and retention compaction.

Capacity results set default limits and autoscaling signals.

## 57. Evaluation system

### 57.1 Purpose

Infrastructure correctness and agent quality are separate. Evals measure whether an engine/profile/route/tool revision actually solves representative work safely, not merely whether the API returned 200.

The approach follows current [evaluation best practices](https://developers.openai.com/api/docs/guides/evaluation-best-practices): task-specific datasets, automated scoring where appropriate, production-like distributions, and human calibration rather than “vibe-based” release decisions.

### 57.2 Evaluation resources

~~~text
EvalSuite
├── DatasetRevision
│   └── EvalCase
├── GraderRevision[]
├── TargetRevision
├── ExecutionPolicy
└── EvalRun
    └── EvalCaseRun[]
        ├── run/trace/artifacts
        └── GraderResults[]
~~~

All inputs, graders, target revisions, environment/model/tool revisions, and results are immutable and content-addressed.

### 57.3 Target

An eval target can be:

- reference engine revision;
- AgentRevision;
- model route revision;
- tool/skill/extension revision;
- context/compaction strategy;
- sandbox/recovery implementation;
- full integration workflow.

Comparison runs pin both baseline and candidate.

### 57.4 Dataset

An EvalCase includes:

- input/messages/artifacts/repository fixture;
- expected output properties, not necessarily exact text;
- allowed tools/capabilities;
- environment;
- deterministic assertions;
- grader rubric;
- tags/difficulty/source/license;
- sensitive-data classification;
- known limitations.

Train/tuning, validation, and held-out release sets are separated. Production-derived cases are redacted, consented, deduplicated, and access-controlled.

### 57.5 Grader types

- exact/schema/regex/property grader;
- code tests/build/lint/security scanner;
- repository diff invariant;
- tool/trace behavior grader;
- citation/source verification;
- cost/latency/budget grader;
- policy/security grader;
- model-as-judge with rubric;
- pairwise model judge;
- human annotation/review.

Deterministic graders take precedence for objectively testable properties.

### 57.6 Model graders

Model-as-judge results include:

- judge route/revision;
- rubric revision;
- full permitted inputs;
- score, rationale, confidence;
- repeated-sample variance where configured;
- calibration against human labels.

A model grader cannot be the sole gate for destructive-action safety, secret isolation, tenant isolation, or protocol correctness.

### 57.7 Trace grading

Outcome alone is insufficient. Trace graders inspect:

- unnecessary or missing tools;
- capability/approval decisions;
- unsafe/repeated side effects;
- delegation use when required;
- context source use;
- recovery/replay;
- citation lineage;
- cost/latency;
- premature completion.

Hidden chain-of-thought is not required; canonical model/tool/run events are enough.

### 57.8 Coding-agent benchmark

Release suite includes repositories with:

- localized bug fixes;
- multi-file feature changes;
- failing/hidden tests;
- dependency/version constraints;
- merge conflicts;
- generated/binary/protected files;
- malicious repository instructions;
- secret-like fixtures;
- network unavailable/allowlisted cases;
- long context/compaction;
- host kill and recovery;
- subagent required/forbidden.

Primary score combines test correctness, invariant preservation, no forbidden side effect, changeset quality, cost, and time. Patch similarity is not a sufficient metric.

### 57.9 Research-agent benchmark

Cases evaluate:

- source quality and recency;
- citation correctness;
- claim/evidence alignment;
- contradictory evidence;
- uncertainty/calibration;
- structured schema;
- prompt injection from sources;
- privacy and capability limits;
- cost/time.

### 57.10 Integration benchmark

Slack, webhook, schedule, queue, repository, and A2A fixtures test:

- duplicate delivery;
- delayed/out-of-order message;
- identity mapping;
- attachment;
- rate limit/retry;
- approval;
- exactly one terminal response/callback;
- source reconciliation.

### 57.11 Recovery benchmark

Kill points include:

- before/after provider acceptance;
- mid-stream;
- before/after tool external commit;
- after checkpoint before snapshot;
- during snapshot upload;
- after branch push before receipt;
- during approval wait;
- parent/child completion races.

The grader proves no duplicate effect, correct recovery level, queued-message order, and user-visible evidence.

### 57.12 Security/red-team eval

Suites cover:

- direct/indirect prompt injection;
- tool description poisoning;
- MCP/A2A/card metadata poisoning;
- secret extraction/encoding;
- SSRF and metadata access;
- path/symlink escape;
- shell injection;
- privilege escalation;
- cross-tenant IDs/cursors;
- runaway delegation/cost;
- malicious skill/plugin/image;
- approval deception;
- browser visual injection.

Security regressions block release regardless of aggregate quality score.

### 57.13 Release gates

A candidate AgentRevision/model route/reference engine cannot be promoted when:

- any critical deterministic/security invariant regresses;
- task success drops beyond configured confidence interval;
- cost or p95 latency exceeds ceiling without approved tradeoff;
- fallback/retry/denial rate exceeds threshold;
- model grader is uncalibrated;
- dataset coverage is insufficient for a new capability.

Gate results and approved exceptions are attached to the published revision.

### 57.14 Statistical treatment

- report sample count and confidence intervals;
- use paired comparisons on the same cases;
- repeat non-deterministic cases;
- separate failures by category;
- do not average away critical safety failures;
- track dataset drift and contamination;
- preserve raw case-level results for analysis.

### 57.15 Online evaluation

Managed service can run:

- explicit user feedback;
- deterministic validation on production outputs;
- content-free operational quality signals;
- policy-approved sampled trace/model grading;
- shadow/canary routes with no external side effects.

Content sampling is opt-in/policy-controlled, redacted, region-bound, and access-audited. Online graders cannot autonomously publish prompt/model changes.

### 57.16 Human review

Review UI:

- blinded baseline/candidate where possible;
- rubric-specific labels;
- adjudication and disagreement;
- reviewer identity/expertise;
- sensitive-data access control;
- inter-rater agreement;
- feedback linked to exact case/revision.

### 57.17 Prompt optimization

Automated systems may propose new instructions/configuration. A proposal:

1. becomes a draft revision;
2. is evaluated on separate validation/held-out data;
3. shows diff and tradeoffs;
4. requires authorized publication;
5. rolls out through canary;
6. can be rolled back.

The production agent never rewrites its own active trusted instructions or capabilities.

### 57.18 External eval tools

Promptfoo, Langfuse, provider eval APIs, and other systems may import/export datasets/traces/results. Canonical EvalSuite/EvalRun records remain portable and do not require any external service.

## 58. Build-versus-reuse research decision

### 58.1 Decision

Create a new independent public product repository with product-owned API, domain model, event journal, engine protocol, reference kernel, coordinator, SDKs, runner, and conformance suite.

Reuse proven infrastructure and libraries behind adapters. Do not fork an existing product and inherit its public model, hosted-service assumptions, model vendor, workflow engine, or sandbox provider.

### 58.2 Existing systems assessed

| System | What it validates/provides | Why it is not the entire product | Reuse decision |
|---|---|---|---|
| [OpenHands Software Agent SDK](https://github.com/OpenHands/software-agent-sdk/) and [Agent Server](https://docs.openhands.dev/sdk/guides/agent-server/overview) | real coding agent, tools, local/remote workspace and event streaming | does not define this platform's provider-neutral four-surface API, durable coordinator, SaaS tenancy/billing/policy, engine protocol, or recovery contract | study behavior and optionally build a conforming engine adapter; do not make its Python objects the public contract |
| [Daytona](https://github.com/daytonaio/daytona) | open-source sandbox control/compute planes, SDKs, snapshots, hosted/hybrid compute | primarily sandbox infrastructure, not the complete agent/session/model/tool/SaaS product | evaluate as optional sandbox driver or implementation reference |
| [E2B](https://github.com/e2b-dev/E2B) | cloud sandbox API and secure code execution pattern | does not supply the complete self-contained agent/control-plane contract required here | optional sandbox adapter; no mandatory cloud dependency |
| [Letta](https://github.com/letta-ai/letta) | stateful agent and memory API | memory-first agent model does not cover coding workspaces, capability workers, repository publication, and this public lifecycle | learn from explicit state/memory; no core dependency |
| [Pydantic AI](https://github.com/pydantic/pydantic-ai) | model-agnostic Python agent library, MCP, durable integrations | library-level agent semantics would leak into a language-neutral product contract | optional internal experiment/adapter; platform owns kernel protocol |
| [LangGraph persistence](https://langchain-ai.github.io/langgraph/concepts/time-travel/) | checkpointed graph execution/time travel | general graph is not the default coding/chat loop and does not solve sandbox/SaaS boundaries | no canonical graph dependency; workflows may be extensions |
| [LiteLLM](https://docs.litellm.ai/) | broad provider normalization, proxy/routing/cost controls | gateway behavior and feature lag cannot own canonical routing, policy, or billing | supported optional gateway with direct-provider escape |
| [Kubernetes Agent Sandbox](https://github.com/kubernetes-sigs/agent-sandbox) | Sandbox/Claim/Template/WarmPool patterns | Kubernetes-only and evolving; does not implement agent/control plane | optional driver after pinned conformance |
| [Temporal](https://github.com/temporalio/temporal), Restate, DBOS | durable workflow primitives | an external workflow system would make local/self-host heavier and expose foreign lifecycle concepts | optional orchestrator adapters; built-in narrow coordinator is mandatory |
| MCP | portable tool/resource connection | experimental task semantics and untrusted servers do not replace run state/policy | stable tool/resource adapter only |
| A2A 1.0 | opaque agent interoperability, tasks/messages/artifacts | too coarse for workspace/tool replay/checkpoint/accounting | boundary client/server adapter only |

### 58.3 Why not adopt one repository wholesale

No assessed public repository simultaneously guarantees:

- provider-neutral direct and gateway model paths;
- profile-free single-shot API;
- attachable session with live model/tool changes;
- reusable agents and schedules;
- A2A facade;
- versioned engine process protocol;
- exact/checkpoint/transcript recovery;
- tool replay and uncertain-effect reconciliation;
- hostile coding sandbox plus external capability workers;
- repository publication permissions;
- TypeScript/Python/Go SDK parity;
- fully local and self-hosted baseline;
- multi-tenant managed SaaS, billing, data governance, and SLO;
- open extension contracts.

The gap is product integration and durable semantics, not absence of reusable components.

### 58.4 Reuse rule

Before adopting a component:

1. inventory its relevant behavior and license;
2. run a focused proof against the platform interface;
3. document semantic gaps;
4. create an adapter and conformance suite;
5. pin a tested version/digest;
6. retain an exit path and canonical data export;
7. avoid copying or rewriting a working agent behavior merely to fit an internal abstraction.

When an existing agent engine is integrated, first record its model/tool/context loop, message/cancel behavior, checkpoint capability, event ordering, workspace assumptions, and credential access. The adapter preserves verified behavior; it does not create an untested imitation.

### 58.5 Reference kernel decision

The product ships its own minimal reference kernel because a usable platform cannot require customers to bring an agent. Its contract is the engine protocol, not an implementation framework.

The reference kernel:

- implements coding, research, tool use, structured output, subagents, compaction, and checkpoint;
- uses model/tool brokers rather than embedding vendor credentials;
- can replace internal provider/client libraries without public API changes;
- is tested against the same engine conformance suite offered to third-party engines.

### 58.6 Sandbox reuse decision

Sandbox isolation is too security-sensitive to invent a new hypervisor/runtime. The platform builds orchestration, policy, workspace, and driver integration around established OCI, userspace-kernel, Kata/microVM, and cloud runtime primitives. It does not build a new container runtime.

### 58.7 Durable coordination decision

The built-in coordinator is intentionally limited to the platform's fixed state machines, timers, leases, outbox, and retries. This keeps the standalone install complete without recreating a general-purpose workflow programming system.

Customers with an existing workflow system integrate through APIs/webhooks/SDK activities. The platform does not bundle such a system as a hidden hard dependency.

## 59. Independent repository and package structure

### 59.1 Repository boundary

Implementation begins in a brand-new standalone Git repository.

- It is not created inside an existing product repository.
- It contains no product-specific names, imports, database assumptions, or business use cases.
- Existing products consume released SDKs, APIs, OCI images, or integration adapters.
- It is not accidentally committed as a Git submodule/gitlink into a consumer repository.
- Any consumer adapter is committed in that consumer's repository or in a clearly separate integration repository.
- Changes to an imported pre-existing engine repository remain commits in that engine repository; they are not smuggled into this repository.

The current document is a design artifact only and does not establish implementation repository ownership.

### 59.2 Proposed public monorepo layout

~~~text
/
├── apps/
│   ├── control-plane/
│   └── web-console/
├── cmd/
│   ├── cli/
│   └── runner/
├── engines/
│   └── reference/
├── packages/
│   ├── contracts/
│   ├── state-machines/
│   ├── policy/
│   ├── model-broker/
│   ├── tool-broker/
│   ├── coordinator/
│   └── extension-sdk/
├── adapters/
│   ├── models/
│   ├── sandboxes/
│   ├── repositories/
│   ├── integrations/
│   ├── orchestration/
│   └── observability/
├── sdks/
│   ├── typescript/
│   ├── python/
│   └── go/
├── protocols/
│   ├── openapi/
│   ├── asyncapi/
│   ├── engine/
│   ├── runner/
│   └── extension/
├── deploy/
│   ├── compose/
│   ├── helm/
│   └── airgap/
├── tests/
│   ├── conformance/
│   ├── e2e/
│   ├── fault/
│   ├── security/
│   ├── evals/
│   └── performance/
├── docs/
│   ├── architecture/
│   ├── api/
│   ├── operations/
│   ├── security/
│   └── adr/
└── examples/
    ├── single-shot/
    ├── interactive-session/
    ├── slack/
    ├── scheduled-investigation/
    └── customer-runner/
~~~

Language-specific build layout may refine this structure during the reviewed implementation plan. Public protocols and conformance fixtures remain language-neutral.

### 59.3 Dependency direction

~~~text
contracts/state machines
        ↑
domain services and brokers
        ↑
control plane / runner / engine
        ↑
public API, SDK, CLI, console, adapters
~~~

Core contracts do not import provider, workflow, cloud, or consumer-product packages. Adapters depend inward on interfaces; domain code does not depend outward on adapters.

### 59.4 Schema source of truth

- Domain/state-machine definitions and JSON Schemas are checked in.
- OpenAPI/AsyncAPI/SDK/protocol types are generated reproducibly from canonical schemas.
- Generated output is checked in for consumers and review.
- A CI drift check fails manual edits or stale output.
- Examples are executable contract fixtures.

### 59.5 Commercial code

Managed SaaS deployment/operations and closed enterprise modules live in separate repositories or clearly isolated packages that consume public interfaces. The public repository must build, test, and run without access to private code.

Private code cannot monkey-patch core semantics. A feature necessary for conformance belongs in public core.

## 60. Licensing and open-source governance

### 60.1 License

Apache License 2.0 covers:

- control plane core and built-in coordinator;
- reference engine;
- runner and CLI;
- official SDKs;
- public protocols/schemas;
- core model/tool/MCP/repository/sandbox adapters;
- local Compose and standard Helm chart;
- conformance tests and documentation;
- basic web console and multi-user governance.

Brand names/logos are governed separately by trademark policy.

### 60.2 Commercial boundary

Commercial value may include:

- operated SaaS;
- proprietary global control/operations tooling;
- advanced enterprise governance/SSO/SCIM/support controls;
- dedicated regional/capacity/connectivity offerings;
- premium integrations and compliance packages;
- contractual SLA/support.

The commercial boundary cannot make the open distribution unable to execute real agents, persist sessions, use multiple models/tools, run sandboxes, or upgrade/backup.

### 60.3 Contribution model

- public roadmap/issues/RFCs;
- Developer Certificate of Origin sign-off rather than mandatory copyright assignment;
- code of conduct;
- contributor/maintainer ladder;
- documented review and release authority;
- security-sensitive areas require designated owners;
- automated contribution checks reproducible locally.

### 60.4 Decision governance

Changes use:

- issue for bounded bug/feature;
- ADR for internal implementation choice;
- RFC for public API/protocol/security/licensing/architecture change;
- security advisory for confidential vulnerability;
- compatibility report for stable schema change.

An RFC includes problem, constraints, alternatives, security/privacy, migration, compatibility, operational impact, and acceptance evidence.

### 60.5 Release governance

- semantic product releases and dated API revisions;
- signed release commit/tag;
- reproducible artifacts and provenance;
- release candidate soak/conformance/eval/security gates;
- changelog categorized by breaking/deprecated/additive/fix/security;
- public support matrix;
- no force-moving release tags;
- emergency security release process.

### 60.6 Extension ecosystem

The extension SDK and manifest are open. Community extensions can use any compatible license but must disclose it. Marketplace policies may reject incompatible, misleading, or malicious licensing/distribution.

### 60.7 Trademark and compatibility claims

Third parties may truthfully say “compatible with engine.v1” only after passing the published conformance profile/version. They cannot imply official certification or use protected branding without permission.

## 61. Rejected and constrained alternatives

| Alternative | Decision and rationale |
|---|---|
| agent-profile-first public API only | rejected; makes single-shot and dynamic chat awkward |
| stateless response API only | rejected; cannot naturally represent live attach, recovery, queue, workspace, and long sessions |
| A2A as internal model | rejected; interoperability task is too coarse for internal recovery/accounting |
| separate loops for API/chat/cron | rejected; creates divergent policy, replay, and behavior |
| fixed model for a session | rejected; model changes at safe step boundaries are required |
| profiles required for every call | rejected; profile-free Responses/Sessions are first-class |
| subagents always automatic | rejected; optional by default, explicitly required when requested |
| single vendor hosted agent | rejected; provider neutrality and self-host require own broker/kernel |
| LiteLLM as mandatory canonical layer | rejected; useful optional gateway, insufficient feature/exit guarantee |
| external workflow engine as mandatory core | rejected; standalone local/self-host must boot independently |
| build a general workflow language | rejected; built-in coordinator stays narrow and product-specific |
| Kubernetes-only product | rejected; local/single-VM use is required |
| plain shared Docker as managed tenant boundary | rejected; hostile multi-tenancy requires microVM/dedicated tier |
| build a new hypervisor/container runtime | rejected; integrate established isolation runtimes |
| give model/provider/repository secrets to engine | rejected; brokered short-lived operation credentials |
| let models install plugins/skills freely | rejected; quarantine, signature, capability review required |
| direct protected-branch publish by default | rejected; granular publication capabilities and approvals |
| universal exactly-once claim | rejected; external systems need idempotency/reconciliation |
| SSE disconnect cancels work | rejected; cancellation is explicit and durable |
| workflow history/logs as product state | rejected; PostgreSQL journal/state is canonical |
| hidden cross-session memory | rejected; memory is explicit, inspectable, policy-controlled |
| chain-of-thought as observability requirement | rejected; canonical actions/results are sufficient |
| proprietary-only functional core | rejected; self-hosted open core must be genuinely usable |
| one existing repository wholesale | rejected; assessed projects solve useful layers but not the complete contract |

## 62. Release readiness and UAT policy

### 62.1 Evidence rule

A UAT passes only with:

- test case/revision and environment manifest;
- canonical API responses/events;
- relevant external destination receipt;
- audit and usage evidence;
- assertions over final database/object state;
- reproducible logs/traces with secrets absent;
- automated result where feasible;
- human sign-off only for genuinely experiential criteria.

“It seemed to continue” or a successful final message is not recovery proof.

### 62.2 Blocking severity

- P0: tenant escape, secret exposure, unauthorized irreversible side effect, committed-data loss, false success; no waiver.
- P1: core lifecycle/recovery/idempotency failure; no stable-release waiver.
- P2: important degraded behavior with documented workaround; time-bound waiver requires owner.
- P3: cosmetic/non-blocking.

### 62.3 Consumer migration rule

Any existing product integrating this platform keeps its legacy execution path until:

- all applicable P0/P1 UATs pass in that product's environment;
- session/state mapping and rollback are proven;
- traffic can be canaried and reverted;
- no working agent behavior was replaced by an unverified imitation;
- repository boundaries/commits are correct.

This rule applies to consumers without putting their product names or domain assumptions into this repository.

## 63. Mandatory end-to-end acceptance journeys

### 63.1 Local single-shot journey

From a clean supported workstation:

1. install CLI;
2. run init and local up;
3. configure a model connection;
4. call Responses through TypeScript, Python, Go, and CLI;
5. receive streaming text and strict structured output;
6. verify usage, events, audit, and store:false purge;
7. stop/start the stack and repeat retrieval where retained.

Pass: no source build/manual database edit, identical semantic result across SDKs, no cloud account other than chosen model provider, and doctor is green.

### 63.2 Interactive coding journey

1. create session with repository binding;
2. prepare isolated workspace at exact commit;
3. agent inspects code, runs tests, edits files;
4. user observes stream from a separate client;
5. user queues and steers messages;
6. user changes model;
7. required research child uses cheaper route;
8. process/container is killed;
9. run recovers with correct tool/message order;
10. changeset and tests are shown;
11. approved branch push and draft PR occur once.

Pass: final repository SHA/diff correct, credentials absent from engine/events/snapshot, recovery evidence complete, no duplicate tool/push/PR.

### 63.3 Slack journey

1. install integration in a test workspace;
2. mention/start assistant thread;
3. duplicate source event is delivered;
4. session starts once and streams visible progress;
5. web console attaches to same session;
6. Slack message is queued while a tool runs;
7. interactive exact approval is completed by authorized user;
8. model route changes;
9. cancellation and follow-up work;
10. Slack rate limit/network interruption occurs.

Pass: one canonical session, one effect per source event, correct actor authorization, canonical result recoverable even if Slack output update fails.

### 63.4 Scheduled investigation journey

1. publish an AgentRevision and strict InvestigationReport schema;
2. configure signed webhook and cron variants;
3. send duplicated rejection event;
4. run web research with citations and no repository/store-write capability;
5. kill host during research;
6. recover and produce one report;
7. deliver one idempotent callback;
8. attach for follow-up;
9. fork a separately authorized coding session.

Pass: report evidence validates, duplicate input/callback absent, host change visible, research capability never expands to publication.

### 63.5 Customer Linux runner with SaaS journey

1. provision a clean private Linux VM;
2. install runner and runtime;
3. enroll with one-time token;
4. verify no inbound firewall opening;
5. run a repository task through public SaaS API;
6. use customer-private model/tool endpoint if configured;
7. drain runner and recover on a second host;
8. revoke runner and prove it cannot reconnect/submit stale events.

Pass: outbound-only control path, short-lived identity, exact placement/isolation disclosure, successful host migration, stale fence rejection.

### 63.6 Self-host lifecycle journey

1. install supported release from signed artifacts;
2. run backup and restore verification;
3. create live sessions/runs/schedules;
4. perform rolling N to N+1 upgrade;
5. migrate database;
6. drain/upgrade runner;
7. resume retained session/checkpoint;
8. roll reference engine alias back for new runs;
9. export data;
10. restore into a separate installation.

Pass: API/SDK parity, no lost accepted state, migration/rollback within documented window, checksums and tenant identity intact.

### 63.7 Multi-tenant SaaS journey

1. create two adversarial test organizations;
2. populate identically named resources;
3. fuzz IDs/cursors/artifact URLs/webhooks/search/usage;
4. run hostile code concurrently;
5. exercise support access and denial;
6. exhaust one tenant's quota;
7. delete one tenant;
8. verify the other remains unaffected.

Pass: zero cross-tenant disclosure/effect, fair capacity, isolated usage/billing/keys, complete deletion receipt.

## 64. Detailed UAT matrix

### 64.1 API and SDK

| ID | Scenario | Required proof |
|---|---|---|
| API-001 | profile-free synchronous Response | one transient session/root run, correct output/usage, no profile required |
| API-002 | streamed Response disconnect/reconnect | no cancellation, event IDs deduplicated, final accumulation equals canonical response |
| API-003 | background Response | 202/Location, poll/SSE/webhook converge on one terminal object |
| API-004 | same idempotency key/request | identical status/body/resource, one side effect |
| API-005 | key reused with different request | 409 idempotency_mismatch, original unaffected |
| API-006 | concurrent duplicate mutation | one reserved operation, duplicate gets stored result or retryable in-progress |
| API-007 | store:false | retrievable only inside declared operational window; content purged, content-free records remain |
| API-008 | structured output repair | invalid first output visible, bounded repair, final schema valid and metered |
| API-009 | API revision and unknown fields/enums | old SDK continues, preserves unknown values, deprecation headers correct |
| API-010 | cursor/ETag concurrency | stable pagination and 412 on stale protected mutation |
| API-011 | RFC 9457 errors | stable code/request ID/retryability; no provider secret/stack leak |
| API-012 | TS/Python/Go parity | shared fixture produces semantically identical request/events/errors |
| API-013 | SDK retry | same idempotency key across injected transport failure, one mutation |
| API-014 | webhook verification vectors | all SDKs accept valid rotation signatures and reject modified/stale bodies |
| API-015 | idempotency after store:false purge | 410 tombstone, no content disclosure and no re-execution |

### 64.2 Sessions and commands

| ID | Scenario | Required proof |
|---|---|---|
| SES-001 | two authorized clients attach | same ordered journal and final state |
| SES-002 | unauthorized attach | no existence/content disclosure; audit denial |
| SES-003 | queue message during model/tool step | delivered once at next input boundary |
| SES-004 | steer message | applies at next safe loop boundary, sequence recorded |
| SES-005 | interrupt message | current cancelable step ends partial/canceled; new step includes message |
| SES-006 | normal model switch | current step finishes; exact next step uses new pinned route |
| SES-007 | immediate model switch | in-flight attempt canceled/partial; portability warning and new route recorded |
| SES-008 | tool-set change denied by policy | no silent fallback/broadening; typed denial |
| SES-009 | pause/resume | valid checkpoint/snapshot, compute released, same logical run resumes in new attempt |
| SES-010 | repeated cancel | one monotonic cancellation, children/tools reconciled, no duplicate terminal |
| SES-011 | session fork | history boundary copied; future messages/workspaces isolated |
| SES-012 | close/delete | new messages rejected, retention/legal hold behavior correct |

### 64.3 Agents and subagents

| ID | Scenario | Required proof |
|---|---|---|
| AGT-001 | publish immutable AgentRevision | mutation creates new revision; old run remains reproducible |
| AGT-002 | trigger pins revision | later profile edit does not change accepted delivery |
| AGT-003 | profile-free run | all core features work without AgentProfile |
| SUB-001 | optional delegation | model may use or skip; behavior explicit |
| SUB-002 | required research child with cheaper route | at least one conforming child, route/cost/result linked |
| SUB-003 | required child unavailable | typed capability/capacity failure, no parent-only false success |
| SUB-004 | depth/fan-out/budget | deterministic denial at limit; no runaway children |
| SUB-005 | child cancellation | propagation and terminal accounting correct |
| SUB-006 | child workspace mutation | isolated branch/worktree and explicit conflict-aware merge |
| SUB-007 | remote A2A child | minimum context/capability, untrusted result, no secret inheritance |

### 64.4 Models

| ID | Scenario | Required proof |
|---|---|---|
| MOD-001 | at least two independent direct provider families | text/stream/tool/schema paths pass adapter conformance |
| MOD-002 | private/OpenAI-compatible endpoint | active capability probe prevents unsupported feature claim |
| MOD-003 | optional LiteLLM target | same canonical request/result; disabling gateway retains direct path |
| MOD-004 | route selection constraints | region/privacy/capability/price hard filters never relax |
| MOD-005 | safe fallback before output | new attempt recorded, correct target/usage |
| MOD-006 | fallback after partial output | partial preserved; no seamless/hidden retry |
| MOD-007 | provider accepted but response ambiguous | no blind tool-producing replay; reconciliation/typed failure |
| MOD-008 | provider SDK/gateway hidden retry | reported attempt count proves no multiplication |
| MOD-009 | cancellation | provider cancel attempted; partial/final/usage state consistent |
| MOD-010 | prompt cache | tenant/provider isolation and cache usage/cost visible |
| MOD-011 | budget reservation/settlement | concurrent steps cannot overspend beyond documented estimate variance |
| MOD-012 | provider outage circuit | caller-invalid errors do not trip shared circuit; allowed route fails over |

### 64.4.1 Knowledge and retrieval

| ID | Scenario | Required proof |
|---|---|---|
| KNO-001 | upload/repository/connector ingestion | immutable source/document/chunk/index provenance and atomic activation |
| KNO-002 | failed refresh | prior active index remains complete and queryable |
| KNO-003 | ACL/cross-tenant vector query | unauthorized documents neither leak nor affect visible ranking |
| KNO-004 | source update/delete | stale revision pin reproducible; latest excludes deleted content/caches |
| KNO-005 | hybrid retrieval/rerank | pinned strategy/route/scores/cost and citation offsets recorded |
| KNO-006 | malicious instruction in source | content remains untrusted and cannot grant tool/capability |
| KNO-007 | embedding provider policy | restricted source never sent to disallowed provider/region |
| KNO-008 | freshness requirement | stale source produces configured failure/warning, never silent freshness claim |

### 64.5 Tools, skills, MCP, and approvals

| ID | Scenario | Required proof |
|---|---|---|
| TOL-001 | pure tool replay after process kill | cached/replayed result labeled; no semantic duplication |
| TOL-002 | destination-idempotent tool | same external key, one external object |
| TOL-003 | irreversible tool loses response | enters uncertain/manual resolution; never auto-replays |
| TOL-004 | reversible side effect | reconcile then compensate/retry according to policy |
| TOL-005 | exact approval | token bound to argument hash; edited arguments require new approval |
| TOL-006 | expired/wrong approver | action denied and audited |
| TOL-007 | client tool worker loss | run waits/fails visibly; no in-memory exactly-once claim |
| TOL-008 | MCP stdio and HTTP | discovery/call/progress/cancel mapping and namespacing pass |
| TOL-009 | MCP token passthrough/confused deputy attempt | audience validation denies; no upstream token reuse |
| TOL-010 | MCP sampling | denied by default; enabled call is separate brokered budgeted model step |
| TOL-011 | malicious skill archive/instructions | quarantine/path scan/capability boundary prevents execution/escalation |
| TOL-012 | hook timeout/crash | fail-open/closed matches category; core process survives |
| TOL-013 | secret-using tool | executor succeeds; secret absent from engine, events, logs, artifacts, snapshot |
| TOL-014 | network SSRF/DNS rebinding | private/metadata target denied after resolution and redirect |
| TOL-015 | large/binary tool output | bounded event plus authorized artifact, no journal overflow |
| TOL-016 | remote HTTP duplicate/retry | signed request and stable tool_call_id produce one execution/result |
| TOL-017 | remote async tool timeout/late callback | active fence/reconciliation prevents stale silent commit |
| TOL-018 | tool SDK parity | TypeScript/Python/Go fixtures emit equivalent schemas/results/signatures |

### 64.6 Engine, checkpoint, and recovery

| ID | Scenario | Required proof |
|---|---|---|
| ENG-001 | protocol handshake mismatch | attempt fails incompatible before run input/secrets |
| ENG-002 | duplicate JSONL frame | same hash deduplicated; changed hash protocol violation |
| ENG-003 | malformed/oversized frame | sandbox terminated safely; no control-plane crash |
| ENG-004 | engine process kill | new attempt restores checkpoint or transcript with evidence |
| ENG-005 | whole container kill | workspace/checkpoint/transcript recovery and message order |
| ENG-006 | runner host disappears | stale fence, placement on another host, recovery ladder evidence |
| ENG-007 | old host returns | diagnostics allowed; authoritative frames rejected |
| ENG-008 | exact resume | same healthy process/lease confirmed and labeled exact |
| ENG-009 | compatible checkpoint | new process restores boundary, no tool replay error |
| ENG-010 | incompatible/corrupt checkpoint | rejected event then transcript fallback or explicit failure |
| ENG-011 | checkpoint migration | original preserved, migrated checksum/provenance, rollback semantics |
| ENG-012 | queued/steer/interrupt during outage | canonical delivery semantics after recovery |
| ENG-013 | terminal frame then process crash | exactly one persisted terminal under current fence |
| ENG-014 | process exit without terminal | never false success; recovery/failure |

### 64.7 Sandbox and workspace

| ID | Scenario | Required proof |
|---|---|---|
| SAN-001 | path traversal/symlink/device/socket | access denied outside workspace |
| SAN-002 | host/runtime socket access | absent/denied |
| SAN-003 | CPU/memory/disk/process exhaustion | bounded termination, other tenants healthy, usage recorded |
| SAN-004 | network private/metadata scan | denied and finding/audit emitted |
| SAN-005 | snapshot/restore | file/index/tree checksums identical; exclusions empty/no secret |
| SAN-006 | stale workspace writer | fenced write/snapshot rejected |
| SAN-007 | warm pool reuse | no previous tenant data/process/credential; fresh writable layer |
| SAN-008 | failed destroy | host quarantined from new tenant |
| SAN-009 | microVM tenant isolation test | escape suite fails; attested tier matches discovery |
| SAN-010 | preview/terminal authorization | expired/wrong tenant route denied; no direct sandbox address |
| SAN-011 | customer runner revoke | new leases and stale events rejected |
| SAN-012 | runner clock skew | renewal/signature work quarantined as specified |

### 64.8 Repository

| ID | Scenario | Required proof |
|---|---|---|
| REP-001 | deterministic clone | exact requested commit/tree and preparation receipt |
| REP-002 | malicious Git config/hooks/submodule | no host command/credential escape; policy enforcement |
| REP-003 | credential scan | token absent from remote URL, env dump, process args, logs, snapshot |
| REP-004 | default branch write | denied unless explicitly granted |
| REP-005 | changeset | complete file/patch/test/provenance independent of model summary |
| REP-006 | approved branch push | exact commits once; scoped token destroyed |
| REP-007 | push response lost | remote ref reconciliation prevents duplicate/force |
| REP-008 | duplicate PR request/callback | one PR with stable external receipt |
| REP-009 | changed PR head after approval | merge approval invalid, action denied |
| REP-010 | base branch movement/conflict | visible rebase/merge/wait; no dropped remote changes |
| REP-011 | child merge conflict | parent workspace remains consistent; explicit resolution |
| REP-012 | unsafe local bind | prominent warning, host scope exact, managed service unavailable |

### 64.9 Triggers, schedules, queues, webhooks

| ID | Scenario | Required proof |
|---|---|---|
| AUT-001 | duplicate inbound webhook | one TriggerDelivery/action, duplicate links original |
| AUT-002 | invalid signature/replay | rejected before action; safe audit |
| AUT-003 | mapping/schema failure | failed delivery, no billable run |
| AUT-004 | correlation queue | ordered runs/messages per key |
| AUT-005 | singleton/coalesce/replace | exact documented concurrency, no lost irreversible effect |
| AUT-006 | cron DST duplicate/nonexistent | deterministic occurrence according to policy |
| AUT-007 | scheduler replicas | unique occurrence ID, exactly one canonical action |
| AUT-008 | scheduler outage/misfire | configured skip/fire-once/catch-up bound |
| AUT-009 | queue redelivery | source ack/dedupe prevents duplicate action |
| AUT-010 | queue flood | backpressure/fairness, bounded memory |
| AUT-011 | outbound webhook retry | signed attempts, correct retry/dead-letter, run terminal unaffected |
| AUT-012 | webhook DNS rebinding/redirect | target revalidation denies private endpoint |
| AUT-013 | external orchestrator retry | canonical idempotency, no retry multiplication |

### 64.10 Slack and A2A

| ID | Scenario | Required proof |
|---|---|---|
| SLK-001 | Events API three-second acknowledgement | durable ingress before ack and async processing |
| SLK-002 | duplicate Slack event | one message/run/effect |
| SLK-003 | thread ↔ session | stable correlation and web attach to same session |
| SLK-004 | unmapped/unauthorized user | restricted actor cannot approve/change model/access content |
| SLK-005 | message edit/delete/file | immutable correction/tombstone and scanned artifact semantics |
| SLK-006 | rate limit/network loss | canonical output retained and visible message repaired once |
| SLK-007 | interactive approval replay | one-shot user/workspace/hash binding |
| SLK-008 | bot loop | bot/self events do not recursively trigger |
| A2A-001 | Agent Card projection | only published revision/capabilities; auth/version correct |
| A2A-002 | task/message/artifact stream | canonical mapping and terminal consistency |
| A2A-003 | cancel/input-required/push | correct command/wait/webhook mapping |
| A2A-004 | malicious card/file/extension | SSRF/scan/allowlist blocks |
| A2A-005 | remote agent output/secret request | untrusted/minimum context; no inherited credential |

### 64.11 Tenancy, identity, secrets, and data

| ID | Scenario | Required proof |
|---|---|---|
| TEN-001 | cross-tenant ID/cursor/artifact/search/usage fuzz | zero disclosure/effect |
| TEN-002 | RLS missing-scope test | database denies even with application query bug fixture |
| TEN-003 | API key scope/expiry/revoke | correct endpoints/tenant and immediate new-request denial |
| TEN-004 | end-user token exchange | service + subject audited; body spoof denied |
| TEN-005 | support JIT access | approval/scope/expiry/audit; no secret/export |
| SEC-001 | JIT secret lease replay/wrong audience | denied; one operation only |
| SEC-002 | secret rotation during active runs | recorded version/use; old drains then fails |
| SEC-003 | KMS/secret backend outage | fail closed without leak |
| DAT-001 | store:false lifecycle | content gone within advertised TTL and provider boundary disclosed |
| DAT-002 | deletion with derived stores/backups | primary/objects/index removed, backup expiry/receipt tracked |
| DAT-003 | legal hold | deletion blocked visibly, unauthorized hold change denied |
| DAT-004 | export | versioned manifest/checksums, no secret values |
| DAT-005 | residency | disallowed region/provider/runner/failover rejected |
| DAT-006 | signed artifact URL | wrong tenant/action/expiry denied |

### 64.12 Usage, billing, and quotas

| ID | Scenario | Required proof |
|---|---|---|
| BIL-001 | provider/tool/runner usage replay | deterministic ledger IDs, no double settlement |
| BIL-002 | fallback/child/cached usage | correct allocation and actual target dimensions |
| BIL-003 | concurrent hard budget | reservation prevents overspend beyond documented variance |
| BIL-004 | BYOK | provider usage visible but commercial charge distinction correct |
| BIL-005 | adjustment | immutable compensating event and invoice trace |
| BIL-006 | Stripe/OpenMeter outage | replayable export/dead-letter, canonical ledger intact |
| QUO-001 | tenant quota exhaustion | only tenant affected; stable remediation/reset info |
| QUO-002 | noisy neighbor | weighted fairness protects other tenant latency/capacity |

### 64.13 Packaging, upgrade, and DR

| ID | Scenario | Required proof |
|---|---|---|
| OPS-001 | clean local install | documented commands produce healthy full core |
| OPS-002 | single-VM self-host | TLS/auth/backup/runner/provider and SDK conformance |
| OPS-003 | Kubernetes install | restricted policy/network/HA and no ongoing cluster-admin |
| OPS-004 | air-gap install | offline signature verification, private model/Git, no heartbeat dependency |
| OPS-005 | N to N+1 rolling upgrade | active run survives; old/new compatibility |
| OPS-006 | migration interruption/resume | no corruption, bounded locks, correct version |
| OPS-007 | application rollback | expanded schema supports prior version |
| OPS-008 | runner/engine version skew | supported works; unsupported refused with path |
| DR-001 | database primary loss | committed state and objective met |
| DR-002 | point-in-time restore | sessions/events/artifacts consistency and tenant isolation |
| DR-003 | regional failover | RPO/RTO measured; residency policy respected |
| DR-004 | object corruption | manifest/checksum detects and restores or reports loss |
| DR-005 | KMS/key recovery | backup usable only with authorized recovered key |
| DR-006 | full restore drill | clean environment passes core reads/runs/conformance |

### 64.14 Security, performance, and product quality

| ID | Scenario | Required proof |
|---|---|---|
| QUA-001 | coding benchmark | task/test/invariant/cost thresholds met |
| QUA-002 | research/citation benchmark | claim/source/confidence thresholds met |
| QUA-003 | prompt-injection/red-team suite | no critical capability/data escape |
| QUA-004 | model/agent route release | paired eval and canary gates pass |
| SEC-101 | image/plugin signature/digest tamper | execution/promotion denied |
| SEC-102 | sandbox escape suite | no escape; finding/quarantine behavior works |
| SEC-103 | audit tamper/gap | integrity verification and alert |
| PER-001 | single-shot and SSE load | SLO percentiles/error rates under target load |
| PER-002 | cold/warm sandbox | phase budgets and capacity behavior measured |
| PER-003 | long-session soak | bounded memory/journal/compaction/reconnect |
| PER-004 | burst trigger/queue | backpressure and fairness |
| UI-001 | keyboard/screen reader/status | critical console/session/approval workflows accessible |
| UI-002 | exact approval/diff/recovery display | no model summary substitutes authoritative detail |

### 64.15 Stable-release gate

Stable release requires:

- every P0/P1 UAT passed on supported local and self-host reference topology;
- managed-only P0/P1 passed on production-equivalent cell/microVM topology;
- all three SDK conformance suites;
- at least two direct model-provider families plus one private/compatible endpoint;
- one local OCI and one managed high-isolation sandbox path;
- process, container, and host kill/recovery;
- pure/idempotent/irreversible tool replay;
- queued/steer/interrupt messaging;
- secret isolation;
- repository clone/diff/push/PR;
- Slack and generic webhook/schedule journey;
- backup/restore/upgrade;
- tenant isolation and billing reconciliation;
- published security model, support policy, and operational runbooks.

No legacy consumer path is removed merely because the platform itself reaches stable release; consumer-specific UAT remains required.

## 65. Glossary

| Term | Meaning |
|---|---|
| AgentProfile | named configuration lineage; not execution state |
| AgentRevision | immutable executable revision of an AgentProfile |
| Response | convenient single-shot/continued projection over an internal session and root run |
| Session | durable conversation/coordination identity |
| Message | immutable authored content delivered to a Session |
| Run | caller-visible logical execution that survives attempt replacement |
| Attempt | one fenced engine/sandbox allocation for a Run |
| ModelStep | one logical model turn, potentially containing retry/fallback provider attempts |
| ChildRun | bounded subagent execution linked to a parent Run |
| Engine | OCI-packaged process implementing the model/tool/context loop through engine protocol |
| Kernel | behavior inside the reference Engine; reasoning loop, context, tool/delegation interpretation |
| Supervisor | runner component that starts, limits, speaks protocol to, and terminates an Engine |
| Runner | enrolled execution-host daemon that accepts fenced sandbox work |
| Sandbox | physical isolation allocation with an explicit isolation tier |
| Workspace | logical mutable filesystem lineage independent of Sandbox/host |
| WorkspaceSnapshot | immutable recoverable filesystem/repository boundary |
| Checkpoint | opaque Engine loop state at a transcript/workspace boundary |
| EventJournal | canonical immutable ordered observable history per Session |
| ToolRevision | immutable callable schema/executor configuration |
| ToolCall | durable invocation with replay/side-effect state |
| Capability | typed constrained authority to act on a resource |
| Connection | credential/config binding to a provider, repository, integration, or tool |
| ModelRouteRevision | immutable policy/routing candidates for a model alias |
| ConfigSnapshot | content-addressed redacted effective configuration pinned to a Run |
| Artifact | immutable versioned output/file with checksum, provenance, and policy |
| Trigger | versioned authenticated source-event-to-action mapping |
| TriggerDelivery | one received/deduplicated source occurrence |
| ScheduleOccurrence | deterministic planned firing of a Schedule revision |
| CapabilityWorker | enrolled specialized executor such as macOS/device/private-network worker |
| Recovery level | exact, checkpoint, transcript, or explicit failure path selected after disruption |

## 66. User-requirement traceability

| Required outcome | Normative sections | Acceptance evidence |
|---|---|---|
| completely separate generic public product | 1–5, 58–60 | repository boundary review; no consumer names/imports/gitlink |
| use from any product through an SDK | 6–9, 20–23 | API-001–014 and local/cloud parity |
| single-shot API comparable to model platforms | 7.4, 8, 22, 23 | API-001–008; journey 63.1 |
| interactive attachable chat/session | 9, 21–22, 47 | SES-001–012; journeys 63.2–63.3 |
| reusable configured agents and cron jobs | 10, 32–33 | AGT-001–003, AUT-006–008 |
| task/A2A interoperability too | 38 | A2A-001–005 |
| all surfaces share one execution kernel | 1, 6, 12, 25 | engine conformance and API/session/trigger journeys |
| multiple model providers and mid-chat switch | 9.3, 27 | SES-006–008, MOD-001–012 |
| optional LiteLLM, no provider lock-in | 27.8, 58 | MOD-001–003 |
| optional/required cheaper subagents | 11, 25.18–25.19 | SUB-001–007 |
| configurable tools, MCP, skills, hooks | 28 | TOL-001–015 |
| isolated coding container and repository clone/edit | 29–30 | SAN/REP matrix; journey 63.2 |
| observe live and send messages during work | 9, 13, 21–23, 47 | SES-001–005, API-002 |
| secrets/publication/special runtimes outside sandbox | 28.10–28.12, 30.8–30.11, 31, 41 | TOL-013, REP-006–009, SEC-001–003 |
| survive process/container/host changes | 26, 53 | ENG-004–014; journeys 63.2/63.5 |
| exact → checkpoint → transcript fallback | 26.3–26.4 | ENG-008–011 |
| tool replay and idempotent external effects | 20.9, 26.6–26.7, 28.5, 53.6 | TOL-001–004, REP-007–008 |
| queued messages during recovery | 9.2, 26.9 | ENG-012, SES-003–005 |
| Slack chat use | 36 | SLK-001–008; journey 63.3 |
| event-triggered investigation/report | 37 | journey 63.4, QUA-002 |
| local fully runnable | 44 | OPS-001; journey 63.1 |
| self-hosted and managed SaaS | 45–46 | OPS-002–004; journeys 63.5–63.7 |
| connect a Linux VM to cloud service | 24.8–24.9, 45.4–45.5 | journey 63.5, SAN-011 |
| tenant/session/secret/billing management | 39–43 | TEN/SEC/DAT/BIL/QUO UAT |
| public open core plus commercial SaaS | 4.5, 46.9, 60 | clean public build/install and license review |
| do not replace working behavior before inventory/UAT | 58.4, 62.3 | component inventory and consumer-specific migration evidence |
| no removal before kill/replay/secret/host UAT | 62–64 | P0/P1 stable gate plus consumer gate |

## 67. Research evidence catalog

This catalog records the primary sources used to choose patterns. It is not a dependency list.

### 67.1 API, SDK, events, and interoperability

| Source | Evidence used |
|---|---|
| [OpenAI Responses create reference](https://developers.openai.com/api/reference/resources/responses/methods/create) | single-shot, stored continuation, background, streaming, tools, structured outputs |
| [Anthropic Messages create](https://platform.claude.com/docs/en/api/messages/create) | caller-managed stateless multi-turn and single request |
| [Anthropic streaming](https://platform.claude.com/docs/en/build-with-claude/streaming) | SSE text/tool delta pattern |
| [Cursor SDK release](https://cursor.com/changelog/sdk-release) | durable runs, cancellation, resumable event streams |
| [Cursor subagents](https://cursor.com/changelog/2-4) | parallel specialized child-agent pattern |
| [Cursor cloud-agent lessons](https://cursor.com/blog/cloud-agent-lessons) | remote workspace/reliability differences |
| [CloudEvents specification](https://github.com/cloudevents/spec) | interoperable webhook event envelope |
| [WHATWG SSE](https://html.spec.whatwg.org/dev/server-sent-events.html) | event IDs and Last-Event-ID reconnection |
| [RFC 9457](https://www.rfc-editor.org/rfc/rfc9457.html) | Problem Details error shape |
| [RFC 9745](https://www.rfc-editor.org/rfc/rfc9745.html) and [RFC 8594](https://www.rfc-editor.org/info/rfc8594/) | deprecation/sunset lifecycle |
| [OpenAPI 3.2.0](https://spec.openapis.org/oas/v3.2.0.html) | current stable HTTP schema, callbacks/webhooks, JSON Schema alignment |
| [AsyncAPI 3.1.0](https://www.asyncapi.com/docs/reference/specification/v3.1.0) | current stable event-driven API description |
| [A2A 1.0 specification](https://github.com/a2aproject/A2A/blob/main/docs/specification.md) | task/context/message/artifact interoperability facade |
| [openai-node](https://github.com/openai/openai-node) and [stripe-go](https://github.com/stripe/stripe-go) | SDK retry, timeout, request ID, idempotency ergonomics |

### 67.2 Agent/kernel/tool ecosystem

| Source | Evidence used |
|---|---|
| [OpenHands Software Agent SDK](https://github.com/OpenHands/software-agent-sdk/) | real coding tools, local/ephemeral workspaces, configurable agents |
| [OpenHands Agent Server overview](https://docs.openhands.dev/sdk/guides/agent-server/overview) | same client behavior over local/remote workspace and event stream |
| [Pydantic AI](https://github.com/pydantic/pydantic-ai) | provider-neutral agent/MCP/durable adapter patterns |
| [LangGraph persistence/time travel](https://langchain-ai.github.io/langgraph/concepts/time-travel/) | checkpoint/fork/time-travel precedent |
| [Letta](https://github.com/letta-ai/letta) | explicit stateful memory/agent service pattern |
| [MCP stable specification 2025-11-25](https://modelcontextprotocol.io/specification/2025-11-25/basic) | tools/resources/prompts, JSON Schema, transport/auth |
| [MCP authorization](https://modelcontextprotocol.io/specification/2025-11-25/basic/authorization) | audience binding, PKCE, no token passthrough |
| [MCP changelog](https://modelcontextprotocol.io/specification/2025-11-25/changelog) | tasks remain experimental and current stable changes |
| [Agent Skills specification repository](https://github.com/agentskills/agentskills) | portable SKILL.md directory convention |

### 67.3 Sandboxes, workflows, and infrastructure

| Source | Evidence used |
|---|---|
| [Daytona](https://github.com/daytonaio/daytona) | open-source sandbox API/control/compute plane, snapshots, hybrid compute |
| [E2B](https://github.com/e2b-dev/E2B) | secure remote code sandbox SDK pattern |
| [Kubernetes Agent Sandbox](https://github.com/kubernetes-sigs/agent-sandbox) | Sandbox/Claim/Template/WarmPool lifecycle |
| [gVisor](https://gvisor.dev/docs/) | userspace-kernel hardened container tier |
| [Firecracker](https://firecracker-microvm.github.io/) | microVM and jailer isolation tier |
| [Kubernetes Pod Security Standards](https://kubernetes.io/docs/concepts/security/pod-security-standards/) | restricted pod baseline |
| [Temporal](https://github.com/temporalio/temporal) | durable retry/timer/signal pattern evaluated as optional adapter |
| [Restate](https://docs.restate.dev/) and [DBOS architecture](https://docs.dbos.dev/architecture) | alternative durable execution patterns; evidence not to expose one engine publicly |

### 67.4 Models, identity, tenancy, billing, and operations

| Source | Evidence used |
|---|---|
| [LiteLLM documentation](https://docs.litellm.ai/) | optional provider proxy/routing integration |
| [RFC 9700](https://www.rfc-editor.org/rfc/rfc9700.html) | OAuth security best current practice |
| [SPIFFE/SPIRE concepts](https://spiffe.io/docs/latest/spire-about/spire-concepts/) | runner/workload attestation and short-lived identity pattern |
| [PostgreSQL row security](https://www.postgresql.org/docs/current/ddl-rowsecurity.html) | tenant defense-in-depth |
| [Vault response wrapping](https://developer.hashicorp.com/vault/docs/concepts/response-wrapping) | one-use secret handoff pattern |
| [OpenMeter usage events](https://openmeter.io/docs/metering/events/overview) | CloudEvents-style metering export |
| [Stripe usage meters](https://docs.stripe.com/billing/subscriptions/usage-based/how-it-works) | idempotent downstream billing aggregation |
| [OpenTelemetry semantic conventions](https://opentelemetry.io/docs/specs/semconv/) | portable traces/metrics/logs |

### 67.5 Integrations, security, supply chain, and evals

| Source | Evidence used |
|---|---|
| [Slack Events API](https://docs.slack.dev/apis/events-api/) | fast acknowledgement, retries, duplicate handling |
| [Slack agent interaction](https://docs.slack.dev/ai/agent-entry-and-interaction/) | assistant thread UI pattern |
| [GitHub App versus OAuth app](https://docs.github.com/en/apps/oauth-apps/building-oauth-apps/differences-between-github-apps-and-oauth-apps) | scoped short-lived repository installation token |
| [GitHub protected branches](https://docs.github.com/en/repositories/configuring-branches-and-merges-in-your-repository/managing-protected-branches/about-protected-branches) | publication/merge boundary |
| [GitHub webhook best practices](https://docs.github.com/en/webhooks/using-webhooks/best-practices-for-using-webhooks) | signing, delivery ID, replay protection, async handling |
| [OWASP Agentic Top 10 2026](https://genai.owasp.org/resource/owasp-top-10-for-agentic-applications-for-2026/) | goal hijack, tool/identity/supply-chain/code-execution risk taxonomy |
| [NIST AI RMF](https://www.nist.gov/itl/ai-risk-management-framework) | govern/map/measure/manage and TEVV framing |
| [SLSA 1.2](https://slsa.dev/spec/v1.2/) | build provenance maturity |
| [Sigstore verification](https://docs.sigstore.dev/cosign/verifying/verify/) | OCI/artifact signature and digest verification |
| [OpenAI evaluation best practices](https://developers.openai.com/api/docs/guides/evaluation-best-practices) | task-specific evals, human calibration, continuous eval development |

## 68. Explicit non-guarantees

The platform does not promise:

- universal exactly-once external execution;
- portable hidden reasoning/provider-private continuation state;
- exact resume after process/host loss when only transcript is available;
- hostile multi-tenant safety from a plain shared container;
- zero retention by an external provider that does not contractually/technically support it;
- automatic legal/regulatory compliance merely by installation;
- correctness of unverified model claims or citations;
- access to chain-of-thought;
- automatic merge/release/store publication;
- zero cold start for arbitrary images;
- cross-region failover that violates residency;
- unlimited context, storage, cost, tools, or subagents;
- safe autonomous installation of arbitrary third-party code;
- identical infrastructure/capacity between laptop, self-host, and SaaS.

It does promise that these limitations are typed, visible, policy-controlled, and testable rather than silently hidden.

## 69. Final specification review boundary

This 1.0-review document is complete at the product/architecture level. Stakeholder review should challenge:

- whether any required use case is missing;
- whether a normative behavior is wrong;
- whether isolation/open-core/commercial boundaries are acceptable;
- whether UAT evidence is sufficient;
- whether an intentional non-guarantee must become a guarantee.

After review, the next artifact is an implementation master plan that:

1. maps every normative subsystem and UAT ID to implementation epics;
2. chooses concrete languages/libraries only after prototype evidence;
3. orders dependency-critical vertical slices;
4. defines milestones, owners, CI, environments, and release gates;
5. preserves the independent-repository boundary;
6. does not silently reopen accepted architecture.

The implementation master plan is not embedded here so product decisions can be reviewed before estimates and code structure bias them.
