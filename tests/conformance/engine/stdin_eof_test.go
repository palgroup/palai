package engine_test

import (
	"bufio"
	"context"
	"io"
	"testing"
	"time"

	"github.com/palgroup/palai/adapters/sandboxes/oci"
	"github.com/palgroup/palai/packages/runner"
)

// AN ENGINE ONLY EXITS WHEN ITS STDIN CLOSES, AND THE SUPERVISOR USED TO WAIT FOR IT FIRST.
//
// ‼️ engines/reference's whole loop is `for line in sys.stdin`, ending on a TERMINAL frame or on EOF.
// So a run whose stdout ended WITHOUT a terminal — which is every coding run that finishes its tool
// frames and stops — leaves the engine sitting on a stdin nobody has closed. The supervisor then spent
// its thirty-second drain waiting for a container to exit while being itself the reason it could not:
// both halves of the duplex hung off one context, and that context was only cancelled when the whole
// supervise call RETURNED, which is after the wait.
//
// Measured 2026-08-07 on a live plane: coding runs failed this way roughly half the time —
// `engine wait did not complete after stdout closed`, then `engine exceeded wall-time bound` on the
// retry, and a run that reached no terminal at all.
//
// The fix is one ordering: shut the write half BEFORE waiting. This test models the engine honestly —
// a process whose Wait does not return until its stdin reaches EOF — so it fails on the old ordering
// by timing out and passes on the new one in milliseconds.

// stdinEOFProcess is a fake engine that exits only when the supervisor closes stdin, which is what the
// reference engine does. Everything else is the shared fake.
type stdinEOFProcess struct{ *fakeProcess }

func (p *stdinEOFProcess) Wait(context.Context) (oci.Outcome, error) {
	// Returns when the write half of stdin is closed. Draining rather than peeking, because the
	// supervisor writes its hello and any injected frames here and an unread pipe would block it.
	_, _ = io.Copy(io.Discard, p.stdinR)
	return oci.Outcome{ExitCode: 0}, nil
}

type stdinEOFDriver struct{ process *stdinEOFProcess }

func (d stdinEOFDriver) Start(context.Context, oci.ContainerSpec) (oci.Process, error) {
	return d.process, nil
}

func TestStreamClosesStdinBeforeWaitingForTheEngine(t *testing.T) {
	proc := &stdinEOFProcess{newFakeProcess()}
	sup := runner.NewStreamSupervisor(stdinEOFDriver{proc})
	sink := &collectingSink{}

	done := make(chan error, 1)
	go func() {
		_, err := sup.Stream(context.Background(), streamRequest(), nil, sink.sink)
		done <- err
	}()

	// The engine announces itself, emits one ordinary frame, and its output ends — WITHOUT a terminal.
	// That is the case the ordering bug lived in: with a terminal frame the loop breaks for a different
	// reason and the engine has already decided to leave.
	writeEngineFrame(t, proc.stdoutW, engineFrame("engine.ready", 1, "frm_ready", readyData()))
	writeEngineFrame(t, proc.stdoutW, engineFrame("model_step.created", 2, "frm_step", map[string]any{}))
	_ = proc.stdoutW.Close()
	_ = proc.stderrW.Close()

	// ‼️ THE BOUND HERE IS NOT THE ASSERTION, IT IS THE FAILURE MODE. The supervisor's own drain is
	// thirty seconds; on the old ordering this select falls through to the timeout every time, and on
	// the new one Stream returns as soon as stdin reaches EOF. Five seconds is far past "fast" and far
	// short of the drain, so the two outcomes cannot be confused for slowness.
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Stream did not return: the engine is waiting on a stdin the supervisor has not closed, " +
			"and the supervisor is waiting on an engine that cannot leave until it does")
	}

	// AND STDIN REALLY REACHED EOF, which the Wait above already required — asserted separately so a
	// future Stream that returned early for some other reason could not pass this test.
	if _, err := bufio.NewReader(proc.stdinR).ReadByte(); err != io.EOF && err != io.ErrClosedPipe {
		t.Fatalf("stdin did not reach EOF after Stream returned: %v", err)
	}
}
