package execution

// White-box guard for the single-closer invariant behind MUST-FIX 1: Dial's write-error path used to
// call gc.closeFrames() while readLoop could be mid-emit on the same channel — a send on a closed
// channel, which panics and (unrecovered, in a goroutine) crashes the whole control plane. The fix
// closes pr.release instead; emit unblocks as false and readLoop stays the SOLE frames-closer. These
// tests reproduce that interleaving at the channel level (no websocket needed) and would panic if the
// invariant regressed.

import (
	"errors"
	"testing"
	"time"

	"github.com/palgroup/palai/packages/contracts"
)

func testPending() *pendingRunner {
	return &pendingRunner{release: make(chan struct{}), disconnected: make(chan struct{})}
}

// TestEmitNeverParksTheConnectionsSoleReader is the STRONGER form of the guard this file opened with,
// and the reason it changed is A.3.
//
// The original reproduced the Dial-write-error interleaving directly: a frame mid-emit (blocked, no
// Receiver) when the handler tore down, which must unblock as false rather than panic on a closed
// channel. That interleaving can no longer arise, because emit no longer parks at all — since a shell
// command's answer arrives through the same goroutine, a send that waited for a receiver could wait for
// a receiver that is itself waiting on this reader. So the hazard is eliminated rather than handled,
// and what is asserted here is the elimination: emit RETURNS with no Receiver, at every depth including
// a full backlog, and the single-closer invariant still holds around it.
func TestEmitNeverParksTheConnectionsSoleReader(t *testing.T) {
	pr := testPending()
	gc := newGatewayChannel(pr, AttemptDescriptor{})
	pr.gc.Store(gc)

	// Fill the backlog with nobody receiving — the state an orchestrator inside a tool call leaves.
	emitted := make(chan bool, 1)
	for i := 0; i < relayBacklog+2; i++ {
		go func() { emitted <- gc.emit(relayRead{frame: contracts.EngineFrame{ID: "frm_x"}}) }()
		select {
		case <-emitted:
		case <-time.After(2 * time.Second):
			t.Fatalf("emit parked at depth %d with no Receiver: the reader that would deliver a waiting "+
				"command's answer is the one that is stuck", i)
		}
	}

	// Past the backlog emit reports "stop" rather than growing, and the reason is IN the stream so the
	// attempt learns why instead of seeing an engine that closed early.
	var reasons int
	for len(gc.frames) > 0 {
		if read := <-gc.frames; errors.Is(read.err, errRelayBacklogFull) {
			reasons++
		}
	}
	if reasons != 1 {
		t.Fatalf("the drained relay carried %d backlog-full reasons, want exactly 1", reasons)
	}

	// A command waiting on this lease was answered rather than left for the connection to outlive.
	answers, _, err := gc.execs.register("exec_after_overflow")
	if err == nil {
		t.Fatalf("a command registered after the relay gave up and was accepted (answers=%v)", answers)
	}

	// readLoop is the sole frames-closer; closing here (and twice) must be panic-free and idempotent.
	close(pr.release)
	gc.closeFrames()
	gc.closeFrames()
}

// TestCloseFramesIsIdempotent guards closeFrames's sync.Once directly: readLoop reaches it from several
// return paths, so a second close must not panic.
func TestCloseFramesIsIdempotent(t *testing.T) {
	gc := newGatewayChannel(testPending(), AttemptDescriptor{})
	gc.closeFrames()
	gc.closeFrames()
	select {
	case _, ok := <-gc.frames:
		if ok {
			t.Fatal("frames channel yielded a value after closeFrames")
		}
	default:
		t.Fatal("frames channel not closed after closeFrames")
	}
}
