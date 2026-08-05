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
	"github.com/palgroup/palai/packages/coordinator"
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

// extractEnvironmentID reads the id out of a CreateEnvironment body when the caller needs the RESULT rather
// than the helper's fatal-on-error convenience — the duplicate-name leg below asserts about the answer
// first and only then wants its id.
func extractEnvironmentID(t *testing.T, body []byte) string {
	t.Helper()
	var r struct{ ID string }
	if err := json.Unmarshal(body, &r); err != nil {
		t.Fatalf("decode environment body: %v", err)
	}
	if r.ID == "" {
		t.Fatalf("no environment id in body: %s", body)
	}
	return r.ID
}

// TestEnvironmentWriteResolveRotateAndUnbind is the whole R1 store contract in one pass.
func TestEnvironmentWriteResolveRotateAndUnbind(t *testing.T) {
	cs := openHarness(t)
	ctx := context.Background()
	idstore := identity.New(cs.Pool())
	store := identity.NewSecretStore(cs.Pool(), masterKey(t))

	project, _ := provisionOrg(t, idstore, "env-alpha")
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
	value, ok, err := store.Resolve(ctx, coordinator.Tenant{Project: scope.Project}, derived)
	if err != nil || !ok {
		t.Fatalf("Resolve(%q) ok=%v err=%v — the derived name the orchestrator builds does not address the stored value", derived, ok, err)
	}
	if string(value) != sentinelValue {
		t.Fatalf("Resolve returned %q, want the sentinel", value)
	}
	// And NOT by the bare key name, which is the collision an installation-wide flat namespace would have
	// produced: two environments each holding JIRA_TOKEN must not share one secret.
	if _, ok, _ := store.Resolve(ctx, coordinator.Tenant{Project: scope.Project}, "JIRA_TOKEN"); ok {
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
	if v, _, _ := store.Resolve(ctx, coordinator.Tenant{Project: scope.Project}, derived); string(v) != sentinelValue+"-v2" {
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
	if v, ok, _ := store.Resolve(ctx, coordinator.Tenant{Project: scope.Project}, derived); !ok || string(v) != sentinelValue+"-v2" {
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
	project, _ := provisionOrg(t, identity.New(cs.Pool()), "env-keys")
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

// TestAForeignProjectCannotReachAnotherProjectsEnvironment IS THE THIRD NAME THIS TEST HAS HAD, and the
// round trip is the record rather than a repair.
//
//  1. Its first name paired "a foreign tenant" with "another org's environment". It drove all five
//     environment paths as tenant B against tenant A's environment and required every one to refuse,
//     naming the consequence in its own words: "A's agent receiving B's credential". The exact identifier
//     is deliberately not written here: it is the `rested_on` of
//     evidence/superseded/con-003-environment-boundary-2026-08-05.json, and that guard proves a superseded
//     symbol is GONE by sweeping the tree for it — a history note spelling it out would keep it alive and
//     turn the record into an opinion.
//  2. A.2 Task 6 removed organizations and left `environments`/`environment_values` with NO project_id, so
//     their policy could only key on the INSTALLATION. Every one of those five paths began to REACH, and
//     the test was renamed TestEnvironmentsAreInstallationWide and inverted to say so — deliberately, and
//     correctly: a test asserting a boundary the tree does not have is worse than no test.
//  3. Migration 000006 gives both tables project_id. The five paths refuse again, so the assertions come
//     back to what (1) asserted.
//
// WHY (2) IS WRITTEN DOWN INSTEAD OF BEING QUIETLY DROPPED. For one phase this tree SHIPPED the open
// posture and a green test that said so. Someone reading a comment written in that phase — and there are
// several, in main.go and in storage/queries — will have absorbed "an environment is installation-wide" as
// a property of the design. It was a property of a MISSING COLUMN. Recording the interval is what stops the
// next person from "fixing" this test back to (2) when they meet one of those sentences.
//
// The five paths below are the ones a read-only tenancy sweep would miss: three of them WRITE.
func TestAForeignProjectCannotReachAnotherProjectsEnvironment(t *testing.T) {
	cs := openHarness(t)
	ctx := context.Background()
	idstore := identity.New(cs.Pool())
	store := identity.NewSecretStore(cs.Pool(), masterKey(t))

	projectA, _ := provisionOrg(t, idstore, "env-tenant-a")
	projectB, _ := provisionOrg(t, idstore, "env-tenant-b")
	scopeA := middleware.Scope{Project: projectA}
	scopeB := middleware.Scope{Project: projectB}
	tenantA := coordinator.Tenant{Project: projectA}
	tenantB := coordinator.Tenant{Project: projectB}

	// Per-run because this database is retained between component runs and shared with neighbouring
	// packages; a literal would collide for a reason unrelated to the claim.
	sharedName := "a-production-" + newID("env")
	envA := createEnvironment(t, store, scopeA, sharedName)
	if out, err := store.PutEnvironmentValue(ctx, scopeA, envA, []byte(`{"key":"JIRA_TOKEN","value":"`+sentinelValue+`"}`)); err != nil || out.BadField {
		t.Fatalf("seed A's key: %+v %v", out, err)
	}

	// PATH 1 — GET /v1/environments/{id}: the other project must MISS, and the miss must be the same 404 an
	// absent id produces. Distinguishing them would make this route an existence oracle.
	if got, err := store.GetEnvironment(ctx, scopeB, envA); err != nil || !got.NotFound {
		t.Fatalf("GetEnvironment as B notFound=%v err=%v — B read A's environment", got.NotFound, err)
	}

	// PATH 2 — GET /v1/environments: it must not be in the other project's list.
	list, err := store.ListEnvironments(ctx, scopeB)
	if err != nil {
		t.Fatalf("ListEnvironments as B error = %v", err)
	}
	if strings.Contains(string(list.Body), envA) {
		t.Fatalf("B's list carries A's environment: %s", list.Body)
	}

	// PATH 3 — POST /v1/environments/{id}/values: the other project must not WRITE into it. This is the leg
	// that costs the most when it is open — the value an agent receives becomes writable by another
	// project — and it is the one a read-only sweep never reaches.
	put, err := store.PutEnvironmentValue(ctx, scopeB, envA, []byte(`{"key":"INJECTED","value":"b-controlled"}`))
	if err != nil || !put.NotFound {
		t.Fatalf("PutEnvironmentValue as B notFound=%v err=%v — B wrote into A's environment", put.NotFound, err)
	}
	// AND THE WRITE LANDED NOWHERE. A refusal reported to the caller while a row was still written is a
	// shape this tree has shipped, so the refusal is checked at the STORE and not only in the answer.
	if v, ok, _ := store.Resolve(ctx, tenantA, "env:"+envA+":INJECTED"); ok {
		t.Fatalf("the refused write is resolvable as A (%q) — the refusal was reported, not performed", v)
	}

	// PATH 4 — DELETE .../values/{key}: the other project must not unbind a key it did not write. Checked
	// twice: the answer is a 404, AND A's own value still resolves afterwards.
	del, err := store.DeleteEnvironmentValue(ctx, scopeB, envA, "JIRA_TOKEN")
	if err != nil || !del.NotFound {
		t.Fatalf("DeleteEnvironmentValue as B notFound=%v err=%v — B unbound A's key", del.NotFound, err)
	}
	if v, ok, err := store.Resolve(ctx, tenantA, "env:"+envA+":JIRA_TOKEN"); !ok || err != nil || string(v) != sentinelValue {
		t.Fatalf("A's own value did not survive B's delete attempt (ok=%v err=%v)", ok, err)
	}

	// PATH 5 — POST /v1/environments: the NAME is per-project again. 000006 rebuilt the constraint as
	// `UNIQUE (project_id, name)`, so B asking for the same name gets its OWN environment rather than a
	// 409. The 409 was a cross-tenant existence oracle, which is exactly why the middle phase's version of
	// this test asserted one: with `UNIQUE (name)` the product genuinely refused.
	dup, err := store.CreateEnvironment(ctx, scopeB, []byte(`{"name":"`+sharedName+`"}`))
	if err != nil {
		t.Fatalf("CreateEnvironment(same name, other project) error = %v", err)
	}
	if dup.Conflict {
		t.Fatalf("B was refused a name A holds — the constraint is still installation-wide: %+v", dup)
	}
	if strings.Contains(string(dup.Body), envA) {
		t.Fatalf("B was handed A's environment id instead of its own: %s", dup.Body)
	}

	// AND THE TWO ENVIRONMENTS ARE SEPARATE CREDENTIAL SPACES. Same name, same key, two values — which is
	// the property the whole change exists for and the one the middle phase could not have.
	envB := extractEnvironmentID(t, dup.Body)
	if out, err := store.PutEnvironmentValue(ctx, scopeB, envB, []byte(`{"key":"JIRA_TOKEN","value":"b-own-value"}`)); err != nil || out.BadField {
		t.Fatalf("seed B's key: %+v %v", out, err)
	}
	aVal, aOK, _ := store.Resolve(ctx, tenantA, "env:"+envA+":JIRA_TOKEN")
	bVal, bOK, _ := store.Resolve(ctx, tenantB, "env:"+envB+":JIRA_TOKEN")
	if !aOK || !bOK || string(aVal) == string(bVal) {
		t.Fatalf("two projects' same-named keys share a value (a=%q ok=%v, b=%q ok=%v)", aVal, aOK, bVal, bOK)
	}

	// The boundary that held through all three phases: a connection that declared no scope at all sees
	// nothing. This is what makes the policy a policy rather than a formality, and the reason these tables
	// stay ENABLED and FORCED instead of having row-level security switched off.
	var visible int
	unscoped := cs.Pool()
	if err := unscoped.QueryRow(storage.WithTenant(ctx, ""), `SELECT count(*) FROM environments`).Scan(&visible); err == nil {
		t.Fatalf("a scope-less connection acquired and saw %d environment(s); it must be refused outright", visible)
	}
}
