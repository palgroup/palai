package mcp

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/palgroup/palai/adapters/sandboxes/oci"
)

// failDriver is an InteractiveDriver whose Start always fails, simulating an MCP server that crashes on
// launch. It counts starts so a test can prove the breaker sheds without dialing once open.
type failDriver struct{ starts int }

func (d *failDriver) Start(context.Context, oci.ContainerSpec) (oci.Process, error) {
	d.starts++
	return nil, errors.New("mcp server crashed on launch")
}

// TestMCPServerCrashTripsBreakerToolUnavailable proves EXT-005's connection-level defence: repeated launch
// failures trip the in-memory breaker; once open, a further call returns ErrToolUnavailable WITHOUT dialing
// (the container start count stops advancing) — a visible, fast failure, and the control plane stays up.
func TestMCPServerCrashTripsBreakerToolUnavailable(t *testing.T) {
	driver := &failDriver{}
	m := NewManager(Config{Driver: driver, BreakerThreshold: 3, BreakerCooldown: time.Hour})
	conn := ConnConfig{ID: "mcpc_x", Transport: "stdio", ImageDigest: "sha256:" + zeros64(), Cmd: []string{"/mcp"}}
	ctx := context.Background()

	// The first three calls each attempt a start and fail (a real dial error, not shed).
	for i := 0; i < 3; i++ {
		if _, err := m.Call(ctx, CallScope{}, conn, "echo", nil); err == nil {
			t.Fatalf("call %d: expected a launch failure", i)
		}
	}
	if driver.starts != 3 {
		t.Fatalf("start attempts = %d, want 3 before the breaker trips", driver.starts)
	}
	// The breaker is now open: the next call is shed FAST as ErrToolUnavailable, with no new dial.
	if _, err := m.Call(ctx, CallScope{}, conn, "echo", nil); !errors.Is(err, ErrToolUnavailable) {
		t.Fatalf("post-trip call err = %v, want ErrToolUnavailable", err)
	}
	if driver.starts != 3 {
		t.Fatalf("start attempts = %d after the breaker opened, want it unchanged at 3 (shed without dialing)", driver.starts)
	}
}

// TestMCPStdioRequiresDriver proves the stdio path fails cleanly (never escapes) when no OCI driver is
// wired — a call returns an error rather than running the server on the host.
func TestMCPStdioRequiresDriver(t *testing.T) {
	m := NewManager(Config{}) // no driver
	_, err := m.Call(context.Background(), CallScope{}, ConnConfig{ID: "c", Transport: "stdio", ImageDigest: "sha256:" + zeros64(), Cmd: []string{"/mcp"}}, "echo", nil)
	if err == nil {
		t.Fatal("stdio call with no driver returned nil, want a clean failure (no host escape)")
	}
}

func zeros64() string { return "0000000000000000000000000000000000000000000000000000000000000000" }
