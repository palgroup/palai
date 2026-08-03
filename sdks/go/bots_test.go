package palai

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"testing"
)

func TestBotsGetRequestsTheEscapedPath(t *testing.T) {
	var gotPath, gotMethod string
	rt := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		gotPath = r.URL.EscapedPath()
		gotMethod = r.Method
		return jsonResp(200, `{"id":"bot_1","object":"bot","name":"ios-bot","kind":"slack",`+
			`"agent_revision_id":"rev_1","repository_binding_id":"rb_1","principal_id":"prin_1",`+
			`"config":{"team_id":"T1"},"disabled":false,"created_at":"2026-08-03T00:00:00Z"}`), nil
	})
	c := testClient(t, rt)

	bot, err := c.Bots.Get(context.Background(), "bot/needs-escape")
	if err != nil {
		t.Fatalf("Bots.Get: %v", err)
	}
	if gotMethod != http.MethodGet {
		t.Fatalf("method = %q, want GET", gotMethod)
	}
	if gotPath != "/v1/bots/bot%2Fneeds-escape" {
		t.Fatalf("path = %q, want the id URL-escaped as one segment", gotPath)
	}
	if bot.ID != "bot_1" || bot.Name != "ios-bot" || bot.Kind != "slack" || bot.Disabled {
		t.Fatalf("bot = %+v", bot)
	}
	if bot.AgentRevisionID != "rev_1" || bot.RepositoryBindingID != "rb_1" || bot.PrincipalID != "prin_1" {
		t.Fatalf("bot identity fields wrong: %+v", bot)
	}
	var cfg map[string]any
	if err := json.Unmarshal(bot.Config, &cfg); err != nil || cfg["team_id"] != "T1" {
		t.Fatalf("config not carried opaquely: %s", bot.Config)
	}
}

// TestBotsGetNotFoundIsTypedAPIError locks the same 404 mapping every other Get on this SDK gets
// (GetConnection, GetRoute, …): an absent or foreign id is a typed *APIError, not a nil bot.
func TestBotsGetNotFoundIsTypedAPIError(t *testing.T) {
	rt := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		return jsonResp(404, problemBody(404, "not_found")), nil
	})
	c := testClient(t, rt)

	_, err := c.Bots.Get(context.Background(), "bot_missing")
	var apiErr *APIError
	if !errors.As(err, &apiErr) || apiErr.Status != 404 {
		t.Fatalf("want *APIError 404, got %v", err)
	}
}

// TestBotsGetIsRetriedOnNetworkFailure: a GET is a safe method, so a torn connection is retried
// within budget rather than surfacing on the first attempt (isSafeMethod, doJSON).
func TestBotsGetIsRetriedOnNetworkFailure(t *testing.T) {
	attempts := 0
	rt := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		attempts++
		if attempts == 1 {
			return nil, errors.New("network unreachable")
		}
		return jsonResp(200, `{"id":"bot_1","object":"bot","name":"ios-bot","kind":"slack",`+
			`"agent_revision_id":"","repository_binding_id":"","principal_id":"",`+
			`"config":{},"disabled":false,"created_at":"2026-08-03T00:00:00Z"}`), nil
	})
	c := testClient(t, rt)

	bot, err := c.Bots.Get(context.Background(), "bot_1")
	if err != nil {
		t.Fatalf("Bots.Get: %v", err)
	}
	if bot.ID != "bot_1" {
		t.Fatalf("bot = %+v", bot)
	}
	if attempts != 2 {
		t.Fatalf("one failure then success is two attempts, got %d", attempts)
	}
}

// TestBotsCredentialsRequestsTheEscapedSubpath: the redemption route hangs off the escaped bot id, and
// the values come back keyed by the CONFIG KEY the caller's own row carries.
func TestBotsCredentialsRequestsTheEscapedSubpath(t *testing.T) {
	var gotPath, gotMethod string
	rt := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		gotPath, gotMethod = r.URL.EscapedPath(), r.Method
		return jsonResp(200, `{"bot_id":"bot_1","object":"bot_credentials",`+
			`"credentials":{"app_token_ref":"xapp-live","bot_token_ref":"xoxb-live"},"unresolved":[]}`), nil
	})
	c := testClient(t, rt)

	creds, err := c.Bots.Credentials(context.Background(), "bot/needs-escape")
	if err != nil {
		t.Fatalf("Bots.Credentials: %v", err)
	}
	if gotMethod != http.MethodGet {
		t.Fatalf("method = %q, want GET", gotMethod)
	}
	if gotPath != "/v1/bots/bot%2Fneeds-escape/credentials" {
		t.Fatalf("path = %q, want the id escaped as ONE segment with /credentials after it", gotPath)
	}
	if creds.Credentials["app_token_ref"] != "xapp-live" || creds.Credentials["bot_token_ref"] != "xoxb-live" {
		t.Fatalf("credentials = %v", creds.Credentials)
	}
}

// TestBotsCredentialsCarriesUnresolvedThrough: the field that makes a missing credential loud must
// survive the decode, or a caller checking it silently checks nothing.
func TestBotsCredentialsCarriesUnresolvedThrough(t *testing.T) {
	rt := roundTripFunc(func(*http.Request) (*http.Response, error) {
		return jsonResp(200, `{"bot_id":"bot_1","object":"bot_credentials",`+
			`"credentials":{"app_token_ref":"xapp-live"},"unresolved":["bot_token_ref"]}`), nil
	})
	c := testClient(t, rt)

	creds, err := c.Bots.Credentials(context.Background(), "bot_1")
	if err != nil {
		t.Fatalf("Bots.Credentials: %v", err)
	}
	if len(creds.Unresolved) != 1 || creds.Unresolved[0] != "bot_token_ref" {
		t.Fatalf("unresolved = %v, want [bot_token_ref]", creds.Unresolved)
	}
	if _, present := creds.Credentials["bot_token_ref"]; present {
		t.Fatalf("an unresolved key was carried as a value: %v", creds.Credentials)
	}
}

// TestBotsCredentialsForbiddenIsTypedAPIError: a key without the capability must arrive as a 403
// *APIError a caller can branch on, not as an opaque decode failure.
func TestBotsCredentialsForbiddenIsTypedAPIError(t *testing.T) {
	rt := roundTripFunc(func(*http.Request) (*http.Response, error) {
		return jsonResp(403, `{"type":"insufficient_scope","title":"Forbidden","status":403,`+
			`"detail":"this API key lacks the bots.credentials capability"}`), nil
	})
	c := testClient(t, rt)

	_, err := c.Bots.Credentials(context.Background(), "bot_1")
	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("err = %v (%T), want *APIError", err, err)
	}
	if apiErr.Status != 403 {
		t.Fatalf("status = %d, want 403", apiErr.Status)
	}
}
