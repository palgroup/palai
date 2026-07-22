package api

import (
	"context"
	"crypto/subtle"
	"errors"
	"io"
	"log"
	"net/http"
	"time"

	remotehttp "github.com/palgroup/palai/adapters/tools/http"
	"github.com/palgroup/palai/apps/control-plane/api/middleware"
)

// callbackTolerance is the replay window a signed tool-http.v1 callback's timestamp must fall within
// (the webhook receiver default). A callback outside it is a stale reject, never persisted.
const callbackTolerance = 5 * time.Minute

// ToolCallbackStore is the operation-ledger seam the callback endpoint drives: read the verify inputs,
// then atomically consume the one-use token. *remotehttp.Operations satisfies it.
type ToolCallbackStore interface {
	ForCallback(ctx context.Context, operationID string) (remotehttp.CallbackRow, bool, error)
	Consume(ctx context.Context, operationID string, result []byte, resultHash string) (newState string, consumed bool, err error)
}

// toolCallbackHandler serves the broker-controlled remote-tool result callback (POST
// /v1/tool-callbacks/{operation_id}, spec §28.24). Like the inbound-webhook receiver its AUTH IS the
// per-operation HMAC signature PLUS the one-use audience-bound token, so it mounts on the UNAUTHENTICATED
// top mux (bypassing middleware.Auth). It is ACK-ONLY: it returns no tool data (tenant isolation), and an
// unknown operation / token mismatch is a generic 404 (no config oracle). The result commits to the run
// ONLY through the waiting executor under a live fence — this handler NEVER touches the ledger.
type toolCallbackHandler struct {
	ops       ToolCallbackStore
	resolver  func(org, ref string) ([]byte, error)
	tolerance time.Duration
	now       func() time.Time
}

// NewToolCallbackHandler builds the callback endpoint over the operation ledger + the org-scoped secret
// resolver (the same handle bridge the outbound invoke signs with). nil in tiers that never touch it.
func NewToolCallbackHandler(ops ToolCallbackStore, resolver func(org, ref string) ([]byte, error)) http.Handler {
	h := &toolCallbackHandler{ops: ops, resolver: resolver, tolerance: callbackTolerance, now: time.Now}
	return http.HandlerFunc(h.receive)
}

// receive ingests one signed result callback: size cap → read the operation → resolve the secret →
// verify-before-persist (timestamp + HMAC) → constant-time one-use token → atomic consume → ack. Every
// disqualifier an unauthenticated prober could probe collapses to a generic 404.
func (h *toolCallbackHandler) receive(w http.ResponseWriter, r *http.Request) {
	operationID := r.PathValue("operation_id")
	raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxBodyBytes))
	if err != nil {
		middleware.WriteProblem(w, r, http.StatusRequestEntityTooLarge, "payload_too_large", "the callback exceeds the size limit")
		return
	}

	row, found, err := h.ops.ForCallback(r.Context(), operationID)
	if err != nil {
		middleware.WriteProblem(w, r, http.StatusInternalServerError, "internal_error", "")
		return
	}
	if !found {
		// Generic 404 for an unknown operation — an unauthenticated prober learns nothing (no oracle).
		middleware.WriteProblem(w, r, http.StatusNotFound, "not_found", "no such tool callback")
		return
	}

	secret, serr := h.resolver(row.Org, row.SecretRef)
	if serr != nil || len(secret) == 0 {
		// An unresolvable secret is indistinguishable from an unknown operation — a generic 404, never an
		// oracle for "this operation exists but its secret bridge is missing".
		middleware.WriteProblem(w, r, http.StatusNotFound, "not_found", "no such tool callback")
		return
	}

	headers := map[string]string{
		"Webhook-Timestamp": r.Header.Get("Webhook-Timestamp"),
		"Webhook-Signature": r.Header.Get("Webhook-Signature"),
	}
	cb, perr := remotehttp.ParseCallback(operationID, headers, raw, secret, h.now(), h.tolerance)
	switch {
	case errors.Is(perr, remotehttp.ErrCallbackBadSignature), errors.Is(perr, remotehttp.ErrCallbackStale):
		middleware.WriteProblem(w, r, http.StatusUnauthorized, "invalid_signature", "the callback signature did not verify")
		return
	case errors.Is(perr, remotehttp.ErrCallbackMalformed):
		middleware.WriteProblem(w, r, http.StatusBadRequest, "invalid_request", "the callback envelope is malformed")
		return
	case perr != nil:
		middleware.WriteProblem(w, r, http.StatusInternalServerError, "internal_error", "")
		return
	}

	// One-use audience-bound token: constant-time compare of its hash against the operation row's stored
	// hash. A mismatch is a generic 404 (the token is the audience binding — a wrong one is not this
	// operation's callback, and revealing more would be an oracle).
	token := r.Header.Get(remotehttp.HeaderCallbackToken)
	if subtle.ConstantTimeCompare([]byte(remotehttp.HashToken(token)), []byte(row.TokenHash)) != 1 {
		middleware.WriteProblem(w, r, http.StatusNotFound, "not_found", "no such tool callback")
		return
	}

	// Persist the payload in the discriminated {result|problem} envelope so the waiting executor + the
	// prober can tell a successful result from an RFC 9457 problem (MF2 — a problem must NOT surface as a
	// success). The hash is over the payload, so duplicate-callback idempotency is unaffected.
	payload, isProblem := remotehttp.Payload(cb)
	blob := remotehttp.EncodeStoredResult(payload, isProblem)
	hash := remotehttp.ResultHash(payload)

	newState, consumed, cerr := h.ops.Consume(r.Context(), operationID, blob, hash)
	if cerr != nil {
		middleware.WriteProblem(w, r, http.StatusInternalServerError, "internal_error", "")
		return
	}
	if consumed {
		// completed (before deadline) or late_result (after) — both an ack-only 200, NO tool data returned.
		writeJSON(w, http.StatusOK, map[string]any{"state": newState})
		return
	}

	// The token was already spent (a duplicate callback). Re-read the current stored hash (the first
	// callback's) and compare: the SAME payload is idempotent 200, a DIVERGED one is a 409 + audit.
	current, _, rerr := h.ops.ForCallback(r.Context(), operationID)
	if rerr == nil && hash == current.ResultHash {
		writeJSON(w, http.StatusOK, map[string]any{"state": "already_delivered"})
		return
	}
	log.Printf("security: divergent duplicate tool callback for operation %s (incoming hash %s != stored %s)", operationID, hash, current.ResultHash)
	middleware.WriteProblem(w, r, http.StatusConflict, "callback_conflict", "a different result was already delivered for this operation")
}
