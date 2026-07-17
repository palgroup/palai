//go:build component

package postgres

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/palgroup/palai/packages/coordinator"
)

// seedTerminalResponse inserts a session-bound response in a terminal state with an
// explicit store flag and updated_at age, so a purge test controls exactly which
// rows the TTL gate and the store filter should reach. It returns the response id.
func seedTerminalResponse(t *testing.T, pool *pgxpool.Pool, tenant coordinator.Tenant, sessionID string, store bool, age time.Duration) string {
	t.Helper()
	respID := newID("resp")
	exec(t, pool,
		`INSERT INTO responses (id, organization_id, project_id, session_id, state, input, output, store, updated_at)
		 VALUES ($1, $2, $3, $4, 'completed', $5, $6, $7, clock_timestamp() - $8::bigint * interval '1 millisecond')`,
		respID, tenant.Organization, tenant.Project, sessionID,
		[]byte(`{"prompt":"secret input"}`),
		[]byte(`{"output":[{"type":"message","content":"secret output"}],"usage":{"total_tokens":9}}`),
		store, age.Milliseconds())
	return respID
}

// responseContent reads a response's tombstone-relevant columns.
func responseContent(t *testing.T, pool *pgxpool.Pool, id string) (output *string, purgedAt *time.Time) {
	t.Helper()
	if err := pool.QueryRow(context.Background(),
		`SELECT output::text, purged_at FROM responses WHERE id = $1`, id).Scan(&output, &purgedAt); err != nil {
		t.Fatalf("read response %s error = %v", id, err)
	}
	return output, purgedAt
}

// isPurged reports whether a response has been tombstoned: purged_at set and its
// customer output cleared.
func isPurged(t *testing.T, pool *pgxpool.Pool, id string) bool {
	t.Helper()
	output, purgedAt := responseContent(t, pool, id)
	return purgedAt != nil && output == nil
}

// TestReaperPurgesOnlyExpiredStoreFalseResponses proves the sweep selects only
// store=false, terminal responses whose retention TTL has elapsed, and leaves
// store=true and too-new rows untouched (spec §20.9, §8.3).
func TestReaperPurgesOnlyExpiredStoreFalseResponses(t *testing.T) {
	cs := openHarness(t)
	ctx := context.Background()
	pool := cs.Pool()

	tenant, sessionID, _ := seedRun(t, pool)

	// Expired, store=false, terminal — the only row the sweep should purge.
	expired := seedTerminalResponse(t, pool, tenant, sessionID, false, time.Hour)
	// Fresh store=false: content is transient but the TTL has not elapsed yet.
	fresh := seedTerminalResponse(t, pool, tenant, sessionID, false, 0)
	// Retained (store=true): never purged regardless of age.
	retained := seedTerminalResponse(t, pool, tenant, sessionID, true, time.Hour)

	purged, err := cs.PurgeExpiredStoreFalse(ctx, time.Minute)
	if err != nil {
		t.Fatalf("PurgeExpiredStoreFalse() error = %v", err)
	}
	if purged != 1 {
		t.Fatalf("purged = %d, want 1 (only the expired store=false response)", purged)
	}

	if !isPurged(t, pool, expired) {
		t.Fatalf("expired store=false response %s was not purged", expired)
	}
	if isPurged(t, pool, fresh) {
		t.Fatalf("fresh store=false response %s was purged before its TTL elapsed", fresh)
	}
	if isPurged(t, pool, retained) {
		t.Fatalf("retained store=true response %s was purged", retained)
	}
}

// TestPurgeKeepsTombstoneRequestHashAndFingerprint proves the idempotency record of
// a purged response keeps only its request hash plus the §20.9 tombstone fields —
// resource tombstone, outcome fingerprint, and purge time — and drops the cached
// response body.
func TestPurgeKeepsTombstoneRequestHashAndFingerprint(t *testing.T) {
	cs := openHarness(t)
	ctx := context.Background()
	pool := cs.Pool()

	tenant, sessionID, _ := seedRun(t, pool)
	principal := newID("prin")
	exec(t, pool, `INSERT INTO principals (id, organization_id, project_id, kind) VALUES ($1, $2, $3, 'api_key')`,
		principal, tenant.Organization, tenant.Project)

	respID := seedTerminalResponse(t, pool, tenant, sessionID, false, time.Hour)
	const requestHash = "req-hash-abc123"
	exec(t, pool,
		`INSERT INTO idempotency_records
		   (organization_id, project_id, principal_id, method, route, idempotency_key, request_hash, status, response_body)
		 VALUES ($1, $2, $3, 'POST', '/v1/responses', $4, $5, 'completed', $6)`,
		tenant.Organization, tenant.Project, principal, newID("idem"), requestHash,
		[]byte(`{"id":"`+respID+`","status":"completed","output":[{"type":"message","content":"secret output"}]}`))

	if _, err := cs.PurgeExpiredStoreFalse(ctx, time.Minute); err != nil {
		t.Fatalf("PurgeExpiredStoreFalse() error = %v", err)
	}

	var (
		gotHash        string
		responseBody   *string
		resourceTomb   *string
		fingerprint    *string
		resultPurgedAt *time.Time
	)
	if err := pool.QueryRow(ctx,
		`SELECT request_hash, response_body::text, resource_tombstone, outcome_fingerprint, result_purged_at
		 FROM idempotency_records WHERE project_id = $1 AND resource_tombstone = $2`,
		tenant.Project, respID).Scan(&gotHash, &responseBody, &resourceTomb, &fingerprint, &resultPurgedAt); err != nil {
		t.Fatalf("read tombstoned idempotency record error = %v", err)
	}

	if gotHash != requestHash {
		t.Fatalf("request_hash = %q, want preserved %q", gotHash, requestHash)
	}
	if responseBody != nil {
		t.Fatalf("response_body = %q, want NULL (cached body must not survive purge)", *responseBody)
	}
	if resourceTomb == nil || *resourceTomb != respID {
		t.Fatalf("resource_tombstone = %v, want %q", resourceTomb, respID)
	}
	if fingerprint == nil || *fingerprint == "" {
		t.Fatalf("outcome_fingerprint = %v, want a non-empty digest", fingerprint)
	}
	if resultPurgedAt == nil {
		t.Fatalf("result_purged_at is NULL, want the purge time")
	}
}

// TestPurgeReplacesEventPayloadsButKeepsSequence proves the sweep scrubs the content
// out of a purged response's journal events yet preserves every row and the
// contiguous per-session sequence (spec §20.9, §21.1).
func TestPurgeReplacesEventPayloadsButKeepsSequence(t *testing.T) {
	cs := openHarness(t)
	ctx := context.Background()
	pool := cs.Pool()

	tenant, sessionID, _ := seedRun(t, pool)
	_ = seedTerminalResponse(t, pool, tenant, sessionID, false, time.Hour)

	// A content-bearing journal for the transient session: three contiguous events.
	for seq := 1; seq <= 3; seq++ {
		exec(t, pool,
			`INSERT INTO events (id, organization_id, project_id, session_id, seq, type, payload)
			 VALUES ($1, $2, $3, $4, $5, 'output.item.v1', $6)`,
			newID("evt"), tenant.Organization, tenant.Project, sessionID, seq,
			[]byte(`{"content":"secret message body"}`))
	}

	if _, err := cs.PurgeExpiredStoreFalse(ctx, time.Minute); err != nil {
		t.Fatalf("PurgeExpiredStoreFalse() error = %v", err)
	}

	rows, err := pool.Query(ctx,
		`SELECT seq, payload::text FROM events WHERE session_id = $1 ORDER BY seq`, sessionID)
	if err != nil {
		t.Fatalf("read events error = %v", err)
	}
	defer rows.Close()
	var seqs []int
	for rows.Next() {
		var seq int
		var payload string
		if err := rows.Scan(&seq, &payload); err != nil {
			t.Fatalf("scan event error = %v", err)
		}
		if payload != `{"purged": true}` {
			t.Fatalf("event %d payload = %s, want {\"purged\": true}", seq, payload)
		}
		seqs = append(seqs, seq)
	}
	if len(seqs) != 3 {
		t.Fatalf("event rows = %d, want 3 preserved", len(seqs))
	}
	for i, seq := range seqs {
		if seq != i+1 {
			t.Fatalf("event sequence = %v, want contiguous 1..3", seqs)
		}
	}
}
