//go:build e2e

package responses

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
)

// post issues an authenticated POST to a management route and returns status + decoded JSON body.
func (h *harness) postAgent(path, body string) (int, map[string]any) {
	h.t.Helper()
	req, err := http.NewRequest(http.MethodPost, h.base+path, strings.NewReader(body))
	if err != nil {
		h.t.Fatalf("build POST %s: %v", path, err)
	}
	req.Header.Set("Authorization", "Bearer "+h.token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		h.t.Fatalf("POST %s: %v", path, err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	out := map[string]any{}
	_ = json.Unmarshal(raw, &out)
	return resp.StatusCode, out
}

// TestAgentRevisionManagementAndPinnedRun proves the E11 T1 management surface end to end over real
// HTTP: create profile → create draft revision → publish → admit a run pinned to it (accepted). It
// also proves the guards: an unsupported (E12) field is a 400, and pinning a DRAFT revision is a 409.
func TestAgentRevisionManagementAndPinnedRun(t *testing.T) {
	h := newHarness(t)

	// Create a profile.
	st, profile := h.postAgent("/v1/agents", `{"name":"reviewer"}`)
	if st != http.StatusCreated {
		t.Fatalf("create profile status = %d, want 201", st)
	}
	profileID, _ := profile["id"].(string)
	if profileID == "" {
		t.Fatalf("create profile returned no id: %v", profile)
	}

	// An unsupported (E12) field is rejected at create — no dead config stored.
	if st, _ := h.postAgent("/v1/agents/"+profileID+"/revisions", `{"model":"m","mcp":{"servers":[]}}`); st != http.StatusBadRequest {
		t.Fatalf("revision with an E12 field status = %d, want 400", st)
	}

	// A published revision that pins a model.
	st, rev := h.postAgent("/v1/agents/"+profileID+"/revisions", `{"model":"pinned-model","tools":["file"],"instructions":"be careful"}`)
	if st != http.StatusCreated {
		t.Fatalf("create revision status = %d, want 201", st)
	}
	revID, _ := rev["id"].(string)
	if rev["status"] != "draft" {
		t.Fatalf("new revision status = %v, want draft", rev["status"])
	}

	// Pinning the DRAFT is a 409 — a draft cannot be run.
	if st := h.postResponseStatus(`{"input":"go","agent_revision_id":"` + revID + `"}`); st != http.StatusConflict {
		t.Fatalf("pin draft status = %d, want 409", st)
	}

	// Publish, then the pin is accepted.
	if st, _ := h.postAgent("/v1/agents/"+profileID+"/revisions/"+revID+"/publish", ``); st != http.StatusOK {
		t.Fatalf("publish status = %d, want 200", st)
	}
	if st := h.postResponseStatus(`{"input":"go","agent_revision_id":"` + revID + `"}`); st != http.StatusAccepted {
		t.Fatalf("pin published revision status = %d, want 202", st)
	}

	// An unknown pin is a 404.
	if st := h.postResponseStatus(`{"input":"go","agent_revision_id":"arev_nope"}`); st != http.StatusNotFound {
		t.Fatalf("pin unknown revision status = %d, want 404", st)
	}
}

// postResponseStatus posts a create with a fresh idempotency key and returns just the status.
func (h *harness) postResponseStatus(body string) int {
	h.t.Helper()
	resp := h.postResponse(body, newID("idem"), h.token)
	defer resp.Body.Close()
	return resp.StatusCode
}
