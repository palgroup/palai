//go:build live

// CASE=cron-fires-once (E11 Task 3, AUT-006/007/008): a per-minute cron schedule on a cron trigger pinned
// to a published AgentRevision fires its trigger through the SAME §20.2.2 delivery pipeline the manual/API
// path uses; in the observed window EXACTLY ONE occurrence is materialized, which bears EXACTLY ONE real
// provider-one run — proven by a broker-seam request-count counter whose value equals the occurrence count
// (a count assert, not "probably once"). The occurrence → delivery → run chain is linked by ids and the
// occurrence id is unique.
//
// HONEST CEILING (mandatory, spec §33, brief §6): the schedule ticker's exactly-once is SINGLE-PostgreSQL —
// N replicas share one PG (the AUT-007 proof is two loops, one PG); a multi-host fleet scheduler is E14.
// Exactly-once OCCURRENCE ≠ exactly-once run SIDE EFFECTS: the fired run recovers via the E10 ladder; the
// scheduler's own outage recovery is durable-row + misfire policy (proven in the fault CASE=scheduler),
// NOT checkpoints. The run's effective tool set is empty (no default_tools configured), so dispatchModel
// advertises nothing and the run stays single-step + REAL; this case makes NO model→tool claim — that
// claim lives in the tool live cases (CASE=coding-tools / CASE=spontaneous-tool-roundtrip). Minute
// granularity is the cron floor. The credential lives ONLY in .env.local (loaded via `set -a`, never
// `set -x`), is used only as an opaque needle for the leak scan, and appears in no captured surface.
package live

import (
	"context"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	providerone "github.com/palgroup/palai/adapters/models/provider_one"
	"github.com/palgroup/palai/apps/control-plane/internal/automation"
	"github.com/palgroup/palai/packages/contracts"
	"github.com/palgroup/palai/packages/coordinator"
	modelbroker "github.com/palgroup/palai/packages/model-broker"

	"github.com/palgroup/palai/storage"
)

// TestLiveCronFiresOnce seeds a due per-minute cron schedule against a real PG, ticks the REAL schedule
// ticker across the window, and proves exactly one occurrence bears exactly one REAL provider-one run.
func TestLiveCronFiresOnce(t *testing.T) {
	secret := os.Getenv(credentialEnv)
	if secret == "" {
		t.Fatalf("%s is unset; the live tier loads it from .env.local at runtime", credentialEnv)
	}
	pgURL := os.Getenv("PALAI_COMPONENT_POSTGRES_URL")
	if pgURL == "" {
		t.Skip("PALAI_COMPONENT_POSTGRES_URL is required; run make test-live-provider PROVIDER=provider-one CASE=cron-fires-once")
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
	rev, err := agents.CreateRevision(ctx, org, project, profileID, []byte(`{"model":"`+liveModel()+`","instructions":"run the scheduled task"}`))
	if err != nil {
		t.Fatalf("CreateRevision error = %v", err)
	}
	if _, _, err := agents.PublishRevision(ctx, org, project, rev.ID); err != nil {
		t.Fatalf("PublishRevision error = %v", err)
	}

	// A type='cron' trigger pinned to the revision, mapping a constant task into the run input.
	triggers := automation.NewTriggerStore(pool).WithAdmitter(spine)
	triggerID, err := triggers.CreateTrigger(ctx, org, project, principal, randID("cron"), "cron")
	if err != nil {
		t.Fatalf("CreateTrigger error = %v", err)
	}
	if _, err := triggers.ReviseTrigger(ctx, org, project, triggerID, automation.TriggerRevisionInput{
		AgentRevisionID: rev.ID,
		InputMapping:    []byte(`{"fields":{"input":{"const":"summarize today's scheduled report"}},"required":["input"]}`),
	}); err != nil {
		t.Fatalf("ReviseTrigger error = %v", err)
	}

	// A per-minute cron schedule, seeded already DUE at a minute-aligned instant so the ticker fires it in
	// the observed window without waiting a real minute.
	store := automation.NewScheduleStore(pool, triggers)
	base := time.Now().UTC().Truncate(time.Minute)
	scheduleID := randID("sch")
	exec(`INSERT INTO schedules (id, organization_id, project_id, name, trigger_id, created_by, kind, cron_expr,
	      timezone, misfire_policy, misfire_grace_seconds, max_catch_up, jitter_seconds, next_fire_at)
	      VALUES ($1,$2,$3,$4,$5,$6,'cron','* * * * *','UTC','fire_once_now',60,0,0,$7)`,
		scheduleID, org, project, randID("name"), triggerID, principal, base)

	// Tick the REAL ticker twice within the same minute (a fixed `now` within grace): exactly-once must hold
	// — the second tick materializes nothing new.
	now := base.Add(30 * time.Second)
	ticker := automation.NewScheduleTicker(store, time.Millisecond, 100, nil).WithClock(func() time.Time { return now })
	for i := 0; i < 2; i++ {
		if err := ticker.Tick(ctx); err != nil {
			t.Fatalf("ticker.Tick() error = %v", err)
		}
	}

	// Exactly ONE occurrence, admitted, with a unique id and a delivery link.
	var occurrenceCount, distinctIDs int
	if err := pool.QueryRow(storage.WithSystemScope(ctx), `SELECT count(*), count(DISTINCT occurrence_id) FROM schedule_occurrences WHERE schedule_id=$1`, scheduleID).
		Scan(&occurrenceCount, &distinctIDs); err != nil {
		t.Fatalf("count occurrences error = %v", err)
	}
	if occurrenceCount != 1 || distinctIDs != 1 {
		t.Fatalf("occurrences = %d (distinct ids %d), want exactly 1 (exactly-once across two ticks)", occurrenceCount, distinctIDs)
	}
	var occurrenceID, occState, occDelivery string
	if err := pool.QueryRow(storage.WithSystemScope(ctx), `SELECT occurrence_id, state, delivery_id FROM schedule_occurrences WHERE schedule_id=$1`, scheduleID).
		Scan(&occurrenceID, &occState, &occDelivery); err != nil {
		t.Fatalf("read occurrence error = %v", err)
	}
	if occState != "admitted" || occDelivery == "" {
		t.Fatalf("occurrence = state:%q delivery:%q, want 'admitted' with a delivery link", occState, occDelivery)
	}

	// The linked delivery bore a real run, keyed by the occurrence_id dedupe (the third defense line).
	var deliveryState, runID, responseID, dedupeKey string
	if err := pool.QueryRow(storage.WithSystemScope(ctx), `SELECT state, run_id, response_id, dedupe_key FROM trigger_deliveries WHERE id=$1`, occDelivery).
		Scan(&deliveryState, &runID, &responseID, &dedupeKey); err != nil {
		t.Fatalf("read delivery error = %v", err)
	}
	if deliveryState != "run_created" || runID == "" || responseID == "" {
		t.Fatalf("delivery = state:%q run:%q response:%q, want run_created with a born run", deliveryState, runID, responseID)
	}
	if dedupeKey != occurrenceID {
		t.Fatalf("delivery dedupe_key = %q, want the occurrence_id %q", dedupeKey, occurrenceID)
	}

	// The load-bearing count assert: the counting seam over the REAL provider-one adapter. The born run is
	// admitted 'queued' (no dispatch worker runs here); we drive its ONE mapped input through the broker
	// ourselves, so provider requests == occurrence count (one real completion per fired occurrence) — the
	// same broker-seam-counter shape the T2 dedupe smoke uses.
	adapter := &countingAdapter{inner: providerone.Adapter{}}
	broker := modelbroker.New(modelbroker.Config{
		Adapters: map[string]modelbroker.ModelAdapter{"provider-one": adapter},
		Secrets:  modelbroker.EnvResolver{"provider-one": credentialEnv},
	})
	mapped := mappedInputFor(t, pool, occDelivery)
	req := modelbroker.Request{
		ModelRequestID: contracts.ModelRequestID(randID("mreq")),
		RouteRevision:  1, ModelStepID: "step-cron", Model: liveModel(),
		Messages:    []modelbroker.Message{{Role: "user", Content: "Summarize this scheduled action in one short sentence: " + string(mapped)}},
		Deadline:    time.Now().Add(60 * time.Second),
		Reservation: modelbroker.Reservation{MaxTotalTokens: 2000},
		Secret:      modelbroker.SecretRef("provider-one"),
	}
	res, err := broker.Route(ctx, "provider-one", req, func(modelbroker.Delta) {})
	if err != nil {
		t.Fatalf("route scheduled run: %v", err)
	}
	assertRealCompletion(t, res)

	if got := atomic.LoadInt32(&adapter.count); int(got) != occurrenceCount {
		t.Fatalf("provider request count = %d, want the occurrence count %d (one real run per occurrence)", got, occurrenceCount)
	}
	if !strings.HasPrefix(res.Model, liveModel()) {
		t.Fatalf("completion model = %q, want the revision-pinned %q family", res.Model, liveModel())
	}
	if strings.Contains(string(mustJSON(res)), secret) {
		t.Fatal("the completion result contains the credential value")
	}

	t.Logf("live cron-fires-once PASS (real provider-one, cron source, single-step run, NO tool claim, single-PG): "+
		"occurrence=%s → delivery=%s → run=%s provider_calls=%d model=%s chatcmpl=%s…",
		safePrefix(occurrenceID), safePrefix(occDelivery), safePrefix(runID), atomic.LoadInt32(&adapter.count), res.Model, safePrefix(res.ProviderRequestID))
}
