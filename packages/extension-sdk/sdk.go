// Package extsdk is the server-side helper set for building a remote_http tool
// endpoint that speaks tool-http.v1 (spec §28.23/§28.24, TOL-018). It gives a
// customer's tool server the four contract-correct primitives without letting it
// get the wire shape or the MAC wrong:
//
//  1. define-tool schema emit — canonical registration bytes for a tool revision;
//  2. signed-invocation verify + result-callback sign — standard-webhooks HMAC in
//     BOTH directions, byte-identical to the reference webhook signer;
//  3. normalized {result|problem} bodies — the sync-200 body and the tool-http.v1
//     callback envelope (built from packages/contracts, never a hand-rolled shape);
//  4. tool_call_id idempotency store — the executor's same-hash-replay /
//     diverged-hash-409 rule.
//
// The SDK NEVER assigns trust; it only makes producing the contract correct (spec
// §28.23). It is a deliberate SECOND implementation of the signing input, so the
// shared conformance corpus runs it AND the reference webhook.Verify/Signer — any
// divergence fails a test, not a review.
package extsdk

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/palgroup/palai/packages/contracts"
)

// Protocol is the tool-http.v1 envelope protocol constant (the schema's `protocol`
// const). SignatureVersion is the standard-webhooks scheme prefix bound INTO the
// signed input, so a receiver cannot be tricked into verifying a different scheme.
const (
	Protocol         = "tool-http.v1"
	SignatureVersion = "v1"
)

// The standard-webhooks attempt headers (spec §21.5) a tool server reads off an
// invoke and writes onto a callback. These are protocol header names, not a
// reinvented shape.
const (
	HeaderID        = "Webhook-Id"
	HeaderTimestamp = "Webhook-Timestamp"
	HeaderSignature = "Webhook-Signature"
)

// canonical renders v as sorted-key compact JSON with HTML escaping OFF, so the
// bytes are identical to the TS (sorted-key JSON.stringify) and Python
// (json.dumps sort_keys, compact) legs. It marshals through a generic value so
// every nested map's keys sort — the ONE definition of canonical bytes.
func canonical(v any) ([]byte, error) {
	raw, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	var generic any
	if err := json.Unmarshal(raw, &generic); err != nil {
		return nil, err
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(generic); err != nil {
		return nil, err
	}
	return bytes.TrimRight(buf.Bytes(), "\n"), nil
}

// ToolDefinition is the executor-config subset a tool server declares to register
// a remote_http tool revision (spec §28.4). Only these known fields are emitted;
// a secret is a SecretRef handle only, never a raw credential.
type ToolDefinition struct {
	Executor       string         `json:"executor"`
	Description    string         `json:"description"`
	InputSchema    map[string]any `json:"input_schema"`
	OutputSchema   map[string]any `json:"output_schema"`
	ReplayClass    string         `json:"replay_class"`
	TimeoutMS      *int           `json:"timeout_ms"`
	ExecutorConfig map[string]any `json:"executor_config"`
	SecretRef      string         `json:"secret_ref"`
}

// Canonical emits the tool revision registration body as canonical bytes, dropping
// unset optional fields (the same omit rule the TS/Py legs apply).
func (d ToolDefinition) Canonical() ([]byte, error) {
	if d.Executor == "" {
		return nil, errors.New("extsdk: tool definition needs an executor")
	}
	if d.InputSchema == nil {
		return nil, errors.New("extsdk: tool definition needs an input_schema")
	}
	body := map[string]any{"executor": d.Executor, "input_schema": d.InputSchema}
	if d.Description != "" {
		body["description"] = d.Description
	}
	if d.OutputSchema != nil {
		body["output_schema"] = d.OutputSchema
	}
	if d.ReplayClass != "" {
		body["replay_class"] = d.ReplayClass
	}
	if d.TimeoutMS != nil {
		body["timeout_ms"] = *d.TimeoutMS
	}
	if d.ExecutorConfig != nil {
		body["executor_config"] = d.ExecutorConfig
	}
	if d.SecretRef != "" {
		body["secret_ref"] = d.SecretRef
	}
	return canonical(body)
}

// Sign computes the hex HMAC-SHA-256 over the standard-webhooks signed input:
// version, delivery id, unix timestamp, and the EXACT raw body, joined by "."
// (spec §21.5). Binding version + id defeats cross-context replay. Byte-identical
// to the reference webhook signer (proven by the shared corpus).
func Sign(secret []byte, deliveryID string, ts time.Time, body []byte) string {
	mac := hmac.New(sha256.New, secret)
	mac.Write([]byte(SignatureVersion + "." + deliveryID + "." + strconv.FormatInt(ts.Unix(), 10) + "."))
	mac.Write(body)
	return hex.EncodeToString(mac.Sum(nil))
}

// SignatureHeader builds the Webhook-Signature value: one space-separated v1=
// field per secret, so a rotation overlap (old + new) is accepted by a receiver
// that has advanced to either.
func SignatureHeader(deliveryID string, ts time.Time, body []byte, secrets ...[]byte) string {
	parts := make([]string, 0, len(secrets))
	for _, s := range secrets {
		parts = append(parts, SignatureVersion+"="+Sign(s, deliveryID, ts, body))
	}
	return strings.Join(parts, " ")
}

// Verify is the receiver-side check: it recomputes the MAC over the raw body,
// compares in constant time (hmac.Equal), enforces the timestamp tolerance around
// now (the replay window, enforced on BOTH skew directions), and accepts a header
// carrying several v1= values (rotation overlap). Mirrors webhook.Verify verbatim.
func Verify(secret []byte, deliveryID string, ts time.Time, body []byte, header string, now time.Time, tolerance time.Duration) bool {
	if skew := now.Sub(ts); skew > tolerance || skew < -tolerance {
		return false // outside the replay window
	}
	want := []byte(Sign(secret, deliveryID, ts, body))
	for _, field := range strings.Fields(header) {
		value, ok := strings.CutPrefix(field, SignatureVersion+"=")
		if !ok {
			continue
		}
		if hmac.Equal([]byte(value), want) {
			return true
		}
	}
	return false
}

// CallbackHeaders returns the standard-webhooks headers a tool server posts a
// result callback with (multi-secret during rotation). The one-use callback token
// header (Tool-Callback-Token) is added by the caller from the invoke's
// callback.token — the SDK signs, it does not mint tokens.
func CallbackHeaders(deliveryID string, ts time.Time, body []byte, secrets ...[]byte) map[string]string {
	return map[string]string{
		HeaderID:        deliveryID,
		HeaderTimestamp: strconv.FormatInt(ts.Unix(), 10),
		HeaderSignature: SignatureHeader(deliveryID, ts, body, secrets...),
	}
}

// SyncResult builds the synchronous 200 body carrying a tool result; SyncProblem
// builds the {problem} body for an RFC 9457 error. Exactly one of the two shapes
// a remote_http server may answer 200 with (spec §28.24), as canonical bytes.
func SyncResult(result map[string]any) ([]byte, error) {
	return canonical(map[string]any{"result": result})
}
func SyncProblem(problem map[string]any) ([]byte, error) {
	return canonical(map[string]any{"problem": problem})
}

// Callback builds the tool-http.v1 result callback envelope carrying a result;
// CallbackProblem carries an RFC 9457 problem instead. Both are built from the
// generated contracts.ToolHTTPCallback (no hand-rolled envelope) and emitted as
// canonical bytes the server signs with CallbackHeaders.
func Callback(operationID, toolCallID string, result map[string]any) ([]byte, error) {
	return canonical(contracts.ToolHTTPCallback{
		Protocol: Protocol, OperationID: operationID, ToolCallID: toolCallID, Result: result,
	})
}
func CallbackProblem(operationID, toolCallID string, problem map[string]any) ([]byte, error) {
	return canonical(contracts.ToolHTTPCallback{
		Protocol: Protocol, OperationID: operationID, ToolCallID: toolCallID, Problem: problem,
	})
}

// Outcome classifies a tool_call_id + request_hash against the idempotency store.
type Outcome int

const (
	Fresh    Outcome = iota // first time this tool_call_id is seen; caller executes then Store()s
	Replay                  // same tool_call_id + same request_hash; caller replays the stored response
	Conflict                // same tool_call_id, DIVERGED request_hash; caller answers 409
)

// IdempotencyStore is the in-memory tool_call_id replay guard a tool server keys
// on the invoke's Idempotency-Key (= tool_call_id) and request_hash, mirroring the
// control-plane executor's rule (spec §28.24): a same-hash duplicate replays the
// stored answer, a diverged hash is a 409 (a duplicate that changed content must
// not answer a different call). The SEMANTICS are what the SDK pins; a multi-
// replica server backs the same rule with its own shared store.
//
// ponytail: one process-wide mutex — a single-process helper; a sharded map is a
// later upgrade only if a hot tool server contends measurably.
type IdempotencyStore struct {
	mu   sync.Mutex
	seen map[string]entry
}

type entry struct {
	requestHash string
	response    []byte
}

// NewIdempotencyStore returns an empty store.
func NewIdempotencyStore() *IdempotencyStore {
	return &IdempotencyStore{seen: make(map[string]entry)}
}

// Classify reports how the server should answer a (tool_call_id, request_hash):
// Fresh (execute + Store), Replay (return the stored response), or Conflict (409).
func (s *IdempotencyStore) Classify(toolCallID, requestHash string) (Outcome, []byte) {
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.seen[toolCallID]
	if !ok {
		return Fresh, nil
	}
	if e.requestHash != requestHash {
		return Conflict, nil
	}
	return Replay, e.response
}

// Store records a fresh call's response so a later same-hash duplicate replays it.
func (s *IdempotencyStore) Store(toolCallID, requestHash string, response []byte) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.seen[toolCallID] = entry{requestHash: requestHash, response: append([]byte(nil), response...)}
}
