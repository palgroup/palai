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
	// admissionRetryAfterSeconds is the Retry-After hint on a capacity 429 (§20.12): a run frees a
	// slot when it terminates, which the edge cannot predict, so a short fixed hint is honest.
	admissionRetryAfterSeconds = "1"
)

// Admitter is the store seam for response admission and retrieval. The Postgres store
// implements it in production; a fake implements it in the conformance tier.
type Admitter interface {
	AdmitResponse(ctx context.Context, req AdmitRequest) (AdmitResult, error)
	GetResponse(ctx context.Context, scope middleware.Scope, id string) (RetrieveResult, error)
	// ListResponses returns a tenant-scoped page of run history (spec §22.3, E13 T4). The store
	// runs under RLS, so the request scope confines the rows; the ListQuery carries only the keyset
	// position and the basic filters. It fetches Limit+1 rows so the handler detects a further page.
	ListResponses(ctx context.Context, scope middleware.Scope, q ListQuery) ([]ListRow, error)
	// CancelResponse cancels a response's run within scope and returns the terminal
	// projection to render. It shares retrieval's result: Found=false is a 404 (unknown or
	// foreign id), Purged is a 410, and a hit carries the canceled (or already-terminal)
	// projection. It is a monotonic no-op on an already-terminal run — cancel is retry-safe.
	CancelResponse(ctx context.Context, scope middleware.Scope, id string) (RetrieveResult, error)
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
	// RequestedSessionID / PreviousResponseID chain onto an existing session (spec §9). They
	// carry the request's opt-in ids verbatim; the store resolves them to the effective
	// session (or a tenant-scoped 404 / 409) — the minted SessionID opens a fresh session.
	RequestedSessionID *string
	PreviousResponseID *string
	Input              []byte
	Body               []byte
	// Store is the resolved §8.3 retention flag (default true) persisted on the response.
	Store bool
	// Delegations is the root run's required-delegation JSON ({"emit":[...],"budget":N}) or nil
	// (spec §25.18). Resolved from the raw body, it seeds the run.start delegations the engine emits.
	Delegations []byte
	// RepositoryBindingID / RepositoryRef carry the contracted `repository` field (spec §30.1, E09
	// Task 10): resolved from the raw body like Delegations, they attach a session-scoped coding
	// workspace the root run auto-provisions. Empty leaves the response non-coding.
	RepositoryBindingID string
	RepositoryRef       string
	// AgentRevisionID / RunTemplateRevisionID pin the run's executable config to a published revision
	// (spec §10, AGT-001). At most one is set (validateCreate rejects both). Empty leaves the run
	// profile-free.
	AgentRevisionID       string
	RunTemplateRevisionID string
	// MaxConcurrentRuns / MaxQueuedRuns are the §20.12 per-project run caps the handler resolves from
	// its configured AdmissionLimits. Zero on either disables that cap; the store enforces them against
	// live DB counters inside the admission transaction.
	MaxConcurrentRuns int
	MaxQueuedRuns     int
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
	// SessionNotFound is a chain onto an unknown or foreign session/response (404, no
	// existence disclosure); SessionConflict a chain onto a non-active session (409);
	// ActiveRunConflict a chain onto a session that already has a live root run (409,
	// one-active-root — spec §22.3). RepositoryBindingNotFound is a `repository` field
	// naming an unknown or foreign binding (404, spec §30.1).
	SessionNotFound           bool
	SessionConflict           bool
	ActiveRunConflict         bool
	RepositoryBindingNotFound bool
	// PinnedRevisionNotFound is an agent_revision_id / run_template_revision_id naming an unknown or
	// foreign revision (404); PinnedRevisionNotPublished is a pin onto a draft revision (409, spec §10).
	PinnedRevisionNotFound     bool
	PinnedRevisionNotPublished bool
	// ConcurrencyLimited / QueueDepthExceeded mark an admission the §20.12 per-project caps rejected —
	// too many executing runs, or a full queued backlog. Both render as 429 + Retry-After; the rejected
	// request created no run and no idempotency record, so a retry after the delay is safe.
	ConcurrencyLimited bool
	QueueDepthExceeded bool
}

type responseHandler struct {
	admitter Admitter
	// limits are the per-project run-admission caps (§20.12). Zero fields disable the caps, so a
	// stack that configures no edge limits admits exactly as before.
	limits AdmissionLimits
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

	// Required delegations ride the raw body (spec §25.18), parsed here like store — a Palai
	// extension the generated OpenAI-shaped contract does not carry. The root run stores them and
	// the orchestrator seeds run.start, so a real single-step run still delegates.
	delegations := resolveDelegations(raw)

	// The coding session's repository attachment rides the contracted `repository` field (spec §30.1):
	// {binding_id, ref}. Resolved here like delegations, it attaches the session-scoped workspace the
	// root run auto-provisions (E09 Task 10). An absent field leaves the response non-coding.
	bindingID, repositoryRef := resolveRepository(raw)

	// The pinned executable-config revision (spec §10, AGT-001): agent_revision_id OR
	// run_template_revision_id, typed contract fields so they ride the semantic request hash.
	agentRevisionID, templateRevisionID := "", ""
	if req.AgentRevisionID != nil {
		agentRevisionID = *req.AgentRevisionID
	}
	if req.RunTemplateRevisionID != nil {
		templateRevisionID = *req.RunTemplateRevisionID
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
		Scope:                 scope,
		IdempotencyKey:        middleware.IdempotencyKey(r.Context()),
		Method:                http.MethodPost,
		Route:                 createRoute,
		RequestHash:           hash,
		ResponseID:            responseID,
		RunID:                 runID,
		SessionID:             sessionID,
		RequestedSessionID:    req.SessionID,
		PreviousResponseID:    req.PreviousResponseID,
		Input:                 input,
		Body:                  body,
		Store:                 store,
		Delegations:           delegations,
		RepositoryBindingID:   bindingID,
		RepositoryRef:         repositoryRef,
		AgentRevisionID:       agentRevisionID,
		RunTemplateRevisionID: templateRevisionID,
		MaxConcurrentRuns:     h.limits.MaxConcurrentRuns,
		MaxQueuedRuns:         h.limits.MaxQueuedRuns,
	})
	if err != nil {
		middleware.WriteProblem(w, r, http.StatusInternalServerError, "internal_error", "")
		return
	}
	// A chain onto an unknown/foreign session is a tenant-scoped 404 (no existence
	// disclosure); a chain onto a non-active session is a 409 (spec §9, §22.1).
	if out.SessionNotFound {
		middleware.WriteProblem(w, r, http.StatusNotFound, "not_found", "no such session in this project")
		return
	}
	// A `repository` field naming an unknown or foreign binding is a tenant-scoped 404 (spec §30.1).
	if out.RepositoryBindingNotFound {
		middleware.WriteProblem(w, r, http.StatusNotFound, "not_found", "no such repository binding in this project")
		return
	}
	// A pin naming an unknown revision is a tenant-scoped 404; a pin onto a draft is a 409 — a draft
	// revision cannot be run until it is published (spec §10, AGT-001).
	if out.PinnedRevisionNotFound {
		middleware.WriteProblem(w, r, http.StatusNotFound, "not_found", "no such agent revision or run template in this project")
		return
	}
	if out.PinnedRevisionNotPublished {
		middleware.WriteProblem(w, r, http.StatusConflict, "revision_not_published", "the pinned revision is a draft; publish it before running it")
		return
	}
	// The §20.12 per-project run caps: too many executing runs, or a full queued backlog. Both are the
	// §20.10 stable 429 concurrency_exceeded (the registered admission-capacity code — there is no
	// separate public queued code; the detail distinguishes them) + Retry-After. The admission created
	// nothing, so a retry after the delay is safe. ponytail: a fixed 1s hint; a slot frees when a run
	// terminates, which is not a clock the edge can predict, so a short constant beats a fabricated deadline.
	if out.ConcurrencyLimited {
		w.Header().Set("Retry-After", admissionRetryAfterSeconds)
		middleware.WriteProblem(w, r, http.StatusTooManyRequests, "concurrency_exceeded", "the project has too many concurrent runs; retry shortly")
		return
	}
	if out.QueueDepthExceeded {
		w.Header().Set("Retry-After", admissionRetryAfterSeconds)
		middleware.WriteProblem(w, r, http.StatusTooManyRequests, "concurrency_exceeded", "the project's run queue is full; retry shortly")
		return
	}
	if out.SessionConflict {
		middleware.WriteProblem(w, r, http.StatusConflict, "session_not_active", "the session is not active and cannot accept a new response")
		return
	}
	// One-active-root (spec §22.3): the session already has a non-terminal root run, so a new
	// response cannot open a second concurrent root. The client retries once the live run ends.
	if out.ActiveRunConflict {
		middleware.WriteProblem(w, r, http.StatusConflict, "active_run_exists", "the session already has an active run; wait for it to finish before starting another")
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

// list returns a tenant-scoped page of run history (spec §22.3, E13 T4): GET /v1/responses. The
// page is confined to the verified scope by RLS — there is no org/project query parameter (that
// would be an IDOR) — and supports the two basic filters (?status=, ?created_after=/?created_before=)
// plus opaque cursor paging (?after=, ?limit=). A foreign or malformed cursor is a 400 invalid_cursor.
func (h *responseHandler) list(w http.ResponseWriter, r *http.Request) {
	scope, ok := middleware.ScopeFrom(r.Context())
	if !ok {
		middleware.WriteProblem(w, r, http.StatusUnauthorized, "authentication_required", "a bearer API key is required")
		return
	}
	q, ok := beginList(w, r, "responses", scope)
	if !ok {
		return
	}
	rows, err := h.admitter.ListResponses(r.Context(), scope, q)
	if err != nil {
		middleware.WriteProblem(w, r, http.StatusInternalServerError, "internal_error", "")
		return
	}
	renderPage(w, r, "responses", scope, rows, q.Limit)
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

// cancel cancels a response's run within the verified scope and returns its terminal
// projection. Success is 202 with the canonical response projection (OpenAPI cancelResponse);
// an unknown or foreign id is 404 (never leaking a foreign response's existence); a reaped
// store:false resource is 410. Cancel is naturally idempotent, so the route carries no
// idempotency key, and canceling an already-terminal response is a safe no-op (spec §22.3).
func (h *responseHandler) cancel(w http.ResponseWriter, r *http.Request) {
	scope, ok := middleware.ScopeFrom(r.Context())
	if !ok {
		middleware.WriteProblem(w, r, http.StatusUnauthorized, "authentication_required", "a bearer API key is required")
		return
	}
	out, err := h.admitter.CancelResponse(r.Context(), scope, r.PathValue("response_id"))
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
	w.WriteHeader(http.StatusAccepted)
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

// resolveDelegations parses the required delegations (and the optional parent budget children
// intersect against) from the raw create body into the run's delegation JSON — {"emit":[...],
// "budget":N} — or nil when none are configured (spec §25.18). Each spec passes through verbatim;
// the engine emits it as a child.request and the controller admits it.
func resolveDelegations(raw []byte) []byte {
	var probe struct {
		Delegations []json.RawMessage `json:"delegations"`
		Budget      int               `json:"delegation_budget"`
	}
	if err := json.Unmarshal(raw, &probe); err != nil || len(probe.Delegations) == 0 {
		return nil
	}
	envelope := map[string]any{"emit": probe.Delegations}
	if probe.Budget > 0 {
		envelope["budget"] = probe.Budget
	}
	out, err := json.Marshal(envelope)
	if err != nil {
		return nil
	}
	return out
}

// resolveRepository parses the coding session's repository attachment from the raw create body — the
// already-contracted `repository` field (spec §30.1): {binding_id, ref}. Resolved raw like
// resolveDelegations because it drives admission (attaching the session workspace), not the semantic
// request hash. An absent field or empty binding_id yields "", "" — a non-coding response.
func resolveRepository(raw []byte) (bindingID, ref string) {
	var probe struct {
		Repository struct {
			BindingID string `json:"binding_id"`
			Ref       string `json:"ref"`
		} `json:"repository"`
	}
	if err := json.Unmarshal(raw, &probe); err != nil {
		return "", ""
	}
	return probe.Repository.BindingID, probe.Repository.Ref
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
	// A run pins EITHER an agent revision OR a run template, never both (spec §10, §32.2): an agent
	// revision carries identity, a template is profile-free, so one request cannot mean both.
	if req.AgentRevisionID != nil && req.RunTemplateRevisionID != nil {
		return errors.New("agent_revision_id and run_template_revision_id are mutually exclusive")
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
