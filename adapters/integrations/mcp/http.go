package mcp

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"sync/atomic"
	"time"

	"github.com/palgroup/palai/packages/egress"
)

// maxHTTPBody bounds the response body read from an untrusted MCP server over HTTP.
const maxHTTPBody = 4 * 1024 * 1024

// errHTTPRedirectDenied is returned from CheckRedirect so a 3xx is never followed (SSRF: a redirect could
// point at an internal address the static+pinned vet already cleared for the original host).
var errHTTPRedirectDenied = fmt.Errorf("%w: redirect not followed", ErrProtocol)

// HTTPOptions configures a Streamable HTTP transport. Bearer is the connection's OWN upstream credential,
// resolved from its secret_ref at REQUEST time by the manager — it is the ONLY Authorization the transport
// ever sends, and it is never the platform's token (no confused-deputy). AllowPrivate opens loopback for
// the local test harness ONLY (the webhook-sender discipline); production leaves it false.
type HTTPOptions struct {
	URL          string
	Bearer       string
	AllowPrivate bool
	Resolver     egress.Resolver
	Dial         func(ctx context.Context, network, addr string) (net.Conn, error)
	TLSConfig    *tls.Config
	Timeout      time.Duration
}

// httpTransport speaks JSON-RPC over MCP Streamable HTTP: a POST per message, Accept: json + SSE, the
// Mcp-Session-Id carried across requests, redirects denied, and the connection pinned to a vetted resolved
// IP so a rebind cannot swap the target between vet and connect (egress.PinnedDialer, TOCTOU closed).
type httpTransport struct {
	client    *http.Client
	url       string
	bearer    string
	sessionID atomic.Pointer[string]
	nextID    atomic.Int64
}

// NewHTTPTransport builds a Streamable HTTP transport after statically vetting the URL (https-only, no
// embedded credentials, literal-IP vetted). The authoritative connect-time gate is the pinned dialer; a
// name that resolves internal is denied on the dial. A denied URL is a terminal error.
func NewHTTPTransport(opts HTTPOptions) (Transport, error) {
	if err := egress.VetURL(opts.URL, opts.AllowPrivate); err != nil {
		return nil, err
	}
	timeout := opts.Timeout
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	dial := opts.Dial
	if dial == nil {
		dial = (&net.Dialer{Timeout: 10 * time.Second}).DialContext
	}
	transport := &http.Transport{
		DisableKeepAlives:   true,
		TLSClientConfig:     opts.TLSConfig,
		TLSHandshakeTimeout: timeout,
		DialContext:         egress.PinnedDialer(opts.Resolver, opts.AllowPrivate, dial),
	}
	client := &http.Client{
		Timeout:   timeout,
		Transport: transport,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return errHTTPRedirectDenied
		},
	}
	return &httpTransport{client: client, url: opts.URL, bearer: opts.Bearer}, nil
}

// VetHTTPURL is the fail-fast registration/discovery gate: the static check PLUS a DNS resolution whose
// every answer is vetted (a name that already points internal is rejected early). Resolution failure is
// permissive — the pinned dialer is authoritative. It delegates to egress.VetResolved so there is one copy
// of the resolve→vet idiom.
func VetHTTPURL(ctx context.Context, resolver egress.Resolver, rawURL string, allowPrivate bool) error {
	return egress.VetResolved(ctx, resolver, rawURL, allowPrivate)
}

// Call POSTs a JSON-RPC request and returns its result, routing any progress notifications (over an SSE
// response) to onProgress. It captures the Mcp-Session-Id from the response for subsequent requests.
func (t *httpTransport) Call(ctx context.Context, method string, params any, onProgress func(Progress)) (json.RawMessage, error) {
	resp, err := t.post(ctx, map[string]any{"jsonrpc": "2.0", "id": t.nextID.Add(1), "method": method, "params": params})
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if sid := resp.Header.Get("Mcp-Session-Id"); sid != "" {
		t.sessionID.Store(&sid)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("%w: http status %d", ErrProtocol, resp.StatusCode)
	}
	if strings.HasPrefix(resp.Header.Get("Content-Type"), "text/event-stream") {
		return t.readSSE(resp.Body, onProgress)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxHTTPBody))
	if err != nil {
		return nil, fmt.Errorf("%w: read body: %v", ErrProtocol, err)
	}
	return resultOf(body)
}

// Notify POSTs a fire-and-forget notification (no id). A 2xx with no result is expected; the body is drained.
func (t *httpTransport) Notify(ctx context.Context, method string, params any) error {
	resp, err := t.post(ctx, map[string]any{"jsonrpc": "2.0", "method": method, "params": params})
	if err != nil {
		return err
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxHTTPBody))
	_ = resp.Body.Close()
	return nil
}

// Close is a no-op for HTTP (no persistent process); keep-alives are already disabled per request.
func (t *httpTransport) Close(ctx context.Context) error { return nil }

// post issues one JSON-RPC POST with the MCP headers. The bearer is the connection's own credential (or
// none); the platform token is NEVER attached (confused-deputy defence, TOL-009).
func (t *httpTransport) post(ctx context.Context, msg map[string]any) (*http.Response, error) {
	body, err := json.Marshal(msg)
	if err != nil {
		return nil, fmt.Errorf("%w: marshal request: %v", ErrProtocol, err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, t.url, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	if t.bearer != "" {
		req.Header.Set("Authorization", "Bearer "+t.bearer)
	}
	if sid := t.sessionID.Load(); sid != nil {
		req.Header.Set("Mcp-Session-Id", *sid)
	}
	resp, err := t.client.Do(req)
	if err != nil {
		// Preserve the underlying error chain (a %w, not %v) so an egress.ErrDenied at connect — a
		// rebinding name the pinned dialer refused — stays matchable as a terminal policy denial, not
		// masked behind a generic protocol error.
		return nil, fmt.Errorf("mcp: http request: %w", err)
	}
	return resp, nil
}

// readSSE parses a text/event-stream body: each `data:` line is one JSON-RPC message. Progress notifications
// route to onProgress; the first response message (a result or error) terminates the read.
func (t *httpTransport) readSSE(r io.Reader, onProgress func(Progress)) (json.RawMessage, error) {
	sc := bufio.NewScanner(io.LimitReader(r, maxHTTPBody))
	sc.Buffer(make([]byte, 0, 64*1024), maxStdioMessage)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		data, ok := strings.CutPrefix(line, "data:")
		if !ok {
			continue // SSE framing lines (event:, id:, blank) are skipped
		}
		data = strings.TrimSpace(data)
		var msg rpcMessage
		if json.Unmarshal([]byte(data), &msg) != nil {
			continue
		}
		if msg.Method == "notifications/progress" && onProgress != nil {
			if p, ok := decodeProgress(msg.Params); ok {
				onProgress(p)
			}
			continue
		}
		if len(msg.ID) == 0 {
			continue
		}
		if msg.Error != nil {
			return nil, fmt.Errorf("%w: %s (code %d)", ErrProtocol, msg.Error.Message, msg.Error.Code)
		}
		return msg.Result, nil
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("%w: read sse: %v", ErrProtocol, err)
	}
	return nil, fmt.Errorf("%w: sse stream ended without a response", ErrProtocol)
}

// resultOf extracts the result from a single JSON-RPC response body.
func resultOf(body []byte) (json.RawMessage, error) {
	var msg rpcMessage
	if err := json.Unmarshal(body, &msg); err != nil {
		return nil, fmt.Errorf("%w: parse response: %v", ErrProtocol, err)
	}
	if msg.Error != nil {
		return nil, fmt.Errorf("%w: %s (code %d)", ErrProtocol, msg.Error.Message, msg.Error.Code)
	}
	return msg.Result, nil
}
