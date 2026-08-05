// This file adds the secret-ref store to the identity package (E13 Task 3, SEC-002/MCI-002): the durable,
// envelope-encrypted secret store behind the restart-less secret write-path. It lives beside the tenancy
// provisioning Store because it shares that surface's scoping discipline (provisioningScope), strict
// decode, and metadata-only projection helpers — a secret-ref is another tenant-admin resource. Its Resolve
// is the DB-backed hook main.go puts IN FRONT of the env-file secret bridge: a hit returns the decrypted
// value, a miss falls through to the env bridge, so the E09 credential-broker seam is preserved.
//
// HONEST CEILING: the value is sealed at rest under ONE master-key AES-256-GCM envelope held by the process
// (PALAI_SECRET_MASTER_KEY_FILE). There is no KMS backend, no per-secret data key, and no one-operation
// audience/fence lease ceremony — those are E13-H (SEC-001/003). The value has NO read-back path: every API
// projection is metadata only (name/version/updated_at), and no error message or log carries it.
package identity

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/palgroup/palai/apps/control-plane/api"
	"github.com/palgroup/palai/apps/control-plane/api/middleware"
	"github.com/palgroup/palai/packages/coordinator"
	"github.com/palgroup/palai/storage"
)

// ErrSecretDecrypt marks a stored secret that EXISTS but could not be decrypted (wrong master key or
// corruption) — distinct from a genuine miss (no row). It is errors.Is-able so the resolver chain fails
// CLOSED on it: falling back to a superseded env-file secret when a rotated DB secret cannot be decrypted
// would silently defeat the rotation (the SEC-002 failure). A miss returns ok=false with a nil error.
var ErrSecretDecrypt = errors.New("secret ref exists but could not be decrypted")

// SecretStore envelope-encrypts each secret value at rest and resolves the effective version at request
// time. It shares one pool with the provisioning Store; every method scopes itself to the caller's PROJECT,
// so the row-level-security policy 000006 installed isolates one tenant's secrets from another's.
//
// THAT SENTENCE WAS TRUE, THEN FALSE FOR A WHILE, AND IS TRUE AGAIN — worth the three lines because the
// middle state is what a reader of any surrounding comment written in it will have absorbed. It named
// `organization_id` until A.2 dropped that column; A.2 added no project_id here, so 000002 could only give
// secret_refs `palai_apply_installation_policy` and every project in the installation resolved the same
// row. 000006 adds project_id, and this store is the surface that fills it.
type SecretStore struct {
	pool *pgxpool.Pool
	aead cipher.AEAD
}

// ParseMasterKey decodes the 32-byte AES-256 master key from its hex encoding (64 hex chars) — the shape
// main.go reads from PALAI_SECRET_MASTER_KEY_FILE. Exported so the composition root and the component tests
// validate the key the same way. An invalid key is a startup error, never a silent weakening.
func ParseMasterKey(encoded string) ([]byte, error) {
	key, err := hex.DecodeString(strings.TrimSpace(encoded))
	if err != nil {
		return nil, fmt.Errorf("secret master key: not valid hex: %w", err)
	}
	if len(key) != 32 {
		return nil, fmt.Errorf("secret master key: got %d bytes, want 32 (AES-256)", len(key))
	}
	return key, nil
}

// NewSecretStore builds the store over the durable spine's pool with a validated 32-byte AES-256 master key
// (ParseMasterKey). The AEAD construction only fails on a wrong key length, which ParseMasterKey already
// rejected, so a failure here is a programmer error, not an operational one.
func NewSecretStore(pool *pgxpool.Pool, masterKey []byte) *SecretStore {
	block, err := aes.NewCipher(masterKey)
	if err != nil {
		panic(fmt.Sprintf("identity: build secret cipher: %v", err))
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		panic(fmt.Sprintf("identity: build secret GCM: %v", err))
	}
	return &SecretStore{pool: pool, aead: aead}
}

// CreateSecretRef writes the first (or a further) version of a named secret. The value is write-only: it is
// sealed and stored, and the response carries metadata only. A repeat name is accepted and versioned (the
// explicit rotate route exists for the same effect against a known name).
func (s *SecretStore) CreateSecretRef(ctx context.Context, scope middleware.Scope, body []byte) (api.ProvisionResult, error) {
	var in struct {
		Name  string `json:"name"`
		Value string `json:"value"`
	}
	if err := strictDecode(body, &in); err != nil {
		return api.ProvisionResult{BadField: true}, nil
	}
	if in.Name == "" {
		return api.ProvisionResult{MissingField: "name"}, nil
	}
	if in.Value == "" {
		return api.ProvisionResult{MissingField: "value"}, nil
	}
	return s.putVersion(ctx, scope, in.Name, in.Value, false)
}

// RotateSecretRef inserts a new version of an EXISTING secret (name from the path, value from the body). A
// name with no prior version is a NotFound — a rotation implies an existing secret. The next Resolve reads
// the new version with no restart (SEC-002).
func (s *SecretStore) RotateSecretRef(ctx context.Context, scope middleware.Scope, name string, body []byte) (api.ProvisionResult, error) {
	var in struct {
		Value string `json:"value"`
	}
	if err := strictDecode(body, &in); err != nil {
		return api.ProvisionResult{BadField: true}, nil
	}
	if in.Value == "" {
		return api.ProvisionResult{MissingField: "value"}, nil
	}
	return s.putVersion(ctx, scope, name, in.Value, true)
}

// putVersion seals the value and inserts the next version. The version is computed and inserted in one
// transaction; the `UNIQUE (project_id, name, version)` constraint 000006 installed (still NAMED
// secret_refs_name_version_key — see that file for why the baseline's name is kept) is the backstop
// against a concurrent insert of the same version. A secret ref name is unique WITHIN A PROJECT: two
// customers may each hold `github-token`, which the installation-wide `UNIQUE (name, version)` this
// replaced made impossible. requireExisting turns a rotate of a never-created name into a NotFound — and
// since 000006 that includes a name owned by ANOTHER project, because NextSecretVersion does not see it.
//
// THE EMPTY-PROJECT REFUSAL IS THE WRITE HALF OF 000006 AND IT IS NOT DECORATION. That migration leaves
// existing rows with an EMPTY project_id, which its policy still admits from every scope — a deliberate record
// of the past. A DEFAULT is a value a writer inherits by saying nothing, so without this check the first
// new secret written by a key that names no project would land in that same installation-wide bucket and
// the boundary would be undone by its own compatibility provision. provisioningScope hands a `system` key
// a system scope, under which every policy admits, so the credential is the only thing standing between
// such a key and an empty-project write; there is no database constraint that could catch it.
//
// It is 403 rather than 400 because the request is well-formed — what is missing is in the CREDENTIAL, and
// a 400 would send an operator looking at their request body for a field that does not exist.
// identity.TestANewSecretCannotBeWrittenWithoutAProject is the guard.
func (s *SecretStore) putVersion(ctx context.Context, scope middleware.Scope, name, value string, requireExisting bool) (api.ProvisionResult, error) {
	if scope.Project == "" {
		return api.ProvisionResult{InsufficientScope: "project"}, nil
	}
	ctx = provisioningScope(ctx, scope)
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return api.ProvisionResult{}, fmt.Errorf("begin put secret: %w", err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()

	version, createdAt, err := s.insertVersion(ctx, tx, scope.Project, name, value, requireExisting)
	if err != nil {
		return api.ProvisionResult{}, err
	}
	if version == 0 {
		return api.ProvisionResult{NotFound: true}, nil // rotate of a never-created name
	}
	if err := tx.Commit(ctx); err != nil {
		return api.ProvisionResult{}, fmt.Errorf("commit put secret: %w", err)
	}
	return api.ProvisionResult{Body: mustJSON(secretRefView{Name: name, Object: "secret_ref", Version: version, UpdatedAt: &createdAt})}, nil
}

// insertVersion seals value and appends the next version of name INSIDE the caller's transaction. It is
// the ONE place a plaintext becomes a sealed row, and it is extracted from putVersion for exactly one
// caller beyond it: an environment value write, which has to land its membership row and its secret
// version atomically (E25 T3). Extracting it is what keeps the environment feature from opening a second
// encryption path — the alternative was a second seal + a second INSERT, which is how a tree ends up with
// two envelope formats.
//
// IT DOES NOT COMMIT, AND IT DOES NOT ROLL BACK. The transaction belongs to the caller, which is the only
// party that knows whether the rest of its work succeeded. (This tree has shipped a helper called under a
// `defer tx.Rollback` whose caller `return`ed the helper's result without committing, twice in one
// function, with comments claiming otherwise — hence the sentence.)
//
// version 0 with a nil error is the "rotate of a name that has no prior version" answer, which the caller
// renders as a 404; every other failure is an error.
//
// project is the OWNER the version is written under (000006). It is threaded explicitly rather than read
// back out of ctx because both callers already hold it and a context value would make the owner ambient —
// the property this whole cut-over exists to remove. Both callers reject an empty project before reaching
// here, so this function does not re-check it; what it must never do is DEFAULT one.
func (s *SecretStore) insertVersion(ctx context.Context, tx pgx.Tx, project, name, value string, requireExisting bool) (int, time.Time, error) {
	sealed, err := s.seal([]byte(value))
	if err != nil {
		return 0, time.Time{}, err
	}
	var version int
	if err := tx.QueryRow(ctx, storage.Query("NextSecretVersion"), project, name).Scan(&version); err != nil {
		return 0, time.Time{}, fmt.Errorf("next secret version: %w", err)
	}
	if requireExisting && version == 1 {
		return 0, time.Time{}, nil
	}
	var createdAt time.Time
	if err := tx.QueryRow(ctx, storage.Query("InsertSecretRef"),
		middleware.NewID("sec"), project, name, version, sealed).Scan(&createdAt); err != nil {
		return 0, time.Time{}, fmt.Errorf("insert secret ref: %w", err)
	}
	return version, createdAt, nil
}

// ListSecretRefs lists secret-ref METADATA (name/version/updated_at) — never a value or ciphertext. One
// row per name, at its EFFECTIVE version.
//
// THE SET IS THE CALLER'S PROJECT PLUS THE PRE-000006 ROWS, and both halves are real rather than one being
// a rounding of the other. A secret another project owns is not in this list — that narrowing was absent
// between A.2 and 000006, and this comment claimed a wider set correctly for that whole interval. A secret
// written before 000006 carries `project_id = ”` and IS in the list, for every project, because 000006
// assigns those rows to nobody; that residue is the honest ceiling, not an oversight here. The statement's
// precedence makes a name held in both scopes appear once, at the project's own version, so this list
// cannot disagree with Resolve about which secret a name means.
func (s *SecretStore) ListSecretRefs(ctx context.Context, scope middleware.Scope) (api.ProvisionResult, error) {
	ctx = provisioningScope(ctx, scope)
	rows, err := s.pool.Query(ctx, storage.Query("ListSecretRefs"), scope.Project)
	if err != nil {
		return api.ProvisionResult{}, fmt.Errorf("list secret refs: %w", err)
	}
	defer rows.Close()
	data := []secretRefView{}
	for rows.Next() {
		v, err := scanSecretRef(rows)
		if err != nil {
			return api.ProvisionResult{}, err
		}
		data = append(data, v)
	}
	if err := rows.Err(); err != nil {
		return api.ProvisionResult{}, fmt.Errorf("iterate secret refs: %w", err)
	}
	return api.ProvisionResult{Body: mustJSON(listView{Object: "list", Data: data})}, nil
}

// GetSecretRef reads one secret's metadata. Since 000006 it produces TWO 404s and renders them
// identically: a name this installation does not hold, and a name ANOTHER project owns. Conflating them is
// the intended answer — distinguishing them would tell a caller that a name exists somewhere it cannot
// reach. Between A.2 and 000006 only the first of the two existed, and this comment said so.
func (s *SecretStore) GetSecretRef(ctx context.Context, scope middleware.Scope, name string) (api.ProvisionResult, error) {
	ctx = provisioningScope(ctx, scope)
	v, err := scanSecretRef(s.pool.QueryRow(ctx, storage.Query("GetSecretRef"), scope.Project, name))
	if errors.Is(err, pgx.ErrNoRows) {
		return api.ProvisionResult{NotFound: true}, nil
	}
	if err != nil {
		return api.ProvisionResult{}, fmt.Errorf("get secret ref: %w", err)
	}
	return api.ProvisionResult{Body: mustJSON(v)}, nil
}

// Resolve returns the decrypted effective value of name FOR ONE TENANT, or ok=false when that tenant has
// no such secret. It is the DB-backed hook main.go puts in FRONT of the env-file bridge: ok=false means
// "fall back to the env bridge".
//
// THE TENANT ARGUMENT IS THE POINT OF THIS METHOD AND IT HAS BEEN REMOVED ONCE ALREADY, so the history is
// here rather than in a commit message. It used to take an `org`; A.2 Task 6 deleted it, correctly, because
// by then the column it selected on was gone and "a parameter that selects nothing is a boundary the
// signature keeps claiming". What that left was worse than an inert parameter: the method ran under
// storage.WithInstallationScope, so EVERY project in the installation resolved the same row, and a caller
// could not have named a tenant even if it wanted to. 000006 gives secret_refs a project_id, and this is
// the read that spends it.
//
// WHAT IT REFUSES, IN THREE BRANCHES, because "project-scoped" alone would be a claim a reader cannot check:
//   - a name no scope holds — ok=false, the clean miss the resolver chain treats as "try the env bridge".
//   - a name ANOTHER project owns — also ok=false, and this branch is the boundary 000006 restored. It is
//     indistinguishable from the first by design.
//   - a name written BEFORE 000006 (an EMPTY project_id) — RESOLVED, for every tenant. Those rows are
//     assigned to nobody, so this is the pre-000006 behaviour deliberately preserved; applying the
//     migration must not stop a running installation's existing secrets from resolving. It is the residual
//     leak, it is not closed here, and ResolveSecretRef's ORDER BY makes the tenant's OWN row win whenever
//     both exist.
//
// AN EMPTY PROJECT IS REFUSED OUTRIGHT rather than widened. storage.WithTenant would publish an empty
// palai.project_id, which the policy reads as a row whose own project_id is EMPTY — that is not "every
// project", but it IS exactly the legacy bucket, so an unscoped caller would silently acquire the reach of
// every pre-000006 secret in the installation. Refusing costs a caller nothing that is not a bug.
func (s *SecretStore) Resolve(ctx context.Context, tenant coordinator.Tenant, name string) ([]byte, bool, error) {
	if name == "" {
		return nil, false, nil
	}
	if tenant.Project == "" {
		return nil, false, errors.New("identity: resolve secret ref: no project — a credential is always resolved on behalf of someone")
	}
	ctx = storage.WithTenant(ctx, tenant.Project)
	var ciphertext []byte
	err := s.pool.QueryRow(ctx, storage.Query("ResolveSecretRef"), tenant.Project, name).Scan(&ciphertext)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, fmt.Errorf("resolve secret ref %q: %w", name, err)
	}
	value, err := s.open(ciphertext)
	if err != nil {
		// The name is safe to name in an error; the value never is. Tag with ErrSecretDecrypt so the caller
		// fails closed rather than serving a superseded env-file secret (SEC-002).
		return nil, false, fmt.Errorf("decrypt secret ref %q: %w", name, ErrSecretDecrypt)
	}
	return value, true, nil
}

// seal AES-256-GCM-encrypts a value with a fresh random nonce, returning nonce || sealed. The nonce is
// unique per call (crypto/rand), so GCM's nonce-reuse hazard does not arise.
func (s *SecretStore) seal(plaintext []byte) ([]byte, error) {
	nonce := make([]byte, s.aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, fmt.Errorf("mint secret nonce: %w", err)
	}
	return s.aead.Seal(nonce, nonce, plaintext, nil), nil
}

// open reverses seal: it splits the nonce prefix off and AES-256-GCM-decrypts, authenticating the tag.
func (s *SecretStore) open(sealed []byte) ([]byte, error) {
	n := s.aead.NonceSize()
	if len(sealed) < n {
		return nil, errors.New("secret ciphertext shorter than the nonce")
	}
	return s.aead.Open(nil, sealed[:n], sealed[n:], nil)
}

// secretRefView is a secret-ref METADATA projection. It carries name/version/updated_at and — the
// load-bearing invariant — NEVER a value or ciphertext field, so no read path can disclose a secret.
type secretRefView struct {
	Name      string     `json:"name"`
	Object    string     `json:"object"`
	Version   int        `json:"version"`
	UpdatedAt *time.Time `json:"updated_at,omitempty"`
}

func scanSecretRef(row scanner) (secretRefView, error) {
	v := secretRefView{Object: "secret_ref"}
	var updatedAt time.Time
	if err := row.Scan(&v.Name, &v.Version, &updatedAt); err != nil {
		return secretRefView{}, err
	}
	v.UpdatedAt = &updatedAt
	return v, nil
}
