package a2a

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// maxBody bounds an A2A request body before parsing — the same posture the rest of the HTTP surface takes.
const maxBody = 1 << 20 // 1 MiB

// pathPrefix anchors every A2A operation under a per-interface base (spec §38.1). The interface id keys the
// published card + all of that interface's task lifecycle.
const pathPrefix = "/v1/a2a/interfaces/"

// Scope is the authenticated tenant the request runs as. It is resolved by the injected ScopeFunc from the
// bearer identity the outer auth middleware established — NEVER from anything the A2A client supplies (§38.6).
type Scope struct{ Organization, Project string }

// ScopeFunc extracts the authenticated tenant from a request. Production wires it to middleware.ScopeFrom;
// tests wire a deterministic one. It returns ok=false when the request is unauthenticated (401 on authed
// routes). Keeping it injected keeps this package free of the api/middleware import (no cycle) and lets a
// loopback client drive the whole server standalone.
type ScopeFunc func(*http.Request) (Scope, bool)

// InterfaceStore resolves published A2A interfaces. ResolvePublic serves the UNAUTHENTICATED public card
// (system-scoped, keyed by the server-minted interface id); Get resolves within the authenticated scope.
type InterfaceStore interface {
	ResolvePublic(ctx context.Context, interfaceID string) (PublishedInterface, bool, error)
	Get(ctx context.Context, org, project, interfaceID string) (PublishedInterface, bool, error)
}

// RunRequest / RunResult are the a2a-owned canonical run seam. The adapter admits + reads runs through Runs
// and invents NO run identity (§38.2, §34.1): RunResult.RunID/SessionID are the platform-minted canonical
// ids, returned here, never supplied by the A2A client.
type RunRequest struct {
	Org, Project    string
	Input           string
	IdempotencyKey  string
	AgentRevisionID string
	Store           bool
}
type RunResult struct {
	RunID, SessionID string
	State            string // canonical run status; MapRunState projects it to a TaskState
	OutputText       string
	Durable          bool
}

// CancelReport is the §38.3 cancel outcome: whether the cancel Command was accepted, plus a report of any
// non-cancelable, uncertain side-effect the run may already have committed (surfaced honestly, not hidden).
type CancelReport struct {
	UncertainSideEffect string
}

// Runs is the canonical run admission + read + cancel seam.
type Runs interface {
	Admit(ctx context.Context, req RunRequest) (RunResult, error)
	Get(ctx context.Context, org, project, runID string) (RunResult, bool, error)
	Cancel(ctx context.Context, org, project, runID string) (RunResult, CancelReport, error)
}

// TaskRef is the stored external->canonical bridge (§38.2).
type TaskRef struct {
	InterfaceID  string
	A2ATaskID    string
	A2AContextID string
	RunID        string
	SessionID    string
	PushConfigs  []PushNotificationConfig
}

// Tasks stores + reads the external A2A task/context <-> canonical run/session bridge and each task's push
// configs.
type Tasks interface {
	Put(ctx context.Context, org, project string, ref TaskRef) error
	Get(ctx context.Context, org, project, interfaceID, a2aTaskID string) (TaskRef, bool, error)
	List(ctx context.Context, org, project, interfaceID string, limit int) ([]TaskRef, error)
	SetPushConfigs(ctx context.Context, org, project, interfaceID, a2aTaskID string, cfgs []PushNotificationConfig) error
}

// Files ingests an inbound A2A file part -> a scanned, stored artifact (the A2A-004 server half). The raw
// bytes never become a privileged instruction; they land as an artifact the run may read as tool-result data.
type Files interface {
	Ingest(ctx context.Context, org, project, runID string, f FilePart) (artifactID string, err error)
}

// Pusher sends a signed outbound push notification, reusing the existing signed webhook delivery model
// (A2A-003). nil disables push (the capability is advertised only when a Pusher is wired).
type Pusher interface {
	Push(ctx context.Context, cfg PushNotificationConfig, payload []byte) error
}

// Server is the A2A 1.0 HTTP+JSON server projection. It routes the 12 endpoints manually because Go's
// ServeMux cannot express A2A's `{task_id}:cancel` (wildcard + literal suffix in one segment) or the
// `message:send` colon-verb forms.
type Server struct {
	Interfaces InterfaceStore
	Runs       Runs
	Tasks      Tasks
	Files      Files
	Pusher     Pusher
	ScopeFunc  ScopeFunc
	BaseURL    string
	NewID      func(prefix string) string
	Clock      func() time.Time
}

func (s *Server) newID(prefix string) string {
	if s.NewID != nil {
		return s.NewID(prefix)
	}
	return prefix + "_" + strconv.FormatInt(time.Now().UnixNano(), 36)
}

func (s *Server) now() time.Time {
	if s.Clock != nil {
		return s.Clock()
	}
	return time.Now()
}

func (s *Server) scope(r *http.Request) (Scope, bool) {
	if s.ScopeFunc == nil {
		return Scope{}, false
	}
	return s.ScopeFunc(r)
}

// ServeHTTP is the authed A2A surface (mounted under /v1/a2a/ inside the auth middleware). The public card is
// served separately by PublicCardHandler on the unauthenticated top mux.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	interfaceID, rest, ok := splitPath(r.URL.Path)
	if !ok {
		writeErr(w, http.StatusNotFound, "not_found", "unknown A2A path")
		return
	}
	seg := strings.Split(rest, "/")
	switch {
	case rest == "agent-card.json" && r.Method == http.MethodGet:
		s.publicCard(w, r, interfaceID) // reachable here too; carries only safe fields, no scope needed
	case rest == "extendedAgentCard" && r.Method == http.MethodGet:
		s.extendedCard(w, r, interfaceID)
	case rest == "message:send" && r.Method == http.MethodPost:
		s.messageSend(w, r, interfaceID)
	case rest == "message:stream" && r.Method == http.MethodPost:
		s.messageStream(w, r, interfaceID)
	case rest == "tasks" && r.Method == http.MethodGet:
		s.listTasks(w, r, interfaceID)
	case seg[0] == "tasks" && len(seg) == 2:
		s.taskVerb(w, r, interfaceID, seg[1])
	case seg[0] == "tasks" && len(seg) == 3 && seg[2] == "pushNotificationConfigs":
		s.pushCollection(w, r, interfaceID, seg[1])
	case seg[0] == "tasks" && len(seg) == 4 && seg[2] == "pushNotificationConfigs":
		s.pushItem(w, r, interfaceID, seg[1], seg[3])
	default:
		writeErr(w, http.StatusNotFound, "not_found", "unknown A2A operation")
	}
}

// PublicCardHandler serves ONLY the unauthenticated public Agent Card. It is mounted on the top mux so it
// bypasses bearer auth (the card is a safe published projection — A2A-001); every other route requires the
// authenticated scope.
func (s *Server) PublicCardHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		interfaceID, rest, ok := splitPath(r.URL.Path)
		if !ok || rest != "agent-card.json" || r.Method != http.MethodGet {
			writeErr(w, http.StatusNotFound, "not_found", "unknown A2A path")
			return
		}
		s.publicCard(w, r, interfaceID)
	})
}

// splitPath extracts the interface id and the remaining operation path from a /v1/a2a/interfaces/{id}/... URL.
func splitPath(p string) (interfaceID, rest string, ok bool) {
	if !strings.HasPrefix(p, pathPrefix) {
		return "", "", false
	}
	tail := strings.TrimPrefix(p, pathPrefix)
	slash := strings.IndexByte(tail, '/')
	if slash < 0 {
		return "", "", false
	}
	interfaceID = tail[:slash]
	rest = tail[slash+1:]
	if interfaceID == "" || rest == "" {
		return "", "", false
	}
	return interfaceID, rest, true
}

// ---- Agent Card ----

func (s *Server) publicCard(w http.ResponseWriter, r *http.Request, interfaceID string) {
	iface, ok, err := s.Interfaces.ResolvePublic(r.Context(), interfaceID)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal_error", "")
		return
	}
	if !ok {
		writeErr(w, http.StatusNotFound, "not_found", "no such A2A interface")
		return
	}
	card := RenderCard(iface, CardEndpoint{BaseURL: s.BaseURL, InterfaceID: interfaceID})
	// Revisioned + cacheable (A2A-001): the etag lets a client cache the card across unchanged revisions.
	if iface.ETag != "" {
		w.Header().Set("ETag", `"`+iface.ETag+`"`)
	}
	writeJSON(w, http.StatusOK, card)
}

func (s *Server) extendedCard(w http.ResponseWriter, r *http.Request, interfaceID string) {
	sc, ok := s.scope(r)
	if !ok {
		writeErr(w, http.StatusUnauthorized, "authentication_required", "a bearer API key is required")
		return
	}
	iface, ok, err := s.Interfaces.Get(r.Context(), sc.Organization, sc.Project, interfaceID)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal_error", "")
		return
	}
	if !ok {
		writeErr(w, http.StatusNotFound, "not_found", "no such A2A interface")
		return
	}
	card := RenderExtendedCard(iface, CardEndpoint{BaseURL: s.BaseURL, InterfaceID: interfaceID})
	writeJSON(w, http.StatusOK, card)
}

// ---- message:send / message:stream ----

// sendParams is the message/send request body (the A2A REST binding wraps the message plus optional config).
type sendParams struct {
	Message       Message `json:"message"`
	Configuration struct {
		Blocking *bool `json:"blocking,omitempty"`
	} `json:"configuration"`
}

// admitFromMessage runs the canonical admission for an inbound A2A message: it resolves the interface within
// the AUTHENTICATED scope, governs identity (metadata ignored — §38.6), admits a run through the canonical
// seam, ingests any inbound file parts as scanned artifacts (A2A-004), and — for a durable/non-complete
// outcome — records the external->canonical task ref (§38.2). It returns the interface, the run result, and
// the minted external task/context ids (empty for the direct-message case).
func (s *Server) admitFromMessage(r *http.Request, interfaceID string) (PublishedInterface, RunResult, string, string, *problem) {
	sc, ok := s.scope(r)
	if !ok {
		return PublishedInterface{}, RunResult{}, "", "", &problem{http.StatusUnauthorized, "authentication_required", "a bearer API key is required"}
	}
	iface, ok, err := s.Interfaces.Get(r.Context(), sc.Organization, sc.Project, interfaceID)
	if err != nil {
		return PublishedInterface{}, RunResult{}, "", "", &problem{http.StatusInternalServerError, "internal_error", ""}
	}
	if !ok {
		return PublishedInterface{}, RunResult{}, "", "", &problem{http.StatusNotFound, "not_found", "no such A2A interface"}
	}
	raw, err := io.ReadAll(http.MaxBytesReader(nil, r.Body, maxBody))
	if err != nil {
		return PublishedInterface{}, RunResult{}, "", "", &problem{http.StatusBadRequest, "invalid_request", "the request body could not be read"}
	}
	var params sendParams
	if err := json.Unmarshal(raw, &params); err != nil {
		return PublishedInterface{}, RunResult{}, "", "", &problem{http.StatusBadRequest, "invalid_request", "the request body is not valid JSON"}
	}

	// Identity governance: the authenticated scope governs; message metadata is discarded (§38.6).
	org, project := GovernIdentity(sc.Organization, sc.Project, params.Message)

	idem := params.Message.MessageID
	if idem == "" {
		idem = s.newID("a2amsg")
	}
	res, err := s.Runs.Admit(r.Context(), RunRequest{
		Org: org, Project: project,
		Input:           MessageText(params.Message),
		IdempotencyKey:  idem,
		AgentRevisionID: iface.AgentRevisionID,
		Store:           true,
	})
	if err != nil {
		return PublishedInterface{}, RunResult{}, "", "", &problem{http.StatusBadGateway, "admission_failed", "the run could not be admitted"}
	}

	// A2A-004 server half: ingest each inbound file part as a scanned artifact under the canonical run.
	if s.Files != nil {
		for _, f := range FileParts(params.Message) {
			_, _ = s.Files.Ingest(r.Context(), org, project, res.RunID, f)
		}
	}

	state := MapRunState(res.State)
	if DecideDirectMessage(state, res.Durable) {
		return iface, res, "", "", nil // direct message; no task ref persisted
	}
	// Task outcome: mint EXTERNAL ids and bridge them to the CANONICAL run/session (never replacing them).
	a2aTaskID := s.newID("a2atask")
	a2aContextID := params.Message.ContextID
	if a2aContextID == "" {
		a2aContextID = s.newID("a2actx")
	}
	if err := s.Tasks.Put(r.Context(), org, project, TaskRef{
		InterfaceID: interfaceID, A2ATaskID: a2aTaskID, A2AContextID: a2aContextID,
		RunID: res.RunID, SessionID: res.SessionID,
	}); err != nil {
		return PublishedInterface{}, RunResult{}, "", "", &problem{http.StatusInternalServerError, "internal_error", ""}
	}
	return iface, res, a2aTaskID, a2aContextID, nil
}

func (s *Server) messageSend(w http.ResponseWriter, r *http.Request, interfaceID string) {
	_, res, taskID, contextID, prob := s.admitFromMessage(r, interfaceID)
	if prob != nil {
		writeErr(w, prob.status, prob.code, prob.detail)
		return
	}
	state := MapRunState(res.State)
	if taskID == "" { // direct message (genuinely-complete, non-durable)
		msg := BuildDirectMessage(res.OutputText, s.newID("a2amsg"), s.newID("a2actx"))
		writeJSON(w, http.StatusOK, msg)
		return
	}
	writeJSON(w, http.StatusOK, BuildTask(taskID, contextID, TaskStatus{State: state, Timestamp: s.now().UTC().Format(time.RFC3339)}, s.artifactsFor(res)))
}

// messageStream admits the run, then streams A2A status/artifact updates with terminal consistency
// (A2A-002): the final frame carries the terminal state with final=true, and a subsequent tasks/{id} GET
// returns the SAME terminal state.
//
// ponytail: this projects the run's snapshot deterministically — a working frame, artifact frames, then the
// terminal frame. Following a long-lived run's incremental events reuses the existing
// /v1/sessions/{id}/events SSE seam and is not re-implemented here (honest ceiling; the fake/live proof
// drives a synchronously-terminal run).
func (s *Server) messageStream(w http.ResponseWriter, r *http.Request, interfaceID string) {
	iface, res, taskID, contextID, prob := s.admitFromMessage(r, interfaceID)
	_ = iface
	if prob != nil {
		writeErr(w, prob.status, prob.code, prob.detail)
		return
	}
	if taskID == "" { // a direct-message outcome still needs a task id for the stream's frames
		taskID = s.newID("a2atask")
		contextID = s.newID("a2actx")
	}
	s.streamRun(w, r, taskID, contextID, res)
}

func (s *Server) streamRun(w http.ResponseWriter, r *http.Request, taskID, contextID string, res RunResult) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeErr(w, http.StatusInternalServerError, "internal_error", "streaming unsupported")
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(http.StatusOK)

	writeSSE(w, NewStatusUpdate(taskID, contextID, TaskStatus{State: TaskStateWorking, Timestamp: s.now().UTC().Format(time.RFC3339)}, false))
	flusher.Flush()

	state := MapRunState(res.State)
	for _, a := range s.artifactsFor(res) {
		writeSSE(w, NewArtifactUpdate(taskID, a))
	}
	// Terminal consistency: the final frame carries the terminal (or current) state; final=true only when
	// the run has actually reached a lifecycle end, so a still-working stream never lies about completion.
	writeSSE(w, NewStatusUpdate(taskID, contextID, TaskStatus{State: state, Timestamp: s.now().UTC().Format(time.RFC3339)}, state.Terminal()))
	flusher.Flush()
}

// artifactsFor projects a completed run's text output into a single A2A artifact. A run with no output yields
// none. Larger/binary outputs are stored artifacts referenced by id (fetched over the authenticated artifact
// API), not inlined — this is the text-output projection.
func (s *Server) artifactsFor(res RunResult) []Artifact {
	if strings.TrimSpace(res.OutputText) == "" {
		return nil
	}
	return []Artifact{{
		ArtifactID: s.newID("a2aart"),
		Parts:      []Part{{Kind: "text", Text: res.OutputText}},
	}}
}

// ---- tasks get/list/cancel/subscribe ----

func (s *Server) listTasks(w http.ResponseWriter, r *http.Request, interfaceID string) {
	sc, ok := s.scope(r)
	if !ok {
		writeErr(w, http.StatusUnauthorized, "authentication_required", "a bearer API key is required")
		return
	}
	// Resolve the interface within scope first: an unknown or foreign interface is a 404, not an empty 200
	// (no existence oracle, and no listing under an interface that isn't the caller's).
	if _, ok, err := s.Interfaces.Get(r.Context(), sc.Organization, sc.Project, interfaceID); err != nil {
		writeErr(w, http.StatusInternalServerError, "internal_error", "")
		return
	} else if !ok {
		writeErr(w, http.StatusNotFound, "not_found", "no such A2A interface")
		return
	}
	refs, err := s.Tasks.List(r.Context(), sc.Organization, sc.Project, interfaceID, 100)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal_error", "")
		return
	}
	tasks := make([]Task, 0, len(refs))
	for _, ref := range refs {
		res, found, err := s.Runs.Get(r.Context(), sc.Organization, sc.Project, ref.RunID)
		if err != nil {
			writeErr(w, http.StatusInternalServerError, "internal_error", "")
			return
		}
		state := TaskStateSubmitted
		if found {
			state = MapRunState(res.State)
		}
		tasks = append(tasks, BuildTask(ref.A2ATaskID, ref.A2AContextID, TaskStatus{State: state, Timestamp: s.now().UTC().Format(time.RFC3339)}, s.artifactsFor(res)))
	}
	writeJSON(w, http.StatusOK, map[string]any{"tasks": tasks})
}

// taskVerb dispatches GET tasks/{id}, POST tasks/{id}:cancel, POST tasks/{id}:subscribe. The verb rides the
// same segment after a colon (A2A's method suffix), so it is split here.
func (s *Server) taskVerb(w http.ResponseWriter, r *http.Request, interfaceID, seg string) {
	taskID, verb, _ := strings.Cut(seg, ":")
	switch {
	case verb == "" && r.Method == http.MethodGet:
		s.getTask(w, r, interfaceID, taskID)
	case verb == "cancel" && r.Method == http.MethodPost:
		s.cancelTask(w, r, interfaceID, taskID)
	case verb == "subscribe" && r.Method == http.MethodPost:
		s.subscribeTask(w, r, interfaceID, taskID)
	default:
		writeErr(w, http.StatusNotFound, "not_found", "unknown task operation")
	}
}

// resolveTask resolves the external task ref within scope and reads its canonical run. A miss (unknown or
// foreign id) is an indistinguishable 404 — no existence oracle.
func (s *Server) resolveTask(r *http.Request, sc Scope, interfaceID, taskID string) (TaskRef, RunResult, bool) {
	ref, ok, err := s.Tasks.Get(r.Context(), sc.Organization, sc.Project, interfaceID, taskID)
	if err != nil || !ok {
		return TaskRef{}, RunResult{}, false
	}
	res, _, _ := s.Runs.Get(r.Context(), sc.Organization, sc.Project, ref.RunID)
	return ref, res, true
}

func (s *Server) getTask(w http.ResponseWriter, r *http.Request, interfaceID, taskID string) {
	sc, ok := s.scope(r)
	if !ok {
		writeErr(w, http.StatusUnauthorized, "authentication_required", "a bearer API key is required")
		return
	}
	ref, res, ok := s.resolveTask(r, sc, interfaceID, taskID)
	if !ok {
		writeErr(w, http.StatusNotFound, "not_found", "no such task")
		return
	}
	status := TaskStatus{State: MapRunState(res.State), Timestamp: s.now().UTC().Format(time.RFC3339)}
	writeJSON(w, http.StatusOK, BuildTask(ref.A2ATaskID, ref.A2AContextID, status, s.artifactsFor(res)))
}

func (s *Server) cancelTask(w http.ResponseWriter, r *http.Request, interfaceID, taskID string) {
	sc, ok := s.scope(r)
	if !ok {
		writeErr(w, http.StatusUnauthorized, "authentication_required", "a bearer API key is required")
		return
	}
	ref, _, ok := s.resolveTask(r, sc, interfaceID, taskID)
	if !ok {
		writeErr(w, http.StatusNotFound, "not_found", "no such task")
		return
	}
	// Cancel issues a canonical cancel Command (monotonic, retry-safe) and reports any non-cancelable
	// uncertain side-effect honestly (§38.3) rather than claiming a clean cancel.
	res, report, err := s.Runs.Cancel(r.Context(), sc.Organization, sc.Project, ref.RunID)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal_error", "")
		return
	}
	status := TaskStatus{State: MapRunState(res.State), Timestamp: s.now().UTC().Format(time.RFC3339)}
	task := BuildTask(ref.A2ATaskID, ref.A2AContextID, status, s.artifactsFor(res))
	body := map[string]any{"task": task}
	if report.UncertainSideEffect != "" {
		body["uncertainSideEffect"] = report.UncertainSideEffect
	}
	writeJSON(w, http.StatusOK, body)
}

func (s *Server) subscribeTask(w http.ResponseWriter, r *http.Request, interfaceID, taskID string) {
	sc, ok := s.scope(r)
	if !ok {
		writeErr(w, http.StatusUnauthorized, "authentication_required", "a bearer API key is required")
		return
	}
	ref, res, ok := s.resolveTask(r, sc, interfaceID, taskID)
	if !ok {
		writeErr(w, http.StatusNotFound, "not_found", "no such task")
		return
	}
	s.streamRun(w, r, ref.A2ATaskID, ref.A2AContextID, res)
}

// ---- pushNotificationConfigs set/get/list/delete ----

func (s *Server) pushCollection(w http.ResponseWriter, r *http.Request, interfaceID, taskID string) {
	switch r.Method {
	case http.MethodPost:
		s.setPush(w, r, interfaceID, taskID)
	case http.MethodGet:
		s.listPush(w, r, interfaceID, taskID)
	default:
		writeErr(w, http.StatusMethodNotAllowed, "method_not_allowed", "")
	}
}

func (s *Server) pushItem(w http.ResponseWriter, r *http.Request, interfaceID, taskID, configID string) {
	switch r.Method {
	case http.MethodGet:
		s.getPush(w, r, interfaceID, taskID, configID)
	case http.MethodDelete:
		s.deletePush(w, r, interfaceID, taskID, configID)
	default:
		writeErr(w, http.StatusMethodNotAllowed, "method_not_allowed", "")
	}
}

func (s *Server) setPush(w http.ResponseWriter, r *http.Request, interfaceID, taskID string) {
	sc, ok := s.scope(r)
	if !ok {
		writeErr(w, http.StatusUnauthorized, "authentication_required", "a bearer API key is required")
		return
	}
	ref, _, ok := s.resolveTask(r, sc, interfaceID, taskID)
	if !ok {
		writeErr(w, http.StatusNotFound, "not_found", "no such task")
		return
	}
	raw, _ := io.ReadAll(http.MaxBytesReader(nil, r.Body, maxBody))
	var cfg PushNotificationConfig
	if err := json.Unmarshal(raw, &cfg); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid_request", "the request body is not valid JSON")
		return
	}
	if cfg.ID == "" {
		cfg.ID = s.newID("a2apush")
	}
	// Upsert the config into the task's array (replace on matching id).
	next := upsertPush(ref.PushConfigs, cfg)
	if err := s.Tasks.SetPushConfigs(r.Context(), sc.Organization, sc.Project, interfaceID, taskID, next); err != nil {
		writeErr(w, http.StatusInternalServerError, "internal_error", "")
		return
	}
	writeJSON(w, http.StatusOK, redactPush(cfg))
}

func (s *Server) listPush(w http.ResponseWriter, r *http.Request, interfaceID, taskID string) {
	sc, ok := s.scope(r)
	if !ok {
		writeErr(w, http.StatusUnauthorized, "authentication_required", "a bearer API key is required")
		return
	}
	ref, _, ok := s.resolveTask(r, sc, interfaceID, taskID)
	if !ok {
		writeErr(w, http.StatusNotFound, "not_found", "no such task")
		return
	}
	out := make([]PushNotificationConfig, 0, len(ref.PushConfigs))
	for _, c := range ref.PushConfigs {
		out = append(out, redactPush(c))
	}
	writeJSON(w, http.StatusOK, map[string]any{"configs": out})
}

func (s *Server) getPush(w http.ResponseWriter, r *http.Request, interfaceID, taskID, configID string) {
	sc, ok := s.scope(r)
	if !ok {
		writeErr(w, http.StatusUnauthorized, "authentication_required", "a bearer API key is required")
		return
	}
	ref, _, ok := s.resolveTask(r, sc, interfaceID, taskID)
	if !ok {
		writeErr(w, http.StatusNotFound, "not_found", "no such task")
		return
	}
	for _, c := range ref.PushConfigs {
		if c.ID == configID {
			writeJSON(w, http.StatusOK, redactPush(c))
			return
		}
	}
	writeErr(w, http.StatusNotFound, "not_found", "no such push config")
}

func (s *Server) deletePush(w http.ResponseWriter, r *http.Request, interfaceID, taskID, configID string) {
	sc, ok := s.scope(r)
	if !ok {
		writeErr(w, http.StatusUnauthorized, "authentication_required", "a bearer API key is required")
		return
	}
	ref, _, ok := s.resolveTask(r, sc, interfaceID, taskID)
	if !ok {
		writeErr(w, http.StatusNotFound, "not_found", "no such task")
		return
	}
	next := make([]PushNotificationConfig, 0, len(ref.PushConfigs))
	found := false
	for _, c := range ref.PushConfigs {
		if c.ID == configID {
			found = true
			continue
		}
		next = append(next, c)
	}
	if !found {
		writeErr(w, http.StatusNotFound, "not_found", "no such push config")
		return
	}
	if err := s.Tasks.SetPushConfigs(r.Context(), sc.Organization, sc.Project, interfaceID, taskID, next); err != nil {
		writeErr(w, http.StatusInternalServerError, "internal_error", "")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// upsertPush replaces a config with a matching id, else appends it.
func upsertPush(existing []PushNotificationConfig, cfg PushNotificationConfig) []PushNotificationConfig {
	out := make([]PushNotificationConfig, 0, len(existing)+1)
	replaced := false
	for _, c := range existing {
		if c.ID == cfg.ID {
			out = append(out, cfg)
			replaced = true
			continue
		}
		out = append(out, c)
	}
	if !replaced {
		out = append(out, cfg)
	}
	return out
}

// redactPush strips the bearer token from a config before returning it on a read (a secret is a handle,
// never echoed back on a get/list).
func redactPush(c PushNotificationConfig) PushNotificationConfig {
	c.Token = ""
	return c
}

// problem is a deferred error rendering (status + code + detail) an internal helper returns instead of
// writing mid-computation.
type problem struct {
	status int
	code   string
	detail string
}
