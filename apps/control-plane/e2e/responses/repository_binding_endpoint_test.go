//go:build e2e

package responses

// E09 Task 10 review blocker 3 (§24 cross-tenant clone): POST /v1/repository-bindings must reject a
// local-transport clone_url on the production API path, or any API-key holder could register
// clone_url=file:///<PALAI_WORKSPACE_ROOT>/alloc_X/repo and make the control plane clone ANOTHER tenant's
// allocation (same host on the collapsed compose). Only http(s) is accepted at the endpoint; file: (and
// schemeless local paths) are refused unless PALAI_ALLOW_LOCAL_REPOSITORY is set for a dev/test stack.
// The deterministic harness registers its local file remotes through the coordinator spine directly, so
// this endpoint gate never breaks them.

import (
	"net/http"
	"strings"
	"testing"
)

func (h *harness) postBinding(body string) *http.Response {
	h.t.Helper()
	req, err := http.NewRequest(http.MethodPost, h.base+"/v1/repository-bindings", strings.NewReader(body))
	if err != nil {
		h.t.Fatalf("build POST: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+h.token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		h.t.Fatalf("POST /v1/repository-bindings: %v", err)
	}
	return resp
}

func TestRepositoryBindingRejectsLocalCloneURL(t *testing.T) {
	h := newHarness(t)

	// A file: clone_url is a §24 cross-tenant hazard on the shared host — rejected at the endpoint.
	resp := h.postBinding(`{"provider":"github","repository_identity":"acme/widgets","clone_url":"file:///tmp/evil/repo"}`)
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("POST file:// clone_url status = %d, want 400 (a local transport must be refused)", resp.StatusCode)
	}

	// A bare local path (no scheme) is the same hazard — also rejected.
	resp = h.postBinding(`{"provider":"github","repository_identity":"acme/widgets","clone_url":"/var/lib/palai/alloc_x/repo"}`)
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("POST bare-path clone_url status = %d, want 400", resp.StatusCode)
	}

	// A real https remote is accepted.
	resp = h.postBinding(`{"provider":"github","repository_identity":"acme/widgets","clone_url":"https://github.com/acme/widgets.git"}`)
	resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("POST https clone_url status = %d, want 201", resp.StatusCode)
	}
}

// TestCodingResponseRejectsUnknownBinding (blocker 2, admit half): a response whose `repository`
// field names a binding that does not exist (or belongs to another tenant) is refused at admit with a
// 404, rather than admitted and left to fail the run after the clone can't resolve the binding.
func TestCodingResponseRejectsUnknownBinding(t *testing.T) {
	h := newHarness(t)
	resp := h.postResponse(`{"input":"x","repository":{"binding_id":"bnd_does_not_exist","ref":"main"}}`, newID("idem"), h.token)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("POST with unknown binding_id status = %d, want 404 (no such binding in scope)", resp.StatusCode)
	}
}
