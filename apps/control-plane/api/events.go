package api

import (
	"context"
	"net/http"
	"strconv"
	"time"

	"github.com/palgroup/palai/apps/control-plane/api/middleware"
	"github.com/palgroup/palai/packages/contracts"
)

// EventReader is the journal seam the SSE endpoint reads through. execution.Journal
// implements it in production; every method is tenant-scoped by org+project so a
// session from another tenant is never readable (spec §39.2).
type EventReader interface {
	SessionExists(ctx context.Context, org, project, sessionID string) (bool, error)
	ResolveCursor(ctx context.Context, org, project, sessionID, eventID string) (int64, bool, error)
	After(ctx context.Context, org, project, sessionID string, afterSeq int64, limit int) ([]contracts.Event, error)
}

// SSEConfig tunes the event-stream timers. Zero values take production defaults
// via withDefaults(); tests shorten them so assertions never wait on wall-clock.
type SSEConfig struct {
	Heartbeat    time.Duration // max idle before a keep-alive comment (spec: 15s)
	PollInterval time.Duration // journal tail poll cadence
	WriteTimeout time.Duration // per-write deadline; a slower consumer is dropped
	BatchLimit   int           // events read per poll — bounds per-connection memory
}

func (c SSEConfig) withDefaults() SSEConfig {
	if c.Heartbeat <= 0 {
		c.Heartbeat = 15 * time.Second
	}
	if c.PollInterval <= 0 {
		c.PollInterval = 500 * time.Millisecond
	}
	if c.WriteTimeout <= 0 {
		c.WriteTimeout = 15 * time.Second
	}
	if c.BatchLimit <= 0 {
		c.BatchLimit = 256
	}
	return c
}

// terminalEventTypes are the run terminal states (spec §22.3); the stream closes
// cleanly after emitting one. ponytail: an LP session carries a single run, so its
// terminal is the journal's end; widen if response/session terminals must close.
var terminalEventTypes = map[string]bool{
	"run.completed.v1":       true,
	"run.failed.v1":          true,
	"run.canceled.v1":        true,
	"run.timed_out.v1":       true,
	"run.budget_exceeded.v1": true,
}

type eventsHandler struct {
	reader EventReader
	cfg    SSEConfig
}

// stream serves the resumable SSE endpoint. It replays the journal from the resume
// cursor, tails it for new events, sends keep-alive heartbeats while idle, and
// closes cleanly after a terminal event. The journal is the source of truth, so a
// dropped or slow consumer loses nothing: it reconnects with Last-Event-ID and
// resumes from the next sequence. The handler only reads — a client disconnect
// never writes state, so it cannot cancel the run.
func (h *eventsHandler) stream(w http.ResponseWriter, r *http.Request) {
	scope, ok := middleware.ScopeFrom(r.Context())
	if !ok {
		middleware.WriteProblem(w, r, http.StatusUnauthorized, "authentication_required", "a bearer API key is required")
		return
	}
	sessionID := r.PathValue("session_id")
	ctx := r.Context()

	exists, err := h.reader.SessionExists(ctx, scope.Organization, scope.Project, sessionID)
	if err != nil {
		middleware.WriteProblem(w, r, http.StatusInternalServerError, "internal_error", "")
		return
	}
	if !exists {
		// A foreign or unknown session is a 404 — never a signal that the id exists
		// in another tenant (spec §39.2).
		middleware.WriteProblem(w, r, http.StatusNotFound, "not_found", "the session does not exist")
		return
	}

	cursor := h.resolveCursor(ctx, scope, sessionID, r)

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.WriteHeader(http.StatusOK)

	rc := http.NewResponseController(w)
	_ = rc.Flush() // flush 200 + headers so the client starts reading immediately

	h.pump(ctx, w, rc, scope, sessionID, cursor)
}

// resolveCursor picks the resume point: an explicit after_sequence query param
// wins, else a Last-Event-ID header (an evt_* id) resolved to its sequence, else 0
// (from the beginning). An unparsable param or unknown id falls back to 0.
func (h *eventsHandler) resolveCursor(ctx context.Context, scope middleware.Scope, sessionID string, r *http.Request) int64 {
	if raw := r.URL.Query().Get("after_sequence"); raw != "" {
		if n, err := strconv.ParseInt(raw, 10, 64); err == nil && n >= 0 {
			return n
		}
		return 0 // ponytail: lenient — a garbage cursor replays from the start, never errors
	}
	if last := r.Header.Get("Last-Event-ID"); last != "" {
		if seq, ok, err := h.reader.ResolveCursor(ctx, scope.Organization, scope.Project, sessionID, last); err == nil && ok {
			return seq
		}
	}
	return 0
}

// pump is the replay-then-tail loop. It holds at most one journal batch in memory,
// so a slow or stalled consumer cannot grow server memory: the write deadline drops
// it and the durable journal replays on reconnect.
func (h *eventsHandler) pump(ctx context.Context, w http.ResponseWriter, rc *http.ResponseController, scope middleware.Scope, sessionID string, cursor int64) {
	poll := time.NewTicker(h.cfg.PollInterval)
	defer poll.Stop()
	lastWrite := time.Now()

	for {
		batch, err := h.reader.After(ctx, scope.Organization, scope.Project, sessionID, cursor, h.cfg.BatchLimit)
		if err != nil {
			return // ctx canceled (client gone) or read error — the journal keeps the events
		}
		for i := range batch {
			frame, err := batch[i].MarshalSSE()
			if err != nil {
				return
			}
			if !h.write(rc, w, frame) {
				return // slow consumer hit the write deadline, or the client disconnected
			}
			cursor = int64(batch[i].Sequence)
			lastWrite = time.Now()
			if terminalEventTypes[batch[i].Type] {
				return // clean close after the terminal event
			}
		}
		if len(batch) == h.cfg.BatchLimit {
			continue // journal has more buffered — drain before waiting
		}
		select {
		case <-ctx.Done():
			return
		case <-poll.C:
			if time.Since(lastWrite) >= h.cfg.Heartbeat {
				if !h.write(rc, w, []byte(": heartbeat\n\n")) {
					return
				}
				lastWrite = time.Now()
			}
		}
	}
}

// write flushes one frame under a fresh write deadline. A consumer too slow to
// drain within the deadline trips it, so the write fails and the connection is
// dropped rather than buffering unboundedly.
func (h *eventsHandler) write(rc *http.ResponseController, w http.ResponseWriter, frame []byte) bool {
	_ = rc.SetWriteDeadline(time.Now().Add(h.cfg.WriteTimeout))
	if _, err := w.Write(frame); err != nil {
		return false
	}
	return rc.Flush() == nil
}
