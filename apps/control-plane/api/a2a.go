package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/palgroup/palai/adapters/integrations/a2a"
	"github.com/palgroup/palai/apps/control-plane/api/middleware"
	"github.com/palgroup/palai/packages/contracts"
)

// a2aAdmitRoute is the idempotency route the A2A admission path keys on. It is DELIBERATELY distinct from
// createRoute ("/v1/responses") so an A2A messageId can never collide with a native Idempotency-Key of the
// same value (each surface owns its own idempotency namespace).
const a2aAdmitRoute = "/v1/a2a/messages"

// a2aScopeFunc is the PRODUCTION ScopeFunc the A2A server reads its authenticated tenant from: the scope the
// auth middleware published from the verified bearer. It is the ONLY identity authority — never anything the
// A2A client supplies (§38.6). NewA2AServer wires exactly this, and the router mounts the authed surface
// INSIDE middleware.Auth, so ScopeFrom is populated by the time a handler runs.
func a2aScopeFunc(r *http.Request) (a2a.Scope, bool) {
	s, ok := middleware.ScopeFrom(r.Context())
	return a2a.Scope{Organization: s.Organization, Project: s.Project}, ok
}

// NewA2AServer builds the production A2A 1.0 server projection (E17 T2, spec §38). It wires the real
// admission Admitter behind the narrow a2a.Runs seam (an inbound A2A message becomes exactly the admission
// POST /v1/responses takes — no invented run identity, §34.1), the DB-backed interface + task store, the
// production ScopeFunc, and the per-project admission caps. Files/Pusher are left nil: inbound file ingest
// and push DELIVERY are honest ceilings (§5/§6) — the card advertises push only when a Pusher is wired, and
// the message path never silently drops a file part (it fails the request if a real Files sink is later set).
func NewA2AServer(admitter Admitter, interfaces a2a.InterfaceStore, tasks a2a.Tasks, limits AdmissionLimits, baseURL string) *a2a.Server {
	return &a2a.Server{
		Interfaces: interfaces,
		Runs:       a2aRuns{admitter: admitter, limits: limits},
		Tasks:      tasks,
		ScopeFunc:  a2aScopeFunc,
		BaseURL:    baseURL,
		NewID:      middleware.NewID,
	}
}

// a2aRuns adapts the response-admission Admitter to the A2A server's narrow Runs seam. It invents no run
// identity (§34.1): the platform mints the canonical response/run/session ids inside AdmitResponse and this
// adapter returns them, never anything an A2A client supplied. Get/Cancel resolve the canonical RESPONSE
// resource — the run's retrievable identity in the platform's own API (GetResponse/CancelResponse key on it),
// which the a2a_task_refs row bridges to beside the external A2A ids (§38.2). So an A2A task reads the exact
// state the native /v1/responses GET reports, and a cancel routes through the SAME §26.10 reconcile the
// native cancel does (uncertain-side-effect honesty included).
type a2aRuns struct {
	admitter Admitter
	limits   AdmissionLimits
}

// Admit runs the canonical admission for an inbound A2A message. It mints the same resp/run/ses ids create()
// mints, builds the same queued projection, and delegates the atomic reservation to AdmitResponse. A typed
// admission rejection (bad pin, session/limit/conflict) becomes an error, so message:send surfaces a 502
// admission_failed rather than a fabricated task.
func (a a2aRuns) Admit(ctx context.Context, req a2a.RunRequest) (a2a.RunResult, error) {
	scope := middleware.Scope{Organization: req.Org, Project: req.Project}
	responseID := middleware.NewID("resp")
	runID := middleware.NewID("run")
	sessionID := middleware.NewID("ses")

	create := contracts.ResponseCreateRequest{Input: req.Input, Store: req.Store}
	if req.AgentRevisionID != "" {
		rev := req.AgentRevisionID
		create.AgentRevisionID = &rev
	}
	hash, err := canonicalRequestHash(create)
	if err != nil {
		return a2a.RunResult{}, fmt.Errorf("hash a2a request: %w", err)
	}
	body, err := json.Marshal(contracts.Response{
		ID:             contracts.ResponseID(responseID),
		Object:         "response",
		Status:         "queued",
		CreatedAt:      time.Now().UTC().Format(time.RFC3339Nano),
		Output:         []contracts.ContentItem{},
		Usage:          contracts.Usage{},
		SessionID:      contracts.SessionID(sessionID),
		RunID:          contracts.RunID(runID),
		OrganizationID: contracts.OrganizationID(req.Org),
		ProjectID:      contracts.ProjectID(req.Project),
	})
	if err != nil {
		return a2a.RunResult{}, fmt.Errorf("marshal a2a projection: %w", err)
	}
	input, err := json.Marshal(req.Input)
	if err != nil {
		return a2a.RunResult{}, fmt.Errorf("marshal a2a input: %w", err)
	}

	out, err := a.admitter.AdmitResponse(ctx, AdmitRequest{
		Scope:             scope,
		IdempotencyKey:    req.IdempotencyKey,
		Method:            http.MethodPost,
		Route:             a2aAdmitRoute,
		RequestHash:       hash,
		ResponseID:        responseID,
		RunID:             runID,
		SessionID:         sessionID,
		Input:             input,
		Body:              body,
		Store:             req.Store,
		AgentRevisionID:   req.AgentRevisionID,
		MaxConcurrentRuns: a.limits.MaxConcurrentRuns,
		MaxQueuedRuns:     a.limits.MaxQueuedRuns,
	})
	if err != nil {
		return a2a.RunResult{}, err
	}
	if rej := admitRejection(out); rej != "" {
		return a2a.RunResult{}, fmt.Errorf("a2a run not admitted: %s", rej)
	}

	// On a replay out.Body/out.ResponseID are the ORIGINAL resource, so a retried messageId yields the same
	// canonical response — the dedupe the A2A retry story relies on (M-2).
	state, session, outputText := projectResponse(out.Body)
	if session == "" {
		session = sessionID
	}
	return a2a.RunResult{
		RunID:      out.ResponseID, // the canonical response id; Get/Cancel resolve by it
		SessionID:  session,
		State:      state,
		OutputText: outputText,
		Durable:    req.Store,
	}, nil
}

// Get resolves the canonical response the A2A task bridges to. A missing or reaped response reports
// found=false, which the server projects to an honest terminal/unknown state rather than a forever-"working"
// task (M-1).
func (a a2aRuns) Get(ctx context.Context, org, project, responseID string) (a2a.RunResult, bool, error) {
	res, err := a.admitter.GetResponse(ctx, middleware.Scope{Organization: org, Project: project}, responseID)
	if err != nil {
		return a2a.RunResult{}, false, err
	}
	if !res.Found || res.Purged {
		return a2a.RunResult{RunID: responseID}, false, nil
	}
	state, _, outputText := projectResponse(res.Body)
	return a2a.RunResult{RunID: responseID, State: state, OutputText: outputText, Durable: true}, true, nil
}

// Cancel issues the canonical cancel through the SAME reconcile the native cancel uses and reports any
// non-cancelable uncertain side-effect honestly (§38.3) instead of claiming a clean cancel.
func (a a2aRuns) Cancel(ctx context.Context, org, project, responseID string) (a2a.RunResult, a2a.CancelReport, error) {
	res, err := a.admitter.CancelResponse(ctx, middleware.Scope{Organization: org, Project: project}, responseID)
	if err != nil {
		return a2a.RunResult{}, a2a.CancelReport{}, err
	}
	if !res.Found {
		return a2a.RunResult{RunID: responseID}, a2a.CancelReport{}, nil
	}
	state, _, outputText := projectResponse(res.Body)
	report := a2a.CancelReport{}
	if code := problemCode(res.Body); code == contracts.UncertainSideEffectProblem().Code {
		report.UncertainSideEffect = contracts.UncertainSideEffectProblem().Detail
	}
	return a2a.RunResult{RunID: responseID, State: state, OutputText: outputText, Durable: true}, report, nil
}

// admitRejection names the typed admission reject a message:send must surface as a failed admission (rather
// than mint a task over a run that never started). Empty means the run was admitted.
func admitRejection(out AdmitResult) string {
	switch {
	case out.SessionNotFound:
		return "no such session in this project"
	case out.SessionConflict:
		return "the session is not active"
	case out.ActiveRunConflict:
		return "the session already has an active run"
	case out.RepositoryBindingNotFound:
		return "no such repository binding"
	case out.PinnedRevisionNotFound:
		return "no such agent revision"
	case out.PinnedRevisionNotPublished:
		return "the pinned revision is a draft"
	case out.ConcurrencyLimited:
		return "too many concurrent runs"
	case out.QueueDepthExceeded:
		return "the run queue is full"
	case out.Conflict:
		return "the idempotency key was reused with a different request"
	case out.Purged:
		return "the idempotent result has been reaped"
	case out.LimitExceeded != nil:
		return "a durable budget or quota is exhausted"
	}
	return ""
}

// projectResponse pulls the projected status, session id, and concatenated text output from a stored
// response body. It tolerates the two shapes the surface produces: the queued admission projection (carries
// session_id) and the retrieval projection (carries output text, no session id).
func projectResponse(body []byte) (state, sessionID, outputText string) {
	if len(body) == 0 {
		return "", "", ""
	}
	var proj struct {
		Status    string           `json:"status"`
		SessionID string           `json:"session_id"`
		Output    []map[string]any `json:"output"`
	}
	if err := json.Unmarshal(body, &proj); err != nil {
		return "", "", ""
	}
	var b strings.Builder
	for _, item := range proj.Output {
		// The canonical output-text shape (mirrors execution.childOutputText): each item carries its text in
		// a string `content`. Non-text/richer parts are not projected here (text-output ceiling).
		if content, ok := item["content"].(string); ok && content != "" {
			if b.Len() > 0 {
				b.WriteString("\n")
			}
			b.WriteString(content)
		}
	}
	return proj.Status, proj.SessionID, b.String()
}

// problemCode reads the error.code off a terminal projection (the canceled / uncertain-side-effect problem),
// or "" when the projection carries no error.
func problemCode(body []byte) string {
	if len(body) == 0 {
		return ""
	}
	var proj struct {
		Error *struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(body, &proj); err != nil || proj.Error == nil {
		return ""
	}
	return proj.Error.Code
}
