//go:build live

// Package live is the E09 Task 2 live artifact-write smoke. It runs only under the `live`
// build tag, in `make test-live-provider PROVIDER=provider-one CASE=artifact-write`, which
// stands up a throwaway Postgres + SeaweedFS and loads the real provider credential from
// .env.local. In ONE flow it proves: a REAL provider-one chat completion produces output,
// that output is persisted as an artifact to a REAL object store, and the recorded row +
// bytes + checksum match — the write-path end to end against real infrastructure.
//
// HONEST CEILING: this is a component-real INFRA smoke. The LIVE element is a real run
// writing real bytes to a real S3, NOT model reasoning — the model does not drive an
// artifact tool (that is E09 Task 4/T5); the smoke stands in the run's terminal output for
// the tool output T4 will later produce. The credential is used only as an opaque needle for
// the leak scan and is never printed.
//
// It lives under apps/control-plane because the object store is internal to the control
// plane (spec §24) and Go forbids importing internal packages from tests/.
package live

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	providerone "github.com/palgroup/palai/adapters/models/provider_one"
	"github.com/palgroup/palai/apps/control-plane/internal/artifacts"
	"github.com/palgroup/palai/apps/control-plane/internal/store"
	"github.com/palgroup/palai/packages/contracts"
	modelbroker "github.com/palgroup/palai/packages/model-broker"
)

const credentialEnv = "OPENAI_API_KEY"

func liveModel() string {
	if m := os.Getenv("PALAI_LIVE_MODEL"); m != "" {
		return m
	}
	return "gpt-4o-mini"
}

// TestLiveArtifactWriteRealProviderRun is CASE=artifact-write: a real provider-one
// completion whose output is persisted as an artifact to a real SeaweedFS + Postgres, then
// read back to prove the row + bytes + checksum agree — the object-store write-path proven
// on real infrastructure, not a mock.
func TestLiveArtifactWriteRealProviderRun(t *testing.T) {
	secret := os.Getenv(credentialEnv)
	if secret == "" {
		t.Fatalf("%s is unset; the live tier loads it from .env.local at runtime", credentialEnv)
	}
	pgURL := os.Getenv("PALAI_COMPONENT_POSTGRES_URL")
	endpoint := os.Getenv("PALAI_S3_ENDPOINT")
	if pgURL == "" || endpoint == "" {
		t.Skip("PALAI_COMPONENT_POSTGRES_URL + PALAI_S3_ENDPOINT are required; run make test-live-provider PROVIDER=provider-one CASE=artifact-write")
	}
	ctx := context.Background()

	// 1. A REAL provider-one completion. Its streamed text stands in for a run's terminal
	// output (the shell/file tool output T4 will later produce).
	broker := modelbroker.New(modelbroker.Config{
		Adapters: map[string]modelbroker.ModelAdapter{"provider-one": providerone.Adapter{}},
		Secrets:  modelbroker.EnvResolver{"provider-one": credentialEnv},
	})
	req := modelbroker.Request{
		ModelRequestID: contracts.ModelRequestID("mreq_live_artifact"),
		RouteRevision:  1,
		ModelStepID:    "step-1",
		Model:          liveModel(),
		Messages: []modelbroker.Message{
			{Role: "user", Content: "Reply with one short line, like a CI terminal reporting a successful build (no code fences)."},
		},
		Deadline:    time.Now().Add(60 * time.Second),
		Reservation: modelbroker.Reservation{MaxTotalTokens: 2000},
		Secret:      modelbroker.SecretRef("provider-one"),
	}
	var streamed bytes.Buffer
	res, err := broker.Route(ctx, "provider-one", req, func(d modelbroker.Delta) {
		streamed.WriteString(d.Text)
	})
	if err != nil {
		t.Fatalf("route: %v", err)
	}
	if res.Error != nil {
		t.Fatalf("provider returned a sanitized error: code=%s status=%d", res.Error.Code, res.Error.Status)
	}
	if !strings.HasPrefix(res.ProviderRequestID, "chatcmpl") {
		t.Fatalf("provider request id %q is not a real chat completion id", res.ProviderRequestID)
	}
	output := streamed.Bytes()
	if len(output) == 0 {
		t.Fatal("the real completion produced no text output to persist as an artifact")
	}

	// 2. Persist the real output as an artifact to the REAL object store + durable row.
	repo, err := store.Open(ctx, pgURL)
	if err != nil {
		t.Fatalf("store.Open() error = %v", err)
	}
	defer repo.Close()
	if err := repo.Migrate(ctx); err != nil {
		t.Fatalf("Migrate() error = %v", err)
	}
	s3, err := artifacts.NewStore(artifacts.Config{
		Endpoint:  endpoint,
		Bucket:    envOr("PALAI_S3_BUCKET", "palai-artifacts-live"),
		Region:    os.Getenv("PALAI_S3_REGION"),
		AccessKey: os.Getenv("PALAI_S3_ACCESS_KEY"),
		SecretKey: os.Getenv("PALAI_S3_SECRET_KEY"),
	})
	if err != nil {
		t.Fatalf("NewStore() error = %v", err)
	}
	if err := s3.EnsureBucket(ctx); err != nil {
		t.Fatalf("EnsureBucket() error = %v", err)
	}
	pool := repo.Spine().Pool()
	writer := artifacts.NewWriter(s3, pool)
	org, project, runID := seedRun(t, pool)

	art, err := writer.Write(ctx, artifacts.WriteRequest{Organization: org, Project: project, RunID: runID, Content: output})
	if err != nil {
		t.Fatalf("Write() error = %v", err)
	}

	// 3. Read it back from the real object store and prove row + bytes + checksum agree.
	gotArt, body, found, err := writer.Read(ctx, org, project, art.ID)
	if err != nil {
		t.Fatalf("Read() error = %v", err)
	}
	if !found {
		t.Fatalf("artifact %s absent after a real write to %s", art.ID, endpoint)
	}
	if !bytes.Equal(body, output) {
		t.Fatalf("stored bytes differ from the completion output")
	}
	sum := sha256.Sum256(output)
	wantChecksum := "sha256:" + hex.EncodeToString(sum[:])
	if gotArt.Checksum != wantChecksum || art.Checksum != wantChecksum {
		t.Fatalf("checksum = %q/%q, want %q (SHA-256 of the persisted bytes)", art.Checksum, gotArt.Checksum, wantChecksum)
	}
	if gotArt.SizeBytes != int64(len(output)) {
		t.Fatalf("size = %d, want %d", gotArt.SizeBytes, len(output))
	}

	// 4. Credential absence by construction: the provider secret must not appear in the
	// streamed output, the persisted artifact bytes, or the object key. The comparison is
	// opaque; the value is never printed.
	surfaces := map[string][]byte{
		"streamed output": streamed.Bytes(),
		"artifact bytes":  body,
		"object key":      []byte(gotArt.ObjectKey),
	}
	for name, captured := range surfaces {
		if bytes.Contains(captured, []byte(secret)) {
			t.Fatalf("%s contains the credential value", name) // never echo the value
		}
	}

	t.Logf("live PASS provider_request_id=%s… artifact=%s bytes=%d checksum=%s endpoint=%s",
		safePrefix(res.ProviderRequestID), art.ID, art.SizeBytes, art.Checksum, endpoint)
}

// seedRun creates org -> project -> session -> run so the artifacts row's foreign keys hold.
func seedRun(t *testing.T, pool *pgxpool.Pool) (org, project, runID string) {
	t.Helper()
	org, project = newID("org"), newID("prj")
	session := newID("ses")
	runID = newID("run")
	execSQL(t, pool, `INSERT INTO organizations (id) VALUES ($1)`, org)
	execSQL(t, pool, `INSERT INTO projects (id, organization_id) VALUES ($1, $2)`, project, org)
	execSQL(t, pool, `INSERT INTO sessions (id, organization_id, project_id) VALUES ($1, $2, $3)`, session, org, project)
	execSQL(t, pool, `INSERT INTO runs (id, organization_id, project_id, session_id) VALUES ($1, $2, $3, $4)`, runID, org, project, session)
	return org, project, runID
}

func execSQL(t *testing.T, pool *pgxpool.Pool, sql string, args ...any) {
	t.Helper()
	if _, err := pool.Exec(context.Background(), sql, args...); err != nil {
		t.Fatalf("exec %q error = %v", sql, err)
	}
}

func safePrefix(id string) string {
	if len(id) > 16 {
		return id[:16]
	}
	return id
}

func envOr(name, def string) string {
	if v := os.Getenv(name); v != "" {
		return v
	}
	return def
}

func newID(prefix string) string {
	var raw [8]byte
	_, _ = rand.Read(raw[:])
	return prefix + "_" + hex.EncodeToString(raw[:])
}
