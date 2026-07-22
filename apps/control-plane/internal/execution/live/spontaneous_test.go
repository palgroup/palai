//go:build live

// This file is CASE=spontaneous-tool-roundtrip, the E12 Task 1 live smoke: the FIRST fully spontaneous
// real-provider tool call through the PRODUCTION orchestrator. dispatchModel advertises the run's
// effective tool set to the real provider (the project's default_tools = [palai.workspace.file]); the
// model is OFFERED the file tool and calls it OF ITS OWN CHOICE (the prompt instructs but does NOT force
// — no tool_choice:required), and the broker executes it from the fenced ledger against a REAL workspace.
// This is the tool-advertising ceiling named across E08-E11 finally lifted: a real model sees the schema
// → SPONTANEOUSLY calls the tool → the broker runs it against a real workspace. Evidence: a
// real chatcmpl id on the spontaneous tool step, a completed palai.workspace.file ledger row, and the
// file actually on disk.
//
// It lives under the control-plane internal tree (not tests/live/) because it drives the real
// execution.Orchestrator (an internal package Go forbids importing from tests/live/), like its sibling
// checkpoint-restore smoke.
//
// HONEST CEILINGS (spec §10.2):
//  1. SINGLE PROVIDER: proven against provider-one only. Second-provider parity (the advertise + parse
//     surface re-proven on a second adapter) is E16.
//  2. SPONTANEITY IS PROBABILISTIC: the prompt instructs but does not force. A run where the model
//     declines to call the tool produces no tool call and this test goes RED and is re-run — a GREEN run
//     IS the proof. The prompt carries no tool_choice:required.
//  3. SMALL TOOL SET is a deliberate cost bound: one tool, not a 5-tool orchestrator loop.
//  4. MULTI-STEP TERMINAL CONTINUATION (E12 T1b, now threaded): the engine wire carries the provider
//     tool_call id (engine.schema.json $defs/tool_call.id; toEngineToolCalls threads it and the engine
//     translates the synthetic tcall_ id to it on the tool result), so the threaded assistant-tool_calls
//     + tool-result conversation the orchestrator sends back for the NEXT step is well-formed for the
//     real OpenAI chat API. This smoke now asserts the run reaches a terminal COMPLETION with >=2 distinct
//     real chatcmpl ids — the spontaneous tool step, then the continuation that reads the tool result and
//     completes: the model spontaneously calls the tool, the broker executes it, and the run finishes.
//
// GATED: serialized with every LIVE/fault smoke on the shared :local Docker stack; NOT part of make
// verify / CI. Skips cleanly without creds. The credential is an opaque env-resolved secret, never printed.
package live

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	providerone "github.com/palgroup/palai/adapters/models/provider_one"
	"github.com/palgroup/palai/apps/control-plane/internal/execution"
	"github.com/palgroup/palai/apps/control-plane/internal/execution/tools"
	"github.com/palgroup/palai/apps/control-plane/internal/store"
	"github.com/palgroup/palai/packages/coordinator"
	modelbroker "github.com/palgroup/palai/packages/model-broker"
	toolbroker "github.com/palgroup/palai/packages/tool-broker"

	"github.com/palgroup/palai/storage"
)

// TestLiveSpontaneousToolRoundTripRealProvider is CASE=spontaneous-tool-roundtrip (see the file ceilings).
func TestLiveSpontaneousToolRoundTripRealProvider(t *testing.T) {
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

	// A real workspace on the host: the file tool writes here directly (no provisioning infra needed —
	// WorkspaceHostPath alone gives the tool a confined root).
	alloc := t.TempDir()
	tenant, sessionID, respID, runID := seedSpontaneousRun(t, pool)

	broker := modelbroker.New(modelbroker.Config{
		Adapters: map[string]modelbroker.ModelAdapter{"provider-one": providerone.Adapter{}},
		Secrets:  modelbroker.EnvResolver{"provider-one": credentialEnv},
	})
	tb := toolbroker.New(tools.FileTool())

	orch := execution.NewOrchestrator(repo, &subprocessDialer{engineDir: engineDir}, broker, tb)
	orch.SetModelRoute(execution.ModelRoute{Provider: "provider-one", Model: liveModel(), Secret: modelbroker.SecretRef("provider-one")})

	desc := descriptor(runID, 1)
	desc.WorkspaceHostPath = alloc
	// ExecuteAttempt drives the real engine + provider through to a terminal run: T1b threads the
	// tool_call id, so the continuation after the tool executes is well-formed and completes. A
	// dial/engine error is a hard failure worth surfacing.
	if err := orch.ExecuteAttempt(ctx, desc); err != nil {
		t.Fatalf("execute spontaneous tool round-trip: %v", err)
	}

	// (a) The run reached a terminal COMPLETION on the real provider: the spontaneous tool step AND the
	//     continuation that reads the tool result both completed (T1b's well-formed threaded conversation).
	if state := responseState(t, pool, tenant, respID); state != "completed" {
		dumpRunDiagnostics(t, pool, tenant, sessionID, runID)
		t.Fatalf("response state = %q, want completed (T1b threads the tool_call id, so the continuation completes)", state)
	}

	// (b) >=2 distinct real chatcmpl ids: the model called the tool (step 1), read its result, and
	//     continued to a REAL terminal completion (step 2) — two separate provider round-trips.
	ids := distinctProviderIDs(t, pool, tenant, runID)
	if len(ids) < 2 {
		dumpRunDiagnostics(t, pool, tenant, sessionID, runID)
		t.Fatalf("real chatcmpl ids = %d (%v), want >=2 (tool step then the continuation that completes)", len(ids), ids)
	}

	// (c) The FIRST committed result carried tool_calls — the model SPONTANEOUSLY chose the file tool
	//     from the advertised schema (probabilistic — re-run if red).
	if !firstResultHasToolCalls(t, pool, tenant, runID) {
		dumpRunDiagnostics(t, pool, tenant, sessionID, runID)
		t.Fatal("the first committed model result carried no tool_calls — the model did not spontaneously call the advertised tool (spontaneity is probabilistic; re-run if red)")
	}

	// (d) A COMPLETED palai.workspace.file tool_calls ledger row exists, and the file is actually on disk
	//     — the broker really executed the spontaneously-chosen tool.
	if n := countRows(t, pool, `SELECT count(*) FROM tool_calls WHERE run_id=$1 AND name='palai.workspace.file' AND state='completed'`, runID); n < 1 {
		dumpRunDiagnostics(t, pool, tenant, sessionID, runID)
		t.Fatalf("completed palai.workspace.file tool_calls rows = %d, want >=1 (the broker executes the spontaneously-chosen tool)", n)
	}
	if !workspaceHasAnyFile(t, alloc) {
		t.Fatalf("the file tool executed but wrote no file into the real workspace %s", alloc)
	}
	t.Logf("spontaneous-tool-roundtrip PASS: real model spontaneously called palai.workspace.file, the broker executed it against a real workspace, and the run completed with %d distinct chatcmpl ids (tool step + threaded continuation, T1b).", len(ids))
}

// seedSpontaneousRun seeds org→project→session→response→run where the project's config_policy puts the
// file tool in the effective set (so dispatchModel advertises it) and the prompt INSTRUCTS but does not
// force a file write. The run starts queued so the orchestrator drives it.
func seedSpontaneousRun(t *testing.T, pool *pgxpool.Pool) (coordinator.Tenant, string, string, string) {
	t.Helper()
	ctx := context.Background()
	tenant := coordinator.Tenant{Organization: newID("org"), Project: newID("prj")}
	session, response, runID := newID("ses"), newID("resp"), newID("run")
	do := func(sql string, args ...any) {
		if _, err := pool.Exec(storage.WithSystemScope(ctx), sql, args...); err != nil {
			t.Fatalf("seed exec %q: %v", sql, err)
		}
	}
	do(`INSERT INTO organizations (id) VALUES ($1)`, tenant.Organization)
	do(`INSERT INTO projects (id, organization_id, config_policy) VALUES ($1, $2, $3)`,
		tenant.Project, tenant.Organization, []byte(`{"default_tools":["palai.workspace.file"]}`))
	do(`INSERT INTO sessions (id, organization_id, project_id) VALUES ($1, $2, $3)`, session, tenant.Organization, tenant.Project)
	do(`INSERT INTO responses (id, organization_id, project_id, session_id, state, input) VALUES ($1,$2,$3,$4,'queued',$5)`,
		response, tenant.Organization, tenant.Project, session,
		[]byte(`"Use the file tool to write a file named hello.txt with the content hello, then confirm you are done."`))
	do(`INSERT INTO runs (id, organization_id, project_id, session_id, response_id, state) VALUES ($1,$2,$3,$4,$5,'queued')`,
		runID, tenant.Organization, tenant.Project, session, response)
	return tenant, session, response, runID
}

// responseState reads the terminal state the run's response projection settled at.
func responseState(t *testing.T, pool *pgxpool.Pool, tenant coordinator.Tenant, respID string) string {
	t.Helper()
	var state string
	if err := pool.QueryRow(storage.WithSystemScope(context.Background()),
		`SELECT state FROM responses WHERE id=$1 AND organization_id=$2 AND project_id=$3`,
		respID, tenant.Organization, tenant.Project).Scan(&state); err != nil {
		t.Fatalf("read response state: %v", err)
	}
	return state
}

// distinctProviderIDs returns the set of real chatcmpl-... ids across the run's completed model results.
func distinctProviderIDs(t *testing.T, pool *pgxpool.Pool, tenant coordinator.Tenant, runID string) []string {
	t.Helper()
	rows, err := pool.Query(storage.WithSystemScope(context.Background()),
		`SELECT result FROM model_requests WHERE run_id=$1 AND organization_id=$2 AND project_id=$3 AND state='completed'`,
		runID, tenant.Organization, tenant.Project)
	if err != nil {
		t.Fatalf("read model results: %v", err)
	}
	defer rows.Close()
	seen := map[string]struct{}{}
	for rows.Next() {
		var result []byte
		if err := rows.Scan(&result); err != nil {
			t.Fatalf("scan model result: %v", err)
		}
		var body struct {
			ProviderRequestID string `json:"provider_request_id"`
		}
		_ = json.Unmarshal(result, &body)
		if body.ProviderRequestID != "" {
			seen[body.ProviderRequestID] = struct{}{}
		}
	}
	out := make([]string, 0, len(seen))
	for id := range seen {
		out = append(out, id)
	}
	return out
}

// firstResultHasToolCalls reports whether the earliest committed model result carried a tool call — the
// evidence that the model spontaneously chose the tool on its first step (the round-trip's opening move).
func firstResultHasToolCalls(t *testing.T, pool *pgxpool.Pool, tenant coordinator.Tenant, runID string) bool {
	t.Helper()
	rows, err := pool.Query(storage.WithSystemScope(context.Background()),
		`SELECT result FROM model_requests WHERE run_id=$1 AND organization_id=$2 AND project_id=$3 AND state='completed' ORDER BY updated_at ASC`,
		runID, tenant.Organization, tenant.Project)
	if err != nil {
		t.Fatalf("read model results: %v", err)
	}
	defer rows.Close()
	for rows.Next() {
		var result []byte
		if err := rows.Scan(&result); err != nil {
			t.Fatalf("scan model result: %v", err)
		}
		var body struct {
			ToolCalls []map[string]any `json:"tool_calls"`
		}
		_ = json.Unmarshal(result, &body)
		if len(body.ToolCalls) > 0 {
			return true
		}
	}
	return false
}

// dumpRunDiagnostics surfaces why a run did not complete: the response output and the tail of the
// journal (the failure event carries the sanitized reason). Kept so a red live smoke is debuggable.
func dumpRunDiagnostics(t *testing.T, pool *pgxpool.Pool, tenant coordinator.Tenant, sessionID, runID string) {
	t.Helper()
	var output []byte
	_ = pool.QueryRow(storage.WithSystemScope(context.Background()),
		`SELECT output FROM responses WHERE id IN (SELECT response_id FROM runs WHERE id=$1) AND organization_id=$2 AND project_id=$3`,
		runID, tenant.Organization, tenant.Project).Scan(&output)
	t.Logf("diagnostic: response output = %s", string(output))
	rows, err := pool.Query(storage.WithSystemScope(context.Background()),
		`SELECT type, payload FROM events WHERE session_id=$1 AND organization_id=$2 AND project_id=$3 ORDER BY seq DESC LIMIT 12`,
		sessionID, tenant.Organization, tenant.Project)
	if err != nil {
		return
	}
	defer rows.Close()
	for rows.Next() {
		var typ string
		var payload []byte
		if err := rows.Scan(&typ, &payload); err == nil {
			t.Logf("diagnostic: event %s %s", typ, string(payload))
		}
	}
	mrows, err := pool.Query(storage.WithSystemScope(context.Background()),
		`SELECT state, result FROM model_requests WHERE run_id=$1 AND organization_id=$2 AND project_id=$3 ORDER BY updated_at ASC`,
		runID, tenant.Organization, tenant.Project)
	if err == nil {
		defer mrows.Close()
		for mrows.Next() {
			var st string
			var result []byte
			if err := mrows.Scan(&st, &result); err == nil {
				t.Logf("diagnostic: model_request state=%s result=%s", st, string(result))
			}
		}
	}
	trows, err := pool.Query(storage.WithSystemScope(context.Background()),
		`SELECT name, state, request_hash FROM tool_calls WHERE run_id=$1 AND organization_id=$2 AND project_id=$3`,
		runID, tenant.Organization, tenant.Project)
	if err == nil {
		defer trows.Close()
		for trows.Next() {
			var name, st, hash string
			if err := trows.Scan(&name, &st, &hash); err == nil {
				t.Logf("diagnostic: tool_call name=%s state=%s hash=%s", name, st, hash)
			}
		}
	}
}

// workspaceHasAnyFile reports whether the real workspace holds at least one regular file the tool wrote.
func workspaceHasAnyFile(t *testing.T, root string) bool {
	t.Helper()
	var found bool
	_ = filepath.WalkDir(root, func(_ string, d os.DirEntry, err error) error {
		if err == nil && d.Type().IsRegular() {
			found = true
		}
		return nil
	})
	return found
}
