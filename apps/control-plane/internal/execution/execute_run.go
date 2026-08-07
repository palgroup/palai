package execution

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/palgroup/palai/apps/control-plane/internal/store"
	"github.com/palgroup/palai/packages/contracts"
	"github.com/palgroup/palai/packages/coordinator"
	"github.com/palgroup/palai/packages/runner"
)

// defaultEngineWallTime is how long ONE attempt's engine may run before the supervisor kills it.
//
// ‼️ IT WAS SIXTY SECONDS, FIXED, FOR EVERY RUN — and that is below the honest duration of the work it
// bounds. Measured on a live plane 2026-08-07, successful runs that clone a repository, call a model
// several times, write a file and commit it took 94.7s, 80.0s, 30.5s, 19.0s and 13.9s end to end; a
// plain answer takes 9-15s. Two of those five were over the bound, and they completed only because the
// retry ladder gave them another attempt after the first had been killed mid-work. That is what the
// intermittent coding failure looked like from the outside once the stdin-ordering hang was gone.
//
// FIVE MINUTES IS ~3x THE SLOWEST MEASURED HONEST RUN, the same margin scripts/test/component chose over
// its own slowest pass, and it still cuts a genuinely wedged engine well inside a human's patience.
const defaultEngineWallTime = 5 * time.Minute

// EngineWallTime reads the operator's bound, falling back to the measured default.
//
// ‼️ IT IS A KNOB BECAUSE THE RIGHT VALUE IS A PROPERTY OF THE DEPLOYMENT, not of this code: model
// latency, repository size and machine speed all move it, and a fixed number that kills legitimate work
// is the defect this replaced. PALAI_SANDBOX_WALL_TIME is the same decision one layer down for a single
// shell call, with the same grammar.
//
// AN UNPARSEABLE VALUE IS THE DEFAULT AND NOT A ZERO, deliberately: `runner.Limits` refuses a
// non-positive wall time, so coercing "5min" to 0 would take the deployment from "runs are slow" to
// "nothing runs at all" — the shell posture's own note records that exact trap.
func EngineWallTime() time.Duration {
	if raw := os.Getenv("PALAI_ENGINE_WALL_TIME"); raw != "" {
		if d, err := time.ParseDuration(raw); err == nil && d > 0 {
			return d
		}
	}
	return defaultEngineWallTime
}

// attemptLimits are the execution bounds the control plane leases to the runner for one response
// attempt — the proven e2e/gateway-parity values, with the wall time read per call so a deployment that
// changes it does not need this constant edited. E13 T8 made model_routes readable, but a route
// revision's config carries only the model + connection: the §27.6 per-target concurrency/rate/latency
// fields are not stored, so there are no per-route limits to read yet.
func attemptLimits() runner.Limits {
	return runner.Limits{
		WallTimeMS: EngineWallTime().Milliseconds(), MaxStdoutBytes: 1 << 20, MaxStderrBytes: 1 << 16,
		MaxFrameBytes: 1 << 20, MaxMemoryBytes: 1 << 28, MaxProcessCount: 64,
	}
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
			Limits:      attemptLimits(),
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
