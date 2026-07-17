//go:build fault

package runner

import (
	"bytes"
	"context"
	"errors"
	"testing"
	"time"

	"github.com/palgroup/palai/adapters/sandboxes/oci"
	"github.com/palgroup/palai/packages/contracts"
	"github.com/palgroup/palai/packages/runner"
)

// newStreamSupervisor builds a streaming supervisor over the real Docker interactive
// driver, mirroring newSupervisor for the batch path. The driver's client is closed on
// cleanup.
func newStreamSupervisor(t *testing.T) *runner.StreamSupervisor {
	t.Helper()
	driver, err := oci.NewDockerInteractiveDriver()
	if err != nil {
		t.Fatalf("create Docker interactive driver: %v", err)
	}
	t.Cleanup(func() {
		if closer, ok := driver.(interface{ Close() error }); ok {
			if err := closer.Close(); err != nil {
				t.Errorf("close interactive driver: %v", err)
			}
		}
	})
	return runner.NewStreamSupervisor(driver)
}

// TestStreamKillClassifiesLostNotSuccess proves a streaming engine that is force-killed
// at the wall-time bound while it blocks for a controller model.result is classified
// terminal/lost (ErrEngineTimeout) — never a false success — and the partial frames it
// did stream never include a terminal. The container is gone afterward.
func TestStreamKillClassifiesLostNotSuccess(t *testing.T) {
	sup := newStreamSupervisor(t)
	request := fixtureRequest(engineDigest(t), "interactive")
	// The fixture blocks on stdin for a model.result the test never sends; the wall-time
	// bound force-kills it. A short bound keeps the fault deterministic under -count.
	request.Limits.WallTimeMS = 1500

	var forwarded []contracts.EngineFrame
	sink := func(_ context.Context, frame contracts.EngineFrame) error {
		forwarded = append(forwarded, frame)
		return nil
	}
	inbound := make(chan contracts.EngineFrame) // never fed: the engine stays blocked

	started := time.Now()
	result, err := sup.Stream(context.Background(), request, inbound, sink)
	if !errors.Is(err, runner.ErrEngineTimeout) {
		t.Fatalf("mid-stream kill error = %v, want ErrEngineTimeout (lost, not success)", err)
	}
	if elapsed := time.Since(started); elapsed > 10*time.Second {
		t.Fatalf("forced timeout took %s, want the wall-time bound to fire promptly", elapsed)
	}
	for _, frame := range forwarded {
		if frame.Type == "run.terminal" {
			t.Fatalf("a killed engine forwarded a terminal frame; partial output must not read as success: %+v", frame)
		}
	}
	assertContainerGone(t, result.ContainerID)
}

// TestStreamKillRedactsSentinelInForwardedStderr proves the streaming supervisor masks a
// secret-shaped token the engine wrote to stderr before the runner surfaces it. The
// fixture completes cleanly (the test feeds the model.result), so the whole stderr is
// captured and redacted rather than truncated by a kill.
func TestStreamKillRedactsSentinelInForwardedStderr(t *testing.T) {
	const sentinel = "sk-live-FAULTREDACTSENTINEL0123456789"
	sup := newStreamSupervisor(t)
	request := fixtureRequest(engineDigest(t), "interactive")
	request.Limits.WallTimeMS = 5000

	inbound := make(chan contracts.EngineFrame, 1)
	sawModelRequest := make(chan struct{}, 1)
	sink := func(_ context.Context, frame contracts.EngineFrame) error {
		if frame.Type == "model.request" {
			select {
			case sawModelRequest <- struct{}{}:
			default:
			}
		}
		return nil
	}

	// Answer the engine's model.request so it emits a terminal and exits cleanly.
	go func() {
		<-sawModelRequest
		inbound <- contracts.EngineFrame{
			Protocol:  "engine.v1",
			ID:        "frm_faultctl1",
			Type:      "model.result",
			Sequence:  1,
			Time:      time.Now().UTC().Format(time.RFC3339),
			RunID:     request.RunID,
			AttemptID: request.AttemptID,
			Data:      map[string]any{"model_request_id": "mreq_interactive1"},
		}
		close(inbound)
	}()

	result, err := sup.Stream(context.Background(), request, inbound, sink)
	if err != nil {
		t.Fatalf("interactive stream error = %v, want a clean completion", err)
	}
	if bytes.Contains(result.Stderr, []byte(sentinel)) {
		t.Fatalf("forwarded stderr still contains the sentinel secret: %q", result.Stderr)
	}
	if !bytes.Contains(result.Stderr, []byte("***")) {
		t.Fatalf("forwarded stderr was not redacted (no mask present): %q", result.Stderr)
	}
	assertContainerGone(t, result.ContainerID)
}
