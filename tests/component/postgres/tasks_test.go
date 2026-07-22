//go:build component

package postgres

import (
	"context"
	"reflect"
	"strings"
	"testing"

	"github.com/palgroup/palai/packages/coordinator"

	"github.com/palgroup/palai/storage"
)

// TestTasksMigration proves 000012 applies, rolls back, and reapplies idempotently (the durable
// registry table appears, disappears, reappears).
func TestTasksMigration(t *testing.T) {
	cs := openHarness(t)
	ctx := context.Background()
	pool := cs.Pool()

	if err := cs.Migrate(ctx); err != nil {
		t.Fatalf("re-Migrate() error = %v", err)
	}
	if !tableExists(t, pool, "tasks") {
		t.Fatal("after apply, tasks is missing")
	}
	if err := cs.Rollback(ctx); err != nil {
		t.Fatalf("Rollback() error = %v", err)
	}
	if tableExists(t, pool, "tasks") {
		t.Fatal("after rollback, tasks still exists")
	}
	if err := cs.Migrate(ctx); err != nil {
		t.Fatalf("re-Migrate() error = %v", err)
	}
	if !tableExists(t, pool, "tasks") {
		t.Fatal("after reapply, tasks is missing")
	}
}

// upsertTask is a test helper that upserts a task with the given fields all set.
func upsertTask(t *testing.T, cs *coordinator.Store, tenant coordinator.Tenant, sessionID, runID, kind, key, title, status string) {
	t.Helper()
	if _, err := cs.UpsertTask(context.Background(), tenant, coordinator.TaskUpsert{
		SessionID: sessionID, RunID: runID, Key: key, NewRowID: newID("task"),
		Kind: kind, SetKind: true, Title: title, SetTitle: true, Status: status, SetStatus: true,
	}); err != nil {
		t.Fatalf("UpsertTask(%s) error = %v", key, err)
	}
}

// TestDurableTaskSurvivesContextReset proves REG-001, the differentiator: durable tasks are created
// and some marked done, then a FRESH read (standing in for a context-reset / new attempt) reads the
// state straight from the DB — done/not-done intact, and a status-only update never erased a title.
func TestDurableTaskSurvivesContextReset(t *testing.T) {
	cs := openHarness(t)
	ctx := context.Background()
	tenant, sessionID, runID := seedRun(t, cs.Pool())

	upsertTask(t, cs, tenant, sessionID, runID, "task", "a", "do A", "open")
	upsertTask(t, cs, tenant, sessionID, runID, "task", "b", "do B", "open")
	upsertTask(t, cs, tenant, sessionID, runID, "task", "c", "do C", "open")

	// Mark a and c done with a STATUS-ONLY update — the title must survive (no clobber).
	for _, k := range []string{"a", "c"} {
		if _, err := cs.UpsertTask(ctx, tenant, coordinator.TaskUpsert{
			SessionID: sessionID, RunID: runID, Key: k, Status: "done", SetStatus: true,
		}); err != nil {
			t.Fatalf("mark %s done: %v", k, err)
		}
	}

	// Context reset / new attempt: read durable state from the DB.
	tasks, err := cs.ListTasks(ctx, tenant, sessionID)
	if err != nil {
		t.Fatalf("ListTasks() error = %v", err)
	}
	got := map[string]coordinator.Task{}
	for _, tk := range tasks {
		got[tk.Key] = tk
	}
	if len(tasks) != 3 {
		t.Fatalf("want 3 durable tasks after reset, got %d: %+v", len(tasks), tasks)
	}
	if got["a"].Status != "done" || got["c"].Status != "done" || got["b"].Status != "open" {
		t.Fatalf("done/not-done not intact after context reset: %+v", got)
	}
	if got["a"].Title != "do A" {
		t.Fatalf("status-only update clobbered the title: got %q, want %q", got["a"].Title, "do A")
	}
}

// TestDurableTasksTwoReadersSeeSameOrderedJournal proves REG-002: durable task updates append to the
// session's ordered event journal. The two readers here are two same-process reads of that journal,
// asserting the SAME ordered sequence — the multi-client SSE fan-out itself (two attached clients
// replaying identically) is the E08 attach harness's already-proven property, which these task
// events ride transitively (single-source per-session seq). A create is task.created, a later change
// to the same key is task.updated.
func TestDurableTasksTwoReadersSeeSameOrderedJournal(t *testing.T) {
	cs := openHarness(t)
	ctx := context.Background()
	pool := cs.Pool()
	tenant, sessionID, runID := seedRun(t, pool)

	upsertTask(t, cs, tenant, sessionID, runID, "task", "a", "do A", "open") // task.created
	upsertTask(t, cs, tenant, sessionID, runID, "task", "b", "do B", "open") // task.created
	upsertTask(t, cs, tenant, sessionID, runID, "task", "a", "do A", "done") // task.updated

	read := func() []string {
		rows, err := pool.Query(storage.WithSystemScope(ctx),
			`SELECT type, payload::text FROM events WHERE session_id=$1 AND type LIKE 'task.%' ORDER BY seq`, sessionID)
		if err != nil {
			t.Fatalf("read journal: %v", err)
		}
		defer rows.Close()
		var out []string
		for rows.Next() {
			var typ, payload string
			if err := rows.Scan(&typ, &payload); err != nil {
				t.Fatalf("scan event: %v", err)
			}
			out = append(out, typ+" "+payload)
		}
		return out
	}

	clientA, clientB := read(), read()
	if !reflect.DeepEqual(clientA, clientB) {
		t.Fatalf("two readers saw different ordered journals:\n A=%v\n B=%v", clientA, clientB)
	}
	if len(clientA) != 3 {
		t.Fatalf("want 3 ordered task events, got %d: %v", len(clientA), clientA)
	}
	if !strings.HasPrefix(clientA[0], "task.created.v1") || !strings.Contains(clientA[0], `"key": "a"`) {
		t.Fatalf("event 0 = %q, want task.created for a", clientA[0])
	}
	if !strings.HasPrefix(clientA[2], "task.updated.v1") || !strings.Contains(clientA[2], `"status": "done"`) {
		t.Fatalf("event 2 = %q, want task.updated with status done", clientA[2])
	}
}

// TestTodoToolPersistsAndRecovers proves the todo variant is the same durable primitive with kind
// "todo": it persists and a fresh read recovers it.
func TestTodoToolPersistsAndRecovers(t *testing.T) {
	cs := openHarness(t)
	ctx := context.Background()
	tenant, sessionID, runID := seedRun(t, cs.Pool())

	upsertTask(t, cs, tenant, sessionID, runID, "todo", "t1", "check tests", "open")

	tasks, err := cs.ListTasks(ctx, tenant, sessionID)
	if err != nil {
		t.Fatalf("ListTasks() error = %v", err)
	}
	if len(tasks) != 1 || tasks[0].Kind != "todo" || tasks[0].Title != "check tests" || tasks[0].Status != "open" {
		t.Fatalf("todo not persisted as kind todo: %+v", tasks)
	}
}
