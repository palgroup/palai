package palai

import (
	"context"
	"encoding/json"
	"net/http"
)

// Bots is the /v1/bots resource group (control plane Task 4, apps/control-plane/api/bots.go): a
// project's registered bots — one row per channel adapter (Slack, …) that talks through this
// control plane. This SDK ships read-only Get for now: Task 5's slack-bot process fetches its own
// row at startup, which is the only caller this task has. List/Create/Patch/Delete are a later
// task's surface if a caller needs them.
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
