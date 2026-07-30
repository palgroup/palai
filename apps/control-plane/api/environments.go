package api

import (
	"context"
	"io"
	"net/http"

	"github.com/palgroup/palai/apps/control-plane/api/middleware"
)

// EnvironmentAPI is the store seam for the environment surface (E25 T3, migration 000046): a named group
// of key→value pairs an agent's shell receives. The Postgres-backed internal/identity SecretStore
// implements it — the SAME object that implements SecretRefAPI, because an environment value IS a
// secret_refs version under the derived name `env:<environment_id>:<key>`. Production wires it via
// WithEnvironments alongside WithSecretRefs when a master key is configured; a tier with no master key
// mounts neither family, because an environment with no way to seal a value is a group of names.
//
// TWO PROPERTIES OF THIS INTERFACE ARE THE POINT OF IT.
//
// First, nothing here returns a VALUE. GetEnvironment carries key NAMES, versions and update times.
// PutEnvironmentValue takes a value and answers with metadata. There is no method that could return one
// and no parameter that would ask for one.
//
// Second, the value ARRIVES only on PutEnvironmentValue and only inside a request body. It is never a
// path segment and never a query parameter, so it cannot land in an access log, a proxy log, a Referer
// header or a browser history entry — the argv rule applied to a URL.
type EnvironmentAPI interface {
	CreateEnvironment(ctx context.Context, scope middleware.Scope, body []byte) (ProvisionResult, error)
	ListEnvironments(ctx context.Context, scope middleware.Scope) (ProvisionResult, error)
	GetEnvironment(ctx context.Context, scope middleware.Scope, id string) (ProvisionResult, error)
	PutEnvironmentValue(ctx context.Context, scope middleware.Scope, id string, body []byte) (ProvisionResult, error)
	DeleteEnvironmentValue(ctx context.Context, scope middleware.Scope, id, key string) (ProvisionResult, error)
}

// environmentHandler renders the five environment routes. It reuses ProvisionResult and the `provision`
// capability gate: managing which credentials an agent receives is an org-admin operation of exactly the
// same weight as managing the credentials themselves (secretRefHandler's posture, verbatim).
//
// `provision` rather than a new capability, deliberately. E23 T9 opened `approve` as a SEPARATE capability
// and wrote down why: PATCH /v1/projects writes config_policy.approvers and is provision-gated, so a
// provisioning key could otherwise add itself to the approver list. No such bypass exists here — an
// environment grants no authority over an approval — and a new capability would move
// CapabilityClaimsDigest and redden every case checksum in 23 committed bundles for nothing.
type environmentHandler struct {
	environments EnvironmentAPI
}

func (h *environmentHandler) create(w http.ResponseWriter, r *http.Request) {
	scope, raw, ok := h.begin(w, r)
	if !ok {
		return
	}
	out, err := h.environments.CreateEnvironment(r.Context(), scope, raw)
	h.write(w, r, out, err, http.StatusCreated)
}

func (h *environmentHandler) list(w http.ResponseWriter, r *http.Request) {
	scope, ok := h.authorize(w, r)
	if !ok {
		return
	}
	out, err := h.environments.ListEnvironments(r.Context(), scope)
	h.write(w, r, out, err, http.StatusOK)
}

// get returns the environment with its key NAMES, versions and update times — never a value.
func (h *environmentHandler) get(w http.ResponseWriter, r *http.Request) {
	scope, ok := h.authorize(w, r)
	if !ok {
		return
	}
	out, err := h.environments.GetEnvironment(r.Context(), scope, r.PathValue("environment_id"))
	h.write(w, r, out, err, http.StatusOK)
}

// putValue is create-or-rotate. One route for both, because secret_refs is append-only: there is no
// in-place update that would make a first write and a later one different operations. The membership row
// and the sealed version land in ONE transaction (the store's job, not this handler's).
func (h *environmentHandler) putValue(w http.ResponseWriter, r *http.Request) {
	scope, raw, ok := h.begin(w, r)
	if !ok {
		return
	}
	out, err := h.environments.PutEnvironmentValue(r.Context(), scope, r.PathValue("environment_id"), raw)
	h.write(w, r, out, err, http.StatusCreated)
}

// deleteValue removes the BINDING between an environment and a key. It does not delete the stored value —
// secret_refs grants no DELETE and re-asserts that REVOKE every boot — and the store's response body says
// so in words rather than leaving the caller to infer it from a doc comment.
func (h *environmentHandler) deleteValue(w http.ResponseWriter, r *http.Request) {
	scope, ok := h.authorize(w, r)
	if !ok {
		return
	}
	out, err := h.environments.DeleteEnvironmentValue(r.Context(), scope, r.PathValue("environment_id"), r.PathValue("environment_key"))
	h.write(w, r, out, err, http.StatusOK)
}

// authorize resolves the verified scope and enforces the provision capability. The org comes from the
// VERIFIED key and never from a body field, so an environment is created in the caller's own tenant only.
func (h *environmentHandler) authorize(w http.ResponseWriter, r *http.Request) (middleware.Scope, bool) {
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

func (h *environmentHandler) begin(w http.ResponseWriter, r *http.Request) (middleware.Scope, []byte, bool) {
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

// write renders an environment outcome: the typed rejects first, then 2xx with the metadata projection.
//
// THE BAD-FIELD MESSAGE IS DELIBERATELY GENERIC ABOUT THE OFFENDING VALUE AND SPECIFIC ABOUT THE RULE. A
// rejected key name is safe to describe (`^[A-Z][A-Z0-9_]*$`, no PALAI_ prefix); the rejected key itself
// is not echoed, because an error body is the one place a caller's bytes get reflected back and a
// mistyped request can put a value in the wrong field.
func (h *environmentHandler) write(w http.ResponseWriter, r *http.Request, out ProvisionResult, err error, okStatus int) {
	if err != nil {
		middleware.WriteProblem(w, r, http.StatusInternalServerError, "internal_error", "")
		return
	}
	switch {
	case out.MissingField != "":
		middleware.WriteProblem(w, r, http.StatusBadRequest, "invalid_request", out.MissingField+" is required")
		return
	case out.BadField:
		middleware.WriteProblem(w, r, http.StatusBadRequest, "invalid_request",
			"the request body carries an unsupported field, or a key name outside ^[A-Z][A-Z0-9_]*$ or reserved by the PALAI_ prefix")
		return
	case out.NotFound:
		middleware.WriteProblem(w, r, http.StatusNotFound, "not_found", "no such environment or key in this scope")
		return
	case out.Conflict:
		middleware.WriteProblem(w, r, http.StatusConflict, "conflict", "an environment with this name already exists in this organization")
		return
	}
	w.Header().Set("Content-Type", "application/json")
	// no-store on EVERY environment response, including the read routes. They carry no value, so this is
	// belt-and-braces rather than load-bearing — but a key NAME list is still an inventory of which
	// credentials an organization holds, and an intermediary caching that for a shared browser is a
	// disclosure nobody chose.
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(okStatus)
	_, _ = w.Write(out.Body)
}
