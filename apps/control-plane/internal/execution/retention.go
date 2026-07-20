package execution

import (
	"context"
	"log"
	"time"

	"github.com/palgroup/palai/apps/control-plane/internal/store"
)

// ArtifactDeleter erases an artifact's bytes from the object store by object key. It is
// the seam the retention sweep uses to close the store:false purge, satisfied by the
// control-plane's S3-backed artifacts.Store (spec §24 — the deleter, and thus the S3
// credential, lives only in the control plane).
type ArtifactDeleter interface {
	Delete(ctx context.Context, objectKey string) error
}

// Reaper periodically purges the content of store=false responses whose retention TTL
// has elapsed, leaving a tombstone (spec §8.3, §20.9). It is the retention sibling of
// the Reconciler's dead-letter sweep: a durable maintenance job on the coordinator that
// runs one bounded, tenant-safe pass per tick.
type Reaper struct {
	store         *store.Store
	storeFalseTTL time.Duration
	artifacts     ArtifactDeleter // optional; nil scrubs only the DB row (no object store)
}

// NewReaper binds the store:false retention TTL to the store.
func NewReaper(store *store.Store, storeFalseTTL time.Duration) *Reaper {
	return &Reaper{store: store, storeFalseTTL: storeFalseTTL}
}

// WithArtifactStore wires the object-store byte-deleter the purge uses to erase an expired
// run's artifact bytes (LP §7.2). Nil (the default) scrubs only the DB row, which keeps the
// deployments and tests that run without an object store working unchanged.
func (r *Reaper) WithArtifactStore(a ArtifactDeleter) *Reaper {
	r.artifacts = a
	return r
}

// Sweep runs one retention pass and returns the number of responses purged. The DB scrub
// commits first with each victim's object_key cleared, so the keys it named are surfaced
// here and their bytes deleted from the object store afterward. A delete error is returned
// and logged by Run, but it is NOT retried: the scrub already committed the key away, so a
// failed delete orphans that object exactly like the crash case below — no later tick can
// re-reach those bytes. The purge itself is durable — the rows are correctly tombstoned.
// ponytail: a crash between the commit and a delete orphans that object in S3 — a leaked
// byte range, not a correctness break, swept by the same list-vs-rows reconcile the write
// path defers; wiring an orphan GC now is speculative for a local dev store.
func (r *Reaper) Sweep(ctx context.Context) (purged int, err error) {
	purged, objectKeys, err := r.store.PurgeExpiredStoreFalse(ctx, r.storeFalseTTL)
	if err != nil {
		return 0, err
	}
	if r.artifacts != nil {
		for _, key := range objectKeys {
			if derr := r.artifacts.Delete(ctx, key); derr != nil && err == nil {
				err = derr
			}
		}
	}
	return purged, err
}

// Run sweeps every interval until ctx is cancelled. A sweep error is logged (the reaper's
// only failure surface) and non-fatal: the next tick retries the DB purge, because a
// transient database blip must not stop retention. A delete failure is not retried — it
// orphans the scrubbed object's bytes (see Sweep), so the log is its only trace.
func (r *Reaper) Run(ctx context.Context, interval time.Duration) error {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			if _, err := r.Sweep(ctx); err != nil {
				log.Printf("retention reaper sweep failed: %v", err)
			}
		}
	}
}
