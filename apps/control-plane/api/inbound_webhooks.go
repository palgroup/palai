package api

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"

	"github.com/palgroup/palai/adapters/integrations/webhook"
	"github.com/palgroup/palai/apps/control-plane/api/middleware"
	"github.com/palgroup/palai/apps/control-plane/internal/automation"
)

// inboundHandler serves the UNAUTHENTICATED signed-webhook receiver (POST /v1/inbound/{trigger_id}),
// mounted on the top mux (its auth IS the source signature — see router.go). It bounds the request body,
// forwards the attempt headers + raw bytes to the store's verify-before-persist ingest, and maps the
// typed outcome to a status without leaking a config oracle.
type inboundHandler struct {
	triggers TriggerAPI
}

// receive ingests one signed inbound event. Size cap (413) → verify+persist (the store owns the semaphore,
// signature check, and backlog gate) → 202 with the delivery id once a durable row commits.
func (h *inboundHandler) receive(w http.ResponseWriter, r *http.Request) {
	// http.MaxBytesReader caps the body at the trust boundary; an over-cap read errors → 413. The size cap
	// is the FIRST backpressure gate (before the signature work), so a flood of oversized bodies is cheap.
	raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxBodyBytes))
	if err != nil {
		middleware.WriteProblem(w, r, http.StatusRequestEntityTooLarge, "payload_too_large", "the inbound event exceeds the size limit")
		return
	}
	headers := map[string]string{
		webhook.HeaderID:        r.Header.Get(webhook.HeaderID),
		webhook.HeaderTimestamp: r.Header.Get(webhook.HeaderTimestamp),
		webhook.HeaderSignature: r.Header.Get(webhook.HeaderSignature),
		webhook.HeaderAttempt:   r.Header.Get(webhook.HeaderAttempt),
	}
	res, err := h.triggers.IngestInbound(r.Context(), r.PathValue("trigger_id"), headers, raw)
	switch {
	case errors.Is(err, automation.ErrInboundNotAvailable):
		// Generic 404 for every disqualifier (unknown / non-webhook / disabled / secret-less / no revision)
		// so an unauthenticated prober learns nothing about a source it could not authenticate.
		middleware.WriteProblem(w, r, http.StatusNotFound, "not_found", "no such inbound endpoint")
		return
	case errors.Is(err, automation.ErrInboundUnauthenticated):
		middleware.WriteProblem(w, r, http.StatusUnauthorized, "invalid_signature", "the event signature did not verify")
		return
	case errors.Is(err, automation.ErrInboundMalformed):
		middleware.WriteProblem(w, r, http.StatusBadRequest, "invalid_request", "the event envelope is malformed")
		return
	case err != nil:
		var bp automation.ErrInboundBackpressure
		if errors.As(err, &bp) {
			writeBackpressure(w, r, bp)
			return
		}
		middleware.WriteProblem(w, r, http.StatusInternalServerError, "internal_error", "")
		return
	}
	w.Header().Set("Location", "/v1/trigger-deliveries/"+res.DeliveryID)
	writeJSON(w, http.StatusAccepted, map[string]any{
		"id": res.DeliveryID, "state": res.State, "response_id": res.ResponseID,
		"run_id": res.RunID, "session_id": res.SessionID, "duplicate_of": res.DuplicateOf, "reason": res.Reason,
	})
}

// writeBackpressure renders the AUT-010 shed response: 429 + Retry-After, with the queue depth + oldest
// age reported in the problem body (§34.4 "admission reports...") so a sender can back off intelligently.
func writeBackpressure(w http.ResponseWriter, r *http.Request, bp automation.ErrInboundBackpressure) {
	retry := int(bp.RetryAfter.Seconds())
	if retry < 1 {
		retry = 1
	}
	w.Header().Set("Retry-After", strconv.Itoa(retry))
	w.Header().Set("Content-Type", "application/problem+json")
	w.WriteHeader(http.StatusTooManyRequests)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"type":                "https://docs.palai.dev/problems/inbound_backpressure",
		"title":               "Inbound backpressure",
		"status":              http.StatusTooManyRequests,
		"code":                "inbound_backpressure",
		"detail":              "the trigger's inbound backlog is at capacity; retry later",
		"request_id":          middleware.RequestID(r.Context()),
		"retryable":           true,
		"retry_after_seconds": retry,
		"queue_depth":         bp.Depth,
		"oldest_age_seconds":  int(bp.OldestAge.Seconds()),
	})
}
