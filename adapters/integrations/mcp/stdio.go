package mcp

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"sync"
	"sync/atomic"
)

// maxStdioMessage bounds a single newline-delimited JSON-RPC line read from an untrusted server, so a
// server cannot exhaust memory with one unbounded message.
const maxStdioMessage = 4 * 1024 * 1024

// stdioTransport frames JSON-RPC over a server's stdin/stdout (MCP 2025-11-25 stdio: newline-delimited, no
// embedded newline). It is deliberately decoupled from the OCI driver — it takes plain io streams plus a
// teardown func — so the framing/id-matching/progress/cancel logic is provable without a container, while
// the manager wires a hardened oci.Process's Stdin()/Stdout()/Kill into it for the real, isolated run.
//
// A SINGLE reader goroutine drains stdout for the transport's life and fans messages onto inbound; Call
// selects on inbound + ctx. Per-call-container ceiling: exactly one request is outstanding at a time, so a
// notification is unambiguously for the active call.
type stdioTransport struct {
	w      io.Writer
	closer func(context.Context) error
	nextID atomic.Int64

	inbound   chan rpcMessage
	readErr   atomic.Pointer[error]
	writeMu   sync.Mutex
	done      chan struct{}
	closeOnce sync.Once
	sampling  SamplingHandler // nil ⇒ default-deny (a server sampling/createMessage gets a JSON-RPC error)
}

// NewStdioTransport frames JSON-RPC over w (the server's stdin) and r (its stdout). closer tears the
// underlying process/container down; a nil closer is a no-op. The reader goroutine exits when r reaches EOF
// OR when Close is called — so a hostile server flooding notifications after answering cannot park it on a
// full inbound channel forever (goroutine + memory leak).
func NewStdioTransport(w io.Writer, r io.Reader, closer func(context.Context) error) Transport {
	t := &stdioTransport{w: w, closer: closer, inbound: make(chan rpcMessage, 32), done: make(chan struct{})}
	go t.readLoop(r)
	return t
}

// setSampling binds the per-call sampling handler after construction (the manager sets it from Config.Sampling
// + the connection's sampling flag). A nil handler leaves default-deny. It runs before the first Call — only
// Call reads t.sampling (the reader goroutine just fans frames), so there is no race.
func (t *stdioTransport) setSampling(h SamplingHandler) { t.sampling = h }

// rpcMessage is the inbound frame shape: a response carries id + result/error; a notification carries method
// + params and no id.
type rpcMessage struct {
	ID     json.RawMessage `json:"id"`
	Result json.RawMessage `json:"result"`
	Error  *rpcError       `json:"error"`
	Method string          `json:"method"`
	Params json.RawMessage `json:"params"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

// readLoop drains stdout, unmarshals each line, and fans it onto inbound until EOF/error, then closes
// inbound so a blocked Call unblocks with the recorded read error.
func (t *stdioTransport) readLoop(r io.Reader) {
	defer close(t.inbound)
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), maxStdioMessage)
	for sc.Scan() {
		var msg rpcMessage
		if err := json.Unmarshal(sc.Bytes(), &msg); err != nil {
			continue // ignore a non-JSON line
		}
		select {
		case t.inbound <- msg:
		case <-t.done:
			return // Close aborted us: stop parking on a full channel (a post-answer notification flood
			// would otherwise leak this goroutine + its buffered messages forever).
		}
	}
	err := sc.Err()
	if err == nil {
		err = io.EOF
	}
	t.readErr.Store(&err)
}

// Call writes a request and consumes inbound until the matching-id response arrives, routing any
// notifications/progress to onProgress. On ctx cancellation it sends notifications/cancelled referencing
// this request's id and returns the ctx error (the manager then tears the container down).
func (t *stdioTransport) Call(ctx context.Context, method string, params any, onProgress func(Progress)) (json.RawMessage, error) {
	id := t.nextID.Add(1)
	if err := t.writeMessage(map[string]any{"jsonrpc": "2.0", "id": id, "method": method, "params": params}); err != nil {
		return nil, err
	}
	want := fmt.Sprintf("%d", id)
	// One gate per call: it carries the flood-cap counter across every server request this tools/call sees.
	gate := &samplingGate{handler: t.sampling}
	for {
		select {
		case <-ctx.Done():
			_ = t.Notify(context.Background(), "notifications/cancelled", map[string]any{"requestId": id})
			return nil, ctx.Err()
		case msg, ok := <-t.inbound:
			if !ok {
				return nil, t.closedErr()
			}
			if msg.Method == "notifications/progress" && onProgress != nil {
				if p, ok := decodeProgress(msg.Params); ok {
					onProgress(p)
				}
				continue
			}
			if isServerRequest(msg) {
				// A server→client request (e.g. sampling/createMessage) arrives interleaved with our
				// outstanding tools/call. Serve it synchronously HERE (still exactly one client request
				// outstanding — the invariant holds) and write the response back on the same stdin. A denial
				// is a JSON-RPC error, never a returned error, so it never trips the breaker.
				result, rerr := gate.serve(ctx, msg.Method, msg.Params)
				_ = t.writeMessage(serverResponseFrame(msg.ID, result, rerr))
				continue
			}
			if len(msg.ID) == 0 || trimID(msg.ID) != want {
				continue // an unrelated notification or an earlier response
			}
			if msg.Error != nil {
				return nil, fmt.Errorf("%w: %s (code %d)", ErrProtocol, msg.Error.Message, msg.Error.Code)
			}
			return msg.Result, nil
		}
	}
}

// Notify writes a fire-and-forget notification (no id, no response awaited).
func (t *stdioTransport) Notify(ctx context.Context, method string, params any) error {
	return t.writeMessage(map[string]any{"jsonrpc": "2.0", "method": method, "params": params})
}

// Close signals the reader goroutine to stop (unblocking it if it is parked sending to a full inbound), then
// tears the underlying process/container down. Idempotent — the manager's teardown always routes through it.
func (t *stdioTransport) Close(ctx context.Context) error {
	t.closeOnce.Do(func() { close(t.done) })
	if t.closer == nil {
		return nil
	}
	return t.closer(ctx)
}

// closedErr reports why the inbound stream ended (the server exited / the container died).
func (t *stdioTransport) closedErr() error {
	if p := t.readErr.Load(); p != nil {
		return fmt.Errorf("%w: server stream closed: %v", ErrProtocol, *p)
	}
	return fmt.Errorf("%w: server stream closed", ErrProtocol)
}

// writeMessage serialises one message as a single newline-delimited line (no embedded newline). The write
// lock keeps a Notify (e.g. a cancel) from interleaving bytes with a concurrent request write.
func (t *stdioTransport) writeMessage(msg map[string]any) error {
	encoded, err := json.Marshal(msg)
	if err != nil {
		return fmt.Errorf("%w: marshal request: %v", ErrProtocol, err)
	}
	encoded = append(encoded, '\n')
	t.writeMu.Lock()
	defer t.writeMu.Unlock()
	if _, err := t.w.Write(encoded); err != nil {
		return fmt.Errorf("%w: write: %v", ErrProtocol, err)
	}
	return nil
}

// decodeProgress parses a notifications/progress params object.
func decodeProgress(raw json.RawMessage) (Progress, bool) {
	var p struct {
		Progress float64 `json:"progress"`
		Total    float64 `json:"total"`
		Message  string  `json:"message"`
	}
	if err := json.Unmarshal(raw, &p); err != nil {
		return Progress{}, false
	}
	return Progress{Progress: p.Progress, Total: p.Total, Message: p.Message}, true
}

// trimID renders a JSON-RPC id (a number or string) to its comparison form, stripping quotes from a string
// id so "1" and 1 do not spuriously differ across a server that echoes ids as strings.
func trimID(raw json.RawMessage) string {
	s := string(raw)
	if len(s) >= 2 && s[0] == '"' && s[len(s)-1] == '"' {
		return s[1 : len(s)-1]
	}
	return s
}
