package palai

import (
	"context"
	"io"
	"net/http"
	"strconv"
)

// Sessions.Events is the durable spine a long-lived relay (e.g. a Slack bot) reads a run's progress
// from: it attaches to an EXISTING session's event stream and survives being restarted, because
// resume is keyed on a plain integer sequence the caller can persist itself, not on in-memory state.
//
// It deliberately reuses stream.go's framer (scanSSE, decodeEvent, IsTerminalEvent) and reconnect
// backoff (fullJitterBackoff, sleep) rather than parsing SSE a second time. It does NOT reuse
// ResponseStream's Last-Event-ID resume: the wire contract measured against the live control plane
// (apps/control-plane/api/events.go:111-112) resolves `?after_sequence=<n>` first, and the store's
// After() read is `seq > afterSequence` (storage/queries/events.sql) — strictly greater-than — so
// resuming with the last SEQUENCE actually seen is exactly correct on its own: no dedup-the-boundary
// step is needed the way ResponseStream needs one for an id-keyed resume.

// EventsParams resumes a session's event stream after the given sequence. Zero (the default) is not
// a special case to branch on — the store's own semantics make it "replay everything", since no real
// sequence is <= 0 (measured: a fresh connection to an idle session replayed from the start).
type EventsParams struct {
	AfterSequence int64
}

// sessionStreamItem is one delivery over SessionEventStream's internal channel. err is set on at
// most the FINAL item: a clean stop (a terminal event was already delivered as its own item with
// err=nil) sends no item at all, just a channel close, so Next() reports it as io.EOF.
type sessionStreamItem struct {
	event Event
	err   error
}

// SessionEventStream is a resumable, pull-based consumer of a session's event stream (Next/Close) —
// the shape a synchronous relay loop wants, unlike ResponseStream's range-over-func iterator. The
// connection is read on its own goroutine (run) so a caller who stops calling Next(), or calls
// Close(), cannot deadlock a blocked delivery: every send to items races a ctx cancellation.
type SessionEventStream struct {
	client    *Client
	sessionID string
	cursor    int64 // last sequence delivered (or the caller's starting point); read/written only by run

	items  chan sessionStreamItem
	cancel context.CancelFunc
	done   chan struct{} // closed when run() returns, so Close() can wait out in-flight cleanup

	maxReconnects int
	backoffBaseMs int
	backoffMaxMs  int
}

// Events attaches to sessionID's event stream, resuming from params.AfterSequence. The first
// connection is opened SYNCHRONOUSLY — a caller sees a connect error (an unknown session's 404, a
// bad key's 401) immediately, not on the first Next() — after which delivery moves to a background
// goroutine that keeps reconnecting (full-jitter backoff) until a terminal event, Close(), or an
// exhausted reconnect budget.
//
// opts: WithRequestTimeout bounds the WHOLE stream's lifetime (not a single request — matching what
// "timeout" means for doJSON's retry-inclusive deadline); WithRequestMaxRetries overrides the
// reconnect budget (default 5). WithIdempotencyKey has no effect — this call never writes anything.
func (s *Sessions) Events(ctx context.Context, sessionID string, params EventsParams, opts ...CallOption) (*SessionEventStream, error) {
	o := requestOptions{}
	for _, opt := range opts {
		opt(&o)
	}
	maxReconnects := 5
	if o.maxRetries != nil {
		maxReconnects = *o.maxRetries
	}
	var streamCtx context.Context
	var cancel context.CancelFunc
	if o.timeout != nil {
		streamCtx, cancel = context.WithTimeout(ctx, *o.timeout)
	} else {
		streamCtx, cancel = context.WithCancel(ctx)
	}

	resp, err := s.client.openSessionEventStream(streamCtx, sessionID, params.AfterSequence)
	if err != nil {
		cancel()
		return nil, err
	}
	if resp.StatusCode/100 != 2 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, maxSSELineBytes))
		resp.Body.Close()
		cancel()
		return nil, errorForResponse(resp.StatusCode, string(body), resp.Header.Get("Request-Id"))
	}

	st := &SessionEventStream{
		client:        s.client,
		sessionID:     sessionID,
		cursor:        params.AfterSequence,
		items:         make(chan sessionStreamItem),
		cancel:        cancel,
		done:          make(chan struct{}),
		maxReconnects: maxReconnects,
		backoffBaseMs: 100,
		backoffMaxMs:  5_000,
	}
	go st.run(streamCtx, resp)
	return st, nil
}

// Next blocks for the next event. Three outcomes: (event, nil) for each delivered event, including
// the terminal one; (zero Event, io.EOF) once the stream has cleanly ended (the terminal event was
// already returned by a prior Next()); or (zero Event, some other error) — an exhausted reconnect
// budget or a non-2xx on a reconnect attempt — which also ends the stream.
func (s *SessionEventStream) Next() (Event, error) {
	item, ok := <-s.items
	if !ok {
		return Event{}, io.EOF
	}
	return item.event, item.err
}

// Close stops the stream and waits for its connection to be released. Safe to call more than once,
// and safe to call after the stream already ended on its own.
func (s *SessionEventStream) Close() error {
	s.cancel()
	<-s.done
	return nil
}

// run owns the single goroutine that reads scanSSE's callback stream and relays each decoded event
// to items, reconnecting on a clean drop (EOF or a read error, neither of which is a terminal event)
// with full-jitter backoff, resuming each reconnect from s.cursor. first is the already-open
// connection Events() opened synchronously.
func (s *SessionEventStream) run(ctx context.Context, first *http.Response) {
	defer close(s.done)
	defer close(s.items)

	resp := first
	reconnects := 0
	for {
		if resp == nil {
			opened, err := s.client.openSessionEventStream(ctx, s.sessionID, s.cursor)
			if err != nil {
				if ctx.Err() != nil {
					return
				}
				if reconnects >= s.maxReconnects {
					s.emit(ctx, sessionStreamItem{err: &ConnectionError{Message: "session event stream could not be (re)opened", Cause: err}})
					return
				}
				if sleep(ctx, fullJitterBackoff(reconnects, s.backoffBaseMs, s.backoffMaxMs)) != nil {
					return
				}
				reconnects++
				continue
			}
			if opened.StatusCode/100 != 2 {
				body, _ := io.ReadAll(io.LimitReader(opened.Body, maxSSELineBytes))
				opened.Body.Close()
				s.emit(ctx, sessionStreamItem{err: errorForResponse(opened.StatusCode, string(body), opened.Header.Get("Request-Id"))})
				return
			}
			resp = opened
		}

		terminal, stopped := s.consume(ctx, resp)
		resp = nil // this connection is spent either way; a next iteration (re)opens
		if terminal || stopped || ctx.Err() != nil {
			return
		}
		if reconnects >= s.maxReconnects {
			s.emit(ctx, sessionStreamItem{err: &ConnectionError{Message: "session event stream dropped before a terminal event and exhausted reconnects"}})
			return
		}
		if sleep(ctx, fullJitterBackoff(reconnects, s.backoffBaseMs, s.backoffMaxMs)) != nil {
			return
		}
		reconnects++
	}
}

// consume drains one connection's SSE frames until a terminal event (terminal=true), the caller
// stops reading (stopped=true, via emit's ctx race), or the connection simply ends (both false — a
// clean EOF or a read error, the two cases run treats identically: reconnect). It always closes resp
// Body before returning.
func (s *SessionEventStream) consume(ctx context.Context, resp *http.Response) (terminal, stopped bool) {
	defer resp.Body.Close()
	_ = scanSSE(resp.Body, func(f SSEFrame) bool {
		if f.Data == "" {
			return true
		}
		e, ok := decodeEvent(f.Data)
		if !ok {
			return true
		}
		s.cursor = int64(e.Sequence)
		if !s.emit(ctx, sessionStreamItem{event: e}) {
			stopped = true
			return false
		}
		if IsTerminalEvent(e) {
			terminal = true
			return false
		}
		return true
	})
	return terminal, stopped
}

// emit sends item on items, or reports false if ctx ends first — the escape hatch that keeps a
// blocked send from deadlocking against a caller who stopped calling Next() (e.g. after Close()).
func (s *SessionEventStream) emit(ctx context.Context, item sessionStreamItem) bool {
	select {
	case s.items <- item:
		return true
	case <-ctx.Done():
		return false
	}
}

// openSessionEventStream opens the raw SSE response for a session, resuming via the measured
// ?after_sequence=<n> query param (apps/control-plane/api/events.go:112) rather than
// openEventStream's Last-Event-ID header — the numeric cursor this stream tracks natively, with no
// id-to-sequence resolution round trip server-side. It does not retry; SessionEventStream.run owns
// reconnection.
func (c *Client) openSessionEventStream(ctx context.Context, sessionID string, afterSequence int64) (*http.Response, error) {
	url := c.baseURL + "/v1/sessions/" + escapePathSegment(sessionID) + "/events?after_sequence=" + strconv.FormatInt(afterSequence, 10)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+c.apiKey)
	req.Header.Set("API-Version", APIVersion)
	req.Header.Set("Accept", "text/event-stream")
	req.Header.Set("Cache-Control", "no-store")
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, &ConnectionError{Message: "GET " + url + " failed to reach the server", Cause: err}
	}
	return resp, nil
}
