package repositories

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
)

// fakeInstallationToken is the installation token the fake GitHub returns. It is NOT shaped like a
// real ghs_ token, so the credential-literal hygiene grep never matches it, while still serving as a
// rigorous marker: it must reach only the credential-helper file, never the opaque Credential.
const fakeInstallationToken = "palai-REPMARK-installation-token-fake-must-never-leak"

// TestGitHubAppBrokerMintsScopedTokenAndRevokes proves the GitHub App broker's provider-specific logic
// against a fake GitHub API (spec §30.2): Mint signs a JWT, exchanges it for a scope-appropriate
// installation token, and hands back only an opaque handle; the token materializes solely into the
// credential-helper file; Revoke deletes it at the provider. No real network, deterministic.
func TestGitHubAppBrokerMintsScopedTokenAndRevokes(t *testing.T) {
	var gotPermissions map[string]string
	var revokedBearer string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/access_tokens"):
			if auth := r.Header.Get("Authorization"); !strings.HasPrefix(auth, "Bearer ") || strings.Count(auth, ".") != 2 {
				http.Error(w, "missing/invalid app JWT", http.StatusUnauthorized)
				return
			}
			var body struct {
				Permissions  map[string]string `json:"permissions"`
				Repositories []string          `json:"repositories"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			gotPermissions = body.Permissions
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"token": fakeInstallationToken, "expires_at": "2999-01-01T00:00:00Z",
			})
		case r.Method == http.MethodDelete && strings.HasSuffix(r.URL.Path, "/installation/token"):
			revokedBearer = strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
			w.WriteHeader(http.StatusNoContent)
		default:
			http.Error(w, "unexpected", http.StatusNotFound)
		}
	}))
	defer srv.Close()

	broker, err := NewGitHubAppBroker(GitHubAppConfig{
		AppID: "12345", InstallationID: "67890", PrivateKeyPEM: testRSAKeyPEM(t),
		Repositories: []string{"repo"}, BaseURL: srv.URL, HTTPClient: srv.Client(),
	})
	if err != nil {
		t.Fatalf("NewGitHubAppBroker() error = %v", err)
	}

	cred, err := broker.Mint(context.Background(), ScopeRead, Audience{Organization: "org_x", Run: "run_y", ToolCall: "tcall_z"})
	if err != nil {
		t.Fatalf("Mint() error = %v", err)
	}
	// The returned credential is opaque — the installation token is on no field.
	blob, _ := json.Marshal(cred)
	if strings.Contains(string(blob)+cred.Handle+cred.Username, fakeInstallationToken) {
		t.Fatal("Mint() leaked the installation token into the opaque credential")
	}
	if gotPermissions["contents"] != "read" {
		t.Fatalf("read scope requested permissions %v, want contents:read", gotPermissions)
	}

	// The token materializes only into the 0600 helper file.
	dir := t.TempDir()
	helperCfg, err := broker.writeHelper(cred.Handle, "https://github.com/org/repo", dir)
	if err != nil {
		t.Fatalf("writeHelper() error = %v", err)
	}
	body, _ := os.ReadFile(strings.TrimPrefix(helperCfg, "store --file="))
	if !strings.Contains(string(body), fakeInstallationToken) {
		t.Fatal("the helper file is the ONE place the token may live; it is absent")
	}

	if err := broker.Revoke(context.Background(), cred.Handle); err != nil {
		t.Fatalf("Revoke() error = %v", err)
	}
	if revokedBearer != fakeInstallationToken {
		t.Fatalf("Revoke() deleted with bearer %q, want the installation token", revokedBearer)
	}
}

// TestPermissionsForScopeSeparation proves each scope maps to its minimal, distinct permission set
// (spec §30.2 separately-scoped credentials).
func TestPermissionsForScopeSeparation(t *testing.T) {
	cases := map[Scope]map[string]string{
		ScopeRead:        {"contents": "read"},
		ScopePush:        {"contents": "write"},
		ScopePullRequest: {"pull_requests": "write", "contents": "read"},
		ScopeChecks:      {"checks": "write"},
	}
	for scope, want := range cases {
		got := permissionsForScope(scope)
		if len(got) != len(want) {
			t.Errorf("permissionsForScope(%s) = %v, want %v", scope, got, want)
			continue
		}
		for k, v := range want {
			if got[k] != v {
				t.Errorf("permissionsForScope(%s)[%s] = %q, want %q", scope, k, got[k], v)
			}
		}
	}
}

func testRSAKeyPEM(t *testing.T) []byte {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate test key: %v", err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)})
}
