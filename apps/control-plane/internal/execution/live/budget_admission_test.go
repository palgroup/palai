//go:build live

// This file is CASE=budget-admission, the E13 Task 6 live smoke: durable metering on a REAL
// control-plane router over a real Postgres, settled by a REAL provider-one completion. It proves the
// two halves the plan names, end to end:
//
//  1. LOW-BUDGET PROJECT'S SECOND RUN IS REJECTED. A project carrying a small durable budget admits its
//     first run normally; that run completes on the REAL provider and settles its token usage; the very
//     next POST /v1/responses is refused at ADMISSION with 429 quota_exceeded and a stable remediation
//     body naming the limit, what was used, and what to do — leaving no run, no response, and no
//     idempotency record behind. Raising the budget admits the same request, so the refusal was the
//     limit and nothing else.
//  2. THE LEDGER RECONCILES WITH THE REAL PROVIDER USAGE. The settled ledger rows for the run are
//     compared against the run's own terminal projection — two INDEPENDENT accumulations of the same
//     provider receipts (the orchestrator's in-process sum versus per-step rows written into the DB by
//     the settlement transaction). Agreement at a non-zero total is what makes the metering trustworthy;
//     a ledger that only agreed with itself would prove nothing.
//
// HONEST CEILINGS (named here, matching the plan):
//   - METERING ONLY: no price, no invoice, no adjustment entry, no BYOK split, no exporter
//     (BIL-004/005/006 → E13-H/SaaS). This smoke proves consumption is recorded and enforced, not billed.
//   - RECONCILIATION SOURCE: both sides descend from the same provider response bodies, so this proves
//     the METERING PATH is lossless and double-count-free — not that the provider's own dashboard
//     agrees. An external reconciliation against a provider invoice is E13-H.
//   - DOCUMENTED VARIANCE (BIL-003): the budget gate reads SETTLED usage under ReadCommitted with no row
//     lock, so a run already in flight can overshoot a budget by its own usage, and two admissions
//     racing the exact boundary can both pass. The ledger stays exact; the gate is accurate to ±the runs
//     in flight.
//   - SINGLE PROVIDER: the settled run is provider-one only; the metering path is provider-agnostic.
//
// GATED: serialized with every LIVE smoke on the shared stack; NOT part of make verify. Skips cleanly
// without creds. The credential is an opaque env-resolved secret, never printed.
package live

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	providerone "github.com/palgroup/palai/adapters/models/provider_one"
	"github.com/palgroup/palai/apps/control-plane/api"
	"github.com/palgroup/palai/apps/control-plane/api/middleware"
	"github.com/palgroup/palai/apps/control-plane/internal/execution"
	"github.com/palgroup/palai/apps/control-plane/internal/execution/tools"
	"github.com/palgroup/palai/apps/control-plane/internal/metering"
	"github.com/palgroup/palai/apps/control-plane/internal/store"
	modelbroker "github.com/palgroup/palai/packages/model-broker"
	toolbroker "github.com/palgroup/palai/packages/tool-broker"

	"github.com/palgroup/palai/storage"
)

// budgetTokens is the project's whole allowance. It is small enough that ONE real completion exhausts
// it (the prompt alone costs more input tokens than this) and large enough to be a real limit rather
// than "any usage at all".
const budgetTokens = 5

// TestLiveBudgetRejectsSecondRunAndLedgerReconciles is CASE=budget-admission (see the file ceilings).
func TestLiveBudgetRejectsSecondRunAndLedgerReconciles(t *testing.T) {
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

	token, tenant := seedTenantWithKey(t, pool, "budget")
	meters := metering.New(pool)
	scope := middleware.Scope{Organization: tenant.Organization, Project: tenant.Project}

	// The tenant sets its own low budget through the very code POST /v1/budgets calls.
	if _, err := meters.SetBudget(ctx, scope,
		[]byte(fmt.Sprintf(`{"meter_prefix":"model.","limit_quantity":%d}`, budgetTokens))); err != nil {
		t.Fatalf("SetBudget error = %v", err)
	}

	srv := httptest.NewServer(api.NewRouter(repo, repo, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil,
		nil, nil, api.SSEConfig{}, nil, nil, api.WithUsage(meters)))
	t.Cleanup(srv.Close)

	// --- Run 1 admits: the budget still has headroom, because nothing has settled yet. ---
	code, body := postResponse(t, srv.URL, token, "budget-run-1")
	if code != http.StatusAccepted {
		t.Fatalf("first admission status = %d, want 202 (an unspent budget must not reject): %s", code, body)
	}
	runID := runIDFromBody(t, body)
	responseID := responseIDFromBody(t, body)

	// --- Run 1 completes on the REAL provider; its usage settles into the ledger per model step. ---
	broker := modelbroker.New(modelbroker.Config{
		Adapters: map[string]modelbroker.ModelAdapter{"provider-one": providerone.Adapter{}},
		Secrets:  modelbroker.EnvResolver{"provider-one": credentialEnv},
	})
	orch := execution.NewOrchestrator(repo, &subprocessDialer{engineDir: engineDir}, broker, toolbroker.New(tools.FileTool()))
	orch.SetModelRoute(execution.ModelRoute{Provider: "provider-one", Model: liveModel(), Secret: modelbroker.SecretRef("provider-one")})
	if err := orch.ExecuteAttempt(ctx, descriptor(runID, 1)); err != nil {
		t.Fatalf("execute the budgeted run on the real provider: %v", err)
	}
	if n := countRows(t, pool, `SELECT count(*) FROM model_requests WHERE run_id=$1 AND state='completed'`, runID); n < 1 {
		t.Fatalf("completed model_requests = %d, want >=1 (the real provider must have answered)", n)
	}

	// --- Reconciliation: the ledger's per-step rows equal the run's own terminal projection. ---
	settled := ledgerTokens(t, pool, runID)
	projected := projectionTokens(t, pool, tenant.Organization, responseID)
	if settled == 0 || projected == 0 {
		t.Fatalf("settled=%v projected=%v, want a non-zero real token spend on both sides", settled, projected)
	}
	if settled != projected {
		t.Fatalf("ledger settled %v model tokens but the run's terminal projection reports %v — the metering path lost or double-counted a step",
			settled, projected)
	}
	if settled <= budgetTokens {
		t.Fatalf("the real completion spent %v tokens, which does not exceed the %d-token budget; the case cannot prove a rejection",
			settled, budgetTokens)
	}

	// --- Run 2 is REJECTED by admission, with a stable remediation body and nothing left behind. ---
	code, body = postResponse(t, srv.URL, token, "budget-run-2")
	if code != http.StatusTooManyRequests {
		t.Fatalf("second admission status = %d, want 429 (the budget is exhausted): %s", code, body)
	}
	assertProblemCode(t, body, "quota_exceeded")
	detail := problemDetail(t, body)
	for _, want := range []string{"budget", `"model."`, "raise the budget"} {
		if !strings.Contains(detail, want) {
			t.Fatalf("429 detail %q does not carry %q — the remediation body is not stable/actionable", detail, want)
		}
	}
	if n := countRows(t, pool, `SELECT count(*) FROM runs WHERE organization_id=$1 AND project_id=$2`, tenant.Organization, tenant.Project); n != 1 {
		t.Fatalf("runs in project = %d, want exactly 1 (the rejected admission created nothing)", n)
	}
	if n := countRows(t, pool, `SELECT count(*) FROM idempotency_records WHERE idempotency_key='budget-run-2' AND project_id=$1`, tenant.Project); n != 0 {
		t.Fatalf("idempotency records for the rejected key = %d, want 0 (the key must be free to retry)", n)
	}

	// --- The metering surface reports the same spend the gate acted on. ---
	summary := usageSummary(t, srv.URL, token)
	if summary.total("model.") != settled {
		t.Fatalf("GET /v1/usage reports %v model tokens, want the %v the ledger settled", summary.total("model."), settled)
	}
	if len(summary.Budgets) != 1 || summary.Budgets[0].MeterPrefix != "model." {
		t.Fatalf("GET /v1/usage carried budgets %+v, want the one 'model.' budget that binds the project", summary.Budgets)
	}

	// --- Raising the budget admits the SAME request: the refusal was the limit, nothing else. ---
	if _, err := meters.SetBudget(ctx, scope,
		[]byte(fmt.Sprintf(`{"meter_prefix":"model.","limit_quantity":%d}`, int(settled)*100))); err != nil {
		t.Fatalf("SetBudget(raise) error = %v", err)
	}
	if code, body = postResponse(t, srv.URL, token, "budget-run-2"); code != http.StatusAccepted {
		t.Fatalf("after raising the budget, the retried admission status = %d, want 202: %s", code, body)
	}

	t.Logf("budget-admission PASS: run 1 settled %v REAL provider tokens into the ledger (matching its terminal projection), run 2 was refused at admission with a stable 429 leaving nothing behind, and raising the budget admitted the same request.",
		settled)
}

// ledgerTokens sums the model meters the settlement transaction wrote for one run — the DB-side
// accumulation, one row per model step per direction.
func ledgerTokens(t *testing.T, pool *pgxpool.Pool, runID string) float64 {
	t.Helper()
	var total float64
	if err := pool.QueryRow(storage.WithSystemScope(context.Background()),
		`SELECT coalesce(sum(quantity), 0) FROM usage_ledger WHERE run_id=$1 AND meter LIKE 'model.%'`, runID).Scan(&total); err != nil {
		t.Fatalf("sum settled ledger tokens: %v", err)
	}
	return total
}

// projectionTokens reads the run's own terminal projection usage (responses.output, the committed
// terminal body) — the orchestrator's INDEPENDENT in-process accumulation of the same provider receipts.
func projectionTokens(t *testing.T, pool *pgxpool.Pool, org, responseID string) float64 {
	t.Helper()
	var raw []byte
	if err := pool.QueryRow(storage.WithSystemScope(context.Background()),
		`SELECT output FROM responses WHERE id=$1 AND organization_id=$2`, responseID, org).Scan(&raw); err != nil {
		t.Fatalf("read terminal projection: %v", err)
	}
	var body struct {
		Usage struct {
			InputTokens  float64 `json:"input_tokens"`
			OutputTokens float64 `json:"output_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(raw, &body); err != nil {
		t.Fatalf("decode terminal projection %s: %v", raw, err)
	}
	return body.Usage.InputTokens + body.Usage.OutputTokens
}

func responseIDFromBody(t *testing.T, body []byte) string {
	t.Helper()
	var env struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(body, &env); err != nil || env.ID == "" {
		t.Fatalf("no id in 202 body %s (err %v)", body, err)
	}
	return env.ID
}

func problemDetail(t *testing.T, body []byte) string {
	t.Helper()
	var p struct {
		Detail string `json:"detail"`
	}
	if err := json.Unmarshal(body, &p); err != nil {
		t.Fatalf("decode problem %s: %v", body, err)
	}
	return p.Detail
}

type usageSummaryBody struct {
	Meters []struct {
		Meter    string  `json:"meter"`
		Quantity float64 `json:"quantity"`
	} `json:"meters"`
	Budgets []struct {
		MeterPrefix   string  `json:"meter_prefix"`
		LimitQuantity float64 `json:"limit_quantity"`
	} `json:"budgets"`
}

// total sums every meter whose name starts with prefix — the same prefix rule the limits use.
func (s usageSummaryBody) total(prefix string) float64 {
	var out float64
	for _, m := range s.Meters {
		if strings.HasPrefix(m.Meter, prefix) {
			out += m.Quantity
		}
	}
	return out
}

func usageSummary(t *testing.T, baseURL, token string) usageSummaryBody {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, baseURL+"/v1/usage", nil)
	if err != nil {
		t.Fatalf("build usage request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET /v1/usage: %v", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /v1/usage status = %d, body %s", resp.StatusCode, raw)
	}
	var out usageSummaryBody
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("decode usage summary %s: %v", raw, err)
	}
	return out
}
