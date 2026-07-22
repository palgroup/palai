package extensions

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

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
// sha256 image digest and a non-empty argv; an http connection must carry a url. A credential is never
// inline — it is the secret_ref handle, which this never inspects.
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
		return nil
	case "http":
		if url, _ := config["url"].(string); url == "" {
			return fmt.Errorf("%w: http needs a url", ErrInvalidConnectionConfig)
		}
		return nil
	default:
		return fmt.Errorf("%w: got %q", ErrInvalidTransport, transport)
	}
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
