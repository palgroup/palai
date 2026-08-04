//go:build component

package execution

import (
	"context"
	"os"
	"testing"

	"github.com/palgroup/palai/packages/contracts"
	"github.com/palgroup/palai/packages/coordinator"
	"github.com/palgroup/palai/packages/runner"

	"github.com/palgroup/palai/storage"
)

// AN INLINE CHILD THAT FAILED IS STILL A RUN SOMEBODY IS WAITING ON.
//
// Measured on a live stack 2026-08-02. `run_e821e4fb4e0c0fc1` had been `running` for over a day with
// one `assigned` attempt, and `select count(*) from durable_jobs where payload::text like '%e821e4fb%'`
// returned ZERO. Its session journal explains it in four lines:
//
//	6 | child.requested.v1  {"child_run_id":"run_e821e4fb…"}
//	7 | run.provisioning.v1        (the child, driven INLINE by dispatchChild)
//	8 | run.running.v1
//	9 | child.completed.v1  {"status":"failed","child_run_id":"run_e821e4fb…"}
//	10| run.failed.v1              (the PARENT)
//
// The parent is honest: it reports the child failed and fails itself. The CHILD's own run row stays
// `running` and its response stays `in_progress` forever. `child_dispatch.go` runs it with
// `_ = o.ExecuteAttempt(ctx, childDesc)` — the error is discarded by design, because a child's failure
// is not the parent's — but nothing then settles the child's own row, and an inline child has NO
// durable job, so `DeadLetteredResponseRuns` (the only bridge from a dead job to a failed run) cannot
// reach it and no reaper covers it. It is a run whose progress depends on a row nothing ever created.
//
// `foldChildResult` is the right place because it is the ONE code path both the inline dispatch and the
// detached rebind fold through, and it ALREADY reads the child's state to decide what to tell the
// parent. It knew the child was non-terminal and told the parent "failed" while leaving the child's own
// row claiming to be running.

// childTerminalHarness is the redelivery harness's shape for one parent and one inline child.
type childTerminalHarness struct {
	orch                 *Orchestrator
	tenant               coordinator.Tenant
	sessionID            string
	parentRunID          string
	parentResponseID     string
	childRunID           string
	childResponseID      string
	ch                   *recordingChannel
	childRequestID       string
	childStartingState   string
	childResponseInitial string
}

// newChildTerminalHarness seeds a parent run and a child run left in `childState` — the state an inline
// ExecuteAttempt leaves behind when it returns an error after driving the child to running.
func newChildTerminalHarness(t *testing.T, childState, childResponseState string) *childTerminalHarness {
	t.Helper()
	url := os.Getenv("PALAI_COMPONENT_POSTGRES_URL")
	if url == "" {
		t.Skip("PALAI_COMPONENT_POSTGRES_URL is required; run make test-component TEST=postgres")
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

	h := &childTerminalHarness{
		orch:                 &Orchestrator{spine: cs},
		tenant:               coordinator.Tenant{Project: redeliveryID("prj")},
		sessionID:            redeliveryID("ses"),
		parentRunID:          redeliveryID("run"),
		parentResponseID:     redeliveryID("resp"),
		childRunID:           redeliveryID("run"),
		childResponseID:      redeliveryID("resp"),
		ch:                   &recordingChannel{},
		childRequestID:       redeliveryID("chld"),
		childStartingState:   childState,
		childResponseInitial: childResponseState,
	}
	pool := cs.Pool()
	execSQL(t, pool, `INSERT INTO projects (id) VALUES ($1)`, h.tenant.Project)
	execSQL(t, pool, `INSERT INTO sessions (id, project_id) VALUES ($1,$2)`, h.sessionID, h.tenant.Project)
	// The child carries parent_run_id and depth 1, exactly as dispatchChild writes it — and not only for
	// realism: `runs_one_active_root_per_session` is UNIQUE on session_id for non-terminal rows WHERE
	// parent_run_id IS NULL, so a fixture that seeded two roots in one session could not exist at all.
	execSQL(t, pool, `INSERT INTO responses (id, project_id, session_id, state, input)
	                   VALUES ($1,$2,$3,'in_progress','"go"'::jsonb)`,
		h.parentResponseID, h.tenant.Project, h.sessionID)
	execSQL(t, pool, `INSERT INTO runs (id, project_id, session_id, response_id, state)
	                   VALUES ($1,$2,$3,$4,'running')`,
		h.parentRunID, h.tenant.Project, h.sessionID, h.parentResponseID)
	execSQL(t, pool, `INSERT INTO responses (id, project_id, session_id, state, input)
	                   VALUES ($1,$2,$3,$4,'"go"'::jsonb)`,
		h.childResponseID, h.tenant.Project, h.sessionID, childResponseState)
	execSQL(t, pool, `INSERT INTO runs (id, project_id, session_id, response_id, state, parent_run_id, depth)
	                   VALUES ($1,$2,$3,$4,$5,$6,1)`,
		h.childRunID, h.tenant.Project, h.sessionID, h.childResponseID, childState, h.parentRunID)
	return h
}

func (h *childTerminalHarness) fold(t *testing.T) {
	t.Helper()
	st := &attemptState{
		attempt:    AttemptDescriptor{RunID: contracts.RunID(h.parentRunID), AttemptID: contracts.AttemptID(redeliveryID("att"))},
		tenant:     h.tenant,
		sessionID:  h.sessionID,
		responseID: h.parentResponseID,
		ch:         h.ch,
		ledger:     runner.NewFrameLedger(),
	}
	if err := h.orch.foldChildResult(context.Background(), st, h.childRequestID, h.childRunID,
		contracts.EngineFrame{ID: contracts.FrameID(redeliveryID("frm"))}); err != nil {
		t.Fatalf("foldChildResult: %v", err)
	}
}

func (h *childTerminalHarness) childRow(t *testing.T) (runState, responseState string) {
	t.Helper()
	ctx := storage.WithSystemScope(context.Background())
	if err := h.orch.spine.Pool().QueryRow(ctx,
		`SELECT r.state, resp.state FROM runs r JOIN responses resp ON resp.id = r.response_id WHERE r.id = $1`,
		h.childRunID).Scan(&runState, &responseState); err != nil {
		t.Fatalf("read the child run: %v", err)
	}
	return runState, responseState
}

// TestAnInlineChildThatNeverFinishedReachesATerminal is RED.
//
// The child was driven to `running` and its inline attempt then errored. Nothing else in the tree can
// move it — no durable job, no lease to reclaim, no dead-letter bridge, no reaper — so the fold that
// already reads its state must settle it.
func TestAnInlineChildThatNeverFinishedReachesATerminal(t *testing.T) {
	h := newChildTerminalHarness(t, "running", "in_progress")
	h.fold(t)

	runState, responseState := h.childRow(t)
	if !childRunTerminal(runState) {
		t.Fatalf("the child run is %q after its parent folded a failed result: an inline child has no durable job, so no reclaim, no dead-letter bridge and no reaper can reach it — this run stays %q forever while its response stays %q", runState, runState, responseState)
	}
	if responseState == "in_progress" || responseState == "queued" || responseState == "provisioning" {
		t.Fatalf("the child run reached %q but its response is still %q: anyone reading that response sees a live run forever", runState, responseState)
	}
	// The parent still hears exactly what it heard before — the fold's contract to the engine is unchanged.
	var told string
	for _, f := range h.ch.sent {
		if f.Type == "child.result" {
			told, _ = f.Data["status"].(string)
		}
	}
	if told != "failed" {
		t.Fatalf("the parent was told status=%q, want failed — settling the child must not change what the engine folds", told)
	}
}

// TestAParkedInlineChildIsLeftAlone is the half without which the fix would be a regression, not a fix.
//
// `waiting` is a run parked ON PURPOSE — a human's approval, a gated tool call, a live background task,
// a pool with no machine — and each of those has its own waker. Settling it would end a run somebody is
// about to answer. The distinction the fix must make is between a run that is WAITING FOR SOMETHING and
// a run that nothing will ever come back to.
func TestAParkedInlineChildIsLeftAlone(t *testing.T) {
	h := newChildTerminalHarness(t, "waiting", "waiting_for_tool")
	h.fold(t)

	runState, _ := h.childRow(t)
	if runState != "waiting" {
		t.Fatalf("a PARKED inline child is now %q, want waiting — `waiting` has four wakers of its own and settling it ends a run a human is about to answer", runState)
	}
}
