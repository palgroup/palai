//go:build component

package postgres

import (
	"context"
	"errors"
	"sort"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/palgroup/palai/packages/coordinator"
	statemachines "github.com/palgroup/palai/packages/state-machines"
)

func TestSessionSequenceStrictlyAllocatedInTransaction(t *testing.T) {
	cs := openHarness(t)
	tenant, sessionID, firstRun := seedRun(t, cs.Pool())
	ctx := context.Background()

	// Many queued runs in one session; each concurrent transition allocates one
	// sequence through the real (tenant-gated) mutation path.
	const workers = 20
	runs := make([]string, workers)
	runs[0] = firstRun
	for i := 1; i < workers; i++ {
		runs[i] = newID("run")
		exec(t, cs.Pool(), `INSERT INTO runs (id, organization_id, project_id, session_id) VALUES ($1, $2, $3, $4)`,
			runs[i], tenant.Organization, tenant.Project, sessionID)
	}

	seqs := make([]int64, workers)
	var wg sync.WaitGroup
	var firstErr error
	var errOnce sync.Once
	start := make(chan struct{})
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			tr, err := cs.ApplyRunTransition(ctx, tenant, runs[i], statemachines.RunCmdProvision)
			if err != nil {
				errOnce.Do(func() { firstErr = err })
				return
			}
			seqs[i] = tr.Sequence
		}(i)
	}
	close(start)
	wg.Wait()
	if firstErr != nil {
		t.Fatalf("ApplyRunTransition() error = %v", firstErr)
	}

	sorted := append([]int64(nil), seqs...)
	sort.Slice(sorted, func(a, b int) bool { return sorted[a] < sorted[b] })
	for i, seq := range sorted {
		if seq != int64(i+1) {
			t.Fatalf("allocated sequences = %v, want unique 1..%d", sorted, workers)
		}
	}
}

func TestRunTerminalStateCannotReturnToNonTerminal(t *testing.T) {
	cs := openHarness(t)
	tenant, _, runID := seedRun(t, cs.Pool())
	ctx := context.Background()

	for _, cmd := range []statemachines.RunCommand{
		statemachines.RunCmdProvision,
		statemachines.RunCmdStart,
		statemachines.RunCmdComplete,
	} {
		if _, err := cs.ApplyRunTransition(ctx, tenant, runID, cmd); err != nil {
			t.Fatalf("ApplyRunTransition(%s) error = %v", cmd, err)
		}
	}

	// The database trigger rejects a raw update out of a terminal state.
	_, err := cs.Pool().Exec(ctx, `UPDATE runs SET state = 'running' WHERE id = $1`, runID)
	if got := pgCode(err); got != "23514" {
		t.Fatalf("terminal-run update code = %q (%v), want 23514 check_violation", got, err)
	}

	// The pure state machine also refuses any command out of a terminal state.
	if _, err := cs.ApplyRunTransition(ctx, tenant, runID, statemachines.RunCmdComplete); !errors.Is(err, statemachines.ErrInvalidState) {
		t.Fatalf("terminal-run transition error = %v, want ErrInvalidState", err)
	}
}

func TestTransitionCommitsStateEventAndOutboxAtomically(t *testing.T) {
	cs := openHarness(t)
	tenant, sessionID, runID := seedRun(t, cs.Pool())
	ctx := context.Background()

	transition, err := cs.ApplyRunTransition(ctx, tenant, runID, statemachines.RunCmdProvision)
	if err != nil {
		t.Fatalf("ApplyRunTransition(provision) error = %v", err)
	}
	if transition.To != statemachines.RunProvisioning || transition.Event != "run.provisioning.v1" || transition.Sequence != 1 {
		t.Fatalf("transition = %+v, want provisioning/run.provisioning.v1/seq 1", transition)
	}

	assertRunState(t, cs, runID, "provisioning")
	if got := eventCount(t, cs, sessionID); got != 1 {
		t.Fatalf("event count = %d, want 1", got)
	}
	if got := outboxCount(t, cs, runID, transition.Sequence); got != 1 {
		t.Fatalf("outbox count for seq %d = %d, want 1", transition.Sequence, got)
	}

	// A rejected command commits nothing: no state change, no event, no outbox row.
	if _, err := cs.ApplyRunTransition(ctx, tenant, runID, statemachines.RunCmdComplete); !errors.Is(err, statemachines.ErrInvalidState) {
		t.Fatalf("illegal transition error = %v, want ErrInvalidState", err)
	}
	assertRunState(t, cs, runID, "provisioning")
	if got := eventCount(t, cs, sessionID); got != 1 {
		t.Fatalf("event count after rejected command = %d, want 1", got)
	}
}

func TestJobLeaseFenceIncreasesAfterReclaim(t *testing.T) {
	cs := openHarness(t)
	tenant, _, _ := seedRun(t, cs.Pool())
	ctx := context.Background()
	jobID := newID("job")

	if err := cs.Enqueue(ctx, tenant, jobID, "response.run"); err != nil {
		t.Fatalf("Enqueue() error = %v", err)
	}
	const lease = 150 * time.Millisecond
	first, err := cs.Claim(ctx, tenant, jobID, "worker-a", lease)
	if err != nil {
		t.Fatalf("first Claim() error = %v", err)
	}

	waitLeaseExpiry(t, cs, tenant, jobID)
	second, err := cs.Claim(ctx, tenant, jobID, "worker-b", lease)
	if err != nil {
		t.Fatalf("reclaim Claim() error = %v", err)
	}
	if second.Fence <= first.Fence {
		t.Fatalf("reclaim fence = %d, want > %d", second.Fence, first.Fence)
	}
	if second.AttemptCount != first.AttemptCount+1 {
		t.Fatalf("reclaim attempt count = %d, want %d", second.AttemptCount, first.AttemptCount+1)
	}

	snap, err := cs.Snapshot(ctx, tenant, jobID)
	if err != nil {
		t.Fatalf("Snapshot() error = %v", err)
	}
	if snap.Fence != second.Fence {
		t.Fatalf("snapshot fence = %d, want %d", snap.Fence, second.Fence)
	}

	// The superseded holder cannot complete; only the live fence writes an
	// authoritative result and its single outbox row (spec §53.5, §24.5).
	if err := cs.Complete(ctx, first, "stale-result"); !errors.Is(err, coordinator.ErrStaleFence) {
		t.Fatalf("stale Complete() error = %v, want ErrStaleFence", err)
	}
	if err := cs.Complete(ctx, second, "final-result"); err != nil {
		t.Fatalf("authoritative Complete() error = %v", err)
	}
	snap, err = cs.Snapshot(ctx, tenant, jobID)
	if err != nil {
		t.Fatalf("Snapshot() after completion error = %v", err)
	}
	if snap.Status != "completed" || snap.ResultHash == nil || *snap.ResultHash != "final-result" {
		t.Fatalf("completed snapshot = %+v, want completed/final-result", snap)
	}
	var outbox int
	if err := cs.Pool().QueryRow(ctx, `SELECT count(*) FROM outbox WHERE dedupe_key = $1`,
		"job:"+jobID+":fence:"+strconv.FormatInt(second.Fence, 10)+":completed").Scan(&outbox); err != nil {
		t.Fatalf("count job outbox error = %v", err)
	}
	if outbox != 1 {
		t.Fatalf("job completion outbox rows = %d, want 1", outbox)
	}
}

func waitLeaseExpiry(t *testing.T, cs *coordinator.Store, tenant coordinator.Tenant, jobID string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	for {
		expired, err := cs.LeaseExpired(ctx, tenant, jobID)
		if err != nil {
			t.Fatalf("LeaseExpired() error = %v", err)
		}
		if expired {
			return
		}
		select {
		case <-ctx.Done():
			t.Fatalf("lease did not expire: %v", ctx.Err())
		case <-time.After(10 * time.Millisecond):
		}
	}
}

func assertRunState(t *testing.T, cs *coordinator.Store, runID, want string) {
	t.Helper()
	var state string
	if err := cs.Pool().QueryRow(context.Background(), `SELECT state FROM runs WHERE id = $1`, runID).Scan(&state); err != nil {
		t.Fatalf("read run state error = %v", err)
	}
	if state != want {
		t.Fatalf("run state = %q, want %q", state, want)
	}
}

func eventCount(t *testing.T, cs *coordinator.Store, sessionID string) int {
	t.Helper()
	var count int
	if err := cs.Pool().QueryRow(context.Background(), `SELECT count(*) FROM events WHERE session_id = $1`, sessionID).Scan(&count); err != nil {
		t.Fatalf("count events error = %v", err)
	}
	return count
}

func outboxCount(t *testing.T, cs *coordinator.Store, runID string, seq int64) int {
	t.Helper()
	var count int
	dedupe := "run:" + runID + ":seq:" + strconv.FormatInt(seq, 10)
	if err := cs.Pool().QueryRow(context.Background(), `SELECT count(*) FROM outbox WHERE dedupe_key = $1`, dedupe).Scan(&count); err != nil {
		t.Fatalf("count outbox error = %v", err)
	}
	return count
}
