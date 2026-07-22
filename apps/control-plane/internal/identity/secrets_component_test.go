//go:build component

// Package identity_test also holds the real-PostgreSQL component tests for the secret-ref store (E13
// Task 3, SEC-002/MCI-002). They run only under `make test-component TEST=postgres`; the build tag keeps
// them out of the credential-free unit tier. The store envelope-encrypts each value at rest (single
// master-key AES-256-GCM), so these prove the value round-trips through the resolver but never appears in
// a read projection, and that a rotation is visible to the next Resolve with no process restart.
package identity_test

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"strings"
	"testing"

	"github.com/palgroup/palai/apps/control-plane/api/middleware"
	"github.com/palgroup/palai/apps/control-plane/internal/identity"
	"github.com/palgroup/palai/storage"
)

// masterKey mints a random 32-byte AES-256 master key (hex), the shape main.go reads from
// PALAI_SECRET_MASTER_KEY_FILE.
func masterKey(t *testing.T) []byte {
	t.Helper()
	var raw [32]byte
	if _, err := rand.Read(raw[:]); err != nil {
		t.Fatalf("mint master key: %v", err)
	}
	key, err := identity.ParseMasterKey(hex.EncodeToString(raw[:]))
	if err != nil {
		t.Fatalf("ParseMasterKey: %v", err)
	}
	return key
}

// TestSecretRefWriteResolveRotate is the heart of SEC-002/MCI-002: a value written through the API path is
// returned by Resolve (decrypted), a rotation inserts a new version the very next Resolve reads with NO
// restart, and neither the create/list/get projections nor the DB carry the plaintext.
func TestSecretRefWriteResolveRotate(t *testing.T) {
	cs := openHarness(t)
	ctx := context.Background()
	idstore := identity.New(cs.Pool())
	store := identity.NewSecretStore(cs.Pool(), masterKey(t))

	org, _, _ := provisionOrg(t, idstore, "sec-alpha")
	scope := middleware.Scope{Organization: org}

	created, err := store.CreateSecretRef(ctx, scope, []byte(`{"name":"provider-one","value":"sk-live-v1"}`))
	if err != nil {
		t.Fatalf("CreateSecretRef error = %v", err)
	}
	if strings.Contains(string(created.Body), "sk-live-v1") || strings.Contains(string(created.Body), `"value"`) {
		t.Fatalf("create projection disclosed the value: %s", created.Body)
	}

	// Resolve is the resolver-chain hook main.go puts in front of the env-file bridge. It decrypts.
	got, ok, err := store.Resolve(ctx, org, "provider-one")
	if err != nil || !ok {
		t.Fatalf("Resolve(v1) ok=%v err=%v", ok, err)
	}
	if string(got) != "sk-live-v1" {
		t.Fatalf("Resolve(v1) = %q, want sk-live-v1", got)
	}

	// Rotation inserts a new version; the next Resolve sees it with no restart (SEC-002).
	rotated, err := store.RotateSecretRef(ctx, scope, "provider-one", []byte(`{"value":"sk-live-v2"}`))
	if err != nil {
		t.Fatalf("RotateSecretRef error = %v", err)
	}
	if strings.Contains(string(rotated.Body), "sk-live-v2") {
		t.Fatalf("rotate projection disclosed the value: %s", rotated.Body)
	}
	got2, ok, err := store.Resolve(ctx, org, "provider-one")
	if err != nil || !ok {
		t.Fatalf("Resolve(v2) ok=%v err=%v", ok, err)
	}
	if string(got2) != "sk-live-v2" {
		t.Fatalf("Resolve after rotate = %q, want sk-live-v2 (rotation not visible without restart)", got2)
	}

	// The list/get metadata projections carry name/version/updated_at and NEVER the value.
	list, err := store.ListSecretRefs(ctx, scope)
	if err != nil {
		t.Fatalf("ListSecretRefs error = %v", err)
	}
	body := string(list.Body)
	if strings.Contains(body, "sk-live") || strings.Contains(body, `"value"`) || strings.Contains(body, "ciphertext") {
		t.Fatalf("list projection disclosed a secret: %s", body)
	}
	if !strings.Contains(body, `"version":2`) {
		t.Fatalf("list metadata missing the rotated version: %s", body)
	}

	// The plaintext is nowhere in the row: the stored ciphertext bytes must not contain it. The read is
	// tenant-scoped — an unscoped context sees zero rows under RLS (migration 000031), which is itself the
	// isolation guarantee, so scope to the org to actually inspect the stored bytes.
	var cipher []byte
	if err := cs.Pool().QueryRow(storage.WithTenant(ctx, org, ""),
		"SELECT ciphertext FROM secret_refs WHERE organization_id = $1 AND name = $2 ORDER BY version DESC LIMIT 1",
		org, "provider-one").Scan(&cipher); err != nil {
		t.Fatalf("read stored ciphertext: %v", err)
	}
	if strings.Contains(string(cipher), "sk-live-v2") {
		t.Fatalf("the value is stored in plaintext at rest")
	}
}

// TestSecretRefCrossOrgResolveDenied proves a resolver call scoped to org A can never read org B's secret:
// the store scopes Resolve to the named org, and migration 000031's RLS policy denies the foreign row, so a
// foreign ref resolves to a clean miss (ok=false) rather than another tenant's bytes.
func TestSecretRefCrossOrgResolveDenied(t *testing.T) {
	cs := openHarness(t)
	ctx := context.Background()
	idstore := identity.New(cs.Pool())
	store := identity.NewSecretStore(cs.Pool(), masterKey(t))

	aOrg, _, _ := provisionOrg(t, idstore, "sec-b-a")
	bOrg, _, _ := provisionOrg(t, idstore, "sec-b-b")
	if _, err := store.CreateSecretRef(ctx, middleware.Scope{Organization: bOrg}, []byte(`{"name":"shared-name","value":"sk-b-only"}`)); err != nil {
		t.Fatalf("CreateSecretRef(b) error = %v", err)
	}

	// A resolves the SAME ref name; RLS isolates B's row, so A gets a miss, not B's bytes.
	if got, ok, err := store.Resolve(ctx, aOrg, "shared-name"); err != nil {
		t.Fatalf("Resolve(a) error = %v", err)
	} else if ok {
		t.Fatalf("org A resolved org B's secret (%q) — RLS did not isolate", got)
	}
}

// TestSecretRefRotateUnknownIsNotFound proves rotate of a name with no prior version is a 404 (a rotation
// implies an existing secret), while create of a fresh name succeeds at version 1.
func TestSecretRefRotateUnknownIsNotFound(t *testing.T) {
	cs := openHarness(t)
	ctx := context.Background()
	idstore := identity.New(cs.Pool())
	store := identity.NewSecretStore(cs.Pool(), masterKey(t))

	org, _, _ := provisionOrg(t, idstore, "sec-gamma")
	scope := middleware.Scope{Organization: org}

	if r, _ := store.RotateSecretRef(ctx, scope, "never-created", []byte(`{"value":"x"}`)); !r.NotFound {
		t.Fatal("rotate of an unknown secret was not a NotFound")
	}
	if r, _ := store.CreateSecretRef(ctx, scope, []byte(`{"name":"fresh","value":"x"}`)); r.NotFound || r.BadField || r.MissingField != "" {
		t.Fatalf("create of a fresh secret was rejected: %+v", r)
	}
}

// TestSecretRefStrictDecode proves the write-path uses the E11 T1 strict decode: an unknown field is a
// typed reject (400), and a create with no value is a missing-field 400.
func TestSecretRefStrictDecode(t *testing.T) {
	cs := openHarness(t)
	ctx := context.Background()
	idstore := identity.New(cs.Pool())
	store := identity.NewSecretStore(cs.Pool(), masterKey(t))

	org, _, _ := provisionOrg(t, idstore, "sec-delta")
	scope := middleware.Scope{Organization: org}

	if r, _ := store.CreateSecretRef(ctx, scope, []byte(`{"name":"x","value":"y","nope":1}`)); !r.BadField {
		t.Fatal("create with an unknown field was not rejected")
	}
	if r, _ := store.CreateSecretRef(ctx, scope, []byte(`{"name":"x"}`)); r.MissingField != "value" {
		t.Fatalf("create with no value MissingField = %q, want value", r.MissingField)
	}
	if r, _ := store.CreateSecretRef(ctx, scope, []byte(`{"value":"y"}`)); r.MissingField != "name" {
		t.Fatalf("create with no name MissingField = %q, want name", r.MissingField)
	}
}
