// Package identity is the durable store behind the tenancy provisioning API (spec §39.2, E13 Task 2,
// TEN-003/MCI-001): projects (+ the §14 config_policy write-path) and API keys. It is the write half of the
// identity tables migration 000001 declared, and the machinery bootstrap reuses to seed the very first
// tenant — so a second tenant is opened by exactly the same code path, over the API, with no process
// restart and no manual SQL.
//
// SCOPING (the load-bearing rule, migrations 000029 and 000066). TENANT CREATION runs under the system
// scope, because it establishes a tenant before one exists — the deliberate, greppable escape hatch,
// exactly like bootstrap and VerifyAPIKey. Every other operation runs under provisioningScope, which
// answers a two-way question rather than widening: a key holding `system` is the PLATFORM and administers
// the whole installation (it is what opens the projects); a key without it is a tenant admin and
// administers ITS OWN PROJECT, because after A.2 Task 6 a tenant IS a project. Another project's key,
// principal or policy is invisible rather than forbidden, so a refusal discloses nothing.
//
// HONEST CEILING: this is basic scopes only. A key that can provision can provision its whole project;
// named roles, relationships, and OIDC are E13-H/E17.
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
	"github.com/palgroup/palai/apps/control-plane/internal/fleet"
	"github.com/palgroup/palai/packages/coordinator"
	"github.com/palgroup/palai/storage"
)

// Store provisions tenants over one shared pool. Each method scopes itself by the verified identity passed
// in (tenant creation under the system scope), so no tenant state leaks between requests.
type Store struct {
	pool *pgxpool.Pool
}

// New builds a provisioning store over the durable spine's pool.
func New(pool *pgxpool.Pool) *Store { return &Store{pool: pool} }

// The fixed identity bootstrap seeds as the first tenant. Stable so a re-boot against a retained
// volume is a no-op (ProvisionFirstTenant is guarded by the caller's api_keys count and every insert is
// ON CONFLICT DO NOTHING). A SECOND tenant never reuses these — it is minted with fresh ids over the API.
const (
	firstProject   = "prj_local"
	firstPrincipal = "prin_local"
	firstKey       = "key_local"
)

// tenantSeed is the four rows a provisioned tenant is born with: its project, a service principal, an
// admin API key (stored as a hash — the bearer value never persists), and its default runner pool. It
// carried an organization above those four until A.2 Task 6 dropped the table.
type tenantSeed struct {
	projectID, projectName string
	principalID            string
	keyID, keyHash         string
	scopes                 []string
	expiresAt              *time.Time
	// poolID is the tenant's default runner pool (E24 T1, migration 000045 R6). A runner enrolls INTO
	// a pool, so a tenant with none is a tenant whose runner cannot enroll — which is why this is
	// seeded here, in the transaction that makes the tenant exist, rather than lazily on first
	// enrollment. Migration 000045 seeds the same row for an installation UPGRADING from 000044; it
	// cannot serve a FRESH stack, because migrations run before this bootstrap and there is no project
	// to reference yet. Two populations, two statements, one ON CONFLICT DO NOTHING each.
	poolID string
}

// systemTx runs body in one system-scoped transaction and commits it. Tenant creation must run under the
// system scope: no palai.project_id exists yet for the tenant being born, so the RLS policies
// would otherwise deny every insert. The commit is HERE rather than in each caller because a helper that
// returns before committing discards the write while every error check still reads as success.
func (s *Store) systemTx(ctx context.Context, body func(context.Context, pgx.Tx) error) error {
	ctx = storage.WithSystemScope(ctx)
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin provision: %w", err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	if err := body(ctx, tx); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit provision: %w", err)
	}
	return nil
}

// seedTenant inserts the four rows a tenant is born with: its project, a service principal, an admin API
// key (hash only — the bearer value never persists), and its default runner pool. Every statement is
// ON CONFLICT DO NOTHING, so a re-run against an already-seeded id is a clean no-op (the bootstrap re-boot
// path). Both creation paths — the bootstrap and POST /v1/projects — are now exactly these four rows.
func seedTenant(ctx context.Context, tx pgx.Tx, seed tenantSeed) error {
	if _, err := tx.Exec(ctx, storage.Query("InsertProject"), seed.projectID, seed.projectName); err != nil {
		return fmt.Errorf("insert project: %w", err)
	}
	if _, err := tx.Exec(ctx, storage.Query("InsertPrincipal"), seed.principalID, seed.projectID, "service"); err != nil {
		return fmt.Errorf("insert principal: %w", err)
	}
	if _, err := tx.Exec(ctx, storage.Query("InsertAPIKey"),
		seed.keyID, seed.projectID, seed.principalID, seed.keyHash, seed.scopes, seed.expiresAt); err != nil {
		return fmt.Errorf("insert api key: %w", err)
	}
	// The default runner pool. posture 'sandboxed-linux' and strict_enrollment false are today's
	// deployment stated as data: a new tenant behaves exactly as every tenant does now.
	if _, err := tx.Exec(ctx, storage.Query("InsertDefaultRunnerPool"),
		seed.poolID, seed.projectID); err != nil {
		return fmt.Errorf("insert default runner pool: %w", err)
	}
	return nil
}

// provisionTenant opens a tenant in one system-scoped transaction. It is what both the bootstrap and
// POST /v1/projects run; until A.2 Task 6 the bootstrap ran a longer path that also inserted an
// organization above these four rows.
func (s *Store) provisionTenant(ctx context.Context, seed tenantSeed) error {
	return s.systemTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		return seedTenant(ctx, tx, seed)
	})
}

// ProvisionFirstTenant seeds the single bootstrap tenant and its admin key from bootstrapKey, reusing
// the same tenant-creation path the API uses for every later tenant. Only the key's hash is stored.
// The caller (store.Bootstrap) guards this with an api_keys-empty check, so it runs once per fresh stack.
//
// THE SCOPES ARE NO LONGER EMPTY (Faz A.1 Task 3). Before this task an empty set was the whole story: this
// key is the ONLY key on a fresh self-hosted install, HasScope's empty-set rule made it unrestricted for
// every ordinary capability, and that was correct because there was only one tenant and one operator, the
// same person. Task 3 puts the runner-fleet routes behind `system` (router.go's systemOnly), and
// Scope.HasSystem() is DELIBERATELY excluded from the empty-set-is-unrestricted rule —
// middleware/auth_test.go's TestEmptyScopeIsUnrestrictedButNeverSystem pins that a tenant admin key must
// never inherit the platform capability — so an empty scope set can no longer reach the fleet at all, and
// `palai admin runner list` against a fresh bootstrap would 403.
//
// self-hosting is exactly the case Scope.HasSystem's own doc comment carves out as safe: "whoever runs
// their own installation is the operator" (this task's brief). So the fix is not to special-case empty
// scopes back into meaning `system` too — that would reopen the boundary Task 1/2 built, handing a HOSTED
// tenant admin key the platform capability along with it — it is to seed this ONE key with an EXPLICIT
// scope list instead of relying on the empty-set idiom, so it keeps exactly the access it had before
// (`provision`, `approve` — the only two ordinary capabilities anything in this tree gates on; see
// api/provisioning.go's provisionScope and api/approvals.go's approveScope) and gains `system` alongside
// them. Every OTHER tenant's admin key (CreateProject, below) keeps the empty set: only the operator's
// own key needs to be both a tenant admin AND the platform.
func (s *Store) ProvisionFirstTenant(ctx context.Context, bootstrapKey string) error {
	return s.provisionTenant(ctx, tenantSeed{
		projectID:   firstProject,
		principalID: firstPrincipal,
		keyID:       firstKey,
		keyHash:     coordinator.HashAPIKey(bootstrapKey),
		scopes:      []string{middleware.ScopeSystem, "provision", "approve"},
		// The fixed bootstrap pool id, in the same spirit as the four ids above: stable so a re-boot
		// against a retained volume is a no-op. It was once shared with a migration that seeded the same
		// id for an install upgrading into the fleet tables, so the two populations could not end up with
		// two different default pools; that seed went with the chain squash, and this is now the only
		// statement that writes it.
		poolID: fleet.DefaultPoolID,
	})
}

// provisioningScope binds a provisioning read/write to what the caller may administer. THE PLATFORM SEES
// THE INSTALLATION; A TENANT SEES ITS PROJECT — two answers because the callers are two different things,
// and the split is the same one authorizeSystem already draws at the route.
//
// It replaces orgScope, and the replacement is forced rather than chosen. Until A.2 Task 6 this widened to
// an organization-only scope — organization set, project empty — which worked because api_keys, principals and
// projects were the three tables 000062 deliberately left keyed on organization_id. Migration 000066 rekeys
// all three to project, and under that rule an empty project GUC matches only rows whose own project_id is
// ” — which no api_keys row ever has. Widening by leaving the project blank stops meaning "the whole
// organization" the moment the organization is not the key.
//
// SO THE WIDENING IS NOW EXPLICIT. A key holding `system` is the platform (the operator's own bootstrap
// key, or Palai Cloud's), and administering every project of the installation is exactly its job: it is
// what opens them. A key WITHOUT it is a tenant admin, and after this task a tenant IS a project — it
// lists, mints and revokes within its own, and another project's key is invisible rather than forbidden,
// which is the answer that discloses nothing.
func provisioningScope(ctx context.Context, scope middleware.Scope) context.Context {
	if scope.HasSystem() {
		return storage.WithSystemScope(ctx)
	}
	return storage.WithTenant(ctx, scope.Project)
}

// CreateProject OPENS A TENANT. Until A.2 Task 6 it inserted one projects row and nothing else — no
// principal, no key — so a project it created could not be used by anybody: the whole tenant (project +
// service principal + one-time admin key + default runner pool) was reachable ONLY through
// POST /v1/organizations, and this epic removes organizations. This is where CreateOrganization's
// remaining three rows moved, so the call that opens a customer and hands back its key still exists.
//
// The plaintext admin key is returned exactly once, in this response, and only its hash is stored — the
// same contract CreateOrganization and CreateAPIKey carry (apiKeyView.Key is omitempty and no read
// projection ever sets it).
//
// SYSTEM-SCOPED, like the organization creation it replaces and for the same reason, stated in
// provisioningHandler.authorizeSystem: opening a tenant is not an operation ON the caller's tenant, it is
// the creation of another one. A tenant admin key (empty scopes) therefore no longer reaches this route.
func (s *Store) CreateProject(ctx context.Context, scope middleware.Scope, body []byte) (api.ProvisionResult, error) {
	var in struct {
		DisplayName string `json:"display_name"`
	}
	if err := strictDecode(body, &in); err != nil {
		return api.ProvisionResult{BadField: true}, nil
	}
	seed := tenantSeed{
		projectID:   middleware.NewID("prj"),
		projectName: in.DisplayName,
		principalID: middleware.NewID("prin"),
		keyID:       middleware.NewID("key"),
		scopes:      []string{},
		poolID:      middleware.NewID("pool"),
	}
	secret := newSecret()
	seed.keyHash = coordinator.HashAPIKey(secret)
	if err := s.provisionTenant(ctx, seed); err != nil {
		return api.ProvisionResult{}, err
	}
	out := projectCreated{
		projectView: projectView{
			ID: seed.projectID, Object: "project",
			DisplayName: in.DisplayName, ConfigPolicy: json.RawMessage("null"),
		},
		AdminAPIKey: apiKeyView{
			ID: seed.keyID, Object: "api_key",
			ProjectID: seed.projectID, PrincipalID: seed.principalID, Scopes: seed.scopes, Key: secret,
		},
	}
	return api.ProvisionResult{Body: mustJSON(out)}, nil
}

// ListProjects lists every project the caller may administer: the installation's, for a `system` key;
// the caller's own, for a tenant admin (provisioningScope draws that line).
func (s *Store) ListProjects(ctx context.Context, scope middleware.Scope) (api.ProvisionResult, error) {
	ctx = provisioningScope(ctx, scope)
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

// GetProject reads one project within the caller's scope; a foreign/unknown id is a miss (404).
func (s *Store) GetProject(ctx context.Context, scope middleware.Scope, id string) (api.ProvisionResult, error) {
	ctx = provisioningScope(ctx, scope)
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
	ctx = provisioningScope(ctx, scope)
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

// CreateAPIKey mints a key (and its service principal) for a project within the caller's scope. The
// plaintext is returned exactly once; only its hash is stored. An absent project_id is a 400; a
// project outside the caller's scope is invisible under RLS and rendered a 404.
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
	ctx = provisioningScope(ctx, scope)
	if err := s.pool.QueryRow(ctx, storage.Query("ProjectExists"), in.ProjectID).Scan(new(int)); errors.Is(err, pgx.ErrNoRows) {
		return api.ProvisionResult{NotFound: true}, nil
	} else if err != nil {
		return api.ProvisionResult{}, fmt.Errorf("verify project: %w", err)
	}
	scopes := in.Scopes
	if scopes == nil {
		scopes = []string{}
	}
	// A KEY CANNOT MINT A KEY MORE CAPABLE THAN ITSELF. Every requested capability is checked against the
	// caller through middleware.Scope.CanGrant, which applies the SAME test that gates the capability —
	// literal possession for a never-inherited one, HasScope for the rest.
	//
	// This is checked HERE, on the write path, because the route's own gate is the wrong place to look for
	// it: the provisioning routes are gated on `provision`, and the capability being handed out is a
	// separate value in the BODY. Reading the gate tells you the caller may create a key; it says nothing
	// about what may go IN it. Every read-side use of the platform capability was already correct.
	for _, c := range scopes {
		if !scope.CanGrant(c) {
			return api.ProvisionResult{InsufficientScope: c}, nil
		}
	}
	principalID, keyID := middleware.NewID("prin"), middleware.NewID("key")
	secret := newSecret()

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return api.ProvisionResult{}, fmt.Errorf("begin create api key: %w", err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	if _, err := tx.Exec(ctx, storage.Query("InsertPrincipal"), principalID, in.ProjectID, "service"); err != nil {
		return api.ProvisionResult{}, fmt.Errorf("insert principal: %w", err)
	}
	if _, err := tx.Exec(ctx, storage.Query("InsertAPIKey"),
		keyID, in.ProjectID, principalID, coordinator.HashAPIKey(secret), scopes, in.ExpiresAt); err != nil {
		return api.ProvisionResult{}, fmt.Errorf("insert api key: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return api.ProvisionResult{}, fmt.Errorf("commit create api key: %w", err)
	}
	v := apiKeyView{
		ID: keyID, Object: "api_key", ProjectID: in.ProjectID,
		PrincipalID: principalID, Scopes: scopes, ExpiresAt: in.ExpiresAt, Key: secret,
	}
	return api.ProvisionResult{Body: mustJSON(v)}, nil
}

// ListAPIKeys lists key METADATA (never the hash or plaintext) for every key within the caller's scope.
func (s *Store) ListAPIKeys(ctx context.Context, scope middleware.Scope) (api.ProvisionResult, error) {
	ctx = provisioningScope(ctx, scope)
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

// GetAPIKey reads one key's metadata within the caller's scope; a foreign/unknown id is a miss (404).
func (s *Store) GetAPIKey(ctx context.Context, scope middleware.Scope, id string) (api.ProvisionResult, error) {
	ctx = provisioningScope(ctx, scope)
	v, err := scanAPIKey(s.pool.QueryRow(ctx, storage.Query("GetAPIKey"), id))
	if errors.Is(err, pgx.ErrNoRows) {
		return api.ProvisionResult{NotFound: true}, nil
	}
	if err != nil {
		return api.ProvisionResult{}, fmt.Errorf("get api key: %w", err)
	}
	return api.ProvisionResult{Body: mustJSON(v)}, nil
}

// RevokeAPIKey revokes a key within the caller's scope (idempotent — the first revoked_at is kept). A
// foreign/unknown id is a miss (404). The response renders the key's current metadata.
func (s *Store) RevokeAPIKey(ctx context.Context, scope middleware.Scope, id string) (api.ProvisionResult, error) {
	ctx = provisioningScope(ctx, scope)
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
//
// Approvers (E23 T2) is the project's approver allow-list. It is named HERE and not only in
// coordinator.ConfigPolicy because of what the strictness above means: a field this struct does not carry
// cannot be WRITTEN at all. The plan for this task recorded the write path as already shipped, which was
// half true — the endpoint ships, the field did not, so `{"config_policy":{"approvers":[…]}}` was a 400
// and the list had no way in. A control nobody can configure is not a control.
//
// POOL (E24 T2) IS THE SAME FINDING A SECOND TIME. The E24 plan recorded the pool policy's write path
// as shipped, citing this endpoint — and it was half true in exactly the same way: DisallowUnknownFields
// made `{"config_policy":{"pool":"mac-pool"}}` a 400, so the placement policy the epic rests on had no
// way in. It is `omitempty` here alone among these fields, so a policy that names no pool marshals to
// the bytes it always did and no existing project's stored JSON changes shape.
type configPolicyInput struct {
	AllowedModels []string `json:"allowed_models"`
	AllowedTools  []string `json:"allowed_tools"`
	DefaultTools  []string `json:"default_tools"`
	Approvers     []string `json:"approvers"`
	Pool          string   `json:"pool,omitempty"`
}

// projectCreated is the create-only projection: the project plus the admin key whose plaintext this
// response is the sole place to read. Reads render projectView, which has no key field at all.
type projectCreated struct {
	projectView
	AdminAPIKey apiKeyView `json:"admin_api_key"`
}

type projectView struct {
	ID           string          `json:"id"`
	Object       string          `json:"object"`
	DisplayName  string          `json:"display_name"`
	ConfigPolicy json.RawMessage `json:"config_policy"`
	CreatedAt    *time.Time      `json:"created_at,omitempty"`
}

// apiKeyView renders a key. Key (the plaintext) carries omitempty and is set ONLY on a create response —
// every read leaves it empty, so a listing or retrieval can never disclose a secret. key_hash is never a
// field at all.
type apiKeyView struct {
	ID          string     `json:"id"`
	Object      string     `json:"object"`
	ProjectID   string     `json:"project_id"`
	PrincipalID string     `json:"principal_id"`
	Scopes      []string   `json:"scopes"`
	ExpiresAt   *time.Time `json:"expires_at,omitempty"`
	CreatedAt   *time.Time `json:"created_at,omitempty"`
	RevokedAt   *time.Time `json:"revoked_at,omitempty"`
	Key         string     `json:"key,omitempty"`
}

type listView struct {
	Object string `json:"object"`
	Data   any    `json:"data"`
}

// scanner is the row shape shared by pool.QueryRow and rows.
type scanner interface {
	Scan(dest ...any) error
}

func scanProject(row scanner) (projectView, error) {
	v := projectView{Object: "project"}
	var policy []byte
	var createdAt time.Time
	if err := row.Scan(&v.ID, &v.DisplayName, &policy, &createdAt); err != nil {
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
	if err := row.Scan(&v.ID, &v.ProjectID, &v.PrincipalID, &v.Scopes, &v.ExpiresAt, &createdAt, &v.RevokedAt); err != nil {
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
