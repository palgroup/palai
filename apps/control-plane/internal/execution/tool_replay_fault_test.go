//go:build fault

// This is the fault-injection proof for the E10 T7 uncertain-tool reconciliation subsystem (spec
// §26.6-26.7, §26.10). It lives in package execution because it drives the in-process UncertainReconciler
// against real Postgres — an internal package a tests/fault/* package cannot import (the E09 T2
// internal-import placement rule). It runs under `make test-fault CASE=recovery`, which starts a
// throwaway PostgreSQL container and exports PALAI_FAULT_POSTGRES_URL. The build tag keeps it out of the
// credential-free, Docker-free unit tier.
//
// The invariant is the exit-gate core: DUPLICATE EXTERNAL EFFECT = 0 across a kill between a
// side-effecting tool's EXECUTE and its RECORD. A real local HTTP destination counts requests; a tool
// fires the effect, the process is killed before the commit (simulated at the seam: the durable
// 'executing' pre-write exists, the commit never landed), a fresh attempt marks the row `uncertain` and
// STOPS, and the reconcile loop PROBES the real destination — a reversible effect that landed reconciles
// to completed WITHOUT re-firing (the counter stays 1), an irreversible one escalates to
// manual_resolution and never auto-replays.
//
// This fault tier IS the provider-agnostic core of the reconcile invariant and runs here (CASE=recovery).
// The plan's LIVE case CASE=tool-replay-reconcile (a real provider driving the tool via a forced
// tool_call on the serialized :local stack) would wrap the SAME assertions with a real chatcmpl, but it
// is NOT wired: scripts/test/live-provider has no such case yet. Recorded deviation — the reconcile
// invariant is provider-independent, so the fault tier proves it; the live wrapper is a named follow-up
// (it adds only the real-provider round-trip, not new reconcile logic).
package execution

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/palgroup/palai/packages/coordinator"

	"github.com/palgroup/palai/storage"
)

func faultRecoveryURL(t *testing.T) string {
	t.Helper()
	url := os.Getenv("PALAI_FAULT_POSTGRES_URL")
	if url == "" {
		t.Skip("PALAI_FAULT_POSTGRES_URL is required; run make test-fault CASE=recovery")
	}
	return url
}

func openFaultStore(t *testing.T) *coordinator.Store {
	t.Helper()
	cs, err := coordinator.Open(context.Background(), faultRecoveryURL(t))
	if err != nil {
		t.Fatalf("coordinator.Open() error = %v", err)
	}
	t.Cleanup(cs.Close)
	if err := cs.Migrate(context.Background()); err != nil {
		t.Fatalf("Migrate() error = %v", err)
	}
	return cs
}

func faultID(prefix string) string {
	var raw [8]byte
	_, _ = rand.Read(raw[:])
	return prefix + "_" + hex.EncodeToString(raw[:])
}

func seedFaultRun(t *testing.T, cs *coordinator.Store) (coordinator.Tenant, string, string) {
	t.Helper()
	ctx := context.Background()
	tenant := coordinator.Tenant{Organization: faultID("org"), Project: faultID("prj")}
	sessionID, runID := faultID("ses"), faultID("run")
	for _, q := range []struct {
		sql  string
		args []any
	}{
		{`INSERT INTO organizations (id) VALUES ($1)`, []any{tenant.Organization}},
		{`INSERT INTO projects (id, organization_id) VALUES ($1, $2)`, []any{tenant.Project, tenant.Organization}},
		{`INSERT INTO sessions (id, organization_id, project_id) VALUES ($1, $2, $3)`, []any{sessionID, tenant.Organization, tenant.Project}},
		{`INSERT INTO runs (id, organization_id, project_id, session_id, state) VALUES ($1, $2, $3, $4, 'running')`, []any{runID, tenant.Organization, tenant.Project, sessionID}},
	} {
		if _, err := cs.Pool().Exec(storage.WithSystemScope(ctx), q.sql, q.args...); err != nil {
			t.Fatalf("seed exec %q error = %v", q.sql, err)
		}
	}
	return tenant, sessionID, runID
}

// faultDestination is a faithful external destination double: POST creates one object per idempotency
// key (folding retries), GET /?key= reports whether the object exists (the tool's own read surface the
// reconcile prober queries). It counts POSTs so a duplicate external effect is observable.
type faultDestination struct {
	mu      sync.Mutex
	objects map[string]bool
	posts   int32
	server  *httptest.Server
}

func newFaultDestination() *faultDestination {
	d := &faultDestination{objects: map[string]bool{}}
	d.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		key := r.URL.Query().Get("key")
		switch r.Method {
		case http.MethodPost:
			atomic.AddInt32(&d.posts, 1)
			d.mu.Lock()
			d.objects[key] = true
			d.mu.Unlock()
			w.WriteHeader(http.StatusOK)
		case http.MethodGet: // the probe surface
			d.mu.Lock()
			exists := d.objects[key]
			d.mu.Unlock()
			if exists {
				w.WriteHeader(http.StatusOK)
				_, _ = io.WriteString(w, "applied")
				return
			}
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	return d
}

func (d *faultDestination) post(key string) error {
	resp, err := http.Post(d.server.URL+"?key="+key, "application/json", http.NoBody)
	if err != nil {
		return err
	}
	return resp.Body.Close()
}

func (d *faultDestination) postCount() int32 { return atomic.LoadInt32(&d.posts) }

// faultHTTPProber probes the destination's GET surface to decide whether an uncertain effect landed — the
// tool's own read surface, not a generic prober framework (spec §26.7, fork 5).
type faultHTTPProber struct{ dest *faultDestination }

func (p faultHTTPProber) Probe(_ context.Context, call coordinator.UncertainToolCall) (bool, []byte, bool, error) {
	resp, err := http.Get(p.dest.server.URL + "?key=" + call.ExternalKey)
	if err != nil {
		return false, nil, false, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusOK {
		return true, []byte(fmt.Sprintf(`{"reconciled":true,"key":%q}`, call.ExternalKey)), true, nil
	}
	return false, nil, true, nil
}

// TestFaultReversibleReconcileNoDuplicateExternalEffect proves the exit-gate core
// (duplicate-external-effect = 0) for a reversible tool killed between execute and record: the effect
// fired once, the reconcile loop probes the REAL destination, sees it landed, and reconciles to completed
// WITHOUT re-firing — the request counter stays at 1.
func TestFaultReversibleReconcileNoDuplicateExternalEffect(t *testing.T) {
	ctx := context.Background()
	cs := openFaultStore(t)
	tenant, sessionID, runID := seedFaultRun(t, cs)
	dest := newFaultDestination()
	defer dest.server.Close()

	callID, key := faultID("tc"), "obj-rev-1"
	// Execute: the durable 'executing' pre-write lands, the effect fires — then the process is KILLED
	// before CommitToolResult (no commit). The row is stuck 'executing'.
	if err := cs.BeginToolCall(ctx, tenant, sessionID, "", runID, 1, callID, "http.post", []byte(`{}`), "reversible", "sha256:x", key, ""); err != nil {
		t.Fatalf("BeginToolCall error = %v", err)
	}
	if err := dest.post(key); err != nil {
		t.Fatalf("destination post error = %v", err)
	}
	// --- kill here: no CommitToolResult ---

	// A fresh attempt reclaims: it finds the 'executing' reversible row and marks it uncertain (STOP).
	if _, err := cs.MarkToolCallUncertain(ctx, tenant, sessionID, "", runID, callID); err != nil {
		t.Fatalf("MarkToolCallUncertain error = %v", err)
	}

	// The reconcile loop probes the real destination — the effect landed → reconciled_completed, no re-fire.
	rec := NewUncertainReconciler(cs, faultHTTPProber{dest: dest}, time.Second, 10)
	if n, err := rec.Sweep(ctx); err != nil || n != 1 {
		t.Fatalf("reconcile Sweep() = (%d, %v), want (1, nil)", n, err)
	}
	if got := dest.postCount(); got != 1 {
		t.Fatalf("external effect fired %d times, want 1 (DUPLICATE EXTERNAL EFFECT = 0)", got)
	}
	var state, recon, result string
	if err := cs.Pool().QueryRow(storage.WithSystemScope(ctx), `SELECT state, reconciliation_state, coalesce(result::text,'') FROM tool_calls WHERE id=$1`, callID).
		Scan(&state, &recon, &result); err != nil {
		t.Fatalf("read reconciled row error = %v", err)
	}
	if state != "reconciled_completed" {
		t.Fatalf("row state = %q, want reconciled_completed (labeled result)", state)
	}
	if result == "" {
		t.Fatal("reconciled_completed row carries no probed result")
	}
}

// TestFaultIrreversibleReconcileStopsUncertain proves the irreversible variant: a kill between execute
// and record leaves the row uncertain, and the reconcile loop NEVER auto-replays an irreversible effect —
// it escalates to manual_resolution (a human decides) and does not re-fire the destination.
func TestFaultIrreversibleReconcileStopsUncertain(t *testing.T) {
	ctx := context.Background()
	cs := openFaultStore(t)
	tenant, sessionID, runID := seedFaultRun(t, cs)
	dest := newFaultDestination()
	defer dest.server.Close()

	callID, key := faultID("tc"), "obj-irr-1"
	if err := cs.BeginToolCall(ctx, tenant, sessionID, "", runID, 1, callID, "charge", []byte(`{}`), "irreversible", "sha256:x", key, ""); err != nil {
		t.Fatalf("BeginToolCall error = %v", err)
	}
	if err := dest.post(key); err != nil {
		t.Fatalf("destination post error = %v", err)
	}
	// --- kill: no commit ---
	if _, err := cs.MarkToolCallUncertain(ctx, tenant, sessionID, "", runID, callID); err != nil {
		t.Fatalf("MarkToolCallUncertain error = %v", err)
	}

	// A prober that would claim "applied" — it must NOT be consulted for an irreversible effect.
	rec := NewUncertainReconciler(cs, faultHTTPProber{dest: dest}, time.Second, 10)
	if _, err := rec.Sweep(ctx); err != nil {
		t.Fatalf("reconcile Sweep() error = %v", err)
	}
	var state string
	if err := cs.Pool().QueryRow(storage.WithSystemScope(ctx), `SELECT state FROM tool_calls WHERE id=$1`, callID).Scan(&state); err != nil {
		t.Fatalf("read row error = %v", err)
	}
	if state != "manual_resolution" {
		t.Fatalf("irreversible uncertain resolved to %q, want manual_resolution (never auto-replays)", state)
	}
	if got := dest.postCount(); got != 1 {
		t.Fatalf("irreversible effect fired %d times, want 1 (no re-fire during reconcile)", got)
	}
}
