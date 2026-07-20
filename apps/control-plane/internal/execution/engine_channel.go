package execution

import (
	"context"

	"github.com/palgroup/palai/packages/contracts"
	"github.com/palgroup/palai/packages/runner"
)

// AttemptDescriptor is one run attempt the orchestrator executes: the fenced
// run/attempt identity, the pinned engine image, and the execution bounds. It carries
// no tenant — the orchestrator resolves scope from the run (spec §39.2).
type AttemptDescriptor struct {
	RunID       contracts.RunID
	AttemptID   contracts.AttemptID
	Fence       uint64
	ImageDigest string
	Limits      runner.Limits
	// WorkspaceHostPath is the host allocation directory this attempt's workspace tools confine to
	// and the runner bind-mounts to /workspace (spec §29.9). Empty means no workspace bound — the
	// pre-E09 behaviour. WorkspaceReadOnly binds a child's read-only snapshot (spec §29.8, T6).
	WorkspaceHostPath string
	WorkspaceReadOnly bool
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
