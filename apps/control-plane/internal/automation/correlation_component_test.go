//go:build component

package automation

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

// TestCorrelationModesScopedAndBounded pins the four correlation modes (spec §20.2.2): per_event opens a
// fresh session; bounded_key_reuse chains a same-key delivery onto the prior session (authz not bypassed
// — only this tenant's deliveries are queried, only the hash is stored); named_session appends the mapped
// input to an existing named session via the send_message accept path (no new command kind, no new run);
// reject_if_active rejects when the correlated session still holds an active root run.
func TestCorrelationModesScopedAndBounded(t *testing.T) {
	store, pool := wiredTriggerStore(t)
	ctx := context.Background()
	org, project, _ := seedSession(t, pool)
	principal := seedPrincipal(t, pool, org, project)

	t.Run("per_event opens a fresh session", func(t *testing.T) {
		triggerID, _ := seedTrigger(t, store, org, project, "per-event", TriggerRevisionInput{CorrelationMode: "per_event"})
		a, err := store.CreateDelivery(ctx, org, project, principal, triggerID, []byte(`{}`))
		if err != nil {
			t.Fatalf("first delivery error = %v", err)
		}
		b, err := store.CreateDelivery(ctx, org, project, principal, triggerID, []byte(`{}`))
		if err != nil {
			t.Fatalf("second delivery error = %v", err)
		}
		if a.SessionID == "" || b.SessionID == "" || a.SessionID == b.SessionID {
			t.Fatalf("per_event sessions = %q, %q; want two distinct non-empty sessions", a.SessionID, b.SessionID)
		}
	})

	t.Run("bounded_key_reuse chains onto the prior session", func(t *testing.T) {
		triggerID, _ := seedTrigger(t, store, org, project, "bounded", TriggerRevisionInput{
			CorrelationMode: "bounded_key_reuse", CorrelationKeyExpr: `{"select":"corr"}`,
		})
		first, err := store.CreateDelivery(ctx, org, project, principal, triggerID, []byte(`{"corr":"c1"}`))
		if err != nil {
			t.Fatalf("first delivery error = %v", err)
		}
		// Complete the first run so the chained second delivery is not blocked by one-active-root.
		completeRun(t, pool, first.RunID)

		second, err := store.CreateDelivery(ctx, org, project, principal, triggerID, []byte(`{"corr":"c1"}`))
		if err != nil {
			t.Fatalf("second delivery error = %v", err)
		}
		if second.State != "run_created" {
			t.Fatalf("chained delivery state = %q, want run_created", second.State)
		}
		if second.SessionID != first.SessionID {
			t.Fatalf("chained session = %q, want the prior %q", second.SessionID, first.SessionID)
		}
		if second.RunID == first.RunID {
			t.Fatal("chained delivery reused the run id; want a new run in the same session")
		}
	})

	t.Run("named_session appends to an existing session", func(t *testing.T) {
		// An ongoing session with an active root run — created by a per_event delivery.
		host, _ := seedTrigger(t, store, org, project, "host", TriggerRevisionInput{CorrelationMode: "per_event"})
		ongoing, err := store.CreateDelivery(ctx, org, project, principal, host, []byte(`{}`))
		if err != nil {
			t.Fatalf("host delivery error = %v", err)
		}

		named, _ := seedTrigger(t, store, org, project, "named", TriggerRevisionInput{
			CorrelationMode: "named_session", CorrelationKeyExpr: `{"select":"target"}`,
		})
		del, err := store.CreateDelivery(ctx, org, project, principal, named, []byte(`{"target":"`+ongoing.SessionID+`"}`))
		if err != nil {
			t.Fatalf("named_session delivery error = %v", err)
		}
		if del.State != "run_created" {
			t.Fatalf("named_session delivery state = %q, want run_created", del.State)
		}
		if del.SessionID != ongoing.SessionID {
			t.Fatalf("named_session appended to %q, want the ongoing %q", del.SessionID, ongoing.SessionID)
		}
		if del.RunID != ongoing.RunID {
			t.Fatalf("named_session joined run %q, want the ongoing run %q (no new run)", del.RunID, ongoing.RunID)
		}
		// A send_message command was queued on the ongoing session (the existing accept path, no new kind).
		if got := count(t, pool, `SELECT count(*) FROM commands WHERE session_id=$1 AND kind='send_message'`, ongoing.SessionID); got != 1 {
			t.Fatalf("send_message commands on the named session = %d, want 1", got)
		}
		// Exactly one run in the session — the named delivery joined it, it did not create a second.
		if got := count(t, pool, `SELECT count(*) FROM runs WHERE session_id=$1`, ongoing.SessionID); got != 1 {
			t.Fatalf("runs in the named session = %d, want 1 (append, not a new run)", got)
		}
	})

	t.Run("reject_if_active rejects a busy session", func(t *testing.T) {
		triggerID, _ := seedTrigger(t, store, org, project, "reject", TriggerRevisionInput{
			CorrelationMode: "reject_if_active", CorrelationKeyExpr: `{"select":"corr"}`,
		})
		if _, err := store.CreateDelivery(ctx, org, project, principal, triggerID, []byte(`{"corr":"c2"}`)); err != nil {
			t.Fatalf("first delivery error = %v", err)
		}
		// The first delivery's run is queued (active); a same-key delivery is rejected, not queued.
		second, err := store.CreateDelivery(ctx, org, project, principal, triggerID, []byte(`{"corr":"c2"}`))
		if err != nil {
			t.Fatalf("second delivery error = %v", err)
		}
		if second.State != "rejected" {
			t.Fatalf("reject_if_active second delivery state = %q, want rejected", second.State)
		}
		if second.RunID != "" {
			t.Fatal("a rejected delivery must not create a run")
		}
	})
}

// completeRun terminalizes a run so a chained delivery is not blocked by one-active-root.
func completeRun(t *testing.T, pool *pgxpool.Pool, runID string) {
	t.Helper()
	if _, err := pool.Exec(context.Background(), `UPDATE runs SET state='completed' WHERE id=$1`, runID); err != nil {
		t.Fatalf("complete run error = %v", err)
	}
}
