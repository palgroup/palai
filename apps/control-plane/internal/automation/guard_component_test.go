//go:build component

// Real-PostgreSQL component tests for the post-irreversible concurrency guard (spec §32.6, E10 tool ledger,
// E11 Task 6). replace/coalesce must NOT cancel or subsume an active run that already EXECUTED an
// irreversible tool action (a completed/uncertain tool_calls row of replay_class='irreversible') without a
// reconciliation contract — the admission is REJECTED, fail-closed, and the active run is left intact. A
// 'pure' class run still allows a normal replace cancel+admit (the control group).
package automation

import (
	"context"
	"strings"
	"testing"

	"github.com/palgroup/palai/storage"
)

// TestReplaceDeniedAfterIrreversibleToolCall pins B6 (replace half): a replace against a key whose active
// run executed an irreversible tool action is rejected with a visible reason, and the active run is NOT
// canceled — while a 'pure'-class run still allows the normal replace cancel+admit.
func TestReplaceDeniedAfterIrreversibleToolCall(t *testing.T) {
	store, pool := wiredTriggerStore(t)
	ctx := context.Background()
	org, project, _ := seedSession(t, pool)
	principal := seedPrincipal(t, pool, org, project)

	for _, ledgerState := range []string{"completed", "uncertain"} {
		t.Run("irreversible/"+ledgerState, func(t *testing.T) {
			triggerID, _ := seedTrigger(t, store, org, project, "replace-irr-"+ledgerState, TriggerRevisionInput{
				ConcurrencyPolicy: "replace", CorrelationKeyExpr: `{"select":"key"}`,
			})
			first, err := store.CreateDelivery(ctx, org, project, principal, triggerID, []byte(`{"key":"k"}`))
			if err != nil {
				t.Fatalf("first delivery error = %v", err)
			}
			mustExec(t, pool,
				`INSERT INTO tool_calls (id, organization_id, project_id, run_id, name, replay_class, state) VALUES ($1,$2,$3,$4,'send_email',$5,$6)`,
				randID("tc"), org, project, first.RunID, "irreversible", ledgerState)

			second, err := store.CreateDelivery(ctx, org, project, principal, triggerID, []byte(`{"key":"k"}`))
			if err != nil {
				t.Fatalf("second delivery error = %v", err)
			}
			if second.State != "rejected" {
				t.Fatalf("replace after irreversible tool call state = %q, want rejected", second.State)
			}
			if !strings.Contains(second.Reason, "irreversible") {
				t.Fatalf("rejection reason = %q, want it to name the irreversible action", second.Reason)
			}
			if second.RunID != "" {
				t.Fatal("a rejected replace must not admit a new run")
			}
			// The active run was NOT canceled — a run that performed an irreversible side effect is left intact.
			var firstState string
			if err := pool.QueryRow(storage.WithSystemScope(ctx), `SELECT state FROM runs WHERE id=$1`, first.RunID).Scan(&firstState); err != nil {
				t.Fatalf("read first run state error = %v", err)
			}
			if firstState == "canceled" {
				t.Fatalf("the active run was canceled despite an irreversible tool action; it must stay intact")
			}
		})
	}

	// Control group: a 'pure'-class run does NOT block a replace — the normal cancel+admit still happens.
	t.Run("pure class allows normal replace", func(t *testing.T) {
		triggerID, _ := seedTrigger(t, store, org, project, "replace-pure", TriggerRevisionInput{
			ConcurrencyPolicy: "replace", CorrelationKeyExpr: `{"select":"key"}`,
		})
		first, err := store.CreateDelivery(ctx, org, project, principal, triggerID, []byte(`{"key":"k"}`))
		if err != nil {
			t.Fatalf("first delivery error = %v", err)
		}
		mustExec(t, pool,
			`INSERT INTO tool_calls (id, organization_id, project_id, run_id, name, replay_class, state) VALUES ($1,$2,$3,$4,'noop',$5,$6)`,
			randID("tc"), org, project, first.RunID, "pure", "completed")

		second, err := store.CreateDelivery(ctx, org, project, principal, triggerID, []byte(`{"key":"k"}`))
		if err != nil {
			t.Fatalf("second delivery error = %v", err)
		}
		if second.State != "run_created" {
			t.Fatalf("pure-class replace state = %q, want run_created (normal replace)", second.State)
		}
		var firstState string
		if err := pool.QueryRow(storage.WithSystemScope(ctx), `SELECT state FROM runs WHERE id=$1`, first.RunID).Scan(&firstState); err != nil {
			t.Fatalf("read first run state error = %v", err)
		}
		if firstState != "canceled" {
			t.Fatalf("pure-class replaced run state = %q, want canceled", firstState)
		}
	})
}

// TestCoalesceDeniedAfterIrreversibleToolCall pins B6 (coalesce half): a coalesce whose key's active run
// executed an irreversible tool action is REJECTED (not deferred — §32.6 fail-closed: an irreversible run's
// events are not silently coalesced away).
func TestCoalesceDeniedAfterIrreversibleToolCall(t *testing.T) {
	store, pool := wiredTriggerStore(t)
	ctx := context.Background()
	org, project, _ := seedSession(t, pool)
	principal := seedPrincipal(t, pool, org, project)

	triggerID, _ := seedTrigger(t, store, org, project, "coalesce-irr", TriggerRevisionInput{
		ConcurrencyPolicy: "coalesce", CorrelationKeyExpr: `{"select":"key"}`,
	})
	first, err := store.CreateDelivery(ctx, org, project, principal, triggerID, []byte(`{"key":"k"}`))
	if err != nil {
		t.Fatalf("first delivery error = %v", err)
	}
	mustExec(t, pool,
		`INSERT INTO tool_calls (id, organization_id, project_id, run_id, name, replay_class, state) VALUES ($1,$2,$3,$4,'charge_card',$5,$6)`,
		randID("tc"), org, project, first.RunID, "irreversible", "completed")

	second, err := store.CreateDelivery(ctx, org, project, principal, triggerID, []byte(`{"key":"k"}`))
	if err != nil {
		t.Fatalf("second delivery error = %v", err)
	}
	if second.State != "rejected" {
		t.Fatalf("coalesce after irreversible tool call state = %q, want rejected (not deferred)", second.State)
	}
	if !strings.Contains(second.Reason, "irreversible") {
		t.Fatalf("rejection reason = %q, want it to name the irreversible action", second.Reason)
	}
}
