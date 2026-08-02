package execution

import (
	"context"
	"strings"
	"sync"
	"time"
)

// deltaWindow is how long streamed text is coalesced before one model_step.delta.v1 is journalled.
//
// IT IS MEASURED RATHER THAN PICKED, and the measurement is the transport's own: the SSE endpoint is a
// journal TAIL, not a live channel — apps/control-plane/api/events.go:38 sets PollInterval to 500ms and
// events.go:67 states "The journal is the source of truth". A window shorter than that poll cadence
// therefore buys a client nothing it can observe, while costing one Postgres row per window. Matching the
// cadence is the smallest window whose cost a reader is actually paid back for.
//
// If that poll interval changes, this should follow it; the two are the same number for one reason.
const deltaWindow = 500 * time.Millisecond

// deltaFlushBytes forces a flush before the window expires when a provider streams faster than the
// window. It is below coordinator.maxDeltaText on purpose: reaching the store's cap means text was
// TRUNCATED, and a sink that silently relies on the far side to trim is a sink that loses tokens.
const deltaFlushBytes = 8 * 1024

// deltaSink coalesces a provider's token stream into windows and journals each window as one advisory
// model_step.delta.v1.
//
// WHY A SINK RATHER THAN A DIRECT CALL. onDelta runs SYNCHRONOUSLY inside the broker's Route on the
// dispatch goroutine (see model_dispatch.go). Journalling from inside it would put a Postgres round trip
// in the provider's read loop and stall the stream it is trying to report. So add() only appends to a
// buffer under a mutex, and a ticker goroutine does the writing.
//
// EVERY WRITE IS BEST-EFFORT. A delta describes text whose authoritative record is the model step's own
// completed event; a delta that cannot be journalled must never fail, stall, or alter the step. Errors
// are dropped here deliberately rather than returned to a caller who has nothing useful to do with them.
type deltaSink struct {
	mu  sync.Mutex
	buf strings.Builder

	write func(ctx context.Context, text string)
	ctx   context.Context

	stop chan struct{}
	done chan struct{}
}

// newDeltaSink starts the flush loop. close() must be called exactly once; it flushes the tail.
func newDeltaSink(ctx context.Context, write func(ctx context.Context, text string)) *deltaSink {
	s := &deltaSink{
		write: write,
		ctx:   ctx,
		stop:  make(chan struct{}),
		done:  make(chan struct{}),
	}
	go s.loop()
	return s
}

func (s *deltaSink) loop() {
	defer close(s.done)
	t := time.NewTicker(deltaWindow)
	defer t.Stop()
	for {
		select {
		case <-s.stop:
			return
		case <-t.C:
			s.flush()
		}
	}
}

// add buffers streamed text. It never blocks on I/O — see the type comment.
func (s *deltaSink) add(text string) {
	if text == "" {
		return
	}
	s.mu.Lock()
	s.buf.WriteString(text)
	full := s.buf.Len() >= deltaFlushBytes
	s.mu.Unlock()
	if full {
		s.flush()
	}
}

// flush writes one window, if there is anything in it. Taking the buffer under the lock and writing
// outside it keeps add() non-blocking while a flush is in flight.
func (s *deltaSink) flush() {
	s.mu.Lock()
	text := s.buf.String()
	s.buf.Reset()
	s.mu.Unlock()
	if text == "" {
		return
	}
	s.write(s.ctx, text)
}

// close stops the loop and flushes whatever the last window holds, SYNCHRONOUSLY, so the tail of a
// model step's text is journalled before that step's terminal event. A delta that landed after its own
// model_step.completed.v1 would put a client's transcript out of order, which is worse than a delta that
// never arrived.
func (s *deltaSink) close() {
	close(s.stop)
	<-s.done
	s.flush()
}
