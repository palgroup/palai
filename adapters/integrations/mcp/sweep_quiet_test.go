package mcp

import (
	"context"
	"errors"
	"log"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/moby/moby/client"
)

// brokenDocker fails every list, which is what a control-plane with no reachable Docker socket sees on
// EVERY tick — the standing condition §3.6 D8 measured.
type brokenDocker struct{ lists atomic.Int64 }

func (b *brokenDocker) ContainerList(context.Context, client.ContainerListOptions) (client.ContainerListResult, error) {
	b.lists.Add(1)
	return client.ContainerListResult{}, errors.New("cannot connect to the docker daemon")
}

func (b *brokenDocker) ContainerRemove(context.Context, string, client.ContainerRemoveOptions) (client.ContainerRemoveResult, error) {
	return client.ContainerRemoveResult{}, errors.New("cannot connect to the docker daemon")
}

// TestSweepPassErrorIsLoggedOncePerProcess is the §3.6 D8 RED, and it is a COUNTER assertion rather than
// an eyeball one: today Run logs the same standing failure on every tick, once a minute, forever.
//
// The justification is not cosmetic. E21 is the epic that puts real weight on the MCP path, and a
// once-a-minute false alarm is precisely the thing a real MCP failure gets buried under. The repo already
// has the right shape for "say a standing truth once" — slack_stream.go's f.statusUnusable.Do — and this
// pins it here.
func TestSweepPassErrorIsLoggedOncePerProcess(t *testing.T) {
	var logged strings.Builder
	restore := log.Writer()
	log.SetOutput(&logged)
	t.Cleanup(func() { log.SetOutput(restore) })

	broken := &brokenDocker{}
	s := NewSweeperWithClient(broken, time.Minute)

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		// Ten ticks of a standing failure. One line, not ten.
		for broken.lists.Load() < 10 {
			time.Sleep(time.Millisecond)
		}
		cancel()
	}()
	if err := s.Run(ctx, time.Millisecond); !errors.Is(err, context.Canceled) {
		t.Fatalf("Run returned %v, want context.Canceled", err)
	}

	if got := broken.lists.Load(); got < 10 {
		t.Fatalf("only %d passes ran; the count assertion below would be vacuous", got)
	}
	n := strings.Count(logged.String(), "mcp orphan-sweep")
	if n != 1 {
		t.Fatalf("a standing sweep failure was logged %d times over %d passes, want exactly 1 per process — "+
			"a once-a-minute false alarm is what buries a real MCP failure:\n%s", n, broken.lists.Load(), logged.String())
	}
	if !strings.Contains(logged.String(), "once per process") {
		t.Fatalf("the one line does not say it is the only one, so a reader cannot tell a standing fault from a "+
			"single blip:\n%s", logged.String())
	}
}
