package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/palgroup/palai/apps/control-plane/api/middleware"
)

// fakeBotCredentials scripts the resolver seam and records what reached it. `askedFor` is the assertion
// several tests below rest on: it proves a request that must not reach the secret store did not.
type fakeBotCredentials struct {
	askedFor     []string
	askedProject string
	result       BotCredentialsResult
	err          error
}

func (f *fakeBotCredentials) ResolveBotCredentials(_ context.Context, scope middleware.Scope, botID string) (BotCredentialsResult, error) {
	f.askedFor = append(f.askedFor, botID)
	f.askedProject = scope.Project
	return f.result, f.err
}

func credentialsServer(t *testing.T, verifier middleware.Verifier, c BotCredentials) string {
	t.Helper()
	srv := httptest.NewServer(NewRouter(verifier, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, SSEConfig{}, nil, nil,
		WithBotCredentials(c)))
	t.Cleanup(srv.Close)
	return srv.URL
}

// The route's whole job: the caller names a BOT and gets values back keyed by the CONFIG KEY its own row
// carries. The `unresolved` field is rendered even when empty, so a caller checking it never reads an
// absent field, and the response is marked no-store because this body carries live tokens.
func TestBotCredentialsRendersValuesByConfigKey(t *testing.T) {
	fake := &fakeBotCredentials{result: BotCredentialsResult{
		Values: map[string][]byte{"app_token_ref": []byte("xapp-live"), "bot_token_ref": []byte("xoxb-live")},
	}}
	base := credentialsServer(t, fakeVerifier{}, fake)

	resp := do(t, "GET", base+"/v1/bots/bot_1/credentials", "", nil)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", resp.StatusCode, readBody(t, resp))
	}
	if cc := resp.Header.Get("Cache-Control"); cc != "no-store" {
		t.Fatalf("Cache-Control = %q, want no-store — this body carries live tokens", cc)
	}
	var got struct {
		BotID       string            `json:"bot_id"`
		Object      string            `json:"object"`
		Credentials map[string]string `json:"credentials"`
		Unresolved  *[]string         `json:"unresolved"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.BotID != "bot_1" || got.Object != "bot_credentials" {
		t.Fatalf("projection = %+v", got)
	}
	if got.Credentials["app_token_ref"] != "xapp-live" || got.Credentials["bot_token_ref"] != "xoxb-live" {
		t.Fatalf("credentials = %v, want the values keyed by config key", got.Credentials)
	}
	if got.Unresolved == nil {
		t.Fatal("`unresolved` was absent; it must render as [] so a caller that checks it never reads a missing field")
	}
	if fake.askedProject != "prj_1" {
		t.Fatalf("resolved in project %q, want the VERIFIED scope's prj_1", fake.askedProject)
	}
}

// THE SECURITY PROPERTY, ASSERTED STRUCTURALLY: the only thing a caller can name is a bot. Every place a
// secret name could ride in — a query field, a body, a header — is offered here, and the resolver still
// receives nothing but the path's bot id. This is the assertion that would fail if the handler ever grew
// a parameter, which is the single change that would turn this route into an arbitrary secret reader.
func TestBotCredentialsTakesNothingFromTheCallerButTheBotID(t *testing.T) {
	fake := &fakeBotCredentials{result: BotCredentialsResult{Values: map[string][]byte{}}}
	base := credentialsServer(t, fakeVerifier{}, fake)

	req, err := http.NewRequest("GET",
		base+"/v1/bots/bot_1/credentials?name=slack-app-T1&secret=anthropic-key&ref=slack-bot-T1",
		strings.NewReader(`{"name":"anthropic-key"}`))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer sk-test")
	req.Header.Set("X-Palai-Secret-Name", "anthropic-key")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", resp.StatusCode, readBody(t, resp))
	}
	if len(fake.askedFor) != 1 || fake.askedFor[0] != "bot_1" {
		t.Fatalf("the resolver was asked for %v, want exactly [bot_1] — no caller-supplied name may reach it", fake.askedFor)
	}
}

// A bot in another tenant and a bot that never existed answer identically, byte for byte with the
// registry's own 404, so this route discloses no cross-tenant existence that GET /v1/bots/{bot_id} does
// not already withhold.
func TestBotCredentialsForAForeignBotIs404AndMatchesTheRegistry(t *testing.T) {
	fake := &fakeBotCredentials{result: BotCredentialsResult{NotFound: true}}
	base := credentialsServer(t, fakeVerifier{}, fake)
	resp := do(t, "GET", base+"/v1/bots/bot_someone_elses/credentials", "", nil)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", resp.StatusCode)
	}
	credentialsBody := problemWithoutRequestID(t, readBody(t, resp))

	// The registry's own miss, through the same router construction.
	registry := &fakeBotRegistry{missing: true}
	registryBase := botTestServer(t, registry)
	registryResp := do(t, "GET", registryBase+"/v1/bots/bot_someone_elses", "", nil)
	defer registryResp.Body.Close()
	if got := problemWithoutRequestID(t, readBody(t, registryResp)); got != credentialsBody {
		t.Fatalf("the credentials 404 body is\n  %s\nbut the registry's is\n  %s\n— an operator could tell the two routes apart on a foreign id", credentialsBody, got)
	}
}

// problemWithoutRequestID re-renders a problem body with `request_id` dropped: it differs on every
// request by design, and it is the only field that may differ between two answers that must otherwise be
// indistinguishable.
func problemWithoutRequestID(t *testing.T, body string) string {
	t.Helper()
	var fields map[string]any
	if err := json.Unmarshal([]byte(body), &fields); err != nil {
		t.Fatalf("decode problem %q: %v", body, err)
	}
	delete(fields, "request_id")
	out, err := json.Marshal(fields)
	if err != nil {
		t.Fatal(err)
	}
	return string(out)
}

// A handle with nothing sealed behind it is named in `unresolved` and absent from `credentials`, rather
// than rendered as an empty string. An operator can act on the first; the second is indistinguishable
// from a token that happens to be empty.
func TestAnUnsealedHandleIsNamedRatherThanRenderedEmpty(t *testing.T) {
	fake := &fakeBotCredentials{result: BotCredentialsResult{
		Values:     map[string][]byte{"app_token_ref": []byte("xapp-live")},
		Unresolved: []string{"bot_token_ref"},
	}}
	base := credentialsServer(t, fakeVerifier{}, fake)
	resp := do(t, "GET", base+"/v1/bots/bot_1/credentials", "", nil)
	defer resp.Body.Close()

	var got struct {
		Credentials map[string]string `json:"credentials"`
		Unresolved  []string          `json:"unresolved"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if _, present := got.Credentials["bot_token_ref"]; present {
		t.Fatalf("an unsealed handle was rendered in credentials: %v", got.Credentials)
	}
	if len(got.Unresolved) != 1 || got.Unresolved[0] != "bot_token_ref" {
		t.Fatalf("unresolved = %v, want [bot_token_ref]", got.Unresolved)
	}
}

// The capability is a NEW one and not `provision`, and this is the test that pins the difference: a key
// carrying `provision` and nothing else — a tenancy-administration key — is REFUSED, and a key carrying
// only `bots.credentials` is admitted. If the gate were ever changed to provisionScope, the first half
// fails.
func TestBotCredentialsGatesOnItsOwnCapabilityAndNotProvision(t *testing.T) {
	provisioner := scopedVerifier{middleware.Scope{Project: "prj_1", Scopes: []string{provisionScope}}}
	fake := &fakeBotCredentials{result: BotCredentialsResult{Values: map[string][]byte{}}}
	base := credentialsServer(t, provisioner, fake)
	resp := do(t, "GET", base+"/v1/bots/bot_1/credentials", "", nil)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("a provision-only key got %d, want 403 — a bot's key must not have to be a provisioning key", resp.StatusCode)
	}
	if len(fake.askedFor) != 0 {
		t.Fatalf("the resolver ran for an unauthorized key: %v", fake.askedFor)
	}
	if body := readBody(t, resp); !strings.Contains(body, botCredentialsScope) {
		t.Fatalf("the refusal does not name the capability that is missing: %s", body)
	}

	narrow := scopedVerifier{middleware.Scope{Project: "prj_1", Scopes: []string{botCredentialsScope}}}
	narrowBase := credentialsServer(t, narrow, &fakeBotCredentials{result: BotCredentialsResult{Values: map[string][]byte{}}})
	narrowResp := do(t, "GET", narrowBase+"/v1/bots/bot_1/credentials", "", nil)
	defer narrowResp.Body.Close()
	if narrowResp.StatusCode != http.StatusOK {
		t.Fatalf("a %s key got %d, want 200", botCredentialsScope, narrowResp.StatusCode)
	}
}

// A resolver failure renders NOTHING. The store tags a decrypt failure with the secret's name, and an
// error body is the one place a caller's bytes get reflected back — so this asserts neither the name nor
// the value reaches the wire.
func TestAResolverFailureLeaksNeitherNameNorValue(t *testing.T) {
	fake := &fakeBotCredentials{err: errors.New(`decrypt secret ref "slack-bot-T1": xoxb-leaked`)}
	base := credentialsServer(t, fakeVerifier{}, fake)
	resp := do(t, "GET", base+"/v1/bots/bot_1/credentials", "", nil)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", resp.StatusCode)
	}
	body := readBody(t, resp)
	for _, forbidden := range []string{"slack-bot-T1", "xoxb-leaked", "decrypt"} {
		if strings.Contains(body, forbidden) {
			t.Fatalf("the 500 body carries %q: %s", forbidden, body)
		}
	}
}

// A stack that wires no resolver — one with no master key, which has nothing to open secrets with —
// leaves the route unmounted rather than answering with an empty document.
func TestTheCredentialsRouteIsUnmountedWithoutTheOption(t *testing.T) {
	base := botTestServer(t, &fakeBotRegistry{})
	resp := do(t, "GET", base+"/v1/bots/bot_1/credentials", "", nil)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d with no WithBotCredentials, want 404", resp.StatusCode)
	}
}
