//go:build component

// Workspace snapshot byte-archive component tests (spec §29.10, E10 Task 6). They run in the artifacts
// package because a snapshot archive's bytes live in the SAME object store as artifacts + checkpoints
// (whose S3 credential is control-plane-only, §24) — so the real SeaweedFS + real Postgres the
// artifacts suite stands up are exactly what the snapshot sink + the orphan-GC union need. They exercise
// the execution.SnapshotSink seam directly against real infrastructure.

package artifacts

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/palgroup/palai/adapters/sandboxes/oci/workspace"
	"github.com/palgroup/palai/apps/control-plane/internal/execution"
	"github.com/palgroup/palai/packages/coordinator"
)

// seedAllocationOnDisk lays out a REAL allocation directory (git repo + a scratch file + an excluded
// secret) and seeds the workspace + allocation rows a snapshot FKs, returning the tenant scope, ids, and
// the on-host allocation path. The allocation is the workspace's only (max-fence) one, so a snapshot the
// sink records passes the fence-currency guard.
func (h *artifactsHarness) seedAllocationOnDisk(t *testing.T) (org, project, workspaceID, allocationID, hostPath string) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skipf("git not found: %v", err)
	}
	org, project, runID := h.seedRun(t)
	// The session id the run belongs to (workspaces.session_id FKs sessions).
	var sessionID string
	if err := h.pool.QueryRow(context.Background(), `SELECT session_id FROM runs WHERE id=$1`, runID).Scan(&sessionID); err != nil {
		t.Fatalf("read run session: %v", err)
	}

	hostPath = t.TempDir()
	if r, err := filepath.EvalSymlinks(hostPath); err == nil {
		hostPath = r
	}
	if err := workspace.Prepare(hostPath); err != nil {
		t.Fatal(err)
	}
	repoDir := filepath.Join(hostPath, workspace.RepoDir)
	git := func(args ...string) {
		cmd := exec.Command("git", args...)
		cmd.Dir = repoDir
		cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@e.test", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@e.test")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}
	git("init", "-q", "-b", "main")
	if err := os.WriteFile(filepath.Join(repoDir, "app.go"), []byte("package main\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	git("add", "-A")
	git("commit", "-q", "-m", "seed")
	if err := os.MkdirAll(filepath.Join(hostPath, "secrets"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(hostPath, "secrets", "token"), []byte("SECRET\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	workspaceID = newID("ws")
	allocationID = newID("alloc")
	h.exec(t, `INSERT INTO workspaces (id, organization_id, project_id, session_id, state) VALUES ($1,$2,$3,$4,'leased')`,
		workspaceID, org, project, sessionID)
	h.exec(t, `INSERT INTO workspace_allocations (id, workspace_id, organization_id, project_id, fence, host_path, state)
		VALUES ($1,$2,$3,$4,1,$5,'active')`, allocationID, workspaceID, org, project, hostPath)
	return org, project, workspaceID, allocationID, hostPath
}

// TestSnapshotObjectsSurviveOrphanGC (T6-CONSTRAINT / SAN-005 durability): a captured snapshot's
// byte-archive shares the artifacts bucket, so the orphan-GC must treat a live workspace_snapshots row
// as a reference. With the grace elapsed an unreferenced object is reclaimed; this one SURVIVES because
// its row references it. Deleting the row then makes the SAME object an orphan the next pass reclaims —
// both directions of the union.
func TestSnapshotObjectsSurviveOrphanGC(t *testing.T) {
	h := openArtifactsHarness(t)
	ctx := context.Background()
	org, project, workspaceID, allocationID, hostPath := h.seedAllocationOnDisk(t)
	sink := execution.NewSnapshotSink(h.s3, h.repo.Spine())

	snapID := newID("snap")
	if _, err := sink.Capture(ctx, execution.SnapshotCaptureInput{
		SnapshotID: snapID, Organization: org, Project: project,
		WorkspaceID: workspaceID, AllocationID: allocationID, HostPath: hostPath, Reason: "test",
	}); err != nil {
		t.Fatalf("Capture() error = %v", err)
	}
	var snapKey string
	if err := h.pool.QueryRow(ctx, `SELECT object_key FROM workspace_snapshots WHERE id=$1`, snapID).Scan(&snapKey); err != nil {
		t.Fatalf("read snapshot object_key: %v", err)
	}
	if snapKey == "" || !h.objectPresent(t, snapKey) {
		t.Fatalf("precondition: snapshot object %q absent after capture", snapKey)
	}

	// Grace elapsed: an unreferenced object here would be reclaimed. The live snapshot row keeps it —
	// reference beats grace, for snapshots exactly as for artifacts + checkpoints.
	if _, err := NewCollector(h.s3, h.pool, graceElapsed).Collect(ctx); err != nil {
		t.Fatalf("Collect() error = %v", err)
	}
	if !h.objectPresent(t, snapKey) {
		t.Fatalf("live snapshot object %q was reclaimed by GC — restore bytes destroyed", snapKey)
	}

	// Delete the row: the SAME object is now truly unreferenced and the next pass reclaims it.
	h.exec(t, `DELETE FROM workspace_snapshots WHERE id=$1`, snapID)
	if _, err := NewCollector(h.s3, h.pool, graceElapsed).Collect(ctx); err != nil {
		t.Fatalf("Collect() (row-less) error = %v", err)
	}
	if h.objectPresent(t, snapKey) {
		t.Fatalf("row-less snapshot object %q survived the GC pass", snapKey)
	}
}

// TestSnapshotRestoreRoundTripsThroughStore (SAN-005 restore, over real S3): capture archives the
// allocation + PUTs it; RestoreTo fetches the bytes and re-derives a manifest whose checksums EQUAL the
// create-side row, into a FRESH dir. The restored .git pushes to a bare remote (E09 T8 publication).
func TestSnapshotRestoreRoundTripsThroughStore(t *testing.T) {
	h := openArtifactsHarness(t)
	ctx := context.Background()
	org, project, workspaceID, allocationID, hostPath := h.seedAllocationOnDisk(t)
	sink := execution.NewSnapshotSink(h.s3, h.repo.Spine())
	tenant := coordinator.Tenant{Organization: org, Project: project}

	snapID := newID("snap")
	if _, err := sink.Capture(ctx, execution.SnapshotCaptureInput{
		SnapshotID: snapID, Organization: org, Project: project,
		WorkspaceID: workspaceID, AllocationID: allocationID, HostPath: hostPath, Reason: "pause",
	}); err != nil {
		t.Fatalf("Capture() error = %v", err)
	}
	var wantTree string
	if err := h.pool.QueryRow(ctx, `SELECT tree_checksum FROM workspace_snapshots WHERE id=$1`, snapID).Scan(&wantTree); err != nil {
		t.Fatalf("read create-side tree checksum: %v", err)
	}

	dest := filepath.Join(t.TempDir(), "restored")
	restored, err := sink.RestoreTo(ctx, tenant, snapID, dest)
	if err != nil {
		t.Fatalf("RestoreTo() error = %v", err)
	}
	if restored.TreeChecksum != wantTree {
		t.Fatalf("restored tree %s != create-side %s", restored.TreeChecksum, wantTree)
	}
	// No secret entered the archive/restore (SAN-005 secret-absence).
	if _, err := os.Stat(filepath.Join(dest, "secrets", "token")); !os.IsNotExist(err) {
		t.Fatalf("restored allocation contains the excluded secret (stat err=%v)", err)
	}
	// The restored .git pushes to a bare remote — the publication path (E09 T8 devir).
	bare := filepath.Join(t.TempDir(), "remote.git")
	if out, err := exec.Command("git", "init", "-q", "--bare", bare).CombinedOutput(); err != nil {
		t.Fatalf("init bare remote: %v: %s", err, out)
	}
	push := exec.Command("git", "push", bare, "main")
	push.Dir = filepath.Join(dest, workspace.RepoDir)
	if out, err := push.CombinedOutput(); err != nil {
		t.Fatalf("push restored repo: %v: %s", err, bytes.TrimSpace(out))
	}
}
