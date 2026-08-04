//go:build component

package execution_test

// A POOL'S BIRTH PATH (E28 T1), against a REAL Postgres, the REAL router, the REAL gateway and a real
// machine — and every step below is taken through the PUBLIC API, because the measurement this task exists
// for is that none of them could be.
//
// WHAT WAS MEASURED ON `main` @ f8a53532 (2026-07-31):
//
//   - `POST /v1/runner-pools` is not in the route table (`api/router.go:359-392`). The only statement that
//     writes a pool row is `InsertDefaultRunnerPool`, and it writes `'default'`, `'sandboxed-linux'` and
//     `false` as LITERALS. `grep -rn "UPDATE runner_pools" … | grep -v _test | wc -l` → 0.
//   - So a second pool cannot exist, an `unsandboxed-host` (rented Mac) pool cannot exist, and
//     `strict_enrollment` cannot be switched on. The two places that switch it on are two TEST FILES
//     issuing raw SQL.
//   - Which makes E24 T6's `POST /v1/runners/{runner_id}/approve` — correctly written and correctly gated
//     on `approve` rather than `provision` — the decider of a state NO OPERATOR CAN PRODUCE. `FLT-P12`
//     says *"the waiting room exists and nothing is in it unless an operator asks"*; the operator could
//     not ask.
//
// THE CROWN TEST BELOW IS THE FIRST PROOF THAT THE APPROVE ROUTE PASSES THROUGH A STATE AN OPERATOR
// PRODUCED. Everything before the approval is done with a bearer token over the shipped mux.
//
// Every test here is named `TestPoolBirth*` on purpose: scripts/test/component's postgres leg is an
// ALLOW-LIST, and a component test whose name it does not match never runs and reports the same green as
// one that passes.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/palgroup/palai/apps/control-plane/api/middleware"
	"github.com/palgroup/palai/apps/control-plane/internal/execution"
	"github.com/palgroup/palai/apps/control-plane/internal/fleet"
	"github.com/palgroup/palai/packages/contracts"
	"github.com/palgroup/palai/packages/coordinator"
	"github.com/palgroup/palai/packages/runner"
	"github.com/palgroup/palai/storage"
)

// birthFixture is the placement fixture plus the two things an OPERATOR needs and a machine does not: the
// shipped router with the fleet registry mounted, and a real `api_keys` row to present to it.
type birthFixture struct {
	*poolKeyFixture
	server *httptest.Server
	admin  string // provision + approve
	runKey string // run only — the capability gate's negative side
	org    string
	prj    string
}

func newBirthFixture(t *testing.T) *birthFixture {
	t.Helper()
	f := newPlacementFixture(t)
	org, prj := poolKeyID("org"), poolKeyID("prj")
	sys := storage.WithSystemScope(context.Background())
	admin, runOnly := "sk_birth_admin_"+poolKeyID("v"), "sk_birth_run_"+poolKeyID("v")
	principal := "prin_birth_" + org
	stmts := [][]any{
		{`INSERT INTO organizations (id) VALUES ($1)`, org},
		{`INSERT INTO projects (id, organization_id) VALUES ($1, $2)`, prj, org},
		{`INSERT INTO principals (id, organization_id, project_id, kind) VALUES ($1,$2,$3,'service')`, principal, org, prj},
		// The operator's key carries every capability the routes below check and neither is the empty
		// set: an empty `scopes` means every ordinary capability (api/middleware/auth.go:30), but
		// `system` is deliberately excluded from that rule (Faz A.1 Task 3 — HasSystem never treats an
		// empty set as unrestricted), so it must be named explicitly alongside `provision`/`approve` or
		// the fleet's systemOnly gate refuses this key before any of the assertions below are reached.
		{`INSERT INTO api_keys (id, organization_id, project_id, principal_id, key_hash, scopes)
		  VALUES ($1,$2,$3,$4,$5,$6)`, "key_birth_adm_" + org, org, prj, principal,
			coordinator.HashAPIKey(admin), []string{middleware.ScopeSystem, "provision", "approve"}},
		// This key still carries `system` — it must clear the fleet's systemOnly gate to REACH the
		// `provision` check TestPoolBirthIsProvisionGatedAndTenantScoped is actually about. Without
		// `system` it would be refused at the outer gate for the wrong reason and the test would pass
		// without ever exercising the capability it names.
		{`INSERT INTO api_keys (id, organization_id, project_id, principal_id, key_hash, scopes)
		  VALUES ($1,$2,$3,$4,$5,$6)`, "key_birth_run_" + org, org, prj, principal,
			coordinator.HashAPIKey(runOnly), []string{middleware.ScopeSystem, "run"}},
	}
	for _, stmt := range stmts {
		if _, err := f.pool.Exec(sys, stmt[0].(string), stmt[1:]...); err != nil {
			t.Fatalf("seed the operator's tenant: %v", err)
		}
	}
	registry := fleet.NewRegistryAPI(f.registry, f.keys).WithLifecycle(f.gateway)
	return &birthFixture{
		poolKeyFixture: f, server: poolKeyRouter(t, registry),
		admin: admin, runKey: runOnly, org: org, prj: prj,
	}
}

// call issues one authenticated request against the shipped router and returns the status and body. Nothing
// here interprets the body: every assertion below reads the JSON the operator would.
func (b *birthFixture) call(t *testing.T, method, path, bearer string, payload any) (int, []byte) {
	t.Helper()
	var body io.Reader
	if payload != nil {
		raw, err := json.Marshal(payload)
		if err != nil {
			t.Fatalf("marshal %s %s body: %v", method, path, err)
		}
		body = bytes.NewReader(raw)
	}
	req, err := http.NewRequest(method, b.server.URL+path, body)
	if err != nil {
		t.Fatalf("build %s %s: %v", method, path, err)
	}
	req.Header.Set("Authorization", "Bearer "+bearer)
	if payload != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, raw
}

// createPool is the step that did not exist. It returns the decoded projection so a caller can assert the
// SHAPE as well as the id.
func (b *birthFixture) createPool(t *testing.T, name, posture string, strict bool) map[string]any {
	t.Helper()
	status, raw := b.call(t, http.MethodPost, "/v1/runner-pools", b.admin, map[string]any{
		"name": name, "posture": posture, "os": "darwin", "arch": "arm64", "strict_enrollment": strict,
	})
	if status != http.StatusCreated {
		t.Fatalf("POST /v1/runner-pools = %d, want 201: %s", status, raw)
	}
	var view map[string]any
	if err := json.Unmarshal(raw, &view); err != nil {
		t.Fatalf("decode the create response %q: %v", raw, err)
	}
	return view
}

// mintKeyOverTheAPI mints a pool enrolment key through the public route, so the machine below is admitted
// by a credential an operator could have produced rather than by a store call a test made.
func (b *birthFixture) mintKeyOverTheAPI(t *testing.T, poolID string) string {
	t.Helper()
	status, raw := b.call(t, http.MethodPost, "/v1/runner-pools/"+poolID+"/keys", b.admin, map[string]any{})
	if status != http.StatusCreated {
		t.Fatalf("POST /v1/runner-pools/%s/keys = %d, want 201: %s", poolID, status, raw)
	}
	var view struct {
		Key string `json:"key"`
	}
	if err := json.Unmarshal(raw, &view); err != nil {
		t.Fatalf("decode the mint response: %v", err)
	}
	if view.Key == "" {
		t.Fatal("the mint response carried no key value; the create response is the only place it exists")
	}
	return view.Key
}

// TestPoolBirthReachesTheWaitingRoomAndTheApproveRoute is RED (1) and the crown.
//
// ONE MACHINE'S WHOLE LIFE, and every operator step of it over the public API: a pool is created with the
// posture no code path could write, its waiting room is opened with the switch no code path could flip, a
// key is minted, a real machine enrols and is held OUT of the rendezvous, the approve route admits it, and
// the SAME machine then relays a run's traffic through the lease it is given.
//
// THE FINAL STEP IS THE ENGINE'S TRAFFIC, NOT A SHELL CALL, and the distinction is `FLT-P15` rather than a
// weakening: a `lease.offer` carries an `image_digest` — the ENGINE — and every TOOL still executes in the
// control plane's own process. There is no shell call to send down this lease, and asserting one would
// assert a relay this tree has not built.
func TestPoolBirthReachesTheWaitingRoomAndTheApproveRoute(t *testing.T) {
	b := newBirthFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	view := b.createPool(t, "mac-pool", "unsandboxed-host", true)
	poolID, _ := view["id"].(string)
	if poolID == "" {
		t.Fatalf("the create response carried no id: %+v", view)
	}
	for field, want := range map[string]any{
		"object": "runner_pool", "name": "mac-pool", "posture": "unsandboxed-host",
		"os": "darwin", "arch": "arm64", "strict_enrollment": true,
	} {
		if view[field] != want {
			t.Errorf("create response %s = %v, want %v", field, view[field], want)
		}
	}
	// THE ROW, not the response. A projection that reported a posture the database did not hold would place
	// runs by one value and answer an operator with another.
	var posture string
	var strict bool
	if err := b.pool.QueryRow(storage.WithSystemScope(ctx),
		`SELECT posture, strict_enrollment FROM runner_pools WHERE id = $1`, poolID).Scan(&posture, &strict); err != nil {
		t.Fatalf("read the created pool row: %v", err)
	}
	if posture != "unsandboxed-host" || !strict {
		t.Fatalf("runner_pools row = (%q, strict=%v), want (unsandboxed-host, strict=true)", posture, strict)
	}

	key := b.mintKeyOverTheAPI(t, poolID)
	identity, err := b.enrolAsking(ctx, key, poolID)
	if err != nil {
		t.Fatalf("enrol into the created strict pool: %v", err)
	}
	id := runnerIDOf(t, identity)
	if got := runnerState(t, b.poolKeyFixture, id); got != "pending" {
		t.Fatalf("runners.state = %q after enrolling into a pool created with strict_enrollment=true, want pending — the switch reached the row but not the door", got)
	}
	// The machine inherited the pool's posture, which is what makes the pool's posture MEAN anything: a
	// created pool that did not stamp its machines would be a label.
	var machinePosture string
	if err := b.pool.QueryRow(storage.WithSystemScope(ctx),
		`SELECT posture FROM runners WHERE id = $1`, id).Scan(&machinePosture); err != nil {
		t.Fatalf("read the enrolled machine's posture: %v", err)
	}
	if machinePosture != "unsandboxed-host" {
		t.Fatalf("the machine enrolled as %q into an unsandboxed-host pool", machinePosture)
	}

	// THE MACHINE PARKS ONCE AND STAYS PARKED, which is what a real runner's loop does — and it is ONE park
	// rather than a park now and a lease session later, because two waiters on one machine are two competing
	// consumers of the same offer. The first draft of this test held both (`parkIdentity` for the pending
	// phase and `OpenLease` for the relay) and passed in isolation twice; under the full postgres tier it
	// failed with "the approved machine never obtained the lease", because the OTHER waiter had taken it.
	// A race that only shows under load is still a race, so the shape is corrected rather than retried.
	sessCtx, cancelSession := context.WithCancel(ctx)
	defer cancelSession()
	leaseReady := make(chan *runner.LeaseSession, 1)
	go func() {
		if ls, err := b.session(identity).OpenLease(sessCtx); err == nil {
			leaseReady <- ls
		}
	}()
	waitConnected(t, b.gateway, 1)
	tenant := coordinator.Tenant{Project: b.prj}
	starved, cancelStarved := context.WithTimeout(ctx, 3*time.Second)
	defer cancelStarved()
	if _, err := b.gateway.Dial(starved, b.tenantAttempt(tenant, poolID, "run_birth", "att_birth")); !errors.Is(err, execution.ErrPoolHasNoRunner) {
		t.Fatalf("Dial into the created strict pool = %v, want ErrPoolHasNoRunner — a waiting room a Dial can reach is not a waiting room", err)
	}
	select {
	case <-leaseReady:
		t.Fatal("the pending machine was offered a lease: strict enrolment a Dial can still reach is not a waiting room")
	default:
	}

	// AND THE COUNTER HAS A READER (FLT-P14). An attempt is left QUEUED against the pool — a Dial that has
	// not given up — and the depth is read back through the AUTHENTICATED pool list, the placement the gap
	// row names and not /healthz/runner.
	//
	// IT ASSERTS A NUMBER, NOT A FIELD. A `waiting` that rendered a constant 0 would satisfy "the field
	// exists" and would leave the question the counter was written to answer exactly as unanswered as it
	// was, so the assertion is made while something IS waiting and again after nothing is.
	queued, cancelQueued := context.WithCancel(ctx)
	go func() { _, _ = b.gateway.Dial(queued, b.tenantAttempt(tenant, poolID, "run_birth_q", "att_birth_q")) }()
	waiting := b.waitFor(t, poolID, func(n int64) bool { return n >= 1 })
	if waiting == nil {
		t.Fatal("the pool projection never reported a queued attempt: `RunnerGateway.Waiting(poolID)` has counted them since E24 and nothing has ever read it (FLT-P14)")
	}
	cancelQueued()
	if drained := b.waitFor(t, poolID, func(n int64) bool { return n == 0 }); drained == nil {
		t.Fatal("the pool's `waiting` never came back down: a counter that only rises is not the queue depth")
	}

	// THE HUMAN, through the route gated on `approve`. This is the first time in this tree that the route
	// has been reached over HTTP with a pending machine an operator's own pool produced.
	status, raw := b.call(t, http.MethodPost, "/v1/runners/"+id+"/approve", b.admin, nil)
	if status != http.StatusOK {
		t.Fatalf("POST /v1/runners/%s/approve = %d, want 200: %s", id, status, raw)
	}
	var admitted map[string]any
	if err := json.Unmarshal(raw, &admitted); err != nil {
		t.Fatalf("decode the approve response: %v", err)
	}
	if admitted["state"] != "active" {
		t.Fatalf("approve response state = %v, want active: %s", admitted["state"], raw)
	}
	if got := runnerState(t, b.poolKeyFixture, id); got != "active" {
		t.Fatalf("runners.state = %q after the approve route, want active", got)
	}

	// THE SAME MACHINE, over the connection it has been holding the whole time, now takes the lease and a
	// run's traffic passes THROUGH it — which is what makes the approval reach the machine's own session
	// rather than only its row.
	attempt := b.tenantAttempt(tenant, poolID, "run_birth_ok", "att_birth_ok")
	ch, err := b.gateway.Dial(ctx, attempt)
	if err != nil {
		t.Fatalf("Dial after the approval = %v, want a lease", err)
	}
	defer ch.Close()
	var lease *runner.LeaseSession
	select {
	case lease = <-leaseReady:
	case <-time.After(20 * time.Second):
		t.Fatal("the approved machine never obtained the lease")
	}
	frame := contracts.EngineFrame{
		Protocol: "engine.v1", ID: "frm_birth1", Type: "engine.ready", Sequence: 1,
		Time: time.Now().UTC().Format(time.RFC3339), RunID: "run_birth_ok",
	}
	if err := lease.SendEngineFrame(ctx, frame); err != nil {
		t.Fatalf("the admitted machine could not send a frame: %v", err)
	}
	relayed, err := ch.Receive(ctx)
	if err != nil {
		t.Fatalf("nothing reached the attempt over the admitted machine's lease: %v", err)
	}
	if relayed.ID != frame.ID {
		t.Fatalf("relayed frame = %q, want %q", relayed.ID, frame.ID)
	}
}

// waitFor polls the AUTHENTICATED pool list until the named pool's `waiting` satisfies want, and returns
// it — or nil if it never did. Polling rather than reading once because the queue depth is moved by a Dial
// parked in another goroutine, and a single read would be a race dressed as an assertion.
func (b *birthFixture) waitFor(t *testing.T, poolID string, want func(int64) bool) *int64 {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		for _, row := range b.listPools(t, b.admin) {
			if row["id"] != poolID {
				continue
			}
			raw, ok := row["waiting"].(float64)
			if !ok {
				// The field is absent, which on this surface means the gateway could not be asked. A fixture
				// that wired one and got no answer is a failure, not a retry.
				t.Fatalf("pool %s renders no numeric `waiting`: %v", poolID, row["waiting"])
			}
			if n := int64(raw); want(n) {
				return &n
			}
		}
		if time.Now().After(deadline) {
			return nil
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// listPools reads GET /v1/runner-pools as the operator does and returns the rendered rows.
func (b *birthFixture) listPools(t *testing.T, bearer string) []map[string]any {
	t.Helper()
	status, raw := b.call(t, http.MethodGet, "/v1/runner-pools?limit=100", bearer, nil)
	if status != http.StatusOK {
		t.Fatalf("GET /v1/runner-pools = %d: %s", status, raw)
	}
	var page struct {
		Data []map[string]any `json:"data"`
	}
	if err := json.Unmarshal(raw, &page); err != nil {
		t.Fatalf("decode the pool page: %v", err)
	}
	return page.Data
}

// TestPoolBirthRendersTheSameShapeAsTheListing is the projection claim, and it is not cosmetic: a create
// that answered in a different shape from the read would make every consumer code against two shapes, and
// the first one to diverge would do so silently.
func TestPoolBirthRendersTheSameShapeAsTheListing(t *testing.T) {
	b := newBirthFixture(t)
	created := b.createPool(t, "shape-pool", "unsandboxed-host", false)
	var listed map[string]any
	for _, row := range b.listPools(t, b.admin) {
		if row["id"] == created["id"] {
			listed = row
		}
	}
	if listed == nil {
		t.Fatalf("the created pool %v is not in GET /v1/runner-pools", created["id"])
	}
	// The listing carries a live `waiting` this create response cannot: a pool that does not exist yet has
	// no queue. Compared key by key over the STORED fields, so a field added to one and not the other fails.
	for _, field := range []string{"id", "object", "name", "posture", "os", "arch", "strict_enrollment", "created_at"} {
		if created[field] != listed[field] {
			t.Errorf("create[%s] = %v, list[%s] = %v — one shape or two", field, created[field], field, listed[field])
		}
	}
	for field := range created {
		if _, ok := listed[field]; !ok {
			t.Errorf("the create response carries %q and the listing does not", field)
		}
	}
}

// TestPoolBirthRefusesADuplicateNameAsAConflictNotAnError turns 000045's UNIQUE index into an answer an
// operator can act on. A 500 here would be the tree telling somebody their control plane is broken when
// what happened is that they typed a name twice.
func TestPoolBirthRefusesADuplicateNameAsAConflictNotAnError(t *testing.T) {
	b := newBirthFixture(t)
	b.createPool(t, "twice", "sandboxed-linux", false)
	status, raw := b.call(t, http.MethodPost, "/v1/runner-pools", b.admin, map[string]any{
		"name": "twice", "posture": "sandboxed-linux",
	})
	if status != http.StatusConflict {
		t.Fatalf("a second pool named `twice` = %d, want 409: %s", status, raw)
	}
	var problem struct{ Code string }
	_ = json.Unmarshal(raw, &problem)
	if problem.Code != "already_exists" {
		t.Errorf("duplicate-name refusal code = %q, want already_exists", problem.Code)
	}
	// AND ANOTHER TENANT MAY USE THE SAME NAME: the index is (organization_id, project_id, name), so a 409
	// that fired across tenants would be a cross-tenant disclosure wearing a validation error's clothes.
	other := newBirthFixture(t)
	other.createPool(t, "twice", "sandboxed-linux", false)
}

// TestPoolBirthValidatesThePostureOnTheRouteRatherThanAtTheDatabase is the CHECK-is-the-last-defence claim.
// 000045's constraint would refuse an unknown posture with a 23514 the handler would render as a 500; the
// route refuses it as bad input, names it, and never opens a transaction.
func TestPoolBirthValidatesThePostureOnTheRouteRatherThanAtTheDatabase(t *testing.T) {
	b := newBirthFixture(t)
	for _, tc := range []struct {
		body map[string]any
		why  string
	}{
		{map[string]any{"name": "bad", "posture": "macos"}, "a posture outside the two 000045 declares"},
		{map[string]any{"name": "bad"}, "no posture at all — a pool IS a posture, so there is no default to fall back on"},
		{map[string]any{"posture": "sandboxed-linux"}, "no name — the uniqueness index is on a name, and an empty one is a name"},
		{map[string]any{"name": "bad", "posture": "sandboxed-linux", "capacity": 4}, "an unknown field: DisallowUnknownFields, so a knob that does not exist is refused rather than ignored"},
	} {
		status, raw := b.call(t, http.MethodPost, "/v1/runner-pools", b.admin, tc.body)
		if status != http.StatusBadRequest {
			t.Errorf("POST /v1/runner-pools %v = %d, want 400 (%s): %s", tc.body, status, tc.why, raw)
		}
	}
	// Nothing was written by any of the four.
	if pools := b.listPools(t, b.admin); len(pools) != 0 {
		t.Fatalf("a refused create left %d pool(s) behind", len(pools))
	}
}

// TestPoolBirthStrictSwitchIsTheOnlyPatchableField is the correctness requirement §5 and fleet/store.go:110
// state together: a machine INHERITS its pool's posture at enrolment, so changing a populated pool's
// posture would retroactively change what its machines ARE. The switch an operator needs is the waiting
// room; the value they must not be able to move is the posture.
func TestPoolBirthStrictSwitchIsTheOnlyPatchableField(t *testing.T) {
	b := newBirthFixture(t)
	created := b.createPool(t, "switchable", "sandboxed-linux", false)
	poolID := created["id"].(string)

	status, raw := b.call(t, http.MethodPatch, "/v1/runner-pools/"+poolID, b.admin, map[string]any{"strict_enrollment": true})
	if status != http.StatusOK {
		t.Fatalf("PATCH strict_enrollment = %d, want 200: %s", status, raw)
	}
	var view map[string]any
	_ = json.Unmarshal(raw, &view)
	if view["strict_enrollment"] != true {
		t.Fatalf("PATCH response strict_enrollment = %v, want true", view["strict_enrollment"])
	}
	var strict bool
	if err := b.pool.QueryRow(storage.WithSystemScope(context.Background()),
		`SELECT strict_enrollment FROM runner_pools WHERE id = $1`, poolID).Scan(&strict); err != nil {
		t.Fatalf("read the patched row: %v", err)
	}
	if !strict {
		t.Fatal("the PATCH answered 200 and the row is still non-strict")
	}
	// And it closes as well as it opens: a switch that could only be turned on would leave an operator who
	// opened one by accident with no way back.
	if status, raw := b.call(t, http.MethodPatch, "/v1/runner-pools/"+poolID, b.admin, map[string]any{"strict_enrollment": false}); status != http.StatusOK {
		t.Fatalf("PATCH strict_enrollment=false = %d, want 200: %s", status, raw)
	}

	for _, tc := range []struct {
		body map[string]any
		why  string
	}{
		{map[string]any{"posture": "unsandboxed-host"}, "posture is NOT patchable: fleet/store.go:110 makes a machine inherit the pool's posture at enrolment, so moving it would retroactively change what the machines in it ARE"},
		{map[string]any{"name": "renamed"}, "name is not patchable here; nothing asked for it and a rename is its own decision"},
		{map[string]any{"strict_enrollment": true, "os": "linux"}, "one unknown field alongside a known one is still an unknown field"},
		{map[string]any{}, "an empty body names no field: a PATCH that silently does nothing reads as a PATCH that worked"},
	} {
		status, raw := b.call(t, http.MethodPatch, "/v1/runner-pools/"+poolID, b.admin, tc.body)
		if status != http.StatusBadRequest {
			t.Errorf("PATCH %v = %d, want 400 — %s: %s", tc.body, status, tc.why, raw)
		}
	}
	var posture string
	if err := b.pool.QueryRow(storage.WithSystemScope(context.Background()),
		`SELECT posture FROM runner_pools WHERE id = $1`, poolID).Scan(&posture); err != nil {
		t.Fatalf("re-read the pool's posture: %v", err)
	}
	if posture != "sandboxed-linux" {
		t.Fatalf("the pool's posture is now %q; a refused PATCH moved it anyway", posture)
	}
}

// TestPoolBirthIsProvisionGatedAndTenantScoped is the security half, and it is two claims: a key without
// `provision` writes nothing, and a key from another tenant cannot see, create into or PATCH this tenant's
// pool. The second is asserted through the ROUTES rather than through the store, because the routes are
// what a stranger reaches.
func TestPoolBirthIsProvisionGatedAndTenantScoped(t *testing.T) {
	b := newBirthFixture(t)
	mine := b.createPool(t, "mine", "unsandboxed-host", false)
	poolID := mine["id"].(string)

	for _, tc := range []struct {
		method, path string
		body         any
	}{
		{http.MethodPost, "/v1/runner-pools", map[string]any{"name": "sneaky", "posture": "unsandboxed-host"}},
		{http.MethodPatch, "/v1/runner-pools/" + poolID, map[string]any{"strict_enrollment": true}},
	} {
		status, raw := b.call(t, tc.method, tc.path, b.runKey, tc.body)
		if status != http.StatusForbidden {
			t.Errorf("%s %s with a run-only key = %d, want 403: %s", tc.method, tc.path, status, raw)
		}
	}

	// THE FOREIGN TENANT. A second fixture is a second organization with its own key against the SAME
	// database, which is the shape a shared component database makes easy to get wrong: an assertion that
	// swept every tenant would pass here for the wrong reason, so both directions name the id.
	stranger := newBirthFixture(t)
	for _, row := range stranger.listPools(t, stranger.admin) {
		if row["id"] == poolID {
			t.Fatalf("a foreign tenant's pool list carries %v", poolID)
		}
	}
	if status, raw := stranger.call(t, http.MethodPatch, "/v1/runner-pools/"+poolID, stranger.admin,
		map[string]any{"strict_enrollment": true}); status != http.StatusNotFound {
		t.Fatalf("a foreign tenant's PATCH of %s = %d, want 404: %s", poolID, status, raw)
	}
	var strict bool
	if err := b.pool.QueryRow(storage.WithSystemScope(context.Background()),
		`SELECT strict_enrollment FROM runner_pools WHERE id = $1`, poolID).Scan(&strict); err != nil {
		t.Fatalf("re-read the pool: %v", err)
	}
	if strict {
		t.Fatal("a foreign tenant's PATCH answered 404 and opened the waiting room anyway")
	}
	// And a stranger's create lands in the STRANGER's tenant, not in a tenant they named.
	strangerPool := stranger.createPool(t, "mine", "sandboxed-linux", false)
	var owner string
	if err := b.pool.QueryRow(storage.WithSystemScope(context.Background()),
		`SELECT organization_id FROM runner_pools WHERE id = $1`, strangerPool["id"]).Scan(&owner); err != nil {
		t.Fatalf("read the stranger's pool: %v", err)
	}
	if owner != stranger.org {
		t.Fatalf("a create by %s landed in organization %s", stranger.org, owner)
	}
}

// TestPoolBirthLeavesTheBornWithPoolBitUnchanged is RED (3) at the tier that can see a row: a tenant born
// through the SHIPPED provisioning statement still gets exactly one pool, named `default`, posture
// `sandboxed-linux`, non-strict — and creating pools beside it does not touch it.
//
// The two rows are created HERE rather than counted across the table, for the reason
// TestStrictIsOffOnEveryPoolThisTreeCreates gives: a `count(*)` over a shared component database is
// answered by whatever the other tests in this package have just seeded.
func TestPoolBirthLeavesTheBornWithPoolBitUnchanged(t *testing.T) {
	b := newBirthFixture(t)
	ctx := storage.WithSystemScope(context.Background())
	born := poolKeyID("pool")
	if _, err := b.pool.Exec(ctx, storage.Query("InsertDefaultRunnerPool"), born, b.org, b.prj); err != nil {
		t.Fatalf("provision a tenant's default pool: %v", err)
	}
	before := poolRow(t, b.pool, born)

	// Everything T1 adds, against the same tenant.
	created := b.createPool(t, "mac-pool", "unsandboxed-host", true)
	if status, raw := b.call(t, http.MethodPatch, "/v1/runner-pools/"+created["id"].(string), b.admin,
		map[string]any{"strict_enrollment": false}); status != http.StatusOK {
		t.Fatalf("PATCH: %d %s", status, raw)
	}

	after := poolRow(t, b.pool, born)
	if before != after {
		t.Fatalf("the pool this tenant was BORN with changed from %+v to %+v — every installation alive today has this row, and T1 must add a second one rather than move it", before, after)
	}
	if before.name != "default" || before.posture != "sandboxed-linux" || before.strict {
		t.Fatalf("InsertDefaultRunnerPool produced %+v, want {default sandboxed-linux false}", before)
	}
}

// bornPool is the four columns the seed decides.
type bornPool struct {
	name, posture, os, arch string
	strict                  bool
}

func poolRow(t *testing.T, pool *pgxpool.Pool, id string) bornPool {
	t.Helper()
	var row bornPool
	if err := pool.QueryRow(storage.WithSystemScope(context.Background()),
		`SELECT name, posture, os, arch, strict_enrollment FROM runner_pools WHERE id = $1`, id).
		Scan(&row.name, &row.posture, &row.os, &row.arch, &row.strict); err != nil {
		t.Fatalf("read pool %s: %v", id, err)
	}
	return row
}
