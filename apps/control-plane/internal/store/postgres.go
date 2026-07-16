// Package store is the control-plane's repository boundary over the durable
// execution spine. It binds the verified tenant scope once (spec §39.2: scope
// comes from identity, not request-body fields) and delegates persistence to the
// durable coordinator, so request handlers cannot accidentally issue an unscoped
// query. The transactional guarantees themselves live in packages/coordinator.
//
// ponytail: thin tenant-binding facade — it gains handler-facing methods as the
// control-plane API lands; today it fixes the tenant-scope invariant in one place.
package store

import (
	"context"

	"github.com/palgroup/palai/packages/coordinator"
	statemachines "github.com/palgroup/palai/packages/state-machines"
)

// Store is a tenant-bound view of the durable coordinator.
type Store struct {
	spine  *coordinator.Store
	tenant coordinator.Tenant
}

// Open connects the durable spine and binds it to a verified tenant scope.
func Open(ctx context.Context, databaseURL string, tenant coordinator.Tenant) (*Store, error) {
	spine, err := coordinator.Open(ctx, databaseURL)
	if err != nil {
		return nil, err
	}
	return &Store{spine: spine, tenant: tenant}, nil
}

// Close releases the underlying pool.
func (s *Store) Close() { s.spine.Close() }

// ApplyRunTransition applies a run command within the bound tenant scope. It is
// the only mutation path; session-sequence allocation happens inside it, gated by
// the tenant-scoped run lookup, so there is no unscoped allocation surface.
func (s *Store) ApplyRunTransition(ctx context.Context, runID string, command statemachines.RunCommand) (coordinator.Transition, error) {
	return s.spine.ApplyRunTransition(ctx, s.tenant, runID, command)
}
