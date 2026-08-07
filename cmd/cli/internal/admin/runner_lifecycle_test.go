package admin

// REACHABILITY (E24 T5), and it is the whole reason this file exists.
//
// §3.6 D15: `Revoke()` — SAN-011's hard stop, written for a compromised runner — was implemented,
// tested, and registered in the UAT catalogue, and NOTHING IN THE TREE CALLED IT. `Resume()` likewise.
// That is the same shape as E19 T9's `CreateSlackConnection` and E23's `DecideToolApproval`: fully
// built, fully tested, reachable from nothing. A security control with no operator surface is a
// security control that does not exist.
//
// So this drives the ADMIN CLI through the GERÇEK router: the verb an operator types has to reach a
// route the real mux actually serves. A stub server would prove the CLI builds a path; only the real
// router proves something answers it.

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	capi "github.com/palgroup/palai/apps/control-plane/api"
	"github.com/palgroup/palai/apps/control-plane/api/middleware"
)

// fakeFleet implements api.RunnerRegistryAPI. It records the lifecycle action it was asked for, which
// is what makes "the verb reached the control plane" an assertion rather than a 200.
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
func TestRunnerLifecycleCLIReachesTheRealRouter(t *testing.T) {
	fleet := &fakeFleet{}
	h := capi.NewRouter(staticVerifier{scope: middleware.Scope{Project: "prj_1",
		Scopes: grantSystemAlongside(nil)}},
		nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, fakeProv{}, nil, capi.SSEConfig{}, nil, nil,
		capi.WithRunners(fleet))
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	t.Setenv("PALAI_BASE_URL", srv.URL)
	t.Setenv("PALAI_API_KEY", "bootstrap-admin-key")

	for _, tc := range []struct {
		args       []string
		wantAction string
		wantOut    string
	}{
		{args: []string{"list"}, wantAction: "", wantOut: `"rnr_one"`},
		{args: []string{"cordon", "rnr_one"}, wantAction: "cordon", wantOut: `"cordoned"`},
		{args: []string{"resume", "rnr_one"}, wantAction: "resume", wantOut: `"active"`},
		{args: []string{"revoke", "rnr_one"}, wantAction: "revoke", wantOut: `"revoked"`},
		// E24 T6: the waiting room's door, through the same real router. A machine admitted by a human reads
		// back `active`, which is what tells the operator it can now take work.
		{args: []string{"approve", "rnr_one"}, wantAction: "approve", wantOut: `"active"`},
	} {
		t.Run(strings.Join(tc.args, " "), func(t *testing.T) {
			fleet.action, fleet.id = "", ""
			var out bytes.Buffer
			if err := Run("runner", tc.args, &out, strings.NewReader("")); err != nil {
				t.Fatalf("palai admin runner %s: %v — `Revoke()` has been implemented, tested and unreachable since it was written (§3.6 D15)", strings.Join(tc.args, " "), err)
			}
			if !strings.Contains(out.String(), tc.wantOut) {
				t.Errorf("output %q missing %q", out.String(), tc.wantOut)
			}
			if tc.wantAction != "" {
				if fleet.action != tc.wantAction {
					t.Errorf("the control plane was asked for %q, want %q", fleet.action, tc.wantAction)
				}
				if fleet.id != "rnr_one" {
					t.Errorf("the control plane was given runner id %q, want rnr_one — a lifecycle call that names no machine is the whole-gateway bool this task removes", fleet.id)
				}
			}
		})
	}
}

// TestRunnerLifecycleRejectsASecondPositional keeps the arity guard honest for the new resource, for the
// reason every other one has it: an extra positional is never silently dropped.
func TestRunnerLifecycleRejectsASecondPositional(t *testing.T) {
	t.Setenv("PALAI_BASE_URL", "http://127.0.0.1:1")
	t.Setenv("PALAI_API_KEY", "k")
	var out bytes.Buffer
	if err := Run("runner", []string{"revoke", "rnr_one", "rnr_two"}, &out, strings.NewReader("")); err == nil {
		t.Fatal("`runner revoke rnr_one rnr_two` was accepted; the second id would be silently dropped")
	}
}

// TestRunnerLifecycleNeedsTheProvisionCapability is the authorization half. Cordoning a machine takes it
// out of service and revoking one decommissions it; both are org-admin actions, so a key without
// `provision` must be refused by the real gate — and `list`, a read, must not be.
func TestRunnerLifecycleNeedsTheProvisionCapability(t *testing.T) {
	fleet := &fakeFleet{}
	h := capi.NewRouter(staticVerifier{scope: middleware.Scope{Project: "prj_1",
		Scopes: grantSystemAlongside([]string{"run"})}},
		nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, fakeProv{}, nil, capi.SSEConfig{}, nil, nil,
		capi.WithRunners(fleet))
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	t.Setenv("PALAI_BASE_URL", srv.URL)
	t.Setenv("PALAI_API_KEY", "run-only-key")

	var out bytes.Buffer
	if err := Run("runner", []string{"revoke", "rnr_one"}, &out, strings.NewReader("")); err == nil {
		t.Fatal("a run-only key revoked a runner: revoking a machine is an org-admin action")
	}
	if fleet.action != "" {
		t.Fatalf("the store was asked for %q by an unauthorized key", fleet.action)
	}
	out.Reset()
	if err := Run("runner", []string{"list"}, &out, strings.NewReader("")); err != nil {
		t.Fatalf("a run-only key could not LIST runners: %v — the read surface is not gated on provision", err)
	}
}

// TestRunnerLifecycleRevokeIsRenderedAsIrreversible is the operator-facing half of "revoke is
// irreversible". The CLI is a thin client and renders what it is given, so what this pins is that the
// response carries the decommissioned state rather than a bare 200 an operator has to interpret.
func TestRunnerLifecycleRevokeIsRenderedAsIrreversible(t *testing.T) {
	fleet := &fakeFleet{}
	h := capi.NewRouter(staticVerifier{scope: middleware.Scope{Project: "prj_1",
		Scopes: grantSystemAlongside(nil)}},
		nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, fakeProv{}, nil, capi.SSEConfig{}, nil, nil,
		capi.WithRunners(fleet))
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	t.Setenv("PALAI_BASE_URL", srv.URL)
	t.Setenv("PALAI_API_KEY", "bootstrap-admin-key")

	var out bytes.Buffer
	if err := Run("runner", []string{"revoke", "rnr_one", "--json"}, &out, strings.NewReader("")); err != nil {
		t.Fatalf("revoke: %v", err)
	}
	var body map[string]any
	if err := json.Unmarshal(out.Bytes(), &body); err != nil {
		t.Fatalf("decode the revoke response %q: %v", out.String(), err)
	}
	if body["state"] != "revoked" {
		t.Fatalf("revoke response state = %v, want revoked", body["state"])
	}
}
