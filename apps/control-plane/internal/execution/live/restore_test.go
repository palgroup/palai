//go:build live

// Package live is the E10 Task 4 live checkpoint-restore smoke. It runs only under the `live` build
// tag, in `make test-live-provider PROVIDER=provider-one CASE=checkpoint-restore`, which loads the
// real provider credential from .env.local and stands up a throwaway Postgres + SeaweedFS
// (needs_object_store=1). In ONE real run it drives the recovery ladder end to end against a REAL
// provider: a forced-tool run persists a checkpoint at the tool boundary to REAL S3; the engine
// uv-subprocess is SIGKILLed right after the checkpoint is durable; a fresh attempt RESTORES the
// checkpoint (ladder rung 2, run.restore) and the run completes on the real provider — with the
// completed tool NOT re-run and a complete §26.12 RecoveryProof journaled.
//
// It lives under the control-plane internal tree (not tests/live/) because the smoke drives the real
// execution.Orchestrator — the owner of the ladder/restore/checkpoint — which is an internal package
// (Go forbids importing it from tests/live/). Its sibling coding-tools live smoke lives here for the
// same reason.
//
// HONEST CEILINGS (spec §10.2 discipline):
//  1. The deterministic tier (apps/control-plane/e2e/responses recovery_ladder + pause_checkpoint)
//     already proves the ladder, restore, drain, and proof with a fake provider + hook-driven kill — a
//     restore proof needs no provider-realness. This tier CONFIRMS it with a REAL provider and REAL
//     S3: two DISTINCT real chatcmpl ids (before and after the kill) are the live evidence.
//  2. The kill is boundary-hook'd (post-persist), not a mid-window race — mid-window kill matrices are
//     T5/T6.
//  3. SPONTANEOUS TOOL CALL (E12 T1): dispatchModel now advertises the run's effective tool set, so the
//     real provider is offered recovery_note (seedRun puts it in the project's default_tools) and calls
//     it of its own choice to reach the tool boundary — no forcing. Spontaneity is probabilistic: a run
//     where the model declines to call the tool never reaches the checkpoint and the smoke re-runs; a
//     green run is the proof. The deterministic tier already proves the recovery behaviour it exercises.
//
// GATED: serialized with every LIVE/fault smoke on the shared :local Docker stack; NOT part of make
// verify / CI. Skips cleanly without creds. The credential is used only as an opaque env-resolved
// secret and never printed.
package live

import (
	"context"
	"os"
	"strings"
	"testing"

	providerone "github.com/palgroup/palai/adapters/models/provider_one"
	"github.com/palgroup/palai/apps/control-plane/internal/artifacts"
	"github.com/palgroup/palai/apps/control-plane/internal/execution"
	"github.com/palgroup/palai/apps/control-plane/internal/store"
	"github.com/palgroup/palai/packages/contracts"
	"github.com/palgroup/palai/packages/coordinator/recovery"
	modelbroker "github.com/palgroup/palai/packages/model-broker"
	"github.com/palgroup/palai/packages/runner"
	toolbroker "github.com/palgroup/palai/packages/tool-broker"
)

const credentialEnv = "OPENAI_API_KEY"

func liveModel() string {
	if m := os.Getenv("PALAI_LIVE_MODEL"); m != "" {
		return m
	}
	return "gpt-4o-mini"
}

func requireEnv(t *testing.T, name string) string {
	t.Helper()
	v := os.Getenv(name)
	if v == "" {
		t.Skipf("%s is required; run make test-live-provider PROVIDER=provider-one CASE=checkpoint-restore", name)
	}
	return v
}

// TestLiveCheckpointRestoreRealProvider is CASE=checkpoint-restore (see the package ceilings).
func TestLiveCheckpointRestoreRealProvider(t *testing.T) {
	secret := requireEnv(t, credentialEnv)
	engineDir := requireEnv(t, "PALAI_ENGINE_DIR")
	pgURL := requireEnv(t, "PALAI_COMPONENT_POSTGRES_URL")
	s3Endpoint := requireEnv(t, "PALAI_S3_ENDPOINT")
	_ = secret // resolved through the env secret resolver; never referenced directly

	ctx := context.Background()
	repo, err := store.Open(ctx, pgURL)
	if err != nil {
		t.Fatalf("store.Open() error = %v", err)
	}
	t.Cleanup(repo.Close)
	if err := repo.Migrate(ctx); err != nil {
		t.Fatalf("Migrate() error = %v", err)
	}
	pool := repo.Spine().Pool()

	s3, err := artifacts.NewStore(artifacts.Config{
		Endpoint:  s3Endpoint,
		Bucket:    envOr("PALAI_S3_BUCKET", "palai-recovery-live"),
		Region:    os.Getenv("PALAI_S3_REGION"),
		AccessKey: os.Getenv("PALAI_S3_ACCESS_KEY"),
		SecretKey: os.Getenv("PALAI_S3_SECRET_KEY"),
	})
	if err != nil {
		t.Fatalf("NewStore() error = %v", err)
	}
	if err := s3.EnsureBucket(ctx); err != nil {
		t.Fatalf("EnsureBucket() error = %v", err)
	}

	tenant, sessionID, _, runID := seedRun(t, pool)

	broker := modelbroker.New(modelbroker.Config{
		Adapters: map[string]modelbroker.ModelAdapter{"provider-one": providerone.Adapter{}},
		Secrets:  modelbroker.EnvResolver{"provider-one": credentialEnv},
	})
	tool := &countingTool{}
	tools := toolbroker.New(tool.tool())

	newOrch := func(dialer execution.EngineDialer) *execution.Orchestrator {
		o := execution.NewOrchestrator(repo, dialer, broker, tools)
		o.SetModelRoute(execution.ModelRoute{Provider: "provider-one", Model: liveModel(), Secret: modelbroker.SecretRef("provider-one")})
		o.SetCheckpointSink(execution.NewCheckpointSink(s3, recovery.New(pool)))
		return o
	}

	// --- Attempt 1: run to the tool boundary, persist a checkpoint, then SIGKILL the engine. ---
	if err1 := newOrch(&subprocessDialer{engineDir: engineDir, killAfterCheckpoint: true}).ExecuteAttempt(ctx, descriptor(runID, 1)); err1 == nil {
		t.Fatal("attempt 1 should fail: the engine is SIGKILLed after the checkpoint persists")
	}
	if _, found, err := repo.Spine().LatestRunCheckpoint(ctx, tenant, runID); err != nil || !found {
		t.Fatalf("no checkpoint persisted at the tool boundary before the kill: found=%v err=%v", found, err)
	}
	firstProviderID := lastProviderRequestID(t, pool, tenant, runID)
	if firstProviderID == "" {
		t.Fatal("attempt 1 recorded no real provider_request_id (chatcmpl-...)")
	}
	if tool.runs() != 1 {
		t.Fatalf("tool ran %d times on attempt 1, want 1", tool.runs())
	}

	// --- Attempt 2: restore from the checkpoint (ladder rung 2) and complete on the real provider. ---
	if err := newOrch(&subprocessDialer{engineDir: engineDir}).ExecuteAttempt(ctx, descriptor(runID, 2)); err != nil {
		t.Fatalf("attempt 2 (restore) error = %v", err)
	}
	if lvl := latestRecoveryLevel(t, pool, tenant, sessionID); lvl != string(recovery.LevelCompatibleCheckpoint) {
		t.Fatalf("recovery level = %q, want compatible_checkpoint (a real restore, not transcript-only)", lvl)
	}
	if proof := recoveryProof(t, pool, tenant, sessionID); !proof.Complete() {
		t.Fatalf("RecoveryProof is not complete: %+v", proof)
	}
	if tool.runs() != 1 {
		t.Fatalf("tool ran %d times total, want 1 (the completed tool must not replay after restore)", tool.runs())
	}
	secondProviderID := lastProviderRequestID(t, pool, tenant, runID)
	if secondProviderID == firstProviderID || secondProviderID == "" {
		t.Fatalf("want a distinct second real chatcmpl id after restore, got %q (first %q)", secondProviderID, firstProviderID)
	}
}

func envOr(name, fallback string) string {
	if v := os.Getenv(name); v != "" {
		return v
	}
	return fallback
}

// countingTool is a side-effect-free tool whose Invoke increments a counter, so the smoke proves the
// completed tool is not re-executed after a restore.
type countingTool struct{ count int }

func (c *countingTool) tool() toolbroker.Tool {
	return toolbroker.Tool{
		Name:         "recovery_note",
		InputSchema:  map[string]any{"type": "object", "properties": map[string]any{"note": map[string]any{"type": "string"}}, "required": []any{"note"}, "additionalProperties": true},
		OutputSchema: map[string]any{"type": "object", "properties": map[string]any{"ok": map[string]any{"type": "boolean"}}, "required": []any{"ok"}, "additionalProperties": false},
		Invoke: func(map[string]any) (map[string]any, error) {
			c.count++
			return map[string]any{"ok": true}, nil
		},
	}
}

func (c *countingTool) runs() int { return c.count }

func descriptor(runID string, fence uint64) execution.AttemptDescriptor {
	return execution.AttemptDescriptor{
		RunID:       contracts.RunID(runID),
		AttemptID:   contracts.AttemptID(newID("att")),
		Fence:       fence,
		ImageDigest: "sha256:" + strings.Repeat("a", 64),
		Limits:      runner.Limits{WallTimeMS: 90000, MaxStdoutBytes: 1 << 20, MaxStderrBytes: 1 << 16, MaxFrameBytes: 1 << 20, MaxMemoryBytes: 1 << 28, MaxProcessCount: 64},
	}
}
