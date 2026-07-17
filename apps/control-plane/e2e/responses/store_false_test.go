//go:build e2e

package responses

import (
	"context"
	"encoding/json"
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	"github.com/palgroup/palai/apps/control-plane/internal/execution"
	"github.com/palgroup/palai/packages/contracts"
)

// reapUntilPurged sweeps with the reaper until the response is tombstoned or the
// deadline elapses. It drives on committed DB state (purged_at) rather than a fixed
// wall-clock sleep, so the proof is deterministic once the short TTL has elapsed.
func reapUntilPurged(t *testing.T, h *harness, reaper *execution.Reaper, responseID string, within time.Duration) {
	t.Helper()
	deadline := time.Now().Add(within)
	for time.Now().Before(deadline) {
		if _, err := reaper.Sweep(context.Background()); err != nil {
			t.Fatalf("reaper Sweep error = %v", err)
		}
		if h.purgedAt(responseID) != nil {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("response %s was not purged within %s", responseID, within)
}

// eventPayloads reads the raw JSON payloads of a session's journal in sequence order.
func (h *harness) eventPayloads(sessionID string) []string {
	h.t.Helper()
	rows, err := h.spine.Pool().Query(context.Background(),
		`SELECT payload::text FROM events WHERE session_id=$1 AND organization_id=$2 AND project_id=$3 ORDER BY seq`,
		sessionID, h.tenant.Organization, h.tenant.Project)
	if err != nil {
		h.t.Fatalf("read event payloads error = %v", err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var payload string
		if err := rows.Scan(&payload); err != nil {
			h.t.Fatalf("scan event payload error = %v", err)
		}
		out = append(out, payload)
	}
	return out
}

// TestStoreFalseContentIsGoneAfterConfiguredTTL drives a store:false response to a
// committed terminal, runs the reaper past its short TTL, and proves every trace of
// customer content — response output/input, journal event payloads, and produced
// artifact bytes — is gone while the tombstone row remains (spec §8.3, §20.9).
func TestStoreFalseContentIsGoneAfterConfiguredTTL(t *testing.T) {
	h := newHarness(t)
	stop := h.runWorker(h.newOrchestrator(subprocessDialer{engineDir: h.engineDir}))
	defer stop()

	responseID, sessionID, runID := h.admitWith(`{"input":"do the work","store":false}`, newID("idem"))
	h.awaitResponseState(responseID, "completed", 60*time.Second)

	// An artifact bound to the run stands in for produced bytes the purge must delete.
	artifactID := newID("art")
	if _, err := h.spine.Pool().Exec(context.Background(),
		`INSERT INTO artifacts (id, organization_id, project_id, run_id, object_key, size_bytes, checksum)
		 VALUES ($1, $2, $3, $4, 'blob/output.bin', 4096, 'sha256:deadbeef')`,
		artifactID, h.tenant.Organization, h.tenant.Project, runID); err != nil {
		t.Fatalf("seed artifact error = %v", err)
	}

	reaper := execution.NewReaper(h.repo, 20*time.Millisecond)
	reapUntilPurged(t, h, reaper, responseID, 10*time.Second)

	// The response output is cleared; the row itself survives as a tombstone.
	_, projection := h.response(responseID)
	if len(projection.Output) != 0 {
		t.Fatalf("response output survived purge: %+v", projection.Output)
	}
	// input is NOT NULL, so purge scrubs its content to an empty object.
	var input string
	if err := h.spine.Pool().QueryRow(context.Background(),
		`SELECT input::text FROM responses WHERE id=$1`, responseID).Scan(&input); err != nil {
		t.Fatalf("read response input error = %v", err)
	}
	if input != "{}" {
		t.Fatalf("response input survived purge: %q, want scrubbed to {}", input)
	}

	// Every journal payload is scrubbed, and at least one event existed to scrub.
	payloads := h.eventPayloads(sessionID)
	if len(payloads) == 0 {
		t.Fatalf("no journal events found for the completed session")
	}
	for i, payload := range payloads {
		if payload != `{"purged": true}` {
			t.Fatalf("event %d payload = %s, want scrubbed", i, payload)
		}
	}

	// The artifact bytes are deleted.
	var size int64
	var key string
	if err := h.spine.Pool().QueryRow(context.Background(),
		`SELECT size_bytes, object_key FROM artifacts WHERE id=$1`, artifactID).Scan(&size, &key); err != nil {
		t.Fatalf("read artifact error = %v", err)
	}
	if size != 0 || key != "" {
		t.Fatalf("artifact bytes survived purge: size=%d key=%q", size, key)
	}
}

// TestDuplicateCreateAfterPurgeReturns410WithoutReexecution proves a replay of the
// original request after purge is answered 410 idempotency_result_expired with the
// original operation identity, and never re-runs the model (spec §20.9).
func TestDuplicateCreateAfterPurgeReturns410WithoutReexecution(t *testing.T) {
	h := newHarness(t)
	stop := h.runWorker(h.newOrchestrator(subprocessDialer{engineDir: h.engineDir}))
	defer stop()

	idemKey := newID("idem")
	const body = `{"input":"do the work","store":false}`
	responseID, _, _ := h.admitWith(body, idemKey)
	h.awaitResponseState(responseID, "completed", 60*time.Second)
	callsAfterRun := atomic.LoadInt32(&h.provider.calls)

	reaper := execution.NewReaper(h.repo, 20*time.Millisecond)
	reapUntilPurged(t, h, reaper, responseID, 10*time.Second)

	// Same key + same body: the tombstone answers 410 without a second execution.
	resp := h.postResponse(body, idemKey, h.token)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusGone {
		t.Fatalf("replay status = %d, want 410", resp.StatusCode)
	}
	var problem contracts.Problem
	if err := json.NewDecoder(resp.Body).Decode(&problem); err != nil {
		t.Fatalf("decode problem error = %v", err)
	}
	if problem.Code != "idempotency_result_expired" {
		t.Fatalf("code = %q, want idempotency_result_expired", problem.Code)
	}
	// Original operation identity: the tombstone points at the original resource.
	if loc := resp.Header.Get("Location"); loc != "/v1/responses/"+responseID {
		t.Fatalf("Location = %q, want the original resource /v1/responses/%s", loc, responseID)
	}

	// No re-execution: the model dispatch counter does not move, and no new run exists.
	if calls := atomic.LoadInt32(&h.provider.calls); calls != callsAfterRun {
		t.Fatalf("model provider calls = %d after replay, want unchanged %d", calls, callsAfterRun)
	}
	if n := h.count(`SELECT count(*) FROM runs WHERE organization_id=$1 AND project_id=$2`, h.tenant.Organization, h.tenant.Project); n != 1 {
		t.Fatalf("run count = %d after replay, want 1 (no new run dispatched)", n)
	}
}

// TestRetrieveAfterPurgeReturns410RetentionExpired proves retrieval of a purged
// response is answered 410 retention_expired over the GET endpoint (spec §8.3).
func TestRetrieveAfterPurgeReturns410RetentionExpired(t *testing.T) {
	h := newHarness(t)
	stop := h.runWorker(h.newOrchestrator(subprocessDialer{engineDir: h.engineDir}))
	defer stop()

	responseID, _, _ := h.admitWith(`{"input":"do the work","store":false}`, newID("idem"))
	h.awaitResponseState(responseID, "completed", 60*time.Second)

	reaper := execution.NewReaper(h.repo, 20*time.Millisecond)
	reapUntilPurged(t, h, reaper, responseID, 10*time.Second)

	resp := h.getResponse(responseID, h.token)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusGone {
		t.Fatalf("retrieve status = %d, want 410", resp.StatusCode)
	}
	var problem contracts.Problem
	if err := json.NewDecoder(resp.Body).Decode(&problem); err != nil {
		t.Fatalf("decode problem error = %v", err)
	}
	if problem.Code != "retention_expired" {
		t.Fatalf("code = %q, want retention_expired", problem.Code)
	}
}

// TestRetainedResponseSurvivesReaper proves the reaper never touches a retained
// (store:true, the default) response: even under an aggressively short TTL it is
// spared, and it retrieves 200 with its content intact (spec §8.3).
func TestRetainedResponseSurvivesReaper(t *testing.T) {
	h := newHarness(t)
	stop := h.runWorker(h.newOrchestrator(subprocessDialer{engineDir: h.engineDir}))
	defer stop()

	// Default store (no store field) is retained.
	responseID, _, _ := h.admit()
	h.awaitResponseState(responseID, "completed", 60*time.Second)

	reaper := execution.NewReaper(h.repo, time.Nanosecond)
	if purged, err := reaper.Sweep(context.Background()); err != nil || purged != 0 {
		t.Fatalf("reaper Sweep purged = %d err = %v, want 0 (retained response is spared)", purged, err)
	}
	if h.purgedAt(responseID) != nil {
		t.Fatalf("retained response %s was purged", responseID)
	}

	resp := h.getResponse(responseID, h.token)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("retrieve status = %d, want 200", resp.StatusCode)
	}
	var got contracts.Response
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatalf("decode response error = %v", err)
	}
	if got.Status != "completed" {
		t.Fatalf("retained status = %q, want completed", got.Status)
	}
	if len(got.Output) == 0 {
		t.Fatalf("retained response lost its output")
	}
}

// TestRetrieveForeignTenantReturns404 proves retrieval is tenant-scoped: a second
// tenant's key cannot reach the first tenant's response, which reads as 404 rather
// than leaking its existence (spec §39.2, LP-005 scope immunity).
func TestRetrieveForeignTenantReturns404(t *testing.T) {
	h := newHarness(t)
	stop := h.runWorker(h.newOrchestrator(subprocessDialer{engineDir: h.engineDir}))
	defer stop()

	responseID, _, _ := h.admit()
	h.awaitResponseState(responseID, "completed", 60*time.Second)

	otherToken := newID("e2e-tok")
	seedTenantWithKey(t, h.spine.Pool(), otherToken)

	resp := h.getResponse(responseID, otherToken)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("cross-tenant retrieve status = %d, want 404", resp.StatusCode)
	}
	var problem contracts.Problem
	if err := json.NewDecoder(resp.Body).Decode(&problem); err != nil {
		t.Fatalf("decode problem error = %v", err)
	}
	if problem.Code != "not_found" {
		t.Fatalf("code = %q, want not_found", problem.Code)
	}
}
