package tools

import (
	"context"

	"github.com/palgroup/palai/adapters/repositories"
	"github.com/palgroup/palai/adapters/sandboxes/oci/workspace"
	toolbroker "github.com/palgroup/palai/packages/tool-broker"
)

// LocalWorkspace is the toolbroker.WorkspaceOps that acts on a directory in THIS process's own
// filesystem. It is what the coding tools did inline before A.3 T5 split the surface from its
// implementation, moved here unchanged, so an allocation that really does live on this host is
// edited exactly as it always was.
//
// IT IS NOT A FALLBACK FOR A MISSING MACHINE. The orchestrator chooses this one only for an
// allocation it realized on its own disk (execution.workspaceOpsFor), which is every attempt whose
// channel reaches no machine — the deterministic e2e tier, child worktrees, and the hand-built
// component orchestrators. An attempt whose workspace was realized on a machine NEVER gets this: it
// gets the remote ops, and if the connection is gone it gets an error rather than this host's disk.
// The distinction is recorded on the attempt at provisioning time, not guessed here.
//
// The filesystem is bound lazily, once per operation, which is what the tools did when each of them
// called NewWorkspaceFS inside its own Exec. Binding it eagerly in this constructor would need an
// error return the orchestrator's env builder has nowhere to put, and would move the
// root-unusable fault from the operation that asked to the attempt that had not yet asked for
// anything.
func LocalWorkspace(root string) toolbroker.WorkspaceOps { return localWorkspace{root: root} }

type localWorkspace struct{ root string }

func (l localWorkspace) fs() (*workspace.WorkspaceFS, error) { return workspace.NewWorkspaceFS(l.root) }

func (l localWorkspace) Read(_ context.Context, rel string, maxBytes int64) ([]byte, bool, error) {
	fs, err := l.fs()
	if err != nil {
		return nil, false, err
	}
	return fs.Read(rel, maxBytes)
}

func (l localWorkspace) Write(_ context.Context, rel string, data []byte) (toolbroker.WriteReport, error) {
	fs, err := l.fs()
	if err != nil {
		return toolbroker.WriteReport{}, err
	}
	report, err := fs.Write(rel, data)
	if err != nil {
		return toolbroker.WriteReport{}, err
	}
	return toolbroker.WriteReport{
		Path: report.Path, BeforeHash: report.BeforeHash,
		AfterHash: report.AfterHash, Created: report.Created,
	}, nil
}

func (l localWorkspace) List(_ context.Context, rel string) ([]toolbroker.DirEntry, error) {
	fs, err := l.fs()
	if err != nil {
		return nil, err
	}
	entries, err := fs.List(rel)
	if err != nil {
		return nil, err
	}
	out := make([]toolbroker.DirEntry, 0, len(entries))
	for _, e := range entries {
		out = append(out, toolbroker.DirEntry{Name: e.Name, IsDir: e.IsDir, Size: e.Size})
	}
	return out, nil
}

func (l localWorkspace) Stat(_ context.Context, rel string) (toolbroker.FileStat, error) {
	fs, err := l.fs()
	if err != nil {
		return toolbroker.FileStat{}, err
	}
	st, err := fs.Stat(rel)
	if err != nil {
		return toolbroker.FileStat{}, err
	}
	return toolbroker.FileStat{Path: st.Path, IsDir: st.IsDir, Size: st.Size}, nil
}

func (l localWorkspace) Glob(_ context.Context, pattern string, limit int) ([]string, bool, error) {
	fs, err := l.fs()
	if err != nil {
		return nil, false, err
	}
	return fs.Glob(pattern, limit)
}

func (l localWorkspace) Grep(ctx context.Context, req toolbroker.GrepRequest) (toolbroker.GrepResult, error) {
	fs, err := l.fs()
	if err != nil {
		return toolbroker.GrepResult{}, err
	}
	res, err := fs.Grep(ctx, workspace.GrepQuery{
		Pattern:    req.Pattern,
		Path:       req.Path,
		Glob:       req.Glob,
		OutputMode: req.OutputMode,
		Before:     req.Before,
		After:      req.After,
		Multiline:  req.Multiline,
		Limit:      req.Limit,
	})
	if err != nil {
		return toolbroker.GrepResult{}, err
	}
	out := toolbroker.GrepResult{
		Mode:      res.Mode,
		Files:     res.Files,
		Counts:    res.Counts,
		Total:     res.Total,
		Truncated: res.Truncated,
	}
	for _, m := range res.Matches {
		out.Matches = append(out.Matches, toolbroker.GrepMatch{
			Path: m.Path, Line: m.Line, Text: m.Text, Before: m.Before, After: m.After,
		})
	}
	return out, nil
}

func (l localWorkspace) Checksum(_ context.Context, rel string) (string, error) {
	fs, err := l.fs()
	if err != nil {
		return "", err
	}
	return fs.Checksum(rel)
}

// Head and Commit act on <root>/repo, the §29.9 layout's repository. They do NOT go through
// WorkspaceFS: git owns its own directory and the confinement WorkspaceFS provides is about paths a
// MODEL named, while these two name no path at all.
func (l localWorkspace) Head(ctx context.Context) (string, string, error) {
	return repositories.Head(ctx, workspace.RepoPath(l.root))
}

func (l localWorkspace) Commit(ctx context.Context, message string) (string, error) {
	return repositories.Commit(ctx, workspace.RepoPath(l.root), message)
}
