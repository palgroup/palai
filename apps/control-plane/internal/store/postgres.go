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
	"github.com/palgroup/palai/packages/contracts"
	"github.com/palgroup/palai/packages/coordinator"
)

// Store is a Postgres-backed repository that serves every tenant from one pool;
// each method scopes itself by the verified identity passed in, so no shared
// tenant state leaks between requests.
type Store struct {
	spine   *coordinator.Store
	journal *Journal
}

// Open connects the durable spine. databaseURL carries a local throwaway credential.
func Open(ctx context.Context, databaseURL string) (*Store, error) {
	spine, err := coordinator.Open(ctx, databaseURL)
	if err != nil {
		return nil, err
	}
	return &Store{spine: spine, journal: NewJournal(spine.Pool())}, nil
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
			Principal:      req.Scope.Principal,
			IdempotencyKey: req.IdempotencyKey,
			Method:         req.Method,
			Route:          req.Route,
			RequestHash:    req.RequestHash,
			ResponseID:     req.ResponseID,
			RunID:          req.RunID,
			SessionID:      req.SessionID,
			Input:          req.Input,
			Body:           req.Body,
			Store:          req.Store,
		})
	if err != nil {
		return api.AdmitResult{}, err
	}
	result := api.AdmitResult{
		ResponseID: responseID(adm.Body),
		Body:       adm.Body,
		Replayed:   adm.Replayed,
		Conflict:   adm.Conflict,
		Purged:     adm.Purged,
	}
	// On a purged replay the body is gone; the tombstone identity is the resource id.
	if adm.Purged {
		result.ResponseID = adm.ResourceTombstone
	}
	return result, nil
}

// GetResponse reads a response's terminal projection within the request's verified
// scope and renders the retrieval body. A missing or foreign row is a miss (404); a
// reaped row is Purged (410). Model is not part of the durable terminal projection, so
// the retrieved resource carries the committed status, output, and usage (spec §22.3).
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
		}
		if err := json.Unmarshal(view.Output, &projection); err != nil {
			return api.RetrieveResult{}, fmt.Errorf("decode response projection: %w", err)
		}
		if projection.Output != nil {
			resp.Output = projection.Output
		}
		resp.Usage = projection.Usage
	}
	body, err := json.Marshal(resp)
	if err != nil {
		return api.RetrieveResult{}, fmt.Errorf("marshal response projection: %w", err)
	}
	return api.RetrieveResult{Body: body, Found: true}, nil
}

// PurgeExpiredStoreFalse runs one retention sweep over the durable spine, reaping the
// content of store=false responses whose terminal state has aged past ttl (spec §8.3).
func (s *Store) PurgeExpiredStoreFalse(ctx context.Context, ttl time.Duration) (int, error) {
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

// responseID reads the response id from a stored/created response body so both the
// created and replayed paths yield the same Location.
func responseID(body []byte) string {
	var envelope struct {
		ID string `json:"id"`
	}
	_ = json.Unmarshal(body, &envelope)
	return envelope.ID
}
