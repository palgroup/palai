package api

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"time"

	"github.com/palgroup/palai/apps/control-plane/api/middleware"
	"github.com/palgroup/palai/packages/contracts"
)

const (
	createRoute  = "/v1/responses"
	maxBodyBytes = 1 << 20 // 1 MiB request-body ceiling at the trust boundary.
)

// Admitter is the store seam for idempotent response admission. The Postgres store
// implements it in production; a fake implements it in the conformance tier.
type Admitter interface {
	AdmitResponse(ctx context.Context, req AdmitRequest) (AdmitResult, error)
}

// AdmitRequest carries a fully-resolved admission: the verified scope, the
// idempotency coordinates, the canonical request hash, and the minted IDs plus
// response body so a replay can return the exact original resource (spec §20.9).
type AdmitRequest struct {
	Scope          middleware.Scope
	IdempotencyKey string
	Method         string
	Route          string
	RequestHash    string
	ResponseID     string
	RunID          string
	SessionID      string
	Input          []byte
	Body           []byte
}

// AdmitResult is the admission outcome. Conflict marks a key reused with a
// different request; Replayed marks a duplicate of the same request. In both the
// created and replayed cases Body is the resource to return verbatim.
type AdmitResult struct {
	ResponseID string
	Body       []byte
	Replayed   bool
	Conflict   bool
}

type responseHandler struct {
	admitter Admitter
}

// create admits a response: it authenticates via the scope set by Auth, validates
// and canonicalizes the request, mints the transient resource, and delegates the
// atomic reservation-and-creation to the Admitter. Success is 202 + Location; a
// duplicate replays the original; a divergent reuse is 409.
func (h *responseHandler) create(w http.ResponseWriter, r *http.Request) {
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
	var req contracts.ResponseCreateRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		middleware.WriteProblem(w, r, http.StatusBadRequest, "invalid_request", "the request body is not valid JSON")
		return
	}
	if err := validateCreate(req); err != nil {
		middleware.WriteProblem(w, r, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}

	hash, err := canonicalRequestHash(req)
	if err != nil {
		middleware.WriteProblem(w, r, http.StatusInternalServerError, "internal_error", "")
		return
	}

	responseID := middleware.NewID("resp")
	runID := middleware.NewID("run")
	sessionID := middleware.NewID("ses")
	projection := contracts.Response{
		ID:             contracts.ResponseID(responseID),
		Object:         "response",
		Status:         "queued",
		CreatedAt:      time.Now().UTC().Format(time.RFC3339Nano),
		Model:          req.Model,
		Output:         []contracts.ContentItem{},
		Usage:          contracts.Usage{},
		SessionID:      contracts.SessionID(sessionID),
		RunID:          contracts.RunID(runID),
		OrganizationID: contracts.OrganizationID(scope.Organization),
		ProjectID:      contracts.ProjectID(scope.Project),
	}
	body, err := json.Marshal(projection)
	if err != nil {
		middleware.WriteProblem(w, r, http.StatusInternalServerError, "internal_error", "")
		return
	}
	input, err := json.Marshal(req.Input)
	if err != nil {
		middleware.WriteProblem(w, r, http.StatusInternalServerError, "internal_error", "")
		return
	}

	out, err := h.admitter.AdmitResponse(r.Context(), AdmitRequest{
		Scope:          scope,
		IdempotencyKey: middleware.IdempotencyKey(r.Context()),
		Method:         http.MethodPost,
		Route:          createRoute,
		RequestHash:    hash,
		ResponseID:     responseID,
		RunID:          runID,
		SessionID:      sessionID,
		Input:          input,
		Body:           body,
	})
	if err != nil {
		middleware.WriteProblem(w, r, http.StatusInternalServerError, "internal_error", "")
		return
	}
	if out.Conflict {
		middleware.WriteProblem(w, r, http.StatusConflict, "idempotency_mismatch", "the idempotency key was reused with a different request")
		return
	}

	// Created and replayed both return the stored resource; on a replay this is the
	// original body and id, not the freshly minted ones (spec §20.9 step 4).
	w.Header().Set("Location", createRoute+"/"+out.ResponseID)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusAccepted)
	_, _ = w.Write(out.Body)
}

// validateCreate enforces the two request invariants a malformed body can violate
// before any operation runs, so they are 400 invalid_request and never cached
// (spec §20.9 step 7). Full schema validation is a later task.
func validateCreate(req contracts.ResponseCreateRequest) error {
	if req.Input == nil {
		return errors.New("input is required")
	}
	if req.PreviousResponseID != nil && req.SessionID != nil {
		return errors.New("previous_response_id and session_id are mutually exclusive")
	}
	return nil
}

// canonicalRequestHash hashes the canonical semantic request (spec §20.9 step 2).
// Decoding into the typed contract normalizes the request: omitted fields collapse
// to their canonical defaults via omitempty and map keys marshal in sorted order,
// so semantically identical requests hash identically. Fuller server-default
// resolution is deferred.
func canonicalRequestHash(req contracts.ResponseCreateRequest) (string, error) {
	canonical, err := json.Marshal(req)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(canonical)
	return hex.EncodeToString(sum[:]), nil
}
