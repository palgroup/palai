package artifacts

import (
	"context"
	"fmt"
	"log"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/palgroup/palai/storage"
)

// Collector reclaims orphaned objects — bytes in the store that no live artifacts row
// references — with ONE reconcile that closes both write-path gaps retention cannot reach:
// an object whose row insert never committed (writer.go), and a retention delete that failed
// after the row was already tombstoned (retention.go). Both surface identically here: the
// object is present, but no non-empty object_key row points at it. The reconcile lists the
// bucket, subtracts the referenced keys, and deletes what remains once it is older than the
// grace window. A referenced object is NEVER deleted; the delete decision is the pure absence
// of a referencing row, independent of tenant — the loop is control-plane-internal (spec §24),
// so it discloses nothing across tenants (spec §22.6; E10 REC-004).
type Collector struct {
	store  *Store
	pool   *pgxpool.Pool
	grace  time.Duration
	rounds atomic.Int64
}

// NewCollector binds the object store, the durable pool the reference set is read from, and
// the grace window that spares an object whose row may still be committing. The grace must
// comfortably exceed the worst-case object-PUT→row-insert gap, or a live in-progress write
// could be reclaimed.
func NewCollector(store *Store, pool *pgxpool.Pool, grace time.Duration) *Collector {
	return &Collector{store: store, pool: pool, grace: grace}
}

// Collect runs one reconcile pass and returns the number of orphan objects reclaimed. It
// lists the objects FIRST and reads the referenced keys AFTER, so a row that commits mid-pass
// is still seen as a reference — belt-and-suspenders atop the grace window, which is the
// primary guard against reclaiming an object whose row has not yet been written. A delete
// failure is best-effort: it is recorded and the pass continues (the next round retries),
// mirroring the retention reaper.
func (c *Collector) Collect(ctx context.Context) (int, error) {
	objects, err := c.store.List(ctx)
	if err != nil {
		return 0, fmt.Errorf("orphan-gc: list objects: %w", err)
	}
	referenced, err := c.referencedKeys(ctx)
	if err != nil {
		return 0, err
	}
	cutoff := time.Now().Add(-c.grace)
	reclaimed := 0
	for _, obj := range objects {
		if _, ok := referenced[obj.Key]; ok {
			continue // referenced by a live row — never deleted (the safety invariant)
		}
		if withinGrace(obj.LastModified, cutoff) {
			continue // inside the grace window (or unknown mtime) — a row may still be committing
		}
		if derr := c.store.Delete(ctx, obj.Key); derr != nil {
			if err == nil {
				err = fmt.Errorf("orphan-gc: delete orphan %q: %w", obj.Key, derr)
			}
			continue // best-effort: a transient delete failure is retried next round
		}
		reclaimed++
	}
	return reclaimed, err
}

// withinGrace reports whether an object is too fresh to reclaim: newer than the cutoff, or
// with an UNKNOWN modified time (zero). Treating unknown as in-grace makes the guard fail
// CLOSED — a listing that omits the timestamp never triggers a delete, rather than reading
// as infinitely old and being reclaimed on sight.
func withinGrace(lastModified, cutoff time.Time) bool {
	return lastModified.IsZero() || lastModified.After(cutoff)
}

// referencedKeys is the UNION of the object keys every authoritative class in the bucket still
// points at: a live artifacts row (ReferencedArtifactObjectKeys) OR a live checkpoints row
// (ReferencedCheckpointObjectKeys, E10 T1 — checkpoint bytes share this bucket under
// checkpoints/<id>). A key referenced by EITHER is never an orphan. A tombstoned artifacts row
// (retention scrubbed object_key to ”) is intentionally excluded, so its once-referenced object
// joins the orphan set exactly like a write-side orphan. Each scan is bucket-wide across every
// tenant — the reference set must be COMPLETE, or GC could delete a live foreign object.
//
// HAZARD when T6 lands: workspace_snapshots (E10 T6) write to this SAME bucket and carry the same
// data-loss risk — their object keys MUST join this union (or T6 must use a separate bucket/prefix)
// or GC will reclaim live snapshot bytes, mirroring the note at store.go List().
// ponytail: the referenced set is held in memory; fine for the local/single-bucket scale, a
// streaming anti-join is the upgrade path if the index ever outgrows one map.
func (c *Collector) referencedKeys(ctx context.Context) (map[string]struct{}, error) {
	keys := map[string]struct{}{}
	for _, query := range []string{"ReferencedArtifactObjectKeys", "ReferencedCheckpointObjectKeys"} {
		if err := c.addReferencedKeys(ctx, keys, query); err != nil {
			return nil, err
		}
	}
	return keys, nil
}

// addReferencedKeys runs one reference query and folds its object keys into the shared set.
func (c *Collector) addReferencedKeys(ctx context.Context, keys map[string]struct{}, query string) error {
	rows, err := c.pool.Query(ctx, storage.Query(query))
	if err != nil {
		return fmt.Errorf("orphan-gc: query referenced keys: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var key string
		if err := rows.Scan(&key); err != nil {
			return fmt.Errorf("orphan-gc: scan referenced key: %w", err)
		}
		keys[key] = struct{}{}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("orphan-gc: iterate referenced keys: %w", err)
	}
	return nil
}

// Rounds is the number of reconcile passes Run has completed — the counter that makes a
// stalled or crashed loop visible (the supervisor restarts it; this proves it is ticking).
func (c *Collector) Rounds() int64 { return c.rounds.Load() }

// Run reconciles every interval until ctx is cancelled, mirroring the retention Reaper's
// supervised loop. A pass error is logged and non-fatal — the next tick retries — and every
// completed pass advances Rounds() so the loop cannot die silently.
func (c *Collector) Run(ctx context.Context, interval time.Duration) error {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			reclaimed, err := c.Collect(ctx)
			c.rounds.Add(1)
			if err != nil {
				log.Printf("artifact orphan-gc pass failed: %v", err)
			} else if reclaimed > 0 {
				log.Printf("artifact orphan-gc reclaimed %d orphan object(s)", reclaimed)
			}
		}
	}
}
