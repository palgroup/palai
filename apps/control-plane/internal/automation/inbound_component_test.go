//go:build component

// External-package (automation_test) component test for the signed inbound-webhook receiver, over real
// PostgreSQL and the REAL router. It proves the AUT-002/001/009 crux: HMAC verification strictly BEFORE
// any persistence, durable ack after the delivery row commits, atomic source-dedupe, and honest
// ack-on-redelivery. It lives in package automation_test so it can import api + store.
package automation_test

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/palgroup/palai/adapters/integrations/webhook"
	"github.com/palgroup/palai/apps/control-plane/api"
	"github.com/palgroup/palai/apps/control-plane/internal/automation"
	"github.com/palgroup/palai/apps/control-plane/internal/store"
)

// recorder captures the sanitized audit lines the reject path emits, so a test can assert what they DO
// and DO NOT contain (never payload/signature/secret bytes).
type recorder struct {
	mu    sync.Mutex
	lines []string
}

func (r *recorder) Log(format string, args ...any) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.lines = append(r.lines, fmt.Sprintf(format, args...))
}
func (r *recorder) all() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return strings.Join(r.lines, "\n")
}
func (r *recorder) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.lines)
}

// inboundHarness stands up the durable spine + a trigger store wired for inbound ingestion (real
// admitter, a temp-file secret resolver, an injected audit recorder), a webhook trigger with a pinned
// revision + a secret ref, and the REAL router. It returns everything a case drives.
type inboundHarness struct {
	t            *testing.T
	pool         *pgxpool.Pool
	store        *automation.TriggerStore
	srv          *httptest.Server
	audit        *recorder
	secret       []byte
	secretsByRef map[string][]byte // ref -> bytes; the resolver reads this (rotation seeds a second ref)
	org, proj    string
	principal    string
	token        string
	triggerID    string
}

const inboundSecretRef = "src-primary"

func newInboundHarness(t *testing.T, maxInflight, backlogMax int) *inboundHarness {
	t.Helper()
	url := os.Getenv("PALAI_COMPONENT_POSTGRES_URL")
	if url == "" {
		t.Skip("PALAI_COMPONENT_POSTGRES_URL is required; run make test-component TEST=postgres")
	}
	ctx := context.Background()
	repo, err := store.Open(ctx, url)
	if err != nil {
		t.Fatalf("store.Open() error = %v", err)
	}
	t.Cleanup(repo.Close)
	if err := repo.Migrate(ctx); err != nil {
		t.Fatalf("Migrate() error = %v", err)
	}
	pool := repo.Spine().Pool()
	token := randID("tok")
	org, proj, principal := seedTenantReturning(t, pool, token)

	secret := []byte("whsec_inbound_" + randID("s"))
	secretsByRef := map[string][]byte{inboundSecretRef: secret}
	resolver := func(o, ref string) ([]byte, error) {
		if o != org {
			return nil, os.ErrNotExist
		}
		if b, ok := secretsByRef[ref]; ok {
			return b, nil
		}
		return nil, os.ErrNotExist
	}
	audit := &recorder{}
	ts := automation.NewTriggerStore(pool).
		WithAdmitter(repo.Spine()).
		WithInboundSecrets(resolver).
		WithInboundGate(audit.Log, 5*time.Minute, maxInflight, backlogMax)

	// A webhook trigger created AS the principal (created_by), pinned to a revision, with a secret ref set.
	triggerID, err := ts.CreateTrigger(ctx, org, proj, principal, randID("inbound"), "webhook")
	if err != nil {
		t.Fatalf("CreateTrigger error = %v", err)
	}
	// The mapping operates on the event's opaque data payload (source-dedupe replaces content dedupe for
	// inbound, so no dedupe_key_expr).
	if _, err := ts.ReviseTrigger(ctx, org, proj, triggerID, automation.TriggerRevisionInput{
		InputMapping: []byte(`{"fields":{"input":{"select":"order.summary"}},"required":["input"]}`),
	}); err != nil {
		t.Fatalf("ReviseTrigger error = %v", err)
	}
	if err := ts.SetInboundSecretRefs(ctx, org, proj, triggerID, inboundSecretRef, ""); err != nil {
		t.Fatalf("SetInboundSecretRefs error = %v", err)
	}

	router := api.NewRouter(repo, repo, repo, repo, repo, repo, automation.NewWebhookStore(pool), ts, nil, nil, api.SSEConfig{}, nil)
	srv := httptest.NewServer(router)
	t.Cleanup(srv.Close)

	return &inboundHarness{t: t, pool: pool, store: ts, srv: srv, audit: audit, secret: secret,
		secretsByRef: secretsByRef, org: org, proj: proj, principal: principal, token: token, triggerID: triggerID}
}

// post signs body under the given secret + event id and POSTs it to the UNAUTHENTICATED inbound route.
func (h *inboundHarness) post(triggerID, eventID string, ts time.Time, body []byte, secret []byte) *http.Response {
	h.t.Helper()
	headers := webhook.NewSigner(secret).Headers(eventID, ts, 1, body)
	req, _ := http.NewRequest(http.MethodPost, h.srv.URL+"/v1/inbound/"+triggerID, strings.NewReader(string(body)))
	req.Close = true
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		h.t.Fatalf("POST /v1/inbound/%s error = %v", triggerID, err)
	}
	return resp
}

// postRaw POSTs a body with caller-supplied headers (used to send an unsigned/garbled request).
func (h *inboundHarness) postRaw(triggerID string, headers map[string]string, body []byte) *http.Response {
	h.t.Helper()
	req, _ := http.NewRequest(http.MethodPost, h.srv.URL+"/v1/inbound/"+triggerID, strings.NewReader(string(body)))
	req.Close = true
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		h.t.Fatalf("POST raw error = %v", err)
	}
	return resp
}

// deliveryRows counts inbound trigger_deliveries rows for the harness trigger.
func (h *inboundHarness) deliveryRows() int {
	h.t.Helper()
	var n int
	if err := h.pool.QueryRow(context.Background(),
		`SELECT count(*) FROM trigger_deliveries WHERE trigger_id=$1`, h.triggerID).Scan(&n); err != nil {
		h.t.Fatalf("count deliveries error = %v", err)
	}
	return n
}

// getDelivery reads a delivery projection over the AUTHENTICATED read route.
func (h *inboundHarness) getDelivery(id string) map[string]any {
	h.t.Helper()
	req, _ := http.NewRequest(http.MethodGet, h.srv.URL+"/v1/trigger-deliveries/"+id, nil)
	req.Header.Set("Authorization", "Bearer "+h.token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		h.t.Fatalf("GET delivery error = %v", err)
	}
	defer resp.Body.Close()
	var out map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&out)
	return out
}

func decodeBody(t *testing.T, resp *http.Response) map[string]any {
	t.Helper()
	defer resp.Body.Close()
	var out map[string]any
	raw, _ := io.ReadAll(resp.Body)
	_ = json.Unmarshal(raw, &out)
	return out
}

// TestInvalidSignatureRejectedBeforePersistence pins AUT-002: a tampered/unsigned/stale POST is 401 with
// ZERO trigger_deliveries rows and exactly ONE sanitized audit line (trigger id + reason, never the
// payload/signature/secret bytes).
func TestInvalidSignatureRejectedBeforePersistence(t *testing.T) {
	h := newInboundHarness(t, 0, 0)
	ts := time.Now()
	body := []byte(`{"source":"harness","data":{"order":{"id":"o1","summary":"do it"}}}`)

	// Tampered body: signed over the original, but a byte flipped in flight.
	tampered := append([]byte{}, body...)
	tampered[len(tampered)-3] ^= 0x01
	headers := webhook.NewSigner(h.secret).Headers("evt-tamper", ts, 1, body)
	resp := h.postRaw(h.triggerID, headers, tampered)
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("tampered POST status = %d, want 401", resp.StatusCode)
	}
	resp.Body.Close()

	// Unsigned: no signature headers at all.
	resp = h.postRaw(h.triggerID, map[string]string{"Content-Type": "application/json"}, body)
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("unsigned POST status = %d, want 401", resp.StatusCode)
	}
	resp.Body.Close()

	// Stale: correctly signed but far outside the 5m tolerance.
	resp = h.post(h.triggerID, "evt-stale", ts.Add(-10*time.Minute), body, h.secret)
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("stale POST status = %d, want 401", resp.StatusCode)
	}
	resp.Body.Close()

	if got := h.deliveryRows(); got != 0 {
		t.Fatalf("rejected POSTs left %d delivery rows, want 0 (verification before persistence)", got)
	}
	if h.audit.count() != 3 {
		t.Fatalf("audit lines = %d, want 3 (one per reject)", h.audit.count())
	}
	log := h.audit.all()
	if !strings.Contains(log, h.triggerID) {
		t.Fatalf("audit log missing trigger id: %q", log)
	}
	// The sanitized line must never carry the raw body, the signature, or the secret bytes.
	for _, leak := range []string{string(body), string(h.secret), webhook.NewSigner(h.secret).Headers("evt-tamper", ts, 1, body)[webhook.HeaderSignature]} {
		if strings.Contains(log, leak) {
			t.Fatalf("audit log leaked a sensitive value: %q", log)
		}
	}
}

// TestAckOnlyAfterDurableRecordAndDedupe pins durable ack (B4): a 202 implies the row is committed with
// the source cols + raw_payload + principal_id=trigger.created_by (queried directly), and the inline
// map→admit reached run_created before the response.
func TestAckOnlyAfterDurableRecordAndDedupe(t *testing.T) {
	h := newInboundHarness(t, 0, 0)
	body := []byte(`{"source":"harness","source_tenant":"acme","data":{"order":{"id":"o-ack","summary":"summarize"}}}`)
	resp := h.post(h.triggerID, "evt-ack", time.Now(), body, h.secret)
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("valid POST status = %d, want 202", resp.StatusCode)
	}
	out := decodeBody(t, resp)
	id, _ := out["id"].(string)
	if id == "" {
		t.Fatalf("202 body carried no delivery id: %v", out)
	}
	if out["state"] != "run_created" {
		t.Fatalf("delivery state = %v, want run_created (inline map→admit before ack)", out["state"])
	}

	// The durable row is committed with the source envelope + raw_payload + the trigger's created_by.
	var src, srcTenant, srcEvent, principal string
	var raw []byte
	if err := h.pool.QueryRow(context.Background(),
		`SELECT source, source_tenant, source_event_id, principal_id, raw_payload FROM trigger_deliveries WHERE id=$1`, id).
		Scan(&src, &srcTenant, &srcEvent, &principal, &raw); err != nil {
		t.Fatalf("read durable row error = %v", err)
	}
	if src != "harness" || srcTenant != "acme" || srcEvent != "evt-ack" {
		t.Fatalf("source cols = (%q,%q,%q), want (harness,acme,evt-ack)", src, srcTenant, srcEvent)
	}
	if principal != h.principal {
		t.Fatalf("principal_id = %q, want the trigger's created_by %q", principal, h.principal)
	}
	if len(raw) == 0 {
		t.Fatal("raw_payload was not persisted")
	}
}

// TestDuplicateSourceEventSingleActionOriginalLinkage pins AUT-001 inbound: two POSTs of the same
// (source, source_tenant, source_event_id) yield ONE canonical delivery + ONE run; the second is a
// duplicate linked to the original and bears no run.
func TestDuplicateSourceEventSingleActionOriginalLinkage(t *testing.T) {
	h := newInboundHarness(t, 0, 0)
	body := []byte(`{"source":"harness","data":{"order":{"id":"o-dup","summary":"once"}}}`)

	first := decodeBody(t, h.post(h.triggerID, "evt-dup", time.Now(), body, h.secret))
	if first["state"] != "run_created" {
		t.Fatalf("first state = %v, want run_created", first["state"])
	}
	second := decodeBody(t, h.post(h.triggerID, "evt-dup", time.Now(), body, h.secret))
	if second["state"] != "duplicate" {
		t.Fatalf("second state = %v, want duplicate", second["state"])
	}
	if second["duplicate_of"] != first["id"] {
		t.Fatalf("second linked to %v, want original %v", second["duplicate_of"], first["id"])
	}

	// Exactly one canonical (non-duplicate) row → exactly one run.
	var canonical, withRun int
	if err := h.pool.QueryRow(context.Background(),
		`SELECT count(*) FILTER (WHERE duplicate_of IS NULL), count(*) FILTER (WHERE run_id <> '')
		 FROM trigger_deliveries WHERE trigger_id=$1 AND source_event_id='evt-dup'`, h.triggerID).Scan(&canonical, &withRun); err != nil {
		t.Fatalf("count error = %v", err)
	}
	if canonical != 1 || withRun != 1 {
		t.Fatalf("canonical=%d runs=%d, want exactly 1 and 1", canonical, withRun)
	}
}

// TestRedeliveryAfterLostAckDoesNotDuplicate pins AUT-009: a fully-processed event re-POSTed as if the
// 2xx was lost acks 2xx with duplicate linkage and adds no run.
func TestRedeliveryAfterLostAckDoesNotDuplicate(t *testing.T) {
	h := newInboundHarness(t, 0, 0)
	body := []byte(`{"source":"harness","data":{"order":{"id":"o-re","summary":"again"}}}`)
	first := decodeBody(t, h.post(h.triggerID, "evt-re", time.Now(), body, h.secret))

	resp := h.post(h.triggerID, "evt-re", time.Now(), body, h.secret)
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("redelivery status = %d, want 202 (idempotent ack)", resp.StatusCode)
	}
	redel := decodeBody(t, resp)
	if redel["state"] != "duplicate" || redel["duplicate_of"] != first["id"] {
		t.Fatalf("redelivery = %v, want duplicate linked to %v", redel, first["id"])
	}

	var runs int
	if err := h.pool.QueryRow(context.Background(),
		`SELECT count(*) FROM trigger_deliveries WHERE trigger_id=$1 AND run_id <> ''`, h.triggerID).Scan(&runs); err != nil {
		t.Fatalf("count runs error = %v", err)
	}
	if runs != 1 {
		t.Fatalf("run count after redelivery = %d, want 1", runs)
	}
}

// newTrigger creates a webhook trigger (as the harness principal) with the given revision config and the
// harness's primary secret ref, so h.post(...) with h.secret signs for it too. Returns the trigger id.
func (h *inboundHarness) newTrigger(in automation.TriggerRevisionInput) string {
	h.t.Helper()
	ctx := context.Background()
	id, err := h.store.CreateTrigger(ctx, h.org, h.proj, h.principal, randID("t"), "webhook")
	if err != nil {
		h.t.Fatalf("newTrigger CreateTrigger error = %v", err)
	}
	if _, err := h.store.ReviseTrigger(ctx, h.org, h.proj, id, in); err != nil {
		h.t.Fatalf("newTrigger ReviseTrigger error = %v", err)
	}
	if err := h.store.SetInboundSecretRefs(ctx, h.org, h.proj, id, inboundSecretRef, ""); err != nil {
		h.t.Fatalf("newTrigger SetInboundSecretRefs error = %v", err)
	}
	return id
}

// activeRevision reads a trigger's active (highest) revision id, for directly seeding delivery rows.
func (h *inboundHarness) activeRevision(triggerID string) string {
	h.t.Helper()
	var rev string
	if err := h.pool.QueryRow(context.Background(),
		`SELECT id FROM trigger_revisions WHERE trigger_id=$1 ORDER BY revision_number DESC LIMIT 1`, triggerID).Scan(&rev); err != nil {
		h.t.Fatalf("read active revision error = %v", err)
	}
	return rev
}

// seedInboundRow durably inserts an inbound delivery in a non-terminal state with a past updated_at,
// modelling a crash after the durable insert (the continuation never ran). raw is the whole envelope.
func (h *inboundHarness) seedInboundRow(triggerID, eventID, envelope, state string, ageSeconds int) string {
	h.t.Helper()
	id := randID("tdel")
	rev := h.activeRevision(triggerID)
	if _, err := h.pool.Exec(context.Background(),
		`INSERT INTO trigger_deliveries
		   (id, organization_id, project_id, trigger_id, trigger_revision_id, principal_id,
		    source, source_tenant, source_event_id, raw_payload, state, received_at, updated_at)
		 VALUES ($1,$2,$3,$4,$5,$6,'harness','',$7,$8,$9,
		         clock_timestamp() - make_interval(secs => $10), clock_timestamp() - make_interval(secs => $10))`,
		id, h.org, h.proj, triggerID, rev, h.principal, eventID, []byte(envelope), state, ageSeconds); err != nil {
		h.t.Fatalf("seed inbound row error = %v", err)
	}
	return id
}

// patchSecretRefs rotates the trigger's inbound secret handles over the AUTHENTICATED PATCH route.
func (h *inboundHarness) patchSecretRefs(triggerID, ref, refNext string) {
	h.t.Helper()
	body, _ := json.Marshal(map[string]string{"inbound_secret_ref": ref, "inbound_secret_ref_next": refNext})
	req, _ := http.NewRequest(http.MethodPatch, h.srv.URL+"/v1/triggers/"+triggerID, strings.NewReader(string(body)))
	req.Header.Set("Authorization", "Bearer "+h.token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		h.t.Fatalf("PATCH secret refs error = %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(resp.Body)
		h.t.Fatalf("PATCH secret refs status = %d body = %s", resp.StatusCode, raw)
	}
}

// TestFloodBoundsMemoryReportsDepthApplies429 pins AUT-010 backpressure: an oversized body is 413; a
// per-trigger durable backlog at its ceiling sheds 429 + Retry-After with depth + oldest-age while OTHER
// triggers keep flowing; and the in-flight semaphore bounds concurrency (excess sheds rather than buffers).
func TestFloodBoundsMemoryReportsDepthApplies429(t *testing.T) {
	valid := []byte(`{"source":"harness","data":{"order":{"id":"o","summary":"s"}}}`)

	// (a) Size cap → 413 (the first gate, before any signature work).
	h := newInboundHarness(t, 0, 0)
	huge := make([]byte, (1<<20)+64)
	for i := range huge {
		huge[i] = 'a'
	}
	resp := h.postRaw(h.triggerID, map[string]string{"Content-Type": "application/json"}, huge)
	if resp.StatusCode != http.StatusRequestEntityTooLarge {
		t.Fatalf("oversized POST status = %d, want 413", resp.StatusCode)
	}
	resp.Body.Close()

	// (b) Backlog at ceiling → 429 + Retry-After + depth/oldest-age; a different trigger keeps flowing.
	hb := newInboundHarness(t, 0, 2)
	for i := 0; i < 3; i++ {
		hb.seedInboundRow(hb.triggerID, "backlog-"+strconv.Itoa(i), `{"source":"harness","data":{}}`, "received", 1)
	}
	resp = hb.post(hb.triggerID, "evt-flood", time.Now(), valid, hb.secret)
	if resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("flooded trigger status = %d, want 429", resp.StatusCode)
	}
	if resp.Header.Get("Retry-After") == "" {
		t.Fatal("429 carried no Retry-After header")
	}
	shed := decodeBody(t, resp)
	if shed["queue_depth"] == nil || shed["oldest_age_seconds"] == nil {
		t.Fatalf("429 body missing depth/oldest-age: %v", shed)
	}
	if d, _ := shed["queue_depth"].(float64); int(d) < 2 {
		t.Fatalf("reported queue_depth = %v, want >= 2 (the ceiling)", shed["queue_depth"])
	}
	other := hb.newTrigger(automation.TriggerRevisionInput{InputMapping: []byte(`{"fields":{"input":{"select":"order.summary"}},"required":["input"]}`)})
	resp = hb.post(other, "evt-other", time.Now(), valid, hb.secret)
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("other-trigger POST status = %d, want 202 (a flooded trigger must not block others)", resp.StatusCode)
	}
	resp.Body.Close()

	// (c) In-flight semaphore (maxInflight=1) bounds concurrency: a concurrent burst sheds some 429s
	// (memory stays bounded — excess is rejected, not buffered), and no request errors.
	hc := newInboundHarness(t, 1, 0)
	var mu sync.Mutex
	codes := map[int]int{}
	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			r := hc.post(hc.triggerID, "evt-c-"+strconv.Itoa(i), time.Now(), valid, hc.secret)
			mu.Lock()
			codes[r.StatusCode]++
			mu.Unlock()
			r.Body.Close()
		}(i)
	}
	wg.Wait()
	if codes[http.StatusTooManyRequests] == 0 {
		t.Fatalf("maxInflight=1 burst produced no 429 — the in-flight cap did not engage: %v", codes)
	}
	if codes[http.StatusAccepted] == 0 {
		t.Fatalf("maxInflight=1 burst admitted nothing: %v", codes)
	}
}

// TestPoisonEventDeadLettersAndAdvancesOrderedKey pins §34.3: a signed but unmappable event terminalizes
// `failed` (the dead-letter view IS the row), bears NO run, and does NOT hold the ordered correlation key —
// a later good event with the same key admits. The sweep variant terminalizes a stranded poison remnant.
func TestPoisonEventDeadLettersAndAdvancesOrderedKey(t *testing.T) {
	h := newInboundHarness(t, 0, 0)
	// A queue trigger keyed on data.key so events serialize per key; the mapping requires order.summary.
	trg := h.newTrigger(automation.TriggerRevisionInput{
		InputMapping:       []byte(`{"fields":{"input":{"select":"order.summary"}},"required":["input"]}`),
		ConcurrencyPolicy:  "queue",
		CorrelationKeyExpr: `{"select":"key"}`,
	})

	poison := decodeBody(t, h.post(trg, "evt-poison", time.Now(), []byte(`{"source":"harness","data":{"key":"k","nope":1}}`), h.secret))
	if poison["state"] != "failed" {
		t.Fatalf("poison state = %v, want failed", poison["state"])
	}
	if rid, _ := poison["run_id"].(string); rid != "" {
		t.Fatal("poison event bore a run; it must bear none")
	}
	if view := h.getDelivery(poison["id"].(string)); view["state"] != "failed" {
		t.Fatalf("dead-letter view state = %v, want failed", view["state"])
	}

	// The ordered key ADVANCES: a later good event with the SAME key admits (failed never held the gate).
	good := decodeBody(t, h.post(trg, "evt-good", time.Now(), []byte(`{"source":"harness","data":{"key":"k","order":{"summary":"do"}}}`), h.secret))
	if good["state"] != "run_created" {
		t.Fatalf("good event after poison = %v, want run_created (the failed poison must not hold the key)", good["state"])
	}

	// Sweep variant: a stranded received remnant whose data is unmappable terminalizes `failed`, not a loop.
	remnant := h.seedInboundRow(h.triggerID, "evt-remnant", `{"source":"harness","data":{"nope":1}}`, "received", 5)
	if err := automation.NewDeliveryReconciler(h.store, time.Second, 0, 100, nil).Tick(context.Background()); err != nil {
		t.Fatalf("Tick error = %v", err)
	}
	if view := h.getDelivery(remnant); view["state"] != "failed" {
		t.Fatalf("swept poison remnant state = %v, want failed", view["state"])
	}
}

// TestInboundSecretRotationBothRefsVerifyWindowBounded pins §21.4 rotation over the PATCH surface: with
// both refs set, events signed under EITHER secret verify; after cutover (next promoted, old dropped) the
// old signature fails while the new one still verifies. Rotation is a mutable-column PATCH, not a revise.
func TestInboundSecretRotationBothRefsVerifyWindowBounded(t *testing.T) {
	h := newInboundHarness(t, 0, 0)
	newSecret := []byte("whsec_rotated_" + randID("n"))
	h.secretsByRef["src-next"] = newSecret

	// Overlap: both refs active.
	h.patchSecretRefs(h.triggerID, inboundSecretRef, "src-next")
	if got := h.post(h.triggerID, "evt-old", time.Now(), []byte(`{"source":"harness","data":{"order":{"id":"r1","summary":"a"}}}`), h.secret); got.StatusCode != http.StatusAccepted {
		t.Fatalf("event under the OLD secret during overlap = %d, want 202", got.StatusCode)
	} else {
		got.Body.Close()
	}
	if got := h.post(h.triggerID, "evt-new", time.Now(), []byte(`{"source":"harness","data":{"order":{"id":"r2","summary":"b"}}}`), newSecret); got.StatusCode != http.StatusAccepted {
		t.Fatalf("event under the NEW secret during overlap = %d, want 202", got.StatusCode)
	} else {
		got.Body.Close()
	}

	// Cutover: promote next, drop old.
	h.patchSecretRefs(h.triggerID, "src-next", "")
	if got := h.post(h.triggerID, "evt-old2", time.Now(), []byte(`{"source":"harness","data":{"order":{"id":"r3","summary":"c"}}}`), h.secret); got.StatusCode != http.StatusUnauthorized {
		t.Fatalf("OLD secret after cutover = %d, want 401 (bounded rotation window)", got.StatusCode)
	} else {
		got.Body.Close()
	}
	if got := h.post(h.triggerID, "evt-new2", time.Now(), []byte(`{"source":"harness","data":{"order":{"id":"r4","summary":"d"}}}`), newSecret); got.StatusCode != http.StatusAccepted {
		t.Fatalf("NEW secret after cutover = %d, want 202", got.StatusCode)
	} else {
		got.Body.Close()
	}
}

// TestRawPayloadScrubbedAfterTerminalTTL pins the short-retention behavior: a TERMINAL inbound row older
// than the raw TTL has its raw_payload NULLed by the sweep, while a fresh terminal row keeps it. "Short
// retention" is a behavior, not a caption (encryption-at-rest is E13 — no encrypted claim).
func TestRawPayloadScrubbedAfterTerminalTTL(t *testing.T) {
	h := newInboundHarness(t, 0, 0)
	old := h.seedInboundRow(h.triggerID, "evt-old-terminal", `{"source":"harness","data":{"order":{"summary":"x"}}}`, "run_created", 3600)
	fresh := h.seedInboundRow(h.triggerID, "evt-fresh-terminal", `{"source":"harness","data":{"order":{"summary":"y"}}}`, "run_created", 1)

	rec := automation.NewDeliveryReconciler(h.store, time.Second, time.Hour, 100, nil).WithInboundRawTTL(10 * time.Minute)
	if err := rec.Tick(context.Background()); err != nil {
		t.Fatalf("Tick error = %v", err)
	}

	raw := func(id string) []byte {
		var b []byte
		if err := h.pool.QueryRow(context.Background(), `SELECT raw_payload FROM trigger_deliveries WHERE id=$1`, id).Scan(&b); err != nil {
			t.Fatalf("read raw_payload error = %v", err)
		}
		return b
	}
	if raw(old) != nil {
		t.Fatalf("terminal row older than the TTL kept its raw_payload; want scrubbed")
	}
	if raw(fresh) == nil {
		t.Fatalf("terminal row within the TTL was scrubbed; want retained")
	}
}

// nonTerminalBacklog counts a trigger's non-terminal inbound rows (the AUT-010 backlog set) — a poison
// row that never terminalizes would stay counted here forever (review #1).
func (h *inboundHarness) nonTerminalBacklog(triggerID string) int {
	h.t.Helper()
	var n int
	if err := h.pool.QueryRow(context.Background(),
		`SELECT count(*) FROM trigger_deliveries WHERE trigger_id=$1 AND source_event_id <> ''
		   AND state IN ('received','authenticated','deduplicated','mapped','admitted','deferred')`, triggerID).Scan(&n); err != nil {
		h.t.Fatalf("count backlog error = %v", err)
	}
	return n
}

// TestNonObjectPayloadPoisonTerminalizesNotRetries pins review #1: a signed event whose `data` is NOT a
// JSON object is poison BEFORE the deduplicated state, so it must terminalize `failed` (not error the
// continuation and leave a received row the sweep retries forever + counts in the backlog + never scrubs).
// Both the inline path and the sweep re-drive must reach `failed`.
func TestNonObjectPayloadPoisonTerminalizesNotRetries(t *testing.T) {
	h := newInboundHarness(t, 0, 0)

	// Inline: a non-object data payload terminalizes `failed`, bears no run, and 202s (durably dead-lettered).
	res := h.post(h.triggerID, "evt-nonobj", time.Now(), []byte(`{"source":"harness","data":5}`), h.secret)
	if res.StatusCode != http.StatusAccepted {
		t.Fatalf("non-object poison status = %d, want 202 (durably dead-lettered)", res.StatusCode)
	}
	out := decodeBody(t, res)
	if out["state"] != "failed" {
		t.Fatalf("non-object poison state = %v, want failed", out["state"])
	}
	if rid, _ := out["run_id"].(string); rid != "" {
		t.Fatal("non-object poison bore a run; it must bear none")
	}
	id, _ := out["id"].(string)
	if view := h.getDelivery(id); view["state"] != "failed" {
		t.Fatalf("dead-letter view state = %v, want failed", view["state"])
	}

	// The sweep does NOT retry it, and it no longer counts in the backlog.
	rec := automation.NewDeliveryReconciler(h.store, time.Second, 0, 100, nil)
	for i := 0; i < 2; i++ {
		if err := rec.Tick(context.Background()); err != nil {
			t.Fatalf("Tick %d error = %v", i, err)
		}
	}
	if view := h.getDelivery(id); view["state"] != "failed" {
		t.Fatalf("after sweeps, poison state = %v, want failed (no retry loop)", view["state"])
	}
	if n := h.nonTerminalBacklog(h.triggerID); n != 0 {
		t.Fatalf("terminalized poison still counts %d in the backlog, want 0", n)
	}

	// Sweep re-drive of a stranded non-object remnant (a JSON array `data`) also terminalizes `failed`.
	remnant := h.seedInboundRow(h.triggerID, "evt-nonobj-remnant", `{"source":"harness","data":[1,2,3]}`, "received", 5)
	if err := rec.Tick(context.Background()); err != nil {
		t.Fatalf("Tick error = %v", err)
	}
	if view := h.getDelivery(remnant); view["state"] != "failed" {
		t.Fatalf("swept non-object remnant state = %v, want failed", view["state"])
	}
}

// TestEmptyEventIdRejectedBeforePersistence pins review #2 end-to-end: a signed event with an EMPTY
// Webhook-Id is a malformed envelope → 400 with ZERO delivery rows (an empty source_event_id would be
// invisible to the dedupe index / sweep / backlog / scrub).
func TestEmptyEventIdRejectedBeforePersistence(t *testing.T) {
	h := newInboundHarness(t, 0, 0)
	// h.post signs with the given event id; "" produces a signed but empty Webhook-Id.
	resp := h.post(h.triggerID, "", time.Now(), []byte(`{"source":"harness","data":{"order":{"summary":"x"}}}`), h.secret)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("empty-id POST status = %d, want 400", resp.StatusCode)
	}
	resp.Body.Close()
	if got := h.deliveryRows(); got != 0 {
		t.Fatalf("empty-id POST left %d rows, want 0", got)
	}
}

// TestTriggerWithoutCreatedByIsUnavailable pins review #3: a webhook trigger whose created_by was never
// stamped (a pre-023 lineage that later gets a secret ref) is NOT ingestible — admitting with an empty
// principal would blow the principals FK and strand the row in the sweep loop. It is a generic 404, zero rows.
func TestTriggerWithoutCreatedByIsUnavailable(t *testing.T) {
	h := newInboundHarness(t, 0, 0)
	ctx := context.Background()
	// A webhook trigger created with an EMPTY principal (created_by=''), given a revision + secret ref.
	trg, err := h.store.CreateTrigger(ctx, h.org, h.proj, "", randID("no-principal"), "webhook")
	if err != nil {
		t.Fatalf("CreateTrigger error = %v", err)
	}
	if _, err := h.store.ReviseTrigger(ctx, h.org, h.proj, trg, automation.TriggerRevisionInput{
		InputMapping: []byte(`{"fields":{"input":{"select":"order.summary"}},"required":["input"]}`),
	}); err != nil {
		t.Fatalf("ReviseTrigger error = %v", err)
	}
	if err := h.store.SetInboundSecretRefs(ctx, h.org, h.proj, trg, inboundSecretRef, ""); err != nil {
		t.Fatalf("SetInboundSecretRefs error = %v", err)
	}
	resp := h.post(trg, "evt-noprin", time.Now(), []byte(`{"source":"harness","data":{"order":{"summary":"x"}}}`), h.secret)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("no-principal trigger POST status = %d, want 404", resp.StatusCode)
	}
	resp.Body.Close()
	var rows int
	if err := h.pool.QueryRow(ctx, `SELECT count(*) FROM trigger_deliveries WHERE trigger_id=$1`, trg).Scan(&rows); err != nil {
		t.Fatalf("count rows error = %v", err)
	}
	if rows != 0 {
		t.Fatalf("no-principal trigger left %d rows, want 0", rows)
	}
}

// TestConcurrentSameSourceEventSingleCanonical pins the source-dedupe RACE (review minor-5, the T2 idiom):
// two goroutines POST the SAME (source, source_tenant, source_event_id) at once → exactly ONE canonical
// delivery + ONE run; the loser is a duplicate linked to the canonical, decided by the UNIQUE index (not
// app logic).
func TestConcurrentSameSourceEventSingleCanonical(t *testing.T) {
	h := newInboundHarness(t, 0, 0)
	body := []byte(`{"source":"harness","data":{"order":{"id":"o-race","summary":"once"}}}`)

	var wg sync.WaitGroup
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			resp := h.post(h.triggerID, "evt-race", time.Now(), body, h.secret)
			resp.Body.Close()
		}()
	}
	wg.Wait()

	var canonical, runs, dups int
	if err := h.pool.QueryRow(context.Background(),
		`SELECT count(*) FILTER (WHERE duplicate_of IS NULL),
		        count(*) FILTER (WHERE run_id <> ''),
		        count(*) FILTER (WHERE state='duplicate')
		 FROM trigger_deliveries WHERE trigger_id=$1 AND source_event_id='evt-race'`, h.triggerID).
		Scan(&canonical, &runs, &dups); err != nil {
		t.Fatalf("count error = %v", err)
	}
	if canonical != 1 || runs != 1 || dups != 1 {
		t.Fatalf("race result canonical=%d runs=%d dups=%d, want 1/1/1 (the index decides)", canonical, runs, dups)
	}
}

// TestInboundRejectCarriesCorrelationID pins review minor-6: the inbound route is wrapped in
// RequestContext (only Auth is bypassed), so a reject still stamps the Request-Id correlation header.
func TestInboundRejectCarriesCorrelationID(t *testing.T) {
	h := newInboundHarness(t, 0, 0)
	resp := h.postRaw(h.triggerID, map[string]string{"Content-Type": "application/json"}, []byte(`{"source":"harness"}`))
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("unsigned POST status = %d, want 401", resp.StatusCode)
	}
	if resp.Header.Get("Request-Id") == "" {
		t.Fatal("inbound reject carried no Request-Id header (RequestContext not wired on the top-mux route)")
	}
}
