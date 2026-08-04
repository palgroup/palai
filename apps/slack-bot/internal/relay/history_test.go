package relay

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"

	slack "github.com/palgroup/palai/adapters/integrations/slack"
)

// fakeHistory is ThreadHistory's double. It records the COORDINATES it was asked for, not just the count:
// the whole authority argument for this read is that it addresses the admitted event's own channel and
// thread and nothing else, and a test that only counted calls could not tell a correct read from one keyed
// on something a payload carried in.
type fakeHistory struct {
	msgs    []slack.ThreadMessage
	hasMore bool
	err     error

	calls    int
	channels []string
	threads  []string
	limits   []int
}

func (f *fakeHistory) ThreadReplies(_ context.Context, channel, threadTS string, limit int) ([]slack.ThreadMessage, bool, error) {
	f.calls++
	f.channels = append(f.channels, channel)
	f.threads = append(f.threads, threadTS)
	f.limits = append(f.limits, limit)
	if f.err != nil {
		return nil, false, f.err
	}
	return f.msgs, f.hasMore, nil
}

// newHistoryDeps wires the inbound deps over fakes with BOTH optional halves mounted — the image leg
// (pointed at a local file server through the redirecting Doer images_test.go already uses) and the thread
// read. It is the shape a real deployment has, since main.go mounts the two together.
func newHistoryDeps(t *testing.T, hist *fakeHistory) (InboundDeps, *fakePalai, *fakeStore, *fakeUploader) {
	t.Helper()
	deps, fp, fs := newTestDeps(t)
	files := newSlackFileServer(t, pngBytes, http.StatusOK)
	up := &fakeUploader{}
	deps = deps.WithImages(&ImageLeg{
		Doer: redirectingDoer{to: files.srv.URL}, Token: []byte("xoxb-secret"), Artifacts: up,
	}, nil).WithThreadHistory(hist)
	return deps, fp, fs, up
}

// threadMessage builds one message of a fetched thread page.
func threadMessage(ts string, fileIDs ...string) slack.ThreadMessage {
	m := slack.ThreadMessage{UserID: "U2", TS: ts, Text: "look at this"}
	for _, id := range fileIDs {
		m.Files = append(m.Files, imageFile(id))
	}
	return m
}

// lateMention is the event at the centre of every test here: a human @mentions the bot as a REPLY IN A
// THREAD the bot has no session for, carrying no attachment of its own — the shape that made the reported
// defect invisible, since the picture is one message above it.
func lateMention() slack.Event {
	return slack.Event{Type: "app_mention", Kind: slack.KindMessage, InThread: true,
		TeamID: "T1", ChannelID: "C1", ThreadTS: "1.1", MessageTS: "100.005",
		SourceEventID: "Ev1", UserID: "U2", Text: "fix the bug in the screenshot"}
}

func inputItems(t *testing.T, fp *fakePalai) []any {
	t.Helper()
	items, ok := fp.lastCreateReq.Input.([]any)
	if !ok {
		t.Fatalf("the run input is %T (%v), want a content array — a bare string names no artifact and the "+
			"model sees no picture", fp.lastCreateReq.Input, fp.lastCreateReq.Input)
	}
	return items
}

func imageRefs(items []any) []string {
	var ids []string
	for _, item := range items {
		if m, ok := item.(map[string]any); ok && m["type"] == "image_ref" {
			ids = append(ids, fmt.Sprint(m["artifact_id"]))
		}
	}
	return ids
}

func inputText(items []any) string {
	if len(items) == 0 {
		return ""
	}
	first, _ := items[0].(map[string]any)
	return fmt.Sprint(first["text"])
}

// TestAScreenshotPostedOneMessageEARLIERReachesTheModel is THE test for this defect, and it is the exact
// live episode: a human posts a screenshot, then @mentions the bot in the next message. The run is born from
// the mention — which carries no files at all — so before this the bot answered that it could not see any
// screenshot about a screenshot sitting one line above it.
//
// It asserts the end-to-end shape rather than an intermediate: the bytes were fetched from Slack with the
// bot token, they reached the artifact writer, and the id that writer returned is named as an `image_ref` in
// the input the SDK was asked to send.
func TestAScreenshotPostedOneMessageEarlierReachesTheModel(t *testing.T) {
	hist := &fakeHistory{msgs: []slack.ThreadMessage{
		threadMessage("100.001"),          // the thread's root, no files
		threadMessage("100.004", "F_OLD"), // the screenshot
		threadMessage("100.005"),          // the @mention itself, as conversations.replies returns it
	}}
	deps, fp, _, up := newHistoryDeps(t, hist)

	if err := HandleEvent(context.Background(), deps, lateMention()); err != nil {
		t.Fatalf("HandleEvent: %v", err)
	}

	if hist.calls != 1 {
		t.Fatalf("thread reads = %d, want exactly 1", hist.calls)
	}
	if hist.channels[0] != "C1" || hist.threads[0] != "1.1" {
		t.Fatalf("the read addressed (%s,%s), want the ADMITTED EVENT's own channel and thread (C1,1.1)",
			hist.channels[0], hist.threads[0])
	}
	if up.n != 1 {
		t.Fatalf("artifacts uploaded = %d, want 1 — the screenshot from the earlier message never reached Palai", up.n)
	}
	items := inputItems(t, fp)
	if refs := imageRefs(items); len(refs) != 1 || refs[0] != "art_1" {
		t.Fatalf("image_refs = %v, want exactly the artifact the earlier screenshot became", refs)
	}
	if text := inputText(items); !strings.Contains(text, "fix the bug") {
		t.Fatalf("the input text is %q, want the human's own words kept first", text)
	}
}

// TestAnEarlierImageSaysItIsEarlier pins the provenance sentence. Handed a picture with no note a model
// reads it as "the screenshot attached to this request" and answers as though the person pointed at it —
// but it might be a diagram somebody posted twenty minutes ago about something else. The note is what turns
// a confidently wrong answer into an accurate one, so it is asserted separately from the attachment itself.
func TestAnEarlierImageSaysItIsEarlier(t *testing.T) {
	hist := &fakeHistory{msgs: []slack.ThreadMessage{
		threadMessage("100.004", "F_OLD"),
		threadMessage("100.005"),
	}}
	deps, fp, _, _ := newHistoryDeps(t, hist)

	if err := HandleEvent(context.Background(), deps, lateMention()); err != nil {
		t.Fatalf("HandleEvent: %v", err)
	}
	text := inputText(inputItems(t, fp))
	if !strings.Contains(text, "EARLIER in this Slack thread") || !strings.Contains(text, "not with it") {
		t.Fatalf("the input text is %q, want it to say the picture was posted BEFORE this request rather "+
			"than with it", text)
	}
}

// TestTheTriggeringMessagesOwnFileIsNotAttachedTwice pins the skip rule, and pins that it is an ID
// comparison rather than a text heuristic: conversations.replies returns the very message that caused the
// read, files and all. Taking it here as well as from ev.Files would fetch one screenshot twice, upload it
// twice and put two image_refs for one picture in front of the model — which also spends two slots of the
// platform's per-request image ceiling on one image.
//
// The second half is the same defence against a RE-SHARE: the same Slack file object appearing in two
// different messages is one file id, and it is attached once.
func TestTheTriggeringMessagesOwnFileIsNotAttachedTwice(t *testing.T) {
	hist := &fakeHistory{msgs: []slack.ThreadMessage{
		threadMessage("100.003", "F_SAME"), // the same file, re-shared earlier in the thread
		threadMessage("100.005", "F_SAME"), // the triggering message, as Slack returns it
	}}
	deps, fp, _, up := newHistoryDeps(t, hist)
	ev := lateMention()
	ev.Files = []slack.SharedFile{imageFile("F_SAME")}

	if err := HandleEvent(context.Background(), deps, ev); err != nil {
		t.Fatalf("HandleEvent: %v", err)
	}
	if up.n != 1 {
		t.Fatalf("artifacts uploaded = %d, want 1 — one picture must not become two", up.n)
	}
	if refs := imageRefs(inputItems(t, fp)); len(refs) != 1 {
		t.Fatalf("image_refs = %v, want one for one picture", refs)
	}
}

// TestAThreadTheBotIsAlreadyInReadsNothing pins the second clause of the fetch rule, and it is the clause
// that keeps this cheap and correct: a thread with a session of its own already carries every image shared
// in it while the bot was present — the control plane replays those turns — so a read here would re-fetch,
// re-upload and re-attach pictures the conversation already has, spending the per-request image ceiling on
// duplicates.
func TestAThreadTheBotIsAlreadyInReadsNothing(t *testing.T) {
	hist := &fakeHistory{msgs: []slack.ThreadMessage{threadMessage("100.004", "F_OLD")}}
	deps, _, fs, up := newHistoryDeps(t, hist)
	if _, err := fs.BindThread(context.Background(), "bot_1", "T1", "C1", "1.1", "ses_existing"); err != nil {
		t.Fatalf("seed the bound thread: %v", err)
	}

	reply := slack.Event{Type: "message", Kind: slack.KindMessage, InThread: true,
		TeamID: "T1", ChannelID: "C1", ThreadTS: "1.1", MessageTS: "100.006",
		SourceEventID: "Ev2", UserID: "U2", Text: "and now what"}
	if err := HandleEvent(context.Background(), deps, reply); err != nil {
		t.Fatalf("HandleEvent: %v", err)
	}
	if hist.calls != 0 {
		t.Fatalf("thread reads = %d on a thread this bot already holds, want 0", hist.calls)
	}
	if up.n != 0 {
		t.Fatalf("artifacts uploaded = %d, want 0", up.n)
	}
}

// TestATopLevelMentionReadsNothing pins the first clause. A top-level mention IS its own thread root, so
// conversations.replies would answer with that one message — the files this relay already has on the event —
// and nothing else: one Slack round trip, per new conversation, to learn what it already knows.
func TestATopLevelMentionReadsNothing(t *testing.T) {
	hist := &fakeHistory{}
	deps, _, _, _ := newHistoryDeps(t, hist)

	ev := lateMention()
	ev.InThread = false
	ev.ThreadTS, ev.MessageTS = "200.001", "200.001" // its own root, which is what a top-level mention is
	if err := HandleEvent(context.Background(), deps, ev); err != nil {
		t.Fatalf("HandleEvent: %v", err)
	}
	if hist.calls != 0 {
		t.Fatalf("thread reads = %d for a message that IS its own thread root, want 0", hist.calls)
	}
}

// TestAMessageThatBirthsNoRunReadsNothing is the scope property, and it is the one worth being strictest
// about: this app holds `channels:history`, so a read keyed on coordinates from a message nobody addressed
// the bot in would be an arbitrary read of the workspace dressed up as the app doing its job. Ordinary
// chatter in a channel the bot merely sits in must produce no Slack call at all.
func TestAMessageThatBirthsNoRunReadsNothing(t *testing.T) {
	hist := &fakeHistory{msgs: []slack.ThreadMessage{threadMessage("300.001", "F_OLD")}}
	deps, fp, _, up := newHistoryDeps(t, hist)

	chatter := slack.Event{Type: "message", Kind: slack.KindMessage, InThread: true, ChannelType: "channel",
		TeamID: "T1", ChannelID: "C1", ThreadTS: "300.001", MessageTS: "300.002",
		SourceEventID: "Ev9", UserID: "U9", Text: "two colleagues talking to each other"}
	if err := HandleEvent(context.Background(), deps, chatter); err != nil {
		t.Fatalf("HandleEvent: %v", err)
	}
	if hist.calls != 0 {
		t.Fatalf("thread reads = %d for a message the bot was never addressed in, want 0 — this read is only "+
			"legitimate because birthsRun already said yes", hist.calls)
	}
	if fp.responses != 0 || up.n != 0 {
		t.Fatalf("responses=%d uploads=%d, want 0/0", fp.responses, up.n)
	}
}

// TestATruncatedPageAttachesNothing pins the rule that makes this read trustworthy at all. Slack's
// conversations.replies returns "the earliest messages in the time range first", so ONE page is the START of
// a thread — and the picture this read exists to find is at the tail. When Slack says the thread did not fit
// (has_more), attaching from that page would hand the model a screenshot from hundreds of messages back and
// describe it as "shared earlier in this thread": confidently wrong rather than merely incomplete.
func TestATruncatedPageAttachesNothing(t *testing.T) {
	hist := &fakeHistory{hasMore: true, msgs: []slack.ThreadMessage{
		threadMessage("1.2", "F_ANCIENT"),
		threadMessage("1.3", "F_ALSO_ANCIENT"),
	}}
	deps, fp, _, up := newHistoryDeps(t, hist)

	if err := HandleEvent(context.Background(), deps, lateMention()); err != nil {
		t.Fatalf("HandleEvent: %v", err)
	}
	if up.n != 0 {
		t.Fatalf("uploads = %d from a page Slack said was incomplete, want 0", up.n)
	}
	text, ok := fp.lastCreateReq.Input.(string)
	if !ok {
		t.Fatalf("the run input is %T, want the bare string — nothing was attached", fp.lastCreateReq.Input)
	}
	if strings.Contains(text, "EARLIER in this Slack thread") {
		t.Fatalf("the input says %q; a page that never reached the thread's recent messages knows of no "+
			"earlier images to describe", text)
	}
}

// TestTheNewestEarlierImagesWinTheBudget pins BOTH halves of the selection, which a single-image fixture
// cannot reach: the budget is spent on the pictures nearest the question (a forward walk would fill all
// three slots from the thread's oldest messages and drop the screenshot from one message ago — the exact
// case this whole file exists for), and what survives is handed over in the order a reader saw it.
//
// The count of what was left behind is asserted with it, because a cap without a note is a silent drop.
func TestTheNewestEarlierImagesWinTheBudgetAndArriveInOrder(t *testing.T) {
	hist := &fakeHistory{msgs: []slack.ThreadMessage{
		threadMessage("100.001", "F_1"),
		threadMessage("100.002", "F_2"),
		threadMessage("100.003", "F_3", "F_4"),
		threadMessage("100.004", "F_5"),
		threadMessage("100.005"), // the mention
	}}
	deps, fp, _, up := newHistoryDeps(t, hist)

	if err := HandleEvent(context.Background(), deps, lateMention()); err != nil {
		t.Fatalf("HandleEvent: %v", err)
	}
	if up.n != maxThreadImages {
		t.Fatalf("uploads = %d, want the thread budget of %d", up.n, maxThreadImages)
	}
	// The uploader mints art_1, art_2, … in the order it was CALLED, so asserting the ids in the input is
	// asserting the fetch order too: art_1 must be the oldest survivor.
	if refs := imageRefs(inputItems(t, fp)); len(refs) != 3 || refs[0] != "art_1" || refs[2] != "art_3" {
		t.Fatalf("image_refs = %v, want three in the order they were posted", refs)
	}
	if len(hist.msgs[2].Files) != 2 {
		t.Fatal("fixture drift: the third message must carry two files for the within-message ordering to matter")
	}
	text := inputText(inputItems(t, fp))
	if !strings.Contains(text, "2 further image(s)") {
		t.Fatalf("the input text is %q, want it to COUNT the two older images that did not fit", text)
	}
}

// TestTheMessagesOwnPictureOutranksTheThreads pins the two budgets against each other. The attachment the
// human just made must never lose a slot to a picture from earlier in the thread — and it must come LAST in
// the input, because the control plane spends its own per-request image ceiling on the most recent images
// (execution.decodeMessages), so the tail of the array is what survives a long conversation.
func TestTheMessagesOwnPictureOutranksTheThreads(t *testing.T) {
	hist := &fakeHistory{msgs: []slack.ThreadMessage{
		threadMessage("100.003", "F_OLD"),
		threadMessage("100.005", "F_NEW"),
	}}
	deps, fp, _, _ := newHistoryDeps(t, hist)
	ev := lateMention()
	ev.Files = []slack.SharedFile{imageFile("F_NEW")}

	if err := HandleEvent(context.Background(), deps, ev); err != nil {
		t.Fatalf("HandleEvent: %v", err)
	}
	refs := imageRefs(inputItems(t, fp))
	if len(refs) != 2 {
		t.Fatalf("image_refs = %v, want the thread's picture and the message's own", refs)
	}
	// art_1 is the message's own (fetched first, so it can never lose a slot); art_2 is the thread's. The
	// INPUT order is the reverse of the fetch order, and deliberately so.
	if refs[0] != "art_2" || refs[1] != "art_1" {
		t.Fatalf("image_refs = %v, want the thread's older picture first and the message's own LAST — the "+
			"array's tail is what survives the platform's per-request image ceiling", refs)
	}
}

// TestANonImageSharedEarlierIsNotReportedAsARefusal pins the asymmetry between the two sources, which is
// deliberate and easy to get wrong in either direction. A PDF attached to THIS request is a refusal the
// asker must be told about; the same PDF shared in the thread two hours ago was offered to nobody, and
// counting it into the prompt would report a refusal no one asked for.
func TestANonImageSharedEarlierIsNotReportedAsARefusal(t *testing.T) {
	doc := slack.SharedFile{ID: "F_DOC", Name: "spec.pdf", MimeType: "application/pdf", Size: 1000,
		DownloadURL: "https://files.slack.com/files-pri/T1-F_DOC/spec.pdf"}
	hist := &fakeHistory{msgs: []slack.ThreadMessage{
		{UserID: "U2", TS: "100.003", Text: "the spec", Files: []slack.SharedFile{doc}},
		threadMessage("100.005"),
	}}
	deps, fp, _, up := newHistoryDeps(t, hist)

	if err := HandleEvent(context.Background(), deps, lateMention()); err != nil {
		t.Fatalf("HandleEvent: %v", err)
	}
	if up.n != 0 {
		t.Fatalf("uploads = %d, want 0 — a PDF is never fetched", up.n)
	}
	text, ok := fp.lastCreateReq.Input.(string)
	if !ok {
		t.Fatalf("the run input is %T, want the bare string", fp.lastCreateReq.Input)
	}
	if strings.Contains(text, "could not be attached") {
		t.Fatalf("the input says %q about a document nobody offered to this request", text)
	}
}

// TestAFailedThreadReadStillDeliversTheTurn is the honesty rule of the whole leg applied to its newest
// failure point: no thread history is a less useful answer, a lost message is a lost message. A workspace
// that refuses the read (a private channel needs groups:history, which this app is not granted) must still
// get its question answered.
func TestAFailedThreadReadStillDeliversTheTurn(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
	}{
		{"slack refuses", &slack.APIError{Code: "missing_scope"}},
		{"the transport fails", errors.New("dial tcp: connection refused")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			hist := &fakeHistory{err: tc.err}
			deps, fp, _, _ := newHistoryDeps(t, hist)

			if err := HandleEvent(context.Background(), deps, lateMention()); err != nil {
				t.Fatalf("HandleEvent: %v — a failed history read must not cost the turn", err)
			}
			if fp.responses != 1 {
				t.Fatalf("responses = %d, want 1", fp.responses)
			}
			if got, ok := fp.lastCreateReq.Input.(string); !ok || got != "fix the bug in the screenshot" {
				t.Fatalf("the run input is %#v, want the message's own words unchanged", fp.lastCreateReq.Input)
			}
		})
	}
}

// TestNoThreadIsReadWithNoImageLegMounted pins the third clause of the fetch rule: with no leg (or one
// missing a token or an artifact writer) nothing the read finds can become something the model sees, so the
// call is a Slack round trip paid for nothing.
func TestNoThreadIsReadWithNoImageLegMounted(t *testing.T) {
	for _, tc := range []struct {
		name string
		leg  *ImageLeg
	}{
		{"no leg at all", nil},
		{"a leg with no token", &ImageLeg{Doer: redirectingDoer{to: "http://127.0.0.1:1"}, Artifacts: &fakeUploader{}}},
		{"a leg with no artifact writer", &ImageLeg{Doer: redirectingDoer{to: "http://127.0.0.1:1"}, Token: []byte("xoxb")}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			deps, _, _ := newTestDeps(t)
			hist := &fakeHistory{msgs: []slack.ThreadMessage{threadMessage("100.004", "F_OLD")}}
			deps = deps.WithImages(tc.leg, nil).WithThreadHistory(hist)

			if err := HandleEvent(context.Background(), deps, lateMention()); err != nil {
				t.Fatalf("HandleEvent: %v", err)
			}
			if hist.calls != 0 {
				t.Fatalf("thread reads = %d with nothing able to attach what they find, want 0", hist.calls)
			}
		})
	}
}

// TestNoThreadIsReadWithNoHistoryMounted is the other optional half, and it pins the posture every optional
// piece of this relay takes: a deployment that has not mounted the read behaves exactly as it did before
// this file existed, rather than failing.
func TestNoThreadIsReadWithNoHistoryMounted(t *testing.T) {
	deps, fp, _ := newTestDeps(t)
	files := newSlackFileServer(t, pngBytes, http.StatusOK)
	deps = deps.WithImages(&ImageLeg{
		Doer: redirectingDoer{to: files.srv.URL}, Token: []byte("xoxb-secret"), Artifacts: &fakeUploader{},
	}, nil) // no WithThreadHistory call at all

	if err := HandleEvent(context.Background(), deps, lateMention()); err != nil {
		t.Fatalf("HandleEvent: %v", err)
	}
	if got, ok := fp.lastCreateReq.Input.(string); !ok || got != "fix the bug in the screenshot" {
		t.Fatalf("the run input is %#v, want the message's own words and nothing else", fp.lastCreateReq.Input)
	}
}

// TestTheThreadReadIsBounded pins the page bound at the call. Slack's `limit` DEFAULTS TO 1000, so an unset
// one is an unbounded response written by whoever has been loudest in the thread — and the bound here is
// also what the has_more refusal above is measured against.
func TestTheThreadReadIsBounded(t *testing.T) {
	hist := &fakeHistory{}
	deps, _, _, _ := newHistoryDeps(t, hist)

	if err := HandleEvent(context.Background(), deps, lateMention()); err != nil {
		t.Fatalf("HandleEvent: %v", err)
	}
	if len(hist.limits) != 1 || hist.limits[0] != threadHistoryMaxMessages {
		t.Fatalf("the read passed limits %v, want exactly one bounded at %d", hist.limits, threadHistoryMaxMessages)
	}
}
