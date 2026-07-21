//go:build fault

package recovery

import (
	"context"
	"testing"

	"github.com/palgroup/palai/packages/contracts"
)

// TestContainerKillNeverFalseSuccess proves the ENG-005 streaming half (spec §26.8): an EXTERNAL
// `docker rm -f` of the engine container mid-run — not a wall-time kill — must fail the attempt
// (lost, never a false success), and the partial frames it forwarded never include a terminal.
//
// Honest ceiling: this test owns the container-kill CLASSIFICATION half. ENG-005's ladder-restore,
// canonical message-ORDER (T2 delivered_messages, §26.9), and workspace-manifest checksum equality
// (T6 snapshot restore) ride those substrates — the ladder under a real kill is proven by ENG-004's
// e2e process-kill restore, and the control-plane-over-container workspace restore is T6's harness.
func TestContainerKillNeverFalseSuccess(t *testing.T) {
	sup := newStreamSupervisor(t)
	request := fixtureRequest(engineDigest(t), "interactive")
	// A long wall-time bound so the EXTERNAL container removal, not the wall clock, ends the run.
	request.Limits.WallTimeMS = 30000

	sawReady := make(chan struct{}, 1)
	var forwarded []contracts.EngineFrame
	sink := func(_ context.Context, frame contracts.EngineFrame) error {
		forwarded = append(forwarded, frame)
		if frame.Type == "engine.ready" {
			select {
			case sawReady <- struct{}{}:
			default:
			}
		}
		return nil
	}
	inbound := make(chan contracts.EngineFrame) // never fed: the engine stays blocked for a model.result

	// Once the engine is live, remove its container out from under the supervisor.
	go func() {
		<-sawReady
		removeSandboxContainers(t)
	}()

	result, err := sup.Stream(context.Background(), request, inbound, sink)
	if err == nil {
		t.Fatal("an externally-removed container must fail the attempt, not report success")
	}
	for _, frame := range forwarded {
		if frame.Type == "run.terminal" {
			t.Fatalf("a container-killed engine forwarded a terminal frame; partial output must not read as success: %+v", frame)
		}
	}
	assertContainerGone(t, result.ContainerID)
}
