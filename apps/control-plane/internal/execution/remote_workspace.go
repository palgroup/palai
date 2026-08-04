package execution

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/coder/websocket"

	"github.com/palgroup/palai/adapters/repositories"
	"github.com/palgroup/palai/adapters/sandboxes/oci/workspace"
	"github.com/palgroup/palai/packages/contracts"
	"github.com/palgroup/palai/packages/runner"
	toolbroker "github.com/palgroup/palai/packages/tool-broker"
)

// The control plane's end of the A.3 T5 workspace wire: a toolbroker.WorkspaceOps whose filesystem is
// the one belonging to the MACHINE THAT HOLDS THE ATTEMPT'S LEASE. It is the exact sibling of
// RemoteShell one layer over — same connection, same correlation discipline, same reason for existing:
// where a run's bytes live stops being a property of the deployment and becomes a property of the
// lease.
//
// IT IS BUILT THE SAME WAY AND FOR THE SAME REASON RemoteShell IS: the lease connection already has a
// sole reader, so the ANSWER is routed by that reader (gatewayChannel) and only the policy — minting
// the id, waiting, turning the wire's two answer shapes into an error that still carries its
// sentinel — belongs here.

// WorkspaceAnswer is one ws.result, decoded. Exactly one side is meaningful: Err for a failure the
// machine named, Data for an operation that completed.
type WorkspaceAnswer struct {
	Data map[string]any
	Err  error
}

// WorkspaceConn is the lease connection RemoteWorkspace asks a machine to act on its own disk over. It
// is a narrow interface for the reason ExecConn is one: Dial returns an EngineChannel, and whether
// that channel can also reach a machine's filesystem is a fact about the transport rather than about
// every engine channel.
type WorkspaceConn interface {
	// StartWorkspace registers wsID as awaiting an answer and then sends the request, in that order —
	// the connection's reader can deliver an answer the instant after the write returns, and a
	// registration made afterwards would race it. It returns the channel the answer arrives on and the
	// release that unregisters the wait.
	StartWorkspace(ctx context.Context, wsID, op, root string, payload map[string]any) (<-chan WorkspaceAnswer, func(), error)
}

var (
	_ WorkspaceConn           = (*gatewayChannel)(nil)
	_ toolbroker.WorkspaceOps = (*RemoteWorkspace)(nil)
)

// StartWorkspace asks the machine holding this lease to act on its own disk. The message is a sibling
// of Send's controller.frame and of StartExec's exec.request on the same connection and in the same
// runner.v1 envelope — same lease identity, same fence — because it is the same lease speaking.
func (c *gatewayChannel) StartWorkspace(ctx context.Context, wsID, op, root string, payload map[string]any) (<-chan WorkspaceAnswer, func(), error) {
	answers, release, err := c.workspaces.register(wsID)
	if err != nil {
		return nil, nil, err
	}
	message := contracts.RunnerMessage{
		Protocol:  runner.RunnerProtocolV1,
		Type:      runner.WorkspaceRequestType,
		Time:      time.Now().UTC().Format(time.RFC3339),
		LeaseID:   c.leaseID,
		RunID:     c.attempt.RunID,
		AttemptID: c.attempt.AttemptID,
		Fence:     int(c.attempt.Fence),
		// Built by the runner package that also reads it, so the two ends of this wire cannot drift
		// into two spellings of the same field.
		Data: runner.WorkspaceRequestData(wsID, op, root, payload),
	}
	encoded, err := json.Marshal(message)
	if err != nil {
		release()
		return nil, nil, fmt.Errorf("encode workspace request: %w", err)
	}
	if err := c.pr.conn.Write(ctx, websocket.MessageText, encoded); err != nil {
		release()
		return nil, nil, fmt.Errorf("send workspace request: %w", err)
	}
	return answers, release, nil
}

// deliverWorkspaceResult routes one ws.result to the operation that asked for it. readLoop calls it,
// which is the point: the connection already has a reader, so the answer is handed over by that reader
// rather than by a second one. An answer nobody waits for is dropped, for the two reasons an unclaimed
// exec answer is — the caller gave up, or a machine answered twice.
func (c *gatewayChannel) deliverWorkspaceResult(data map[string]any) {
	wsID, _ := data["ws_id"].(string)
	c.workspaces.deliver(wsID, decodeWorkspaceAnswer(data))
}

// decodeWorkspaceAnswer reads the two shapes WorkspaceServer.Handle produces.
//
// THE SENTINEL IS REBUILT FROM data.code, WHICH IS THE WHOLE POINT OF THAT FIELD. The file tool
// classifies by errors.Is — a refused traversal is `refused`, a missing file is `not_found`, an
// unbindable allocation is not an answer at all — and a plain errors.New here would make every one of
// them `failed`: the security control would still fire and would stop being legible, which is the
// half that costs a reader an hour.
func decodeWorkspaceAnswer(data map[string]any) WorkspaceAnswer {
	if failure, ok := data["error"].(string); ok && failure != "" {
		code, _ := data["code"].(string)
		return WorkspaceAnswer{Err: workspace.ErrorForCode(code, failure)}
	}
	return WorkspaceAnswer{Data: data}
}

// RemoteWorkspace acts on the workspace of the machine that holds one attempt's lease. It satisfies
// toolbroker.WorkspaceOps, so the file, media, commit, push and pull-request tools reach it without
// knowing where the bytes are — the substitution IS the feature, exactly as it is for RemoteShell.
type RemoteWorkspace struct {
	conn WorkspaceConn
	root string
}

// NewRemoteWorkspace binds one attempt's lease connection and the allocation root ON THAT MACHINE. A
// RemoteWorkspace is therefore attempt-scoped, the same as the lease and the same as RemoteShell.
func NewRemoteWorkspace(conn WorkspaceConn, root string) *RemoteWorkspace {
	return &RemoteWorkspace{conn: conn, root: root}
}

// call performs one operation and waits for the machine's answer.
//
// THERE IS NO TIMER HERE, for RemoteShell.Run's reason: a clone of a large repository is bounded on
// the MACHINE, and a second bound on this side would be either shorter than that one — killing the
// preparation this epic exists to make possible — or longer, in which case it never fires. What ends
// an unanswerable wait is the connection: when the lease drops, every operation still waiting is
// answered (pendingAnswers.closeAll).
func (w *RemoteWorkspace) call(ctx context.Context, op string, payload map[string]any) (map[string]any, error) {
	wsID := newExecID("ws")
	answers, release, err := w.conn.StartWorkspace(ctx, wsID, op, w.root, payload)
	if err != nil {
		return nil, fmt.Errorf("ask the machine to %s the workspace: %w", op, err)
	}
	defer release()
	select {
	case answer := <-answers:
		if answer.Err != nil {
			return nil, answer.Err
		}
		return answer.Data, nil
	case <-ctx.Done():
		return nil, fmt.Errorf("workspace %s %s: no answer before the context ended: %w", op, wsID, ctx.Err())
	}
}

// Root reports the allocation root on the machine. It is the value ShellCommand.WorkspaceRoot carries,
// so a shell command and a file write name the same directory on the same host.
func (w *RemoteWorkspace) Root() string { return w.root }

// Open creates the §29.9 layout for this allocation on the machine and returns the path the machine
// gave it back. The returned path may differ from the requested one by symlink resolution — a macOS
// runner rooted under /var answers /private/var — and the ANSWER is the one to keep, because it is
// what every later under-root check on that machine compares against.
func (w *RemoteWorkspace) Open(ctx context.Context) (string, error) {
	data, err := w.call(ctx, runner.WorkspaceOpOpen, nil)
	if err != nil {
		return "", err
	}
	root, _ := data["root"].(string)
	if root == "" {
		return "", errors.New("the machine opened the allocation and named no path")
	}
	w.root = root
	return root, nil
}

// Clone runs the §30.3 deterministic preparation on the machine and returns its receipt and findings.
// The credential travels in the request and is the caller's to revoke — see provision.go, which mints
// and revokes it around this call exactly as repositories.Prepare's deferred Revoke did when the clone
// ran here.
func (w *RemoteWorkspace) Clone(ctx context.Context, in runner.WorkspaceCloneRequest) (contracts.PreparationReceipt, []repositories.Finding, error) {
	data, err := w.call(ctx, runner.WorkspaceOpClone, map[string]any{"clone": in})
	if err != nil {
		return contracts.PreparationReceipt{}, nil, err
	}
	var out struct {
		Receipt  contracts.PreparationReceipt `json:"receipt"`
		Findings []repositories.Finding       `json:"findings"`
	}
	if err := remarshalInto(data, &out); err != nil {
		return contracts.PreparationReceipt{}, nil, err
	}
	return out.Receipt, out.Findings, nil
}

func (w *RemoteWorkspace) Read(ctx context.Context, rel string, maxBytes int64) ([]byte, bool, error) {
	data, err := w.call(ctx, runner.WorkspaceOpRead, map[string]any{"path": rel, "max_bytes": maxBytes})
	if err != nil {
		return nil, false, err
	}
	var out struct {
		Data      []byte `json:"data"`
		Truncated bool   `json:"truncated"`
	}
	if err := remarshalInto(data, &out); err != nil {
		return nil, false, err
	}
	return out.Data, out.Truncated, nil
}

func (w *RemoteWorkspace) Write(ctx context.Context, rel string, content []byte) (toolbroker.WriteReport, error) {
	data, err := w.call(ctx, runner.WorkspaceOpWrite, map[string]any{"path": rel, "data": content})
	if err != nil {
		return toolbroker.WriteReport{}, err
	}
	var out struct {
		Report toolbroker.WriteReport `json:"report"`
	}
	if err := remarshalInto(data, &out); err != nil {
		return toolbroker.WriteReport{}, err
	}
	return out.Report, nil
}

func (w *RemoteWorkspace) List(ctx context.Context, rel string) ([]toolbroker.DirEntry, error) {
	data, err := w.call(ctx, runner.WorkspaceOpList, map[string]any{"path": rel})
	if err != nil {
		return nil, err
	}
	var out struct {
		Entries []toolbroker.DirEntry `json:"entries"`
	}
	if err := remarshalInto(data, &out); err != nil {
		return nil, err
	}
	return out.Entries, nil
}

func (w *RemoteWorkspace) Stat(ctx context.Context, rel string) (toolbroker.FileStat, error) {
	data, err := w.call(ctx, runner.WorkspaceOpStat, map[string]any{"path": rel})
	if err != nil {
		return toolbroker.FileStat{}, err
	}
	var out struct {
		Stat toolbroker.FileStat `json:"stat"`
	}
	if err := remarshalInto(data, &out); err != nil {
		return toolbroker.FileStat{}, err
	}
	return out.Stat, nil
}

func (w *RemoteWorkspace) Checksum(ctx context.Context, rel string) (string, error) {
	data, err := w.call(ctx, runner.WorkspaceOpChecksum, map[string]any{"path": rel})
	if err != nil {
		return "", err
	}
	sum, _ := data["checksum"].(string)
	return sum, nil
}

// Glob asks the machine holding this allocation to search its own disk by filename. The pattern
// travels; the walk does not — a control plane that globbed its own filesystem here would answer for
// the wrong machine, and would answer at all only by accident of the two happening to be the same.
func (w *RemoteWorkspace) Glob(ctx context.Context, pattern string, limit int) ([]string, bool, error) {
	data, err := w.call(ctx, runner.WorkspaceOpGlob, map[string]any{"pattern": pattern, "limit": limit})
	if err != nil {
		return nil, false, err
	}
	// The wire carries JSON, so a []string arrives as []any. Anything that is not a string is dropped
	// rather than guessed at: a path this side cannot read as a path is not one it should return.
	raw, _ := data["paths"].([]any)
	paths := make([]string, 0, len(raw))
	for _, entry := range raw {
		if s, ok := entry.(string); ok {
			paths = append(paths, s)
		}
	}
	truncated, _ := data["truncated"].(bool)
	return paths, truncated, nil
}

func (w *RemoteWorkspace) Head(ctx context.Context) (string, string, error) {
	data, err := w.call(ctx, runner.WorkspaceOpHead, nil)
	if err != nil {
		return "", "", err
	}
	commit, _ := data["commit"].(string)
	tree, _ := data["tree"].(string)
	return commit, tree, nil
}

func (w *RemoteWorkspace) Commit(ctx context.Context, message string) (string, error) {
	data, err := w.call(ctx, runner.WorkspaceOpCommit, map[string]any{"message": message})
	if err != nil {
		return "", err
	}
	sha, _ := data["commit"].(string)
	return sha, nil
}

// remarshalInto re-marshals a decoded answer before decoding it into its struct, the same way
// decodeRelayFrame, decodeExecCommand and decodeExecAnswer do: a value is a map[string]any once it
// has crossed the wire and the struct itself when it has not.
func remarshalInto(data map[string]any, into any) error {
	encoded, err := json.Marshal(data)
	if err != nil {
		return fmt.Errorf("encode workspace answer: %w", err)
	}
	if err := json.Unmarshal(encoded, into); err != nil {
		return fmt.Errorf("decode workspace answer: %w", err)
	}
	return nil
}
