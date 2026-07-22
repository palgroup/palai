//go:build component

package extensions

import (
	"context"
	"errors"
	"os"
	"testing"

	"github.com/palgroup/palai/packages/coordinator"
)

// TestMCPConnectionCreateReadIdempotentMigration proves the 000026 store spine: a connection is created and
// read back; the migration is idempotent (a re-run of Migrate leaves the row intact); a duplicate name is a
// typed collision; a bad transport/config is rejected before any write; and the admin kill-switch disables
// once. It runs against a real migrated Postgres (the registry_component_test openStore pattern).
func TestMCPConnectionCreateReadIdempotentMigration(t *testing.T) {
	s, org, project := openStore(t)
	ctx := context.Background()

	body := []byte(`{"name":"docs","transport":"stdio","config":{"image_digest":"sha256:` + hex64() + `","cmd":["/mcp"]},"trust_level":"untrusted"}`)
	conn, err := s.CreateMCPConnection(ctx, org, project, body)
	if err != nil {
		t.Fatalf("create connection: %v", err)
	}
	if conn.Name != "docs" || conn.Transport != "stdio" || conn.TrustLevel != "untrusted" {
		t.Fatalf("created connection = %+v, want docs/stdio/untrusted", conn)
	}

	// Read-back matches, and is present after a second Migrate (idempotent chain re-run).
	cs, err := coordinator.Open(ctx, componentURL(t))
	if err != nil {
		t.Fatalf("coordinator.Open: %v", err)
	}
	t.Cleanup(cs.Close)
	if err := cs.Migrate(ctx); err != nil {
		t.Fatalf("re-Migrate (idempotency): %v", err)
	}
	got, err := s.GetMCPConnection(ctx, org, project, conn.ID)
	if err != nil {
		t.Fatalf("get connection after re-migrate: %v", err)
	}
	if got.Name != "docs" || got.Disabled {
		t.Fatalf("read-back = %+v, want docs enabled (row survived idempotent re-migrate)", got)
	}
	// The secret is a handle only — the config carries no credential bytes.
	if _, hasCred := got.Config["credential"]; hasCred {
		t.Fatal("connection config carries an inline credential; a secret must be a secret_ref handle only")
	}

	// A duplicate name is a typed collision.
	if _, err := s.CreateMCPConnection(ctx, org, project, body); !errors.Is(err, ErrConnectionNameCollision) {
		t.Fatalf("duplicate name: err = %v, want ErrConnectionNameCollision", err)
	}

	// A mutable stdio image / missing http url / an INLINE credential in config are rejected before any
	// write — the config JSONB must carry only non-secret wiring (allowlisted keys).
	for name, bad := range map[string]string{
		"mutable stdio image": `{"name":"a","transport":"stdio","config":{"image_digest":"latest","cmd":["/mcp"]}}`,
		"empty stdio cmd":     `{"name":"b","transport":"stdio","config":{"image_digest":"sha256:` + hex64() + `","cmd":[]}}`,
		"http no url":         `{"name":"c","transport":"http","config":{}}`,
		"http inline bearer":  `{"name":"f","transport":"http","config":{"url":"https://x","bearer":"sk-live-abc"}}`,
		"stdio inline token":  `{"name":"g","transport":"stdio","config":{"image_digest":"sha256:` + hex64() + `","cmd":["/mcp"],"token":"sk-live-abc"}}`,
	} {
		if _, err := s.CreateMCPConnection(ctx, org, project, []byte(bad)); !errors.Is(err, ErrInvalidConnectionConfig) {
			t.Errorf("%s: create err = %v, want ErrInvalidConnectionConfig (no inline secret / invalid wiring)", name, err)
		}
	}
	// An unknown transport is its own typed reject.
	if _, err := s.CreateMCPConnection(ctx, org, project, []byte(`{"name":"d","transport":"carrier-pigeon","config":{}}`)); !errors.Is(err, ErrInvalidTransport) {
		t.Errorf("unknown transport: err = %v, want ErrInvalidTransport", err)
	}
	// Prove the inline credential never reached the DB: no connection row carries a bearer/token in config.
	var leaked int
	if err := s.pool.QueryRow(ctx,
		`SELECT count(*) FROM mcp_connections WHERE jsonb_exists(config, 'bearer') OR jsonb_exists(config, 'token')`).Scan(&leaked); err != nil {
		t.Fatalf("scan for leaked inline credential: %v", err)
	}
	if leaked != 0 {
		t.Fatalf("an inline credential landed in %d mcp_connections config rows; a secret must be a secret_ref handle only", leaked)
	}

	// An inline credential field is rejected (DisallowUnknownFields — no secret inline).
	if _, err := s.CreateMCPConnection(ctx, org, project, []byte(`{"name":"e","transport":"http","config":{"url":"https://x"},"credential":"sk-live"}`)); !errors.Is(err, ErrUnknownField) {
		t.Fatalf("inline credential: err = %v, want ErrUnknownField", err)
	}

	// The admin kill-switch disables once; the connection then reads back disabled.
	disabled, err := s.DisableMCPConnection(ctx, org, project, conn.ID)
	if err != nil || !disabled {
		t.Fatalf("disable = %v err = %v, want disabled", disabled, err)
	}
	if got, _ := s.GetMCPConnection(ctx, org, project, conn.ID); !got.Disabled {
		t.Fatal("connection did not read back disabled after the kill-switch")
	}
}

// TestMCPConnectionSamplingAuthConfig proves the E12 T6 create-path: sampling + audience are accepted as
// non-secret wiring and map into the dial config; a declared oauth block is passively validated; and neither
// the widened allowlist nor the oauth block opens an inline-credential door (a client_secret, an internal
// URL, or a plain-PKCE block is a typed reject).
func TestMCPConnectionSamplingAuthConfig(t *testing.T) {
	s, org, project := openStore(t)
	ctx := context.Background()

	body := []byte(`{"name":"sampled","transport":"http","config":{"url":"https://mcp.example.test/mcp","audience":"https://mcp.example.test","sampling":true,"sampling_max_tokens":50,"oauth":{"code_challenge_method":"S256","redirect_uri":"https://app.example.test/cb"}}}`)
	conn, err := s.CreateMCPConnection(ctx, org, project, body)
	if err != nil {
		t.Fatalf("create sampling connection: %v", err)
	}
	got, err := s.GetMCPConnection(ctx, org, project, conn.ID)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	cc := connConfig(org, got)
	if !cc.SamplingEnabled || cc.SamplingMaxTokens != 50 || cc.Audience != "https://mcp.example.test" {
		t.Fatalf("connConfig = %+v, want sampling on, budget 50, audience bound", cc)
	}

	// A plain-PKCE oauth block, an inline oauth client_secret, and an internal URL are all typed rejects —
	// the widened allowlist stays a closed credential door + SSRF gate.
	for name, bad := range map[string]string{
		"plain pkce":   `{"name":"badpkce","transport":"http","config":{"url":"https://x","oauth":{"code_challenge_method":"plain","redirect_uri":"https://app.example.test/cb"}}}`,
		"oauth secret": `{"name":"oauthsec","transport":"http","config":{"url":"https://x","oauth":{"code_challenge_method":"S256","redirect_uri":"https://app.example.test/cb","client_secret":"sk-oops"}}}`,
		"internal url": `{"name":"ssrf","transport":"http","config":{"url":"https://169.254.169.254/mcp"}}`,
		// A raw-string oauth (a secret smuggled as the value) must not silently pass the type assertion and
		// land as plaintext in config JSONB; a secret hiding in an endpoint value must also be rejected.
		"oauth not object":      `{"name":"oauthstr","transport":"http","config":{"url":"https://x","oauth":"sk-live-RAW-BEARER"}}`,
		"oauth endpoint secret": `{"name":"oauthep","transport":"http","config":{"url":"https://x","oauth":{"authorization_endpoint":"sk-live-SECRET","token_endpoint":"https://idp/t","code_challenge_method":"S256","redirect_uri":"https://app.example.test/cb"}}}`,
	} {
		if _, err := s.CreateMCPConnection(ctx, org, project, []byte(bad)); !errors.Is(err, ErrInvalidConnectionConfig) {
			t.Errorf("%s: err = %v, want ErrInvalidConnectionConfig", name, err)
		}
	}
}

// hex64 returns a fixed 64-char lowercase hex string for a pinned digest fixture.
func hex64() string { return "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef" }

// componentURL returns the component postgres URL (the openStore skip already guards its absence).
func componentURL(t *testing.T) string {
	t.Helper()
	return os.Getenv("PALAI_COMPONENT_POSTGRES_URL")
}
