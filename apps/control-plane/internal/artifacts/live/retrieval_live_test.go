//go:build live

package live

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	providerone "github.com/palgroup/palai/adapters/models/provider_one"
	"github.com/palgroup/palai/apps/control-plane/api"
	"github.com/palgroup/palai/apps/control-plane/api/middleware"
	"github.com/palgroup/palai/apps/control-plane/internal/artifacts"
	"github.com/palgroup/palai/apps/control-plane/internal/store"
	"github.com/palgroup/palai/packages/contracts"
	modelbroker "github.com/palgroup/palai/packages/model-broker"
)

// scopedVerifier resolves every bearer to one fixed tenant scope, so the live smoke drives the real
// retrieval routes as the tenant that owns the artifact. The point under test is the streaming download +
// bit-identity, not auth (that is proven in the unit/component tiers).
type scopedVerifier struct{ scope middleware.Scope }

func (v scopedVerifier) VerifyAPIKey(context.Context, string) (middleware.Scope, error) {
	return v.scope, nil
}

// TestLiveArtifactRetrievalOverAPI is the E13 T5 live smoke: a REAL provider-one completion's output is
// persisted as an artifact to a real object store, then downloaded THROUGH the public HTTP API, and the
// downloaded bytes are bit-identical to the produced file (checksum + Content-Digest match). It runs under
// `make test-live-provider PROVIDER=provider-one CASE=artifact-retrieval`.
//
// HONEST CEILING: the produced "file" is the run's completion output (the model does not yet drive a file
// tool — E12/T5 registry work), and the download is an AUTHENTICATED DIRECT stream; a pre-signed URL policy
// + expiry ceremony is E13-H (DAT-006's remainder). The LIVE element is a real completion's bytes crossing
// a real S3 and a real HTTP download, checksum-verified end to end — not a mock.
func TestLiveArtifactRetrievalOverAPI(t *testing.T) {
	secret := os.Getenv(credentialEnv)
	if secret == "" {
		t.Fatalf("%s is unset; the live tier loads it from .env.local at runtime", credentialEnv)
	}
	pgURL := os.Getenv("PALAI_COMPONENT_POSTGRES_URL")
	endpoint := os.Getenv("PALAI_S3_ENDPOINT")
	if pgURL == "" || endpoint == "" {
		t.Skip("PALAI_COMPONENT_POSTGRES_URL + PALAI_S3_ENDPOINT are required; run make test-live-provider PROVIDER=provider-one CASE=artifact-retrieval")
	}
	ctx := context.Background()

	// 1. A REAL provider-one completion stands in for a coding run's produced file.
	broker := modelbroker.New(modelbroker.Config{
		Adapters: map[string]modelbroker.ModelAdapter{"provider-one": providerone.Adapter{}},
		Secrets:  modelbroker.EnvResolver{"provider-one": credentialEnv},
	})
	var streamed bytes.Buffer
	res, err := broker.Route(ctx, "provider-one", modelbroker.Request{
		ModelRequestID: contracts.ModelRequestID("mreq_live_retrieval"),
		RouteRevision:  1,
		ModelStepID:    "step-1",
		Model:          liveModel(),
		Messages: []modelbroker.Message{
			{Role: "user", Content: "Reply with one short line, like a CI terminal reporting a successful build (no code fences)."},
		},
		Deadline:    time.Now().Add(60 * time.Second),
		Reservation: modelbroker.Reservation{MaxTotalTokens: 2000},
		Secret:      modelbroker.SecretRef("provider-one"),
	}, func(d modelbroker.Delta) { streamed.WriteString(d.Text) })
	if err != nil {
		t.Fatalf("route: %v", err)
	}
	if res.Error != nil {
		t.Fatalf("provider returned a sanitized error: code=%s status=%d", res.Error.Code, res.Error.Status)
	}
	produced := streamed.Bytes()
	if len(produced) == 0 {
		t.Fatal("the real completion produced no output to persist as an artifact")
	}

	// 2. Persist it to the REAL object store + durable row, and stand the retrieval API on the same store.
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
	org, project, runID := seedRun(t, pool)
	art, err := artifacts.NewWriter(s3, pool).Write(ctx, artifacts.WriteRequest{
		Organization: org, Project: project, RunID: runID, Content: produced,
		MediaType: "text/plain", LogicalType: "log",
	})
	if err != nil {
		t.Fatalf("Write() error = %v", err)
	}

	verifier := scopedVerifier{middleware.Scope{Organization: org, Project: project}}
	reader := artifacts.NewReader(s3, pool)
	srv := httptest.NewServer(api.NewRouter(verifier, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, reader, api.SSEConfig{}, nil, nil))
	defer srv.Close()

	// 3. Download the artifact THROUGH the public API and prove the bytes are bit-identical to the produced
	// file — the checksum of the downloaded stream equals the write-path's recorded checksum.
	resp, err := httpGet(t, srv.URL+"/v1/artifacts/"+art.ID+"/content")
	if err != nil {
		t.Fatalf("download: %v", err)
	}
	downloaded, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("download status = %d, want 200", resp.StatusCode)
	}
	if !bytes.Equal(downloaded, produced) {
		t.Fatal("downloaded bytes are NOT bit-identical to the produced file")
	}
	sum := sha256.Sum256(downloaded)
	if "sha256:"+hex.EncodeToString(sum[:]) != art.Checksum {
		t.Fatalf("downloaded checksum != recorded %s", art.Checksum)
	}
	wantDigest := "sha-256=:" + base64.StdEncoding.EncodeToString(sum[:]) + ":"
	if got := resp.Header.Get("Content-Digest"); got != wantDigest {
		t.Fatalf("Content-Digest = %q, want %q", got, wantDigest)
	}

	// 4. Credential absence by construction: the download bytes must not carry the provider secret. The
	// comparison is opaque; the value is never printed.
	if bytes.Contains(downloaded, []byte(secret)) {
		t.Fatal("the downloaded artifact contains the credential value") // never echo the value
	}

	t.Logf("live PASS provider_request_id=%s… artifact=%s bytes=%d checksum=%s endpoint=%s",
		safePrefix(res.ProviderRequestID), art.ID, art.SizeBytes, art.Checksum, endpoint)
}

// httpGet issues an authenticated GET and returns the response for the caller to drain.
func httpGet(t *testing.T, url string) (*http.Response, error) {
	t.Helper()
	req, _ := http.NewRequest(http.MethodGet, url, nil)
	req.Header.Set("Authorization", "Bearer live-smoke")
	return http.DefaultClient.Do(req)
}
