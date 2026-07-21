//go:build component

package postgres

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/palgroup/palai/packages/coordinator"
	"github.com/palgroup/palai/packages/coordinator/recovery"
)

// seedAttempt inserts an attempt (fence 1) for a run and returns its id — a checkpoint FKs the
// fenced attempt that authored it.
func seedAttempt(t *testing.T, pool *pgxpool.Pool, tenant coordinator.Tenant, runID string) string {
	t.Helper()
	id := newID("att")
	exec(t, pool, `INSERT INTO attempts (id, organization_id, project_id, run_id, fence, state) VALUES ($1,$2,$3,$4,$5,'assigned')`,
		id, tenant.Organization, tenant.Project, runID, 1)
	return id
}

// baseCheckpointInput is a valid checkpoint persist with fresh ids, ready for a test to mutate one field.
func baseCheckpointInput(tenant coordinator.Tenant, runID, attemptID string) recovery.PersistInput {
	return recovery.PersistInput{
		CheckpointID: newID("chk"), BoundaryID: newID("bnd"),
		Organization: tenant.Organization, Project: tenant.Project,
		RunID: runID, AttemptID: attemptID,
		EngineVersion: "0.1.0", ProtocolVersion: "engine.v1",
		Format: "reference-kernel", FormatVersion: 1,
		ConfigSnapshotHash: "sha256:cfg", TranscriptSequence: 7,
		ContentChecksum: "sha256:deadbeef", ObjectKey: "org/prj/run/chk", SizeBytes: 128,
	}
}

// TestPersistCheckpointWritesImmutableBoundaryAndRow proves a checkpoint persists as two linked rows
// and is immutable: a second write to the same id is rejected, never an overwrite (spec §26.1).
func TestPersistCheckpointWritesImmutableBoundaryAndRow(t *testing.T) {
	cs := openHarness(t)
	ctx := context.Background()
	pool := cs.Pool()
	tenant, _, runID := seedRun(t, pool)
	attemptID := seedAttempt(t, pool, tenant, runID)
	obj := recovery.New(pool)
	in := baseCheckpointInput(tenant, runID, attemptID)

	if err := obj.Persist(ctx, in); err != nil {
		t.Fatalf("Persist() error = %v", err)
	}

	var boundaryOnCheckpoint string
	if err := pool.QueryRow(ctx, `SELECT boundary_id FROM checkpoints WHERE id=$1`, in.CheckpointID).Scan(&boundaryOnCheckpoint); err != nil {
		t.Fatalf("read checkpoint.boundary_id: %v", err)
	}
	if boundaryOnCheckpoint != in.BoundaryID {
		t.Fatalf("checkpoint.boundary_id = %q, want the shared boundary %q", boundaryOnCheckpoint, in.BoundaryID)
	}
	var boundaryCount int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM transcript_boundaries WHERE id=$1`, in.BoundaryID).Scan(&boundaryCount); err != nil {
		t.Fatalf("count boundary: %v", err)
	}
	if boundaryCount != 1 {
		t.Fatalf("transcript_boundaries rows = %d, want 1", boundaryCount)
	}

	// Immutable: the same id cannot be written twice.
	if err := obj.Persist(ctx, in); err != recovery.ErrCheckpointExists {
		t.Fatalf("second Persist() = %v, want ErrCheckpointExists", err)
	}
}

// TestRecoveryObjectsAppendOnlyToApplicationRole proves the withheld UPDATE grant is real, not just
// asserted: as palai_app, UPDATE on checkpoints and transcript_boundaries is denied (42501), so an
// immutable checkpoint / boundary can never be silently rewritten (spec §26.1). DELETE stays granted
// for retention/GC, so only UPDATE is checked.
func TestRecoveryObjectsAppendOnlyToApplicationRole(t *testing.T) {
	cs := openHarness(t)
	ctx := context.Background()
	pool := cs.Pool()
	tenant, _, runID := seedRun(t, pool)
	attemptID := seedAttempt(t, pool, tenant, runID)
	if err := recovery.New(pool).Persist(ctx, baseCheckpointInput(tenant, runID, attemptID)); err != nil {
		t.Fatalf("Persist() error = %v", err)
	}

	conn, err := pool.Acquire(ctx)
	if err != nil {
		t.Fatalf("Acquire() error = %v", err)
	}
	defer conn.Release()
	if _, err := conn.Exec(ctx, `SET ROLE palai_app`); err != nil {
		t.Fatalf("SET ROLE palai_app error = %v", err)
	}
	defer func() { _, _ = conn.Exec(ctx, `RESET ROLE`) }()

	if got := pgCode(mustFail(conn.Exec(ctx, `UPDATE checkpoints SET content_checksum = 'tampered'`))); got != "42501" {
		t.Fatalf("checkpoints UPDATE code = %q, want 42501 (immutable, UPDATE withheld)", got)
	}
	if got := pgCode(mustFail(conn.Exec(ctx, `UPDATE transcript_boundaries SET transcript_sequence = 999`))); got != "42501" {
		t.Fatalf("transcript_boundaries UPDATE code = %q, want 42501 (immutable, UPDATE withheld)", got)
	}
}

// TestLatestRunCheckpointReadsNewestWithBoundary proves the recovery ladder's read (spec §26.3-26.4,
// E10 Task 4): LatestRunCheckpoint returns a run's newest checkpoint with the §26.4 compatibility
// inputs (format/version, config hash, protocol, transcript boundary), and reports found=false for a
// run that has no checkpoint at all (so the ladder falls to reconstruction, never a phantom restore).
func TestLatestRunCheckpointReadsNewestWithBoundary(t *testing.T) {
	cs := openHarness(t)
	ctx := context.Background()
	pool := cs.Pool()
	tenant, _, runID := seedRun(t, pool)
	attemptID := seedAttempt(t, pool, tenant, runID)
	obj := recovery.New(pool)

	// An older checkpoint, then a newer one. Force created_at so "newest" is unambiguous regardless
	// of clock resolution (the index is checkpoints_by_run (run_id, created_at DESC)).
	older := baseCheckpointInput(tenant, runID, attemptID)
	older.TranscriptSequence = 7
	older.ContentChecksum = "sha256:older"
	if err := obj.Persist(ctx, older); err != nil {
		t.Fatalf("Persist(older) error = %v", err)
	}
	exec(t, pool, `UPDATE checkpoints SET created_at = clock_timestamp() - interval '1 hour' WHERE id=$1`, older.CheckpointID)

	newer := baseCheckpointInput(tenant, runID, attemptID)
	newer.TranscriptSequence = 11
	newer.ContentChecksum = "sha256:newer"
	newer.ConfigSnapshotHash = "sha256:cfg-new"
	if err := obj.Persist(ctx, newer); err != nil {
		t.Fatalf("Persist(newer) error = %v", err)
	}

	got, found, err := cs.LatestRunCheckpoint(ctx, tenant, runID)
	if err != nil {
		t.Fatalf("LatestRunCheckpoint() error = %v", err)
	}
	if !found {
		t.Fatal("LatestRunCheckpoint found=false, want the newest checkpoint")
	}
	if got.CheckpointID != newer.CheckpointID {
		t.Fatalf("CheckpointID = %q, want the newest %q", got.CheckpointID, newer.CheckpointID)
	}
	if got.BoundaryID != newer.BoundaryID {
		t.Fatalf("BoundaryID = %q, want %q", got.BoundaryID, newer.BoundaryID)
	}
	if got.TranscriptSequence != 11 || got.ContentChecksum != "sha256:newer" || got.ConfigSnapshotHash != "sha256:cfg-new" {
		t.Fatalf("newest row §26.4 fields wrong: seq=%d checksum=%q config=%q", got.TranscriptSequence, got.ContentChecksum, got.ConfigSnapshotHash)
	}
	if got.Format != "reference-kernel" || got.FormatVersion != 1 || got.ProtocolVersion != "engine.v1" {
		t.Fatalf("newest row format fields wrong: %q/%d proto=%q", got.Format, got.FormatVersion, got.ProtocolVersion)
	}
	if got.WorkspaceSnapshotID != "" {
		t.Fatalf("checkpoint with no snapshot must read empty workspace id, got %q", got.WorkspaceSnapshotID)
	}
	if got.ObjectKey == "" {
		t.Fatal("newest row must carry its object key for the bytes fetch")
	}

	// A run with no checkpoint reads found=false — the ladder falls to reconstruction.
	_, _, freshRun := seedRun(t, pool)
	if _, found, err := cs.LatestRunCheckpoint(ctx, tenant, freshRun); err != nil || found {
		t.Fatalf("LatestRunCheckpoint(no checkpoints) = found %v err %v, want found=false", found, err)
	}
}

// TestCheckpointSizeBoundRejected proves an oversized checkpoint is refused BEFORE any row is written
// (spec §26.2 size-bound) — so an oversize offer leaves no orphan boundary for GC to chase.
func TestCheckpointSizeBoundRejected(t *testing.T) {
	cs := openHarness(t)
	ctx := context.Background()
	pool := cs.Pool()
	tenant, _, runID := seedRun(t, pool)
	attemptID := seedAttempt(t, pool, tenant, runID)
	obj := recovery.New(pool)
	in := baseCheckpointInput(tenant, runID, attemptID)
	in.SizeBytes = recovery.MaxCheckpointBytes + 1

	if err := obj.Persist(ctx, in); err != recovery.ErrCheckpointTooLarge {
		t.Fatalf("Persist(oversize) = %v, want ErrCheckpointTooLarge", err)
	}
	var boundaries int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM transcript_boundaries WHERE id=$1`, in.BoundaryID).Scan(&boundaries); err != nil {
		t.Fatalf("count boundary: %v", err)
	}
	if boundaries != 0 {
		t.Fatal("an oversized checkpoint must leave NO boundary row (rejected before any write)")
	}
}

// TestCheckpointRequiresChecksum proves a checkpoint with no integrity coverage is refused rather
// than stored unverifiable (spec §26.2).
func TestCheckpointRequiresChecksum(t *testing.T) {
	cs := openHarness(t)
	ctx := context.Background()
	pool := cs.Pool()
	tenant, _, runID := seedRun(t, pool)
	attemptID := seedAttempt(t, pool, tenant, runID)
	obj := recovery.New(pool)
	in := baseCheckpointInput(tenant, runID, attemptID)
	in.ContentChecksum = ""

	if err := obj.Persist(ctx, in); err != recovery.ErrChecksumRequired {
		t.Fatalf("Persist(no checksum) = %v, want ErrChecksumRequired", err)
	}
}

// TestBoundaryLinksThreeObjectsIndependently proves the shared boundary links the checkpoint and the
// workspace snapshot WITHOUT one implying the other (spec §26.1): a checkpoint with no snapshot
// declares no workspace dependency (workspace_snapshot_id IS NULL, §26.4), and a workspace snapshot
// can independently reference the SAME boundary.
func TestBoundaryLinksThreeObjectsIndependently(t *testing.T) {
	cs := openHarness(t)
	ctx := context.Background()
	pool := cs.Pool()
	tenant, sessionID, runID := seedRun(t, pool)
	attemptID := seedAttempt(t, pool, tenant, runID)
	obj := recovery.New(pool)

	// A checkpoint with no workspace snapshot: it must declare no workspace dependency (NULL), never
	// a dangling reference.
	in := baseCheckpointInput(tenant, runID, attemptID)
	if err := obj.Persist(ctx, in); err != nil {
		t.Fatalf("Persist() error = %v", err)
	}
	var wsnap *string
	if err := pool.QueryRow(ctx, `SELECT workspace_snapshot_id FROM checkpoints WHERE id=$1`, in.CheckpointID).Scan(&wsnap); err != nil {
		t.Fatalf("read checkpoint.workspace_snapshot_id: %v", err)
	}
	if wsnap != nil {
		t.Fatalf("checkpoint with no snapshot must declare no workspace dependency (NULL), got %q", *wsnap)
	}

	// A workspace snapshot can independently anchor to the SAME boundary — three separate objects,
	// one shared id, no implied restore of the others.
	wsID, alloc := seedWorkspace(t, cs, tenant, sessionID, runID)
	_ = wsID
	snapID := newID("wsnap")
	if err := cs.CreateWorkspaceSnapshot(ctx, coordinator.SnapshotInput{
		SnapshotID: snapID, AllocationID: alloc.ID, TreeChecksum: "sha256:tree", Reason: "boundary",
	}); err != nil {
		t.Fatalf("CreateWorkspaceSnapshot() error = %v", err)
	}
	exec(t, pool, `UPDATE workspace_snapshots SET boundary_id=$1 WHERE id=$2`, in.BoundaryID, snapID)

	var snapBoundary string
	if err := pool.QueryRow(ctx, `SELECT boundary_id FROM workspace_snapshots WHERE id=$1`, snapID).Scan(&snapBoundary); err != nil {
		t.Fatalf("read snapshot.boundary_id: %v", err)
	}
	if snapBoundary != in.BoundaryID {
		t.Fatalf("snapshot.boundary_id = %q, want the shared boundary %q", snapBoundary, in.BoundaryID)
	}
}
