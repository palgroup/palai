package api

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
	edge    EdgeLimits
	secrets SecretRefAPI
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
