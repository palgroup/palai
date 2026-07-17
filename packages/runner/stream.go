package runner

import (
	"bufio"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"strconv"
	"time"

	"github.com/palgroup/palai/adapters/sandboxes/oci"
	"github.com/palgroup/palai/packages/contracts"
)

// defaultHandshakeTimeout bounds the wait for engine.ready when the caller sets no
// explicit StreamSupervisor.HandshakeTimeout.
const defaultHandshakeTimeout = 10 * time.Second

// streamDrainTimeout bounds the post-exit wait for the container outcome and the stderr
// drain, so a wedged daemon cannot hang the supervisor after the engine has exited.
const streamDrainTimeout = 30 * time.Second

const (
	frameSupervisorHello = "supervisor.hello"
	frameEngineReady     = "engine.ready"
)

// ErrIncompatibleEngine reports an engine that did not answer the supervisor.hello with
// engine.ready inside the startup deadline. It is a terminal attempt failure — nothing
// downstream proceeds past an incomplete handshake (ENG-001).
var ErrIncompatibleEngine = errors.New("engine did not complete the handshake within the startup deadline")

// FrameSink receives each validated, de-duplicated engine frame in stream order,
// starting with engine.ready. The runner gateway (Task 11c) relays it to the controller.
type FrameSink func(ctx context.Context, frame contracts.EngineFrame) error

// StreamSupervisor supervises a live, interactive engine attempt: it writes the §25.6
// handshake, injects controller frames into stdin mid-run, reads stdout frames
// incrementally under the same envelope and bound rules the batch supervisor enforces
// post-hoc, and classifies the outcome identically. It is the streaming counterpart of
// Supervisor; both share buildSpec, validateEnvelope, and the FrameLedger.
type StreamSupervisor struct {
	driver oci.InteractiveDriver
	// HandshakeTimeout bounds the wait for engine.ready after the hello is written.
	// Zero uses defaultHandshakeTimeout.
	HandshakeTimeout time.Duration
}

// NewStreamSupervisor returns a streaming supervisor backed by driver.
func NewStreamSupervisor(driver oci.InteractiveDriver) *StreamSupervisor {
	return &StreamSupervisor{driver: driver}
}

// waitOutcome carries the container's terminal outcome from the wall-time-enforcing
// Wait goroutine to the classification step.
type waitOutcome struct {
	outcome oci.Outcome
	err     error
}

// streamRead is one parsed, per-frame-bounded stdout line, or the error that ended the
// stdout stream.
type streamRead struct {
	frame contracts.EngineFrame
	bytes int
	err   error
}

// Stream supervises one interactive engine attempt to a terminal outcome. It starts a
// hardened container, writes supervisor.hello, waits for engine.ready inside the startup
// deadline, forwards every validated stdout frame to sink, injects inbound controller
// frames into stdin, and applies the batch supervisor's outcome classification. A
// handshake timeout, a bound violation, a frame-id conflict, a wall-time kill, or a
// non-zero exit each fails the attempt; a killed engine never yields a false success.
func (s *StreamSupervisor) Stream(ctx context.Context, request EngineRequest, inbound <-chan contracts.EngineFrame, sink FrameSink) (EngineResult, error) {
	if s == nil || s.driver == nil {
		return EngineResult{}, errors.New("stream supervisor is not initialized")
	}
	if err := request.validate(); err != nil {
		return EngineResult{}, err
	}

	process, err := s.driver.Start(ctx, buildSpec(request))
	if err != nil {
		return EngineResult{}, err
	}
	runCtx, cancel := context.WithCancel(ctx)
	defer func() {
		killCtx, cancelKill := context.WithTimeout(context.Background(), streamDrainTimeout)
		defer cancelKill()
		_ = process.Kill(killCtx)
	}()
	defer cancel()

	result := EngineResult{}

	// Drain the bounded stderr for the whole run; redaction is applied post-hoc on the
	// assembled buffer so a secret split across reads is still masked.
	stderr := boundedStderr{limit: request.Limits.MaxStderrBytes}
	stderrDone := make(chan struct{})
	go func() {
		defer close(stderrDone)
		stderr.readFrom(process.Stderr())
	}()

	// Enforce the wall-time bound independently of stdout: a hung engine that never
	// closes stdout is still force-killed by Wait, which then closes the stream.
	waitCh := make(chan waitOutcome, 1)
	go func() {
		outcome, waitErr := process.Wait(runCtx)
		waitCh <- waitOutcome{outcome: outcome, err: waitErr}
	}()

	// The handshake hello is written before any engine output is read (§25.6). It is the
	// last stdin write the main goroutine makes; the injector owns stdin thereafter.
	if err := writeStreamFrame(process.Stdin(), helloFrame(request)); err != nil {
		return result, fmt.Errorf("write supervisor.hello: %w", err)
	}

	frames := make(chan streamRead)
	go readStdout(runCtx, process.Stdout(), request.Limits, frames)

	go injectControllerFrames(runCtx, process, inbound)

	ledger := NewFrameLedger()
	stdoutBytes, lastSeq, err := s.handshake(runCtx, request, frames, ledger, sink)
	if err != nil {
		result.StdoutBytes = stdoutBytes
		return result, err
	}

	// Forwarding loop: validate the envelope, dedupe by frame id, enforce the monotonic
	// sequence on the de-duplicated stream, then forward. A dropped retransmit does not
	// advance the sequence, matching the ordered stdout it rides.
loop:
	for {
		select {
		case <-runCtx.Done():
			result.StdoutBytes = stdoutBytes
			return result, runCtx.Err()
		case read, ok := <-frames:
			if !ok {
				break loop // clean EOF: the engine exited
			}
			if read.err != nil {
				result.StdoutBytes = stdoutBytes
				return result, read.err
			}
			stdoutBytes += int64(read.bytes)
			frame := read.frame
			if err := validateEnvelope(frame, request); err != nil {
				result.StdoutBytes = stdoutBytes
				return result, err
			}
			duplicate, err := ledger.Admit(frame)
			if err != nil {
				result.StdoutBytes = stdoutBytes
				return result, err
			}
			if duplicate {
				continue
			}
			if frame.Sequence != lastSeq+1 {
				result.StdoutBytes = stdoutBytes
				return result, fmt.Errorf("%w: frame %s sequence %d is not %d", ErrInvalidEngineOutput, frame.ID, frame.Sequence, lastSeq+1)
			}
			lastSeq = frame.Sequence
			if err := sink(runCtx, frame); err != nil {
				result.StdoutBytes = stdoutBytes
				return result, err
			}
		}
	}

	return s.classify(request, &result, stdoutBytes, waitCh, stderrDone, &stderr)
}

// handshake reads the first stdout frame inside the startup deadline, requires it to be
// a valid engine.ready, records and forwards it, and returns the running stdout byte
// count and the baseline sequence. Any timeout or non-ready first frame is an
// incompatible-engine failure.
func (s *StreamSupervisor) handshake(ctx context.Context, request EngineRequest, frames <-chan streamRead, ledger *FrameLedger, sink FrameSink) (int64, int, error) {
	timeout := s.HandshakeTimeout
	if timeout <= 0 {
		timeout = defaultHandshakeTimeout
	}
	var ready contracts.EngineFrame
	var stdoutBytes int64
	select {
	case read, ok := <-frames:
		if !ok {
			return 0, 0, fmt.Errorf("%w: engine stdout closed before engine.ready", ErrIncompatibleEngine)
		}
		if read.err != nil {
			return 0, 0, fmt.Errorf("%w: %w", ErrIncompatibleEngine, read.err)
		}
		ready = read.frame
		stdoutBytes = int64(read.bytes)
	case <-time.After(timeout):
		return 0, 0, ErrIncompatibleEngine
	case <-ctx.Done():
		return 0, 0, fmt.Errorf("%w: %w", ErrIncompatibleEngine, ctx.Err())
	}
	if err := validateEnvelope(ready, request); err != nil {
		return stdoutBytes, 0, fmt.Errorf("%w: %w", ErrIncompatibleEngine, err)
	}
	if ready.Type != frameEngineReady {
		return stdoutBytes, 0, fmt.Errorf("%w: first frame type = %q", ErrIncompatibleEngine, ready.Type)
	}
	if _, err := ledger.Admit(ready); err != nil {
		return stdoutBytes, 0, err
	}
	if err := sink(ctx, ready); err != nil {
		return stdoutBytes, 0, err
	}
	return stdoutBytes, ready.Sequence, nil
}

// classify collects the container outcome and the redacted stderr and applies the batch
// supervisor's outcome rules: a wall-time kill is ErrEngineTimeout, a non-zero exit is
// ErrEngineExit, and only a clean exit is a success.
func (s *StreamSupervisor) classify(request EngineRequest, result *EngineResult, stdoutBytes int64, waitCh <-chan waitOutcome, stderrDone <-chan struct{}, stderr *boundedStderr) (EngineResult, error) {
	var wo waitOutcome
	select {
	case wo = <-waitCh:
	case <-time.After(streamDrainTimeout):
		wo = waitOutcome{err: errors.New("engine wait did not complete after stdout closed")}
	}
	select {
	case <-stderrDone:
	case <-time.After(streamDrainTimeout):
	}

	result.ContainerID = wo.outcome.ContainerID
	result.ImageID = wo.outcome.ImageID
	result.ExitCode = wo.outcome.ExitCode
	result.StdoutBytes = stdoutBytes
	result.Stderr = redactStderr(stderr.data)
	result.StderrBytes = stderr.total
	result.StderrTruncated = stderr.truncated

	if wo.err != nil {
		return *result, wo.err
	}
	if wo.outcome.TimedOut {
		return *result, ErrEngineTimeout
	}
	if wo.outcome.ExitCode != 0 {
		return *result, fmt.Errorf("%w: exit code %d", ErrEngineExit, wo.outcome.ExitCode)
	}
	return *result, nil
}

// injectControllerFrames writes each inbound controller frame to the engine's stdin with
// a per-frame flush, and owns the stdin close: it shuts stdin (signalling EOF) when the
// run ends or the caller closes inbound.
func injectControllerFrames(ctx context.Context, process oci.Process, inbound <-chan contracts.EngineFrame) {
	defer func() { _ = process.Stdin().Close() }()
	for {
		select {
		case <-ctx.Done():
			return
		case frame, ok := <-inbound:
			if !ok {
				return
			}
			if err := writeStreamFrame(process.Stdin(), frame); err != nil {
				return
			}
		}
	}
}

// readStdout parses the engine's stdout line by line, enforcing the per-frame bound
// (MaxFrameBytes) and the total bound (MaxStdoutBytes) as the stream arrives, and
// delivers each frame — or the terminating error — on out. A clean EOF closes out.
func readStdout(ctx context.Context, r io.Reader, limits Limits, out chan<- streamRead) {
	defer close(out)
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 0, 64*1024), int(limits.MaxFrameBytes))
	var total int64
	for scanner.Scan() {
		line := scanner.Bytes()
		size := len(line) + 1
		total += int64(size)
		if total > limits.MaxStdoutBytes {
			sendRead(ctx, out, streamRead{err: fmt.Errorf("%w", ErrStdoutLimit)})
			return
		}
		if len(line) == 0 || int64(len(line)) > limits.MaxFrameBytes {
			sendRead(ctx, out, streamRead{err: fmt.Errorf("%w: frame violates frame bound", ErrInvalidEngineOutput)})
			return
		}
		var frame contracts.EngineFrame
		if err := json.Unmarshal(line, &frame); err != nil {
			sendRead(ctx, out, streamRead{err: fmt.Errorf("%w: frame is not JSON", ErrInvalidEngineOutput)})
			return
		}
		if !sendRead(ctx, out, streamRead{frame: frame, bytes: size}) {
			return
		}
	}
	if err := scanner.Err(); err != nil {
		if errors.Is(err, bufio.ErrTooLong) {
			sendRead(ctx, out, streamRead{err: fmt.Errorf("%w: frame exceeds frame bound", ErrInvalidEngineOutput)})
			return
		}
		sendRead(ctx, out, streamRead{err: fmt.Errorf("read engine stdout: %w", err)})
	}
}

// sendRead delivers one read to out unless the run context is already done.
func sendRead(ctx context.Context, out chan<- streamRead, read streamRead) bool {
	select {
	case out <- read:
		return true
	case <-ctx.Done():
		return false
	}
}

// helloFrame is the §25.6 supervisor.hello: the engine protocol, a fresh frame id, the
// run/attempt identity, the per-frame limit, and the lease fencing token as a hash. The
// engine compares fence_hash on E10 recovery to bind the handshake to the authorizing
// lease; here it is transported. The reference engine accepts any well-formed hello, so
// the value is not yet verified downstream.
func helloFrame(request EngineRequest) contracts.EngineFrame {
	return contracts.EngineFrame{
		Protocol:  EngineProtocolV1,
		ID:        newSupervisorFrameID(),
		Type:      frameSupervisorHello,
		Sequence:  1,
		Time:      time.Now().UTC().Format(time.RFC3339),
		RunID:     request.RunID,
		AttemptID: request.AttemptID,
		Data: map[string]any{
			"protocol_version": EngineProtocolV1,
			"fence_hash":       fenceHash(request.RunID, request.Fence),
			"limits":           map[string]any{"max_frame_bytes": request.Limits.MaxFrameBytes},
		},
	}
}

// fenceHash is the supervisor.hello fencing token: the hex sha256 of "<run_id>/<fence>",
// so the engine can bind the handshake to the lease that authorized it (§25.6) without
// the raw fence crossing the boundary.
func fenceHash(runID contracts.RunID, fence uint64) string {
	sum := sha256.Sum256([]byte(string(runID) + "/" + strconv.FormatUint(fence, 10)))
	return hex.EncodeToString(sum[:])
}

// writeStreamFrame marshals frame and writes it as one JSONL line with a per-frame flush
// (io.Writer.Write on the hijacked connection is unbuffered).
func writeStreamFrame(w io.Writer, frame contracts.EngineFrame) error {
	line, err := json.Marshal(frame)
	if err != nil {
		return err
	}
	if _, err := w.Write(append(line, '\n')); err != nil {
		return err
	}
	return nil
}

func newSupervisorFrameID() contracts.FrameID {
	var raw [16]byte
	_, _ = rand.Read(raw[:])
	return contracts.FrameID("frm_" + hex.EncodeToString(raw[:]))
}

// stderrSecretPatterns are the secret shapes the supervisor masks in engine stderr.
// ponytail: a focused set (provider secret keys, bearer tokens); extend it for a new
// secret shape rather than reaching for a full-entropy scanner.
var stderrSecretPatterns = []*regexp.Regexp{
	regexp.MustCompile(`sk-[A-Za-z0-9._-]{6,}`),
	regexp.MustCompile(`(?i)bearer\s+[A-Za-z0-9._-]{8,}`),
}

// redactStderr masks secret-shaped tokens in engine stderr before the runner persists or
// forwards it. The engine redacts its own logs, but the supervisor does not trust that.
func redactStderr(b []byte) []byte {
	for _, pattern := range stderrSecretPatterns {
		b = pattern.ReplaceAll(b, []byte("***"))
	}
	return b
}

// boundedStderr captures up to limit bytes of stderr while still counting the true total,
// mirroring the batch driver's bounded capture for the streaming path.
type boundedStderr struct {
	data      []byte
	limit     int64
	total     int64
	truncated bool
}

func (b *boundedStderr) readFrom(r io.Reader) {
	buf := make([]byte, 4096)
	for {
		n, err := r.Read(buf)
		if n > 0 {
			b.total += int64(n)
			if room := b.limit - int64(len(b.data)); room > 0 {
				take := int64(n)
				if take > room {
					take = room
				}
				b.data = append(b.data, buf[:take]...)
			}
			if b.total > b.limit {
				b.truncated = true
			}
		}
		if err != nil {
			return
		}
	}
}
