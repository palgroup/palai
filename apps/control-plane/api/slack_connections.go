package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"time"

	"github.com/palgroup/palai/apps/control-plane/api/middleware"
)

// The Slack workspace registration surface (E19 T9, spec §36). It exists because without it the phase's own
// promise — "the owner supplies credentials and one command runs every live leg with NO code change" — was
// false: extensions.Store.CreateSlackConnection had ZERO non-test callers, so registering a workspace meant
// hand-written SQL against slack_connections. The queue surface (E19 T6) closed exactly this gap for
// queue_connections and this is its twin: same list envelope, same "tenant comes from the verified bearer"
// rule, same strict decode at the boundary.
//
// It is an ADMIN surface, INSIDE the auth middleware — unlike the Slack events/interactivity receivers,
// which carry a per-request v0 signature and therefore live on the unauthenticated top mux. A registration
// carries no source signature of its own, so the bearer scope is the only tenant authority.

// Registration refusals, declared HERE rather than re-exported from the store, because the api package
// cannot import apps/control-plane/internal/extensions (extensions imports api — the SlackEventsAPI seam).
// The adapter maps the store's typed errors onto these two.
//
// COLLAPSING THE TWO CONFLICTS INTO ONE ERROR IS THE POINT, not laziness: the store distinguishes "this
// workspace is already bound in YOUR project" (ErrSlackConnectionExists) from "…already bound by a
// DIFFERENT tenant" (ErrSlackWorkspaceBoundElsewhere — the E19 T1 cross-tenant hijack fix). The registering
// admin has no business learning which, because the second answer proves another customer exists and holds
// that workspace. Giving this handler ONE error to map means it is structurally incapable of telling them
// apart, rather than merely choosing not to.
var (
	// ErrSlackRegistrationInvalid is a body the store refused: a missing team id or signing-secret ref, an
	// unknown field, or an inline credential value.
	ErrSlackRegistrationInvalid = errors.New("api: slack connection registration is invalid")
	// ErrSlackRegistrationConflict is a workspace already bound — by this tenant or by another. One error
	// on purpose (see above).
	ErrSlackRegistrationConflict = errors.New("api: slack workspace is already bound")
)

// SlackConnectionItem is one registered workspace binding's LIST projection. It carries no secret-ref
// handles: a browse surface has no use for them, and a field that is never rendered is a field that cannot
// be logged by accident.
type SlackConnectionItem struct {
	ID           string
	TeamID       string
	EnterpriseID string
	BotUserID    string
	Disabled     bool
	CreatedAt    time.Time
}

// SlackListWindow is the keyset page window the list seam takes (the automation.ListWindow shape, declared
// here for the same import-direction reason as the errors above).
type SlackListWindow struct {
	CreatedGTE     *time.Time
	CreatedLTE     *time.Time
	AfterCreatedAt *time.Time
	AfterID        string
	Limit          int
}

// SlackConnectionAPI is the store seam for the registration surface; extensions.SlackRegistry implements it.
// Tiers that wire no Slack store pass nil and the routes stay unmounted — the same posture that lets
// discovery derive `slack` from an actual mount rather than a static string (§2).
//
// Create takes the RAW body rather than a decoded struct: the store's decodeSlackInput is already the
// STRICT decoder that refuses an inline `signing_secret` / `bot_token` / `app_token` value, and duplicating
// that shape here would create a second definition of "what a registration is" that could drift from the
// one the database actually enforces.
type SlackConnectionAPI interface {
	CreateSlackConnection(ctx context.Context, org, project string, raw []byte) (id string, err error)
	ListSlackConnections(ctx context.Context, org, project string, w SlackListWindow) ([]SlackConnectionItem, error)
	// The repair half. Each mutation reports found=false for an id that is not in the caller's tenant, so
	// the handler answers 404 without ever learning whether it exists elsewhere.
	GetSlackConnection(ctx context.Context, org, project, id string) (SlackConnectionDetail, bool, error)
	UpdateSlackConnection(ctx context.Context, org, project, id string, patch SlackConnectionPatch) (bool, error)
	DeleteSlackConnection(ctx context.Context, org, project, id string) (bool, error)
}

type slackConnectionHandler struct{ slack SlackConnectionAPI }

// slackRegistrationBody is the EDGE's copy of the accepted shape. It exists for one reason the store's own
// strict decode cannot serve: an inline credential must be refused BEFORE it reaches a store call, a pgx
// argument list or an error string built from the body. Every field here is a handle or a plain identifier;
// there is deliberately no `signing_secret`, `bot_token` or `app_token`, so DisallowUnknownFields turns an
// inline value into a 400 at the boundary.
//
// Note what is also NOT here: no organization_id, no project_id. A registration cannot describe a tenant
// even to itself — the row's tenant columns are written from the verified scope.
type slackRegistrationBody struct {
	TeamID           string          `json:"team_id"`
	EnterpriseID     string          `json:"enterprise_id"`
	BotUserID        string          `json:"bot_user_id"`
	SigningSecretRef string          `json:"signing_secret_ref"`
	BotTokenRef      string          `json:"bot_token_ref"`
	AppTokenRef      string          `json:"app_token_ref"`
	Scopes           string          `json:"scopes"`
	AllowedChannels  []string        `json:"allowed_channels"`
	AllowedUsers     []string        `json:"allowed_users"`
	DefaultPolicy    json.RawMessage `json:"default_policy"`
}

// slackDefaultPolicy is the ONLY shape default_policy may carry. The column is documented as "the default
// run policy for events on this connection" and extensions/slack_admit.go reads exactly these keys out
// of it. Pinning the shape is a security property rather than tidiness: it is tenant-supplied JSONB that
// the admission bridge reads a RUN TARGET out of, so an open shape is precisely where a bearer token or a
// second "organization_id" would be parked.
//
// E22 T3 added the repository half, and the STRUCT STAYS CLOSED: an unknown field is still a 400. The two
// new fields are OPTIONAL and `omitempty`, which is what keeps a repository-less connection BIT-unchanged —
// its canonical policy bytes are the same two keys they have always been, so no stored row moves and no
// admission behaves differently. A connection that names one binds every event on it to that repository:
// the run's coding workspace is attached at admission (spec §30.1), exactly as the `repository` field on
// POST /v1/responses does. It is CONNECTION configuration on purpose — a Slack payload can no more choose a
// repository than it can choose a principal.
type slackDefaultPolicy struct {
	AgentRevisionID string `json:"agent_revision_id"`
	PrincipalID     string `json:"principal_id"`
	// RepositoryBindingID names a binding registered in THIS tenant (POST /v1/repository-bindings). It is
	// verified to exist in scope at admission, so an unknown or foreign id refuses the event rather than
	// birthing a run that fails when the clone cannot resolve it.
	RepositoryBindingID string `json:"repository_binding_id,omitempty"`
	// RepositoryRef is the branch/tag/commit to check out. Empty falls back to the binding's default_branch
	// (adapters/repositories/prepare.go), which is ALSO the base a publication targets — leaving it empty is
	// how those two stay one value rather than two that can disagree.
	RepositoryRef string `json:"repository_ref,omitempty"`
}

// createConnection registers a workspace binding (POST /v1/slack-connections). Durable config, server-minted
// id, no Idempotency-Key — the sibling POST /v1/queue-connections posture: a rare operator action whose
// duplicate is operator-visible (and here, additionally, refused as a conflict).
func (h *slackConnectionHandler) createConnection(w http.ResponseWriter, r *http.Request) {
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
	body, detail := vetSlackRegistration(raw)
	if detail != "" {
		middleware.WriteProblem(w, r, http.StatusBadRequest, "invalid_request", detail)
		return
	}

	id, err := h.slack.CreateSlackConnection(r.Context(), scope.Organization, scope.Project, body)
	switch {
	case errors.Is(err, ErrSlackRegistrationConflict):
		// One detail for both conflict kinds: this handler cannot tell them apart, and the wording names
		// the WORKSPACE (which the caller supplied) and nothing else.
		middleware.WriteProblem(w, r, http.StatusConflict, "conflict",
			"this Slack workspace is already bound; it can be registered once")
		return
	case errors.Is(err, ErrSlackRegistrationInvalid):
		middleware.WriteProblem(w, r, http.StatusBadRequest, "invalid_request",
			"the registration was refused: team_id and signing_secret_ref are required, and every credential must be a *_ref handle")
		return
	case err != nil:
		middleware.WriteProblem(w, r, http.StatusInternalServerError, "internal_error", "")
		return
	}
	w.Header().Set("Location", "/v1/slack-connections/"+id)
	writeJSON(w, http.StatusCreated, map[string]any{"id": id, "object": "slack_connection"})
}

// vetSlackRegistration strict-decodes the accepted shape and returns the CANONICAL bytes to hand the store,
// or an operator-safe problem detail. Re-marshalling rather than forwarding the caller's bytes is what makes
// the refusal above load-bearing: an unknown key cannot survive the round trip, so nothing the edge rejected
// can reach the store by riding along in the original body.
func vetSlackRegistration(raw []byte) ([]byte, string) {
	if len(bytes.TrimSpace(raw)) == 0 {
		return nil, "a registration body is required"
	}
	var in slackRegistrationBody
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&in); err != nil {
		return nil, "the body accepts only team_id, enterprise_id, bot_user_id, the *_ref credential HANDLES, scopes, allowed_channels, allowed_users and default_policy — a raw signing secret or token is never accepted"
	}
	if in.TeamID == "" {
		return nil, "team_id is required"
	}
	if in.SigningSecretRef == "" {
		return nil, "signing_secret_ref is required (the v0 signature verify redeems it)"
	}
	if len(bytes.TrimSpace(in.DefaultPolicy)) == 0 {
		return nil, "default_policy is required: it carries the run target every event on this connection admits with"
	}
	canonicalPolicy, detail := vetSlackDefaultPolicy(in.DefaultPolicy)
	if detail != "" {
		return nil, detail
	}
	in.DefaultPolicy = canonicalPolicy
	out, err := json.Marshal(in)
	if err != nil {
		return nil, "the registration could not be encoded"
	}
	return out, ""
}

// listConnections returns a tenant-scoped page of registered workspaces (GET /v1/slack-connections), in the
// shared keyset page envelope. This is how an operator finds the id of the workspace they just registered.
func (h *slackConnectionHandler) listConnections(w http.ResponseWriter, r *http.Request) {
	scope, ok := middleware.ScopeFrom(r.Context())
	if !ok {
		middleware.WriteProblem(w, r, http.StatusUnauthorized, "authentication_required", "a bearer API key is required")
		return
	}
	q, ok := beginList(w, r, "slack_connection", scope)
	if !ok {
		return
	}
	window := SlackListWindow{
		CreatedGTE: q.CreatedGTE, CreatedLTE: q.CreatedLTE, Limit: q.Limit + 1, // +1 over-fetch: renderPage reads it as has_more
	}
	if q.After != nil {
		window.AfterCreatedAt, window.AfterID = &q.After.CreatedAt, q.After.ID
	}
	items, err := h.slack.ListSlackConnections(r.Context(), scope.Organization, scope.Project, window)
	if err != nil {
		middleware.WriteProblem(w, r, http.StatusInternalServerError, "internal_error", "")
		return
	}
	rows := make([]ListRow, 0, len(items))
	for _, it := range items {
		body, _ := json.Marshal(map[string]any{
			"id": it.ID, "object": "slack_connection", "team_id": it.TeamID,
			"enterprise_id": it.EnterpriseID, "bot_user_id": it.BotUserID, "disabled": it.Disabled,
		})
		rows = append(rows, ListRow{ID: it.ID, CreatedAt: it.CreatedAt, Body: body})
	}
	renderPage(w, r, "slack_connection", scope, rows, q.Limit)
}

// ============================================================================
// REPAIR: read one, revise, unbind.
// ============================================================================
//
// WHY THIS EXISTS, and it is an operational fact rather than a feature idea: a workspace registered before a
// field was added — an `app_token_ref` on a connection created before Socket Mode needed one — could not be
// corrected through any API. The only repair was raw SQL against slack_connections, which an operator of a
// deployment they did not build does not have and should not need. A registration surface without a revise
// is a surface that can only ever be wrong once.
//
// The tenancy rule is the create path's, unchanged: the org/project come from the verified bearer, every
// statement carries them in its predicate, and a connection belonging to another tenant answers 404 — never
// 403, which would confirm it exists.
//
// The SQUAT REFUSAL is preserved STRUCTURALLY rather than re-run: team_id and enterprise_id are not
// revisable (see UpdateSlackConnection), so there is no statement by which a binding can be moved onto a
// workspace someone else holds. A check that cannot be forgotten beats a check that must be remembered.

// SlackConnectionDetail is one binding's full read projection. It carries the secret-ref HANDLES — which is
// the point of having it: an operator repairing a mis-registered connection has to see which handles are set
// before they can say which one is wrong. A handle is a name, never a credential; the values live in the
// secret store and never enter this struct.
type SlackConnectionDetail struct {
	ID               string
	TeamID           string
	EnterpriseID     string
	BotUserID        string
	SigningSecretRef string
	BotTokenRef      string
	AppTokenRef      string
	Scopes           string
	Disabled         bool
}

// SlackConnectionPatch is a partial revision: a nil field is unchanged. Nothing here can name a tenant or a
// workspace, and nothing here is a credential VALUE — the same three exclusions the registration body makes,
// for the same three reasons.
type SlackConnectionPatch struct {
	BotUserID        *string
	SigningSecretRef *string
	BotTokenRef      *string
	AppTokenRef      *string
	Scopes           *string
	AllowedChannels  []string // nil = unchanged; an empty non-nil slice clears the list (every channel)
	AllowedUsers     []string
	DefaultPolicy    []byte // canonical {agent_revision_id, principal_id} bytes, or nil
	Disabled         *bool
}

// slackConnectionPatchBody is the edge's strict shape. Pointers distinguish "absent" from "set to empty" —
// clearing bot_token_ref is a real operation (a workspace that no longer posts) and must not be confused
// with a PATCH that simply did not mention it.
type slackConnectionPatchBody struct {
	BotUserID        *string         `json:"bot_user_id"`
	SigningSecretRef *string         `json:"signing_secret_ref"`
	BotTokenRef      *string         `json:"bot_token_ref"`
	AppTokenRef      *string         `json:"app_token_ref"`
	Scopes           *string         `json:"scopes"`
	AllowedChannels  *[]string       `json:"allowed_channels"`
	AllowedUsers     *[]string       `json:"allowed_users"`
	DefaultPolicy    json.RawMessage `json:"default_policy"`
	Disabled         *bool           `json:"disabled"`
}

// getConnection reads one binding (GET /v1/slack-connections/{connection_id}) — the address createConnection
// has always advertised in its Location header and nothing served until now.
func (h *slackConnectionHandler) getConnection(w http.ResponseWriter, r *http.Request) {
	scope, ok := middleware.ScopeFrom(r.Context())
	if !ok {
		middleware.WriteProblem(w, r, http.StatusUnauthorized, "authentication_required", "a bearer API key is required")
		return
	}
	detail, found, err := h.slack.GetSlackConnection(r.Context(), scope.Organization, scope.Project, r.PathValue("connection_id"))
	switch {
	case err != nil:
		middleware.WriteProblem(w, r, http.StatusInternalServerError, "internal_error", "")
		return
	case !found:
		middleware.WriteProblem(w, r, http.StatusNotFound, "not_found", "no such Slack connection in this project")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"id": detail.ID, "object": "slack_connection", "team_id": detail.TeamID,
		"enterprise_id": detail.EnterpriseID, "bot_user_id": detail.BotUserID,
		"signing_secret_ref": detail.SigningSecretRef, "bot_token_ref": detail.BotTokenRef,
		"app_token_ref": detail.AppTokenRef, "scopes": detail.Scopes, "disabled": detail.Disabled,
	})
}

// reviseConnection repairs a binding (PATCH /v1/slack-connections/{connection_id}).
func (h *slackConnectionHandler) reviseConnection(w http.ResponseWriter, r *http.Request) {
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
	patch, detail := vetSlackPatch(raw)
	if detail != "" {
		middleware.WriteProblem(w, r, http.StatusBadRequest, "invalid_request", detail)
		return
	}
	found, err := h.slack.UpdateSlackConnection(r.Context(), scope.Organization, scope.Project, r.PathValue("connection_id"), patch)
	switch {
	case err != nil:
		middleware.WriteProblem(w, r, http.StatusInternalServerError, "internal_error", "")
		return
	case !found:
		middleware.WriteProblem(w, r, http.StatusNotFound, "not_found", "no such Slack connection in this project")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"id": r.PathValue("connection_id"), "object": "slack_connection"})
}

// deleteConnection unbinds a workspace (DELETE /v1/slack-connections/{connection_id}). The thread
// correlations it owns go with it; the sessions and runs they pointed at do not — those are canonical
// results that merely arrived over Slack.
func (h *slackConnectionHandler) deleteConnection(w http.ResponseWriter, r *http.Request) {
	scope, ok := middleware.ScopeFrom(r.Context())
	if !ok {
		middleware.WriteProblem(w, r, http.StatusUnauthorized, "authentication_required", "a bearer API key is required")
		return
	}
	found, err := h.slack.DeleteSlackConnection(r.Context(), scope.Organization, scope.Project, r.PathValue("connection_id"))
	switch {
	case err != nil:
		middleware.WriteProblem(w, r, http.StatusInternalServerError, "internal_error", "")
		return
	case !found:
		middleware.WriteProblem(w, r, http.StatusNotFound, "not_found", "no such Slack connection in this project")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// vetSlackPatch strict-decodes a revision. It refuses the same three families the registration body refuses
// — an inline credential VALUE, a tenant field, and now a workspace field — and DisallowUnknownFields is
// what makes all three refusals structural: `team_id`, `organization_id` and `signing_secret` are simply not
// members of the accepted shape, so each is a 400 rather than a silently ignored key.
func vetSlackPatch(raw []byte) (SlackConnectionPatch, string) {
	if len(bytes.TrimSpace(raw)) == 0 {
		return SlackConnectionPatch{}, "a revision body is required"
	}
	var in slackConnectionPatchBody
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&in); err != nil {
		return SlackConnectionPatch{}, "a revision accepts only bot_user_id, the *_ref credential HANDLES, scopes, allowed_channels, allowed_users, default_policy and disabled — the workspace (team_id/enterprise_id) is immutable, and a raw secret or token is never accepted"
	}
	// A signing ref is what the v0 verify redeems; a binding without one can never authenticate an event
	// again, and the create path already refuses to make one. A revise must not be able to reach the state
	// the create path forbids.
	if in.SigningSecretRef != nil && *in.SigningSecretRef == "" {
		return SlackConnectionPatch{}, "signing_secret_ref cannot be cleared: the v0 signature verify redeems it, and a binding without one can authenticate nothing"
	}
	out := SlackConnectionPatch{
		BotUserID: in.BotUserID, SigningSecretRef: in.SigningSecretRef, BotTokenRef: in.BotTokenRef,
		AppTokenRef: in.AppTokenRef, Scopes: in.Scopes, Disabled: in.Disabled,
	}
	// A present-but-empty list is a real revision (clear the allow-list); nil is "unchanged". Normalizing
	// nil-inside-present to an empty slice keeps the two apart all the way to the statement.
	if in.AllowedChannels != nil {
		if out.AllowedChannels = *in.AllowedChannels; out.AllowedChannels == nil {
			out.AllowedChannels = []string{}
		}
	}
	if in.AllowedUsers != nil {
		if out.AllowedUsers = *in.AllowedUsers; out.AllowedUsers == nil {
			out.AllowedUsers = []string{}
		}
	}
	if len(bytes.TrimSpace(in.DefaultPolicy)) > 0 {
		// The same pinned shape the create path enforces, through the SAME function — a revise may not widen
		// what a create refused, and one decoder is the only way that stays true as the shape grows.
		canonical, detail := vetSlackDefaultPolicy(in.DefaultPolicy)
		if detail != "" {
			return SlackConnectionPatch{}, detail
		}
		out.DefaultPolicy = canonical
	}
	return out, ""
}

// vetSlackDefaultPolicy strict-decodes a run policy and returns its CANONICAL bytes. Shared by create and
// revise: this JSONB is where the admission bridge reads a RUN TARGET from, so an open shape is exactly where
// a foreign principal, a second "organization_id" or a bearer token would be parked, and two copies of the
// rule are two chances for one of them to widen.
func vetSlackDefaultPolicy(raw []byte) (canonical []byte, detail string) {
	const accepts = "default_policy accepts only agent_revision_id, principal_id, repository_binding_id and repository_ref"
	var policy slackDefaultPolicy
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&policy); err != nil {
		return nil, accepts
	}
	if policy.AgentRevisionID == "" || policy.PrincipalID == "" {
		return nil, "default_policy.agent_revision_id and default_policy.principal_id are required — a binding that has not been told what to run, or as whom, admits nothing"
	}
	// A ref with no binding is a field that does nothing: the admission attaches a workspace only when a
	// binding is named, so the ref would be accepted, stored, and silently ignored forever. This repository
	// has paid for that shape often enough (a tool nobody could advertise, a registration that resolved
	// nowhere) that a settable-but-inert field is refused rather than documented.
	if policy.RepositoryRef != "" && policy.RepositoryBindingID == "" {
		return nil, "default_policy.repository_ref names a ref with no repository_binding_id to check it out of; set the binding too, or drop the ref"
	}
	out, err := json.Marshal(policy)
	if err != nil {
		return nil, "default_policy could not be encoded"
	}
	return out, ""
}
