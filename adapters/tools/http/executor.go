package remotehttp

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"time"

	"github.com/palgroup/palai/adapters/integrations/webhook"
	"github.com/palgroup/palai/packages/contracts"
	"github.com/palgroup/palai/packages/egress"
)

// Protocol is the tool-http.v1 envelope protocol constant (the schema's `protocol` const field).
const Protocol = "tool-http.v1"

// maxResultBytes bounds the result body read from a remote tool server (a NEW trust boundary): a
// misbehaving server cannot stream an unbounded body. The result schema-validates in the broker after.
const maxResultBytes = 1 << 20

// defaultTimeout applies when a tool revision declares no timeout_ms.
const defaultTimeout = 30 * time.Second

// The typed executor outcomes the broker maps to a tool error (spec §28.24). Each is deliberately
// coarse — a customer server must not become a config oracle through fine-grained error text.
var (
	// ErrRemoteRejected: the server refused our signed request (401/403) — a secret mismatch or a replayed
	// timestamp on their side. It is retryable (a fresh attempt re-signs).
	ErrRemoteRejected = errors.New("remote_tool_rejected")
	// ErrRequestHashMismatch: the server saw the SAME Idempotency-Key with a DIFFERENT request_hash (409)
	// — a duplicate that diverged in content. Terminal: replaying it would answer a different call.
	ErrRequestHashMismatch = errors.New("remote_tool_request_hash_mismatch")
	// ErrRemoteProblem: the tool returned an RFC 9457 problem instead of a result.
	ErrRemoteProblem = errors.New("remote_tool_problem")
	// ErrRemoteTimeout: the async operation did not resolve before its deadline. The durable executing
	// marker carries the tool_call to uncertain; the RemoteToolProber reconciles any late result.
	ErrRemoteTimeout = errors.New("remote_tool_timeout")
	// ErrRemoteProtocol: the server answered an unexpected status or an unparseable body.
	ErrRemoteProtocol = errors.New("remote_tool_protocol")
)

// ledger is the narrow durable-operation seam the executor depends on (the callback endpoint + prober
// depend on their own). *Operations satisfies it; a test fakes it in-memory.
type ledger interface {
	Open(ctx context.Context, in OpenOperation) (opened bool, err error)
	Poll(ctx context.Context, operationID string) (state string, result []byte, err error)
	CompleteSync(ctx context.Context, operationID string, result []byte, resultHash string) error
	Timeout(ctx context.Context, operationID string) error
	ProberRead(ctx context.Context, toolCallID string) (state string, result []byte, found bool, err error)
}

// Executor signs and dispatches a tool-http.v1 invoke to a remote tool server, riding the shared egress
// SSRF layer and the webhook signer. It is composed once and invoked per tool call by the broker binder.
type Executor struct {
	ledger       ledger
	resolver     egress.Resolver
	dial         func(ctx context.Context, network, addr string) (net.Conn, error)
	tlsConfig    *tls.Config
	now          func() time.Time
	pollEvery    time.Duration
	callbackBase string // this CP's public base URL for the async callback (empty => sync-only)
}

// Option configures an Executor (tests inject a resolver/dialer/TLS/clock; production takes defaults).
type Option func(*Executor)

// WithResolver injects the DNS resolver the pinned dialer re-resolves through (default net.DefaultResolver).
func WithResolver(r egress.Resolver) Option { return func(e *Executor) { e.resolver = r } }

// WithDialContext injects the low-level dialer (default net.Dialer); the executor always hands it a
// vetted resolved IP, never a hostname.
func WithDialContext(d func(ctx context.Context, network, addr string) (net.Conn, error)) Option {
	return func(e *Executor) { e.dial = d }
}

// WithTLSConfig injects a TLS config (a test trusts an httptest server's cert; production uses system roots).
func WithTLSConfig(c *tls.Config) Option { return func(e *Executor) { e.tlsConfig = c } }

// WithClock injects the clock (deterministic tests).
func WithClock(now func() time.Time) Option { return func(e *Executor) { e.now = now } }

// WithPollInterval injects the async poll cadence (default 500ms).
func WithPollInterval(d time.Duration) Option { return func(e *Executor) { e.pollEvery = d } }

// WithCallbackBaseURL sets this CP's public base URL the async result callback is posted to (e.g.
// https://cp.example.com). Empty leaves the callback URL empty so only a synchronous 200 tool works.
func WithCallbackBaseURL(base string) Option { return func(e *Executor) { e.callbackBase = base } }

// NewExecutor builds an executor over the durable operation ledger with production defaults.
func NewExecutor(l ledger, opts ...Option) *Executor {
	e := &Executor{
		ledger:    l,
		resolver:  net.DefaultResolver,
		dial:      (&net.Dialer{Timeout: 10 * time.Second}).DialContext,
		now:       time.Now,
		pollEvery: 500 * time.Millisecond,
	}
	for _, opt := range opts {
		opt(e)
	}
	return e
}

// Invocation is one remote tool call the broker binder hands the executor: the destination + secret
// (resolved fresh per call, never held), the tool_call identity the invoke signs + keys, and the async
// callback base the server posts a 202 result to.
type Invocation struct {
	URL             string
	AllowPrivate    bool // self-host egress downgrade (http + private ranges); false for a public server
	Secret          []byte
	ToolCallID      string
	ToolRevision    string
	RunID           string
	AttemptID       string
	RequestHash     string
	Arguments       map[string]any
	Org             string
	Project         string
	SecretRef       string
	Fence        uint64
	TimeoutMS    int
}

// Invoke signs and dispatches one tool-http.v1 invoke and returns the tool result (the broker validates
// it against the output schema as UNTRUSTED content). It opens a durable operation row BEFORE the POST so
// a fast callback can never race ahead of a row to persist into; a 200 returns the result inline, a 202
// polls the operation until the signed callback resolves it or the deadline lapses.
func (e *Executor) Invoke(ctx context.Context, in Invocation) (map[string]any, error) {
	if len(in.Secret) == 0 {
		return nil, fmt.Errorf("%w: no signing secret resolved", ErrRemoteRejected)
	}
	timeout := time.Duration(in.TimeoutMS) * time.Millisecond
	if timeout <= 0 {
		timeout = defaultTimeout
	}
	start := e.now()
	deadline := start.Add(timeout)

	operationID, err := mintID("rop_")
	if err != nil {
		return nil, err
	}
	token, err := mintToken()
	if err != nil {
		return nil, err
	}
	tokenHash := sha256Hex(token)

	callback := map[string]any{"url": "", "token": string(token)}
	if e.callbackBase != "" {
		callback["url"] = e.callbackBase + "/v1/tool-callbacks/" + operationID
	}
	envelope := contracts.ToolHTTPInvoke{
		Protocol:     Protocol,
		ToolCallID:   in.ToolCallID,
		ToolRevision: in.ToolRevision,
		RunID:        in.RunID,
		AttemptID:    in.AttemptID,
		RequestHash:  in.RequestHash,
		Deadline:     deadline.UTC().Format(time.RFC3339Nano),
		Arguments:    in.Arguments,
		Callback:     callback,
	}
	rawBody, err := json.Marshal(envelope)
	if err != nil {
		return nil, fmt.Errorf("marshal invoke envelope: %w", err)
	}

	// Open the durable pending row BEFORE the POST (a callback can never beat it). A duplicate live invoke
	// (opened=false) does not re-POST — it polls the existing row (a killed pure re-drive, spec §26.7).
	// ponytail: DB-poll the operation ~pollEvery; a LISTEN/NOTIFY wake is a later upgrade if latency bites.
	opened, err := e.ledger.Open(ctx, OpenOperation{
		OperationID: operationID, Org: in.Org, Project: in.Project, ToolCallID: in.ToolCallID,
		SecretRef: in.SecretRef, TokenHash: tokenHash, Deadline: deadline, Fence: in.Fence,
	})
	if err != nil {
		return nil, err
	}
	if !opened {
		return e.awaitExisting(ctx, in.ToolCallID, deadline)
	}

	headers := e.signHeaders(in, rawBody)
	status, respBody, err := e.post(ctx, in.URL, in.AllowPrivate, headers, rawBody, timeout)
	if err != nil {
		return nil, err
	}
	switch status {
	case http.StatusOK:
		result, perr := decodeResult(respBody)
		if perr != nil {
			return nil, perr
		}
		hash := resultHash(result)
		if cerr := e.ledger.CompleteSync(ctx, operationID, mustJSON(result), hash); cerr != nil {
			return nil, cerr
		}
		return result, nil
	case http.StatusAccepted:
		return e.await(ctx, operationID, deadline)
	case http.StatusConflict:
		return nil, fmt.Errorf("%w: server saw a diverged retry for %s", ErrRequestHashMismatch, in.ToolCallID)
	case http.StatusUnauthorized, http.StatusForbidden:
		return nil, fmt.Errorf("%w: server refused the signed invoke (status %d)", ErrRemoteRejected, status)
	default:
		return nil, fmt.Errorf("%w: unexpected status %d", ErrRemoteProtocol, status)
	}
}

// signHeaders builds the standard-webhooks signature (reusing the webhook signer — no new MAC) over the
// raw body, plus the Idempotency-Key = tool_call_id (a duplicate retry settles ONE server execution).
func (e *Executor) signHeaders(in Invocation, rawBody []byte) map[string]string {
	signer := webhook.NewSigner(in.Secret)
	headers := signer.Headers(in.ToolCallID, e.now(), 1, rawBody)
	headers["Idempotency-Key"] = in.ToolCallID
	headers["Content-Type"] = "application/json"
	return headers
}

// await polls the operation until the signed callback resolves it or the deadline lapses. Only a
// 'completed' state returns the result under the live fence; a 'late_result'/'timed_out' means the
// callback arrived too late (or none did) — the executor times out and the prober reconciles.
func (e *Executor) await(ctx context.Context, operationID string, deadline time.Time) (map[string]any, error) {
	for {
		state, result, err := e.ledger.Poll(ctx, operationID)
		if err != nil {
			return nil, err
		}
		switch state {
		case "completed":
			return decodeResultBody(result)
		case "late_result", "timed_out":
			return nil, fmt.Errorf("%w: operation %s resolved late", ErrRemoteTimeout, operationID)
		}
		if !e.sleep(ctx, deadline) {
			// Deadline: flip the pending row to timed_out (a callback that already completed it wins the
			// no-op) and re-poll once so a just-landed result is not lost to the race.
			if terr := e.ledger.Timeout(ctx, operationID); terr != nil {
				return nil, terr
			}
			state, result, err := e.ledger.Poll(ctx, operationID)
			if err != nil {
				return nil, err
			}
			if state == "completed" {
				return decodeResultBody(result)
			}
			return nil, fmt.Errorf("%w: operation %s deadline lapsed", ErrRemoteTimeout, operationID)
		}
	}
}

// awaitExisting polls the resolved result for a tool_call whose pending operation another (killed) invoke
// already opened — the executor does not re-POST, it waits for that invoke's callback (spec §26.7).
func (e *Executor) awaitExisting(ctx context.Context, toolCallID string, deadline time.Time) (map[string]any, error) {
	for {
		state, result, found, err := e.ledger.ProberRead(ctx, toolCallID)
		if err != nil {
			return nil, err
		}
		if found && state == "completed" {
			return decodeResultBody(result)
		}
		if found && state == "late_result" {
			return nil, fmt.Errorf("%w: prior operation for %s resolved late", ErrRemoteTimeout, toolCallID)
		}
		if !e.sleep(ctx, deadline) {
			return nil, fmt.Errorf("%w: prior operation for %s did not resolve", ErrRemoteTimeout, toolCallID)
		}
	}
}

// sleep waits one poll interval, returning false when the deadline is reached or ctx is cancelled.
func (e *Executor) sleep(ctx context.Context, deadline time.Time) bool {
	if !e.now().Before(deadline) {
		return false
	}
	timer := time.NewTimer(e.pollEvery)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return e.now().Before(deadline)
	}
}

// post performs the egress-safe signed POST: it vets the destination, pins the dial to the vetted
// resolved IP (DNS-rebind TOCTOU closed), denies redirects, disables keep-alives (re-resolve every
// attempt), and reads a BOUNDED response body (a customer server cannot stream unbounded). It reuses the
// shared egress layer — no new SSRF machinery.
func (e *Executor) post(ctx context.Context, rawURL string, allowPrivate bool, headers map[string]string, body []byte, timeout time.Duration) (int, []byte, error) {
	if err := egress.VetURL(rawURL, allowPrivate); err != nil {
		return 0, nil, err // egress.ErrDenied — terminal SSRF deny
	}
	transport := &http.Transport{
		DisableKeepAlives:   true,
		TLSClientConfig:     e.tlsConfig,
		TLSHandshakeTimeout: timeout,
		DialContext:         egress.PinnedDialer(e.resolver, allowPrivate, e.dial),
	}
	client := &http.Client{
		Timeout:   timeout,
		Transport: transport,
		CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
			return fmt.Errorf("%w: redirect not followed", egress.ErrDenied)
		},
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, rawURL, bytes.NewReader(body))
	if err != nil {
		return 0, nil, fmt.Errorf("build invoke request: %w", err)
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := client.Do(req)
	if err != nil {
		if errors.Is(err, egress.ErrDenied) {
			return 0, nil, err // a denied redirect/target is terminal
		}
		return 0, nil, fmt.Errorf("%w: %v", ErrRemoteProtocol, err)
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(io.LimitReader(resp.Body, maxResultBytes))
	return resp.StatusCode, respBody, nil
}

// decodeResult strictly reads the 200 sync result envelope: exactly a result (returned) or an RFC 9457
// problem (a typed tool error). An empty/unparseable body is a protocol error.
func decodeResult(body []byte) (map[string]any, error) {
	var env struct {
		Result  map[string]any `json:"result"`
		Problem map[string]any `json:"problem"`
	}
	dec := json.NewDecoder(bytes.NewReader(body))
	if err := dec.Decode(&env); err != nil {
		return nil, fmt.Errorf("%w: undecodable result body: %v", ErrRemoteProtocol, err)
	}
	if env.Problem != nil {
		return nil, fmt.Errorf("%w: %v", ErrRemoteProblem, env.Problem)
	}
	if env.Result == nil {
		return nil, fmt.Errorf("%w: result body carried neither result nor problem", ErrRemoteProtocol)
	}
	return env.Result, nil
}

// decodeResultBody decodes a result JSON blob persisted on the operation row (the callback stored the
// bare result object). A NULL/empty blob is a protocol error.
func decodeResultBody(body []byte) (map[string]any, error) {
	if len(body) == 0 {
		return nil, fmt.Errorf("%w: completed operation carried no result", ErrRemoteProtocol)
	}
	var result map[string]any
	if err := json.Unmarshal(body, &result); err != nil {
		return nil, fmt.Errorf("%w: undecodable stored result: %v", ErrRemoteProtocol, err)
	}
	return result, nil
}

// resultHash is the canonical sha256 of a result object (json.Marshal sorts keys), so a duplicate
// callback carrying the SAME result is recognised idempotent and a diverged one is a 409.
func resultHash(result map[string]any) string { return sha256Hex(mustJSON(result)) }

func sha256Hex(b []byte) string {
	sum := sha256.Sum256(b)
	return "sha256:" + hex.EncodeToString(sum[:])
}

func mustJSON(v any) []byte { b, _ := json.Marshal(v); return b }

// mintID returns prefix + 16 random hex chars (rop_<hex>).
func mintID(prefix string) (string, error) {
	var raw [8]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", fmt.Errorf("mint id: %w", err)
	}
	return prefix + hex.EncodeToString(raw[:]), nil
}

// mintToken returns a 256-bit random one-use callback token as hex bytes.
func mintToken() ([]byte, error) {
	var raw [32]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return nil, fmt.Errorf("mint token: %w", err)
	}
	out := make([]byte, hex.EncodedLen(len(raw)))
	hex.Encode(out, raw[:])
	return out, nil
}
