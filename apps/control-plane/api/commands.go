package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"

	"github.com/palgroup/palai/apps/control-plane/api/middleware"
	"github.com/palgroup/palai/packages/contracts"
)

// SessionManager is the store seam for the standalone session resource and its durable
// commands (spec §9.1, §22.4). The Postgres store implements it; production wires it, and the
// conformance HTTP tier (which never touches sessions) passes nil, so the routes stay unmounted
// there. Every method is scoped by the verified identity, never a request-body field (§39.2).
type SessionManager interface {
	CreateSession(ctx context.Context, scope middleware.Scope, name string) (SessionResult, error)
	GetSession(ctx context.Context, scope middleware.Scope, id string) (SessionResult, error)
	// ListSessions is the E13 T4 read side: a tenant-scoped page of sessions, confined by RLS,
	// with cursor + status/created_at filters.
	ListSessions(ctx context.Context, scope middleware.Scope, q ListQuery) ([]ListRow, error)
	// RenameSession sets a session's operator label (E29). It is a separate verb from CreateSession
	// because most sessions are never created through it: admission opens one implicitly on the first
	// POST /v1/responses, so a create-time name would leave exactly the sessions a Sessions screen is
	// full of with no way to be labelled.
	RenameSession(ctx context.Context, scope middleware.Scope, id, name string) (SessionResult, error)
	// SetSessionAutoApprove arms or disarms the session's two approval families (E30 T1). It takes the
	// two halves as separate arguments rather than a struct with a combined flag, so that a caller
	// physically cannot ask for "auto-approve" without saying WHICH — the signature carries the split
	// that the columns, the schema and the screen all carry.
	//
	// There is no principal parameter: the store stamps it from the verified scope, the same rule
	// ApprovalDecision states for a click.
	SetSessionAutoApprove(ctx context.Context, scope middleware.Scope, id string, tools, publications bool) (SessionResult, error)
	AcceptCommand(ctx context.Context, scope middleware.Scope, sessionID string, req contracts.CommandCreateRequest) (CommandResult, error)
}

// maxSessionNameRunes bounds an operator label. It counts RUNES, matching the schema's maxLength (JSON
// Schema counts characters, not bytes), so "Gece Doğrulama" costs 14 here and not 16.
const maxSessionNameRunes = 200

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
	// accounts releases the per-session uid when a session closes (macOS). Nil in every deployment that
	// mints none, which is all of them until PALAI_SESSION_ACCOUNT_HELPER is set.
	accounts SessionAccountReleaser
}

// SessionAccountReleaser is the half of the session-account lifecycle this layer owns: the release, after
// the close has COMMITTED. The acquire half lives where a session first does work
// (execution.Orchestrator.provisionFreshAllocation) and the two are wired from one value, because a
// deployment that acquired without releasing would mint an account per session that nothing removes.
type SessionAccountReleaser interface {
	Release(ctx context.Context, sessionID string) error
}

// SetSessionAccounts wires the release. It is a setter for the reason every seam in this tree is: the
// composition root decides, and every existing caller compiles and behaves unchanged.
func (h *sessionHandler) SetSessionAccounts(a SessionAccountReleaser) { h.accounts = a }

// create opens a session (spec §9.1 POST /v1/sessions). It is 201 + Location + the session
// projection. Session creation is cheap and unkeyed: a retried create mints a new session.
//
// The body is OPTIONAL and carries at most a label (E29). An absent body is the pre-E29 request
// verbatim, so every existing caller is unchanged; a present one is decoded strictly, so a typo'd
// field is a 400 rather than a name the caller believes it set and did not.
func (h *sessionHandler) create(w http.ResponseWriter, r *http.Request) {
	scope, ok := middleware.ScopeFrom(r.Context())
	if !ok {
		middleware.WriteProblem(w, r, http.StatusUnauthorized, "authentication_required", "a bearer API key is required")
		return
	}
	req, ok := decodeSessionWrite(w, r)
	if !ok {
		return
	}
	name := ""
	if req.Name != nil {
		name = *req.Name
	}
	out, err := h.sessions.CreateSession(r.Context(), scope, name)
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

// list returns a tenant-scoped page of sessions (spec §9.1 GET /v1/sessions), confined to the
// verified scope by RLS. Supports the basic filters (?status=, created_at bounds) and cursor paging.
func (h *sessionHandler) list(w http.ResponseWriter, r *http.Request) {
	scope, ok := middleware.ScopeFrom(r.Context())
	if !ok {
		middleware.WriteProblem(w, r, http.StatusUnauthorized, "authentication_required", "a bearer API key is required")
		return
	}
	q, ok := beginList(w, r, "sessions", scope)
	if !ok {
		return
	}
	rows, err := h.sessions.ListSessions(r.Context(), scope, q)
	if err != nil {
		middleware.WriteProblem(w, r, http.StatusInternalServerError, "internal_error", "")
		return
	}
	renderPage(w, r, "sessions", scope, rows, q.Limit)
}

// rename sets a session's operator label (E29 PATCH /v1/sessions/{session_id}). It is 200 with the
// re-read projection, so the caller sees the label AND the name_source that now reports "operator".
//
// `name` must be PRESENT. An empty string is a real value — it clears the label and returns the row to
// its derived one — but an absent field is a 400: a rename request that carries no name is a request
// that lost its field, and silently succeeding at nothing is how a caller comes to believe a rename
// happened that did not.
func (h *sessionHandler) rename(w http.ResponseWriter, r *http.Request) {
	scope, ok := middleware.ScopeFrom(r.Context())
	if !ok {
		middleware.WriteProblem(w, r, http.StatusUnauthorized, "authentication_required", "a bearer API key is required")
		return
	}
	req, ok := decodeSessionWrite(w, r)
	if !ok {
		return
	}

	// THE AUTO-APPROVE ARM (E30 T1) SHARES THIS ROUTE, and the two changes are kept apart rather than
	// merged, for a reason this tree has paid for before: a request that carries only a name must not
	// touch the standing authorization, and a request that carries only the flags must not blank the
	// label. Both fields are POINTERS in the generated contract precisely so absent and false are
	// distinguishable — with a plain bool, `PATCH {"name":"x"}` would decode both flags as false and
	// SILENTLY DISARM a session the operator armed a minute ago.
	if req.AutoApproveTools != nil || req.AutoApprovePublications != nil {
		// A PATCH that names one half leaves the other EXACTLY as it was, which means reading the current
		// state first. Sending false for the unnamed half would make "arm publications" a way to disarm
		// tools, and the whole point of two columns is that one is never a lever on the other.
		current, err := h.sessions.GetSession(r.Context(), scope, r.PathValue("session_id"))
		if err != nil {
			middleware.WriteProblem(w, r, http.StatusInternalServerError, "internal_error", "")
			return
		}
		if !current.Found {
			middleware.WriteProblem(w, r, http.StatusNotFound, "not_found", "no such session in this project")
			return
		}
		tools, publications, ok := currentAutoApprove(w, r, current.Body)
		if !ok {
			return
		}
		if req.AutoApproveTools != nil {
			tools = *req.AutoApproveTools
		}
		if req.AutoApprovePublications != nil {
			publications = *req.AutoApprovePublications
		}
		out, err := h.sessions.SetSessionAutoApprove(r.Context(), scope, r.PathValue("session_id"), tools, publications)
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
		return
	}

	if req.Name == nil {
		middleware.WriteProblem(w, r, http.StatusBadRequest, "invalid_request",
			"name is required (or an auto_approve_tools / auto_approve_publications field)")
		return
	}
	out, err := h.sessions.RenameSession(r.Context(), scope, r.PathValue("session_id"), *req.Name)
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

// currentAutoApprove reads the two flags back out of a rendered session projection, so a PATCH that
// names one half can leave the other untouched.
//
// It parses the PROJECTION rather than taking a second store method, because the projection is the one
// shape both GET and PATCH already agree on — a separate read path here would be a second answer to
// "what is this session's standing authorization", and two answers is how they come to disagree.
func currentAutoApprove(w http.ResponseWriter, r *http.Request, body []byte) (tools, publications, ok bool) {
	var current contracts.Session
	if err := json.Unmarshal(body, &current); err != nil {
		middleware.WriteProblem(w, r, http.StatusInternalServerError, "internal_error", "")
		return false, false, false
	}
	return current.AutoApproveTools, current.AutoApprovePublications, true
}

// decodeSessionWrite reads the shared {name} body both session write routes take. An EMPTY body is a
// valid absent-name request (POST /v1/sessions has always been bodyless and stays so). Decoding is
// STRICT — an unknown field is a 400 — for the reason this tree keeps rediscovering: a silently
// ignored field is a caller acting on a change that never landed.
func decodeSessionWrite(w http.ResponseWriter, r *http.Request) (contracts.SessionWrite, bool) {
	raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxBodyBytes))
	if err != nil {
		middleware.WriteProblem(w, r, http.StatusBadRequest, "invalid_request", "the request body could not be read")
		return contracts.SessionWrite{}, false
	}
	var req contracts.SessionWrite
	if len(bytes.TrimSpace(raw)) > 0 {
		dec := json.NewDecoder(bytes.NewReader(raw))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&req); err != nil {
			middleware.WriteProblem(w, r, http.StatusBadRequest, "invalid_request", "the request body is not a valid session write")
			return contracts.SessionWrite{}, false
		}
	}
	if req.Name != nil && len([]rune(*req.Name)) > maxSessionNameRunes {
		middleware.WriteProblem(w, r, http.StatusBadRequest, "invalid_request",
			"name must be at most 200 characters")
		return contracts.SessionWrite{}, false
	}
	return req, true
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
	// THE ACCOUNT GOES WHEN THE SESSION DOES, and this is the only place that can do it: applyCloseSessionTx
	// runs INSIDE the transaction, and destroying a local account is an exec that must not be attempted
	// while a database transaction is open — a slow `sudo` there holds a session row lock, and a rollback
	// after it cannot un-delete an account.
	//
	// So it happens here, after AcceptCommand has committed. Best effort by contract: the session IS closed,
	// and refusing the caller because a uid could not be removed would report a failure for something that
	// succeeded. The slot stays held until an operator reconciles (`palai agentd reconciliation --mode accounts`),
	// which is the same trade Release itself makes — losing a slot is cheaper than handing a live account's
	// index to the next session.
	if req.Kind == "close_session" && h.accounts != nil {
		if err := h.accounts.Release(r.Context(), r.PathValue("session_id")); err != nil {
			log.Printf("session %s closed, account not released: %v", r.PathValue("session_id"), err)
		}
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusAccepted)
	_, _ = w.Write(out.Body)
}

// commandKinds and deliveryModes are the command surface. send_message carries queue/steer/
// interrupt delivery (spec §9.2); change_config a model and/or tool-set change (spec §9.3); the
// lifecycle kinds pause/resume (run wait/resume — spec §22.3), fork_session (§22.8), and
// close_session (§22.1) arrive with T4. clear resets a session's history WITHOUT closing it
// (E21 T1) — fork_session's sibling that does not copy. cancel stays the response-scoped endpoint,
// not a command kind. approve/deny are accepted but rejected (no approval source until E09 — the
// store rejects them typed).
//
// This map IS the surface: a kind the store handles but this list omits is a kind no HTTP caller can
// ever send, so adding one here is not bookkeeping.
var commandKinds = map[string]bool{
	"send_message": true, "change_config": true, "approve": true, "deny": true,
	"pause": true, "resume": true, "fork_session": true, "close_session": true, "clear": true,
}
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
		return errors.New("kind must be one of send_message, change_config, approve, deny, pause, resume, fork_session, close_session, clear")
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
