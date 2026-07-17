//go:build component

package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/palgroup/palai/packages/coordinator"
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
	if err := cs.Pool().QueryRow(ctx, `SELECT key_hash FROM api_keys WHERE principal_id = $1`, principalID).Scan(&stored); err != nil {
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
	if err := pool.QueryRow(context.Background(), sql, args...).Scan(&got); err != nil {
		t.Fatalf("count query error = %v", err)
	}
	if got != want {
		t.Fatalf("count = %d, want %d (%s)", got, want, sql)
	}
}
