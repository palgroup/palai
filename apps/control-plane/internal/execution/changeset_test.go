package execution

import (
	"context"
	"encoding/json"
	"fmt"
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
	// The file-tool ledger recorded nothing, and the shell-written secret is flagged from the patch.
	//
	// This assertion used to read `len(rec.Files) != 0` — "want none (no file-tool write recorded)".
	// It was PINNING the defect: the suite asserted, as intended behaviour, that a file the shell wrote
	// is absent from the changed set. The scan is what fixes that, so the file is now named, and the
	// finding beside it proves the two paths agree about the same write.
	if len(rec.Files) != 1 || rec.Files[0].Path != "repo/secret.env" {
		t.Fatalf("files = %+v, want the shell-written repo/secret.env observed in the clone", rec.Files)
	}
	if rec.Files[0].Provenance != coordinator.FileProvenanceWorkspaceScan {
		t.Fatalf("files[0] = %+v, want provenance=%s", rec.Files[0], coordinator.FileProvenanceWorkspaceScan)
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

// TestChangesetNamesAFileOnlyTheShellToolWrote is the defect: the changed-file set was projected from
// the file-tool ledger ALONE, so every mutation the shell tool made to the same clone — a heredoc,
// `cat > f`, `git apply`, a code generator, a compiler — was invisible to it, and a run whose only
// writer was the shell compiled to a changeset naming ZERO files.
//
// MEASURED on the live spine before the fix (2026-08-02): run_4ab998ccd917dbeb0b7c24d3e6a71ff8 issued
// exactly ONE tool call — `swift build --package-path repo` — wrote 1284 files under repo/.build/, and
// produced a 628,982-byte patch artifact beside a changed-file set of length 0.
//
// The three writers below are the three shapes: a new file, a rewrite of a tracked one, and a deletion.
// None has a file-tool row; all three are real changes to the clone.
func TestChangesetNamesAFileOnlyTheShellToolWrote(t *testing.T) {
	requireGit(t)
	root, base := newAllocRepo(t) // root/repo is a git repo with f.txt="base" and keep.txt="keep"
	repoDir := filepath.Join(root, "repo")

	writeFile(t, filepath.Join(repoDir, "generated.swift"), "// written by a generator\n")
	writeFile(t, filepath.Join(repoDir, "f.txt"), "rewritten by the shell\n")
	if err := os.Remove(filepath.Join(repoDir, "keep.txt")); err != nil {
		t.Fatal(err)
	}

	ledger := &fakeChangesetLedger{
		base: base, baseOK: true,
		rows: []coordinator.ToolCallRow{
			shellRow("tc_1", []string{"bash", "-c", "swift build && rm keep.txt"}, 0, ""),
		},
	}
	rec, compiled, err := CompileChangeset(context.Background(), ledger, &fakeArtifactWriter{},
		ChangesetInput{Tenant: coordinator.Tenant{Organization: "org", Project: "prj"}, RunID: "run_1", AllocationRoot: root})
	if err != nil || !compiled {
		t.Fatalf("CompileChangeset() = compiled %v err %v, want compiled", compiled, err)
	}

	byPath := map[string]coordinator.ChangesetFile{}
	for _, f := range rec.Files {
		byPath[f.Path] = f
	}
	for path, wantChange := range map[string]string{
		"repo/generated.swift": "added",
		"repo/f.txt":           "modified",
		"repo/keep.txt":        "deleted",
	} {
		got, ok := byPath[path]
		if !ok {
			t.Fatalf("%s is absent from the changeset; a shell write changed no file according to %+v", path, rec.Files)
		}
		if got.Change != wantChange {
			t.Fatalf("%s = %+v, want change=%s", path, got, wantChange)
		}
		// The entry must say plainly that it is an OBSERVATION. A workspace scan cannot produce a
		// tool_call_id or the before/after hashes the file tool records, and claiming either would
		// attribute the write to a call that never made it.
		if got.Provenance != coordinator.FileProvenanceWorkspaceScan {
			t.Fatalf("%s = %+v, want provenance=%s", path, got, coordinator.FileProvenanceWorkspaceScan)
		}
		if got.ToolCallID != "" {
			t.Fatalf("%s claims tool call %q, but no file-tool row wrote it", path, got.ToolCallID)
		}
	}
	if len(rec.Files) != 3 {
		t.Fatalf("files = %+v, want exactly the 3 shell-made changes", rec.Files)
	}
}

// TestChangesetKeepsToolCallProvenanceWhereTheFileToolWroteIt is the other half of the merge, and the
// reason the fix is not "scan the workspace instead". A path the file tool wrote is ALSO seen by the
// scan; the ledger row is the better record of it (it carries the tool call and the before/after
// hashes a directory scan cannot produce), so the merge must keep the ledger entry and not overwrite it
// with the weaker observation.
func TestChangesetKeepsToolCallProvenanceWhereTheFileToolWroteIt(t *testing.T) {
	requireGit(t)
	root, base := newAllocRepo(t)
	repoDir := filepath.Join(root, "repo")

	writeFile(t, filepath.Join(repoDir, "f.txt"), "changed\n")      // the file tool wrote this one
	writeFile(t, filepath.Join(repoDir, "shell.txt"), "by shell\n") // and this one has no ledger row

	ledger := &fakeChangesetLedger{
		base: base, baseOK: true,
		rows: []coordinator.ToolCallRow{
			fileWriteRow("tc_file", "repo/f.txt", "changed\n", "sha256:old", "sha256:new", false),
			shellRow("tc_shell", []string{"bash", "-c", "printf 'by shell\\n' > repo/shell.txt"}, 0, ""),
		},
	}
	rec, compiled, err := CompileChangeset(context.Background(), ledger, &fakeArtifactWriter{},
		ChangesetInput{Tenant: coordinator.Tenant{Organization: "org", Project: "prj"}, RunID: "run_1", AllocationRoot: root})
	if err != nil || !compiled {
		t.Fatalf("CompileChangeset() = compiled %v err %v, want compiled", compiled, err)
	}
	byPath := map[string]coordinator.ChangesetFile{}
	for _, f := range rec.Files {
		byPath[f.Path] = f
	}
	tooled := byPath["repo/f.txt"]
	if tooled.ToolCallID != "tc_file" || tooled.BeforeHash != "sha256:old" || tooled.AfterHash != "sha256:new" {
		t.Fatalf("repo/f.txt = %+v, want the file-tool row's attribution and hashes preserved", tooled)
	}
	if tooled.Provenance != coordinator.FileProvenanceToolCall {
		t.Fatalf("repo/f.txt = %+v, want provenance=%s", tooled, coordinator.FileProvenanceToolCall)
	}
	if observed := byPath["repo/shell.txt"]; observed.Provenance != coordinator.FileProvenanceWorkspaceScan {
		t.Fatalf("repo/shell.txt = %+v, want provenance=%s", observed, coordinator.FileProvenanceWorkspaceScan)
	}
	if len(rec.Files) != 2 {
		t.Fatalf("files = %+v, want the ledger write and the shell write exactly once each", rec.Files)
	}
}

// TestChangesetCountsIgnoredOutputRatherThanListingIt pins the decision about build output. A run that
// compiles writes thousands of .gitignore'd files; listing them would bury the changed set and would
// contradict the patch, which is computed through `git add -A` and skips them for the same reason.
// So they are EXCLUDED — and counted, because "this run changed nothing" and "this run changed one
// file and 2 ignored build outputs" are different sentences and only the second is true.
func TestChangesetCountsIgnoredOutputRatherThanListingIt(t *testing.T) {
	requireGit(t)
	root, base := newAllocRepo(t)
	repoDir := filepath.Join(root, "repo")

	writeFile(t, filepath.Join(repoDir, ".gitignore"), ".build/\n")
	if err := os.MkdirAll(filepath.Join(repoDir, ".build", "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(repoDir, ".build", "a.o"), "obj\n")
	writeFile(t, filepath.Join(repoDir, ".build", "sub", "b.o"), "obj\n")

	ledger := &fakeChangesetLedger{
		base: base, baseOK: true,
		rows: []coordinator.ToolCallRow{shellRow("tc_1", []string{"swift", "build"}, 0, "")},
	}
	rec, compiled, err := CompileChangeset(context.Background(), ledger, &fakeArtifactWriter{},
		ChangesetInput{Tenant: coordinator.Tenant{Organization: "org", Project: "prj"}, RunID: "run_1", AllocationRoot: root})
	if err != nil || !compiled {
		t.Fatalf("CompileChangeset() = compiled %v err %v, want compiled", compiled, err)
	}
	for _, f := range rec.Files {
		if strings.Contains(f.Path, "/.build/") {
			t.Fatalf("an ignored build output entered the changed set: %+v", f)
		}
	}
	// The two .o files are ignored and excluded; .gitignore itself is a real, tracked-scope change.
	if rec.IgnoredFileCount != 2 {
		t.Fatalf("ignored count = %d, want 2 — the excluded files must still be counted", rec.IgnoredFileCount)
	}
	if len(rec.Files) != 1 || rec.Files[0].Path != "repo/.gitignore" {
		t.Fatalf("files = %+v, want exactly repo/.gitignore", rec.Files)
	}
}

// TestTwoRunsOnTheSameBaseCompileToDistinctChangesetIDs is a SECOND defect, found while measuring the
// first and made near-certain by it. The changeset id is content-addressed so an E10 replay
// re-compiles to the same id and the primary key dedupes it — but the hash covered only
// base/final/files/findings and NOT the run, so any two runs agreeing on all of those collided. An
// empty changed set is exactly such an agreement, and the shell blindness above made it the common case.
//
// MEASURED on the live spine (2026-08-02): four runs on base 5c6105f39 — run_9542ef28, run_4ab998cc,
// run_259dc6e1, run_813fa521 — all compiled to sha256:26ef5060…, so `ON CONFLICT (id) DO NOTHING`
// recorded the FIRST and silently discarded three. run_4ab998cc, the run that wrote 1284 files, has no
// changeset row at all, and the surviving row names a different run.
//
// The two ledgers below differ ONLY in their run id, which is the whole point: the compile must still
// be idempotent per run (asserted at the end) without two runs sharing an identity.
func TestTwoRunsOnTheSameBaseCompileToDistinctChangesetIDs(t *testing.T) {
	requireGit(t)
	root, base := newAllocRepo(t)

	compile := func(runID string) coordinator.ChangesetRecord {
		t.Helper()
		ledger := &fakeChangesetLedger{base: base, baseOK: true}
		rec, compiled, err := CompileChangeset(context.Background(), ledger, &fakeArtifactWriter{},
			ChangesetInput{Tenant: coordinator.Tenant{Organization: "org", Project: "prj"}, RunID: runID, AllocationRoot: root})
		if err != nil || !compiled {
			t.Fatalf("CompileChangeset(%s) = compiled %v err %v", runID, compiled, err)
		}
		return rec
	}

	first, second := compile("run_first"), compile("run_second")
	if first.ID == second.ID {
		t.Fatalf("two runs share changeset id %s — the second run's changeset is dropped by ON CONFLICT DO NOTHING", first.ID)
	}
	if first.ContentHash == second.ContentHash {
		t.Fatalf("two runs share content hash %s", first.ContentHash)
	}
	// Non-vacuity, and the property the content address exists for: the SAME run re-compiles to the
	// SAME id, so a replay still inserts 0 rows rather than a duplicate.
	if again := compile("run_first"); again.ID != first.ID || again.ContentHash != first.ContentHash {
		t.Fatalf("re-compiling run_first moved: %s/%s then %s/%s", first.ID, first.ContentHash, again.ID, again.ContentHash)
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
// (f.txt="base", keep.txt="keep"), returning the root and the base sha. keep.txt is a second TRACKED
// file so a test can prove a deletion — the change kind the file-tool ledger can never produce.
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
	writeFile(t, filepath.Join(repoDir, "keep.txt"), "keep\n")
	run("add", "f.txt", "keep.txt")
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

// TestARefusedWriteIsNotCompiledIntoTheChangeset closes a hole this epic's own fix opened. Since a tool
// refusal became a delivered RESULT, a `completed` tool_calls row can carry `{"status":"error", …}`
// instead of a write report — and changedFiles reads every missing field as a meaningful value: `path`
// falls back to the ARGUMENT, both hashes come back empty, and an empty before_hash is the very test for
// "added". So a write the workspace REFUSED would have been compiled into REP-005's load-bearing
// provenance as a file this run added.
//
// The two rows below are the exact refusals a real run produces: a traversal write, and a write to a
// read-only workspace. Neither touched a file; neither may appear.
func TestARefusedWriteIsNotCompiledIntoTheChangeset(t *testing.T) {
	requireGit(t)
	root, base := newAllocRepo(t)
	writeFile(t, filepath.Join(root, "repo", "real.txt"), "written\n")

	refused := func(id, path, code, message string) coordinator.ToolCallRow {
		return coordinator.ToolCallRow{
			ID:        id,
			Name:      "palai.workspace.file",
			Arguments: fmt.Sprintf(`{"op":"write","path":%q,"content":"x"}`, path),
			Result: fmt.Sprintf(`{"status":"error","error":{"code":%q,"tool":"palai.workspace.file","message":%q}}`,
				code, message),
		}
	}
	ledger := &fakeChangesetLedger{
		base: base, baseOK: true,
		rows: []coordinator.ToolCallRow{
			refused("tc_esc", "../outside.txt", "refused", `path escapes the workspace: "../outside.txt"`),
			refused("tc_ro", "repo/blocked.txt", "refused", "file tool: workspace is read-only for this run"),
			fileWriteRow("tc_ok", "repo/real.txt", "written\n", "", "sha256:aa", true),
		},
	}
	rec, compiled, err := CompileChangeset(context.Background(), ledger, &fakeArtifactWriter{},
		ChangesetInput{Tenant: coordinator.Tenant{Organization: "org", Project: "prj"}, RunID: "run_1", AllocationRoot: root})
	if err != nil || !compiled {
		t.Fatalf("CompileChangeset() = compiled %v err %v, want compiled", compiled, err)
	}
	for _, f := range rec.Files {
		if f.ToolCallID == "tc_esc" || f.ToolCallID == "tc_ro" {
			t.Fatalf("a REFUSED write was compiled into the changeset as %+v", f)
		}
		if f.Path == "../outside.txt" || f.Path == "repo/blocked.txt" {
			t.Fatalf("a refused write's path entered the changeset: %+v", f)
		}
	}
	// Non-vacuity: the real write beside them IS compiled, so this is the refusal being skipped rather
	// than the compiler having stopped working.
	if len(rec.Files) != 1 || rec.Files[0].ToolCallID != "tc_ok" {
		t.Fatalf("files = %+v, want exactly the one real write (tc_ok)", rec.Files)
	}
}

// TestARefusedShellCallIsNamedInTheChecksTranscript is the same defect one field over and quieter. A
// refused shell call has no exit_code and no output, so the test-log artifact would render it as a bare
// `$ cmd` — indistinguishable, to whoever reads it, from a command that ran and printed nothing.
func TestARefusedShellCallIsNamedInTheChecksTranscript(t *testing.T) {
	rows := []coordinator.ToolCallRow{
		{
			ID: "tc_refused", Name: "palai.workspace.shell",
			Arguments: `{"argv":["xcodebuild","-version"]}`,
			Result:    `{"status":"error","error":{"code":"unavailable","tool":"palai.workspace.shell","message":"shell tool: no sandbox shell runner wired for this run"}}`,
		},
		shellRow("tc_ran", []string{"go", "test", "./..."}, 1, "FAIL\n"),
	}
	log := checksTranscript(rows)
	if !strings.Contains(log, "refused: shell tool: no sandbox shell runner wired for this run") {
		t.Fatalf("the checks transcript does not name the refusal:\n%s", log)
	}
	// Non-vacuity: a command that DID run still renders its exit code and output.
	if !strings.Contains(log, "exit: 1") || !strings.Contains(log, "FAIL") {
		t.Fatalf("the checks transcript lost a real command's result:\n%s", log)
	}
	// And a refusal must not be given an exit code it never had.
	if strings.Contains(log, "$ xcodebuild -version\nexit:") {
		t.Fatalf("a refused command was given an exit code:\n%s", log)
	}
}
