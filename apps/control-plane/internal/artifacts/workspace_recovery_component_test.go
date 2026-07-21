//go:build component

// Workspace host-loss + recovery component tests (spec §29.7-29.8, REC-005/ENG-006/SAN-006, E10 Task 6).
// They run in the artifacts package because a recovery RESTORES a real snapshot archive from the object
// store — so the real SeaweedFS + Postgres the artifacts suite stands up are what the driver needs.

package artifacts

import (
	"context"
	"testing"

	"github.com/palgroup/palai/apps/control-plane/internal/execution"
	"github.com/palgroup/palai/packages/coordinator"
)

// TestHostMoveKeepsLogicalIdNewFencedAllocation (REC-005/ENG-006): a leased workspace whose host is
// lost recovers onto a NEW allocation — logical id STABLE, fence STRICTLY higher, restored tree
// checksum-EQUAL (SAN-005) — driven through the real leased→host_lost→recovering→ready SM. The old
// allocation is now a lower fence, so its writer-lease + snapshot attempts are rejected at the DB
// (ErrStaleAllocation, SAN-006). A workspace.restored.v1 event records the move.
func TestHostMoveKeepsLogicalIdNewFencedAllocation(t *testing.T) {
	h := openArtifactsHarness(t)
	ctx := context.Background()
	org, project, workspaceID, oldAllocID, hostPath := h.seedAllocationOnDisk(t)
	tenant := coordinator.Tenant{Organization: org, Project: project}
	sink := execution.NewSnapshotSink(h.s3, h.repo.Spine())

	// Capture the boundary snapshot on the current (old) allocation, then simulate the host loss.
	snapID := newID("snap")
	if _, err := sink.Capture(ctx, execution.SnapshotCaptureInput{
		SnapshotID: snapID, Organization: org, Project: project,
		WorkspaceID: workspaceID, AllocationID: oldAllocID, HostPath: hostPath, Reason: "boundary",
	}); err != nil {
		t.Fatalf("Capture() error = %v", err)
	}
	var oldFence int64
	if err := h.pool.QueryRow(ctx, `SELECT fence FROM workspace_allocations WHERE id=$1`, oldAllocID).Scan(&oldFence); err != nil {
		t.Fatalf("read old fence: %v", err)
	}

	recovery := execution.NewWorkspaceRecovery(h.repo.Spine(), sink, t.TempDir())
	res, err := recovery.RecoverWorkspace(ctx, tenant, execution.RecoverInput{
		WorkspaceID: workspaceID, RunID: newID("run"), SessionID: sessionOf(t, h, workspaceID), SnapshotID: snapID,
	})
	if err != nil {
		t.Fatalf("RecoverWorkspace() error = %v", err)
	}

	// New allocation: SAME logical workspace, STRICTLY higher fence.
	if res.Allocation.ID == oldAllocID {
		t.Fatal("recovery reused the old allocation id — a host move must mint a new one")
	}
	if res.Allocation.Fence <= oldFence {
		t.Fatalf("new fence %d is not > old fence %d", res.Allocation.Fence, oldFence)
	}
	var newWorkspaceID string
	if err := h.pool.QueryRow(ctx, `SELECT workspace_id FROM workspace_allocations WHERE id=$1`, res.Allocation.ID).Scan(&newWorkspaceID); err != nil {
		t.Fatalf("read new allocation workspace: %v", err)
	}
	if newWorkspaceID != workspaceID {
		t.Fatalf("logical workspace id changed on host move: %q != %q", newWorkspaceID, workspaceID)
	}

	// The workspace is back to ready, and the restore is checksum-equal to the create-side snapshot.
	var state, wantTree string
	if err := h.pool.QueryRow(ctx, `SELECT state FROM workspaces WHERE id=$1`, workspaceID).Scan(&state); err != nil {
		t.Fatalf("read workspace state: %v", err)
	}
	if state != "ready" {
		t.Fatalf("workspace state = %q, want ready after recovery", state)
	}
	if err := h.pool.QueryRow(ctx, `SELECT tree_checksum FROM workspace_snapshots WHERE id=$1`, snapID).Scan(&wantTree); err != nil {
		t.Fatalf("read create tree: %v", err)
	}
	if res.Manifest.TreeChecksum != wantTree {
		t.Fatalf("restored tree %s != create %s", res.Manifest.TreeChecksum, wantTree)
	}

	// SAN-006: the OLD (now fenced-out) allocation can no longer acquire the writer lease or snapshot.
	if err := h.repo.Spine().AcquireWriterLease(ctx, newID("lease"), oldAllocID, newID("run")); err != coordinator.ErrStaleAllocation {
		t.Fatalf("old-allocation lease = %v, want ErrStaleAllocation (fenced out)", err)
	}
	if err := h.repo.Spine().CreateWorkspaceSnapshot(ctx, coordinator.SnapshotInput{
		SnapshotID: newID("snap"), AllocationID: oldAllocID, TreeChecksum: "sha256:x", ObjectKey: "k",
	}); err != coordinator.ErrStaleAllocation {
		t.Fatalf("old-allocation snapshot = %v, want ErrStaleAllocation (fenced out)", err)
	}

	// The restored move is journaled.
	var events int
	if err := h.pool.QueryRow(ctx, `SELECT count(*) FROM events WHERE type='workspace.restored.v1' AND payload->>'workspace_id'=$1`, workspaceID).Scan(&events); err != nil {
		t.Fatalf("read restored events: %v", err)
	}
	if events != 1 {
		t.Fatalf("workspace.restored.v1 events = %d, want 1", events)
	}
}

// TestOldHostAuthoritativeFramesDeniedDiagnosticsAllowed (ENG-007): after a host move advances the
// fence, the returning old host's AUTHORITATIVE writes (a writer-lease acquire, a snapshot) are DENIED
// at the DB (ErrStaleAllocation), while its NON-authoritative diagnostics path stays OPEN — a plain
// journal append is not fence-gated, so the old host can still report what it saw. Honest ceiling: the
// diagnostics acceptance is at the journal-record level (the brief); there is no separate diagnostics
// pipeline.
func TestOldHostAuthoritativeFramesDeniedDiagnosticsAllowed(t *testing.T) {
	h := openArtifactsHarness(t)
	ctx := context.Background()
	org, project, workspaceID, oldAllocID, hostPath := h.seedAllocationOnDisk(t)
	tenant := coordinator.Tenant{Organization: org, Project: project}
	sink := execution.NewSnapshotSink(h.s3, h.repo.Spine())
	session := sessionOf(t, h, workspaceID)

	snapID := newID("snap")
	if _, err := sink.Capture(ctx, execution.SnapshotCaptureInput{
		SnapshotID: snapID, Organization: org, Project: project,
		WorkspaceID: workspaceID, AllocationID: oldAllocID, HostPath: hostPath, Reason: "boundary",
	}); err != nil {
		t.Fatalf("Capture() error = %v", err)
	}
	recovery := execution.NewWorkspaceRecovery(h.repo.Spine(), sink, t.TempDir())
	if _, err := recovery.RecoverWorkspace(ctx, tenant, execution.RecoverInput{
		WorkspaceID: workspaceID, RunID: newID("run"), SessionID: session, SnapshotID: snapID,
	}); err != nil {
		t.Fatalf("RecoverWorkspace() error = %v", err)
	}

	// Authoritative write from the fenced-out old allocation: DENIED at the DB.
	if err := h.repo.Spine().CreateWorkspaceSnapshot(ctx, coordinator.SnapshotInput{
		SnapshotID: newID("snap"), AllocationID: oldAllocID, TreeChecksum: "sha256:stale", ObjectKey: "k",
	}); err != coordinator.ErrStaleAllocation {
		t.Fatalf("old-host authoritative snapshot = %v, want ErrStaleAllocation (denied)", err)
	}

	// Diagnostics path: a non-authoritative journal record from the returning old host is ALLOWED —
	// RecordRecoveryEvent has no fence/run-active guard, so a stale host can still report.
	before := recoveryEventCount(t, h, session)
	if _, err := h.repo.Spine().RecordRecoveryEvent(ctx, tenant, session, "", "attempt.recovering.v1",
		[]byte(`{"detail":"old host diagnostics: observed stale fence"}`)); err != nil {
		t.Fatalf("old-host diagnostics append = %v, want it accepted (non-authoritative)", err)
	}
	if got := recoveryEventCount(t, h, session); got != before+1 {
		t.Fatalf("diagnostics event count = %d, want %d (the append was rejected)", got, before+1)
	}
}

// recoveryEventCount counts attempt.recovering.v1 diagnostics events in a session's journal.
func recoveryEventCount(t *testing.T, h *artifactsHarness, session string) int {
	t.Helper()
	var n int
	if err := h.pool.QueryRow(context.Background(), `SELECT count(*) FROM events WHERE session_id=$1 AND type='attempt.recovering.v1'`, session).Scan(&n); err != nil {
		t.Fatalf("count diagnostics events: %v", err)
	}
	return n
}

// TestRecoveringFailsExplicitlyWhenRestoreImpossible: a host-lost workspace whose only snapshot is
// manifest-only (no archived bytes) has no boundary to restore — the recovery drives recovering→failed
// with ErrRecoveryImpossible, never a silent empty-tree resume.
func TestRecoveringFailsExplicitlyWhenRestoreImpossible(t *testing.T) {
	h := openArtifactsHarness(t)
	ctx := context.Background()
	org, project, workspaceID, _, _ := h.seedAllocationOnDisk(t)
	tenant := coordinator.Tenant{Organization: org, Project: project}
	sink := execution.NewSnapshotSink(h.s3, h.repo.Spine())

	// No byte-archived snapshot exists (Capture never ran) — the recovery cannot restore.
	recovery := execution.NewWorkspaceRecovery(h.repo.Spine(), sink, t.TempDir())
	_, err := recovery.RecoverWorkspace(ctx, tenant, execution.RecoverInput{
		WorkspaceID: workspaceID, RunID: newID("run"), SessionID: sessionOf(t, h, workspaceID),
	})
	if err == nil {
		t.Fatal("RecoverWorkspace() with no restorable snapshot returned nil, want ErrRecoveryImpossible")
	}
	var state string
	if err := h.pool.QueryRow(ctx, `SELECT state FROM workspaces WHERE id=$1`, workspaceID).Scan(&state); err != nil {
		t.Fatalf("read workspace state: %v", err)
	}
	if state != "failed" {
		t.Fatalf("workspace state = %q, want failed (explicit, not silent)", state)
	}
}

// sessionOf reads the session a workspace belongs to (for journaling the recovery event).
func sessionOf(t *testing.T, h *artifactsHarness, workspaceID string) string {
	t.Helper()
	var session string
	if err := h.pool.QueryRow(context.Background(), `SELECT session_id FROM workspaces WHERE id=$1`, workspaceID).Scan(&session); err != nil {
		t.Fatalf("read workspace session: %v", err)
	}
	return session
}
