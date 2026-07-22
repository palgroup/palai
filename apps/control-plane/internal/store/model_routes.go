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

// CreateModelConnection binds a provider family to a secret-ref handle for the caller's project. The body
// is strictly decoded, so an attempt to inline a credential value (any unsupported field) is a 400 rather
// than a silently-ignored field.
func (s *Store) CreateModelConnection(ctx context.Context, scope middleware.Scope, body []byte) (api.ProvisionResult, error) {
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
	return api.ProvisionResult{Body: modelRouteRevisionBody(routeID, rev, false)}, nil
}

// PublishModelRouteRevision makes a draft revision the project's routed target. Publishing an
// already-published revision is an idempotent success.
func (s *Store) PublishModelRouteRevision(ctx context.Context, scope middleware.Scope, routeID, revisionID string) (api.ProvisionResult, error) {
	err := s.spine.PublishModelRouteRevision(ctx, tenantOf(scope), routeID, revisionID)
	switch {
	case errors.Is(err, coordinator.ErrModelRouteNotFound), errors.Is(err, coordinator.ErrModelRouteRevisionNotFound):
		return api.ProvisionResult{NotFound: true}, nil
	case err != nil:
		return api.ProvisionResult{}, err
	}
	return api.ProvisionResult{Body: modelRouteRevisionBody(routeID, coordinator.ModelRouteRevision{ID: revisionID}, true)}, nil
}

// modelRouteRevisionBody renders a revision. Zero-valued fields are omitted so the publish projection
// (which knows only the id) claims nothing it did not read back.
func modelRouteRevisionBody(routeID string, rev coordinator.ModelRouteRevision, published bool) []byte {
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
	return mustJSON(out)
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
