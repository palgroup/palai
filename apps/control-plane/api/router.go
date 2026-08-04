// Package api is the control-plane HTTP surface. NewRouter composes the middleware
// stack around the response-admission handler; the durable work is delegated to an
// Admitter seam so the HTTP contract is exercised without a database.
package api

import (
	"net"
	"net/http"

	"github.com/palgroup/palai/apps/control-plane/api/middleware"
)

// NewRouter builds the LP-0 HTTP handler. RequestContext is outermost so every
// response — success or problem — carries the correlation headers; Auth runs
// before routing so an unauthenticated request never reaches a handler; the
// idempotency-key requirement is scoped to the mutating route. The event stream is
// a plain GET (no idempotency key) that reads the journal through events.
//
// runner, when non-nil, is the runner gateway surface (enrollment + mTLS session):
// it is mounted under /v1/runner/ ahead of and bypassing the public API auth and
// correlation middleware, because it carries its own one-use-token and mTLS identity.
// It is served over a separate mutually-authenticated listener; binding the CA and that
// listener is Task 12, so production passes nil until then.
func NewRouter(verifier middleware.Verifier, admitter Admitter, events EventReader, sessions SessionManager, bindings BindingRegistrar, agents AgentRegistry, webhooks WebhookAPI, triggers TriggerAPI, schedules ScheduleAPI, tools ToolRegistryAPI, mcp MCPConnectionAPI, skills SkillRegistryAPI, hooks HookAPI, provisioning ProvisioningAPI, artifacts ArtifactAPI, sse SSEConfig, runner http.Handler, toolCallbacks http.Handler, opts ...RouterOption) http.Handler {
	var cfg routerConfig
	for _, opt := range opts {
		opt(&cfg)
	}
	mux := http.NewServeMux()
	responses := &responseHandler{admitter: admitter, limits: cfg.edge.admissionLimits()}
	mux.Handle("POST /v1/responses", middleware.RequireIdempotencyKey(http.HandlerFunc(responses.create)))
	mux.HandleFunc("GET /v1/responses", responses.list)
	mux.HandleFunc("GET /v1/responses/{response_id}", responses.get)
	// Cancel is naturally idempotent (a canceled terminal is monotonic), so it is not wrapped
	// with RequireIdempotencyKey; the OpenAPI cancelResponse operation defines no key parameter.
	mux.HandleFunc("POST /v1/responses/{response_id}/cancel", responses.cancel)
	mux.HandleFunc("GET /v1/capabilities", capabilities(cfg))
	// The machine's own effective configuration (see deployment.go). UNCONDITIONAL, and unlike every
	// mount-gated surface below it that is not a shortcut: it reports THIS PROCESS's environment, which
	// exists whatever else was wired, and a binary that answered nothing here would reproduce the hole it
	// was added to close. It is gated on the `provision` capability inside the handler rather than by a
	// mount, because there is no optional seam to derive a mount from.
	mux.HandleFunc("GET /v1/deployment", deployment(cfg.desired))

	// The DESIRED configuration (E29, migration 000052): what this machine SHOULD be running with. The READ
	// rides GET /v1/deployment above, because desired and effective are one answer and serving them apart
	// would let a client render one against a stale copy of the other. Only the WRITE is its own route, and
	// it is mount-gated: a deployment with no durable spine has nowhere to keep a document, and a route that
	// accepted one and dropped it would be the defect this whole surface exists to expose.
	if cfg.desired != nil {
		dh := &desiredHandler{desired: cfg.desired}
		mux.HandleFunc("PUT /v1/deployment/desired", dh.put)
	}

	// Repository-binding registration (spec §30.1): a project registers the external repository its
	// coding sessions attach via the `repository` field. A durable, unkeyed create — nil in tiers that
	// do not touch bindings (the Docker-free conformance HTTP tier).
	if bindings != nil {
		bh := &bindingHandler{bindings: bindings}
		mux.HandleFunc("POST /v1/repository-bindings", bh.create)
		mux.HandleFunc("GET /v1/repository-bindings", bh.list)
		mux.HandleFunc("GET /v1/repository-bindings/{binding_id}", bh.get)
		// THE LIFECYCLE HALF (E30, migration 000057), and it closes a measured dead end rather than adding
		// a convenience: on a live stack 20 bindings carried no connection_ref and NOTHING could give one
		// to any of them — this block was POST + GET + GET, the store seam was Create/Get/List, and
		// queries/repository_bindings.sql held one INSERT and no UPDATE. A binding registered before its
		// credential existed was unfixable, and since there was no delete either, "register another" left
		// the original in the picker forever (8 of those 20 were duplicates of one repository).
		//
		// THE CREDENTIAL IS THE ONLY MUTABLE FIELD and it gets its own sub-resource rather than a PATCH on
		// the binding. Identity is what preparation_receipts have already asserted about past runs; a
		// general PATCH would put a rotation and a rewrite-of-history behind the same verb.
		//
		// ARCHIVE IS NOT DELETE. preparation_receipts holds a foreign key onto the row, this API has no
		// destructive verb anywhere, and the runner fleet already ships the shape being borrowed
		// (cordon/resume, never delete). What makes it more than a display flag is one clause in
		// RepositoryBindingExists — the run-admission guard — so an archived binding REFUSES NEW RUNS.
		mux.HandleFunc("PUT /v1/repository-bindings/{binding_id}/connection", bh.setConnection)
		mux.HandleFunc("POST /v1/repository-bindings/{binding_id}/archive", bh.archive)
		mux.HandleFunc("POST /v1/repository-bindings/{binding_id}/unarchive", bh.unarchive)
	}

	// The automation-agent management surface (spec §20.2.1, §10, E11 Task 1): AgentProfiles +
	// immutable publishable AgentRevisions + profile-free RunTemplateRevisions. Durable config, not
	// idempotent operations, so no Idempotency-Key. nil in tiers that never touch agents.
	if agents != nil {
		ah := &agentHandler{agents: agents}
		mux.HandleFunc("POST /v1/agents", ah.createProfile)
		mux.HandleFunc("GET /v1/agents", ah.listProfiles)
		mux.HandleFunc("GET /v1/agents/{agent_id}", ah.getProfile)
		mux.HandleFunc("GET /v1/agents/{agent_id}/revisions", ah.listRevisions)
		mux.HandleFunc("POST /v1/agents/{agent_id}/revisions", ah.createRevision)
		mux.HandleFunc("POST /v1/agents/{agent_id}/revisions/{revision_id}/publish", ah.publishRevision)
		mux.HandleFunc("POST /v1/run-templates/{template}/revisions", ah.createTemplateRevision)
		mux.HandleFunc("POST /v1/run-templates/{template}/revisions/{revision_id}/publish", ah.publishTemplateRevision)
	}

	// Trigger management + manual/API delivery ingestion (spec §20.2.2, E11 Task 2). Durable config
	// (create/revise/get) plus a delivery POST that births a run via the SAME admission path as
	// /v1/responses — so the delivery POST carries the Idempotency-Key requirement (per-key delivery
	// dedup, AUT-013, is T6; the header is required by contract here). nil in tiers that never touch
	// triggers. The trigger delivery view is read at GET /v1/trigger-deliveries/{id}.
	if triggers != nil {
		th := &triggerHandler{triggers: triggers}
		mux.HandleFunc("POST /v1/triggers", th.createTrigger)
		mux.HandleFunc("GET /v1/triggers", th.listTriggers)
		mux.HandleFunc("POST /v1/triggers/{trigger_id}/revisions", th.reviseTrigger)
		// PATCH rotates the inbound source-secret handles in place (E11 Task 5) — NOT a revise (rotation must
		// not mint a pipeline revision); it accepts ONLY the two secret refs.
		mux.HandleFunc("PATCH /v1/triggers/{trigger_id}", th.reviseInboundSecret)
		mux.HandleFunc("GET /v1/triggers/{trigger_id}", th.getTrigger)
		mux.Handle("POST /v1/triggers/{trigger_id}/deliveries", middleware.RequireIdempotencyKey(http.HandlerFunc(th.createDelivery)))
		mux.HandleFunc("GET /v1/trigger-deliveries/{delivery_id}", th.getDelivery)
	}

	// Schedule management (spec §33, E11 Task 3): a cron/one-time cadence that fires a trigger. Durable
	// config (create/revise/pause/resume/delete/get) — the create validates the cron + IANA timezone at the
	// edge (a 400, never a stored row); a firing edit is a PATCH that bumps the revision. nil in tiers that
	// never touch schedules. The occurrence log is read at GET /v1/schedules/{id}/occurrences.
	if schedules != nil {
		sh := &scheduleHandler{schedules: schedules}
		mux.HandleFunc("POST /v1/schedules", sh.createSchedule)
		// GET /v1/schedules (E29 T1). Before it, a schedule was findable only by an id its creator kept —
		// and asking for the collection did not even 404, it answered 405, because POST was mounted at this
		// exact address. A surface that says "this address exists, just not for reading" is the sharpest
		// possible statement of the hole.
		mux.HandleFunc("GET /v1/schedules", sh.listSchedules)
		mux.HandleFunc("GET /v1/schedules/{schedule_id}", sh.getSchedule)
		mux.HandleFunc("PATCH /v1/schedules/{schedule_id}", sh.reviseSchedule)
		mux.HandleFunc("POST /v1/schedules/{schedule_id}/pause", sh.pauseSchedule)
		mux.HandleFunc("POST /v1/schedules/{schedule_id}/resume", sh.resumeSchedule)
		mux.HandleFunc("DELETE /v1/schedules/{schedule_id}", sh.deleteSchedule)
		mux.HandleFunc("GET /v1/schedules/{schedule_id}/occurrences", sh.listOccurrences)
	}

	// The E12 extensibility registry management surface (spec §20.2, §28.2-28.4, E12 Task 2): Tool
	// lineages + immutable publishable ToolRevisions + named publishable ToolSetRevisions. Durable config,
	// not idempotent operations, so no Idempotency-Key. nil in tiers that never touch the registry.
	if tools != nil {
		th := &toolHandler{tools: tools}
		mux.HandleFunc("POST /v1/tools", th.createTool)
		mux.HandleFunc("GET /v1/tools", th.listTools)
		mux.HandleFunc("GET /v1/tools/{tool_id}", th.getTool)
		mux.HandleFunc("GET /v1/tool-sets", th.listToolSets)
		// The two E25 T7 reads. They exist because a SHIPPED runbook rested on them:
		// docs/operations/jira-mcp-connection.md §3c said "find the ids with GET /v1/tools, then publish
		// each revision", and no route returned a revision id — so the publish, the pin and the grant that
		// follow it were all unreachable on the public API. The set read is the pins, which the list
		// projection has never carried.
		mux.HandleFunc("GET /v1/tools/{tool_id}/revisions", th.listRevisions)
		mux.HandleFunc("GET /v1/tool-sets/{set}/revisions/{revision_id}", th.getSetRevision)
		// The address POST /v1/tools/{tool_id}/revisions has named in its 201 Location since E12. It was
		// `/v1/tool-revisions/<id>`, a prefix no epic ever mounted, so following the header 404'd.
		//
		// That was one instance of a CLASS, and the class was not swept when the instance was fixed: on
		// 2026-07-31 seven more Location prefixes still answered net/http's own not-found. The rule is now
		// mechanical and total — see location_guard_test.go, which parses every Location this package writes
		// and follows each one against a router this file builds. A Location either resolves, or it is not
		// written; there is no exception list, because the header's absence is honest and a header naming an
		// address nothing serves is not. Five of the seven closed by losing the header and each says why at
		// the write; this one and the queue-connection create closed by gaining the route.
		mux.HandleFunc("GET /v1/tools/{tool_id}/revisions/{revision_id}", th.getRevision)
		mux.HandleFunc("POST /v1/tools/{tool_id}/revisions", th.createRevision)
		mux.HandleFunc("POST /v1/tools/{tool_id}/revisions/{revision_id}/publish", th.publishRevision)
		mux.HandleFunc("POST /v1/tool-sets/{set}/revisions", th.createSetRevision)
		mux.HandleFunc("POST /v1/tool-sets/{set}/revisions/{revision_id}/publish", th.publishSetRevision)
	}

	// The E12 Task 5 MCP connection management surface (spec §28.13-28.14): admin registration of upstream
	// MCP servers + the admin discover action. Deliberately ADMIN-ONLY — there is no model-facing MCP-add or
	// discover tool. nil in tiers that never touch MCP.
	if mcp != nil {
		mh := &mcpConnectionHandler{mcp: mcp}
		mux.HandleFunc("POST /v1/mcp-connections", mh.createConnection)
		mux.HandleFunc("GET /v1/mcp-connections", mh.listConnections)
		mux.HandleFunc("GET /v1/mcp-connections/{id}", mh.getConnection)
		mux.HandleFunc("POST /v1/mcp-connections/{id}/discover", mh.discoverConnection)
	}

	// The E12 Task 7 skills management surface (spec §20.2, §28.15-28.16, TOL-011): skill lineages +
	// install-by-URL of an immutable quarantine-sanitized revision + the enable transition. Install and
	// enable are ADMIN-ONLY — there is no model-facing skill-install tool (a skill is untrusted content,
	// not a tool). nil in tiers that never touch skills.
	if skills != nil {
		sh := &skillHandler{skills: skills}
		mux.HandleFunc("POST /v1/skills", sh.createSkill)
		mux.HandleFunc("GET /v1/skills", sh.listSkills)
		mux.HandleFunc("POST /v1/skills/{skill_id}/revisions", sh.installRevision)
		mux.HandleFunc("POST /v1/skills/{skill_id}/revisions/{revision_id}/enable", sh.enableRevision)
	}

	// The E12 Task 8 hooks management surface (spec §28.17, TOL-012): admin registration of extension points
	// that fire inside the run's single dispatch loop + the admin disable kill-switch. Deliberately ADMIN-ONLY
	// — there is no model-facing hook-register tool (a hook is a project policy control, not a capability the
	// model can grant itself). nil in tiers that never touch hooks.
	if hooks != nil {
		hh := &hookHandler{hooks: hooks}
		mux.HandleFunc("POST /v1/hooks", hh.createHook)
		// The read half (E29 T1). This family shipped in E12 with a create and a kill-switch and NOT ONE GET:
		// a hook fires inside every run's dispatch loop, and neither the set of them nor any one of them
		// could be read back — `disable` was a write whose only confirmation was its own 200. The singular
		// read was CHEAP the whole time (extensions.Store.GetHook was written and component-tested against a
		// real Postgres in E12 and had no production caller until this line); the list was not, and needed a
		// query of its own, because the dispatch loop's read filters out exactly the rows a management list
		// exists to show.
		//
		// The 201 at POST /v1/hooks names /v1/hooks/{id} again for the same reason — see createHook.
		mux.HandleFunc("GET /v1/hooks", hh.listHooks)
		mux.HandleFunc("GET /v1/hooks/{id}", hh.getHook)
		mux.HandleFunc("POST /v1/hooks/{id}/disable", hh.disableHook)
	}

	// The tenancy provisioning surface (spec §39.2, E13 Task 2, TEN-003/MCI-001): organizations, projects
	// (+ the §14 config_policy PATCH write-path), and API keys. Durable identity, not idempotent operations,
	// so no Idempotency-Key. Every route is scoped by the verified key and gated on the `provision`
	// capability, EXCEPT the two that open a tenant — POST /v1/organizations and, since A.2 Task 6,
	// POST /v1/projects — which are gated on `system` and each return a tenant admin key's plaintext once,
	// so a new tenant is provisioned with NO restart. nil in tiers that never provision (the Docker-free
	// conformance tiers).
	if provisioning != nil {
		ph := &provisioningHandler{provisioning: provisioning}
		mux.HandleFunc("POST /v1/organizations", ph.createOrganization)
		mux.HandleFunc("GET /v1/organizations", ph.listOrganizations)
		mux.HandleFunc("GET /v1/organizations/{organization_id}", ph.getOrganization)
		mux.HandleFunc("POST /v1/projects", ph.createProject)
		mux.HandleFunc("GET /v1/projects", ph.listProjects)
		mux.HandleFunc("GET /v1/projects/{project_id}", ph.getProject)
		mux.HandleFunc("PATCH /v1/projects/{project_id}", ph.patchProject)
		mux.HandleFunc("POST /v1/api-keys", ph.createAPIKey)
		mux.HandleFunc("GET /v1/api-keys", ph.listAPIKeys)
		mux.HandleFunc("GET /v1/api-keys/{key_id}", ph.getAPIKey)
		mux.HandleFunc("POST /v1/api-keys/{key_id}/revoke", ph.revokeAPIKey)
	}

	// The artifact surface (spec §22.6, E13 Task 5, DAT-006/MCI-004): the once-never-opened READ half of
	// the E09 write-path — an artifact's metadata, its authenticated streaming byte download, and a
	// response's run-scoped artifact list — plus the image INGEST that opens the write half.
	//
	// THE POST IS WHY AN `image_ref` IS REACHABLE FROM OUTSIDE THIS PROCESS AT ALL. The content contract has
	// declared `image_ref` (protocols/schemas/execution/content.json) and execution.decodeContentParts has
	// resolved it to bytes for as long as both have existed, but every writer of that row lived INSIDE the
	// control plane (internal/extensions' Slack bridge). A client that consumes Palai over `/v1` — which is
	// what an integration relay is — could name an artifact in a run's input and could read one back, and
	// had no verb anywhere that could make one. This is that verb, and it is the whole reason a screenshot
	// pasted at a bot can now reach a model.
	//
	// It carries no Idempotency-Key, matching POST /v1/bots and for the same reason: the id is server-minted
	// and the resource is inert, so the cost of a duplicated create is a stored object, not a second run.
	//
	// Every route is tenant-scoped by the verified key and renders a wrong-tenant/unknown id as a
	// non-disclosing 404. nil in tiers with no object store configured (the Docker-free conformance tiers),
	// so the routes stay unmounted — INCLUDING the ingest, which cannot store bytes with no store to put
	// them in.
	if artifacts != nil {
		ah := &artifactHandler{artifacts: artifacts}
		mux.HandleFunc("POST /v1/artifacts", ah.create)
		mux.HandleFunc("GET /v1/artifacts/{artifact_id}", ah.get)
		mux.HandleFunc("GET /v1/artifacts/{artifact_id}/content", ah.content)
		mux.HandleFunc("GET /v1/responses/{response_id}/artifacts", ah.listForResponse)
	}

	// The tool-call read surface (E30 T2, spec §26.7): a response's tool calls with their names,
	// arguments and results, projected straight from the `tool_calls` ledger. It is the READ half of the
	// gap the journal frames leave open on purpose — the frames carry the NAME so a stream can label a
	// call as it happens, and the unbounded model-authored bytes are asked for here instead of being
	// fanned out to every webhook endpoint and stored immutably per delivery. See tool_calls.go for the
	// measurement that decided it.
	if cfg.toolCalls != nil {
		tch := &toolCallHandler{toolCalls: cfg.toolCalls}
		mux.HandleFunc("GET /v1/responses/{response_id}/tool-calls", tch.listForResponse)
	}

	// The restart-less secret-ref write-path (spec §39.x, E13 Task 3, SEC-002/MCI-002): a tenant POSTs a
	// secret VALUE (write-only — it never comes back) that the resolver chain reads fresh, so a rotation
	// takes effect with no restart; reads return metadata only. Wired via WithSecretRefs (a trailing option)
	// only when a master key is configured, so a stack without one — and every non-production caller —
	// mounts no secret-ref route. Gated on the same `provision` capability as tenancy provisioning.
	if cfg.secrets != nil {
		sh := &secretRefHandler{secrets: cfg.secrets}
		mux.HandleFunc("POST /v1/secret-refs", sh.create)
		mux.HandleFunc("GET /v1/secret-refs", sh.list)
		mux.HandleFunc("GET /v1/secret-refs/{secret_name}", sh.get)
		mux.HandleFunc("POST /v1/secret-refs/{secret_name}/rotate", sh.rotate)
	}

	// The ENVIRONMENT surface (E25 T3, migration 000046): a named group of key→value pairs an agent's
	// shell receives at exec time. Five routes, all `provision`-gated, all in the ProvisionResult
	// envelope. Mounted via WithEnvironments beside WithSecretRefs and from the same store, because an
	// environment value IS a secret_refs version under the derived name `env:<environment_id>:<key>` —
	// there is no second table of ciphertext and no second master key.
	//
	// THE READ ROUTE CARRIES KEY NAMES, VERSIONS AND UPDATE TIMES, AND NO VALUE. That is not a property of
	// this block; it is a property of there being no query that reads one (guarded by
	// storage/ciphertext_sweep_test.go) and no view field that could carry one (guarded by
	// internal/identity/environments_view_test.go). The value arrives only in a POST BODY, never in a path
	// segment, so it cannot land in an access log or a browser history entry.
	//
	// The DELETE removes the BINDING between an environment and a key, never the stored bytes: secret_refs
	// grants no DELETE and re-asserts that REVOKE on every boot, so the versions are retained for audit
	// with nothing naming them. The response body says that in words.
	if cfg.environments != nil {
		eh := &environmentHandler{environments: cfg.environments}
		mux.HandleFunc("POST /v1/environments", eh.create)
		mux.HandleFunc("GET /v1/environments", eh.list)
		mux.HandleFunc("GET /v1/environments/{environment_id}", eh.get)
		mux.HandleFunc("POST /v1/environments/{environment_id}/values", eh.putValue)
		mux.HandleFunc("DELETE /v1/environments/{environment_id}/values/{environment_key}", eh.deleteValue)
	}

	// The metering surface (spec §43, E13 Task 6, BIL-001/BIL-003/QUO-001): the durable budget/quota
	// limits admission enforces, and the tenant's own view of what has been settled. Setting a limit is an
	// administrative act gated on the `provision` capability; reading usage is not (a tenant must be able
	// to see what it has spent). Mounted via WithUsage, so a tier that wires no metering store leaves the
	// routes unmounted — while any limit already stored still binds, because enforcement lives in the
	// admission transaction rather than here.
	// The knowledge spine (E17 Task 4, 17b): knowledge bases, their ingest sources, the immutable ingest ->
	// FTS index build, ranked ACL-first retrieval, and the append-only index-revision history. Mounted via
	// WithKnowledge, so a tier that wires no knowledge store leaves the routes unmounted. Gated on the same
	// `provision` capability as the other org-admin surfaces (retrieval's principal-level auth is T5).
	if cfg.knowledge != nil {
		kh := &knowledgeHandler{knowledge: cfg.knowledge}
		mux.HandleFunc("POST /v1/knowledge-bases", kh.createBase)
		mux.HandleFunc("GET /v1/knowledge-bases", kh.listBases)
		mux.HandleFunc("POST /v1/knowledge-bases/{kb_id}/sources", kh.createSource)
		mux.HandleFunc("GET /v1/knowledge-bases/{kb_id}/sources", kh.listSources)
		mux.HandleFunc("DELETE /v1/knowledge-bases/{kb_id}/sources/{source_id}", kh.deleteSource)
		mux.HandleFunc("POST /v1/knowledge-bases/{kb_id}/sources/{source_id}/ingest", kh.ingest)
		mux.HandleFunc("POST /v1/knowledge-bases/{kb_id}/query", kh.query)
		mux.HandleFunc("GET /v1/knowledge-bases/{kb_id}/index-revisions", kh.listIndexRevisions)
	}

	if cfg.usage != nil {
		uh := &usageHandler{usage: cfg.usage}
		mux.HandleFunc("POST /v1/budgets", uh.setBudget)
		mux.HandleFunc("GET /v1/budgets", uh.listBudgets)
		mux.HandleFunc("POST /v1/quotas", uh.setQuota)
		mux.HandleFunc("GET /v1/quotas", uh.listQuotas)
		mux.HandleFunc("GET /v1/usage", uh.summary)
		mux.HandleFunc("GET /v1/usage/ledger", uh.ledger)
		// The bucketed chart source. A sibling route rather than a mode on GET /v1/usage, so the shipped
		// usage_summary response keeps exactly one shape — see usageHandler.series for the trade.
		mux.HandleFunc("GET /v1/usage/series", uh.series)
	}

	// DB-backed model routing (spec §27.2/§27.6, E13 Task 8, MCI-006): a project binds its own provider
	// connection (a secret REF, never a value) and publishes route revisions, so two projects on ONE stack
	// run different models on different credentials. Wired via WithModelRoutes (a trailing option); a stack
	// that never routes mounts nothing and keeps running on the env deployment default. Gated on the same
	// `provision` capability as tenancy provisioning and secret refs.
	if cfg.modelRoutes != nil {
		mh := &modelRouteHandler{routes: cfg.modelRoutes}
		mux.HandleFunc("POST /v1/model-connections", mh.createConnection)
		mux.HandleFunc("POST /v1/model-routes", mh.createRoute)
		mux.HandleFunc("POST /v1/model-routes/{route_id}/revisions", mh.createRevision)
		mux.HandleFunc("POST /v1/model-routes/{route_id}/revisions/{revision_id}/publish", mh.publishRevision)
		// The E16 T1 read-back half (the E13 T10 write-only gap): GET(one) + LIST for every
		// connection/route/revision. Admin ListView envelope, provision-gated, tenant-scoped under RLS.
		mux.HandleFunc("GET /v1/model-connections", mh.listConnections)
		mux.HandleFunc("GET /v1/model-connections/{connection_id}", mh.getConnection)
		// The credential probe (E29). A real HTTP call to the connection's endpoint, so an operator learns
		// their key is wrong when they paste it rather than when an agent needs it. POST because it leaves
		// the process and stamps the row.
		mux.HandleFunc("POST /v1/model-connections/{connection_id}/verify", mh.verifyConnection)
		// The models list (E29). The provider's OWN catalogue for THIS connection's credential, so a model
		// picker never needs a model name typed into this tree. A GET because it writes nothing — the
		// verify above is a POST for its stamp, not for its egress.
		mux.HandleFunc("GET /v1/model-connections/{connection_id}/models", mh.connectionModels)
		mux.HandleFunc("GET /v1/model-routes", mh.listRoutes)
		mux.HandleFunc("GET /v1/model-routes/{route_id}", mh.getRoute)
		mux.HandleFunc("GET /v1/model-routes/{route_id}/revisions", mh.listRevisions)
		mux.HandleFunc("GET /v1/model-routes/{route_id}/revisions/{revision_id}", mh.getRevision)
	}

	// The Slack-less approval surface (E23 T9, spec §22.4, §26.7): list the gated tool calls a human still
	// owes an answer on, and decide one. INSIDE the auth middleware — a decision carries no source signature
	// of its own (unlike the Slack interactivity receiver, whose auth IS its v0 signature), so the bearer
	// scope is the only tenant authority and the verified key is the deciding principal.
	//
	// Read-only list + two action POSTs, so no Idempotency-Key: a decision carries its own one-shot binding
	// (the request hash), which is a stronger dedupe than a header — a replayed approve finds the call
	// already decided and is refused as void.
	if cfg.approvals != nil {
		ah := &approvalHandler{approvals: cfg.approvals}
		mux.HandleFunc("GET /v1/approvals", ah.list)
		mux.HandleFunc("POST /v1/approvals/{approval_id}/approve", ah.approve)
		mux.HandleFunc("POST /v1/approvals/{approval_id}/deny", ah.deny)
	}

	// The kind-agnostic bot registry (2026-08-03 plan Task 4): a project's registered bots — one row per
	// relay process the console can create. INSIDE the auth middleware like every other operator-facing
	// registration surface here (queues, Slack connections, runners): a registration carries no source
	// signature of its own, so the bearer scope is the only tenant authority. nil in tiers that wire no
	// bot store.
	if cfg.bots != nil {
		bh := &botHandler{bots: cfg.bots}
		mux.HandleFunc("POST /v1/bots", bh.create)
		mux.HandleFunc("GET /v1/bots", bh.list)
		mux.HandleFunc("GET /v1/bots/{bot_id}", bh.get)
		mux.HandleFunc("PATCH /v1/bots/{bot_id}", bh.patch)
		mux.HandleFunc("DELETE /v1/bots/{bot_id}", bh.delete)
	}

	// Credential redemption sits beside the registry but behind its OWN seam and its own capability, and
	// the split is not cosmetic: a stack can wire the registry (so a console can create bots) while wiring
	// no resolver at all, which is what a deployment with no master key has — identity.SecretStore is nil
	// there and there is nothing to redeem with. See bot_credentials.go for the route's whole argument,
	// including why it does not gate on `provision`.
	if cfg.botCredentials != nil {
		bch := &botCredentialsHandler{credentials: cfg.botCredentials}
		mux.HandleFunc("GET /v1/bots/{bot_id}/credentials", bch.get)
	}

	stream := &eventsHandler{reader: events, cfg: sse.withDefaults()}
	mux.HandleFunc("GET /v1/sessions/{session_id}/events", stream.stream)

	// Outbound webhook endpoints + deliveries (spec §21.4-21.6). Durable project configuration and an
	// operator-facing delivery view + idempotent redelivery — nil in tiers that do not exercise
	// webhooks (the Docker-free conformance HTTP tier, the SSE read-path e2e).
	//
	// The singular GET and the DELETE arrived in E29 T3, eighteen epics after the create whose comment
	// already described an endpoint as "operator-visible + deletable". The DELETE is `provision`-gated
	// inside the handler and the GET is not: the read is the singular form of a list any key has been able
	// to enumerate since E11, while destroying configuration is the org-admin act.
	if webhooks != nil {
		wh := &webhookHandler{webhooks: webhooks, resolver: net.DefaultResolver}
		mux.HandleFunc("POST /v1/webhook-endpoints", wh.createEndpoint)
		mux.HandleFunc("GET /v1/webhook-endpoints", wh.listEndpoints)
		mux.HandleFunc("GET /v1/webhook-endpoints/{endpoint_id}", wh.getEndpoint)
		mux.HandleFunc("DELETE /v1/webhook-endpoints/{endpoint_id}", wh.deleteEndpoint)
		mux.HandleFunc("GET /v1/webhook-deliveries", wh.listDeliveries)
		mux.HandleFunc("GET /v1/webhook-deliveries/{delivery_id}", wh.getDelivery)
		mux.HandleFunc("POST /v1/webhook-deliveries/{delivery_id}/redeliver", wh.redeliver)
	}

	// The standalone session resource and its durable commands (spec §9.1, §22.4). Commands
	// carry their own idempotency (command_id), so the POST needs no Idempotency-Key header.
	// nil in tiers that do not exercise sessions (the Docker-free conformance HTTP tier).
	if sessions != nil {
		sh := &sessionHandler{sessions: sessions, accounts: cfg.sessionAccounts}
		mux.HandleFunc("POST /v1/sessions", sh.create)
		mux.HandleFunc("GET /v1/sessions", sh.list)
		mux.HandleFunc("GET /v1/sessions/{session_id}", sh.get)
		// PATCH, not PUT: the body carries one field and replaces only that field (E29).
		mux.HandleFunc("PATCH /v1/sessions/{session_id}", sh.rename)
		mux.HandleFunc("POST /v1/sessions/{session_id}/commands", sh.command)
	}

	// The A2A 1.0 server projection (E17 T2, spec §38): the authed message/task lifecycle + extended card
	// mount under /v1/a2a/ INSIDE the auth middleware, so every A2A operation runs under the bearer scope the
	// server reads (the authenticated identity governs — §38.6). The public Agent Card mounts separately on
	// the unauthenticated top mux below. nil in tiers that wire no A2A store.
	if cfg.a2a != nil {
		mux.Handle("/v1/a2a/", cfg.a2a)
	}

	// The queue-binding admin surface (E19 T6, spec §34.1): register an inbound consumer binding or an
	// outbound result destination, and list this project's bindings. INSIDE the auth middleware — unlike the
	// Slack/inbound receivers, this is an operator surface with no source signature of its own, so the
	// bearer scope is the only tenant authority, and a binding is always created in the caller's own tenant.
	if cfg.queues != nil {
		resolver := cfg.queueResolver
		if resolver == nil {
			resolver = net.DefaultResolver
		}
		qh := &queueConnectionHandler{queues: cfg.queues, resolver: resolver}
		mux.HandleFunc("POST /v1/queue-connections", qh.createConnection)
		mux.HandleFunc("GET /v1/queue-connections", qh.listConnections)
		mux.HandleFunc("GET /v1/queue-connections/{connection_id}", qh.getConnection)
	}

	// The Slack workspace REGISTRATION surface (E19 T9, spec §36): register a workspace binding with its
	// secret_ref handles, and list what this project has registered. INSIDE the auth middleware for the same
	// reason the queue admin surface is — an operator action with no source signature of its own, so the
	// bearer scope is the only tenant authority. The two Slack RECEIVERS mount unauthenticated below; this
	// one must not, and the split is why it is its own option.
	if cfg.slackConnections != nil {
		sch := &slackConnectionHandler{slack: cfg.slackConnections}
		mux.HandleFunc("POST /v1/slack-connections", sch.createConnection)
		mux.HandleFunc("GET /v1/slack-connections", sch.listConnections)
		// The repair half: a binding registered before a field existed (an app_token_ref, say) was previously
		// only fixable with raw SQL against slack_connections, which is not a thing an operator has.
		mux.HandleFunc("GET /v1/slack-connections/{connection_id}", sch.getConnection)
		mux.HandleFunc("PATCH /v1/slack-connections/{connection_id}", sch.reviseConnection)
		mux.HandleFunc("DELETE /v1/slack-connections/{connection_id}", sch.deleteConnection)
	}

	// The runner registry (E24 T1): the read surface for the fleet. Bearer-scoped and RLS-confined, so
	// a caller sees its own tenant's machines and nothing else. The write routes below it are T3's key
	// surface and T5's machine lifecycle — see api/runners.go for why each exists.
	//
	// THE WHOLE BLOCK ADDITIONALLY REQUIRES `system` (Faz A.1 Task 3). The fleet is not a customer
	// surface at all — in a hosted deployment no tenant key may even learn which machines exist, and in
	// a self-hosted one the operator's own key carries the platform capability. That is a DIFFERENT axis
	// from `provision`/`approve` above (RLS + those capabilities still decide what a system-scoped call
	// may do to ITS OWN tenant's fleet); systemOnly is the gate that decides whether a call reaches this
	// block's handlers at all. It wraps at REGISTRATION rather than adding a line inside each handler —
	// a per-handler check misses the route someone adds tomorrow, whereas
	// TestEveryFleetRouteRefusesATenantKey (apps/control-plane/internal/store) derives its route list
	// from THIS file and drives every one of them with a tenant key, so a route added below without
	// going through systemOnly is one that test catches rather than one a reviewer has to remember to
	// check by eye.
	if cfg.runners != nil {
		rh := &runnerHandler{runners: cfg.runners, desired: cfg.desired}
		mux.HandleFunc("GET /v1/runners", systemOnly(rh.listRunners))
		mux.HandleFunc("GET /v1/runners/{runner_id}", systemOnly(rh.getRunner))
		// The pools those machines are in (E24 T2), and THE BIRTH PATH THAT WAS MISSING (E28 T1).
		//
		// THE COMMENT THAT STOOD HERE SAID "Read-only: creating and deleting a pool is T5/T6's", and the same
		// sentence stood over ListRunnerPools in storage/queries/runners.sql. T5 shipped the machine lifecycle,
		// T6 shipped the waiting room, and neither opened either — so a fleet's pools could not be created at
		// all, an `unsandboxed-host` (rented Mac) pool could not exist, and `strict_enrollment` could not be
		// switched on outside two test files. That left `approve` below deciding a state no operator could
		// produce. A comment can name what is true; it cannot schedule work, and this one is written to state
		// rather than to defer.
		//
		// CREATE and PATCH are gated on `provision` like the key surface, because deciding where a tenant's
		// runs execute is org administration. DELETE is absent, deliberately: `runner_pool_keys` carries ON
		// DELETE CASCADE on `runner_pools` (000045 R2), so deleting a pool would silently delete its enrolment
		// keys, and what becomes of the machines whose `pool_id` names it is a separate decision nobody has
		// asked for. PATCH takes ONE field — `strict_enrollment` — and api/runners.go says why `posture` is
		// not among them.
		mux.HandleFunc("GET /v1/runner-pools", systemOnly(rh.listRunnerPools))
		mux.HandleFunc("POST /v1/runner-pools", systemOnly(rh.createRunnerPool))
		mux.HandleFunc("PATCH /v1/runner-pools/{pool_id}", systemOnly(rh.patchRunnerPool))
		// The pool's ENROLMENT KEYS (E24 T3) — the one write half of this surface, and the reason it
		// exists is that a fleet credential has no other home: the runner plane authenticates machines,
		// not people, so minting and revoking a key is an operator action over the public API or it is
		// raw SQL. Gated on the `provision` capability like every other org-admin surface. The value is
		// returned by the create route ONCE and by nothing else; a revoke names the key by id, so no
		// route anywhere takes a key value as input.
		mux.HandleFunc("POST /v1/runner-pools/{pool_id}/keys", systemOnly(rh.mintPoolKey))
		mux.HandleFunc("GET /v1/runner-pools/{pool_id}/keys", systemOnly(rh.listPoolKeys))
		// THE CONFIGURATION OF A POOL AND OF ONE MACHINE. They are reads only: the write is
		// PUT /v1/deployment/desired carrying `plane` and `scope_id`, so all three scopes go through one
		// validator. Registered only when this deployment has somewhere to keep a document — see
		// runnerHandler.desired for why an absent route beats one that always answers "none".
		if cfg.desired != nil {
			mux.HandleFunc("GET /v1/runner-pools/{pool_id}/desired", systemOnly(rh.desiredForScope(planeRunnerPool, "pool_id")))
			mux.HandleFunc("GET /v1/runners/{runner_id}/desired", systemOnly(rh.desiredForScope(planeRunnerMachine, "runner_id")))
		}
		mux.HandleFunc("POST /v1/runner-pool-keys/{key_id}/revoke", systemOnly(rh.revokePoolKey))
		// THE MACHINE LIFECYCLE (E24 T5), and the reason it is here is §3.6 D15: `Revoke()` was SAN-011's
		// hard stop for a compromised runner, implemented, tested, registered in the UAT catalogue, and
		// reachable from nothing at all — the same shape as E19 T9's CreateSlackConnection and E23's
		// DecideToolApproval. These three routes are what make it reachable.
		//
		// One pattern per verb, so the action reaching the store is a literal in this file and an unknown verb
		// is a 404 from the mux rather than a string somebody has to remember to validate. Gated on
		// `provision` like every other org-admin surface, because taking a Mac out of service and
		// decommissioning one are org administration.
		mux.HandleFunc("POST /v1/runners/{runner_id}/cordon", systemOnly(rh.setRunnerState("cordon")))
		mux.HandleFunc("POST /v1/runners/{runner_id}/resume", systemOnly(rh.setRunnerState("resume")))
		mux.HandleFunc("POST /v1/runners/{runner_id}/revoke", systemOnly(rh.setRunnerState("revoke")))
		// THE WAITING ROOM'S DOOR (E24 T6): the human who admits a machine that enrolled into a pool with
		// `strict_enrollment`. It is a route of its own rather than a fourth verb on setRunnerState because it
		// carries a different gate — `approve`, not `provision`, since a provision key can rewrite the approver
		// list it would be checked against — and because a decision carries the deciding PRINCIPAL, which the
		// three verbs above do not. api/runners.go's approveRunner says why it is not E23's throat.
		mux.HandleFunc("POST /v1/runners/{runner_id}/approve", systemOnly(rh.approveRunner))
	}

	var root http.Handler = mux
	root = middleware.Auth(verifier)(root)
	// The §20.12 request-rate limiter sits INSIDE RequestContext (so a shed 429 still carries the
	// correlation id) but OUTSIDE Auth (so a flood is rejected before the credential DB read). A
	// zero rate leaves it a pass-through, so a stack that configures no edge limits is unchanged.
	root = middleware.RateLimit(cfg.edge.RequestRatePerSec, cfg.edge.RequestBurst)(root)
	root = middleware.RequestContext(root)

	// /healthz is an unauthenticated liveness probe the Compose stack's healthcheck
	// polls; it carries no contract surface (not in the OpenAPI spec) and bypasses auth
	// and correlation so a probe needs no credential. The runner gateway, when present,
	// mounts ahead of the public router with its own token/mTLS identity (see doc above).
	top := http.NewServeMux()
	top.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		_, _ = w.Write([]byte("ok"))
	})
	// /metrics is the Prometheus text-exposition surface (E14 Task 6). It rides the SAME
	// unauthenticated top mux as /healthz — internal-network only: the production edge path-matches
	// `reverse_proxy /v1/*` (deploy/compose/production.yml), so neither /metrics nor /healthz is
	// proxied externally. It carries installation-aggregate series with no per-tenant labels, so an
	// unauthenticated scrape leaks no tenant identity. Wired via WithMetrics; unset in every tier that
	// passes no collector, so the route simply stays unmounted there.
	if cfg.metrics != nil {
		top.Handle("GET /metrics", cfg.metrics)
	}
	// The signed inbound-webhook receiver (spec §20.2.2/§21.7, E11 Task 5): its auth IS the per-source HMAC
	// signature, so it mounts on the UNAUTHENTICATED top mux beside /healthz — the sole such precedent —
	// bypassing middleware.Auth. Only Auth is bypassed: it is still wrapped in RequestContext so its problem
	// bodies + responses carry the correlation id. An unresolvable/unauthenticated source is a generic 404
	// (no config oracle). NOTE: no OpenAPI operation is declared for this route — the whole automation
	// surface (triggers/webhooks/schedules, T2-T5) has zero OpenAPI ops, so an inbound-only op would be
	// asymmetric; documenting all four consistently is a deferred E11-exit follow-up (review minor-4).
	if triggers != nil {
		ih := &inboundHandler{triggers: triggers}
		top.Handle("POST /v1/inbound/{trigger_id}", middleware.RequestContext(http.HandlerFunc(ih.receive)))
	}
	// The signed remote-tool result callback (spec §28.24, E12 T4): like the inbound receiver its auth IS
	// the per-operation HMAC signature + one-use token, so it mounts on the UNAUTHENTICATED top mux
	// (bypassing middleware.Auth) but stays wrapped in RequestContext for the correlation id. It is
	// ack-only and returns a generic 404 for an unknown operation/token (no config oracle). nil in tiers
	// that never touch remote tools.
	if toolCallbacks != nil {
		top.Handle("POST /v1/tool-callbacks/{operation_id}", middleware.RequestContext(toolCallbacks))
	}
	// The Slack Events API receiver (E19 T1, spec §36): its auth IS the per-request v0 signature, so — like
	// the signed inbound receiver and the tool callback — it mounts on the UNAUTHENTICATED top mux, bypassing
	// middleware.Auth but still wrapped in RequestContext so its problem bodies carry the correlation id. An
	// unresolvable or unauthenticated workspace is a generic 404 (no config oracle). nil in every tier that
	// wires no Slack bridge, which is also what keeps `slack` out of discovery there.
	if cfg.slack != nil {
		sh := &slackHandler{slack: cfg.slack}
		top.Handle("POST /v1/slack/events", middleware.RequestContext(http.HandlerFunc(sh.receive)))
	}
	// The Slack interactivity receiver (E19 T2, spec §36): the same unauthenticated posture as the events
	// route above, and the same v0 signature as its auth — but over a form body rather than JSON, which is
	// why it is its own handler (slack_interactions.go states the contract and the inference behind the
	// verify-then-decode order). Separately gated so a stack can mount the inbound half alone.
	if cfg.slackInteractions != nil {
		ih := &slackInteractionsHandler{slack: cfg.slackInteractions}
		top.Handle("POST /v1/slack/interactions", middleware.RequestContext(http.HandlerFunc(ih.receive)))
	}
	// The A2A public Agent Card (E17 T2, spec §38.1): a safe published projection carrying no internal detail
	// (A2A-001), so — like /healthz and the signed inbound receiver — it mounts on the UNAUTHENTICATED top mux
	// bypassing middleware.Auth (still wrapped in RequestContext for the correlation id). This exact-path route
	// is more specific than the `/` fallthrough, so it intercepts the card before the authed router; every
	// other A2A route requires the bearer scope on the authed mux above.
	if cfg.a2aCard != nil {
		top.Handle("GET /v1/a2a/interfaces/{interface_id}/agent-card.json", middleware.RequestContext(cfg.a2aCard))
	}
	if runner != nil {
		top.Handle("/v1/runner/", runner)
	}
	top.Handle("/", root)
	return top
}

// systemOnly wraps a handler so the platform capability is required before it runs (Faz A.1 Task 3). The
// fleet is operator surface: in a hosted deployment no customer key may read which machines exist, and in
// a self-hosted one the operator's own key carries `system`. Registering the gate as a wrapper rather than
// a line inside each handler is deliberate — a new fleet route added below inherits it by being registered
// through the same helper, and TestEveryFleetRouteRefusesATenantKey fails if one is not.
func systemOnly(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		scope, ok := middleware.ScopeFrom(r.Context())
		if !ok {
			middleware.WriteProblem(w, r, http.StatusUnauthorized, "authentication_required", "a bearer API key is required")
			return
		}
		if !scope.HasSystem() {
			middleware.WriteProblem(w, r, http.StatusForbidden, "insufficient_scope", "this API key lacks the system capability")
			return
		}
		h(w, r)
	}
}
