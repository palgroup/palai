package api

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
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
	// The ingest half: what the handler decided the bytes WERE (its sniff) and what it passed on, so a
	// test can assert the request's own Content-Type never reached the store.
	created          []byte
	createdMediaType string
	createID         string
	createErr        error
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

func (f *fakeArtifactAPI) CreateInboundArtifact(_ context.Context, s middleware.Scope, content []byte, mediaType string) (ArtifactIngestResult, error) {
	f.lastScope, f.created, f.createdMediaType = s, content, mediaType
	return ArtifactIngestResult{ID: f.createID}, f.createErr
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
	if fake.lastScope.Project != "prj_1" {
		t.Fatalf("metadata seam scope project = %q, want the verified prj_1 (identity, not a body field)", fake.lastScope.Project)
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

// onePixelPNG is a real, minimal PNG. It is REAL BYTES rather than a `[]byte("\x89PNG...")` prefix because
// the handler sniffs with http.DetectContentType, and a test whose fixture only satisfies the first eight
// bytes would pass while telling us nothing about whether a genuine upload survives the same path.
var onePixelPNG = []byte{
	0x89, 'P', 'N', 'G', 0x0d, 0x0a, 0x1a, 0x0a,
	0x00, 0x00, 0x00, 0x0d, 'I', 'H', 'D', 'R',
	0x00, 0x00, 0x00, 0x01, 0x00, 0x00, 0x00, 0x01, 0x08, 0x06, 0x00, 0x00, 0x00,
	0x1f, 0x15, 0xc4, 0x89,
	0x00, 0x00, 0x00, 0x0a, 'I', 'D', 'A', 'T',
	0x78, 0x9c, 0x63, 0x00, 0x01, 0x00, 0x00, 0x05, 0x00, 0x01,
	0x0d, 0x0a, 0x2d, 0xb4,
	0x00, 0x00, 0x00, 0x00, 'I', 'E', 'N', 'D', 0xae, 0x42, 0x60, 0x82,
}

// TestArtifactIngestAcceptsAnImageAndSniffsIt pins the write verb the public API never had: a POST of image
// bytes answers 201 with a server-minted id, and the media type that reaches the store is the one the
// handler SNIFFED — not the one the request declared.
//
// The declared Content-Type here is a deliberate lie ("text/plain"). If the handler ever starts believing
// it, this test fails on the media type rather than passing quietly, which is the whole point: the bytes
// are the only authority about what bytes are.
func TestArtifactIngestAcceptsAnImageAndSniffsIt(t *testing.T) {
	fake := &fakeArtifactAPI{createID: "art_minted"}
	base := artifactTestServer(t, fake)

	resp := do(t, "POST", base+"/v1/artifacts", string(onePixelPNG), map[string]string{"Content-Type": "text/plain"})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("POST /v1/artifacts status = %d, want 201", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if !strings.Contains(string(body), `"art_minted"`) {
		t.Fatalf("the create body did not carry the minted id: %s", body)
	}
	if fake.createdMediaType != "image/png" {
		t.Fatalf("the store received media type %q, want image/png — the handler must sniff the bytes, never trust the request's Content-Type", fake.createdMediaType)
	}
	if !bytes.Equal(fake.created, onePixelPNG) {
		t.Fatalf("the store received %d bytes, want the %d posted", len(fake.created), len(onePixelPNG))
	}
	if fake.lastScope.Project == "" {
		t.Fatal("the ingest reached the seam with no verified scope — a write verb must be tenant-scoped by the key, never by a body field")
	}
}

// TestArtifactIngestRefusesWhatIsNotAnImage pins the allow-list over SNIFFED bytes. The payload is a shell
// script sent as image/png, which is exactly the confusion an uploader-declared mimetype invites: it must
// be refused with 415 and must never reach the store.
func TestArtifactIngestRefusesWhatIsNotAnImage(t *testing.T) {
	fake := &fakeArtifactAPI{createID: "art_should_not_exist"}
	base := artifactTestServer(t, fake)

	resp := do(t, "POST", base+"/v1/artifacts", "#!/bin/sh\nrm -rf /\n", map[string]string{"Content-Type": "image/png"})
	if resp.StatusCode != http.StatusUnsupportedMediaType {
		t.Fatalf("POST of a shell script declared image/png = %d, want 415", resp.StatusCode)
	}
	resp.Body.Close()
	if fake.created != nil {
		t.Fatal("a refused payload reached the store — the type check must happen BEFORE the write, or the object store holds bytes no reader accepts")
	}
}

// TestArtifactIngestRefusesOversizeBeforeStoring pins the relay-independent half of the ceiling: the server
// applies its own bound, so a client that skipped its check stores nothing.
//
// The body is one byte over, which is the boundary that actually matters — a test using 50 MiB would pass
// against an off-by-a-megabyte limit.
func TestArtifactIngestRefusesOversizeBeforeStoring(t *testing.T) {
	fake := &fakeArtifactAPI{createID: "art_should_not_exist"}
	base := artifactTestServer(t, fake)

	oversize := make([]byte, maxInboundArtifactBytes+1)
	copy(oversize, onePixelPNG) // sniffs as a PNG, so ONLY the size can refuse it
	resp := do(t, "POST", base+"/v1/artifacts", string(oversize), nil)
	if resp.StatusCode != http.StatusRequestEntityTooLarge {
		t.Fatalf("POST of %d bytes = %d, want 413", len(oversize), resp.StatusCode)
	}
	resp.Body.Close()
	if fake.created != nil {
		t.Fatalf("an oversize payload reached the store (%d bytes) — the bound must refuse before the write", len(fake.created))
	}
}

// TestArtifactIngestRefusesAnEmptyBody pins the 400: an empty POST is a client mistake, and answering it
// with a stored zero-byte artifact would put a row in the table that no reader can ever resolve to an image.
func TestArtifactIngestRefusesAnEmptyBody(t *testing.T) {
	fake := &fakeArtifactAPI{createID: "art_should_not_exist"}
	base := artifactTestServer(t, fake)

	resp := do(t, "POST", base+"/v1/artifacts", "", nil)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("POST of an empty body = %d, want 400", resp.StatusCode)
	}
	resp.Body.Close()
	if fake.created != nil {
		t.Fatal("an empty payload reached the store")
	}
}

// TestIngestCeilingMatchesDispatchCeiling is the test maxInboundArtifactBytes' own comment promises, and it
// exists because that comment makes a claim about a DIFFERENT package's constant — the exact shape of claim
// this tree has repeatedly found to be false by the time somebody reads it.
//
// It does not compare two numbers this package can see (there is only one; api does not import execution,
// the dependency runs the other way). It READS execution's source and pins the literal, so raising either
// ceiling without the other turns this red instead of leaving an ingest that stores bytes no run can read.
func TestIngestCeilingMatchesDispatchCeiling(t *testing.T) {
	src, err := os.ReadFile(filepath.Join("..", "internal", "execution", "model_dispatch.go"))
	if err != nil {
		t.Fatalf("read execution's source: %v", err)
	}
	const want = "const maxImageBytes = 5 << 20"
	if !strings.Contains(string(src), want) {
		t.Fatalf("execution.maxImageBytes is no longer %q — the ingest ceiling (maxInboundArtifactBytes = %d) is a copy of it and must move with it, or POST /v1/artifacts will accept images the model dispatch then refuses",
			want, maxInboundArtifactBytes)
	}
	if maxInboundArtifactBytes != 5<<20 {
		t.Fatalf("maxInboundArtifactBytes = %d, want %d to match execution.maxImageBytes", maxInboundArtifactBytes, 5<<20)
	}
}
