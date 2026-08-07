package admin

// A POOL HAS NO BIRTH PATH (E28 T1), and this file is the CLI half of the measurement.
//
// E24 shipped eight tasks over a fleet whose pools cannot be created. `POST /v1/runner-pools` is absent
// from the route table (`api/router.go:359-392`), `grep -rn "UPDATE runner_pools" … | grep -v _test`
// answers 0, and the one statement that writes a pool row writes `'default'`, `'sandboxed-linux'` and
// `false` as LITERALS (`storage/queries/runners.sql:175`). So a rented-Mac pool cannot exist and
// `strict_enrollment` cannot be switched on — which makes E24 T6's `approve` route the decider of a state
// no operator can produce.
//
// The two comments that handed pool creation away name T5 and T6; both shipped without it.
//
// This file drives the ADMIN CLI through the GERÇEK router for `runner_lifecycle_test.go`'s reason: a stub
// server proves the CLI builds a path, and only the real router proves something answers it.

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	capi "github.com/palgroup/palai/apps/control-plane/api"
	"github.com/palgroup/palai/apps/control-plane/api/middleware"
)

// fakeFleet is the RunnerRegistryAPI stub the pool tests drive. It moved here on 2026-08-07 from
// runner_lifecycle_test.go, which was deleted with the `palai admin runner` verb it existed to keep a
// caller in front of — the fixture outlived the file because `pool` still uses it. Only the pieces
// pool_test.go actually reaches are kept; a stub carrying methods nothing calls is a second surface
// with no caller, which is the defect the deleted file was written about.
type fakeFleet struct {
	action string
	id     string
	// scope is the verified scope an APPROVE carried (E24 T6). The lifecycle verbs take org/project strings;
	// an approval takes the whole scope because the principal is derived from its key id.
	scope middleware.Scope
	// The E28 T1 birth path: what the control plane was asked to CREATE, and which pool it was asked to open
	// the waiting room on. `strict` is a pointer so "asked for false" and "never asked" stay different
	// answers — the same reason api.RunnerItem.ActiveLeases is one.
	created createdPool
	// createdProject is the SCOPE the create arrived under, and it is recorded because it is the field the
	// free-pool contract is about: "" is the plane's pool, a project is one reserved to that tenant. The
	// method used to take it as `_`, so a fake could not tell the two apart -- and a fake that discards the
	// deciding field answers every question about it with silence.
	createdProject string
	strictID       string
	strict         *bool
}

// createdPool is what a `pool create` reached the control plane with. It is declared here rather than
// reusing the api type so that pool_test.go's RED is a BEHAVIOURAL failure (the CLI has no `pool` resource,
// so `execute` returns usageErr) rather than a compile error against a type that does not exist yet.
type createdPool struct {
	Name, Posture, OS, Arch string
	StrictEnrollment        bool
	// IsolationMode is what the operator asked every machine in this pool to be able to PROVIDE
	// (000007). Recorded here because a flag the CLI parses and never sends is indistinguishable from
	// one it sends, on the CLI's own output alone.
	IsolationMode string
}

func (f *fakeFleet) ListRunners(context.Context, string, capi.RunnerListWindow) ([]capi.RunnerItem, error) {
	return []capi.RunnerItem{{
		ID: "rnr_one", PoolID: "pool_mac", Label: "mac-mini-01", DNS: "rnr_one.runners.palai.internal",
		State: "active", Posture: "unsandboxed-host", Capacity: 1,
		CreatedAt: time.Unix(1700000000, 0).UTC(), LastSeenAt: time.Unix(1700000060, 0).UTC(),
	}}, nil
}

func (f *fakeFleet) GetRunner(_ context.Context, _, id string) (capi.RunnerItem, bool, error) {
	return capi.RunnerItem{ID: id, State: "active", PoolID: "pool_mac"}, true, nil
}

func (f *fakeFleet) ListRunnerPools(context.Context, string, capi.RunnerListWindow) ([]capi.RunnerPoolItem, error) {
	if f.created.Name == "" {
		return []capi.RunnerPoolItem{}, nil
	}
	// The pool the create made, so `palai pool list` has something to render and the CLI's read half is
	// asserted against a pool that was actually created rather than a canned row.
	return []capi.RunnerPoolItem{{
		ID: "pool_created", Name: f.created.Name, Posture: f.created.Posture,
		OS: f.created.OS, Arch: f.created.Arch, StrictEnrollment: f.created.StrictEnrollment,
		CreatedAt: time.Unix(1700000000, 0).UTC(),
	}}, nil
}

// CreateRunnerPool and SetRunnerPoolStrictEnrollment are the E28 T1 surface `palai pool` fronts.
func (f *fakeFleet) CreateRunnerPool(_ context.Context, project string, in capi.RunnerPoolCreate) (capi.RunnerPoolItem, error) {
	f.createdProject = project
	f.created = createdPool{Name: in.Name, Posture: in.Posture, OS: in.OS, Arch: in.Arch,
		StrictEnrollment: in.StrictEnrollment, IsolationMode: in.IsolationMode}
	return capi.RunnerPoolItem{
		ID: "pool_created", Name: in.Name, Posture: in.Posture, OS: in.OS, Arch: in.Arch,
		StrictEnrollment: in.StrictEnrollment, CreatedAt: time.Unix(1700000000, 0).UTC(),
	}, nil
}

func (f *fakeFleet) SetRunnerPoolStrictEnrollment(_ context.Context, _, poolID string, strict bool) (capi.RunnerPoolItem, bool, error) {
	f.strictID, f.strict = poolID, &strict
	return capi.RunnerPoolItem{ID: poolID, Name: "mac-pool", Posture: "unsandboxed-host", StrictEnrollment: strict}, true, nil
}

func (f *fakeFleet) MintRunnerPoolKey(context.Context, string, string, *time.Time) (capi.RunnerPoolKeyItem, bool, error) {
	return capi.RunnerPoolKeyItem{}, false, nil
}

func (f *fakeFleet) ListRunnerPoolKeys(context.Context, string, string) ([]capi.RunnerPoolKeyItem, error) {
	return []capi.RunnerPoolKeyItem{}, nil
}

func (f *fakeFleet) RevokeRunnerPoolKey(context.Context, string, string) (capi.RunnerPoolKeyItem, bool, error) {
	return capi.RunnerPoolKeyItem{}, false, nil
}

// SetRunnerState is the surface this task adds and the surface this test demands: the operator's
// cordon/resume/revoke of ONE machine, named by the id the server minted.
func (f *fakeFleet) SetRunnerState(_ context.Context, _, id, action string) (capi.RunnerItem, bool, error) {
	f.action, f.id = action, id
	state := map[string]string{"cordon": "cordoned", "resume": "active", "revoke": "revoked"}[action]
	return capi.RunnerItem{ID: id, State: state, PoolID: "pool_mac"}, true, nil
}

// ApproveRunner is E24 T6's surface: the human a strict pool waits for. It records the scope as well as the
// id, because the deciding principal is derived from the scope one layer down and a CLI path that reached a
// route which dropped it would approve as nobody.
func (f *fakeFleet) ApproveRunner(_ context.Context, scope middleware.Scope, id string) (capi.RunnerItem, capi.ApprovalOutcome, error) {
	f.action, f.id, f.scope = "approve", id, scope
	return capi.RunnerItem{ID: id, State: "active", PoolID: "pool_mac"},
		capi.ApprovalOutcome{Found: true, Applied: true}, nil
}

// grantSystemAlongside translates a test's ordinary-capability intent into a Scopes value that ALSO
// carries `system` (Faz A.1 Task 3): every fleet route this package's tests drive now sits behind
// router.go's systemOnly gate, on top of whichever `provision`/`approve` check the route already had. nil
// meant "the unrestricted admin key", and that idiom cannot survive literally — Scope.HasSystem() is
// deliberately excluded from the empty-set-is-unrestricted rule (middleware/auth_test.go pins it) — so nil
// is translated to the two ordinary capabilities this package's fleet routes check, `provision` and
// `approve`, alongside `system`. A non-nil list is a caller testing ONE capability in isolation, and
// `system` is appended so the only question left is the one the test names.
func grantSystemAlongside(scopes []string) []string {
	if scopes == nil {
		return []string{middleware.ScopeSystem, "provision", "approve"}
	}
	return append([]string{middleware.ScopeSystem}, scopes...)
}

// TestRunnerLifecycleCLIReachesTheRealRouter is RED (3), REACHABILITY. It demands a surface that does
// not exist today — `palai admin runner cordon|resume|revoke|list` and the routes behind them — which
// is the point: the test is what turns "implemented" into "reachable".

// TestPoolCreateReachesTheRealRouter is RED (2). `palai pool create --name mac-pool --posture
// unsandboxed-host` returns `usageErr` today: `pool` is not a resource `execute` knows, so the
// `positionalArity` lookup misses and the command dies before a request is built (`admin.go:190-198`).
//
// The posture is the whole point of the assertion rather than decoration: `unsandboxed-host` is the value
// migration 000045's CHECK declares and NO CODE PATH WRITES, so a create that cannot carry it would leave
// the tree exactly where it is.
func TestPoolCreateReachesTheRealRouter(t *testing.T) {
	fleet := &fakeFleet{}
	srv := poolCLIServer(t, fleet, middleware.Scope{Project: "prj_1"})
	t.Setenv("PALAI_BASE_URL", srv.URL)
	t.Setenv("PALAI_API_KEY", "bootstrap-admin-key")

	var out bytes.Buffer
	if err := Run("pool", []string{"create", "--name", "mac-pool", "--posture", "unsandboxed-host",
		"--os", "darwin", "--arch", "arm64", "--strict"}, &out, strings.NewReader("")); err != nil {
		t.Fatalf("palai pool create: %v — a fleet whose pools cannot be created is a fleet with one pool, named `default`, forever", err)
	}
	var body map[string]any
	if err := json.Unmarshal(out.Bytes(), &body); err != nil {
		t.Fatalf("decode the create response %q: %v", out.String(), err)
	}
	for field, want := range map[string]any{
		"object": "runner_pool", "name": "mac-pool", "posture": "unsandboxed-host",
		"os": "darwin", "arch": "arm64", "strict_enrollment": true,
	} {
		if body[field] != want {
			t.Errorf("create response %s = %v, want %v", field, body[field], want)
		}
	}
	// The control plane was ASKED for the posture, rather than the CLI having rendered a hopeful echo.
	if fleet.created.Posture != "unsandboxed-host" || !fleet.created.StrictEnrollment {
		t.Fatalf("the control plane was asked for %+v, want an unsandboxed-host pool with strict enrolment on", fleet.created)
	}

	// And `list`, which is the read an operator needs before `--pool` can name anything.
	out.Reset()
	if err := Run("pool", []string{"list"}, &out, strings.NewReader("")); err != nil {
		t.Fatalf("palai pool list: %v", err)
	}
	if !strings.Contains(out.String(), "pool_created") {
		t.Errorf("pool list output %q does not carry the pool the create made", out.String())
	}
}

// TestPoolSetStrictReachesTheRealRouter is the switch `FLT-P12` says an operator asks with. Its own
// sentence is *"the waiting room exists and nothing is in it unless an operator asks"* — and before this
// task the only two places that turned `strict_enrollment` on were two TEST FILES issuing raw
// `UPDATE runner_pools`, which is CLAUDE.md's "a test that builds its own config never sees the config
// production builds" verbatim.
func TestPoolSetStrictReachesTheRealRouter(t *testing.T) {
	fleet := &fakeFleet{}
	srv := poolCLIServer(t, fleet, middleware.Scope{Project: "prj_1"})
	t.Setenv("PALAI_BASE_URL", srv.URL)
	t.Setenv("PALAI_API_KEY", "bootstrap-admin-key")

	var out bytes.Buffer
	if err := Run("pool", []string{"set-strict", "pool_mac", "--strict"}, &out, strings.NewReader("")); err != nil {
		t.Fatalf("palai pool set-strict: %v — with no writer, `strict_enrollment` cannot be turned on by anybody outside a test file", err)
	}
	if fleet.strictID != "pool_mac" || fleet.strict == nil || !*fleet.strict {
		t.Fatalf("the control plane was asked to set strict=%v on %q, want true on pool_mac", fleet.strict, fleet.strictID)
	}
	out.Reset()
	if err := Run("pool", []string{"set-strict", "pool_mac"}, &out, strings.NewReader("")); err != nil {
		t.Fatalf("palai pool set-strict (off): %v", err)
	}
	if fleet.strict == nil || *fleet.strict {
		t.Fatalf("omitting --strict asked for %v, want false — the switch has to close as well as open", fleet.strict)
	}
}

// TestE24HandoverBlockStillDoesNotWork is the OTHER half of RED (2), and it is a documentation claim made
// as a test: `phase-24-runner-fleet.md:72`'s §0.2 owner handover block — the place an owner COPY-PASTES from
// — spells three commands, and the block has to be re-measured rather than remembered.
//
// CORRECTED BY E28 T4, AND THE CORRECTION IS THE POINT RATHER THAN A TIDY-UP. This test's first draft drove
// `Run("admin pool", …)` and asserted a usage refusal. That refusal is real and it is about a string
// `cmd/cli/main.go` NEVER PRODUCES: main.go's `case "admin"` splits the prefix off and calls
// `admin.Run(args[1], args[2:], …)`, so what an operator typing `palai admin pool create` reaches is
// `Run("pool", ["create", …])` — which, once T1 added the `pool` resource, WORKS. The guard was passing on an
// input the binary cannot generate, which is the exact shape it was written to catch, one layer down.
//
// SO THE HONEST STATEMENT TODAY IS A SPLIT, and both halves are asserted:
//
//   - `palai admin pool create …` NOW WORKS. §3.6 D2 said it does not; that was true before T1 and this task
//     made it false, because `palai admin <resource>` reaches every resource admin.Run dispatches. Verified
//     against the real binary: it builds the request and fails on CONNECT, not on dispatch.
//   - `palai admin pool key create --pool <id>` STILL DOES NOT WORK, and the correct spelling is
//     `palai poolkey create --pool <id>`. It reaches `Run("pool", ["key", "create", …])`, where the flag
//     parse dies first: the `pool` resource registers no `--pool` flag at all.
//
// The accident this catches is cheap and repeats: a plan's §0 is prose, prose is not run, and a command that
// has never been executed reads exactly like one that has — including inside the test that checks it.
func TestE24HandoverBlockStillDoesNotWork(t *testing.T) {
	t.Setenv("PALAI_BASE_URL", "http://127.0.0.1:1")
	t.Setenv("PALAI_API_KEY", "k")

	// THE HALF THAT NOW WORKS. The dispatch reaches the pool create path; the only failure left is the
	// network, and asserting "not a usage error" is what distinguishes the two.
	var out bytes.Buffer
	err := Run("pool", []string{"create", "--name", "mac-pool", "--posture", "unsandboxed-host", "--os", "darwin", "--arch", "arm64"}, &out, strings.NewReader(""))
	if err != nil && strings.Contains(err.Error(), "usage: palai") {
		t.Fatalf("`palai admin pool create …` is refused as a usage error (%v) — E28 T1 added the `pool` resource and main.go's admin prefix reaches it, so the E24 handover block's FIRST line works today and this test says otherwise", err)
	}

	// THE HALF THAT DOES NOT, with the failure REASON asserted rather than any-error-will-do: a test that
	// accepted any error would keep passing if `pool key` started meaning something else.
	out.Reset()
	err = Run("pool", []string{"key", "create", "--pool", "pool_mac"}, &out, strings.NewReader(""))
	if err == nil {
		t.Fatal("`palai admin pool key create --pool <id>` was accepted — the real spelling is `palai poolkey create --pool <id>`, and a block an owner copy-pastes must not have two spellings that both appear to work")
	}
	if !strings.Contains(err.Error(), "not defined") && !strings.Contains(err.Error(), "usage: palai") {
		t.Errorf("`palai admin pool key create` failed with %v — want the flag-parse refusal (`pool` registers no --pool flag) or the usage refusal", err)
	}
}

// TestPoolNeedsTheProvisionCapability is the authorization half, and it is the same claim the key surface
// makes: creating a pool and opening its waiting room are org administration, so a key without `provision`
// is refused by the REAL gate rather than by a check this test wrote.
func TestPoolNeedsTheProvisionCapability(t *testing.T) {
	fleet := &fakeFleet{}
	srv := poolCLIServer(t, fleet,
		middleware.Scope{Project: "prj_1", Scopes: []string{"run"}})
	t.Setenv("PALAI_BASE_URL", srv.URL)
	t.Setenv("PALAI_API_KEY", "run-only-key")

	// THE REFUSAL IS NAMED, not merely counted. `err != nil` alone would be satisfied today by the flag
	// parser rejecting `--name`, which is a green that says nothing about a capability gate — and a
	// vacuous authorization assertion is worse than none, because the next reader trusts it.
	for _, tc := range []struct{ args []string }{
		{[]string{"create", "--name", "mac-pool", "--posture", "unsandboxed-host"}},
		{[]string{"set-strict", "pool_mac", "--strict"}},
	} {
		var out bytes.Buffer
		err := Run("pool", tc.args, &out, strings.NewReader(""))
		if err == nil {
			t.Fatalf("a run-only key ran `palai pool %s`: creating a pool decides where a tenant's runs execute, and opening its waiting room decides who may admit a machine", strings.Join(tc.args, " "))
		}
		if !strings.Contains(err.Error(), "insufficient_scope") {
			t.Errorf("`palai pool %s` with a run-only key failed with %v, want the real router's insufficient_scope refusal", strings.Join(tc.args, " "), err)
		}
	}
	if fleet.created.Name != "" {
		t.Fatalf("the store was asked to create %q by an unauthorized key", fleet.created.Name)
	}
	if fleet.strict != nil {
		t.Fatalf("the store was asked to set strict=%v by an unauthorized key", *fleet.strict)
	}
}

// TestPoolRejectsASecondPositional keeps the arity guard honest for the new resource: `set-strict` names
// ONE pool, for the reason `runner revoke` names one machine.
func TestPoolRejectsASecondPositional(t *testing.T) {
	t.Setenv("PALAI_BASE_URL", "http://127.0.0.1:1")
	t.Setenv("PALAI_API_KEY", "k")
	var out bytes.Buffer
	err := Run("pool", []string{"set-strict", "pool_a", "pool_b"}, &out, strings.NewReader(""))
	if err == nil {
		t.Fatal("`pool set-strict pool_a pool_b` was accepted; the second id would be silently dropped")
	}
	// Named, for TestPoolNeedsTheProvisionCapability's reason: today this fails because `pool` is not a
	// resource at all, and an unnamed `err != nil` would keep reporting green after the arity guard was
	// deleted.
	if !strings.Contains(err.Error(), "positional argument") {
		t.Errorf("`pool set-strict pool_a pool_b` failed with %v, want the arity refusal", err)
	}
}

// TestNoPoolFlagCarriesACredential is the file's credential fence, in the shape `register`'s own comment
// asks for: the resource's flags are enumerated and none of them is a value a secret could ride.
func TestNoPoolFlagCarriesACredential(t *testing.T) {
	var f flags
	fs := flag.NewFlagSet("palai pool create", flag.ContinueOnError)
	f.register(fs, "pool")
	seen := map[string]bool{}
	fs.VisitAll(func(fl *flag.Flag) { seen[fl.Name] = true })
	for _, banned := range []string{"key", "api-key", "token", "secret", "value", "password"} {
		if seen[banned] {
			t.Errorf("`palai pool` registers a --%s flag; nothing on this resource may put a credential in argv", banned)
		}
	}
	for _, want := range []string{"name", "posture", "os", "arch", "strict"} {
		if !seen[want] {
			t.Errorf("`palai pool` does not register --%s", want)
		}
	}
}

// TestPoolCreateWithNoProjectScopeCreatesAFreePool is the contract that REPLACED a refusal, and the
// replacement is the point rather than an incidental rename.
//
// This test used to be TestPoolCreateNeedsAProjectScope, and it was correct when it was written: the
// tenant RLS expression spelled "shared" as the empty string, `project_id = ”` violates
// runner_pools_project_id_fkey, and `fleet.Store.Register` therefore refused to enrol a machine into a
// pool with no project. A project-less pool really was a row nothing could ever join.
//
// 000002's palai_apply_fleet_policy makes NULL the free pool: one the PLANE owns, that every project on
// the installation may be placed onto. A device enrols once and serves everybody, which is what a fleet
// is. So the surface must CREATE one — a route that still answered 400 would leave the policy reachable
// by nothing a human drives.
//
// It is proved here rather than at the component tier for the same reason the refusal was: the scope is
// what decides it, and a fake verifier is the only thing that can hand the real router a scope with no
// project.
func TestPoolCreateWithNoProjectScopeCreatesAFreePool(t *testing.T) {
	// A NORMAL scope, with a project on it, because that is the ONLY scope a served request can carry:
	// coordinator.Store.VerifyAPIKey refuses a key whose project is NULL. A fixture that handed the router
	// an empty-project scope would be measuring a request no credential can produce — and this test was
	// written that way for one commit, which is exactly the shape it now guards against.
	fleet := &fakeFleet{}
	srv := poolCLIServer(t, fleet, middleware.Scope{Project: "prj_1", Scopes: []string{"provision"}})
	t.Setenv("PALAI_BASE_URL", srv.URL)
	t.Setenv("PALAI_API_KEY", "plane-wide-key")

	var out bytes.Buffer
	if err := Run("pool", []string{"create", "--name", "free", "--posture", "unsandboxed-host", "--shared"}, &out, strings.NewReader("")); err != nil {
		t.Fatalf("a plane-scoped key could not create a free pool: %v", err)
	}
	if fleet.created.Name != "free" {
		t.Fatalf("the store was asked to create %q, want the free pool", fleet.created.Name)
	}
	// The recorded project is the whole claim: an empty one is what the statement converts to NULL, and a
	// non-empty one would be a pool reserved to a tenant the caller never named.
	if fleet.createdProject != "" {
		t.Fatalf("`--shared` created the pool inside project %q. The caller's own project must not become the "+
			"owner of a pool it asked the PLANE to hold, or one tenant silently owns the machines every "+
			"other tenant runs on", fleet.createdProject)
	}
}

// poolCLIServer serves the shipped router with the fleet registry mounted and the given verified scope, so
// every command above meets the real mux, the real capability gate and the real RFC9457 problem shape.
//
// grantSystemAlongside augments scope.Scopes with `system` (Faz A.1 Task 3): every route this file drives
// is now behind router.go's systemOnly gate on top of whatever this file already asserted, and a fixture
// caller's Organization/Project (or its deliberate absence, in TestPoolCreateWithNoProjectScopeCreatesAFreePool) is left
// untouched.
func poolCLIServer(t *testing.T, fleet capi.RunnerRegistryAPI, scope middleware.Scope) *httptest.Server {
	t.Helper()
	scope.Scopes = grantSystemAlongside(scope.Scopes)
	srv := httptest.NewServer(capi.NewRouter(staticVerifier{scope: scope},
		nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, fakeProv{}, nil, capi.SSEConfig{}, nil, nil,
		capi.WithRunners(fleet)))
	t.Cleanup(srv.Close)
	return srv
}

// TestPoolCreateCarriesTheIsolationRequirementTheOperatorTyped — the operator's end of 000007.
//
// The column, its CHECK, the fleet store's refusal and that refusal's journal entry all shipped; what did
// not was any way to ASK. Measured 2026-08-07: `isolation_mode` appeared in one SELECT and zero
// INSERT/UPDATE statements, so `fleet.Store.Register`'s ErrIsolationUnsupported waited on a state no
// operator could produce — the same sentence TestPoolSetStrictReachesTheRealRouter records above it.
//
// THE ASSERTION IS ON WHAT THE PLANE WAS ASKED FOR, never on the CLI's own echo, because a flag parsed
// into a variable nothing sends is exactly the shape this test exists to catch.
func TestPoolCreateCarriesTheIsolationRequirementTheOperatorTyped(t *testing.T) {
	fleet := &fakeFleet{}
	srv := poolCLIServer(t, fleet, middleware.Scope{Project: "prj_1"})
	t.Setenv("PALAI_BASE_URL", srv.URL)
	t.Setenv("PALAI_API_KEY", "bootstrap-admin-key")

	var out bytes.Buffer
	if err := Run("pool", []string{"create", "--name", "mac-dense", "--posture", "unsandboxed-host",
		"--isolation", "accounts"}, &out, strings.NewReader("")); err != nil {
		t.Fatalf("palai pool create --isolation accounts: %v", err)
	}
	if fleet.created.IsolationMode != "accounts" {
		t.Fatalf("the control plane was asked for isolation_mode=%q, want \"accounts\" — an operator cannot arm the enrolment refusal that turns away a Mac with no palai-agentd",
			fleet.created.IsolationMode)
	}

	// AND OMITTING IT ASKS FOR NOTHING, which is every pool shipped so far. A CLI that sent "" or a default
	// would either impose a requirement nobody typed or start failing the day the route stops special-casing
	// the empty string.
	fleet.created = createdPool{}
	out.Reset()
	if err := Run("pool", []string{"create", "--name", "mac-plain", "--posture", "unsandboxed-host"},
		&out, strings.NewReader("")); err != nil {
		t.Fatalf("palai pool create with no --isolation: %v", err)
	}
	if fleet.created.IsolationMode != "" {
		t.Fatalf("a create with no --isolation asked for %q, want \"\"", fleet.created.IsolationMode)
	}
}
