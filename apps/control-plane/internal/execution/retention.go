package execution

import (
	"context"
	"time"

	"github.com/palgroup/palai/apps/control-plane/internal/store"
)

// Reaper periodically purges the content of store=false responses whose retention TTL
// has elapsed, leaving a tombstone (spec §8.3, §20.9). It is the retention sibling of
// the Reconciler's dead-letter sweep: a durable maintenance job on the coordinator that
// runs one bounded, tenant-safe pass per tick.
type Reaper struct {
	store         *store.Store
	storeFalseTTL time.Duration
}

// NewReaper binds the store:false retention TTL to the store.
func NewReaper(store *store.Store, storeFalseTTL time.Duration) *Reaper {
	return &Reaper{store: store, storeFalseTTL: storeFalseTTL}
}

// Sweep runs one retention pass and returns the number of responses purged.
func (r *Reaper) Sweep(ctx context.Context) (purged int, err error) {
	return r.store.PurgeExpiredStoreFalse(ctx, r.storeFalseTTL)
}

// Run sweeps every interval until ctx is cancelled. A sweep error is non-fatal: the
// next tick retries, because a transient database blip must not stop retention.
func (r *Reaper) Run(ctx context.Context, interval time.Duration) error {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			_, _ = r.Sweep(ctx)
		}
	}
}
