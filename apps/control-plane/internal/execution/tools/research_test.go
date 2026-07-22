package tools

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"errors"
	"math"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	toolbroker "github.com/palgroup/palai/packages/tool-broker"
)

type researchResolver func(ctx context.Context, host string) ([]net.IPAddr, error)

func (f researchResolver) LookupIPAddr(ctx context.Context, host string) ([]net.IPAddr, error) {
	return f(ctx, host)
}

// dialRecorder records every address the pinned dialer vetted-then-dialed, and (when redirectTo is set)
// redirects the connection to a real local listener — so a test can present a hostname that resolves to
// a PUBLIC IP (research never allows private) while the bytes come from a local TLS server.
func dialRecorder(redirectTo string) (func(context.Context, string, string) (net.Conn, error), *[]string) {
	var dialed []string
	dial := func(ctx context.Context, network, addr string) (net.Conn, error) {
		dialed = append(dialed, addr)
		target := addr
		if redirectTo != "" {
			target = redirectTo
		}
		return (&net.Dialer{}).DialContext(ctx, network, target)
	}
	return dial, &dialed
}

func dialedAny(dialed []string, prefix string) bool {
	for _, a := range dialed {
		if strings.HasPrefix(a, prefix) {
			return true
		}
	}
	return false
}

// TestResearchTLSRequired pins the https-only gate: an http:// URL is a terminal deny (research never
// sets an allowPrivate downgrade), and no connection is attempted.
func TestResearchTLSRequired(t *testing.T) {
	dial, dialed := dialRecorder("")
	tool := ResearchFetchTool(WithResearchDialContext(dial))
	_, err := tool.Exec(context.Background(), toolbroker.ExecEnv{}, map[string]any{"url": "http://example.com/"})
	if err == nil {
		t.Fatal("research fetch of an http:// URL = nil error, want a terminal https-required deny")
	}
	if len(*dialed) != 0 {
		t.Fatalf("http:// URL dialed %v, want NO dial (denied at the static gate)", *dialed)
	}
}

// TestResearchDeniesPrivateAndMetadataTargetsAfterResolveAndRedirect is the SSRF heart of the tool: a
// model-supplied URL is a fully-untrusted primitive, so a private/loopback/link-local/metadata target
// must be denied — as a literal IP (static gate), through a hostname that RESOLVES internal (pinned
// dial, zero internal dial), and through a REDIRECT into an internal target (re-vetted per hop, never
// dialed). Rebinding under a redirect is caught on the hop it flips.
func TestResearchDeniesPrivateAndMetadataTargetsAfterResolveAndRedirect(t *testing.T) {
	// --- Literal internal/metadata targets: denied at the static gate, never dialed. ---
	for _, u := range []string{
		"https://169.254.169.254/", // AWS/GCP/Azure metadata (link-local)
		"https://127.0.0.1/",       // loopback
		"https://10.0.0.8/",        // RFC1918
		"https://100.100.100.200/", // Alibaba metadata (RFC6598 CGNAT)
		"https://[::1]/",           // loopback v6
		"https://[fd00::1]/",       // ULA v6
	} {
		dial, dialed := dialRecorder("")
		tool := ResearchFetchTool(WithResearchDialContext(dial))
		if _, err := tool.Exec(context.Background(), toolbroker.ExecEnv{}, map[string]any{"url": u}); err == nil {
			t.Errorf("research fetch of %s = nil error, want denied", u)
		}
		if len(*dialed) != 0 {
			t.Errorf("research fetch of %s dialed %v, want NO dial", u, *dialed)
		}
	}

	// --- Hostname that RESOLVES internal: static gate passes (it is a name), the pinned dial resolves,
	// vets, and denies — ZERO internal dial. ---
	res := researchResolver(func(_ context.Context, host string) ([]net.IPAddr, error) {
		if host == "internal.attacker.example" {
			return []net.IPAddr{{IP: net.ParseIP("10.0.0.9")}}, nil
		}
		return nil, net.UnknownNetworkError("no such host")
	})
	dial, dialed := dialRecorder("")
	tool := ResearchFetchTool(WithResearchResolver(res), WithResearchDialContext(dial))
	if _, err := tool.Exec(context.Background(), toolbroker.ExecEnv{}, map[string]any{"url": "https://internal.attacker.example/"}); err == nil {
		t.Error("research fetch of a name resolving to 10.0.0.9 = nil error, want denied")
	}
	if dialedAny(*dialed, "10.0.0.") {
		t.Errorf("the internal resolution was dialed %v — pinned-dial vet failed", *dialed)
	}

	// --- Redirect INTO the metadata IP: the redirector is public, its 302 Location is the metadata IP.
	// The redirect is re-vetted and denied; the metadata IP is never dialed. ---
	pubResolver := researchResolver(func(_ context.Context, _ string) ([]net.IPAddr, error) {
		return []net.IPAddr{{IP: net.ParseIP("93.184.216.34")}}, nil // always public
	})
	redirector := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Location", "https://169.254.169.254/latest/meta-data")
		w.WriteHeader(http.StatusFound)
	}))
	defer redirector.Close()
	rDial, rDialed := dialRecorder(redirector.Listener.Addr().String())
	rTool := ResearchFetchTool(WithResearchResolver(pubResolver), WithResearchDialContext(rDial), WithResearchTLSConfig(trustServer(redirector)))
	if _, err := rTool.Exec(context.Background(), toolbroker.ExecEnv{}, map[string]any{"url": "https://redirector.example/"}); err == nil {
		t.Error("research fetch that redirects to metadata = nil error, want denied")
	}
	if dialedAny(*rDialed, "169.254.169.254:") {
		t.Errorf("the metadata IP was dialed via the redirect %v — SSRF redirect vector is open", *rDialed)
	}

	// --- Rebind under a redirect: hop 1 resolves public and is dialed; the same host flips to private on
	// hop 2, which the pinned dial denies before connecting. ---
	var lookups int
	flip := researchResolver(func(_ context.Context, _ string) ([]net.IPAddr, error) {
		lookups++
		if lookups == 1 {
			return []net.IPAddr{{IP: net.ParseIP("93.184.216.34")}}, nil // public
		}
		return []net.IPAddr{{IP: net.ParseIP("10.0.0.7")}}, nil // flipped private
	})
	selfRedirect := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Location", "https://example.com/next")
		w.WriteHeader(http.StatusFound)
	}))
	defer selfRedirect.Close()
	fDial, fDialed := dialRecorder(selfRedirect.Listener.Addr().String())
	fTool := ResearchFetchTool(WithResearchResolver(flip), WithResearchDialContext(fDial), WithResearchTLSConfig(trustServer(selfRedirect)))
	if _, err := fTool.Exec(context.Background(), toolbroker.ExecEnv{}, map[string]any{"url": "https://example.com/"}); err == nil {
		t.Error("research fetch that rebinds to private under a redirect = nil error, want denied")
	}
	if dialedAny(*fDialed, "10.0.0.7:") {
		t.Errorf("the flipped private IP was dialed %v — rebind under redirect not closed", *fDialed)
	}
}

// TestResearchFetchProducesCitations proves the happy path: a real HTTPS fetch of an HTML page returns
// the extracted text as an excerpt plus a single citation carrying the final URL, the <title>, an
// RFC3339-UTC retrieval time, and a content_hash over the RAW bytes. The request carries the honest UA
// and NO credential. A redirect variant pins that the citation URL is the FINAL (post-redirect) URL.
func TestResearchFetchProducesCitations(t *testing.T) {
	body := []byte("<!DOCTYPE html><html><head><title>Example Domain</title></head>" +
		"<body><h1>Heading</h1><p>Hello &amp; welcome to the research fetch.</p>" +
		"<script>var x = 1;</script></body></html>")
	var sawCredential bool
	var sawUA string
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Cookie") != "" || r.Header.Get("Authorization") != "" {
			sawCredential = true
		}
		sawUA = r.Header.Get("User-Agent")
		if r.URL.Path == "/redir" {
			w.Header().Set("Location", "https://example.com/final")
			w.WriteHeader(http.StatusFound)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write(body)
	}))
	defer srv.Close()

	pubResolver := researchResolver(func(_ context.Context, _ string) ([]net.IPAddr, error) {
		return []net.IPAddr{{IP: net.ParseIP("93.184.216.34")}}, nil
	})
	dial, _ := dialRecorder(srv.Listener.Addr().String())
	tool := ResearchFetchTool(WithResearchResolver(pubResolver), WithResearchDialContext(dial), WithResearchTLSConfig(trustServer(srv)))

	out, err := tool.Exec(context.Background(), toolbroker.ExecEnv{}, map[string]any{"url": "https://example.com/"})
	if err != nil {
		t.Fatalf("research fetch = %v, want success", err)
	}
	if sawCredential {
		t.Error("the fetch carried a Cookie/Authorization header — research must send no credential")
	}
	if sawUA != "palai-research/1" {
		t.Errorf("User-Agent = %q, want palai-research/1", sawUA)
	}
	if excerpt, _ := out["excerpt"].(string); !strings.Contains(excerpt, "Hello & welcome") || strings.Contains(excerpt, "var x") {
		t.Errorf("excerpt = %q, want the unescaped body text with the <script> stripped", out["excerpt"])
	}
	if out["truncated"] != false {
		t.Errorf("truncated = %v, want false for a small page", out["truncated"])
	}
	if out["url"] != "https://example.com/" {
		t.Errorf("url = %v, want the fetched URL", out["url"])
	}
	cites, ok := out["citations"].([]any)
	if !ok || len(cites) != 1 {
		t.Fatalf("citations = %v, want exactly one", out["citations"])
	}
	cite := cites[0].(map[string]any)
	if cite["title"] != "Example Domain" {
		t.Errorf("citation title = %v, want the <title>", cite["title"])
	}
	if cite["url"] != "https://example.com/" {
		t.Errorf("citation url = %v, want the final URL", cite["url"])
	}
	wantHash := "sha256:" + hex.EncodeToString(sha256Sum(body))
	if cite["content_hash"] != wantHash {
		t.Errorf("content_hash = %v, want sha256 over the raw bytes %s", cite["content_hash"], wantHash)
	}
	if ra, _ := cite["retrieved_at"].(string); !strings.HasSuffix(ra, "Z") || len(ra) < 20 {
		t.Errorf("retrieved_at = %q, want RFC3339 UTC", cite["retrieved_at"])
	}

	// Redirect variant: the citation URL is the FINAL post-redirect URL.
	out2, err := tool.Exec(context.Background(), toolbroker.ExecEnv{}, map[string]any{"url": "https://example.com/redir"})
	if err != nil {
		t.Fatalf("research fetch (redirect) = %v, want success", err)
	}
	if out2["url"] != "https://example.com/final" {
		t.Errorf("post-redirect url = %v, want the final URL https://example.com/final", out2["url"])
	}
	cite2 := out2["citations"].([]any)[0].(map[string]any)
	if cite2["url"] != "https://example.com/final" {
		t.Errorf("post-redirect citation url = %v, want the final URL", cite2["url"])
	}
}

type fakeResearchArtifactWriter struct {
	content     []byte
	mediaType   string
	logicalType string
}

func (f *fakeResearchArtifactWriter) WriteArtifact(_ context.Context, _, _, _ string, content []byte, mediaType, logicalType string, _ map[string]any) (string, error) {
	f.content = append([]byte(nil), content...)
	f.mediaType, f.logicalType = mediaType, logicalType
	return "art_research_1", nil
}

// TestResearchLargeBodyBoundedExcerptPlusArtifact pins the large-page path: the model gets a bounded
// excerpt while the FULL (fetch-capped) body is persisted to the artifact store, and the citation's
// content_hash is exactly the persisted bytes — model claim → citation → artifact → bytes. When no
// store is wired the tool still returns the excerpt with an empty artifact id (clean fall).
func TestResearchLargeBodyBoundedExcerptPlusArtifact(t *testing.T) {
	big := bytes.Repeat([]byte("A"), 100<<10) // 100 KiB: over the 64 KiB excerpt cap, under the 4 MiB fetch cap
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		_, _ = w.Write(big)
	}))
	defer srv.Close()

	pubResolver := researchResolver(func(_ context.Context, _ string) ([]net.IPAddr, error) {
		return []net.IPAddr{{IP: net.ParseIP("93.184.216.34")}}, nil
	})
	dial, _ := dialRecorder(srv.Listener.Addr().String())
	tool := ResearchFetchTool(WithResearchResolver(pubResolver), WithResearchDialContext(dial), WithResearchTLSConfig(trustServer(srv)))

	// --- With an artifact store: bounded excerpt + full body persisted, hash pins the persisted bytes. ---
	aw := &fakeResearchArtifactWriter{}
	env := toolbroker.ExecEnv{Artifacts: aw, Scope: toolbroker.TaskScope{Org: "o", Project: "p", RunID: "run_1"}}
	out, err := tool.Exec(context.Background(), env, map[string]any{"url": "https://example.com/"})
	if err != nil {
		t.Fatalf("research fetch (large) = %v, want success", err)
	}
	if excerpt, _ := out["excerpt"].(string); len(excerpt) > maxResearchExcerptBytes {
		t.Errorf("excerpt is %d bytes, want ≤ %d (bounded)", len(excerpt), maxResearchExcerptBytes)
	}
	if out["truncated"] != true {
		t.Errorf("truncated = %v, want true (the excerpt was capped)", out["truncated"])
	}
	if out["artifact_id"] != "art_research_1" {
		t.Errorf("artifact_id = %v, want the persisted id", out["artifact_id"])
	}
	if !bytes.Equal(aw.content, big) {
		t.Errorf("artifact stored %d bytes, want the full %d-byte body", len(aw.content), len(big))
	}
	if aw.logicalType != "research_fetch" {
		t.Errorf("artifact logicalType = %q, want research_fetch", aw.logicalType)
	}
	cite := out["citations"].([]any)[0].(map[string]any)
	wantHash := "sha256:" + hex.EncodeToString(sha256Sum(aw.content))
	if cite["content_hash"] != wantHash {
		t.Errorf("content_hash = %v, want the hash of the persisted bytes %s", cite["content_hash"], wantHash)
	}

	// --- No artifact store: excerpt still returned, artifact id empty (a workspace-less run is fine). ---
	out2, err := tool.Exec(context.Background(), toolbroker.ExecEnv{}, map[string]any{"url": "https://example.com/"})
	if err != nil {
		t.Fatalf("research fetch (no store) = %v, want success", err)
	}
	if out2["artifact_id"] != "" {
		t.Errorf("artifact_id = %v, want empty with no store", out2["artifact_id"])
	}
	if excerpt, _ := out2["excerpt"].(string); excerpt == "" {
		t.Error("excerpt is empty with no store, want the bounded excerpt still returned")
	}
}

// TestResearchHugeMaxBytesDoesNotPanic pins the fetch-cap logic against a model-supplied max_bytes that
// overflows an int conversion: on amd64 int(1e300) is NEGATIVE (minInt64), which would drive fetchCap
// negative → raw[:fetchCap] panics → the model can repeat it for a restart-loop DoS. The cap must be
// computed in float space, so a huge/Inf/sub-1 value leaves the 4 MiB ceiling intact and never negative.
// Asserting the cap function directly makes this meaningful on arm64 (which saturates instead of wraps).
func TestResearchHugeMaxBytesDoesNotPanic(t *testing.T) {
	cases := []struct {
		in   any
		want int
	}{
		{1e300, maxResearchFetchBytes},       // overflows int on amd64 → must NOT lower the cap
		{math.Inf(1), maxResearchFetchBytes}, // +Inf
		{0.5, maxResearchFetchBytes},         // 0<mb<1 must not zero the cap
		{0.0, maxResearchFetchBytes},
		{-5.0, maxResearchFetchBytes},
		{1024.0, 1024}, // a sane lower cap is honored
		{"not a number", maxResearchFetchBytes},
		{nil, maxResearchFetchBytes},
	}
	for _, c := range cases {
		got := researchFetchCap(c.in)
		if got != c.want {
			t.Errorf("researchFetchCap(%v) = %d, want %d", c.in, got, c.want)
		}
		if got < 0 {
			t.Errorf("researchFetchCap(%v) = %d — a negative cap panics raw[:cap]", c.in, got)
		}
	}
}

// TestResearchDeniesEmbeddedCredentials pins that a model-supplied userinfo URL (https://user:pass@host)
// is denied before any dial: net/http would otherwise send an Authorization: Basic header built from the
// model's credentials to an arbitrary host — a probing primitive. Denied at the static gate, zero dial.
func TestResearchDeniesEmbeddedCredentials(t *testing.T) {
	var dialed []string
	dial := func(_ context.Context, _, addr string) (net.Conn, error) {
		dialed = append(dialed, addr)
		return nil, errors.New("no dial")
	}
	res := researchResolver(func(_ context.Context, _ string) ([]net.IPAddr, error) {
		return []net.IPAddr{{IP: net.ParseIP("93.184.216.34")}}, nil // public, so only the userinfo gate can stop it
	})
	tool := ResearchFetchTool(WithResearchResolver(res), WithResearchDialContext(dial))
	if _, err := tool.Exec(context.Background(), toolbroker.ExecEnv{}, map[string]any{"url": "https://user:pass@example.com/"}); err == nil {
		t.Fatal("research fetch of a URL with embedded credentials = nil, want denied")
	}
	if len(dialed) != 0 {
		t.Fatalf("credentialed URL dialed %v, want NO dial (denied at the static gate)", dialed)
	}
}

// TestResearchDeniesCredentialRedirect pins that a REDIRECT into a credentialed URL is denied on the
// redirected hop — not just the initial URL. Without the userinfo check inside the shared egress gate,
// CheckRedirect would follow https://user:pass@host and net/http would put Authorization: Basic on the
// wire. The redirect is denied and the server never sees an Authorization header.
func TestResearchDeniesCredentialRedirect(t *testing.T) {
	var sawAuth bool
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "" {
			sawAuth = true
		}
		if r.URL.Path == "/" {
			w.Header().Set("Location", "https://user:pass@example.com/authed")
			w.WriteHeader(http.StatusFound)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write([]byte("<title>authed</title>ok"))
	}))
	defer srv.Close()

	pubResolver := researchResolver(func(_ context.Context, _ string) ([]net.IPAddr, error) {
		return []net.IPAddr{{IP: net.ParseIP("93.184.216.34")}}, nil
	})
	dial, _ := dialRecorder(srv.Listener.Addr().String())
	tool := ResearchFetchTool(WithResearchResolver(pubResolver), WithResearchDialContext(dial), WithResearchTLSConfig(trustServer(srv)))

	if _, err := tool.Exec(context.Background(), toolbroker.ExecEnv{}, map[string]any{"url": "https://example.com/"}); err == nil {
		t.Fatal("research fetch redirecting to a credentialed URL = nil, want denied")
	}
	if sawAuth {
		t.Fatal("the redirect to https://user:pass@host put Authorization: Basic on the wire")
	}
}

func trustServer(srv *httptest.Server) *tls.Config {
	pool := x509.NewCertPool()
	pool.AddCert(srv.Certificate())
	return &tls.Config{RootCAs: pool}
}

func sha256Sum(b []byte) []byte {
	sum := sha256.Sum256(b)
	return sum[:]
}
