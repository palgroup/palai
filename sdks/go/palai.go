// Package palai is the server-side Go SDK for the Palai control plane. It speaks the dated
// contract (API-Version), authenticates with a Bearer API key, retries idempotently with a stable
// Idempotency-Key, decodes forward-compatibly (unknown fields preserved, spec API-009), and maps
// non-2xx responses to typed RFC 9457 errors.
//
// Server-side by design (positive stance, plan §2): this SDK holds an API key, so it is meant for
// trusted server processes — never a browser. There is no browser entrypoint and no browser-direct
// token; that separation is the E13 server-relay decision, kept deliberately. It is stdlib-only
// (net/http + bufio SSE + encoding/json) with no third-party dependency.
package palai

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
)

// APIVersion is the dated contract this SDK speaks; it rides every request (spec §20.13).
const APIVersion = "2026-07-16"

// Client is the transport client: Bearer auth, the dated API version, idempotent retry, and typed
// RFC 9457 errors. Resource groups hang off it.
type Client struct {
	baseURL       string
	apiKey        string
	httpClient    *http.Client
	maxRetries    int
	timeout       time.Duration
	backoffBaseMs int
	backoffMaxMs  int

	Responses   *Responses
	ModelRoutes *ModelRoutes
}

// Option configures a Client (functional options — the idiomatic Go equivalent of the TS options
// object). Unset options fall back to the PALAI_* environment, then to defaults.
type Option func(*Client)

// WithBaseURL overrides the API base URL (default $PALAI_BASE_URL or http://localhost:8080).
func WithBaseURL(u string) Option { return func(c *Client) { c.baseURL = strings.TrimRight(u, "/") } }

// WithAPIKey sets the Bearer API key (default $PALAI_API_KEY). Keep it server-side.
func WithAPIKey(k string) Option { return func(c *Client) { c.apiKey = k } }

// WithHTTPClient injects a custom *http.Client (for tests, proxies, or custom transports).
func WithHTTPClient(h *http.Client) Option { return func(c *Client) { c.httpClient = h } }

// WithMaxRetries sets the max retry attempts after the first try for a retryable failure (default 2).
func WithMaxRetries(n int) Option { return func(c *Client) { c.maxRetries = n } }

// WithTimeout sets the total per-request deadline including retries and backoff (default 60s). A
// caller's context deadline still applies and wins if shorter.
func WithTimeout(d time.Duration) Option { return func(c *Client) { c.timeout = d } }

// WithBackoff tunes the full-jitter retry backoff bounds in milliseconds.
func WithBackoff(baseMs, maxMs int) Option {
	return func(c *Client) { c.backoffBaseMs, c.backoffMaxMs = baseMs, maxMs }
}

// New builds a Client. An API key is required — pass WithAPIKey or set PALAI_API_KEY — otherwise
// New returns an error rather than silently issuing unauthenticated requests.
func New(opts ...Option) (*Client, error) {
	c := &Client{
		baseURL:       strings.TrimRight(envOr("PALAI_BASE_URL", "http://localhost:8080"), "/"),
		apiKey:        os.Getenv("PALAI_API_KEY"),
		httpClient:    &http.Client{},
		maxRetries:    2,
		timeout:       60 * time.Second,
		backoffBaseMs: 200,
		backoffMaxMs:  10_000,
	}
	for _, o := range opts {
		o(c)
	}
	if c.apiKey == "" {
		return nil, &ConnectionError{Message: "palai: an API key is required — pass WithAPIKey or set PALAI_API_KEY (keep it server-side)"}
	}
	c.Responses = &Responses{client: c}
	c.ModelRoutes = &ModelRoutes{client: c}
	return c, nil
}

// String / GoString redact the API key so a %v/%+v/%#v of a Client (or anything embedding it) never
// leaks the secret in a log line — Go structs print their fields by default, so this is explicit.
func (c *Client) String() string {
	return "palai.Client{baseURL:" + c.baseURL + ", apiKey:REDACTED}"
}
func (c *Client) GoString() string { return c.String() }

// requestOptions are the per-call transport controls a resource method threads through.
type requestOptions struct {
	body           any
	idempotencyKey string
	maxRetries     *int
	timeout        *time.Duration
	accept         string
	// idempotent marks a NETWORK-level failure (no HTTP status seen) safe to re-send. It defaults
	// to true for safe methods and any request carrying an Idempotency-Key; a non-idempotent create
	// leaves it false so a connection torn after the server committed cannot double-provision.
	idempotent bool
}

// doJSON performs a request with idempotent retry and decodes a 2xx JSON body into out (out may be
// nil to discard). A non-2xx becomes a typed *APIError; a torn connection becomes *ConnectionError.
func (c *Client) doJSON(ctx context.Context, method, path string, opt requestOptions, out any) error {
	var bodyBytes []byte
	if opt.body != nil {
		b, err := json.Marshal(opt.body)
		if err != nil {
			return err
		}
		bodyBytes = b
	}
	idempotent := isSafeMethod(method) || opt.idempotencyKey != "" || opt.idempotent

	maxRetries := c.maxRetries
	if opt.maxRetries != nil {
		maxRetries = *opt.maxRetries
	}
	timeout := c.timeout
	if opt.timeout != nil {
		timeout = *opt.timeout
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	accept := opt.accept
	if accept == "" {
		accept = "application/json"
	}

	attempt := 0
	for {
		req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, bodyReader(bodyBytes))
		if err != nil {
			return err
		}
		req.Header.Set("Authorization", "Bearer "+c.apiKey)
		req.Header.Set("API-Version", APIVersion)
		req.Header.Set("Accept", accept)
		if opt.idempotencyKey != "" {
			req.Header.Set("Idempotency-Key", opt.idempotencyKey)
		}
		if bodyBytes != nil {
			req.Header.Set("Content-Type", "application/json")
		}

		resp, err := c.httpClient.Do(req)
		if err != nil {
			// Network-level failure: only re-send an idempotent request, so a non-idempotent create
			// fails closed instead of risking a double-commit. A context cancel is terminal.
			if ctx.Err() != nil {
				return &ConnectionError{Message: method + " " + path + " was canceled", Cause: ctx.Err()}
			}
			if !idempotent || attempt >= maxRetries {
				return &ConnectionError{Message: method + " " + path + " failed to reach the server", Cause: err}
			}
			if serr := sleep(ctx, fullJitterBackoff(attempt, c.backoffBaseMs, c.backoffMaxMs)); serr != nil {
				return &ConnectionError{Message: method + " " + path + " was canceled", Cause: serr}
			}
			attempt++
			continue
		}

		if resp.StatusCode/100 == 2 {
			defer resp.Body.Close()
			return decodeJSON(resp, out)
		}

		// A retryable status (408/429/5xx) is retried within budget; the SAME Idempotency-Key rides
		// every attempt, so a retried create settles exactly one response.
		if isRetryableStatus(resp.StatusCode) && attempt < maxRetries {
			wait := retryAfter(resp)
			if wait < 0 {
				wait = fullJitterBackoff(attempt, c.backoffBaseMs, c.backoffMaxMs)
			}
			io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<16))
			resp.Body.Close()
			if serr := sleep(ctx, wait); serr != nil {
				return &ConnectionError{Message: method + " " + path + " was canceled", Cause: serr}
			}
			attempt++
			continue
		}
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		resp.Body.Close()
		return errorForResponse(resp.StatusCode, string(body), resp.Header.Get("Request-Id"))
	}
}

func decodeJSON(resp *http.Response, out any) error {
	if out == nil {
		io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
		return nil
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return &ConnectionError{Message: "reading response body", Cause: err}
	}
	if len(bytes.TrimSpace(body)) == 0 {
		return nil
	}
	return json.Unmarshal(body, out)
}

func bodyReader(b []byte) io.Reader {
	if b == nil {
		return nil
	}
	return bytes.NewReader(b)
}

// isSafeMethod reports whether a method has no side effect, so re-sending it after a network
// failure cannot double-apply anything (RFC 9110 §9.2.1).
func isSafeMethod(method string) bool {
	return method == http.MethodGet || method == http.MethodHead
}

// retryAfter honors a server Retry-After header (delta-seconds); returns -1 when absent/invalid.
func retryAfter(resp *http.Response) time.Duration {
	h := resp.Header.Get("Retry-After")
	if h == "" {
		return -1
	}
	secs, err := strconv.Atoi(h)
	if err != nil || secs < 0 {
		return -1
	}
	return time.Duration(secs) * time.Second
}

// newIdempotencyKey mints a fresh, collision-resistant key for one logical create (spec §20.9).
func newIdempotencyKey() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "idem_" + strconv.FormatInt(time.Now().UnixNano(), 36)
	}
	return "idem_" + hex.EncodeToString(b[:])
}

// escapePathSegment URL-encodes an id for a single path segment (encodes "/" as %2F). It agrees with
// the TS SDK's encodeURIComponent for real ids ([A-Za-z0-9_-] plus "/"); the two differ only on
// sub-delims (e.g. url.PathEscape leaves "!$&'()*+,;=" raw), which no Palai id contains.
func escapePathSegment(s string) string { return url.PathEscape(s) }

// queryEscape URL-encodes a list query value (application/x-www-form-urlencoded, matching the TS
// SDK's URLSearchParams).
func queryEscape(s string) string { return url.QueryEscape(s) }

func envOr(name, fallback string) string {
	if v := strings.TrimSpace(os.Getenv(name)); v != "" {
		return v
	}
	return fallback
}
