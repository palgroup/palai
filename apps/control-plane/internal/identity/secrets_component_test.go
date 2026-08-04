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
	"errors"
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

	org, project, _ := provisionOrg(t, idstore, "sec-alpha")
	scope := middleware.Scope{Project: project}

	// Per-run, because 000065 made the ref name unique across the INSTALLATION: a literal shared with any
	// sibling fixture makes this test's "version 2" assertion depend on how many of them ran first.
	name := "provider-one-" + newID("sec")
	created, err := store.CreateSecretRef(ctx, scope, []byte(`{"name":"`+name+`","value":"sk-live-v1"}`))
	if err != nil {
		t.Fatalf("CreateSecretRef error = %v", err)
	}
	if strings.Contains(string(created.Body), "sk-live-v1") || strings.Contains(string(created.Body), `"value"`) {
		t.Fatalf("create projection disclosed the value: %s", created.Body)
	}

	// Resolve is the resolver-chain hook main.go puts in front of the env-file bridge. It decrypts.
	got, ok, err := store.Resolve(ctx, org, name)
	if err != nil || !ok {
		t.Fatalf("Resolve(v1) ok=%v err=%v", ok, err)
	}
	if string(got) != "sk-live-v1" {
		t.Fatalf("Resolve(v1) = %q, want sk-live-v1", got)
	}

	// Rotation inserts a new version; the next Resolve sees it with no restart (SEC-002).
	rotated, err := store.RotateSecretRef(ctx, scope, name, []byte(`{"value":"sk-live-v2"}`))
	if err != nil {
		t.Fatalf("RotateSecretRef error = %v", err)
	}
	if strings.Contains(string(rotated.Body), "sk-live-v2") {
		t.Fatalf("rotate projection disclosed the value: %s", rotated.Body)
	}
	got2, ok, err := store.Resolve(ctx, org, name)
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
	// secret_refs carries no project_id (000031); WithOrgScope is the named exception WithTenant no
	// longer allows for an empty project (A.2 Task 1).
	if err := cs.Pool().QueryRow(storage.WithOrgScope(ctx, org),
		"SELECT ciphertext FROM secret_refs WHERE organization_id = $1 AND name = $2 ORDER BY version DESC LIMIT 1",
		org, name).Scan(&cipher); err != nil {
		t.Fatalf("read stored ciphertext: %v", err)
	}
	if strings.Contains(string(cipher), "sk-live-v2") {
		t.Fatalf("the value is stored in plaintext at rest")
	}
}

// TestSecretRefNamesAreInstallationWide REPLACES TestSecretRefCrossOrgResolveDenied, and the replacement is
// the point rather than a rename. That test proved a resolver scoped to org A could never read org B's
// secret of the same name. A.2 Task 6 removes organizations, and secret_refs carries no project_id at all
// (000031) — so there is nothing narrower than the installation left for its policy to key on, and
// migration 000066 keys it there. The boundary is GONE, not moved, and this test exists so that fact is
// asserted out loud instead of discovered by whoever needs it.
//
// It is written as the strongest true statement, not the weakest: a second tenant writing the same NAME
// does not get its own secret and does not get an error — it appends the next VERSION of the one secret,
// and every resolver in the installation then reads the newer value. Re-introducing a boundary here means
// deliberately changing this test, which is exactly the amount of friction that change deserves.
func TestSecretRefNamesAreInstallationWide(t *testing.T) {
	cs := openHarness(t)
	ctx := context.Background()
	idstore := identity.New(cs.Pool())
	store := identity.NewSecretStore(cs.Pool(), masterKey(t))

	_, aProj, _ := provisionOrg(t, idstore, "sec-b-a")
	aOrg, err := storage.OrganizationForProject(ctx, cs.Pool(), aProj)
	if err != nil {
		t.Fatalf("resolve org A: %v", err)
	}
	_, bProj, _ := provisionOrg(t, idstore, "sec-b-b")
	// Per-run, for the reason the test is about: the name is unique across the INSTALLATION, so a literal
	// would pass once against a retained database and then be somebody's version 2 forever after.
	sharedName := "shared-name-" + newID("sec")
	if _, err := store.CreateSecretRef(ctx, middleware.Scope{Project: bProj}, []byte(`{"name":"`+sharedName+`","value":"sk-b-only"}`)); err != nil {
		t.Fatalf("CreateSecretRef(b) error = %v", err)
	}

	// The other tenant reads it. This is the removed boundary, stated as a value comparison so it cannot
	// pass on a technicality: a miss would fail here too.
	got, ok, err := store.Resolve(ctx, aOrg, sharedName)
	if err != nil || !ok {
		t.Fatalf("Resolve(a, shared-name) ok=%v err=%v — a secret ref is installation-wide and must resolve", ok, err)
	}
	if string(got) != "sk-b-only" {
		t.Fatalf("Resolve(a, shared-name) = %q, want the one installation-wide value %q", got, "sk-b-only")
	}

	// And the name is single-occupancy: the other tenant's write is the NEXT VERSION of the same secret,
	// so both now read the newer value. Version 2, not a second version 1 and not a duplicate-key error.
	out, err := store.CreateSecretRef(ctx, middleware.Scope{Project: aProj}, []byte(`{"name":"`+sharedName+`","value":"sk-a-wins"}`))
	if err != nil {
		t.Fatalf("CreateSecretRef(a, same name) error = %v", err)
	}
	if !strings.Contains(string(out.Body), `"version":2`) {
		t.Fatalf("the second tenant's write rendered %s, want version 2 of the one shared ref", out.Body)
	}
	if got, ok, err := store.Resolve(ctx, aOrg, sharedName); err != nil || !ok || string(got) != "sk-a-wins" {
		t.Fatalf("after the second write Resolve = %q ok=%v err=%v, want sk-a-wins", got, ok, err)
	}
}

// TestSecretRefResolveWrongKeyIsDecryptError proves the fail-closed primitive (SEC-002): a secret written
// under one master key and resolved by a store holding a DIFFERENT key is a DECRYPT error (errors.Is
// ErrSecretDecrypt, ok=false), NOT a silent miss. The resolver chain relies on this to fail closed rather
// than serve a superseded env secret when a rotated DB secret cannot be decrypted.
func TestSecretRefResolveWrongKeyIsDecryptError(t *testing.T) {
	cs := openHarness(t)
	ctx := context.Background()
	idstore := identity.New(cs.Pool())

	org, project, _ := provisionOrg(t, idstore, "sec-wrongkey")
	writer := identity.NewSecretStore(cs.Pool(), masterKey(t))
	if _, err := writer.CreateSecretRef(ctx, middleware.Scope{Project: project}, []byte(`{"name":"k","value":"sk-under-key-a"}`)); err != nil {
		t.Fatalf("CreateSecretRef error = %v", err)
	}

	// A DIFFERENT store (different master key) reading the same row: the row exists, so this is a decrypt
	// failure, not a miss.
	reader := identity.NewSecretStore(cs.Pool(), masterKey(t))
	got, ok, err := reader.Resolve(ctx, org, "k")
	if ok || got != nil {
		t.Fatalf("Resolve with wrong key returned ok=%v got=%q, want a decrypt error", ok, got)
	}
	if !errors.Is(err, identity.ErrSecretDecrypt) {
		t.Fatalf("Resolve with wrong key err = %v, want errors.Is ErrSecretDecrypt (fail-closed primitive)", err)
	}
}

// TestSecretRefRotateUnknownIsNotFound proves rotate of a name with no prior version is a 404 (a rotation
// implies an existing secret), while create of a fresh name succeeds at version 1.
func TestSecretRefRotateUnknownIsNotFound(t *testing.T) {
	cs := openHarness(t)
	ctx := context.Background()
	idstore := identity.New(cs.Pool())
	store := identity.NewSecretStore(cs.Pool(), masterKey(t))

	_, project, _ := provisionOrg(t, idstore, "sec-gamma")
	scope := middleware.Scope{Project: project}

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

	_, project, _ := provisionOrg(t, idstore, "sec-delta")
	scope := middleware.Scope{Project: project}

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
