package extensions

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/palgroup/palai/adapters/integrations/mcp"
	"github.com/palgroup/palai/packages/egress"
	"github.com/palgroup/palai/storage"
)

// The MCP connection management surface (spec §28.13-28.14, E12 Task 5). A connection is an admin-registered
// upstream MCP server binding; create + discover are admin actions (never model-facing). This file owns the
// management writes/reads; discovery.go turns a connection's tools/list into namespaced tool_revisions, and
// lookup.go's mcp branch resolves a discovered tool to its rider-intersected connection at dispatch.

var (
	// ErrInvalidTransport is returned for a transport other than stdio|http.
	ErrInvalidTransport = errors.New("extensions: mcp transport must be stdio or http")
	// ErrInvalidConnectionName is returned for a name that is not a single non-empty ASCII segment within
	// the length bound (it is the discovery namespace segment — mcp.<name>.<tool> — so it must be a clean
	// canonical segment).
	ErrInvalidConnectionName = errors.New("extensions: mcp connection name must be a single ASCII segment")
	// ErrInvalidConnectionConfig is returned when the transport-specific config is missing/malformed: a
	// stdio config needs a pinned image_digest + non-empty cmd; an http config needs a url. A secret is
	// NEVER inline — it is the secret_ref handle only.
	ErrInvalidConnectionConfig = errors.New("extensions: mcp connection config is invalid for its transport")
	// ErrConnectionNameCollision is returned when a connection name is already taken in the project.
	ErrConnectionNameCollision = errors.New("extensions: mcp connection name already exists in this project")
	// ErrConnectionNotFound is returned when a discover/disable targets a connection absent from scope.
	ErrConnectionNotFound = errors.New("extensions: mcp connection not found in scope")
)

// Connection is a registered MCP server binding's committed shape. Config carries only NON-secret wiring;
// a credential is the SecretRef handle. TrustLevel is explicit (§28.13); the capability ceiling is the
// AgentRevision.mcp_connections rider intersection, enforced at lookup, not by this value.
type Connection struct {
	ID         string
	Name       string
	Transport  string
	Config     map[string]any
	SecretRef  string
	TrustLevel string
	Disabled   bool
}

// MCPConnectionInput is the strict-decoded create body. A field outside this struct — including an inline
// credential — is rejected by DisallowUnknownFields, so a secret can only enter as a secret_ref handle.
type MCPConnectionInput struct {
	Name       string         `json:"name"`
	Transport  string         `json:"transport"`
	Config     map[string]any `json:"config"`
	SecretRef  string         `json:"secret_ref"`
	TrustLevel string         `json:"trust_level"`
}

// CreateMCPConnection registers a connection after validating its name, transport, and transport-specific
// config. It is an admin action — never reachable from a tool the model can call.
func (s *Store) CreateMCPConnection(ctx context.Context, org, project string, raw []byte) (Connection, error) {
	ctx = storage.ScopeToTenant(ctx, org, project)
	in, err := decodeMCPConnectionInput(raw)
	if err != nil {
		return Connection{}, err
	}
	if !isASCIIName(in.Name) || in.Name == "" || len(in.Name) > maxSegmentLen {
		return Connection{}, fmt.Errorf("%w: got %q", ErrInvalidConnectionName, in.Name)
	}
	if err := validateConnectionConfig(in.Transport, in.Config); err != nil {
		return Connection{}, err
	}
	// Fail-fast SSRF gate at REGISTRATION (E12 T6 step 4): resolve-vet the http URL + audience through the
	// wired MCP client, so a name that ALREADY points internal is rejected before the connection is stored —
	// not only at the first dial. A binderless store (no client wired) skips it, symmetric with the current
	// creatable-but-not-discoverable posture; the static egress.VetURL in validateConnectionConfig still ran.
	if s.mcp != nil {
		vetConn := connConfig(org, Connection{Name: in.Name, Transport: in.Transport, Config: in.Config})
		if err := s.mcp.VetConnection(ctx, vetConn); err != nil {
			return Connection{}, fmt.Errorf("%w: %v", ErrInvalidConnectionConfig, err)
		}
	}
	trust := in.TrustLevel
	if trust == "" {
		trust = "untrusted"
	}
	id := newID("mcpc")
	if _, err := s.pool.Exec(ctx, storage.Query("InsertMCPConnection"),
		id, org, project, in.Name, in.Transport, marshalJSON(in.Config), nullableText(in.SecretRef), trust); err != nil {
		if isUniqueViolation(err) {
			return Connection{}, ErrConnectionNameCollision
		}
		return Connection{}, fmt.Errorf("insert mcp connection: %w", err)
	}
	return Connection{ID: id, Name: in.Name, Transport: in.Transport, Config: in.Config, SecretRef: in.SecretRef, TrustLevel: trust}, nil
}

// GetMCPConnection reads a connection for a discover action (tenant-scoped, disabled or not).
func (s *Store) GetMCPConnection(ctx context.Context, org, project, id string) (Connection, error) {
	ctx = storage.ScopeToTenant(ctx, org, project)
	c := Connection{ID: id}
	var configJSON []byte
	var secretRef *string
	err := s.pool.QueryRow(ctx, storage.Query("GetMCPConnection"), id, org, project).
		Scan(&c.ID, &c.Name, &c.Transport, &configJSON, &secretRef, &c.TrustLevel, &c.Disabled)
	if errors.Is(err, pgx.ErrNoRows) {
		return Connection{}, ErrConnectionNotFound
	}
	if err != nil {
		return Connection{}, fmt.Errorf("read mcp connection: %w", err)
	}
	c.Config = decodeSchema(configJSON)
	if secretRef != nil {
		c.SecretRef = *secretRef
	}
	return c, nil
}

// MCPConnectionExists reports whether a connection id is in scope — the AgentRevision-rider validation gate
// (a revision may only name connections that really exist in the project).
func (s *Store) MCPConnectionExists(ctx context.Context, org, project, id string) (bool, error) {
	ctx = storage.ScopeToTenant(ctx, org, project)
	switch err := s.pool.QueryRow(ctx, storage.Query("MCPConnectionExists"), id, org, project).Scan(new(int)); {
	case errors.Is(err, pgx.ErrNoRows):
		return false, nil
	case err != nil:
		return false, fmt.Errorf("check mcp connection: %w", err)
	}
	return true, nil
}

// DisableMCPConnection flips the admin kill-switch once. Reports whether the connection existed in scope.
func (s *Store) DisableMCPConnection(ctx context.Context, org, project, id string) (bool, error) {
	ctx = storage.ScopeToTenant(ctx, org, project)
	switch err := s.pool.QueryRow(ctx, storage.Query("DisableMCPConnection"), id, org, project).Scan(new(string)); {
	case err == nil:
		return true, nil
	case !errors.Is(err, pgx.ErrNoRows):
		return false, fmt.Errorf("disable mcp connection: %w", err)
	}
	// No flip: either already-disabled or unknown. Existence disambiguates.
	return s.MCPConnectionExists(ctx, org, project, id)
}

// decodeMCPConnectionInput strictly decodes the create body, rejecting unknown fields (no inline secret).
func decodeMCPConnectionInput(raw []byte) (MCPConnectionInput, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	var in MCPConnectionInput
	if err := dec.Decode(&in); err != nil {
		return MCPConnectionInput{}, fmt.Errorf("%w: %v", ErrUnknownField, err)
	}
	return in, nil
}

// validateConnectionConfig enforces the transport-specific non-secret wiring. A stdio connection must pin a
// sha256 image digest and a non-empty argv; an http connection must carry a url. It also ALLOWLISTS the keys
// per transport, so a credential can never land inline in the connection JSONB (e.g. {"bearer":"sk-.."}) —
// the secret_ref handle is the ONLY credential path (spec §28.4). An unknown/credential-shaped key is a reject.
func validateConnectionConfig(transport string, config map[string]any) error {
	switch transport {
	case "stdio":
		digest, _ := config["image_digest"].(string)
		if !immutableImageDigest(digest) {
			return fmt.Errorf("%w: stdio needs a sha256 image_digest", ErrInvalidConnectionConfig)
		}
		cmd, ok := config["cmd"].([]any)
		if !ok || len(cmd) == 0 {
			return fmt.Errorf("%w: stdio needs a non-empty cmd", ErrInvalidConnectionConfig)
		}
		// sampling/sampling_max_tokens are the only E12 T6 additions for stdio (a stdio server has no HTTP
		// origin, so no url/audience/oauth). A stdio server's sampling request rides the same gate.
		return allowlistConfigKeys(config, "image_digest", "cmd", "sampling", "sampling_max_tokens")
	case "http":
		url, _ := config["url"].(string)
		if url == "" {
			return fmt.Errorf("%w: http needs a url", ErrInvalidConnectionConfig)
		}
		// Static egress gate at REGISTRATION: an internal literal IP, an http downgrade, or a URL embedding a
		// credential is rejected here (the resolve-vet fail-fast half is Manager.VetConnection, wired into
		// create/discover). The pinned dialer stays the authoritative connect-time gate.
		if err := egress.VetURL(url, false); err != nil {
			return fmt.Errorf("%w: %v", ErrInvalidConnectionConfig, err)
		}
		// A declared oauth block is validated passively (PKCE S256 + exact https redirect; no inline secret).
		// A PRESENT oauth that is not an object is rejected outright — otherwise a raw-string oauth (a secret
		// smuggled as the value) would silently pass the type assertion and land as PLAINTEXT in config JSONB.
		if raw, present := config["oauth"]; present {
			oauth, ok := raw.(map[string]any)
			if !ok {
				return fmt.Errorf("%w: oauth must be an object", ErrInvalidConnectionConfig)
			}
			if err := mcp.ValidateOAuthMetadata(oauth); err != nil {
				return fmt.Errorf("%w: %v", ErrInvalidConnectionConfig, err)
			}
		}
		return allowlistConfigKeys(config, "url", "audience", "oauth", "sampling", "sampling_max_tokens")
	default:
		return fmt.Errorf("%w: got %q", ErrInvalidTransport, transport)
	}
}

// allowlistConfigKeys rejects any config key outside the transport's non-secret allowlist — the guard that
// keeps a credential out of the connection row entirely (it can only enter as a secret_ref handle).
func allowlistConfigKeys(config map[string]any, allowed ...string) error {
	set := make(map[string]bool, len(allowed))
	for _, k := range allowed {
		set[k] = true
	}
	for k := range config {
		if !set[k] {
			return fmt.Errorf("%w: unexpected config key %q (a credential must be a secret_ref, never inline)", ErrInvalidConnectionConfig, k)
		}
	}
	return nil
}

// immutableImageDigest reports whether s is a canonical lowercase sha256 content digest (the oci driver's
// pin rule, checked here so a mutable stdio image is a create reject, not a run-time surprise).
func immutableImageDigest(s string) bool {
	if len(s) != len("sha256:")+64 || s[:7] != "sha256:" {
		return false
	}
	for i := 7; i < len(s); i++ {
		c := s[i]
		if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f')) {
			return false
		}
	}
	return true
}
