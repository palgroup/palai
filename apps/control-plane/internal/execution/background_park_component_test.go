//go:build component

package execution

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/palgroup/palai/adapters/sandboxes/host"
	"github.com/palgroup/palai/apps/control-plane/internal/execution/tools"
	"github.com/palgroup/palai/apps/control-plane/internal/store"
	"github.com/palgroup/palai/packages/contracts"
	"github.com/palgroup/palai/packages/coordinator"
	modelbroker "github.com/palgroup/palai/packages/model-broker"
	toolbroker "github.com/palgroup/palai/packages/tool-broker"

	"github.com/palgroup/palai/storage"
)

// E26 T3 — THE PARK, driven end to end through ExecuteAttempt against a REAL Postgres and REAL
// detached host processes.
//
// EVERY PROOF HERE GOES THROUGH THE WHOLE SHIPPED ATTEMPT, not through finalize() called by hand. That
// is deliberate and it is the difference between proving a function and proving a behaviour: the claim
// is that a RUN does not finish, and the thing that finishes a run is ExecuteAttempt — the run
// transitions, the tool dispatch, the frame loop's terminal arm and the response projection are all in
// it, and a test that skipped to the last of them could not tell a park apart from a finalize that
// happened to write the same row.
//
// The background task is started the way a model starts one: a `tool.request` for
// palai.workspace.shell with `background: true`, dispatched by the production dispatcher through the
// shipped tool. The liveness that decides the park is then read from the KERNEL by the shipped runner's
// own Probe, never from anything the test remembered.

// parkScript is one shell line that records its OWN process-group id and then blocks. The pgid is read
// back from that file and nowhere else, so the cleanup below kills a group the PROCESS named.
func parkScript(pidFile string) string { return "echo $$ > " + pidFile + "; sleep 30" }

// parkFixture is a real spine, a seeded tenant with a real pool, a queued run with its own response,
// a real allocation root, and the REAL host executor wearing both hats (ShellRunner and
// BackgroundRunner).
type parkFixture struct {
	spine      *coordinator.Store
	repo       *store.Store
	tenant     coordinator.Tenant
	sessionID  string
	responseID string
	runID      string
	root       string
	orch       *Orchestrator
	// exec is this fixture's machine: since A.3 the synchronous executor rides the attempt's channel,
	// so every scripted dialer below carries it.
	exec toolbroker.ShellRunner
}

func newParkFixture(t *testing.T) *parkFixture {
	t.Helper()
	url := os.Getenv("PALAI_COMPONENT_POSTGRES_URL")
	if url == "" {
		t.Skip("PALAI_COMPONENT_POSTGRES_URL is required; run TEST=postgres scripts/test/component")
	}
	ctx := context.Background()
	cs, err := coordinator.Open(ctx, url)
	if err != nil {
		t.Fatalf("coordinator.Open: %v", err)
	}
	t.Cleanup(cs.Close)
	if err := cs.Migrate(ctx); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	repo, err := store.Open(ctx, url)
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(repo.Close)

	f := &parkFixture{
		spine: cs, repo: repo, root: t.TempDir(),
		tenant:    coordinator.Tenant{Organization: redeliveryID("org"), Project: redeliveryID("prj")},
		sessionID: redeliveryID("ses"), responseID: redeliveryID("resp"), runID: redeliveryID("run"),
	}
	sys := storage.WithSystemScope(ctx)
	// The pool is seeded because runs.pool_id carries a foreign key: `place` resolves the tenant's
	// default pool and records it before the dial, so a fixture without one fails on the FK rather than
	// on anything this file is about.
	stmts := [][]any{
		{`INSERT INTO organizations (id) VALUES ($1)`, f.tenant.Organization},
		{`INSERT INTO projects (id, organization_id) VALUES ($1, $2)`, f.tenant.Project, f.tenant.Organization},
		{`INSERT INTO runner_pools (id, organization_id, project_id, name, posture)
		  VALUES ($1,$2,$3,'default','unsandboxed-host')`, redeliveryID("pool"), f.tenant.Organization, f.tenant.Project},
		{`INSERT INTO sessions (id, organization_id, project_id) VALUES ($1,$2,$3)`, f.sessionID, f.tenant.Organization, f.tenant.Project},
		{`INSERT INTO responses (id, organization_id, project_id, session_id, state, input)
		  VALUES ($1,$2,$3,$4,'queued','"build it"'::jsonb)`, f.responseID, f.tenant.Organization, f.tenant.Project, f.sessionID},
		{`INSERT INTO runs (id, organization_id, project_id, session_id, response_id, state)
		  VALUES ($1,$2,$3,$4,$5,'queued')`, f.runID, f.tenant.Organization, f.tenant.Project, f.sessionID, f.responseID},
	}
	for _, stmt := range stmts {
		if _, err := cs.Pool().Exec(sys, stmt[0].(string), stmt[1:]...); err != nil {
			t.Fatalf("seed the run: %v", err)
		}
	}

	// The production orchestrator, with the REAL host executor as both the shell runner and the
	// background runner — which is exactly what main.go wires on a native posture.
	exec := host.NewExecutor(0)
	f.orch = NewOrchestrator(repo, nil, modelbroker.New(modelbroker.Config{}), toolbroker.New(tools.ShellTool(), tools.FileTool()))
	// The SYNCHRONOUS executor is no longer set here (A.3): it reaches an attempt through that attempt's
	// own channel, so it is handed to the scripted dialer below. The BACKGROUND one is still
	// process-wide, which is the asymmetry orchestrator.go names.
	f.exec = exec
	// SetBackgroundRunner also wires the cancellation killer onto the orchestrator's OWN spine
	// (repo.Spine()), which in production is the same *coordinator.Store startDispatch cancels through.
	// This fixture holds a second handle to the same database for its seeding, so a cancellation proof
	// must go through f.orch.spine rather than through f.spine — otherwise it would be measuring a store
	// production never builds.
	f.orch.SetBackgroundRunner(exec)
	return f
}

// scriptedChannel replays a fixed frame script to the run loop and records what the controller sent
// back. It is the engine's half of one attempt.
type scriptedChannel struct {
	frames []contracts.EngineFrame
	next   int
	sent   []contracts.EngineFrame
}

func (c *scriptedChannel) Send(_ context.Context, f contracts.EngineFrame) error {
	c.sent = append(c.sent, f)
	return nil
}

func (c *scriptedChannel) Receive(ctx context.Context) (contracts.EngineFrame, error) {
	if c.next >= len(c.frames) {
		// The script ran out. Block until the attempt's context ends rather than returning EOF, which
		// the run loop classifies as "the engine closed before a terminal" — an ERROR, and an error is
		// not what any claim here is about.
		<-ctx.Done()
		return contracts.EngineFrame{}, ctx.Err()
	}
	f := c.frames[c.next]
	c.next++
	return f, nil
}

func (c *scriptedChannel) Close() error { return nil }

// scriptedDialer hands the run loop the scripted engine. It replaces the gateway, and nothing else in
// ExecuteAttempt is replaced.
//
// Since A.3 it also carries the attempt's MACHINE, because that is where an executor comes from now:
// the channel it hands back answers exec requests on `exec`, the same way a gatewayChannel answers them
// on the runner that took the lease. A dialer with no exec yields an attempt with no shell — which is
// what the tests about a machineless attempt want, so it stays expressible.
type scriptedDialer struct {
	ch   *scriptedChannel
	exec toolbroker.ShellRunner
}

func (d *scriptedDialer) Dial(context.Context, AttemptDescriptor) (EngineChannel, error) {
	if d.exec == nil {
		return d.ch, nil
	}
	return hostMachineChannel{EngineChannel: d.ch, exec: d.exec}, nil
}

func engineFrame(seq int, typ string, data map[string]any) contracts.EngineFrame {
	return contracts.EngineFrame{
		Protocol: engineProtocol, ID: newFrameID(), Type: typ, Sequence: seq,
		Time: time.Now().UTC().Format(time.RFC3339), Data: data,
	}
}

// runAttempt drives ONE whole attempt whose engine spawns `background` tasks and then reports the given
// terminal outcome. It returns ExecuteAttempt's own error, which is half of every claim below: a park
// that returned an error would be a FAILED attempt, and the dispatch worker would retry it.
func (f *parkFixture) runAttempt(t *testing.T, outcome string, background int) error {
	t.Helper()
	frames := []contracts.EngineFrame{engineFrame(1, "engine.ready", map[string]any{
		"selected_protocol": engineProtocol, "engine": map[string]any{"version": "test"},
	})}
	seq := 2
	for i := 0; i < background; i++ {
		pidFile := filepath.Join(f.root, fmt.Sprintf("task-%d.pgid", i))
		frames = append(frames, engineFrame(seq, "tool.request", map[string]any{
			"tool_call_id": redeliveryID("tc"), "name": "palai.workspace.shell",
			"arguments": map[string]any{"argv": []any{parkScript(pidFile)}, "shell": true, "background": true},
		}))
		seq++
		t.Cleanup(func() {
			if pgid, err := readPgid(pidFile); err == nil {
				_ = syscall.Kill(-pgid, syscall.SIGKILL)
			}
		})
	}
	frames = append(frames, engineFrame(seq, "run.terminal", map[string]any{"outcome": outcome}))

	f.orch.dialer = &scriptedDialer{ch: &scriptedChannel{frames: frames}, exec: f.exec}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	err := f.orch.ExecuteAttempt(ctx, AttemptDescriptor{
		RunID: contracts.RunID(f.runID), AttemptID: contracts.AttemptID(redeliveryID("att")),
		Fence: 1, WorkspaceHostPath: f.root,
	})
	// Every spawned task must actually be running by the time the terminal frame was handled, or the
	// park this file measures would be a park on nothing.
	for i := 0; i < background; i++ {
		pidFile := filepath.Join(f.root, fmt.Sprintf("task-%d.pgid", i))
		if pgid, perr := waitPgid(pidFile); perr != nil {
			t.Fatalf("background task %d never wrote its process-group id: %v", i, perr)
		} else if syscall.Kill(-pgid, 0) != nil {
			t.Fatalf("background task %d was not running when the attempt ended; the park would be a park on nothing", i)
		}
	}
	return err
}

func readPgid(path string) (int, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return 0, err
	}
	return strconv.Atoi(strings.TrimSpace(string(raw)))
}

func waitPgid(path string) (int, error) {
	deadline := time.Now().Add(10 * time.Second)
	var err error
	for time.Now().Before(deadline) {
		var pgid int
		if pgid, err = readPgid(path); err == nil && pgid > 0 {
			return pgid, nil
		}
		time.Sleep(20 * time.Millisecond)
	}
	return 0, err
}

func (f *parkFixture) runState(t *testing.T) string {
	t.Helper()
	var state string
	if err := f.spine.Pool().QueryRow(storage.WithSystemScope(context.Background()),
		`SELECT state FROM runs WHERE id = $1`, f.runID).Scan(&state); err != nil {
		t.Fatalf("read run state: %v", err)
	}
	return state
}

func (f *parkFixture) responseState(t *testing.T) string {
	t.Helper()
	var state string
	if err := f.spine.Pool().QueryRow(storage.WithSystemScope(context.Background()),
		`SELECT state FROM responses WHERE id = $1`, f.responseID).Scan(&state); err != nil {
		t.Fatalf("read response state: %v", err)
	}
	return state
}

// sessionEventTypes reads the run's own journal in sequence order — the journal the SSE endpoint tails.
func (f *parkFixture) sessionEventTypes(t *testing.T) []string {
	t.Helper()
	rows, err := f.spine.Pool().Query(storage.WithSystemScope(context.Background()),
		`SELECT type FROM events WHERE session_id = $1 ORDER BY seq`, f.sessionID)
	if err != nil {
		t.Fatalf("read the session journal: %v", err)
	}
	defer rows.Close()
	var types []string
	for rows.Next() {
		var typ string
		if err := rows.Scan(&typ); err != nil {
			t.Fatalf("scan event type: %v", err)
		}
		types = append(types, typ)
	}
	return types
}

// ---------------------------------------------------------------------------------------------------
// RED 1 — a run with a live background task does NOT complete.
// ---------------------------------------------------------------------------------------------------

// TestARunWithALiveBackgroundTaskParksInsteadOfCompleting is T3's first RED. The model said it was
// done; the operating system says a process it started is still running. The run must reach `waiting`,
// not `completed`, and ExecuteAttempt must return NIL — the nil is asserted as hard as the state,
// because a park that returned an error would be a failed attempt and the retry ladder, not the exit
// notification, would be what happened next.
//
// It was RED for the reason the whole epic exists: `finalize` applied RunCmdComplete unconditionally,
// so the run terminated while its build was still compiling and the process became an orphan nothing
// would ever fold back in.
func TestARunWithALiveBackgroundTaskParksInsteadOfCompleting(t *testing.T) {
	f := newParkFixture(t)

	if err := f.runAttempt(t, "completed", 1); err != nil {
		t.Fatalf("ExecuteAttempt with a live background task returned %v, want nil — a park releases the dispatch worker, an error retries it", err)
	}
	if got := f.runState(t); got != "waiting" {
		t.Fatalf("run state = %q after run.terminal{completed} with a live background task, want waiting", got)
	}
	// AND NOTHING WAS FINALIZED. The terminal projection is the other half: a run that parked has no
	// output document, because it has not finished.
	var output *string
	if err := f.spine.Pool().QueryRow(storage.WithSystemScope(context.Background()),
		`SELECT output::text FROM responses WHERE id = $1`, f.responseID).Scan(&output); err != nil {
		t.Fatalf("read the response projection: %v", err)
	}
	if output != nil {
		t.Fatalf("a parked run wrote a terminal projection %q; it has not finished", *output)
	}
}

// TestARunWithNoLiveBackgroundTaskStillCompletes is the NON-VACUITY half of the test above, and
// without it the park proves nothing: a gate that parked every run would satisfy RED 1 exactly as well
// as the right one does. The same fixture, the same frames, the same terminal — with no background
// task the run completes and its projection is written.
func TestARunWithNoLiveBackgroundTaskStillCompletes(t *testing.T) {
	f := newParkFixture(t)

	if err := f.runAttempt(t, "completed", 0); err != nil {
		t.Fatalf("ExecuteAttempt: %v", err)
	}
	if got := f.runState(t); got != "completed" {
		t.Fatalf("run state = %q with no background task, want completed — the park must not fire on a run that has nothing running", got)
	}
	if got := f.responseState(t); got != "completed" {
		t.Fatalf("response state = %q, want completed", got)
	}
}

// TestAFailedTerminalDoesNotParkOnALiveBackgroundTask pins the gate to `completed` alone, which is a
// decision rather than an omission. A run that FAILED has no next turn for an exit notification to fold
// into: parking it would leave it waiting for a wake that would do nothing with it, so it terminates
// and the run's cancellation is what deals with the process (T5).
func TestAFailedTerminalDoesNotParkOnALiveBackgroundTask(t *testing.T) {
	f := newParkFixture(t)

	if err := f.runAttempt(t, "failed", 1); err != nil {
		t.Fatalf("ExecuteAttempt: %v", err)
	}
	if got := f.runState(t); got != "failed" {
		t.Fatalf("run state = %q on run.terminal{failed} with a live background task, want failed — only a COMPLETED run parks", got)
	}
}

// unprovableBackground starts real processes through the real executor but answers every probe the way a
// handle nobody can vouch for is answered. It is how the two non-running readings are reached
// deterministically: `lost` (a pgid whose recorded start time does not match the live process — T1's
// PID-reuse guard) and a probe that cannot answer at all.
type unprovableBackground struct {
	inner toolbroker.BackgroundRunner
	state toolbroker.BackgroundState
	err   error
}

func (u *unprovableBackground) Start(ctx context.Context, cmd toolbroker.ShellCommand, spec toolbroker.BackgroundSpec) (toolbroker.Handle, error) {
	return u.inner.Start(ctx, cmd, spec)
}

func (u *unprovableBackground) Probe(context.Context, toolbroker.Handle) (toolbroker.BackgroundStatus, error) {
	if u.err != nil {
		return toolbroker.BackgroundStatus{}, u.err
	}
	return toolbroker.BackgroundStatus{State: u.state}, nil
}

func (u *unprovableBackground) Kill(ctx context.Context, h toolbroker.Handle) error {
	return u.inner.Kill(ctx, h)
}

// TestALostOrUnprobeableBackgroundTaskDoesNotParkTheRun pins the two readings that are NOT `running`, and
// it is a decision rather than an accident of the condition's shape.
//
// A `lost` handle is one that may name a live process and may name a stranger's (PID reuse). T1's rule for
// it is absolute — it is never signalled. Parking on one would be that same mistake pointed the other way:
// the run would wait for the exit of a process nothing is entitled to watch, and only T5's reaper would
// ever free it. A probe that ERRORS is the same trade with the arithmetic written out — parking on an
// unreadable probe strands a run, not parking loses one exit notification for a run that has finished, and
// the second is what an operator can recover from by reading the log file the task wrote.
func TestALostOrUnprobeableBackgroundTaskDoesNotParkTheRun(t *testing.T) {
	for _, tc := range []struct {
		name  string
		state toolbroker.BackgroundState
		err   error
	}{
		{"lost", toolbroker.BackgroundLost, nil},
		{"unprobeable", "", errors.New("the container daemon is unreachable")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newParkFixture(t)
			f.orch.SetBackgroundRunner(&unprovableBackground{inner: host.NewExecutor(0), state: tc.state, err: tc.err})

			if err := f.runAttempt(t, "completed", 1); err != nil {
				t.Fatalf("ExecuteAttempt: %v", err)
			}
			if got := f.runState(t); got != "completed" {
				t.Fatalf("run state = %q with a %s background handle, want completed — a run must never park on a handle nothing can watch, because nothing would ever wake it", got, tc.name)
			}
		})
	}
}

// ---------------------------------------------------------------------------------------------------
// RED 2 (§3.6 D3) — the response says waiting_for_tool, and this is ResponseTable's first production use.
// ---------------------------------------------------------------------------------------------------

// TestAParkedRunsResponseReadsWaitingForTool is D3 made true. The published Response schema has
// advertised `waiting_for_tool` for three years (protocols/schemas/execution/response.json) and the
// conformance contract asserts the enum — and nothing had ever written one.
//
// THE MEASURED SHAPE OF THE RED IS NOT THE ONE THE PLAN PREDICTED, and the difference is the point. The
// plan said a live response reads `in_progress`. It does not: `InsertResponse` writes 'queued' and the
// ONLY other writer was the terminal UpdateResponse, so a response was `queued` for the entire life of
// its run. `waiting_for_tool` is unreachable from `queued` in ResponseTable, so making D3 true required
// wiring the response's ENTRY transitions too — which is why this test asserts in_progress BEFORE the
// park as hard as it asserts waiting_for_tool during it.
func TestAParkedRunsResponseReadsWaitingForTool(t *testing.T) {
	f := newParkFixture(t)

	if err := f.runAttempt(t, "completed", 1); err != nil {
		t.Fatalf("ExecuteAttempt: %v", err)
	}
	if got := f.responseState(t); got != "waiting_for_tool" {
		t.Fatalf("response state = %q during the park, want waiting_for_tool (published schema §8.3, protocols/schemas/execution/response.json)", got)
	}
}

// TestALiveResponseReachesInProgressBeforeItParks is the entry half, separately named because it is a
// separate claim and because a proof this tier's -run does not name is a proof that never runs. A
// response whose run is executing must read `in_progress` — the state the published schema has
// declared since the beginning and that no line of production code had ever written.
func TestALiveResponseReachesInProgressBeforeItParks(t *testing.T) {
	f := newParkFixture(t)

	// One attempt with NO terminal frame: the engine goes quiet after the spawn, so the attempt is
	// still live when the context ends and the response is observed mid-run.
	f.orch.dialer = &scriptedDialer{ch: &scriptedChannel{frames: []contracts.EngineFrame{
		engineFrame(1, "engine.ready", map[string]any{"selected_protocol": engineProtocol}),
	}}, exec: f.exec}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = f.orch.ExecuteAttempt(ctx, AttemptDescriptor{
		RunID: contracts.RunID(f.runID), AttemptID: contracts.AttemptID(redeliveryID("att")),
		Fence: 1, WorkspaceHostPath: f.root,
	})
	if got := f.responseState(t); got != "in_progress" {
		t.Fatalf("a live response reads %q, want in_progress — the published schema has declared it since §8.3 was written", got)
	}
}

// ---------------------------------------------------------------------------------------------------
// RED 3 — MONOTONICITY: waiting_for_tool never lands on a terminal response.
// ---------------------------------------------------------------------------------------------------

// TestWaitingForToolNeverOverwritesATerminalResponse is the same discipline FinalizeResponse's
// conditional UPDATE carries (storage/queries/responses.sql): the first terminal write wins, and a
// later non-terminal write must affect zero rows.
//
// The race is real rather than theoretical — it is the 2-tx cancel window the UpdateResponse comment
// names. A cancel finalizes the response while this attempt is in flight; the attempt then reaches its
// terminal frame with a background task still running and tries to park. The park must not resurrect a
// canceled response into a waiting one, which a caller would read as a run still going.
func TestWaitingForToolNeverOverwritesATerminalResponse(t *testing.T) {
	f := newParkFixture(t)

	// The cancel wins the race: the response is terminal before the attempt reaches its own terminal.
	if err := f.spine.FinalizeResponse(context.Background(), f.tenant, f.responseID, "canceled",
		[]byte(`{"output":[],"usage":{}}`)); err != nil {
		t.Fatalf("finalize the response canceled: %v", err)
	}
	if err := f.runAttempt(t, "completed", 1); err != nil {
		t.Fatalf("ExecuteAttempt: %v", err)
	}
	if got := f.responseState(t); got != "canceled" {
		t.Fatalf("response state = %q after a park raced a terminal write, want canceled — a terminal response is never reopened", got)
	}
}

// ---------------------------------------------------------------------------------------------------
// RED 4 — the SDK and the console do not break: the park emits no terminal the stream would close on.
// ---------------------------------------------------------------------------------------------------

// TestTheParkWritesNoRunTerminalIntoTheSessionJournal is the durable half of RED 4. The SSE endpoint
// closes on a RUN terminal event and on nothing else (apps/control-plane/api/events.go), so what keeps a
// consumer attached across the park is the absence of one — asserted here against the real journal the
// endpoint tails. The endpoint's own half is
// TestTheEventStreamStaysOpenAcrossAParkAndClosesOnATerminal, beside the handler.
func TestTheParkWritesNoRunTerminalIntoTheSessionJournal(t *testing.T) {
	f := newParkFixture(t)

	if err := f.runAttempt(t, "completed", 1); err != nil {
		t.Fatalf("ExecuteAttempt: %v", err)
	}
	types := f.sessionEventTypes(t)
	var sawWaiting bool
	for _, typ := range types {
		switch typ {
		case "run.waiting.v1":
			sawWaiting = true
		case "run.completed.v1", "run.failed.v1", "run.canceled.v1", "run.timed_out.v1", "run.budget_exceeded.v1":
			t.Fatalf("the park emitted %q into the session journal; the SSE endpoint closes on it and every attached SDK and console would see the run end: %v", typ, types)
		}
	}
	if !sawWaiting {
		t.Fatalf("the park emitted no run.waiting.v1 into the session journal: %v", types)
	}
}
