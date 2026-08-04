package execution_test

// A.3 T5's acceptance, and it is deliberately SIX tools rather than one.
//
// The plan's first shape drove only the shell tool, and a shell-only test is VACUOUS for this task:
// the shell tool already reached the machine after T3, so it would have gone green while
// `palai.workspace.file`, `palai.workspace.commit`, `palai.publish.push`,
// `palai.publish.pull_request` and `palai.workspace.show_media` all still read and wrote the CONTROL
// PLANE's filesystem — five dead tools under a green test. That is this tree's recorded lesson
// ("proving a mechanism is not proving the SURFACE a human uses") applied to its own plan.
//
// So every test here drives the SHIPPED tool — tools.FileTool().Exec and its five siblings, with the
// ExecEnv the orchestrator builds — over a REAL gateway, a REAL mTLS lease and the REAL
// packages/runner router, with the machine's WorkspaceServer on the far end. The only stand-ins are
// the machine's shell executor and the two registries a tool answers through (publications,
// artifacts), because neither is on the path this task changed.

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/palgroup/palai/adapters/sandboxes/oci/workspace"
	"github.com/palgroup/palai/apps/control-plane/internal/execution"
	"github.com/palgroup/palai/apps/control-plane/internal/execution/tools"
	"github.com/palgroup/palai/packages/contracts"
	"github.com/palgroup/palai/packages/runner"
	toolbroker "github.com/palgroup/palai/packages/tool-broker"
)

// machineDisk is the allocation as it exists ON THE MACHINE: a managed root, and one allocation under
// it laid out to §29.9. The control plane in these tests never opens either — it only names the
// allocation over the wire.
type machineDisk struct {
	root  string // the runner's PALAI_WORKSPACE_ROOT
	alloc string // <root>/alloc_<id>, the allocation the lease carries
}

func newMachineDisk(t *testing.T) machineDisk {
	t.Helper()
	// realTempDir semantics, spelled here because this package cannot reach the tools package's
	// helper: macOS puts t.TempDir() behind /var -> /private/var, and the machine resolves its root,
	// so an unresolved root would make every path comparison in this test compare two spellings.
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatalf("resolve machine root: %v", err)
	}
	alloc := filepath.Join(root, "alloc_t5")
	if err := os.MkdirAll(alloc, 0o755); err != nil {
		t.Fatalf("seed machine allocation: %v", err)
	}
	if err := workspace.Prepare(alloc); err != nil {
		t.Fatalf("lay out machine allocation: %v", err)
	}
	return machineDisk{root: root, alloc: alloc}
}

// servingMachine is the machine as cmd/runner assembles it for a lease: the SHIPPED router, with this
// test's shell executor and a workspace server rooted at this machine's managed root.
func servingMachine(disk machineDisk, shell toolbroker.ShellRunner) func(context.Context, *runner.LeaseSession) {
	return func(ctx context.Context, lease *runner.LeaseSession) {
		inbound := make(chan contracts.EngineFrame, 4)
		go func() {
			for range inbound { // drain: this machine relays no engine frame in these tests
			}
		}()
		runner.RelayInboundServing(ctx, lease, runner.MachineServers{
			Tools:     runner.NewToolServer(shell),
			Workspace: runner.NewWorkspaceServer(disk.root),
		}, inbound, func(string, ...any) {})
	}
}

// remoteWorkspaceOn is the type assertion the orchestrator makes (workspaceOpsFor): the value Dial
// returns is an EngineChannel, and the workspace surface is a SIBLING of that interface. A channel
// that fails it is an attempt whose coding tools have no machine to act on.
func remoteWorkspaceOn(t *testing.T, ch execution.EngineChannel, root string) *execution.RemoteWorkspace {
	t.Helper()
	conn, ok := ch.(execution.WorkspaceConn)
	if !ok {
		t.Fatalf("the channel Dial returned (%T) does not carry the workspace surface, so no attempt could edit a file on its machine", ch)
	}
	return execution.NewRemoteWorkspace(conn, root)
}

// recordingPublications records the publication a push / pull-request tool asked for, so the test can
// read the head those tools computed. It is the seam's whole surface.
type recordingPublications struct{ ops []map[string]any }

func (r *recordingPublications) RequestPublication(_ context.Context, _ toolbroker.TaskScope, op map[string]any) (map[string]any, error) {
	r.ops = append(r.ops, op)
	return map[string]any{"status": "pending_approval", "operation": op["operation"]}, nil
}

// recordingArtifactStore records what the media tool put in the object store — the BYTES, because a
// test that only checked an id came back would pass against a tool that stored the wrong file.
type recordingArtifactStore struct {
	content   []byte
	mediaType string
}

func (r *recordingArtifactStore) WriteArtifact(_ context.Context, _, _ string, content []byte, mediaType, _ string, _ map[string]any) (string, error) {
	r.content, r.mediaType = content, mediaType
	return "art_t5", nil
}

// initMachineRepo makes the allocation's repo dir a real git repository with one commit, the way a
// preparation leaves it. It runs on the MACHINE's disk, which in this process is the same filesystem
// — the separation this test measures is which SIDE OF THE WIRE reaches it, not which kernel.
func initMachineRepo(t *testing.T, disk machineDisk) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skipf("git not found: %v", err)
	}
	repo := workspace.RepoPath(disk.alloc)
	git := func(args ...string) string {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = repo
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=Palai", "GIT_AUTHOR_EMAIL=palai@example.invalid",
			"GIT_COMMITTER_NAME=Palai", "GIT_COMMITTER_EMAIL=palai@example.invalid")
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
		}
		return strings.TrimSpace(string(out))
	}
	git("init", "-q", "-b", "main")
	if err := os.WriteFile(filepath.Join(repo, "seed.txt"), []byte("seed\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	git("add", "seed.txt")
	git("commit", "-q", "-m", "seed")
}

// TestAllSixCodingToolsActOnTheMachineThatHoldsTheLease is the task's acceptance in one function, and
// it is one function on purpose: the claim is about the SET. Five of these tools reached the control
// plane's own filesystem before this task, and a per-tool test that happened to be written for four
// of them would have said nothing about the fifth.
func TestAllSixCodingToolsActOnTheMachineThatHoldsTheLease(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	disk := newMachineDisk(t)
	initMachineRepo(t, disk)

	machineExec := &recordingExecutor{result: toolbroker.ShellResult{ExitCode: 0, Stdout: "Darwin"}}
	ch := remoteShellFixture(t, ctx, "t5-six-token", "run_t5six", servingMachine(disk, machineExec))

	publications := &recordingPublications{}
	artifacts := &recordingArtifactStore{}
	env := toolbroker.ExecEnv{
		// The two fields together are what an attempt holds: the path ON THE MACHINE, and the surface
		// that reaches it. Nothing here is a filesystem handle this process could use.
		WorkspaceRoot: disk.alloc,
		Workspace:     remoteWorkspaceOn(t, ch, disk.alloc),
		Shell:         execution.NewRemoteShell(mustExecConn(t, ch)),
		Publications:  publications,
		Artifacts:     artifacts,
		Scope:         toolbroker.TaskScope{Project: "proj_t5", RunID: "run_t5six"},
	}

	// 1/6 — shell. The command reaches the machine's own executor, carrying the allocation as its cwd.
	if _, err := tools.ShellTool().Exec(ctx, env, map[string]any{"argv": []any{"uname", "-s"}}); err != nil {
		t.Fatalf("shell tool: %v", err)
	}
	ran := machineExec.commands()
	if len(ran) != 1 || len(ran[0].Argv) != 2 || ran[0].Argv[0] != "uname" {
		t.Fatalf("the shell command did not reach the machine's executor: %+v", ran)
	}
	if ran[0].WorkspaceRoot != disk.alloc {
		t.Fatalf("shell cwd = %q, want the machine's allocation %q", ran[0].WorkspaceRoot, disk.alloc)
	}

	// 2/6 — file write. The BYTES have to be on the machine's disk, at the machine's path.
	if _, err := tools.FileTool().Exec(ctx, env, map[string]any{
		"op": "write", "path": "repo/edit.txt", "content": "agent edit\n",
	}); err != nil {
		t.Fatalf("file write: %v", err)
	}
	onMachine, err := os.ReadFile(filepath.Join(workspace.RepoPath(disk.alloc), "edit.txt"))
	if err != nil {
		t.Fatalf("the file tool's write did not land on the machine: %v", err)
	}
	if string(onMachine) != "agent edit\n" {
		t.Fatalf("the machine holds %q, want the bytes the tool wrote", onMachine)
	}

	// 3/6 — file read. THE SECOND CALL IS THE ONE THAT MATTERS: a workspace that did not persist
	// between calls would answer an empty directory here, and a coding agent could do nothing at all.
	read, err := tools.FileTool().Exec(ctx, env, map[string]any{"op": "read", "path": "repo/edit.txt"})
	if err != nil {
		t.Fatalf("file read: %v", err)
	}
	if got, _ := read["content"].(string); got != "agent edit\n" {
		t.Fatalf("the second call read %q, so it did not see the workspace the first call wrote", got)
	}

	// 4/6 — commit. A REAL git commit in the repository on the machine's disk.
	committed, err := tools.CommitTool().Exec(ctx, env, map[string]any{"message": "agent edit"})
	if err != nil {
		t.Fatalf("commit tool: %v", err)
	}
	sha, _ := committed["commit"].(string)
	if sha == "" {
		t.Fatal("the commit tool returned no commit")
	}
	if head := gitHeadOnMachine(t, disk); head != sha {
		t.Fatalf("the machine's HEAD is %s and the tool reported %s — the commit landed somewhere else", head, sha)
	}

	// 5/6 — push, and 6/6's sibling: both read the head, and both must read the MACHINE's.
	if _, err := tools.PushTool().Exec(ctx, env, map[string]any{}); err != nil {
		t.Fatalf("push tool: %v", err)
	}
	if _, err := tools.PullRequestTool().Exec(ctx, env, map[string]any{"title": "agent edit"}); err != nil {
		t.Fatalf("pull request tool: %v", err)
	}
	if len(publications.ops) != 2 {
		t.Fatalf("recorded %d publications, want one push and one pull request", len(publications.ops))
	}
	for _, op := range publications.ops {
		if got, _ := op["head_sha"].(string); got != sha {
			t.Fatalf("%v recorded head_sha %q, want the machine's HEAD %q", op["operation"], got, sha)
		}
	}

	// 6/6 — media. The bytes that reach the store must be the bytes on the machine.
	shot := []byte("\x89PNG\r\n\x1a\nnot-really-a-png-but-these-exact-bytes")
	if err := os.WriteFile(filepath.Join(disk.alloc, "shot.png"), shot, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := tools.MediaTool().Exec(ctx, env, map[string]any{"path": "shot.png", "caption": "the screen"}); err != nil {
		t.Fatalf("media tool: %v", err)
	}
	if string(artifacts.content) != string(shot) {
		t.Fatalf("the store holds %q, want the file that is on the machine", artifacts.content)
	}
	if artifacts.mediaType != "image/png" {
		t.Fatalf("media type = %q, want image/png", artifacts.mediaType)
	}
}

// TestAWorkspaceToolRefusesRatherThanFallingBackToThisHostsDisk is the negative half, and without it
// the test above proves nothing about WHERE: this process and the machine share a filesystem in a
// test, so a tool that quietly opened the path itself would pass every assertion up there.
//
// It ends the lease connection and then asks for a write. The honest answer is a refusal; the answer
// that would mean the surface had rotted back is a file appearing on this host's disk. Both are
// checked, because a refusal that ALSO wrote would satisfy only the first.
func TestAWorkspaceToolRefusesRatherThanFallingBackToThisHostsDisk(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	disk := newMachineDisk(t)
	ch := remoteShellFixture(t, ctx, "t5-nofallback-token", "run_t5nofb", servingMachine(disk, &recordingExecutor{}))
	env := toolbroker.ExecEnv{WorkspaceRoot: disk.alloc, Workspace: remoteWorkspaceOn(t, ch, disk.alloc)}

	if err := ch.Close(); err != nil {
		t.Fatalf("close the lease: %v", err)
	}
	_, err := tools.FileTool().Exec(ctx, env, map[string]any{
		"op": "write", "path": "orphan.txt", "content": "written with no machine\n",
	})
	if err == nil {
		t.Fatal("a write with no machine succeeded — the tool reached a filesystem it should not have")
	}
	if _, statErr := os.Stat(filepath.Join(disk.alloc, "orphan.txt")); statErr == nil {
		t.Fatal("the file exists: the tool fell back to this host's disk instead of refusing")
	}
}

// TestAMachineRefusalKeepsItsSentinelAcrossTheWire is the part that would break silently rather than
// loudly. The file tool classifies by errors.Is, so a refused traversal is reported to the model as
// `refused` — the control fired, the path was not read — while an unclassifiable failure is reported
// as `failed`, which reads as a platform bug. An error rebuilt from JSON carries no sentinel unless
// the wire carries the CAUSE, so this asserts the ANSWER CODE and not merely that it failed.
func TestAMachineRefusalKeepsItsSentinelAcrossTheWire(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	disk := newMachineDisk(t)
	ch := remoteShellFixture(t, ctx, "t5-sentinel-token", "run_t5sent", servingMachine(disk, &recordingExecutor{}))
	env := toolbroker.ExecEnv{WorkspaceRoot: disk.alloc, Workspace: remoteWorkspaceOn(t, ch, disk.alloc)}

	_, err := tools.FileTool().Exec(ctx, env, map[string]any{"op": "read", "path": "../../etc/passwd"})
	answer, ok := toolbroker.AsAnswer(err)
	if !ok {
		t.Fatalf("a refused traversal = %v, want an answer (the read changed nothing)", err)
	}
	if answer.Code != toolbroker.AnswerRefused {
		t.Fatalf("a refused traversal came back as %q, want %q — the cause did not survive the wire, so every "+
			"confinement refusal now reads to the model as a platform failure", answer.Code, toolbroker.AnswerRefused)
	}
	// The message must still be the machine's own, and must name only the relative request.
	if strings.Contains(answer.Error(), disk.root) {
		t.Fatalf("the refusal carries the machine's host path: %q", answer.Error())
	}

	// A MISSING FILE IS A DIFFERENT CODE, and checking both is what keeps the one above honest: a wire
	// that collapsed every cause to `refused` would pass the first assertion on its own.
	_, err = tools.FileTool().Exec(ctx, env, map[string]any{"op": "read", "path": "no-such-file"})
	answer, ok = toolbroker.AsAnswer(err)
	if !ok {
		t.Fatalf("a missing file = %v, want an answer", err)
	}
	if answer.Code != toolbroker.AnswerNotFound {
		t.Fatalf("a missing file came back as %q, want %q", answer.Code, toolbroker.AnswerNotFound)
	}
}

// TestTheMachineRefusesAnAllocationOutsideItsManagedRoot pins the §24 boundary on the NEW surface. The
// path in a ws.request is control-plane-supplied exactly as a lease's workspace path is, so a control
// plane that named /etc must be refused by the machine rather than trusted.
func TestTheMachineRefusesAnAllocationOutsideItsManagedRoot(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	disk := newMachineDisk(t)
	ch := remoteShellFixture(t, ctx, "t5-outside-token", "run_t5out", servingMachine(disk, &recordingExecutor{}))

	// An allocation the machine never minted, in a directory that really exists on this host.
	outside, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatalf("resolve the outside directory: %v", err)
	}
	env := toolbroker.ExecEnv{WorkspaceRoot: outside, Workspace: remoteWorkspaceOn(t, ch, outside)}

	_, err = tools.FileTool().Exec(ctx, env, map[string]any{
		"op": "write", "path": "planted.txt", "content": "the control plane named this path\n",
	})
	if err == nil {
		t.Fatal("the machine accepted an allocation outside its managed root")
	}
	if _, statErr := os.Stat(filepath.Join(outside, "planted.txt")); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("the file was written outside the machine's root (stat: %v)", statErr)
	}
}

func gitHeadOnMachine(t *testing.T, disk machineDisk) string {
	t.Helper()
	cmd := exec.Command("git", "rev-parse", "HEAD")
	cmd.Dir = workspace.RepoPath(disk.alloc)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("read the machine's HEAD: %v\n%s", err, out)
	}
	return strings.TrimSpace(string(out))
}

func mustExecConn(t *testing.T, ch execution.EngineChannel) execution.ExecConn {
	t.Helper()
	conn, ok := ch.(execution.ExecConn)
	if !ok {
		t.Fatalf("the channel Dial returned (%T) does not carry the exec surface", ch)
	}
	return conn
}
