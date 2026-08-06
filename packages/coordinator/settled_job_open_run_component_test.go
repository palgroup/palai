//go:build component

package coordinator

import (
	"context"
	"testing"
	"time"

	"github.com/palgroup/palai/storage"
)

// A JOB THAT FINISHED OVER A RUN THAT DID NOT.
//
// ‼️ MEASURED ON A LIVE PLANE, 2026-08-07. A coding run's durable job finished `completed` — twice, at
// attempt_count 2 and 3 — while its run sat `running` twenty-five minutes later, its response
// `in_progress`, its output empty. The machine's own log says how:
//
//	engine exceeded wall-time bound
//	send workspace result: failed to write msg: use of closed network connection
//	engine wait did not complete after stdout closed
//
// The runner could not write the failure back, the control-plane side of the attempt returned without a
// hard error, and the job was marked done. The reconciler's bridge only looked at `dead` jobs, so this
// one was invisible to it: a settled queue, no dead letters, and one response that never ends. That is
// worse than a dead-lettered job, which at least LOOKS wrong.
//
// The two tests are the two halves of the same predicate, and shipping only the first would be shipping
// a sweep that fails healthy runs: a completed job's terminal transition and its completion commit in
// SEPARATE transactions, so there is a legitimate window where a completed job has a non-terminal run.

// settledJobFixture seeds a project, a session, a response and a non-terminal run, then a response.run
// job for it at the given status and age. It returns the run id.
//
// The job's `updated_at` is written EXPLICITLY rather than slept for: the grace is two minutes and a
// test that waited it out would add two minutes to the tier for nothing. What is being asserted is the
// predicate's reading of the column, and the column is what the sweep reads.
func settledJobFixture(t *testing.T, cs *Store, status string, age time.Duration) (Tenant, string) {
	t.Helper()
	project := pinTestID("prj")
	session, response, runID, jobID := pinTestID("ses"), pinTestID("resp"), pinTestID("run"), pinTestID("job")
	mustExecPin(t, cs, `INSERT INTO projects (id) VALUES ($1)`, project)
	mustExecPin(t, cs, `INSERT INTO sessions (id, project_id) VALUES ($1,$2)`, session, project)
	mustExecPin(t, cs, `INSERT INTO responses (id, project_id, session_id, state, input)
	                     VALUES ($1,$2,$3,'queued','"hi"'::jsonb)`, response, project, session)
	mustExecPin(t, cs, `INSERT INTO runs (id, project_id, session_id, response_id, state)
	                     VALUES ($1,$2,$3,$4,'running')`, runID, project, session, response)
	mustExecPin(t, cs, `INSERT INTO durable_jobs (id, project_id, kind, status, payload, updated_at)
	                     VALUES ($1,$2,'response.run',$3,jsonb_build_object('run_id',$4::text), now() - $5::interval)`,
		jobID, project, status, runID, age.String())
	return Tenant{Project: project}, runID
}

func runState(t *testing.T, cs *Store, runID string) string {
	t.Helper()
	var state string
	if err := cs.pool.QueryRow(storage.WithSystemScope(context.Background()),
		`SELECT state FROM runs WHERE id = $1`, runID).Scan(&state); err != nil {
		t.Fatalf("read run state: %v", err)
	}
	return state
}

func TestACompletedJobOverANonTerminalRunIsFailed(t *testing.T) {
	cs := failureReasonFixture(t)
	_, runID := settledJobFixture(t, cs, "completed", 10*time.Minute)

	if got := runState(t, cs, runID); got != "running" {
		t.Fatalf("the fixture seeded a %q run, want running — nothing below is a statement about the sweep", got)
	}
	if _, err := cs.SweepDeadLetteredRuns(context.Background()); err != nil {
		t.Fatalf("SweepDeadLetteredRuns error = %v", err)
	}
	if got := runState(t, cs, runID); got != "failed" {
		t.Fatalf("a run whose job completed ten minutes ago is still %q: its response never projects "+
			"terminal and its stream never closes, which is the state measured on a live plane and the "+
			"reason this arm of the predicate exists", got)
	}
}

func TestARunWhoseJobJustCompletedIsLeftAlone(t *testing.T) {
	cs := failureReasonFixture(t)
	_, runID := settledJobFixture(t, cs, "completed", 5*time.Second)

	if _, err := cs.SweepDeadLetteredRuns(context.Background()); err != nil {
		t.Fatalf("SweepDeadLetteredRuns error = %v", err)
	}
	// THE HALF THAT KEEPS THE FIX FROM BEING WORSE THAN THE DEFECT. A run's terminal transition and its
	// job's completion commit separately, so a sweep with no grace would race an ORDINARY finishing run
	// into a failure it did not earn — and it would do it under load, on the runs that took longest,
	// which are exactly the ones a customer is watching.
	if got := runState(t, cs, runID); got != "running" {
		t.Fatalf("a run whose job completed five seconds ago was driven to %q: the sweep is racing the "+
			"ordinary commit window rather than catching an abandoned run", got)
	}
}
