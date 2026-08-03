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
// It runs on stream.go's shared runStream engine — the exact reconnect/backoff/terminal loop
// ResponseStream.Events uses — via sessionStreamTransport, so no control flow is duplicated between
// the two streams; only the connect strategy and cursor bookkeeping differ, which is what
// streamTransport exists to isolate. The wire contract measured against the live control plane
// (apps/control-plane/api/events.go:111-112) resolves `?after_sequence=<n>` first, and the store's
// After() read is `seq > afterSequence` (storage/queries/events.sql) — strictly greater-than — so
// sessionStreamTransport resumes with the last SEQUENCE actually seen and, unlike
// responseStreamTransport's Last-Event-ID resume, needs no dedup-the-boundary step.

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
// connection is read on its own goroutine (run, on the shared runStream engine — see stream.go) so a
// caller who stops calling Next(), or calls Close(), cannot deadlock a blocked delivery: every send
// to items races a ctx cancellation.
type SessionEventStream struct {
	client    *Client
	sessionID string

	items  chan sessionStreamItem
	cancel context.CancelFunc
	done   chan struct{} // closed when run() returns, so Close() can wait out in-flight cleanup

	maxReconnects int
	backoffBaseMs int
	backoffMaxMs  int
}

// sessionStreamTransport adapts SessionEventStream to streamTransport (stream.go): resume is the
// ?after_sequence query param, tracked as a plain integer cursor updated from each decoded event's
// own Sequence — no dedupe step is needed, unlike responseStreamTransport's Last-Event-ID resume,
// because the server's After() read is strictly-greater-than (storage/queries/events.sql:61): resuming
// from the last sequence actually seen can never redeliver it.
type sessionStreamTransport struct {
	client    *Client
	sessionID string
	cursor    int64
}

func (t *sessionStreamTransport) open(ctx context.Context) (*http.Response, error) {
	return t.client.openSessionEventStream(ctx, t.sessionID, t.cursor)
}

func (t *sessionStreamTransport) processFrame(f SSEFrame) (Event, bool) {
	if f.Data == "" {
		return Event{}, false
	}
	e, ok := decodeEvent(f.Data)
	if !ok {
		return Event{}, false
	}
	t.cursor = int64(e.Sequence)
	return e, true
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
		items:         make(chan sessionStreamItem),
		cancel:        cancel,
		done:          make(chan struct{}),
		maxReconnects: maxReconnects,
		backoffBaseMs: 100,
		backoffMaxMs:  5_000,
	}
	go st.run(streamCtx, resp, params.AfterSequence)
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

// run drives the shared runStream engine (stream.go) with a sessionStreamTransport, translating its
// deliver(Event, error) calls into sends on items. first is the already-open connection Events()
// opened synchronously; startCursor seeds the transport's resume cursor for any later reconnect.
func (s *SessionEventStream) run(ctx context.Context, first *http.Response, startCursor int64) {
	defer close(s.done)
	defer close(s.items)

	t := &sessionStreamTransport{client: s.client, sessionID: s.sessionID, cursor: startCursor}
	deliver := func(e Event, err error) bool {
		return s.emit(ctx, sessionStreamItem{event: e, err: err})
	}
	runStream(ctx, t, first, s.maxReconnects, s.backoffBaseMs, s.backoffMaxMs, deliver)
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
