package middleware

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// A VERIFIER THAT COULD NOT LOOK IS NOT A KEY THAT IS WRONG.
//
// ‼️ EVERY ERROR ANSWERED 401 UNTIL 2026-08-07, and the cost was measured that night. The plane's
// Postgres container was stopped by a disk cleanup; a key that had worked all evening began answering
// `401 the API key is not valid`, and the hour that followed went on the key — its bytes, its trailing
// newline, the tunnel in front of it, its row in a database that was not running — because the answer
// named the credential. /healthz said `ok` throughout.
//
// The store has always drawn the line: ErrInvalidToken for a hash matching no live credential, a wrapped
// error for anything else. This middleware collapsed the two, so the one fact a caller most needs — "it
// is not you" — was the one the plane could not say.

type verifierFunc func(context.Context, string) (Scope, error)

func (f verifierFunc) VerifyAPIKey(ctx context.Context, token string) (Scope, error) {
	return f(ctx, token)
}

// probe runs one authenticated request through Auth and returns its status and body.
func probe(t *testing.T, v Verifier) (int, string) {
	t.Helper()
	handler := RequestContext(Auth(v)(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})))
	req := httptest.NewRequest(http.MethodGet, "/v1/sessions", nil)
	req.Header.Set("Authorization", "Bearer sk_whatever")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	return rec.Code, rec.Body.String()
}

func TestAnUnreachableStoreIsNotAnInvalidKey(t *testing.T) {
	code, body := probe(t, verifierFunc(func(context.Context, string) (Scope, error) {
		// What a stopped database produces: not the sentinel, just an error.
		return Scope{}, errors.New("dial tcp 127.0.0.1:5432: connect: connection refused")
	}))

	if code != http.StatusServiceUnavailable {
		t.Fatalf("a verifier that could not reach its store answered %d, want 503 — 401 tells a caller "+
			"holding a perfectly good key to go and rotate it, which is the hour this test exists to save",
			code)
	}
	// ‼️ AND THE SENTENCE MATTERS AS MUCH AS THE CODE. A 503 whose detail still said "the API key is not
	// valid" would send the reader to the same wrong place.
	if strings.Contains(body, "the API key is not valid") {
		t.Fatalf("the 503 still blames the key: %s", body)
	}
	if !strings.Contains(body, "not the key") {
		t.Fatalf("the 503 does not say whose fault it is: %s", body)
	}
}

func TestAKeyThatMatchesNothingIsStillUnauthorized(t *testing.T) {
	// THE OTHER HALF, and without it the fix would be a plane that never says 401 at all: a wrong key
	// must stay a wrong key, or an attacker probing credentials learns nothing from a 503 and a customer
	// with a revoked key is told to wait for a recovery that will not come.
	code, body := probe(t, verifierFunc(func(context.Context, string) (Scope, error) {
		return Scope{}, ErrInvalidToken
	}))
	if code != http.StatusUnauthorized {
		t.Fatalf("a key matching no live credential answered %d, want 401", code)
	}
	if !strings.Contains(body, "invalid_token") {
		t.Fatalf("the 401 does not name invalid_token: %s", body)
	}
}
