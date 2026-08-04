package execution

import (
	"context"
	"encoding/json"
	"path/filepath"
	"testing"

	"github.com/palgroup/palai/packages/coordinator"
)

// editorWriteRow is the ledger row the Anthropic text editor leaves behind. Its RESULT is the same
// shape the file tool's write produces — path plus both hashes — because the changeset reads those
// fields; what differs is the tool NAME and the argument that identifies the operation (`command`
// rather than `op`), which is exactly what the walk has to learn to recognise.
func editorWriteRow(id, command, path, before, after string, created bool) coordinator.ToolCallRow {
	args, _ := json.Marshal(map[string]any{"command": command, "path": path})
	res, _ := json.Marshal(map[string]any{"path": path, "before_hash": before, "after_hash": after, "created": created})
	return coordinator.ToolCallRow{ID: id, Name: "str_replace_based_edit_tool", Arguments: string(args), Result: string(res)}
}

// TestChangesetCountsTheTextEditorsWrites is the wiring this tree charges the most for missing. The
// changeset walk selected rows by `row.Name != fileToolName`, so adopting a second editing tool
// without touching it would have produced a run that edits files and reports an EMPTY changeset —
// the publication path deriving "nothing changed" from a ledger full of changes.
//
// It is a separate defect from the one the plan looked for. Measuring which code reads `before_hash`
// found changeset.go, but the field was never the constraint: the NAME FILTER above it was.
func TestChangesetCountsTheTextEditorsWrites(t *testing.T) {
	requireGit(t)
	root, base := newAllocRepo(t)
	repoDir := filepath.Join(root, "repo")

	writeFile(t, filepath.Join(repoDir, "added.txt"), "brand new\n")
	writeFile(t, filepath.Join(repoDir, "f.txt"), "changed\n")

	ledger := &fakeChangesetLedger{
		base: base, baseOK: true,
		rows: []coordinator.ToolCallRow{
			editorWriteRow("tc_1", "create", "repo/added.txt", "", "sha256:aa", true),
			editorWriteRow("tc_2", "str_replace", "repo/f.txt", "sha256:old", "sha256:new", false),
		},
	}
	aw := &fakeArtifactWriter{}
	in := ChangesetInput{Tenant: coordinator.Tenant{Project: "prj"}, RunID: "run_editor", AllocationRoot: root}

	rec, compiled, err := CompileChangeset(context.Background(), ledger, aw, in)
	if err != nil || !compiled {
		t.Fatalf("CompileChangeset() = compiled %v err %v, want compiled", compiled, err)
	}
	if len(rec.Files) != 2 {
		t.Fatalf("changeset carried %d files, want the editor's two: %+v", len(rec.Files), rec.Files)
	}
	byPath := map[string]coordinator.ChangesetFile{}
	for _, f := range rec.Files {
		byPath[f.Path] = f
	}
	if got := byPath["repo/added.txt"]; got.Change != "added" {
		t.Errorf("added.txt classified %q, want added", got.Change)
	}
	if got := byPath["repo/f.txt"]; got.Change != "modified" || got.BeforeHash != "sha256:old" {
		t.Errorf("f.txt = %+v, want modified with its before hash", got)
	}
}

// TestChangesetIgnoresANonWritingEditorCommand — `view` reads and changes nothing, so a changeset
// that counted it would report a file as touched on the strength of the model having looked at it.
func TestChangesetIgnoresANonWritingEditorCommand(t *testing.T) {
	requireGit(t)
	root, base := newAllocRepo(t)

	ledger := &fakeChangesetLedger{
		base: base, baseOK: true,
		rows: []coordinator.ToolCallRow{
			editorWriteRow("tc_1", "view", "repo/f.txt", "", "", false),
		},
	}
	aw := &fakeArtifactWriter{}
	in := ChangesetInput{Tenant: coordinator.Tenant{Project: "prj"}, RunID: "run_view", AllocationRoot: root}

	rec, _, err := CompileChangeset(context.Background(), ledger, aw, in)
	if err != nil {
		t.Fatalf("CompileChangeset: %v", err)
	}
	for _, f := range rec.Files {
		if f.Path == "repo/f.txt" {
			t.Errorf("a `view` was recorded as a change: %+v", f)
		}
	}
}
