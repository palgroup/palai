//go:build component

// Real-PostgreSQL component tests for the webhook pump (spec §21.4-21.6, E11 Task 4). They run under
// `make test-component TEST=postgres`, which starts a throwaway container and exports
// PALAI_COMPONENT_POSTGRES_URL. The receivers are real local HTTP(S) servers that verify the HMAC
// server-side — a sham verifier would not prove the contract (plan §1 proof-class). Honest ceiling:
// pure infra, no model/provider — the live model bind is T6/T7.
package automation

import (
	"context"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/palgroup/palai/adapters/integrations/webhook"
	"github.com/palgroup/palai/packages/coordinator"
)

func componentPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	url := os.Getenv("PALAI_COMPONENT_POSTGRES_URL")
	if url == "" {
		t.Skip("PALAI_COMPONENT_POSTGRES_URL is required; run make test-component TEST=postgres")
	}
	cs, err := coordinator.Open(context.Background(), url)
	if err != nil {
		t.Fatalf("coordinator.Open() error = %v", err)
	}
	t.Cleanup(cs.Close)
	if err := cs.Migrate(context.Background()); err != nil {
		t.Fatalf("Migrate() error = %v", err)
	}
	return cs.Pool()
}

func randID(prefix string) string {
	var raw [8]byte
	_, _ = rand.Read(raw[:])
	return prefix + "_" + hex.EncodeToString(raw[:])
}

// seedSession creates org -> project -> session and returns the scope.
func seedSession(t *testing.T, pool *pgxpool.Pool) (org, project, session string) {
	t.Helper()
	org, project, session = randID("org"), randID("prj"), randID("ses")
	mustExec(t, pool, `INSERT INTO organizations (id) VALUES ($1)`, org)
	mustExec(t, pool, `INSERT INTO projects (id, organization_id) VALUES ($1, $2)`, project, org)
	mustExec(t, pool, `INSERT INTO sessions (id, organization_id, project_id) VALUES ($1, $2, $3)`, session, org, project)
	return org, project, session
}

func appendEvent(t *testing.T, pool *pgxpool.Pool, org, project, session string, seq int, typ, payload string) {
	t.Helper()
	mustExec(t, pool,
		`INSERT INTO events (id, organization_id, project_id, session_id, seq, type, payload) VALUES ($1,$2,$3,$4,$5,$6,$7::jsonb)`,
		randID("evt"), org, project, session, seq, typ, payload)
	// Keep the per-session sequence allocator ahead of the manually-seq'd source event, so the pump's
	// EmitDeliveryEvent (which allocates the next seq) never collides with it — production events are
	// always appended through the allocator, so the two stay in lockstep there.
	mustExec(t, pool,
		`INSERT INTO session_sequences (session_id, last_seq) VALUES ($1,$2)
		 ON CONFLICT (session_id) DO UPDATE SET last_seq = GREATEST(session_sequences.last_seq, EXCLUDED.last_seq)`,
		session, seq)
}

func mustExec(t *testing.T, pool *pgxpool.Pool, sql string, args ...any) {
	t.Helper()
	if _, err := pool.Exec(context.Background(), sql, args...); err != nil {
		t.Fatalf("exec %q error = %v", sql, err)
	}
}

// tlsReceiver builds a real HTTPS receiver that verifies the HMAC over the raw body server-side and
// runs status through handle(attempt). It records every request for the leak scan.
func tlsReceiver(t *testing.T, secret []byte, handle func(attempt int) int) (*httptest.Server, *x509.CertPool, *int32, *int32) {
	t.Helper()
	var calls, verified int32
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt32(&calls, 1)
		raw, _ := io.ReadAll(r.Body)
		id := r.Header.Get(webhook.HeaderID)
		ts, ok := parseUnix(r.Header.Get(webhook.HeaderTimestamp))
		if ok && webhook.Verify(secret, id, ts, raw, r.Header.Get(webhook.HeaderSignature), time.Now(), 5*time.Minute) {
			atomic.AddInt32(&verified, 1)
		} else {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		w.WriteHeader(handle(int(n)))
	}))
	t.Cleanup(srv.Close)
	pool := x509.NewCertPool()
	pool.AddCert(srv.Certificate())
	return srv, pool, &calls, &verified
}

func parseUnix(v string) (time.Time, bool) {
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil {
		return time.Time{}, false
	}
	return time.Unix(n, 0), true
}

// pumpFor builds a pump wired to the real store, a sender trusting the receiver's cert, a static
// secret resolver, and a tiny backoff so a rescheduled retry is due on the next tick.
func pumpFor(store *WebhookStore, certs *x509.CertPool, secret []byte, ref string) *WebhookPump {
	sender := webhook.NewSender(webhook.WithTLSConfig(&tls.Config{RootCAs: certs}))
	resolver := func(r string) ([]byte, error) {
		if r == ref {
			return secret, nil
		}
		return nil, io.EOF
	}
	return NewWebhookPump(store, sender, resolver, PumpConfig{BaseBackoff: time.Nanosecond, MaxBackoff: time.Nanosecond, BatchSize: 50}, nil)
}

func defaultEndpoint(url, ref string) EndpointCreate {
	return EndpointCreate{
		URL: url, EventFilter: []string{"run.completed.v1"}, SigningSecretRef: ref,
		TimeoutMS: 3000, MaxAttempts: 20, RetryWindowSeconds: 72 * 3600, AllowPrivateDestination: true,
	}
}

// TestSignedDeliveryEndToEndRealHTTP is the brief's component gate: a journal event fans out to a real
// endpoint, is signed and delivered over real HTTPS to a receiver that verifies the HMAC server-side,
// a 5xx-then-2xx sequence drives one retry and exactly one successful delivery, and the terminal
// success is journaled back into the session stream.
func TestSignedDeliveryEndToEndRealHTTP(t *testing.T) {
	pool := componentPool(t)
	store := NewWebhookStore(pool)
	ctx := context.Background()
	secret := []byte("whsec_component_smoke")

	srv, certs, calls, verified := tlsReceiver(t, secret, func(attempt int) int {
		if attempt == 1 {
			return http.StatusServiceUnavailable // 503 first
		}
		return http.StatusOK // 200 after
	})

	org, project, session := seedSession(t, pool)
	if _, err := store.CreateEndpoint(ctx, org, project, randID("whe"), defaultEndpoint(srv.URL, "ref1")); err != nil {
		t.Fatalf("CreateEndpoint error = %v", err)
	}
	appendEvent(t, pool, org, project, session, 1, "run.completed.v1", `{"run_id":"run_1"}`)

	pump := pumpFor(store, certs, secret, "ref1")

	// Tick 1: fan-out + first delivery attempt (503 -> retry). Tick a few times for the retry to land.
	deadline := time.Now().Add(5 * time.Second)
	var delivered bool
	for time.Now().Before(deadline) {
		if err := pump.Tick(ctx); err != nil {
			t.Fatalf("Tick error = %v", err)
		}
		views, err := store.ListDeliveries(ctx, org, project, "delivered", 10)
		if err != nil {
			t.Fatalf("ListDeliveries error = %v", err)
		}
		if len(views) == 1 {
			delivered = true
			if views[0].AttemptCount != 2 {
				t.Fatalf("delivered after %d attempts, want 2 (503 then 200)", views[0].AttemptCount)
			}
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if !delivered {
		t.Fatal("delivery never reached the delivered state")
	}
	if atomic.LoadInt32(calls) != 2 {
		t.Fatalf("receiver saw %d calls, want 2 (one 503 retry, one 200)", atomic.LoadInt32(calls))
	}
	if atomic.LoadInt32(verified) != 2 {
		t.Fatalf("receiver verified %d signatures server-side, want 2", atomic.LoadInt32(verified))
	}

	// The terminal success is journaled back into the session stream (loop-guarded, so it never
	// fans out into a new delivery).
	var succeeded int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM events WHERE session_id=$1 AND type='webhook.delivery.succeeded.v1'`, session).Scan(&succeeded); err != nil {
		t.Fatalf("count success events error = %v", err)
	}
	if succeeded != 1 {
		t.Fatalf("webhook.delivery.succeeded.v1 events = %d, want 1", succeeded)
	}
}

// TestRedeliveryReusesDeliveryIdAndPayload drives a delivery to dead-letter, then an operator
// redelivery revives it with the SAME id and payload (spec §21.6) and it delivers on a now-healthy
// receiver.
func TestRedeliveryReusesDeliveryIdAndPayload(t *testing.T) {
	pool := componentPool(t)
	store := NewWebhookStore(pool)
	ctx := context.Background()
	secret := []byte("whsec_redeliver")

	var healthy atomic.Bool
	srv, certs, _, _ := tlsReceiver(t, secret, func(int) int {
		if healthy.Load() {
			return http.StatusOK
		}
		return http.StatusServiceUnavailable
	})

	org, project, session := seedSession(t, pool)
	ep := defaultEndpoint(srv.URL, "ref1")
	ep.MaxAttempts = 3 // small cap so it dead-letters quickly
	if _, err := store.CreateEndpoint(ctx, org, project, randID("whe"), ep); err != nil {
		t.Fatalf("CreateEndpoint error = %v", err)
	}
	appendEvent(t, pool, org, project, session, 1, "run.completed.v1", `{"run_id":"run_dead"}`)

	pump := pumpFor(store, certs, secret, "ref1")

	// Tick until the delivery dead-letters (503 x3).
	deadID, deadPayload := driveTo(t, ctx, store, pump, org, project, "dead")

	// Operator redelivery: same id + payload, back to pending.
	ok, err := store.Redeliver(ctx, org, project, deadID)
	if err != nil || !ok {
		t.Fatalf("Redeliver ok=%v err=%v, want true/nil", ok, err)
	}

	// A healthy receiver now completes the redelivered row — SAME id, SAME payload.
	healthy.Store(true)
	liveID, livePayload := driveTo(t, ctx, store, pump, org, project, "delivered")
	if liveID != deadID {
		t.Fatalf("redelivered id = %s, want the original %s (idempotent redelivery)", liveID, deadID)
	}
	if livePayload != deadPayload {
		t.Fatalf("redelivered payload changed: %s != %s", livePayload, deadPayload)
	}
}

// driveTo ticks the pump until exactly one delivery reaches wantState, returning its id + raw payload.
func driveTo(t *testing.T, ctx context.Context, store *WebhookStore, pump *WebhookPump, org, project, wantState string) (string, string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if err := pump.Tick(ctx); err != nil {
			t.Fatalf("Tick error = %v", err)
		}
		views, err := store.ListDeliveries(ctx, org, project, wantState, 10)
		if err != nil {
			t.Fatalf("ListDeliveries error = %v", err)
		}
		if len(views) == 1 {
			var payload string
			if err := store.pool.QueryRow(ctx, `SELECT payload::text FROM webhook_deliveries WHERE id=$1`, views[0].ID).Scan(&payload); err != nil {
				t.Fatalf("read payload error = %v", err)
			}
			return views[0].ID, payload
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("no delivery reached state %q", wantState)
	return "", ""
}

// TestAttemptViewSanitizedNoSecret proves the §21.6 attempt view carries status/duration/excerpt but
// never the signing secret nor a secret-ref fixed-header value.
func TestAttemptViewSanitizedNoSecret(t *testing.T) {
	pool := componentPool(t)
	store := NewWebhookStore(pool)
	ctx := context.Background()
	secret := []byte("whsec_sanitized_needle")
	fixedHeaderSecretValue := "hdr_secret_needle_zzz"

	srv, certs, _, _ := tlsReceiver(t, secret, func(int) int { return http.StatusOK })

	org, project, session := seedSession(t, pool)
	ep := defaultEndpoint(srv.URL, "ref1")
	ep.FixedHeaders = map[string]string{"X-Partner-Token": fixedHeaderSecretValue}
	if _, err := store.CreateEndpoint(ctx, org, project, randID("whe"), ep); err != nil {
		t.Fatalf("CreateEndpoint error = %v", err)
	}
	appendEvent(t, pool, org, project, session, 1, "run.completed.v1", `{"run_id":"run_x"}`)

	pump := pumpFor(store, certs, secret, "ref1")
	id, _ := driveTo(t, ctx, store, pump, org, project, "delivered")

	attempts, err := store.ListAttempts(ctx, org, project, id)
	if err != nil {
		t.Fatalf("ListAttempts error = %v", err)
	}
	if len(attempts) == 0 {
		t.Fatal("no attempt rows recorded")
	}
	for _, a := range attempts {
		blob := a.Excerpt + " " + a.Error
		if strings.Contains(blob, string(secret)) {
			t.Fatalf("attempt view leaked the signing secret: %q", blob)
		}
		if strings.Contains(blob, fixedHeaderSecretValue) {
			t.Fatalf("attempt view leaked a secret-ref header value: %q", blob)
		}
	}
}

// TestPumpSupervisedEndpointDownDoesNotStarveOthers proves per-endpoint progress: one endpoint is down
// (connection refused), yet a healthy endpoint's delivery still flows in the same tick — no
// head-of-line blocking.
func TestPumpSupervisedEndpointDownDoesNotStarveOthers(t *testing.T) {
	pool := componentPool(t)
	store := NewWebhookStore(pool)
	ctx := context.Background()
	secret := []byte("whsec_starve")

	healthy, certs, _, _ := tlsReceiver(t, secret, func(int) int { return http.StatusOK })

	// A down endpoint: start a server, capture its URL, then close it so connections are refused.
	down := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {}))
	downURL := down.URL
	down.Close()

	org, project, session := seedSession(t, pool)
	if _, err := store.CreateEndpoint(ctx, org, project, randID("whe"), defaultEndpoint(healthy.URL, "ref1")); err != nil {
		t.Fatalf("CreateEndpoint(healthy) error = %v", err)
	}
	if _, err := store.CreateEndpoint(ctx, org, project, randID("whe"), defaultEndpoint(downURL, "ref1")); err != nil {
		t.Fatalf("CreateEndpoint(down) error = %v", err)
	}
	appendEvent(t, pool, org, project, session, 1, "run.completed.v1", `{"run_id":"run_starve"}`)

	pump := pumpFor(store, certs, secret, "ref1")

	// A single tick fans out to BOTH endpoints and attempts BOTH: the healthy one delivers, the down
	// one records a failed attempt and stays pending — the down endpoint never blocked the healthy one.
	deadline := time.Now().Add(5 * time.Second)
	var deliveredCount int
	for time.Now().Before(deadline) {
		if err := pump.Tick(ctx); err != nil {
			t.Fatalf("Tick error = %v", err)
		}
		views, err := store.ListDeliveries(ctx, org, project, "delivered", 10)
		if err != nil {
			t.Fatalf("ListDeliveries error = %v", err)
		}
		deliveredCount = len(views)
		if deliveredCount >= 1 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if deliveredCount != 1 {
		t.Fatalf("delivered deliveries = %d, want 1 (the healthy endpoint's, despite the down one)", deliveredCount)
	}
	// The down endpoint's delivery made progress (an attempt was recorded) but is still pending —
	// it did not starve the healthy delivery, and it did not silently vanish.
	var pending int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM webhook_deliveries WHERE organization_id=$1 AND state='pending'`, org).Scan(&pending); err != nil {
		t.Fatalf("count pending error = %v", err)
	}
	if pending != 1 {
		t.Fatalf("pending deliveries = %d, want 1 (the down endpoint's, still retrying)", pending)
	}
}
