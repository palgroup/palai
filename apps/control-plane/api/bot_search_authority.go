package api

import (
	"encoding/json"
	"io"
	"net/http"

	"github.com/palgroup/palai/apps/control-plane/api/middleware"
)

// PUT /v1/bots/{bot_id}/slack-search-authority — how a relay running OUTSIDE this control plane tells it
// that the next run in a session may search the workspace, and with which one-turn credential.
//
// WHY IT IS A ROUTE OF ITS OWN AND NOT A FIELD ON POST /v1/responses. The obvious carrier is the response
// create body — it already accepts a free-form `context` object, and the schema's additionalProperties is
// true, so an action_token could ride it with no new surface at all. It is the wrong place for two reasons
// that are not style. First, the create body is the ONE request in this system whose contents are journalled
// by design: `responses.input` stores the turn, `commands.payload` and `events.payload` store what the run
// then does. A credential travelling in that envelope is one future "log the request for debugging" away
// from being written down, and the whole design of this token is that it is never persisted. Second, the
// grant has to be in effect BEFORE the run exists: a run asks the broker which tools it may have as it
// starts, so a credential arriving with the create call would have to be read, resolved and granted inside
// the hot admission path. Keeping it on its own route means the create path never sees it.
//
// WHAT CROSSES THE WIRE IS THE ACTION TOKEN AND NOTHING ELSE, and this is the security decision the route
// exists to make. The alternative was for the relay to send the whole search authority — team, API base and
// the BOT TOKEN — since it already holds all of it. That was rejected: the bot token is long-lived and broad
// (this deployment's carries channels:history, im:history, files:read and users:read among eight scopes),
// while an action_token authorises one search on one conversation and expires on its own. More decisive,
// the control plane ALREADY HOLDS the bot token — it is sealed in this installation's secret store under the
// handle the bot's own row names — so sending it would be putting a secret on the network that the receiver
// can already open locally. That is pure downside: a second copy in a second place, and nothing gained. The
// handler therefore resolves the bot token itself, through the same tenant-scoped registry read
// GET /v1/bots/{bot_id} performs, and the relay never transmits it.
//
// IT IS A PUT BECAUSE IT IS A REPLACE. Each turn overwrites the previous grant, and a turn with no token is
// an empty `action_token`, which WITHDRAWS. That is not tidying — see tools.SearchAuthorities.GrantSession:
// measured live on 2026-08-04, an addressed message carries an action_token and a thread reply does not, so
// a grant left standing from an earlier turn would hand a later reply a power its own message never granted.
// Modelling it as a replace makes the withdraw the same call as the grant, so there is no second request a
// relay can forget to send.
type botSearchAuthorityHandler struct {
	credentials BotCredentials
	grants      SlackSearchGrants
	// apiBase is this deployment's Slack Web API root. It is the CONTROL PLANE's own configuration and is
	// deliberately not a request field: accepting it would let a caller aim a bot's sealed token at a host
	// of its choosing, which is the credential-exfiltration shape this route must not have.
	apiBase string
}

// slackAPIBase is the vendor default, applied when the deployment names none — the same default
// extensions/slack_decision.go applies for the in-process admission route.
const slackAPIBase = "https://slack.com/api"

// SlackSearchGrants is the narrow seam this route writes through; *tools.SearchAuthorities satisfies it
// structurally. It is declared here rather than imported so the api package keeps no dependency on the
// execution tree — the same reason SearchAuthorities.Grant takes primitives instead of its own struct.
type SlackSearchGrants interface {
	GrantSession(sessionID, teamID, apiBase string, botToken []byte, actionToken string)
}

// botSearchAuthorityRequest is the wire body. There is deliberately no bot_token field and no api_base
// field: both are the control plane's own knowledge, and a route that ACCEPTED them would let a caller
// point a bot's sealed credential at a host of its choosing.
type botSearchAuthorityRequest struct {
	SessionID string `json:"session_id"`
	// TeamID is a workspace identifier, not a secret, and it is used for ONE thing: the per-workspace pacer
	// key. It is accepted rather than derived because the event carries it and a bot row may name a
	// different workspace than the message did; being wrong costs pacing accuracy and nothing else, since
	// the search itself is authorised entirely by the token pair.
	TeamID string `json:"team_id"`
	// ActionToken is the credential. Empty WITHDRAWS the session's authority — see the type's doc comment.
	ActionToken string `json:"action_token"`
}

// botSearchAuthorityScope is the capability a key must hold. It REUSES the bot-credentials capability
// rather than minting a second one, and the argument is the principal: the caller here is the same
// long-lived relay process, holding the same key, that redeems the bot's tokens through
// GET /v1/bots/{bot_id}/credentials. This route is strictly LESS powerful than that one — it discloses
// nothing at all, it only accepts a short-lived credential the caller already has — so a key that can
// already read the bot's long-lived tokens gains no reach by being allowed to hand one back. Minting a
// second capability would mean every narrowly-scoped bot key ever issued breaks on the day its bot wants
// to search, for no security gain.
const botSearchAuthorityScope = botCredentialsScope

// maxSearchAuthorityBody bounds the read. The body is three short strings; anything larger is a mistake or
// a probe, and neither should be buffered.
const maxSearchAuthorityBody = 8 << 10

// put records or withdraws one session's search authority.
func (h *botSearchAuthorityHandler) put(w http.ResponseWriter, r *http.Request) {
	scope, ok := middleware.ScopeFrom(r.Context())
	if !ok {
		middleware.WriteProblem(w, r, http.StatusUnauthorized, "authentication_required", "a bearer API key is required")
		return
	}
	if !scope.HasScope(botSearchAuthorityScope) {
		middleware.WriteProblem(w, r, http.StatusForbidden, "insufficient_scope",
			"this API key lacks the "+botSearchAuthorityScope+" capability")
		return
	}

	var req botSearchAuthorityRequest
	dec := json.NewDecoder(io.LimitReader(r.Body, maxSearchAuthorityBody))
	// DisallowUnknownFields so a caller that sends `{"bot_token": "..."}` — the shape somebody WILL try,
	// because the relay is holding one — is told no instead of having it silently ignored while believing
	// the search was authorised with it.
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		middleware.WriteProblem(w, r, http.StatusBadRequest, "invalid_request", "the request body is not a valid search-authority document")
		return
	}
	if req.SessionID == "" {
		middleware.WriteProblem(w, r, http.StatusBadRequest, "invalid_request", "session_id is required")
		return
	}

	botID := r.PathValue("bot_id")
	// THE REGISTRY READ COMES FIRST and a miss returns before any secret is opened, which is the ordering
	// property bot_credentials.go states for the same resolver: a caller naming a bot in another tenant
	// reaches the secret store zero times.
	out, err := h.credentials.ResolveBotCredentials(r.Context(), scope, botID)
	if err != nil {
		// Not rendered: a resolution failure can carry a secret NAME, and an error body is the one place
		// bytes get reflected to a caller.
		middleware.WriteProblem(w, r, http.StatusInternalServerError, "internal_error", "")
		return
	}
	if out.NotFound {
		// Byte-identical to GetBot's 404 so an absent id and a foreign one stay indistinguishable.
		middleware.WriteProblem(w, r, http.StatusNotFound, "not_found", "no such bot in this project")
		return
	}
	botToken := out.Values["bot_token_ref"]
	if len(botToken) == 0 && req.ActionToken != "" {
		// A GRANT needs a token to search with; a WITHDRAW does not, and must still be honoured — refusing
		// it would leave the previous turn's authority standing, which is the one outcome this route must
		// never produce.
		middleware.WriteProblem(w, r, http.StatusConflict, "bot_not_configured",
			"this bot names no redeemable bot_token_ref, so no search can be authorised with it")
		return
	}

	apiBase := h.apiBase
	if apiBase == "" {
		apiBase = slackAPIBase
	}
	h.grants.GrantSession(req.SessionID, req.TeamID, apiBase, botToken, req.ActionToken)

	// 204: there is nothing to return, and returning nothing is also the point — an echo of the request
	// would put the action_token in a response body.
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusNoContent)
}
