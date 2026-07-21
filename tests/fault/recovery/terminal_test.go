//go:build fault

package recovery

import (
	"context"
	"testing"

	"github.com/palgroup/palai/packages/contracts"
)

// TestFastExitEngineTerminalFrameNeverLost is the REC-001 drain proof against a REAL container: a
// fixture engine that emits run.terminal and exits immediately races the supervisor's stream drain.
// Over N iterations, the terminal must reach the sink EVERY time and the attempt must classify as a
// clean success — this is a deterministic drain proof, not a flake-tolerant retry. Before the OCI
// drain-then-reap fix, the container's buffered terminal could be severed by destroy()'s
// attach.Close before the scanner read it, dropping the terminal on some iterations.
func TestFastExitEngineTerminalFrameNeverLost(t *testing.T) {
	const iterations = 20
	digest := engineDigest(t)

	for i := 0; i < iterations; i++ {
		sup := newStreamSupervisor(t)
		request := fixtureRequest(digest, "fastexit")

		sawTerminal := false
		sink := func(_ context.Context, frame contracts.EngineFrame) error {
			if frame.Type == "run.terminal" {
				sawTerminal = true
			}
			return nil
		}
		inbound := make(chan contracts.EngineFrame)
		close(inbound) // the fast-exit fixture never blocks for a controller frame

		result, err := sup.Stream(context.Background(), request, inbound, sink)
		if err != nil {
			t.Fatalf("iteration %d: fast-exit stream error = %v, want a clean completion", i, err)
		}
		if !sawTerminal {
			t.Fatalf("iteration %d: run.terminal never reached the sink — the tail frame was lost in the drain race", i)
		}
		if result.ExitCode != 0 {
			t.Fatalf("iteration %d: exit code = %d, want 0 (a fast clean exit)", i, result.ExitCode)
		}
		assertContainerGone(t, result.ContainerID)
	}
}
