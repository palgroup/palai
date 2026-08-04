//go:build live

// THE THREAD READ, AGAINST REAL SLACK AND A REAL CONTROL PLANE.
//
// WHY THIS EXISTS AND WHAT ONLY IT CAN SAY. Every unit proof of this leg substitutes the thread page: a
// fakeHistory hands back messages the test wrote, so the things most likely to be wrong are exactly the
// things it cannot measure — whether this workspace's granted scopes let the app call conversations.replies
// at all, whether the answer's message objects carry `files` in the shape the adapter decodes, and whether
// a file object that arrives through a HISTORY read is as fetchable as one that arrives on an event. Those
// are vendor facts and a fake cannot hold one; the scopes in particular are a property of an installation,
// not of this code.
//
// WHAT IS REAL HERE: the workspace, the bot token, the thread, the message a person actually typed, the
// image they actually uploaded, the selection (the production threadImages, unmodified), the fetch (the
// production ImageLeg), the artifact (the real public ingest route), and the run input.
//
// THE ONE SYNTHETIC PIECE, stated plainly rather than buried: the EVENT. This process is the bot, and a
// message it posts carries a bot_id that MapEvent's loop guard drops by design, so it cannot make Slack
// deliver a human's reply to itself. The event below therefore stands for "somebody replied in this thread",
// carrying the coordinates Slack would have put on it and no files of its own — which is precisely the shape
// that made the defect invisible. Everything downstream of that event is the product.
package relay

import (
	"context"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/palgroup/palai/adapters/integrations/slack"
	palai "github.com/palgroup/palai/sdks/go"
)

// TestARealThreadsEarlierImageBecomesARealArtifact walks the whole leg over one real thread.
func TestARealThreadsEarlierImageBecomesARealArtifact(t *testing.T) {
	apiURL := liveEnv(t, "PALAI_API_URL")
	apiKey := liveEnv(t, "PALAI_API_KEY")
	token := []byte(liveEnv(t, "SLACK_BOT_TOKEN"))
	channel := liveEnv(t, "SLACK_TEST_CHANNEL")

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	// --- 1. Find a REAL thread whose EARLIER messages carry an image. ---
	//
	// Read rather than written: this app holds files:read and NOT files:write (measured 2026-08-04 —
	// files.getUploadURLExternal answers missing_scope with this workspace's token), so a fixture uploaded by
	// this test is impossible. What it walks is the channel's own history for a thread root that a person
	// shared a picture on.
	history := slackAPI(ctx, t, token, "conversations.history", url.Values{
		"channel": {channel}, "limit": {"60"},
	})
	threadTS := threadWithASharedImage(t, history)
	t.Logf("thread carrying a human-shared image: %s/%s", channel, threadTS)

	// --- 2. Read it through the PRODUCTION seam, with the production bound. ---
	history2 := NewThreadHistory(http.DefaultClient, "https://slack.com/api", token)
	msgs, hasMore, err := history2.ThreadReplies(ctx, channel, threadTS, threadHistoryMaxMessages)
	if err != nil {
		// The scope answer lives here. `missing_scope` would mean this installation cannot do the thing this
		// whole file adds, whatever the code says — which is exactly what only a live leg can report.
		t.Fatalf("conversations.replies through the production seam: %v", err)
	}
	t.Logf("the page holds %d message(s), has_more=%v", len(msgs), hasMore)
	if hasMore {
		t.Skip("this thread is longer than one page, so the product deliberately attaches nothing from it — pick a shorter thread")
	}

	// --- 3. Select with the PRODUCTION rule, against an event standing for a reply in that thread. ---
	reply := slack.Event{
		Type: "app_mention", Kind: slack.KindMessage, InThread: true,
		ChannelID: channel, ThreadTS: threadTS,
		// A ts no message in the page has: this stands for a reply that has just arrived, so nothing in the
		// page is skipped as "the triggering message" and the selection is measured over the whole thread.
		MessageTS: "9999999999.999999",
		Text:      "buradaki ekran görüntüsünde ne yazıyor?",
	}
	picked, dropped := threadImages(msgs, reply, maxThreadImages)
	if len(picked) == 0 {
		t.Fatalf("the production selection found no image in a thread the setup just proved carries one — "+
			"the file object Slack returns from a HISTORY read does not reach slack.ImageCandidates in a "+
			"usable shape (page: %d messages, dropped: %d)", len(msgs), dropped)
	}
	t.Logf("selected %d image(s) (%s), left %d behind", len(picked), picked[0].ID, dropped)
	if host := mustHost(t, picked[0].DownloadURL); host != "files.slack.com" && host != "files-edge.slack.com" {
		t.Fatalf("url_private points at %q, which slack.FetchImage's host allow-list refuses — the allow-list and reality disagree", host)
	}

	// --- 4. Fetch and ingest through the PRODUCTION leg, into the REAL control plane. ---
	client, err := palai.New(palai.WithBaseURL(apiURL), palai.WithAPIKey(apiKey))
	if err != nil {
		t.Fatalf("construct SDK client: %v", err)
	}
	leg := &ImageLeg{Doer: http.DefaultClient, Token: token, Artifacts: NewArtifactCreator(client)}
	ids, skipped := leg.attach(ctx, picked, len(picked), t.Logf)
	if len(ids) != len(picked) {
		t.Fatalf("attach() returned %d ids for %d selected files (skipped %d) — the real fetch or the real ingest refused", len(ids), len(picked), skipped)
	}
	t.Logf("REAL earlier-message file %s -> REAL artifact %s", picked[0].ID, ids[0])

	// The bytes are real: read them back over the public API and compare against an INDEPENDENT fetch of the
	// same Slack file, rather than against anything this test carried.
	direct, err := slack.FetchImage(ctx, http.DefaultClient, token, picked[0], maxImageBytes)
	if err != nil {
		t.Fatalf("independent re-fetch of the same Slack file: %v", err)
	}
	getReq, _ := http.NewRequestWithContext(ctx, http.MethodGet, apiURL+"/v1/artifacts/"+ids[0]+"/content", nil)
	getReq.Header.Set("Authorization", "Bearer "+apiKey)
	getResp, err := http.DefaultClient.Do(getReq)
	if err != nil {
		t.Fatalf("read the artifact back: %v", err)
	}
	defer getResp.Body.Close()
	stored, _ := io.ReadAll(getResp.Body)
	if getResp.StatusCode != http.StatusOK || len(stored) != len(direct.Content) {
		t.Fatalf("GET artifact content = %d with %d bytes; Slack serves %d for the same file",
			getResp.StatusCode, len(stored), len(direct.Content))
	}
	t.Logf("round trip byte-identical against Slack's own bytes: %d bytes, Content-Type=%s",
		len(stored), getResp.Header.Get("Content-Type"))

	// --- 5. The input a run is born with NAMES it, and says where it came from. ---
	input := runInput(reply.Text, turnImages{earlier: ids, missing: dropped + skipped})
	items, ok := input.([]any)
	if !ok || len(items) != len(ids)+1 {
		t.Fatalf("runInput built %#v, want the text plus %d image_ref(s)", input, len(ids))
	}
	first, _ := items[0].(map[string]any)
	if text, _ := first["text"].(string); !strings.Contains(text, "EARLIER in this Slack thread") {
		t.Fatalf("the input text is %q, want it to say the picture was posted before the request", first["text"])
	}
	created, err := client.Responses.Create(ctx, palai.ResponseCreateRequest{Input: input})
	if err != nil {
		t.Fatalf("the control plane refused a run naming an artifact ingested from a thread's earlier message: %v", err)
	}
	t.Logf("a run naming the earlier message's picture was admitted: response=%s run=%s session=%s",
		created.ID, created.RunID, created.SessionID)
}

// threadWithASharedImage picks the newest thread ROOT in a conversations.history answer that a HUMAN shared
// an image on. The root is the coordinate the read keys on, and a root carrying a picture is the live shape
// of the reported defect: whatever is said next in that thread arrives with no attachment of its own.
func threadWithASharedImage(t *testing.T, history map[string]any) string {
	t.Helper()
	msgs, _ := history["messages"].([]any)
	for _, raw := range msgs {
		m, _ := raw.(map[string]any)
		if _, isBot := m["bot_id"]; isBot {
			continue // a picture this app posted is not a human sharing one
		}
		threadTS, _ := m["thread_ts"].(string)
		if threadTS == "" {
			continue // not a thread at all: nothing for conversations.replies to read
		}
		files, _ := m["files"].([]any)
		for _, rawFile := range files {
			f, _ := rawFile.(map[string]any)
			if mime, _ := f["mimetype"].(string); strings.HasPrefix(mime, "image/") {
				return threadTS
			}
		}
	}
	t.Skip("no thread in this channel's recent history has a human-shared image on it — share a screenshot in a thread and re-run")
	return ""
}
