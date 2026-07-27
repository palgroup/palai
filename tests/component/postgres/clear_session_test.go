//go:build component

package postgres

import (
	"context"
	"testing"

	"github.com/palgroup/palai/packages/coordinator"
	"github.com/palgroup/palai/storage"
)

// TestClearSessionEmptiesHistoryAndKeepsTheSessionAlive is the durable half of E21 T1's reset.
//
// A new Slack thread is ALREADY a clear — correlation is (team, channel, thread_ts), so a new thread
// is a new session and carries nothing. That is free and it is the answer most people want. `clear`
// is for the other case: the person wants to keep talking HERE, in this thread, with the history
// dropped. So the two halves it must prove are exactly the two halves of that sentence — the history
// goes, and the session stays.
//
// It is a real-Postgres test rather than a unit test because the claim is durability: the command is
// a row, the retraction is a row, and they commit together or the reset is a lie after a restart.
func TestClearSessionEmptiesHistoryAndKeepsTheSessionAlive(t *testing.T) {
	cs := openHarness(t)
	ctx := context.Background()
	sys := storage.WithSystemScope(ctx) // raw fixture reads bypass RLS the way exec() does
	pool := cs.Pool()
	tenant, sessionID, runID := seedRun(t, pool)
	// The seeded run is finished: this is a thread with history and nothing in flight.
	exec(t, pool, `UPDATE runs SET state = 'completed' WHERE id = $1`, runID)

	// Three settled turns, then the response the NEXT run will be — SessionHistory is read relative
	// to it, so it is the boundary every assertion here is made against.
	seedResponse(t, pool, tenant, sessionID, "completed")
	seedResponse(t, pool, tenant, sessionID, "completed")
	seedResponse(t, pool, tenant, sessionID, "completed")
	next := seedResponse(t, pool, tenant, sessionID, "queued")

	before, err := cs.SessionHistory(ctx, tenant, sessionID, next)
	if err != nil {
		t.Fatalf("SessionHistory() before clear error = %v", err)
	}
	if len(before) != 3 {
		t.Fatalf("history before clear = %d turns, want 3 (the fixture)", len(before))
	}

	cmd, err := cs.AcceptCommand(ctx, tenant, sessionID, coordinator.CommandInput{
		CommandID: newID("cmd"), Kind: "clear",
		// What the HTTP adapter marshals for every non-change_config kind (store/postgres.go).
		Payload: []byte(`{"message":""}`),
	})
	if err != nil {
		t.Fatalf("AcceptCommand(clear) error = %v", err)
	}
	if cmd.SessionNotFound {
		t.Fatal("clear reported the session as not found")
	}
	if cmd.State != "applied" {
		t.Fatalf("clear command state = %q, want applied — a clear that stays queued has reset nothing", cmd.State)
	}

	after, err := cs.SessionHistory(ctx, tenant, sessionID, next)
	if err != nil {
		t.Fatalf("SessionHistory() after clear error = %v", err)
	}
	if len(after) != 0 {
		t.Fatalf("history after clear = %d turns, want 0", len(after))
	}

	// The session LIVES. This is the whole difference from close_session: the thread keeps working,
	// the next message lands in the same session, and it starts from nothing.
	var state string
	if err := pool.QueryRow(sys, `SELECT state FROM sessions WHERE id = $1`, sessionID).Scan(&state); err != nil {
		t.Fatalf("read session state: %v", err)
	}
	if state != "active" {
		t.Fatalf("session state after clear = %q, want active — clear resets the history, it does not close the thread", state)
	}

	// Durable, and durable means re-readable by a process that was not there: the command row and the
	// retraction are both committed, so a control plane restarted after this reads the same emptiness.
	// A fresh Store over the same database is that process.
	fresh := openHarness(t)
	rehydrated, err := fresh.SessionHistory(ctx, tenant, sessionID, next)
	if err != nil {
		t.Fatalf("SessionHistory() from a fresh store error = %v", err)
	}
	if len(rehydrated) != 0 {
		t.Fatalf("history read back by a fresh store = %d turns, want 0: the clear did not survive", len(rehydrated))
	}

	// And the rows survive: clear withdraws turns from the conversation, it does not destroy the
	// record. An operator asking what was said still has an answer.
	var rows int
	if err := pool.QueryRow(sys, `SELECT count(*) FROM responses WHERE session_id = $1`, sessionID).Scan(&rows); err != nil {
		t.Fatalf("count responses: %v", err)
	}
	if rows != 4 {
		t.Fatalf("responses remaining = %d, want 4 — clear must retract, never delete", rows)
	}
}

// TestClearSessionIsIdempotentAndLeavesLiveWorkAlone. A redelivered clear must not be a second
// truth, and a clear must not reach into a run that is still going: the in-flight response is not
// history yet, and retracting it would delete an answer the person is waiting for out from under
// the run producing it.
func TestClearSessionIsIdempotentAndLeavesLiveWorkAlone(t *testing.T) {
	cs := openHarness(t)
	ctx := context.Background()
	sys := storage.WithSystemScope(ctx) // raw fixture reads bypass RLS the way exec() does
	pool := cs.Pool()
	tenant, sessionID, runID := seedRun(t, pool)

	seedResponse(t, pool, tenant, sessionID, "completed")
	live := seedResponse(t, pool, tenant, sessionID, "in_progress")
	next := seedResponse(t, pool, tenant, sessionID, "queued")
	// The seeded run is the live one, producing `live` — a clear must not reach into it.
	exec(t, pool, `UPDATE runs SET state = 'running', response_id = $1 WHERE id = $2`, live, runID)

	for i := range 2 {
		if _, err := cs.AcceptCommand(ctx, tenant, sessionID, coordinator.CommandInput{
			CommandID: newID("cmd"), Kind: "clear",
			// What the HTTP adapter marshals for every non-change_config kind (store/postgres.go).
			Payload: []byte(`{"message":""}`),
		}); err != nil {
			t.Fatalf("AcceptCommand(clear) #%d error = %v", i+1, err)
		}
	}

	var retracted *string
	if err := pool.QueryRow(sys, `SELECT retracted_at::text FROM responses WHERE id = $1`, live).Scan(&retracted); err != nil {
		t.Fatalf("read live response: %v", err)
	}
	if retracted != nil {
		t.Fatal("clear retracted a response that was still running; a reset is about settled history, not live work")
	}

	history, err := cs.SessionHistory(ctx, tenant, sessionID, next)
	if err != nil {
		t.Fatalf("SessionHistory() error = %v", err)
	}
	// Only the live response survives the clear, and it carries no output yet, so the assembled
	// history the next run sees is still empty of settled turns.
	if len(history) != 1 {
		t.Fatalf("history after two clears = %d rows, want 1 (the untouched live response)", len(history))
	}
}
