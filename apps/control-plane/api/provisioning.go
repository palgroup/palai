package api

import (
	"context"
	"io"
	"net/http"

	"github.com/palgroup/palai/apps/control-plane/api/middleware"
)

// ProvisioningAPI is the store seam for the tenancy provisioning surface (spec §39.2, E13 Task 2,
// TEN-003/MCI-001): organizations, projects (+ the §14 config_policy write-path), and API keys. The
// Postgres-backed internal/identity store implements it; production wires it, and tiers that never
// provision pass nil so the routes stay unmounted. Every method is scoped by the verified identity,
// never a request-body field. Organization CREATION is the single genuinely cross-tenant operation (it
// establishes a new tenant, like bootstrap) — every other method acts within the caller's own organization.
type ProvisioningAPI interface {
	CreateOrganization(ctx context.Context, scope middleware.Scope, body []byte) (ProvisionResult, error)
	ListOrganizations(ctx context.Context, scope middleware.Scope) (ProvisionResult, error)
	GetOrganization(ctx context.Context, scope middleware.Scope, id string) (ProvisionResult, error)

	CreateProject(ctx context.Context, scope middleware.Scope, body []byte) (ProvisionResult, error)
	ListProjects(ctx context.Context, scope middleware.Scope) (ProvisionResult, error)
	GetProject(ctx context.Context, scope middleware.Scope, id string) (ProvisionResult, error)
	UpdateProjectPolicy(ctx context.Context, scope middleware.Scope, id string, body []byte) (ProvisionResult, error)

	CreateAPIKey(ctx context.Context, scope middleware.Scope, body []byte) (ProvisionResult, error)
	ListAPIKeys(ctx context.Context, scope middleware.Scope) (ProvisionResult, error)
	GetAPIKey(ctx context.Context, scope middleware.Scope, id string) (ProvisionResult, error)
	RevokeAPIKey(ctx context.Context, scope middleware.Scope, id string) (ProvisionResult, error)
}

// ProvisionResult is a provisioning projection. Exactly one outcome is set: Body carries the created/read
// resource (2xx); MissingField marks a required body field absent (400); BadField marks a body outside the
// strict schema — an unknown field (400); NotFound marks an absent/foreign org, project, key, or a key's
// referenced project (404, leaking no cross-tenant existence). The API-key create Body is the ONLY place a
// key's plaintext appears; every read renders metadata only.
type ProvisionResult struct {
	Body         []byte
	MissingField string
	BadField     bool
	NotFound     bool
	// Conflict renders 409: a well-formed request that a current-state precondition refuses (E17 T5
	// retrieval uses it for a missed freshness deadline under the fail policy, and for a disabled vector
	// strategy). Other provisioning surfaces leave it false, so their renderers are unaffected.
	Conflict bool
}

// provisionScope is the capability a key must hold to administer tenancy. A key with an empty scope set
// (an admin/bootstrap key) holds it implicitly (Scope.HasScope). HONEST CEILING: this is the whole
// authorization model for T2 — any key that can provision can provision the whole organization; named
// roles/relationships are E13-H/E17.
const provisionScope = "provision"

type provisioningHandler struct {
	provisioning ProvisioningAPI
}

func (h *provisioningHandler) createOrganization(w http.ResponseWriter, r *http.Request) {
	scope, raw, ok := h.begin(w, r)
	if !ok {
		return
	}
	out, err := h.provisioning.CreateOrganization(r.Context(), scope, raw)
	h.write(w, r, out, err, http.StatusCreated, "/v1/organizations/")
}

func (h *provisioningHandler) listOrganizations(w http.ResponseWriter, r *http.Request) {
	scope, ok := h.authorize(w, r)
	if !ok {
		return
	}
	out, err := h.provisioning.ListOrganizations(r.Context(), scope)
	h.write(w, r, out, err, http.StatusOK, "")
}

func (h *provisioningHandler) getOrganization(w http.ResponseWriter, r *http.Request) {
	scope, ok := h.authorize(w, r)
	if !ok {
		return
	}
	out, err := h.provisioning.GetOrganization(r.Context(), scope, r.PathValue("organization_id"))
	h.write(w, r, out, err, http.StatusOK, "")
}

func (h *provisioningHandler) createProject(w http.ResponseWriter, r *http.Request) {
	scope, raw, ok := h.begin(w, r)
	if !ok {
		return
	}
	out, err := h.provisioning.CreateProject(r.Context(), scope, raw)
	h.write(w, r, out, err, http.StatusCreated, "/v1/projects/")
}

func (h *provisioningHandler) listProjects(w http.ResponseWriter, r *http.Request) {
	scope, ok := h.authorize(w, r)
	if !ok {
		return
	}
	out, err := h.provisioning.ListProjects(r.Context(), scope)
	h.write(w, r, out, err, http.StatusOK, "")
}

func (h *provisioningHandler) getProject(w http.ResponseWriter, r *http.Request) {
	scope, ok := h.authorize(w, r)
	if !ok {
		return
	}
	out, err := h.provisioning.GetProject(r.Context(), scope, r.PathValue("project_id"))
	h.write(w, r, out, err, http.StatusOK, "")
}

// patchProject writes the §14 project-layer config_policy (strict schema, unknown-field reject). It is the
// first API that makes the resolver's project layer reachable.
func (h *provisioningHandler) patchProject(w http.ResponseWriter, r *http.Request) {
	scope, raw, ok := h.begin(w, r)
	if !ok {
		return
	}
	out, err := h.provisioning.UpdateProjectPolicy(r.Context(), scope, r.PathValue("project_id"), raw)
	h.write(w, r, out, err, http.StatusOK, "")
}

func (h *provisioningHandler) createAPIKey(w http.ResponseWriter, r *http.Request) {
	scope, raw, ok := h.begin(w, r)
	if !ok {
		return
	}
	out, err := h.provisioning.CreateAPIKey(r.Context(), scope, raw)
	h.write(w, r, out, err, http.StatusCreated, "/v1/api-keys/")
}

func (h *provisioningHandler) listAPIKeys(w http.ResponseWriter, r *http.Request) {
	scope, ok := h.authorize(w, r)
	if !ok {
		return
	}
	out, err := h.provisioning.ListAPIKeys(r.Context(), scope)
	h.write(w, r, out, err, http.StatusOK, "")
}

func (h *provisioningHandler) getAPIKey(w http.ResponseWriter, r *http.Request) {
	scope, ok := h.authorize(w, r)
	if !ok {
		return
	}
	out, err := h.provisioning.GetAPIKey(r.Context(), scope, r.PathValue("key_id"))
	h.write(w, r, out, err, http.StatusOK, "")
}

// revokeAPIKey is naturally idempotent (revoked_at is monotonic), so it carries no Idempotency-Key.
func (h *provisioningHandler) revokeAPIKey(w http.ResponseWriter, r *http.Request) {
	scope, ok := h.authorize(w, r)
	if !ok {
		return
	}
	out, err := h.provisioning.RevokeAPIKey(r.Context(), scope, r.PathValue("key_id"))
	h.write(w, r, out, err, http.StatusOK, "")
}

// authorize resolves the verified scope and enforces the provision capability. It backs the routes that
// carry no request body (GET/LIST/revoke).
func (h *provisioningHandler) authorize(w http.ResponseWriter, r *http.Request) (middleware.Scope, bool) {
	scope, ok := middleware.ScopeFrom(r.Context())
	if !ok {
		middleware.WriteProblem(w, r, http.StatusUnauthorized, "authentication_required", "a bearer API key is required")
		return middleware.Scope{}, false
	}
	if !scope.HasScope(provisionScope) {
		middleware.WriteProblem(w, r, http.StatusForbidden, "insufficient_scope", "this API key lacks the provision capability")
		return middleware.Scope{}, false
	}
	return scope, true
}

// begin authorizes and reads the bounded body, shared by the create/patch handlers.
func (h *provisioningHandler) begin(w http.ResponseWriter, r *http.Request) (middleware.Scope, []byte, bool) {
	scope, ok := h.authorize(w, r)
	if !ok {
		return middleware.Scope{}, nil, false
	}
	raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxBodyBytes))
	if err != nil {
		middleware.WriteProblem(w, r, http.StatusBadRequest, "invalid_request", "the request body could not be read")
		return middleware.Scope{}, nil, false
	}
	return scope, raw, true
}

// write renders a provisioning outcome: the typed rejects first, then 2xx with the resource (and a
// Location header for a create).
func (h *provisioningHandler) write(w http.ResponseWriter, r *http.Request, out ProvisionResult, err error, okStatus int, locationPrefix string) {
	if err != nil {
		middleware.WriteProblem(w, r, http.StatusInternalServerError, "internal_error", "")
		return
	}
	switch {
	case out.MissingField != "":
		middleware.WriteProblem(w, r, http.StatusBadRequest, "invalid_request", out.MissingField+" is required")
		return
	case out.BadField:
		middleware.WriteProblem(w, r, http.StatusBadRequest, "invalid_request", "the request body carries an unsupported field")
		return
	case out.NotFound:
		middleware.WriteProblem(w, r, http.StatusNotFound, "not_found", "no such organization, project, or API key in this scope")
		return
	}
	if locationPrefix != "" {
		w.Header().Set("Location", locationPrefix+resourceIDOf(out.Body))
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(okStatus)
	_, _ = w.Write(out.Body)
}
