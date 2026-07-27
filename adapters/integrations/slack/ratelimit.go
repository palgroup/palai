package slack

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"sync"
	"time"
)

// ErrRateLimited is returned when Slack keeps returning 429 after the bounded repair budget is spent. It is
// a DELIVERY failure only: the canonical run result the message was rendering is untouched (§SLK-006 — a
// Slack failure never erases the canonical result), so the caller surfaces the delivery failure without
// discarding the run's outcome.
var ErrRateLimited = errors.New("slack: rate limited after repair budget exhausted")

// APIError is Slack REFUSING a call it received and understood: HTTP 200 carrying {"ok":false,"error":…}.
// It is separate from a transport failure because the caller's correct answer differs — a
// `channel_not_found` will fail identically forever while a dialing failure will not — and E20 T1 needs one
// code in particular: `stopped_by_user` says the HUMAN stopped the stream, which stops appending and must
// not be mistaken for Slack being unreachable.
//
// CONTRACT: https://docs.slack.dev/reference/methods/chat.appendStream/ (checked 2026-07-27) documents
// `stopped_by_user` as "The streaming message was stopped by the user and no further appends are accepted".
type APIError struct{ Code string }

func (e *APIError) Error() string { return "slack: api refused the call: " + e.Code }

// CodeStoppedByUser is the append refusal the Slack UI's stop button produces. It says the STREAM stopped;
// it says nothing about the run, and it carries no authenticated actor, so it is not run control (plan §2).
const CodeStoppedByUser = "stopped_by_user"

// CodeInvalidThreadTS is Slack refusing a call because the thread it names is not one: in practice the root
// message was deleted between the event and the call. It is not a capability answer and must not be read as
// one — a workspace that refuses ONE thread this way has refused nothing about the method.
//
// CONTRACT: https://docs.slack.dev/reference/methods/assistant.threads.setStatus/ (checked 2026-07-27)
// documents it verbatim as "Error returned when given an invalid thread_ts". chat.startStream's own reference
// (checked the same day) does NOT list the code at all, and a real workspace returned it there anyway — see
// the E20 plan §3.5 divergence row.
const CodeInvalidThreadTS = "invalid_thread_ts"

// APIErrorCode returns Slack's own error string when err is an API refusal, and "" for anything else
// (transport failures, cancellations, the bounded-repair give-up). Callers switch on this rather than on
// substring matching an error message.
func APIErrorCode(err error) string {
	var apiErr *APIError
	if errors.As(err, &apiErr) {
		return apiErr.Code
	}
	return ""
}

// Doer is the minimal HTTP client PostMessage drives — *http.Client satisfies it; a fake in the test drives
// the 429→200 repair deterministically without a real network or a real sleep.
type Doer interface {
	Do(*http.Request) (*http.Response, error)
}

// PostRequest is one outbound Slack Web API call (chat.postMessage / chat.update). Token is the bot token
// bytes resolved from a secret_ref handle at call time — it rides only the Authorization header, never a
// log or an argv. MethodURL is the full endpoint (e.g. https://slack.com/api/chat.postMessage).
type PostRequest struct {
	MethodURL string
	Token     []byte
	Body      []byte
}

// PostResult reports the delivered message ts (the reconciliation handle a later single repair edits) and
// whether a 429 was repaired. Attempts counts HTTP round trips made.
type PostResult struct {
	MessageTS string
	Repaired  bool
	Attempts  int
}

// PostOptions bounds the repair. MaxRepairs caps how many 429 Retry-After waits are honored (default 1 — a
// single repair of the visible message, not an unbounded retry storm). MaxWait clamps one Retry-After so a
// hostile/huge value cannot pin the caller. Wait defaults to a real sleep; a test injects a recorder.
type PostOptions struct {
	MaxRepairs int
	MaxWait    time.Duration
	Wait       func(context.Context, time.Duration) error
}

func (o PostOptions) withDefaults() PostOptions {
	if o.MaxRepairs <= 0 {
		o.MaxRepairs = 1
	}
	if o.MaxWait <= 0 {
		o.MaxWait = 30 * time.Second
	}
	if o.Wait == nil {
		o.Wait = sleep
	}
	return o
}

// sleep is the default Wait: a context-cancelable pause.
func sleep(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// PostMessage posts to Slack, repairing a 429 by honoring Retry-After for a BOUNDED number of waits, then
// retrying the SAME body. It replays from the caller's canonical body (idempotent for chat.update against a
// known ts), so a rate-limited live update is repaired exactly once against the visible message rather than
// duplicating it. A persistent 429 past the budget is ErrRateLimited — a delivery failure that leaves the
// canonical result intact.
func PostMessage(ctx context.Context, doer Doer, req PostRequest, opts PostOptions) (PostResult, error) {
	opts = opts.withDefaults()
	var result PostResult
	for {
		httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, req.MethodURL, bytes.NewReader(req.Body))
		if err != nil {
			return result, fmt.Errorf("slack: build request: %w", err)
		}
		httpReq.Header.Set("Content-Type", "application/json; charset=utf-8")
		if len(req.Token) > 0 {
			httpReq.Header.Set("Authorization", "Bearer "+string(req.Token))
		}
		resp, err := doer.Do(httpReq)
		if err != nil {
			return result, fmt.Errorf("slack: post: %w", err)
		}
		result.Attempts++

		if resp.StatusCode == http.StatusTooManyRequests {
			delay := retryAfter(resp.Header.Get("Retry-After"), opts.MaxWait)
			_ = resp.Body.Close()
			if result.Attempts > opts.MaxRepairs {
				return result, ErrRateLimited // budget spent — do not spam Slack; canonical result stands
			}
			if err := opts.Wait(ctx, delay); err != nil {
				return result, err
			}
			result.Repaired = true
			continue
		}

		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		_ = resp.Body.Close()
		ts, ok, apiErr := decodeChatResponse(body)
		if !ok {
			// Wrapped rather than formatted in, so errors.As reaches the code (see APIErrorCode). The message
			// reads the same as it always did; what changed is that a caller can now branch on it.
			return result, fmt.Errorf("slack: chat post failed: %w", &APIError{Code: apiErr})
		}
		result.MessageTS = ts
		return result, nil
	}
}

// retryAfter parses Slack's Retry-After (seconds), clamped to max. A missing/garbled value falls back to
// one second so the repair still makes progress rather than busy-looping.
func retryAfter(header string, max time.Duration) time.Duration {
	secs, err := strconv.Atoi(header)
	if err != nil || secs < 0 {
		return time.Second
	}
	d := time.Duration(secs) * time.Second
	if d > max {
		return max
	}
	return d
}

// SpecialTierPerChannel is how fast we may write to ONE channel (E19 plan §3.5 row D10).
//
// CONTRACT: https://docs.slack.dev/apis/web-api/rate-limits/ (checked 2026-07-26) — chat.postMessage is NOT
// in a numbered tier; it is the Web API "Special Tier", and the page says it "generally allows posting one
// message per second per channel, while also maintaining a workspace-wide limit". Short bursts above that are
// allowed but come with "no guarantee that messages will be stored or displayed to users" — which for an
// approval message is the same as losing it. So the pacing is the documented steady rate, not the burst.
const SpecialTierPerChannel = time.Second

// ChannelPacer holds outbound writes to the documented per-channel rate. It is the missing half of the 429
// repair above: PostMessage recovers AFTER Slack refuses, while this keeps coalesced updates from asking to
// be refused in the first place — an update loop that edits a message twice in one instant is exactly the
// traffic the Special Tier exists to shed.
//
// Per CHANNEL, deliberately: the documented limit is per channel, and pacing globally would throttle
// unrelated conversations against a limit they are not near.
//
// ponytail: a map of next-allowed instants, never pruned. One entry per channel the deployment has ever
// posted to, a few dozen bytes each — a sweep is worth writing when a deployment posts to enough distinct
// channels for it to matter, and nothing in this tree is near that.
type ChannelPacer struct {
	mu   sync.Mutex
	next map[string]time.Time

	// Interval overrides SpecialTierPerChannel (zero uses it). Now/Sleep are the injectable clock a test
	// drives without a real pause.
	Interval time.Duration
	Now      func() time.Time
	Sleep    func(context.Context, time.Duration) error
}

// Wait blocks until this channel's next write is due, then reserves the following slot. A cancelled context
// returns its error and reserves nothing.
func (p *ChannelPacer) Wait(ctx context.Context, channel string) error {
	now, sleep, interval := time.Now, sleep, p.Interval
	if p.Now != nil {
		now = p.Now
	}
	if p.Sleep != nil {
		sleep = p.Sleep
	}
	if interval <= 0 {
		interval = SpecialTierPerChannel
	}

	p.mu.Lock()
	if p.next == nil {
		p.next = map[string]time.Time{}
	}
	due, delay := p.next[channel], time.Duration(0)
	if at := now(); due.After(at) {
		delay = due.Sub(at)
	}
	// The slot is reserved BEFORE the wait is served, so two concurrent writers to one channel queue behind
	// each other instead of both measuring the same "due" and both firing at once.
	p.next[channel] = now().Add(delay + interval)
	p.mu.Unlock()

	if delay <= 0 {
		return nil
	}
	if err := sleep(ctx, delay); err != nil {
		p.mu.Lock()
		delete(p.next, channel) // the write never happened; do not leave a phantom reservation behind
		p.mu.Unlock()
		return err
	}
	return nil
}

// decodeChatResponse reads Slack's Web API envelope {"ok":bool,"ts":string,"error":string}. ok=false carries
// the api error string (e.g. "channel_not_found") the caller surfaces; the ts is the reconciliation handle.
func decodeChatResponse(body []byte) (ts string, ok bool, apiErr string) {
	var env struct {
		OK    bool   `json:"ok"`
		TS    string `json:"ts"`
		Error string `json:"error"`
	}
	if err := json.Unmarshal(body, &env); err != nil {
		return "", false, "unparseable response"
	}
	if !env.OK {
		if env.Error == "" {
			env.Error = "unknown"
		}
		return "", false, env.Error
	}
	return env.TS, true, ""
}
