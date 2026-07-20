package execution

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"path/filepath"
	"sort"
	"strings"

	"github.com/palgroup/palai/adapters/repositories"
	"github.com/palgroup/palai/adapters/sandboxes/oci/workspace"
	"github.com/palgroup/palai/packages/coordinator"
)

// The built-in tool names the changeset compiler filters the ledger by. They match the tools the
// broker registers (tools.FileTool()/ShellTool().Name); kept as literals here so the compiler does not
// import the tools package. ponytail: stable tool ids — if one is renamed, update it in both places.
const (
	fileToolName  = "palai.workspace.file"
	shellToolName = "palai.workspace.shell"
)

// maxPatchBytes bounds the stored patch artifact so a huge diff cannot exhaust memory or the object
// store; a diff over the bound is truncated with the marker set (spec §30.6). ponytail: 1 MiB fits a
// coding changeset; streaming a larger diff is future work.
const maxPatchBytes = 1 << 20

// ChangesetLedger is the coordinator seam the compiler reads the run's tool ledger + base commit from
// and records the changeset through. *coordinator.Store implements it; a fake implements it in the
// unit test (the RepositoryStore idiom), so the projection is provable without a database.
type ChangesetLedger interface {
	RunToolCalls(ctx context.Context, tenant coordinator.Tenant, runID string) ([]coordinator.ToolCallRow, error)
	RunBaseCommit(ctx context.Context, tenant coordinator.Tenant, runID string) (string, bool, error)
	RecordChangeset(ctx context.Context, tenant coordinator.Tenant, sessionID, responseID string, rec coordinator.ChangesetRecord) error
}

// ArtifactWriter is the object-store write-path the compiler persists the patch + test-log artifacts
// through (spec §22.6, T2), returning the artifact id. *artifacts.Writer implements it; a fake records
// the writes in the unit test. Primitive params keep this seam free of the artifacts package, so
// execution does not depend on the S3 write-path's types (the retention ArtifactDeleter decoupling,
// and it breaks the artifacts↔execution test import cycle).
type ArtifactWriter interface {
	WriteArtifact(ctx context.Context, org, project, runID string, content []byte, mediaType, logicalType string, provenance map[string]any) (string, error)
}

// ChangesetInput is the infrastructure-owned input to a changeset compile. AllocationRoot is the
// workspace allocation dir; the repo the changeset diffs lives at AllocationRoot/repo (spec §29.9).
type ChangesetInput struct {
	Tenant         coordinator.Tenant
	SessionID      string
	ResponseID     string
	RunID          string
	AllocationRoot string
}

// CompileChangeset compiles a first-class, immutable changeset from the run's file-tool write ledger —
// NOT from the model's prose (spec §30.6, REP-005): the changed-file set + provenance come from the
// tool_calls the run actually issued, the patch is the real working-tree diff against the preparation
// base, and any likely-committed-secret is a finding. It writes the patch + test-log artifacts to the
// object store, records the changeset, and returns it. compiled is false when the run prepared no
// repository (no base to diff against) — the caller then has no changeset to record.
//
// It is a COMPOSED step (like PrepareRepository), driven by the live smoke + coding journey. ponytail:
// auto-invocation at run finalize waits for workspace provisioning to land in the orchestrator (the
// same gate PrepareRepository waits on, repository.go); this is the exact call finalize will make.
func CompileChangeset(ctx context.Context, ledger ChangesetLedger, aw ArtifactWriter, in ChangesetInput) (coordinator.ChangesetRecord, bool, error) {
	base, ok, err := ledger.RunBaseCommit(ctx, in.Tenant, in.RunID)
	if err != nil {
		return coordinator.ChangesetRecord{}, false, err
	}
	if !ok {
		return coordinator.ChangesetRecord{}, false, nil // no prepared repo -> no changeset
	}
	rows, err := ledger.RunToolCalls(ctx, in.Tenant, in.RunID)
	if err != nil {
		return coordinator.ChangesetRecord{}, false, err
	}

	repoDir := filepath.Join(in.AllocationRoot, workspace.RepoDir)
	patch, truncated, err := repositories.WorkingDiff(ctx, repoDir, base, maxPatchBytes)
	if err != nil {
		return coordinator.ChangesetRecord{}, false, fmt.Errorf("compile changeset diff: %w", err)
	}
	finalCommit, finalTree, err := repositories.Head(ctx, repoDir)
	if err != nil {
		return coordinator.ChangesetRecord{}, false, fmt.Errorf("compile changeset head: %w", err)
	}

	rec := coordinator.ChangesetRecord{
		RunID:          in.RunID,
		BaseCommit:     base,
		FinalCommit:    finalCommit,
		FinalTree:      finalTree,
		Files:          changedFiles(rows),
		PatchTruncated: truncated,
		Findings:       scanPatchFindings(patch),
	}
	// Content-address the id so re-compiling the SAME ledger yields the SAME id — the insert then
	// dedupes on the primary key and the changeset is genuinely immutable (E10 replay re-compiles).
	// The hash excludes the id and the (random) artifact ids, so it is computable before either exists.
	rec.ContentHash = changesetContentHash(rec)
	rec.ID = changesetID(rec.ContentHash)

	provenance := map[string]any{"run_id": in.RunID, "changeset_id": rec.ID}
	if rec.PatchArtifactID, err = writeArtifact(ctx, aw, in, patch, "text/x-diff", "patch", provenance); err != nil {
		return coordinator.ChangesetRecord{}, false, err
	}
	if rec.TestLogArtifactID, err = writeArtifact(ctx, aw, in, checksTranscript(rows), "text/plain", "test-result", provenance); err != nil {
		return coordinator.ChangesetRecord{}, false, err
	}

	if err := ledger.RecordChangeset(ctx, in.Tenant, in.SessionID, in.ResponseID, rec); err != nil {
		return coordinator.ChangesetRecord{}, false, err
	}
	return rec, true, nil
}

// changesetID derives a content-addressed changeset id from the content hash, so an equal ledger
// re-compiles to an equal id (the primary key dedupes a replay). 128 bits of the digest is
// collision-safe for a run's changesets.
func changesetID(contentHash string) string {
	return "chg_" + strings.TrimPrefix(contentHash, "sha256:")[:32]
}

// changedFiles projects the file-tool write ledger into the changeset's changed-file set. Rows are
// chronological, so a path written twice resolves to its last write. A created file is "added"; a
// rewrite of an existing one is "modified". This is the load-bearing REP-005 projection — the changed
// set + provenance derived from the ledger, not model prose.
func changedFiles(rows []coordinator.ToolCallRow) []coordinator.ChangesetFile {
	byPath := map[string]*coordinator.ChangesetFile{}
	var order []string
	for _, row := range rows {
		if row.Name != fileToolName {
			continue
		}
		args := decodeJSON(row.Arguments)
		if s, _ := args["op"].(string); s != "write" {
			continue
		}
		res := decodeJSON(row.Result)
		path, _ := res["path"].(string)
		if path == "" {
			path, _ = args["path"].(string)
		}
		if path == "" {
			continue
		}
		before, _ := res["before_hash"].(string)
		after, _ := res["after_hash"].(string)
		change := "modified"
		if created, _ := res["created"].(bool); created || before == "" {
			change = "added"
		}
		if _, seen := byPath[path]; !seen {
			order = append(order, path)
			byPath[path] = &coordinator.ChangesetFile{Path: path}
		}
		f := byPath[path]
		// Keep the FIRST change kind (added stays added even after a later rewrite) but the LATEST
		// hashes, so before_hash is the pre-run state and after_hash the final state.
		if change == "added" || f.Change == "" {
			f.Change = change
		}
		f.AfterHash = after
		if f.BeforeHash == "" {
			f.BeforeHash = before
		}
		f.ToolCallID = row.ID
	}
	out := make([]coordinator.ChangesetFile, 0, len(order))
	for _, p := range order {
		out = append(out, *byPath[p])
	}
	return out
}

// checksTranscript renders the run's shell-tool calls into a plain-text checks/test log (spec §30.6
// "tests/checks + evidence"): the argv, exit code, and captured stdout/stderr of each command the
// agent ran. Empty when the run ran no shell command.
func checksTranscript(rows []coordinator.ToolCallRow) string {
	var b strings.Builder
	for _, row := range rows {
		if row.Name != shellToolName {
			continue
		}
		args := decodeJSON(row.Arguments)
		res := decodeJSON(row.Result)
		fmt.Fprintf(&b, "$ %s\n", argvString(args["argv"]))
		if code, ok := res["exit_code"]; ok {
			fmt.Fprintf(&b, "exit: %v\n", code)
		}
		if out, _ := res["stdout"].(string); out != "" {
			fmt.Fprintf(&b, "%s\n", out)
		}
		if errOut, _ := res["stderr"].(string); errOut != "" {
			fmt.Fprintf(&b, "stderr: %s\n", errOut)
		}
	}
	return b.String()
}

// scanPatchFindings runs the committed-secret scanner over the ADDED lines of the compiled patch
// (spec §30.4), attributing each hit to the file it lands in. Scanning the patch — not just file-tool
// write contents — is the complete detection: the patch captures EVERY change entering the changeset,
// including a secret written by the shell tool (echo secret > f), which never appears in the file-tool
// ledger. A secret already in the base (an unchanged line) is not an added line, so it is not
// re-flagged. Paths are prefixed with the repo subdir to match the ledger-derived file paths.
func scanPatchFindings(patch string) []coordinator.ChangesetFinding {
	var out []coordinator.ChangesetFinding
	seen := map[string]bool{}
	path := ""
	for _, line := range strings.Split(patch, "\n") {
		if p, ok := strings.CutPrefix(line, "+++ b/"); ok {
			path = filepath.ToSlash(filepath.Join(workspace.RepoDir, p))
			continue
		}
		// Added content only ("+"), never the "+++" file header or a context/removed line.
		if !strings.HasPrefix(line, "+") || strings.HasPrefix(line, "+++") {
			continue
		}
		for _, hit := range repositories.ScanSecrets(line) {
			if key := path + "\x00" + hit.Rule; !seen[key] {
				seen[key] = true
				out = append(out, coordinator.ChangesetFinding{ID: "csf_" + randHex16(), Kind: "secret", Path: path, Rule: hit.Rule})
			}
		}
	}
	return out
}

// writeArtifact persists one changeset artifact, returning "" for empty content (a changeset with no
// diff or no checks records no artifact). The S3 credential stays in the write-path (§24); the row
// carries the §22.6 classification + provenance.
func writeArtifact(ctx context.Context, aw ArtifactWriter, in ChangesetInput, content, mediaType, logicalType string, provenance map[string]any) (string, error) {
	if content == "" {
		return "", nil
	}
	id, err := aw.WriteArtifact(ctx, in.Tenant.Organization, in.Tenant.Project, in.RunID, []byte(content), mediaType, logicalType, provenance)
	if err != nil {
		return "", fmt.Errorf("write %s artifact: %w", logicalType, err)
	}
	return id, nil
}

// changesetContentHash is the content address of a changeset (spec §30.6 immutable summary): a digest
// over base/final + the sorted file set + sorted findings. It deliberately excludes the changeset id
// and the (random per compile) artifact ids, so equal ledgers hash equal — the REP-005 immutability
// anchor, and what makes the derived id stable across a re-compile.
func changesetContentHash(rec coordinator.ChangesetRecord) string {
	files := append([]coordinator.ChangesetFile(nil), rec.Files...)
	sort.Slice(files, func(i, j int) bool { return files[i].Path < files[j].Path })
	findings := append([]coordinator.ChangesetFinding(nil), rec.Findings...)
	sort.Slice(findings, func(i, j int) bool {
		if findings[i].Path != findings[j].Path {
			return findings[i].Path < findings[j].Path
		}
		return findings[i].Rule < findings[j].Rule
	})
	// Finding ids are random per compile, so hash only the stable (path, rule) — not the id.
	stableFindings := make([][2]string, len(findings))
	for i, f := range findings {
		stableFindings[i] = [2]string{f.Path, f.Rule}
	}
	canonical, _ := json.Marshal(map[string]any{
		"base": rec.BaseCommit, "final": rec.FinalCommit, "tree": rec.FinalTree,
		"files": files, "patch_truncated": rec.PatchTruncated, "findings": stableFindings,
	})
	sum := sha256.Sum256(canonical)
	return "sha256:" + hex.EncodeToString(sum[:])
}

func decodeJSON(s string) map[string]any {
	m := map[string]any{}
	if s != "" {
		_ = json.Unmarshal([]byte(s), &m)
	}
	return m
}

// argvString renders a shell tool's argv argument (a JSON array) as a space-joined command line for
// the transcript.
func argvString(v any) string {
	xs, ok := v.([]any)
	if !ok {
		return ""
	}
	parts := make([]string, 0, len(xs))
	for _, x := range xs {
		if s, ok := x.(string); ok {
			parts = append(parts, s)
		}
	}
	return strings.Join(parts, " ")
}
