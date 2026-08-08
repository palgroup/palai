package execution

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"
)

// TestANudgeSweepsWithoutWaitingForTheTick — THE PRODUCT SENTENCE IS AN EVENT, NOT A TIMER.
//
// "When the agent stops and nothing is running, the machine goes back" names a moment that is knowable
// exactly: an attempt finished with its workspace and drove it to `ready`. Before this, that moment was
// followed by up to a full sweep interval of pure waiting, on top of the TTL an operator actually chose.
//
// The interval here is deliberately far longer than the test could tolerate: if the nudge did nothing,
// this test would time out rather than fail on a subtle count, which is the difference between a test
// that measures the mechanism and one that measures the clock.
func TestANudgeSweepsWithoutWaitingForTheTick(t *testing.T) {
	swept := make(chan struct{}, 4)
	r := &IdleReleaser{wake: make(chan struct{}, 1), sweep: func(context.Context) (int, error) {
		swept <- struct{}{}
		return 0, nil
	}}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = r.Run(ctx, time.Hour) }()

	r.Nudge()
	select {
	case <-swept:
	case <-time.After(5 * time.Second):
		t.Fatal("a nudge did not sweep: a quiet session waits for the next tick, which is latency nobody chose")
	}
}

// TestABurstOfNudgesCollapsesIntoOneSweep — a finishing burst must not queue one pass per run. The
// channel is buffered to one and a full buffer means a sweep is already pending; the alternative is a
// sweep loop that spends its time re-asking a question it just answered.
func TestABurstOfNudgesCollapsesIntoOneSweep(t *testing.T) {
	r := &IdleReleaser{wake: make(chan struct{}, 1)}
	for i := 0; i < 100; i++ {
		r.Nudge() // must not block, ever: a caller is never slowed down by having told us
	}
	if len(r.wake) != 1 {
		t.Errorf("the wake channel holds %d, want exactly 1 — a burst must collapse", len(r.wake))
	}
}

// TestReleasingAWorkspaceTELLSTheSweep — THE CALL SITE IS THE PART A COMPILER CANNOT DEFEND.
//
// Deleting `o.idleNudge()` from releaseWorkspace compiles cleanly, passes every unit test, and produces
// a stack where a quiet session waits for the next tick — the exact "declared but not wired" shape this
// tree keeps finding, and the reason the nudge exists at all.
//
// So this reads the source: the one function that drives a workspace back to `ready` must be the one
// that says so. Anchored on the function's own name, so a rename fails loudly rather than making the
// guard vacuous.
func TestReleasingAWorkspaceTELLSTheSweep(t *testing.T) {
	body, err := os.ReadFile("provision.go")
	if err != nil {
		t.Fatalf("read the source: %v", err)
	}
	src := string(body)

	start := strings.Index(src, "func (o *Orchestrator) releaseWorkspace")
	if start < 0 {
		t.Fatal("releaseWorkspace is gone or renamed: this guard is reading a file it does not understand, " +
			"and everything it concludes is about code that is not there")
	}
	end := strings.Index(src[start:], "\n}\n")
	if end < 0 {
		t.Fatal("releaseWorkspace's body did not parse")
	}
	if !strings.Contains(src[start:start+end], "o.idleNudge()") {
		t.Error("releaseWorkspace does not nudge the idle sweep. The workspace goes back to `ready` here and " +
			"nothing notices until the next tick — up to a full sweep interval of latency on top of the TTL, " +
			"on the one path the product sentence is about.")
	}
}
