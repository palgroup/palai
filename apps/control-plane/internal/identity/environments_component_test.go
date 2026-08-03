//go:build component

// The real-PostgreSQL component proofs for the ENVIRONMENT surface (E25 T3, migration 000046). They run
// only under `make test-component TEST=postgres`.
//
// What they prove, against a real database with real RLS and the real append-only grants:
//
//  1. An environment is created, two keys are written, and NO projection carries a value.
//  2. A value written through the API is resolvable by the DERIVED secret name and by nothing else — which
//     is what makes the orchestrator's read (RunEnvironmentKeys) able to find it.
//  3. Create-and-rotate are one operation, and a rotation moves the version.
//  4. Removing a key removes the BINDING and the sealed versions SURVIVE — the property migration 000046's
//     asymmetric grants exist to produce, asserted rather than described.
//  5. A FOREIGN tenant cannot read or write another organization's environment, on all five paths.
package identity_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/palgroup/palai/apps/control-plane/api/middleware"
	"github.com/palgroup/palai/apps/control-plane/internal/identity"
)

// sentinelValue is the credential every assertion below hunts for. It is one literal so a single grep of a
// failure message tells a reader whether the value escaped or the test merely failed.
const sentinelValue = "e25-t3-sentinel-jira-token-9f2b7c4d"

func createEnvironment(t *testing.T, store *identity.SecretStore, scope middleware.Scope, name string) string {
	t.Helper()
	out, err := store.CreateEnvironment(context.Background(), scope, []byte(`{"name":"`+name+`","description":"component fixture"}`))
	if err != nil {
		t.Fatalf("CreateEnvironment(%s) error = %v", name, err)
	}
	var r struct{ ID string }
	if err := json.Unmarshal(out.Body, &r); err != nil {
		t.Fatalf("decode environment body: %v", err)
	}
	if r.ID == "" {
		t.Fatalf("CreateEnvironment returned no id: %s", out.Body)
	}
	return r.ID
}

// TestEnvironmentWriteResolveRotateAndUnbind is the whole R1 store contract in one pass.
func TestEnvironmentWriteResolveRotateAndUnbind(t *testing.T) {
	cs := openHarness(t)
	ctx := context.Background()
	idstore := identity.New(cs.Pool())
	store := identity.NewSecretStore(cs.Pool(), masterKey(t))

	org, project, _ := provisionOrg(t, idstore, "env-alpha")
	scope := middleware.Scope{Project: project}

	envID := createEnvironment(t, store, scope, "production")

	// A keyless environment LISTS. This is the whole reason `environments` is a table rather than a naming
	// convention over secret_refs (migration 000046's header), so it is asserted rather than assumed.
	listed, err := store.ListEnvironments(ctx, scope)
	if err != nil {
		t.Fatalf("ListEnvironments error = %v", err)
	}
	if !strings.Contains(string(listed.Body), envID) || !strings.Contains(string(listed.Body), `"key_count":0`) {
		t.Fatalf("a keyless environment did not list with a zero key count: %s", listed.Body)
	}

	// Two keys, one carrying the sentinel.
	for _, body := range []string{
		`{"key":"JIRA_TOKEN","value":"` + sentinelValue + `"}`,
		`{"key":"DATABASE_URL","value":"postgres://u:p@db/x"}`,
	} {
		out, err := store.PutEnvironmentValue(ctx, scope, envID, []byte(body))
		if err != nil {
			t.Fatalf("PutEnvironmentValue error = %v", err)
		}
		if out.MissingField != "" || out.BadField || out.NotFound {
			t.Fatalf("PutEnvironmentValue refused a well-formed write: %+v", out)
		}
		if strings.Contains(string(out.Body), sentinelValue) || strings.Contains(string(out.Body), `"value"`) {
			t.Fatalf("the write projection disclosed the value: %s", out.Body)
		}
	}

	// THE READ ROUTE'S PAYLOAD: names, versions, times. Not the value.
	got, err := store.GetEnvironment(ctx, scope, envID)
	if err != nil {
		t.Fatalf("GetEnvironment error = %v", err)
	}
	for _, want := range []string{`"JIRA_TOKEN"`, `"DATABASE_URL"`, `"version":1`, `"updated_at"`} {
		if !strings.Contains(string(got.Body), want) {
			t.Fatalf("GetEnvironment is missing %s: %s", want, got.Body)
		}
	}
	if strings.Contains(string(got.Body), sentinelValue) || strings.Contains(string(got.Body), "postgres://u:p@db/x") {
		t.Fatalf("GetEnvironment disclosed a value: %s", got.Body)
	}

	// THE ORCHESTRATOR'S PATH: the value is resolvable by the DERIVED name and by that name only. This is
	// what makes RunEnvironmentKeys' `'env:' || environment_id || ':' || key` construction correct; if the
	// two disagreed, every run would resolve nothing and fail closed silently.
	derived := "env:" + envID + ":JIRA_TOKEN"
	value, ok, err := store.Resolve(ctx, org, derived)
	if err != nil || !ok {
		t.Fatalf("Resolve(%q) ok=%v err=%v — the derived name the orchestrator builds does not address the stored value", derived, ok, err)
	}
	if string(value) != sentinelValue {
		t.Fatalf("Resolve returned %q, want the sentinel", value)
	}
	// And NOT by the bare key name, which is the collision an org-wide flat namespace would have produced:
	// two environments each holding JIRA_TOKEN must not share one secret.
	if _, ok, _ := store.Resolve(ctx, org, "JIRA_TOKEN"); ok {
		t.Fatal("the bare key name resolves a value — the environment id is not part of the secret's address, so two environments would share one credential")
	}

	// CREATE AND ROTATE ARE ONE OPERATION (secret_refs is append-only, so there is no update to
	// distinguish them). The version moves and the next Resolve sees the new value with no restart.
	rotated, err := store.PutEnvironmentValue(ctx, scope, envID, []byte(`{"key":"JIRA_TOKEN","value":"`+sentinelValue+`-v2"}`))
	if err != nil {
		t.Fatalf("rotate error = %v", err)
	}
	if !strings.Contains(string(rotated.Body), `"version":2`) {
		t.Fatalf("a rotation did not move the version: %s", rotated.Body)
	}
	if v, _, _ := store.Resolve(ctx, org, derived); string(v) != sentinelValue+"-v2" {
		t.Fatalf("Resolve after rotate = %q, want the v2 value", v)
	}

	// REMOVING A KEY REMOVES THE BINDING AND NOT THE BYTES, and both halves are asserted because the
	// asymmetry is the point of migration 000046's grants. A "delete" that silently kept the credential
	// would be worse than no delete; a doc comment claiming it kept them is not evidence.
	removed, err := store.DeleteEnvironmentValue(ctx, scope, envID, "JIRA_TOKEN")
	if err != nil {
		t.Fatalf("DeleteEnvironmentValue error = %v", err)
	}
	if !strings.Contains(string(removed.Body), `"removed":true`) {
		t.Fatalf("the removal did not report itself: %s", removed.Body)
	}
	after, err := store.GetEnvironment(ctx, scope, envID)
	if err != nil {
		t.Fatalf("GetEnvironment after removal error = %v", err)
	}
	if strings.Contains(string(after.Body), "JIRA_TOKEN") {
		t.Fatalf("the unbound key is still listed: %s", after.Body)
	}
	if !strings.Contains(string(after.Body), "DATABASE_URL") {
		t.Fatalf("the removal took the wrong key with it: %s", after.Body)
	}
	// The sealed versions are STILL THERE. Nothing names them — the derived name is only ever built from a
	// membership row — but they were not deleted, because secret_refs grants no DELETE.
	if v, ok, _ := store.Resolve(ctx, org, derived); !ok || string(v) != sentinelValue+"-v2" {
		t.Fatalf("removing the binding DELETED the stored versions (ok=%v) — secret_refs is supposed to be append-only, and the API's own message tells the operator the bytes are retained", ok)
	}

	// Removing a key the environment never had is a 404, not a reported success.
	if out, err := store.DeleteEnvironmentValue(ctx, scope, envID, "NEVER_EXISTED"); err != nil || !out.NotFound {
		t.Fatalf("removing an absent key = (%+v, %v), want NotFound", out, err)
	}
}

// TestEnvironmentRefusesReservedAndMalformedKeyNames is the WRITE-time half of the two-place key rule. The
// exec-time half — the load-bearing one — lives in the sandbox adaptors
// (adapters/sandboxes/host/exec_test.go, adapters/sandboxes/oci/workspace/exec_env_test.go).
//
// NOTE WHAT IS *NOT* REFUSED HERE, DELIBERATELY: `PATH`. Which names a sandbox reserves is a property of
// the POSTURE (a Mac reserves DEVELOPER_DIR and PALAI_SIMCTL_SET; a container does not), a deployment can
// change posture without touching this table, and a list copied into a write path is a list that rots. So
// `PATH` is storable and unusable — the executor refuses it, before any process starts, deriving the
// reserved set from the environment it actually built. The order is honest: the check that cannot be
// reached around is the second one.
func TestEnvironmentRefusesReservedAndMalformedKeyNames(t *testing.T) {
	cs := openHarness(t)
	ctx := context.Background()
	store := identity.NewSecretStore(cs.Pool(), masterKey(t))
	_, project, _ := provisionOrg(t, identity.New(cs.Pool()), "env-keys")
	scope := middleware.Scope{Project: project}
	envID := createEnvironment(t, store, scope, "keyrules")

	for _, key := range []string{"lowercase", "With_Mixed", "WITH-DASH", "1LEADING", "HAS SPACE", "PALAI_ANYTHING", "PALAI_SIMCTL_SET", ""} {
		body, _ := json.Marshal(map[string]string{"key": key, "value": sentinelValue})
		out, err := store.PutEnvironmentValue(ctx, scope, envID, body)
		if err != nil {
			t.Fatalf("PutEnvironmentValue(%q) error = %v", key, err)
		}
		if !out.BadField && out.MissingField == "" {
			t.Errorf("key %q was accepted at write time: %s", key, out.Body)
		}
	}

	// And a legal key still works, so the loop above is not passing because every write fails.
	if out, err := store.PutEnvironmentValue(ctx, scope, envID, []byte(`{"key":"GH_TOKEN","value":"`+sentinelValue+`"}`)); err != nil || out.BadField {
		t.Fatalf("a legal key was refused (%+v, %v) — the refusals above prove nothing", out, err)
	}
}

// TestAForeignTenantCannotReachAnotherOrgsEnvironment drives ALL FIVE paths as tenant B against tenant A's
// environment. RLS is what refuses; this proves the refusal reaches the API as a 404 rather than as an
// error that leaks the row's existence, and — the half a read-only test would miss — that B cannot WRITE
// into A's environment either.
func TestAForeignTenantCannotReachAnotherOrgsEnvironment(t *testing.T) {
	cs := openHarness(t)
	ctx := context.Background()
	idstore := identity.New(cs.Pool())
	store := identity.NewSecretStore(cs.Pool(), masterKey(t))

	orgA, projectA, _ := provisionOrg(t, idstore, "env-tenant-a")
	orgB, projectB, _ := provisionOrg(t, idstore, "env-tenant-b")
	scopeA := middleware.Scope{Project: projectA}
	scopeB := middleware.Scope{Project: projectB}

	envA := createEnvironment(t, store, scopeA, "a-production")
	if out, err := store.PutEnvironmentValue(ctx, scopeA, envA, []byte(`{"key":"JIRA_TOKEN","value":"`+sentinelValue+`"}`)); err != nil || out.BadField {
		t.Fatalf("seed A's key: %+v %v", out, err)
	}

	// PATH 1 — GET /v1/environments/{id}: a 404, and nothing about A in the body.
	got, err := store.GetEnvironment(ctx, scopeB, envA)
	if err != nil {
		t.Fatalf("GetEnvironment as B error = %v", err)
	}
	if !got.NotFound {
		t.Fatalf("tenant B read tenant A's environment: %s", got.Body)
	}

	// PATH 2 — GET /v1/environments: A's environment is not in B's list.
	list, err := store.ListEnvironments(ctx, scopeB)
	if err != nil {
		t.Fatalf("ListEnvironments as B error = %v", err)
	}
	if strings.Contains(string(list.Body), envA) || strings.Contains(string(list.Body), "a-production") {
		t.Fatalf("tenant B's list carries tenant A's environment: %s", list.Body)
	}

	// PATH 3 — POST /v1/environments/{id}/values: B cannot write INTO A's environment. This is the leg a
	// read-only tenancy test misses, and the consequence would be A's agent receiving B's credential.
	put, err := store.PutEnvironmentValue(ctx, scopeB, envA, []byte(`{"key":"INJECTED","value":"b-controlled"}`))
	if err != nil {
		t.Fatalf("PutEnvironmentValue as B error = %v", err)
	}
	if !put.NotFound {
		t.Fatalf("tenant B wrote a key into tenant A's environment: %+v", put)
	}

	// PATH 4 — DELETE .../values/{key}: B cannot unbind A's key.
	del, err := store.DeleteEnvironmentValue(ctx, scopeB, envA, "JIRA_TOKEN")
	if err != nil {
		t.Fatalf("DeleteEnvironmentValue as B error = %v", err)
	}
	if !del.NotFound {
		t.Fatalf("tenant B removed a key from tenant A's environment: %+v", del)
	}

	// PATH 5 — POST /v1/environments: B creating an environment with A's NAME succeeds, and that is
	// CORRECT rather than a leak: UNIQUE is (organization_id, name), so two tenants may each have a
	// "a-production". Asserted so nobody later "fixes" it into a cross-tenant existence oracle — a 409
	// here would tell B that A holds that name.
	mine := createEnvironment(t, store, scopeB, "a-production")
	if mine == envA {
		t.Fatal("two tenants were given the same environment id")
	}

	// A's environment is untouched by all of the above: still one key, still resolvable, still A's.
	after, err := store.GetEnvironment(ctx, scopeA, envA)
	if err != nil {
		t.Fatalf("GetEnvironment as A error = %v", err)
	}
	if !strings.Contains(string(after.Body), "JIRA_TOKEN") || strings.Contains(string(after.Body), "INJECTED") {
		t.Fatalf("tenant A's environment changed under tenant B's attempts: %s", after.Body)
	}
	if v, ok, _ := store.Resolve(ctx, orgA, "env:"+envA+":JIRA_TOKEN"); !ok || string(v) != sentinelValue {
		t.Fatalf("tenant A's value did not survive tenant B's attempts (ok=%v)", ok)
	}
	// And B cannot resolve A's derived name even knowing it verbatim. Resolve is the internal path the
	// orchestrator uses, so this is the deepest of the five: RLS, not a projection, is what refuses.
	if _, ok, _ := store.Resolve(ctx, orgB, "env:"+envA+":JIRA_TOKEN"); ok {
		t.Fatal("tenant B resolved tenant A's environment value by its derived name")
	}
}
