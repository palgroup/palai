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

// Admitter is the store seam for response admission and retrieval. The Postgres store
// implements it in production; a fake implements it in the conformance tier.
type Admitter interface {
	AdmitResponse(ctx context.Context, req AdmitRequest) (AdmitResult, error)
	GetResponse(ctx context.Context, scope middleware.Scope, id string) (RetrieveResult, error)
}

// RetrieveResult is the outcome of a response retrieval. Found is false for an unknown
// or out-of-scope id (404); Purged is true once the content has been reaped (410); Body
// is the committed terminal projection to return verbatim on a hit (200).
type RetrieveResult struct {
	Body   []byte
	Found  bool
	Purged bool
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
	// Store is the resolved §8.3 retention flag (default true) persisted on the response.
	Store bool
}

// AdmitResult is the admission outcome. Conflict marks a key reused with a
// different request; Replayed marks a duplicate of the same request; Purged marks a
// matching replay whose result has been reaped (410, no re-execution). In the created
// and replayed cases Body is the resource to return verbatim; on Purged, ResponseID is
// the tombstoned resource's identity.
type AdmitResult struct {
	ResponseID string
	Body       []byte
	Replayed   bool
	Conflict   bool
	Purged     bool
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

	// store defaults true (§8.3): the generated contract can't distinguish an absent
	// flag from an explicit false, so an absent field resolves to persistent.
	store := resolveStore(raw)

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
		Store:          store,
	})
	if err != nil {
		middleware.WriteProblem(w, r, http.StatusInternalServerError, "internal_error", "")
		return
	}
	if out.Conflict {
		middleware.WriteProblem(w, r, http.StatusConflict, "idempotency_mismatch", "the idempotency key was reused with a different request")
		return
	}
	// A replay whose transient result was reaped is a tombstone: 410, no re-execution.
	// Location carries the original operation identity (spec §20.9).
	if out.Purged {
		w.Header().Set("Location", createRoute+"/"+out.ResponseID)
		middleware.WriteProblem(w, r, http.StatusGone, "idempotency_result_expired", "the idempotent result has been reaped and is no longer available")
		return
	}

	// Created and replayed both return the stored resource; on a replay this is the
	// original body and id, not the freshly minted ones (spec §20.9 step 4).
	w.Header().Set("Location", createRoute+"/"+out.ResponseID)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusAccepted)
	_, _ = w.Write(out.Body)
}

// get retrieves a response's terminal projection within the verified scope. A hit is
// 200 with the committed projection; an unknown or foreign id is 404 (never leaking a
// foreign response's existence); a reaped store:false resource is 410 retention_expired
// (spec §8.3, §22.3, §39.2).
func (h *responseHandler) get(w http.ResponseWriter, r *http.Request) {
	scope, ok := middleware.ScopeFrom(r.Context())
	if !ok {
		middleware.WriteProblem(w, r, http.StatusUnauthorized, "authentication_required", "a bearer API key is required")
		return
	}
	out, err := h.admitter.GetResponse(r.Context(), scope, r.PathValue("response_id"))
	if err != nil {
		middleware.WriteProblem(w, r, http.StatusInternalServerError, "internal_error", "")
		return
	}
	if !out.Found {
		middleware.WriteProblem(w, r, http.StatusNotFound, "not_found", "no such response in this project")
		return
	}
	if out.Purged {
		middleware.WriteProblem(w, r, http.StatusGone, "retention_expired", "the response content has been reaped and is no longer available")
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(out.Body)
}

// resolveStore reads the store flag from the raw request, honoring §8.3's true default:
// an absent field is persistent; only an explicit false opts into transient retention.
// The generated bool contract can't carry this tri-state, so the raw body is reprobed.
func resolveStore(raw []byte) bool {
	var probe struct {
		Store *bool `json:"store"`
	}
	if err := json.Unmarshal(raw, &probe); err != nil || probe.Store == nil {
		return true
	}
	return *probe.Store
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
