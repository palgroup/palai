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
	t         *testing.T
	pool      *pgxpool.Pool
	store     *automation.TriggerStore
	srv       *httptest.Server
	audit     *recorder
	secret    []byte
	org, proj string
	principal string
	token     string
	triggerID string
}

const inboundSecretRef = "src-primary"

func newInboundHarness(t *testing.T) *inboundHarness {
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
	resolver := func(o, ref string) ([]byte, error) {
		if o == org && ref == inboundSecretRef {
			return secret, nil
		}
		return nil, os.ErrNotExist
	}
	audit := &recorder{}
	ts := automation.NewTriggerStore(pool).
		WithAdmitter(repo.Spine()).
		WithInboundSecrets(resolver).
		WithInboundGate(audit.Log, 5*time.Minute, 0, 0)

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

	router := api.NewRouter(repo, repo, repo, repo, repo, repo, automation.NewWebhookStore(pool), ts, nil, api.SSEConfig{}, nil)
	srv := httptest.NewServer(router)
	t.Cleanup(srv.Close)

	return &inboundHarness{t: t, pool: pool, store: ts, srv: srv, audit: audit, secret: secret,
		org: org, proj: proj, principal: principal, token: token, triggerID: triggerID}
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
	h := newInboundHarness(t)
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
	h := newInboundHarness(t)
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
	h := newInboundHarness(t)
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
	h := newInboundHarness(t)
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
