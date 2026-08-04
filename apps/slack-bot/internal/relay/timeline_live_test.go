//go:build live

// WHAT A HUMAN SEES WHILE THE BOT IS STILL WORKING — measured by reading the thread DURING the run, not
// only after it.
//
// WHY A SEPARATE LEG: every other piece of evidence in this package reads conversations.replies ONCE, at
// the end, and a finished message cannot answer the only question the operator asked ("çalışırken hiçbir
// düşünüyor hâli göremedim"). A run whose steps all land in the last two seconds and a run that narrates
// itself for a minute leave the SAME final message. So this one samples the thread on a timer while
// relay.Run is still draining, and prints the samples in order.
//
// It drives the REAL relay over a REAL session's events into a REAL Slack thread. Nothing here fabricates
// an event.
package relay

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/palgroup/palai/adapters/integrations/slack"
	palai "github.com/palgroup/palai/sdks/go"
)

func tlEnv(t *testing.T, key string) string {
	t.Helper()
	v := os.Getenv(key)
	if v == "" {
		t.Skipf("%s is unset — this leg needs a running control plane and real Slack credentials", key)
	}
	return v
}

// TestWhatTheThreadShowsWhileTheRunIsStillWorking samples the live message and reports the transitions.
func TestWhatTheThreadShowsWhileTheRunIsStillWorking(t *testing.T) {
	apiURL := tlEnv(t, "PALAI_API_URL")
	apiKey := tlEnv(t, "PALAI_API_KEY")
	token := []byte(tlEnv(t, "SLACK_BOT_TOKEN"))
	channel := tlEnv(t, "SLACK_TEST_CHANNEL")
	teamID := tlEnv(t, "SLACK_TEAM_ID")
	userID := tlEnv(t, "SLACK_RECIPIENT_USER_ID")
	agentRevision := tlEnv(t, "PALAI_AGENT_REVISION_ID")
	bindingID := tlEnv(t, "PALAI_REPOSITORY_BINDING_ID")
	prompt := os.Getenv("PALAI_LIVE_PROMPT")
	if prompt == "" {
		prompt = "bu projede kaç tane Swift dosyası var, ve ana ekran hangi dosyada?"
	}

	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Minute)
	defer cancel()

	client, err := palai.New(palai.WithBaseURL(apiURL), palai.WithAPIKey(apiKey))
	if err != nil {
		t.Fatalf("palai client: %v", err)
	}
	session, err := client.Sessions.Create(ctx, palai.CreateSessionParams{Name: "slack thinking-ui live sample"})
	if err != nil {
		t.Fatalf("create session: %v", err)
	}
	parent, err := tlPost(ctx, token, "https://slack.com/api/chat.postMessage", map[string]any{
		"channel": channel, "text": "live sample: " + prompt,
	})
	if err != nil {
		t.Fatalf("post parent: %v", err)
	}
	threadTS, _ := parent["ts"].(string)
	t.Logf("session=%s thread=%s", session.ID, threadTS)

	// assistant.threads.setStatus, MEASURED in a public channel thread rather than assumed from the page.
	// https://docs.slack.dev/reference/methods/assistant.threads.setStatus/ describes an "assistant thread"
	// and the surrounding guide is written for the assistant container; whether a channel thread is refused
	// is the thing this line settles.
	statusRes, statusErr := tlPost(ctx, token, "https://slack.com/api/assistant.threads.setStatus", map[string]any{
		"channel_id": channel, "thread_ts": threadTS, "status": "is thinking...",
	})
	t.Logf("assistant.threads.setStatus in a CHANNEL thread → res=%v err=%v", statusRes, statusErr)

	resp, err := client.Responses.Create(ctx, palai.ResponseCreateRequest{
		Input:           prompt,
		SessionID:       &session.ID,
		AgentRevisionID: &agentRevision,
		Repository:      map[string]any{"binding_id": bindingID},
	})
	if err != nil {
		t.Fatalf("create response: %v", err)
	}
	t.Logf("response=%s", resp.ID)

	events, err := client.Sessions.Events(ctx, session.ID, palai.EventsParams{})
	if err != nil {
		t.Fatalf("open session events: %v", err)
	}

	started := time.Now()
	stream := NewChannelSlackStreamer(http.DefaultClient, "https://slack.com/api", token)(userID, teamID)
	seen := &tlRecorder{inner: stream, started: started}

	// The sampler runs alongside Run and stops when it does.
	done := make(chan struct{})
	var samples []tlSample
	var mu sync.Mutex
	go func() {
		for {
			select {
			case <-done:
				return
			case <-time.After(1200 * time.Millisecond):
			}
			s := tlSnapshot(ctx, token, channel, threadTS, time.Since(started))
			mu.Lock()
			if len(samples) == 0 || samples[len(samples)-1].fingerprint != s.fingerprint {
				samples = append(samples, s)
			}
			mu.Unlock()
		}
	}()

	runErr := Run(ctx, Deps{Events: events, Slack: seen, OnApproval: func(context.Context, string, string, palai.Event) {}},
		session.ID, channel, threadTS)
	close(done)
	if runErr != nil {
		t.Fatalf("relay.Run: %v", runErr)
	}
	final := tlSnapshot(ctx, token, channel, threadTS, time.Since(started))

	t.Logf("=== WHAT THE RELAY SENT (%d call(s), t=elapsed since the stream opened) ===", len(seen.calls))
	for _, c := range seen.calls {
		t.Logf("  t+%6.1fs  %s", c.at.Seconds(), c.what)
	}
	mu.Lock()
	t.Logf("=== WHAT THE THREAD SHOWED, one line per CHANGE (%d distinct state(s)) ===", len(samples))
	for _, s := range samples {
		t.Logf("  t+%6.1fs  %s", s.at.Seconds(), s.fingerprint)
	}
	mu.Unlock()
	t.Logf("=== AFTER THE RUN ===\n%s", final.detail)
}

type tlCall struct {
	at   time.Duration
	what string
}

// tlRecorder passes every call to the real Slack seam and timestamps it, so the sent-vs-shown comparison
// is against the wire rather than against an expectation.
type tlRecorder struct {
	inner   Slack
	started time.Time
	mu      sync.Mutex
	calls   []tlCall
}

func (r *tlRecorder) note(format string, args ...any) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls = append(r.calls, tlCall{at: time.Since(r.started), what: fmt.Sprintf(format, args...)})
}

func (r *tlRecorder) StartStream(ctx context.Context, channel, threadTS, markdownText string) (string, error) {
	r.note("startStream %q", markdownText)
	return r.inner.StartStream(ctx, channel, threadTS, markdownText)
}

func (r *tlRecorder) AppendStream(ctx context.Context, channel, ts, markdownText string) error {
	r.note("appendStream text %q", tlClip(markdownText))
	return r.inner.AppendStream(ctx, channel, ts, markdownText)
}

func (r *tlRecorder) StopStream(ctx context.Context, channel, ts, markdownText string) error {
	r.note("stopStream %q", tlClip(markdownText))
	return r.inner.StopStream(ctx, channel, ts, markdownText)
}

func (r *tlRecorder) UpdateTask(ctx context.Context, channel, ts string, task slack.Task) error {
	r.note("task_update id=%s status=%-11s title=%q detail=%q", task.ID, task.Status, task.Title, tlClip(task.Detail))
	return r.inner.UpdateTask(ctx, channel, ts, task)
}

type tlSample struct {
	at time.Duration
	// fingerprint is a one-line summary that CHANGES exactly when what a reader sees changes, so the
	// sampler can drop duplicates and print a transition log rather than a wall of identical polls.
	fingerprint string
	detail      string
}

// tlSnapshot reads the bot's message in the thread and renders both a fingerprint and a full dump.
func tlSnapshot(ctx context.Context, token []byte, channel, threadTS string, at time.Duration) tlSample {
	replies, err := tlGet(ctx, token, "https://slack.com/api/conversations.replies?channel="+
		url.QueryEscape(channel)+"&ts="+url.QueryEscape(threadTS))
	if err != nil {
		return tlSample{at: at, fingerprint: "READ FAILED: " + err.Error()}
	}
	msgs, _ := replies["messages"].([]any)
	if len(msgs) < 2 {
		return tlSample{at: at, fingerprint: "(no reply in the thread yet)"}
	}
	msg, _ := msgs[1].(map[string]any)
	var fp, detail strings.Builder
	if _, streaming := msg["streaming_state"]; streaming {
		fmt.Fprintf(&fp, "[streaming_state=%v] ", msg["streaming_state"])
	}
	blocks, _ := msg["blocks"].([]any)
	for _, b := range blocks {
		blk, _ := b.(map[string]any)
		switch blk["type"] {
		case "task_card":
			fmt.Fprintf(&fp, "[card %v %q]", blk["status"], blk["title"])
			fmt.Fprintf(&detail, "  [CARD] status=%-11v title=%q details=%v\n", blk["status"], blk["title"], blk["details"])
		case "plan":
			fmt.Fprintf(&fp, "[plan %q]", blk["title"])
			fmt.Fprintf(&detail, "  [PLAN] %v\n", tlJSON(blk))
		case "rich_text":
			text := tlFlatten(blk["elements"])
			fmt.Fprintf(&fp, "[body %q]", tlClip(text))
			fmt.Fprintf(&detail, "  [BODY] %q\n", text)
		default:
			fmt.Fprintf(&fp, "[%v]", blk["type"])
			fmt.Fprintf(&detail, "  [%v] %v\n", blk["type"], tlJSON(blk))
		}
	}
	fmt.Fprintf(&detail, "  message.text: %q\n", msg["text"])
	return tlSample{at: at, fingerprint: fp.String(), detail: detail.String()}
}

func tlJSON(v any) string {
	b, _ := json.Marshal(v)
	return tlClip(string(b))
}

func tlClip(s string) string {
	s = strings.ReplaceAll(s, "\n", "\\n")
	if len([]rune(s)) > 110 {
		return string([]rune(s)[:110]) + "…"
	}
	return s
}

func tlFlatten(v any) string {
	var sb strings.Builder
	els, _ := v.([]any)
	for _, e := range els {
		el, _ := e.(map[string]any)
		if t, ok := el["text"].(string); ok {
			sb.WriteString(t)
		}
		if inner, ok := el["elements"]; ok {
			sb.WriteString(tlFlatten(inner))
		}
	}
	return sb.String()
}

func tlPost(ctx context.Context, token []byte, endpoint string, payload map[string]any) (map[string]any, error) {
	body, _ := json.Marshal(payload)
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(string(body)))
	req.Header.Set("Content-Type", "application/json; charset=utf-8")
	req.Header.Set("Authorization", "Bearer "+string(token))
	return tlDo(req)
}

func tlGet(ctx context.Context, token []byte, endpoint string) (map[string]any, error) {
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	req.Header.Set("Authorization", "Bearer "+string(token))
	return tlDo(req)
}

func tlDo(req *http.Request) (map[string]any, error) {
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()
	raw, _ := io.ReadAll(res.Body)
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("decode %q: %w", raw, err)
	}
	if ok, _ := out["ok"].(bool); !ok {
		return nil, fmt.Errorf("slack refused: %s", raw)
	}
	return out, nil
}
