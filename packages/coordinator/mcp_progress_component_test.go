//go:build component

package coordinator

import (
	"context"
	"os"
	"testing"
)

// TestMCPProgressEmitsAdvisoryToolCallProgressEvent proves an MCP tools/call progress notification journals a
// tool_call.progress.v1 event WITHOUT any tool-call state-machine transition (advisory): the event lands in
// the session journal carrying the tool_call_id it correlates to, and no tool_calls row is created.
func TestMCPProgressEmitsAdvisoryToolCallProgressEvent(t *testing.T) {
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

	org, project, session := pinTestID("org"), pinTestID("prj"), pinTestID("ses")
	mustExecPin(t, cs, `INSERT INTO organizations (id) VALUES ($1)`, org)
	mustExecPin(t, cs, `INSERT INTO projects (id, organization_id) VALUES ($1,$2)`, project, org)
	mustExecPin(t, cs, `INSERT INTO sessions (id, organization_id, project_id) VALUES ($1,$2,$3)`, session, org, project)

	tenant := Tenant{Organization: org, Project: project}
	if err := cs.AppendToolProgress(ctx, tenant, session, "", "tc_progress_1", 2, 5, "halfway"); err != nil {
		t.Fatalf("AppendToolProgress: %v", err)
	}

	// The advisory event is in the journal, scoped to the session, carrying the tool_call_id.
	var count int
	if err := cs.pool.QueryRow(ctx,
		`SELECT count(*) FROM events WHERE session_id=$1 AND type='tool_call.progress.v1' AND payload->>'tool_call_id'='tc_progress_1'`,
		session).Scan(&count); err != nil {
		t.Fatalf("read progress event: %v", err)
	}
	if count != 1 {
		t.Fatalf("tool_call.progress.v1 events = %d, want 1", count)
	}
	// No tool-call row was created — progress advances no state machine.
	var toolRows int
	if err := cs.pool.QueryRow(ctx, `SELECT count(*) FROM tool_calls WHERE id='tc_progress_1'`).Scan(&toolRows); err != nil {
		t.Fatalf("read tool_calls: %v", err)
	}
	if toolRows != 0 {
		t.Fatalf("tool_calls rows for tc_progress_1 = %d, want 0 (advisory, no state transition)", toolRows)
	}
}

func mustExecPin(t *testing.T, cs *Store, sql string, args ...any) {
	t.Helper()
	if _, err := cs.pool.Exec(context.Background(), sql, args...); err != nil {
		t.Fatalf("exec %q: %v", sql, err)
	}
}
