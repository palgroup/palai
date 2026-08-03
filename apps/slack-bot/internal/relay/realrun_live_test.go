//go:build live

// A REAL RUN, rendered by the REAL relay, into a REAL Slack thread.
//
// WHY THIS EXISTS: every earlier piece of evidence for the task timeline was a probe that FED ITS OWN
// events, and one of those events — tool_call.progress.v1 — is journalled ZERO times in this deployment
// (`SELECT count(*) FROM events WHERE type='tool_call.progress.v1'` → 0, 2026-08-04). Its only writer is
// the MCP progress sink (apps/control-plane/internal/execution/mcp_progress.go), so the built-in
// palai.workspace.* tools, which are dispatched by the control plane itself, cannot produce it. A probe
// that supplies one shows a card richer than any real run draws — the harness's property mistaken for the
// product's.
//
// So this drives the product: it creates a session and a response against the running control plane with
// the bot's own agent revision and repository binding, hands relay.Run the REAL event stream, and reads the
// finished message back with conversations.replies.
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
	"testing"
	"time"

	"github.com/palgroup/palai/adapters/integrations/slack"
	palai "github.com/palgroup/palai/sdks/go"
)

func liveEnv(t *testing.T, key string) string {
	t.Helper()
	v := os.Getenv(key)
	if v == "" {
		t.Skipf("%s is unset — this leg needs a running control plane and real Slack credentials", key)
	}
	return v
}

// TestARealRunRendersItsStepsAsCards is the whole point: no fabricated event reaches it.
func TestARealRunRendersItsStepsAsCards(t *testing.T) {
	apiURL := liveEnv(t, "PALAI_API_URL")
	apiKey := liveEnv(t, "PALAI_API_KEY")
	token := []byte(liveEnv(t, "SLACK_BOT_TOKEN"))
	channel := liveEnv(t, "SLACK_TEST_CHANNEL")
	teamID := liveEnv(t, "SLACK_TEAM_ID")
	userID := liveEnv(t, "SLACK_RECIPIENT_USER_ID")
	agentRevision := liveEnv(t, "PALAI_AGENT_REVISION_ID")
	bindingID := liveEnv(t, "PALAI_REPOSITORY_BINDING_ID")
	prompt := os.Getenv("PALAI_LIVE_PROMPT")
	if prompt == "" {
		prompt = "kısaca cevapla: bu projede kaç tane Swift dosyası var?"
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	client, err := palai.New(palai.WithBaseURL(apiURL), palai.WithAPIKey(apiKey))
	if err != nil {
		t.Fatalf("palai client: %v", err)
	}

	// The two-call turn inbound.go describes: a session, then the response that is the run.
	session, err := client.Sessions.Create(ctx, palai.CreateSessionParams{Name: "slack task-card live leg"})
	if err != nil {
		t.Fatalf("create session: %v", err)
	}
	resp, err := client.Responses.Create(ctx, palai.ResponseCreateRequest{
		Input:           prompt,
		SessionID:       &session.ID,
		AgentRevisionID: &agentRevision,
		Repository:      map[string]any{"binding_id": bindingID},
	})
	if err != nil {
		t.Fatalf("create response: %v", err)
	}
	t.Logf("session=%s response=%s", session.ID, resp.ID)

	// A thread for the answer to land in, standing in for the human's message.
	parent, err := slackCall(ctx, token, "https://slack.com/api/chat.postMessage", map[string]any{
		"channel": channel, "text": "live leg: " + prompt,
	})
	if err != nil {
		t.Fatalf("post parent: %v", err)
	}
	threadTS, _ := parent["ts"].(string)

	events, err := client.Sessions.Events(ctx, session.ID, palai.EventsParams{})
	if err != nil {
		t.Fatalf("open session events: %v", err)
	}

	// THE REAL WIRING, not a re-implementation of it.
	stream := NewChannelSlackStreamer(http.DefaultClient, "https://slack.com/api", token)(userID, teamID)
	seen := &recordingSlack{inner: stream}
	if err := Run(ctx, Deps{Events: events, Slack: seen, OnApproval: func(context.Context, string, string, palai.Event) {}},
		session.ID, channel, threadTS); err != nil {
		t.Fatalf("relay.Run: %v", err)
	}

	// WHAT THE PRODUCT ACTUALLY JOURNALLED, reported rather than assumed.
	t.Logf("the run drew %d card update(s):", len(seen.tasks))
	for _, task := range seen.tasks {
		t.Logf("  id=%s status=%-11s title=%q detail=%q", task.ID, task.Status, task.Title, task.Detail)
	}

	replies, err := slackGet(ctx, token, "https://slack.com/api/conversations.replies?channel="+
		url.QueryEscape(channel)+"&ts="+url.QueryEscape(threadTS))
	if err != nil {
		t.Fatalf("conversations.replies: %v", err)
	}
	msgs, _ := replies["messages"].([]any)
	if len(msgs) < 2 {
		t.Fatalf("the thread has %d message(s) — the relay wrote nothing back", len(msgs))
	}
	msg, _ := msgs[1].(map[string]any)
	blocks, _ := msg["blocks"].([]any)

	var cards, bodies int
	for _, b := range blocks {
		blk, _ := b.(map[string]any)
		switch blk["type"] {
		case "task_card":
			cards++
			t.Logf("  [CARD] status=%v title=%q details=%v", blk["status"], blk["title"], blk["details"])
			// The defect this whole change exists to prevent: a step left mid-flight in a finished message.
			if blk["status"] == "in_progress" || blk["status"] == "pending" {
				t.Errorf("a card is still %v in a FINISHED message — a terminal event did not resolve it", blk["status"])
			}
		case "rich_text":
			bodies++
			t.Logf("  [BODY] %s", flattenRich(blk["elements"]))
		}
	}
	t.Logf("message.text fallback: %q", msg["text"])

	// The SLK-P6 property, asserted against what the WORKSPACE kept rather than what was sent.
	body, _ := msg["text"].(string)
	for _, jargon := range []string{"palai.workspace.", "▶️", "✅", "Running `"} {
		if strings.Contains(body, jargon) {
			t.Errorf("the finished message body carries %q — progress must ride the task timeline", jargon)
		}
	}
	if cards == 0 {
		t.Logf("NOTE: this run made no tool calls, so it drew no cards — the body-only path")
	}
}

// recordingSlack passes every call through to the real Slack seam and keeps the task updates, so the test
// can report what the RUN produced rather than what a fixture supplied.
type recordingSlack struct {
	inner Slack
	tasks []slack.Task
}

func (r *recordingSlack) StartStream(ctx context.Context, channel, threadTS, markdownText string) (string, error) {
	return r.inner.StartStream(ctx, channel, threadTS, markdownText)
}

func (r *recordingSlack) AppendStream(ctx context.Context, channel, ts, markdownText string) error {
	return r.inner.AppendStream(ctx, channel, ts, markdownText)
}

func (r *recordingSlack) StopStream(ctx context.Context, channel, ts, markdownText string) error {
	return r.inner.StopStream(ctx, channel, ts, markdownText)
}

func (r *recordingSlack) UpdateTask(ctx context.Context, channel, ts string, task slack.Task) error {
	r.tasks = append(r.tasks, task)
	return r.inner.UpdateTask(ctx, channel, ts, task)
}

func flattenRich(v any) string {
	var sb strings.Builder
	els, _ := v.([]any)
	for _, e := range els {
		el, _ := e.(map[string]any)
		if t, ok := el["text"].(string); ok {
			sb.WriteString(t)
		}
		if inner, ok := el["elements"]; ok {
			sb.WriteString(flattenRich(inner))
		}
	}
	return sb.String()
}

func slackCall(ctx context.Context, token []byte, endpoint string, payload map[string]any) (map[string]any, error) {
	body, _ := json.Marshal(payload)
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(string(body)))
	req.Header.Set("Content-Type", "application/json; charset=utf-8")
	req.Header.Set("Authorization", "Bearer "+string(token))
	return slackDo(req)
}

func slackGet(ctx context.Context, token []byte, endpoint string) (map[string]any, error) {
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	req.Header.Set("Authorization", "Bearer "+string(token))
	return slackDo(req)
}

func slackDo(req *http.Request) (map[string]any, error) {
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
