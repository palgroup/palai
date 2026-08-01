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
	// GetHook and ListHooks are the E29 T1 read half. Until they landed this family mounted a create and a
	// kill-switch and NOT ONE GET: a hook that fires inside every run of a project could be neither
	// enumerated nor read back, and `disable` was a write whose only confirmation was its own 200.
	//
	// The list returns DISABLED hooks too. The dispatch loop's read (HooksForPoint) does not, and cannot be
	// borrowed for this: it takes a point and filters disabled_at, so a management list built on it would
	// make the kill-switch unobservable.
	GetHook(ctx context.Context, scope middleware.Scope, id string) (HookResult, error)
	ListHooks(ctx context.Context, scope middleware.Scope, q ListQuery) ([]ListRow, error)
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
	// The Location is back, and it is back because the address now RESOLVES: E29 T2 removed it while
	// `/v1/hooks/<id>` was a prefix nothing mounted, and wrote that it would return with the GET. T1 mounted
	// the GET, so the dangling-Location guard follows this header into a 200 rather than into net/http's own
	// not-found. The header is not restored because a create "should" carry one — it is restored because
	// there is finally a resource at the address it names.
	h.write(w, r, out, err, http.StatusCreated, "/v1/hooks/")
}

// getHook returns one hook's management projection (GET /v1/hooks/{id}). An absent or foreign hook is a
// 404 — the same answer, so an outsider cannot distinguish "not yours" from "not there".
func (h *hookHandler) getHook(w http.ResponseWriter, r *http.Request) {
	scope, ok := h.authorize(w, r)
	if !ok {
		return
	}
	out, err := h.hooks.GetHook(r.Context(), scope, r.PathValue("id"))
	h.write(w, r, out, err, http.StatusOK, "")
}

// listHooks returns a tenant-scoped page of hooks (GET /v1/hooks), DISABLED ONES INCLUDED, in the shared
// page envelope. It carries no ?status=: `disabled_at` is a timestamp, not a lifecycle-state column, and
// statusFilterKinds means exactly what it says.
func (h *hookHandler) listHooks(w http.ResponseWriter, r *http.Request) {
	scope, ok := h.authorize(w, r)
	if !ok {
		return
	}
	q, ok := beginList(w, r, "hooks", scope)
	if !ok {
		return
	}
	// The +1 over-fetch renderPage trims: the store returns Limit+1 rows so has_more needs no second query.
	rows, err := h.hooks.ListHooks(r.Context(), scope, ListQuery{
		After: q.After, Limit: q.Limit + 1, CreatedGTE: q.CreatedGTE, CreatedLTE: q.CreatedLTE,
	})
	if err != nil {
		middleware.WriteProblem(w, r, http.StatusInternalServerError, "internal_error", "")
		return
	}
	renderPage(w, r, "hooks", scope, rows, q.Limit)
}

// authorize resolves the verified scope and enforces the `provision` capability for the two E29 T1 reads.
//
// THE ASYMMETRY WITH THE WRITES ABOVE IS DELIBERATE AND RECORDED RATHER THAN INFERRED, and it is the E25 T7
// tool-revision precedent verbatim: the shipped E12 create/disable routes carry no capability gate, and
// retro-gating a shipped surface is a contract change rather than a read route. What these two answer is
// the policy an operator provisioned — which extension points this project runs, and against which remote
// worker — so they sit with the other provisioned surfaces (environments, secret-refs, model-routes).
// HONEST CEILING: a key with an EMPTY scope set holds `provision` implicitly (middleware.Scope.HasScope),
// and every bootstrap and admin key has one, so this gate separates a NARROWED key from a broad one and
// never an operator from an operator.
func (h *hookHandler) authorize(w http.ResponseWriter, r *http.Request) (middleware.Scope, bool) {
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
