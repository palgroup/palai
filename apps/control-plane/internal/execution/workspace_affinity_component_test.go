//go:build component

package execution

// WORKSPACE AFFINITY ACROSS A POOL OF MACS (Faz A.5 T5) — the door a multi-machine pool cannot be
// opened without.
//
// A session's bytes live on the Mac its last attempt ran on. The wake that re-enters a parked run is
// keyed on the POOL (runner_gateway.go's wakeParkedRun passes poolKey(poolID)), and the rendezvous hands
// over whichever machine is free, so attempt N+1 of the SAME run can land somewhere attempt N's tree is
// not. What the platform did about that is the subject of this file.
//
// WHAT WAS BELIEVED, AND WHAT WAS MEASURED. The devir note (§2c) and provision.go's own comment both
// said the reuse path "fails loudly" on a foreign machine — the head read has nothing to read. It does,
// on a run's FIRST reuse. A park-then-wake produces the SECOND, and on that one reuseAllocation found
// the preparation receipt attempt 1 had recorded for the same run id and returned BEFORE reading the
// tree at all. The run continued against the empty directory the new machine had just created, with no
// error, no log line and no terminal. These tests are that measurement, and then its repair.
//
// TWO MACS, ONE PROCESS, AND WHAT THAT MODEL DOES AND DOES NOT PROVE. Each machine here is a real
// runner.WorkspaceServer — the SHIPPED one, with its own managed allocation root — reached over a
// connection that satisfies the same WorkspaceConn the gateway's channel does. What is stood in for is
// the kernel: two Macs configure the SAME PALAI_WORKSPACE_ROOT, so one absolute path resolves to two
// different disks, and one process cannot have that. The fake connection supplies it by REWRITING the
// control plane's spelling of the root into this machine's, which is exactly the property two hosts
// have for free. Everything the control plane does — plan, open, head, migrate, restore, verify — runs
// unmodified against it.
//
// SO THE HONEST LIMIT OF THIS FILE: it proves the control plane and the machine protocol behave
// correctly when the bytes are elsewhere. It does not prove a network, and it cannot: there is no
// second Mac on the machine these tests run on.

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/palgroup/palai/adapters/repositories"
	"github.com/palgroup/palai/adapters/sandboxes/oci/workspace"
	"github.com/palgroup/palai/apps/control-plane/internal/store"
	"github.com/palgroup/palai/packages/contracts"
	"github.com/palgroup/palai/packages/coordinator"
	modelbroker "github.com/palgroup/palai/packages/model-broker"
	"github.com/palgroup/palai/packages/runner"
	toolbroker "github.com/palgroup/palai/packages/tool-broker"

	"github.com/palgroup/palai/storage"
)

// fakeMac is one machine in the pool: an identity, a managed allocation root of its own, and the shipped
// workspace server bound to it.
//
// `cpRoot` is the root the CONTROL PLANE names in every path it mints, and `root` is where this machine
// actually keeps its allocations. On two real Macs the two are the same string and different disks; here
// they are different strings and the translation below is what stands in for the disk.
type fakeMac struct {
	id     string
	root   string
	cpRoot string
	server *runner.WorkspaceServer
}

func newFakeMac(t *testing.T, id, cpRoot string) *fakeMac {
	t.Helper()
	// EvalSymlinks for the reason every allocation path in this tree is resolved once: macOS puts
	// t.TempDir() behind /var -> /private/var, the workspace server resolves its own root, and an
	// unresolved root would make the server's under-root check compare two spellings of one directory.
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatalf("resolve the root of machine %s: %v", id, err)
	}
	return &fakeMac{id: id, root: root, cpRoot: cpRoot, server: runner.NewWorkspaceServer(root)}
}

// diskPath is where a control-plane-named allocation lands on THIS machine. A path that is already this
// machine's spelling is returned unchanged, which is what makes the sequence open-then-operate work: the
// open answers with the machine's own path and RemoteWorkspace keeps that answer, exactly as it keeps a
// macOS runner's /private/var answer.
func (m *fakeMac) diskPath(root string) string {
	rel, err := filepath.Rel(m.cpRoot, root)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return root
	}
	return filepath.Join(m.root, rel)
}

// macChannel is an EngineChannel that reaches one machine — the two sibling interfaces the orchestrator
// type-asserts for, MachineIdentified and WorkspaceConn, on one value, the way gatewayChannel carries
// them.
type macChannel struct {
	EngineChannel
	mac *fakeMac
}

func (c macChannel) MachineID() string { return c.mac.id }

var (
	_ MachineIdentified = macChannel{}
	_ WorkspaceConn     = macChannel{}
)

// StartWorkspace performs one ws.request against this machine's server.
//
// IT GOES THROUGH JSON IN BOTH DIRECTIONS, AND THAT IS NOT CEREMONY. A `[]byte` marshals to a base64
// STRING and comes back as one, so an archive handed to the server as a live []byte would exercise a
// decode path production never takes — and the restore verb's whole payload is bytes. A double that
// skipped the encoding would be green here and broken on the wire.
func (c macChannel) StartWorkspace(ctx context.Context, wsID, op, root string, payload map[string]any) (<-chan WorkspaceAnswer, func(), error) {
	encoded, err := json.Marshal(contracts.RunnerMessage{
		Protocol: runner.RunnerProtocolV1,
		Type:     runner.WorkspaceRequestType,
		Data:     runner.WorkspaceRequestData(wsID, op, c.mac.diskPath(root), payload),
	})
	if err != nil {
		return nil, nil, err
	}
	var request contracts.RunnerMessage
	if err := json.Unmarshal(encoded, &request); err != nil {
		return nil, nil, err
	}
	back, err := json.Marshal(c.mac.server.Handle(ctx, request))
	if err != nil {
		return nil, nil, err
	}
	var result contracts.RunnerMessage
	if err := json.Unmarshal(back, &result); err != nil {
		return nil, nil, err
	}
	answers := make(chan WorkspaceAnswer, 1)
	answers <- decodeWorkspaceAnswer(result.Data)
	return answers, func() {}, nil
}

// affinityFixture is a real spine, one tenant, one session with a bound workspace, and TWO machines in
// one pool: `full` declares a capacity that is already taken (so an attempt landing on it parks after
// provisioning, which is §2c's scenario verbatim) and `free` declares none.
type affinityFixture struct {
	cs         *coordinator.Store
	tenant     coordinator.Tenant
	sessionID  string
	responseID string
	runID      string
	poolID     string
	wsID       string
	cpRoot     string
	allocPath  string
	full       *fakeMac
	free       *fakeMac
	objects    *memObjectStore
	orch       *Orchestrator
}

func newAffinityFixture(t *testing.T) *affinityFixture {
	t.Helper()
	url := os.Getenv("PALAI_COMPONENT_POSTGRES_URL")
	if url == "" {
		t.Skip("PALAI_COMPONENT_POSTGRES_URL is required; run TEST=postgres scripts/test/component")
	}
	if _, err := exec.LookPath("git"); err != nil {
		t.Skipf("git not found: %v", err)
	}
	ctx := context.Background()
	cs, err := coordinator.Open(ctx, url)
	if err != nil {
		t.Fatalf("coordinator.Open: %v", err)
	}
	t.Cleanup(cs.Close)
	if err := cs.Migrate(ctx); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	repo, err := store.Open(ctx, url)
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(repo.Close)

	cpRoot, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatalf("resolve the control plane's provision root: %v", err)
	}
	f := &affinityFixture{
		cs:         cs,
		tenant:     coordinator.Tenant{Project: redeliveryID("prj")},
		sessionID:  redeliveryID("ses"),
		responseID: redeliveryID("resp"),
		runID:      redeliveryID("run"),
		poolID:     redeliveryID("pool"),
		wsID:       redeliveryID("wsp"),
		cpRoot:     cpRoot,
		objects:    newMemObjectStore(),
	}
	f.full = newFakeMac(t, redeliveryID("rnr"), cpRoot)
	f.free = newFakeMac(t, redeliveryID("rnr"), cpRoot)

	bindingID := redeliveryID("bnd")
	pool := cs.Pool()
	execSQL(t, pool, `INSERT INTO projects (id) VALUES ($1)`, f.tenant.Project)
	// Named 'default' because fleet.ResolvePool's last-but-one step resolves the tenant's own pool by
	// that name; a randomly-named one sends `place` to the bootstrap CONSTANT and the run fails as
	// not-servable long before it reaches a machine.
	execSQL(t, pool, `INSERT INTO runner_pools (id, project_id, name, posture)
	                  VALUES ($1,$2,'default','unsandboxed-host')`, f.poolID, f.tenant.Project)
	// ONE capacity on the machine that holds the bytes, so the first attempt provisions there and then
	// PARKS — the exact sequence §2c names — and NONE on the second, so the woken attempt runs.
	execSQL(t, pool, `INSERT INTO runners (id, project_id, pool_id, state, posture, capacity)
	                  VALUES ($1,$2,$3,'active','unsandboxed-host',1)`, f.full.id, f.tenant.Project, f.poolID)
	execSQL(t, pool, `INSERT INTO runners (id, project_id, pool_id, state, posture, capacity)
	                  VALUES ($1,$2,$3,'active','unsandboxed-host',0)`, f.free.id, f.tenant.Project, f.poolID)
	execSQL(t, pool, `INSERT INTO sessions (id, project_id) VALUES ($1,$2)`, f.sessionID, f.tenant.Project)
	execSQL(t, pool, `INSERT INTO responses (id, project_id, session_id, state, input)
	                  VALUES ($1,$2,$3,'queued','"keep going"'::jsonb)`, f.responseID, f.tenant.Project, f.sessionID)
	execSQL(t, pool, `INSERT INTO runs (id, project_id, session_id, response_id, state)
	                  VALUES ($1,$2,$3,$4,'queued')`, f.runID, f.tenant.Project, f.sessionID, f.responseID)
	execSQL(t, pool, `INSERT INTO repository_bindings (id, project_id, provider, repository_identity, clone_url)
	                  VALUES ($1,$2,'local','palai/fixture','file:///dev/null')`, bindingID, f.tenant.Project)

	if err := cs.CreateWorkspace(ctx, f.tenant, coordinator.WorkspaceInput{
		WorkspaceID: f.wsID, SessionID: f.sessionID, State: "ready",
	}); err != nil {
		t.Fatalf("CreateWorkspace: %v", err)
	}
	// WorkspaceForSession requires repository_binding_id <> '', and CreateWorkspace takes no binding.
	// Without it planRootWorkspace answers found=false and every attempt skips the workspace arm.
	execSQL(t, pool, `UPDATE workspaces SET repository_binding_id = $2 WHERE id = $1`, f.wsID, bindingID)

	// THE ALLOCATION IS NAMED IN THE CONTROL PLANE'S SPELLING AND EXISTS ON ONE MACHINE ONLY. That is the
	// whole starting condition: a session mid-conversation, its tree on the Mac that last served it.
	f.allocPath = filepath.Join(cpRoot, "alloc_affinity")
	if _, err := cs.AllocateWorkspace(storage.WithTenant(ctx, f.tenant.Project),
		redeliveryID("wal"), f.wsID, f.allocPath); err != nil {
		t.Fatalf("AllocateWorkspace: %v", err)
	}
	seedMacRepo(t, f.full.diskPath(f.allocPath))

	f.orch = NewOrchestrator(repo, nil, modelbroker.New(modelbroker.Config{}), toolbroker.New())
	f.orch.SetWorkspaceProvisioner(cpRoot, repositories.NewAnonymousBroker())
	f.orch.snapshots = NewSnapshotSink(f.objects, cs)
	return f
}

// affinityUncommitted is the file this session has edited and NOT committed. It is the whole reason a
// migration may not re-clone: a clean checkout at the same ref would satisfy every structural assertion
// about the repository and would have thrown this away.
const affinityUncommitted = "the work this session had not committed yet\n"

// seedMacRepo lays an allocation out on one machine's disk and puts a real repository in it: one commit,
// plus an uncommitted edit.
func seedMacRepo(t *testing.T, dir string) {
	t.Helper()
	if err := workspace.Prepare(dir); err != nil {
		t.Fatalf("lay out the allocation on the machine: %v", err)
	}
	repoDir := workspace.RepoPath(dir)
	git := func(args ...string) {
		cmd := exec.Command("git", append([]string{"-C", repoDir}, args...)...)
		cmd.Env = append(os.Environ(),
			"GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null",
			"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@e.test",
			"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@e.test")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v in %s: %v\n%s", args, repoDir, err, out)
		}
	}
	git("init", "-q", "-b", "main")
	if err := os.WriteFile(filepath.Join(repoDir, "committed.txt"), []byte("base\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	git("add", "-A")
	git("commit", "-q", "-m", "seed")
	if err := os.WriteFile(filepath.Join(repoDir, "in-progress.txt"), []byte(affinityUncommitted), 0o644); err != nil {
		t.Fatal(err)
	}
}

// macDialer places the next attempt on one named machine. Production picks with a FIFO rendezvous and
// this test picks by hand, which is the difference between measuring placement and measuring what the
// platform does once a placement has happened — the second is this file's subject.
type macDialer struct {
	ch  *scriptedChannel
	mac *fakeMac
}

func (d *macDialer) Dial(context.Context, AttemptDescriptor) (EngineChannel, error) {
	return macChannel{EngineChannel: d.ch, mac: d.mac}, nil
}

// attemptOn drives ONE whole attempt through ExecuteAttempt against the given machine, and returns the
// channel so a caller can ask whether the engine was spoken to at all.
func (f *affinityFixture) attemptOn(t *testing.T, mac *fakeMac) (*scriptedChannel, error) {
	t.Helper()
	ch := &scriptedChannel{frames: []contracts.EngineFrame{
		engineFrame(1, "engine.ready", map[string]any{
			"selected_protocol": engineProtocol, "engine": map[string]any{"version": "test"},
		}),
		engineFrame(2, "run.terminal", map[string]any{"outcome": "completed"}),
	}}
	f.orch.dialer = &macDialer{ch: ch, mac: mac}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	return ch, f.orch.ExecuteAttempt(ctx, AttemptDescriptor{
		RunID:     contracts.RunID(f.runID),
		AttemptID: contracts.AttemptID(redeliveryID("att")),
		Tenant:    f.tenant,
		Fence:     1,
	})
}

// fillTheMachineThatHoldsTheBytes takes the one slot `full` declared, with a DIFFERENT session, through
// the same HoldMachine the attempt path calls. That is what makes the first attempt park.
func (f *affinityFixture) fillTheMachineThatHoldsTheBytes(t *testing.T) {
	t.Helper()
	other := redeliveryID("ses")
	execSQL(t, f.cs.Pool(), `INSERT INTO sessions (id, project_id) VALUES ($1,$2)`, other, f.tenant.Project)
	if _, err := f.cs.HoldMachine(context.Background(), f.tenant, other, f.full.id); err != nil {
		t.Fatalf("fill the machine that holds the bytes: %v — nothing below is about a park", err)
	}
}

// captureThroughTheMachineThatHoldsIt cuts the boundary snapshot the SHIPPED pause path cuts, through
// the SHIPPED seam: captureBoundarySnapshot with an attemptState whose workspaceOps reach the machine.
//
// It is called by hand rather than by driving a pause boundary because a pause needs an engine that
// offers a checkpoint, and what is being proved here is not the pause protocol. What must NOT be faked
// is which disk the tar comes off, so this goes through captureBoundarySnapshot — the one production
// caller that can reach a machine at all — rather than through SnapshotSink.Capture directly.
func (f *affinityFixture) captureThroughTheMachineThatHoldsIt(t *testing.T) string {
	t.Helper()
	ctx := storage.WithTenant(context.Background(), f.tenant.Project)
	ops := workspaceOpsFor(macChannel{EngineChannel: &scriptedChannel{}, mac: f.full}, f.allocPath)
	if remote, ok := ops.(*RemoteWorkspace); ok {
		if _, err := remote.Open(ctx); err != nil {
			t.Fatalf("open the allocation on the machine that holds it: %v", err)
		}
	}
	snapID, err := f.orch.captureBoundarySnapshot(ctx, &attemptState{
		attempt:      AttemptDescriptor{RunID: contracts.RunID(f.runID), WorkspaceHostPath: f.allocPath},
		tenant:       f.tenant,
		sessionID:    f.sessionID,
		workspaceID:  f.wsID,
		workspaceOps: ops,
	})
	if err != nil {
		t.Fatalf("cut the boundary snapshot on the machine that holds the bytes: %v", err)
	}
	if snapID == "" {
		t.Fatal("the boundary snapshot is empty — nothing was archived, so every claim below about resuming from it is vacuous")
	}
	return snapID
}

// theMachineThatHeldTheBytesIsGone removes that machine's whole managed root. After this the ONLY copy
// of the session's work is the archive in the object store — which is what makes the assertions that
// follow statements about the migration and not about a shared filesystem.
func (f *affinityFixture) theMachineThatHeldTheBytesIsGone(t *testing.T) {
	t.Helper()
	if err := os.RemoveAll(f.full.root); err != nil {
		t.Fatalf("remove the root of the machine that held the bytes: %v", err)
	}
}

func (f *affinityFixture) wake(t *testing.T) {
	t.Helper()
	woken, err := f.cs.WakeRunAwaitingCapacity(context.Background(), f.tenant, f.poolID)
	if err != nil {
		t.Fatalf("WakeRunAwaitingCapacity: %v", err)
	}
	if woken != f.runID {
		t.Fatalf("the wake re-entered %q, want the parked run %q", woken, f.runID)
	}
}

func (f *affinityFixture) runState(t *testing.T) string {
	t.Helper()
	var state string
	if err := f.cs.Pool().QueryRow(storage.WithSystemScope(context.Background()),
		`SELECT state FROM runs WHERE id = $1`, f.runID).Scan(&state); err != nil {
		t.Fatalf("read run state: %v", err)
	}
	return state
}

// currentAllocation is the workspace's current (max-fence) allocation, read the way production reads it.
func (f *affinityFixture) currentAllocation(t *testing.T) coordinator.Allocation {
	t.Helper()
	alloc, err := f.cs.CurrentAllocation(storage.WithSystemScope(context.Background()), f.wsID)
	if err != nil {
		t.Fatalf("read the workspace's current allocation: %v", err)
	}
	return alloc
}

// readOnMachine reads a file from an allocation as it exists on one machine's disk.
func readOnMachine(t *testing.T, mac *fakeMac, allocPath, rel string) (string, bool) {
	t.Helper()
	body, err := os.ReadFile(filepath.Join(workspace.RepoPath(mac.diskPath(allocPath)), rel))
	if os.IsNotExist(err) {
		return "", false
	}
	if err != nil {
		t.Fatalf("read %s on machine %s: %v", rel, mac.id, err)
	}
	return string(body), true
}

// TestARunResumedOnADifferentMachineContinuesFromItsSnapshot — A RUN THAT WAKES ON ANOTHER MAC PICKS UP
// WHERE IT LEFT OFF.
//
// The sequence is §2c's, driven through ExecuteAttempt twice: the first attempt provisions on the Mac
// that holds the session's tree and PARKS because that Mac is at the capacity it declared; the boundary
// snapshot is cut on that Mac; the Mac then goes away; and the wake re-enters the run on the other one.
//
// WHAT THIS FAILS ON BEFORE THE FIX, AND IT IS NOT THE FAILURE THE PLAN PREDICTED. `reuseAllocation`
// short-circuited on the preparation receipt the FIRST attempt recorded for this run, so the head was
// never read, the empty directory the second machine had just created was accepted, and the run
// completed on it. So the pre-fix behaviour satisfies "the run completes" — which is why that is NOT the
// assertion here. The assertion is that the REPOSITORY IS ON THE MACHINE THE RUN IS ON.
func TestARunResumedOnADifferentMachineContinuesFromItsSnapshot(t *testing.T) {
	f := newAffinityFixture(t)
	f.fillTheMachineThatHoldsTheBytes(t)

	// 1. The attempt provisions on the machine that holds the tree, and parks.
	if _, err := f.attemptOn(t, f.full); err != nil {
		t.Fatalf("the first attempt returned %v, want nil — a park that returns an error is a FAILED attempt", err)
	}
	if got := f.runState(t); got != "waiting" {
		t.Fatalf("run state = %q after landing on a machine at its declared capacity, want waiting — nothing parked, so there is no wake to test", got)
	}
	before := f.currentAllocation(t)

	// 2. The boundary snapshot is cut ON THAT MACHINE. Without it there is nothing to move, and the
	//    honest answer would be the refusal the third test covers.
	snapID := f.captureThroughTheMachineThatHoldsIt(t)

	// 3. That machine is gone. From here the archive is the only copy in existence.
	f.theMachineThatHeldTheBytesIsGone(t)

	// 4. The wake is keyed on the POOL, so the run re-enters on the machine that has room — which is not
	//    the one its bytes were on.
	f.wake(t)
	ch, err := f.attemptOn(t, f.free)
	if err != nil {
		t.Fatalf("the woken attempt on the other machine returned %v, want nil", err)
	}
	if got := f.runState(t); got != "completed" {
		t.Fatalf("run state = %q after waking on the other machine, want completed", got)
	}
	if len(ch.sent) == 0 {
		t.Fatal("the woken attempt sent the engine nothing — it never ran, so nothing below is a statement about continuing")
	}

	// THE REPOSITORY IS ON THE MACHINE THE RUN IS ON. This is the assertion the shipped build fails: it
	// accepted an allocation directory that had just been created empty.
	head := exec.Command("git", "-C", workspace.RepoPath(f.free.diskPath(f.allocPath)), "rev-parse", "HEAD")
	head.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null")
	if out, err := head.CombinedOutput(); err != nil {
		t.Fatalf("the machine the run woke on has no repository at %s: %v\n%s — the run continued on an EMPTY workspace, which is the silent half of the affinity ceiling",
			f.free.diskPath(f.allocPath), err, out)
	}

	// AND THE MOVE IS A DURABLE FACT, NOT A SIDE EFFECT ON DISK. A new allocation with a strictly higher
	// fence is what stops the machine the bytes came from recording a snapshot against this workspace
	// ever again (CreateWorkspaceSnapshot is fence-guarded, SAN-006).
	after := f.currentAllocation(t)
	if after.ID == before.ID {
		t.Fatalf("the workspace still points at allocation %s after moving machines — nothing recorded that the bytes are somewhere else, so the next attempt has no way to know either", before.ID)
	}
	if after.Fence <= before.Fence {
		t.Fatalf("the migrated allocation's fence is %d, want more than %d — the machine the tree came from is not fenced out", after.Fence, before.Fence)
	}
	if after.HostPath != f.free.diskPath(f.allocPath) {
		t.Fatalf("the migrated allocation's host path is %q, want the path on the machine that is running it (%q)", after.HostPath, f.free.diskPath(f.allocPath))
	}
	if snapID == "" {
		t.Fatal("no snapshot id")
	}
}

// TestTheResumedTreeCarriesWhatTheSnapshotCaptured — AND NOTHING IS LOST ON THE WAY.
//
// THIS IS THE TEST WITHOUT WHICH THE ONE ABOVE IS DANGEROUS. "The run continues on the new machine" is
// satisfiable by provisioning a fresh clone there: every structural assertion passes, `git rev-parse
// HEAD` answers, and the session's uncommitted work is gone. So the subject here is a file that a clean
// checkout at the same ref CANNOT produce — the edit this session had not committed.
//
// It is the same property SAN-005 states about an idle release and the reason the archive includes .git:
// what a session keeps between messages is not the repository, it is the repository PLUS everything it
// has done to it since.
func TestTheResumedTreeCarriesWhatTheSnapshotCaptured(t *testing.T) {
	f := newAffinityFixture(t)
	f.fillTheMachineThatHoldsTheBytes(t)

	// NON-VACUITY: the uncommitted edit is really on the first machine before anything moves. Without
	// this, an assertion about it arriving somewhere else could pass by never having existed.
	if body, found := readOnMachine(t, f.full, f.allocPath, "in-progress.txt"); !found || body != affinityUncommitted {
		t.Fatalf("the machine that holds the bytes has in-progress.txt = %q (found=%v), want %q — the fixture never created the work this test is about",
			body, found, affinityUncommitted)
	}

	if _, err := f.attemptOn(t, f.full); err != nil {
		t.Fatalf("the first attempt returned %v, want nil", err)
	}
	if got := f.runState(t); got != "waiting" {
		t.Fatalf("run state = %q, want waiting", got)
	}
	f.captureThroughTheMachineThatHoldsIt(t)
	f.theMachineThatHeldTheBytesIsGone(t)

	f.wake(t)
	if _, err := f.attemptOn(t, f.free); err != nil {
		t.Fatalf("the woken attempt on the other machine returned %v, want nil", err)
	}

	body, found := readOnMachine(t, f.free, f.allocPath, "in-progress.txt")
	if !found {
		t.Fatal("the machine the run woke on does not have in-progress.txt — the session's uncommitted work was left on a Mac that is gone, which is the data loss a 'resume' that provisions an empty tree ships silently")
	}
	if body != affinityUncommitted {
		t.Fatalf("in-progress.txt on the machine the run woke on = %q, want %q — the tree arrived but is not the tree that was captured", body, affinityUncommitted)
	}
	// The committed half too, so a restore that dropped the repository and kept the loose file could not
	// pass: the two failures have different causes and one assertion cannot separate them.
	if body, found := readOnMachine(t, f.free, f.allocPath, "committed.txt"); !found || body != "base\n" {
		t.Fatalf("committed.txt on the machine the run woke on = %q (found=%v), want %q", body, found, "base\n")
	}
}

// TestAWorkspaceThatCannotFollowTheSessionRefusesInsteadOfRunningEmpty — THE ANSWER WHEN THERE IS
// NOTHING TO MOVE.
//
// A tree on a machine that is not here, with no archive anywhere, cannot be brought to this one by
// anybody: only the machine holding it can archive it, and it is not on this attempt's connection. The
// platform has exactly two honest answers — refuse, or lose the work — and this fixes which one it
// gives. A re-clone would be the second wearing the first's clothes: it succeeds, it produces a
// repository, and the session's uncommitted work is gone.
//
// IT IS ALSO THE TEST THAT KEEPS THE OTHER TWO HONEST. An implementation that answered a missing tree by
// provisioning a fresh one would pass "the run continues on the new machine" and would be caught here.
func TestAWorkspaceThatCannotFollowTheSessionRefusesInsteadOfRunningEmpty(t *testing.T) {
	f := newAffinityFixture(t)
	f.fillTheMachineThatHoldsTheBytes(t)

	if _, err := f.attemptOn(t, f.full); err != nil {
		t.Fatalf("the first attempt returned %v, want nil", err)
	}
	if got := f.runState(t); got != "waiting" {
		t.Fatalf("run state = %q, want waiting", got)
	}
	// NO SNAPSHOT IS CUT. This is the park case exactly as it happens today: a run parks for capacity
	// without ever reaching a pause boundary, so nothing has archived its tree.
	f.theMachineThatHeldTheBytesIsGone(t)

	f.wake(t)
	ch, err := f.attemptOn(t, f.free)
	if err != nil {
		t.Fatalf("the woken attempt returned %v — a provisioning refusal is reported as a terminal on the run, not as an error out of ExecuteAttempt", err)
	}
	if got := f.runState(t); got != "failed" {
		t.Fatalf("run state = %q after waking on a machine that has neither the tree nor a way to get it, want failed — the run was allowed to proceed against a workspace that does not exist", got)
	}
	if len(ch.sent) != 0 {
		t.Fatalf("the attempt sent %d frame(s) to the engine, want 0 — the model was asked to work in a workspace the platform knew was not there", len(ch.sent))
	}
}
