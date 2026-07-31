package api

import (
	"context"
	"io"
	"net/http"

	"github.com/palgroup/palai/apps/control-plane/api/middleware"
)

// HookAPI is the store seam for the E12 Task 8 hooks management surface (spec §28.17, TOL-012): admin
// registration of extension points that fire inside the run's single dispatch loop + the admin disable
// kill-switch. It is an ADMIN surface — there is deliberately NO model-facing hook-register tool (a test pins
// that the tool broker exposes no such name). Scoped by the verified identity, never a request-body field
// (§39.2). nil in tiers that never touch hooks.
type HookAPI interface {
	CreateHook(ctx context.Context, scope middleware.Scope, body []byte) (HookResult, error)
	DisableHook(ctx context.Context, scope middleware.Scope, id string) (HookResult, error)
}

// HookResult is a management projection. Exactly one outcome is set: Body carries the created hook or the
// disable summary (2xx); BadField marks an unknown point/category/executor, an out-of-matrix pair, an invalid
// config, or an inline secret (400); Conflict marks a name collision (409); NotFound marks an absent hook
// (404).
type HookResult struct {
	Body     []byte
	BadField bool
	Conflict bool
	NotFound bool
}

type hookHandler struct {
	hooks HookAPI
}

// createHook registers a hook (POST /v1/hooks). Durable config, server-minted id.
func (h *hookHandler) createHook(w http.ResponseWriter, r *http.Request) {
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
	out, err := h.hooks.CreateHook(r.Context(), scope, raw)
	// No Location: `/v1/hooks/<id>` is not mounted — the family has POST and POST-disable and not one GET
	// (E29 T2). The store side of the read already exists and is tested against a real Postgres; what is
	// missing is a method on HookAPI and a handler, and that is the task that opens the hooks read half.
	// When GET /v1/hooks/{id} lands, this header comes back with it and the guard accepts it.
	h.write(w, r, out, err, http.StatusCreated, "")
}

// disableHook flips a hook's admin kill-switch (POST /v1/hooks/{id}/disable). A disabled hook never fires.
func (h *hookHandler) disableHook(w http.ResponseWriter, r *http.Request) {
	scope, ok := middleware.ScopeFrom(r.Context())
	if !ok {
		middleware.WriteProblem(w, r, http.StatusUnauthorized, "authentication_required", "a bearer API key is required")
		return
	}
	out, err := h.hooks.DisableHook(r.Context(), scope, r.PathValue("id"))
	h.write(w, r, out, err, http.StatusOK, "")
}

// write renders a management outcome: the typed rejects first, then 2xx with the resource.
func (h *hookHandler) write(w http.ResponseWriter, r *http.Request, out HookResult, err error, okStatus int, locationPrefix string) {
	if err != nil {
		middleware.WriteProblem(w, r, http.StatusInternalServerError, "internal_error", "")
		return
	}
	switch {
	case out.BadField:
		middleware.WriteProblem(w, r, http.StatusBadRequest, "invalid_request", "the request carries an unsupported field, an unknown hook point/category/executor, an out-of-matrix pair, or an inline secret")
		return
	case out.Conflict:
		middleware.WriteProblem(w, r, http.StatusConflict, "conflict", "a hook with this name already exists")
		return
	case out.NotFound:
		middleware.WriteProblem(w, r, http.StatusNotFound, "not_found", "no such hook in this project")
		return
	}
	if locationPrefix != "" {
		w.Header().Set("Location", locationPrefix+resourceIDOf(out.Body))
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(okStatus)
	_, _ = w.Write(out.Body)
}
