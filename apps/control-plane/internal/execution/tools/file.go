package tools

import (
	"context"
	"fmt"
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
		Name: "palai.workspace.file",
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
func fileExec(_ context.Context, env toolbroker.ExecEnv, args map[string]any) (map[string]any, error) {
	if env.WorkspaceRoot == "" {
		return nil, fmt.Errorf("file tool: no workspace bound for this run")
	}
	fs, err := workspace.NewWorkspaceFS(env.WorkspaceRoot)
	if err != nil {
		return nil, fmt.Errorf("file tool: %w", err)
	}
	op, _ := args["op"].(string)
	path, _ := args["path"].(string)

	switch op {
	case "read":
		if likelySecretPath(path) {
			return nil, fmt.Errorf("file tool: refusing to read likely-secret path %q", path)
		}
		data, truncated, err := fs.Read(path, maxFileReadBytes)
		if err != nil {
			return nil, err
		}
		return map[string]any{"path": path, "content": string(data), "truncated": truncated, "size": len(data)}, nil
	case "write":
		if env.ReadOnly {
			return nil, fmt.Errorf("file tool: workspace is read-only for this run")
		}
		content, _ := args["content"].(string)
		report, err := fs.Write(path, []byte(content))
		if err != nil {
			return nil, err
		}
		return map[string]any{
			"path": report.Path, "before_hash": report.BeforeHash,
			"after_hash": report.AfterHash, "created": report.Created,
		}, nil
	case "list":
		entries, err := fs.List(path)
		if err != nil {
			return nil, err
		}
		items := make([]any, 0, len(entries))
		for _, e := range entries {
			items = append(items, map[string]any{"name": e.Name, "is_dir": e.IsDir, "size": e.Size})
		}
		return map[string]any{"path": path, "entries": items}, nil
	case "stat":
		st, err := fs.Stat(path)
		if err != nil {
			return nil, err
		}
		return map[string]any{"path": st.Path, "is_dir": st.IsDir, "size": st.Size}, nil
	case "checksum":
		sum, err := fs.Checksum(path)
		if err != nil {
			return nil, err
		}
		return map[string]any{"path": path, "checksum": sum}, nil
	default:
		return nil, fmt.Errorf("file tool: unknown op %q", op)
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
