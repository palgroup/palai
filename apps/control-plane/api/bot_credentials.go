package api

import (
	"context"
	"encoding/json"
	"net/http"

	"github.com/palgroup/palai/apps/control-plane/api/middleware"
)

// GET /v1/bots/{bot_id}/credentials — the one route that lets a relay process running OUTSIDE this
// control plane redeem the secret handles its own registry row names.
//
// WHY IT EXISTS. A sealed secret has no read-back path — secret_refs.go states it as a property ("the
// value is envelope-encrypted at rest and has NO read-back path") and it was measured live on 2026-08-03:
// POST /v1/secret-refs → 201, then GET /v1/secret-refs/{name} → {name,object,version,updated_at}, with no
// parameter that adds the value. Every resolver that DOES redeem a handle runs inside THIS process,
// holding the master key. A relay process does not, so a bot could be fully configured in the console and
// could still never start. This route closes exactly that gap and nothing wider.
//
// THE PROPERTY THAT KEEPS THE HOLE NARROW: THE CALLER NAMES A BOT, NEVER A SECRET. The request carries a
// bot id in the path and nothing else — no body, no query field, no header a name could ride in. What
// gets resolved is exactly the values of the keys ending in `_ref` in THAT row's own config document,
// read through the same tenant-scoped registry read GET /v1/bots/{bot_id} performs. So this route cannot
// be turned into an arbitrary secret reader by any input, and that is a STRUCTURAL property rather than a
// validation one: there is no input to validate.
//
// The generic `_ref` walk is what makes that true, and it is also the registry's own architecture rule —
// `kind` is a bare string this package never branches on and `config` is opaque to it. Special-casing a
// particular channel's key names would break both at once. The walk itself lives in internal/botcreds.
type botCredentialsHandler struct{ credentials BotCredentials }

// botCredentialsScope is the capability a key must hold to redeem a bot's credentials. It is a NEW
// capability rather than a reuse of `provision`, and the argument is here because this line is where the
// next reader will come looking.
//
// A gate is needed at all because reading a bot's credentials is strictly more powerful than reading its
// row, and the registry routes themselves carry no capability (see router.go). `provision` — the gate the
// secret-ref routes use — was the obvious candidate and was rejected: the key that holds this capability
// belongs to a LONG-LIVED RELAY PROCESS, which is the most exposed credential in a deployment (it sits in
// a bot dialled into a third-party chat service), while `provision` is tenancy administration — creating
// organizations, minting API keys, sealing secret-refs. Gating here on `provision` would make every bot's
// API key a provisioning key, so the blast radius of one leaked bot credential would be the whole tenant.
//
// A dedicated capability costs one string and buys a key that can redeem the handles of bots in its own
// project and do nothing else. It composes with the existing model unchanged: HasScope treats an empty
// scope set as unrestricted, so today's admin and bootstrap keys keep working with no migration, and an
// operator who wants a narrow bot key mints one with scopes ["bots.credentials"] (POST /v1/api-keys takes
// the list verbatim — there is no allow-list to extend).
const botCredentialsScope = "bots.credentials"

// BotCredentials resolves the secret handles named in one bot's own config document.
// internal/botcreds implements it; a stack that wires no resolver leaves the route unmounted.
type BotCredentials interface {
	ResolveBotCredentials(ctx context.Context, scope middleware.Scope, botID string) (BotCredentialsResult, error)
}

// BotCredentialsResult is one resolution.
//
// Values is keyed by the CONFIG KEY (`app_token_ref`, …) and never by the secret NAME: the config key is
// what the caller's own document already told it to look for, whereas keying by name would mean handing
// the caller a mapping from names to values — the shape this route exists to avoid being.
//
// The values are []byte rather than string on purpose. They cross three function boundaries between the
// store and the one line that renders them, and a []byte does not land in a %v as a readable token by
// accident. Nothing on this path logs, journals or echoes a value, which is the rule the capability-worker
// gateway states for its own redemption ("NEVER written to the journal, a log, or evidence") and this
// route inherits.
//
// Unresolved names the config keys that carry a handle the store could not turn into a value — either the
// key holds no usable name, or no secret was ever sealed under it. It is a field rather than a refusal
// because this package cannot know which key a given channel actually needs; that is the same
// kind-agnosticism the walk rests on, and the decision belongs to the caller, which knows. It is present
// rather than a silent omission because "the key is simply missing from the map" is precisely the failure
// shape that gets read downstream as "the value was empty".
type BotCredentialsResult struct {
	Values     map[string][]byte
	Unresolved []string
	NotFound   bool
}

// botCredentialsView is the wire projection. `unresolved` is always rendered — as `[]` when everything
// resolved — so a caller that checks it is never reading an absent field.
type botCredentialsView struct {
	BotID       string            `json:"bot_id"`
	Object      string            `json:"object"`
	Credentials map[string]string `json:"credentials"`
	Unresolved  []string          `json:"unresolved"`
}

// get resolves one bot's credentials within the verified scope.
func (h *botCredentialsHandler) get(w http.ResponseWriter, r *http.Request) {
	scope, ok := middleware.ScopeFrom(r.Context())
	if !ok {
		middleware.WriteProblem(w, r, http.StatusUnauthorized, "authentication_required", "a bearer API key is required")
		return
	}
	if !scope.HasScope(botCredentialsScope) {
		middleware.WriteProblem(w, r, http.StatusForbidden, "insufficient_scope",
			"this API key lacks the "+botCredentialsScope+" capability")
		return
	}
	botID := r.PathValue("bot_id")
	out, err := h.credentials.ResolveBotCredentials(r.Context(), scope, botID)
	if err != nil {
		// The error is deliberately not rendered. A resolution failure can carry a secret NAME (the store
		// tags a decrypt failure with the name it was opening), and while a name is not a value, an error
		// body is the one place bytes get reflected back to a caller — so this answers with nothing.
		middleware.WriteProblem(w, r, http.StatusInternalServerError, "internal_error", "")
		return
	}
	if out.NotFound {
		// Byte-identical to GetBot's 404, so an absent id and a foreign one are indistinguishable and this
		// route discloses no cross-tenant existence that GET /v1/bots/{bot_id} does not already withhold.
		middleware.WriteProblem(w, r, http.StatusNotFound, "not_found", "no such bot in this project")
		return
	}

	values := make(map[string]string, len(out.Values))
	for key, value := range out.Values {
		values[key] = string(value)
	}
	unresolved := out.Unresolved
	if unresolved == nil {
		unresolved = []string{}
	}
	body, err := json.Marshal(botCredentialsView{
		BotID: botID, Object: "bot_credentials", Credentials: values, Unresolved: unresolved,
	})
	if err != nil {
		middleware.WriteProblem(w, r, http.StatusInternalServerError, "internal_error", "")
		return
	}
	w.Header().Set("Content-Type", "application/json")
	// no-store, and here it is load-bearing rather than belt-and-braces: this body carries live tokens, so
	// no intermediary, proxy or client cache may keep a copy.
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(body)
}
