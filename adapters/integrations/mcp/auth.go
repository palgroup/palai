package mcp

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strings"

	"github.com/palgroup/palai/packages/egress"
)

// This file is the MCP connection-authorization surface (spec §28.14, E12 Task 6, TOL-009/TOL-010). It owns
// the confused-deputy defences the HTTP transport and the create/discover admin paths enforce:
//
//   - token passthrough is FORBIDDEN — the ONLY Authorization the transport sends is the connection's OWN
//     resolved bearer, never the platform's token (proven by TestUpstreamTokenNeverForwardedToMCPServer;
//     the transport structurally has no platform token to leak — this file keeps it that way);
//   - a resolved bearer is bound to the connection's registered resource origin (its audience). A dial whose
//     origin differs from the declared audience is denied, so connection A's token can never be replayed to
//     server B (a static-token audience model — the HONEST CEILING: we do NOT parse/introspect the token's
//     `aud` claim, which is OAuth Resource Indicators / RFC8707 territory, deferred to E17's interactive flow);
//   - the HTTP POST carries an Origin header derived from the registered URL (a server's DNS-rebinding
//     defence has something to pin), and redirects stay denied (no cross-origin session migration);
//   - a registration/discovery URL is resolve-vetted (VetHTTPURL) so a name that ALREADY points at a
//     private/loopback/metadata range is rejected at create/discover, not only at dial.
//
// A credential NEVER enters this file: an audience is a non-secret origin string; the bearer is resolved and
// used only inside the transport (manager.resolveBearer). Nothing here logs, returns, or stores a bearer.

var (
	// ErrAudienceMismatch is returned when a connection's dial origin differs from the audience its bearer is
	// bound to — a replay of one connection's token to a different upstream. It wraps egress.ErrDenied so a
	// caller classifies it terminal alongside the SSRF denials.
	ErrAudienceMismatch = fmt.Errorf("%w: mcp bearer audience does not match the dial origin", egress.ErrDenied)
	// ErrOAuthMetadata is returned when a connection declares OAuth metadata that fails the passive
	// PKCE/exact-redirect validation (E12 T6 step 8). It is a create/discover reject, never a runtime one.
	ErrOAuthMetadata = errors.New("mcp: oauth metadata is invalid")
)

// OriginOf renders a URL's origin — scheme://host[:port], lowercased — which is both the Origin header value
// and the audience a static bearer is bound to. A URL that does not parse, or carries no host, is an error.
func OriginOf(rawURL string) (string, error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return "", fmt.Errorf("parse url origin: %w", err)
	}
	if u.Scheme == "" || u.Host == "" {
		return "", fmt.Errorf("url %q has no scheme/host to form an origin", rawURL)
	}
	return strings.ToLower(u.Scheme) + "://" + strings.ToLower(u.Host), nil
}

// VetAudience denies a dial whose origin differs from the bearer's declared audience (TOL-009 replay
// defence). An empty audience opts out of the extra binding: the connection's own URL origin IS the audience
// then (the pinned dialer + Origin header already bind the token to the registered host), so a connection
// that does not pin an explicit audience is not blocked. A non-empty audience that mismatches the dial origin
// is a terminal denial — connection A's token, declared for A, is never sent to a URL whose origin is B.
func VetAudience(dialURL, audience string) error {
	if audience == "" {
		return nil
	}
	origin, err := OriginOf(dialURL)
	if err != nil {
		return err
	}
	if !strings.EqualFold(origin, audience) {
		return fmt.Errorf("%w: audience %q, dial origin %q", ErrAudienceMismatch, audience, origin)
	}
	return nil
}

// VetHTTPURL is the fail-fast registration/discovery gate: the static check PLUS a DNS resolution whose every
// answer is vetted (a name that already points internal is rejected early). Resolution failure is permissive
// — the pinned dialer is authoritative at connect. It delegates to egress.VetResolved so there is one copy of
// the resolve→vet idiom. (Wired into CreateMCPConnection + DiscoverConnection via Manager.VetConnection —
// before T6 it had NO production caller.)
func VetHTTPURL(ctx context.Context, resolver egress.Resolver, rawURL string, allowPrivate bool) error {
	return egress.VetResolved(ctx, resolver, rawURL, allowPrivate)
}

// ValidateOAuthMetadata is the PASSIVE OAuth validator (E12 T6 step 8): when a connection declares an `oauth`
// metadata block, PKCE MUST be S256 and the redirect_uri MUST be an exact https URL (no wildcard, no plain
// http). An absent/empty block is a no-op — most connections use a static secret_ref bearer and carry none.
//
// HONEST CEILING: this validates SHAPE only. Palai runs NO interactive OAuth flow (no authorization-code
// redirect, no token exchange) — that is E17. The block also carries NO secret: a client_secret (or any key
// outside the allowlist below) is a reject, so this never opens an inline-credential door.
func ValidateOAuthMetadata(oauth map[string]any) error {
	if len(oauth) == 0 {
		return nil
	}
	for k := range oauth {
		switch k {
		case "authorization_endpoint", "token_endpoint", "code_challenge_method", "redirect_uri":
		default:
			return fmt.Errorf("%w: unexpected oauth key %q (a credential must be a secret_ref, never inline)", ErrOAuthMetadata, k)
		}
	}
	// An endpoint, when present, MUST parse as an https URL — so a secret cannot hide as an endpoint VALUE
	// (the allowlist gates keys; this gates the values a would-be smuggler controls).
	for _, k := range []string{"authorization_endpoint", "token_endpoint"} {
		v, present := oauth[k]
		if !present {
			continue
		}
		s, _ := v.(string)
		if u, err := url.Parse(s); err != nil || u.Scheme != "https" || u.Host == "" {
			return fmt.Errorf("%w: %s must be an https URL", ErrOAuthMetadata, k)
		}
	}
	if m, _ := oauth["code_challenge_method"].(string); m != "S256" {
		return fmt.Errorf("%w: PKCE code_challenge_method must be S256, got %q", ErrOAuthMetadata, m)
	}
	redirect, _ := oauth["redirect_uri"].(string)
	if redirect == "" {
		return fmt.Errorf("%w: an exact https redirect_uri is required", ErrOAuthMetadata)
	}
	if strings.Contains(redirect, "*") {
		return fmt.Errorf("%w: redirect_uri must be exact, no wildcard", ErrOAuthMetadata)
	}
	u, err := url.Parse(redirect)
	if err != nil || u.Scheme != "https" || u.Host == "" {
		return fmt.Errorf("%w: redirect_uri must be an absolute https URL", ErrOAuthMetadata)
	}
	return nil
}
