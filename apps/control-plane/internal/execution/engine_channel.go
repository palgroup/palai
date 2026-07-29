package execution

import (
	"context"
	"time"

	"github.com/palgroup/palai/packages/contracts"
	"github.com/palgroup/palai/packages/coordinator"
	"github.com/palgroup/palai/packages/runner"
)

// AttemptDescriptor is one run attempt the orchestrator executes: the fenced
// run/attempt identity, the pinned engine image, the execution bounds, and — since E24 T4 — the
// tenant the attempt belongs to.
type AttemptDescriptor struct {
	RunID     contracts.RunID
	AttemptID contracts.AttemptID
	Fence     uint64
	// Tenant is the org/project this attempt runs for, and it is here because the runner plane had NO
	// tenant on it at all (§3.6 D8): enrolment carried none, this descriptor carried none, the lease
	// offer carried none, and Dial checked none — so ANY enrolled machine could take ANY tenant's
	// attempt. In a single-runner topology that is a definition; the moment two customers have Macs it
	// is a hole. The orchestrator fills it from the run's own scope, which it has already resolved.
	//
	// IT IS DELIBERATELY NOT ON THE LEASE OFFER, and that is a correction to how the plan described the
	// fix: the refusal is a control-plane decision, and adding a field to the offer would tell a machine
	// something it cannot act on while breaking the byte-identical offer §2 makes non-negotiable.
	// Empty is the pre-E24 attempt (every wire proof, the conformance tier) and matches a machine whose
	// tenant is equally unknown — it is not a wildcard.
	Tenant      coordinator.Tenant
	ImageDigest string
	Limits      runner.Limits
	// WorkspaceHostPath is the host allocation directory this attempt's workspace tools confine to
	// and the runner bind-mounts to /workspace (spec §29.9). Empty means no workspace bound — the
	// pre-E09 behaviour. WorkspaceReadOnly binds a child's read-only snapshot (spec §29.8, T6).
	WorkspaceHostPath string
	WorkspaceReadOnly bool
	// WorkspaceUnsafe marks a §30.13 unsafe-local-bind (REP-012): the host path is deliberately
	// outside the runner's managed allocation root, so the runner skips its containment check. It
	// defaults false — a normal allocation must sit under the runner's root.
	WorkspaceUnsafe bool
	// JobID is the durable response.run job this attempt was claimed under, threaded so the recovery
	// ladder's "exact" rung can exclude the attempt's OWN live lease when it asks whether the ORIGINAL
	// process is still driving the run (spec §26.3 rung 1, E10 T4). Empty for a direct-drive attempt
	// with no claimed job (tests) — then any live sibling job wins exact.
	JobID string
	// PoolID is the runner pool this attempt must run in (E24 T2). The gateway offers it to a machine
	// enrolled in THAT pool and to no other — a pool is a posture, so "the nearest runner" is not a
	// weaker answer, it is the wrong machine. Empty means fleet.DefaultPoolID, which is where a
	// deployment that has configured no pool places every run, so its behaviour is unchanged.
	PoolID string
	// QueuedAt is the moment this attempt's RUN entered the queue — the run's created_at, not this
	// attempt's. It is what orders a pool's waiting attempts (E24 T2: created_at FIFO), and the
	// distinction is the whole point: a run that was requeued (a cordon, a retry, a resume) re-dials
	// with its ORIGINAL timestamp and keeps its place, instead of going to the back of the line every
	// time it is bounced. Zero means "order me by the moment I reached the gateway".
	QueuedAt time.Time
}

// EngineChannel is a handshake-complete, single-attempt frame transport. The first
// frame Receive yields is engine.ready; a clean close returns io.EOF. Implementations
// own the engine lifecycle: the deterministic e2e drives a bare subprocess, and the
// hardened production path (Task 11c) drives the runner gateway — the orchestrator is
// written once against this seam and never learns which.
type EngineChannel interface {
	Send(ctx context.Context, frame contracts.EngineFrame) error
	Receive(ctx context.Context) (contracts.EngineFrame, error)
	Close() error
}

// EngineDialer opens a live channel for one attempt. Dial completes the handshake, so
// the channel it returns is ready for run.start. ctx bounds ONLY the dial and handshake —
// the orchestrator gives it an attempt-scoped deadline and stops honoring it once the
// handshake completes — so the returned channel must not tie its lifetime to ctx.
type EngineDialer interface {
	Dial(ctx context.Context, attempt AttemptDescriptor) (EngineChannel, error)
}
