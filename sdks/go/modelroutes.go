package palai

import (
	"context"
	"net/http"
)

// ModelRoutes is the model-routing admin surface (E13 T8 write + E16 T1 read-back, MCI-006): a
// project binds its own provider connection and publishes route revisions, then reads them back.
// The list methods return the admin ListView envelope ({object:"list", data:[…]}) — a full, small,
// tenant-scoped set, no cursor. Requires a key with the `provision` capability. A connection
// projection carries the secret REF name only, never a value.
type ModelRoutes struct{ client *Client }

// ModelConnectionCreateParams binds a provider family to a secret REF (never a value — a request
// that inlines a credential is rejected by the server's strict decode).
type ModelConnectionCreateParams struct {
	Provider  string `json:"provider"`
	SecretRef string `json:"secret_ref"`
}

// ModelRouteCreateParams opens a named route alias.
type ModelRouteCreateParams struct {
	Name string `json:"name"`
}

// ModelRouteRevisionCreateParams adds a draft revision to a route.
type ModelRouteRevisionCreateParams struct {
	Model        string `json:"model"`
	ConnectionID string `json:"connection_id"`
}

// --- write surface (E13 T8) ------------------------------------------------------------------

// CreateConnection binds a provider family to a secret-ref handle (201).
func (m *ModelRoutes) CreateConnection(ctx context.Context, params ModelConnectionCreateParams, opts ...CallOption) (*ModelConnection, error) {
	var out ModelConnection
	return ptrOrErr(&out, m.post(ctx, "/v1/model-connections", params, &out, opts))
}

// CreateRoute opens a named route alias for the project (201).
func (m *ModelRoutes) CreateRoute(ctx context.Context, params ModelRouteCreateParams, opts ...CallOption) (*ModelRoute, error) {
	var out ModelRoute
	return ptrOrErr(&out, m.post(ctx, "/v1/model-routes", params, &out, opts))
}

// CreateRevision adds a DRAFT revision to a route (201) — a draft never steers a run; publishing does.
func (m *ModelRoutes) CreateRevision(ctx context.Context, routeID string, params ModelRouteRevisionCreateParams, opts ...CallOption) (*ModelRouteRevision, error) {
	var out ModelRouteRevision
	return ptrOrErr(&out, m.post(ctx, "/v1/model-routes/"+escapePathSegment(routeID)+"/revisions", params, &out, opts))
}

// PublishRevision makes a draft revision the project's routed target. Re-publishing is idempotent.
func (m *ModelRoutes) PublishRevision(ctx context.Context, routeID, revisionID string, opts ...CallOption) (*ModelRouteRevision, error) {
	var out ModelRouteRevision
	path := "/v1/model-routes/" + escapePathSegment(routeID) + "/revisions/" + escapePathSegment(revisionID) + "/publish"
	o := requestOptions{idempotent: true}
	applyCallOptions(&o, opts)
	return ptrOrErr(&out, m.client.doJSON(ctx, http.MethodPost, path, o, &out))
}

// --- read-back (E16 T1: the E13 T10 write-only gap) ------------------------------------------

// ListConnections returns the project's connections (secret REF names only, never values).
func (m *ModelRoutes) ListConnections(ctx context.Context, opts ...CallOption) (*ListView[ModelConnection], error) {
	var out ListView[ModelConnection]
	return ptrOrErr(&out, m.get(ctx, "/v1/model-connections", &out, opts))
}

// GetConnection reads one connection by id; an absent/foreign id is a 404.
func (m *ModelRoutes) GetConnection(ctx context.Context, connectionID string, opts ...CallOption) (*ModelConnection, error) {
	var out ModelConnection
	return ptrOrErr(&out, m.get(ctx, "/v1/model-connections/"+escapePathSegment(connectionID), &out, opts))
}

// ListRoutes returns the project's route aliases.
func (m *ModelRoutes) ListRoutes(ctx context.Context, opts ...CallOption) (*ListView[ModelRoute], error) {
	var out ListView[ModelRoute]
	return ptrOrErr(&out, m.get(ctx, "/v1/model-routes", &out, opts))
}

// GetRoute reads one route alias by id; an absent/foreign id is a 404.
func (m *ModelRoutes) GetRoute(ctx context.Context, routeID string, opts ...CallOption) (*ModelRoute, error) {
	var out ModelRoute
	return ptrOrErr(&out, m.get(ctx, "/v1/model-routes/"+escapePathSegment(routeID), &out, opts))
}

// ListRevisions returns a route's revisions (each with its derived `published` flag).
func (m *ModelRoutes) ListRevisions(ctx context.Context, routeID string, opts ...CallOption) (*ListView[ModelRouteRevision], error) {
	var out ListView[ModelRouteRevision]
	return ptrOrErr(&out, m.get(ctx, "/v1/model-routes/"+escapePathSegment(routeID)+"/revisions", &out, opts))
}

// GetRevision reads one revision of a route; an absent/foreign route or revision is a 404.
func (m *ModelRoutes) GetRevision(ctx context.Context, routeID, revisionID string, opts ...CallOption) (*ModelRouteRevision, error) {
	var out ModelRouteRevision
	path := "/v1/model-routes/" + escapePathSegment(routeID) + "/revisions/" + escapePathSegment(revisionID)
	return ptrOrErr(&out, m.get(ctx, path, &out, opts))
}

// --- small shared plumbing -------------------------------------------------------------------

func (m *ModelRoutes) post(ctx context.Context, path string, body, out any, opts []CallOption) error {
	o := requestOptions{body: body}
	applyCallOptions(&o, opts)
	return m.client.doJSON(ctx, http.MethodPost, path, o, out)
}

func (m *ModelRoutes) get(ctx context.Context, path string, out any, opts []CallOption) error {
	o := requestOptions{}
	applyCallOptions(&o, opts)
	return m.client.doJSON(ctx, http.MethodGet, path, o, out)
}

func applyCallOptions(o *requestOptions, opts []CallOption) {
	for _, opt := range opts {
		opt(o)
	}
}

// ptrOrErr returns (nil, err) on error, else (out, nil) — a one-liner for the resource getters.
func ptrOrErr[T any](out *T, err error) (*T, error) {
	if err != nil {
		return nil, err
	}
	return out, nil
}
