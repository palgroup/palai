package execution

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"

	"github.com/palgroup/palai/apps/control-plane/internal/store"
	"github.com/palgroup/palai/packages/contracts"
	"github.com/palgroup/palai/packages/coordinator"
	"github.com/palgroup/palai/packages/runner"
)

// defaultAttemptLimits are the execution bounds the control plane leases to the runner for
// one response attempt — the proven e2e/gateway-parity values. ponytail: still fixed here. E13 T8 made
// model_routes readable, but a route revision's config carries only the model + connection: the §27.6
// per-target concurrency/rate/latency fields are not stored, so there are no per-route limits to read yet.
var defaultAttemptLimits = runner.Limits{
	WallTimeMS: 60000, MaxStdoutBytes: 1 << 20, MaxStderrBytes: 1 << 16,
	MaxFrameBytes: 1 << 20, MaxMemoryBytes: 1 << 28, MaxProcessCount: 64,
}

// ExecuteRun is the production worker handler: it drives a claimed response.run job to a
// terminal outcome through the orchestrator. Where AdvanceRun (the listener-off wiring) only
// assigns the run, ExecuteRun opens the engine and runs the full kernel: ExecuteAttempt
// applies the same idempotent provision -> start transitions before dialing, so a redelivered
// or reclaimed job resumes at the claim's fence under a fresh attempt id, and any error falls
// to the coordinator's retry/dead-letter policy — no hidden retry. The engine image the
// control plane pins into every lease comes from PALAI_ENGINE_IMAGE (compose deploy config).
//
// The spine and store are the coordinator's durable dependencies the handler interface
// carries; this implementation routes entirely through the orchestrator, which already owns
// both, so they are unused here.
func ExecuteRun(_ *coordinator.Store, _ *store.Store, orch *Orchestrator) coordinator.Handler {
	engineImage := os.Getenv("PALAI_ENGINE_IMAGE")
	return func(ctx context.Context, claim coordinator.Claim, payload []byte) (string, error) {
		var body runJobPayload
		if err := json.Unmarshal(payload, &body); err != nil {
			return "", fmt.Errorf("decode run job payload: %w", err)
		}
		if body.RunID == "" {
			return "", errors.New("run job payload is missing run_id")
		}
		if err := orch.ExecuteAttempt(ctx, AttemptDescriptor{
			RunID:       contracts.RunID(body.RunID),
			AttemptID:   newAttemptID(),
			Fence:       uint64(claim.Fence),
			ImageDigest: engineImage,
			Limits:      defaultAttemptLimits,
			// The claimed job, so the ladder's exact rung excludes this attempt's own live lease.
			JobID: claim.JobID,
		}); err != nil {
			return "", fmt.Errorf("execute run %s: %w", body.RunID, err)
		}
		return "run:" + body.RunID + ":executed", nil
	}
}

// newAttemptID mints a fresh, envelope-valid attempt id (att_ + 16 hex) for one claim, so a
// reclaimed job opens a new attempt rather than reusing a fenced-out one (spec §53.4).
func newAttemptID() contracts.AttemptID {
	var raw [8]byte
	_, _ = rand.Read(raw[:])
	return contracts.AttemptID("att_" + hex.EncodeToString(raw[:]))
}
