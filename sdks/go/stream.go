package palai

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"math/rand"
	"net/http"
	"regexp"
	"strings"
	"time"
)

// The streaming layer: a WHATWG SSE frame parser mirroring the TS SDK's parseEventStream (and the
// conformance harness's reference framer) exactly, plus ResponseStream — a resumable, typed
// consumer that reconnects from Last-Event-ID on a drop and stops on a terminal event.

// maxSSELineBytes caps a single SSE line, mirroring provider_one's 1 MiB frame ceiling SDK-side so
// a hostile or runaway stream cannot exhaust memory on one unbounded line.
const maxSSELineBytes = 1 << 20

// SSEFrame is a parsed Server-Sent Events frame. Data is the joined data lines; ID updates the
// reconnection cursor; Event is the event name the server set.
type SSEFrame struct {
	ID    string
	Event string
	Data  string
}

// scanSSE frames an SSE byte stream, invoking fn for each dispatched frame (blank-line boundary),
// stopping early if fn returns false. It tolerates CRLF, skips comment lines (a leading colon),
// joins multi-line data fields with "\n", strips one optional space after a field colon, and caps
// a single line at maxSSELineBytes. It returns the first read error (nil at clean EOF).
//
// A trailing frame not terminated by a blank line is DISCARDED at EOF, mirroring parseEventStream,
// the harness reference framer, and WHATWG: a graceful close mid-frame (e.g. a Caddy-edge idle FIN)
// must not dispatch a truncated event, or a resume would advance Last-Event-ID past an event that
// was never delivered.
func scanSSE(r io.Reader, fn func(SSEFrame) bool) error {
	sc := bufio.NewScanner(r)
	// bufio.ScanLines splits on '\n' and strips a trailing '\r', so CRLF is tolerated. The buffer
	// cap is the 1 MiB frame ceiling: a line past it surfaces as bufio.ErrTooLong, not OOM.
	sc.Buffer(make([]byte, 0, 64*1024), maxSSELineBytes)
	var (
		frame   SSEFrame
		hasData bool
		hasAny  bool
	)
	for sc.Scan() {
		line := sc.Text()
		if line == "" { // blank line dispatches the buffered frame
			if hasAny && !fn(frame) {
				return nil
			}
			frame, hasData, hasAny = SSEFrame{}, false, false
			continue
		}
		hasAny = true
		applySSEField(&frame, &hasData, line)
	}
	return sc.Err()
}

// applySSEField parses one SSE line into the current frame. A leading colon is a comment; a field
// is `name` or `name:value` with one optional space stripped after the colon.
func applySSEField(frame *SSEFrame, hasData *bool, line string) {
	if strings.HasPrefix(line, ":") {
		return // comment / heartbeat
	}
	field, value := line, ""
	if colon := strings.IndexByte(line, ':'); colon != -1 {
		field, value = line[:colon], strings.TrimPrefix(line[colon+1:], " ")
	}
	switch field {
	case "id":
		frame.ID = value
	case "event":
		frame.Event = value
	case "data":
		if *hasData {
			frame.Data += "\n" + value
		} else {
			frame.Data = value
			*hasData = true
		}
	default:
		// retry: and unknown fields are not used by this consumer.
	}
}

var terminalEventRe = regexp.MustCompile(`^(run|response)\.(completed|failed|canceled|timed_out|budget_exceeded)\.v[0-9]+$`)

// IsTerminalEvent reports whether an event closes the run, so the stream stops rather than
// reconnecting (spec §22.3, §24.4).
func IsTerminalEvent(e Event) bool {
	return terminalEventRe.MatchString(e.Type)
}

// ScanEvents frames an SSE byte stream and delivers each decoded canonical Event to fn (unknown
// event types and fields preserved; comments, heartbeats, and non-JSON data lines skipped), stopping
// early if fn returns false. It is the low-level parser ResponseStream builds on, exported for a
// caller that holds a raw event-stream body directly.
func ScanEvents(r io.Reader, fn func(Event) bool) error {
	return scanSSE(r, func(f SSEFrame) bool {
		if f.Data == "" {
			return true
		}
		e, ok := decodeEvent(f.Data)
		if !ok {
			return true
		}
		return fn(e)
	})
}

// decodeEvent decodes an SSE data line into a canonical Event iff it is a JSON object with a
// string `type`; otherwise it returns ok=false (a non-JSON heartbeat or plain line).
func decodeEvent(data string) (Event, bool) {
	var probe struct {
		Type *string `json:"type"`
	}
	if json.Unmarshal([]byte(data), &probe) != nil || probe.Type == nil {
		return Event{}, false
	}
	var e Event
	if json.Unmarshal([]byte(data), &e) != nil {
		return Event{}, false
	}
	return e, true
}

// fullJitterBackoff returns a delay in [0, min(maxMs, baseMs*2^attempt)] — the AWS "full jitter"
// schedule, which spreads retries so many clients do not synchronize their reconnect storms
// (spec §23.7). attempt is 0-based.
func fullJitterBackoff(attempt, baseMs, maxMs int) time.Duration {
	if baseMs <= 0 || attempt < 0 {
		return 0
	}
	ceiling := maxMs
	if exp := baseMs << attempt; attempt < 31 && exp < ceiling {
		ceiling = exp
	}
	return time.Duration(rand.Intn(ceiling+1)) * time.Millisecond
}

// sleep waits d or returns ctx.Err() if the context ends first, so a backoff is cancelable.
func sleep(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return ctx.Err()
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// streamTransport is the seam a shared reconnect/backoff/terminal engine (runStream) is built on.
// Only the connect strategy and per-frame cursor bookkeeping differ between ResponseStream
// (Last-Event-ID header + a dedupe check on the resume boundary, see responseStreamTransport in this
// file) and SessionEventStream (?after_sequence, no dedupe needed — the server's read is
// strictly-greater-than, see sessionStreamTransport in sessionevents.go). Everything else — backoff,
// terminal handling, non-2xx and exhausted-reconnect errors — is identical and lives once in
// runStream.
type streamTransport interface {
	// open connects (or reconnects), resuming from whatever cursor state the transport holds.
	open(ctx context.Context) (*http.Response, error)
	// processFrame turns one raw SSE frame into a decoded Event ready to deliver (ok=false for a
	// non-JSON/heartbeat frame or a dedup boundary), updating the transport's own resume cursor as a
	// side effect. It is called for EVERY dispatched frame, decoded or not, so an id-only frame still
	// advances an id-keyed cursor.
	processFrame(f SSEFrame) (Event, bool)
}

// runStream is the reconnect/backoff/terminal engine shared by ResponseStream.Events and
// SessionEventStream.run: open a connection (or reuse first, an already-open one a caller needing a
// synchronous connect error opened itself — nil for a caller with no such requirement), scan it with
// scanSSE, deliver each decoded event, stop cleanly on a terminal event or a deliver-requested stop,
// and otherwise reconnect with full-jitter backoff (bounded by maxReconnects) — on an open failure, a
// non-2xx open (immediate, no retry), or the connection simply ending (a clean EOF or a read error;
// scanSSE's returned error is not distinguished between the two, matching both streams' original
// behavior pre-extraction).
func runStream(ctx context.Context, t streamTransport, first *http.Response, maxReconnects, backoffBaseMs, backoffMaxMs int, deliver func(Event, error) bool) {
	reconnects := 0
	resp := first
	for {
		if resp == nil {
			opened, err := t.open(ctx)
			if err != nil {
				if ctx.Err() != nil {
					return
				}
				if reconnects >= maxReconnects {
					deliver(Event{}, &ConnectionError{Message: "event stream could not be (re)opened", Cause: err})
					return
				}
				if sleep(ctx, fullJitterBackoff(reconnects, backoffBaseMs, backoffMaxMs)) != nil {
					return
				}
				reconnects++
				continue
			}
			if opened.StatusCode/100 != 2 {
				body, _ := io.ReadAll(io.LimitReader(opened.Body, maxSSELineBytes))
				opened.Body.Close()
				deliver(Event{}, errorForResponse(opened.StatusCode, string(body), opened.Header.Get("Request-Id")))
				return
			}
			resp = opened
		}

		terminal, stopped := false, false
		_ = scanSSE(resp.Body, func(f SSEFrame) bool {
			e, ok := t.processFrame(f)
			if !ok {
				return true
			}
			if !deliver(e, nil) {
				stopped = true
				return false
			}
			if IsTerminalEvent(e) {
				terminal = true
				return false
			}
			return true
		})
		resp.Body.Close()
		resp = nil // this connection is spent either way; a next iteration (re)opens
		if terminal || stopped || ctx.Err() != nil {
			return
		}
		if reconnects >= maxReconnects {
			deliver(Event{}, &ConnectionError{Message: "event stream dropped before a terminal event and exhausted reconnects"})
			return
		}
		if sleep(ctx, fullJitterBackoff(reconnects, backoffBaseMs, backoffMaxMs)) != nil {
			return
		}
		reconnects++
	}
}

// ResponseStream is a resumable, typed consumer of a run's event stream. Iterating it via Events
// yields each canonical Event (unknown event types are delivered, not dropped: API-009). A
// transport drop before a terminal event reconnects from the last seen id via Last-Event-ID with
// full-jitter backoff; a terminal event stops without reconnecting.
type ResponseStream struct {
	client        *Client
	responseID    string
	sessionID     string
	lastEventID   string
	maxReconnects int
	backoffBaseMs int
	backoffMaxMs  int
}

// LastEventID exposes the reconnection cursor reached so far, so a caller can persist and resume.
func (s *ResponseStream) LastEventID() string { return s.lastEventID }

// ResponseID / SessionID identify the run this stream follows.
func (s *ResponseStream) ResponseID() string { return s.responseID }
func (s *ResponseStream) SessionID() string  { return s.sessionID }

// responseStreamTransport adapts ResponseStream to streamTransport: resume is the Last-Event-ID
// header (an evt_* id). A resumed connection's first decoded event is suppressed if its frame id
// equals the resume boundary, in case the server ever redelivers it.
type responseStreamTransport struct {
	stream        *ResponseStream
	resumedFrom   string
	dedupePending bool
}

func (t *responseStreamTransport) open(ctx context.Context) (*http.Response, error) {
	t.resumedFrom = t.stream.lastEventID
	t.dedupePending = t.resumedFrom != ""
	return t.stream.client.openEventStream(ctx, t.stream.sessionID, t.resumedFrom)
}

func (t *responseStreamTransport) processFrame(f SSEFrame) (Event, bool) {
	if f.ID != "" {
		t.stream.lastEventID = f.ID
	}
	if f.Data == "" {
		return Event{}, false
	}
	e, ok := decodeEvent(f.Data)
	if !ok {
		return Event{}, false
	}
	if t.dedupePending {
		t.dedupePending = false
		if f.ID != "" && f.ID == t.resumedFrom {
			return Event{}, false // duplicate of the resume boundary
		}
	}
	return e, true
}

// Events returns a range-over-func iterator over the run's events. A non-nil error is yielded once,
// last, when the stream fails terminally (a typed APIError on a status error, a ConnectionError on
// an exhausted reconnect). Breaking out of the range closes the transport.
func (s *ResponseStream) Events(ctx context.Context) func(yield func(Event, error) bool) {
	return func(yield func(Event, error) bool) {
		runStream(ctx, &responseStreamTransport{stream: s}, nil, s.maxReconnects, s.backoffBaseMs, s.backoffMaxMs, yield)
	}
}

// FinalResponse drains the stream to its terminal event, then returns the canonical terminal
// Response (a subsequent authenticated retrieve). It shares the single underlying iteration.
func (s *ResponseStream) FinalResponse(ctx context.Context) (*Response, error) {
	var streamErr error
	for _, err := range s.Events(ctx) {
		if err != nil {
			streamErr = err
			break
		}
	}
	if streamErr != nil {
		return nil, streamErr
	}
	return s.client.Responses.Retrieve(ctx, s.responseID)
}

// openEventStream opens the raw SSE response for a session with Bearer auth and, on a resume, the
// Last-Event-ID cursor. It does not retry — ResponseStream owns reconnection.
func (c *Client) openEventStream(ctx context.Context, sessionID, lastEventID string) (*http.Response, error) {
	url := c.baseURL + "/v1/sessions/" + escapePathSegment(sessionID) + "/events"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+c.apiKey)
	req.Header.Set("API-Version", APIVersion)
	req.Header.Set("Accept", "text/event-stream")
	req.Header.Set("Cache-Control", "no-store")
	if lastEventID != "" {
		req.Header.Set("Last-Event-ID", lastEventID)
	}
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, &ConnectionError{Message: "GET " + url + " failed to reach the server", Cause: err}
	}
	return resp, nil
}
