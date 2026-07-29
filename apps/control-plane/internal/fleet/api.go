package fleet

import (
	"context"
	"errors"
	"time"

	"github.com/palgroup/palai/apps/control-plane/api"
)

// The adapter that makes Store the api.RunnerRegistryAPI the read routes take. It is a projection and
// nothing more: no field is computed here, and in particular nothing is derived that would look like
// health. The registry records when a machine last authenticated; deciding what that MEANS is T5's,
// and a `healthy` bool invented in this file would be a lie with a type.

// ListRunners implements api.RunnerRegistryAPI.
func (s *Store) ListRunners(ctx context.Context, org, project string, w api.RunnerListWindow) ([]api.RunnerItem, error) {
	rows, err := s.List(ctx, org, project, ListWindow{
		CreatedGTE: w.CreatedGTE, CreatedLTE: w.CreatedLTE,
		AfterCreatedAt: w.AfterCreatedAt, AfterID: w.AfterID, Limit: w.Limit,
	})
	if err != nil {
		return nil, err
	}
	out := make([]api.RunnerItem, 0, len(rows))
	for _, row := range rows {
		out = append(out, runnerItem(row))
	}
	return out, nil
}

// GetRunner implements api.RunnerRegistryAPI.
func (s *Store) GetRunner(ctx context.Context, org, project, id string) (api.RunnerItem, bool, error) {
	row, found, err := s.Get(ctx, org, project, id)
	if err != nil || !found {
		return api.RunnerItem{}, false, err
	}
	return runnerItem(row), true, nil
}

// ListRunnerPools implements api.RunnerRegistryAPI (E24 T2).
func (s *Store) ListRunnerPools(ctx context.Context, org, project string, w api.RunnerListWindow) ([]api.RunnerPoolItem, error) {
	rows, err := s.ListPools(ctx, org, project, ListWindow{
		CreatedGTE: w.CreatedGTE, CreatedLTE: w.CreatedLTE,
		AfterCreatedAt: w.AfterCreatedAt, AfterID: w.AfterID, Limit: w.Limit,
	})
	if err != nil {
		return nil, err
	}
	out := make([]api.RunnerPoolItem, 0, len(rows))
	for _, row := range rows {
		out = append(out, api.RunnerPoolItem{
			ID: row.ID, Name: row.Name, Posture: row.Posture, OS: row.OS, Arch: row.Arch,
			StrictEnrollment: row.StrictEnrollment, CreatedAt: row.CreatedAt,
		})
	}
	return out, nil
}

// THE KEY SURFACE ADAPTER (E24 T3). It hangs off PoolEnrollmentKeys rather than Store because the two
// are different concerns — inventory and credential — and the router takes ONE interface, so the
// composition root passes a small struct that holds both. That is cheaper than merging the types and it
// keeps the credential path out of every caller that only wants to list machines.

// RegistryAPI is the api.RunnerRegistryAPI implementation: the registry's reads plus the key surface's
// writes, joined where the router needs them joined and nowhere deeper.
type RegistryAPI struct {
	*Store
	keys *PoolEnrollmentKeys
}

// NewRegistryAPI joins the two. keys may be nil, in which case the key routes answer as if no pool had
// keys — a deployment that wires no key store has none.
func NewRegistryAPI(store *Store, keys *PoolEnrollmentKeys) *RegistryAPI {
	return &RegistryAPI{Store: store, keys: keys}
}

// MintRunnerPoolKey implements api.RunnerRegistryAPI.
func (a *RegistryAPI) MintRunnerPoolKey(ctx context.Context, org, project, poolID string, expiresAt *time.Time) (api.RunnerPoolKeyItem, bool, error) {
	if a.keys == nil {
		return api.RunnerPoolKeyItem{}, false, nil
	}
	minted, err := a.keys.Mint(ctx, org, project, poolID, expiresAt)
	if errors.Is(err, ErrUnknownPool) {
		return api.RunnerPoolKeyItem{}, false, nil
	}
	if err != nil {
		return api.RunnerPoolKeyItem{}, false, err
	}
	return api.RunnerPoolKeyItem{
		ID: minted.ID, PoolID: minted.PoolID, Prefix: minted.Prefix, Value: minted.Value,
		CreatedAt: minted.CreatedAt, ExpiresAt: minted.ExpiresAt,
	}, true, nil
}

// ListRunnerPoolKeys implements api.RunnerRegistryAPI.
func (a *RegistryAPI) ListRunnerPoolKeys(ctx context.Context, org, project, poolID string) ([]api.RunnerPoolKeyItem, error) {
	if a.keys == nil {
		return []api.RunnerPoolKeyItem{}, nil
	}
	rows, err := a.keys.List(ctx, org, project, poolID)
	if err != nil {
		return nil, err
	}
	out := make([]api.RunnerPoolKeyItem, 0, len(rows))
	for _, row := range rows {
		out = append(out, poolKeyItem(row))
	}
	return out, nil
}

// RevokeRunnerPoolKey implements api.RunnerRegistryAPI.
func (a *RegistryAPI) RevokeRunnerPoolKey(ctx context.Context, org, project, keyID string) (api.RunnerPoolKeyItem, bool, error) {
	if a.keys == nil {
		return api.RunnerPoolKeyItem{}, false, nil
	}
	revoked, err := a.keys.Revoke(ctx, org, project, keyID)
	if errors.Is(err, ErrUnknownPoolKey) {
		return api.RunnerPoolKeyItem{}, false, nil
	}
	if err != nil {
		return api.RunnerPoolKeyItem{}, false, err
	}
	item := poolKeyItem(revoked.Key)
	// Always non-nil, including when it is empty: "this key admitted no machine" and "we did not look"
	// are different answers, and the renderer distinguishes them by nil.
	item.EnrolledRunners = make([]api.RunnerPoolKeyEnrollment, 0, len(revoked.EnrolledRunners))
	for _, m := range revoked.EnrolledRunners {
		item.EnrolledRunners = append(item.EnrolledRunners, api.RunnerPoolKeyEnrollment{
			ID: m.ID, Label: m.Label, DNS: m.DNS, State: m.State, PoolID: m.PoolID, EnrolledAt: m.EnrolledAt,
		})
	}
	return item, true, nil
}

// poolKeyItem projects a key's metadata. There is no branch here that could carry a value: PoolKey does
// not have one, which is the type system doing the work a review would otherwise have to.
func poolKeyItem(row PoolKey) api.RunnerPoolKeyItem {
	return api.RunnerPoolKeyItem{
		ID: row.ID, PoolID: row.PoolID, Prefix: row.Prefix, CreatedAt: row.CreatedAt,
		ExpiresAt: row.ExpiresAt, RevokedAt: row.RevokedAt, LastUsedAt: row.LastUsedAt,
	}
}

func runnerItem(r Runner) api.RunnerItem {
	return api.RunnerItem{
		ID: r.ID, PoolID: r.PoolID, Label: r.Label, DNS: r.DNS, PublicKeySHA256: r.PublicKeySHA256,
		State: r.State, OS: r.OS, Arch: r.Arch, Posture: r.Posture, Capacity: r.Capacity,
		CertNotAfter: r.CertNotAfter, EnrolledAt: r.EnrolledAt, LastSeenAt: r.LastSeenAt,
		CreatedAt: r.CreatedAt,
	}
}
