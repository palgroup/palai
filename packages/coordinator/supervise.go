package coordinator

import (
	"context"
	"fmt"
	"maps"
	"sync"
	"time"
)

// Supervisor keeps long-lived background loops alive: it runs each named function and, when
// one returns a non-nil, non-cancellation error, logs it, records a restart, backs off, and
// runs it again. There is deliberately NO restart cap — a logging restart loop is the right
// default for the dispatcher, reconciler, and retention reaper, because a cap would
// reintroduce the silent death this guards against (LP-15 adjudication). A function that
// finishes cleanly (nil) or whose context is cancelled is not restarted.
type Supervisor struct {
	log     func(string, ...any)
	backoff time.Duration

	mu       sync.Mutex
	restarts map[string]int
}

// NewSupervisor binds a logger and a restart backoff (zero uses a one-second default).
func NewSupervisor(log func(string, ...any), backoff time.Duration) *Supervisor {
	if backoff <= 0 {
		backoff = time.Second
	}
	return &Supervisor{log: log, backoff: backoff, restarts: map[string]int{}}
}

// Supervise runs fn under name until ctx is cancelled, restarting it after any non-nil,
// non-cancellation error. It blocks; callers run it in a goroutine, one per supervised loop.
func (s *Supervisor) Supervise(ctx context.Context, name string, fn func(context.Context) error) {
	for {
		if ctx.Err() != nil {
			return
		}
		err := runGuarded(ctx, fn)
		if ctx.Err() != nil {
			return // cancelled: a clean shutdown, not a crash
		}
		if err == nil {
			return // fn finished its work and does not ask to be restarted
		}
		s.mu.Lock()
		s.restarts[name]++
		count := s.restarts[name]
		s.mu.Unlock()
		if s.log != nil {
			s.log("supervised %q failed (restart %d); restarting after backoff: %v", name, count, err)
		}
		if sleep(ctx, s.backoff) != nil {
			return // cancelled during backoff
		}
	}
}

// runGuarded runs fn and converts a panic into a returned error, so a supervised loop that
// panics is restarted exactly as one that returns an error is — never crashing the whole
// process. The kill matrix (E10 T5) drives real panics through this path; without the recover
// a single loop panic takes the dispatcher, reconciler, reaper, and GC down with it.
func runGuarded(ctx context.Context, fn func(context.Context) error) (err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("supervised loop panicked: %v", r)
		}
	}()
	return fn(ctx)
}

// Restarts returns a snapshot of the restart count per supervised name — the counter doctor
// surfaces so a silently-restarting loop is visible instead of hidden.
func (s *Supervisor) Restarts() map[string]int {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make(map[string]int, len(s.restarts))
	maps.Copy(out, s.restarts)
	return out
}
