//go:build fault

// Package subagents holds the fault-injection proof for ChildRun cancel propagation
// (spec §25.18-19, SUB-005). It runs only under `make test-fault TEST=subagents` (CASE is
// honored too), which starts a throwaway PostgreSQL container and exports
// PALAI_FAULT_POSTGRES_URL. The build tag keeps it out of the credential-free, Docker-free
// unit tier.
//
// It exercises packages/coordinator against real Postgres: a parent run with a live ChildRun is
// canceled, the cancel propagates to the child, and the terminal accounting stays consistent —
// exactly one terminal per run, and a late child completion (its in-flight attempt finishing
// after the cancel) is dropped at the database rather than overwriting the canceled row.
package subagents

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/palgroup/palai/packages/coordinator"
	statemachines "github.com/palgroup/palai/packages/state-machines"
)

func faultURL(t *testing.T) string {
	t.Helper()
	url := os.Getenv("PALAI_FAULT_POSTGRES_URL")
	if url == "" {
		t.Skip("PALAI_FAULT_POSTGRES_URL is required; run make test-fault TEST=subagents")
	}
	return url
}

func openStore(t *testing.T) *coordinator.Store {
	t.Helper()
	store, err := coordinator.Open(context.Background(), faultURL(t))
	if err != nil {
		t.Fatalf("coordinator.Open() error = %v", err)
	}
	t.Cleanup(store.Close)
	if err := store.Migrate(context.Background()); err != nil {
		t.Fatalf("Migrate() error = %v", err)
	}
	return store
}

func newID(prefix string) string {
	var raw [8]byte
	_, _ = rand.Read(raw[:])
	return prefix + "_" + hex.EncodeToString(raw[:])
}

func exec(t *testing.T, pool *pgxpool.Pool, sql string, args ...any) {
	t.Helper()
	if _, err := pool.Exec(context.Background(), sql, args...); err != nil {
		t.Fatalf("exec %q error = %v", sql, err)
	}
}

func count(t *testing.T, pool *pgxpool.Pool, sql string, args ...any) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(context.Background(), sql, args...).Scan(&n); err != nil {
		t.Fatalf("count %q error = %v", sql, err)
	}
	return n
}

func runState(t *testing.T, pool *pgxpool.Pool, runID string) string {
	t.Helper()
	var state string
	if err := pool.QueryRow(context.Background(), `SELECT state FROM runs WHERE id=$1`, runID).Scan(&state); err != nil {
		t.Fatalf("read run state %s error = %v", runID, err)
	}
	return state
}

// seedParentWithChild creates org -> project -> session -> parent (root) run + response and one
// live ChildRun + response, both running. Returns the tenant and the run/response ids.
func seedParentWithChild(t *testing.T, pool *pgxpool.Pool) (tenant coordinator.Tenant, parentRun, parentResp, childRun, childResp string) {
	t.Helper()
	return seedParent(t, pool, "running", "running", false)
}

// seedParent creates org -> project -> session -> a root run + response and one ChildRun + response in
// the given run states. detached marks the child's delegation.spec so the E10 T8 wake/rebind paths see
// it. The session id lives on the parent's session; the child is created strictly AFTER the parent, so
// its created_at is later (the detached-window addressing relies on latest-live-run).
func seedParent(t *testing.T, pool *pgxpool.Pool, parentState, childState string, detached bool) (tenant coordinator.Tenant, parentRun, parentResp, childRun, childResp string) {
	t.Helper()
	tenant = coordinator.Tenant{Organization: newID("org"), Project: newID("prj")}
	sessionID := newID("ses")
	parentRun, parentResp = newID("run"), newID("resp")
	childRun, childResp = newID("run"), newID("resp")
	exec(t, pool, `INSERT INTO organizations (id) VALUES ($1)`, tenant.Organization)
	exec(t, pool, `INSERT INTO projects (id, organization_id) VALUES ($1, $2)`, tenant.Project, tenant.Organization)
	exec(t, pool, `INSERT INTO sessions (id, organization_id, project_id) VALUES ($1, $2, $3)`, sessionID, tenant.Organization, tenant.Project)
	exec(t, pool, `INSERT INTO responses (id, organization_id, project_id, session_id, state, input) VALUES ($1,$2,$3,$4,'in_progress','{}')`,
		parentResp, tenant.Organization, tenant.Project, sessionID)
	exec(t, pool, `INSERT INTO runs (id, organization_id, project_id, session_id, response_id, state) VALUES ($1,$2,$3,$4,$5,$6)`,
		parentRun, tenant.Organization, tenant.Project, sessionID, parentResp, parentState)
	// The ChildRun shares the session (parent_run_id set, so it does not consume the root slot). Its
	// delegation.spec carries the child_request_id + detached flag exactly as dispatchChild writes them.
	deleg := fmt.Sprintf(`{"spec":{"child_request_id":%q,"detached":%t}}`, "creq_"+childRun, detached)
	exec(t, pool, `INSERT INTO responses (id, organization_id, project_id, session_id, state, input) VALUES ($1,$2,$3,$4,'in_progress','{}')`,
		childResp, tenant.Organization, tenant.Project, sessionID)
	exec(t, pool, `INSERT INTO runs (id, organization_id, project_id, session_id, response_id, state, parent_run_id, depth, delegation) VALUES ($1,$2,$3,$4,$5,$6,$7,1,$8)`,
		childRun, tenant.Organization, tenant.Project, sessionID, childResp, childState, parentRun, deleg)
	return tenant, parentRun, parentResp, childRun, childResp
}

// sessionOf reads a run's session id.
func sessionOf(t *testing.T, pool *pgxpool.Pool, runID string) string {
	t.Helper()
	var s string
	if err := pool.QueryRow(context.Background(), `SELECT session_id FROM runs WHERE id=$1`, runID).Scan(&s); err != nil {
		t.Fatalf("read session of %s error = %v", runID, err)
	}
	return s
}

// TestChildTargetedSendMessageAddressesDetachedChild proves DET-002 child-addressing (spec §25.18-19,
// E10 T8): during a detached child's window the parent is WAITING and the child is the session's live
// run, so a send_message accepted on the parent's session binds to the CHILD's run — the existing
// send_message spine + delivered-message pump reach the child with no new command kind. The exactly-
// once fold is E10 T2's delivered_messages, which the pump applies run-generically to the child.
func TestChildTargetedSendMessageAddressesDetachedChild(t *testing.T) {
	store := openStore(t)
	pool := store.Pool()
	ctx := context.Background()
	// Parent released (waiting), child running detached.
	tenant, _, _, childRun, _ := seedParent(t, pool, "waiting", "running", true)
	sessionID := sessionOf(t, pool, childRun)

	payload, _ := json.Marshal(map[string]any{"message": "keep going"})
	cmd, err := store.AcceptCommand(ctx, tenant, sessionID, coordinator.CommandInput{
		CommandID: newID("cmd"), Kind: "send_message", Delivery: "queue", Payload: payload,
	})
	if err != nil {
		t.Fatalf("AcceptCommand error = %v", err)
	}
	if cmd.State != "queued" {
		t.Fatalf("send_message state = %q, want queued (the live child accepts it)", cmd.State)
	}
	// The command bound to the CHILD run, so the child's boundary pump delivers it.
	pending, err := store.PendingBoundaryCommands(ctx, tenant, childRun)
	if err != nil {
		t.Fatalf("PendingBoundaryCommands error = %v", err)
	}
	if len(pending) != 1 || pending[0].ID != cmd.ID {
		t.Fatalf("child pending boundary commands = %+v, want the send_message bound to the child run", pending)
	}
}

// TestWakeDetachedParentIsSingleWinner proves the exactly-once wake primitive (DET-001): a released
// (waiting) parent whose child is terminal is re-entered into running and given ONE response.run job by
// the first wake; a second wake (a redelivered child terminal, or the parent's own self-wake racing the
// child's) sees a running parent and no-ops — so the parent resumes exactly once.
func TestWakeDetachedParentIsSingleWinner(t *testing.T) {
	store := openStore(t)
	pool := store.Pool()
	ctx := context.Background()
	tenant, parentRun, _, _, _ := seedParent(t, pool, "waiting", "completed", true)

	woken, err := store.WakeDetachedParent(ctx, tenant, parentRun)
	if err != nil || !woken {
		t.Fatalf("first WakeDetachedParent = (%v, %v), want (true, nil)", woken, err)
	}
	if s := runState(t, pool, parentRun); s != "running" {
		t.Fatalf("parent state after wake = %q, want running", s)
	}
	if n := count(t, pool, `SELECT count(*) FROM durable_jobs WHERE kind='response.run' AND payload->>'run_id'=$1`, parentRun); n != 1 {
		t.Fatalf("enqueued parent jobs after wake = %d, want 1", n)
	}
	// A second wake (double terminal / self-wake race) is a no-op: no second job, no second resume.
	again, err := store.WakeDetachedParent(ctx, tenant, parentRun)
	if err != nil || again {
		t.Fatalf("second WakeDetachedParent = (%v, %v), want (false, nil) — single-winner", again, err)
	}
	if n := count(t, pool, `SELECT count(*) FROM durable_jobs WHERE kind='response.run' AND payload->>'run_id'=$1`, parentRun); n != 1 {
		t.Fatalf("enqueued parent jobs after double wake = %d, want 1 (exactly-once)", n)
	}
}

// TestWakeDetachedParentWaitsForAllChildren proves the wake holds until EVERY child is terminal (fan-out
// safety): a waiting parent with one terminal and one still-running child is NOT woken, so its resume
// never re-emits a child.request to a still-running child (which would re-release anyway).
func TestWakeDetachedParentWaitsForAllChildren(t *testing.T) {
	store := openStore(t)
	pool := store.Pool()
	ctx := context.Background()
	tenant, parentRun, _, _, _ := seedParent(t, pool, "waiting", "completed", true)
	// A second child, still running, under the same parent.
	sessionID := sessionOf(t, pool, parentRun)
	secondResp, secondRun := newID("resp"), newID("run")
	exec(t, pool, `INSERT INTO responses (id, organization_id, project_id, session_id, state, input) VALUES ($1,$2,$3,$4,'in_progress','{}')`,
		secondResp, tenant.Organization, tenant.Project, sessionID)
	exec(t, pool, `INSERT INTO runs (id, organization_id, project_id, session_id, response_id, state, parent_run_id, depth) VALUES ($1,$2,$3,$4,$5,'running',$6,1)`,
		secondRun, tenant.Organization, tenant.Project, sessionID, secondResp, parentRun)

	woken, err := store.WakeDetachedParent(ctx, tenant, parentRun)
	if err != nil || woken {
		t.Fatalf("WakeDetachedParent with a live sibling = (%v, %v), want (false, nil)", woken, err)
	}
	if s := runState(t, pool, parentRun); s != "waiting" {
		t.Fatalf("parent state = %q, want waiting (a live child remains)", s)
	}
}

// TestChildCompletionJournaledOnceAcrossRefold proves the exactly-once fold survives a repeated parent
// restore (E10 T8, the DET kill-recovery property): if a detached parent restores more than once — its
// resume attempt killed after folding but before completing, then reclaimed — it re-emits the
// child.request and re-folds the terminal child each time, but JournalChildCompletionOnce keeps the
// parent's stream carrying child.completed.v1 EXACTLY once. This is what makes "both killed → ladder
// restores each; the conversation is intact and folds are exactly-once" hold at the journal.
func TestChildCompletionJournaledOnceAcrossRefold(t *testing.T) {
	store := openStore(t)
	pool := store.Pool()
	ctx := context.Background()
	// A running parent (the fold is guarded on the parent being active) with a completed detached child.
	tenant, parentRun, parentResp, childRun, _ := seedParent(t, pool, "running", "completed", true)
	sessionID := sessionOf(t, pool, parentRun)
	payload, _ := json.Marshal(map[string]any{"child_run_id": childRun, "child_request_id": "creq_" + childRun, "status": "completed"})

	// Simulate the same fold running on three separate restore attempts of the parent.
	for i := 0; i < 3; i++ {
		if err := store.JournalChildCompletionOnce(ctx, tenant, sessionID, parentResp, parentRun, "child.completed.v1", childRun, payload); err != nil {
			t.Fatalf("refold %d error = %v", i, err)
		}
	}
	if c := count(t, pool, `SELECT count(*) FROM events WHERE response_id=$1 AND type='child.completed.v1' AND payload->>'child_run_id'=$2`, parentResp, childRun); c != 1 {
		t.Fatalf("child.completed.v1 across three restores = %d, want 1 (exactly-once fold)", c)
	}
}

// TestParentCancelPropagatesToDetachedChild proves SUB-005's detached variant (spec §25.18-19, E10 T8):
// canceling a parent that RELEASED its compute (waiting) still propagates the cancel to its detached,
// still-running child — the cancel walks parent_run_id regardless of detach — and the child-terminal
// wake never fires for the canceled (terminal) parent, so no spurious resume job is enqueued.
func TestParentCancelPropagatesToDetachedChild(t *testing.T) {
	store := openStore(t)
	pool := store.Pool()
	ctx := context.Background()
	tenant, parentRun, parentResp, childRun, childResp := seedParent(t, pool, "waiting", "running", true)
	canceled, _ := json.Marshal(map[string]any{"output": []any{}, "usage": map[string]any{}, "model": ""})

	if _, err := store.ApplyRunTransition(ctx, tenant, parentRun, statemachines.RunCmdCancel); err != nil {
		t.Fatalf("cancel waiting parent error = %v", err)
	}
	if err := store.FinalizeResponse(ctx, tenant, parentResp, "canceled", canceled); err != nil {
		t.Fatalf("finalize parent canceled error = %v", err)
	}
	n, err := store.CancelChildren(ctx, tenant, parentRun, canceled)
	if err != nil || n != 1 {
		t.Fatalf("CancelChildren = (%d, %v), want (1, nil) — the detached child is canceled", n, err)
	}
	if s := runState(t, pool, childRun); s != "canceled" {
		t.Fatalf("detached child state = %q, want canceled (propagation reaches a released parent's child)", s)
	}
	if c := count(t, pool, `SELECT count(*) FROM events WHERE response_id=$1 AND type='run.canceled.v1'`, childResp); c != 1 {
		t.Fatalf("child run.canceled.v1 = %d, want 1 (single terminal)", c)
	}
	// The canceled parent is terminal, so the child terminal wakes nothing — no spurious resume job.
	woken, err := store.WakeParentOfChild(ctx, tenant, childRun)
	if err != nil || woken {
		t.Fatalf("WakeParentOfChild for a canceled parent = (%v, %v), want (false, nil)", woken, err)
	}
	if c := count(t, pool, `SELECT count(*) FROM durable_jobs WHERE kind='response.run' AND payload->>'run_id'=$1`, parentRun); c != 0 {
		t.Fatalf("resume jobs for a canceled parent = %d, want 0", c)
	}
}

// TestParentCancelPropagatesToChildren proves SUB-005 (spec §25.18-19): canceling a parent run
// drives its live ChildRun to the canceled terminal, the terminal accounting is consistent (one
// terminal per run), a late child completion is dropped at the database, and a repeated cancel is
// a monotonic no-op.
func TestParentCancelPropagatesToChildren(t *testing.T) {
	store := openStore(t)
	pool := store.Pool()
	ctx := context.Background()
	tenant, parentRun, parentResp, childRun, childResp := seedParentWithChild(t, pool)
	canceled, _ := json.Marshal(map[string]any{"output": []any{}, "usage": map[string]any{}, "model": ""})

	// Cancel the parent — the run transition, its terminal projection, then propagation to children.
	if _, err := store.ApplyRunTransition(ctx, tenant, parentRun, statemachines.RunCmdCancel); err != nil {
		t.Fatalf("cancel parent run error = %v", err)
	}
	if err := store.FinalizeResponse(ctx, tenant, parentResp, "canceled", canceled); err != nil {
		t.Fatalf("finalize parent canceled error = %v", err)
	}
	n, err := store.CancelChildren(ctx, tenant, parentRun, canceled)
	if err != nil {
		t.Fatalf("CancelChildren error = %v", err)
	}
	if n != 1 {
		t.Fatalf("CancelChildren canceled = %d children, want 1", n)
	}

	// Both runs reached the canceled terminal, each with exactly one terminal event.
	if s := runState(t, pool, parentRun); s != "canceled" {
		t.Fatalf("parent run state = %q, want canceled", s)
	}
	if s := runState(t, pool, childRun); s != "canceled" {
		t.Fatalf("child run state = %q, want canceled (propagation)", s)
	}
	if c := count(t, pool, `SELECT count(*) FROM events WHERE response_id=$1 AND type='run.canceled.v1'`, childResp); c != 1 {
		t.Fatalf("child run.canceled.v1 count = %d, want 1 (single terminal)", c)
	}

	// A late child completion — its in-flight attempt finishing just after the cancel — must be
	// dropped at the database (conditional UpdateResponse), leaving the canceled row intact.
	completed, _ := json.Marshal(map[string]any{"output": []any{map[string]any{"type": "message", "content": "late"}}, "model": "fake"})
	if err := store.FinalizeResponse(ctx, tenant, childResp, "completed", completed); err != nil {
		t.Fatalf("late child finalize returned error = %v, want a silent no-op", err)
	}
	if c := count(t, pool, `SELECT count(*) FROM responses WHERE id=$1 AND state='canceled'`, childResp); c != 1 {
		t.Fatalf("child response after late completion is not canceled — the late terminal overwrote the canceled row")
	}

	// A repeated cancel is a monotonic no-op: no second terminal, no re-canceled child.
	if n, err := store.CancelChildren(ctx, tenant, parentRun, canceled); err != nil || n != 0 {
		t.Fatalf("re-cancel CancelChildren = (%d, %v), want (0, nil) — the child is already terminal", n, err)
	}
	if c := count(t, pool, `SELECT count(*) FROM events WHERE response_id=$1 AND type='run.canceled.v1'`, childResp); c != 1 {
		t.Fatalf("child run.canceled.v1 after re-cancel = %d, want 1 (monotonic terminal)", c)
	}
}
