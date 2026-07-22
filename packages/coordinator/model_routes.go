package coordinator

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/palgroup/palai/storage"
)

// DB-backed model routing (E13 Task 8; spec §27.2, §27.6, §27.7 — the LP §7.3 carve-out). This file is the
// first reader AND writer of migration 000001's model_connections / model_routes / model_route_revisions,
// so it introduces no migration.
//
// WHAT IT DECIDES, EXACTLY: which model id goes on the provider wire for a project's run, and which
// connection credential redeems that call. Nothing else. It does NOT rank candidate targets, probe
// capabilities, weigh price/latency, hedge, or fail over — the §27.6 revision fields for those are not
// stored and are not claimed here; a second provider family + capability probe is E16.

// DefaultModelRouteAlias is the route alias a run resolves through. §27.6 describes aliases (primary,
// fast, research…), but no config layer can yet ASK for one, so storing per-run alias selection would be
// dead config. One alias per project, named honestly.
// ponytail: when a config layer grows a route field, resolution takes the alias from it and this constant
// becomes the fallback.
const DefaultModelRouteAlias = "default"

var (
	// ErrModelRouteNotFound marks a route id that is absent OR belongs to another tenant — deliberately
	// the same answer, so the API renders a non-disclosing 404 rather than confirming the id exists.
	ErrModelRouteNotFound = errors.New("model route not found")
	// ErrModelRouteRevisionNotFound marks a revision id absent from the named route.
	ErrModelRouteRevisionNotFound = errors.New("model route revision not found")
	// ErrModelConnectionNotFound marks a connection id a revision cannot bind: absent, or owned by
	// another tenant.
	ErrModelConnectionNotFound = errors.New("model connection not found")
)

// ModelRouteTarget is one project's resolved routing decision: the model id, the adapter family, and the
// credential REFERENCE the broker redeems at call time. SecretRef is a handle into the secret store — a
// credential value never travels on this struct, in a query, or in an event.
type ModelRouteTarget struct {
	RevisionID string
	Revision   int
	Provider   string
	Model      string
	SecretRef  string
}

// ModelRouteRevision is a created/published revision's projection.
type ModelRouteRevision struct {
	ID           string
	Revision     int
	Model        string
	ConnectionID string
	Published    bool
}

// ProjectModelRoute resolves the project's published route, or found=false when the project has none —
// the caller then falls back to the deployment default (the env route). A published revision whose
// connection no longer resolves in this tenant is an ERROR, never a fall-through: silently running a
// tenant on the deployment credential is exactly the "route cannot silently select" failure of §27.7.
func (s *Store) ProjectModelRoute(ctx context.Context, tenant Tenant) (ModelRouteTarget, bool, error) {
	ctx = storage.ScopeToTenant(ctx, tenant.Organization, tenant.Project)
	var target ModelRouteTarget
	var provider, secretRef *string
	err := s.pool.QueryRow(ctx, storage.Query("ResolveProjectModelRoute"), tenant.Organization, tenant.Project, DefaultModelRouteAlias).
		Scan(&target.RevisionID, &target.Revision, &target.Model, &provider, &secretRef)
	if errors.Is(err, pgx.ErrNoRows) {
		return ModelRouteTarget{}, false, nil
	}
	if err != nil {
		return ModelRouteTarget{}, false, fmt.Errorf("resolve project model route: %w", err)
	}
	if provider == nil || secretRef == nil {
		return ModelRouteTarget{}, false, fmt.Errorf("model route revision %s names a connection that does not resolve in this tenant: %w",
			target.RevisionID, ErrModelConnectionNotFound)
	}
	target.Provider, target.SecretRef = *provider, *secretRef
	return target, true, nil
}

// CreateModelConnection binds a provider family to a secret-ref handle for one project (spec §27.2: a
// connection carries references and non-secret settings only). The ref names a secret in the E13 T3 store;
// the value is redeemed at call time and never persisted here.
//
// ponytail: project-scoped only. 000001 allows a NULL project_id (an org-wide connection), but migration
// 000029's policy compares project_id to the scoped project, so a NULL-project row is invisible to a
// project-scoped read — an org-wide connection would need a policy change, not a code change.
func (s *Store) CreateModelConnection(ctx context.Context, tenant Tenant, provider, secretRef string) (string, error) {
	ctx = storage.ScopeToTenant(ctx, tenant.Organization, tenant.Project)
	id := newModelRoutingID("mconn")
	if _, err := s.pool.Exec(ctx, storage.Query("InsertModelConnection"), id, tenant.Organization, tenant.Project, provider, secretRef); err != nil {
		return "", fmt.Errorf("insert model connection: %w", err)
	}
	return id, nil
}

// CreateModelRoute opens (or returns) the named alias for a project. It is get-or-create: an alias names
// ONE lineage per project, so creating it twice is not an error and mints no second lineage.
// ponytail: 000001 declares no UNIQUE(organization_id, project_id, name), so two concurrent creates could
// still both insert; resolution stays deterministic regardless (ResolveProjectModelRoute's total ordering).
func (s *Store) CreateModelRoute(ctx context.Context, tenant Tenant, name string) (string, error) {
	ctx = storage.ScopeToTenant(ctx, tenant.Organization, tenant.Project)
	var existing string
	switch err := s.pool.QueryRow(ctx, storage.Query("GetModelRouteByName"), tenant.Organization, tenant.Project, name).Scan(&existing); {
	case err == nil:
		return existing, nil
	case !errors.Is(err, pgx.ErrNoRows):
		return "", fmt.Errorf("look up model route: %w", err)
	}
	id := newModelRoutingID("mroute")
	if _, err := s.pool.Exec(ctx, storage.Query("InsertModelRoute"), id, tenant.Organization, tenant.Project, name); err != nil {
		return "", fmt.Errorf("insert model route: %w", err)
	}
	return id, nil
}

// CreateModelRouteRevision adds a DRAFT revision to a route (the E11 immutable-revision shape): a revise is
// always a NEW revision, never an edit of a published one. The route and the connection are both verified
// in scope first, so a revision can neither attach to a foreign route nor bind a foreign credential.
func (s *Store) CreateModelRouteRevision(ctx context.Context, tenant Tenant, routeID, model, connectionID string) (ModelRouteRevision, error) {
	ctx = storage.ScopeToTenant(ctx, tenant.Organization, tenant.Project)
	if err := s.requireModelRoute(ctx, tenant, routeID); err != nil {
		return ModelRouteRevision{}, err
	}
	switch err := s.pool.QueryRow(ctx, storage.Query("ModelConnectionExists"), connectionID, tenant.Organization, tenant.Project).Scan(new(int)); {
	case errors.Is(err, pgx.ErrNoRows):
		return ModelRouteRevision{}, ErrModelConnectionNotFound
	case err != nil:
		return ModelRouteRevision{}, fmt.Errorf("verify model connection: %w", err)
	}

	var number int
	if err := s.pool.QueryRow(ctx, storage.Query("NextModelRouteRevision"), routeID).Scan(&number); err != nil {
		return ModelRouteRevision{}, fmt.Errorf("next model route revision: %w", err)
	}
	id := newModelRoutingID("mrev")
	config, _ := json.Marshal(map[string]string{"model": model, "connection_id": connectionID})
	if _, err := s.pool.Exec(ctx, storage.Query("InsertModelRouteRevision"), id, routeID, number, config); err != nil {
		return ModelRouteRevision{}, fmt.Errorf("insert model route revision: %w", err)
	}
	return ModelRouteRevision{ID: id, Revision: number, Model: model, ConnectionID: connectionID}, nil
}

// PublishModelRouteRevision makes a draft revision routable. Publish is the ONE legitimate mutation of a
// revision (the routing fields are never rewritten), and it is irreversible: rolling back means publishing
// a new revision. Re-publishing an already-published revision is an idempotent success.
func (s *Store) PublishModelRouteRevision(ctx context.Context, tenant Tenant, routeID, revisionID string) error {
	ctx = storage.ScopeToTenant(ctx, tenant.Organization, tenant.Project)
	if err := s.requireModelRoute(ctx, tenant, routeID); err != nil {
		return err
	}
	switch err := s.pool.QueryRow(ctx, storage.Query("PublishModelRouteRevision"), revisionID, routeID).Scan(new(string)); {
	case err == nil:
		return nil
	case !errors.Is(err, pgx.ErrNoRows):
		return fmt.Errorf("publish model route revision: %w", err)
	}
	// No row updated: either the revision is already published (idempotent success) or it does not belong
	// to this route (404).
	switch err := s.pool.QueryRow(ctx, storage.Query("ModelRouteRevisionExists"), revisionID, routeID).Scan(new(int)); {
	case errors.Is(err, pgx.ErrNoRows):
		return ErrModelRouteRevisionNotFound
	case err != nil:
		return fmt.Errorf("verify model route revision: %w", err)
	}
	return nil
}

// requireModelRoute fails with ErrModelRouteNotFound unless the route is in the caller's scope. An absent
// route and another tenant's route are the same answer by design.
func (s *Store) requireModelRoute(ctx context.Context, tenant Tenant, routeID string) error {
	switch err := s.pool.QueryRow(ctx, storage.Query("ModelRouteExists"), routeID, tenant.Organization, tenant.Project).Scan(new(int)); {
	case errors.Is(err, pgx.ErrNoRows):
		return ErrModelRouteNotFound
	case err != nil:
		return fmt.Errorf("verify model route: %w", err)
	}
	return nil
}

func newModelRoutingID(prefix string) string {
	var raw [16]byte
	_, _ = rand.Read(raw[:])
	return prefix + "_" + hex.EncodeToString(raw[:])
}
