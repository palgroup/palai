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
	if env.WorkspaceRoot == "" {
		// The case docs/operations/palai-on-a-mac.md §4.1 measured on 2026-07-28 and wrote down: a run
		// offered a workspace tool with no workspace bound HUNG rather than failing. Nothing ran, so it is
		// an answer, and a model told this can say so instead of the run never ending.
		return nil, toolbroker.Answerf(toolbroker.AnswerUnavailable, "file tool: no workspace bound for this run")
	}
	fs, err := workspace.NewWorkspaceFS(env.WorkspaceRoot)
	if err != nil {
		// NOT an answer, deliberately, and it is a NARROWER branch than it looks: NewWorkspaceFS only
		// refuses a root that is blank or that EvalSymlinks cannot resolve — i.e. an allocation that was
		// never provisioned, or one that has gone away underneath a live run. That is the deployment
		// being broken rather than the model being wrong, so it keeps today's abort. A root that exists
		// and is merely unusable constructs fine and fails inside the read below, where it is an answer
		// (TestAnAllocationRootThatDoesNotExistIsRefusedBeforeAnyRead pins both halves).
		return nil, fmt.Errorf("file tool: %w", err)
	}
	op, _ := args["op"].(string)
	path, _ := args["path"].(string)

	switch op {
	case "read":
		if likelySecretPath(path) {
			return nil, toolbroker.Answerf(toolbroker.AnswerRefused, "file tool: refusing to read likely-secret path %q", path)
		}
		data, truncated, err := fs.Read(path, maxFileReadBytes)
		if err != nil {
			return nil, toolbroker.Answer(fileAnswerCode(err), err)
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
		report, err := fs.Write(path, []byte(content))
		if err != nil {
			// A WRITE IS THE ONE OPERATION HERE THAT CAN LAND HALFWAY, so the read rule above does not
			// apply and the classification is by SENTINEL rather than by "it failed". ErrPathEscape and
			// ErrNotRegular are raised by resolve/assertRegularOrAbsent, both of which run BEFORE the temp
			// file is created — so those two, and only those two, are provably pre-effect and answerable.
			// Every other Write error (staging, rename, reading the prior content) sits at or after the
			// mutation and keeps today's abort, because a rename that may or may not have happened is
			// exactly what `uncertain` is for.
			if errors.Is(err, workspace.ErrPathEscape) || errors.Is(err, workspace.ErrNotRegular) {
				return nil, toolbroker.Answer(fileAnswerCode(err), err)
			}
			return nil, err
		}
		return map[string]any{
			"path": report.Path, "before_hash": report.BeforeHash,
			"after_hash": report.AfterHash, "created": report.Created,
		}, nil
	case "list":
		entries, err := fs.List(path)
		if err != nil {
			return nil, toolbroker.Answer(fileAnswerCode(err), err)
		}
		items := make([]any, 0, len(entries))
		for _, e := range entries {
			items = append(items, map[string]any{"name": e.Name, "is_dir": e.IsDir, "size": e.Size})
		}
		return map[string]any{"path": path, "entries": items}, nil
	case "stat":
		st, err := fs.Stat(path)
		if err != nil {
			return nil, toolbroker.Answer(fileAnswerCode(err), err)
		}
		return map[string]any{"path": st.Path, "is_dir": st.IsDir, "size": st.Size}, nil
	case "checksum":
		sum, err := fs.Checksum(path)
		if err != nil {
			return nil, toolbroker.Answer(fileAnswerCode(err), err)
		}
		return map[string]any{"path": path, "checksum": sum}, nil
	default:
		return nil, toolbroker.Answerf(toolbroker.AnswerInvalidArguments, "file tool: unknown op %q", op)
	}
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
