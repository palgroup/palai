//go:build component

// Workspace idle-release + resume component tests. A session that goes quiet must hand its machine back
// WITHOUT closing, and the next message must bring the workspace back — possibly on a different machine —
// with nothing lost, including work that was never committed.
//
// They run in the artifacts package because a release ARCHIVES a real allocation into the object store and
// a resume RESTORES it back out, so the real object store + Postgres this suite stands up are what the
// paths need. The suite runs its whole package (`-run "${PALAI_SUITE_RUN:-.}"`), so a test added here is a
// test that runs — unlike the postgres suite's execution leg, whose -run allow-list would have to name
// each one.

package artifacts

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/palgroup/palai/apps/control-plane/internal/execution"
	"github.com/palgroup/palai/apps/control-plane/internal/store"
	"github.com/palgroup/palai/packages/coordinator"

	"github.com/palgroup/palai/storage"
)

// idleTestTTL is the TTL every test here sweeps with. It is deliberately a REAL five minutes rather than
// something near zero, because this suite shares one database: a sub-second TTL would make every other
// test's freshly-seeded workspace an idle candidate and let one test delete another's directory. A
// candidate is instead created explicitly, by aging its session's run activity past this (ageSession).
const idleTestTTL = 5 * time.Minute

// seedIdleWorkspace seeds an allocation on disk and puts it in the state an idle sweep looks for: a
// `ready` workspace (the state a finished run's release leaves behind) whose session has had no run
// activity for an hour. It also leaves UNCOMMITTED work in the repo, which is what the requirement is
// really about — no git commit is needed for a release to be lossless.
func (h *artifactsHarness) seedIdleWorkspace(t *testing.T) (project, session, workspaceID, allocationID, hostPath string) {
	t.Helper()
	project, workspaceID, allocationID, hostPath = h.seedAllocationOnDisk(t)
	session = sessionOf(t, h, workspaceID)
	// EVERY CANDIDATE THIS FUNCTION MINTS IS RETIRED WHEN ITS TEST ENDS, AND THAT IS REGISTERED HERE RATHER
	// THAN LEFT TO EACH TEST TO REMEMBER. See retireCandidate: it is registered before the fixture is filled
	// in, so a t.Fatal anywhere below still retires it.
	h.retireCandidate(t, workspaceID)
	// A prior run finished: the workspace is back to ready, still holding its host directory.
	h.exec(t, `UPDATE workspaces SET state='ready' WHERE id=$1`, workspaceID)
	h.ageSession(t, session, time.Hour)
	// Work that exists only on disk: a modified tracked file and an untracked one, neither committed.
	repo := filepath.Join(hostPath, "repo")
	if err := os.WriteFile(filepath.Join(repo, "app.go"), []byte("package main\n\n// edited, never committed\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, "notes.txt"), []byte("uncommitted scratch\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return project, session, workspaceID, allocationID, hostPath
}

// ageSession backdates every run of a session so the sweep's idle clock — max(runs.updated_at), the same
// expression the Sessions screen renders as last_activity_at — reads as `by` in the past.
func (h *artifactsHarness) ageSession(t *testing.T, session string, by time.Duration) {
	t.Helper()
	h.exec(t, `UPDATE runs SET updated_at = clock_timestamp() - make_interval(secs => $2) WHERE session_id=$1`,
		session, by.Seconds())
}

// touchSession brings a session's run activity back to now, which is what a REAL resume implies: the
// workspace came back because a new message arrived, and that message's run moves runs.updated_at.
//
// IT IS NARRATIVE, NOT CLEANUP, AND THAT DISTINCTION IS THE WHOLE OF retireCandidate BELOW. Until
// 2026-08-05 this function was both: five fixtures in this package leaked a `ready` workspace whose
// t.TempDir directory was about to be removed, and what kept a LATER test's sweep from adopting that
// workspace and failing on the missing path was that touchSession had bought five minutes on the idle
// clock. That worked, with a measured margin of roughly 1300x — and it is still a TIMING rather than a
// FACT. A leg that slows past the TTL turns it red and blames a test that touched none of it, which is the
// exact failure the margin was protecting against in the first place.
func (h *artifactsHarness) touchSession(t *testing.T, session string) {
	t.Helper()
	h.exec(t, `UPDATE runs SET updated_at = clock_timestamp() WHERE session_id=$1`, session)
}

// retireCandidate takes a workspace out of the idle sweep's candidate set PERMANENTLY, by recording the one
// fact that stops being true when a test ends: `IdleWorkspacesForRelease` requires a NON-EMPTY
// `a.host_path` on the workspace's current allocation, and the directory these fixtures point at is a
// t.TempDir that Go
// removes on the way out. An allocation row still naming a path that no longer exists is the lie; blanking
// it is not fixture bookkeeping but the truth, and it is the same thing the release path itself records
// when it hands a machine back.
//
// WHY A FACT RATHER THAN THE CLOCK. Five fixtures were clock-protected before this and only five of the ten
// were fact-protected, which is a coin-flip a reader has to re-derive per test. A blanked host_path cannot
// elapse: no TTL, no load, no ordering between packages. And two of the five had a second weakness the
// clock hid — idle_release_billing_component_test.go's hand-back was the LAST STATEMENT OF THE TEST BODY
// rather than a t.Cleanup, so any Fatal above it armed the landmine it existed to disarm.
//
// It is registered from seedIdleWorkspace so that a test added to this package inherits it without knowing
// it exists. Cleanups run LIFO, so this fires BEFORE the t.TempDir removal registered inside
// seedAllocationOnDisk; the order does not matter to a later sweep, but it does mean the row is honest at
// every instant rather than for a window.
func (h *artifactsHarness) retireCandidate(t *testing.T, workspaceID string) {
	t.Helper()
	t.Cleanup(func() {
		h.exec(t, `UPDATE workspace_allocations SET host_path='' WHERE workspace_id=$1`, workspaceID)
	})
}

// workspaceState reads a workspace's lifecycle state directly, so an assertion about it never goes
// through the code under test.
func workspaceState(t *testing.T, h *artifactsHarness, workspaceID string) string {
	t.Helper()
	var state string
	if err := h.pool.QueryRow(storage.WithSystemScope(context.Background()), `SELECT state FROM workspaces WHERE id=$1`, workspaceID).Scan(&state); err != nil {
		t.Fatalf("read workspace state: %v", err)
	}
	return state
}

// recordingAccounts is a SessionAccounts that records which sessions were acquired and released rather
// than touching a real uid — the privileged half needs root, and the mapping is the part with behaviour.
type recordingAccounts struct {
	// uid/gid is the identity Acquire answers with. Zero means THIS PROCESS'S OWN, and that default is a
	// statement about the machine the test runs on rather than about production.
	//
	// ‼️ WHY NOT 701, WHICH IS WHAT macagent.AccountUID(1) REALLY RETURNS. Since A.5 T3 the resume path
	// does not merely record the account, it HANDS THE RESTORED TREE OVER to it (execution.HandTreeTo) —
	// and chowning to another uid needs root, measured on this platform 2026-08-05. A fake answering 701
	// would make every run of this suite assert the FAILURE branch, on every unprivileged machine, which
	// is every machine. So the default names the one uid an unprivileged process may give a tree to, the
	// hand-over really happens, and the round trip below is measured over a tree that really changed
	// hands. The production arithmetic (700+slot, gid 20) is asserted where it belongs and where no
	// privilege is needed to see it: execution.TestAnAcquiredAccountCarriesTheUidTheDaemonWouldHaveCreated.
	uid, gid int
	acquired []string
	released []string
}

func (a *recordingAccounts) account() execution.SessionAccount {
	uid, gid := a.uid, a.gid
	if uid == 0 {
		uid, gid = os.Getuid(), os.Getgid()
	}
	return execution.SessionAccount{Name: "palai-s01", UID: uid, GID: gid}
}

func (a *recordingAccounts) Acquire(_ context.Context, sessionID string) (execution.SessionAccount, error) {
	a.acquired = append(a.acquired, sessionID)
	return a.account(), nil
}

func (a *recordingAccounts) Lookup(sessionID string) (execution.SessionAccount, bool) {
	for _, s := range a.acquired {
		if s == sessionID {
			return a.account(), true
		}
	}
	return execution.SessionAccount{}, false
}

func (a *recordingAccounts) Release(_ context.Context, sessionID string) error {
	a.released = append(a.released, sessionID)
	return nil
}

// TestAnIdleWorkspaceIsArchivedAndItsMachineHandedBack is the headline behaviour: a session quiet past the
// TTL loses its ALLOCATION — bytes archived, host directory reclaimed, macOS account slot handed back —
// while the SESSION itself stays open. The workspace lands in `paused`, which is the state the next
// message restores from, NOT `destroyed`, which nothing can come back from.
func TestAnIdleWorkspaceIsArchivedAndItsMachineHandedBack(t *testing.T) {
	h := openArtifactsHarness(t)
	ctx := context.Background()
	_, session, workspaceID, _, hostPath := h.seedIdleWorkspace(t)
	accounts := &recordingAccounts{}

	releaser := execution.NewIdleReleaser(h.repo.Spine(), execution.NewSnapshotSink(h.s3, h.repo.Spine()), idleTestTTL).
		WithSessionAccounts(accounts)
	if _, err := releaser.Sweep(ctx); err != nil {
		t.Fatalf("Sweep() error = %v", err)
	}

	// The machine is back: the allocation's directory is gone.
	if _, err := os.Stat(hostPath); !os.IsNotExist(err) {
		t.Fatalf("allocation dir survived the idle release (stat err=%v) — the machine is still held", err)
	}
	// The workspace is paused, not destroyed: paused is resumable, destroyed is terminal.
	if state := workspaceState(t, h, workspaceID); state != "paused" {
		t.Fatalf("workspace state = %q, want paused", state)
	}
	// THE SESSION DID NOT CLOSE. This is the distinction the whole change rests on — the occupancy is the
	// allocation, the identity is the session, and only the first is reclaimable while the thread lives.
	var sessionState string
	if err := h.pool.QueryRow(storage.WithSystemScope(ctx), `SELECT state FROM sessions WHERE id=$1`, session).Scan(&sessionState); err != nil {
		t.Fatalf("read session state: %v", err)
	}
	if sessionState != "active" {
		t.Fatalf("session state = %q, want active — an idle release must not end the conversation", sessionState)
	}
	// The uid slot was handed back, keyed by SESSION (the key SessionAccounts is defined on).
	if len(accounts.released) != 1 || accounts.released[0] != session {
		t.Fatalf("accounts.released = %v, want exactly [%s]", accounts.released, session)
	}
	// An archive exists to come back from. Without this the release above is data loss.
	if _, found, err := h.repo.Spine().LatestRestorableWorkspaceSnapshot(ctx, coordinator.Tenant{Project: projectOf(t, h, workspaceID)}, workspaceID); err != nil || !found {
		t.Fatalf("LatestRestorableWorkspaceSnapshot() = found %v, err %v; want a byte-archived snapshot", found, err)
	}
}

// TestAResumedWorkspaceCarriesUncommittedWorkBack is the requirement in its own terms: "run kapanırken de
// tüm değişikliklerin commitlenmesi lazım veya başka bir şey olması lazım ki veri kaybı olmasın". The
// "başka bir şey" is this — the archive is a raw filesystem capture, so uncommitted and untracked work
// come back without anyone having committed anything.
//
// It asserts the ROUND TRIP, not the archive: release, then resume, then read the files off the NEW
// allocation's disk. An assertion on the snapshot row alone would pass while the restore was broken.
func TestAResumedWorkspaceCarriesUncommittedWorkBack(t *testing.T) {
	h := openArtifactsHarness(t)
	ctx := context.Background()
	project, session, workspaceID, oldAlloc, hostPath := h.seedIdleWorkspace(t)
	tenant := coordinator.Tenant{Project: project}
	accounts := &recordingAccounts{}

	releaser := execution.NewIdleReleaser(h.repo.Spine(), execution.NewSnapshotSink(h.s3, h.repo.Spine()), idleTestTTL).
		WithSessionAccounts(accounts)
	if _, err := releaser.Sweep(ctx); err != nil {
		t.Fatalf("Sweep() error = %v", err)
	}
	if _, err := os.Stat(hostPath); !os.IsNotExist(err) {
		t.Fatal("the release did not reclaim the directory, so the resume below would prove nothing")
	}

	// The next message in the thread. The allocation id and path are minted by the caller, exactly as
	// planRootWorkspace mints them before the dial.
	newRoot := t.TempDir()
	if r, err := filepath.EvalSymlinks(newRoot); err == nil {
		newRoot = r
	}
	newAlloc := newID("alloc")
	newPath := filepath.Join(newRoot, newAlloc)
	alloc, err := execution.ResumeReleasedWorkspace(ctx, h.repo.Spine(), execution.NewSnapshotSink(h.s3, h.repo.Spine()), accounts, tenant,
		execution.ResumeInput{
			WorkspaceID:  workspaceID,
			SessionID:    session,
			AllocationID: newAlloc,
			HostPath:     newPath,
		})
	if err != nil {
		t.Fatalf("ResumeReleasedWorkspace() error = %v", err)
	}
	// The message that woke it counts as activity, exactly as a real run would — see touchSession.
	h.touchSession(t, session)

	// A NEW allocation at a strictly higher fence: the old host's writes are fenced out at the DB.
	if alloc.ID == oldAlloc {
		t.Fatalf("resume reused allocation %s; a resume must mint a new one", oldAlloc)
	}
	if alloc.Fence <= 1 {
		t.Fatalf("resumed allocation fence = %d, want strictly greater than the released allocation's 1", alloc.Fence)
	}
	if state := workspaceState(t, h, workspaceID); state != "ready" {
		t.Fatalf("workspace state after resume = %q, want ready", state)
	}

	// THE WORK IS BACK — the committed file, the uncommitted edit to it, and the untracked file.
	edited, err := os.ReadFile(filepath.Join(newPath, "repo", "app.go"))
	if err != nil {
		t.Fatalf("read restored app.go: %v", err)
	}
	if string(edited) != "package main\n\n// edited, never committed\n" {
		t.Fatalf("restored app.go = %q, want the UNCOMMITTED edit — the session lost work", edited)
	}
	untracked, err := os.ReadFile(filepath.Join(newPath, "repo", "notes.txt"))
	if err != nil {
		t.Fatalf("read restored notes.txt: %v — an untracked file was lost", err)
	}
	if string(untracked) != "uncommitted scratch\n" {
		t.Fatalf("restored notes.txt = %q, want the untracked scratch file", untracked)
	}
	// .git came back too, so the resumed session can still commit and push what it was working on.
	if _, err := os.Stat(filepath.Join(newPath, "repo", ".git", "HEAD")); err != nil {
		t.Fatalf("restored repo has no .git/HEAD: %v — the thread cannot commit what it resumes", err)
	}
	// The secret staging area is NOT restored, because it was never archived (SAN-005).
	if _, err := os.Stat(filepath.Join(newPath, "secrets", "token")); !os.IsNotExist(err) {
		t.Fatalf("a secret survived the archive/restore round trip (stat err=%v)", err)
	}
	// The uid was re-acquired for the SAME session — a resumed thread runs as its own user again.
	if len(accounts.acquired) != 1 || accounts.acquired[0] != session {
		t.Fatalf("accounts.acquired = %v, want exactly [%s]", accounts.acquired, session)
	}
}

// TestASweepThatFailsOneCandidateStillReleasesTheOthers pins Sweep's own claim ("A failure on one
// candidate is logged and the pass continues to the next") against a live-looking symptom that read the
// other way: control-plane.log showed a run of "sweep failed after releasing 0" lines for two
// permanently-oversized workspaces and, without cross-referencing the per-candidate lines above each one,
// that looks exactly like "one failure stops the whole sweep". It does not — the SAME log's earlier line
// was "sweep failed after releasing 30" in the pass that first hit both stuck candidates, and this test is
// the regression guard for that: a candidate whose capture cannot succeed must not stop a DIFFERENT
// candidate later in the same pass from being handed back.
func TestASweepThatFailsOneCandidateStillReleasesTheOthers(t *testing.T) {
	h := openArtifactsHarness(t)
	ctx := context.Background()

	_, _, wsA, _, hostA := h.seedIdleWorkspace(t)
	_, _, wsB, _, hostB := h.seedIdleWorkspace(t)

	// IdleWorkspacesForRelease orders by w.id, a total order over a random id (storage/queries/workspaces.sql)
	// — NOT insertion order. Which of these two the sweep reaches first is decided here, or this test could
	// pass by accident: if the failing one happened to sort LAST, even a sweep that stopped at the first
	// failure would still show released=1, and the bug this guards against would go uncaught.
	badWS, badHost, goodWS, goodHost := wsA, hostA, wsB, hostB
	if wsB < wsA {
		badWS, badHost, goodWS, goodHost = wsB, hostB, wsA, hostA
	}

	// The bad candidate is LEFT `ready` on purpose (that is what this test asserts), and its t.TempDir
	// directory dies with this test — exactly the shared-database trap TestIdleReleaseRefusesWithoutASnapshotSink
	// already names: the NEXT test's sweep would otherwise re-adopt this workspace as an idle candidate, find
	// its directory gone, and fail at a test that touched none of this. seedIdleWorkspace's retireCandidate
	// closes it on the allocation's host_path, so `ready` here is a claim about THIS sweep and not a debt the
	// idle clock has to keep paying.

	// The bad candidate's directory is gone before the sweep claims it, so its capture fails outright and
	// deterministically — no size bound or injected remover needed to make exactly one candidate fail.
	if err := os.RemoveAll(badHost); err != nil {
		t.Fatal(err)
	}

	releaser := execution.NewIdleReleaser(h.repo.Spine(), execution.NewSnapshotSink(h.s3, h.repo.Spine()), idleTestTTL)
	released, err := releaser.Sweep(ctx)
	if err == nil {
		t.Fatal("Sweep() with one unreadable candidate returned a nil error, want the capture failure surfaced")
	}
	if released != 1 {
		t.Fatalf("Sweep() released = %d, want 1 — a candidate ordered after a failing one must still be released", released)
	}
	if state := workspaceState(t, h, goodWS); state != "paused" {
		t.Fatalf("workspace after the failing candidate: state = %q, want paused", state)
	}
	if _, err := os.Stat(goodHost); !os.IsNotExist(err) {
		t.Fatalf("workspace after the failing candidate: directory survived (stat err=%v) — the sweep never reached it", err)
	}
	// The failing candidate's own claim was returned (snapshotting -> ready), not left stuck mid-snapshot.
	if state := workspaceState(t, h, badWS); state != "ready" {
		t.Fatalf("failing workspace state = %q, want ready (claim returned)", state)
	}
}

// TestABusyWorkspaceIsNotReleased pins the three independent skip reasons. Each is a separate fact and
// none implies the others, which is why the query tests them separately and so does this: a workspace
// that is not `ready`, one holding an active writer lease, and one whose session has a live response.run
// job must all survive a sweep with their bytes intact.
func TestABusyWorkspaceIsNotReleased(t *testing.T) {
	h := openArtifactsHarness(t)
	ctx := context.Background()
	releaser := execution.NewIdleReleaser(h.repo.Spine(), execution.NewSnapshotSink(h.s3, h.repo.Spine()), idleTestTTL)

	t.Run("a leased workspace keeps its machine", func(t *testing.T) {
		_, _, workspaceID, _, hostPath := h.seedIdleWorkspace(t)
		h.exec(t, `UPDATE workspaces SET state='leased' WHERE id=$1`, workspaceID)
		if _, err := releaser.Sweep(ctx); err != nil {
			t.Fatalf("Sweep() error = %v", err)
		}
		if _, err := os.Stat(hostPath); err != nil {
			t.Fatalf("a leased workspace lost its directory: %v", err)
		}
		if state := workspaceState(t, h, workspaceID); state != "leased" {
			t.Fatalf("workspace state = %q, want leased (untouched)", state)
		}
	})

	t.Run("a ready workspace with a dangling active lease keeps its machine", func(t *testing.T) {
		project, _, workspaceID, allocationID, hostPath := h.seedIdleWorkspace(t)
		h.exec(t, `INSERT INTO workspace_leases (id, workspace_id, allocation_id, project_id, run_id, state, fence)
			VALUES ($1,$2,$3,$4,$5,'active',1)`,
			newID("lease"), workspaceID, allocationID, project, runOf(t, h, workspaceID))
		if _, err := releaser.Sweep(ctx); err != nil {
			t.Fatalf("Sweep() error = %v", err)
		}
		if _, err := os.Stat(hostPath); err != nil {
			t.Fatalf("a workspace with an active writer lease lost its directory: %v", err)
		}
	})

	t.Run("a session with a live response.run job keeps its machine", func(t *testing.T) {
		project, session, workspaceID, _, hostPath := h.seedIdleWorkspace(t)
		// The five-minute lease window is this sub-test's SUBJECT, not its cleanup: the query proves liveness
		// on the DATABASE clock (`j.lease_expires_at > clock_timestamp()`), so a live job has to be one. What
		// it is NOT any more is what keeps this workspace from leaking into a later test's sweep — until
		// 2026-08-05 it was both, and this was the one leg in the file with no hand-back at all because the
		// window happened to cover the rest of the run. retireCandidate owns that half now.
		h.exec(t, `INSERT INTO durable_jobs (id, project_id, kind, payload, status, lease_expires_at)
			VALUES ($1,$2,'response.run',$3,'running', clock_timestamp() + interval '5 minutes')`,
			newID("job"), project, `{"run_id":"`+runOf(t, h, workspaceID)+`"}`)
		if _, err := releaser.Sweep(ctx); err != nil {
			t.Fatalf("Sweep() error = %v", err)
		}
		if _, err := os.Stat(hostPath); err != nil {
			t.Fatalf("a session still driving a run lost its directory: %v — session %s", err, session)
		}
	})
}

// TestIdleReleaseRefusesWithoutASnapshotSink pins the correctness gate. A releaser with no archive path
// would delete a session's uncommitted work with nothing to restore from, so a nil sink must make every
// sweep refuse rather than delete — the composition root's `artStore == nil` check is the first line of
// that defence and this is the second.
func TestIdleReleaseRefusesWithoutASnapshotSink(t *testing.T) {
	h := openArtifactsHarness(t)
	// This test deliberately leaves a real candidate behind — a refusal that released nothing is the point —
	// and its directory dies with t.TempDir. seedIdleWorkspace's retireCandidate is what stops a later sweep
	// in this shared database from trying to archive a path that is gone.
	_, _, _, _, hostPath := h.seedIdleWorkspace(t)

	releaser := execution.NewIdleReleaser(h.repo.Spine(), nil, idleTestTTL)
	if _, err := releaser.Sweep(context.Background()); err == nil {
		t.Fatal("Sweep() with no snapshot sink returned nil, want an explicit refusal")
	}
	if _, err := os.Stat(hostPath); err != nil {
		t.Fatalf("a sink-less sweep still removed the directory: %v", err)
	}
}

// TestResumeRefusesWhenTheArchiveIsGone pins the other end of the same argument: a paused workspace whose
// archive cannot be found FAILS rather than coming back as an empty tree. Re-cloning the binding would
// present a session's lost work as a clean checkout, which is the failure this refusal exists to prevent.
func TestResumeRefusesWhenTheArchiveIsGone(t *testing.T) {
	h := openArtifactsHarness(t)
	ctx := context.Background()
	project, session, workspaceID, _, _ := h.seedIdleWorkspace(t)
	// Pause it WITHOUT ever capturing a snapshot — the state a release would leave if its archive were
	// later reaped out from under it.
	h.exec(t, `UPDATE workspaces SET state='paused' WHERE id=$1`, workspaceID)

	dir := filepath.Join(t.TempDir(), "alloc")
	_, err := execution.ResumeReleasedWorkspace(ctx, h.repo.Spine(), execution.NewSnapshotSink(h.s3, h.repo.Spine()), nil,
		coordinator.Tenant{Project: project},
		execution.ResumeInput{WorkspaceID: workspaceID, SessionID: session, AllocationID: newID("alloc"), HostPath: dir})
	if err == nil {
		t.Fatal("ResumeReleasedWorkspace() with no archive returned nil, want an explicit failure")
	}
	if !errors.Is(err, execution.ErrRecoveryImpossible) {
		t.Fatalf("ResumeReleasedWorkspace() error = %v, want ErrRecoveryImpossible", err)
	}
}

// projectOf reads the project a workspace belongs to, for the tenant an assertion needs.
func projectOf(t *testing.T, h *artifactsHarness, workspaceID string) string {
	t.Helper()
	var project string
	if err := h.pool.QueryRow(storage.WithSystemScope(context.Background()), `SELECT project_id FROM workspaces WHERE id=$1`, workspaceID).Scan(&project); err != nil {
		t.Fatalf("read workspace project: %v", err)
	}
	return project
}

// runOf reads a run of the workspace's session, for the rows a liveness fixture needs to FK against.
func runOf(t *testing.T, h *artifactsHarness, workspaceID string) string {
	t.Helper()
	var runID string
	if err := h.pool.QueryRow(storage.WithSystemScope(context.Background()),
		`SELECT r.id FROM runs r JOIN workspaces w ON w.session_id = r.session_id WHERE w.id=$1 LIMIT 1`, workspaceID).Scan(&runID); err != nil {
		t.Fatalf("read workspace run: %v", err)
	}
	return runID
}

// TestAResumeWhoseTreeCannotBeHandedOverFailsInsteadOfReadying is A.5 Task 3's fail-closed leg on the
// real resume path, and it is the leg that proves execution.HandTreeTo is actually CALLED here rather
// than merely existing. It drives the shipped ResumeReleasedWorkspace against a machine whose session
// account this process cannot become — uid 701, the uid palai-agentd really gives slot 1 — and asserts
// that the workspace does NOT reach `ready`.
//
// WHY THE REFUSAL IS THE PRODUCT BEHAVIOUR AND NOT A TEST ARTEFACT. A workspace marked ready whose bytes
// still belong to the control plane is a run that will fail somewhere inside a build, with a permission
// error attributed to the build. The whole finding this task came from — docs/measurements/faz-a5-residue.md
// §2 — was a machine that reported a boundary it did not have, so a hand-over that could not happen must
// stop the resume rather than be logged past.
//
// A PRIVILEGED RUN OF THIS SUITE TAKES THE OTHER BRANCH, and it is written out rather than skipped: as
// root the chown succeeds and the resume must complete, with the restored bytes owned by 701.
func TestAResumeWhoseTreeCannotBeHandedOverFailsInsteadOfReadying(t *testing.T) {
	h := openArtifactsHarness(t)
	ctx := context.Background()
	project, session, workspaceID, _, _ := h.seedIdleWorkspace(t)
	tenant := coordinator.Tenant{Project: project}
	// 701 is macagent.AccountUID(1) — a real session uid, and one no unprivileged process may chown to.
	accounts := &recordingAccounts{uid: 701, gid: 20}

	releaser := execution.NewIdleReleaser(h.repo.Spine(), execution.NewSnapshotSink(h.s3, h.repo.Spine()), idleTestTTL).
		WithSessionAccounts(accounts)
	if _, err := releaser.Sweep(ctx); err != nil {
		t.Fatalf("Sweep() error = %v", err)
	}

	newRoot := t.TempDir()
	if r, err := filepath.EvalSymlinks(newRoot); err == nil {
		newRoot = r
	}
	newAlloc := newID("alloc")
	_, err := execution.ResumeReleasedWorkspace(ctx, h.repo.Spine(), execution.NewSnapshotSink(h.s3, h.repo.Spine()), accounts, tenant,
		execution.ResumeInput{
			WorkspaceID:  workspaceID,
			SessionID:    session,
			AllocationID: newAlloc,
			HostPath:     filepath.Join(newRoot, newAlloc),
		})

	if os.Geteuid() == 0 {
		if err != nil {
			t.Fatalf("ResumeReleasedWorkspace() as root: %v — the hand-over is possible here and must succeed", err)
		}
		return
	}

	if !errors.Is(err, execution.ErrCannotHandOverTree) {
		t.Fatalf("ResumeReleasedWorkspace() = %v, want ErrCannotHandOverTree — this process cannot give the "+
			"restored tree to uid 701, so a resume that reported success would hand the session a workspace "+
			"it cannot write and a boundary that exists only in the log", err)
	}
	// AND THE WORKSPACE DID NOT GO READY. The error alone is not the property: a caller that logged and
	// carried on would find a `ready` workspace and dial against it.
	if state := workspaceState(t, h, workspaceID); state == "ready" {
		t.Fatalf("workspace state = %q after a refused hand-over: the run would dial against a tree owned "+
			"by the control plane", state)
	}
}

// TestAFinishedSessionIsREADABLEAfterItsMachineIsGone is the device plan's T6 leg that says a finished
// session is answerable "without shell access to the device".
//
// ‼️ THE FAILURE IT REFUSES IS NAMED IN THE PLAN: a view that reads a file on the machine. That file is
// gone by design here — the idle release deletes the allocation directory and hands the uid slot back —
// and on a `user`-mode Mac it is worse than gone, because the machine belongs to a person. The record was
// never on the machine, and this asserts it: the same journal read that serves
// GET /v1/sessions/{id}/events answers AFTER the deletion that a support question always follows.
//
// The order is what makes it a proof rather than a coincidence: the event is written through the
// PRODUCTION journal writer while the machine still exists, the sweep then really deletes, and both facts
// are asserted before the read. A test that read the journal without proving the deletion happened would
// pass on a machine that was never released.
func TestAFinishedSessionIsREADABLEAfterItsMachineIsGone(t *testing.T) {
	h := openArtifactsHarness(t)
	ctx := context.Background()
	project, session, _, _, hostPath := h.seedIdleWorkspace(t)

	// COALESCE, because a run's response_id is genuinely NULLABLE: appendEvent stores it through
	// nullableText, and a run seeded without a response is a shape production writes rather than a fixture
	// shortcut. Scanning it as a bare string made this test fail on the fixture instead of on the property.
	var runID, responseID string
	if err := h.pool.QueryRow(storage.WithSystemScope(ctx),
		`SELECT id, COALESCE(response_id, '') FROM runs WHERE session_id=$1 ORDER BY id LIMIT 1`,
		session).Scan(&runID, &responseID); err != nil {
		t.Fatalf("read the session's run: %v", err)
	}

	tenant := coordinator.Tenant{Project: project}
	const said = `{"delta":"the machine answered this before it was handed back"}`
	if err := h.repo.Spine().JournalRunEvent(ctx, tenant, session, responseID, runID,
		"response.output_text.delta", []byte(said)); err != nil {
		t.Fatalf("journal a run event: %v", err)
	}

	accounts := &recordingAccounts{}
	releaser := execution.NewIdleReleaser(h.repo.Spine(), execution.NewSnapshotSink(h.s3, h.repo.Spine()), idleTestTTL).
		WithSessionAccounts(accounts)
	if _, err := releaser.Sweep(ctx); err != nil {
		t.Fatalf("Sweep() error = %v", err)
	}

	// The machine really is gone. Without these two the read below proves nothing.
	if _, err := os.Stat(hostPath); !os.IsNotExist(err) {
		t.Fatalf("the allocation directory survived the sweep (stat err=%v) — this test would then be reading "+
			"a session whose machine was never reclaimed", err)
	}
	if len(accounts.released) != 1 || accounts.released[0] != session {
		t.Fatalf("accounts.released = %v, want exactly [%s] — the uid slot was not handed back", accounts.released, session)
	}

	events, err := store.NewJournal(h.pool).After(ctx, project, session, 0, 100)
	if err != nil {
		t.Fatalf("the journal behind GET /v1/sessions/{id}/events refused a session whose machine is gone: %v", err)
	}
	var found bool
	for _, e := range events {
		// The event's PAYLOAD, not merely its type: a journal that kept the row and lost the text would
		// answer "the session said something" and not what it said.
		body, err := json.Marshal(e.Data)
		if err != nil {
			t.Fatalf("marshal event data: %v", err)
		}
		if strings.Contains(string(body), "handed back") {
			found = true
		}
	}
	if !found {
		t.Fatalf("the session's own output is not readable after its machine was reclaimed (%d events) — "+
			"\"what did that session do\" would have to be answered from a disk that no longer exists", len(events))
	}
}
