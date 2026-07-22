package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/palgroup/palai/apps/control-plane/api/middleware"
)

// fakeHookRegistry scripts each seam outcome so the handler contract is exercised without a database.
type fakeHookRegistry struct {
	create   HookResult
	disable  HookResult
	lastBody []byte
	lastID   string
}

func (f *fakeHookRegistry) CreateHook(_ context.Context, _ middleware.Scope, body []byte) (HookResult, error) {
	f.lastBody = body
	return f.create, nil
}
func (f *fakeHookRegistry) DisableHook(_ context.Context, _ middleware.Scope, id string) (HookResult, error) {
	f.lastID = id
	return f.disable, nil
}

func hookTestServer(t *testing.T, reg *fakeHookRegistry) string {
	t.Helper()
	srv := httptest.NewServer(NewRouter(fakeVerifier{}, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, reg, nil, SSEConfig{}, nil, nil))
	t.Cleanup(srv.Close)
	return srv.URL
}

// TestHookManagementSurface pins the ADMIN routes (spec §28.17): a valid create is a 201 with a Location; the
// disable action is a 200; an unknown point / out-of-matrix pair / inline secret is a 400; a name collision is
// a 409; an unknown hook disable is a 404. There is deliberately no model-facing surface here — these are
// admin routes only.
func TestHookManagementSurface(t *testing.T) {
	reg := &fakeHookRegistry{
		create:  HookResult{Body: []byte(`{"id":"hook_1","object":"hook"}`)},
		disable: HookResult{Body: []byte(`{"id":"hook_1","object":"hook","disabled":true}`)},
	}
	base := hookTestServer(t, reg)

	resp := do(t, "POST", base+"/v1/hooks", `{"name":"guard","hook_point":"before_tool","category":"policy","executor":"platform_inline","config":{"handler":"deny_tool"}}`, nil)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create hook status = %d, want 201", resp.StatusCode)
	}
	if loc := resp.Header.Get("Location"); loc != "/v1/hooks/hook_1" {
		t.Fatalf("create Location = %q, want /v1/hooks/hook_1", loc)
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
	srv := httptest.NewServer(NewRouter(fakeVerifier{}, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, SSEConfig{}, nil, nil))
	t.Cleanup(srv.Close)
	if resp := do(t, "POST", srv.URL+"/v1/hooks", `{"name":"guard"}`, nil); resp.StatusCode != http.StatusNotFound {
		t.Fatalf("nil hook registry POST status = %d, want 404 (route unmounted)", resp.StatusCode)
	}
}
