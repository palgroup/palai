// This file adds the secret-ref store to the identity package (E13 Task 3, SEC-002/MCI-002): the durable,
// envelope-encrypted secret store behind the restart-less secret write-path. It lives beside the tenancy
// provisioning Store because it shares that surface's scoping discipline (orgScope), strict decode, and
// metadata-only projection helpers — a secret-ref is another org-admin tenant resource. Its Resolve method
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
	"github.com/palgroup/palai/storage"
)

// ErrSecretDecrypt marks a stored secret that EXISTS but could not be decrypted (wrong master key or
// corruption) — distinct from a genuine miss (no row). It is errors.Is-able so the resolver chain fails
// CLOSED on it: falling back to a superseded env-file secret when a rotated DB secret cannot be decrypted
// would silently defeat the rotation (the SEC-002 failure). A miss returns ok=false with a nil error.
var ErrSecretDecrypt = errors.New("secret ref exists but could not be decrypted")

// SecretStore envelope-encrypts each secret value at rest and resolves the latest version at request time.
// It shares one pool with the provisioning Store; each method scopes itself to the caller's org, so RLS
// (migration 000031) isolates one tenant's secrets from another's.
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

// putVersion seals the value and inserts the next version under the caller's org (RLS-scoped). The version
// is computed and inserted in one transaction; the UNIQUE(organization_id, name, version) constraint is the
// backstop against a concurrent insert of the same version. requireExisting turns a rotate of a never-created
// name into a NotFound.
func (s *SecretStore) putVersion(ctx context.Context, scope middleware.Scope, name, value string, requireExisting bool) (api.ProvisionResult, error) {
	sealed, err := s.seal([]byte(value))
	if err != nil {
		return api.ProvisionResult{}, err
	}
	ctx = orgScope(ctx, scope)
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return api.ProvisionResult{}, fmt.Errorf("begin put secret: %w", err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()

	var version int
	if err := tx.QueryRow(ctx, storage.Query("NextSecretVersion"), name).Scan(&version); err != nil {
		return api.ProvisionResult{}, fmt.Errorf("next secret version: %w", err)
	}
	if requireExisting && version == 1 {
		return api.ProvisionResult{NotFound: true}, nil
	}
	var createdAt time.Time
	if err := tx.QueryRow(ctx, storage.Query("InsertSecretRef"),
		middleware.NewID("sec"), scope.Organization, name, version, sealed).Scan(&createdAt); err != nil {
		return api.ProvisionResult{}, fmt.Errorf("insert secret ref: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return api.ProvisionResult{}, fmt.Errorf("commit put secret: %w", err)
	}
	return api.ProvisionResult{Body: mustJSON(secretRefView{Name: name, Object: "secret_ref", Version: version, UpdatedAt: &createdAt})}, nil
}

// ListSecretRefs lists secret-ref METADATA (name/version/updated_at) for the caller's organization — never a
// value or ciphertext. One row per name, at its latest version.
func (s *SecretStore) ListSecretRefs(ctx context.Context, scope middleware.Scope) (api.ProvisionResult, error) {
	ctx = orgScope(ctx, scope)
	rows, err := s.pool.Query(ctx, storage.Query("ListSecretRefs"))
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

// GetSecretRef reads one secret's metadata within the caller's organization; a foreign/unknown name is a
// miss (404).
func (s *SecretStore) GetSecretRef(ctx context.Context, scope middleware.Scope, name string) (api.ProvisionResult, error) {
	ctx = orgScope(ctx, scope)
	v, err := scanSecretRef(s.pool.QueryRow(ctx, storage.Query("GetSecretRef"), name))
	if errors.Is(err, pgx.ErrNoRows) {
		return api.ProvisionResult{NotFound: true}, nil
	}
	if err != nil {
		return api.ProvisionResult{}, fmt.Errorf("get secret ref: %w", err)
	}
	return api.ProvisionResult{Body: mustJSON(v)}, nil
}

// Resolve returns the decrypted latest value for (org, name), or ok=false when no such secret exists in that
// org. It is the DB-backed hook main.go puts in FRONT of the env-file bridge: ok=false means "fall back to
// the env bridge". The org is server-minted (from the connection/run, never a tenant-forgeable body), so the
// read is scoped to that org and RLS denies any foreign row — a shared ref name never crosses tenants.
func (s *SecretStore) Resolve(ctx context.Context, org, name string) ([]byte, bool, error) {
	if org == "" || name == "" {
		return nil, false, nil
	}
	ctx = storage.WithTenant(ctx, org, "")
	var ciphertext []byte
	err := s.pool.QueryRow(ctx, storage.Query("ResolveSecretRef"), name).Scan(&ciphertext)
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
