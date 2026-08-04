package palai

import (
	"context"
	"encoding/json"
	"net/http"
)

// Bots is the /v1/bots resource group (control plane Task 4, apps/control-plane/api/bots.go): a
// project's registered bots — one row per channel adapter (Slack, …) that talks through this
// control plane. This SDK ships the two reads a relay process makes at startup and nothing else: Get
// for its own row, and Credentials for the sealed values that row's handles name. List/Create/Patch/
// Delete are a later task's surface if a caller needs them.
type Bots struct{ client *Client }

// Bot is one registered bot's projection (apps/control-plane/internal/bots's wireBot, rendered
// verbatim by the server — no field is optional on the wire). Config is carried as
// json.RawMessage and never parsed by this SDK, mirroring the control plane's own opacity: a bot's
// kind-specific settings (a Slack token's secret handle, its channels, …) live inside it, and this
// SDK does not need to know what a `kind` means to fetch the row.
type Bot struct {
	ID                  string                     `json:"id"`
	Object              string                     `json:"object"`
	Name                string                     `json:"name"`
	Kind                string                     `json:"kind"`
	AgentRevisionID     string                     `json:"agent_revision_id"`
	RepositoryBindingID string                     `json:"repository_binding_id"`
	PrincipalID         string                     `json:"principal_id"`
	Config              json.RawMessage            `json:"config"`
	Disabled            bool                       `json:"disabled"`
	CreatedAt           string                     `json:"created_at"`
	Extra               map[string]json.RawMessage `json:"-"`
}

func (b *Bot) UnmarshalJSON(raw []byte) error {
	type alias Bot
	var a alias
	if err := forwardUnmarshal(raw, &a, &a.Extra); err != nil {
		return err
	}
	*b = Bot(a)
	return nil
}

func (b Bot) MarshalJSON() ([]byte, error) {
	type alias Bot
	return forwardMarshal(alias(b), b.Extra)
}

// Get reads one bot by id (GET /v1/bots/{bot_id}); an absent or foreign id is a 404 *APIError.
func (b *Bots) Get(ctx context.Context, id string, opts ...CallOption) (*Bot, error) {
	o := requestOptions{}
	applyCallOptions(&o, opts)
	var out Bot
	path := "/v1/bots/" + escapePathSegment(id)
	if err := b.client.doJSON(ctx, http.MethodGet, path, o, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// BotCredentials is one bot's redeemed credentials (apps/control-plane/api/bot_credentials.go).
//
// Credentials is keyed by the CONFIG KEY the bot's own row carries — `app_token_ref`, … — and holds the
// VALUE sealed under the name that key names. The caller reads it with the same key it read the handle
// with, and never needs to know a secret's name.
//
// Unresolved names the config keys the control plane could not turn into a value: no secret is sealed
// under that name, or the key holds no usable name. IT IS THE FIELD THAT MAKES A MISSING CREDENTIAL LOUD —
// a caller reading only Credentials would see an absent map entry and could mistake it for an empty
// token, so a caller that needs a particular key should check this list before it decides it is
// configured.
type BotCredentials struct {
	BotID       string                     `json:"bot_id"`
	Object      string                     `json:"object"`
	Credentials map[string]string          `json:"credentials"`
	Unresolved  []string                   `json:"unresolved"`
	Extra       map[string]json.RawMessage `json:"-"`
}

func (c *BotCredentials) UnmarshalJSON(raw []byte) error {
	type alias BotCredentials
	var a alias
	if err := forwardUnmarshal(raw, &a, &a.Extra); err != nil {
		return err
	}
	*c = BotCredentials(a)
	return nil
}

func (c BotCredentials) MarshalJSON() ([]byte, error) {
	type alias BotCredentials
	return forwardMarshal(alias(c), c.Extra)
}

// Credentials redeems the secret handles named in one bot's own config
// (GET /v1/bots/{bot_id}/credentials). An absent or foreign id is a 404 *APIError, and a key lacking the
// `bots.credentials` capability is a 403.
//
// THE ONLY THING THIS METHOD CAN ASK FOR IS A BOT. There is no parameter for a secret name, because the
// route has none: what it resolves is fixed by the row, so this call can never be pointed at a secret the
// caller's own bot does not already name.
//
// The returned values are live credentials. Nothing in this SDK logs them, and a caller should treat the
// map the way it treats a token it read from anywhere else.
func (b *Bots) Credentials(ctx context.Context, id string, opts ...CallOption) (*BotCredentials, error) {
	o := requestOptions{}
	applyCallOptions(&o, opts)
	var out BotCredentials
	path := "/v1/bots/" + escapePathSegment(id) + "/credentials"
	if err := b.client.doJSON(ctx, http.MethodGet, path, o, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// SlackSearchAuthorityParams is one turn's Slack search grant.
//
// ActionToken is a CREDENTIAL and is the only secret this SDK ever sends. It authorises one Slack
// Real-time Search call on behalf of the conversation the message arrived in, and it is deliberately the
// ONLY secret on this request: the bot's long-lived token is never transmitted, because the control plane
// already holds it sealed and resolves it from the bot id in the path.
//
// AN EMPTY ActionToken IS A WITHDRAWAL, not a no-op. Slack attaches a token to the message that ADDRESSES
// the app and to nothing else — measured 2026-08-04: an app_mention and the message.channels of the same
// message each carried one, while a thread reply, a file share, an edit and every bot-authored message
// carried none. A relay must therefore call this on EVERY turn with whatever the message carried, so the
// tokenless turn clears the previous turn's grant instead of inheriting it.
type SlackSearchAuthorityParams struct {
	SessionID   string `json:"session_id"`
	TeamID      string `json:"team_id,omitempty"`
	ActionToken string `json:"action_token"`
}

// GrantSlackSearchAuthority records (or withdraws) the Slack search authority for the next run in a
// session — PUT /v1/bots/{bot_id}/slack-search-authority. It is a PUT because it REPLACES: each turn
// overwrites the last, and there is no separate revoke call to forget.
//
// It answers 204 with no body, so nothing is decoded — an echo would put the action_token in a response.
func (b *Bots) GrantSlackSearchAuthority(ctx context.Context, id string, p SlackSearchAuthorityParams, opts ...CallOption) error {
	o := requestOptions{body: p}
	applyCallOptions(&o, opts)
	path := "/v1/bots/" + escapePathSegment(id) + "/slack-search-authority"
	return b.client.doJSON(ctx, http.MethodPut, path, o, nil)
}
