//go:build live

// CASE=trigger-dedupe-run (E11 Task 2, AUT-001): a manual/API trigger delivery starts a REAL provider-one
// run pinned to a published AgentRevision, and a SECOND delivery carrying the same dedupe key is a
// duplicate linked to the original that starts NO second run — proven by a broker-seam request-count
// counter that stays at exactly 1.
//
// HONEST CEILING (mandatory, spec §20.2.2, brief §5): the source is manual/API — signed inbound HTTP
// ingestion (durable ack, source-dedupe, backpressure) is T5. The run is single-step and REAL (the E08
// pin: the effective tool set is empty (no default_tools configured), so dispatchModel advertises nothing
// and the run stays single-step, making NO model→tool claim). The dedupe → no-second-run invariant is proven at the
// broker/adapter seam: the counting adapter wraps the REAL provider-one adapter, and only the deliveries
// that actually bore a run dispatch a real completion — the deduped second delivery bears none, so the
// count is 1, not "probably not called". The full durable admission→worker→engine path is proven in the
// component tier (TestDeliveryRunEntersSameAdmissionPath); this smoke joins dedupe with a real pinned
// completion. The credential is used only as an opaque needle for the leak scan and is never printed.
package live

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	providerone "github.com/palgroup/palai/adapters/models/provider_one"
	"github.com/palgroup/palai/apps/control-plane/internal/automation"
	"github.com/palgroup/palai/packages/contracts"
	"github.com/palgroup/palai/packages/coordinator"
	modelbroker "github.com/palgroup/palai/packages/model-broker"

	"github.com/palgroup/palai/storage"
)

const credentialEnv = "OPENAI_API_KEY"

func liveModel() string {
	if m := os.Getenv("PALAI_LIVE_MODEL"); m != "" {
		return m
	}
	return "gpt-4o-mini"
}

func randID(prefix string) string {
	var raw [8]byte
	_, _ = rand.Read(raw[:])
	return prefix + "_" + hex.EncodeToString(raw[:])
}

// countingAdapter wraps the REAL provider-one adapter and counts every provider request that reaches the
// seam — the load-bearing dedupe assertion (a deduped delivery must add ZERO calls).
type countingAdapter struct {
	inner modelbroker.ModelAdapter
	count int32
}

func (c *countingAdapter) Execute(ctx context.Context, req modelbroker.Request, secret string, delta func(modelbroker.Delta)) (modelbroker.Result, error) {
	atomic.AddInt32(&c.count, 1)
	return c.inner.Execute(ctx, req, secret, delta)
}

// TestLiveTriggerDedupeRun ingests two manual deliveries carrying the same dedupe key against a real PG:
// the first bears a run pinned to a published AgentRevision and dispatches a REAL provider-one completion;
// the second is a duplicate linked to the original that bears no run. The counting adapter proves the
// second delivery adds no provider call (count == 1).
func TestLiveTriggerDedupeRun(t *testing.T) {
	secret := os.Getenv(credentialEnv)
	if secret == "" {
		t.Fatalf("%s is unset; the live tier loads it from .env.local at runtime", credentialEnv)
	}
	pgURL := os.Getenv("PALAI_COMPONENT_POSTGRES_URL")
	if pgURL == "" {
		t.Skip("PALAI_COMPONENT_POSTGRES_URL is required; run make test-live-provider PROVIDER=provider-one CASE=trigger-dedupe-run")
	}
	ctx := context.Background()

	spine, err := coordinator.Open(ctx, pgURL)
	if err != nil {
		t.Fatalf("coordinator.Open() error = %v", err)
	}
	t.Cleanup(spine.Close)
	if err := spine.Migrate(ctx); err != nil {
		t.Fatalf("Migrate() error = %v", err)
	}
	pool := spine.Pool()
	org, project, principal := randID("org"), randID("prj"), randID("prin")
	exec := func(sql string, args ...any) {
		if _, err := pool.Exec(storage.WithSystemScope(ctx), sql, args...); err != nil {
			t.Fatalf("seed exec %q error = %v", sql, err)
		}
	}
	exec(`INSERT INTO organizations (id) VALUES ($1)`, org)
	exec(`INSERT INTO projects (id, organization_id) VALUES ($1, $2)`, project, org)
	exec(`INSERT INTO principals (id, organization_id, project_id, kind) VALUES ($1, $2, $3, 'service')`, principal, org, project)

	// A published AgentRevision pinning the live model.
	agents := automation.New(pool)
	profileID, err := agents.CreateProfile(ctx, org, project, randID("profile"))
	if err != nil {
		t.Fatalf("CreateProfile error = %v", err)
	}
	rev, err := agents.CreateRevision(ctx, org, project, profileID, []byte(`{"model":"`+liveModel()+`","instructions":"summarize the order"}`))
	if err != nil {
		t.Fatalf("CreateRevision error = %v", err)
	}
	if _, _, err := agents.PublishRevision(ctx, org, project, rev.ID); err != nil {
		t.Fatalf("PublishRevision error = %v", err)
	}

	// A trigger pinned to the revision, deduping on the order id, mapping the order summary to the input.
	store := automation.NewTriggerStore(pool).WithAdmitter(spine)
	triggerID, err := store.CreateTrigger(ctx, org, project, principal, randID("orders"), "manual_api")
	if err != nil {
		t.Fatalf("CreateTrigger error = %v", err)
	}
	if _, err := store.ReviseTrigger(ctx, org, project, triggerID, automation.TriggerRevisionInput{
		AgentRevisionID: rev.ID,
		InputMapping:    []byte(`{"fields":{"input":{"select":"order.summary"}},"required":["input"]}`),
		DedupeKeyExpr:   `{"select":"order.id"}`,
	}); err != nil {
		t.Fatalf("ReviseTrigger error = %v", err)
	}

	payload := []byte(`{"order":{"id":"o-live-1","summary":"fulfil the widget order"}}`)
	first, err := store.CreateDelivery(ctx, org, project, principal, triggerID, payload)
	if err != nil {
		t.Fatalf("first CreateDelivery error = %v", err)
	}
	if first.State != "run_created" || first.RunID == "" {
		t.Fatalf("first delivery = %+v, want run_created with a run", first)
	}
	second, err := store.CreateDelivery(ctx, org, project, principal, triggerID, payload)
	if err != nil {
		t.Fatalf("second CreateDelivery error = %v", err)
	}
	if second.State != "duplicate" {
		t.Fatalf("second delivery state = %q, want duplicate", second.State)
	}
	if second.DuplicateOf != first.ID {
		t.Fatalf("second delivery linked to %q, want the original %q", second.DuplicateOf, first.ID)
	}
	if second.RunID != "" {
		t.Fatal("the deduped second delivery bore a run; it must bear none")
	}

	// The counting seam over the REAL provider-one adapter. Only deliveries that bore a run dispatch a
	// real completion — so the deduped second delivery adds ZERO calls.
	adapter := &countingAdapter{inner: providerone.Adapter{}}
	broker := modelbroker.New(modelbroker.Config{
		Adapters: map[string]modelbroker.ModelAdapter{"provider-one": adapter},
		Secrets:  modelbroker.EnvResolver{"provider-one": credentialEnv},
	})

	var lastResult modelbroker.Result
	for i, del := range []automation.DeliveryResult{first, second} {
		if del.RunID == "" {
			continue // the deduped delivery bears no run → no provider call
		}
		mapped := mappedInputFor(t, pool, del.ID)
		req := modelbroker.Request{
			ModelRequestID: contracts.ModelRequestID(randID("mreq")),
			RouteRevision:  1, ModelStepID: "step-dedupe", Model: liveModel(),
			Messages:    []modelbroker.Message{{Role: "user", Content: "Summarize this triggered action in one short sentence: " + string(mapped)}},
			Deadline:    time.Now().Add(60 * time.Second),
			Reservation: modelbroker.Reservation{MaxTotalTokens: 2000},
			Secret:      modelbroker.SecretRef("provider-one"),
		}
		res, err := broker.Route(ctx, "provider-one", req, func(modelbroker.Delta) {})
		if err != nil {
			t.Fatalf("route triggered run %d: %v", i, err)
		}
		assertRealCompletion(t, res)
		lastResult = res
	}

	// The load-bearing dedupe assertion: exactly ONE provider request across the two deliveries.
	if got := atomic.LoadInt32(&adapter.count); got != 1 {
		t.Fatalf("provider request count = %d, want exactly 1 (the deduped delivery must add no call)", got)
	}
	// The completion ran under the revision-pinned model (the provider may return a dated variant, e.g.
	// gpt-4o-mini-2024-07-18, so match the pinned family as a prefix).
	if !strings.HasPrefix(lastResult.Model, liveModel()) {
		t.Fatalf("completion model = %q, want the revision-pinned %q family", lastResult.Model, liveModel())
	}
	// Leak scan by construction: the credential appears in no captured surface.
	if strings.Contains(string(mustJSON(lastResult)), secret) {
		t.Fatal("the completion result contains the credential value")
	}

	t.Logf("live trigger-dedupe PASS (real provider-one, manual/API source, single-step run, NO tool claim): "+
		"first_run=%s dedup_original=%s provider_calls=1 model=%s chatcmpl=%s…",
		first.RunID, second.DuplicateOf, lastResult.Model, safePrefix(lastResult.ProviderRequestID))
}

// mappedInputFor reads the canonical mapped input the pipeline stored for a delivery.
func mappedInputFor(t *testing.T, pool *pgxpool.Pool, deliveryID string) []byte {
	t.Helper()
	var mapped []byte
	if err := pool.QueryRow(storage.WithSystemScope(context.Background()), `SELECT mapped_input FROM trigger_deliveries WHERE id=$1`, deliveryID).Scan(&mapped); err != nil {
		t.Fatalf("read mapped input error = %v", err)
	}
	return mapped
}

func assertRealCompletion(t *testing.T, res modelbroker.Result) {
	t.Helper()
	if res.Error != nil {
		t.Fatalf("provider returned a sanitized error: code=%s status=%d", res.Error.Code, res.Error.Status)
	}
	if !strings.HasPrefix(res.ProviderRequestID, "chatcmpl") {
		t.Fatalf("provider request id %q is not a real chat completion id", res.ProviderRequestID)
	}
	if res.Usage.TotalTokens <= 0 {
		t.Fatalf("usage is not populated: %+v", res.Usage)
	}
}

func mustJSON(v any) []byte {
	b, _ := json.Marshal(v)
	return b
}

func safePrefix(id string) string {
	if len(id) > 16 {
		return id[:16]
	}
	return id
}
