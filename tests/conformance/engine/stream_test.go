package engine_test

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/palgroup/palai/adapters/sandboxes/oci"
	"github.com/palgroup/palai/packages/contracts"
	"github.com/palgroup/palai/packages/runner"
)

// fakeProcess is an in-memory oci.Process backed by three synchronous pipes. The test
// plays the engine (writing stdout/stderr frames) and the container lifecycle (Wait,
// Kill); the supervisor writes its stdin (hello + injected controller frames), which
// the test reads back to prove what was injected. It lets the streaming supervisor be
// proven Docker-free — the real Docker interactive driver is proven in the fault tier.
type fakeProcess struct {
	stdinR   *io.PipeReader // test reads to observe what the supervisor injected
	stdinW   *io.PipeWriter // Stdin(): the supervisor writes here
	stdoutR  *io.PipeReader // Stdout(): the supervisor reads here
	stdoutW  *io.PipeWriter // test writes engine frames here
	stderrR  *io.PipeReader // Stderr(): the supervisor reads here
	stderrW  *io.PipeWriter // test writes engine stderr here
	exitCode int64
	timedOut bool
	killed   chan struct{}
}

func newFakeProcess() *fakeProcess {
	stdinR, stdinW := io.Pipe()
	stdoutR, stdoutW := io.Pipe()
	stderrR, stderrW := io.Pipe()
	return &fakeProcess{
		stdinR: stdinR, stdinW: stdinW,
		stdoutR: stdoutR, stdoutW: stdoutW,
		stderrR: stderrR, stderrW: stderrW,
		killed: make(chan struct{}),
	}
}

func (p *fakeProcess) Stdin() io.WriteCloser { return p.stdinW }
func (p *fakeProcess) Stdout() io.Reader     { return p.stdoutR }
func (p *fakeProcess) Stderr() io.Reader     { return p.stderrR }

func (p *fakeProcess) Wait(context.Context) (oci.Outcome, error) {
	return oci.Outcome{ExitCode: p.exitCode, TimedOut: p.timedOut}, nil
}

func (p *fakeProcess) Kill(context.Context) error {
	select {
	case <-p.killed:
	default:
		close(p.killed)
	}
	_ = p.stdoutW.Close()
	_ = p.stderrW.Close()
	return nil
}

// exit models the engine process exiting cleanly: a real container closes both output
// streams at once, so a test that only closed stdout would leave the supervisor draining
// stderr until its safety ceiling.
func (p *fakeProcess) exit() {
	_ = p.stdoutW.Close()
	_ = p.stderrW.Close()
}

// fakeDriver hands the same fake process to the supervisor on Start.
type fakeDriver struct{ process *fakeProcess }

func (d fakeDriver) Start(context.Context, oci.ContainerSpec) (oci.Process, error) {
	return d.process, nil
}

// streamRequest is a valid streaming request with generous bounds; individual tests
// tighten a single bound to exercise it.
func streamRequest() runner.EngineRequest {
	return runner.EngineRequest{
		ImageDigest: "sha256:" + strings.Repeat("a", 64),
		RunID:       "run_streamfixture",
		AttemptID:   "att_streamfixture",
		Limits: runner.Limits{
			WallTimeMS:      5000,
			MaxStdoutBytes:  1 << 20,
			MaxStderrBytes:  16 * 1024,
			MaxFrameBytes:   64 * 1024,
			MaxMemoryBytes:  64 * 1024 * 1024,
			MaxProcessCount: 16,
		},
	}
}

// engineFrame builds an engine.v1 stdout frame for the fake engine to emit.
func engineFrame(typ string, seq int, id string, data map[string]any) contracts.EngineFrame {
	return contracts.EngineFrame{
		Protocol:  "engine.v1",
		ID:        contracts.FrameID(id),
		Type:      typ,
		Sequence:  seq,
		Time:      "2026-07-16T12:00:00Z",
		RunID:     "run_streamfixture",
		AttemptID: "att_streamfixture",
		Data:      data,
	}
}

func readyData() map[string]any {
	return map[string]any{
		"selected_protocol": "engine.v1",
		"engine":            map[string]any{"name": "fake", "version": "0"},
		"max_frame_bytes":   1024,
		"nonce":             "n",
	}
}

// writeEngineFrame emits one JSONL frame on the fake engine's stdout.
func writeEngineFrame(t *testing.T, w io.Writer, frame contracts.EngineFrame) {
	t.Helper()
	line, err := json.Marshal(frame)
	if err != nil {
		t.Fatalf("marshal engine frame: %v", err)
	}
	if _, err := w.Write(append(line, '\n')); err != nil {
		t.Fatalf("write engine frame: %v", err)
	}
}

// readInjectedFrame reads one JSONL frame the supervisor injected into stdin.
func readInjectedFrame(t *testing.T, sc *bufio.Scanner) contracts.EngineFrame {
	t.Helper()
	if !sc.Scan() {
		t.Fatalf("no injected stdin frame: %v", sc.Err())
	}
	var frame contracts.EngineFrame
	if err := json.Unmarshal(sc.Bytes(), &frame); err != nil {
		t.Fatalf("decode injected frame %q: %v", sc.Text(), err)
	}
	return frame
}

// collectingSink records every forwarded engine frame.
type collectingSink struct{ frames []contracts.EngineFrame }

func (s *collectingSink) sink(_ context.Context, frame contracts.EngineFrame) error {
	s.frames = append(s.frames, frame)
	return nil
}

// fakeBatchDriver is a batch oci.Driver that returns a preset stdout — the post-hoc
// parseEngineFrames path, Docker-free.
type fakeBatchDriver struct{ stdout []byte }

func (d fakeBatchDriver) Run(context.Context, oci.ContainerSpec) (oci.Outcome, error) {
	return oci.Outcome{Stdout: d.stdout, StdoutBytes: int64(len(d.stdout)), ExitCode: 0}, nil
}
func (d fakeBatchDriver) Close() error { return nil }

// TestStreamWritesFenceHashInHello proves the §25.6 supervisor.hello carries the lease
// fencing token as a hash — sha256("<run_id>/<fence>") — before any run input, so the
// engine can bind the handshake to the lease that authorized it (E10 recovery compares
// it; here it is transported).
func TestStreamWritesFenceHashInHello(t *testing.T) {
	proc := newFakeProcess()
	sup := runner.NewStreamSupervisor(fakeDriver{proc})
	sink := &collectingSink{}
	req := streamRequest()
	req.Fence = 9

	done := make(chan error, 1)
	go func() {
		_, err := sup.Stream(context.Background(), req, nil, sink.sink)
		done <- err
	}()

	stdin := bufio.NewScanner(proc.stdinR)
	hello := readInjectedFrame(t, stdin)
	if hello.Type != "supervisor.hello" {
		t.Fatalf("first stdin frame type = %q, want supervisor.hello", hello.Type)
	}
	got, _ := hello.Data["fence_hash"].(string)
	want := fmt.Sprintf("%x", sha256.Sum256([]byte(string(req.RunID)+"/"+strconv.FormatUint(req.Fence, 10))))
	if got == "" {
		t.Fatal("supervisor.hello carried no fence_hash")
	}
	if got != want {
		t.Fatalf("hello fence_hash = %q, want %q", got, want)
	}

	writeEngineFrame(t, proc.stdoutW, engineFrame("engine.ready", 1, "frm_ready", readyData()))
	writeEngineFrame(t, proc.stdoutW, engineFrame("run.terminal", 2, "frm_term", map[string]any{"outcome": "completed"}))
	proc.exit()
	if err := <-done; err != nil {
		t.Fatalf("Stream() error = %v, want nil", err)
	}
}

// TestBatchRunRejectsChangedHashDuplicateFrame proves the batch supervisor closes the
// LP8 Minor-2 gap: a frame id reused with a different payload in the parsed stdout is a
// protocol violation (ENG-002), the same stable request-id discipline the streaming path
// enforces — so the secondary batch path cannot silently accept what streaming rejects.
func TestBatchRunRejectsChangedHashDuplicateFrame(t *testing.T) {
	first := engineFrame("output.item", 1, "frm_batchdup", map[string]any{"v": 1})
	second := engineFrame("output.item", 2, "frm_batchdup", map[string]any{"v": 2}) // same id, different payload
	var stdout []byte
	for _, frame := range []contracts.EngineFrame{first, second} {
		line, err := json.Marshal(frame)
		if err != nil {
			t.Fatalf("marshal frame: %v", err)
		}
		stdout = append(stdout, append(line, '\n')...)
	}

	sup := runner.NewSupervisor(fakeBatchDriver{stdout: stdout})
	if _, err := sup.Run(context.Background(), streamRequest()); !errors.Is(err, runner.ErrFrameHashConflict) {
		t.Fatalf("batch Run() error = %v, want ErrFrameHashConflict", err)
	}
}

// TestStreamWritesHelloBeforeAnyRunInput proves the streaming supervisor opens with the
// §25.6 supervisor.hello before it reads any engine output — the handshake the batch
// supervisor never performed.
func TestStreamWritesHelloBeforeAnyRunInput(t *testing.T) {
	proc := newFakeProcess()
	sup := runner.NewStreamSupervisor(fakeDriver{proc})
	sink := &collectingSink{}

	done := make(chan error, 1)
	go func() {
		_, err := sup.Stream(context.Background(), streamRequest(), nil, sink.sink)
		done <- err
	}()

	stdin := bufio.NewScanner(proc.stdinR)
	hello := readInjectedFrame(t, stdin)
	if hello.Type != "supervisor.hello" {
		t.Fatalf("first stdin frame type = %q, want supervisor.hello", hello.Type)
	}

	// Complete the run so Stream returns cleanly.
	writeEngineFrame(t, proc.stdoutW, engineFrame("engine.ready", 1, "frm_ready", readyData()))
	writeEngineFrame(t, proc.stdoutW, engineFrame("run.terminal", 2, "frm_term", map[string]any{"outcome": "completed"}))
	proc.exit()

	if err := <-done; err != nil {
		t.Fatalf("Stream() error = %v, want nil", err)
	}
}

// TestStreamEnforcesFrameBoundMidStream proves a stdout frame exceeding MaxFrameBytes
// fails the attempt while it streams — not after buffering the whole output — and the
// oversized frame is never forwarded.
func TestStreamEnforcesFrameBoundMidStream(t *testing.T) {
	proc := newFakeProcess()
	sup := runner.NewStreamSupervisor(fakeDriver{proc})
	sink := &collectingSink{}
	req := streamRequest()
	req.Limits.MaxFrameBytes = 2048

	done := make(chan error, 1)
	go func() {
		_, err := sup.Stream(context.Background(), req, nil, sink.sink)
		done <- err
	}()

	stdin := bufio.NewScanner(proc.stdinR)
	_ = readInjectedFrame(t, stdin) // hello

	writeEngineFrame(t, proc.stdoutW, engineFrame("engine.ready", 1, "frm_ready", readyData()))
	oversized := engineFrame("progress", 2, "frm_big", map[string]any{"padding": strings.Repeat("x", 8192)})
	writeEngineFrame(t, proc.stdoutW, oversized)
	_ = proc.stdoutW.Close()

	err := <-done
	if err == nil {
		t.Fatal("Stream() accepted an oversized mid-stream frame")
	}
	for _, f := range sink.frames {
		if f.ID == "frm_big" {
			t.Fatal("oversized frame was forwarded to the sink")
		}
	}
}

// TestStreamRejectsChangedHashDuplicateFrame proves a frame id re-used with a different
// payload is a protocol violation (ENG-002) that ends the attempt.
func TestStreamRejectsChangedHashDuplicateFrame(t *testing.T) {
	proc := newFakeProcess()
	sup := runner.NewStreamSupervisor(fakeDriver{proc})
	sink := &collectingSink{}

	done := make(chan error, 1)
	go func() {
		_, err := sup.Stream(context.Background(), streamRequest(), nil, sink.sink)
		done <- err
	}()

	stdin := bufio.NewScanner(proc.stdinR)
	_ = readInjectedFrame(t, stdin) // hello

	writeEngineFrame(t, proc.stdoutW, engineFrame("engine.ready", 1, "frm_ready", readyData()))
	writeEngineFrame(t, proc.stdoutW, engineFrame("progress", 2, "frm_dup", map[string]any{"v": 1}))
	writeEngineFrame(t, proc.stdoutW, engineFrame("progress", 3, "frm_dup", map[string]any{"v": 2}))
	_ = proc.stdoutW.Close()

	err := <-done
	if !errors.Is(err, runner.ErrFrameHashConflict) {
		t.Fatalf("Stream() error = %v, want ErrFrameHashConflict", err)
	}
}

// TestStreamDropsIdenticalRetransmitWithoutForwarding proves an identical retransmit
// (same id, same payload) is dropped from the forwarding loop, not double-forwarded.
func TestStreamDropsIdenticalRetransmitWithoutForwarding(t *testing.T) {
	proc := newFakeProcess()
	sup := runner.NewStreamSupervisor(fakeDriver{proc})
	sink := &collectingSink{}

	done := make(chan error, 1)
	go func() {
		_, err := sup.Stream(context.Background(), streamRequest(), nil, sink.sink)
		done <- err
	}()

	stdin := bufio.NewScanner(proc.stdinR)
	_ = readInjectedFrame(t, stdin) // hello

	writeEngineFrame(t, proc.stdoutW, engineFrame("engine.ready", 1, "frm_ready", readyData()))
	// A retransmit rides the same ordered stream, so it keeps its sequence; the ledger
	// drops it before the monotonicity gate sees it.
	writeEngineFrame(t, proc.stdoutW, engineFrame("progress", 2, "frm_once", map[string]any{"v": 1}))
	writeEngineFrame(t, proc.stdoutW, engineFrame("progress", 2, "frm_once", map[string]any{"v": 1}))
	writeEngineFrame(t, proc.stdoutW, engineFrame("run.terminal", 3, "frm_term", map[string]any{"outcome": "completed"}))
	proc.exit()

	if err := <-done; err != nil {
		t.Fatalf("Stream() error = %v, want nil", err)
	}
	seen := 0
	for _, f := range sink.frames {
		if f.ID == "frm_once" {
			seen++
		}
	}
	if seen != 1 {
		t.Fatalf("frame frm_once forwarded %d times, want exactly 1", seen)
	}
}

// TestStreamRedactsStderrBeforeForwarding proves a secret-shaped token written to the
// engine's stderr is masked before the runner surfaces the stderr — the supervisor does
// not trust the engine to redact its own logs.
func TestStreamRedactsStderrBeforeForwarding(t *testing.T) {
	const sentinel = "sk-live-STREAMREDACTSENTINEL0123456789"
	proc := newFakeProcess()
	sup := runner.NewStreamSupervisor(fakeDriver{proc})
	sink := &collectingSink{}

	done := make(chan struct {
		result runner.EngineResult
		err    error
	}, 1)
	go func() {
		result, err := sup.Stream(context.Background(), streamRequest(), nil, sink.sink)
		done <- struct {
			result runner.EngineResult
			err    error
		}{result, err}
	}()

	stdin := bufio.NewScanner(proc.stdinR)
	_ = readInjectedFrame(t, stdin) // hello

	if _, err := proc.stderrW.Write([]byte("provider call failed token=" + sentinel + "\n")); err != nil {
		t.Fatalf("write engine stderr: %v", err)
	}
	writeEngineFrame(t, proc.stdoutW, engineFrame("engine.ready", 1, "frm_ready", readyData()))
	writeEngineFrame(t, proc.stdoutW, engineFrame("run.terminal", 2, "frm_term", map[string]any{"outcome": "completed"}))
	_ = proc.stderrW.Close()
	_ = proc.stdoutW.Close()

	got := <-done
	if got.err != nil {
		t.Fatalf("Stream() error = %v, want nil", got.err)
	}
	if strings.Contains(string(got.result.Stderr), sentinel) {
		t.Fatalf("forwarded stderr still contains the secret: %q", got.result.Stderr)
	}
	if !strings.Contains(string(got.result.Stderr), "***") {
		t.Fatalf("forwarded stderr was not masked: %q", got.result.Stderr)
	}
}

// TestStreamRedactsSecretSplitAcrossChunkBoundary proves a secret written to stderr in
// two separate writes — split across a read/chunk boundary — is still masked before the
// runner surfaces (and digests) the stderr. The supervisor drains the whole bounded
// stderr and redacts the assembled buffer, so a secret straddling two writes is reunited
// before the redactor runs and never leaks in the persisted/forwarded stderr.
func TestStreamRedactsSecretSplitAcrossChunkBoundary(t *testing.T) {
	const sentinel = "sk-live-SPLITBOUNDARYSENTINEL0123456789"
	proc := newFakeProcess()
	sup := runner.NewStreamSupervisor(fakeDriver{proc})
	sink := &collectingSink{}

	done := make(chan struct {
		result runner.EngineResult
		err    error
	}, 1)
	go func() {
		result, err := sup.Stream(context.Background(), streamRequest(), nil, sink.sink)
		done <- struct {
			result runner.EngineResult
			err    error
		}{result, err}
	}()

	stdin := bufio.NewScanner(proc.stdinR)
	_ = readInjectedFrame(t, stdin) // hello

	// Split the sentinel across two writes: "sk-live-SPLIT..." lands in one chunk, the
	// remainder in the next, so a naive per-chunk redactor would miss it.
	head, tail := sentinel[:6], sentinel[6:]
	if _, err := proc.stderrW.Write([]byte("provider call failed token=" + head)); err != nil {
		t.Fatalf("write stderr head: %v", err)
	}
	if _, err := proc.stderrW.Write([]byte(tail + "\n")); err != nil {
		t.Fatalf("write stderr tail: %v", err)
	}
	writeEngineFrame(t, proc.stdoutW, engineFrame("engine.ready", 1, "frm_ready", readyData()))
	writeEngineFrame(t, proc.stdoutW, engineFrame("run.terminal", 2, "frm_term", map[string]any{"outcome": "completed"}))
	_ = proc.stderrW.Close()
	_ = proc.stdoutW.Close()

	got := <-done
	if got.err != nil {
		t.Fatalf("Stream() error = %v, want nil", got.err)
	}
	if strings.Contains(string(got.result.Stderr), sentinel) {
		t.Fatalf("forwarded stderr still contains the split secret: %q", got.result.Stderr)
	}
	if strings.Contains(string(got.result.Stderr), "sk-live") {
		t.Fatalf("forwarded stderr leaked a secret fragment: %q", got.result.Stderr)
	}
}

// TestStreamInjectsControllerFramesToStdin proves an inbound controller frame
// (model.result) is flushed to the engine's stdin — the mid-run injection path the
// batch supervisor lacked entirely.
func TestStreamInjectsControllerFramesToStdin(t *testing.T) {
	proc := newFakeProcess()
	sup := runner.NewStreamSupervisor(fakeDriver{proc})
	sink := &collectingSink{}
	inbound := make(chan contracts.EngineFrame, 1)

	done := make(chan error, 1)
	go func() {
		_, err := sup.Stream(context.Background(), streamRequest(), inbound, sink.sink)
		done <- err
	}()

	stdin := bufio.NewScanner(proc.stdinR)
	hello := readInjectedFrame(t, stdin)
	if hello.Type != "supervisor.hello" {
		t.Fatalf("first stdin frame type = %q, want supervisor.hello", hello.Type)
	}

	writeEngineFrame(t, proc.stdoutW, engineFrame("engine.ready", 1, "frm_ready", readyData()))

	result := engineFrame("model.result", 1, "frm_result", map[string]any{"model_request_id": "mreq_1"})
	inbound <- result
	injected := readInjectedFrame(t, stdin)
	if injected.Type != "model.result" || injected.ID != "frm_result" {
		t.Fatalf("injected stdin frame = %+v, want the model.result frame", injected)
	}

	close(inbound)
	writeEngineFrame(t, proc.stdoutW, engineFrame("run.terminal", 2, "frm_term", map[string]any{"outcome": "completed"}))
	proc.exit()
	if err := <-done; err != nil {
		t.Fatalf("Stream() error = %v, want nil", err)
	}
}

// TestStreamHandshakeDeadlineFailsAsIncompatibleEngine proves an engine that never
// answers the hello within the startup deadline fails the attempt as an incompatible
// engine — nothing downstream proceeds past an incomplete handshake (ENG-001).
func TestStreamHandshakeDeadlineFailsAsIncompatibleEngine(t *testing.T) {
	proc := newFakeProcess()
	sup := runner.NewStreamSupervisor(fakeDriver{proc})
	sup.HandshakeTimeout = 50 * time.Millisecond
	sink := &collectingSink{}

	done := make(chan error, 1)
	go func() {
		_, err := sup.Stream(context.Background(), streamRequest(), nil, sink.sink)
		done <- err
	}()

	stdin := bufio.NewScanner(proc.stdinR)
	_ = readInjectedFrame(t, stdin) // hello, but the engine never answers

	select {
	case err := <-done:
		if !errors.Is(err, runner.ErrIncompatibleEngine) {
			t.Fatalf("Stream() error = %v, want ErrIncompatibleEngine", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Stream() did not fail on the handshake deadline")
	}
	_ = proc.stdoutW.Close()
	_ = proc.stderrW.Close()
}
