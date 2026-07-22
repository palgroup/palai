//go:build live

// This file is the E10 Task 8 live detached-child-conversation smoke (CASE=detached-child-conversation).
// It runs only under the `live` build tag, in
// `make test-live-provider PROVIDER=provider-one CASE=detached-child-conversation`, which loads the real
// provider credential from .env.local and stands up a throwaway Postgres + SeaweedFS
// (needs_object_store=1). In ONE real run it drives the DET-001 detached flow end to end against a REAL
// provider: a parent with a config-seeded DETACHED delegation makes a real completion, RELEASES its
// compute (a real checkpoint persisted to REAL S3 + run.waiting; NO engine subprocess held), the child
// runs as a durable job on the REAL provider (its own distinct chatcmpl id), the child terminal WAKES
// the parent, which RESTORES from the S3 checkpoint (ladder rung 2) and folds the child's typed result
// to completion — two DISTINCT real chatcmpl ids. The parent holds NO provider call across its release
// window by construction (its attempt ended; the release is observable as run.waiting + a persisted S3
// checkpoint + a compatible-checkpoint restore rung — what this test asserts).
//
// It lives under the control-plane internal tree (not tests/live/) because it drives the real
// execution.Orchestrator (an internal package Go forbids importing from tests/live/), exactly like its
// sibling checkpoint-restore + coding-tools live smokes.
//
// HONEST CEILINGS (spec §10.2 discipline; brief §e):
//  1. The DETERMINISTIC tier (apps/control-plane/e2e/responses/detached_child_test.go +
//     tests/fault/subagents) already proves release/wake/rebind/exactly-once + child-addressing with a
//     fake provider. This tier CONFIRMS the DETACH + DURABLE half with a REAL provider and REAL S3: two
//     DISTINCT real chatcmpl ids + a real checkpoint in S3 are the live evidence.
//  2. SPAWN IS CONFIG-SEEDED, not model-spontaneous: the delegation is seeded in the run. The proof is
//     DETACH + DURABLE, not the model electing to delegate. dispatchModel now advertises the effective
//     tool set (E12 T1), but model-driven SPONTANEOUS delegation — the `agent` tool advertised and
//     chosen by the model — is a distinct claim a later task owns; this smoke deliberately keeps the
//     spawn config-seeded so its assertion stays honest.
//  3. PARENT↔CHILD CONVERSATION (send_message to the detached child at a tool boundary, DET-002): proven
//     deterministically (TestDetachedChildIdleReceivesSpineMessage). Its live half — a detached child
//     driven multi-step by real advertised tool calls, receiving a spine message at its tool boundary — is
//     bound to the SAME engine-wire tool_call-id follow-up (T1b) as checkpoint-restore: a multi-step child
//     re-threads the assistant tool_call + tool result, which the engine wire (dropped tool_call id) makes
//     malformed for the real chat API, so the child cannot be driven multi-step live until T1b lands. It is
//     NOT gated on any env flag. This smoke proves the DETACH + DURABLE half with the real provider (its
//     parent + child are single-step, so it is unaffected by T1b and PASSES live).
//
// GATED: serialized with every LIVE/fault smoke on the shared :local Docker stack; NOT part of make
// verify / CI. Skips cleanly without creds. The credential is used only as an opaque env-resolved
// secret and never printed.
package live

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	providerone "github.com/palgroup/palai/adapters/models/provider_one"
	"github.com/palgroup/palai/apps/control-plane/internal/artifacts"
	"github.com/palgroup/palai/apps/control-plane/internal/execution"
	"github.com/palgroup/palai/apps/control-plane/internal/store"
	"github.com/palgroup/palai/packages/contracts"
	"github.com/palgroup/palai/packages/coordinator"
	"github.com/palgroup/palai/packages/coordinator/recovery"
	modelbroker "github.com/palgroup/palai/packages/model-broker"
	"github.com/palgroup/palai/packages/runner"
	toolbroker "github.com/palgroup/palai/packages/tool-broker"

	"github.com/palgroup/palai/storage"
)

// seedDetachParent seeds org→project→session→response→run where the run carries a config-seeded
// DETACHED delegation (delegation.emit[0].detach=true) to a distinct child model, so the parent's
// engine emits a detached child.request after its first model step — no model tool-call needed.
func seedDetachParent(t *testing.T, pool *pgxpool.Pool, childModel string) (coordinator.Tenant, string, string, string) {
	t.Helper()
	ctx := context.Background()
	tenant := coordinator.Tenant{Organization: newID("org"), Project: newID("prj")}
	session, response, runID := newID("ses"), newID("resp"), newID("run")
	deleg, _ := json.Marshal(map[string]any{"emit": []map[string]any{{
		"role": "researcher", "objective": "summarize the topic in one sentence",
		"model": childModel, "required": true, "detach": true, "workspace_mode": "none",
	}}})
	do := func(sql string, args ...any) {
		if _, err := pool.Exec(storage.WithSystemScope(ctx), sql, args...); err != nil {
			t.Fatalf("seed exec %q: %v", sql, err)
		}
	}
	do(`INSERT INTO organizations (id) VALUES ($1)`, tenant.Organization)
	do(`INSERT INTO projects (id, organization_id) VALUES ($1, $2)`, tenant.Project, tenant.Organization)
	do(`INSERT INTO sessions (id, organization_id, project_id) VALUES ($1, $2, $3)`, session, tenant.Organization, tenant.Project)
	do(`INSERT INTO responses (id, organization_id, project_id, session_id, state, input) VALUES ($1,$2,$3,$4,'queued',$5)`,
		response, tenant.Organization, tenant.Project, session, []byte(`"Delegate the research, then summarize the result."`))
	do(`INSERT INTO runs (id, organization_id, project_id, session_id, response_id, state, delegation) VALUES ($1,$2,$3,$4,$5,'queued',$6)`,
		runID, tenant.Organization, tenant.Project, session, response, deleg)
	return tenant, session, response, runID
}

// TestLiveDetachedChildConversationRealProvider is CASE=detached-child-conversation (see package + this
// file's ceilings). It drives the detached flow through a real worker so the child + parent-wake jobs
// run, exactly as production does.
func TestLiveDetachedChildConversationRealProvider(t *testing.T) {
	secret := requireEnvDetach(t, credentialEnv)
	engineDir := requireEnvDetach(t, "PALAI_ENGINE_DIR")
	pgURL := requireEnvDetach(t, "PALAI_COMPONENT_POSTGRES_URL")
	s3Endpoint := requireEnvDetach(t, "PALAI_S3_ENDPOINT")
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
		Endpoint: s3Endpoint, Bucket: envOr("PALAI_S3_BUCKET", "palai-recovery-live"),
		Region: envOr("PALAI_S3_REGION", ""), AccessKey: envOr("PALAI_S3_ACCESS_KEY", ""), SecretKey: envOr("PALAI_S3_SECRET_KEY", ""),
	})
	if err != nil {
		t.Fatalf("NewStore() error = %v", err)
	}
	if err := s3.EnsureBucket(ctx); err != nil {
		t.Fatalf("EnsureBucket() error = %v", err)
	}

	// A distinct child model id so the parent and child produce distinguishable real chatcmpl ids.
	childModel := envOr("PALAI_LIVE_CHILD_MODEL", liveModel())
	tenant, sessionID, responseID, runID := seedDetachParent(t, pool, childModel)
	// The project must allow the child model or the required delegation is unroutable.
	allow, _ := json.Marshal(map[string]any{"allowed_models": []string{liveModel(), childModel}})
	if _, err := pool.Exec(storage.WithSystemScope(ctx), `UPDATE projects SET config_policy=$1 WHERE id=$2 AND organization_id=$3`, allow, tenant.Project, tenant.Organization); err != nil {
		t.Fatalf("set allowed models: %v", err)
	}

	broker := modelbroker.New(modelbroker.Config{
		Adapters: map[string]modelbroker.ModelAdapter{"provider-one": providerone.Adapter{}},
		Secrets:  modelbroker.EnvResolver{"provider-one": credentialEnv},
	})
	tools := toolbroker.New()
	newOrch := func() *execution.Orchestrator {
		o := execution.NewOrchestrator(repo, &subprocessDialer{engineDir: engineDir}, broker, tools)
		o.SetModelRoute(execution.ModelRoute{Provider: "provider-one", Model: liveModel(), Secret: modelbroker.SecretRef("provider-one")})
		o.SetCheckpointSink(execution.NewCheckpointSink(s3, recovery.New(pool)))
		return o
	}

	// A real worker drives the run job, the enqueued child job, and the parent-wake job in turn — the
	// production topology, and what proves a single-runner stack runs a detached child (E08 T5 dissolved).
	stop := runDetachWorker(t, repo.Spine(), newOrch, engineDir)
	defer stop()
	if err := repo.Spine().EnqueueRunJob(ctx, tenant, runID); err != nil {
		t.Fatalf("enqueue parent run job: %v", err)
	}

	awaitState(t, pool, tenant, responseID, "completed", 180*time.Second)

	// The parent RELEASED its compute: run.waiting.v1 + a real checkpoint in S3.
	if n := countRows(t, pool, `SELECT count(*) FROM events WHERE response_id=$1 AND type='run.waiting.v1'`, responseID); n < 1 {
		t.Fatalf("parent run.waiting.v1 = %d, want >=1 (the release)", n)
	}
	if _, found, err := repo.Spine().LatestRunCheckpoint(ctx, tenant, runID); err != nil || !found {
		t.Fatalf("no real checkpoint persisted at the release: found=%v err=%v", found, err)
	}
	// The parent RESTORED via the compatible-checkpoint rung (released + resumed, not held inline).
	if lvl := latestRecoveryLevel(t, pool, tenant, sessionID); lvl != string(recovery.LevelCompatibleCheckpoint) {
		t.Fatalf("recovery level = %q, want compatible_checkpoint", lvl)
	}
	// Exactly one child (rebind, not clone), and it ran on the real provider with its own chatcmpl id.
	var childRun string
	if err := pool.QueryRow(storage.WithSystemScope(ctx), `SELECT id FROM runs WHERE parent_run_id=$1 AND organization_id=$2 AND project_id=$3`,
		runID, tenant.Organization, tenant.Project).Scan(&childRun); err != nil {
		t.Fatalf("read child run: %v", err)
	}
	if n := countRows(t, pool, `SELECT count(*) FROM runs WHERE parent_run_id=$1`, runID); n != 1 {
		t.Fatalf("child runs = %d, want 1 (the re-emitted child.request rebinds, never clones)", n)
	}
	parentID := lastProviderRequestID(t, pool, tenant, runID)
	childID := lastProviderRequestID(t, pool, tenant, childRun)
	if parentID == "" || childID == "" || parentID == childID {
		t.Fatalf("want two DISTINCT real chatcmpl ids (parent=%q child=%q)", parentID, childID)
	}
}

// awaitState polls a response's durable state until it reaches want or the deadline elapses.
func awaitState(t *testing.T, pool *pgxpool.Pool, tenant coordinator.Tenant, responseID, want string, within time.Duration) {
	t.Helper()
	deadline := time.Now().Add(within)
	var last string
	for time.Now().Before(deadline) {
		if err := pool.QueryRow(storage.WithSystemScope(context.Background()),
			`SELECT state FROM responses WHERE id=$1 AND organization_id=$2 AND project_id=$3`,
			responseID, tenant.Organization, tenant.Project).Scan(&last); err == nil && last == want {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("response %s state = %q after %s, want %q", responseID, last, within, want)
}

func countRows(t *testing.T, pool *pgxpool.Pool, sql string, args ...any) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(storage.WithSystemScope(context.Background()), sql, args...).Scan(&n); err != nil {
		t.Fatalf("count %q: %v", sql, err)
	}
	return n
}

func requireEnvDetach(t *testing.T, name string) string {
	t.Helper()
	v := envOr(name, "")
	if v == "" {
		t.Skipf("%s is required; run make test-live-provider PROVIDER=provider-one CASE=detached-child-conversation", name)
	}
	return v
}

// runDetachWorker starts a coordinator worker whose handler drives the claimed response.run job through
// a fresh orchestrator (the child and parent-wake jobs are ordinary response.run jobs). It returns a
// stop func the caller defers.
func runDetachWorker(t *testing.T, spine *coordinator.Store, newOrch func() *execution.Orchestrator, _ string) func() {
	t.Helper()
	handler := func(ctx context.Context, claim coordinator.Claim, payload []byte) (string, error) {
		var body struct {
			RunID string `json:"run_id"`
		}
		if err := json.Unmarshal(payload, &body); err != nil {
			return "", err
		}
		desc := execution.AttemptDescriptor{
			RunID: contracts.RunID(body.RunID), AttemptID: contracts.AttemptID(newID("att")), Fence: uint64(claim.Fence),
			ImageDigest: "sha256:live", Limits: runner.Limits{WallTimeMS: 120000, MaxStdoutBytes: 1 << 20, MaxStderrBytes: 1 << 16, MaxFrameBytes: 1 << 20, MaxMemoryBytes: 1 << 28, MaxProcessCount: 64},
			JobID: claim.JobID,
		}
		if err := newOrch().ExecuteAttempt(ctx, desc); err != nil {
			return "", err
		}
		return "run:" + body.RunID + ":executed", nil
	}
	worker := coordinator.NewWorker(spine, coordinator.WorkerConfig{
		Owner: newID("live-detach-worker"), Lease: 60 * time.Second, Heartbeat: 10 * time.Second, PollInterval: 50 * time.Millisecond,
		Retry: coordinator.RetryPolicy{MaxAttempts: 1, BaseBackoff: 20 * time.Millisecond, MaxBackoff: 200 * time.Millisecond},
	}, handler)
	ctx, cancel := context.WithCancel(context.Background())
	go func() { _ = worker.Run(ctx) }()
	return cancel
}
