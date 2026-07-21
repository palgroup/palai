//go:build component

package postgres

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/palgroup/palai/packages/coordinator"
)

// canceledProj / uncertainProj are minimal terminal projections the store layer would build; the test
// only checks the response STATE, so their body just has to be well-formed JSON.
func canceledProj() []byte  { return []byte(`{"output":[],"error":{"code":"canceled"}}`) }
func uncertainProj() []byte { return []byte(`{"output":[],"error":{"code":"uncertain_side_effect"}}`) }

// seedChildRun creates a running child run of parent, with its own in_progress response, and returns
// (childRunID, childResponseID).
func seedChildRun(t *testing.T, tenant coordinator.Tenant, cs *coordinator.Store, sessionID, parentRunID string) (string, string) {
	t.Helper()
	pool := cs.Pool()
	childRun, childResp := newID("run"), newID("resp")
	exec(t, pool, `INSERT INTO responses (id, organization_id, project_id, session_id, state) VALUES ($1,$2,$3,$4,'in_progress')`,
		childResp, tenant.Organization, tenant.Project, sessionID)
	exec(t, pool, `INSERT INTO runs (id, organization_id, project_id, session_id, state, parent_run_id, depth, response_id) VALUES ($1,$2,$3,$4,'running',$5,1,$6)`,
		childRun, tenant.Organization, tenant.Project, sessionID, parentRunID, childResp)
	return childRun, childResp
}

// TestCancelDuringKillReconcilesChildrenSingleTerminal proves SES-010's recovery half (spec §26.10 steps
// 8-9, E10 T7): a cancel racing a kill, when an irreversible tool_call is still UNCERTAIN, terminalizes
// the run as failed_with_uncertain_side_effect (the effect may have landed — no false clean cancel),
// propagates the cancel to its children (each a single terminal), and is MONOTONIC: a late completed
// terminal cannot overwrite it, and a second cancel is idempotent — exactly one terminal.
func TestCancelDuringKillReconcilesChildrenSingleTerminal(t *testing.T) {
	cs := openHarness(t)
	ctx := context.Background()
	pool := cs.Pool()
	tenant, sessionID, runID := seedRun(t, pool)
	respID := newID("resp")
	exec(t, pool, `INSERT INTO responses (id, organization_id, project_id, session_id, state) VALUES ($1,$2,$3,$4,'in_progress')`,
		respID, tenant.Organization, tenant.Project, sessionID)
	exec(t, pool, `UPDATE runs SET state='running', response_id=$2 WHERE id=$1`, runID, respID)
	childRun, childResp := seedChildRun(t, tenant, cs, sessionID, runID)

	// An irreversible tool_call left UNCERTAIN by the kill (its side effect may have landed).
	exec(t, pool, `INSERT INTO tool_calls (id, organization_id, project_id, run_id, fence, state, name, arguments, replay_class, reconciliation_state)
		VALUES ($1,$2,$3,$4,1,'uncertain','charge','{}','irreversible','reconciling')`, newID("tc"), tenant.Organization, tenant.Project, runID)

	terminal, err := cs.CancelRunReconciled(ctx, tenant, respID, runID, canceledProj(), uncertainProj())
	if err != nil {
		t.Fatalf("CancelRunReconciled error = %v", err)
	}
	if terminal != "failed_with_uncertain_side_effect" {
		t.Fatalf("terminal = %q, want failed_with_uncertain_side_effect (uncertain irreversible op)", terminal)
	}
	// The run is canceled (single terminal) and the child was propagated to canceled.
	var runState, childRunState, childRespState string
	if err := pool.QueryRow(ctx, `SELECT state FROM runs WHERE id=$1`, runID).Scan(&runState); err != nil {
		t.Fatalf("read run state error = %v", err)
	}
	if err := pool.QueryRow(ctx, `SELECT state FROM runs WHERE id=$1`, childRun).Scan(&childRunState); err != nil {
		t.Fatalf("read child run state error = %v", err)
	}
	if err := pool.QueryRow(ctx, `SELECT state FROM responses WHERE id=$1`, childResp).Scan(&childRespState); err != nil {
		t.Fatalf("read child response state error = %v", err)
	}
	if runState != "canceled" {
		t.Fatalf("run state = %q, want canceled", runState)
	}
	if childRunState != "canceled" || childRespState != "canceled" {
		t.Fatalf("child = {run:%q resp:%q}, want both canceled (cancel-propagation)", childRunState, childRespState)
	}

	// MONOTONIC: a late completed terminal (an in-flight attempt that finished recovery just after) must
	// NOT overwrite the failed_with_uncertain_side_effect terminal.
	late, _ := json.Marshal(map[string]any{"output": []any{map[string]any{"type": "message", "content": "late"}}, "model": "fake"})
	if err := cs.FinalizeResponse(ctx, tenant, respID, "completed", late); err != nil {
		t.Fatalf("late finalize error = %v, want a silent no-op", err)
	}
	// A second cancel is idempotent — still the same single terminal.
	again, err := cs.CancelRunReconciled(ctx, tenant, respID, runID, canceledProj(), uncertainProj())
	if err != nil {
		t.Fatalf("second CancelRunReconciled error = %v", err)
	}
	if again != "failed_with_uncertain_side_effect" {
		t.Fatalf("second cancel terminal = %q, want the same failed_with_uncertain_side_effect (monotonic)", again)
	}
	var finalState string
	if err := pool.QueryRow(ctx, `SELECT state FROM responses WHERE id=$1`, respID).Scan(&finalState); err != nil {
		t.Fatalf("read final response state error = %v", err)
	}
	if finalState != "failed_with_uncertain_side_effect" {
		t.Fatalf("final response state = %q, want failed_with_uncertain_side_effect (late completed lost, monotonic)", finalState)
	}
}

// TestCancelDuringKillCleanTerminalIsCanceled proves the clean SES-010 branch: with NO unresolved
// uncertain side effect, a cancel during kill reaches the plain `canceled` terminal (no false
// uncertainty claim). A resolved (reconciled_completed) op does not make the terminal uncertain.
func TestCancelDuringKillCleanTerminalIsCanceled(t *testing.T) {
	cs := openHarness(t)
	ctx := context.Background()
	pool := cs.Pool()
	tenant, sessionID, runID := seedRun(t, pool)
	respID := newID("resp")
	exec(t, pool, `INSERT INTO responses (id, organization_id, project_id, session_id, state) VALUES ($1,$2,$3,$4,'in_progress')`,
		respID, tenant.Organization, tenant.Project, sessionID)
	exec(t, pool, `UPDATE runs SET state='running', response_id=$2 WHERE id=$1`, runID, respID)
	// A RESOLVED op (already reconciled) is not a pending uncertainty.
	exec(t, pool, `INSERT INTO tool_calls (id, organization_id, project_id, run_id, fence, state, name, arguments, replay_class, reconciliation_state)
		VALUES ($1,$2,$3,$4,1,'reconciled_completed','push','{}','idempotent','reconciled_completed')`, newID("tc"), tenant.Organization, tenant.Project, runID)

	terminal, err := cs.CancelRunReconciled(ctx, tenant, respID, runID, canceledProj(), uncertainProj())
	if err != nil {
		t.Fatalf("CancelRunReconciled error = %v", err)
	}
	if terminal != "canceled" {
		t.Fatalf("terminal = %q, want canceled (no unresolved uncertain side effect)", terminal)
	}
}
