package mcp

import (
	"context"
	"crypto/tls"
	"fmt"
	"net"
	"time"

	"github.com/palgroup/palai/adapters/sandboxes/oci"
	"github.com/palgroup/palai/packages/egress"
)

// sandboxLabel is the OCI label class for MCP stdio containers — distinct from the engine ("engine") and
// shell ("shell") classes, so the orphan sweep reclaims only MCP containers and the engine reaper never
// touches an MCP one (and vice versa).
const sandboxLabel = "io.palai.sandbox"
const sandboxLabelMCP = "mcp"

// maxProgressPerCall bounds the advisory progress events one tools/call may journal (flood defence).
const maxProgressPerCall = 100

// defaultMaxTimeout is the hard ceiling on a per-call container's wall-time. It MUST stay below the orphan
// sweep's grace window (default 2m) so a legitimate long call's container is never swept mid-flight — the
// clamp bounds the container's lifetime regardless of a revision's declared timeout_ms.
const defaultMaxTimeout = 90 * time.Second

// ConnConfig is the non-secret wiring the manager dials a connection with, plus the SECRET_REF handle (never
// the resolved bearer) — the credential is resolved from the handle at REQUEST time inside Call, so it never
// enters argv, a log, or a DB row. It is the manager's own type (adapters never import control-plane
// internal): lookup.go maps an extensions.Connection into this.
type ConnConfig struct {
	ID          string
	Name        string
	Transport   string // "stdio" | "http"
	Org         string // for the tenant-scoped secret env-key
	ImageDigest string // stdio
	Cmd         []string
	URL         string // http
	SecretRef   string // handle; resolved at Call time; "" ⇒ no bearer
	TimeoutMS   int
}

// CallScope carries the tenant + session/response + call identity a progress event needs, without importing
// the tool-broker's ExecEnv into this adapter. SessionID/ResponseID scope the advisory progress event to the
// right journal.
type CallScope struct {
	Org, Project, SessionID, ResponseID, RunID, CallID string
}

// ProgressSink receives advisory progress notifications during a tools/call. The compose wiring appends a
// tool_call.progress.v1 event; a nil sink drops progress (advisory, best-effort).
type ProgressSink interface {
	ToolProgress(ctx context.Context, scope CallScope, p Progress)
}

// SecretResolver bridges a connection's secret_ref handle to the bearer bytes at request time (the
// webhookSecretResolver pattern: an org-scoped env-file bridge, never inline, never logged). Nil ⇒ no auth.
type SecretResolver func(org, ref string) ([]byte, error)

// Config wires the manager. Driver nil ⇒ stdio unsupported (a stdio call fails cleanly, never escapes).
// The http egress knobs mirror the webhook sender; AllowPrivate is the test-harness-only self-host flag.
type Config struct {
	Driver           oci.InteractiveDriver
	Secrets          SecretResolver
	Sink             ProgressSink
	Limits           oci.Limits
	MaxStdoutBytes   int64
	MaxStderrBytes   int64
	Resolver         egress.Resolver
	Dial             func(ctx context.Context, network, addr string) (net.Conn, error)
	TLSConfig        *tls.Config
	AllowPrivate     bool
	BreakerThreshold int
	BreakerCooldown  time.Duration
	DefaultTimeout   time.Duration
	// MaxTimeout is the hard ceiling on a per-call container's wall-time; it MUST stay below the orphan
	// sweep grace so a live container is never swept mid-call. Zero defaults to defaultMaxTimeout.
	MaxTimeout time.Duration
}

// Manager dials MCP connections and runs tools/call + discovery through per-call transports, guarded by a
// per-connection circuit breaker. It NEVER routes through the RunnerGateway (that is the engine-only
// invariant, E09 T1): an MCP server is a distinct, labelled, network-less sandbox.
type Manager struct {
	cfg     Config
	breaker *breaker
}

// NewManager builds a manager with sane bounds. Limits default to a modest network-less sandbox.
func NewManager(cfg Config) *Manager {
	if cfg.MaxStdoutBytes <= 0 {
		cfg.MaxStdoutBytes = maxStdioMessage
	}
	if cfg.MaxStderrBytes <= 0 {
		cfg.MaxStderrBytes = 256 * 1024
	}
	if cfg.DefaultTimeout <= 0 {
		cfg.DefaultTimeout = 30 * time.Second
	}
	if cfg.MaxTimeout <= 0 {
		cfg.MaxTimeout = defaultMaxTimeout
	}
	if cfg.Limits.WallTime <= 0 {
		cfg.Limits.WallTime = cfg.DefaultTimeout
	}
	if cfg.Limits.MaxMemoryBytes <= 0 {
		cfg.Limits.MaxMemoryBytes = 512 << 20
	}
	if cfg.Limits.MaxProcessCount <= 0 {
		cfg.Limits.MaxProcessCount = 64
	}
	if cfg.Limits.NanoCPUs <= 0 {
		cfg.Limits.NanoCPUs = 1_000_000_000
	}
	return &Manager{cfg: cfg, breaker: newBreaker(cfg.BreakerThreshold, cfg.BreakerCooldown, nil)}
}

// Call runs one remote tools/call: breaker gate → dial a per-call transport → initialize → tools/call
// (routing progress to the sink) → teardown. A transport/protocol failure records a breaker failure and
// surfaces; a tripped breaker returns ErrToolUnavailable BEFORE any container/dial. The result is data-only
// (the broker output-schema-validates it); an MCP server can never widen capability through it.
func (m *Manager) Call(ctx context.Context, scope CallScope, conn ConnConfig, remoteName string, args map[string]any) (map[string]any, error) {
	if !m.breaker.allow(conn.ID) {
		return nil, ErrToolUnavailable
	}
	callCtx, cancel := context.WithTimeout(ctx, m.timeout(conn))
	defer cancel()

	transport, teardown, err := m.dial(callCtx, conn)
	if err != nil {
		m.breaker.recordFailure(conn.ID)
		return nil, err
	}
	defer teardown()

	client := NewClient(transport)
	if err := client.Initialize(callCtx); err != nil {
		m.breaker.recordFailure(conn.ID)
		return nil, err
	}
	var onProgress func(Progress)
	if m.cfg.Sink != nil {
		// Cap progress events per call: a hostile server flooding notifications cannot write an unbounded
		// number of journal rows (one tx each). Progress is single-threaded per call, so a plain counter is
		// safe. ponytail: fixed cap; make it configurable if a legitimate long tool ever needs more.
		count := 0
		onProgress = func(p Progress) {
			count++
			if count > maxProgressPerCall {
				return
			}
			m.cfg.Sink.ToolProgress(ctx, scope, p)
		}
	}
	result, err := client.CallTool(callCtx, remoteName, args, onProgress)
	if err != nil {
		m.breaker.recordFailure(conn.ID)
		return nil, err
	}
	m.breaker.recordSuccess(conn.ID)
	return result, nil
}

// Discover dials a connection, initializes, and returns its tools/list (an admin action). It is NOT
// breaker-guarded — an admin wants the real dial/protocol error, not a shed one.
func (m *Manager) Discover(ctx context.Context, conn ConnConfig) ([]RemoteTool, error) {
	dialCtx, cancel := context.WithTimeout(ctx, m.timeout(conn))
	defer cancel()
	transport, teardown, err := m.dial(dialCtx, conn)
	if err != nil {
		return nil, err
	}
	defer teardown()
	client := NewClient(transport)
	if err := client.Initialize(dialCtx); err != nil {
		return nil, err
	}
	return client.ListTools(dialCtx)
}

// dial builds a per-call transport for the connection's transport kind and returns a teardown that reclaims
// it. The stdio path starts a hardened, network-less OCI container with NO mounts (workspace/DB/credential
// never enter); the http path resolves the bearer from the secret_ref at THIS moment.
func (m *Manager) dial(ctx context.Context, conn ConnConfig) (Transport, func(), error) {
	switch conn.Transport {
	case "stdio":
		return m.dialStdio(ctx, conn)
	case "http":
		return m.dialHTTP(ctx, conn)
	default:
		return nil, nil, fmt.Errorf("%w: unknown transport %q", ErrProtocol, conn.Transport)
	}
}

// dialStdio starts the MCP server in a per-call, hardened, network-less container and frames JSON-RPC over
// its stdio. ContainerSpec: pinned image + explicit cmd, EMPTY env, EMPTY mounts, the mcp sandbox label,
// wall-time = the connection timeout. Network none is hardcoded in the driver's createOptions.
func (m *Manager) dialStdio(ctx context.Context, conn ConnConfig) (Transport, func(), error) {
	if m.cfg.Driver == nil {
		return nil, nil, fmt.Errorf("%w: stdio transport requires an OCI driver (none wired)", ErrProtocol)
	}
	limits := m.cfg.Limits
	limits.WallTime = m.timeout(conn)
	proc, err := m.cfg.Driver.Start(ctx, oci.ContainerSpec{
		ImageDigest:    conn.ImageDigest,
		Cmd:            conn.Cmd,
		Env:            nil, // no host inheritance, no credential
		Labels:         map[string]string{sandboxLabel: sandboxLabelMCP},
		Limits:         limits,
		MaxStdoutBytes: m.cfg.MaxStdoutBytes,
		MaxStderrBytes: m.cfg.MaxStderrBytes,
		Mounts:         nil, // the MCP server NEVER sees the workspace/DB/credential
	})
	if err != nil {
		return nil, nil, fmt.Errorf("%w: start mcp sandbox: %v", ErrProtocol, err)
	}
	transport := NewStdioTransport(proc.Stdin(), proc.Stdout(), func(ctx context.Context) error { return proc.Kill(ctx) })
	teardown := func() {
		killCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		_ = transport.Close(killCtx) // stops the reader goroutine (no flood-park leak) AND kills the container
	}
	return transport, teardown, nil
}

// dialHTTP builds a Streamable HTTP transport, resolving the bearer from the secret_ref at request time.
func (m *Manager) dialHTTP(ctx context.Context, conn ConnConfig) (Transport, func(), error) {
	bearer, err := m.resolveBearer(conn)
	if err != nil {
		return nil, nil, err
	}
	transport, err := NewHTTPTransport(HTTPOptions{
		URL:          conn.URL,
		Bearer:       bearer,
		AllowPrivate: m.cfg.AllowPrivate,
		Resolver:     m.cfg.Resolver,
		Dial:         m.cfg.Dial,
		TLSConfig:    m.cfg.TLSConfig,
		Timeout:      m.timeout(conn),
	})
	if err != nil {
		return nil, nil, err
	}
	return transport, func() { _ = transport.Close(context.Background()) }, nil
}

// resolveBearer redeems the connection's secret_ref for its bearer bytes at request time. No ref ⇒ no auth;
// a ref with no resolver wired is an error (never a silent unauthenticated call to a server that expects
// auth). The bytes are used here and never returned up or logged.
func (m *Manager) resolveBearer(conn ConnConfig) (string, error) {
	if conn.SecretRef == "" {
		return "", nil
	}
	if m.cfg.Secrets == nil {
		return "", fmt.Errorf("%w: connection needs a secret_ref but no resolver is wired", ErrProtocol)
	}
	b, err := m.cfg.Secrets(conn.Org, conn.SecretRef)
	if err != nil {
		return "", fmt.Errorf("%w: resolve connection secret: %v", ErrProtocol, err)
	}
	return string(b), nil
}

// timeout resolves the per-call wall-time from the connection (falling back to the manager default), CLAMPED
// to MaxTimeout so a container's lifetime always stays below the orphan sweep grace (never swept mid-call).
func (m *Manager) timeout(conn ConnConfig) time.Duration {
	d := m.cfg.DefaultTimeout
	if conn.TimeoutMS > 0 {
		d = time.Duration(conn.TimeoutMS) * time.Millisecond
	}
	if m.cfg.MaxTimeout > 0 && d > m.cfg.MaxTimeout {
		d = m.cfg.MaxTimeout
	}
	return d
}
