//go:build component

package artifacts

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/palgroup/palai/apps/control-plane/internal/execution"
)

// graceElapsed is a negative grace window: the cutoff moves into the future, so every
// listed object is older than it and eligible for reclaim. It proves the delete path
// independent of any host↔container clock skew (a positive-zero grace could see a fresh
// object's server timestamp a few hundred ms in the future and wrongly spare it).
const graceElapsed = -30 * time.Second

// objectPresent reports whether the object at key still holds bytes in the store.
func (h *artifactsHarness) objectPresent(t *testing.T, key string) bool {
	t.Helper()
	_, found, err := h.s3.Get(context.Background(), key)
	if err != nil {
		t.Fatalf("Get(%q) error = %v", key, err)
	}
	return found
}

// failingDeleter is an ArtifactDeleter whose Delete always fails — it injects the
// retention delete-error the GC must later sweep, without touching the real object store.
type failingDeleter struct{}

func (failingDeleter) Delete(context.Context, string) error {
	return errors.New("injected object-store delete failure")
}

// TestGCDeletesUnreferencedObjectAfterGrace proves the purge-crash orphan is reclaimed: a
// row is tombstoned (retention scrubbed object_key to ”) but a crash left its object in the
// store. No live row references the object, and it is past the grace window, so one GC pass
// deletes the bytes (spec §22.6 retention closure; REC-004 write-side/delete-error reconcile).
func TestGCDeletesUnreferencedObjectAfterGrace(t *testing.T) {
	h := openArtifactsHarness(t)
	ctx := context.Background()
	org, project, runID := h.seedRun(t)

	art, err := h.writer.Write(ctx, WriteRequest{Organization: org, Project: project, RunID: runID,
		Content: []byte("orphaned by a purge-crash between the DB scrub and the byte-delete")})
	if err != nil {
		t.Fatalf("Write() error = %v", err)
	}
	// Simulate the crash: the retention scrub committed (object_key cleared, size zeroed) but
	// the object-store delete never ran, so the bytes linger unreferenced.
	h.exec(t, `UPDATE artifacts SET object_key = '', size_bytes = 0 WHERE id = $1`, art.ID)
	if !h.objectPresent(t, art.ObjectKey) {
		t.Fatalf("precondition: orphan object %q absent before GC", art.ObjectKey)
	}

	reclaimed, err := NewCollector(h.s3, h.pool, graceElapsed).Collect(ctx)
	if err != nil {
		t.Fatalf("Collect() error = %v", err)
	}
	if reclaimed < 1 {
		t.Fatalf("Collect() reclaimed = %d, want >=1 (the purge-crash orphan)", reclaimed)
	}
	if h.objectPresent(t, art.ObjectKey) {
		t.Fatalf("orphan object %q survived the GC pass", art.ObjectKey)
	}
}

// TestGCDeletesWriteSideOrphan proves the write-side orphan is reclaimed: the write-path's
// object PUT succeeded but the row insert never committed (writer.go's documented gap), so an
// object exists that no artifacts row ever referenced. Past the grace window, GC deletes it.
func TestGCDeletesWriteSideOrphan(t *testing.T) {
	h := openArtifactsHarness(t)
	ctx := context.Background()
	org, project, runID := h.seedRun(t)

	// The object the write-path would have PUT, with no row insert following it.
	key := objectKey(org, project, runID, newArtifactID())
	if _, _, err := h.s3.Put(ctx, key, []byte("bytes PUT before a row insert that failed")); err != nil {
		t.Fatalf("Put() error = %v", err)
	}
	if !h.objectPresent(t, key) {
		t.Fatalf("precondition: write-side orphan %q absent before GC", key)
	}

	reclaimed, err := NewCollector(h.s3, h.pool, graceElapsed).Collect(ctx)
	if err != nil {
		t.Fatalf("Collect() error = %v", err)
	}
	if reclaimed < 1 {
		t.Fatalf("Collect() reclaimed = %d, want >=1 (the write-side orphan)", reclaimed)
	}
	if h.objectPresent(t, key) {
		t.Fatalf("write-side orphan %q survived the GC pass", key)
	}
}

// TestGCSweepsFailedRetentionDelete proves the reconcile closes the retention delete-error
// path end to end: a real retention Sweep tombstones the row but its object-store delete
// fails (injected), leaving the byte lost — no later retention tick can re-reach it. The next
// GC pass catches that same object as an orphan and reclaims it (retention.go:73 closure).
func TestGCSweepsFailedRetentionDelete(t *testing.T) {
	h := openArtifactsHarness(t)
	ctx := context.Background()
	org, project, runID := h.seedExpiredStoreFalseRun(t)

	art, err := h.writer.Write(ctx, WriteRequest{Organization: org, Project: project, RunID: runID,
		Content: []byte("store:false bytes whose retention delete fails")})
	if err != nil {
		t.Fatalf("Write() error = %v", err)
	}

	// Retention reaps the expired response and scrubs the row, but the byte-delete fails —
	// exactly the delete-error the GC must sweep. The Sweep surfaces the delete error.
	reaper := execution.NewReaper(h.repo, time.Minute).WithArtifactStore(failingDeleter{})
	if _, err := reaper.Sweep(ctx); err == nil {
		t.Fatalf("Sweep() error = nil, want the injected delete failure surfaced")
	}
	// The row is scrubbed (object_key cleared) yet the byte survives — a lost orphan.
	var objectKey string
	if err := h.pool.QueryRow(ctx, `SELECT object_key FROM artifacts WHERE id = $1`, art.ID).Scan(&objectKey); err != nil {
		t.Fatalf("read artifact row error = %v", err)
	}
	if objectKey != "" {
		t.Fatalf("row not tombstoned after retention sweep: object_key = %q", objectKey)
	}
	if !h.objectPresent(t, art.ObjectKey) {
		t.Fatalf("precondition: delete-error orphan %q absent before GC", art.ObjectKey)
	}

	if _, err := NewCollector(h.s3, h.pool, graceElapsed).Collect(ctx); err != nil {
		t.Fatalf("Collect() error = %v", err)
	}
	if h.objectPresent(t, art.ObjectKey) {
		t.Fatalf("delete-error orphan %q survived the GC pass", art.ObjectKey)
	}
}

// TestGCNeverDeletesReferencedOrInGraceObject is the safety invariant, proven from both
// sides. A referenced object (live artifacts row) and a fresh unreferenced object must BOTH
// survive a wide grace window; then, with the grace elapsed, the referenced object STILL
// survives (reference beats grace) while the unreferenced one is finally reclaimed. GC never
// deletes live data, and never on anything but the pure absence of a referencing row.
func TestGCNeverDeletesReferencedOrInGraceObject(t *testing.T) {
	h := openArtifactsHarness(t)
	ctx := context.Background()
	org, project, runID := h.seedRun(t)

	referenced, err := h.writer.Write(ctx, WriteRequest{Organization: org, Project: project, RunID: runID,
		Content: []byte("a live, referenced artifact — must never be reclaimed")})
	if err != nil {
		t.Fatalf("Write() error = %v", err)
	}
	freshOrphanKey := objectKey(org, project, runID, newArtifactID())
	if _, _, err := h.s3.Put(ctx, freshOrphanKey, []byte("an orphan still inside the grace window")); err != nil {
		t.Fatalf("Put() error = %v", err)
	}

	// Wide grace: every object in the bucket is younger than it, so nothing is reclaimed —
	// the referenced object is spared by its row, the fresh orphan by the grace window.
	reclaimed, err := NewCollector(h.s3, h.pool, time.Hour).Collect(ctx)
	if err != nil {
		t.Fatalf("Collect(grace=1h) error = %v", err)
	}
	if reclaimed != 0 {
		t.Fatalf("Collect(grace=1h) reclaimed = %d, want 0 (everything is either referenced or in-grace)", reclaimed)
	}
	if !h.objectPresent(t, referenced.ObjectKey) {
		t.Fatalf("referenced object %q was reclaimed under a wide grace window", referenced.ObjectKey)
	}
	if !h.objectPresent(t, freshOrphanKey) {
		t.Fatalf("in-grace orphan %q was reclaimed inside its grace window", freshOrphanKey)
	}

	// Grace elapsed: the orphan is now eligible and reclaimed, but the referenced object —
	// also past grace — SURVIVES, because the delete decision is reference-absence, not age.
	if _, err := NewCollector(h.s3, h.pool, graceElapsed).Collect(ctx); err != nil {
		t.Fatalf("Collect(grace elapsed) error = %v", err)
	}
	if !h.objectPresent(t, referenced.ObjectKey) {
		t.Fatalf("referenced object %q was reclaimed once its grace elapsed — reference must beat grace", referenced.ObjectKey)
	}
	if h.objectPresent(t, freshOrphanKey) {
		t.Fatalf("orphan %q survived reclaim after its grace elapsed", freshOrphanKey)
	}
}

// TestGCRunsSupervisedAndRecorded proves the reconcile loop is observable, so a stalled or
// crashed GC cannot die silently: Run advances a round counter every pass. A wide grace keeps
// the loop from deleting anything while the test only checks that it is ticking.
func TestGCRunsSupervisedAndRecorded(t *testing.T) {
	h := openArtifactsHarness(t)
	c := NewCollector(h.s3, h.pool, time.Hour)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- c.Run(ctx, 10*time.Millisecond) }()

	deadline := time.Now().Add(5 * time.Second)
	for c.Rounds() < 2 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	cancel()
	<-done

	if c.Rounds() < 2 {
		t.Fatalf("GC loop recorded %d rounds, want >=2 (a supervised loop must not die silently)", c.Rounds())
	}
}
