package slack

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

// fakeSlack is a scripted Slack Web API peer: it returns the queued responses in order, so a test drives a
// 429→200 repair without a real network.
type fakeSlack struct {
	responses []*http.Response
	calls     int
}

func (f *fakeSlack) Do(*http.Request) (*http.Response, error) {
	r := f.responses[f.calls]
	f.calls++
	return r, nil
}

func resp(status int, retryAfter, body string) *http.Response {
	h := http.Header{}
	if retryAfter != "" {
		h.Set("Retry-After", retryAfter)
	}
	return &http.Response{StatusCode: status, Header: h, Body: io.NopCloser(strings.NewReader(body))}
}

// recorder captures the honored wait so the test asserts the Retry-After was respected without sleeping.
type recorder struct{ waited []time.Duration }

func (r *recorder) wait(_ context.Context, d time.Duration) error {
	r.waited = append(r.waited, d)
	return nil
}

func TestPostMessageRepairsA429Once(t *testing.T) {
	peer := &fakeSlack{responses: []*http.Response{
		resp(http.StatusTooManyRequests, "2", ""),
		resp(http.StatusOK, "", `{"ok":true,"ts":"123.456"}`),
	}}
	rec := &recorder{}
	out, err := PostMessage(context.Background(), peer,
		PostRequest{MethodURL: "https://slack.test/api/chat.postMessage", Token: []byte("xoxb-secret"), Body: []byte(`{}`)},
		PostOptions{Wait: rec.wait})
	if err != nil {
		t.Fatalf("PostMessage error = %v", err)
	}
	if !out.Repaired || out.MessageTS != "123.456" || out.Attempts != 2 {
		t.Fatalf("result = %+v, want repaired ts=123.456 attempts=2", out)
	}
	if len(rec.waited) != 1 || rec.waited[0] != 2*time.Second {
		t.Fatalf("honored waits = %v, want exactly one 2s Retry-After", rec.waited)
	}
}

// The repair is bounded: a persistent 429 past the budget returns ErrRateLimited (a delivery failure), and
// the adapter does NOT keep hammering Slack. The canonical result is the caller's to keep.
func TestPostMessageBoundedRepairThenRateLimited(t *testing.T) {
	peer := &fakeSlack{responses: []*http.Response{
		resp(http.StatusTooManyRequests, "1", ""),
		resp(http.StatusTooManyRequests, "1", ""),
	}}
	rec := &recorder{}
	_, err := PostMessage(context.Background(), peer,
		PostRequest{MethodURL: "https://slack.test/api/chat.postMessage", Body: []byte(`{}`)},
		PostOptions{MaxRepairs: 1, Wait: rec.wait})
	if !errors.Is(err, ErrRateLimited) {
		t.Fatalf("err = %v, want ErrRateLimited after the repair budget", err)
	}
	if peer.calls != 2 {
		t.Fatalf("made %d calls, want exactly 2 (one initial + one repair, then stop)", peer.calls)
	}
}

func TestPostMessageClampsAHostileRetryAfter(t *testing.T) {
	peer := &fakeSlack{responses: []*http.Response{
		resp(http.StatusTooManyRequests, "999999", ""),
		resp(http.StatusOK, "", `{"ok":true,"ts":"9.9"}`),
	}}
	rec := &recorder{}
	if _, err := PostMessage(context.Background(), peer,
		PostRequest{MethodURL: "https://slack.test/api/chat.postMessage", Body: []byte(`{}`)},
		PostOptions{MaxWait: 5 * time.Second, Wait: rec.wait}); err != nil {
		t.Fatalf("PostMessage error = %v", err)
	}
	if len(rec.waited) != 1 || rec.waited[0] != 5*time.Second {
		t.Fatalf("waited %v, want the Retry-After clamped to MaxWait=5s", rec.waited)
	}
}

func TestPostMessageSurfacesAnAPIError(t *testing.T) {
	peer := &fakeSlack{responses: []*http.Response{resp(http.StatusOK, "", `{"ok":false,"error":"channel_not_found"}`)}}
	_, err := PostMessage(context.Background(), peer,
		PostRequest{MethodURL: "https://slack.test/api/chat.postMessage", Body: []byte(`{}`)}, PostOptions{})
	if err == nil || !strings.Contains(err.Error(), "channel_not_found") {
		t.Fatalf("err = %v, want a surfaced channel_not_found", err)
	}
}
