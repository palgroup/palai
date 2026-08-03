//go:build component

package coordinator

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/palgroup/palai/storage"
)

// WHY A DEAD JOB DIED, against a REAL Postgres and the REAL Worker claim loop.
//
// This is the defect that made the other four cost a night instead of a minute. On 2026-08-02 a live
// stack carried 11 dead `response.run` jobs and 208 preempted attempts. `job_attempts` recorded, for
// every one of them, that the attempt started and that its outcome was 'failed'. `durable_jobs`
// recorded a status and a backoff. Nothing recorded WHY, and nothing logged it: `Worker.process` called
// `store.Fail(ctx, claim, retry)` and the handler's error went out of scope at the end of the `if`.
//
// The actual cause — an engine exiting at its first model step — was readable only in a runner
// CONTAINER's stdout, which no operator has after a restart and no support case ever includes.
//
// The proof drives the REAL Worker, not Fail() directly, because the discarded value was the WORKER's:
// a test that called Fail with a reason would have asserted the plumbing while the caller that has the
// reason still threw it away. That is the exact shape this repository keeps shipping — a mechanism
// proven and the surface that reaches it left unwired.

// failureReasonFixture opens the component database, migrates it, and returns a store.
func failureReasonFixture(t *testing.T) *Store {
	t.Helper()
	url := os.Getenv("PALAI_COMPONENT_POSTGRES_URL")
	if url == "" {
		t.Skip("PALAI_COMPONENT_POSTGRES_URL is required; run make test-component TEST=postgres")
	}
	ctx := context.Background()
	cs, err := Open(ctx, url)
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	t.Cleanup(cs.Close)
	if err := cs.Migrate(ctx); err != nil {
		t.Fatalf("Migrate() error = %v", err)
	}
	return cs
}

// TestADeadJobRecordsWhyEachOfItsAttemptsFailed is RED.
//
// A handler that fails deterministically — which is what an engine dying at the same step every time
// looks like from the queue — must leave a durable record of the reason on EVERY attempt, not merely
// the word 'failed'. The assertion is per-attempt on purpose: attempt 1 failing to dial and attempt 5
// failing to reach a provider are different facts about one job, and a single column on the job row
// would keep the last and overwrite the four that explain the pattern.
func TestADeadJobRecordsWhyEachOfItsAttemptsFailed(t *testing.T) {
	cs := failureReasonFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	org, project := pinTestID("org"), pinTestID("prj")
	mustExecPin(t, cs, `INSERT INTO organizations (id) VALUES ($1)`, org)
	mustExecPin(t, cs, `INSERT INTO projects (id, organization_id) VALUES ($1,$2)`, project, org)
	jobID := pinTestID("job")
	mustExecPin(t, cs, `INSERT INTO durable_jobs (id, organization_id, project_id, kind, payload)
	                     VALUES ($1,$2,$3,'response.run','{}'::jsonb)`, jobID, org, project)

	// The sentence a person needs, and the one nothing kept. It is deliberately distinctive so a test
	// that found the word 'failed' somewhere could not pass by accident.
	const reason = "engine wait did not complete after stdout closed"
	policy := RetryPolicy{MaxAttempts: 3, BaseBackoff: time.Millisecond, MaxBackoff: 5 * time.Millisecond}
	worker := NewWorker(cs, WorkerConfig{
		Owner: "component-failure-reason", Lease: 5 * time.Second, Heartbeat: time.Second,
		PollInterval: 5 * time.Millisecond, Retry: policy,
	}, func(context.Context, Claim, []byte) (string, error) {
		return "", errors.New(reason)
	})

	runCtx, stop := context.WithCancel(ctx)
	done := make(chan error, 1)
	go func() { done <- worker.Run(runCtx) }()

	// Wait for the job to exhaust its ladder, then stand the worker down.
	deadline := time.Now().Add(30 * time.Second)
	var status string
	for {
		if err := cs.pool.QueryRow(storage.WithSystemScope(ctx),
			`SELECT status FROM durable_jobs WHERE id = $1`, jobID).Scan(&status); err != nil {
			t.Fatalf("read job status: %v", err)
		}
		if status == "dead" {
			break
		}
		if time.Now().After(deadline) {
			stop()
			<-done
			t.Fatalf("job status = %q after 30s, want dead", status)
		}
		time.Sleep(20 * time.Millisecond)
	}
	stop()
	<-done

	rows, err := cs.pool.Query(storage.WithSystemScope(ctx),
		`SELECT fence, coalesce(outcome,''), coalesce(error,'') FROM job_attempts WHERE job_id = $1 ORDER BY fence`, jobID)
	if err != nil {
		t.Fatalf("read the attempt ledger: %v", err)
	}
	defer rows.Close()
	attempts := 0
	for rows.Next() {
		var fence int64
		var outcome, reasonGot string
		if err := rows.Scan(&fence, &outcome, &reasonGot); err != nil {
			t.Fatalf("scan attempt: %v", err)
		}
		attempts++
		if outcome != "failed" && outcome != "dead" {
			t.Fatalf("attempt fence %d outcome = %q, want failed or dead", fence, outcome)
		}
		if !strings.Contains(reasonGot, reason) {
			t.Fatalf("attempt fence %d recorded outcome %q and reason %q: the ledger says THAT it failed and not WHY — this is why 11 dead jobs and 208 preempted attempts on a live stack could only be explained from a container's stdout", fence, outcome, reasonGot)
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate attempts: %v", err)
	}
	// NON-VACUITY: an empty ledger would satisfy every assertion in the loop above.
	if attempts != policy.MaxAttempts {
		t.Fatalf("the ledger holds %d attempt rows, want %d — a loop over nothing proves nothing", attempts, policy.MaxAttempts)
	}
}

// TestASucceedingJobRecordsNoFailureReason is the negative half, and it is not decoration: a writer
// that stamped every attempt would satisfy the test above while making the column meaningless. NULL
// has to keep meaning "this attempt did not fail", which is what lets an operator scan for the rows
// that did.
func TestASucceedingJobRecordsNoFailureReason(t *testing.T) {
	cs := failureReasonFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	org, project := pinTestID("org"), pinTestID("prj")
	mustExecPin(t, cs, `INSERT INTO organizations (id) VALUES ($1)`, org)
	mustExecPin(t, cs, `INSERT INTO projects (id, organization_id) VALUES ($1,$2)`, project, org)

	jobID := pinTestID("job")
	mustExecPin(t, cs, `INSERT INTO durable_jobs (id, organization_id, project_id, kind, payload)
	                     VALUES ($1,$2,$3,'response.run','{}'::jsonb)`, jobID, org, project)

	worker := NewWorker(cs, WorkerConfig{
		Owner: "component-failure-reason-ok", Lease: 5 * time.Second, Heartbeat: time.Second,
		PollInterval: 5 * time.Millisecond,
		Retry:        RetryPolicy{MaxAttempts: 3, BaseBackoff: time.Millisecond, MaxBackoff: 5 * time.Millisecond},
	}, func(context.Context, Claim, []byte) (string, error) { return "ok", nil })

	runCtx, stop := context.WithCancel(ctx)
	done := make(chan error, 1)
	go func() { done <- worker.Run(runCtx) }()

	deadline := time.Now().Add(30 * time.Second)
	var status string
	for {
		if err := cs.pool.QueryRow(storage.WithSystemScope(ctx),
			`SELECT status FROM durable_jobs WHERE id = $1`, jobID).Scan(&status); err != nil {
			t.Fatalf("read job status: %v", err)
		}
		if status == "completed" {
			break
		}
		if time.Now().After(deadline) {
			stop()
			<-done
			t.Fatalf("job status = %q after 30s, want completed", status)
		}
		time.Sleep(20 * time.Millisecond)
	}
	stop()
	<-done

	var withReason int
	if err := cs.pool.QueryRow(storage.WithSystemScope(ctx),
		`SELECT count(*) FROM job_attempts WHERE job_id = $1 AND error IS NOT NULL`, jobID).Scan(&withReason); err != nil {
		t.Fatalf("count reasons: %v", err)
	}
	if withReason != 0 {
		t.Fatalf("%d attempt(s) of a job that SUCCEEDED carry a failure reason, want 0 — a column stamped unconditionally says nothing", withReason)
	}
}
