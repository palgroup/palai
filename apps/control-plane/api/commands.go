package api

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"

	"github.com/palgroup/palai/apps/control-plane/api/middleware"
	"github.com/palgroup/palai/packages/contracts"
)

// SessionManager is the store seam for the standalone session resource and its durable
// commands (spec §9.1, §22.4). The Postgres store implements it; production wires it, and the
// conformance HTTP tier (which never touches sessions) passes nil, so the routes stay unmounted
// there. Every method is scoped by the verified identity, never a request-body field (§39.2).
type SessionManager interface {
	CreateSession(ctx context.Context, scope middleware.Scope) (SessionResult, error)
	GetSession(ctx context.Context, scope middleware.Scope, id string) (SessionResult, error)
	AcceptCommand(ctx context.Context, scope middleware.Scope, sessionID string, req contracts.CommandCreateRequest) (CommandResult, error)
}

// SessionResult is a session projection. Found is false for an unknown or foreign id (404).
type SessionResult struct {
	Body  []byte
	Found bool
}

// CommandResult is a command projection. SessionNotFound is a command posted to an unknown or
// foreign session (404, no existence disclosure). The body is the durable command resource —
// on a duplicate command_id it is the original, unchanged (spec §22.4 idempotency).
type CommandResult struct {
	Body            []byte
	SessionNotFound bool
}

type sessionHandler struct {
	sessions SessionManager
}

// create opens a session (spec §9.1 POST /v1/sessions). It is 201 + Location + the session
// projection. Session creation is cheap and unkeyed: a retried create mints a new session.
func (h *sessionHandler) create(w http.ResponseWriter, r *http.Request) {
	scope, ok := middleware.ScopeFrom(r.Context())
	if !ok {
		middleware.WriteProblem(w, r, http.StatusUnauthorized, "authentication_required", "a bearer API key is required")
		return
	}
	out, err := h.sessions.CreateSession(r.Context(), scope)
	if err != nil {
		middleware.WriteProblem(w, r, http.StatusInternalServerError, "internal_error", "")
		return
	}
	w.Header().Set("Location", "/v1/sessions/"+sessionIDOf(out.Body))
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	_, _ = w.Write(out.Body)
}

// get reads a session within the verified scope (spec §9.1 GET). A hit is 200; an unknown or
// foreign id is 404 (never leaking a foreign session's existence).
func (h *sessionHandler) get(w http.ResponseWriter, r *http.Request) {
	scope, ok := middleware.ScopeFrom(r.Context())
	if !ok {
		middleware.WriteProblem(w, r, http.StatusUnauthorized, "authentication_required", "a bearer API key is required")
		return
	}
	out, err := h.sessions.GetSession(r.Context(), scope, r.PathValue("session_id"))
	if err != nil {
		middleware.WriteProblem(w, r, http.StatusInternalServerError, "internal_error", "")
		return
	}
	if !out.Found {
		middleware.WriteProblem(w, r, http.StatusNotFound, "not_found", "no such session in this project")
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(out.Body)
}

// command accepts a durable command against a session (spec §22.4, §9.2). Acceptance means
// durably queued, not applied: it is 202 with the command projection. A duplicate command_id
// returns the original resource (idempotent, via the command table's own unique). Delivery,
// steering, and rejection happen later at safe boundaries (the command pump). An unknown or
// foreign session is 404. Idempotency is carried by command_id, so no Idempotency-Key header.
func (h *sessionHandler) command(w http.ResponseWriter, r *http.Request) {
	scope, ok := middleware.ScopeFrom(r.Context())
	if !ok {
		middleware.WriteProblem(w, r, http.StatusUnauthorized, "authentication_required", "a bearer API key is required")
		return
	}
	raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxBodyBytes))
	if err != nil {
		middleware.WriteProblem(w, r, http.StatusBadRequest, "invalid_request", "the request body could not be read")
		return
	}
	var req contracts.CommandCreateRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		middleware.WriteProblem(w, r, http.StatusBadRequest, "invalid_request", "the request body is not valid JSON")
		return
	}
	if err := validateCommand(req); err != nil {
		middleware.WriteProblem(w, r, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	out, err := h.sessions.AcceptCommand(r.Context(), scope, r.PathValue("session_id"), req)
	if err != nil {
		middleware.WriteProblem(w, r, http.StatusInternalServerError, "internal_error", "")
		return
	}
	if out.SessionNotFound {
		middleware.WriteProblem(w, r, http.StatusNotFound, "not_found", "no such session in this project")
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusAccepted)
	_, _ = w.Write(out.Body)
}

// commandKinds and deliveryModes are the command surface through T3. change_config carries a
// model and/or tool-set change (spec §9.3); the remaining lifecycle kinds (pause/resume/cancel/
// fork/close) arrive with T4. approve/deny are accepted but rejected here (no approval source
// until E09 — the store rejects them typed).
var commandKinds = map[string]bool{"send_message": true, "change_config": true, "approve": true, "deny": true}
var deliveryModes = map[string]bool{"queue": true, "steer": true, "interrupt": true}

// validateCommand enforces the request invariants a malformed body can violate before any
// durable write, so they are 400 invalid_request (never a durable command). Fuller schema
// validation is a later task. A change_config's policy verdict is NOT decided here — an
// out-of-allowlist model/tool is a durable, typed command.rejected (the store), not a 400.
func validateCommand(req contracts.CommandCreateRequest) error {
	if req.CommandID == "" {
		return errors.New("command_id is required")
	}
	if !commandKinds[req.Kind] {
		return errors.New("kind must be one of send_message, change_config, approve, deny")
	}
	if req.Kind == "send_message" {
		if !deliveryModes[req.Delivery] {
			return errors.New("delivery must be one of queue, steer, interrupt")
		}
		if req.Message == "" {
			return errors.New("message is required for send_message")
		}
	}
	if req.Kind == "change_config" && req.Model == "" && req.Tools == nil {
		return errors.New("change_config requires a model or tools change")
	}
	return nil
}

// sessionIDOf reads the id from a session projection body so create can set Location.
func sessionIDOf(body []byte) string {
	var envelope struct {
		ID string `json:"id"`
	}
	_ = json.Unmarshal(body, &envelope)
	return envelope.ID
}
