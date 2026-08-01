package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/palgroup/palai/apps/control-plane/api/middleware"
)

// fakeHookRegistry scripts each seam outcome so the handler contract is exercised without a database.
type fakeHookRegistry struct {
	create   HookResult
	disable  HookResult
	get      HookResult
	list     []ListRow
	lastBody []byte
	lastID   string
	lastGet  string
	lastList *ListQuery
}

func (f *fakeHookRegistry) CreateHook(_ context.Context, _ middleware.Scope, body []byte) (HookResult, error) {
	f.lastBody = body
	return f.create, nil
}
func (f *fakeHookRegistry) DisableHook(_ context.Context, _ middleware.Scope, id string) (HookResult, error) {
	f.lastID = id
	return f.disable, nil
}
func (f *fakeHookRegistry) GetHook(_ context.Context, _ middleware.Scope, id string) (HookResult, error) {
	f.lastGet = id
	return f.get, nil
}
func (f *fakeHookRegistry) ListHooks(_ context.Context, _ middleware.Scope, q ListQuery) ([]ListRow, error) {
	f.lastList = &q
	return f.list, nil
}

func hookTestServer(t *testing.T, reg *fakeHookRegistry) string {
	t.Helper()
	srv := httptest.NewServer(NewRouter(fakeVerifier{}, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, reg, nil, nil, SSEConfig{}, nil, nil))
	t.Cleanup(srv.Close)
	return srv.URL
}

// TestHookManagementSurface pins the ADMIN routes (spec §28.17): a valid create is a 201 carrying the minted
// id and a Location that RESOLVES (E29 T1 mounted the GET the header names); the
// disable action is a 200; an unknown point / out-of-matrix pair / inline secret is a 400; a name collision is
// a 409; an unknown hook disable is a 404. There is deliberately no model-facing surface here — these are
// admin routes only.
func TestHookManagementSurface(t *testing.T) {
	reg := &fakeHookRegistry{
		create:  HookResult{Body: []byte(`{"id":"hook_1","object":"hook"}`)},
		disable: HookResult{Body: []byte(`{"id":"hook_1","object":"hook","disabled":true}`)},
		get:     HookResult{Body: []byte(`{"id":"hook_1","object":"hook","name":"guard"}`)},
	}
	base := hookTestServer(t, reg)

	resp := do(t, "POST", base+"/v1/hooks", `{"name":"guard","hook_point":"before_tool","category":"policy","executor":"platform_inline","config":{"handler":"deny_tool"}}`, nil)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create hook status = %d, want 201", resp.StatusCode)
	}
	// THE HEADER IS BACK, AND THIS ASSERTION GAINED A DIRECTION RATHER THAN A VALUE. It has now been written
	// three ways: E12 asserted the header was PRESENT (green while it pointed into a 404), E29 T2 asserted it
	// was ABSENT (honest, because nothing served the address), and this asserts it RESOLVES — the only form
	// that could not have been green for the wrong reason in either tree. It is followed against the same
	// router, not compared to a route table.
	loc := resp.Header.Get("Location")
	if loc != "/v1/hooks/hook_1" {
		t.Fatalf("create Location = %q, want /v1/hooks/hook_1", loc)
	}
	if followed := do(t, "GET", base+loc, ``, nil); followed.StatusCode != http.StatusOK {
		t.Fatalf("following the create's own Location %q gave %d, want 200", loc, followed.StatusCode)
	}
	if reg.lastGet != "hook_1" {
		t.Fatalf("following Location reached the store with id %q, want hook_1", reg.lastGet)
	}
	if body := readBody(t, resp); !strings.Contains(body, `"hook_1"`) {
		t.Fatalf("create hook body = %s, want the minted id — the body carries it whether or not a header does", body)
	}

	// An absent hook is a 404 on the singular read, and the list answers in the shared page envelope.
	reg.get = HookResult{NotFound: true}
	if missing := do(t, "GET", base+"/v1/hooks/hook_missing", ``, nil); missing.StatusCode != http.StatusNotFound {
		t.Fatalf("GET unknown hook status = %d, want 404", missing.StatusCode)
	}
	reg.list = []ListRow{{ID: "hook_1", Body: []byte(`{"id":"hook_1","object":"hook","disabled":false}`)}}
	listed := do(t, "GET", base+"/v1/hooks?limit=5", ``, nil)
	if listed.StatusCode != http.StatusOK {
		t.Fatalf("GET /v1/hooks status = %d, want 200", listed.StatusCode)
	}
	if body := readBody(t, listed); !strings.Contains(body, `"data"`) || !strings.Contains(body, `"hook_1"`) {
		t.Fatalf("list body = %s, want the shared page envelope carrying the row", body)
	}
	// The over-fetch is the handler's job, not the store's: the store is asked for limit+1 so renderPage can
	// decide has_more without a second query. A handler that forwarded the bare limit would report has_more
	// false on a page that is exactly full.
	if reg.lastList == nil || reg.lastList.Limit != 6 {
		t.Fatalf("list reached the store with limit %v, want 6 (the +1 over-fetch of ?limit=5)", reg.lastList)
	}
	// ?status= is REFUSED here. disabled_at is a timestamp, not a lifecycle-state column, so hooks is
	// deliberately not in statusFilterKinds and a client must not believe it filtered.
	if bad := do(t, "GET", base+"/v1/hooks?status=disabled", ``, nil); bad.StatusCode != http.StatusBadRequest {
		t.Fatalf("GET /v1/hooks?status=disabled = %d, want 400 (hooks carries no lifecycle-state column)", bad.StatusCode)
	}

	if resp := do(t, "POST", base+"/v1/hooks/hook_1/disable", ``, nil); resp.StatusCode != http.StatusOK {
		t.Fatalf("disable status = %d, want 200", resp.StatusCode)
	}
	if reg.lastID != "hook_1" {
		t.Fatalf("disable id = %q, want hook_1", reg.lastID)
	}

	reg.create = HookResult{BadField: true}
	if resp := do(t, "POST", base+"/v1/hooks", `{"name":"x","hook_point":"before_everything","category":"policy","executor":"platform_inline","config":{}}`, nil); resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("unknown-point status = %d, want 400", resp.StatusCode)
	}
	reg.create = HookResult{Conflict: true}
	if resp := do(t, "POST", base+"/v1/hooks", `{"name":"guard","hook_point":"before_tool","category":"policy","executor":"platform_inline","config":{"handler":"x"}}`, nil); resp.StatusCode != http.StatusConflict {
		t.Fatalf("collision status = %d, want 409", resp.StatusCode)
	}
	reg.disable = HookResult{NotFound: true}
	if resp := do(t, "POST", base+"/v1/hooks/hook_missing/disable", ``, nil); resp.StatusCode != http.StatusNotFound {
		t.Fatalf("unknown-hook disable status = %d, want 404", resp.StatusCode)
	}
}

// TestHookRoutesUnmountedWhenNil proves the nil-seam guard AND the model-facing-absence posture: a tier that
// passes no hook registry mounts no hook route at all (a POST is 404). Hook registration is an admin API
// surface only — there is no model-callable tool for it (the broker exposes no such name).
func TestHookRoutesUnmountedWhenNil(t *testing.T) {
	srv := httptest.NewServer(NewRouter(fakeVerifier{}, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, SSEConfig{}, nil, nil))
	t.Cleanup(srv.Close)
	if resp := do(t, "POST", srv.URL+"/v1/hooks", `{"name":"guard"}`, nil); resp.StatusCode != http.StatusNotFound {
		t.Fatalf("nil hook registry POST status = %d, want 404 (route unmounted)", resp.StatusCode)
	}
	// The two reads unmount with it. A tier that serves no hooks must not serve an empty hook list either —
	// an empty page reads as "this project has no hooks", which is a different claim from "this deployment
	// does not do hooks".
	for _, path := range []string{"/v1/hooks", "/v1/hooks/hook_1"} {
		if resp := do(t, "GET", srv.URL+path, ``, nil); resp.StatusCode != http.StatusNotFound {
			t.Fatalf("nil hook registry GET %s status = %d, want 404 (route unmounted)", path, resp.StatusCode)
		}
	}
}

// TestHookReadsRequireProvisionAndTheShippedWritesDoNot draws the same line schedules draws: the two reads
// E29 T1 added answer what an operator provisioned and carry the gate; the E12 create and kill-switch
// shipped ungated and stay that way, because narrowing a shipped route is a contract change.
func TestHookReadsRequireProvisionAndTheShippedWritesDoNot(t *testing.T) {
	reg := &fakeHookRegistry{
		create:  HookResult{Body: []byte(`{"id":"hook_1","object":"hook"}`)},
		disable: HookResult{Body: []byte(`{"id":"hook_1","object":"hook","disabled":true}`)},
		get:     HookResult{Body: []byte(`{"id":"hook_1","object":"hook"}`)},
	}
	narrow := scopedVerifier{middleware.Scope{Organization: "org_1", Project: "prj_1", Principal: "prin_1", Scopes: []string{"responses"}}}
	srv := httptest.NewServer(NewRouter(narrow, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, reg, nil, nil, SSEConfig{}, nil, nil))
	t.Cleanup(srv.Close)

	for _, path := range []string{"/v1/hooks", "/v1/hooks/hook_1"} {
		if resp := do(t, "GET", srv.URL+path, ``, nil); resp.StatusCode != http.StatusForbidden {
			t.Fatalf("a key without `provision` reading the NEW %s = %d, want 403", path, resp.StatusCode)
		}
	}
	if resp := do(t, "POST", srv.URL+"/v1/hooks", `{"name":"guard"}`, nil); resp.StatusCode != http.StatusCreated {
		t.Fatalf("a key without `provision` on the SHIPPED create = %d, want 201 — retro-gating it is a contract change", resp.StatusCode)
	}
	if resp := do(t, "POST", srv.URL+"/v1/hooks/hook_1/disable", ``, nil); resp.StatusCode != http.StatusOK {
		t.Fatalf("a key without `provision` on the SHIPPED disable = %d, want 200", resp.StatusCode)
	}
}
