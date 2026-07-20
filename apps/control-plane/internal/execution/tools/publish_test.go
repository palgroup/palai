package tools

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/palgroup/palai/adapters/sandboxes/oci/workspace"
	toolbroker "github.com/palgroup/palai/packages/tool-broker"
)

// fakePublicationRegistry records the last publication op a tool requested, so a tool test can assert
// the tool computed the workspace head and asked for the right operation without a database.
type fakePublicationRegistry struct {
	lastScope toolbroker.TaskScope
	lastOp    map[string]any
}

func (f *fakePublicationRegistry) RequestPublication(_ context.Context, scope toolbroker.TaskScope, op map[string]any) (map[string]any, error) {
	f.lastScope, f.lastOp = scope, op
	return map[string]any{"status": "pending_approval", "operation": op["operation"]}, nil
}

// TestPushToolRecordsPendingPublicationAtWorkspaceHead proves the push tool (spec §30.9): it does NOT
// push — it computes the workspace repo's current head and records a PENDING push publication through
// the registry (returning pending_approval), so the push is gated behind an approval. The model never
// supplies a head or destination.
func TestPushToolRecordsPendingPublicationAtWorkspaceHead(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skipf("git not found: %v", err)
	}
	root := realTempDir(t)
	repoDir := filepath.Join(root, workspace.RepoDir)
	initRepo(t, repoDir)
	if err := os.WriteFile(filepath.Join(repoDir, "edit.txt"), []byte("agent edit\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	run := repoGit(t, repoDir)
	run("add", "edit.txt")
	run("commit", "-q", "-m", "agent edit")
	head := run("rev-parse", "HEAD")

	reg := &fakePublicationRegistry{}
	out, err := pushExec(context.Background(), toolbroker.ExecEnv{WorkspaceRoot: root, Publications: reg}, nil)
	if err != nil {
		t.Fatalf("pushExec() error = %v", err)
	}
	if status, _ := out["status"].(string); status != "pending_approval" {
		t.Fatalf("push tool result status = %q, want pending_approval (a push is gated, not immediate)", status)
	}
	if op, _ := reg.lastOp["operation"].(string); op != "push_branch" {
		t.Fatalf("recorded operation = %q, want push_branch", op)
	}
	if got, _ := reg.lastOp["head_sha"].(string); got != head {
		t.Fatalf("recorded head_sha = %q, want the workspace repo head %q", got, head)
	}
}

// TestPushToolFailsCleanlyWithoutRegistry proves the push tool fails cleanly rather than acting when no
// publication registry is wired (the SetShellRunner-nil discipline).
func TestPushToolFailsCleanlyWithoutRegistry(t *testing.T) {
	if _, err := pushExec(context.Background(), toolbroker.ExecEnv{WorkspaceRoot: t.TempDir()}, nil); err == nil {
		t.Fatal("pushExec with no registry = nil error, want a clean failure")
	}
}
