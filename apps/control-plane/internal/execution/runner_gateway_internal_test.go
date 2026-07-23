package execution

// White-box guard for the single-closer invariant behind MUST-FIX 1: Dial's write-error path used to
// call gc.closeFrames() while readLoop could be mid-emit on the same channel — a send on a closed
// channel, which panics and (unrecovered, in a goroutine) crashes the whole control plane. The fix
// closes pr.release instead; emit unblocks as false and readLoop stays the SOLE frames-closer. These
// tests reproduce that interleaving at the channel level (no websocket needed) and would panic if the
// invariant regressed.

import (
	"testing"
	"time"

	"github.com/palgroup/palai/packages/contracts"
)

func testPending() *pendingRunner {
	return &pendingRunner{release: make(chan struct{}), disconnected: make(chan struct{})}
}

// TestReleaseUnblocksEmitWithoutClosedSend is the exact Dial-write-error interleaving: a frame is
// mid-emit (blocked, no Receiver) when the handler tears down. Closing release must unblock emit as
// false with no panic, and readLoop (the single closer) then closes frames safely.
func TestReleaseUnblocksEmitWithoutClosedSend(t *testing.T) {
	pr := testPending()
	gc := newGatewayChannel(pr, AttemptDescriptor{})
	pr.gc.Store(gc)

	emitResult := make(chan bool, 1)
	go func() { emitResult <- gc.emit(relayRead{frame: contracts.EngineFrame{ID: "frm_x"}}) }()

	// Let emit reach its blocking send (no Receiver on frames). The assertion holds regardless of
	// whether emit has blocked yet — either way the close must unblock/short-circuit it as false.
	time.Sleep(20 * time.Millisecond)
	close(pr.release) // the fix's path — NOT close(gc.frames)

	select {
	case delivered := <-emitResult:
		if delivered {
			t.Fatal("emit returned true, but release was closed before any Receiver arrived")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("emit did not unblock after release closed — a stuck emit means a leaked readLoop")
	}

	// readLoop is the sole frames-closer; closing here (and twice) must be panic-free and idempotent.
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
