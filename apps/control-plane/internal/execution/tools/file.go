package tools

import (
	"context"
	"errors"
	"fmt"
	iofs "io/fs"
	"path/filepath"
	"strings"

	"github.com/palgroup/palai/adapters/sandboxes/oci/workspace"
	toolbroker "github.com/palgroup/palai/packages/tool-broker"
)

// maxFileReadBytes bounds one file read so a huge file cannot be inlined into the model context
// (spec §28.7 bounded read). ponytail: fixed 1 MiB; a larger read must go through the artifact
// store (T2), never model text — the binary-via-artifact seam lands with the changeset (T5).
const maxFileReadBytes = 1 << 20

// FileTool is the built-in workspace file tool (spec §28.7, SAN-001). Every path resolves relative
// to the allocation root and an escape (traversal, absolute path, escaping symlink, device/socket)
// is denied; writes are atomic and report the before/after hash the changeset consumes; a
// likely-secret read is refused. It runs behind the broker's sandbox-backed Exec seam.
func FileTool() toolbroker.Tool {
	return toolbroker.Tool{
		Name:        "palai.workspace.file",
		Description: "Read, write, list, stat, or checksum files within the run's sandboxed workspace. Paths resolve relative to the workspace root; traversal, absolute paths, and likely-secret reads are denied.",
		ReplayClass: toolbroker.ClassReversible, // a workspace edit is revertible via the snapshot/git (§26.6)
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"op":      map[string]any{"type": "string"},
				"path":    map[string]any{"type": "string"},
				"content": map[string]any{"type": "string"},
			},
			"required":             []any{"op", "path"},
			"additionalProperties": false,
		},
		OutputSchema: map[string]any{"type": "object"},
		Exec:         fileExec,
	}
}

// fileExec dispatches one file operation against the confined workspace. It is the Exec surface the
// broker calls with the per-attempt ExecEnv.
//
// EVERY REFUSAL AND EVERY FAILED READ HERE IS AN ANSWER (toolbroker.Answer, answer.go), and the rule
// that decides it is one sentence: A READ THAT FAILED CHANGED NOTHING. read/list/stat/checksum touch no
// state, so there is no such thing as a half-applied one and nothing about them can be uncertain — the
// model asked for a path that is not there, or is not readable, or is not inside the workspace, and the
// answer to all three is to say so and let it try again. Before this, every one of them killed the
// attempt and left the pre-written ledger row to be escalated to manual_resolution, so `read "README"`
// instead of `read "repo/README"` ended the run permanently.
//
// THE WRITE PATH IS NOT COVERED BY THAT SENTENCE and is classified by sentinel instead — see below.
func fileExec(ctx context.Context, env toolbroker.ExecEnv, args map[string]any) (map[string]any, error) {
	if env.WorkspaceRoot == "" || env.Workspace == nil {
		// The case docs/operations/palai-on-a-mac.md §4.1 measured on 2026-07-28 and wrote down: a run
		// offered a workspace tool with no workspace bound HUNG rather than failing. Nothing ran, so it is
		// an answer, and a model told this can say so instead of the run never ending.
		//
		// BOTH CONDITIONS, AND THE SECOND IS THE A.3 ONE. WorkspaceRoot names where the bytes are;
		// env.Workspace is the thing that can reach them. An attempt can have the first without the
		// second — a machine whose lease connection is gone, holding a path that is real on a host this
		// process cannot open — and reaching for the path anyway is the failure this epic exists to
		// remove: an edit that silently landed beside the control plane instead of where the run runs.
		return nil, toolbroker.Answerf(toolbroker.AnswerUnavailable, "file tool: no workspace bound for this run")
	}
	fs := env.Workspace
	op, _ := args["op"].(string)
	path, _ := args["path"].(string)

	switch op {
	case "read":
		if likelySecretPath(path) {
			return nil, toolbroker.Answerf(toolbroker.AnswerRefused, "file tool: refusing to read likely-secret path %q", path)
		}
		data, truncated, err := fs.Read(ctx, path, maxFileReadBytes)
		if err != nil {
			return nil, fileFailure(env, err)
		}
		content := string(data)
		// REDACTION ON THE WAY OUT (E26 T6, §3.6 D8). A background task writes its own log file, which
		// bypasses both redactors — they act on a CAPTURED Go string — and this read is one of the two
		// places those bytes reach a model. The other is the exit notice's excerpt, and BOTH call the same
		// function behind this seam so they cannot diverge.
		//
		// IT IS APPLIED TO EVERY READ RATHER THAN TO PATHS THAT LOOK LIKE A LOG, deliberately: deciding
		// which paths carry a credential would be a path comparison deciding a security outcome, and this
		// tree's own history is that every such comparison has shipped defeated. Masking more costs a
		// substring pass; masking the wrong set costs the credential.
		//
		// AN ERROR REFUSES THE READ. The seam only errors when it cannot mask what it knows may be there,
		// and returning the bytes then would be the one outcome the whole path exists to prevent.
		if env.Background != nil {
			content, err = env.Background.RedactOutput(ctx, content)
			if err != nil {
				return nil, fmt.Errorf("file tool: %w", err)
			}
		}
		return map[string]any{"path": path, "content": content, "truncated": truncated, "size": len(data)}, nil
	case "write":
		if env.ReadOnly {
			return nil, toolbroker.Answerf(toolbroker.AnswerRefused, "file tool: workspace is read-only for this run")
		}
		content, _ := args["content"].(string)
		report, err := fs.Write(ctx, path, []byte(content))
		if err != nil {
			// A WRITE IS THE ONE OPERATION HERE THAT CAN LAND HALFWAY, so the read rule above does not
			// apply and the classification is by SENTINEL rather than by "it failed". ErrPathEscape and
			// ErrNotRegular are raised by resolve/assertRegularOrAbsent, both of which run BEFORE the temp
			// file is created — so those two, and only those two, are provably pre-effect and answerable.
			// Every other Write error (staging, rename, reading the prior content) sits at or after the
			// mutation and keeps today's abort, because a rename that may or may not have happened is
			// exactly what `uncertain` is for.
			// A ROOT THAT COULD NOT BE BOUND IS ALSO NOT ANSWERABLE and needs no arm of its own: it
			// matches neither sentinel, so it falls to the abort below, which is where the constructor
			// that used to raise it already sent it.
			if errors.Is(err, workspace.ErrPathEscape) || errors.Is(err, workspace.ErrNotRegular) {
				return nil, fileAnswer(env, fileAnswerCode(err), err)
			}
			return nil, err
		}
		return map[string]any{
			"path": report.Path, "before_hash": report.BeforeHash,
			"after_hash": report.AfterHash, "created": report.Created,
		}, nil
	case "list":
		entries, err := fs.List(ctx, path)
		if err != nil {
			return nil, fileFailure(env, err)
		}
		items := make([]any, 0, len(entries))
		for _, e := range entries {
			items = append(items, map[string]any{"name": e.Name, "is_dir": e.IsDir, "size": e.Size})
		}
		return map[string]any{"path": path, "entries": items}, nil
	case "stat":
		st, err := fs.Stat(ctx, path)
		if err != nil {
			return nil, fileFailure(env, err)
		}
		return map[string]any{"path": st.Path, "is_dir": st.IsDir, "size": st.Size}, nil
	case "checksum":
		sum, err := fs.Checksum(ctx, path)
		if err != nil {
			return nil, fileFailure(env, err)
		}
		return map[string]any{"path": path, "checksum": sum}, nil
	default:
		return nil, toolbroker.Answerf(toolbroker.AnswerInvalidArguments, "file tool: unknown op %q", op)
	}
}

// fileFailure classifies one workspace-filesystem failure, and it carries the ONE asymmetry this tool
// draws — a decision on the record rather than an accident of where a constructor used to sit.
//
// A ROOT THAT CANNOT BE BOUND IS A FAULT, EVERYTHING ELSE IS AN ANSWER. An allocation that was never
// provisioned, or that has gone away underneath a live run, is the deployment being broken rather
// than the model being wrong, so it keeps today's abort; a root that exists and is merely unusable —
// a regular file where a directory should be — fails inside the operation, and that is an answer,
// because the rule is about what the operation DID and a read that failed changed nothing.
// TestAnAllocationRootThatDoesNotExistIsRefusedBeforeAnyRead pins both halves.
//
// IT WAS AN `if err != nil` AROUND A CONSTRUCTOR AND IS NOW A SENTINEL, because the constructor moved.
// When the allocation lives on another machine the filesystem is bound there and the failure crosses a
// wire, so the branch has to read a cause it can still recognise on this side (workspace.ErrRootUnusable,
// rebuilt by workspace.ErrorForCode) instead of a call it can no longer see.
func fileFailure(env toolbroker.ExecEnv, err error) error {
	if errors.Is(err, workspace.ErrRootUnusable) {
		return fmt.Errorf("file tool: %w", err)
	}
	return fileAnswer(env, fileAnswerCode(err), err)
}

// fileAnswer wraps a workspace-filesystem failure as the model's answer, with the allocation's absolute
// host path folded down to `<workspace>`.
//
// MEASURED ON A LIVE RUN, 2026-08-01. The first refusal this change ever delivered to a real model read:
//
//	read "README": open /Users/salih/palai-toolerr/workspaces/alloc_de9207d0b141e8a2/README: no such file
//
// The useful half is already there — exec.go's own wrapper names the RELATIVE path the model asked for —
// and the tail is an *fs.PathError carrying the host path, which now travels into the model's context
// (and, for a hosted provider, off this machine) and into a durable tool_calls row. It tells the model
// nothing it can act on: every path it may name is workspace-relative by construction.
//
// THIS IS HYGIENE, NOT CONTAINMENT, and the distinction matters because this tree has been bitten by
// path comparisons that were load-bearing. Nothing is decided here: if the substitution misses, the
// outcome is exactly today's message, and the refusal itself was made by resolve()'s sentinels long
// before this function is reached.
// IT FOLDS THE RESOLVED ROOT AS WELL AS THE DECLARED ONE, and that is not belt-and-braces — it is the
// only version that works on the target platform. NewWorkspaceFS EvalSymlinks's the root, so every path
// in an *fs.PathError is the REAL one, while ExecEnv carries the DECLARED one; on macOS a workspace under
// /var reports as /private/var and a substitution that read only the name it was handed would walk
// straight through the everyday alias. Same trap as the bring-up's world-writable-parent check, which
// this tree already had to learn to resolve.
func fileAnswer(env toolbroker.ExecEnv, code string, err error) error {
	msg := err.Error()
	root := env.WorkspaceRoot
	if root == "" {
		return toolbroker.Answerf(code, "%s", msg)
	}
	if real, rerr := filepath.EvalSymlinks(root); rerr == nil && real != root {
		msg = strings.ReplaceAll(msg, real, "<workspace>")
	}
	msg = strings.ReplaceAll(msg, root, "<workspace>")
	return toolbroker.Answerf(code, "%s", msg)
}

// fileAnswerCode names a workspace-filesystem failure for the model. It reads the SENTINELS
// (workspace.ErrPathEscape / ErrNotRegular) and the standard io/fs ones through errors.Is, never the
// message text — the messages are built by fmt.Errorf at four different call sites and would be four
// things to keep in step. An unrecognised cause is AnswerFailed rather than a guess: the model still
// learns the read did not work, which is the part it can act on.
//
// A REFUSED TRAVERSAL COMES BACK AS `refused`, NOT AS AN ERROR CODE THAT SOUNDS LIKE A BUG. That naming
// is the security case's whole point: the control fired, /etc/passwd was not read, and the run carries on
// with the model told exactly why.
func fileAnswerCode(err error) string {
	switch {
	case errors.Is(err, workspace.ErrPathEscape):
		return toolbroker.AnswerRefused
	case errors.Is(err, workspace.ErrNotRegular):
		return toolbroker.AnswerRefused
	case errors.Is(err, iofs.ErrNotExist):
		return toolbroker.AnswerNotFound
	case errors.Is(err, iofs.ErrPermission):
		return "permission_denied"
	default:
		return toolbroker.AnswerFailed
	}
}

// secretBasenames are credential files whose contents must never be surfaced to the model (spec
// §28.7). It mirrors the snapshot credential exclusions plus common private-key names.
var secretBasenames = map[string]bool{
	".git-credentials": true, ".netrc": true, ".npmrc": true,
	"id_rsa": true, "id_ed25519": true, "id_ecdsa": true, "credentials": true,
}

// likelySecretPath reports whether a read target is a likely credential file — a dotenv, a private
// key, or a known credential store — that the file tool refuses to read into model context.
func likelySecretPath(rel string) bool {
	base := strings.ToLower(filepath.Base(rel))
	if secretBasenames[base] {
		return true
	}
	if strings.HasPrefix(base, ".env") {
		return true
	}
	return strings.HasSuffix(base, ".pem") || strings.HasSuffix(base, ".key")
}
