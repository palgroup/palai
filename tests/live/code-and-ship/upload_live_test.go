//go:build live

// The Slack FILE UPLOAD live smoke (E22 T5). Written now, runs UNCHANGED the moment the owner supplies
// credentials, and SKIPS — naming the missing variable and the plan §0 row that supplies it — rather than
// failing, so a partial handover reports partial-green.
//
// WHY IT LIVES IN ITS OWN ROOT rather than beside the E20 legs in tests/live/slack: that directory is swept
// tree→table by tests/uat/wiring/live_inventory_test.go, and the table's LENGTH is compared against the
// COMMITTED integration-wiring-0.1.0 bundle. A fifth Slack leg there would therefore not be a new test — it
// would be a regeneration of a shipped historical release. E22's own inventory belongs to E22's own bundle
// (T7), and this root is where it will find this leg.
//
// WHAT IT SETTLES THAT NO FIXTURE CAN — these are plan §3.5 X23, the row that is UNCONFIRMED on purpose
// because two published reference pages do not answer it, and NONE of them entered the code as an assumption:
//
//	(1) THE REAL MAXIMUM. Neither files.getUploadURLExternal nor files.completeUploadExternal prints a size
//	    limit for a bot token. Our 8 MiB is OURS, chosen for what a thread should carry; whether Slack itself
//	    would have taken more (or less) is MEASURED here and asserted nowhere.
//	(2) WHETHER A QUICKTIME RECORDING PLAYS INLINE. `simctl io recordVideo` writes a QuickTime container even
//	    with --codec=h264, and no page says what Slack's player does with one. This test uploads a real .mov
//	    and reports what came back; a human looks at the thread. That is the honest form of the question.
//	(3) WHETHER `blocks` WOULD HAVE ACCEPTED A MARKDOWN BLOCK. Not asked here, because the answer would not
//	    change the code: initial_comment is documented and sufficient, and a vendor's silence is not a design
//	    freedom. It stays in known-gaps as a question rather than a defect.
//
// AND THE CEILING THIS LEG DOES NOT CROSS, said plainly: it drives the WIRE (adapters/integrations/slack)
// against a real workspace. It does not run a control plane, so "the pump published the artifact" is the
// component tier's claim, not this one's.
package live

import (
	"context"
	"net/http"
	"os"
	"testing"

	"github.com/palgroup/palai/adapters/integrations/slack"
)

// need returns an env var's value or skips, naming where the plan §0 row that supplies it lives.
func need(t *testing.T, name, where string) string {
	t.Helper()
	v := os.Getenv(name)
	if v == "" {
		t.Skipf("%s is not set — supply it from E22 plan §0 (%s) and re-run; no code changes are needed", name, where)
	}
	return v
}

// livePNG is a real 1x1 PNG. It must be a real one: the extension is SNIFFED, and bytes that are not
// something this integration can name honestly are refused before they reach Slack.
var livePNG = []byte{
	0x89, 0x50, 0x4E, 0x47, 0x0D, 0x0A, 0x1A, 0x0A, 0x00, 0x00, 0x00, 0x0D, 0x49, 0x48, 0x44, 0x52,
	0x00, 0x00, 0x00, 0x01, 0x00, 0x00, 0x00, 0x01, 0x08, 0x06, 0x00, 0x00, 0x00, 0x1F, 0x15, 0xC4,
	0x89, 0x00, 0x00, 0x00, 0x0A, 0x49, 0x44, 0x41, 0x54, 0x78, 0x9C, 0x63, 0x00, 0x01, 0x00, 0x00,
	0x05, 0x00, 0x01, 0x0D, 0x0A, 0x2D, 0xB4, 0x00, 0x00, 0x00, 0x00, 0x49, 0x45, 0x4E, 0x44, 0xAE,
	0x42, 0x60, 0x82,
}

// liveQuickTime is a QuickTime container header — the `ftyp` box with the `qt  ` major brand that
// `simctl io recordVideo` writes. It is not a playable movie, which is the point of the log line in the test:
// what Slack DOES with a QuickTime container is a thing a human reads off the thread, not an assertion.
var liveQuickTime = append([]byte{
	0x00, 0x00, 0x00, 0x14, 'f', 't', 'y', 'p', 'q', 't', ' ', ' ',
	0x00, 0x00, 0x02, 0x00, 'q', 't', ' ', ' ',
}, make([]byte, 512)...)

// TestLiveSlackUploadsAScreenshotAndARecording is the leg. It opens a thread, puts a PNG and a .mov in it as
// real files, and leaves both there for the operator to look at — the LOOKS half is §6 leg 1 and cannot be
// asserted from here.
func TestLiveSlackUploadsAScreenshotAndARecording(t *testing.T) {
	token := []byte(need(t, "SLACK_BOT_TOKEN", "§0.5 — the bot token, and it must now carry files:write per §0.1"))
	channel := need(t, "SLACK_TEST_CHANNEL", "§0.5 — a channel the app is a member of")
	apiBase := "https://slack.com/api"
	ctx := context.Background()

	// The thread root. Its ts is the PARENT — the value files.completeUploadExternal demands and the one
	// slack_reply_deliveries freezes in production ("Never use a reply's ts value; use its parent instead").
	root, err := slack.PostMessage(ctx, http.DefaultClient, slack.PostRequest{
		MethodURL: apiBase + "/chat.postMessage", Token: token,
		Body: slack.ThreadReply(channel, "", "E22 T5 live smoke: a run's artifacts, delivered as files.", ""),
	}, slack.PostOptions{})
	if err != nil {
		t.Fatalf("open the thread: %v", err)
	}

	for _, tc := range []struct {
		what string
		body []byte
	}{
		{"a screenshot", livePNG},
		{"a screen recording", liveQuickTime},
	} {
		sniffed, ok := slack.SniffUpload(tc.body)
		if !ok {
			t.Fatalf("%s: SniffUpload refused bytes the fixture says it should publish", tc.what)
		}
		err := slack.UploadToThread(ctx, http.DefaultClient, apiBase, token, slack.Upload{
			ChannelID: channel,
			ThreadTS:  root.MessageTS,
			Filename:  "palai-live-smoke" + sniffed.Extension,
			AltText:   sniffed.AltText,
			Comment:   tc.what + " from the E22 T5 live smoke",
			Body:      tc.body,
		})
		if err != nil {
			// A refusal here is the MEASUREMENT, not a surprise: `missing_scope` says the workspace has not
			// been reinstalled with files:write (§0.1), which is an operator step rather than a defect.
			t.Fatalf("%s: upload failed: %v — if this is missing_scope, reinstall the app with the §0.1 manifest", tc.what, err)
		}
		t.Logf("MEASURED (X23): %s uploaded as %s (%s, %d bytes) into thread %s of %s — look at the thread and record whether a QuickTime container plays INLINE or renders as a download",
			tc.what, sniffed.Extension, sniffed.MediaType, len(tc.body), root.MessageTS, channel)
	}

	// THE SIZE QUESTION, asked of Slack rather than of ourselves. Our ceiling refuses this before any call, so
	// what a bot token could ACTUALLY have uploaded is not something the product path can find out — and it is
	// not something this test guesses either. It records the ceiling it enforced and names the open question.
	t.Logf("UNCONFIRMED (X23): our ceiling is %d bytes and it is ours, not Slack's — neither reference page prints a maximum, and this leg deliberately does not probe for one by uploading progressively larger files into a real workspace",
		slack.MaxUploadBytes)
}
