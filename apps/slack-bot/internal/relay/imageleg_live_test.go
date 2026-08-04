//go:build live

// THE IMAGE LEG, AGAINST REAL SLACK AND A REAL CONTROL PLANE.
//
// WHY THIS EXISTS: every other proof of the image leg substitutes something. The unit tests point
// slack.FetchImage at an httptest server, which means the ONE thing they cannot measure is the thing most
// likely to be wrong — whether `url_private` is fetchable with a bot token at all, whether Slack answers
// the bytes or an HTML login page, and whether what comes back still sniffs as an image after Slack has
// stored and re-served it. Those are vendor facts, and a fake cannot hold one.
//
// So this drives the product against the real workspace: it takes the file a REAL PERSON shared in the
// channel, fetches it with the REAL bot token through the REAL ImageLeg, and pushes the bytes into a REAL
// control plane over the REAL public ingest route — ending at an artifact id a run's input can name, and
// at a run the control plane actually admitted.
//
// IT DOES NOT FABRICATE AN INBOUND EVENT. The one seam it cannot drive from here is Slack DELIVERING a
// human's message: this process is the bot, and a message it posts carries a bot_id that MapEvent's loop
// guard drops by design. What it proves is every link the relay owns once such an event arrives — the
// fetch, the ingest, and the id — against real infrastructure on both ends.
package relay

import (
	"bytes"
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

// slackAPI is slackGet (realrun_live_test.go) with this leg's error handling: a non-ok envelope is a fatal
// setup failure here rather than a value to inspect.
func slackAPI(ctx context.Context, t *testing.T, token []byte, method string, params url.Values) map[string]any {
	t.Helper()
	out, err := slackGet(ctx, token, "https://slack.com/api/"+method+"?"+params.Encode())
	if err != nil {
		t.Fatalf("%s: %v", method, err)
	}
	if ok, _ := out["ok"].(bool); !ok {
		t.Fatalf("%s failed: %v", method, out["error"])
	}
	return out
}

// newestSharedImage picks the most recent human-shared IMAGE out of a conversations.history answer, and
// builds the slack.SharedFile an inbound event's `files` array would have carried for it — through the
// adapter's own field choices (url_private_download preferred, url_private as the fallback), so this test
// meets the same value the production mapper would.
func newestSharedImage(t *testing.T, history map[string]any) (slack.SharedFile, string) {
	t.Helper()
	msgs, _ := history["messages"].([]any)
	for _, raw := range msgs {
		m, _ := raw.(map[string]any)
		files, _ := m["files"].([]any)
		for _, rawFile := range files {
			f, _ := rawFile.(map[string]any)
			mime, _ := f["mimetype"].(string)
			if !strings.HasPrefix(mime, "image/") {
				continue
			}
			download, _ := f["url_private_download"].(string)
			if download == "" {
				download, _ = f["url_private"].(string)
			}
			size, _ := f["size"].(float64)
			id, _ := f["id"].(string)
			name, _ := f["name"].(string)
			text, _ := m["text"].(string)
			return slack.SharedFile{ID: id, Name: name, MimeType: mime, Size: int64(size), DownloadURL: download}, text
		}
	}
	t.Skip("no human-shared image found in this channel's recent history — share a screenshot in it and re-run")
	return slack.SharedFile{}, ""
}

// TestARealSlackFileBecomesARealArtifact is the whole live chain the relay owns.
func TestARealSlackFileBecomesARealArtifact(t *testing.T) {
	apiURL := liveEnv(t, "PALAI_API_URL")
	apiKey := liveEnv(t, "PALAI_API_KEY")
	token := []byte(liveEnv(t, "SLACK_BOT_TOKEN"))
	channel := liveEnv(t, "SLACK_TEST_CHANNEL")

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	// --- 1. Take the file a REAL HUMAN shared in the REAL channel. ---
	//
	// IT IS NOT UPLOADED BY THIS TEST, and that is not a shortcut — it is the stronger reading and it is
	// also the only one available. This app holds `files:read` and NOT `files:write` (the manifest grants
	// no write scope for the inbound direction), so a bot-uploaded fixture is impossible here; measured
	// 2026-08-04, files.getUploadURLExternal answers `missing_scope` with this workspace's bot token.
	//
	// What it reads instead is the newest message in the channel carrying an image, which is a file object
	// SLACK BUILT from a person's own upload — the same object an inbound `files` array carries, including
	// the `url_private` this leg keys on. A fixture this test made would prove the round trip; the human's
	// own file proves the case the capability exists for.
	msgs := slackAPI(ctx, t, token, "conversations.history", url.Values{"channel": {channel}, "limit": {"60"}})
	shared, ownerText := newestSharedImage(t, msgs)
	t.Logf("found a REAL human-shared image: file=%s mimetype=%s size=%d, on the message %q",
		shared.ID, shared.MimeType, shared.Size, ownerText)

	// The file object must carry url_private — the whole fetch leg keys on it, and an event whose file
	// object lacked it would be silently unfetchable.
	if shared.DownloadURL == "" {
		t.Fatal("the real file object carried no url_private — the whole fetch leg keys on this field")
	}
	if host := mustHost(t, shared.DownloadURL); host != "files.slack.com" && host != "files-edge.slack.com" {
		t.Fatalf("url_private points at %q, which slack.FetchImage's host allow-list refuses — the allow-list and reality disagree", host)
	}
	t.Logf("url_private host = %s (inside the fetch allow-list)", mustHost(t, shared.DownloadURL))

	// --- 3. Fetch it with the REAL bot token through the REAL leg, into a REAL control plane. ---
	client, err := palai.New(palai.WithBaseURL(apiURL), palai.WithAPIKey(apiKey))
	if err != nil {
		t.Fatalf("construct SDK client: %v", err)
	}
	leg := &ImageLeg{Doer: http.DefaultClient, Token: token, Artifacts: NewArtifactCreator(client)}
	if !leg.Ready() {
		t.Fatal("the leg reports itself not ready with all three halves supplied")
	}

	ids, skipped := leg.attach(ctx, []slack.SharedFile{shared}, maxImagesPerMessage, t.Logf)
	if len(ids) != 1 {
		t.Fatalf("attach() returned %d artifact ids and skipped %d — the real fetch or the real ingest refused", len(ids), skipped)
	}
	t.Logf("REAL Slack file %s -> REAL artifact %s", shared.ID, ids[0])

	// --- 4. The artifact is real: read its bytes back over the public API and compare them against a
	// SECOND, INDEPENDENT fetch of the same Slack file. Comparing against a fixture this test carried
	// would only prove the fixture; comparing against Slack's own bytes proves the leg moved THOSE.
	direct, err := slack.FetchImage(ctx, http.DefaultClient, token, shared, maxImageBytes)
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
	if getResp.StatusCode != http.StatusOK {
		t.Fatalf("GET artifact content = %d, want 200", getResp.StatusCode)
	}
	stored, _ := io.ReadAll(getResp.Body)
	if !bytes.Equal(stored, direct.Content) {
		t.Fatalf("the stored artifact is %d bytes; Slack serves %d for the same file — the leg did not preserve the bytes", len(stored), len(direct.Content))
	}
	if ct := getResp.Header.Get("Content-Type"); ct != direct.MediaType {
		t.Fatalf("the artifact serves Content-Type %q, want the SNIFFED %q", ct, direct.MediaType)
	}
	t.Logf("round trip byte-identical against Slack's own bytes: %d bytes, Content-Type=%s", len(stored), getResp.Header.Get("Content-Type"))

	// --- 5. The id is nameable: a run's input carrying it is accepted. ---
	input := runInput("bu ekran goruntusunde ne yaziyor?", turnImages{own: ids, skipped: skipped})
	items, ok := input.([]any)
	if !ok || len(items) != 2 {
		t.Fatalf("runInput built %#v, want a two-item content array", input)
	}
	created, err := client.Responses.Create(ctx, palai.ResponseCreateRequest{Input: input})
	if err != nil {
		t.Fatalf("the control plane refused a run naming a real ingested artifact: %v", err)
	}
	t.Logf("a run naming the real artifact was admitted: response=%s run=%s session=%s", created.ID, created.RunID, created.SessionID)
}

func mustHost(t *testing.T, raw string) string {
	t.Helper()
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatalf("parse url_private: %v", err)
	}
	return u.Hostname()
}
