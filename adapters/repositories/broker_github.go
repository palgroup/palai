package repositories

import (
	"bytes"
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// GitHubAppConfig configures the GitHub App credential broker. Spec §30.2 prefers a GitHub App
// installation token over a user PAT. The App private key arrives via the LP-0 file-secret bridge and
// is sealed at rest by E13 (§7 deferral); this broker only MINTS short-lived installation tokens
// against it — it never persists the key beyond the process.
type GitHubAppConfig struct {
	AppID          string       // the GitHub App id (or client id) — the JWT `iss`
	InstallationID string       // the installation to mint tokens for
	PrivateKeyPEM  []byte       // the App's RSA private key (PEM); never logged, never stored beyond the process
	Repositories   []string     // scope the token to these repository names (empty = all the installation can access)
	BaseURL        string       // https://api.github.com by default; overridable for GHE / a live-test double
	HTTPClient     *http.Client // nil defaults to a 30s-timeout client
}

// githubAppBroker mints repository-scoped GitHub App installation tokens (spec §30.2, §28.11). Like
// the local broker it hands back only an opaque handle; the installation token enters only the Git
// credential helper (the shared vault) and is revoked at GitHub after the operation.
type githubAppBroker struct {
	*credentialVault
	cfg GitHubAppConfig
	key *rsa.PrivateKey
}

// NewGitHubAppBroker parses the App private key and returns a ready broker. It performs no network
// call; installation tokens are minted lazily on Mint.
func NewGitHubAppBroker(cfg GitHubAppConfig) (Broker, error) {
	key, err := parseRSAPrivateKey(cfg.PrivateKeyPEM)
	if err != nil {
		return nil, fmt.Errorf("github app broker: %w", err)
	}
	if cfg.AppID == "" || cfg.InstallationID == "" {
		return nil, fmt.Errorf("github app broker: app id and installation id are required")
	}
	if cfg.BaseURL == "" {
		cfg.BaseURL = "https://api.github.com"
	}
	if cfg.HTTPClient == nil {
		cfg.HTTPClient = &http.Client{Timeout: 30 * time.Second}
	}
	return &githubAppBroker{credentialVault: newVault(), cfg: cfg, key: key}, nil
}

// Mint exchanges a freshly-signed App JWT for a repository-scoped installation token whose permissions
// match the scope. The token is retained opaquely by handle; the caller never sees it (§30.2).
func (b *githubAppBroker) Mint(ctx context.Context, scope Scope, aud Audience) (Credential, error) {
	token, expires, err := mintGitHubInstallationToken(ctx, b.cfg, b.key, b.now(), scope)
	if err != nil {
		return Credential{}, err
	}
	handle := "rcred_" + randHex(8)
	b.retain(handle, mintedSecret{username: "x-access-token", token: token, scope: scope, aud: aud, expiresAt: expires})
	return Credential{Handle: handle, Username: "x-access-token", Scope: scope, Audience: aud, ExpiresAt: expires}, nil
}

// Revoke removes the local secret + helper file AND revokes the installation token at GitHub, so a
// leaked token cannot be used after the operation (spec §30.2).
func (b *githubAppBroker) Revoke(ctx context.Context, handle string) error {
	sec, ok, err := b.revoke(handle)
	if err != nil {
		return err
	}
	if !ok {
		return nil
	}
	return b.revokeInstallationToken(ctx, sec.token)
}

// githubAppJWT builds and RS256-signs the short-lived (≤10 min) App JWT the token endpoint requires.
// iat is backdated 60s for clock skew; the token itself never leaves this process. A free function so
// both the App broker and the pull-request client (publish.go) sign against the same key without one
// depending on the other.
func githubAppJWT(now time.Time, appID string, key *rsa.PrivateKey) (string, error) {
	claims, err := json.Marshal(map[string]any{
		"iat": now.Add(-60 * time.Second).Unix(),
		"exp": now.Add(9 * time.Minute).Unix(),
		"iss": appID,
	})
	if err != nil {
		return "", fmt.Errorf("encode app jwt claims: %w", err)
	}
	signingInput := b64(`{"alg":"RS256","typ":"JWT"}`) + "." + b64(string(claims))
	sum := sha256.Sum256([]byte(signingInput))
	sig, err := rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, sum[:])
	if err != nil {
		return "", fmt.Errorf("sign app jwt: %w", err)
	}
	return signingInput + "." + base64.RawURLEncoding.EncodeToString(sig), nil
}

// mintGitHubInstallationToken signs an App JWT and POSTs it to the installation access-token endpoint,
// scoping the token to cfg.Repositories and the scope's permissions (spec §30.2 separately-scoped
// credentials). Shared by the App broker (Git credential path) and the pull-request client (API path),
// so one minting implementation serves both. The token is returned to the immediate caller, which keeps
// it inside the broker vault or a one-shot Authorization header — never a log or the model context.
func mintGitHubInstallationToken(ctx context.Context, cfg GitHubAppConfig, key *rsa.PrivateKey, now time.Time, scope Scope) (string, time.Time, error) {
	jwt, err := githubAppJWT(now, cfg.AppID, key)
	if err != nil {
		return "", time.Time{}, err
	}
	body := map[string]any{"permissions": permissionsForScope(scope)}
	if len(cfg.Repositories) > 0 {
		body["repositories"] = cfg.Repositories
	}
	payload, err := json.Marshal(body)
	if err != nil {
		return "", time.Time{}, fmt.Errorf("encode token request: %w", err)
	}
	endpoint := strings.TrimRight(cfg.BaseURL, "/") + "/app/installations/" + cfg.InstallationID + "/access_tokens"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(payload))
	if err != nil {
		return "", time.Time{}, fmt.Errorf("build token request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+jwt)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	resp, err := cfg.HTTPClient.Do(req)
	if err != nil {
		return "", time.Time{}, fmt.Errorf("create installation token: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		// A non-201 body is a GitHub error, never a token — safe to surface for diagnosis.
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return "", time.Time{}, fmt.Errorf("create installation token: status %d: %s", resp.StatusCode, strings.TrimSpace(string(msg)))
	}
	var out struct {
		Token     string    `json:"token"`
		ExpiresAt time.Time `json:"expires_at"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return "", time.Time{}, fmt.Errorf("decode installation token: %w", err)
	}
	if out.Token == "" {
		return "", time.Time{}, fmt.Errorf("installation token response carried no token")
	}
	if out.ExpiresAt.IsZero() {
		out.ExpiresAt = now.Add(time.Hour) // installation tokens expire in ~1h
	}
	return out.Token, out.ExpiresAt, nil
}

// revokeInstallationToken DELETEs the installation token so it cannot be used after the operation
// (spec §30.2). It authenticates with the token being revoked, per the GitHub API.
func (b *githubAppBroker) revokeInstallationToken(ctx context.Context, token string) error {
	endpoint := strings.TrimRight(b.cfg.BaseURL, "/") + "/installation/token"
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, endpoint, nil)
	if err != nil {
		return fmt.Errorf("build revoke request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/vnd.github+json")
	resp, err := b.cfg.HTTPClient.Do(req)
	if err != nil {
		return fmt.Errorf("revoke installation token: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		return fmt.Errorf("revoke installation token: status %d", resp.StatusCode)
	}
	return nil
}

// permissionsForScope maps a repository Scope to the minimal GitHub App permission set (spec §30.2 —
// scoped separately for read, push, pull request, checks, and merge).
func permissionsForScope(scope Scope) map[string]string {
	switch scope {
	case ScopePush, ScopeMerge:
		return map[string]string{"contents": "write"}
	case ScopePullRequest:
		return map[string]string{"pull_requests": "write", "contents": "read"}
	case ScopeChecks:
		return map[string]string{"checks": "write"}
	default: // ScopeRead
		return map[string]string{"contents": "read"}
	}
}

// parseRSAPrivateKey accepts a PKCS#1 or PKCS#8 PEM-encoded RSA key (the two forms GitHub emits).
func parseRSAPrivateKey(pemBytes []byte) (*rsa.PrivateKey, error) {
	block, _ := pem.Decode(pemBytes)
	if block == nil {
		return nil, fmt.Errorf("no PEM block in private key")
	}
	if key, err := x509.ParsePKCS1PrivateKey(block.Bytes); err == nil {
		return key, nil
	}
	keyAny, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("parse private key: %w", err)
	}
	key, ok := keyAny.(*rsa.PrivateKey)
	if !ok {
		return nil, fmt.Errorf("private key is not RSA")
	}
	return key, nil
}

func b64(s string) string { return base64.RawURLEncoding.EncodeToString([]byte(s)) }
