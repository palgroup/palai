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
	"github.com/palgroup/palai/packages/coordinator"
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

	project, _ := provisionOrg(t, idstore, "sec-alpha")
	scope := middleware.Scope{Project: project}

	// Per-run because this database is retained between component runs and shared with sibling packages: a
	// literal would make this test's "version 2" assertion depend on how many of them ran first. Since
	// 000006 the name is unique per PROJECT rather than across the installation, which narrows the collision
	// but does not remove it — the fixtures here share one project seed prefix.
	name := "provider-one-" + newID("sec")
	created, err := store.CreateSecretRef(ctx, scope, []byte(`{"name":"`+name+`","value":"sk-live-v1"}`))
	if err != nil {
		t.Fatalf("CreateSecretRef error = %v", err)
	}
	if strings.Contains(string(created.Body), "sk-live-v1") || strings.Contains(string(created.Body), `"value"`) {
		t.Fatalf("create projection disclosed the value: %s", created.Body)
	}

	// Resolve is the resolver-chain hook main.go puts in front of the env-file bridge. It decrypts.
	got, ok, err := store.Resolve(ctx, coordinator.Tenant{Project: project}, name)
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
	got2, ok, err := store.Resolve(ctx, coordinator.Tenant{Project: project}, name)
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
	// tenant-scoped — an unscoped context is refused a connection outright (A.2 Task 1), which is itself
	// half the isolation guarantee — so it runs under the writing project's own scope.
	//
	// IT RAN UNDER WithInstallationScope UNTIL 000006, and the comment there said why: secret_refs carried
	// no tenant column, so there was no narrower scope to name. There is one now, and the predicate names
	// it — a fixture reading through the widest scope available would still pass if the boundary were gone.
	var cipher []byte
	if err := cs.Pool().QueryRow(storage.WithTenant(ctx, project),
		"SELECT ciphertext FROM secret_refs WHERE project_id = $1 AND name = $2 ORDER BY version DESC LIMIT 1",
		project, name).Scan(&cipher); err != nil {
		t.Fatalf("read stored ciphertext: %v", err)
	}
	if strings.Contains(string(cipher), "sk-live-v2") {
		t.Fatalf("the value is stored in plaintext at rest")
	}
}

// TestASecretNameIsPerProjectAndAForeignOneIsInvisible IS THE THIRD NAME THIS TEST HAS HAD. The round trip
// is recorded rather than repaired, because for one phase the tree SHIPPED the open posture and a green
// test asserting it.
//
//  1. TestSecretRefCrossOrgResolveDenied proved a resolver scoped to org A could never read org B's secret
//     of the same name.
//  2. A.2 Task 6 removed organizations and left secret_refs with no tenant column at all, so 000002 could
//     only key its policy on the INSTALLATION. It was renamed TestSecretRefNamesAreInstallationWide and
//     inverted — deliberately, and its own comment demanded that "re-introducing a boundary here means
//     deliberately changing this test, which is exactly the amount of friction that change deserves".
//  3. Migration 000006 adds project_id. This is that deliberate change, and it pays that friction.
//
// IT IS WRITTEN AS THE STRONGEST TRUE STATEMENT, WHICH IS WHAT (2) GOT RIGHT AND IS WORTH KEEPING: not
// "the read is scoped" but the three consequences a reader can check — a foreign name resolves NOTHING,
// two projects hold the same name at their OWN version 1 rather than one appending to the other, and each
// reads back its own value.
func TestASecretNameIsPerProjectAndAForeignOneIsInvisible(t *testing.T) {
	cs := openHarness(t)
	ctx := context.Background()
	idstore := identity.New(cs.Pool())
	store := identity.NewSecretStore(cs.Pool(), masterKey(t))

	aProj, _ := provisionOrg(t, idstore, "sec-b-a")
	bProj, _ := provisionOrg(t, idstore, "sec-b-b")
	tenantA := coordinator.Tenant{Project: aProj}
	tenantB := coordinator.Tenant{Project: bProj}

	// Per-run because this database is retained between component runs and shared with sibling packages.
	sharedName := "shared-name-" + newID("sec")
	if _, err := store.CreateSecretRef(ctx, middleware.Scope{Project: bProj}, []byte(`{"name":"`+sharedName+`","value":"sk-b-only"}`)); err != nil {
		t.Fatalf("CreateSecretRef(b) error = %v", err)
	}

	// THE RESTORED BOUNDARY. A resolves nothing, and the miss is a clean ok=false rather than an error —
	// the resolver chain reads that as "fall through to the env bridge", which is the same answer it gives
	// for a name nobody holds. A distinguishable failure would be an existence oracle.
	if got, ok, err := store.Resolve(ctx, tenantA, sharedName); ok || err != nil {
		t.Fatalf("Resolve(A, B's name) = (%q, ok=%v, err=%v) — A read B's secret", got, ok, err)
	}

	// THE NAME IS PER-PROJECT. A's write is its OWN version 1, not version 2 of B's secret. This is the
	// exact assertion phase (2) inverted: it required `"version":2` and called the name single-occupancy.
	out, err := store.CreateSecretRef(ctx, middleware.Scope{Project: aProj}, []byte(`{"name":"`+sharedName+`","value":"sk-a-own"}`))
	if err != nil {
		t.Fatalf("CreateSecretRef(a, same name) error = %v", err)
	}
	if !strings.Contains(string(out.Body), `"version":1`) {
		t.Fatalf("A's write rendered %s, want ITS OWN version 1 — a version 2 means A appended to B's secret", out.Body)
	}

	// AND EACH READS ITS OWN. Checked as a value comparison in both directions, because a single-direction
	// check passes just as well when both resolve nothing.
	aVal, aOK, aErr := store.Resolve(ctx, tenantA, sharedName)
	bVal, bOK, bErr := store.Resolve(ctx, tenantB, sharedName)
	if !aOK || aErr != nil || string(aVal) != "sk-a-own" {
		t.Fatalf("Resolve(A) = (%q, %v, %v), want sk-a-own", aVal, aOK, aErr)
	}
	if !bOK || bErr != nil || string(bVal) != "sk-b-only" {
		t.Fatalf("Resolve(B) = (%q, %v, %v), want sk-b-only — A's write overwrote B's secret", bVal, bOK, bErr)
	}
}

// TestANewSecretCannotBeWrittenWithoutAProject is the WRITE half of 000006, and it exists because that
// migration leaves every pre-existing row with an EMPTY project_id whose policy still admits every scope.
//
// A DEFAULT IS A VALUE A WRITER INHERITS BY SAYING NOTHING. Without this refusal the first secret written
// by a credential that names no project would land in that same legacy bucket — readable by every tenant —
// and the boundary would be undone by its own compatibility provision, silently, with no database
// constraint able to catch it (the column is NOT NULL with a default, and the policy admits the value).
//
// The reachable path is a `system`-capability key: identity.provisioningScope hands one a SYSTEM scope,
// under which every policy admits, so the credential is the only thing between such a key and the write.
func TestANewSecretCannotBeWrittenWithoutAProject(t *testing.T) {
	cs := openHarness(t)
	ctx := context.Background()
	store := identity.NewSecretStore(cs.Pool(), masterKey(t))

	name := "unowned-" + newID("sec")
	for _, tc := range []struct {
		what  string
		scope middleware.Scope
	}{
		{"no project at all", middleware.Scope{}},
		{"a system key that names no project", middleware.Scope{Scopes: []string{"system"}}},
	} {
		out, err := store.CreateSecretRef(ctx, tc.scope, []byte(`{"name":"`+name+`","value":"sk-unowned"}`))
		if err != nil {
			t.Fatalf("CreateSecretRef(%s) error = %v, want a typed refusal", tc.what, err)
		}
		if out.InsufficientScope == "" {
			t.Fatalf("CreateSecretRef(%s) = %+v, want a refusal naming what the credential lacks", tc.what, out)
		}
	}

	// AND NOTHING WAS WRITTEN. The refusal is checked at the TABLE and not only in the answer, because a
	// refusal reported to the caller while the row still landed is a shape this tree has shipped. The read
	// runs under the system scope so the policy cannot be what hides the row.
	var rows int
	if err := cs.Pool().QueryRow(storage.WithSystemScope(ctx),
		"SELECT count(*) FROM secret_refs WHERE name = $1", name).Scan(&rows); err != nil {
		t.Fatalf("count secret_refs: %v", err)
	}
	if rows != 0 {
		t.Fatalf("%d row(s) written under a refusal — the write path produced an unowned secret", rows)
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

	project, _ := provisionOrg(t, idstore, "sec-wrongkey")
	writer := identity.NewSecretStore(cs.Pool(), masterKey(t))
	if _, err := writer.CreateSecretRef(ctx, middleware.Scope{Project: project}, []byte(`{"name":"k","value":"sk-under-key-a"}`)); err != nil {
		t.Fatalf("CreateSecretRef error = %v", err)
	}

	// A DIFFERENT store (different master key) reading the same row: the row exists, so this is a decrypt
	// failure, not a miss.
	reader := identity.NewSecretStore(cs.Pool(), masterKey(t))
	got, ok, err := reader.Resolve(ctx, coordinator.Tenant{Project: project}, "k")
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

	project, _ := provisionOrg(t, idstore, "sec-gamma")
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

	project, _ := provisionOrg(t, idstore, "sec-delta")
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
