//go:build component

package postgres

import (
	"context"
	"testing"

	"github.com/palgroup/palai/storage"
)

// TestCommitToolResultRecordsLedgerClassification proves the E10 T7 ledger write (spec §26.6-26.7):
// CommitToolResult now records the tool's DECLARED replay class (copied at execute time) and the
// canonical request hash onto the tool_calls row, alongside the completed state. A redelivered
// tool_call_id is a single-winner no-op (ON CONFLICT DO NOTHING), so the authoritative classified row is
// never overwritten by a re-drive.
func TestCommitToolResultRecordsLedgerClassification(t *testing.T) {
	cs := openHarness(t)
	ctx := context.Background()
	pool := cs.Pool()
	tenant, sessionID, runID := seedRun(t, pool)

	callID := newID("tcall")
	if _, err := cs.CommitToolResult(ctx, tenant, sessionID, "", runID, 5, callID, "palai.workspace.shell",
		[]byte(`{"argv":["ls"]}`), []byte(`{"exit":0}`), "irreversible", "sha256:deadbeef", "tool_call.completed.v1",
		[]byte(`{"run_id":"`+runID+`","tool_call_id":"`+callID+`"}`)); err != nil {
		t.Fatalf("CommitToolResult() error = %v", err)
	}

	var state, replayClass, requestHash string
	if err := pool.QueryRow(storage.WithSystemScope(ctx),
		`SELECT state, replay_class, request_hash FROM tool_calls WHERE id=$1`, callID).
		Scan(&state, &replayClass, &requestHash); err != nil {
		t.Fatalf("read tool_call ledger row error = %v", err)
	}
	if state != "completed" {
		t.Fatalf("tool_call state = %q, want completed", state)
	}
	if replayClass != "irreversible" {
		t.Fatalf("tool_call replay_class = %q, want irreversible (declared, copied at commit)", replayClass)
	}
	if requestHash != "sha256:deadbeef" {
		t.Fatalf("tool_call request_hash = %q, want sha256:deadbeef", requestHash)
	}

	// A redelivered tool_call_id (a reclaim re-committing) is a single-winner no-op: the classified row
	// is authoritative and never overwritten, even if the re-drive declares a different class.
	if _, err := cs.CommitToolResult(ctx, tenant, sessionID, "", runID, 6, callID, "palai.workspace.shell",
		[]byte(`{"argv":["ls"]}`), []byte(`{"exit":0}`), "pure", "sha256:other", "tool_call.completed.v1",
		[]byte(`{"run_id":"`+runID+`","tool_call_id":"`+callID+`"}`)); err != nil {
		t.Fatalf("re-CommitToolResult() error = %v", err)
	}
	if err := pool.QueryRow(storage.WithSystemScope(ctx), `SELECT replay_class, request_hash FROM tool_calls WHERE id=$1`, callID).
		Scan(&replayClass, &requestHash); err != nil {
		t.Fatalf("re-read tool_call ledger row error = %v", err)
	}
	if replayClass != "irreversible" || requestHash != "sha256:deadbeef" {
		t.Fatalf("after re-commit = {class:%q hash:%q}, want the original {irreversible sha256:deadbeef} (authoritative)", replayClass, requestHash)
	}
}
