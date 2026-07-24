// Package workers is the CapabilityWorker contract (spec §31.2-31.6, E17 Task 9, WRK-001..007): the
// outbound-enrolled, lease/fenced execution surface for typed CAPABILITY jobs that run OUTSIDE the control
// plane's process/network — the same enrollment + lease/fencing semantics a runner uses (packages/runner +
// coordinator), applied to a macOS host toolchain the container cannot reach.
//
// The security spine, all proven RED-first:
//   - Outbound enrollment only: a worker DIALS the control plane with a one-time token and gets a short-lived
//     workload identity; NO inbound port is ever opened to the worker (gateway.go dials nothing back).
//   - Typed operation is the whole of what a worker may run (§31.5, no tunnel): an operation absent from its
//     capability's Catalog is refused at dispatch, at claim, and at submit — there is no generic
//     connect/proxy/exec surface, so an ordinary sandbox worker cannot be a general tunnel.
//   - Fenced jobs (§31.6): a lease is fenced at the dispatch fence AND the worker's enrollment fence; a
//     result or redeem under a superseded fence is REJECTED (a re-dispatch or a health/capability change cuts
//     the lease). An append-only journal (migration 000039) makes the fence tamper-evident.
//   - Job-scoped short-lived secret HANDLES: a handle names a secret_ref (000031), is scoped to exactly its
//     job, expires with the job deadline, and its VALUE never lands in a receipt, a log, or an evidence
//     bundle.
//
// HONEST CEILING (the plan's most important, §6 leg 3): NO macOS/iOS BUILD is claimed. There is no signing
// cert, no provisioning profile, no store credential anywhere — the apple-build capability is DISABLED in
// discovery and has NO entry in Catalog, so no apple-build job can even be dispatched. What is proven here is
// the outbound-enrolled typed-operation + private-network + no-tunnel + fenced-job invariants. A real signed
// Apple build (ephemeral keychain, result bundle, store publication) is a separate capability + operator leg.
package workers

import (
	"context"
	"errors"
	"time"
)

// Tenant is the org/project a worker and its jobs are scoped to. It comes from the enrolling worker's
// verified enrollment scope, never from anything the worker sends over the wire.
type Tenant struct {
	Organization string
	Project      string
}

// WorkerSpec is what an enrolling worker declares about itself (§31.2). ToolchainDigests pins what the worker
// actually has (e.g. {"swiftc":"..."}) so a job matches a compatible worker; it never carries signing keys.
type WorkerSpec struct {
	Capability        string
	CapabilityVersion string
	OS                string
	Arch              string
	ToolchainDigests  map[string]string
	Capacity          int
	PoolLabel         string
	TrustLabel        string // "sandbox" (default) — the one that must never be usable as a tunnel
}

// Worker is an enrolled worker. LeaseFence is the enrollment fence: a re-enrollment or a health/capability
// change bumps it, cutting any lease the worker still holds.
type Worker struct {
	ID     string
	Tenant Tenant
	Spec   WorkerSpec
	Health string
	Fence  int64
}

// JobSpec is a dispatch request (§31.3). The control plane — never the worker — owns the run/attempt identity
// and the fence. Operation MUST be a typed operation of Capability (Catalog), or DispatchJob refuses it.
type JobSpec struct {
	JobID            string // stable job id, minted when empty
	IdempotencyKey   string
	RunID            string
	AttemptID        string
	Capability       string
	Operation        string
	InputRefs        []string          // input artifact refs
	SecretHandleRefs []string          // secret_refs NAMES (job-scoped handles); never values
	Deadline         time.Time         // the job deadline; the secret handle expires with it
	ResourceLimits   map[string]any
	OutputSchema     map[string]any
	NetworkPolicy    map[string]any
	SideEffectKey    string // destination idempotency for a side-effecting operation
}

// Claim is a leased job handed to a worker. It carries BOTH fences the §31.6 guard checks: JobFence (bumped
// on a re-dispatch) and WorkerFence (bumped on a health/capability change). ReadOnly is derived from the
// operation Catalog, not from anything the worker asserts.
type Claim struct {
	JobID            string
	Tenant           Tenant
	WorkerID         string
	JobFence         int64
	WorkerFence      int64
	Capability       string
	Operation        string
	ReadOnly         bool
	InputRefs        []string
	SecretHandleRefs []string
	Deadline         time.Time
	OutputSchema     map[string]any
	SideEffectKey    string
}

// Outcome is a worker's structured result for a claim. Class is one of "completed", "failed", or "uncertain"
// (an uncertain failure quarantines the job rather than retrying it — §31.6). Receipt is the execution
// receipt (usage, logs digest, output artifact ref) — the worker MUST NOT put a secret value in it.
type Outcome struct {
	Class        string // "completed" | "failed" | "uncertain"
	Operation    string // must equal the claim's operation (defence in depth against a re-typed submit)
	Receipt      map[string]any
	OutputRefs   []string
}

// SecretResolver resolves a secret_ref name to its value within an organization. It is exactly
// identity.SecretStore.Resolve; the workers package depends on this narrow seam to avoid an import cycle. The
// resolved value is handed to the worker for use and NEVER written to the journal, a log, or evidence.
type SecretResolver interface {
	Resolve(ctx context.Context, org, name string) ([]byte, bool, error)
}

var (
	// ErrUntypedOperation is the no-tunnel refusal (§31.5): an operation that is not a typed operation of its
	// capability's Catalog — there is no generic connect/proxy/exec, so a sandbox worker cannot be a tunnel.
	ErrUntypedOperation = errors.New("workers: operation is not a typed operation of the capability (no tunnel)")
	// ErrStaleFence is the §31.6 reject: a result/redeem under a superseded JOB fence (the job was
	// re-dispatched to another worker).
	ErrStaleFence = errors.New("workers: result submitted under a superseded job fence")
	// ErrWorkerFenced is the §31.6 reject for the worker leg: the worker's enrollment fence advanced (a
	// health/capability change cut the lease).
	ErrWorkerFenced = errors.New("workers: worker enrollment fence is stale")
	// ErrHandleNotScoped is the secret-handle scope reject: a handle not named by THIS job.
	ErrHandleNotScoped = errors.New("workers: secret handle is not scoped to this job")
	// ErrHandleExpired is the secret-handle expiry reject: redeemed after the job deadline (the handle
	// expires with the deadline).
	ErrHandleExpired = errors.New("workers: secret handle expired with the job deadline")
	// ErrUnknownCapability is an enrollment/dispatch refusal for a capability the control plane does not type
	// (e.g. apple-build, which is DISABLED and absent from Catalog — no signing/build/store op exists).
	ErrUnknownCapability = errors.New("workers: capability is not advertised by this control plane")
	// ErrNoSuchJob resolves nothing for the given job id + tenant.
	ErrNoSuchJob = errors.New("workers: no such job")
	// ErrNotRetryable rejects a blind retry of a side-effecting operation (only read-only ops retry on
	// another worker; a side-effecting one relies on destination idempotency — §31.6).
	ErrNotRetryable = errors.New("workers: a side-effecting operation is not blindly retryable")
	// ErrNotLeaseholder rejects a submit/redeem whose claim is not the job's CURRENT lease: the latest journal
	// entry must be a 'leased' entry held by exactly this worker. A caller constructing a claim for a job it
	// does not hold the lease on (fences are small guessable ints) is refused here, not just at the fence.
	ErrNotLeaseholder = errors.New("workers: claim does not hold the job's current lease")
	// ErrJobExists refuses a dispatch onto a job_id that already has journal entries: a fresh 'dispatched' at
	// fence 1 would re-open a terminal job into a wedged state. A re-dispatch goes through RedispatchForRetry.
	ErrJobExists = errors.New("workers: job id already has journal entries; re-dispatch via RedispatchForRetry")
)
