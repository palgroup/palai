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
// run policy for events on this connection" and extensions/slack_admit.go reads exactly these two keys out
// of it. Pinning the shape is a security property rather than tidiness: it is tenant-supplied JSONB that
// the admission bridge reads a RUN TARGET out of, so an open shape is precisely where a bearer token or a
// second "organization_id" would be parked.
type slackDefaultPolicy struct {
	AgentRevisionID string `json:"agent_revision_id"`
	PrincipalID     string `json:"principal_id"`
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
	var policy slackDefaultPolicy
	pdec := json.NewDecoder(bytes.NewReader(in.DefaultPolicy))
	pdec.DisallowUnknownFields()
	if err := pdec.Decode(&policy); err != nil {
		return nil, "default_policy accepts only agent_revision_id and principal_id"
	}
	if policy.AgentRevisionID == "" || policy.PrincipalID == "" {
		return nil, "default_policy.agent_revision_id and default_policy.principal_id are required — a binding that has not been told what to run, or as whom, admits nothing"
	}
	canonicalPolicy, err := json.Marshal(policy)
	if err != nil {
		return nil, "default_policy could not be encoded"
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
