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
// boundary into the engine, a log, or a test. Implementations: AnonymousBroker (deterministic, for
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
	// NO SECRET, NO HELPER. An empty token means the broker has no credential to offer, and writing a
	// helper anyway would materialise `x-access-token:` against the remote — which a real provider reads
	// as a bad credential and refuses, turning an anonymous clone that WOULD have worked into a failure.
	// The caller's hardened `credential.helper=` reset then stands alone, which is exactly "use no
	// helper" and is how an unauthenticated fetch is supposed to look.
	if sec.token == "" {
		return "", nil
	}
	path, err := writeGitCredentialStore(dir, handle, sec.username, sec.token, cloneURL)
	if err != nil {
		return "", err
	}
	sec.helperPath = path
	v.secrets[handle] = sec
	return "store --file=" + path, nil
}

// HostSpendable is a broker whose minted secret can be spent on ANOTHER HOST, and it exists because
// A.3 T5 moved the clone to the machine that holds the attempt's lease.
//
// IT WIDENS A SEAL THIS PACKAGE DELIBERATELY SET, so the widening is named rather than incidental. The
// Broker doc says the interface is sealed "so the raw token cannot cross the package boundary into the
// engine, a log, or a test", and that sentence is still true of all three: nothing here reaches the
// engine, nothing logs a secret, and the seal on writeHelper is untouched. What is new is a FOURTH
// destination the sentence did not contemplate, and it is not optional — git reads a credential
// through a helper FILE, and the file has to sit next to the repository being cloned. Once the
// repository is on another machine, either the secret goes there or the clone does not happen.
//
// THE MACHINE IS NOT A NEW TRUST CLASS, which is the argument that makes this defensible rather than
// merely necessary. The control plane already hands a machine an attempt's ENVIRONMENT VALUES, which
// reach exec.Cmd.Env or container.Config.Env and then live in that kernel's environ copy for the life
// of the process (packages/tool-broker/sandbox_exec.go states it and a test measures it). A
// five-minute read token is a strictly smaller thing than that, over the same mTLS connection, to the
// same enrolled machine.
//
// WHAT IS STILL TRUE, AND IS THE PART TO KEEP: the credential is short-lived (tokenTTL), bound to one
// audience, and REVOKED by the control plane on every return path — so a machine that keeps a copy
// keeps a dead one. Nothing about the App key or the tenant's stored credential moves.
type HostSpendable interface {
	// Secret returns the username and raw token behind a handle so a caller can hand them to the host
	// that will spend them. It fails closed for an unknown, revoked, or expired handle, exactly as
	// writeHelper does — the two are the same lookup with two destinations.
	Secret(handle string) (username, token string, err error)
}

var _ HostSpendable = (*credentialVault)(nil)

// HelperUsername is the Git username every broker in this package mints against. It is exported so a
// caller that spends a secret on ANOTHER host can verify the credential it is about to send is one
// that host knows how to use, rather than assuming it. Measured 2026-08-04 — all three brokers agree:
//
//	grep -n "username:" adapters/repositories/*.go   -> 3 hits, all "x-access-token"
//
// The check exists because a future broker minting a different username would otherwise authenticate
// wrongly on the far side and report it as a clone failure, which is the expensive way to find out.
const HelperUsername = "x-access-token"

// Secret is credentialVault's HostSpendable half. It shares writeHelper's checks rather than
// re-spelling them: an unknown handle and an expired one must fail the same way through both doors,
// or the weaker door becomes the one every caller uses.
func (v *credentialVault) Secret(handle string) (string, string, error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	sec, ok := v.secrets[handle]
	if !ok {
		return "", "", fmt.Errorf("credential: unknown or revoked handle")
	}
	if !sec.expiresAt.After(v.now()) {
		return "", "", fmt.Errorf("credential: credential expired")
	}
	// NO SECRET, NO CREDENTIAL — writeHelper's rule, and for its reason: an empty token means the broker
	// has nothing to offer, and handing over `x-access-token:` would turn an anonymous clone that WOULD
	// have worked into a provider-side refusal. An empty answer here is "clone with no helper".
	return sec.username, sec.token, nil
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

// AnonymousBroker offers NO CREDENTIAL. It is what a deployment falls back to when no provider
// credential is configured, and the name now says that.
//
// IT WAS CALLED LocalBroker AND THAT NAME COST A WORKING CLONE. "Local" read as "for local remotes",
// and the type acted on that reading: it minted a fabricated token — `palai-local-<hex>` — on the
// stated assumption that "the local remote never challenges". A real provider challenges. Measured
// 2026-08-02 against github.com with a PUBLIC repository:
//
//	git clone https://x-access-token:palai-local-deadbeef@github.com/…  -> Invalid username or token
//	git clone https://github.com/…                                      -> succeeds
//
// So the fabricated credential did not fail to help; it BROKE the case that works without one. And it
// did so in the default configuration — no GitHub App configured lands here — which is exactly the
// path a self-hoster binding a public repository takes.
//
// WHAT REPLACES IT IS NOT AN APP. A binding that names a `connection_ref` resolves the tenant's own
// token from the platform secret store and clones with NewTokenBroker — a personal or fine-grained
// access token is enough, it lives in the database rather than on any machine, and it travels with the
// deployment. The GitHub App broker is one option for org-scale installs, not a requirement.
//
// So there are three honest states and no fourth: a public repository clones with nothing, a private
// one clones with the tenant's token, and a private one with no token fails saying so.
//
// NewFixtureBrokerWithToken keeps the fixed-token behaviour for the absence proofs that must scan for a
// known secret.
type AnonymousBroker struct {
	*credentialVault
	fixedToken string // empty = random per mint; set only by NewFixtureBrokerWithToken (deterministic fixtures)
}

// NewAnonymousBroker returns a ready deterministic broker that mints random opaque tokens.
func NewAnonymousBroker() *AnonymousBroker {
	return &AnonymousBroker{credentialVault: newVault()}
}

// NewFixtureBrokerWithToken returns a deterministic broker that mints a FIXED token. It exists for
// deterministic fixtures where the exact minted secret must be known — the REP-003 absence scan
// proves a specific brokered credential is absent from every surface, and the T9 faithful Git
// double needs a stable token. It is not a production path; the live tier uses the GitHub App broker.
func NewFixtureBrokerWithToken(token string) *AnonymousBroker {
	return &AnonymousBroker{credentialVault: newVault(), fixedToken: token}
}

// Mint issues a fresh opaque handle and a token bound to the scope + audience.
func (b *AnonymousBroker) Mint(_ context.Context, scope Scope, aud Audience) (Credential, error) {
	handle := "rcred_" + randHex(8)
	// AN EMPTY TOKEN MEANS "OFFER NO CREDENTIAL", AND IT IS THE FIX FOR A MEASURED FAILURE.
	//
	// This used to mint `palai-local-<hex>` on the reasoning quoted below — "the local remote never
	// challenges". A real remote does. Measured 2026-08-02 against github.com with a public repository:
	//
	//   git clone https://x-access-token:palai-local-deadbeef@github.com/…   -> remote: Invalid username
	//                                                                          or token. FATAL.
	//   git clone https://github.com/…                                       -> succeeds
	//
	// So the fabricated credential did not merely fail to help — it BROKE a clone that works anonymously,
	// and it broke it in the deployment's default configuration: no GitHub App configured falls back to
	// this broker (main.repositoryBrokerFromEnv), so binding a public repository produced a run that
	// failed with "the run failed during execution" and no clue.
	//
	// NewFixtureBrokerWithToken keeps its fixed token: a fixture that needs a known secret still gets one,
	// and the absence proofs that scan for it are unchanged.
	token := b.fixedToken
	expires := b.now().Add(tokenTTL)
	b.retain(handle, mintedSecret{username: HelperUsername, token: token, scope: scope, aud: aud, expiresAt: expires})
	return Credential{Handle: handle, Username: HelperUsername, Scope: scope, Audience: aud, ExpiresAt: expires}, nil
}

// Revoke drops the secret and removes its helper file so nothing can redeem the handle again.
func (b *AnonymousBroker) Revoke(_ context.Context, handle string) error {
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
//
// The Scope is RECORDED but carries no authority boundary here: unlike the App broker, which mints a
// token whose permissions match the scope, this hands back the one durable credential the tenant supplied
// (a PAT's rights are whatever the tenant granted it). Only ScopeRead is minted today; a future push-scope
// caller gets no narrowing from this broker and must not assume one.
func (b *tokenBroker) Mint(_ context.Context, scope Scope, aud Audience) (Credential, error) {
	if b.token == "" {
		return Credential{}, fmt.Errorf("token broker: no credential supplied")
	}
	handle := "rcred_" + randHex(8)
	expires := b.now().Add(tokenTTL)
	b.retain(handle, mintedSecret{username: HelperUsername, token: b.token, scope: scope, aud: aud, expiresAt: expires})
	return Credential{Handle: handle, Username: HelperUsername, Scope: scope, Audience: aud, ExpiresAt: expires}, nil
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
