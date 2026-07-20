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

// BindingResult is a binding projection. Invalid is a create missing a required field (400); Body is
// the created RepositoryBinding resource to return verbatim.
type BindingResult struct {
	Body    []byte
	Invalid bool
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
