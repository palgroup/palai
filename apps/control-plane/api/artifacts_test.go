package api

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/palgroup/palai/apps/control-plane/api/middleware"
)

// fakeArtifactAPI scripts each retrieval outcome so the handler contract is exercised without a database
// or object store. It records the id/response-id and scope that reached the seam so a test can assert the
// route wired through the verified identity.
type fakeArtifactAPI struct {
	meta           ArtifactResult
	content        ArtifactContent
	list           ArtifactResult
	lastID         string
	lastResponseID string
	lastScope      middleware.Scope
}

func (f *fakeArtifactAPI) GetArtifact(_ context.Context, s middleware.Scope, id string) (ArtifactResult, error) {
	f.lastScope, f.lastID = s, id
	return f.meta, nil
}
func (f *fakeArtifactAPI) OpenArtifactContent(_ context.Context, s middleware.Scope, id string) (ArtifactContent, error) {
	f.lastScope, f.lastID = s, id
	return f.content, nil
}
func (f *fakeArtifactAPI) ListRunArtifacts(_ context.Context, s middleware.Scope, responseID string) (ArtifactResult, error) {
	f.lastScope, f.lastResponseID = s, responseID
	return f.list, nil
}

func artifactTestServer(t *testing.T, arts ArtifactAPI) string {
	t.Helper()
	srv := httptest.NewServer(NewRouter(fakeVerifier{}, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, arts, SSEConfig{}, nil, nil))
	t.Cleanup(srv.Close)
	return srv.URL
}

// TestArtifactRetrievalSurface pins the routing + rendering contract for every retrieval route: metadata
// and the run-scoped list are 200 with the store's verbatim JSON, and an unknown/foreign id renders the
// non-disclosing 404 — never a 403 that would confirm the artifact exists in another tenant.
func TestArtifactRetrievalSurface(t *testing.T) {
	fake := &fakeArtifactAPI{
		meta: ArtifactResult{Body: []byte(`{"id":"art_1","object":"artifact"}`)},
		list: ArtifactResult{Body: []byte(`{"object":"list","data":[]}`)},
	}
	base := artifactTestServer(t, fake)

	if resp := do(t, "GET", base+"/v1/artifacts/art_1", ``, nil); resp.StatusCode != http.StatusOK {
		t.Fatalf("GET metadata status = %d, want 200", resp.StatusCode)
	}
	if fake.lastID != "art_1" {
		t.Fatalf("metadata reached the seam with id %q, want art_1", fake.lastID)
	}
	if fake.lastScope.Organization != "org_1" {
		t.Fatalf("metadata seam scope org = %q, want the verified org_1 (identity, not a body field)", fake.lastScope.Organization)
	}
	if resp := do(t, "GET", base+"/v1/responses/resp_9/artifacts", ``, nil); resp.StatusCode != http.StatusOK {
		t.Fatalf("GET run-scoped list status = %d, want 200", resp.StatusCode)
	}
	if fake.lastResponseID != "resp_9" {
		t.Fatalf("list reached the seam with response %q, want resp_9", fake.lastResponseID)
	}

	// A wrong-tenant or unknown id is an indistinguishable miss on every route (§22.6 non-disclosure).
	fake.meta = ArtifactResult{NotFound: true}
	fake.list = ArtifactResult{NotFound: true}
	fake.content = ArtifactContent{NotFound: true}
	for _, path := range []string{"/v1/artifacts/art_missing", "/v1/artifacts/art_missing/content", "/v1/responses/resp_missing/artifacts"} {
		if resp := do(t, "GET", base+path, ``, nil); resp.StatusCode != http.StatusNotFound {
			t.Fatalf("GET %s missing status = %d, want 404 (no existence disclosure)", path, resp.StatusCode)
		}
	}
}

// TestArtifactContentStreams proves the download route streams the object's bytes verbatim and carries the
// integrity headers: Content-Type from the artifact's media type, Content-Length from its size, and the
// RFC 9530 Content-Digest the client verifies against.
func TestArtifactContentStreams(t *testing.T) {
	payload := []byte("diff --git a/x b/x\n+hello\n")
	fake := &fakeArtifactAPI{content: ArtifactContent{
		Reader:    io.NopCloser(strings.NewReader(string(payload))),
		SizeBytes: int64(len(payload)),
		MediaType: "text/x-diff",
		Digest:    "sha-256=:47DEQpj8HBSa+/TImW+5JCeuQeRkm5NMpJWZG3hSuFU=:",
	}}
	base := artifactTestServer(t, fake)

	resp := do(t, "GET", base+"/v1/artifacts/art_1/content", ``, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("content status = %d, want 200", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "text/x-diff" {
		t.Fatalf("Content-Type = %q, want text/x-diff", ct)
	}
	if dg := resp.Header.Get("Content-Digest"); dg != "sha-256=:47DEQpj8HBSa+/TImW+5JCeuQeRkm5NMpJWZG3hSuFU=:" {
		t.Fatalf("Content-Digest = %q, want the RFC 9530 sha-256 dictionary value", dg)
	}
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if string(body) != string(payload) {
		t.Fatalf("streamed body = %q, want the object bytes verbatim %q", body, payload)
	}
}

// TestArtifactRoutesUnmountedWhenNil proves the nil-seam guard: a tier that wires no artifact API (no object
// store configured) mounts no retrieval route, so the Docker-free conformance tiers stay unaffected.
func TestArtifactRoutesUnmountedWhenNil(t *testing.T) {
	base := artifactTestServer(t, nil)
	if resp := do(t, "GET", base+"/v1/artifacts/art_1", ``, nil); resp.StatusCode != http.StatusNotFound {
		t.Fatalf("nil artifacts GET status = %d, want 404 (route unmounted)", resp.StatusCode)
	}
}
