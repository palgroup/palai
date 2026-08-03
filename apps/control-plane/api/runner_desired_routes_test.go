package api

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/palgroup/palai/apps/control-plane/api/middleware"
)

// THE TWO SCOPED READ ROUTES, driven against the REAL router.
//
// WHY THIS FILE EXISTS AT ALL, since the console already has a spec for both: the console drives its own
// FAKE control plane, and a fake is an agreement between a test and itself. CLAUDE.md's rule is that a
// value crossing a boundary and then being used to look the thing up again must have its round trip
// verified against the real dependency — here the "real dependency" is the shipped router, and what is
// asserted is the SHAPE the console's fake claims to imitate. Without this, the console and the server can
// diverge and every test on both sides stays green.

// scopedDesired is a DesiredConfigAPI that answers one document per (plane, scope) and records what it was
// asked for, so the test can assert the route passed the PATH's scope through rather than a constant.
type scopedDesired struct {
	docs     map[string]*DesiredDocument
	askedFor []string
}

func (s *scopedDesired) GetDesiredConfig(_ context.Context, scope middleware.Scope, plane, scopeID string) (*DesiredDocument, error) {
	if !scope.HasScope("provision") {
		return nil, io.ErrUnexpectedEOF // any error; the route must not surface it as a document
	}
	s.askedFor = append(s.askedFor, plane+"/"+scopeID)
	return s.docs[plane+"/"+scopeID], nil
}

func (s *scopedDesired) PutDesiredConfig(context.Context, middleware.Scope, []byte) (ProvisionResult, error) {
	return ProvisionResult{}, nil
}

// The registry fake is runner_pools_test.go's fakeRunnerRegistry and the scoped verifier is
// model_routes_test.go's scopedVerifier — both reused rather than re-declared, which is the point of a
// package-level test helper. The desired routes do not touch the registry at all; it is wired only because
// the router mounts the fleet block on `cfg.runners != nil`, and a machine's configuration is registered
// beside the routes that report that machine.

func TestTheScopedDesiredRoutesAnswerTheShapeAConsoleReads(t *testing.T) {
	docs := &scopedDesired{docs: map[string]*DesiredDocument{
		"runner_pool/pool_default": {
			Revision: 7, Plane: "runner_pool", Scope: "pool_default",
			Settings:  map[string]string{"PALAI_RUNNER_CONCURRENCY": "3"},
			WrittenAt: time.Unix(1_780_000_000, 0).UTC(), WrittenBy: "prin_local",
		},
	}}
	// A scopedVerifier carrying `system` AND `provision`, not fakeVerifier: these two routes are inside the
	// fleet's systemOnly gate (Faz A.1 Task 3) on top of the pre-existing `provision` check
	// (desiredForScope), and fakeVerifier's key carries neither scope explicitly (its empty Scopes is
	// unrestricted only for the ordinary axis, never for `system` — see middleware/auth_test.go).
	verifier := scopedVerifier{middleware.Scope{Organization: "org_1", Project: "prj_1", Principal: "prin_1",
		Scopes: []string{middleware.ScopeSystem, "provision"}}}
	srv := httptest.NewServer(NewRouter(verifier, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil,
		SSEConfig{}, nil, nil, WithRunners(&fakeRunnerRegistry{}), WithDesiredConfig(docs)))
	defer srv.Close()

	get := func(path string) (int, map[string]any) {
		t.Helper()
		request, _ := http.NewRequest(http.MethodGet, srv.URL+path, nil)
		request.Header.Set("Authorization", "Bearer sk-test")
		response, err := srv.Client().Do(request)
		if err != nil {
			t.Fatalf("GET %s: %v", path, err)
		}
		defer response.Body.Close()
		var body map[string]any
		_ = json.NewDecoder(response.Body).Decode(&body)
		return response.StatusCode, body
	}

	// --- A POOL WITH A DOCUMENT ----------------------------------------------------------------------
	status, body := get("/v1/runner-pools/pool_default/desired")
	if status != http.StatusOK {
		t.Fatalf("GET a pool's desired configuration = %d, want 200", status)
	}
	if body["plane"] != "runner_pool" || body["scope_id"] != "pool_default" {
		t.Errorf("the answer names plane=%v scope_id=%v, want runner_pool/pool_default. A console renders the "+
			"scope off the same object it renders the settings off, so a wrong pair puts one machine's "+
			"configuration under another's heading", body["plane"], body["scope_id"])
	}
	desired, ok := body["desired"].(map[string]any)
	if !ok {
		t.Fatalf("`desired` is %T, want the document object", body["desired"])
	}
	if desired["revision"] != float64(7) {
		t.Errorf("revision = %v, want 7", desired["revision"])
	}
	settings, ok := desired["settings"].(map[string]any)
	if !ok || settings["PALAI_RUNNER_CONCURRENCY"] != "3" {
		t.Errorf("settings = %v, want PALAI_RUNNER_CONCURRENCY=3", desired["settings"])
	}

	// --- A MACHINE WITH NO DOCUMENT ------------------------------------------------------------------
	// NULL IS THE ANSWER AND NOT A 404, and this is the assertion the console's empty form depends on. A
	// 404 would mean "there is no such machine", which this route cannot know and must not imply; null
	// means "nobody has decided anything for this scope", which is what makes the form render empty fields
	// instead of an error.
	status, body = get("/v1/runners/rnr_unconfigured/desired")
	if status != http.StatusOK {
		t.Fatalf("GET an unconfigured machine's desired configuration = %d, want 200 with a null document", status)
	}
	if body["desired"] != nil {
		t.Errorf("`desired` = %v, want null: a scope nobody has written a document for is not an error, and a "+
			"console shown anything else renders a form that claims the panel is in control when it is not", body["desired"])
	}
	if body["plane"] != "runner_machine" || body["scope_id"] != "rnr_unconfigured" {
		t.Errorf("the null answer names plane=%v scope_id=%v, want runner_machine/rnr_unconfigured — the scope must "+
			"travel with the absence, or a form cannot tell which machine it is about to configure",
			body["plane"], body["scope_id"])
	}

	// --- THE SCOPE CAME FROM THE PATH ----------------------------------------------------------------
	// GET /v1/deployment hardcodes (control_plane, "") — deliberately, see its own comment — so a route
	// that had inherited that constant would answer both requests above with the control plane's document
	// and look entirely correct from the outside. What proves it did not is WHAT THE STORE WAS ASKED.
	want := []string{"runner_pool/pool_default", "runner_machine/rnr_unconfigured"}
	if len(docs.askedFor) != len(want) {
		t.Fatalf("the store was asked %v, want %v", docs.askedFor, want)
	}
	for i, asked := range docs.askedFor {
		if asked != want[i] {
			t.Errorf("read %d asked the store for %q, want %q", i, asked, want[i])
		}
	}
}

// TestTheScopedDesiredRoutesRequireTheProvisionCapability.
//
// They read a document that carries a deployment's admission bounds and its machines' configuration, and
// they are SYSTEM-scoped underneath — deployment_desired has no organization_id and cannot have one. The
// `provision` capability is therefore the only authority between a caller and every scope of it, which is
// what makes this test a security assertion rather than a routing one.
func TestTheScopedDesiredRoutesRequireTheProvisionCapability(t *testing.T) {
	// A NON-EMPTY capability set WITHOUT `provision`. fakeVerifier returns an EMPTY one, which HasScope
	// treats as unrestricted (the admin/bootstrap-key idiom) — so a refusal leg built on it would never
	// refuse and would pass for a reason unrelated to its claim.
	docs := &scopedDesired{docs: map[string]*DesiredDocument{}}
	verifier := scopedVerifier{middleware.Scope{Organization: "org_1", Project: "prj_1", Principal: "prin_1", Scopes: []string{"responses"}}}
	srv := httptest.NewServer(NewRouter(verifier, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil,
		SSEConfig{}, nil, nil, WithRunners(&fakeRunnerRegistry{}), WithDesiredConfig(docs)))
	defer srv.Close()

	for _, path := range []string{"/v1/runner-pools/pool_default/desired", "/v1/runners/rnr_1/desired"} {
		request, _ := http.NewRequest(http.MethodGet, srv.URL+path, nil)
		request.Header.Set("Authorization", "Bearer sk-scoped")
		response, err := srv.Client().Do(request)
		if err != nil {
			t.Fatalf("GET %s: %v", path, err)
		}
		body, _ := io.ReadAll(response.Body)
		response.Body.Close()
		if response.StatusCode != http.StatusForbidden {
			t.Errorf("GET %s with a key lacking `provision` = %d, want 403. This document holds the machine's "+
				"admission bounds and every machine's configuration, and it is system-scoped underneath: the "+
				"capability is the whole boundary. Body: %s", path, response.StatusCode, body)
		}
	}
	if len(docs.askedFor) != 0 {
		t.Errorf("the store was reached %d time(s) by an unscoped caller: %v. The handler must refuse BEFORE the "+
			"read, or the defence-in-depth check in the store is the only thing standing between a project-scoped "+
			"key and a system-scoped document", len(docs.askedFor), docs.askedFor)
	}
}
