// Package repositories owns the infrastructure side of the repository lifecycle: the
// deterministic, untrusted-repo-hardened preparation (spec §30.3), the untrusted-repo defenses
// (§30.4), and the scoped credential broker (§30.2, §28.11). The engine never enters this package
// — it depends on DB, S3, and Git credentials, which the dependency direction keeps in the control
// plane (§24). The model never receives a raw Git credential (§30.2 line 3255): the broker mints a
// short-lived token, the preparation feeds it only to a Git credential helper, and it is revoked
// after the operation.
package repositories

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// Scope is the separately-scoped repository capability a credential is minted for. Spec §30.2:
// credentials are "scoped separately for read, push, pull request, checks, and merge". A token
// minted for one scope carries no authority for another; T3 uses ScopeRead (preparation), later
// tasks mint ScopePush / ScopePullRequest against the same seam.
type Scope string

const (
	ScopeRead        Scope = "read"
	ScopePush        Scope = "push"
	ScopePullRequest Scope = "pull_request"
	ScopeChecks      Scope = "checks"
	ScopeMerge       Scope = "merge"
)

// Audience binds a minted credential to exactly one operation (spec §28.11): organization,
// project, run, attempt fencing token, and tool call. A credential minted for one Audience is
// "unusable by another tool or destination" — the handle is opaque and single-use, revoked after
// the operation, so it cannot be replayed against a different destination.
type Audience struct {
	Organization string
	Project      string
	Run          string
	AttemptFence uint64
	ToolCall     string
}

// Credential is the OPAQUE reference every caller outside this package ever sees (spec §28.11 "the
// engine sees an opaque handle, not the token or underlying provider credential"). It carries the
// handle, username, scope, audience, and expiry — but NOT the secret. The secret lives only inside
// the broker, keyed by the handle, and reaches only a Git credential helper (§30.2). Because this
// struct has no secret field at all, no reflection, JSON marshal, log line, or fmt verb can leak
// it: absence by construction is the REP-003 exit-gate invariant, not a scrubbing convention.
type Credential struct {
	Handle    string
	Username  string
	Scope     Scope
	Audience  Audience
	ExpiresAt time.Time
}

// Broker mints and revokes short-lived, audience-bound, repository-scoped credentials (spec §30.2,
// §28.11). The secret enters only a credential helper / brokered Git operation and is revoked or
// expires after the operation. The interface is SEALED (writeHelper is unexported): only this
// package's preparation path can materialize a secret, so the raw token cannot cross the package
// boundary into the engine, a log, or a test. Implementations: LocalBroker (deterministic, for
// tests/CI and unauthenticated/local remotes) and githubAppBroker (installation token > user PAT,
// §30.2, for the live tier).
type Broker interface {
	// Mint issues a credential for one scope + audience, retaining the secret keyed by the handle.
	Mint(ctx context.Context, scope Scope, aud Audience) (Credential, error)
	// Revoke destroys the secret (and any helper file) behind a handle so no later use can redeem
	// it (spec §30.2 "revoked or expire after the operation"; TestReadCredentialRevokedAfterPreparation).
	Revoke(ctx context.Context, handle string) error
	// writeHelper materializes the handle's secret into a 0600 Git credential store under dir and
	// returns the `credential.helper` config value Git reads it through (spec §30.2 — the secret
	// enters only a credential helper). Sealed to this package: the raw token never leaves the broker
	// except into that 0600 file, which lives in the snapshot-excluded /secrets area and is removed
	// on Revoke. An expired or unknown handle fails closed.
	writeHelper(handle, cloneURL, dir string) (helperConfig string, err error)
}

// tokenTTL is how long a minted credential is valid (spec §28.11 "expires within minutes"). The
// preparation revokes explicitly after the fetch; the TTL is the backstop if a caller leaks a handle.
const tokenTTL = 5 * time.Minute

// mintedSecret is the broker-internal record behind one handle: the raw token (never exported), the
// scope/audience it is bound to, its expiry, and the helper file it was materialized into (removed
// on Revoke).
type mintedSecret struct {
	username   string
	token      string
	scope      Scope
	aud        Audience
	expiresAt  time.Time
	helperPath string
}

// credentialVault is the shared, broker-agnostic secret store both brokers embed: it retains minted
// secrets keyed by an opaque handle, materializes a secret into a 0600 Git credential-helper store on
// demand, and removes both the secret and its helper file on revoke. Extracting it keeps ONE
// implementation of the credential-materialization + removal path — the security-sensitive core (§30.2).
type credentialVault struct {
	mu      sync.Mutex
	now     func() time.Time
	secrets map[string]mintedSecret
}

func newVault() *credentialVault {
	return &credentialVault{now: time.Now, secrets: map[string]mintedSecret{}}
}

// retain stores a minted secret under handle.
func (v *credentialVault) retain(handle string, sec mintedSecret) {
	v.mu.Lock()
	v.secrets[handle] = sec
	v.mu.Unlock()
}

// writeHelper materializes handle's secret into a 0600 Git credential store under dir and returns the
// `credential.helper` config value Git reads it through (spec §30.2). It fails closed for an unknown,
// revoked, or expired handle.
func (v *credentialVault) writeHelper(handle, cloneURL, dir string) (string, error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	sec, ok := v.secrets[handle]
	if !ok {
		return "", fmt.Errorf("credential helper: unknown or revoked handle")
	}
	if !sec.expiresAt.After(v.now()) {
		return "", fmt.Errorf("credential helper: credential expired")
	}
	path, err := writeGitCredentialStore(dir, handle, sec.username, sec.token, cloneURL)
	if err != nil {
		return "", err
	}
	sec.helperPath = path
	v.secrets[handle] = sec
	return "store --file=" + path, nil
}

// revoke removes handle's secret and helper file, returning the secret so a broker can additionally
// revoke it at the provider. ok is false for an unknown or already-revoked handle (idempotent).
func (v *credentialVault) revoke(handle string) (mintedSecret, bool, error) {
	v.mu.Lock()
	sec, ok := v.secrets[handle]
	delete(v.secrets, handle)
	v.mu.Unlock()
	if !ok {
		return mintedSecret{}, false, nil
	}
	if sec.helperPath != "" {
		if err := os.Remove(sec.helperPath); err != nil && !os.IsNotExist(err) {
			return sec, true, fmt.Errorf("remove credential helper: %w", err)
		}
	}
	return sec, true, nil
}

// LocalBroker is the deterministic broker for tests/CI and unauthenticated/local remotes. It mints
// a random opaque token bound to (scope, audience) and retains it keyed by an opaque handle. It
// authenticates a local/unauthenticated remote (which never challenges) and PROVES the
// credential-absence invariant with a token that need not be real: an absence proof needs no
// provider-realness (REP-003 honest ceiling — the live tier confirms the same invariant with a real
// installation token).
type LocalBroker struct {
	*credentialVault
	fixedToken string // empty = random per mint; set only by NewLocalBrokerWithToken (deterministic fixtures)
}

// NewLocalBroker returns a ready deterministic broker that mints random opaque tokens.
func NewLocalBroker() *LocalBroker {
	return &LocalBroker{credentialVault: newVault()}
}

// NewLocalBrokerWithToken returns a deterministic broker that mints a FIXED token. It exists for
// deterministic fixtures where the exact minted secret must be known — the REP-003 absence scan
// proves a specific brokered credential is absent from every surface, and the T9 faithful Git
// double needs a stable token. It is not a production path; the live tier uses the GitHub App broker.
func NewLocalBrokerWithToken(token string) *LocalBroker {
	return &LocalBroker{credentialVault: newVault(), fixedToken: token}
}

// Mint issues a fresh opaque handle and a token bound to the scope + audience.
func (b *LocalBroker) Mint(_ context.Context, scope Scope, aud Audience) (Credential, error) {
	handle := "rcred_" + randHex(8)
	token := b.fixedToken
	if token == "" {
		token = "palai-local-" + randHex(16) // opaque; the local remote never validates it
	}
	expires := b.now().Add(tokenTTL)
	b.retain(handle, mintedSecret{username: "x-access-token", token: token, scope: scope, aud: aud, expiresAt: expires})
	return Credential{Handle: handle, Username: "x-access-token", Scope: scope, Audience: aud, ExpiresAt: expires}, nil
}

// Revoke drops the secret and removes its helper file so nothing can redeem the handle again.
func (b *LocalBroker) Revoke(_ context.Context, handle string) error {
	_, _, err := b.revoke(handle)
	return err
}

// tokenBroker hands out a credential for a token the CALLER already holds: the binding-scoped Git
// credential a repository_bindings.connection_ref resolved to through the secret-ref store (E13 T9).
// It embeds the same vault as the other brokers, so the token still reaches only a 0600 credential
// helper in the snapshot-excluded /secrets area, stays out of argv, and is removed after the operation
// (§30.2). It does NOT revoke at the provider: this token is the tenant's own durable credential, not
// one this process minted, so retiring it is the tenant's secret-ref rotation — revoking it upstream
// would break every other run that binding serves.
type tokenBroker struct {
	*credentialVault
	token string
}

// NewTokenBroker returns a broker over an already-resolved Git token. The Git basic-auth username is
// "x-access-token", the form a GitHub App installation token and a PAT are both redeemed under.
func NewTokenBroker(token string) Broker {
	return &tokenBroker{credentialVault: newVault(), token: token}
}

// Mint retains the supplied token under a fresh opaque handle, bound to the scope + audience. An empty
// token fails closed rather than producing a credential-shaped nothing that would clone anonymously.
func (b *tokenBroker) Mint(_ context.Context, scope Scope, aud Audience) (Credential, error) {
	if b.token == "" {
		return Credential{}, fmt.Errorf("token broker: no credential supplied")
	}
	handle := "rcred_" + randHex(8)
	expires := b.now().Add(tokenTTL)
	b.retain(handle, mintedSecret{username: "x-access-token", token: b.token, scope: scope, aud: aud, expiresAt: expires})
	return Credential{Handle: handle, Username: "x-access-token", Scope: scope, Audience: aud, ExpiresAt: expires}, nil
}

// Revoke drops the retained secret and removes its helper file, so nothing on the host can redeem the
// handle again. The upstream token itself outlives the operation by design (see tokenBroker).
func (b *tokenBroker) Revoke(_ context.Context, handle string) error {
	_, _, err := b.revoke(handle)
	return err
}

// writeGitCredentialStore writes one 0600 git-credentials line (`<scheme>://<user>:<token>@<host>`)
// that Git's built-in `store` helper reads. The token enters ONLY this file: the fetch remote URL
// stays clean (no token in argv, §30.2), and the file lives under the caller's /secrets area, which
// the snapshot excludes (§29.10) and Revoke removes. A non-http(s) or unparsable URL yields a
// host-less line the local `store` helper simply never matches — harmless, since local remotes do
// not authenticate.
func writeGitCredentialStore(dir, handle, username, token, cloneURL string) (string, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", fmt.Errorf("credential helper dir: %w", err)
	}
	scheme, host := "https", ""
	if u, err := url.Parse(cloneURL); err == nil && u.Host != "" {
		host = u.Host
		if u.Scheme == "http" || u.Scheme == "https" {
			scheme = u.Scheme
		}
	}
	line := fmt.Sprintf("%s://%s:%s@%s\n", scheme, url.QueryEscape(username), url.QueryEscape(token), host)
	path := filepath.Join(dir, "git-credentials-"+handle)
	// O_EXCL so a stale file from a crashed run never silently backs a new handle.
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return "", fmt.Errorf("open credential store: %w", err)
	}
	if _, err := f.WriteString(line); err != nil {
		_ = f.Close()
		return "", fmt.Errorf("write credential store: %w", err)
	}
	if err := f.Close(); err != nil {
		return "", fmt.Errorf("close credential store: %w", err)
	}
	return path, nil
}

func randHex(n int) string {
	raw := make([]byte, n)
	if _, err := rand.Read(raw); err != nil {
		// crypto/rand failure is not recoverable here; a handle must be unguessable.
		panic("repositories: crypto/rand unavailable: " + err.Error())
	}
	return hex.EncodeToString(raw)
}
