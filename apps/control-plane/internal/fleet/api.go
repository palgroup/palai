package fleet

import (
	"context"

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

func runnerItem(r Runner) api.RunnerItem {
	return api.RunnerItem{
		ID: r.ID, PoolID: r.PoolID, Label: r.Label, DNS: r.DNS, PublicKeySHA256: r.PublicKeySHA256,
		State: r.State, OS: r.OS, Arch: r.Arch, Posture: r.Posture, Capacity: r.Capacity,
		CertNotAfter: r.CertNotAfter, EnrolledAt: r.EnrolledAt, LastSeenAt: r.LastSeenAt,
		CreatedAt: r.CreatedAt,
	}
}
