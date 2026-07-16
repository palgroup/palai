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

	"github.com/palgroup/palai/apps/control-plane/api"
	"github.com/palgroup/palai/apps/control-plane/api/middleware"
	"github.com/palgroup/palai/packages/coordinator"
)

// Store is a Postgres-backed repository that serves every tenant from one pool;
// each method scopes itself by the verified identity passed in, so no shared
// tenant state leaks between requests.
type Store struct {
	spine *coordinator.Store
}

// Open connects the durable spine. databaseURL carries a local throwaway credential.
func Open(ctx context.Context, databaseURL string) (*Store, error) {
	spine, err := coordinator.Open(ctx, databaseURL)
	if err != nil {
		return nil, err
	}
	return &Store{spine: spine}, nil
}

// Close releases the underlying pool.
func (s *Store) Close() { s.spine.Close() }

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
		})
	if err != nil {
		return api.AdmitResult{}, err
	}
	return api.AdmitResult{
		ResponseID: responseID(adm.Body),
		Body:       adm.Body,
		Replayed:   adm.Replayed,
		Conflict:   adm.Conflict,
	}, nil
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
