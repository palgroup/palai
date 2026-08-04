package runner

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/palgroup/palai/adapters/repositories"
	"github.com/palgroup/palai/adapters/sandboxes/oci/workspace"
	"github.com/palgroup/palai/packages/contracts"
	toolbroker "github.com/palgroup/palai/packages/tool-broker"
)

// THE WORKSPACE PAIR (A.3 T5), AND IT IS A SIBLING OF exec.* AND bg.*, NOT A SUBTYPE OF EITHER.
//
// exec.request asks this machine to RUN something; ws.request asks it to READ OR WRITE something.
// They are separate because their answers are separate: a command answers with an exit code and
// captured output, a file operation answers with bytes or a confinement refusal, and folding the
// second into the first would mean a traversal refusal arriving as `exit 1` with a message the
// control plane would then have to parse. This tree already records what parsing a message instead
// of reading a code costs.
//
// WHY A MACHINE NEEDS THIS AT ALL. Before it, five of the six coding tools reached the workspace
// through the control plane's own filesystem, so the moment a lease could place a run on another
// machine the shell tool edited files on a Mac and the file tool read a directory somewhere else.
// Measured 2026-08-04, before this file existed:
//
//	grep -n "WorkspaceRoot" apps/control-plane/internal/execution/tools/{file,commit,push,media}.go
//	# -> five call sites, every one of them a path in the control plane's own filesystem
//
// The name was measured free before it was chosen, the same check bg.* passed and `tool.*` failed:
//
//	grep -rhoE '"ws\.[a-z_]+"' --include='*.go' .   -> (nothing)
const (
	// WorkspaceRequestType carries one workspace operation the control plane asks THIS machine to
	// perform, named by data.op.
	WorkspaceRequestType = "ws.request"
	// WorkspaceResultType carries the answer back. It is always sent — see WorkspaceServer.Handle.
	WorkspaceResultType = "ws.result"
)

// The operations one ws.request can name. They are the union of what the six coding tools and the
// provisioner need and NOTHING ELSE: this is a surface a control plane can drive against a machine's
// disk, so every verb on it is a verb somebody has to justify.
const (
	WorkspaceOpOpen     = "open"     // create the §29.9 layout under an allocation root, idempotently
	WorkspaceOpClone    = "clone"    // the infrastructure-owned deterministic preparation (§30.3)
	WorkspaceOpRead     = "read"     // confined file read
	WorkspaceOpWrite    = "write"    // confined atomic file write
	WorkspaceOpList     = "list"     // confined directory listing
	WorkspaceOpStat     = "stat"     // confined path metadata
	WorkspaceOpChecksum = "checksum" // confined content digest
	WorkspaceOpHead     = "head"     // the workspace repository's current commit + tree
	WorkspaceOpCommit   = "commit"   // commit the worktree under the platform's fixed identity
	WorkspaceOpGlob     = "glob"     // confined filename search, newest modification first
)

// WorkspaceRequestData builds the data payload of a ws.request. Correlation is this pair's own field,
// `ws_id`, for the reason exec_id and bg_id are separate from each other: the three pairs travel the
// same connection, and a shared field name would let a stray answer of one kind settle a wait of
// another.
func WorkspaceRequestData(wsID, op, root string, payload map[string]any) map[string]any {
	data := map[string]any{"ws_id": wsID, "op": op, "root": root}
	for k, v := range payload {
		switch k {
		case "ws_id", "op", "root":
			continue // this function's fields to set; a payload cannot rename its own question
		}
		data[k] = v
	}
	return data
}

// WorkspaceServer answers ws.request on THIS machine's disk. It is the workspace half of what
// ToolServer is for a command: the filesystem here is the one this machine has, so where a run's
// bytes live is decided by which machine took the lease.
type WorkspaceServer struct {
	// allocationRoot is the managed root every allocation must sit under. It is the SAME value
	// admitWorkspaceMount checks a lease's path against, and it is required: a machine that cannot
	// tell whether a path is one it minted has no basis on which to write into it, and failing open
	// would hand that decision to the control plane — the party §24 draws the boundary against.
	allocationRoot string
}

// NewWorkspaceServer binds the managed allocation root this machine serves workspaces under. An
// EMPTY root is a machine that serves none: every request is refused rather than answered against an
// unbounded filesystem. That is the same reversal workspaceUnderRoot already made for the bind-mount
// and for the same measured reason — one variable name, two planes, and the plane that guards is the
// one nobody sets.
func NewWorkspaceServer(allocationRoot string) *WorkspaceServer {
	return &WorkspaceServer{allocationRoot: allocationRoot}
}

// Handle performs one workspace operation and returns the ws.result to send back.
//
// IT NEVER RETURNS AN ERROR, for the reason ToolServer.Handle does not: a control plane that asked is
// blocking a tool call on the answer, so silence is not a degraded mode, it is a run that never
// continues.
//
// A FAILURE CROSSES WITH ITS CAUSE NAMED, NOT JUST ITS TEXT. data.code is workspace.FailureCode's
// answer and data.error is the message; the control plane rebuilds an error that errors.Is answers
// the same way (workspace.ErrorForCode). Without the code every refused traversal would arrive as an
// unclassifiable failure and the file tool would tell the model `failed` where it means `refused` —
// the control would still work and would stop being legible, which is the more expensive half.
func (s *WorkspaceServer) Handle(ctx context.Context, request contracts.RunnerMessage) contracts.RunnerMessage {
	wsID, _ := request.Data["ws_id"].(string)
	op, _ := request.Data["op"].(string)
	root, _ := request.Data["root"].(string)

	if s == nil || s.allocationRoot == "" {
		return workspaceRefusal(wsID, errors.New("this runner has no managed allocation root, so it serves no workspace — "+
			"set PALAI_WORKSPACE_ROOT on the runner to the directory it allocates under (spec §30.13, §24)"))
	}
	// THE §24 CHECK, ON EVERY OPERATION AND NOT ONLY ON THE MOUNT. The path is control-plane-supplied,
	// exactly as a lease's workspace path is, so it earns exactly the same refusal — and it is the
	// SHIPPED function rather than a second comparison written here, because every path-equality and
	// prefix check this repository has written by hand has shipped defeated at least once.
	//
	// `open` is exempt from the resolve half only, and that is the one asymmetry: workspaceUnderRoot
	// symlink-resolves both sides, and a directory that does not exist yet cannot be resolved. It is
	// checked by openAllocation instead, which walks the root down to the leaf refusing any component
	// that is not a real directory — a stricter test than the one it replaces, not a weaker one.
	if op != WorkspaceOpOpen {
		if err := workspaceUnderRoot(root, s.allocationRoot); err != nil {
			return workspaceRefusal(wsID, err)
		}
	}

	data, err := s.perform(ctx, op, root, request.Data)
	if err != nil {
		return workspaceFailure(wsID, err)
	}
	return contracts.RunnerMessage{Type: WorkspaceResultType, Data: WorkspaceRequestData(wsID, op, root, data)}
}

// perform dispatches one operation. It returns the answer's payload or the failure, and the failure
// keeps its sentinel all the way to Handle so FailureCode can name it.
func (s *WorkspaceServer) perform(ctx context.Context, op, root string, req map[string]any) (map[string]any, error) {
	switch op {
	case WorkspaceOpOpen:
		dir, err := s.openAllocation(root)
		if err != nil {
			return nil, err
		}
		return map[string]any{"root": dir}, nil
	case WorkspaceOpClone:
		return s.clone(ctx, root, req)
	case WorkspaceOpHead:
		commit, tree, err := repositories.Head(ctx, workspace.RepoPath(root))
		if err != nil {
			return nil, err
		}
		return map[string]any{"commit": commit, "tree": tree}, nil
	case WorkspaceOpCommit:
		message, _ := req["message"].(string)
		sha, err := repositories.Commit(ctx, workspace.RepoPath(root), message)
		if err != nil {
			return nil, err
		}
		return map[string]any{"commit": sha}, nil
	}

	// The confined operations. The filesystem is bound HERE rather than in Handle, because binding it
	// is itself the operation that can fail with ErrRootUnusable and that failure has to reach the
	// control plane carrying its own code.
	fs, err := workspace.NewWorkspaceFS(root)
	if err != nil {
		return nil, err
	}
	path, _ := req["path"].(string)
	switch op {
	case WorkspaceOpRead:
		maxBytes, err := int64Field(req, "max_bytes")
		if err != nil {
			return nil, err
		}
		data, truncated, err := fs.Read(path, maxBytes)
		if err != nil {
			return nil, err
		}
		return map[string]any{"data": data, "truncated": truncated}, nil
	case WorkspaceOpWrite:
		var content []byte
		if err := remarshal(req, "data", &content); err != nil {
			return nil, err
		}
		report, err := fs.Write(path, content)
		if err != nil {
			return nil, err
		}
		return map[string]any{"report": toolbroker.WriteReport{
			Path: report.Path, BeforeHash: report.BeforeHash,
			AfterHash: report.AfterHash, Created: report.Created,
		}}, nil
	case WorkspaceOpList:
		entries, err := fs.List(path)
		if err != nil {
			return nil, err
		}
		out := make([]toolbroker.DirEntry, 0, len(entries))
		for _, e := range entries {
			out = append(out, toolbroker.DirEntry{Name: e.Name, IsDir: e.IsDir, Size: e.Size})
		}
		return map[string]any{"entries": out}, nil
	case WorkspaceOpStat:
		st, err := fs.Stat(path)
		if err != nil {
			return nil, err
		}
		return map[string]any{"stat": toolbroker.FileStat{Path: st.Path, IsDir: st.IsDir, Size: st.Size}}, nil
	case WorkspaceOpChecksum:
		sum, err := fs.Checksum(path)
		if err != nil {
			return nil, err
		}
		return map[string]any{"checksum": sum}, nil
	case WorkspaceOpGlob:
		// The pattern rides its own field rather than `path`: it is not a path, it is a matcher, and
		// the confinement it needs is the one Glob applies to every candidate it produces.
		pattern, _ := req["pattern"].(string)
		limit, err := int64Field(req, "limit")
		if err != nil {
			return nil, err
		}
		paths, truncated, err := fs.Glob(pattern, int(limit))
		if err != nil {
			return nil, err
		}
		return map[string]any{"paths": paths, "truncated": truncated}, nil
	}
	return nil, fmt.Errorf("unknown workspace op %q", op)
}

// clone runs the infrastructure-owned deterministic preparation (§30.3) on THIS machine, into an
// allocation this machine opened.
//
// THE CREDENTIAL IS MINTED ON THE CONTROL PLANE AND SPENT HERE, AND THAT IS A REAL DECISION rather
// than a convenience. The broker holds the deployment's App key or the tenant's secret and neither
// leaves the control plane; what crosses is one short-lived read token, over the machine's mTLS lease
// connection, and it is revoked by the control plane on every return path (repositories.Prepare's
// deferred Revoke moved up to the caller). The alternative — cloning on the control plane and
// shipping a tree — puts the bytes back on the wrong host, which is the whole thing this task exists
// to stop.
//
// WHAT IT COSTS, STATED PLAINLY: the token exists in this process's memory and in a credential helper
// file under <allocation>/secrets for the length of the clone, and the control plane removes that
// staging area right after (provision.go). On the one shipped configuration where workspaces work —
// a native Mac whose runner and control plane share a filesystem — this is exactly where the token
// already went. On a genuinely remote machine it is new, and it is the price of the bytes living
// where the command runs.
func (s *WorkspaceServer) clone(ctx context.Context, root string, req map[string]any) (map[string]any, error) {
	var in WorkspaceCloneRequest
	if err := remarshal(req, "clone", &in); err != nil {
		return nil, err
	}
	if in.Token == "" {
		// FAIL CLOSED. An empty token would clone anonymously, which SUCCEEDS for a public repository
		// and fails for every private one — so a broken credential path would look like a working
		// deployment until the first private binding, which is the most expensive moment to find out.
		return nil, errors.New("clone request carries no read credential")
	}
	result, err := repositories.Prepare(ctx, repositories.NewTokenBroker(in.Token), repositories.Request{
		CloneURL:      in.CloneURL,
		RequestedRef:  in.RequestedRef,
		DefaultBranch: in.DefaultBranch,
		TargetDir:     workspace.RepoPath(root),
		SecretsDir:    in.SecretsDir,
		WorkBranch:    in.WorkBranch,
		Policy:        in.Policy,
		Audience:      in.Audience,
	})
	if err != nil {
		return nil, err
	}
	return map[string]any{"receipt": result.Receipt, "findings": result.Findings}, nil
}

// WorkspaceCloneRequest is the clone half of a ws.request, and it is EXPORTED so the control plane
// builds it with the same struct the machine decodes — the two ends of this wire cannot then drift
// into two spellings of the same field, which is the discipline ExecRequestData already keeps.
//
// Token is the ONLY credential-bearing field on this wire. It is never logged, never journalled and
// never part of an answer; the machine spends it and drops it.
type WorkspaceCloneRequest struct {
	CloneURL      string                `json:"clone_url"`
	RequestedRef  string                `json:"requested_ref,omitempty"`
	DefaultBranch string                `json:"default_branch,omitempty"`
	WorkBranch    string                `json:"work_branch,omitempty"`
	SecretsDir    string                `json:"secrets_dir"`
	Policy        repositories.Policy   `json:"policy"`
	Audience      repositories.Audience `json:"audience"`
	Token         string                `json:"token"`
}

// openAllocation creates the §29.9 layout for one allocation and returns its absolute path. It is
// idempotent: a second lease on the same allocation finds the tree already there, which is what makes
// a later run in a session reuse its edits rather than start over.
//
// IT WALKS THE PATH COMPONENT BY COMPONENT RATHER THAN CALLING MkdirAll, and the difference is the
// whole security argument. The path is control-plane-supplied and does not exist yet, so
// workspaceUnderRoot cannot resolve it; MkdirAll would happily follow an existing symlink component
// out of the root and create the allocation inside whatever it points at. Each component is instead
// created or verified with Lstat, so a symlink anywhere on the way down is a refusal rather than a
// door.
func (s *WorkspaceServer) openAllocation(path string) (string, error) {
	dir, err := resolveNewAllocationDir(path, s.allocationRoot)
	if err != nil {
		return "", err
	}
	if err := workspace.Prepare(dir); err != nil {
		return "", err
	}
	return dir, nil
}

// int64Field reads a numeric field that has crossed JSON, where every number is a float64.
func int64Field(data map[string]any, field string) (int64, error) {
	raw, ok := data[field]
	if !ok {
		return 0, fmt.Errorf("workspace request carries no %s", field)
	}
	switch v := raw.(type) {
	case float64:
		return int64(v), nil
	case int64:
		return v, nil
	case int:
		return int64(v), nil
	case json.Number:
		return v.Int64()
	}
	return 0, fmt.Errorf("workspace request %s is not a number", field)
}

func workspaceFailure(wsID string, err error) contracts.RunnerMessage {
	return contracts.RunnerMessage{Type: WorkspaceResultType, Data: map[string]any{
		"ws_id": wsID,
		"error": err.Error(),
		"code":  workspace.FailureCode(err),
	}}
}

// workspaceRefusal is a failure the machine raised BEFORE reaching the filesystem — an unset root, a
// path outside it. It carries code `other` deliberately: it matches no filesystem sentinel, so
// pretending it did would classify a boundary refusal as a missing file.
func workspaceRefusal(wsID string, err error) contracts.RunnerMessage {
	return contracts.RunnerMessage{Type: WorkspaceResultType, Data: map[string]any{
		"ws_id": wsID,
		"error": err.Error(),
		"code":  workspace.FailureCodeOther,
	}}
}
