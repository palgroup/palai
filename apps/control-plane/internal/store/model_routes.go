package store

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strconv"
	"strings"

	"github.com/palgroup/palai/apps/control-plane/api"
	"github.com/palgroup/palai/apps/control-plane/api/middleware"
	"github.com/palgroup/palai/packages/coordinator"
	"github.com/palgroup/palai/packages/egress"
	modelbroker "github.com/palgroup/palai/packages/model-broker"
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
//
// SINCE E29 IT ALSO CHECKS WHAT IT IS BEING TOLD. Before, `provider` was any string: `{"provider":"openai"}`
// — the name this product's own prose uses — got a 201, was stored, was published onto a route, and killed
// the first model step of the first run with `unknown_provider: openai`. Nothing in between had a reason to
// look, because nothing knew what the accepted values WERE; the adapter map was a literal in the
// composition root. The families are now declared in modelbroker.Families(), which is the same list the
// adapter map is built from, so a family this accepts is a family the broker can dial.
func (s *Store) CreateModelConnection(ctx context.Context, scope middleware.Scope, body []byte) (api.ProvisionResult, error) {
	if out, ok := requireProjectScope(scope); !ok {
		return out, nil
	}
	var in struct {
		Provider  string `json:"provider"`
		SecretRef string `json:"secret_ref"`
		BaseURL   string `json:"base_url"`
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
	if out, ok := vetConnectionEndpoint(in.Provider, in.BaseURL); !ok {
		return out, nil
	}
	id, err := s.spine.CreateModelConnection(ctx, tenantOf(scope), in.Provider, in.SecretRef, in.BaseURL)
	if err != nil {
		return api.ProvisionResult{}, err
	}
	out := map[string]any{
		"id": id, "object": "model_connection", "provider": in.Provider, "secret_ref": in.SecretRef,
	}
	if in.BaseURL != "" {
		out["base_url"] = in.BaseURL
	}
	return api.ProvisionResult{Body: mustJSON(out)}, nil
}

// vetConnectionEndpoint enforces the family rule and the egress rule on a connection's endpoint. ok=false
// carries the refusal to render.
//
// THE FAMILY RULE. A base URL is accepted only by the family whose whole purpose is a custom endpoint, and
// REQUIRED by it. The second half is the one that matters: an `openai-compatible` connection with an empty
// endpoint falls back to the family default, which is api.openai.com — so an operator who meant to keep
// their prompts on their own vLLM would ship them to OpenAI under a key minted for something else, and
// nothing would say so. The first half is the mirror: a base URL on a family named "OpenAI" is a custom
// endpoint wearing a disguise, and the operator who wants one has a family named for it.
//
// THE EGRESS RULE. This address is one the control plane will dial, so it is vetted through the shared
// SSRF layer every other outbound caller vets through (packages/egress) rather than a local URL check.
//
// allowPrivate IS TRUE, AND THAT IS A DECISION. A self-hosted deployment's own Ollama lives on localhost or
// on the LAN; denying private destinations would deny the single most common custom endpoint there is, on a
// product whose promise is "your own resources". What stays denied under that flag is what is never a
// legitimate destination for anyone — the cloud metadata address and the special-use ranges around it,
// unspecified, multicast — so the flag opens the operator's own network, not the instance's credentials.
// The caller here already holds the `provision` capability, which is the capability that writes secrets.
func vetConnectionEndpoint(provider, baseURL string) (api.ProvisionResult, bool) {
	family, known := modelbroker.LookupFamily(provider)
	if !known {
		return api.ProvisionResult{MissingField: "a known provider family (one of " +
			strings.Join(modelbroker.FamilyNames(), ", ") + "); got " + strconv.Quote(provider)}, false
	}
	switch {
	case baseURL == "" && family.RequiresBaseURL:
		return api.ProvisionResult{MissingField: "base_url (the " + family.Label +
			" family has no endpoint of its own; without one this connection would dial the OpenAI API)"}, false
	case baseURL != "" && !family.AcceptsBaseURL:
		return api.ProvisionResult{MissingField: "no base_url (the " + family.Label +
			" family has its own endpoint; for a custom one use the openai-compatible family)"}, false
	case baseURL == "":
		return api.ProvisionResult{}, true
	}
	if err := egress.VetURL(baseURL, true); err != nil {
		// The URL is NOT echoed: an operator can paste one carrying a token in its userinfo or query, and
		// this string reaches a response body and a log.
		return api.ProvisionResult{MissingField: "a base_url the control plane may dial (it must be http(s) " +
			"and must not name a metadata, multicast or unspecified address)"}, false
	}
	return api.ProvisionResult{}, true
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
//
// base_url IS RENDERED AND THE CREDENTIAL IS NOT, and the asymmetry is the point rather than an
// inconsistency: an endpoint is an address the operator chose and must be able to check, while a key is a
// value they wrote once and can never read back. A screen that shows neither cannot tell an operator which
// of two connections points at their vLLM. The URL is rendered as STORED — the write path vetted it, and
// vetting rejects userinfo-bearing schemes before a row exists.
func modelConnectionView(rec coordinator.ModelConnectionRecord) map[string]any {
	out := map[string]any{
		"id": rec.ID, "object": "model_connection", "provider": rec.Provider,
		"secret_ref": rec.SecretRef, "created_at": rec.CreatedAt,
	}
	if rec.BaseURL != "" {
		out["base_url"] = rec.BaseURL
	}
	// The last probe. Both fields or neither: a `verification_outcome` with no timestamp would be a verdict
	// with no age, and an operator reading "accepted" cannot tell whether that was measured a minute ago or
	// before the key was rotated.
	if !rec.VerifiedAt.IsZero() && rec.VerificationOutcome != "" {
		out["verified_at"] = rec.VerifiedAt
		out["verification_outcome"] = rec.VerificationOutcome
	}
	return out
}

// ConnectionProber is the seam the verify action performs its REAL network call through: the family's
// adapter, which knows that family's auth header and endpoint shape. Production wires
// adapters/models/registry.Probers(); a test wires a double and never leaves the process.
type ConnectionProber interface {
	ProbeCredential(ctx context.Context, baseURL, secret string) modelbroker.Probe
}

// ConnectionSecretResolver redeems a connection's credential for a probe. It is the SAME shape the model
// broker's own resolver has and it is given the same value in production (the T3 secret store, org-scoped),
// so the probe checks the bytes a run would actually send — a probe that read the credential by any other
// path would be verifying something other than what ships.
type ConnectionSecretResolver func(org, ref string) ([]byte, bool, error)

// WithModelConnectionProbes wires the verify action. A store built WITHOUT it still serves every other
// model-routing route and answers verify honestly — "no probe is wired on this deployment" — rather than
// pretending to have checked. Nothing about dispatch depends on it.
func (s *Store) WithModelConnectionProbes(probers map[string]ConnectionProber, secrets ConnectionSecretResolver) *Store {
	s.connectionProbers, s.connectionSecrets = probers, secrets
	return s
}

// VerifyModelConnection probes the connection's credential against its endpoint and stamps the outcome.
//
// THE PROBE IS REAL — a network call to the endpoint the run would dial, redeeming the credential the run
// would redeem, through the same secret store. The brief this work was done against put it plainly: a test
// connection that only checks the string is non-empty is theatre, and this tree's rule is to ship nothing
// rather than a fake green. So every path that did NOT reach a verdict says so with its own outcome
// (`not_probed`) instead of borrowing one that reads as a pass.
//
// THE STAMP IS A CACHE, NEVER A GATE. No dispatch path reads verified_at, so a connection that was never
// probed routes exactly like one that was, and a failure to write the stamp costs a display and never a
// run — hence the deliberate discard below.
func (s *Store) VerifyModelConnection(ctx context.Context, scope middleware.Scope, connectionID string) (api.ProvisionResult, error) {
	if out, ok := requireProjectScope(scope); !ok {
		return out, nil
	}
	tenant := tenantOf(scope)
	rec, err := s.spine.GetModelConnection(ctx, tenant, connectionID)
	if errors.Is(err, coordinator.ErrModelConnectionNotFound) {
		return api.ProvisionResult{NotFound: true}, nil
	}
	if err != nil {
		return api.ProvisionResult{}, err
	}

	probe := s.probeConnection(ctx, tenant, rec)
	if probe.Outcome != modelbroker.ProbeUnsupported || probe.Status != 0 {
		// Stamp anything that reached the endpoint. A `not_probed` that never left the process (no prober
		// wired, no derivable URL) is a fact about THIS deployment's wiring, not about the connection, and
		// writing it onto the row would age into a claim about the credential.
		_ = s.spine.RecordModelConnectionVerification(ctx, tenant, rec.ID, string(probe.Outcome))
	}
	return api.ProvisionResult{Body: mustJSON(map[string]any{
		"object": "model_connection_verification", "connection_id": rec.ID, "provider": rec.Provider,
		"outcome": probe.Outcome, "status": probe.Status, "endpoint": probe.Endpoint, "detail": probe.Detail,
		// What a probe DOES NOT establish, said in the response rather than only in a comment: the operator
		// reading a green here must not conclude their route's model id is valid.
		"checked": "that the endpoint answered and did not reject this credential — NOT that the route's model id exists, nor that a quota remains",
	})}, nil
}

// probeConnection redeems the credential and runs the family's probe. Every refusal returns ProbeUnsupported
// with a sentence naming what was not done, because the one thing this function must never do is answer
// "accepted" without having asked.
func (s *Store) probeConnection(ctx context.Context, tenant coordinator.Tenant, rec coordinator.ModelConnectionRecord) modelbroker.Probe {
	prober, ok := s.connectionProbers[rec.Provider]
	if !ok || s.connectionSecrets == nil {
		return modelbroker.Probe{Outcome: modelbroker.ProbeUnsupported,
			Detail: "this deployment wires no credential probe for the " + rec.Provider + " family, so NOTHING was checked"}
	}
	value, found, err := s.connectionSecrets(tenant.Organization, rec.SecretRef)
	if err != nil || !found {
		// The two causes are named together for the reason the broker's own resolver names them: an operator
		// hunting a missing ref during a store outage looks in the wrong place for a long time.
		return modelbroker.Probe{Outcome: modelbroker.ProbeUnsupported,
			Detail: "the credential behind secret ref " + strconv.Quote(rec.SecretRef) + " did not resolve — " +
				"no such ref, or the secret store was unreachable. NOTHING was checked."}
	}
	// The credential lives only in this frame and the probe's Authorization header.
	return prober.ProbeCredential(ctx, rec.BaseURL, string(value))
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
