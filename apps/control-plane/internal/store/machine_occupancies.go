package store

import (
	"context"
	"time"

	"github.com/palgroup/palai/apps/control-plane/api"
	"github.com/palgroup/palai/packages/coordinator"
)

// MachineOccupancies implements api.MachineOccupancyAPI: one machine's session history, newest first.
//
// ‼️ IT IS A PROJECTION AND NOT A SECOND READ. The coordinator owns what an occupancy IS — including the
// billed interval, whose rule lives in exactly one SQL CASE — and this maps that row onto the API type.
// Recomputing any of it here would be a second answer to a question a customer is billed on.
//
// The tenant travels as a coordinator.Tenant because the store's scope is what the query's WHERE reads;
// the handler has already refused an id in another project, so this is the second of two independent
// scopes rather than the only one.
func (s *Store) MachineOccupancies(ctx context.Context, project, runnerID string, before time.Time, beforeID string, limit int) ([]api.MachineOccupancyItem, error) {
	rows, err := s.spine.MachineOccupancies(ctx, coordinator.Tenant{Project: project}, runnerID, before, beforeID, limit)
	if err != nil {
		return nil, err
	}
	items := make([]api.MachineOccupancyItem, 0, len(rows))
	for _, r := range rows {
		items = append(items, api.MachineOccupancyItem{
			ID:             r.ID,
			SessionID:      r.SessionID,
			StartedAt:      r.StartedAt,
			LastActivityAt: r.LastActivityAt,
			ReleasedAt:     r.ReleasedAt,
			ReleaseReason:  r.ReleaseReason,
			// The coordinator carries a Duration; the API renders seconds. Converted here rather than
			// re-derived, so the number the panel shows and the number the ledger bills come from one
			// SQL CASE.
			BilledSeconds: r.Billed.Seconds(),
		})
	}
	return items, nil
}
