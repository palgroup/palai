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

	purged, _, err := cs.PurgeExpiredStoreFalse(ctx, time.Minute)
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

	if _, _, err := cs.PurgeExpiredStoreFalse(ctx, time.Minute); err != nil {
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

// seedEvent appends one journal event to a session, keyed to a response, at an explicit
// sequence. The purge scrub is per-response (spec §22.2), so response_id decides which
// events a purge reaches.
func seedEvent(t *testing.T, pool *pgxpool.Pool, tenant coordinator.Tenant, sessionID, responseID string, seq int, payload string) {
	t.Helper()
	exec(t, pool,
		`INSERT INTO events (id, organization_id, project_id, session_id, response_id, seq, type, payload)
		 VALUES ($1, $2, $3, $4, $5, $6, 'output.item.v1', $7)`,
		newID("evt"), tenant.Organization, tenant.Project, sessionID, responseID, seq, []byte(payload))
}

func eventPayload(t *testing.T, pool *pgxpool.Pool, responseID string, seq int) string {
	t.Helper()
	var payload string
	if err := pool.QueryRow(context.Background(),
		`SELECT payload::text FROM events WHERE response_id = $1 AND seq = $2`, responseID, seq).Scan(&payload); err != nil {
		t.Fatalf("read event payload response=%s seq=%d error = %v", responseID, seq, err)
	}
	return payload
}

// TestStoreFalsePurgeLeavesSiblingResponseEvents proves the store:false purge is scoped to
// the victim response, not its whole session: a retained sibling response sharing the same
// session keeps its journal intact while the victim's events are scrubbed. This is the
// closure of the 000002 session-level scrub ceiling (storage/queries/responses.sql) — under
// a session-wide scrub the sibling's events would be wrongly reaped too (spec §22.2).
func TestStoreFalsePurgeLeavesSiblingResponseEvents(t *testing.T) {
	cs := openHarness(t)
	ctx := context.Background()
	pool := cs.Pool()

	tenant, sessionID, _ := seedRun(t, pool)

	// Two responses in ONE session: an expired store:false victim and a retained sibling.
	victim := seedTerminalResponse(t, pool, tenant, sessionID, false, time.Hour)
	sibling := seedTerminalResponse(t, pool, tenant, sessionID, true, time.Hour)

	// One contiguous per-session journal, but events are keyed per response.
	seedEvent(t, pool, tenant, sessionID, victim, 1, `{"content":"victim secret"}`)
	seedEvent(t, pool, tenant, sessionID, victim, 2, `{"content":"victim secret 2"}`)
	seedEvent(t, pool, tenant, sessionID, sibling, 3, `{"content":"sibling content"}`)
	seedEvent(t, pool, tenant, sessionID, sibling, 4, `{"content":"sibling content 2"}`)

	purged, _, err := cs.PurgeExpiredStoreFalse(ctx, time.Minute)
	if err != nil {
		t.Fatalf("PurgeExpiredStoreFalse() error = %v", err)
	}
	if purged != 1 {
		t.Fatalf("purged = %d, want 1 (only the store:false victim)", purged)
	}

	// The victim's events are scrubbed.
	for _, seq := range []int{1, 2} {
		if got := eventPayload(t, pool, victim, seq); got != `{"purged": true}` {
			t.Fatalf("victim event seq %d payload = %s, want scrubbed", seq, got)
		}
	}
	// The sibling's events are INTACT — the purge must not cross the response boundary.
	// (jsonb renders a space after the colon, matching the {"purged": true} scrub literal.)
	if got := eventPayload(t, pool, sibling, 3); got != `{"content": "sibling content"}` {
		t.Fatalf("sibling event seq 3 payload = %s, want unchanged (a session-wide scrub would reap this retained sibling)", got)
	}
	if got := eventPayload(t, pool, sibling, 4); got != `{"content": "sibling content 2"}` {
		t.Fatalf("sibling event seq 4 payload = %s, want unchanged", got)
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
	respID := seedTerminalResponse(t, pool, tenant, sessionID, false, time.Hour)

	// A content-bearing journal for the transient response: three contiguous events keyed
	// to the response the purge reaps.
	for seq := 1; seq <= 3; seq++ {
		seedEvent(t, pool, tenant, sessionID, respID, seq, `{"content":"secret message body"}`)
	}

	if _, _, err := cs.PurgeExpiredStoreFalse(ctx, time.Minute); err != nil {
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
