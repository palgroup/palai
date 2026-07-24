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
	"time"
)

// ErrRateLimited is returned when Slack keeps returning 429 after the bounded repair budget is spent. It is
// a DELIVERY failure only: the canonical run result the message was rendering is untouched (§SLK-006 — a
// Slack failure never erases the canonical result), so the caller surfaces the delivery failure without
// discarding the run's outcome.
var ErrRateLimited = errors.New("slack: rate limited after repair budget exhausted")

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
			return result, fmt.Errorf("slack: chat post failed: %s", apiErr)
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
