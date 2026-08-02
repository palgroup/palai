package api

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"

	"github.com/palgroup/palai/apps/control-plane/api/middleware"
)

// BindingRegistrar is the store seam for registering a project's external repository binding (spec
// §30.1). The Postgres store implements it; production wires it, and the conformance HTTP tier (which
// never touches bindings) passes nil, so the route stays unmounted there. It is scoped by the verified
// identity, never a request-body field (§39.2) — the raw credential is never carried, only ConnectionRef.
type BindingRegistrar interface {
	CreateRepositoryBinding(ctx context.Context, scope middleware.Scope, req RepositoryBindingCreate) (BindingResult, error)
	// GetRepositoryBinding + ListRepositoryBindings are the E13 T4 read side. Both run under RLS, so
	// the request scope confines them; a missing/foreign id is NotFound (404). The list carries the keyset
	// position + created_at bounds, plus ONE lifecycle switch: archived bindings are hidden unless
	// includeArchived says otherwise (E30, migration 000057). It is a parameter rather than a second
	// method because a forked list is a second place for the tenant predicate to be got wrong.
	GetRepositoryBinding(ctx context.Context, scope middleware.Scope, id string) (BindingResult, error)
	ListRepositoryBindings(ctx context.Context, scope middleware.Scope, q ListQuery, includeArchived bool) ([]ListRow, error)

	// SetRepositoryBindingConnection points an EXISTING binding at a different credential handle, or at
	// none (E30). It exists because its absence was the gap: measured on a live stack, 20 bindings carried
	// no connection_ref and nothing anywhere — route, seam, statement or CLI verb — could give one to any
	// of them, so a binding registered before its credential existed was unfixable and "register another"
	// was the only move. changed is false for an unknown, foreign, or ARCHIVED id.
	//
	// It takes a REF and never a credential. The bytes are written over POST /v1/secret-refs and read at
	// clone time; this moves a pointer, which is why nothing sensitive appears in its parameters.
	SetRepositoryBindingConnection(ctx context.Context, scope middleware.Scope, id, ref string) (bool, error)
	// ArchiveRepositoryBinding / UnarchiveRepositoryBinding retire and restore a binding. Retiring is not
	// a delete — preparation_receipts holds a foreign key onto the row and this API has no destructive
	// verb anywhere — and it is more than a display flag: the run-admission guard refuses an archived
	// binding. changed is false when the call was a no-op (unknown, foreign, or already in that state).
	ArchiveRepositoryBinding(ctx context.Context, scope middleware.Scope, id string) (bool, error)
	UnarchiveRepositoryBinding(ctx context.Context, scope middleware.Scope, id string) (bool, error)
}

// RepositoryBindingCreate is the resolved create body (spec §30.1). Provider + RepositoryIdentity are
// the authoritative identity (display names/URLs are not trusted as identity); the id is minted
// server-side. The credential is never in the body — only ConnectionRef, an opaque handle to it.
type RepositoryBindingCreate struct {
	Provider           string
	RepositoryIdentity string
	CloneURL           string
	DefaultBranch      string
	ConnectionRef      string
	AllowedOperations  []string
	Policy             map[string]any
	DataClassification string
	RegionConstraint   string
}

// BindingResult is a binding projection. Invalid is a create missing a required field (400);
// NotFound is a read of a missing/foreign id (404); Body is the RepositoryBinding resource to
// return verbatim.
type BindingResult struct {
	Body     []byte
	Invalid  bool
	NotFound bool
}

type bindingHandler struct {
	bindings BindingRegistrar
}

// create registers a repository binding (spec §30.1 POST /v1/repository-bindings). It is a thin wrapper
// over the durable CreateRepositoryBinding: parse the body, mint the id in the store, return 201 +
// Location + the created resource. A binding is durable project configuration, not an idempotent
// operation, so it carries no idempotency key — a re-post registers a distinct binding.
func (h *bindingHandler) create(w http.ResponseWriter, r *http.Request) {
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
	var body struct {
		Provider           string         `json:"provider"`
		RepositoryIdentity string         `json:"repository_identity"`
		CloneURL           string         `json:"clone_url"`
		DefaultBranch      string         `json:"default_branch"`
		ConnectionRef      string         `json:"connection_ref"`
		AllowedOperations  []string       `json:"allowed_operations"`
		Policy             map[string]any `json:"policy"`
		DataClassification string         `json:"data_classification"`
		RegionConstraint   string         `json:"region_constraint"`
	}
	if err := json.Unmarshal(raw, &body); err != nil {
		middleware.WriteProblem(w, r, http.StatusBadRequest, "invalid_request", "the request body is not valid JSON")
		return
	}
	// §24 trust-boundary: only an http(s) clone_url is accepted in production. A file: (or schemeless
	// local-path) URL would let any API-key holder point the CP-side clone at another tenant's on-host
	// allocation (same host on the collapsed compose), so it is refused unless PALAI_ALLOW_LOCAL_REPOSITORY
	// is set for a dev/test stack. The deterministic harness registers local file remotes through the
	// coordinator spine directly, so this gate never breaks it.
	if !allowedCloneScheme(body.CloneURL) {
		middleware.WriteProblem(w, r, http.StatusBadRequest, "invalid_request", "clone_url must be an http(s) URL")
		return
	}

	out, err := h.bindings.CreateRepositoryBinding(r.Context(), scope, RepositoryBindingCreate{
		Provider:           body.Provider,
		RepositoryIdentity: body.RepositoryIdentity,
		CloneURL:           body.CloneURL,
		DefaultBranch:      body.DefaultBranch,
		ConnectionRef:      body.ConnectionRef,
		AllowedOperations:  body.AllowedOperations,
		Policy:             body.Policy,
		DataClassification: body.DataClassification,
		RegionConstraint:   body.RegionConstraint,
	})
	if err != nil {
		middleware.WriteProblem(w, r, http.StatusInternalServerError, "internal_error", "")
		return
	}
	if out.Invalid {
		middleware.WriteProblem(w, r, http.StatusBadRequest, "invalid_request", "provider, repository_identity, and clone_url are required")
		return
	}
	w.Header().Set("Location", "/v1/repository-bindings/"+bindingIDOf(out.Body))
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	_, _ = w.Write(out.Body)
}

// get reads one binding within the verified scope (spec §30.1 GET /v1/repository-bindings/{id}). A
// missing or foreign id is a tenant-scoped 404, leaking no cross-tenant existence.
func (h *bindingHandler) get(w http.ResponseWriter, r *http.Request) {
	scope, ok := middleware.ScopeFrom(r.Context())
	if !ok {
		middleware.WriteProblem(w, r, http.StatusUnauthorized, "authentication_required", "a bearer API key is required")
		return
	}
	out, err := h.bindings.GetRepositoryBinding(r.Context(), scope, r.PathValue("binding_id"))
	if err != nil {
		middleware.WriteProblem(w, r, http.StatusInternalServerError, "internal_error", "")
		return
	}
	if out.NotFound {
		middleware.WriteProblem(w, r, http.StatusNotFound, "not_found", "no such repository binding in this project")
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(out.Body)
}

// list returns a tenant-scoped page of bindings (spec §30.1 GET /v1/repository-bindings), confined to
// the verified scope by RLS. Cursor + created_at bounds only — a binding has no lifecycle state.
func (h *bindingHandler) list(w http.ResponseWriter, r *http.Request) {
	scope, ok := middleware.ScopeFrom(r.Context())
	if !ok {
		middleware.WriteProblem(w, r, http.StatusUnauthorized, "authentication_required", "a bearer API key is required")
		return
	}
	q, ok := beginList(w, r, "repository-bindings", scope)
	if !ok {
		return
	}
	// `?include_archived=true` and nothing looser. A truthy-string parser here would make
	// `include_archived=false` mean "yes", which is the direction that silently widens what an operator
	// is shown; anything that is not exactly "true" leaves retired bindings hidden.
	rows, err := h.bindings.ListRepositoryBindings(r.Context(), scope, q, r.URL.Query().Get("include_archived") == "true")
	if err != nil {
		middleware.WriteProblem(w, r, http.StatusInternalServerError, "internal_error", "")
		return
	}
	renderPage(w, r, "repository-bindings", scope, rows, q.Limit)
}

// allowedCloneScheme reports whether a clone_url may be registered over the HTTP endpoint (§24): only
// http/https in production. PALAI_ALLOW_LOCAL_REPOSITORY (any non-empty value) opens it to local
// transports for a dev/test stack. A missing/unparseable scheme is a bare local path — refused.
func allowedCloneScheme(cloneURL string) bool {
	if os.Getenv("PALAI_ALLOW_LOCAL_REPOSITORY") != "" {
		return true
	}
	u, err := url.Parse(cloneURL)
	if err != nil {
		return false
	}
	switch strings.ToLower(u.Scheme) {
	case "http", "https":
		return true
	default:
		return false
	}
}

// bindingIDOf reads the id from a binding projection for the Location header; "" if unparseable.
func bindingIDOf(body []byte) string {
	var probe struct {
		ID string `json:"id"`
	}
	_ = json.Unmarshal(body, &probe)
	return probe.ID
}

// connectionBody is the PUT /v1/repository-bindings/{id}/connection body: a secret_refs NAME and nothing
// else. There is deliberately no credential field — the same structural property the create side has, for
// the same reason, and a reviewer should be able to see it by reading this struct.
type connectionBody struct {
	ConnectionRef string `json:"connection_ref"`
}

// setConnection points an existing binding at a different credential handle (E30).
//
// WHY A NARROW SUB-RESOURCE RATHER THAN A PATCH ON THE BINDING. `provider`, `repository_identity` and
// `clone_url` are the binding's IDENTITY, and every preparation_receipts row already written against it
// asserts which repository a past run cloned; making identity mutable would retroactively falsify that
// provenance, and no receipt could tell a reader it had happened. A credential says only HOW the fetch
// authenticated. A general PATCH would put both behind one verb and one permission, so the field list
// would become the only thing standing between a rotation and a rewrite of history.
//
// `allowed_operations` and `policy` are excluded too, and that is a decision rather than an omission:
// they are CEILINGS a run reads at preparation time, and lowering one under a session that is already
// running is a different question that wants a boundary rather than an UPDATE.
//
// PUT, because it is idempotent and total for this sub-resource: the same call twice leaves the same
// state, and there is no partial form of "which credential does this binding use".
func (h *bindingHandler) setConnection(w http.ResponseWriter, r *http.Request) {
	scope, ok := middleware.ScopeFrom(r.Context())
	if !ok {
		middleware.WriteProblem(w, r, http.StatusUnauthorized, "authentication_required", "a bearer API key is required")
		return
	}
	raw, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		middleware.WriteProblem(w, r, http.StatusBadRequest, "invalid_request", "the request body could not be read")
		return
	}
	var body connectionBody
	// DisallowUnknownFields so a caller that sends `{"credential": "..."}` — the shape somebody WILL try —
	// is refused loudly instead of having it silently dropped and believing the binding took it.
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&body); err != nil {
		middleware.WriteProblem(w, r, http.StatusBadRequest, "invalid_request",
			"the body takes connection_ref (a secret ref NAME) and no other field; a credential is written with POST /v1/secret-refs")
		return
	}
	changed, err := h.bindings.SetRepositoryBindingConnection(r.Context(), scope, r.PathValue("binding_id"), body.ConnectionRef)
	if err != nil {
		middleware.WriteProblem(w, r, http.StatusInternalServerError, "internal_error", "")
		return
	}
	if !changed {
		// One answer for unknown, foreign and archived, matching every other read on this resource: an
		// archived binding accepts no runs, so it accepts no credential either, and the operator finds out
		// which of the three it was from GET, whose projection carries archived_at.
		middleware.WriteProblem(w, r, http.StatusNotFound, "not_found",
			"no such repository binding in this project, or it is archived — an archived binding is restored before it is re-credited")
		return
	}
	h.get(w, r)
}

// archive retires a binding (E30). It is POST rather than DELETE because it is not one: the row stays,
// preparation_receipts keeps its foreign key, and the operation is reversible.
func (h *bindingHandler) archive(w http.ResponseWriter, r *http.Request) {
	h.flipArchive(w, r, true)
}

// unarchive puts a retired binding back in service.
func (h *bindingHandler) unarchive(w http.ResponseWriter, r *http.Request) {
	h.flipArchive(w, r, false)
}

// flipArchive is the shared body of the two above. A no-op — unknown, foreign, or already in the target
// state — is a 404 rather than a 200, so an operator who archives a typo'd id is told, instead of reading
// a success for something that never happened.
func (h *bindingHandler) flipArchive(w http.ResponseWriter, r *http.Request, retire bool) {
	scope, ok := middleware.ScopeFrom(r.Context())
	if !ok {
		middleware.WriteProblem(w, r, http.StatusUnauthorized, "authentication_required", "a bearer API key is required")
		return
	}
	id := r.PathValue("binding_id")
	var (
		changed bool
		err     error
	)
	if retire {
		changed, err = h.bindings.ArchiveRepositoryBinding(r.Context(), scope, id)
	} else {
		changed, err = h.bindings.UnarchiveRepositoryBinding(r.Context(), scope, id)
	}
	if err != nil {
		middleware.WriteProblem(w, r, http.StatusInternalServerError, "internal_error", "")
		return
	}
	if !changed {
		detail := "no such repository binding in this project, or it is already archived"
		if !retire {
			detail = "no such repository binding in this project, or it is not archived"
		}
		middleware.WriteProblem(w, r, http.StatusNotFound, "not_found", detail)
		return
	}
	h.get(w, r)
}
