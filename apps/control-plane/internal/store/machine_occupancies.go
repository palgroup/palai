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
	return occupancyItems(rows), nil
}

// occupancyItems maps coordinator rows onto the API type, and is shared by both reads so a hold cannot
// render one way per machine and another per session.
func occupancyItems(rows []coordinator.Occupancy) []api.MachineOccupancyItem {
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
	return items
}

// SessionOccupancies implements the other half of api.MachineOccupancyAPI: every machine ONE session has
// held. It shares occupancyItems with its sibling for the reason that one is a projection — the billed
// interval's rule lives in a single SQL CASE, and a second mapping here would be a second answer to a
// question a customer is billed on.
func (s *Store) SessionOccupancies(ctx context.Context, project, sessionID string) ([]api.MachineOccupancyItem, error) {
	rows, err := s.spine.SessionOccupancies(ctx, coordinator.Tenant{Project: project}, sessionID)
	if err != nil {
		return nil, err
	}
	return occupancyItems(rows), nil
}
