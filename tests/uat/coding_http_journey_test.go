//go:build uat

package uat

// TestCodingHTTPStackLiveJourney is the E09 Task 10 HTTP-stack live smoke: the FULL production coding
// journey driven over HTTP against the packaged compose stack — the proof that a real end-user session
// reaches the coding tools with NO manual wiring. Unlike the T9 seam test (which wires the workspace by
// hand), this drives only the shipped HTTP surface: create session → POST /v1/repository-bindings → POST
// /v1/responses with the contracted `repository` field → the root run AUTO-PROVISIONS the workspace →
// the real file/shell/commit tools run against the checkout → finalize compiles the changeset → the
// push/PR tools create pending publications whose destination came from the BINDING → an approve command
// drives the real push (+ draft PR with a GitHub App) to the real Git destination.
//
// It is WIRED here and run from main in the batched live wave (make uat-coding PROVIDER=provider-one),
// never in CI: it needs the provider-one compose stack (PALAI_WORKSPACE_ROOT + PALAI_SANDBOX_IMAGE +
// the repository broker configured in deploy/compose), a real credential (OPENAI_API_KEY), and a real
// Git destination (PALAI_GIT_REPO). Absent any of these it skips, so make/CI never needs a key.
//
// ponytail: an approve arriving AFTER a single-step real run terminated leaves the publication approved
// for the boundary pump / E10 to drain (the §30.9 ceiling the T9 seam test names) — so the push-receipt
// assertion polls, driving the approve command and then reading the durable receipt the pump records.

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"
)

func TestCodingHTTPStackLiveJourney(t *testing.T) {
	requireDocker(t)
	if os.Getenv("PALAI_UAT_PROVIDER") != "provider-one" {
		t.Skip("HTTP-stack coding journey needs PALAI_UAT_PROVIDER=provider-one (run make uat-coding PROVIDER=provider-one)")
	}
	key := os.Getenv("OPENAI_API_KEY")
	repoURL := os.Getenv("PALAI_GIT_REPO")
	if key == "" || repoURL == "" {
		t.Skip("PALAI_GIT_REPO + OPENAI_API_KEY are required for the live HTTP-stack coding journey")
	}
	if strings.ContainsRune(repoURL, '@') {
		t.Fatal("PALAI_GIT_REPO must not embed a credential (contains '@'); the broker mints the push/fetch token")
	}

	s := newUATStack(t, "provider-one", key)

	// Step: register the repository binding over the shipped HTTP surface.
	var binding struct {
		ID string `json:"id"`
	}
	body := fmt.Sprintf(`{"provider":"github","repository_identity":%q,"clone_url":%q,"default_branch":"main","allowed_operations":["push_branch","open_pull_request"]}`, repoURL, repoURL)
	_ = json.Unmarshal(s.post("/v1/repository-bindings", body, http.StatusCreated), &binding)
	if binding.ID == "" {
		t.Fatal("repository binding create returned no id")
	}

	// Step: create a session and admit a coding response with the contracted `repository` field attached.
	sessionID := s.createSession()
	respBody := fmt.Sprintf(`{"input":"Add a file feature.txt containing HELLO, run a test that reads it, commit, then open a pull request.","session_id":%q,"repository":{"binding_id":%q,"ref":"main"}}`, sessionID, binding.ID)
	var created struct {
		ID    string `json:"id"`
		RunID string `json:"run_id"`
	}
	_ = json.Unmarshal(s.post("/v1/responses", respBody, http.StatusAccepted), &created)
	if created.ID == "" || created.RunID == "" {
		t.Fatal("coding response create returned no id/run_id")
	}

	final := s.awaitTerminal(created.ID, 5*time.Minute)
	if final.Status != "completed" {
		t.Fatalf("coding response %s status = %q, want completed", created.ID, final.Status)
	}

	// The root run AUTO-PROVISIONED the workspace: an allocation with a host path exists for the session.
	if n := s.query(fmt.Sprintf(
		"SELECT count(*) FROM workspace_allocations a JOIN workspaces w ON w.id=a.workspace_id WHERE w.session_id='%s' AND a.host_path <> ''", sessionID)); n != "1" {
		t.Fatalf("auto-provisioned allocations = %s, want 1 (the run did not provision the workspace over HTTP)", n)
	}
	// The changeset compiled at finalize from the tool ledger (the file/shell tools were reachable).
	if n := s.query(fmt.Sprintf("SELECT count(*) FROM changesets WHERE run_id='%s'", created.RunID)); n != "1" {
		t.Fatalf("changeset rows = %s, want 1 (finalize did not compile a changeset)", n)
	}
	// A pending push publication exists whose destination is the BINDING's remote, not the model's.
	pushRemote := s.query(fmt.Sprintf("SELECT remote FROM publications WHERE run_id='%s' AND operation='push_branch'", created.RunID))
	if pushRemote != repoURL {
		t.Fatalf("pending push remote = %q, want the binding's %q (the model cannot redirect the push)", pushRemote, repoURL)
	}

	// Step: approve the pending publications over HTTP, then read the durable push receipt the boundary
	// pump records. A post-terminal approve on a single-step run rides the pump/E10 (see the ponytail
	// note above), so this polls for the receipt rather than assuming an in-run publish.
	s.post("/v1/sessions/"+sessionID+"/commands", fmt.Sprintf(`{"command_id":%q,"kind":"approve"}`, "cmd_"+created.RunID), http.StatusAccepted)

	deadline := time.Now().Add(3 * time.Minute)
	var receipt string
	for time.Now().Before(deadline) {
		if receipt = s.query(fmt.Sprintf("SELECT count(*) FROM publications WHERE run_id='%s' AND state='published' AND operation='push_branch'", created.RunID)); receipt == "1" {
			break
		}
		time.Sleep(3 * time.Second)
	}
	if receipt != "1" {
		t.Fatalf("published push rows = %q, want 1 (the approved push never landed the external receipt)", receipt)
	}
	fmt.Printf("coding HTTP-stack journey PASS: run=%s remote=%s\n", created.RunID, repoURL)
}
