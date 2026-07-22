//go:build component

package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/palgroup/palai/packages/coordinator"

	"github.com/palgroup/palai/storage"
)

// seedTenantWithKey creates org -> project -> principal -> api_key and returns the
// tenant and principal. The stored verifier is the hash of token, never token.
func seedTenantWithKey(t *testing.T, pool *pgxpool.Pool, token string) (coordinator.Tenant, string) {
	t.Helper()
	tenant := coordinator.Tenant{Organization: newID("org"), Project: newID("prj")}
	principalID := newID("prin")
	exec(t, pool, `INSERT INTO organizations (id) VALUES ($1)`, tenant.Organization)
	exec(t, pool, `INSERT INTO projects (id, organization_id) VALUES ($1, $2)`, tenant.Project, tenant.Organization)
	exec(t, pool, `INSERT INTO principals (id, organization_id, project_id, kind) VALUES ($1, $2, $3, 'service')`,
		principalID, tenant.Organization, tenant.Project)
	exec(t, pool, `INSERT INTO api_keys (id, organization_id, project_id, principal_id, key_hash) VALUES ($1, $2, $3, $4, $5)`,
		newID("key"), tenant.Organization, tenant.Project, principalID, coordinator.HashAPIKey(token))
	return tenant, principalID
}

func admissionInput(principal, key, hash, body string) coordinator.AdmissionInput {
	return coordinator.AdmissionInput{
		Principal:      principal,
		IdempotencyKey: key,
		Method:         "POST",
		Route:          "/v1/responses",
		RequestHash:    hash,
		ResponseID:     newID("resp"),
		RunID:          newID("run"),
		SessionID:      newID("ses"),
		Input:          []byte(`"do the work"`),
		Body:           []byte(body),
	}
}

func TestVerifyAPIKeyResolvesScopeByHash(t *testing.T) {
	cs := openHarness(t)
	ctx := context.Background()
	tenant, principalID := seedTenantWithKey(t, cs.Pool(), "s3cr3t-token")

	id, err := cs.VerifyAPIKey(ctx, "s3cr3t-token")
	if err != nil {
		t.Fatalf("VerifyAPIKey() error = %v", err)
	}
	if id.Organization != tenant.Organization || id.Project != tenant.Project || id.Principal != principalID {
		t.Fatalf("identity = %+v, want %s/%s/%s", id, tenant.Organization, tenant.Project, principalID)
	}

	// An unknown key never resolves a tenant.
	if _, err := cs.VerifyAPIKey(ctx, "wrong-token"); !errors.Is(err, coordinator.ErrInvalidToken) {
		t.Fatalf("VerifyAPIKey(wrong) error = %v, want ErrInvalidToken", err)
	}

	// The raw key is never persisted; only its hash is.
	var stored string
	if err := cs.Pool().QueryRow(storage.WithSystemScope(ctx), `SELECT key_hash FROM api_keys WHERE principal_id = $1`, principalID).Scan(&stored); err != nil {
		t.Fatalf("read key_hash error = %v", err)
	}
	if stored == "s3cr3t-token" {
		t.Fatalf("api_keys.key_hash stored the raw key")
	}
}

func TestAdmitResponseIsIdempotentAndAtomic(t *testing.T) {
	cs := openHarness(t)
	ctx := context.Background()
	tenant, principalID := seedTenantWithKey(t, cs.Pool(), "tok")

	const body = `{"id":"resp_fixed","object":"response","status":"queued"}`
	created, err := cs.AdmitResponse(ctx, tenant, admissionInput(principalID, "key-1", "hash-A", body))
	if err != nil {
		t.Fatalf("AdmitResponse(create) error = %v", err)
	}
	if created.Replayed || created.Conflict {
		t.Fatalf("first admission = %+v, want a fresh create", created)
	}

	// A duplicate (same key, same request hash) replays the stored resource and
	// creates no second response, run, session, event, or record.
	replay, err := cs.AdmitResponse(ctx, tenant, admissionInput(principalID, "key-1", "hash-A", `{"id":"resp_ignored"}`))
	if err != nil {
		t.Fatalf("AdmitResponse(replay) error = %v", err)
	}
	if !replay.Replayed {
		t.Fatalf("duplicate admission = %+v, want replayed", replay)
	}
	if replayedID := decodeID(t, replay.Body); replayedID != "resp_fixed" {
		t.Fatalf("replay body id = %q, want the original resp_fixed", replayedID)
	}

	assertCount(t, cs.Pool(), 1, `SELECT count(*) FROM responses WHERE organization_id=$1 AND project_id=$2`, tenant.Organization, tenant.Project)
	assertCount(t, cs.Pool(), 1, `SELECT count(*) FROM runs WHERE organization_id=$1 AND project_id=$2`, tenant.Organization, tenant.Project)
	assertCount(t, cs.Pool(), 1, `SELECT count(*) FROM sessions WHERE organization_id=$1 AND project_id=$2`, tenant.Organization, tenant.Project)
	assertCount(t, cs.Pool(), 1, `SELECT count(*) FROM events WHERE type='run.queued.v1' AND organization_id=$1 AND project_id=$2`, tenant.Organization, tenant.Project)
	assertCount(t, cs.Pool(), 1, `SELECT count(*) FROM outbox WHERE topic='run.queued.v1' AND project_id=$1`, tenant.Project)
	assertCount(t, cs.Pool(), 1, `SELECT count(*) FROM idempotency_records WHERE idempotency_key='key-1' AND project_id=$1`, tenant.Project)
	// Admission enqueues exactly one response.run dispatch job, whose payload run_id
	// resolves to the admitted run; the replay must not enqueue a second.
	assertCount(t, cs.Pool(), 1,
		`SELECT count(*) FROM durable_jobs j JOIN runs r ON r.id = j.payload->>'run_id'
		 WHERE j.kind='response.run' AND j.project_id=$1`, tenant.Project)

	// The same key with a different request hash is a conflict; the original is
	// untouched and no second response appears.
	conflict, err := cs.AdmitResponse(ctx, tenant, admissionInput(principalID, "key-1", "hash-B", body))
	if err != nil {
		t.Fatalf("AdmitResponse(conflict) error = %v", err)
	}
	if !conflict.Conflict {
		t.Fatalf("divergent reuse = %+v, want conflict", conflict)
	}
	assertCount(t, cs.Pool(), 1, `SELECT count(*) FROM responses WHERE organization_id=$1 AND project_id=$2`, tenant.Organization, tenant.Project)
	assertCount(t, cs.Pool(), 1, `SELECT count(*) FROM durable_jobs WHERE kind='response.run' AND project_id=$1`, tenant.Project)
}

// TestAdmitResponseEnforcesPerProjectRunCaps proves the §20.12 admission caps: with a queued-run
// bound set, the admission whose acceptance would exceed the project's queued backlog is rejected
// (QueueDepthExceeded) and leaves NO durable trace (no run, no idempotency record — the whole tx
// rolls back), so the client may retry the same key once capacity frees. With a concurrent-run cap
// set, an admission is rejected (ConcurrencyLimited) once the project already holds that many
// simultaneously-executing root runs. A run that has reached a terminal state frees its slot.
func TestAdmitResponseEnforcesPerProjectRunCaps(t *testing.T) {
	cs := openHarness(t)
	ctx := context.Background()
	tenant, principalID := seedTenantWithKey(t, cs.Pool(), "cap-tok")

	admit := func(key string, in coordinator.AdmissionInput) coordinator.Admission {
		t.Helper()
		out, err := cs.AdmitResponse(ctx, tenant, in)
		if err != nil {
			t.Fatalf("AdmitResponse(%s) error = %v", key, err)
		}
		return out
	}

	// Fill the project to two queued root runs (each a fresh session → a fresh root run).
	for i, key := range []string{"q-1", "q-2"} {
		in := admissionInput(principalID, key, "h-"+key, `{"id":"resp"}`)
		in.MaxConcurrentRuns = 10
		in.MaxQueuedRuns = 5
		if out := admit(key, in); out.QueueDepthExceeded || out.ConcurrencyLimited {
			t.Fatalf("admission %d rejected under a slack cap: %+v", i, out)
		}
	}
	assertCount(t, cs.Pool(), 2, `SELECT count(*) FROM runs WHERE state='queued' AND organization_id=$1 AND project_id=$2`, tenant.Organization, tenant.Project)

	// A third admission under a queued bound of 2 is rejected — the backlog is full.
	over := admissionInput(principalID, "q-3", "h-q-3", `{"id":"resp"}`)
	over.MaxConcurrentRuns = 10
	over.MaxQueuedRuns = 2
	if out := admit("q-3", over); !out.QueueDepthExceeded {
		t.Fatalf("admission over the queued bound = %+v, want QueueDepthExceeded", out)
	}
	// The rejected admission left nothing behind: still exactly two runs and no q-3 idempotency record.
	assertCount(t, cs.Pool(), 2, `SELECT count(*) FROM runs WHERE organization_id=$1 AND project_id=$2`, tenant.Organization, tenant.Project)
	assertCount(t, cs.Pool(), 0, `SELECT count(*) FROM idempotency_records WHERE idempotency_key='q-3' AND project_id=$1`, tenant.Project)

	// Flip both queued runs to running: the queued backlog is now empty but concurrency is at 2.
	exec(t, cs.Pool(), `UPDATE runs SET state='running' WHERE organization_id=$1 AND project_id=$2`, tenant.Organization, tenant.Project)

	// Under a concurrent-run cap of 2, a new admission is rejected — the executing slots are full —
	// even though the queue is empty.
	conc := admissionInput(principalID, "c-1", "h-c-1", `{"id":"resp"}`)
	conc.MaxConcurrentRuns = 2
	conc.MaxQueuedRuns = 5
	if out := admit("c-1", conc); !out.ConcurrencyLimited {
		t.Fatalf("admission over the concurrent cap = %+v, want ConcurrencyLimited", out)
	}

	// Terminating one running run frees a slot, so the same admission now succeeds.
	exec(t, cs.Pool(), `UPDATE runs SET state='completed' WHERE organization_id=$1 AND project_id=$2 AND state='running' AND id=(SELECT id FROM runs WHERE organization_id=$1 AND project_id=$2 AND state='running' LIMIT 1)`, tenant.Organization, tenant.Project)
	if out := admit("c-1", conc); out.ConcurrencyLimited || out.QueueDepthExceeded {
		t.Fatalf("admission after a slot freed = %+v, want accepted", out)
	}
}

func decodeID(t *testing.T, body []byte) string {
	t.Helper()
	var envelope struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(body, &envelope); err != nil {
		t.Fatalf("decode replay body error = %v", err)
	}
	return envelope.ID
}

func assertCount(t *testing.T, pool *pgxpool.Pool, want int, sql string, args ...any) {
	t.Helper()
	var got int
	if err := pool.QueryRow(storage.WithSystemScope(context.Background()), sql, args...).Scan(&got); err != nil {
		t.Fatalf("count query error = %v", err)
	}
	if got != want {
		t.Fatalf("count = %d, want %d (%s)", got, want, sql)
	}
}
