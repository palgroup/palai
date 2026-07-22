//go:build live

// This file is CASE=hook-deny-visible, the E12 Task 8 approved live smoke (spec §28.17, TOL-012): a REAL
// provider spontaneously calls a tool, a before_tool POLICY hook DENIES it, and the model sees the structured
// control-plane denial mid-run. It reuses the spontaneous-tool-roundtrip harness (the project advertises the
// file tool; the prompt instructs but does NOT force), and registers a before_tool policy hook whose
// platform_inline "deny_all" handler blocks every tool the model spontaneously calls.
//
// HONEST CEILING: the hook worker is OUR fixture (the deterministic, network-less deny_all handler) — what is
// proven LIVE is the REAL model seeing a REAL control-plane deny mid-run (a policy.denied.v1 journal + NO
// executed tool + no file written), not a tenant hook worker's behavior. Spontaneity is probabilistic: a run
// where the model declines to call the tool is RED and re-run; a GREEN run IS the proof. Single provider
// (provider-one); second-provider parity is E16.
//
// GATED: serialized with every LIVE smoke on the shared Docker stack; NOT part of make verify / CI. Skips
// cleanly without creds. The credential is an opaque env-resolved secret, never printed.
package live

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	providerone "github.com/palgroup/palai/adapters/models/provider_one"
	"github.com/palgroup/palai/apps/control-plane/internal/execution"
	"github.com/palgroup/palai/apps/control-plane/internal/execution/tools"
	"github.com/palgroup/palai/apps/control-plane/internal/extensions"
	"github.com/palgroup/palai/apps/control-plane/internal/store"
	"github.com/palgroup/palai/packages/coordinator"
	modelbroker "github.com/palgroup/palai/packages/model-broker"
	toolbroker "github.com/palgroup/palai/packages/tool-broker"

	"github.com/palgroup/palai/storage"
)

// TestLiveHookDenyVisibleRealProvider is CASE=hook-deny-visible (see the file ceilings).
func TestLiveHookDenyVisibleRealProvider(t *testing.T) {
	secret := requireEnv(t, credentialEnv)
	engineDir := requireEnv(t, "PALAI_ENGINE_DIR")
	pgURL := requireEnv(t, "PALAI_COMPONENT_POSTGRES_URL")
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

	alloc := t.TempDir()
	tenant, sessionID, respID, runID := seedSpontaneousRun(t, pool)

	// Register a before_tool POLICY hook whose platform_inline deny_all handler blocks every tool call. This
	// is the fixture: deterministic, network-less — the LIVE bond is the real model seeing its deny.
	ext := extensions.New(pool)
	ext.SetHookHandlers(extensions.PlatformHookHandlers())
	if _, err := ext.CreateHook(ctx, tenant.Organization, tenant.Project,
		[]byte(`{"name":"deny-tools","hook_point":"before_tool","category":"policy","executor":"platform_inline","config":{"handler":"deny_all"}}`)); err != nil {
		t.Fatalf("register before_tool deny hook: %v", err)
	}

	broker := modelbroker.New(modelbroker.Config{
		Adapters: map[string]modelbroker.ModelAdapter{"provider-one": providerone.Adapter{}},
		Secrets:  modelbroker.EnvResolver{"provider-one": credentialEnv},
	})
	tb := toolbroker.New(tools.FileTool())

	orch := execution.NewOrchestrator(repo, &subprocessDialer{engineDir: engineDir}, broker, tb)
	orch.SetModelRoute(execution.ModelRoute{Provider: "provider-one", Model: liveModel(), Secret: modelbroker.SecretRef("provider-one")})
	orch.SetHookFirer(ext)

	desc := descriptor(runID, 1)
	desc.WorkspaceHostPath = alloc
	// The real engine + provider drive to a terminal outcome. A run where the model retries the (always-denied)
	// tool and eventually gives up still terminates; an execute error is tolerated (the evidence is in the DB —
	// what matters is the model saw the deny, not that it completed a productive task it was blocked from).
	if err := orch.ExecuteAttempt(ctx, desc); err != nil {
		t.Logf("hook-deny-visible: ExecuteAttempt returned %v (tolerated — the deny evidence is asserted from the DB)", err)
	}
	_ = respID

	// (a) The model SPONTANEOUSLY chose the advertised tool (probabilistic — re-run if red).
	if !firstResultHasToolCalls(t, pool, tenant, runID) {
		dumpRunDiagnostics(t, pool, tenant, sessionID, runID)
		t.Fatal("the first committed model result carried no tool_calls — the model did not spontaneously call the advertised tool (spontaneity is probabilistic; re-run if red)")
	}

	// (b) The before_tool policy hook DENIED it: a real control-plane policy.denied.v1 fired mid-run.
	if n := hookPolicyDeniedCount(t, pool, tenant, sessionID); n < 1 {
		dumpRunDiagnostics(t, pool, tenant, sessionID, runID)
		t.Fatal("no policy.denied.v1 journaled — the control-plane deny never fired for the spontaneous tool call")
	}

	// (c) The tool was NEVER executed: a policy deny leaves NO committed tool-ledger row for the file tool.
	if n := countRows(t, pool, `SELECT count(*) FROM tool_calls WHERE run_id=$1 AND name='palai.workspace.file' AND state='completed'`, runID); n != 0 {
		dumpRunDiagnostics(t, pool, tenant, sessionID, runID)
		t.Fatalf("a denied tool executed (%d completed palai.workspace.file rows), want 0 — a deny must never run the effect", n)
	}

	// (d) The workspace stayed empty — the denied file tool wrote nothing.
	if workspaceHasAnyFile(t, alloc) {
		t.Fatalf("a denied file tool wrote into the real workspace %s — the deny did not block the effect", alloc)
	}

	t.Logf("hook-deny-visible PASS: the real model spontaneously called palai.workspace.file, the before_tool policy hook denied it (policy.denied.v1), the tool never executed, and no file was written — the model saw a real control-plane deny mid-run.")
}

// hookPolicyDeniedCount counts the run session's reused policy.denied.v1 events — the durable record of a
// fail-closed hook deny.
func hookPolicyDeniedCount(t *testing.T, pool *pgxpool.Pool, tenant coordinator.Tenant, sessionID string) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(storage.WithSystemScope(context.Background()),
		`SELECT count(*) FROM events WHERE session_id=$1 AND organization_id=$2 AND project_id=$3 AND type='policy.denied.v1'`,
		sessionID, tenant.Organization, tenant.Project).Scan(&n); err != nil {
		t.Fatalf("count policy.denied.v1 events: %v", err)
	}
	return n
}
