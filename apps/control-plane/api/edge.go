package api

import (
	"net/http"

	"github.com/palgroup/palai/adapters/integrations/webhook"
)

// EdgeLimits is the §20.12 basic-tier admission-control configuration the composition root resolves
// from the environment and hands NewRouter. Every field defaults to zero = disabled, so a stack that
// sets nothing keeps the pre-E13-T7 behaviour (no limiter, no caps).
//
// The two halves are deliberately distinct (QUO-001 tests them apart): RequestRate* is an INSTANTANEOUS
// per-key request-rate limit (an in-process token bucket, middleware.RateLimit); MaxConcurrentRuns /
// MaxQueuedRuns are per-project admission caps read from durable DB counters at admission time.
//
// ponytail ceiling: the request-rate limiter governs the PUBLIC API surface only. Automation-born runs
// (trigger/webhook/schedule deliveries, and the signed POST /v1/inbound receiver, which mounts outside
// this middleware) are NOT request-rate-limited here — they carry their own AUT-010 ingestion
// backpressure — but they DO admit through the same path and consume the same per-project MaxConcurrentRuns
// / MaxQueuedRuns counters, so the project-level caps still bound them.
type EdgeLimits struct {
	// RequestRatePerSec is the sustained per-API-key request refill (tokens/second); RequestBurst is
	// the bucket depth. RequestRatePerSec <= 0 disables the request-rate limiter.
	RequestRatePerSec float64
	RequestBurst      int
	// MaxConcurrentRuns caps a project's simultaneously-executing (provisioning/running/waiting) root
	// runs; MaxQueuedRuns bounds its queued backlog. Zero on either disables that cap.
	MaxConcurrentRuns int
	MaxQueuedRuns     int
}

// admissionLimits projects the run-admission half of the edge config for the response handler.
func (e EdgeLimits) admissionLimits() AdmissionLimits {
	return AdmissionLimits{MaxConcurrentRuns: e.MaxConcurrentRuns, MaxQueuedRuns: e.MaxQueuedRuns}
}

// AdmissionLimits are the per-project run caps the response-admission path enforces against live DB
// counters. Zero on either field disables that cap.
type AdmissionLimits struct {
	MaxConcurrentRuns int
	MaxQueuedRuns     int
}

// RouterOption configures optional NewRouter behaviour. It is a trailing variadic so existing callers
// (every conformance/component/e2e harness) compile unchanged and opt in only when they pass one.
type RouterOption func(*routerConfig)

type routerConfig struct {
	edge        EdgeLimits
	secrets     SecretRefAPI
	usage       UsageAPI
	modelRoutes ModelRouteAPI
	knowledge   KnowledgeAPI
	metrics     http.Handler
	a2a         http.Handler   // the authed A2A 1.0 surface (E17 T2)
	a2aCard     http.Handler   // the unauthenticated public Agent Card handler
	slack       SlackEventsAPI // the Slack Events API admission bridge (E19 T1); nil ⇒ route unmounted
	// slackInteractions is the Slack interactivity decision bridge (E19 T2); nil ⇒ route unmounted.
	slackInteractions SlackInteractionsAPI
	// queues is the queue-binding admin surface (E19 T6); nil ⇒ routes unmounted, and discovery must not
	// advertise `queues` at all.
	queues QueueConnectionAPI
	// queueResolver vets an outbound destination at registration; nil ⇒ net.DefaultResolver.
	queueResolver webhook.Resolver
	// capabilityWorkers records that this binary SERVES the capability-worker gateway (E17 T9) — on its own
	// listener, not on this router (see WithCapabilityWorkers). It carries no handler because there is
	// nothing for the router to mount; it exists so discovery derives the claim from the live mount.
	capabilityWorkers bool
}

// WithEdgeLimits supplies the §20.12 request-rate limiter and per-project admission caps.
func WithEdgeLimits(e EdgeLimits) RouterOption {
	return func(c *routerConfig) { c.edge = e }
}

// WithSecretRefs mounts the restart-less secret-ref write-path (E13 Task 3). It is a trailing option rather
// than a positional NewRouter param because only production (and its dedicated test) wires it — every other
// caller compiles unchanged, and a stack with no master key leaves it unset so the routes stay unmounted.
func WithSecretRefs(secrets SecretRefAPI) RouterOption {
	return func(c *routerConfig) { c.secrets = secrets }
}

// WithUsage mounts the metering surface (E13 Task 6): the durable budget/quota limits and the
// tenant-scoped view of what has been settled. A trailing option for the same reason as WithSecretRefs —
// only production and its dedicated tests wire it, so every other caller compiles unchanged and a tier
// that passes none leaves the routes unmounted.
//
// The limits themselves are enforced in the admission transaction, NOT here: leaving this option unset
// unmounts the management routes but does not disable a limit a tenant has already set. Enforcement
// lives with the data, so it cannot be bypassed by a caller that mounted a smaller router.
func WithUsage(usage UsageAPI) RouterOption {
	return func(c *routerConfig) { c.usage = usage }
}

// WithModelRoutes mounts the DB-backed model-routing write surface (E13 Task 8): per-project model
// connections and publishable route revisions. A trailing option for the same reason as WithSecretRefs —
// every existing caller compiles unchanged, and a stack that never routes leaves it unset.
func WithModelRoutes(routes ModelRouteAPI) RouterOption {
	return func(c *routerConfig) { c.modelRoutes = routes }
}

// WithKnowledge mounts the knowledge spine (E17 Task 4): knowledge bases, ingest sources, the immutable
// ingest -> FTS index build, ranked retrieval, and the index-revision history. A trailing option for the
// same reason as WithSecretRefs — every existing caller compiles unchanged, and a stack that wires no
// knowledge store leaves the routes unmounted. Discovery advertises `knowledge` STATICALLY as `preview`
// (capabilities.go), like the pre-existing `responses` capability — the maturity flag is a static
// advertisement, not gated on the store being wired.
func WithKnowledge(knowledge KnowledgeAPI) RouterOption {
	return func(c *routerConfig) { c.knowledge = knowledge }
}

// WithA2A mounts the A2A 1.0 server projection (E17 T2, spec §38): the authed surface (message/task
// lifecycle + the authenticated extended card) under /v1/a2a/, and the UNAUTHENTICATED public Agent Card on
// the top mux (a safe published projection — A2A-001 — so it bypasses bearer auth like /healthz). A trailing
// option for the same reason as WithSecretRefs: every existing caller compiles unchanged, and a stack that
// wires no A2A store leaves the surface unmounted. Discovery advertises `a2a` STATICALLY as `preview`
// (capabilities.go); the tier is the T11 exit gate's to recompute, never this task's.
func WithA2A(authed, publicCard http.Handler) RouterOption {
	return func(c *routerConfig) { c.a2a = authed; c.a2aCard = publicCard }
}

// WithCapabilityWorkers declares that this binary SERVES the capability-worker gateway (E17 T9, spec §31):
// the outbound-enrolled enroll/claim/redeem/result surface for out-of-process capability jobs. It takes no
// handler because the gateway is NOT served by this router — it binds its OWN listener, the runner-gateway
// posture, for the reason written at startCapabilityWorkerGateway (main.go): it is the same CLASS of surface
// as the runner (a worker dials in with a one-time enrollment token and carries a short-lived workload
// identity that the /v1 bearer middleware knows nothing about), so putting /capability/* on the public /v1
// edge would be a DIFFERENT security posture than the one E17 T9 built and proved.
//
// So this option is a discovery fact, nothing else: main.go passes it ONLY where
// startCapabilityWorkerGateway returned a bound gateway, which is what makes `capability-workers` derive from
// the live mount rather than a static string (§2, plan §3.5 D14 — the a2a/workspacesCapability posture). It
// does NOT decide the tier: mounting makes a capability advertisABLE, and the E17 T11 CapabilityTierProof
// recomputes its maturity from the WRK claim outcomes.
func WithCapabilityWorkers() RouterOption {
	return func(c *routerConfig) { c.capabilityWorkers = true }
}

// WithQueueConnections mounts the queue-binding admin surface (E19 T6, spec §34.1): register a durable
// inbound consumer binding or an outbound result destination, and list what this project has registered.
//
// It is the ROUTER half of the queue mount. The working halves — the consume→admit bridge and the outbound
// DeliverDue pump — are supervised loops in main.go, not routes; this option is what makes the surface
// reachable and, per §2, what a later discovery change may derive `queues` from. Mounting does NOT raise the
// tier: `queues` stays preview because no broker PRODUCT exists in this tree (E17 §6 leg 5), and only the
// E17 T11 recompute writes the word.
//
// resolver may be nil (⇒ net.DefaultResolver); it exists so a test drives a deterministic egress vet of an
// outbound destination.
func WithQueueConnections(queues QueueConnectionAPI, resolver webhook.Resolver) RouterOption {
	return func(c *routerConfig) { c.queues = queues; c.queueResolver = resolver }
}

// WithSlack mounts the Slack Events API receiver (E19 T1, spec §36) on the UNAUTHENTICATED top mux — its
// auth is the per-request v0 signature, the inbound-webhook posture (see slack.go). A trailing option like
// the rest: a stack that registers no Slack connections leaves the route unmounted, and discovery then does
// not advertise `slack` at all (§2 — the a2a/capability-workers posture). Mounting makes the capability
// advertisABLE; the tier stays whatever the E17 T11 recompute says it is (preview — §6 leg 1 is untouched
// by wiring).
func WithSlack(events SlackEventsAPI) RouterOption {
	return func(c *routerConfig) { c.slack = events }
}

// WithSlackInteractions mounts the Slack interactivity receiver (E19 T2, spec §36) beside the events route on
// the UNAUTHENTICATED top mux — same posture, same v0 signature as its auth (see slack_interactions.go).
//
// It is a SEPARATE option from WithSlack on purpose rather than a fourth method on SlackEventsAPI: the two
// surfaces need different collaborators (the events bridge needs an Admitter; this one needs the coordinator's
// approval spine and an outbound Slack client), so a stack that wires only the inbound half must be able to
// mount only the inbound half. An unmounted route answers 404 rather than 500 on a nil seam.
func WithSlackInteractions(interactions SlackInteractionsAPI) RouterOption {
	return func(c *routerConfig) { c.slackInteractions = interactions }
}

// WithMetrics mounts the Prometheus text-exposition surface (E14 Task 6) at GET /metrics on the
// UNAUTHENTICATED top mux beside /healthz — the same internal-network posture: the production edge
// path-matches `reverse_proxy /v1/*` (deploy/compose/production.yml), so /metrics is reachable to a
// Prometheus on the internal network but never published externally. A trailing option because only
// production (and its dedicated tests) wire the
// collector; every other caller compiles unchanged and mounts no /metrics. The handler exposes
// installation-aggregate series only — no per-tenant labels — so an unauthenticated scrape leaks no
// tenant identity.
func WithMetrics(h http.Handler) RouterOption {
	return func(c *routerConfig) { c.metrics = h }
}
