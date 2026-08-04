package relay

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	slack "github.com/palgroup/palai/adapters/integrations/slack"
)

// pngBytes is a real, minimal PNG. slack.FetchImage SNIFFS what it downloads, so a fixture that only
// satisfied the first eight bytes would be refused by the code under test for the wrong reason — and a
// test that "passes" because its fixture never got past a guard proves nothing about the guard.
var pngBytes = []byte{
	0x89, 'P', 'N', 'G', 0x0d, 0x0a, 0x1a, 0x0a,
	0x00, 0x00, 0x00, 0x0d, 'I', 'H', 'D', 'R',
	0x00, 0x00, 0x00, 0x01, 0x00, 0x00, 0x00, 0x01, 0x08, 0x06, 0x00, 0x00, 0x00,
	0x1f, 0x15, 0xc4, 0x89,
	0x00, 0x00, 0x00, 0x0a, 'I', 'D', 'A', 'T',
	0x78, 0x9c, 0x63, 0x00, 0x01, 0x00, 0x00, 0x05, 0x00, 0x01,
	0x0d, 0x0a, 0x2d, 0xb4,
	0x00, 0x00, 0x00, 0x00, 'I', 'E', 'N', 'D', 0xae, 0x42, 0x60, 0x82,
}

// fakeUploader is ArtifactCreator's double: it records the bytes it was handed and mints a predictable id,
// so a test can assert BOTH that the upload happened and that the id it returned is the one the run's input
// went on to name. Recording the bytes is what makes "the screenshot reached Palai" assertable rather than
// merely "some call was made".
type fakeUploader struct {
	mu       sync.Mutex
	uploaded [][]byte
	err      error
	n        int
}

func (f *fakeUploader) CreateArtifact(_ context.Context, content []byte) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return "", f.err
	}
	f.uploaded = append(f.uploaded, content)
	f.n++
	return fmt.Sprintf("art_%d", f.n), nil
}

// slackFileServer stands in for files.slack.com. It asserts the ONE thing about the request that is a
// security property rather than a detail — that the bot token rode the Authorization header — and hands
// back the bytes.
//
// It is an httptest server, so its host is 127.0.0.1, which slack.FetchImage's host allow-list REFUSES.
// That refusal is real and is exercised by its own test in the adapter package; here the fetch is pointed
// at the server through a Doer that rewrites the destination, so this test measures the leg's behaviour
// rather than re-measuring the adapter's host check.
type slackFileServer struct {
	srv       *httptest.Server
	gotBearer string
}

func newSlackFileServer(t *testing.T, body []byte, status int) *slackFileServer {
	t.Helper()
	s := &slackFileServer{}
	s.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.gotBearer = r.Header.Get("Authorization")
		w.WriteHeader(status)
		_, _ = w.Write(body)
	}))
	t.Cleanup(s.srv.Close)
	return s
}

// redirectingDoer sends every request to the test server while leaving the REQUEST the code built
// untouched in every other way — the URL the leg chose has already passed slack.FetchImage's host check by
// the time this runs, so nothing about that check is bypassed for the assertions below.
type redirectingDoer struct{ to string }

func (d redirectingDoer) Do(req *http.Request) (*http.Response, error) {
	rewritten := req.Clone(req.Context())
	u := *req.URL
	target, err := req.URL.Parse(d.to)
	if err != nil {
		return nil, err
	}
	u.Scheme, u.Host = target.Scheme, target.Host
	rewritten.URL = &u
	rewritten.Host = ""
	return http.DefaultClient.Do(rewritten)
}

func imageFile(id string) slack.SharedFile {
	return slack.SharedFile{
		ID: id, Name: "screenshot.png", MimeType: "image/png",
		Size: int64(len(pngBytes)), DownloadURL: "https://files.slack.com/files-pri/T1-F1/" + id + ".png",
	}
}

// TestASharedScreenshotBecomesAnImageRefInTheRunInput is THE test for this whole change: a message that
// carries a picture must produce a run input that NAMES an artifact, because an input that names nothing is
// exactly the defect this fixes — the model answering "I don't see a screenshot" about a screenshot that
// was right there.
//
// It asserts the end-to-end shape, not an intermediate: the bytes reached the uploader, and the id the
// uploader returned appears as an `image_ref` in the input the SDK was asked to send.
func TestASharedScreenshotBecomesAnImageRefInTheRunInput(t *testing.T) {
	deps, fp, _ := newTestDeps(t)
	files := newSlackFileServer(t, pngBytes, http.StatusOK)
	up := &fakeUploader{}
	deps = deps.WithImages(&ImageLeg{
		Doer: redirectingDoer{to: files.srv.URL}, Token: []byte("xoxb-secret"), Artifacts: up,
	}, nil)

	ev := slack.Event{Type: "app_mention", Kind: slack.KindMessage,
		TeamID: "T1", ChannelID: "C1", ThreadTS: "1.1", MessageTS: "100.001",
		SourceEventID: "Ev1", UserID: "U2", Text: "fix the bug in this screenshot",
		Files: []slack.SharedFile{imageFile("F1")}}

	if err := HandleEvent(context.Background(), deps, ev); err != nil {
		t.Fatalf("HandleEvent() error = %v", err)
	}

	if len(up.uploaded) != 1 {
		t.Fatalf("the uploader received %d artifacts, want 1 — the screenshot never reached Palai", len(up.uploaded))
	}
	if string(up.uploaded[0]) != string(pngBytes) {
		t.Fatalf("the uploader received %d bytes, want the %d fetched from Slack", len(up.uploaded[0]), len(pngBytes))
	}
	if !strings.HasPrefix(files.gotBearer, "Bearer ") {
		t.Fatalf("the file fetch presented Authorization %q, want a Bearer token", files.gotBearer)
	}

	items, ok := fp.lastCreateReq.Input.([]any)
	if !ok {
		t.Fatalf("the run input is %T, want a content array — a bare string cannot carry an image_ref", fp.lastCreateReq.Input)
	}
	if len(items) != 2 {
		t.Fatalf("the run input carries %d items, want the text plus one image_ref", len(items))
	}
	first, _ := items[0].(map[string]any)
	if first["type"] != "input_text" || !strings.Contains(fmt.Sprint(first["text"]), "fix the bug") {
		t.Fatalf("the first content item is %v, want the human's own words first", items[0])
	}
	second, _ := items[1].(map[string]any)
	if second["type"] != "image_ref" || second["artifact_id"] != "art_1" {
		t.Fatalf("the second content item is %v, want an image_ref naming the uploaded artifact art_1", items[1])
	}
}

// TestAMessageWithNoFilesIsUnchanged is the guard on everything that already worked: the no-image path must
// send the BARE STRING it always sent, not a one-item content array that happens to be equivalent.
//
// The distinction matters because the two are not equivalent everywhere — a string and an array take
// different branches in execution.decodeMessages — and because "we did not change the working path" is a
// claim that needs a test rather than a reading of the diff.
func TestAMessageWithNoFilesIsUnchanged(t *testing.T) {
	deps, fp, _ := newTestDeps(t)
	up := &fakeUploader{}
	deps = deps.WithImages(&ImageLeg{
		Doer: redirectingDoer{to: "http://127.0.0.1:1"}, Token: []byte("xoxb-secret"), Artifacts: up,
	}, nil)

	ev := slack.Event{Type: "app_mention", Kind: slack.KindMessage,
		TeamID: "T1", ChannelID: "C1", ThreadTS: "1.1", MessageTS: "100.001",
		SourceEventID: "Ev1", UserID: "U2", Text: "just a question"}

	if err := HandleEvent(context.Background(), deps, ev); err != nil {
		t.Fatalf("HandleEvent() error = %v", err)
	}
	if got, ok := fp.lastCreateReq.Input.(string); !ok || got != "just a question" {
		t.Fatalf("the run input is %#v, want the bare string %q unchanged", fp.lastCreateReq.Input, "just a question")
	}
	if up.n != 0 {
		t.Fatalf("a message with no files uploaded %d artifacts, want 0", up.n)
	}
}

// TestAnUnfetchableFileIsSaidNotSwallowed pins the honesty rule: a file the bot could not read must leave a
// COUNTED note in the prompt, and the turn must still happen.
//
// Both halves are the point. Refusing the message would let a user wedge their own conversation with an
// attachment; attaching nothing silently would have the model answer as though the person sent no picture,
// which is the failure mode that is indistinguishable from the bug this whole change fixes.
func TestAnUnfetchableFileIsSaidNotSwallowed(t *testing.T) {
	deps, fp, _ := newTestDeps(t)
	files := newSlackFileServer(t, []byte("nope"), http.StatusForbidden)
	up := &fakeUploader{}
	deps = deps.WithImages(&ImageLeg{
		Doer: redirectingDoer{to: files.srv.URL}, Token: []byte("xoxb-secret"), Artifacts: up,
	}, nil)

	ev := slack.Event{Type: "app_mention", Kind: slack.KindMessage,
		TeamID: "T1", ChannelID: "C1", ThreadTS: "1.1", MessageTS: "100.001",
		SourceEventID: "Ev1", UserID: "U2", Text: "look at this",
		Files: []slack.SharedFile{imageFile("F1")}}

	if err := HandleEvent(context.Background(), deps, ev); err != nil {
		t.Fatalf("HandleEvent() error = %v — an unreadable attachment must not lose the turn", err)
	}
	if fp.responses != 1 {
		t.Fatalf("responses created = %d, want 1 — the run must be born even when the picture cannot be", fp.responses)
	}
	text, ok := fp.lastCreateReq.Input.(string)
	if !ok {
		t.Fatalf("the run input is %T, want the bare string (nothing was attached)", fp.lastCreateReq.Input)
	}
	if !strings.Contains(text, "could not be attached") {
		t.Fatalf("the run input is %q, want it to SAY that a file could not be attached", text)
	}
}

// TestAnUploadFailureIsSaidNotSwallowed is the same honesty rule at the OTHER failure point: the fetch
// succeeded and Palai refused the bytes. It is a separate test because it is a separate branch — an earlier
// draft handled the fetch error and let an upload error fall through as a nil id, which would have put an
// `image_ref` naming "" into the input.
func TestAnUploadFailureIsSaidNotSwallowed(t *testing.T) {
	deps, fp, _ := newTestDeps(t)
	files := newSlackFileServer(t, pngBytes, http.StatusOK)
	up := &fakeUploader{err: errors.New("413 payload too large")}
	deps = deps.WithImages(&ImageLeg{
		Doer: redirectingDoer{to: files.srv.URL}, Token: []byte("xoxb-secret"), Artifacts: up,
	}, nil)

	ev := slack.Event{Type: "app_mention", Kind: slack.KindMessage,
		TeamID: "T1", ChannelID: "C1", ThreadTS: "1.1", MessageTS: "100.001",
		SourceEventID: "Ev1", UserID: "U2", Text: "look at this",
		Files: []slack.SharedFile{imageFile("F1")}}

	if err := HandleEvent(context.Background(), deps, ev); err != nil {
		t.Fatalf("HandleEvent() error = %v", err)
	}
	text, ok := fp.lastCreateReq.Input.(string)
	if !ok {
		t.Fatalf("the run input is %T, want the bare string — a failed upload must not leave an image_ref behind", fp.lastCreateReq.Input)
	}
	if !strings.Contains(text, "could not be attached") {
		t.Fatalf("the run input is %q, want it to say the file could not be attached", text)
	}
}

// TestAFileShareBirthsATurn is the DM upload case, and it is the one the old kind filter dropped on the
// floor: a screenshot dragged into a direct message arrives as `message` with subtype `file_share`
// (KindFileShare), which HandleEvent used to return nil for — so the person who sent it got no answer and
// nothing anywhere said why.
func TestAFileShareBirthsATurn(t *testing.T) {
	deps, fp, _ := newTestDeps(t)
	files := newSlackFileServer(t, pngBytes, http.StatusOK)
	up := &fakeUploader{}
	deps = deps.WithImages(&ImageLeg{
		Doer: redirectingDoer{to: files.srv.URL}, Token: []byte("xoxb-secret"), Artifacts: up,
	}, nil)

	ev := slack.Event{Type: "message", Kind: slack.KindFileShare, ChannelType: "im",
		TeamID: "T1", ChannelID: "D1", ThreadTS: "1.1", MessageTS: "100.001",
		SourceEventID: "Ev1", UserID: "U2", Text: "what is wrong here",
		Files: []slack.SharedFile{imageFile("F1")}}

	if err := HandleEvent(context.Background(), deps, ev); err != nil {
		t.Fatalf("HandleEvent() error = %v", err)
	}
	if fp.responses != 1 {
		t.Fatalf("responses created = %d, want 1 — a file shared in a DM must become a turn", fp.responses)
	}
	if _, ok := fp.lastCreateReq.Input.([]any); !ok {
		t.Fatalf("the run input is %T, want a content array carrying the image", fp.lastCreateReq.Input)
	}
}

// TestAFileShareWithNoFilesBirthsNothing is the OTHER half of admitting KindFileShare, and it is what keeps
// that change as narrow as its comment claims: the kind is admitted because it carries pictures, so one
// carrying none must admit nothing new.
func TestAFileShareWithNoFilesBirthsNothing(t *testing.T) {
	deps, fp, _ := newTestDeps(t)

	ev := slack.Event{Type: "message", Kind: slack.KindFileShare,
		TeamID: "T1", ChannelID: "C1", ThreadTS: "1.1", MessageTS: "100.001",
		SourceEventID: "Ev1", UserID: "U2", Text: "no files here"}

	if err := HandleEvent(context.Background(), deps, ev); err != nil {
		t.Fatalf("HandleEvent() error = %v", err)
	}
	if fp.responses != 0 {
		t.Fatalf("responses created = %d, want 0 — a file_share carrying nothing must not widen what is admitted", fp.responses)
	}
}

// TestImagesAreNotFetchedWithNoLegMounted pins the nil-safe posture: a deployment with no image leg relays
// the message exactly as it did before this file existed — and SAYS the picture was not attached, rather
// than answering as though none was sent.
func TestImagesAreNotFetchedWithNoLegMounted(t *testing.T) {
	deps, fp, _ := newTestDeps(t) // no WithImages call at all

	ev := slack.Event{Type: "app_mention", Kind: slack.KindMessage,
		TeamID: "T1", ChannelID: "C1", ThreadTS: "1.1", MessageTS: "100.001",
		SourceEventID: "Ev1", UserID: "U2", Text: "look",
		Files: []slack.SharedFile{imageFile("F1")}}

	if err := HandleEvent(context.Background(), deps, ev); err != nil {
		t.Fatalf("HandleEvent() error = %v", err)
	}
	text, ok := fp.lastCreateReq.Input.(string)
	if !ok {
		t.Fatalf("the run input is %T, want the bare string", fp.lastCreateReq.Input)
	}
	if !strings.Contains(text, "could not be attached") {
		t.Fatalf("the run input is %q, want the unmounted leg to still SAY the file is not visible", text)
	}
}

// TestOnlyThreeImagesPerMessageAreAttachedAndTheRestAreCounted pins the per-message cap AND its note
// together, because the cap without the note is a silent drop — the model would answer about three
// screenshots while the human attached five, with nothing anywhere saying so.
func TestOnlyThreeImagesPerMessageAreAttachedAndTheRestAreCounted(t *testing.T) {
	deps, fp, _ := newTestDeps(t)
	files := newSlackFileServer(t, pngBytes, http.StatusOK)
	up := &fakeUploader{}
	deps = deps.WithImages(&ImageLeg{
		Doer: redirectingDoer{to: files.srv.URL}, Token: []byte("xoxb-secret"), Artifacts: up,
	}, nil)

	ev := slack.Event{Type: "app_mention", Kind: slack.KindMessage,
		TeamID: "T1", ChannelID: "C1", ThreadTS: "1.1", MessageTS: "100.001",
		SourceEventID: "Ev1", UserID: "U2", Text: "five of them",
		Files: []slack.SharedFile{imageFile("F1"), imageFile("F2"), imageFile("F3"), imageFile("F4"), imageFile("F5")}}

	if err := HandleEvent(context.Background(), deps, ev); err != nil {
		t.Fatalf("HandleEvent() error = %v", err)
	}
	if up.n != maxImagesPerMessage {
		t.Fatalf("uploaded %d artifacts, want the per-message cap of %d", up.n, maxImagesPerMessage)
	}
	items, ok := fp.lastCreateReq.Input.([]any)
	if !ok {
		t.Fatalf("the run input is %T, want a content array", fp.lastCreateReq.Input)
	}
	if len(items) != maxImagesPerMessage+1 {
		t.Fatalf("the run input carries %d items, want the text plus %d images", len(items), maxImagesPerMessage)
	}
	first, _ := items[0].(map[string]any)
	if !strings.Contains(fmt.Sprint(first["text"]), "2 further files") {
		t.Fatalf("the prompt text is %q, want it to COUNT the two files that were left out", first["text"])
	}
}

// TestAPictureAttachedToASteerIsSaidNotDropped pins the ceiling steerImageNote names. A steer's `message`
// is a string on the wire, so an image genuinely cannot ride one — and the honest response to a ceiling is
// to say so in the turn, not to let the file disappear.
//
// The test drives the real steer path: a run is held open so the second message steers rather than starting
// a new run, which is the only state in which this branch is reachable.
func TestAPictureAttachedToASteerIsSaidNotDropped(t *testing.T) {
	deps, fp, _ := newTestDeps(t)
	up := &fakeUploader{}
	deps = deps.WithImages(&ImageLeg{
		Doer: redirectingDoer{to: "http://127.0.0.1:1"}, Token: []byte("xoxb-secret"), Artifacts: up,
	}, nil)

	// Hold the first run open so the second message lands on the steer branch.
	stream := newChannelStream()
	fp.stream = func(context.Context) EventStream { return stream }
	started := make(chan struct{})
	var once sync.Once
	deps.RunInBackground = func(f func()) {
		go func() { once.Do(func() { close(started) }); f() }()
	}

	first := slack.Event{Type: "app_mention", Kind: slack.KindMessage,
		TeamID: "T1", ChannelID: "C1", ThreadTS: "1.1", MessageTS: "100.001",
		SourceEventID: "Ev1", UserID: "U2", Text: "start"}
	if err := HandleEvent(context.Background(), deps, first); err != nil {
		t.Fatalf("HandleEvent(first) error = %v", err)
	}
	<-started

	second := slack.Event{Type: "message", Kind: slack.KindMessage,
		TeamID: "T1", ChannelID: "C1", ThreadTS: "1.1", MessageTS: "100.002",
		SourceEventID: "Ev2", UserID: "U2", Text: "and this one",
		Files: []slack.SharedFile{imageFile("F1")}}
	if err := HandleEvent(context.Background(), deps, second); err != nil {
		t.Fatalf("HandleEvent(second) error = %v", err)
	}
	stream.Close()

	if fp.steers != 1 {
		t.Fatalf("steers = %d, want 1 — the second message must steer the still-open run", fp.steers)
	}
	if !strings.Contains(fp.lastSteerMsg, "cannot be attached to a turn that is already running") {
		t.Fatalf("the steered message is %q, want it to SAY the picture cannot be attached", fp.lastSteerMsg)
	}
	if up.n != 0 {
		t.Fatalf("a steered message fetched and uploaded %d artifacts; nothing can reference them", up.n)
	}
}
