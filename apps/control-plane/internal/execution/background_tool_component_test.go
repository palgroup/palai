//go:build component

package execution

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/palgroup/palai/adapters/sandboxes/host"
	"github.com/palgroup/palai/apps/control-plane/internal/execution/tools"
	"github.com/palgroup/palai/packages/contracts"
	"github.com/palgroup/palai/packages/coordinator"
	toolbroker "github.com/palgroup/palai/packages/tool-broker"

	"github.com/palgroup/palai/storage"
)

// E26 T2 — THE TOOL SURFACE, against a REAL spine and REAL host processes.
//
// Everything here is driven BY TOOL NAME through the production dispatcher: dispatchTool resolves the
// tool out of the broker the deployment registers, runs the shipped approval gate, writes the shipped
// ledger row and delivers the shipped frame. That is deliberate and it is the brief's rule — a test that
// builds its own config never sees the config production builds, which is how a shell wall time managed
// to be simultaneously unbounded on the host and refusing every call in the container while every
// sandbox test was green.
//
// The liveness measurements come from the KERNEL (kill(-pgid, 0)) and from the process's own record of
// its process-group id, never from anything we returned. A test that asked our own bookkeeping whether a
// process was running would pass on the day the process stopped existing.

// alive asks the kernel whether a process GROUP still exists. Signal 0 performs the existence and
// permission checks and delivers nothing — the only probe here that is not also an effect.
func alive(pgid int) bool { return syscall.Kill(-pgid, 0) == nil }

// countingBackground wraps the real detached runner and counts what it was ASKED to start. The counter
// is the whole measurement in two of the proofs below: "did anything spawn" must not be a matter of
// interpretation, and both the approval gate and the kill switch are claims about ZERO.
type countingBackground struct {
	inner  toolbroker.BackgroundRunner
	starts int32
}

func (c *countingBackground) Start(ctx context.Context, cmd toolbroker.ShellCommand, spec toolbroker.BackgroundSpec) (toolbroker.Handle, error) {
	atomic.AddInt32(&c.starts, 1)
	return c.inner.Start(ctx, cmd, spec)
}

func (c *countingBackground) Probe(ctx context.Context, h toolbroker.Handle) (toolbroker.BackgroundStatus, error) {
	return c.inner.Probe(ctx, h)
}

func (c *countingBackground) Kill(ctx context.Context, h toolbroker.Handle) error {
	return c.inner.Kill(ctx, h)
}

// countingShell wraps the real synchronous runner and counts its calls. It exists for ONE assertion and
// it is the sharpest one in the file: with the feature disabled, `background: true` must be REFUSED and
// must not quietly run the command in the foreground. A silent downgrade means the model believes it
// backgrounded something that is in fact blocking, which is the exact behaviour this epic exists to
// avoid — and a counter is the only way to tell a refusal apart from a fallback that happened to be fast.
type countingShell struct {
	inner toolbroker.ShellRunner
	runs  int32
}

func (c *countingShell) Run(ctx context.Context, cmd toolbroker.ShellCommand) (toolbroker.ShellResult, error) {
	atomic.AddInt32(&c.runs, 1)
	return c.inner.Run(ctx, cmd)
}

// backgroundHarness is a real spine + a seeded running run + a real allocation root + the REAL host
// executor wearing both hats (it is a ShellRunner and a BackgroundRunner), each behind a counter.
type backgroundHarness struct {
	spine      *coordinator.Store
	tenant     coordinator.Tenant
	sessionID  string
	runID      string
	responseID string
	root       string
	shell      *countingShell
	background *countingBackground
	// machineID is the machine both counters belong to. Since A.3 T7 a background task is started BY the
	// machine, so `background` is reached through a machine caller rather than through a field on the
	// orchestrator — and the counter only moves if the attempt names this same id.
	machineID string
	gated     bool // register palai.workspace.shell as approval_required
	// orch is built ONCE and reused across every tool call, because that is what production does: main.go
	// constructs one Orchestrator in startDispatch and every attempt of every run dispatches through it.
	// The ledger tests in this package deliberately mint a fresh one per attempt to simulate a new process
	// — correct there, wrong here, and the difference is the whole point of the restart proof below.
	orch *Orchestrator
}

func newBackgroundHarness(t *testing.T) *backgroundHarness {
	t.Helper()
	cs, tenant, sessionID, runID := openLedgerSpine(t)
	// The host executor with an UNBOUNDED wall time: nothing here may be ended by a timeout, so a
	// measurement of a process that is still running cannot be confused with one the executor reaped.
	exec := host.NewExecutor(0)
	// A RESPONSE ROW, because a run in production always has one and background_tasks.response_id is a
	// foreign key to it (migration 000047). Since E26 T4 a spawn writes that row, so a harness without a
	// response would be a harness where no background task can start — which is not a property of the
	// tool surface, it is a gap in the fixture.
	responseID := redeliveryID("resp")
	if _, err := cs.Pool().Exec(storage.WithSystemScope(context.Background()),
		`INSERT INTO responses (id, project_id, session_id, state, input)
		 VALUES ($1,$2,$3,'in_progress','"go"'::jsonb)`,
		responseID, tenant.Project, sessionID); err != nil {
		t.Fatalf("seed the response: %v", err)
	}
	return &backgroundHarness{
		spine: cs, tenant: tenant, sessionID: sessionID, runID: runID, responseID: responseID,
		root:       t.TempDir(),
		shell:      &countingShell{inner: exec},
		background: &countingBackground{inner: exec},
		machineID:  redeliveryID("mac"),
	}
}

// attempt builds an orchestrator wired the way production wires one and a fresh attempt state at the
// given fence. The broker carries the REAL tools — the shipped ShellTool, FileTool and (once it exists)
// the background kill tool — because the surface under test is what a deployment registers.
func (h *backgroundHarness) attempt(fence uint64) (*Orchestrator, *attemptState, *recordingChannel) {
	if h.orch == nil {
		shellTool := tools.ShellTool()
		shellTool.RequiresApproval = h.gated
		h.orch = &Orchestrator{spine: h.spine, tools: toolbroker.New(shellTool, tools.FileTool(), tools.BackgroundKillTool())}
		// NEITHER RUNNER IS PROCESS-WIDE SINCE A.3. The synchronous one rides the attempt's channel below;
		// the detached one is reached by NAME through the machine caller, which is where production's comes
		// from too (main.go hands the gateway to SetMachineCaller). Wiring `background` onto the
		// orchestrator instead would leave the counter at zero and every spawn refused by the machine gate.
		h.orch.SetMachineCaller(newHostMachine(h.machineID, h.background))
	}
	ch := &recordingChannel{}
	orch := h.orch
	st := &attemptState{
		attempt: AttemptDescriptor{
			RunID: contracts.RunID(h.runID), AttemptID: contracts.AttemptID(redeliveryID("att")),
			Fence: fence, WorkspaceHostPath: h.root,
		},
		tenant:     h.tenant,
		sessionID:  h.sessionID,
		responseID: h.responseID,
	}
	// The attempt's machine, counted by the same countingShell the assertions read: a command reaches it
	// through the connection now, so wrapping the recording channel is how this harness says "this
	// attempt landed on a machine that runs commands".
	//
	// AND THE NAME IS THE SAME ONE THE CALLER ANSWERS FOR (A.3 T7), which is what makes the attempt able
	// to leave a process behind as well as run one. The two halves must agree: a channel naming a machine
	// the caller does not hold is refused as unreachable, exactly as a real lease on a revoked machine is.
	st.ch = hostMachineChannel{EngineChannel: ch, exec: h.shell, machineID: h.machineID}
	return orch, st, ch
}

// dispatch drives ONE tool call through the production dispatcher and returns the decoded result the
// model would have been handed, together with the dispatcher's own error.
func (h *backgroundHarness) dispatch(t *testing.T, name string, args map[string]any) (map[string]any, error) {
	t.Helper()
	orch, st, ch := h.attempt(1)
	err := orch.dispatchTool(context.Background(), st, toolRequestFrame(redeliveryID("tc"), name, args))
	if err != nil {
		return nil, err
	}
	results := toolResults(ch)
	if len(results) != 1 {
		t.Fatalf("%s delivered %d tool.result frames, want exactly 1: %+v", name, len(results), results)
	}
	var out map[string]any
	if uerr := json.Unmarshal([]byte(results[0].content), &out); uerr != nil {
		t.Fatalf("decode %s result %q: %v", name, results[0].content, uerr)
	}
	return out, nil
}

func (h *backgroundHarness) mustDispatch(t *testing.T, name string, args map[string]any) map[string]any {
	t.Helper()
	out, err := h.dispatch(t, name, args)
	if err != nil {
		t.Fatalf("dispatchTool(%s) error = %v", name, err)
	}
	return out
}

// bgScript is one shell line that records its OWN process-group id and then does the given work. The
// pgid is read back from that file and nowhere else, so every liveness assertion below is the kernel's
// answer about a number the PROCESS reported.
func bgScript(pidFile, work string) string { return "echo $$ > " + pidFile + "; " + work }

// spawn starts one background task through the shell tool and returns its ticket plus the process-group
// id the command wrote. It registers a cleanup that kills the group: this suite starts real detached
// processes and nothing else will reap them.
func (h *backgroundHarness) spawn(t *testing.T, label, work string) (ticket map[string]any, pgid int) {
	t.Helper()
	pidFile := filepath.Join(h.root, label+".pgid")
	ticket = h.mustDispatch(t, "palai.workspace.shell", map[string]any{
		"argv": []any{bgScript(pidFile, work)}, "shell": true, "background": true,
	})
	pgid = pgidFrom(t, pidFile)
	t.Cleanup(func() { _ = syscall.Kill(-pgid, syscall.SIGKILL) })
	return ticket, pgid
}

// pgidFrom waits for a started shell to write its own process-group id, and fails if it never does.
func pgidFrom(t *testing.T, path string) int {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if raw, err := os.ReadFile(path); err == nil {
			if pid, cerr := strconv.Atoi(strings.TrimSpace(string(raw))); cerr == nil && pid > 0 {
				return pid
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("no process-group id was ever written to %s; the command never ran", path)
	return 0
}

// readWorkspaceFile reads a path through the SHIPPED file tool, which is the whole point of §2.2: no new
// read tool is added, because the output is a file and palai.workspace.file already reads files. The
// vendor harness this replicates reached the same conclusion and deprecated its own TaskOutput tool.
func (h *backgroundHarness) readWorkspaceFile(t *testing.T, rel string) string {
	t.Helper()
	out := h.mustDispatch(t, "palai.workspace.file", map[string]any{"op": "read", "path": rel})
	content, _ := out["content"].(string)
	return content
}

func lineCount(s string) int {
	s = strings.TrimRight(s, "\n")
	if s == "" {
		return 0
	}
	return strings.Count(s, "\n") + 1
}

// ---------------------------------------------------------------------------------------------------
// RED 1 (§2.1) — the tool returns a HANDLE, not a result, and it returns while the process is running.
// ---------------------------------------------------------------------------------------------------

// TestABackgroundShellCallReturnsAHandleWhileTheProcessIsStillRunning is §2.1. The measurement is not a
// stopwatch alone — a fast return proves nothing if the command was never started — it is a SEQUENCE:
// the call returned, and at that moment the kernel still lists the process group.
func TestABackgroundShellCallReturnsAHandleWhileTheProcessIsStillRunning(t *testing.T) {
	h := newBackgroundHarness(t)

	started := time.Now()
	ticket, pgid := h.spawn(t, "sleeper", "sleep 30")
	elapsed := time.Since(started)

	if !alive(pgid) {
		t.Fatalf("process group %d is gone the moment the tool returned: nothing was backgrounded", pgid)
	}
	// 30 seconds is what the command takes. Anything close to it means the call waited.
	if elapsed > 10*time.Second {
		t.Fatalf("the background call took %s: it waited for the command instead of handing back a handle", elapsed)
	}
	taskID, _ := ticket["task_id"].(string)
	if taskID == "" {
		t.Fatalf("the background call returned no task_id: %+v", ticket)
	}
	outputPath, _ := ticket["output_path"].(string)
	if want := ".palai-session/bg/" + taskID + ".log"; outputPath != want {
		t.Fatalf("output_path = %q, want %q", outputPath, want)
	}
	if status, _ := ticket["status"].(string); status != "running" {
		t.Fatalf("status = %q, want running", status)
	}
	// THE BODY IS MINIMAL AND THAT IS A CONTEXT DECISION (§T2): a five minute build's whole output is
	// not put into the model's context by a call the model made to AVOID waiting for it. Seeing the
	// output takes an explicit read.
	for _, forbidden := range []string{"stdout", "stderr", "content", "output"} {
		if _, present := ticket[forbidden]; present {
			t.Fatalf("the spawn result carries %q; a handle is not a result: %+v", forbidden, ticket)
		}
	}
}

// ---------------------------------------------------------------------------------------------------
// RED 2 (§2.2) — the output is readable MID-FLIGHT, through the file tool that already exists.
// ---------------------------------------------------------------------------------------------------

// TestABackgroundTasksOutputIsReadableMidFlight is §2.2, and it is what makes the feature usable rather
// than merely asynchronous: the model decides whether to keep waiting, act on early output, or kill the
// task, and it can only decide that if it can SEE the output before the end.
//
// Two consecutive reads with a rising line count, both taken while the kernel still lists the process.
func TestABackgroundTasksOutputIsReadableMidFlight(t *testing.T) {
	h := newBackgroundHarness(t)
	ticket, pgid := h.spawn(t, "chatty", "i=0; while [ $i -lt 200 ]; do echo \"line $i\"; i=$((i+1)); sleep 0.1; done")
	outputPath, _ := ticket["output_path"].(string)
	if outputPath == "" {
		t.Fatalf("the spawn named no output path, so there is nothing to read mid-flight: %+v", ticket)
	}

	// Give the command time to produce a first few lines; it writes one every 100ms for 20 seconds.
	time.Sleep(700 * time.Millisecond)
	first := h.readWorkspaceFile(t, outputPath)
	if !alive(pgid) {
		t.Fatalf("the command finished before the first read; the fixture cannot measure a partial read")
	}
	time.Sleep(900 * time.Millisecond)
	second := h.readWorkspaceFile(t, outputPath)
	if !alive(pgid) {
		t.Fatalf("the command finished before the second read; the fixture cannot measure a partial read")
	}

	firstLines, secondLines := lineCount(first), lineCount(second)
	if firstLines == 0 {
		t.Fatalf("the first mid-flight read returned nothing; output is not readable before the end")
	}
	if secondLines <= firstLines {
		t.Fatalf("line count did not rise between two mid-flight reads (%d then %d): the read is not partial",
			firstLines, secondLines)
	}
	if !strings.HasPrefix(second, first) {
		t.Fatalf("the second read is not an extension of the first; the log is being rewritten rather than appended")
	}
}

// ---------------------------------------------------------------------------------------------------
// RED 3 (§2.6) — THE DEFINING TEST OF THIS EPIC. The model does not block.
// ---------------------------------------------------------------------------------------------------

// TestTheModelCallsAnotherToolWhileTheBackgroundProcessIsStillRunning is §2.6, the property most likely
// to be built wrong, and the test is written to DISTINGUISH the two things that would both look like
// success from the outside.
//
// A park-and-wait would also eventually let a second tool run — after the first attempt ENDED and a
// later one resumed. That is not this feature. So two asserts stand here together:
//
//	THE PARK ASSERT: the spawning dispatch returned NIL — not errRunPaused, not errRunParked —
//	the run is still `running` in the database, and the model was handed a tool.result to continue on.
//	The attempt did not end.
//
//	THE SECOND-TOOL ASSERT: a different tool call, made after the spawn, COMPLETED and returned real
//	content — while the kernel still listed the background process.
//
// Neither alone is the claim. Together they are.
func TestTheModelCallsAnotherToolWhileTheBackgroundProcessIsStillRunning(t *testing.T) {
	h := newBackgroundHarness(t)
	ctx := context.Background()

	// One attempt, two tool calls — exactly as a model emits them: it gets an answer to the first and
	// keeps working in the SAME turn.
	orch, st, ch := h.attempt(1)
	pidFile := filepath.Join(h.root, "defining.pgid")
	spawnErr := orch.dispatchTool(ctx, st, toolRequestFrame(redeliveryID("tc"), "palai.workspace.shell", map[string]any{
		"argv": []any{bgScript(pidFile, "sleep 30")}, "shell": true, "background": true,
	}))

	// --- THE PARK ASSERT: the attempt did not end. ---
	if spawnErr != nil {
		t.Fatalf("the spawning dispatch returned %v: the model was parked instead of answered", spawnErr)
	}
	if errors.Is(spawnErr, errRunParked) || errors.Is(spawnErr, errRunPaused) {
		t.Fatalf("the spawning dispatch parked the run (%v); background execution is not a park", spawnErr)
	}
	var runState string
	if err := h.spine.Pool().QueryRow(storage.WithSystemScope(ctx),
		`SELECT state FROM runs WHERE id = $1`, h.runID).Scan(&runState); err != nil {
		t.Fatalf("read run state: %v", err)
	}
	if runState != "running" {
		t.Fatalf("run state = %q after a background spawn, want running: the run stopped to wait", runState)
	}
	if got := toolResults(ch); len(got) != 1 {
		t.Fatalf("the model was handed %d tool.result frames for the spawn, want 1: it has nothing to continue on", len(got))
	}

	pgid := pgidFrom(t, pidFile)
	t.Cleanup(func() { _ = syscall.Kill(-pgid, syscall.SIGKILL) })

	// --- THE SECOND-TOOL ASSERT: another call, in the same attempt, completes. ---
	if !alive(pgid) {
		t.Fatalf("the background process is already gone; there is nothing to be concurrent with")
	}
	secondErr := orch.dispatchTool(ctx, st, toolRequestFrame(redeliveryID("tc"), "palai.workspace.file", map[string]any{
		"op": "write", "path": "notes.md", "content": "the model kept working",
	}))
	if secondErr != nil {
		t.Fatalf("the second tool call failed while a background task was running: %v", secondErr)
	}
	if !alive(pgid) {
		t.Fatalf("the background process ended before the second tool call finished; the two did not overlap")
	}
	results := toolResults(ch)
	if len(results) != 2 {
		t.Fatalf("the attempt delivered %d tool.result frames, want 2 (the spawn and the second call)", len(results))
	}
	var second map[string]any
	if err := json.Unmarshal([]byte(results[1].content), &second); err != nil {
		t.Fatalf("decode the second result %q: %v", results[1].content, err)
	}
	if second["path"] != "notes.md" {
		t.Fatalf("the second tool call returned %+v, not a completed write", second)
	}
	if body, err := os.ReadFile(filepath.Join(h.root, "notes.md")); err != nil || string(body) != "the model kept working" {
		t.Fatalf("the second tool call did not actually run: %q, %v", string(body), err)
	}
}

// ---------------------------------------------------------------------------------------------------
// RED 4 (§2.5) — several tasks run concurrently and their outputs do not interleave.
// ---------------------------------------------------------------------------------------------------

// TestThreeBackgroundTasksRunConcurrentlyWithoutInterleavingTheirOutput is §2.5. Concurrency is measured
// by all three process groups being alive AT THE SAME MOMENT, and separation by each log containing its
// own marker and NEITHER of the other two — a log file per task is what makes "read the output of task
// two" a question with an answer.
func TestThreeBackgroundTasksRunConcurrentlyWithoutInterleavingTheirOutput(t *testing.T) {
	h := newBackgroundHarness(t)

	markers := []string{"alpha", "bravo", "charlie"}
	paths := make([]string, len(markers))
	pgids := make([]int, len(markers))
	for i, marker := range markers {
		// Each writes its own marker twenty times over two seconds, so all three are still writing while
		// the others are.
		ticket, pgid := h.spawn(t, marker,
			"i=0; while [ $i -lt 20 ]; do echo "+marker+"; i=$((i+1)); sleep 0.1; done")
		paths[i], _ = ticket["output_path"].(string)
		pgids[i] = pgid
	}

	for i, pgid := range pgids {
		if !alive(pgid) {
			t.Fatalf("task %s (pgid %d) is not running while the other two are; they did not overlap", markers[i], pgid)
		}
	}
	if paths[0] == paths[1] || paths[1] == paths[2] || paths[0] == paths[2] {
		t.Fatalf("two tasks share an output path: %v", paths)
	}

	// Let them finish, then read each log through the shipped file tool.
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) && (alive(pgids[0]) || alive(pgids[1]) || alive(pgids[2])) {
		time.Sleep(100 * time.Millisecond)
	}
	for i, marker := range markers {
		body := h.readWorkspaceFile(t, paths[i])
		if lineCount(body) != 20 {
			t.Fatalf("task %s wrote %d lines, want 20: %q", marker, lineCount(body), body)
		}
		for _, other := range markers {
			if other == marker {
				continue
			}
			if strings.Contains(body, other) {
				t.Fatalf("task %s's log contains %s's output: the three tasks interleaved into one file", marker, other)
			}
		}
	}
}

// ---------------------------------------------------------------------------------------------------
// RED 5 — THE APPROVAL GATE. Background must not become a side door around E23.
// ---------------------------------------------------------------------------------------------------

// TestAGatedToolCalledWithBackgroundSpawnsNothingUntilAHumanDecides is a STRUCTURAL ORDERING claim, and
// the counter is what makes it one: the gate lives in dispatchTool and the spawn happens inside Execute,
// which runs after it. If a background spawn were ever hoisted above the gate — a "start it now, ask
// afterwards" that would look perfectly reasonable in a diff — this counter would read one.
//
// The whole point of a gate is that nothing has happened yet. A killed background process is not the
// same as a process that never ran.
//
// AND THE NON-VACUITY HALF IS IN THE SAME TEST, because without it this proof is worth nothing. Measured
// at the fork point (d634c1a6): the gated assertion PASSED before a line of T2 was written — there was
// no spawn path at all, so "the counter reads zero" was free. A security proof that cannot fail is a
// security proof that reports the same green as one that passes. So the identical call, on the identical
// harness, with the gate OFF must read ONE.
func TestAGatedToolCalledWithBackgroundSpawnsNothingUntilAHumanDecides(t *testing.T) {
	ungated := newBackgroundHarness(t)
	ungated.spawn(t, "ungated", "sleep 20")
	if n := atomic.LoadInt32(&ungated.background.starts); n != 1 {
		t.Fatalf("the SAME call without the gate started %d background task(s), want 1: the counter below "+
			"cannot distinguish a gate from a feature that does not exist", n)
	}

	h := newBackgroundHarness(t)
	h.gated = true

	_, err := h.dispatch(t, "palai.workspace.shell", map[string]any{
		"argv": []any{"sleep 30"}, "shell": true, "background": true,
	})
	if !errors.Is(err, errRunParked) {
		t.Fatalf("dispatchTool error = %v, want errRunParked", err)
	}
	if n := atomic.LoadInt32(&h.background.starts); n != 0 {
		t.Fatalf("a gated tool started %d background task(s) with NO human decision, want 0", n)
	}
	if n := atomic.LoadInt32(&h.shell.runs); n != 0 {
		t.Fatalf("a gated tool ran %d synchronous command(s) with NO human decision, want 0", n)
	}
	var callState string
	if err := h.spine.Pool().QueryRow(storage.WithSystemScope(context.Background()),
		`SELECT state FROM tool_calls WHERE run_id = $1`, h.runID).Scan(&callState); err != nil {
		t.Fatalf("read tool_call state: %v", err)
	}
	if callState != "approval_pending" {
		t.Fatalf("tool_call state = %q, want approval_pending", callState)
	}
	// And no log file was created either: the spawn path did not get far enough to touch the allocation.
	if entries, err := os.ReadDir(filepath.Join(h.root, ".palai-session", "bg")); err == nil && len(entries) > 0 {
		t.Fatalf("a gated background call created %d output file(s) before a human decided", len(entries))
	}
}

// ---------------------------------------------------------------------------------------------------
// RED 6 (§3.5 P6) — a backgrounded `cd` does not carry over.
// ---------------------------------------------------------------------------------------------------

// TestABackgroundedDirectoryChangeDoesNotCarryOverToTheNextCommand turns a structural accident into a
// DECISION. The vendor harness states it as a rule it enforces ("Session cwd remains <dir>; directory
// changes made by the backgrounded command do not apply to subsequent commands"); in Palai it is true
// because every command gets its own process with c.Dir set from the allocation root, which is a
// property nothing was defending. It is defended here.
func TestABackgroundedDirectoryChangeDoesNotCarryOverToTheNextCommand(t *testing.T) {
	h := newBackgroundHarness(t)
	if err := os.MkdirAll(filepath.Join(h.root, "elsewhere"), 0o755); err != nil {
		t.Fatalf("seed a directory to wander into: %v", err)
	}
	_, pgid := h.spawn(t, "wanderer", "cd elsewhere; sleep 20")

	if !alive(pgid) {
		t.Fatalf("the backgrounded command is gone; it never had a chance to change anything")
	}
	out := h.mustDispatch(t, "palai.workspace.shell", map[string]any{"argv": []any{"pwd"}})
	got := strings.TrimSpace(asString(out["stdout"]))

	// The allocation root through the machine's own symlink resolution: on macOS a temp dir is reached
	// via /var while `pwd` reports /private/var, and comparing the unresolved strings would fail for a
	// reason that has nothing to do with this claim.
	want, err := filepath.EvalSymlinks(h.root)
	if err != nil {
		t.Fatalf("resolve the allocation root: %v", err)
	}
	if got != want {
		t.Fatalf("the next command ran in %q, want the allocation root %q: a backgrounded cd carried over", got, want)
	}
}

// ---------------------------------------------------------------------------------------------------
// RED 7 (§3.5 P5) — the kill switch REFUSES; it does not downgrade.
// ---------------------------------------------------------------------------------------------------

// TestBackgroundIsRefusedRatherThanDowngradedWhenTheFeatureIsDisabled is §3.5 P5, and the second counter
// is the entire test. A silent fall back to a synchronous run would satisfy any assertion about "no
// background task was started" while doing the one thing this feature exists to prevent: blocking the
// model on a command it asked not to wait for.
//
// So: the call is refused, no task spawns, AND the command does not run in the foreground either.
func TestBackgroundIsRefusedRatherThanDowngradedWhenTheFeatureIsDisabled(t *testing.T) {
	t.Setenv("PALAI_BACKGROUND_DISABLED", "1")
	h := newBackgroundHarness(t)

	_, err := h.dispatch(t, "palai.workspace.shell", map[string]any{
		"argv": []any{"echo hello"}, "background": true,
	})
	if err == nil {
		t.Fatal("background: true was accepted while the feature is disabled")
	}
	// THE REFUSAL MUST NAME THE KILL SWITCH, not merely the word "background". Both refusals in this path
	// contain that word — the switch, and A.3 T7's machine gate ("this shell posture cannot start a
	// background task") — so a Contains("background") check cannot tell them apart, and a harness that
	// lost its machine would keep this test green while measuring an entirely different refusal.
	if !strings.Contains(err.Error(), "PALAI_BACKGROUND_DISABLED") {
		t.Fatalf("the refusal does not name the kill switch, so this may be some other refusal entirely: %v", err)
	}
	if n := atomic.LoadInt32(&h.background.starts); n != 0 {
		t.Fatalf("%d background task(s) started while the feature is disabled, want 0", n)
	}
	if n := atomic.LoadInt32(&h.shell.runs); n != 0 {
		t.Fatalf("the disabled background call ran the command SYNCHRONOUSLY (%d run(s)): a silent downgrade "+
			"blocks the model in exactly the way background execution exists to prevent", n)
	}
	// The same call WITHOUT the flag is untouched by the switch: it disables background execution, not
	// the shell tool.
	out := h.mustDispatch(t, "palai.workspace.shell", map[string]any{"argv": []any{"echo", "hello"}})
	if got := strings.TrimSpace(asString(out["stdout"])); got != "hello" {
		t.Fatalf("a non-background call under the kill switch returned %q, want hello", got)
	}
}

// ---------------------------------------------------------------------------------------------------
// The kill tool (§3.5 P3/P7) and the bit-unchanged guarantee (§2).
// ---------------------------------------------------------------------------------------------------

// TestBackgroundKillStopsTheProcessGroupAndKillingTwiceIsKillingOnce is P3 and P7 together. The
// measurement is the kernel's: the group is gone. The second call is P7's "cancelling twice is
// idempotent" taken literally — it is a normal answer, not an error, because a model that killed a task
// that had already exited did exactly what it meant to.
func TestBackgroundKillStopsTheProcessGroupAndKillingTwiceIsKillingOnce(t *testing.T) {
	h := newBackgroundHarness(t)
	ticket, pgid := h.spawn(t, "doomed", "sleep 60")
	taskID, _ := ticket["task_id"].(string)

	if !alive(pgid) {
		t.Fatalf("the task is not running; there is nothing to kill")
	}
	first := h.mustDispatch(t, "palai.workspace.background_kill", map[string]any{"task_id": taskID})
	if status, _ := first["status"].(string); status == "running" {
		t.Fatalf("the kill reported the task still running: %+v", first)
	}
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) && alive(pgid) {
		time.Sleep(20 * time.Millisecond)
	}
	if alive(pgid) {
		t.Fatalf("process group %d survived the kill", pgid)
	}
	// Twice is once.
	second, err := h.dispatch(t, "palai.workspace.background_kill", map[string]any{"task_id": taskID})
	if err != nil {
		t.Fatalf("killing an already-dead task is an error: %v", err)
	}
	if second["task_id"] != taskID {
		t.Fatalf("the second kill answered about %+v, want task %s", second, taskID)
	}
	// A task id nobody started is refused rather than answered: a model that mistyped an id must not be
	// told the task is dead.
	if _, err := h.dispatch(t, "palai.workspace.background_kill", map[string]any{"task_id": "bgt_notarealtask"}); err == nil {
		t.Fatal("killing an unknown task id succeeded; an unknown handle must be refused")
	}
}

// TestABackgroundTaskCannotBeKilledByIdAfterAControlPlaneRestart is this task's HONEST CEILING, written
// as a measurement rather than as a paragraph in a commit message. T2 holds a started task's handle IN
// MEMORY: migration 000047 opened the durable row, and the three things that read it — the park gate, the
// exit notification and the reaper — are T3, T4 and T5.
//
// So the claim has two halves and BOTH are asserted, because only together are they honest:
//
//	The kill by id FAILS after a restart, and fails loudly rather than reporting a task it never stopped.
//	The PROCESS IS UNAFFECTED — it keeps running, which is T1's whole point and the reason this ceiling is
//	survivable rather than a broken feature: the task belongs to the run, not to the process that started
//	it, and what is missing is only this control plane's ability to name it again.
//
// This is T5's RED, already failing, in the place a reader of T2 will look for it.
// TestABackgroundTaskCannotBeKilledByIdAfterAControlPlaneRestart WAS HERE, and E26 T5 deleted it rather
// than relaxing it — which is what its own failure message asked for. It pinned T2's honest ceiling: the
// id -> handle mapping lived in a map built at spawn time, so a restarted control plane could not stop a
// build it had started. T5 replaced that map with the background_tasks row, and the claim is now the
// opposite one, proven in background_reaper_component_test.go by
// TestARestartedControlPlaneAdoptsItsRunningTasksFromTheRowRatherThanFromMemory — against a genuinely
// second control plane (its own store handle, its own orchestrator, its own executor) rather than a
// nil-ed field.

// nonBackgroundShellResultKeys is the EXACT key set a shell tool call produced before background
// execution existed, recorded by running this test against the tree at d634c1a6 (E26 T1's merge, the
// fork point of T2). It is a committed word rather than a recomputed baseline: a guard that derives its
// own expectation from the code it is guarding passes over every regression it exists to catch.
//
// `signal` and `egress_findings` are conditional keys the shipped code adds only when they apply, and
// the fixture below applies neither.
var nonBackgroundShellResultKeys = []string{
	"duration_ms", "exit_code", "oom_killed", "stderr", "stdout", "timed_out", "truncated",
}

// TestAShellCallWithoutBackgroundIsFieldForFieldUnchanged is §2's bit-unchanged clause at the DISPATCH
// level: not the seam structs (packages/tool-broker pins those by reflection), but what a run that never
// mentions `background` actually produces — the keys the model receives and the ledger row an auditor
// reads.
//
// The background runner's counter reading zero is the other half: an unchanged result would mean nothing
// if the call had quietly detached and then waited for the process.
func TestAShellCallWithoutBackgroundIsFieldForFieldUnchanged(t *testing.T) {
	h := newBackgroundHarness(t)
	callID := redeliveryID("tc")
	args := map[string]any{"argv": []any{"echo", "unchanged"}}

	orch, st, ch := h.attempt(1)
	if err := orch.dispatchTool(context.Background(), st, toolRequestFrame(callID, "palai.workspace.shell", args)); err != nil {
		t.Fatalf("dispatchTool error = %v", err)
	}
	results := toolResults(ch)
	if len(results) != 1 {
		t.Fatalf("delivered %d tool.result frames, want 1", len(results))
	}
	var delivered map[string]any
	if err := json.Unmarshal([]byte(results[0].content), &delivered); err != nil {
		t.Fatalf("decode delivered result: %v", err)
	}
	if got := sortedKeys(delivered); !equalStrings(got, nonBackgroundShellResultKeys) {
		t.Fatalf("a non-background shell result carries keys %v, want the committed pre-E26 set %v",
			got, nonBackgroundShellResultKeys)
	}
	if delivered["exit_code"] != float64(0) || strings.TrimSpace(asString(delivered["stdout"])) != "unchanged" {
		t.Fatalf("a non-background shell call did not run as it did before: %+v", delivered)
	}
	if n := atomic.LoadInt32(&h.background.starts); n != 0 {
		t.Fatalf("a call that never mentioned background started %d background task(s)", n)
	}
	if n := atomic.LoadInt32(&h.shell.runs); n != 1 {
		t.Fatalf("the synchronous runner was called %d time(s), want exactly 1", n)
	}

	// The ledger row, field for field. state/replay_class/name/arguments and the committed result are
	// what a replay, a reconcile and an auditor all read.
	var state, replayClass, name, arguments, result string
	if err := h.spine.Pool().QueryRow(storage.WithSystemScope(context.Background()),
		`SELECT state, replay_class, name, arguments::text, result::text FROM tool_calls WHERE id = $1`, callID).
		Scan(&state, &replayClass, &name, &arguments, &result); err != nil {
		t.Fatalf("read the ledger row: %v", err)
	}
	if state != "completed" || replayClass != "irreversible" || name != "palai.workspace.shell" {
		t.Fatalf("ledger row = (%s, %s, %s), want (completed, irreversible, palai.workspace.shell)", state, replayClass, name)
	}
	var storedArgs map[string]any
	if err := json.Unmarshal([]byte(arguments), &storedArgs); err != nil {
		t.Fatalf("decode ledger arguments %q: %v", arguments, err)
	}
	if got := sortedKeys(storedArgs); !equalStrings(got, []string{"argv"}) {
		t.Fatalf("the ledger recorded arguments %v; a call that named no background parameter must record none", got)
	}
	var storedResult map[string]any
	if err := json.Unmarshal([]byte(result), &storedResult); err != nil {
		t.Fatalf("decode ledger result %q: %v", result, err)
	}
	if got := sortedKeys(storedResult); !equalStrings(got, nonBackgroundShellResultKeys) {
		t.Fatalf("the ledger recorded result keys %v, want the committed pre-E26 set %v", got, nonBackgroundShellResultKeys)
	}
}

func sortedKeys(m map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sortStrings(out)
	return out
}

func sortStrings(xs []string) {
	for i := 1; i < len(xs); i++ {
		for j := i; j > 0 && xs[j] < xs[j-1]; j-- {
			xs[j], xs[j-1] = xs[j-1], xs[j]
		}
	}
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
