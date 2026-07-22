package api

import (
	"context"
	"io"
	"net/http"

	"github.com/palgroup/palai/apps/control-plane/api/middleware"
)

// SecretRefAPI is the store seam for the restart-less secret-ref write-path (E13 Task 3, SEC-002/MCI-002):
// a tenant POSTs a secret VALUE (write-only — it never comes back) and reads only metadata
// (name/version/updated_at). The Postgres-backed internal/identity SecretStore implements it; production
// wires it via WithSecretRefs when a master key is configured, and tiers that never resolve secrets leave
// it unset so the routes stay unmounted. Every method is scoped by the verified identity, never a body
// field; the value is envelope-encrypted at rest and has NO read-back path.
type SecretRefAPI interface {
	CreateSecretRef(ctx context.Context, scope middleware.Scope, body []byte) (ProvisionResult, error)
	ListSecretRefs(ctx context.Context, scope middleware.Scope) (ProvisionResult, error)
	GetSecretRef(ctx context.Context, scope middleware.Scope, name string) (ProvisionResult, error)
	RotateSecretRef(ctx context.Context, scope middleware.Scope, name string, body []byte) (ProvisionResult, error)
}

// secretRefHandler renders the secret-ref routes. It reuses ProvisionResult (the generic management-write
// projection) and the provision-capability gate — secret management is an org-admin operation, exactly like
// tenancy provisioning. The auth/render plumbing is kept local rather than shared with provisioningHandler
// so this surface stays decoupled from T2's tested handler.
type secretRefHandler struct {
	secrets SecretRefAPI
}

func (h *secretRefHandler) create(w http.ResponseWriter, r *http.Request) {
	scope, raw, ok := h.begin(w, r)
	if !ok {
		return
	}
	out, err := h.secrets.CreateSecretRef(r.Context(), scope, raw)
	h.write(w, r, out, err, http.StatusCreated)
}

func (h *secretRefHandler) list(w http.ResponseWriter, r *http.Request) {
	scope, ok := h.authorize(w, r)
	if !ok {
		return
	}
	out, err := h.secrets.ListSecretRefs(r.Context(), scope)
	h.write(w, r, out, err, http.StatusOK)
}

func (h *secretRefHandler) get(w http.ResponseWriter, r *http.Request) {
	scope, ok := h.authorize(w, r)
	if !ok {
		return
	}
	out, err := h.secrets.GetSecretRef(r.Context(), scope, r.PathValue("secret_name"))
	h.write(w, r, out, err, http.StatusOK)
}

// rotate inserts a new version for an existing secret (the name comes from the path, the value from the
// body). A rotation of a never-created name is a 404 (the store detects no prior version).
func (h *secretRefHandler) rotate(w http.ResponseWriter, r *http.Request) {
	scope, raw, ok := h.begin(w, r)
	if !ok {
		return
	}
	out, err := h.secrets.RotateSecretRef(r.Context(), scope, r.PathValue("secret_name"), raw)
	h.write(w, r, out, err, http.StatusOK)
}

// authorize resolves the verified scope and enforces the provision capability. It backs the read routes;
// begin wraps it for the write routes.
func (h *secretRefHandler) authorize(w http.ResponseWriter, r *http.Request) (middleware.Scope, bool) {
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

func (h *secretRefHandler) begin(w http.ResponseWriter, r *http.Request) (middleware.Scope, []byte, bool) {
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

// write renders a secret-ref outcome: the typed rejects first, then 2xx with the metadata projection. There
// is no Location header (a secret-ref is addressed by name, already in the path) and — the load-bearing
// invariant — the Body a create/rotate returns is metadata ONLY: the value is never echoed.
func (h *secretRefHandler) write(w http.ResponseWriter, r *http.Request, out ProvisionResult, err error, okStatus int) {
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
		middleware.WriteProblem(w, r, http.StatusNotFound, "not_found", "no such secret ref in this scope")
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(okStatus)
	_, _ = w.Write(out.Body)
}
