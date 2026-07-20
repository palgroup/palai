package tools

import (
	"context"
	"os"
	"path/filepath"
	"syscall"
	"testing"

	toolbroker "github.com/palgroup/palai/packages/tool-broker"
)

// TestFileToolDeniesWorkspaceEscape proves the SAN-001 confinement against a real filesystem: every
// path resolves relative to the allocation root, and a traversal, an absolute path, an escaping
// symlink, and a non-regular target (a socket) are all denied — while an in-workspace write and read
// succeed. The file tool never touches a byte outside /workspace.
func TestFileToolDeniesWorkspaceEscape(t *testing.T) {
	root := realTempDir(t)
	tool := FileTool()
	env := toolbroker.ExecEnv{WorkspaceRoot: root}

	// A symlink inside the workspace that points outside it, and a non-regular special file (a fifo,
	// standing in for a device/socket/runtime interface) inside it.
	if err := os.Symlink("/etc", filepath.Join(root, "escape")); err != nil {
		t.Fatalf("plant escaping symlink: %v", err)
	}
	if err := syscall.Mkfifo(filepath.Join(root, "runtime.fifo"), 0o600); err != nil {
		t.Fatalf("plant fifo: %v", err)
	}

	denied := []map[string]any{
		{"op": "read", "path": "../../../etc/passwd"},             // traversal
		{"op": "read", "path": "/etc/passwd"},                     // absolute
		{"op": "write", "path": "../outside.txt", "content": "x"}, // traversal write
		{"op": "read", "path": "escape/passwd"},                   // escaping symlink
		{"op": "read", "path": "runtime.fifo"},                    // non-regular special file — devices/sockets/fifos refused
		{"op": "stat", "path": "../.."},                           // traversal stat
	}
	for _, args := range denied {
		if _, err := tool.Exec(context.Background(), env, args); err == nil {
			t.Fatalf("file op %v was allowed, want denied", args)
		}
	}

	// The confinement does not break legitimate in-workspace work.
	if _, err := tool.Exec(context.Background(), env, map[string]any{"op": "write", "path": "repo/main.go", "content": "package main"}); err != nil {
		t.Fatalf("in-workspace write denied: %v", err)
	}
	out, err := tool.Exec(context.Background(), env, map[string]any{"op": "read", "path": "repo/main.go"})
	if err != nil {
		t.Fatalf("in-workspace read denied: %v", err)
	}
	if out["content"] != "package main" {
		t.Fatalf("read content = %v, want the bytes just written", out["content"])
	}

	// A likely-secret read is refused even inside the workspace.
	if err := os.WriteFile(filepath.Join(root, ".env"), []byte("OPENAI_API_KEY=sk-x"), 0o600); err != nil {
		t.Fatalf("stage secret: %v", err)
	}
	if _, err := tool.Exec(context.Background(), env, map[string]any{"op": "read", "path": ".env"}); err == nil {
		t.Fatalf("reading .env was allowed, want refused")
	}
}

// TestFileWriteReportsBeforeAfterHashAndChangedPaths proves the write report the changeset consumes
// (spec §28.7): a new file reports an empty before-hash and created=true; a rewrite reports the
// prior content hash as before and the new content hash as after, with created=false.
func TestFileWriteReportsBeforeAfterHashAndChangedPaths(t *testing.T) {
	root := realTempDir(t)
	tool := FileTool()
	env := toolbroker.ExecEnv{WorkspaceRoot: root}

	first, err := tool.Exec(context.Background(), env, map[string]any{"op": "write", "path": "repo/app.py", "content": "print(1)\n"})
	if err != nil {
		t.Fatalf("first write: %v", err)
	}
	if first["created"] != true {
		t.Fatalf("first write created = %v, want true", first["created"])
	}
	if first["before_hash"] != "" {
		t.Fatalf("first write before_hash = %v, want empty for a new file", first["before_hash"])
	}
	if first["path"] != "repo/app.py" {
		t.Fatalf("write path = %v, want the changed workspace-relative path", first["path"])
	}
	afterFirst := first["after_hash"].(string)
	if afterFirst == "" {
		t.Fatal("first write reported no after_hash")
	}

	second, err := tool.Exec(context.Background(), env, map[string]any{"op": "write", "path": "repo/app.py", "content": "print(2)\n"})
	if err != nil {
		t.Fatalf("rewrite: %v", err)
	}
	if second["created"] != false {
		t.Fatalf("rewrite created = %v, want false", second["created"])
	}
	if second["before_hash"] != afterFirst {
		t.Fatalf("rewrite before_hash = %v, want the prior after_hash %v", second["before_hash"], afterFirst)
	}
	if second["after_hash"] == afterFirst {
		t.Fatal("rewrite after_hash did not change with new content")
	}
}

// realTempDir returns a symlink-resolved temp dir so the workspace root the confinement resolves
// matches the path the test plants files under (macOS /tmp is itself a symlink).
func realTempDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	resolved, err := filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatalf("resolve temp dir: %v", err)
	}
	return resolved
}
