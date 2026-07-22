//go:build component

package postgres

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/palgroup/palai/packages/coordinator"
	"github.com/palgroup/palai/packages/coordinator/recovery"

	"github.com/palgroup/palai/storage"
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
	if err := pool.QueryRow(storage.WithSystemScope(ctx), `SELECT boundary_id FROM checkpoints WHERE id=$1`, in.CheckpointID).Scan(&boundaryOnCheckpoint); err != nil {
		t.Fatalf("read checkpoint.boundary_id: %v", err)
	}
	if boundaryOnCheckpoint != in.BoundaryID {
		t.Fatalf("checkpoint.boundary_id = %q, want the shared boundary %q", boundaryOnCheckpoint, in.BoundaryID)
	}
	var boundaryCount int
	if err := pool.QueryRow(storage.WithSystemScope(ctx), `SELECT count(*) FROM transcript_boundaries WHERE id=$1`, in.BoundaryID).Scan(&boundaryCount); err != nil {
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

// TestCheckpointCarriesPendingOperations proves the E10 T7 pending_operations fill (spec §26.2, §26.4):
// a checkpoint records the run's unresolved tool operations at the boundary, and LatestRunCheckpoint
// reads them back — so a RESTORE cannot silently hide an in-flight external effect. PendingToolOperations
// (the CP-side resolver) collects only the uncertain/manual_resolution rows, class-labelled; a checkpoint
// with none records '[]' (never null).
func TestCheckpointCarriesPendingOperations(t *testing.T) {
	cs := openHarness(t)
	ctx := context.Background()
	pool := cs.Pool()
	tenant, _, runID := seedRun(t, pool)
	attemptID := seedAttempt(t, pool, tenant, runID)

	// A completed op (resolved — must NOT appear), an uncertain op, and an escalated one (both unresolved).
	exec(t, pool, `INSERT INTO tool_calls (id, organization_id, project_id, run_id, fence, state, name, arguments, replay_class)
		VALUES ($1,$2,$3,$4,1,'completed','pure_add','{}','pure')`, newID("tc"), tenant.Organization, tenant.Project, runID)
	uncertainID := newID("tc")
	exec(t, pool, `INSERT INTO tool_calls (id, organization_id, project_id, run_id, fence, state, name, arguments, replay_class, reconciliation_state)
		VALUES ($1,$2,$3,$4,2,'uncertain','http_post','{}','irreversible','reconciling')`, uncertainID, tenant.Organization, tenant.Project, runID)
	escalatedID := newID("tc")
	exec(t, pool, `INSERT INTO tool_calls (id, organization_id, project_id, run_id, fence, state, name, arguments, replay_class, reconciliation_state)
		VALUES ($1,$2,$3,$4,3,'manual_resolution','charge','{}','irreversible','manual_resolution')`, escalatedID, tenant.Organization, tenant.Project, runID)

	// The CP-side resolver collects only the two unresolved ops, class-labelled.
	pendingJSON, err := cs.PendingToolOperations(ctx, tenant, runID)
	if err != nil {
		t.Fatalf("PendingToolOperations() error = %v", err)
	}
	var ops []map[string]any
	if err := json.Unmarshal(pendingJSON, &ops); err != nil {
		t.Fatalf("decode pending ops %s error = %v", pendingJSON, err)
	}
	if len(ops) != 2 {
		t.Fatalf("pending tool operations = %d (%s), want 2 (uncertain + manual_resolution, not the completed one)", len(ops), pendingJSON)
	}
	if ops[0]["tool_call_id"] != uncertainID || ops[0]["replay_class"] != "irreversible" {
		t.Fatalf("first pending op = %v, want the uncertain irreversible op %s", ops[0], uncertainID)
	}

	// Persist a checkpoint carrying them; LatestRunCheckpoint reads them back (restore doesn't hide them).
	in := baseCheckpointInput(tenant, runID, attemptID)
	in.PendingOperations = pendingJSON
	if err := recovery.New(pool).Persist(ctx, in); err != nil {
		t.Fatalf("Persist(with pending ops) error = %v", err)
	}
	cp, found, err := cs.LatestRunCheckpoint(ctx, tenant, runID)
	if err != nil || !found {
		t.Fatalf("LatestRunCheckpoint() = (found:%v, %v)", found, err)
	}
	var readBack []map[string]any
	if err := json.Unmarshal(cp.PendingOperations, &readBack); err != nil {
		t.Fatalf("decode read-back pending ops %s error = %v", cp.PendingOperations, err)
	}
	if len(readBack) != 2 {
		t.Fatalf("checkpoint read-back pending operations = %d (%s), want 2", len(readBack), cp.PendingOperations)
	}

	// A run with no unresolved ops records '[]', never null.
	tenant2, _, runID2 := seedRun(t, pool)
	attempt2 := seedAttempt(t, pool, tenant2, runID2)
	empty, err := cs.PendingToolOperations(ctx, tenant2, runID2)
	if err != nil {
		t.Fatalf("PendingToolOperations(empty) error = %v", err)
	}
	if string(empty) != "[]" {
		t.Fatalf("empty pending ops = %q, want []", empty)
	}
	in2 := baseCheckpointInput(tenant2, runID2, attempt2)
	if err := recovery.New(pool).Persist(ctx, in2); err != nil { // PendingOperations left nil -> normalised to '[]'
		t.Fatalf("Persist(no pending ops) error = %v", err)
	}
	cp2, _, _ := cs.LatestRunCheckpoint(ctx, tenant2, runID2)
	if string(cp2.PendingOperations) != "[]" {
		t.Fatalf("checkpoint with no pending ops = %q, want [] (never null)", cp2.PendingOperations)
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
	if _, err := conn.Exec(storage.WithSystemScope(ctx), `SET ROLE palai_app`); err != nil {
		t.Fatalf("SET ROLE palai_app error = %v", err)
	}
	defer func() { _, _ = conn.Exec(storage.WithSystemScope(ctx), `RESET ROLE`) }()

	if got := pgCode(mustFail(conn.Exec(storage.WithSystemScope(ctx), `UPDATE checkpoints SET content_checksum = 'tampered'`))); got != "42501" {
		t.Fatalf("checkpoints UPDATE code = %q, want 42501 (immutable, UPDATE withheld)", got)
	}
	if got := pgCode(mustFail(conn.Exec(storage.WithSystemScope(ctx), `UPDATE transcript_boundaries SET transcript_sequence = 999`))); got != "42501" {
		t.Fatalf("transcript_boundaries UPDATE code = %q, want 42501 (immutable, UPDATE withheld)", got)
	}
}

// TestSoftRequeueNeverDeadLettersAcrossManyStandDowns proves MUST-FIX #2 FIX B: an exact stand-down
// soft-requeues WITHOUT consuming the attempt budget, so a standby facing a long-lived live sibling
// requeues past MaxAttempts rounds and NEVER dead-letters the run. RequeueSoft undoes the claim's
// attempt increment, so the count stays flat and the job stays queued.
func TestSoftRequeueNeverDeadLettersAcrossManyStandDowns(t *testing.T) {
	cs := openHarness(t)
	ctx := context.Background()
	tenant, _, _ := seedRun(t, cs.Pool())
	jobID := newID("job")
	if err := cs.Enqueue(ctx, tenant, jobID, "response.run"); err != nil {
		t.Fatalf("Enqueue() error = %v", err)
	}

	for round := 0; round < 6; round++ { // well past a MaxAttempts of 3-5
		claim, err := cs.Claim(ctx, tenant, jobID, newID("owner"), 2*time.Second)
		if err != nil {
			t.Fatalf("claim round %d: %v", round, err)
		}
		if err := cs.RequeueSoft(ctx, claim); err != nil {
			t.Fatalf("soft requeue round %d: %v", round, err)
		}
		snap, err := cs.Snapshot(ctx, tenant, jobID)
		if err != nil {
			t.Fatalf("snapshot round %d: %v", round, err)
		}
		if snap.Status == "dead" {
			t.Fatalf("job dead-lettered at round %d — a soft requeue must not consume the attempt budget", round)
		}
		if snap.AttemptCount > 1 {
			t.Fatalf("attempt_count = %d at round %d — a soft requeue must undo the claim's increment", snap.AttemptCount, round)
		}
		time.Sleep(150 * time.Millisecond) // past the soft-requeue's small ready_at delay
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
	execAsOwner(t, pool, `UPDATE checkpoints SET created_at = clock_timestamp() - interval '1 hour' WHERE id=$1`, older.CheckpointID)

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
	if err := pool.QueryRow(storage.WithSystemScope(ctx), `SELECT count(*) FROM transcript_boundaries WHERE id=$1`, in.BoundaryID).Scan(&boundaries); err != nil {
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
	// AllocateWorkspace / GetPreparationReceipt are keyed by an opaque id, not by a tenant, so under
	// migration 000029 the CONTEXT is what scopes them — the same way the run worker scopes a claimed
	// job. Declaring it here is what a production caller already does.
	ctx = storage.WithTenant(ctx, tenant.Organization, tenant.Project)
	attemptID := seedAttempt(t, pool, tenant, runID)
	obj := recovery.New(pool)

	// A checkpoint with no workspace snapshot: it must declare no workspace dependency (NULL), never
	// a dangling reference.
	in := baseCheckpointInput(tenant, runID, attemptID)
	if err := obj.Persist(ctx, in); err != nil {
		t.Fatalf("Persist() error = %v", err)
	}
	var wsnap *string
	if err := pool.QueryRow(storage.WithSystemScope(ctx), `SELECT workspace_snapshot_id FROM checkpoints WHERE id=$1`, in.CheckpointID).Scan(&wsnap); err != nil {
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
	if err := pool.QueryRow(storage.WithSystemScope(ctx), `SELECT boundary_id FROM workspace_snapshots WHERE id=$1`, snapID).Scan(&snapBoundary); err != nil {
		t.Fatalf("read snapshot.boundary_id: %v", err)
	}
	if snapBoundary != in.BoundaryID {
		t.Fatalf("snapshot.boundary_id = %q, want the shared boundary %q", snapBoundary, in.BoundaryID)
	}
}
