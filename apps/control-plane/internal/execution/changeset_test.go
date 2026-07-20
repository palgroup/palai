package execution

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/palgroup/palai/packages/coordinator"
)

// fakeChangesetLedger is the coordinator seam under test: a fixed base + tool ledger in, the recorded
// changeset captured out. No database — the projection is provable from the ledger alone (REP-005).
type fakeChangesetLedger struct {
	base     string
	baseOK   bool
	rows     []coordinator.ToolCallRow
	recorded *coordinator.ChangesetRecord
}

func (f *fakeChangesetLedger) RunToolCalls(context.Context, coordinator.Tenant, string) ([]coordinator.ToolCallRow, error) {
	return f.rows, nil
}
func (f *fakeChangesetLedger) RunBaseCommit(context.Context, coordinator.Tenant, string) (string, bool, error) {
	return f.base, f.baseOK, nil
}
func (f *fakeChangesetLedger) RecordChangeset(_ context.Context, _ coordinator.Tenant, _, _ string, rec coordinator.ChangesetRecord) error {
	f.recorded = &rec
	return nil
}

type recordedWrite struct {
	content     string
	mediaType   string
	logicalType string
}

type fakeArtifactWriter struct{ writes []recordedWrite }

func (f *fakeArtifactWriter) WriteArtifact(_ context.Context, _, _, _ string, content []byte, mediaType, logicalType string, _ map[string]any) (string, error) {
	f.writes = append(f.writes, recordedWrite{content: string(content), mediaType: mediaType, logicalType: logicalType})
	return "art_" + logicalType, nil
}

// TestChangesetCompleteIndependentOfModelSummary proves REP-005 (spec §30.6): the changeset is
// compiled from the file-tool write LEDGER — the changed-file set, patch, checks log, and provenance
// are complete and derived from what the run DID, not from any model prose. The compiler takes no
// model-summary input, and re-compiling the same ledger yields the same content hash, so a differing
// final model response cannot change the changeset.
func TestChangesetCompleteIndependentOfModelSummary(t *testing.T) {
	requireGit(t)
	root, base := newAllocRepo(t) // root/repo is a git repo with f.txt="base" at base
	repoDir := filepath.Join(root, "repo")

	// The run edited the worktree: a new file added, the base file modified — the SAME operations the
	// ledger records below (in a real run the file tool does both).
	writeFile(t, filepath.Join(repoDir, "added.txt"), "brand new\n")
	writeFile(t, filepath.Join(repoDir, "f.txt"), "changed\n")

	ledger := &fakeChangesetLedger{
		base: base, baseOK: true,
		rows: []coordinator.ToolCallRow{
			fileWriteRow("tc_1", "repo/added.txt", "brand new\n", "", "sha256:aa", true),
			fileWriteRow("tc_2", "repo/f.txt", "changed\n", "sha256:old", "sha256:new", false),
			shellRow("tc_3", []string{"go", "test", "./..."}, 0, "ok\n"),
		},
	}
	aw := &fakeArtifactWriter{}
	in := ChangesetInput{Tenant: coordinator.Tenant{Organization: "org", Project: "prj"}, RunID: "run_1", AllocationRoot: root}

	rec, compiled, err := CompileChangeset(context.Background(), ledger, aw, in)
	if err != nil || !compiled {
		t.Fatalf("CompileChangeset() = compiled %v err %v, want compiled", compiled, err)
	}

	// The changed-file set is exactly the ledger's writes, correctly classified, with provenance.
	if len(rec.Files) != 2 {
		t.Fatalf("files = %+v, want the 2 ledger writes", rec.Files)
	}
	byPath := map[string]coordinator.ChangesetFile{}
	for _, f := range rec.Files {
		byPath[f.Path] = f
	}
	if got := byPath["repo/added.txt"]; got.Change != "added" || got.ToolCallID != "tc_1" {
		t.Fatalf("added.txt = %+v, want change=added tool_call=tc_1", got)
	}
	if got := byPath["repo/f.txt"]; got.Change != "modified" || got.ToolCallID != "tc_2" {
		t.Fatalf("f.txt = %+v, want change=modified tool_call=tc_2", got)
	}

	// The patch + test-log artifacts were written with their §22.6 classification, and the record
	// references them.
	if rec.PatchArtifactID == "" || rec.TestLogArtifactID == "" {
		t.Fatalf("record = patch:%q test-log:%q, want both artifact ids", rec.PatchArtifactID, rec.TestLogArtifactID)
	}
	var patch, testLog *recordedWrite
	for i := range aw.writes {
		switch aw.writes[i].logicalType {
		case "patch":
			patch = &aw.writes[i]
		case "test-result":
			testLog = &aw.writes[i]
		}
	}
	if patch == nil || patch.mediaType != "text/x-diff" || !strings.Contains(patch.content, "added.txt") {
		t.Fatalf("patch artifact = %v, want a text/x-diff diff naming added.txt", patch)
	}
	if testLog == nil || !strings.Contains(testLog.content, "go test") {
		t.Fatalf("test-log artifact = %v, want the shell checks transcript", testLog)
	}
	if rec.BaseCommit != base || rec.FinalCommit == "" {
		t.Fatalf("record commits = base:%q final:%q, want base=%s and a final", rec.BaseCommit, rec.FinalCommit, base)
	}

	// Independence: the same ledger recompiles to the same content hash — the changeset is a pure
	// projection of what the run did, so a differing model summary cannot move it.
	aw2 := &fakeArtifactWriter{}
	rec2, _, err := CompileChangeset(context.Background(), ledger, aw2, in)
	if err != nil {
		t.Fatalf("second CompileChangeset() error = %v", err)
	}
	if rec2.ContentHash != rec.ContentHash {
		t.Fatalf("content hash not stable across compiles: %q vs %q", rec.ContentHash, rec2.ContentHash)
	}
	// The id is content-addressed, so it too is stable — the DB primary key dedupes a re-compile.
	if rec2.ID != rec.ID || rec.ID == "" {
		t.Fatalf("changeset id not stable across compiles: %q vs %q", rec.ID, rec2.ID)
	}
}

// TestChangesetFlagsSecretFinding proves the changeset secret scan (spec §30.4/§30.6): a likely secret
// in a file entering the changeset is flagged as a finding with its rule + path — the committed-secret
// detection preparation deferred to here.
func TestChangesetFlagsSecretFinding(t *testing.T) {
	requireGit(t)
	root, base := newAllocRepo(t)
	repoDir := filepath.Join(root, "repo")

	// A config file carrying a token-shaped secret (assembled so no secret-shaped literal sits in
	// source). "Bearer <token>" matches the bearer_token rule.
	secretContent := "auth = \"Bearer " + strings.Repeat("a1b2c3d4", 3) + "\"\n"
	writeFile(t, filepath.Join(repoDir, "config.txt"), secretContent)

	ledger := &fakeChangesetLedger{
		base: base, baseOK: true,
		rows: []coordinator.ToolCallRow{fileWriteRow("tc_1", "repo/config.txt", secretContent, "", "sha256:aa", true)},
	}
	rec, compiled, err := CompileChangeset(context.Background(), ledger, &fakeArtifactWriter{},
		ChangesetInput{Tenant: coordinator.Tenant{Organization: "org", Project: "prj"}, RunID: "run_1", AllocationRoot: root})
	if err != nil || !compiled {
		t.Fatalf("CompileChangeset() = compiled %v err %v", compiled, err)
	}
	if len(rec.Findings) == 0 {
		t.Fatal("no secret finding for a token-carrying file entering the changeset")
	}
	f := rec.Findings[0]
	if f.Path != "repo/config.txt" || f.Rule == "" || f.Kind != "secret" {
		t.Fatalf("finding = %+v, want path=repo/config.txt with a rule and kind=secret", f)
	}
}

// TestChangesetFlagsShellWrittenSecret proves the §30.4 scan closes the shell-tool gap: a secret
// written by the shell tool (echo secret > f) is absent from the file-tool ledger but PRESENT in the
// patch, and scanning the patch flags it. Without the patch scan this secret would ship undetected.
func TestChangesetFlagsShellWrittenSecret(t *testing.T) {
	requireGit(t)
	root, base := newAllocRepo(t)
	repoDir := filepath.Join(root, "repo")

	// A secret file that reached the worktree via a shell command — there is NO file-tool write for it.
	secretContent := "token=\"Bearer " + strings.Repeat("a1b2c3d4", 3) + "\"\n"
	writeFile(t, filepath.Join(repoDir, "secret.env"), secretContent)

	ledger := &fakeChangesetLedger{
		base: base, baseOK: true,
		rows: []coordinator.ToolCallRow{shellRow("tc_1", []string{"sh", "-c", "echo secret > secret.env"}, 0, "")},
	}
	rec, compiled, err := CompileChangeset(context.Background(), ledger, &fakeArtifactWriter{},
		ChangesetInput{Tenant: coordinator.Tenant{Organization: "org", Project: "prj"}, RunID: "run_1", AllocationRoot: root})
	if err != nil || !compiled {
		t.Fatalf("CompileChangeset() = compiled %v err %v", compiled, err)
	}
	// The file-tool ledger recorded nothing, but the shell-written secret is still flagged from the patch.
	if len(rec.Files) != 0 {
		t.Fatalf("files = %+v, want none (no file-tool write recorded)", rec.Files)
	}
	found := false
	for _, f := range rec.Findings {
		if f.Rule != "" && strings.Contains(f.Path, "secret.env") {
			found = true
		}
	}
	if !found {
		t.Fatalf("shell-written committed secret not flagged; findings = %+v", rec.Findings)
	}
}

func fileWriteRow(id, path, content, before, after string, created bool) coordinator.ToolCallRow {
	args, _ := json.Marshal(map[string]any{"op": "write", "path": path, "content": content})
	res, _ := json.Marshal(map[string]any{"path": path, "before_hash": before, "after_hash": after, "created": created})
	return coordinator.ToolCallRow{ID: id, Name: "palai.workspace.file", Arguments: string(args), Result: string(res)}
}

func shellRow(id string, argv []string, exit int, stdout string) coordinator.ToolCallRow {
	args, _ := json.Marshal(map[string]any{"argv": argv})
	res, _ := json.Marshal(map[string]any{"exit_code": exit, "stdout": stdout})
	return coordinator.ToolCallRow{ID: id, Name: "palai.workspace.shell", Arguments: string(args), Result: string(res)}
}

// newAllocRepo lays out an allocation root whose repo/ subdir is a git repo with one base commit
// (f.txt="base"), returning the root and the base sha.
func newAllocRepo(t *testing.T) (root, base string) {
	t.Helper()
	root = t.TempDir()
	if r, err := filepath.EvalSymlinks(root); err == nil {
		root = r
	}
	repoDir := filepath.Join(root, "repo")
	if err := os.MkdirAll(repoDir, 0o755); err != nil {
		t.Fatal(err)
	}
	run := repoGit(t, repoDir)
	run("init", "-q", "-b", "main")
	writeFile(t, filepath.Join(repoDir, "f.txt"), "base\n")
	run("add", "f.txt")
	run("commit", "-q", "-m", "base")
	return root, run("rev-parse", "HEAD")
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func requireGit(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skipf("git not found: %v", err)
	}
}
