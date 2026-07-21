//go:build fault

package recovery

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/palgroup/palai/adapters/sandboxes/oci/snapshot"
	"github.com/palgroup/palai/adapters/sandboxes/oci/workspace"
	"github.com/palgroup/palai/packages/contracts"
	"github.com/palgroup/palai/packages/coordinator"
)

// The whole-host kill fault half of E10 Task 6 (spec §26.8, §29.7-29.8, ENG-006/SAN-006). It proves the
// T6 recovery invariants — a host move mints a NEW fenced allocation, the boundary snapshot restores
// checksum-EQUAL, and the fenced-out old host's authoritative writes are denied — SURVIVE a REAL kill of
// the engine sandbox container (the local-tier whole-host stand-in), not a simulated one.
//
// HONEST CEILING (named, not hidden): "whole host" here is the single-machine approach — the engine
// sandbox container + its process are killed and the allocation is reclaimed; a real multi-host fleet
// drain (a runner daemon on a separate box, host-path access severed at the network) is E14/E15. The
// snapshot BYTES ride the local filesystem here, not the object store (the S3 round-trip is proven in
// the artifacts component tier; artifacts.Store is control-plane-internal and not importable here); this
// test owns the FENCING + RESTORE-under-real-kill half. The tests are SERIALIZED on :local and gated on
// a real Postgres + the fixture engine image — they skip cleanly otherwise.

// requireHostKillEnv skips unless a real Postgres is reachable (PALAI_COMPONENT_POSTGRES_URL) — the
// human wires it when serializing this suite on :local. The fixture engine image (for the real kill) is
// resolved by engineDigest, which itself skips when PALAI_RUNNER_ENGINE_IMAGE_ID is unset.
func requireHostKillEnv(t *testing.T) string {
	t.Helper()
	url := os.Getenv("PALAI_COMPONENT_POSTGRES_URL")
	if url == "" {
		t.Skip("PALAI_COMPONENT_POSTGRES_URL is required; the host-kill fault suite is serialized on :local")
	}
	return url
}

func hostKillID(prefix string) string {
	var raw [8]byte
	_, _ = rand.Read(raw[:])
	return prefix + "_" + hex.EncodeToString(raw[:])
}

// killEngineContainer starts the fixture engine in a real container, waits for engine.ready, then does an
// external `docker rm -f` — the whole-host stand-in — and asserts the container is gone. It is the REAL
// kill the recovery below happens after (not a simulated host loss).
func killEngineContainer(t *testing.T) {
	t.Helper()
	sup := newStreamSupervisor(t)
	request := fixtureRequest(engineDigest(t), "interactive")
	request.Limits.WallTimeMS = 30000

	sawReady := make(chan struct{}, 1)
	sink := func(_ context.Context, frame contracts.EngineFrame) error {
		if frame.Type == "engine.ready" {
			select {
			case sawReady <- struct{}{}:
			default:
			}
		}
		return nil
	}
	inbound := make(chan contracts.EngineFrame) // never fed: the engine blocks awaiting a model.result

	done := make(chan struct{})
	go func() {
		<-sawReady
		removeSandboxContainers(t) // the real kill: `docker rm -f` the engine sandbox
		close(done)
	}()

	result, err := sup.Stream(context.Background(), request, inbound, sink)
	if err == nil {
		t.Fatal("a killed engine container must fail the attempt, not report success")
	}
	<-done
	assertContainerGone(t, result.ContainerID)
}

// seedRepoAllocation lays out a real allocation dir with a git repo (so the snapshot has a .git tree) and
// returns the host path.
func seedRepoAllocation(t *testing.T) string {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skipf("git not found: %v", err)
	}
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
	if err := os.WriteFile(filepath.Join(repoDir, "app.go"), []byte("package main\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	git("add", "-A")
	git("commit", "-q", "-m", "work")
	return dir
}

// openSpine opens + migrates a real coordinator against the injected Postgres.
func openSpine(t *testing.T, url string) *coordinator.Store {
	t.Helper()
	ctx := context.Background()
	cs, err := coordinator.Open(ctx, url)
	if err != nil {
		t.Fatalf("coordinator.Open() error = %v", err)
	}
	t.Cleanup(cs.Close)
	if err := cs.Migrate(ctx); err != nil {
		t.Fatalf("Migrate() error = %v", err)
	}
	return cs
}

// seedWorkspaceWithAllocation seeds org→project→session→workspace + one allocation on hostPath, returning
// the tenant, workspace id, and allocation.
func seedWorkspaceWithAllocation(t *testing.T, cs *coordinator.Store, hostPath string) (coordinator.Tenant, string, coordinator.Allocation) {
	t.Helper()
	ctx := context.Background()
	tenant := coordinator.Tenant{Organization: hostKillID("org"), Project: hostKillID("prj")}
	sessionID := hostKillID("ses")
	pool := cs.Pool()
	mustExec(t, pool, `INSERT INTO organizations (id) VALUES ($1)`, tenant.Organization)
	mustExec(t, pool, `INSERT INTO projects (id, organization_id) VALUES ($1, $2)`, tenant.Project, tenant.Organization)
	mustExec(t, pool, `INSERT INTO sessions (id, organization_id, project_id) VALUES ($1, $2, $3)`, sessionID, tenant.Organization, tenant.Project)
	wsID := hostKillID("wsp")
	if err := cs.CreateWorkspace(ctx, tenant, coordinator.WorkspaceInput{WorkspaceID: wsID, SessionID: sessionID, State: "leased"}); err != nil {
		t.Fatalf("CreateWorkspace() error = %v", err)
	}
	alloc, err := cs.AllocateWorkspace(ctx, hostKillID("wal"), wsID, hostPath)
	if err != nil {
		t.Fatalf("AllocateWorkspace() error = %v", err)
	}
	return tenant, wsID, alloc
}

func mustExec(t *testing.T, pool *pgxpool.Pool, sql string, args ...any) {
	t.Helper()
	if _, err := pool.Exec(context.Background(), sql, args...); err != nil {
		t.Fatalf("exec %q: %v", sql, err)
	}
}

// TestRunnerDaemonKillAdvancesFenceAndRecovers (ENG-006 fault-live): a REAL engine container kill (the
// whole-host stand-in), then the recovery mints a NEW fenced allocation and restores the boundary
// snapshot checksum-EQUAL. The logical workspace id is stable; only a strictly higher fence appears.
func TestRunnerDaemonKillAdvancesFenceAndRecovers(t *testing.T) {
	url := requireHostKillEnv(t)
	cs := openSpine(t, url)
	ctx := context.Background()

	// The workspace as it stood before the kill: a real allocation + its boundary snapshot (bytes on the
	// local FS — the S3 round-trip is the component tier's).
	oldDir := seedRepoAllocation(t)
	_, wsID, oldAlloc := seedWorkspaceWithAllocation(t, cs, oldDir)
	var archive bytes.Buffer
	created, err := snapshot.Archive(oldDir, &archive)
	if err != nil {
		t.Fatalf("Archive() error = %v", err)
	}
	if err := cs.CreateWorkspaceSnapshot(ctx, coordinator.SnapshotInput{
		SnapshotID: hostKillID("snap"), AllocationID: oldAlloc.ID,
		TreeChecksum: created.TreeChecksum, IndexChecksum: created.IndexChecksum,
		ObjectKey: "local://" + oldDir, ArchiveChecksum: "sha256:local", SizeBytes: int64(archive.Len()),
	}); err != nil {
		t.Fatalf("CreateWorkspaceSnapshot() error = %v", err)
	}

	// The REAL kill of the engine sandbox container (the whole-host stand-in).
	killEngineContainer(t)

	// Recovery: mint the NEW fenced allocation (logical id stable) and restore the boundary snapshot into
	// it, verifying the create-side checksums re-derive EQUAL (SAN-005).
	newDir := t.TempDir()
	newAlloc, err := cs.AllocateWorkspace(ctx, hostKillID("wal"), wsID, newDir)
	if err != nil {
		t.Fatalf("AllocateWorkspace(new) error = %v", err)
	}
	if newAlloc.Fence <= oldAlloc.Fence {
		t.Fatalf("new fence %d is not > old fence %d — a host move must advance the fence", newAlloc.Fence, oldAlloc.Fence)
	}
	restored, err := snapshot.Restore(bytes.NewReader(archive.Bytes()), filepath.Join(newDir, "restored"), created)
	if err != nil {
		t.Fatalf("Restore() error = %v", err)
	}
	if restored.TreeChecksum != created.TreeChecksum {
		t.Fatalf("restored tree %s != create %s", restored.TreeChecksum, created.TreeChecksum)
	}
}

// TestHostKillFencesStaleWriter (SAN-006 fault-live): after the kill + fence advance, the fenced-out OLD
// allocation can no longer acquire the writer lease or record a snapshot — its authoritative writes are
// rejected at the DB (ErrStaleAllocation), so a returning old host cannot corrupt the recovered workspace.
func TestHostKillFencesStaleWriter(t *testing.T) {
	url := requireHostKillEnv(t)
	cs := openSpine(t, url)
	ctx := context.Background()

	oldDir := seedRepoAllocation(t)
	_, wsID, oldAlloc := seedWorkspaceWithAllocation(t, cs, oldDir)

	killEngineContainer(t) // the real kill

	// The host move: a new fenced allocation supersedes the old one.
	if _, err := cs.AllocateWorkspace(ctx, hostKillID("wal"), wsID, t.TempDir()); err != nil {
		t.Fatalf("AllocateWorkspace(new) error = %v", err)
	}

	// The OLD allocation is now a lower fence: its authoritative writes are DENIED at the DB.
	if err := cs.AcquireWriterLease(ctx, hostKillID("lease"), oldAlloc.ID, hostKillID("run")); err != coordinator.ErrStaleAllocation {
		t.Fatalf("stale old-host lease = %v, want ErrStaleAllocation", err)
	}
	if err := cs.CreateWorkspaceSnapshot(ctx, coordinator.SnapshotInput{
		SnapshotID: hostKillID("snap"), AllocationID: oldAlloc.ID, TreeChecksum: "sha256:stale", ObjectKey: "k",
	}); err != coordinator.ErrStaleAllocation {
		t.Fatalf("stale old-host snapshot = %v, want ErrStaleAllocation", err)
	}
}
