package a2a

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

// This is the A2A 1.0 protocol conformance suite + the GENERIC LOOPBACK HTTP client (A2A-002/003 proof
// seam). A plain net/http client drives a real A2A 1.0 exchange against the Server across the
// endpoint×lifecycle matrix. It is a LOOPBACK/FAKE verifier — same repo, in-memory fakes — NOT a foreign-peer
// interop claim (that is the §6 operator leg; the capability stays "preview").

const (
	testIfaceID = "a2aif_test"
	testOrg     = "org_real"
	testProject = "proj_real"
)

// sensitiveSource carries distinctive markers that must NEVER surface on a rendered card, threaded through
// the REAL projection so the HTTP-layer no-leak assertion is not vacuous.
var sensitiveSource = RevisionSource{
	Organization: testOrg,
	Project:      testProject,
	Model:        "provider-model-CONFIDENTIAL-x1",
	Tools:        []string{"internal_shell_TOOL", "db_admin_TOOL"},
	Instructions: "SYSTEM-PROMPT-CONFIDENTIAL",
	ToolSets:     []string{"toolset_CONFIDENTIAL"},
}

func testInterface() PublishedInterface {
	iface := ProjectInterface("rev_pinned_99", sensitiveSource, PublishMeta{
		Name:              "Loopback Planner",
		Description:       "Plans things.",
		Version:           "7",
		Streaming:         true,
		PushNotifications: true,
		ExtendedCard:      true,
		InputModes:        []string{"text/plain"},
		OutputModes:       []string{"application/json"},
		Skills:            []AgentSkill{{ID: "plan", Name: "Plan"}},
		AuthScheme:        "bearer",
	})
	iface.ID = testIfaceID
	iface.ETag = "etag-1"
	return iface
}

// newServer wires a Server with the given RunResult (drives the lifecycle branch under test).
func newServer(result RunResult) (*Server, *fakeRuns, *fakeTasks, *fakeFiles) {
	var counter int64
	runs := &fakeRuns{result: result, cancelTo: RunResult{RunID: result.RunID, State: "canceled"}}
	tasks := &fakeTasks{}
	files := &fakeFiles{}
	srv := &Server{
		Interfaces: &fakeInterfaces{byID: map[string]PublishedInterface{testIfaceID: testInterface()}},
		Runs:       runs,
		Tasks:      tasks,
		Files:      files,
		Pusher:     nil,
		ScopeFunc:  scopeFromHeader,
		BaseURL:    "https://cp.example.test",
		NewID:      func(p string) string { return fmt.Sprintf("%s_%d", p, atomic.AddInt64(&counter, 1)) },
	}
	return srv, runs, tasks, files
}

// loopback is the generic HTTP client. authed=true sets the tenant headers (the stand-in bearer identity).
func loopback(t *testing.T, base, method, path string, authed bool, body any, hdr map[string]string) (int, []byte) {
	t.Helper()
	var rdr io.Reader
	if body != nil {
		blob, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("marshal body: %v", err)
		}
		rdr = bytes.NewReader(blob)
	}
	req, err := http.NewRequest(method, base+path, rdr)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	if authed {
		req.Header.Set("X-Scope-Org", testOrg)
		req.Header.Set("X-Scope-Project", testProject)
	}
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do request: %v", err)
	}
	defer resp.Body.Close()
	out, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, out
}

func ifacePath(rest string) string { return pathPrefix + testIfaceID + "/" + rest }

// completedDurable is a terminal, stored run — message:send must return a Task (not a direct message).
func completedDurable() RunResult {
	return RunResult{RunID: "run_canon_1", SessionID: "ses_canon_1", State: "completed", OutputText: "done", Durable: true}
}

func TestA2AConformance_PublicCardNeverLeaksAndAdvertisesExactVersion(t *testing.T) {
	srv, _, _, _ := newServer(completedDurable())
	ts := httptest.NewServer(srv.PublicCardHandler())
	defer ts.Close()

	// Unauthenticated GET of the public card (A2A-001).
	status, body := loopback(t, ts.URL, http.MethodGet, ifacePath("agent-card.json"), false, nil, nil)
	if status != http.StatusOK {
		t.Fatalf("public card status = %d, want 200; body=%s", status, body)
	}
	rendered := string(body)
	for _, forbidden := range []string{"provider-model-CONFIDENTIAL-x1", "internal_shell_TOOL", "db_admin_TOOL", "SYSTEM-PROMPT-CONFIDENTIAL", "toolset_CONFIDENTIAL"} {
		if strings.Contains(rendered, forbidden) {
			t.Fatalf("public card LEAKS %q: %s", forbidden, rendered)
		}
	}
	var card Card
	if err := json.Unmarshal(body, &card); err != nil {
		t.Fatalf("unmarshal card: %v", err)
	}
	if card.ProtocolVersion != "1.0" || card.PreferredTransport != "HTTP+JSON" {
		t.Fatalf("card advertises wrong version/binding: %+v", card)
	}
	if len(card.SupportedInterfaces) != 1 || card.SupportedInterfaces[0].ProtocolVersion != "1.0" || card.SupportedInterfaces[0].ProtocolBinding != "HTTP+JSON" {
		t.Fatalf("interface advertisement wrong: %+v", card.SupportedInterfaces)
	}
	if _, ok := card.SecuritySchemes["bearer"]; !ok {
		t.Fatalf("card does not advertise the bearer auth it enforces: %+v", card.SecuritySchemes)
	}
}

func TestA2AConformance_ExtendedCardRequiresAuth(t *testing.T) {
	srv, _, _, _ := newServer(completedDurable())
	ts := httptest.NewServer(srv)
	defer ts.Close()

	if status, _ := loopback(t, ts.URL, http.MethodGet, ifacePath("extendedAgentCard"), false, nil, nil); status != http.StatusUnauthorized {
		t.Fatalf("unauth extended card = %d, want 401", status)
	}
	if status, _ := loopback(t, ts.URL, http.MethodGet, ifacePath("extendedAgentCard"), true, nil, nil); status != http.StatusOK {
		t.Fatalf("authed extended card = %d, want 200", status)
	}
}

func TestA2AConformance_SendReturnsTaskWithExternalCanonicalSeparation(t *testing.T) {
	srv, _, tasks, _ := newServer(completedDurable())
	ts := httptest.NewServer(srv)
	defer ts.Close()

	msg := map[string]any{"message": map[string]any{"role": "user", "parts": []Part{{Kind: "text", Text: "hello"}}, "messageId": "m1"}}
	status, body := loopback(t, ts.URL, http.MethodPost, ifacePath("message:send"), true, msg, nil)
	if status != http.StatusOK {
		t.Fatalf("send status = %d, want 200; %s", status, body)
	}
	var task Task
	if err := json.Unmarshal(body, &task); err != nil {
		t.Fatalf("unmarshal task: %v", err)
	}
	if task.Kind != "task" {
		t.Fatalf("durable-complete send did not return a Task: %s", body)
	}
	if task.Status.State != TaskStateCompleted {
		t.Fatalf("task state = %q, want completed", task.Status.State)
	}
	// §38.2: the external A2A ids are NOT the canonical run/session ids.
	if task.ID == "run_canon_1" || task.ContextID == "ses_canon_1" {
		t.Fatalf("A2A task/context id replaced the canonical run/session id: id=%s context=%s", task.ID, task.ContextID)
	}
	ref, ok, _ := tasks.GetRef(nil, testOrg, testProject, testIfaceID, task.ID)
	if !ok {
		t.Fatalf("no task ref persisted for %s", task.ID)
	}
	if ref.RunID != "run_canon_1" || ref.SessionID != "ses_canon_1" {
		t.Fatalf("task ref did not bridge to the canonical ids: %+v", ref)
	}
}

func TestA2AConformance_ForgedMetadataIdentityIsIgnored(t *testing.T) {
	srv, runs, _, _ := newServer(completedDurable())
	ts := httptest.NewServer(srv)
	defer ts.Close()

	msg := map[string]any{"message": map[string]any{
		"role":     "user",
		"parts":    []Part{{Kind: "text", Text: "hi"}},
		"metadata": map[string]any{"organization": "org_VICTIM", "project": "proj_VICTIM"},
	}}
	if status, body := loopback(t, ts.URL, http.MethodPost, ifacePath("message:send"), true, msg, nil); status != http.StatusOK {
		t.Fatalf("send status = %d; %s", status, body)
	}
	got := runs.lastAdmit()
	if got.Org != testOrg || got.Project != testProject {
		t.Fatalf("forged metadata governed the run: admitted under org=%q project=%q, want %s/%s", got.Org, got.Project, testOrg, testProject)
	}
}

func TestA2AConformance_SendReturnsDirectMessageForCompleteNonDurable(t *testing.T) {
	srv, _, _, _ := newServer(RunResult{RunID: "run_x", SessionID: "ses_x", State: "completed", OutputText: "quick", Durable: false})
	ts := httptest.NewServer(srv)
	defer ts.Close()

	msg := map[string]any{"message": map[string]any{"role": "user", "parts": []Part{{Kind: "text", Text: "q"}}, "messageId": "m2"}}
	status, body := loopback(t, ts.URL, http.MethodPost, ifacePath("message:send"), true, msg, nil)
	if status != http.StatusOK {
		t.Fatalf("send status = %d; %s", status, body)
	}
	var m Message
	if err := json.Unmarshal(body, &m); err != nil {
		t.Fatalf("unmarshal message: %v", err)
	}
	if m.Kind != "message" || m.Role != "agent" {
		t.Fatalf("complete non-durable send did not return a direct agent Message: %s", body)
	}
}

func TestA2AConformance_InboundFilePartIsIngested(t *testing.T) {
	srv, _, _, files := newServer(completedDurable())
	ts := httptest.NewServer(srv)
	defer ts.Close()

	msg := map[string]any{"message": map[string]any{"role": "user", "messageId": "m3", "parts": []Part{
		{Kind: "text", Text: "see file"},
		{Kind: "file", File: &FilePart{Name: "data.txt", MimeType: "text/plain", Bytes: "aGVsbG8="}},
	}}}
	if status, body := loopback(t, ts.URL, http.MethodPost, ifacePath("message:send"), true, msg, nil); status != http.StatusOK {
		t.Fatalf("send status = %d; %s", status, body)
	}
	if files.count() != 1 {
		t.Fatalf("inbound file part not ingested (A2A-004 server half): count=%d", files.count())
	}
}

func TestA2AConformance_StreamTerminalConsistency(t *testing.T) {
	srv, _, _, _ := newServer(completedDurable())
	ts := httptest.NewServer(srv)
	defer ts.Close()

	msg := map[string]any{"message": map[string]any{"role": "user", "parts": []Part{{Kind: "text", Text: "stream"}}, "messageId": "m4"}}
	status, body := loopback(t, ts.URL, http.MethodPost, ifacePath("message:stream"), true, msg, nil)
	if status != http.StatusOK {
		t.Fatalf("stream status = %d; %s", status, body)
	}
	frames := parseSSE(t, body)
	if len(frames) < 2 {
		t.Fatalf("expected >=2 stream frames, got %d: %s", len(frames), body)
	}
	// The FINAL frame must be a terminal status with final=true (A2A-002 terminal consistency).
	last := frames[len(frames)-1]
	su, ok := last["statusUpdate"].(map[string]any)
	if !ok {
		t.Fatalf("last frame is not a statusUpdate: %v", last)
	}
	if fin, _ := su["final"].(bool); !fin {
		t.Fatalf("terminal frame not marked final: %v", su)
	}
	st, _ := su["status"].(map[string]any)
	if st["state"] != "completed" {
		t.Fatalf("terminal frame state = %v, want completed", st["state"])
	}
}

func TestA2AConformance_TaskGetListCancel(t *testing.T) {
	srv, runs, _, _ := newServer(completedDurable())
	runs.cancelTo = RunResult{RunID: "run_canon_1", State: "canceled"}
	runs.cancelSE = "a downstream write may have committed"
	ts := httptest.NewServer(srv)
	defer ts.Close()

	// Create a task via send.
	msg := map[string]any{"message": map[string]any{"role": "user", "parts": []Part{{Kind: "text", Text: "t"}}, "messageId": "m5"}}
	_, body := loopback(t, ts.URL, http.MethodPost, ifacePath("message:send"), true, msg, nil)
	var task Task
	_ = json.Unmarshal(body, &task)

	// GET the task.
	if status, gb := loopback(t, ts.URL, http.MethodGet, ifacePath("tasks/"+task.ID), true, nil, nil); status != http.StatusOK {
		t.Fatalf("get task = %d; %s", status, gb)
	}
	// List tasks contains it.
	_, lb := loopback(t, ts.URL, http.MethodGet, ifacePath("tasks"), true, nil, nil)
	if !strings.Contains(string(lb), task.ID) {
		t.Fatalf("task list missing %s: %s", task.ID, lb)
	}
	// Cancel surfaces the uncertain side-effect (§38.3).
	status, cb := loopback(t, ts.URL, http.MethodPost, ifacePath("tasks/"+task.ID+":cancel"), true, nil, nil)
	if status != http.StatusOK {
		t.Fatalf("cancel = %d; %s", status, cb)
	}
	if !strings.Contains(string(cb), "uncertainSideEffect") || !strings.Contains(string(cb), "downstream write") {
		t.Fatalf("cancel did not report the non-cancelable uncertain side-effect: %s", cb)
	}
}

func TestA2AConformance_InputRequiredMapsToWaiting(t *testing.T) {
	srv, _, _, _ := newServer(RunResult{RunID: "run_ir", SessionID: "ses_ir", State: "waiting_for_input", Durable: true})
	ts := httptest.NewServer(srv)
	defer ts.Close()

	msg := map[string]any{"message": map[string]any{"role": "user", "parts": []Part{{Kind: "text", Text: "need input"}}, "messageId": "m6"}}
	_, body := loopback(t, ts.URL, http.MethodPost, ifacePath("message:send"), true, msg, nil)
	var task Task
	if err := json.Unmarshal(body, &task); err != nil {
		t.Fatalf("unmarshal: %v; %s", err, body)
	}
	if task.Status.State != TaskStateInputRequired {
		t.Fatalf("waiting_for_input -> %q, want input-required (A2A-003)", task.Status.State)
	}
}

func TestA2AConformance_PushConfigCRUD(t *testing.T) {
	srv, _, _, _ := newServer(completedDurable())
	ts := httptest.NewServer(srv)
	defer ts.Close()

	msg := map[string]any{"message": map[string]any{"role": "user", "parts": []Part{{Kind: "text", Text: "p"}}, "messageId": "m7"}}
	_, body := loopback(t, ts.URL, http.MethodPost, ifacePath("message:send"), true, msg, nil)
	var task Task
	_ = json.Unmarshal(body, &task)
	base := "tasks/" + task.ID + "/pushNotificationConfigs"

	// Set.
	cfg := PushNotificationConfig{ID: "pc1", URL: "https://sink.example.test/hook", Token: "SECRET_TOKEN"}
	status, sb := loopback(t, ts.URL, http.MethodPost, ifacePath(base), true, cfg, nil)
	if status != http.StatusOK {
		t.Fatalf("set push = %d; %s", status, sb)
	}
	if strings.Contains(string(sb), "SECRET_TOKEN") {
		t.Fatalf("push set echoed the token back: %s", sb)
	}
	// Get.
	if status, gb := loopback(t, ts.URL, http.MethodGet, ifacePath(base+"/pc1"), true, nil, nil); status != http.StatusOK || strings.Contains(string(gb), "SECRET_TOKEN") {
		t.Fatalf("get push = %d leaked-token=%v; %s", status, strings.Contains(string(gb), "SECRET_TOKEN"), gb)
	}
	// List.
	if status, lb := loopback(t, ts.URL, http.MethodGet, ifacePath(base), true, nil, nil); status != http.StatusOK || !strings.Contains(string(lb), "pc1") {
		t.Fatalf("list push = %d; %s", status, lb)
	}
	// Delete.
	if status, _ := loopback(t, ts.URL, http.MethodDelete, ifacePath(base+"/pc1"), true, nil, nil); status != http.StatusNoContent {
		t.Fatalf("delete push = %d, want 204", status)
	}
	if status, _ := loopback(t, ts.URL, http.MethodGet, ifacePath(base+"/pc1"), true, nil, nil); status != http.StatusNotFound {
		t.Fatalf("get deleted push = %d, want 404", status)
	}
}

func TestA2AConformance_UnknownInterfaceAndTaskAre404(t *testing.T) {
	srv, _, _, _ := newServer(completedDurable())
	ts := httptest.NewServer(srv)
	defer ts.Close()

	if status, _ := loopback(t, ts.URL, http.MethodGet, pathPrefix+"a2aif_nope/tasks", true, nil, nil); status != http.StatusNotFound {
		t.Fatalf("unknown interface list = %d, want 404", status)
	}
	if status, _ := loopback(t, ts.URL, http.MethodGet, ifacePath("tasks/task_nope"), true, nil, nil); status != http.StatusNotFound {
		t.Fatalf("unknown task get = %d, want 404", status)
	}
}

// TestA2ALoopbackExchange drives a full A2A 1.0 exchange (the loopback transcript): discover the card,
// send a message, retrieve the task, subscribe, then cancel — a real end-to-end protocol run against the
// server, over a generic HTTP client. Labelled loopback/fake, NOT foreign-peer interop.
func TestA2ALoopbackExchange(t *testing.T) {
	srv, _, _, _ := newServer(completedDurable())
	ts := httptest.NewServer(srv)
	defer ts.Close()

	// 1. Discover (extended card, authenticated).
	if status, _ := loopback(t, ts.URL, http.MethodGet, ifacePath("extendedAgentCard"), true, nil, nil); status != http.StatusOK {
		t.Fatalf("discover = %d", status)
	}
	// 2. Send.
	msg := map[string]any{"message": map[string]any{"role": "user", "parts": []Part{{Kind: "text", Text: "go"}}, "messageId": "loop-1"}}
	_, body := loopback(t, ts.URL, http.MethodPost, ifacePath("message:send"), true, msg, nil)
	var task Task
	if err := json.Unmarshal(body, &task); err != nil || task.ID == "" {
		t.Fatalf("send produced no task: %v; %s", err, body)
	}
	// 3. Retrieve.
	if status, _ := loopback(t, ts.URL, http.MethodGet, ifacePath("tasks/"+task.ID), true, nil, nil); status != http.StatusOK {
		t.Fatalf("retrieve = %d", status)
	}
	// 4. Subscribe (SSE, terminal).
	status, sub := loopback(t, ts.URL, http.MethodPost, ifacePath("tasks/"+task.ID+":subscribe"), true, nil, nil)
	if status != http.StatusOK {
		t.Fatalf("subscribe = %d", status)
	}
	frames := parseSSE(t, sub)
	if len(frames) == 0 {
		t.Fatalf("subscribe produced no frames")
	}
}

// parseSSE decodes `data: {json}` frames from an SSE body.
func parseSSE(t *testing.T, body []byte) []map[string]any {
	t.Helper()
	var out []map[string]any
	for _, line := range strings.Split(string(body), "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		var m map[string]any
		if err := json.Unmarshal([]byte(strings.TrimSpace(strings.TrimPrefix(line, "data:"))), &m); err != nil {
			t.Fatalf("bad SSE frame %q: %v", line, err)
		}
		out = append(out, m)
	}
	return out
}
