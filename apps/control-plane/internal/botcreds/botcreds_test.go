package botcreds

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/palgroup/palai/apps/control-plane/api"
	"github.com/palgroup/palai/apps/control-plane/api/middleware"
	"github.com/palgroup/palai/packages/coordinator"
)

// fakeRegistry answers one bot projection, or a miss, and records the scope it was read with.
type fakeRegistry struct {
	config     string
	missing    bool
	err        error
	scopeAsked middleware.Scope
}

func (f *fakeRegistry) GetBot(_ context.Context, scope middleware.Scope, id string) (api.BotResult, error) {
	f.scopeAsked = scope
	if f.err != nil {
		return api.BotResult{}, f.err
	}
	if f.missing {
		return api.BotResult{NotFound: true}, nil
	}
	config := f.config
	if config == "" {
		config = `{}`
	}
	body, err := json.Marshal(map[string]any{
		"id": id, "object": "bot", "name": "a-bot", "kind": "a-kind",
		"config": json.RawMessage(config), "disabled": false,
	})
	if err != nil {
		return api.BotResult{}, err
	}
	return api.BotResult{Body: body}, nil
}

// fakeSecrets holds sealed values by name and records EVERY name it was asked for — which is what several
// assertions below are actually about: not what came back, but what was asked.
//
// It ALSO records the tenant of each ask, and that field is the reason this fake was touched at all. The
// struct used to carry an `org` nothing set and nothing read, left behind when A.2 removed organizations;
// replacing it with a recorded ask is the difference between a fake that mirrors the seam and one that can
// be asserted against — this tree has shipped fakes that inherited production's bug precisely because they
// only mirrored it.
type fakeSecrets struct {
	sealed       map[string]string
	asked        []string
	askedTenants []string
	err          error
}

func (f *fakeSecrets) Resolve(_ context.Context, tenant coordinator.Tenant, name string) ([]byte, bool, error) {
	f.asked = append(f.asked, name)
	f.askedTenants = append(f.askedTenants, tenant.Project)
	if f.err != nil {
		return nil, false, f.err
	}
	v, ok := f.sealed[name]
	if !ok {
		return nil, false, nil
	}
	return []byte(v), true, nil
}

func scope() middleware.Scope { return middleware.Scope{Project: "prj_1", Principal: "prin_1"} }

// The convention, stated once and asserted once: a key ending in `_ref` names a secret and every other
// key is data. THE SECOND HALF IS THE SECURITY PROPERTY — `anthropic_key`, `token`, and a nested
// `{"inner_ref": …}` all name real sealed secrets here, and none of them is asked for. A row's non-handle
// fields cannot be turned into a redemption by whoever writes the config.
func TestOnlyTopLevelRefKeysAreResolved(t *testing.T) {
	registry := &fakeRegistry{config: `{
		"team_id":"T1",
		"app_token_ref":"slack-app-T1",
		"bot_token_ref":"slack-bot-T1",
		"anthropic_key":"anthropic-key",
		"token":"anthropic-key",
		"nested":{"inner_ref":"anthropic-key"}
	}`}
	secrets := &fakeSecrets{sealed: map[string]string{
		"slack-app-T1": "xapp-live", "slack-bot-T1": "xoxb-live", "anthropic-key": "sk-ant-SECRET",
	}}
	r := New(registry, secrets)

	out, err := r.ResolveBotCredentials(context.Background(), scope(), "bot_1")
	if err != nil {
		t.Fatalf("ResolveBotCredentials: %v", err)
	}
	if want := []string{"slack-app-T1", "slack-bot-T1"}; !reflect.DeepEqual(secrets.asked, want) {
		t.Fatalf("the store was asked for %v, want exactly %v — a non-`_ref` key or a nested one must never become a redemption", secrets.asked, want)
	}
	if string(out.Values["app_token_ref"]) != "xapp-live" || string(out.Values["bot_token_ref"]) != "xoxb-live" {
		t.Fatalf("values = %v, want them keyed by CONFIG KEY and not by secret name", out.Values)
	}
	if _, present := out.Values["slack-app-T1"]; present {
		t.Fatal("a value was keyed by the SECRET NAME; the caller must never be handed a name→value mapping")
	}
	if len(out.Unresolved) != 0 {
		t.Fatalf("unresolved = %v, want none", out.Unresolved)
	}
}

// THE ORDER IS THE SECURITY PROPERTY: a bot the caller's scope cannot read reaches the secret store zero
// times. Not "gets an empty answer" — is never asked.
func TestAForeignBotNeverReachesTheSecretStore(t *testing.T) {
	registry := &fakeRegistry{missing: true}
	secrets := &fakeSecrets{sealed: map[string]string{"slack-app-T1": "xapp-live"}}
	r := New(registry, secrets)

	out, err := r.ResolveBotCredentials(context.Background(), scope(), "bot_someone_elses")
	if err != nil {
		t.Fatalf("ResolveBotCredentials: %v", err)
	}
	if !out.NotFound {
		t.Fatal("a foreign bot did not answer NotFound")
	}
	if len(secrets.asked) != 0 {
		t.Fatalf("the secret store was asked for %v on a bot the caller cannot read", secrets.asked)
	}
	if registry.scopeAsked.Project != "prj_1" {
		t.Fatalf("the registry was read in project %q, want the caller's own", registry.scopeAsked.Project)
	}
}

// A handle with nothing sealed behind it, and a handle whose value is not a usable name, both land in
// Unresolved rather than being dropped — a dropped key is indistinguishable downstream from an empty
// token. The resolved sibling still comes back, because refusing the whole answer would mean this package
// deciding which key a channel needs, which is the one thing it must not know.
func TestAnUnsealedOrUnusableHandleIsNamedRatherThanDropped(t *testing.T) {
	registry := &fakeRegistry{config: `{
		"app_token_ref":"slack-app-T1",
		"bot_token_ref":"never-sealed",
		"signing_secret_ref":"",
		"weird_ref":{"not":"a name"}
	}`}
	secrets := &fakeSecrets{sealed: map[string]string{"slack-app-T1": "xapp-live"}}
	r := New(registry, secrets)

	out, err := r.ResolveBotCredentials(context.Background(), scope(), "bot_1")
	if err != nil {
		t.Fatalf("ResolveBotCredentials: %v", err)
	}
	if string(out.Values["app_token_ref"]) != "xapp-live" {
		t.Fatalf("the resolvable handle did not come back: %v", out.Values)
	}
	want := []string{"bot_token_ref", "signing_secret_ref", "weird_ref"}
	if !reflect.DeepEqual(out.Unresolved, want) {
		t.Fatalf("unresolved = %v, want %v in key order", out.Unresolved, want)
	}
	// An empty name is never handed to the store: it would be a lookup for "" rather than a stated miss.
	for _, name := range secrets.asked {
		if name == "" {
			t.Fatal("the store was asked to resolve an empty name")
		}
	}
}

// Two identical requests answer with the same set in the same ORDER. A map range would make `unresolved`
// shuffle between calls, which turns any assertion a caller writes about it into a flake.
func TestTheAnswerIsDeterministic(t *testing.T) {
	registry := &fakeRegistry{config: `{"c_ref":"c","a_ref":"a","b_ref":"b","d_ref":"d"}`}
	r := New(registry, &fakeSecrets{})
	want := []string{"a_ref", "b_ref", "c_ref", "d_ref"}
	for i := 0; i < 8; i++ {
		out, err := r.ResolveBotCredentials(context.Background(), scope(), "bot_1")
		if err != nil {
			t.Fatalf("ResolveBotCredentials: %v", err)
		}
		if !reflect.DeepEqual(out.Unresolved, want) {
			t.Fatalf("run %d: unresolved = %v, want %v", i, out.Unresolved, want)
		}
	}
}

// A row that names no handle is an ANSWER — an empty map — and not a miss. It also touches neither the
// organization lookup nor the secret store, because there is nothing to look anything up for.
func TestARowWithNoHandlesAnswersEmpty(t *testing.T) {
	registry := &fakeRegistry{config: `{"team_id":"T1","allowed_approvers":["U1"]}`}
	secrets := &fakeSecrets{}
	r := New(registry, secrets)

	out, err := r.ResolveBotCredentials(context.Background(), scope(), "bot_1")
	if err != nil {
		t.Fatalf("ResolveBotCredentials: %v", err)
	}
	if out.NotFound {
		t.Fatal("a row with no handles answered NotFound; the bot exists and simply names nothing")
	}
	if len(out.Values) != 0 || len(out.Unresolved) != 0 {
		t.Fatalf("values=%v unresolved=%v, want both empty", out.Values, out.Unresolved)
	}
	if len(secrets.asked) != 0 {
		t.Fatalf("asked=%v, want the secret store untouched", secrets.asked)
	}
}

// A.2 Task 6 DELETED TestTheSecretStoreIsScopedByTheCallersOwnOrganization, and this note replaces it
// rather than the test being quietly dropped. It asserted that the resolver bridged the caller's project
// to an ORGANIZATION and scoped the secret store by it. Migration 000066 keys secret_refs on the
// INSTALLATION (the table carries no tenant column), so SecretStore.Resolve takes a name and nothing
// else: there is no scoping argument left to get wrong, and a test that pinned one would be asserting a
// boundary the deployment does not have. tests/security/tenancy's
// TestSecretRefNamesAreInstallationWide is where that reach is now stated, in the honest direction.

// A store failure is an error and never a partial answer: half a credential set is exactly the
// half-configured start this whole path exists to prevent.
func TestAStoreFailureIsAnErrorAndNotAPartialAnswer(t *testing.T) {
	registry := &fakeRegistry{config: `{"app_token_ref":"slack-app-T1","bot_token_ref":"slack-bot-T1"}`}
	secrets := &fakeSecrets{err: errors.New("decrypt secret ref: master key rotated")}
	r := New(registry, secrets)

	out, err := r.ResolveBotCredentials(context.Background(), scope(), "bot_1")
	if err == nil {
		t.Fatalf("a store failure answered %+v instead of erroring", out)
	}
	if len(out.Values) != 0 {
		t.Fatalf("a failed resolution still carried values: %v", out.Values)
	}
	// The error names the CONFIG KEY, which is what an operator repairs.
	if !strings.Contains(err.Error(), "app_token_ref") {
		t.Fatalf("the error does not name the config key: %v", err)
	}
}
