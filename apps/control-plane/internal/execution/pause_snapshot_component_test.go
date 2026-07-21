//go:build component

// SES-009 snapshot-half wiring (spec §26.4, §29.10, E10 Task 6): a pause boundary cuts a WORKSPACE
// snapshot, the checkpoint LINKS it (workspace_snapshot_id), and the recovery ladder reads its real
// restorability. White-box, against a real spine; the snapshot bytes ride a FAKE in-memory object store
// (the real SeaweedFS round-trip is proven in the artifacts component suite), so this stays in the
// postgres component tier without a second backing service. It exercises the unexported orchestrator
// seams directly — captureBoundarySnapshot, persistCheckpoint's link, and workspaceRestorable.

package execution

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/palgroup/palai/adapters/sandboxes/oci/workspace"
	"github.com/palgroup/palai/packages/contracts"
	"github.com/palgroup/palai/packages/coordinator"
	"github.com/palgroup/palai/packages/coordinator/recovery"
	"github.com/palgroup/palai/packages/runner"
)

// memObjectStore is an in-memory SnapshotObjectStore/CheckpointObjectStore: the snapshot + checkpoint
// bytes live in a map keyed by object key. It satisfies both sink interfaces with a real SHA-256 so the
// row checksums are honest; only the transport is faked (proven real in the artifacts suite).
type memObjectStore struct {
	mu   sync.Mutex
	objs map[string][]byte
}

func newMemObjectStore() *memObjectStore { return &memObjectStore{objs: map[string][]byte{}} }

func (m *memObjectStore) Put(_ context.Context, key string, body []byte) (string, int64, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	cp := make([]byte, len(body))
	copy(cp, body)
	m.objs[key] = cp
	return checksumBytes(body), int64(len(body)), nil
}

func (m *memObjectStore) Get(_ context.Context, key string) ([]byte, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	body, ok := m.objs[key]
	return body, ok, nil
}

func checksumBytes(b []byte) string {
	sum := sha256.Sum256(b)
	return "sha256:" + hex.EncodeToString(sum[:])
}

// TestPauseCheckpointCarriesBoundarySnapshot proves the SES-009 snapshot half end to end at the seam:
// captureBoundarySnapshot cuts the workspace snapshot, persistCheckpoint LINKS it on the checkpoint row,
// and workspaceRestorable reads it as restorable. A manifest-only snapshot is NOT restorable, and a
// sink-less orchestrator cuts nothing (the T4 behaviour, bit-unchanged).
func TestPauseCheckpointCarriesBoundarySnapshot(t *testing.T) {
	url := os.Getenv("PALAI_COMPONENT_POSTGRES_URL")
	if url == "" {
		t.Skip("PALAI_COMPONENT_POSTGRES_URL is required; run make test-component TEST=postgres")
	}
	if _, err := exec.LookPath("git"); err != nil {
		t.Skipf("git not found: %v", err)
	}
	ctx := context.Background()
	cs, err := coordinator.Open(ctx, url)
	if err != nil {
		t.Fatalf("coordinator.Open() error = %v", err)
	}
	t.Cleanup(cs.Close)
	if err := cs.Migrate(ctx); err != nil {
		t.Fatalf("Migrate() error = %v", err)
	}
	pool := cs.Pool()

	tenant := coordinator.Tenant{Organization: redeliveryID("org"), Project: redeliveryID("prj")}
	sessionID, runID, attemptID := redeliveryID("ses"), redeliveryID("run"), redeliveryID("att")
	execSQL(t, pool, `INSERT INTO organizations (id) VALUES ($1)`, tenant.Organization)
	execSQL(t, pool, `INSERT INTO projects (id, organization_id) VALUES ($1, $2)`, tenant.Project, tenant.Organization)
	execSQL(t, pool, `INSERT INTO sessions (id, organization_id, project_id) VALUES ($1, $2, $3)`, sessionID, tenant.Organization, tenant.Project)
	execSQL(t, pool, `INSERT INTO runs (id, organization_id, project_id, session_id, state) VALUES ($1, $2, $3, $4, 'running')`, runID, tenant.Organization, tenant.Project, sessionID)
	execSQL(t, pool, `INSERT INTO attempts (id, organization_id, project_id, run_id, fence, state) VALUES ($1, $2, $3, $4, 1, 'assigned')`, attemptID, tenant.Organization, tenant.Project, runID)

	// A real allocation on disk + its workspace/allocation rows.
	workspaceID := seedLeasedWorkspace(t, cs, tenant, sessionID, runID)
	hostPath := seedAllocationDir(t, cs, tenant, workspaceID)

	store := newMemObjectStore()
	o := &Orchestrator{
		spine:       cs,
		route:       defaultModelRoute,
		snapshots:   NewSnapshotSink(store, cs),
		checkpoints: NewCheckpointSink(store, recovery.New(pool)),
	}
	st := &attemptState{
		attempt:     AttemptDescriptor{RunID: contracts.RunID(runID), AttemptID: contracts.AttemptID(attemptID), WorkspaceHostPath: hostPath},
		tenant:      tenant,
		sessionID:   sessionID,
		workspaceID: workspaceID,
	}

	// Cut the boundary snapshot (the pause path's first step) and link it on the checkpoint.
	snapID, err := o.captureBoundarySnapshot(ctx, st)
	if err != nil {
		t.Fatalf("captureBoundarySnapshot() error = %v", err)
	}
	if snapID == "" {
		t.Fatal("captureBoundarySnapshot returned no id — the pause boundary cut no snapshot")
	}
	var snapObjectKey string
	if err := pool.QueryRow(ctx, `SELECT object_key FROM workspace_snapshots WHERE id=$1`, snapID).Scan(&snapObjectKey); err != nil {
		t.Fatalf("read cut snapshot: %v", err)
	}
	if snapObjectKey == "" {
		t.Fatal("cut snapshot recorded no object key — it is manifest-only, not restorable")
	}

	offer := contracts.EngineFrame{Type: "checkpoint.offer", Sequence: 5, Data: map[string]any{
		"format": "reference-kernel", "format_version": float64(1),
		"state": base64.StdEncoding.EncodeToString([]byte(`{"state":"paused","step":3}`)),
	}}
	if err := o.persistCheckpoint(ctx, st, offer, snapID); err != nil {
		t.Fatalf("persistCheckpoint() error = %v", err)
	}
	var linked *string
	if err := pool.QueryRow(ctx, `SELECT workspace_snapshot_id FROM checkpoints WHERE run_id=$1`, runID).Scan(&linked); err != nil {
		t.Fatalf("read checkpoint link: %v", err)
	}
	if linked == nil || *linked != snapID {
		t.Fatalf("checkpoint workspace_snapshot_id = %v, want the cut snapshot %q", linked, snapID)
	}

	// The ladder reads this snapshot as restorable (it has bytes).
	if restorable, err := o.workspaceRestorable(ctx, tenant, snapID); err != nil || !restorable {
		t.Fatalf("workspaceRestorable(byte-archived) = %v, %v; want true", restorable, err)
	}
	// A manifest-only snapshot (E09 shape, no bytes) is NOT restorable — the ladder rejects it.
	manifestOnly := redeliveryID("snap")
	execSQL(t, pool, `INSERT INTO workspace_snapshots (id, workspace_id, allocation_id, organization_id, project_id, fencing_token, tree_checksum)
		SELECT $1, a.workspace_id, a.id, a.organization_id, a.project_id, a.fence, 'sha256:x'
		FROM workspace_allocations a WHERE a.workspace_id=$2 ORDER BY a.fence DESC LIMIT 1`, manifestOnly, workspaceID)
	if restorable, err := o.workspaceRestorable(ctx, tenant, manifestOnly); err != nil || restorable {
		t.Fatalf("workspaceRestorable(manifest-only) = %v, %v; want false", restorable, err)
	}
	// A checkpoint with NO snapshot is vacuously restorable (no workspace dependency, unchanged from T4).
	if restorable, err := o.workspaceRestorable(ctx, tenant, ""); err != nil || !restorable {
		t.Fatalf("workspaceRestorable(none) = %v, %v; want true (vacuous)", restorable, err)
	}

	// A sink-less orchestrator cuts nothing — the pause stays checkpoint-only, bit-unchanged from T4.
	if id, err := (&Orchestrator{spine: cs}).captureBoundarySnapshot(ctx, st); err != nil || id != "" {
		t.Fatalf("sink-less captureBoundarySnapshot = %q, %v; want \"\" (no snapshot cut)", id, err)
	}
}

// seedLeasedWorkspace creates a leased workspace bound to the session/run.
func seedLeasedWorkspace(t *testing.T, cs *coordinator.Store, tenant coordinator.Tenant, sessionID, runID string) string {
	t.Helper()
	wsID := redeliveryID("wsp")
	if err := cs.CreateWorkspace(context.Background(), tenant, coordinator.WorkspaceInput{
		WorkspaceID: wsID, SessionID: sessionID, RunID: runID, State: "leased",
	}); err != nil {
		t.Fatalf("CreateWorkspace() error = %v", err)
	}
	return wsID
}

// seedAllocationDir mints an allocation on a REAL host dir with a git repo (so a snapshot has a .git
// tree to capture) and returns the host path.
func seedAllocationDir(t *testing.T, cs *coordinator.Store, tenant coordinator.Tenant, workspaceID string) string {
	t.Helper()
	dir := t.TempDir()
	if r, err := filepath.EvalSymlinks(dir); err == nil {
		dir = r
	}
	if err := workspace.Prepare(dir); err != nil {
		t.Fatal(err)
	}
	repoDir := filepath.Join(dir, workspace.RepoDir)
	git := func(args ...string) {
		cmd := exec.Command("git", args...)
		cmd.Dir = repoDir
		cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@e.test", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@e.test")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}
	git("init", "-q", "-b", "main")
	if err := os.WriteFile(filepath.Join(repoDir, "f.txt"), []byte("hi\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	git("add", "-A")
	git("commit", "-q", "-m", "seed")
	if _, err := cs.AllocateWorkspace(context.Background(), redeliveryID("wal"), workspaceID, dir); err != nil {
		t.Fatalf("AllocateWorkspace() error = %v", err)
	}
	return dir
}

// offerChannel is an EngineChannel that swallows the controller's checkpoint.request and then hands back
// exactly one checkpoint.offer, so a test can drive the REAL checkpointBeforePause drain loop.
type offerChannel struct {
	offer contracts.EngineFrame
	recv  int
}

func (c *offerChannel) Send(context.Context, contracts.EngineFrame) error { return nil }
func (c *offerChannel) Receive(context.Context) (contracts.EngineFrame, error) {
	c.recv++
	if c.recv == 1 {
		return c.offer, nil
	}
	return contracts.EngineFrame{}, io.EOF
}
func (c *offerChannel) Close() error { return nil }

// TestPauseBoundaryCutsAndLinksSnapshotThroughOrchestrator drives the REAL checkpointBeforePause path
// (not the helpers separately) with the snapshot sink wired — exactly as main.go now wires it — and
// proves the persisted checkpoint carries a NON-NULL workspace_snapshot_id whose snapshot has bytes
// (restorable, NOT vacuous-true). This is the wired-binary proof MUST-FIX #1 asks for.
func TestPauseBoundaryCutsAndLinksSnapshotThroughOrchestrator(t *testing.T) {
	url := os.Getenv("PALAI_COMPONENT_POSTGRES_URL")
	if url == "" {
		t.Skip("PALAI_COMPONENT_POSTGRES_URL is required; run make test-component TEST=postgres")
	}
	if _, err := exec.LookPath("git"); err != nil {
		t.Skipf("git not found: %v", err)
	}
	ctx := context.Background()
	cs, err := coordinator.Open(ctx, url)
	if err != nil {
		t.Fatalf("coordinator.Open() error = %v", err)
	}
	t.Cleanup(cs.Close)
	if err := cs.Migrate(ctx); err != nil {
		t.Fatalf("Migrate() error = %v", err)
	}
	pool := cs.Pool()

	tenant := coordinator.Tenant{Organization: redeliveryID("org"), Project: redeliveryID("prj")}
	sessionID, runID, attemptID := redeliveryID("ses"), redeliveryID("run"), redeliveryID("att")
	execSQL(t, pool, `INSERT INTO organizations (id) VALUES ($1)`, tenant.Organization)
	execSQL(t, pool, `INSERT INTO projects (id, organization_id) VALUES ($1, $2)`, tenant.Project, tenant.Organization)
	execSQL(t, pool, `INSERT INTO sessions (id, organization_id, project_id) VALUES ($1, $2, $3)`, sessionID, tenant.Organization, tenant.Project)
	execSQL(t, pool, `INSERT INTO runs (id, organization_id, project_id, session_id, state) VALUES ($1, $2, $3, $4, 'running')`, runID, tenant.Organization, tenant.Project, sessionID)
	execSQL(t, pool, `INSERT INTO attempts (id, organization_id, project_id, run_id, fence, state) VALUES ($1, $2, $3, $4, 1, 'assigned')`, attemptID, tenant.Organization, tenant.Project, runID)
	workspaceID := seedLeasedWorkspace(t, cs, tenant, sessionID, runID)
	hostPath := seedAllocationDir(t, cs, tenant, workspaceID)

	store := newMemObjectStore()
	o := &Orchestrator{
		spine:       cs,
		route:       defaultModelRoute,
		snapshots:   NewSnapshotSink(store, cs),
		checkpoints: NewCheckpointSink(store, recovery.New(pool)),
	}
	st := &attemptState{
		attempt:     AttemptDescriptor{RunID: contracts.RunID(runID), AttemptID: contracts.AttemptID(attemptID), WorkspaceHostPath: hostPath},
		tenant:      tenant,
		sessionID:   sessionID,
		workspaceID: workspaceID,
		ledger:      runner.NewFrameLedger(),
		ch: &offerChannel{offer: contracts.EngineFrame{
			Protocol: engineProtocol, ID: newFrameID(), Type: "checkpoint.offer", Sequence: 1,
			Time: time.Now().UTC().Format(time.RFC3339), RunID: contracts.RunID(runID), AttemptID: contracts.AttemptID(attemptID),
			Data: map[string]any{"format": "reference-kernel", "format_version": float64(1),
				"state": base64.StdEncoding.EncodeToString([]byte(`{"state":"paused","step":2}`))},
		}},
	}

	if err := o.checkpointBeforePause(ctx, st); err != nil {
		t.Fatalf("checkpointBeforePause() error = %v", err)
	}

	// The persisted checkpoint carries a NON-NULL workspace_snapshot_id, and that snapshot has bytes.
	var snapID *string
	if err := pool.QueryRow(ctx, `SELECT workspace_snapshot_id FROM checkpoints WHERE run_id=$1`, runID).Scan(&snapID); err != nil {
		t.Fatalf("read checkpoint link: %v", err)
	}
	if snapID == nil {
		t.Fatal("a pause with the snapshot sink WIRED left workspace_snapshot_id NULL — the SES-009 cut/link is inert")
	}
	var objectKey string
	if err := pool.QueryRow(ctx, `SELECT object_key FROM workspace_snapshots WHERE id=$1`, *snapID).Scan(&objectKey); err != nil {
		t.Fatalf("read linked snapshot: %v", err)
	}
	if objectKey == "" {
		t.Fatal("the linked snapshot is manifest-only — restorability would be vacuous, not real")
	}
	if restorable, err := o.workspaceRestorable(ctx, tenant, *snapID); err != nil || !restorable {
		t.Fatalf("workspaceRestorable(linked) = %v, %v; want true (the ladder must NOT be vacuous)", restorable, err)
	}
}
