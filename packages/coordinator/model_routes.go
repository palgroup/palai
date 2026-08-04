package coordinator

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"time"

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
	// BaseURL is the CONNECTION's endpoint (000049), empty meaning the family's own. It is what makes a
	// custom OpenAI-compatible provider a per-project property rather than a deployment-wide env var.
	BaseURL string
	// Thinking is whether runs on this route ask the provider to return the model's reasoning where a
	// person can read it — the canonical modelbroker.ThinkingMode vocabulary, empty for "do not ask".
	//
	// IT LIVES ON THE REVISION BECAUSE ITS VALIDITY IS A FUNCTION OF THE MODEL, and the revision is the
	// only durable record that names one. Measured 2026-08-04 against the live Anthropic API: asking for
	// reasoning succeeds on claude-sonnet-5 and claude-opus-4-6 and is a 400 ("adaptive thinking is not
	// supported on this model") on claude-sonnet-4-5, claude-haiku-4-5 and claude-opus-4-5. A deployment
	// env var would have made one stack unable to serve two models; a per-request flag would have let a
	// caller pair the ask with a model that rejects it. Choosing the model and choosing to see its
	// reasoning are one decision, so they are revised together and published together.
	Thinking string
}

// ModelRouteRevision is a created/published revision's projection. CreatedAt is populated only by a
// read-back (the create/publish paths leave it zero, and the store's projection omits a zero timestamp).
type ModelRouteRevision struct {
	ID           string
	Revision     int
	Model        string
	ConnectionID string
	// Thinking is the reasoning preference this revision pinned; empty when it pinned none.
	Thinking  string
	Published bool
	CreatedAt time.Time
}

// ModelConnectionRecord is a connection's read-back projection. It carries the secret REFERENCE name
// only — a credential value is redeemed at call time and never travels here.
type ModelConnectionRecord struct {
	ID        string
	Provider  string
	SecretRef string
	BaseURL   string
	CreatedAt time.Time
	// VerifiedAt / VerificationOutcome are the last credential probe's stamp. Zero/empty means no probe has
	// ever run, which is NOT a failure and is rendered as its own state — a connection that was never
	// verified routes exactly like one that was.
	VerifiedAt          time.Time
	VerificationOutcome string
}

// ModelRouteRecord is a route alias's read-back projection.
type ModelRouteRecord struct {
	ID        string
	Name      string
	CreatedAt time.Time
}

// ProjectModelRoute resolves the project's published route, or found=false when the project has none —
// the caller then falls back to the deployment default (the env route). A published revision whose
// connection no longer resolves in this tenant is an ERROR, never a fall-through: silently running a
// tenant on the deployment credential is exactly the "route cannot silently select" failure of §27.7.
func (s *Store) ProjectModelRoute(ctx context.Context, tenant Tenant) (ModelRouteTarget, bool, error) {
	ctx = storage.ScopeToTenant(ctx, tenant.Project)
	var target ModelRouteTarget
	// All three are LEFT-JOIN columns and therefore NULLable: a revision naming a connection that no longer
	// resolves in this tenant returns a row with NULLs rather than no row, which is what makes the failure
	// below FAIL CLOSED instead of falling through to the deployment credential (§27.7).
	// thinking is NULLable for a different reason than the three above: it is absent from every revision
	// written before the key existed, which is all of them. A missing key reads as "do not ask", so every
	// route already in the tree keeps producing a byte-identical provider request.
	var provider, secretRef, baseURL, thinking *string
	err := s.pool.QueryRow(ctx, storage.Query("ResolveProjectModelRoute"), tenant.Project, DefaultModelRouteAlias).
		Scan(&target.RevisionID, &target.Revision, &target.Model, &provider, &secretRef, &baseURL, &thinking)
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
	if baseURL != nil {
		target.BaseURL = *baseURL
	}
	if thinking != nil {
		target.Thinking = *thinking
	}
	return target, true, nil
}

// The E16 T1 read-back readers (the E13 T10 write-only gap). Every read runs under the caller's
// (organization, project) scope — migration 000029's RLS is the first half of the boundary, the org/project
// predicate in each query the second — so a foreign row is invisible (a non-disclosing 404), never leaked.

// ListModelConnections returns the caller's project connections, secret REF names only.
func (s *Store) ListModelConnections(ctx context.Context, tenant Tenant) ([]ModelConnectionRecord, error) {
	ctx = storage.ScopeToTenant(ctx, tenant.Project)
	rows, err := s.pool.Query(ctx, storage.Query("ListModelConnections"), tenant.Project)
	if err != nil {
		return nil, fmt.Errorf("list model connections: %w", err)
	}
	defer rows.Close()
	out := []ModelConnectionRecord{}
	for rows.Next() {
		rec, err := scanModelConnection(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, rec)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate model connections: %w", err)
	}
	return out, nil
}

// GetModelConnection reads one connection in scope; an absent/foreign id is ErrModelConnectionNotFound.
func (s *Store) GetModelConnection(ctx context.Context, tenant Tenant, connectionID string) (ModelConnectionRecord, error) {
	ctx = storage.ScopeToTenant(ctx, tenant.Project)
	rec, err := scanModelConnection(s.pool.QueryRow(ctx, storage.Query("GetModelConnection"), connectionID, tenant.Project))
	if errors.Is(err, pgx.ErrNoRows) {
		return ModelConnectionRecord{}, ErrModelConnectionNotFound
	}
	if err != nil {
		return ModelConnectionRecord{}, err
	}
	return rec, nil
}

// scanModelConnection reads one connection row. Shared by the list and the get so the two projections
// cannot drift into disagreeing about a column — the shape scanModelRouteRevision established below.
// verified_at is NULLable (no probe has ever run); every other column is NOT NULL by 000049's default.
func scanModelConnection(row pgx.Row) (ModelConnectionRecord, error) {
	var (
		rec        ModelConnectionRecord
		verifiedAt *time.Time
	)
	if err := row.Scan(&rec.ID, &rec.Provider, &rec.SecretRef, &rec.BaseURL, &rec.CreatedAt, &verifiedAt, &rec.VerificationOutcome); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return ModelConnectionRecord{}, err
		}
		return ModelConnectionRecord{}, fmt.Errorf("scan model connection: %w", err)
	}
	if verifiedAt != nil {
		rec.VerifiedAt = *verifiedAt
	}
	return rec, nil
}

// ListModelRoutes returns the caller's project route aliases.
func (s *Store) ListModelRoutes(ctx context.Context, tenant Tenant) ([]ModelRouteRecord, error) {
	ctx = storage.ScopeToTenant(ctx, tenant.Project)
	rows, err := s.pool.Query(ctx, storage.Query("ListModelRoutes"), tenant.Project)
	if err != nil {
		return nil, fmt.Errorf("list model routes: %w", err)
	}
	defer rows.Close()
	out := []ModelRouteRecord{}
	for rows.Next() {
		var rec ModelRouteRecord
		if err := rows.Scan(&rec.ID, &rec.Name, &rec.CreatedAt); err != nil {
			return nil, fmt.Errorf("scan model route: %w", err)
		}
		out = append(out, rec)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate model routes: %w", err)
	}
	return out, nil
}

// GetModelRoute reads one route in scope; an absent/foreign id is ErrModelRouteNotFound.
func (s *Store) GetModelRoute(ctx context.Context, tenant Tenant, routeID string) (ModelRouteRecord, error) {
	ctx = storage.ScopeToTenant(ctx, tenant.Project)
	var rec ModelRouteRecord
	err := s.pool.QueryRow(ctx, storage.Query("GetModelRoute"), routeID, tenant.Project).
		Scan(&rec.ID, &rec.Name, &rec.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return ModelRouteRecord{}, ErrModelRouteNotFound
	}
	if err != nil {
		return ModelRouteRecord{}, fmt.Errorf("get model route: %w", err)
	}
	return rec, nil
}

// ListModelRouteRevisions returns a route's revisions in ascending order. The route is verified in scope
// first, so a foreign/unknown route id is ErrModelRouteNotFound — never another tenant's revision list.
func (s *Store) ListModelRouteRevisions(ctx context.Context, tenant Tenant, routeID string) ([]ModelRouteRevision, error) {
	ctx = storage.ScopeToTenant(ctx, tenant.Project)
	if err := s.requireModelRoute(ctx, tenant, routeID); err != nil {
		return nil, err
	}
	rows, err := s.pool.Query(ctx, storage.Query("ListModelRouteRevisions"), routeID)
	if err != nil {
		return nil, fmt.Errorf("list model route revisions: %w", err)
	}
	defer rows.Close()
	out := []ModelRouteRevision{}
	for rows.Next() {
		rev, err := scanModelRouteRevision(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, rev)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate model route revisions: %w", err)
	}
	return out, nil
}

// GetModelRouteRevision reads one revision of a route the caller owns. A foreign/unknown route is
// ErrModelRouteNotFound; a revision id absent from that route is ErrModelRouteRevisionNotFound.
func (s *Store) GetModelRouteRevision(ctx context.Context, tenant Tenant, routeID, revisionID string) (ModelRouteRevision, error) {
	ctx = storage.ScopeToTenant(ctx, tenant.Project)
	if err := s.requireModelRoute(ctx, tenant, routeID); err != nil {
		return ModelRouteRevision{}, err
	}
	rev, err := scanModelRouteRevision(s.pool.QueryRow(ctx, storage.Query("GetModelRouteRevision"), revisionID, routeID))
	if errors.Is(err, pgx.ErrNoRows) {
		return ModelRouteRevision{}, ErrModelRouteRevisionNotFound
	}
	if err != nil {
		return ModelRouteRevision{}, err
	}
	return rev, nil
}

// scanModelRouteRevision reads a revision row and derives model/connection_id/published from the config
// JSONB (publication state rides config->>'published_at', since 000001 gave the table no published_at
// column). Shared by the list and get readers, and by a pgx.Row or pgx.Rows alike.
func scanModelRouteRevision(row pgx.Row) (ModelRouteRevision, error) {
	var (
		rev    ModelRouteRevision
		config []byte
	)
	if err := row.Scan(&rev.ID, &rev.Revision, &config, &rev.CreatedAt); err != nil {
		return ModelRouteRevision{}, err
	}
	var cfg struct {
		Model        string  `json:"model"`
		ConnectionID string  `json:"connection_id"`
		PublishedAt  *string `json:"published_at"`
	}
	if err := json.Unmarshal(config, &cfg); err != nil {
		return ModelRouteRevision{}, fmt.Errorf("decode model route revision config: %w", err)
	}
	rev.Model, rev.ConnectionID, rev.Published = cfg.Model, cfg.ConnectionID, cfg.PublishedAt != nil
	return rev, nil
}

// CreateModelConnection binds a provider family to a secret-ref handle for one project (spec §27.2: a
// connection carries references and non-secret settings only). The ref names a secret in the E13 T3 store;
// the value is redeemed at call time and never persisted here.
//
// ponytail: project-scoped only. 000001 allows a NULL project_id (an org-wide connection), but migration
// 000029's policy compares project_id to the scoped project, so a NULL-project row is invisible to a
// project-scoped read — an org-wide connection would need a policy change, not a code change.
func (s *Store) CreateModelConnection(ctx context.Context, tenant Tenant, provider, secretRef, baseURL string) (string, error) {
	ctx = storage.ScopeToTenant(ctx, tenant.Project)
	id := newModelRoutingID("mconn")
	if _, err := s.pool.Exec(ctx, storage.Query("InsertModelConnection"), id, tenant.Project, provider, secretRef, baseURL); err != nil {
		return "", fmt.Errorf("insert model connection: %w", err)
	}
	return id, nil
}

// RecordModelConnectionVerification stamps the outcome of a credential probe on a connection the caller
// owns. It writes a CACHE, not a gate: nothing on the dispatch path reads these columns, so a failed stamp
// costs an operator a display and never a run. An absent/foreign id is ErrModelConnectionNotFound.
func (s *Store) RecordModelConnectionVerification(ctx context.Context, tenant Tenant, connectionID, outcome string) error {
	ctx = storage.ScopeToTenant(ctx, tenant.Project)
	tag, err := s.pool.Exec(ctx, storage.Query("RecordModelConnectionVerification"), connectionID, tenant.Project, outcome)
	if err != nil {
		return fmt.Errorf("record model connection verification: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrModelConnectionNotFound
	}
	return nil
}

// CreateModelRoute opens (or returns) the named alias for a project. It is get-or-create: an alias names
// ONE lineage per project, so creating it twice is not an error and mints no second lineage.
// ponytail: 000001 declares no UNIQUE(organization_id, project_id, name), so two concurrent creates could
// still both insert; resolution stays deterministic regardless (ResolveProjectModelRoute's total ordering).
func (s *Store) CreateModelRoute(ctx context.Context, tenant Tenant, name string) (string, error) {
	ctx = storage.ScopeToTenant(ctx, tenant.Project)
	var existing string
	switch err := s.pool.QueryRow(ctx, storage.Query("GetModelRouteByName"), tenant.Project, name).Scan(&existing); {
	case err == nil:
		return existing, nil
	case !errors.Is(err, pgx.ErrNoRows):
		return "", fmt.Errorf("look up model route: %w", err)
	}
	id := newModelRoutingID("mroute")
	if _, err := s.pool.Exec(ctx, storage.Query("InsertModelRoute"), id, tenant.Project, name); err != nil {
		return "", fmt.Errorf("insert model route: %w", err)
	}
	return id, nil
}

// CreateModelRouteRevision adds a DRAFT revision to a route (the E11 immutable-revision shape): a revise is
// always a NEW revision, never an edit of a published one. The route and the connection are both verified
// in scope first, so a revision can neither attach to a foreign route nor bind a foreign credential.
// thinking is the ModelRouteTarget.Thinking value this revision pins; empty means "do not ask the
// provider for reasoning", which is what every revision written before the key existed resolves to.
func (s *Store) CreateModelRouteRevision(ctx context.Context, tenant Tenant, routeID, model, connectionID, thinking string) (ModelRouteRevision, error) {
	ctx = storage.ScopeToTenant(ctx, tenant.Project)
	if err := s.requireModelRoute(ctx, tenant, routeID); err != nil {
		return ModelRouteRevision{}, err
	}
	switch err := s.pool.QueryRow(ctx, storage.Query("ModelConnectionExists"), connectionID, tenant.Project).Scan(new(int)); {
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
	fields := map[string]string{"model": model, "connection_id": connectionID}
	// The key is written only when it was asked for, so a revision that pins no reasoning preference has
	// a config blob byte-identical to one written before this key existed — and ResolveProjectModelRoute's
	// `config->>'thinking'` returns SQL NULL for both, which is the same thing to every reader.
	if thinking != "" {
		fields["thinking"] = thinking
	}
	config, _ := json.Marshal(fields)
	if _, err := s.pool.Exec(ctx, storage.Query("InsertModelRouteRevision"), id, routeID, number, config); err != nil {
		return ModelRouteRevision{}, fmt.Errorf("insert model route revision: %w", err)
	}
	return ModelRouteRevision{ID: id, Revision: number, Model: model, ConnectionID: connectionID, Thinking: thinking}, nil
}

// PublishModelRouteRevision makes a draft revision routable. Publish is the ONE legitimate mutation of a
// revision (the routing fields are never rewritten), and it is irreversible: rolling back means publishing
// a new revision. Re-publishing an already-published revision is an idempotent success.
func (s *Store) PublishModelRouteRevision(ctx context.Context, tenant Tenant, routeID, revisionID string) error {
	ctx = storage.ScopeToTenant(ctx, tenant.Project)
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
	switch err := s.pool.QueryRow(ctx, storage.Query("ModelRouteExists"), routeID, tenant.Project).Scan(new(int)); {
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
