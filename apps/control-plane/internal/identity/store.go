// Package identity is the durable store behind the tenancy provisioning API (spec §39.2, E13 Task 2,
// TEN-003/MCI-001): organizations, projects (+ the §14 config_policy write-path), and API keys. It is the
// write half of the identity tables migration 000001 declared, and the machinery bootstrap reuses to seed
// the very first organization — so a second tenant is opened by exactly the same code path, over the API,
// with no process restart and no manual SQL.
//
// SCOPING (the load-bearing rule, migration 000029): organization CREATION runs under the system scope,
// because it establishes a tenant before one exists (the deliberate, greppable escape hatch, exactly like
// bootstrap and VerifyAPIKey). Every other operation runs under the CALLER's own organization, widened to
// the whole org (project-agnostic): API keys and projects are organization-level identity resources, and
// the per-project narrowing that isolates run/session data does not gate identity management. The
// organization boundary itself is NEVER widened — the org always comes from the verified key, never a body
// field — so a caller can only ever administer its own organization.
//
// HONEST CEILING: this is basic scopes only. A key that can provision can provision its whole
// organization; named roles, relationships, and OIDC are E13-H/E17.
package identity

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/palgroup/palai/apps/control-plane/api"
	"github.com/palgroup/palai/apps/control-plane/api/middleware"
	"github.com/palgroup/palai/packages/coordinator"
	"github.com/palgroup/palai/storage"
)

// Store provisions tenants over one shared pool. Each method scopes itself by the verified identity passed
// in (org creation under the system scope), so no tenant state leaks between requests.
type Store struct {
	pool *pgxpool.Pool
}

// New builds a provisioning store over the durable spine's pool.
func New(pool *pgxpool.Pool) *Store { return &Store{pool: pool} }

// The fixed identity bootstrap seeds as the first organization. Stable so a re-boot against a retained
// volume is a no-op (ProvisionFirstOrg is guarded by the caller's api_keys count and every insert is
// ON CONFLICT DO NOTHING). A SECOND tenant never reuses these — it is minted with fresh ids over the API.
const (
	firstOrg       = "org_local"
	firstProject   = "prj_local"
	firstPrincipal = "prin_local"
	firstKey       = "key_local"
)

// tenantSeed is the four rows a provisioned organization is born with: the organization, its default
// project, a service principal, and an admin API key (stored as a hash — the bearer value never persists).
type tenantSeed struct {
	orgID, orgName         string
	projectID, projectName string
	principalID            string
	keyID, keyHash         string
	scopes                 []string
	expiresAt              *time.Time
}

// provision inserts a tenant seed in one system-scoped transaction. Organization creation must run under
// the system scope: no palai.org_id exists yet for the org being born, so the RLS policies would otherwise
// deny every insert. Every statement is ON CONFLICT DO NOTHING, so a re-run against an already-seeded id
// is a clean no-op (the bootstrap re-boot path).
func (s *Store) provision(ctx context.Context, seed tenantSeed) error {
	ctx = storage.WithSystemScope(ctx)
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin provision: %w", err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()

	if _, err := tx.Exec(ctx, storage.Query("InsertOrganization"), seed.orgID, seed.orgName); err != nil {
		return fmt.Errorf("insert organization: %w", err)
	}
	if _, err := tx.Exec(ctx, storage.Query("InsertProject"), seed.projectID, seed.orgID, seed.projectName); err != nil {
		return fmt.Errorf("insert project: %w", err)
	}
	if _, err := tx.Exec(ctx, storage.Query("InsertPrincipal"), seed.principalID, seed.orgID, seed.projectID, "service"); err != nil {
		return fmt.Errorf("insert principal: %w", err)
	}
	if _, err := tx.Exec(ctx, storage.Query("InsertAPIKey"),
		seed.keyID, seed.orgID, seed.projectID, seed.principalID, seed.keyHash, seed.scopes, seed.expiresAt); err != nil {
		return fmt.Errorf("insert api key: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit provision: %w", err)
	}
	return nil
}

// ProvisionFirstOrg seeds the single bootstrap organization and its admin key from bootstrapKey, reusing
// the same tenant-creation path the API uses for every later organization. Only the key's hash is stored.
// The caller (store.Bootstrap) guards this with an api_keys-empty check, so it runs once per fresh stack.
func (s *Store) ProvisionFirstOrg(ctx context.Context, bootstrapKey string) error {
	return s.provision(ctx, tenantSeed{
		orgID:       firstOrg,
		projectID:   firstProject,
		principalID: firstPrincipal,
		keyID:       firstKey,
		keyHash:     coordinator.HashAPIKey(bootstrapKey),
		scopes:      []string{},
	})
}

// orgScope widens the request to its whole organization for a provisioning read/write. The organization
// comes from the verified key (never a body field), so this can only ever reach the caller's own tenant;
// it relaxes ONLY the intra-org project narrowing, because identity resources are managed org-wide.
func orgScope(ctx context.Context, scope middleware.Scope) context.Context {
	return storage.WithTenant(ctx, scope.Organization, "")
}

// CreateOrganization opens a NEW tenant: an organization, its default project, a service principal, and a
// full-capability admin API key whose plaintext is returned exactly once. This is the sole cross-tenant
// provisioning operation (system-scoped) — the API path bootstrap generalizes.
func (s *Store) CreateOrganization(ctx context.Context, _ middleware.Scope, body []byte) (api.ProvisionResult, error) {
	var in struct {
		DisplayName string `json:"display_name"`
	}
	if err := strictDecode(body, &in); err != nil {
		return api.ProvisionResult{BadField: true}, nil
	}
	seed := tenantSeed{
		orgID:       middleware.NewID("org"),
		orgName:     in.DisplayName,
		projectID:   middleware.NewID("prj"),
		projectName: "default",
		principalID: middleware.NewID("prin"),
		keyID:       middleware.NewID("key"),
		scopes:      []string{},
	}
	secret := newSecret()
	seed.keyHash = coordinator.HashAPIKey(secret)
	if err := s.provision(ctx, seed); err != nil {
		return api.ProvisionResult{}, err
	}
	out := organizationCreated{
		organizationView: organizationView{ID: seed.orgID, Object: "organization", DisplayName: seed.orgName},
		DefaultProjectID: seed.projectID,
		AdminAPIKey: apiKeyView{
			ID: seed.keyID, Object: "api_key", OrganizationID: seed.orgID,
			ProjectID: seed.projectID, PrincipalID: seed.principalID, Scopes: seed.scopes, Key: secret,
		},
	}
	return api.ProvisionResult{Body: mustJSON(out)}, nil
}

// ListOrganizations lists the organizations visible in the caller's scope — under RLS, the caller's own
// organization (no cross-tenant listing).
func (s *Store) ListOrganizations(ctx context.Context, scope middleware.Scope) (api.ProvisionResult, error) {
	ctx = orgScope(ctx, scope)
	rows, err := s.pool.Query(ctx, storage.Query("ListOrganizations"))
	if err != nil {
		return api.ProvisionResult{}, fmt.Errorf("list organizations: %w", err)
	}
	defer rows.Close()
	data := []organizationView{}
	for rows.Next() {
		v, err := scanOrganization(rows)
		if err != nil {
			return api.ProvisionResult{}, err
		}
		data = append(data, v)
	}
	if err := rows.Err(); err != nil {
		return api.ProvisionResult{}, fmt.Errorf("iterate organizations: %w", err)
	}
	return api.ProvisionResult{Body: mustJSON(listView{Object: "list", Data: data})}, nil
}

// GetOrganization reads one organization within the caller's scope; a foreign/unknown id is a miss (404).
func (s *Store) GetOrganization(ctx context.Context, scope middleware.Scope, id string) (api.ProvisionResult, error) {
	ctx = orgScope(ctx, scope)
	v, err := scanOrganization(s.pool.QueryRow(ctx, storage.Query("GetOrganization"), id))
	if errors.Is(err, pgx.ErrNoRows) {
		return api.ProvisionResult{NotFound: true}, nil
	}
	if err != nil {
		return api.ProvisionResult{}, fmt.Errorf("get organization: %w", err)
	}
	return api.ProvisionResult{Body: mustJSON(v)}, nil
}

// CreateProject registers a project in the caller's organization.
func (s *Store) CreateProject(ctx context.Context, scope middleware.Scope, body []byte) (api.ProvisionResult, error) {
	var in struct {
		DisplayName string `json:"display_name"`
	}
	if err := strictDecode(body, &in); err != nil {
		return api.ProvisionResult{BadField: true}, nil
	}
	ctx = orgScope(ctx, scope)
	projID := middleware.NewID("prj")
	if _, err := s.pool.Exec(ctx, storage.Query("InsertProject"), projID, scope.Organization, in.DisplayName); err != nil {
		return api.ProvisionResult{}, fmt.Errorf("insert project: %w", err)
	}
	v := projectView{ID: projID, Object: "project", OrganizationID: scope.Organization, DisplayName: in.DisplayName, ConfigPolicy: json.RawMessage("null")}
	return api.ProvisionResult{Body: mustJSON(v)}, nil
}

// ListProjects lists every project in the caller's organization.
func (s *Store) ListProjects(ctx context.Context, scope middleware.Scope) (api.ProvisionResult, error) {
	ctx = orgScope(ctx, scope)
	rows, err := s.pool.Query(ctx, storage.Query("ListProjects"))
	if err != nil {
		return api.ProvisionResult{}, fmt.Errorf("list projects: %w", err)
	}
	defer rows.Close()
	data := []projectView{}
	for rows.Next() {
		v, err := scanProject(rows)
		if err != nil {
			return api.ProvisionResult{}, err
		}
		data = append(data, v)
	}
	if err := rows.Err(); err != nil {
		return api.ProvisionResult{}, fmt.Errorf("iterate projects: %w", err)
	}
	return api.ProvisionResult{Body: mustJSON(listView{Object: "list", Data: data})}, nil
}

// GetProject reads one project within the caller's organization; a foreign/unknown id is a miss (404).
func (s *Store) GetProject(ctx context.Context, scope middleware.Scope, id string) (api.ProvisionResult, error) {
	ctx = orgScope(ctx, scope)
	return s.readProject(ctx, id)
}

// UpdateProjectPolicy writes the §14 project-layer config_policy the resolver reads (strict schema,
// unknown-field reject — the E11 T1 decode pattern). This is the first API that makes the resolver's
// project layer reachable. A foreign/unknown id updates zero rows and is a miss (404).
func (s *Store) UpdateProjectPolicy(ctx context.Context, scope middleware.Scope, id string, body []byte) (api.ProvisionResult, error) {
	var in struct {
		ConfigPolicy *configPolicyInput `json:"config_policy"`
	}
	if err := strictDecode(body, &in); err != nil {
		return api.ProvisionResult{BadField: true}, nil
	}
	if in.ConfigPolicy == nil {
		return api.ProvisionResult{MissingField: "config_policy"}, nil
	}
	policyJSON, err := json.Marshal(in.ConfigPolicy)
	if err != nil {
		return api.ProvisionResult{}, fmt.Errorf("marshal config policy: %w", err)
	}
	ctx = orgScope(ctx, scope)
	var updated string
	err = s.pool.QueryRow(ctx, storage.Query("UpdateProjectConfigPolicy"), id, policyJSON).Scan(&updated)
	if errors.Is(err, pgx.ErrNoRows) {
		return api.ProvisionResult{NotFound: true}, nil
	}
	if err != nil {
		return api.ProvisionResult{}, fmt.Errorf("update project config policy: %w", err)
	}
	return s.readProject(ctx, id)
}

// CreateAPIKey mints a key (and its service principal) for a project in the caller's organization. The
// plaintext is returned exactly once; only its hash is stored. An absent project_id is a 400; a
// project outside the caller's organization is invisible under RLS and rendered a 404.
func (s *Store) CreateAPIKey(ctx context.Context, scope middleware.Scope, body []byte) (api.ProvisionResult, error) {
	var in struct {
		ProjectID string     `json:"project_id"`
		Scopes    []string   `json:"scopes"`
		ExpiresAt *time.Time `json:"expires_at"`
	}
	if err := strictDecode(body, &in); err != nil {
		return api.ProvisionResult{BadField: true}, nil
	}
	if in.ProjectID == "" {
		return api.ProvisionResult{MissingField: "project_id"}, nil
	}
	ctx = orgScope(ctx, scope)
	if err := s.pool.QueryRow(ctx, storage.Query("ProjectExists"), in.ProjectID).Scan(new(int)); errors.Is(err, pgx.ErrNoRows) {
		return api.ProvisionResult{NotFound: true}, nil
	} else if err != nil {
		return api.ProvisionResult{}, fmt.Errorf("verify project: %w", err)
	}
	scopes := in.Scopes
	if scopes == nil {
		scopes = []string{}
	}
	principalID, keyID := middleware.NewID("prin"), middleware.NewID("key")
	secret := newSecret()

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return api.ProvisionResult{}, fmt.Errorf("begin create api key: %w", err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	if _, err := tx.Exec(ctx, storage.Query("InsertPrincipal"), principalID, scope.Organization, in.ProjectID, "service"); err != nil {
		return api.ProvisionResult{}, fmt.Errorf("insert principal: %w", err)
	}
	if _, err := tx.Exec(ctx, storage.Query("InsertAPIKey"),
		keyID, scope.Organization, in.ProjectID, principalID, coordinator.HashAPIKey(secret), scopes, in.ExpiresAt); err != nil {
		return api.ProvisionResult{}, fmt.Errorf("insert api key: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return api.ProvisionResult{}, fmt.Errorf("commit create api key: %w", err)
	}
	v := apiKeyView{
		ID: keyID, Object: "api_key", OrganizationID: scope.Organization, ProjectID: in.ProjectID,
		PrincipalID: principalID, Scopes: scopes, ExpiresAt: in.ExpiresAt, Key: secret,
	}
	return api.ProvisionResult{Body: mustJSON(v)}, nil
}

// ListAPIKeys lists key METADATA (never the hash or plaintext) for every key in the caller's organization.
func (s *Store) ListAPIKeys(ctx context.Context, scope middleware.Scope) (api.ProvisionResult, error) {
	ctx = orgScope(ctx, scope)
	rows, err := s.pool.Query(ctx, storage.Query("ListAPIKeys"))
	if err != nil {
		return api.ProvisionResult{}, fmt.Errorf("list api keys: %w", err)
	}
	defer rows.Close()
	data := []apiKeyView{}
	for rows.Next() {
		v, err := scanAPIKey(rows)
		if err != nil {
			return api.ProvisionResult{}, err
		}
		data = append(data, v)
	}
	if err := rows.Err(); err != nil {
		return api.ProvisionResult{}, fmt.Errorf("iterate api keys: %w", err)
	}
	return api.ProvisionResult{Body: mustJSON(listView{Object: "list", Data: data})}, nil
}

// GetAPIKey reads one key's metadata within the caller's organization; a foreign/unknown id is a miss (404).
func (s *Store) GetAPIKey(ctx context.Context, scope middleware.Scope, id string) (api.ProvisionResult, error) {
	ctx = orgScope(ctx, scope)
	v, err := scanAPIKey(s.pool.QueryRow(ctx, storage.Query("GetAPIKey"), id))
	if errors.Is(err, pgx.ErrNoRows) {
		return api.ProvisionResult{NotFound: true}, nil
	}
	if err != nil {
		return api.ProvisionResult{}, fmt.Errorf("get api key: %w", err)
	}
	return api.ProvisionResult{Body: mustJSON(v)}, nil
}

// RevokeAPIKey revokes a key in the caller's organization (idempotent — the first revoked_at is kept). A
// foreign/unknown id is a miss (404). The response renders the key's current metadata.
func (s *Store) RevokeAPIKey(ctx context.Context, scope middleware.Scope, id string) (api.ProvisionResult, error) {
	ctx = orgScope(ctx, scope)
	var revoked string
	err := s.pool.QueryRow(ctx, storage.Query("RevokeAPIKey"), id).Scan(&revoked)
	if errors.Is(err, pgx.ErrNoRows) {
		return api.ProvisionResult{NotFound: true}, nil
	}
	if err != nil {
		return api.ProvisionResult{}, fmt.Errorf("revoke api key: %w", err)
	}
	v, err := scanAPIKey(s.pool.QueryRow(ctx, storage.Query("GetAPIKey"), id))
	if err != nil {
		return api.ProvisionResult{}, fmt.Errorf("read revoked api key: %w", err)
	}
	return api.ProvisionResult{Body: mustJSON(v)}, nil
}

// readProject renders one project within an already-scoped context.
func (s *Store) readProject(ctx context.Context, id string) (api.ProvisionResult, error) {
	v, err := scanProject(s.pool.QueryRow(ctx, storage.Query("GetProject"), id))
	if errors.Is(err, pgx.ErrNoRows) {
		return api.ProvisionResult{NotFound: true}, nil
	}
	if err != nil {
		return api.ProvisionResult{}, fmt.Errorf("get project: %w", err)
	}
	return api.ProvisionResult{Body: mustJSON(v)}, nil
}

// configPolicyInput is the strict §14 config_policy schema (spec §9.3). DisallowUnknownFields rejects any
// field outside it, so a typo or an unsupported knob is a 400 rather than a silently dropped write.
type configPolicyInput struct {
	AllowedModels []string `json:"allowed_models"`
	AllowedTools  []string `json:"allowed_tools"`
	DefaultTools  []string `json:"default_tools"`
}

type organizationView struct {
	ID          string     `json:"id"`
	Object      string     `json:"object"`
	DisplayName string     `json:"display_name"`
	CreatedAt   *time.Time `json:"created_at,omitempty"`
}

type organizationCreated struct {
	organizationView
	DefaultProjectID string     `json:"default_project_id"`
	AdminAPIKey      apiKeyView `json:"admin_api_key"`
}

type projectView struct {
	ID             string          `json:"id"`
	Object         string          `json:"object"`
	OrganizationID string          `json:"organization_id"`
	DisplayName    string          `json:"display_name"`
	ConfigPolicy   json.RawMessage `json:"config_policy"`
	CreatedAt      *time.Time      `json:"created_at,omitempty"`
}

// apiKeyView renders a key. Key (the plaintext) carries omitempty and is set ONLY on a create response —
// every read leaves it empty, so a listing or retrieval can never disclose a secret. key_hash is never a
// field at all.
type apiKeyView struct {
	ID             string     `json:"id"`
	Object         string     `json:"object"`
	OrganizationID string     `json:"organization_id"`
	ProjectID      string     `json:"project_id"`
	PrincipalID    string     `json:"principal_id"`
	Scopes         []string   `json:"scopes"`
	ExpiresAt      *time.Time `json:"expires_at,omitempty"`
	CreatedAt      *time.Time `json:"created_at,omitempty"`
	RevokedAt      *time.Time `json:"revoked_at,omitempty"`
	Key            string     `json:"key,omitempty"`
}

type listView struct {
	Object string `json:"object"`
	Data   any    `json:"data"`
}

// scanner is the row shape shared by pool.QueryRow and rows.
type scanner interface {
	Scan(dest ...any) error
}

func scanOrganization(row scanner) (organizationView, error) {
	v := organizationView{Object: "organization"}
	var createdAt time.Time
	if err := row.Scan(&v.ID, &v.DisplayName, &createdAt); err != nil {
		return organizationView{}, err
	}
	v.CreatedAt = &createdAt
	return v, nil
}

func scanProject(row scanner) (projectView, error) {
	v := projectView{Object: "project"}
	var policy []byte
	var createdAt time.Time
	if err := row.Scan(&v.ID, &v.OrganizationID, &v.DisplayName, &policy, &createdAt); err != nil {
		return projectView{}, err
	}
	if policy == nil {
		v.ConfigPolicy = json.RawMessage("null")
	} else {
		v.ConfigPolicy = policy
	}
	v.CreatedAt = &createdAt
	return v, nil
}

func scanAPIKey(row scanner) (apiKeyView, error) {
	v := apiKeyView{Object: "api_key"}
	var createdAt time.Time
	if err := row.Scan(&v.ID, &v.OrganizationID, &v.ProjectID, &v.PrincipalID, &v.Scopes, &v.ExpiresAt, &createdAt, &v.RevokedAt); err != nil {
		return apiKeyView{}, err
	}
	v.CreatedAt = &createdAt
	return v, nil
}

// strictDecode decodes body into v rejecting unknown fields (the E11 T1 pattern). An empty body decodes as
// an empty object, so an all-optional create with no body is accepted.
func strictDecode(body []byte, v any) error {
	if len(bytes.TrimSpace(body)) == 0 {
		body = []byte("{}")
	}
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return err
	}
	return nil
}

// newSecret mints a high-entropy bearer key. API keys are high-entropy tokens, so a fast hash is the
// stored verifier (coordinator.HashAPIKey), not a password KDF.
func newSecret() string {
	var raw [24]byte
	if _, err := rand.Read(raw[:]); err != nil {
		panic(fmt.Sprintf("identity: generate api key: %v", err))
	}
	return "sk_" + hex.EncodeToString(raw[:])
}

func mustJSON(v any) []byte {
	b, err := json.Marshal(v)
	if err != nil {
		panic(fmt.Sprintf("identity: marshal projection: %v", err))
	}
	return b
}
