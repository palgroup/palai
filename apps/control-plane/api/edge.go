package api

import "net/http"

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
