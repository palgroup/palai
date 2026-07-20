package execution_test

import (
	"context"
	"io"
	"testing"
	"time"

	"github.com/palgroup/palai/adapters/sandboxes/oci"
	"github.com/palgroup/palai/apps/control-plane/internal/execution"
	"github.com/palgroup/palai/packages/runner"
)

// blockingDriver simulates an engine container that runs until its lease context is cancelled: its
// process's stdout blocks (never EOFs) so StreamSupervisor.Stream keeps reading, which keeps
// serveLease holding its lease and never re-parking — letting a test observe how many leases a
// single runner identity parks at once.
type blockingDriver struct{}

func (blockingDriver) Start(ctx context.Context, _ oci.ContainerSpec) (oci.Process, error) {
	return blockingProcess{ctx: ctx}, nil
}

type blockingProcess struct{ ctx context.Context }

func (p blockingProcess) Stdin() io.WriteCloser { return nopWriteCloser{} }
func (p blockingProcess) Stdout() io.Reader     { return ctxReader{p.ctx} }
func (p blockingProcess) Stderr() io.Reader     { return ctxReader{p.ctx} }
func (blockingProcess) Kill(context.Context) error { return nil }
func (p blockingProcess) Wait(ctx context.Context) (oci.Outcome, error) {
	<-ctx.Done()
	return oci.Outcome{}, ctx.Err()
}

// ctxReader blocks until its context is cancelled, then reports EOF — a stdout that stays open
// for the life of the lease.
type ctxReader struct{ ctx context.Context }

func (r ctxReader) Read([]byte) (int, error) { <-r.ctx.Done(); return 0, io.EOF }

type nopWriteCloser struct{}

func (nopWriteCloser) Write(p []byte) (int, error) { return len(p), nil }
func (nopWriteCloser) Close() error                { return nil }

// TestRunnerConcurrencyParksLeasesOnOneIdentity proves the delegation runner fix (spec §25.18,
// SUB-002/005): with Concurrency=2 a SINGLE runner identity parks two leases at once, so a
// delegating run's parent and its inline child each get an engine on one runner — where the
// default (1) parks one at a time, so a second concurrent Dial blocks (LP-0 sequential behaviour,
// unregressed).
func TestRunnerConcurrencyParksLeasesOnOneIdentity(t *testing.T) {
	// Concurrency=2: two concurrent Dials both get an engine channel on one runner identity.
	t.Run("two concurrent leases both get an engine", func(t *testing.T) {
		ch1, ok1, ch2, ok2 := serveAndDialTwice(t, 2)
		if !ok1 || !ok2 {
			t.Fatalf("Concurrency=2: both Dials must get an engine (got %v, %v)", ok1, ok2)
		}
		_ = ch1.Close()
		_ = ch2.Close()
	})
	// Concurrency=1 (default): one lease at a time — the first Dial gets an engine, the second
	// blocks because the sole park loop is busy holding the first lease.
	t.Run("default serves one lease at a time", func(t *testing.T) {
		ch1, ok1, _, ok2 := serveAndDialTwice(t, 1)
		if !ok1 || ok2 {
			t.Fatalf("Concurrency=1: exactly one Dial should get an engine (got %v, %v)", ok1, ok2)
		}
		_ = ch1.Close()
	})
}

// serveAndDialTwice enrolls one runner, runs its real Serve loop at the given concurrency against
// the real gateway, and issues two Dials — reporting whether each got an engine channel.
func serveAndDialTwice(t *testing.T, concurrency int) (execution.EngineChannel, bool, execution.EngineChannel, bool) {
	t.Helper()
	f := newGatewayFixture(t, newOneUseTokens("conc-token"))
	identity, err := runner.Enroll(context.Background(), f.bootstrap("conc-token"))
	if err != nil {
		t.Fatalf("enroll: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		runner.ServeConfig{
			Session:     f.session(identity),
			Supervisor:  runner.NewStreamSupervisor(blockingDriver{}),
			Now:         time.Now,
			Backoff:     20 * time.Millisecond,
			Concurrency: concurrency,
		}.Serve(ctx)
	}()
	t.Cleanup(func() { cancel(); <-done })

	ch1, ok1 := tryDial(f, "run_conc1", "att_conc1", 1)
	ch2, ok2 := tryDial(f, "run_conc2", "att_conc2", 2)
	return ch1, ok1, ch2, ok2
}

// tryDial offers a waiting attempt to the runner, returning the engine channel and whether it was
// leased within a bounded window (a blocked Dial — no free lease slot — reports false).
func tryDial(f *gatewayFixture, runID, attemptID string, fence uint64) (execution.EngineChannel, bool) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	ch, err := f.gateway.Dial(ctx, f.attempt(runID, attemptID, fence))
	return ch, err == nil
}
