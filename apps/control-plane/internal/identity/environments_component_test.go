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
//  5. Every one of those five paths is INSTALLATION-WIDE after A.2 Task 6. It used to read "a FOREIGN
//     tenant cannot read or write another organization's environment, on all five paths"; environments
//     carries no project_id and organizations are gone, so 000066 keys its policy on the installation.
//     TestEnvironmentsAreInstallationWide carries the whole argument.
package identity_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/palgroup/palai/apps/control-plane/api/middleware"
	"github.com/palgroup/palai/apps/control-plane/internal/identity"
	"github.com/palgroup/palai/storage"
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

	// Per-run: 000065 made the environment name unique across the INSTALLATION, so a literal is a 409 the
	// second time this suite runs against a retained database.
	envID := createEnvironment(t, store, scope, "production-"+newID("env"))

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
	envID := createEnvironment(t, store, scope, "keyrules-"+newID("env"))

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

// TestEnvironmentsAreInstallationWide REPLACES TestAForeignTenantCannotReachAnotherOrgsEnvironment, and
// the inversion is the finding of this task rather than a test being repaired.
//
// That test drove all five environment paths as tenant B against tenant A's environment and required every
// one to refuse, naming the consequence in its own words: "A's agent receiving B's credential". A.2 Task 6
// removes organizations, and `environments`/`environment_values` carry NO project_id (000046) — exactly the
// posture secret_refs has had since 000031 — so after migration 000066 their policy keys on the
// INSTALLATION, because there is nothing narrower left to key on. Every one of those five paths now
// reaches, and this test says so in full rather than being deleted.
//
// WHY THAT IS THE CHOSEN ANSWER AND NOT AN OVERSIGHT: it is EXACTLY today's behaviour, restated. These
// tables were organization-wide, and an installation has one organization — so nothing an operator can
// observe changes. What changes is the reason: it held because of a boundary, and now it holds because
// there is none. Palai's model after A.2 is one installation per customer (Palai Cloud is the layer that
// keeps customers apart), and within one customer these are that customer's own projects. An installation
// that ever hosts two customers needs project_id on these tables FIRST — 000066's header carries the same
// sentence, and this test is the executable half of it.
//
// The one boundary that DOES survive is asserted at the end: a connection that declared no scope still
// sees nothing, because 000066's expression is `system OR palai.project_id IS NOT NULL`, never `true`.
func TestEnvironmentsAreInstallationWide(t *testing.T) {
	cs := openHarness(t)
	ctx := context.Background()
	idstore := identity.New(cs.Pool())
	store := identity.NewSecretStore(cs.Pool(), masterKey(t))

	orgA, projectA, _ := provisionOrg(t, idstore, "env-tenant-a")
	_, projectB, _ := provisionOrg(t, idstore, "env-tenant-b")
	scopeA := middleware.Scope{Project: projectA}
	scopeB := middleware.Scope{Project: projectB}

	// The name is per-run because the uniqueness under test is INSTALLATION-wide: a literal would make this
	// test pass once against a retained database and fail on the next run for a reason unrelated to it.
	sharedName := "a-production-" + newID("env")
	envA := createEnvironment(t, store, scopeA, sharedName)
	if out, err := store.PutEnvironmentValue(ctx, scopeA, envA, []byte(`{"key":"JIRA_TOKEN","value":"`+sentinelValue+`"}`)); err != nil || out.BadField {
		t.Fatalf("seed A's key: %+v %v", out, err)
	}

	// PATH 1 — GET /v1/environments/{id}: the other project READS it.
	got, err := store.GetEnvironment(ctx, scopeB, envA)
	if err != nil || got.NotFound {
		t.Fatalf("GetEnvironment as B notFound=%v err=%v — environments are installation-wide", got.NotFound, err)
	}

	// PATH 2 — GET /v1/environments: it is in the other project's list.
	list, err := store.ListEnvironments(ctx, scopeB)
	if err != nil {
		t.Fatalf("ListEnvironments as B error = %v", err)
	}
	if !strings.Contains(string(list.Body), envA) {
		t.Fatalf("B's list does not carry the installation's environment: %s", list.Body)
	}

	// PATH 3 — POST /v1/environments/{id}/values: the other project WRITES into it. This is the leg the
	// replaced test called the one a read-only tenancy test would miss, and it is the one that costs the
	// most to have open: the value an agent receives is now writable by any project in the installation.
	put, err := store.PutEnvironmentValue(ctx, scopeB, envA, []byte(`{"key":"INJECTED","value":"b-controlled"}`))
	if err != nil || put.NotFound {
		t.Fatalf("PutEnvironmentValue as B notFound=%v err=%v", put.NotFound, err)
	}
	if v, ok, _ := store.Resolve(ctx, orgA, "env:"+envA+":INJECTED"); !ok || string(v) != "b-controlled" {
		t.Fatalf("the other project's write is not readable as A (ok=%v) — the claim above would be untested", ok)
	}

	// PATH 4 — DELETE .../values/{key}: the other project unbinds a key it did not write.
	del, err := store.DeleteEnvironmentValue(ctx, scopeB, envA, "JIRA_TOKEN")
	if err != nil || del.NotFound {
		t.Fatalf("DeleteEnvironmentValue as B notFound=%v err=%v", del.NotFound, err)
	}

	// PATH 5 — POST /v1/environments: the NAME is now single-occupancy. UNIQUE was
	// (organization_id, name) and 000065 rebuilt it as (name), so a second project asking for
	// "a-production" is refused rather than given its own. The replaced test asserted the opposite, and
	// deliberately: a 409 used to be a cross-tenant existence oracle. With one installation there is no
	// second tenant for it to disclose anything to.
	// The refusal is a typed OUTCOME, not a Go error — CreateEnvironment maps the unique violation to
	// ProvisionResult{Conflict} and the handler renders 409. Asserting `err == nil` here would have been
	// a test failing for a reason unrelated to its claim: the product WAS refusing.
	dup, err := store.CreateEnvironment(ctx, scopeB, []byte(`{"name":"`+sharedName+`"}`))
	if err != nil {
		t.Fatalf("CreateEnvironment(duplicate name) error = %v, want a typed conflict", err)
	}
	if !dup.Conflict {
		t.Fatalf("a second project created an environment with a name the installation already holds: %+v", dup)
	}

	// The boundary that SURVIVES: a connection that declared no scope at all still sees nothing. This is
	// what makes 000066's expression a policy rather than a formality, and it is the reason these tables
	// stay ENABLED and FORCED instead of having row-level security switched off.
	var visible int
	unscoped := cs.Pool()
	if err := unscoped.QueryRow(storage.WithTenant(ctx, ""), `SELECT count(*) FROM environments`).Scan(&visible); err == nil {
		t.Fatalf("a scope-less connection acquired and saw %d environment(s); it must be refused outright", visible)
	}
}
