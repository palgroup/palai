// Package store is the control-plane's repository boundary over the durable
// execution spine. It carries the verified tenant scope on every call (spec §39.2:
// scope comes from identity, not request-body fields) and delegates persistence to
// the durable coordinator, so request handlers never issue an unscoped tenant
// query. The one read not keyed by tenant is VerifyAPIKey, which establishes the
// tenant from the credential hash. The transactional guarantees live in
// packages/coordinator.
package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/palgroup/palai/apps/control-plane/api"
	"github.com/palgroup/palai/apps/control-plane/api/middleware"
	"github.com/palgroup/palai/apps/control-plane/internal/automation"
	"github.com/palgroup/palai/packages/contracts"
	"github.com/palgroup/palai/packages/coordinator"
)

// Store is a Postgres-backed repository that serves every tenant from one pool;
// each method scopes itself by the verified identity passed in, so no shared
// tenant state leaks between requests.
type Store struct {
	spine   *coordinator.Store
	journal *Journal
	agents  *automation.Store
}

// Open connects the durable spine. databaseURL carries a local throwaway credential.
func Open(ctx context.Context, databaseURL string) (*Store, error) {
	spine, err := coordinator.Open(ctx, databaseURL)
	if err != nil {
		return nil, err
	}
	return &Store{spine: spine, journal: NewJournal(spine.Pool()), agents: automation.New(spine.Pool())}, nil
}

// Close releases the underlying pool.
func (s *Store) Close() { s.spine.Close() }

// Spine exposes the durable coordinator so the composition root (main) can start
// background dispatch — the run workers and the reconciler. Request handlers use the
// tenant-scoped methods on this Store, never the spine directly.
func (s *Store) Spine() *coordinator.Store { return s.spine }

// Migrate applies the forward core migration. It is safe to run repeatedly.
func (s *Store) Migrate(ctx context.Context) error { return s.spine.Migrate(ctx) }

// VerifyAPIKey resolves a bearer key to its verified scope. The presented token is
// never stored or logged; only its hash reaches the database (spec §20 security).
func (s *Store) VerifyAPIKey(ctx context.Context, token string) (middleware.Scope, error) {
	id, err := s.spine.VerifyAPIKey(ctx, token)
	if errors.Is(err, coordinator.ErrInvalidToken) {
		return middleware.Scope{}, middleware.ErrInvalidToken
	}
	if err != nil {
		return middleware.Scope{}, err
	}
	return middleware.Scope{
		Organization: id.Organization,
		Project:      id.Project,
		Principal:    id.Principal,
	}, nil
}

// AdmitResponse runs the idempotent admission transaction within the request's
// verified scope. The Location id is read back from the returned body so a replay
// points at the original resource, not a freshly minted one.
func (s *Store) AdmitResponse(ctx context.Context, req api.AdmitRequest) (api.AdmitResult, error) {
	adm, err := s.spine.AdmitResponse(ctx,
		coordinator.Tenant{Organization: req.Scope.Organization, Project: req.Scope.Project},
		coordinator.AdmissionInput{
			Principal:             req.Scope.Principal,
			IdempotencyKey:        req.IdempotencyKey,
			Method:                req.Method,
			Route:                 req.Route,
			RequestHash:           req.RequestHash,
			ResponseID:            req.ResponseID,
			RunID:                 req.RunID,
			SessionID:             req.SessionID,
			RequestedSessionID:    req.RequestedSessionID,
			PreviousResponseID:    req.PreviousResponseID,
			Input:                 req.Input,
			Body:                  req.Body,
			Store:                 req.Store,
			Delegations:           req.Delegations,
			RepositoryBindingID:   req.RepositoryBindingID,
			RepositoryRef:         req.RepositoryRef,
			AgentRevisionID:       req.AgentRevisionID,
			RunTemplateRevisionID: req.RunTemplateRevisionID,
		})
	if err != nil {
		return api.AdmitResult{}, err
	}
	result := api.AdmitResult{
		ResponseID:                 responseID(adm.Body),
		Body:                       adm.Body,
		Replayed:                   adm.Replayed,
		Conflict:                   adm.Conflict,
		Purged:                     adm.Purged,
		SessionNotFound:            adm.SessionNotFound,
		SessionConflict:            adm.SessionConflict,
		ActiveRunConflict:          adm.ActiveRunConflict,
		RepositoryBindingNotFound:  adm.RepositoryBindingNotFound,
		PinnedRevisionNotFound:     adm.PinnedRevisionNotFound,
		PinnedRevisionNotPublished: adm.PinnedRevisionNotPublished,
	}
	// On a purged replay the body is gone; the tombstone identity is the resource id.
	if adm.Purged {
		result.ResponseID = adm.ResourceTombstone
	}
	return result, nil
}

// GetResponse reads a response's terminal projection within the request's verified
// scope and renders the retrieval body. A missing or foreign row is a miss (404); a
// reaped row is Purged (410). The projection carries the committed status, output,
// usage, the actually-used model, and — on a non-completed terminal — a sanitized
// problem-shaped error whose request_id is stamped from this retrieval (spec §22.3,
// §8.3).
func (s *Store) GetResponse(ctx context.Context, scope middleware.Scope, id string) (api.RetrieveResult, error) {
	view, err := s.spine.GetResponse(ctx, coordinator.Tenant{Organization: scope.Organization, Project: scope.Project}, id)
	if err != nil {
		return api.RetrieveResult{}, err
	}
	if !view.Found {
		return api.RetrieveResult{}, nil
	}
	if view.Purged {
		return api.RetrieveResult{Found: true, Purged: true}, nil
	}
	resp := contracts.Response{
		ID:        contracts.ResponseID(id),
		Object:    "response",
		Status:    view.State,
		CreatedAt: view.CreatedAt.UTC().Format(time.RFC3339Nano),
		Output:    []contracts.ContentItem{},
		Usage:     contracts.Usage{},
	}
	if len(view.Output) > 0 {
		var projection struct {
			Output []contracts.ContentItem `json:"output"`
			Usage  contracts.Usage         `json:"usage"`
			Model  string                  `json:"model"`
			Error  *contracts.Problem      `json:"error"`
		}
		if err := json.Unmarshal(view.Output, &projection); err != nil {
			return api.RetrieveResult{}, fmt.Errorf("decode response projection: %w", err)
		}
		if projection.Output != nil {
			resp.Output = projection.Output
		}
		resp.Usage = projection.Usage
		resp.Model = projection.Model
		if projection.Error != nil {
			// The terminal was finalized off any HTTP request, so its error carries no
			// request_id; stamp this retrieval's so the problem is complete (spec §20.10).
			projection.Error.RequestID = contracts.RequestID(middleware.RequestID(ctx))
			resp.Error = projection.Error
		}
	}
	body, err := json.Marshal(resp)
	if err != nil {
		return api.RetrieveResult{}, fmt.Errorf("marshal response projection: %w", err)
	}
	return api.RetrieveResult{Body: body, Found: true}, nil
}

// CancelResponse cancels a response's run within the request's verified scope and returns
// the canceled terminal projection to render (spec §22.3). An unknown or foreign id is a
// miss (Found=false → 404, same contract as retrieval, leaking no cross-tenant existence).
// A non-terminal run is transitioned via RunCmdCancel — ApplyRunTransition writes the
// run.canceled.v1 terminal event that closes the SSE stream — and the canceled projection is
// finalized. A run already terminal is a monotonic no-op: no second transition, no new event
// (ErrRunTerminal), and the existing terminal projection is returned unchanged, so cancel is
// retry-safe and cancel-after-terminal is safe. The read-back renders the projection body
// (stamping the retrieval's request_id on the problem), reusing the retrieval path.
func (s *Store) CancelResponse(ctx context.Context, scope middleware.Scope, id string) (api.RetrieveResult, error) {
	tenant := coordinator.Tenant{Organization: scope.Organization, Project: scope.Project}
	runID, found, err := s.spine.RunIDForResponse(ctx, tenant, id)
	if err != nil {
		return api.RetrieveResult{}, err
	}
	if !found {
		return api.RetrieveResult{}, nil
	}
	// Route through CancelRunReconciled (spec §26.10, SES-010): it reconciles the run's active external
	// ops to a SINGLE monotonic terminal — plain `canceled`, or `failed_with_uncertain_side_effect` when
	// an irreversible tool effect is still uncertain (its outcome unknown) — drives the run canceled
	// monotonically, and propagates the cancel to non-terminal ChildRuns (SUB-005). Both terminal
	// projections are built here (the RFC-problem shapes) and handed in.
	canceled, err := canceledProjection()
	if err != nil {
		return api.RetrieveResult{}, err
	}
	uncertain, err := uncertainSideEffectProjection()
	if err != nil {
		return api.RetrieveResult{}, err
	}
	if _, err := s.spine.CancelRunReconciled(ctx, tenant, id, runID, canceled, uncertain); err != nil {
		return api.RetrieveResult{}, err
	}
	return s.GetResponse(ctx, scope, id)
}

// canceledProjection builds the terminal Response projection an endpoint-initiated cancel
// finalizes: empty output/usage/model and the sanitized canceled problem. The problem is the
// single contracts.CanceledProblem the engine-terminal path (execution/finalize.go) also
// projects, so a retrieval reads the same canceled terminal whichever path canceled the run;
// the problem's request_id is stamped at retrieval, not here (spec §22.3, §8.3).
func canceledProjection() ([]byte, error) {
	return json.Marshal(map[string]any{
		"output": []contracts.ContentItem{},
		"usage":  contracts.Usage{},
		"model":  "",
		"error":  contracts.CanceledProblem(),
	})
}

// uncertainSideEffectProjection builds the terminal projection for a cancel that hit an uncertain
// irreversible side effect (spec §26.10, SES-010): the failed_with_uncertain_side_effect terminal the
// reconcile loop later resolves. Same shape as canceledProjection with the uncertain-side-effect problem.
func uncertainSideEffectProjection() ([]byte, error) {
	return json.Marshal(map[string]any{
		"output": []contracts.ContentItem{},
		"usage":  contracts.Usage{},
		"model":  "",
		"error":  contracts.UncertainSideEffectProblem(),
	})
}

// CreateSession opens a session within the request's verified scope (spec §9.1). The id is
// minted here (one place mints session ids); the coordinator inserts it active and reads back
// the projection this renders.
func (s *Store) CreateSession(ctx context.Context, scope middleware.Scope) (api.SessionResult, error) {
	view, err := s.spine.CreateSession(ctx, tenantOf(scope), middleware.NewID("ses"))
	if err != nil {
		return api.SessionResult{}, err
	}
	body, err := marshalSession(scope, view)
	if err != nil {
		return api.SessionResult{}, err
	}
	return api.SessionResult{Body: body, Found: true}, nil
}

// GetSession reads a session projection within the request's verified scope (spec §9.1). A
// missing or foreign row is a miss (404), leaking no cross-tenant existence.
func (s *Store) GetSession(ctx context.Context, scope middleware.Scope, id string) (api.SessionResult, error) {
	view, err := s.spine.GetSession(ctx, tenantOf(scope), id)
	if err != nil {
		return api.SessionResult{}, err
	}
	if !view.Found {
		return api.SessionResult{}, nil
	}
	body, err := marshalSession(scope, view)
	if err != nil {
		return api.SessionResult{}, err
	}
	return api.SessionResult{Body: body, Found: true}, nil
}

// CreateRepositoryBinding registers a repository binding within the request's verified scope (spec
// §30.1). The id is minted here (one place mints binding ids); the coordinator inserts it and the
// read-back renders the resource the handler returns. A missing required field is a 400 (Invalid),
// resolved before any write so a malformed request persists nothing.
func (s *Store) CreateRepositoryBinding(ctx context.Context, scope middleware.Scope, req api.RepositoryBindingCreate) (api.BindingResult, error) {
	if req.Provider == "" || req.RepositoryIdentity == "" || req.CloneURL == "" {
		return api.BindingResult{Invalid: true}, nil
	}
	tenant := tenantOf(scope)
	bindingID := middleware.NewID("repo")
	if err := s.spine.CreateRepositoryBinding(ctx, tenant, coordinator.RepositoryBindingInput{
		BindingID:          bindingID,
		Provider:           req.Provider,
		RepositoryIdentity: req.RepositoryIdentity,
		CloneURL:           req.CloneURL,
		DefaultBranch:      req.DefaultBranch,
		ConnectionRef:      req.ConnectionRef,
		AllowedOperations:  req.AllowedOperations,
		Policy:             req.Policy,
		DataClassification: req.DataClassification,
		RegionConstraint:   req.RegionConstraint,
	}); err != nil {
		return api.BindingResult{}, err
	}
	binding, found, err := s.spine.GetRepositoryBinding(ctx, tenant, bindingID)
	if err != nil {
		return api.BindingResult{}, err
	}
	if !found {
		return api.BindingResult{}, fmt.Errorf("repository binding %s vanished after create", bindingID)
	}
	body, err := json.Marshal(binding)
	if err != nil {
		return api.BindingResult{}, fmt.Errorf("marshal repository binding: %w", err)
	}
	return api.BindingResult{Body: body}, nil
}

// AcceptCommand records a durable command within the request's verified scope (spec §22.4).
// A duplicate command_id returns the original resource; an unknown or foreign session is a
// miss (404). The command payload carries the send_message text as the command's own content;
// the durable row and journal keep them apart from the response projection.
func (s *Store) AcceptCommand(ctx context.Context, scope middleware.Scope, sessionID string, req contracts.CommandCreateRequest) (api.CommandResult, error) {
	input := coordinator.CommandInput{
		CommandID: string(req.CommandID),
		Kind:      req.Kind,
		Delivery:  req.Delivery,
	}
	// fork_session opens a new child session; mint its id here (one place mints session ids).
	if req.Kind == "fork_session" {
		input.ForkSessionID = middleware.NewID("ses")
	}
	var err error
	switch req.Kind {
	case "change_config":
		// An immediate switch aborts the in-flight step, so it carries the interrupt delivery the
		// in-flight-abort watcher reads (spec §9.3); a normal switch stays a boundary command.
		input.Payload, err = json.Marshal(map[string]any{"model": req.Model, "tools": req.Tools, "immediate": req.Immediate})
		if req.Immediate {
			input.Delivery = "interrupt"
		}
	default:
		input.Payload, err = json.Marshal(map[string]any{"message": req.Message})
	}
	if err != nil {
		return api.CommandResult{}, err
	}
	cmd, err := s.spine.AcceptCommand(ctx, tenantOf(scope), sessionID, input)
	if err != nil {
		return api.CommandResult{}, err
	}
	if cmd.SessionNotFound {
		return api.CommandResult{SessionNotFound: true}, nil
	}
	body, err := marshalCommand(cmd)
	if err != nil {
		return api.CommandResult{}, err
	}
	return api.CommandResult{Body: body}, nil
}

// marshalSession renders a session projection. organization_id/project_id come from the
// verified scope, never a request field (spec §39.2).
func marshalSession(scope middleware.Scope, view coordinator.SessionView) ([]byte, error) {
	return json.Marshal(contracts.Session{
		ID:             contracts.SessionID(view.ID),
		Object:         "session",
		Status:         view.State,
		CreatedAt:      view.CreatedAt.UTC().Format(time.RFC3339Nano),
		OrganizationID: contracts.OrganizationID(scope.Organization),
		ProjectID:      contracts.ProjectID(scope.Project),
	})
}

// marshalCommand renders a command projection from the durable row.
func marshalCommand(cmd coordinator.Command) ([]byte, error) {
	out := contracts.Command{
		ID:        contracts.CommandID(cmd.ID),
		Object:    "command",
		SessionID: contracts.SessionID(cmd.SessionID),
		Kind:      cmd.Kind,
		Delivery:  cmd.Delivery,
		Status:    cmd.State,
		CreatedAt: cmd.CreatedAt.UTC().Format(time.RFC3339Nano),
	}
	if cmd.AppliedSequence != nil {
		seq := int(*cmd.AppliedSequence)
		out.AppliedSequence = &seq
	}
	if len(cmd.Result) > 0 {
		if err := json.Unmarshal(cmd.Result, &out.Result); err != nil {
			return nil, fmt.Errorf("decode command result: %w", err)
		}
	}
	return json.Marshal(out)
}

// tenantOf projects a verified scope onto the coordinator tenant key.
func tenantOf(scope middleware.Scope) coordinator.Tenant {
	return coordinator.Tenant{Organization: scope.Organization, Project: scope.Project}
}

// PurgeExpiredStoreFalse runs one retention sweep over the durable spine, reaping the
// content of store=false responses whose terminal state has aged past ttl (spec §8.3). It
// returns the purged count and the object keys of the scrubbed artifacts so the reaper can
// delete their bytes from the object store after the sweep commits (LP §7.2).
func (s *Store) PurgeExpiredStoreFalse(ctx context.Context, ttl time.Duration) (int, []string, error) {
	return s.spine.PurgeExpiredStoreFalse(ctx, ttl)
}

// SessionExists reports whether the session is visible in the given tenant scope;
// the event stream uses it as the 404 gate for a foreign or unknown session.
func (s *Store) SessionExists(ctx context.Context, org, project, sessionID string) (bool, error) {
	return s.journal.SessionExists(ctx, org, project, sessionID)
}

// ResolveCursor maps a Last-Event-ID to its per-session sequence within scope.
func (s *Store) ResolveCursor(ctx context.Context, org, project, sessionID, eventID string) (int64, bool, error) {
	return s.journal.ResolveCursor(ctx, org, project, sessionID, eventID)
}

// After returns up to limit tenant-scoped events with sequence greater than
// afterSeq, in ascending order, as CloudEvents envelopes.
func (s *Store) After(ctx context.Context, org, project, sessionID string, afterSeq int64, limit int) ([]contracts.Event, error) {
	return s.journal.After(ctx, org, project, sessionID, afterSeq, limit)
}

// RecordAttachDenied records a content-free audit denial for an out-of-scope attach.
func (s *Store) RecordAttachDenied(ctx context.Context, org, project, principal, sessionID string) error {
	return s.journal.RecordAttachDenied(ctx, org, project, principal, sessionID)
}

// responseID reads the response id from a stored/created response body so both the
// created and replayed paths yield the same Location.
func responseID(body []byte) string {
	var envelope struct {
		ID string `json:"id"`
	}
	_ = json.Unmarshal(body, &envelope)
	return envelope.ID
}
