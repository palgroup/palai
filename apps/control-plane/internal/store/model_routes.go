package store

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"

	"github.com/palgroup/palai/apps/control-plane/api"
	"github.com/palgroup/palai/apps/control-plane/api/middleware"
	"github.com/palgroup/palai/packages/coordinator"
)

// The E13 Task 8 model-routing management surface (spec §27.2, §27.6). These adapt the tenant-scoped
// api.ModelRouteAPI contract to the durable spine: scope → coordinator.Tenant, the typed rejects →
// api.ProvisionResult flags, and a committed row → its JSON projection.
//
// A projection NEVER carries a credential: a connection is rendered with its secret REF name only, which is
// a handle into the E13 T3 secret store.

// requireProjectScope rejects a key that is org-granular (the T2 provisioning shape, Scope.Project == "").
// Every model-routing row is keyed by the composite (organization, project) FK to projects, so such a key
// has no project to write into. It is a legitimate key shape, so the answer is a 400 naming what is
// missing — never a composite-FK violation surfacing as a 500.
func requireProjectScope(scope middleware.Scope) (api.ProvisionResult, bool) {
	if scope.Project == "" {
		return api.ProvisionResult{MissingField: "a project-scoped API key (model routing is per project)"}, false
	}
	return api.ProvisionResult{}, true
}

// CreateModelConnection binds a provider family to a secret-ref handle for the caller's project. The body
// is strictly decoded, so an attempt to inline a credential value (any unsupported field) is a 400 rather
// than a silently-ignored field.
func (s *Store) CreateModelConnection(ctx context.Context, scope middleware.Scope, body []byte) (api.ProvisionResult, error) {
	if out, ok := requireProjectScope(scope); !ok {
		return out, nil
	}
	var in struct {
		Provider  string `json:"provider"`
		SecretRef string `json:"secret_ref"`
	}
	if err := strictDecodeBody(body, &in); err != nil {
		return api.ProvisionResult{BadField: true}, nil
	}
	if in.Provider == "" {
		return api.ProvisionResult{MissingField: "provider"}, nil
	}
	if in.SecretRef == "" {
		return api.ProvisionResult{MissingField: "secret_ref"}, nil
	}
	id, err := s.spine.CreateModelConnection(ctx, tenantOf(scope), in.Provider, in.SecretRef)
	if err != nil {
		return api.ProvisionResult{}, err
	}
	return api.ProvisionResult{Body: mustJSON(map[string]any{
		"id": id, "object": "model_connection", "provider": in.Provider, "secret_ref": in.SecretRef,
	})}, nil
}

// CreateModelRoute opens the named route alias for the caller's project. Create is get-or-create: an alias
// names one lineage, so re-creating it returns the same id rather than a second lineage.
func (s *Store) CreateModelRoute(ctx context.Context, scope middleware.Scope, body []byte) (api.ProvisionResult, error) {
	if out, ok := requireProjectScope(scope); !ok {
		return out, nil
	}
	var in struct {
		Name string `json:"name"`
	}
	if err := strictDecodeBody(body, &in); err != nil {
		return api.ProvisionResult{BadField: true}, nil
	}
	if in.Name == "" {
		return api.ProvisionResult{MissingField: "name"}, nil
	}
	id, err := s.spine.CreateModelRoute(ctx, tenantOf(scope), in.Name)
	if err != nil {
		return api.ProvisionResult{}, err
	}
	return api.ProvisionResult{Body: mustJSON(map[string]any{"id": id, "object": "model_route", "name": in.Name})}, nil
}

// CreateModelRouteRevision adds a DRAFT revision to a route. A route or connection the caller cannot see is
// a NotFound (404) — the same answer as one that never existed.
func (s *Store) CreateModelRouteRevision(ctx context.Context, scope middleware.Scope, routeID string, body []byte) (api.ProvisionResult, error) {
	if out, ok := requireProjectScope(scope); !ok {
		return out, nil
	}
	var in struct {
		Model        string `json:"model"`
		ConnectionID string `json:"connection_id"`
	}
	if err := strictDecodeBody(body, &in); err != nil {
		return api.ProvisionResult{BadField: true}, nil
	}
	if in.Model == "" {
		return api.ProvisionResult{MissingField: "model"}, nil
	}
	if in.ConnectionID == "" {
		return api.ProvisionResult{MissingField: "connection_id"}, nil
	}
	rev, err := s.spine.CreateModelRouteRevision(ctx, tenantOf(scope), routeID, in.Model, in.ConnectionID)
	switch {
	case errors.Is(err, coordinator.ErrModelRouteNotFound), errors.Is(err, coordinator.ErrModelConnectionNotFound):
		return api.ProvisionResult{NotFound: true}, nil
	case err != nil:
		return api.ProvisionResult{}, err
	}
	return api.ProvisionResult{Body: mustJSON(modelRouteRevisionView(routeID, rev, false))}, nil
}

// PublishModelRouteRevision makes a draft revision the project's routed target. Publishing an
// already-published revision is an idempotent success.
func (s *Store) PublishModelRouteRevision(ctx context.Context, scope middleware.Scope, routeID, revisionID string) (api.ProvisionResult, error) {
	if out, ok := requireProjectScope(scope); !ok {
		return out, nil
	}
	err := s.spine.PublishModelRouteRevision(ctx, tenantOf(scope), routeID, revisionID)
	switch {
	case errors.Is(err, coordinator.ErrModelRouteNotFound), errors.Is(err, coordinator.ErrModelRouteRevisionNotFound):
		return api.ProvisionResult{NotFound: true}, nil
	case err != nil:
		return api.ProvisionResult{}, err
	}
	return api.ProvisionResult{Body: mustJSON(modelRouteRevisionView(routeID, coordinator.ModelRouteRevision{ID: revisionID}, true))}, nil
}

// modelRouteRevisionView renders a revision. Zero-valued fields are omitted so a thin projection (the
// publish acknowledgement, which knows only the id) claims nothing it did not read back; a read-back fills
// them all. Returned as a map so a LIST can embed it directly in the ListView envelope.
func modelRouteRevisionView(routeID string, rev coordinator.ModelRouteRevision, published bool) map[string]any {
	out := map[string]any{"id": rev.ID, "object": "model_route_revision", "route_id": routeID, "published": published}
	if rev.Revision > 0 {
		out["revision"] = rev.Revision
	}
	if rev.Model != "" {
		out["model"] = rev.Model
	}
	if rev.ConnectionID != "" {
		out["connection_id"] = rev.ConnectionID
	}
	if !rev.CreatedAt.IsZero() {
		out["created_at"] = rev.CreatedAt
	}
	return out
}

// modelConnectionView renders a connection's read-back projection — the secret REF name only, never a value.
func modelConnectionView(rec coordinator.ModelConnectionRecord) map[string]any {
	return map[string]any{
		"id": rec.ID, "object": "model_connection", "provider": rec.Provider,
		"secret_ref": rec.SecretRef, "created_at": rec.CreatedAt,
	}
}

// modelRouteView renders a route alias's read-back projection.
func modelRouteView(rec coordinator.ModelRouteRecord) map[string]any {
	return map[string]any{"id": rec.ID, "object": "model_route", "name": rec.Name, "created_at": rec.CreatedAt}
}

// listView wraps a set of projections in the un-paginated admin ListView envelope ({object:"list", data:[…]})
// — the same shape the provisioning/secret-ref reads return.
func listView(data []map[string]any) []byte {
	return mustJSON(map[string]any{"object": "list", "data": data})
}

// ListModelConnections returns the caller's project connections (E16 T1 read-back).
func (s *Store) ListModelConnections(ctx context.Context, scope middleware.Scope) (api.ProvisionResult, error) {
	if out, ok := requireProjectScope(scope); !ok {
		return out, nil
	}
	recs, err := s.spine.ListModelConnections(ctx, tenantOf(scope))
	if err != nil {
		return api.ProvisionResult{}, err
	}
	data := make([]map[string]any, 0, len(recs))
	for _, rec := range recs {
		data = append(data, modelConnectionView(rec))
	}
	return api.ProvisionResult{Body: listView(data)}, nil
}

// GetModelConnection reads one connection in scope; an absent/foreign id is a non-disclosing 404.
func (s *Store) GetModelConnection(ctx context.Context, scope middleware.Scope, connectionID string) (api.ProvisionResult, error) {
	if out, ok := requireProjectScope(scope); !ok {
		return out, nil
	}
	rec, err := s.spine.GetModelConnection(ctx, tenantOf(scope), connectionID)
	if errors.Is(err, coordinator.ErrModelConnectionNotFound) {
		return api.ProvisionResult{NotFound: true}, nil
	}
	if err != nil {
		return api.ProvisionResult{}, err
	}
	return api.ProvisionResult{Body: mustJSON(modelConnectionView(rec))}, nil
}

// ListModelRoutes returns the caller's project route aliases (E16 T1 read-back).
func (s *Store) ListModelRoutes(ctx context.Context, scope middleware.Scope) (api.ProvisionResult, error) {
	if out, ok := requireProjectScope(scope); !ok {
		return out, nil
	}
	recs, err := s.spine.ListModelRoutes(ctx, tenantOf(scope))
	if err != nil {
		return api.ProvisionResult{}, err
	}
	data := make([]map[string]any, 0, len(recs))
	for _, rec := range recs {
		data = append(data, modelRouteView(rec))
	}
	return api.ProvisionResult{Body: listView(data)}, nil
}

// GetModelRoute reads one route in scope; an absent/foreign id is a non-disclosing 404.
func (s *Store) GetModelRoute(ctx context.Context, scope middleware.Scope, routeID string) (api.ProvisionResult, error) {
	if out, ok := requireProjectScope(scope); !ok {
		return out, nil
	}
	rec, err := s.spine.GetModelRoute(ctx, tenantOf(scope), routeID)
	if errors.Is(err, coordinator.ErrModelRouteNotFound) {
		return api.ProvisionResult{NotFound: true}, nil
	}
	if err != nil {
		return api.ProvisionResult{}, err
	}
	return api.ProvisionResult{Body: mustJSON(modelRouteView(rec))}, nil
}

// ListModelRouteRevisions returns a route's revisions; a foreign/unknown route is a non-disclosing 404.
func (s *Store) ListModelRouteRevisions(ctx context.Context, scope middleware.Scope, routeID string) (api.ProvisionResult, error) {
	if out, ok := requireProjectScope(scope); !ok {
		return out, nil
	}
	revs, err := s.spine.ListModelRouteRevisions(ctx, tenantOf(scope), routeID)
	if errors.Is(err, coordinator.ErrModelRouteNotFound) {
		return api.ProvisionResult{NotFound: true}, nil
	}
	if err != nil {
		return api.ProvisionResult{}, err
	}
	data := make([]map[string]any, 0, len(revs))
	for _, rev := range revs {
		data = append(data, modelRouteRevisionView(routeID, rev, rev.Published))
	}
	return api.ProvisionResult{Body: listView(data)}, nil
}

// GetModelRouteRevision reads one revision of a route the caller owns. A foreign/unknown route is a 404;
// a revision id absent from that route is also a 404 (no cross-tenant existence disclosure).
func (s *Store) GetModelRouteRevision(ctx context.Context, scope middleware.Scope, routeID, revisionID string) (api.ProvisionResult, error) {
	if out, ok := requireProjectScope(scope); !ok {
		return out, nil
	}
	rev, err := s.spine.GetModelRouteRevision(ctx, tenantOf(scope), routeID, revisionID)
	switch {
	case errors.Is(err, coordinator.ErrModelRouteNotFound), errors.Is(err, coordinator.ErrModelRouteRevisionNotFound):
		return api.ProvisionResult{NotFound: true}, nil
	case err != nil:
		return api.ProvisionResult{}, err
	}
	return api.ProvisionResult{Body: mustJSON(modelRouteRevisionView(routeID, rev, rev.Published))}, nil
}

// strictDecodeBody rejects any field outside the declared shape, so a request that tries to smuggle an
// inline credential is a 400 instead of a silently-dropped field. An empty body decodes as {}.
func strictDecodeBody(body []byte, v any) error {
	if len(bytes.TrimSpace(body)) == 0 {
		body = []byte("{}")
	}
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.DisallowUnknownFields()
	return dec.Decode(v)
}
