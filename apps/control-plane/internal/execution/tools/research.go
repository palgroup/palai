package tools

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"errors"
	"fmt"
	"html"
	"io"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/palgroup/palai/packages/egress"
	toolbroker "github.com/palgroup/palai/packages/tool-broker"
)

// researchToolName is the honest name: this tool FETCHES a given URL and cites it — it does NOT search
// (a search/crawl backend is E17). The model supplies the URL; there is no query surface.
const researchToolName = "palai.research.fetch"

const (
	// maxResearchFetchBytes caps the raw body read from the network. ponytail: fixed 4 MiB — a larger
	// page is truncated (io.LimitReader) and the truncation is flagged honestly in the result +
	// content_hash; the full-fidelity upgrade path is streaming the body straight to the artifact store.
	maxResearchFetchBytes = 4 << 20
	// maxResearchExcerptBytes bounds the extracted text inlined into model context (the file.go:16
	// bounded-read pattern); the full body always goes to the artifact when a store is wired.
	maxResearchExcerptBytes = 64 << 10
	researchTimeout         = 10 * time.Second
	researchMaxRedirects    = 5
	researchUserAgent       = "palai-research/1"
	researchLogicalType     = "research_fetch"
)

// researchOptions carries the test-only network seams (a production build passes none): an injected DNS
// resolver, a low-level dialer the pinned dialer hands vetted IPs to, and a TLS config trusting a test
// server. Production uses net.DefaultResolver, a real net.Dialer, and the system roots.
type researchOptions struct {
	resolver  egress.Resolver
	dial      func(ctx context.Context, network, addr string) (net.Conn, error)
	tlsConfig *tls.Config
}

// ResearchOption configures the research fetch tool (test seams only).
type ResearchOption func(*researchOptions)

// WithResearchResolver injects the DNS resolver the pinned dialer re-resolves through.
func WithResearchResolver(r egress.Resolver) ResearchOption {
	return func(o *researchOptions) { o.resolver = r }
}

// WithResearchDialContext injects the low-level dialer; the pinned dialer always hands it a vetted
// resolved IP, never a hostname.
func WithResearchDialContext(d func(ctx context.Context, network, addr string) (net.Conn, error)) ResearchOption {
	return func(o *researchOptions) { o.dial = d }
}

// WithResearchTLSConfig injects a TLS config (a test trusts an httptest server's self-signed cert;
// production leaves it nil for the system roots — TLS is always required).
func WithResearchTLSConfig(c *tls.Config) ResearchOption {
	return func(o *researchOptions) { o.tlsConfig = c }
}

// ResearchFetchTool is the built-in web-research fetch tool (EXT-004, TOL-014/015). It fetches ONE
// model-supplied URL over a hardened egress path — https-only, GET-only, no credential of any kind on
// the wire, the destination re-resolved and vetted at connect time and re-vetted on every redirect hop
// so a private/loopback/link-local/metadata target is never reachable (a model-supplied URL is a fully
// untrusted SSRF primitive) — extracts a bounded text excerpt, and returns a citation (final URL, title,
// retrieval time, content hash) plus the full body written to the artifact store when one is wired.
func ResearchFetchTool(opts ...ResearchOption) toolbroker.Tool {
	o := &researchOptions{}
	for _, opt := range opts {
		opt(o)
	}
	return toolbroker.Tool{
		Name: researchToolName,
		// GET is idempotent (RFC 9110), so a kill-after-execute row re-executes safely; the citation's
		// content_hash pins what THIS execution saw, since the network response is not deterministic.
		ReplayClass: toolbroker.ClassIdempotent,
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				// url is the only untrusted input; method/headers/body are NOT model-supplied (schema
				// closed), so header-injection has no surface.
				"url":       map[string]any{"type": "string"},
				"max_bytes": map[string]any{"type": "number"},
			},
			"required":             []any{"url"},
			"additionalProperties": false,
		},
		OutputSchema: map[string]any{"type": "object"},
		Exec:         o.exec,
	}
}

func (o *researchOptions) exec(ctx context.Context, env toolbroker.ExecEnv, args map[string]any) (map[string]any, error) {
	rawURL, _ := args["url"].(string)
	fetchCap := maxResearchFetchBytes
	if mb, ok := args["max_bytes"].(float64); ok && mb > 0 && int(mb) < fetchCap {
		fetchCap = int(mb)
	}

	// Static gate BEFORE any dial: https-only, and a literal-IP host vetted (research NEVER allows
	// private — allowPrivate is hard-false here, stricter than the webhook self-host downgrade).
	if err := egress.VetURL(rawURL, false); err != nil {
		return nil, fmt.Errorf("research: %w", err)
	}

	resolver := o.resolver
	if resolver == nil {
		resolver = net.DefaultResolver
	}
	dial := o.dial
	if dial == nil {
		dial = (&net.Dialer{Timeout: researchTimeout}).DialContext
	}

	client := &http.Client{
		Timeout: researchTimeout,
		Transport: &http.Transport{
			DisableKeepAlives:   true, // force a fresh dial (and re-resolve+re-vet) for the initial request and every redirect hop
			TLSClientConfig:     o.tlsConfig,
			TLSHandshakeTimeout: researchTimeout,
			DialContext:         egress.PinnedDialer(resolver, false, dial),
		},
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= researchMaxRedirects {
				return fmt.Errorf("research: too many redirects (> %d)", researchMaxRedirects)
			}
			// Re-vet the Location BEFORE following it, so a redirect cannot bounce the fetch into an
			// internal target (the pinned dialer re-vets the resolved IP too — this closes the literal-IP
			// and https-downgrade redirect vectors at the URL layer).
			return egress.VetURL(req.URL.String(), false)
		},
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, fmt.Errorf("research: build request: %w", err)
	}
	// The ONLY headers: an honest UA and an Accept. No cookie, no Authorization, no platform credential.
	req.Header.Set("User-Agent", researchUserAgent)
	req.Header.Set("Accept", "text/html, text/plain, application/json, application/xml;q=0.9, */*;q=0.1")

	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("research: fetch: %w", unwrapURLError(err))
	}
	defer resp.Body.Close()

	// Read at most fetchCap+1 so an over-cap body is detected, then truncate to the cap.
	raw, err := io.ReadAll(io.LimitReader(resp.Body, int64(fetchCap)+1))
	if err != nil {
		return nil, fmt.Errorf("research: read body: %w", err)
	}
	fetchTruncated := len(raw) > fetchCap
	if fetchTruncated {
		raw = raw[:fetchCap]
	}

	contentType := resp.Header.Get("Content-Type")
	if !supportedResearchContentType(contentType) {
		return nil, fmt.Errorf("research: unsupported content type %q (binary/PDF is not fetched in v1)", contentType)
	}

	title, text := extractResearchText(contentType, raw)
	excerpt := text
	excerptTruncated := false
	if len(excerpt) > maxResearchExcerptBytes {
		excerpt = truncateUTF8(excerpt, maxResearchExcerptBytes)
		excerptTruncated = true
	}

	sum := sha256.Sum256(raw)
	contentHash := "sha256:" + hex.EncodeToString(sum[:])
	finalURL := resp.Request.URL.String()
	retrievedAt := time.Now().UTC().Format(time.RFC3339)

	// The full (fetch-capped) body goes to the artifact store when one is wired, so the content_hash the
	// citation carries is exactly the bytes an auditor can re-read. No store → excerpt-only, artifact_id
	// empty; the tool still returns cleanly (a workspace-less run is not a failure).
	artifactID := ""
	if env.Artifacts != nil {
		id, err := env.Artifacts.WriteArtifact(ctx, env.Scope.Org, env.Scope.Project, env.Scope.RunID, raw,
			researchMediaType(contentType), researchLogicalType,
			map[string]any{"tool": researchToolName, "url": finalURL, "retrieved_at": retrievedAt})
		if err != nil {
			return nil, fmt.Errorf("research: persist artifact: %w", err)
		}
		artifactID = id
	}

	return map[string]any{
		"url":          finalURL,
		"status":       resp.StatusCode,
		"content_type": contentType,
		"excerpt":      excerpt,
		"truncated":    fetchTruncated || excerptTruncated,
		"artifact_id":  artifactID,
		"citations": []any{map[string]any{
			"url":          finalURL,
			"title":        title,
			"retrieved_at": retrievedAt,
			"content_hash": contentHash,
		}},
	}, nil
}

// researchMediaType is the upstream media type (before any charset param) recorded on the artifact.
func researchMediaType(contentType string) string {
	if i := strings.IndexByte(contentType, ';'); i >= 0 {
		contentType = contentType[:i]
	}
	mt := strings.ToLower(strings.TrimSpace(contentType))
	if mt == "" {
		return "application/octet-stream"
	}
	return mt
}

// supportedResearchContentType allows text/*, application/json, application/xml, and their +json/+xml
// suffixes; anything else (a binary/PDF) is rejected in v1 rather than inlined or mis-extracted.
func supportedResearchContentType(contentType string) bool {
	mt := researchMediaType(contentType)
	switch {
	case strings.HasPrefix(mt, "text/"):
		return true
	case mt == "application/json" || mt == "application/xml":
		return true
	case strings.HasSuffix(mt, "+json") || strings.HasSuffix(mt, "+xml"):
		return true
	}
	return false
}

// extractResearchText returns (title, text): an HTML page is title-extracted and tag-stripped to plain
// text; any other supported type is returned as raw text with no title. ponytail: a regex tag-strip, not
// readability-grade extraction (a readability library is deliberately NOT taken) — the upgrade path is a
// real DOM extractor if excerpt quality ever matters.
func extractResearchText(contentType string, raw []byte) (title, text string) {
	s := string(raw)
	if researchMediaType(contentType) == "text/html" {
		return htmlTitle(s), htmlToText(s)
	}
	return "", strings.TrimSpace(s)
}

var (
	researchTitleRe       = regexp.MustCompile(`(?is)<title[^>]*>(.*?)</title>`)
	researchScriptStyleRe = regexp.MustCompile(`(?is)<(script|style)\b[^>]*>.*?</(script|style)\s*>`)
	researchTagRe         = regexp.MustCompile(`(?s)<[^>]*>`)
	researchWSRe          = regexp.MustCompile(`\s+`)
)

func htmlTitle(s string) string {
	if m := researchTitleRe.FindStringSubmatch(s); m != nil {
		return strings.TrimSpace(researchWSRe.ReplaceAllString(html.UnescapeString(m[1]), " "))
	}
	return ""
}

func htmlToText(s string) string {
	s = researchScriptStyleRe.ReplaceAllString(s, " ")
	s = researchTagRe.ReplaceAllString(s, " ")
	s = html.UnescapeString(s)
	s = researchWSRe.ReplaceAllString(s, " ")
	return strings.TrimSpace(s)
}

// truncateUTF8 cuts s to at most n bytes without splitting a multi-byte rune.
func truncateUTF8(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n]
}

// unwrapURLError strips the *url.Error envelope so a fetch error does not echo the full destination.
func unwrapURLError(err error) error {
	var ue *url.Error
	if errors.As(err, &ue) {
		return ue.Err
	}
	return err
}
