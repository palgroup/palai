//go:build component

package store_test

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/palgroup/palai/storage"
)

// THE SLACK IMAGE LEG over the SHIPPED route and real PostgreSQL (E20; SLK-005's deferred file half).
//
// WHAT IS REAL HERE and what is not, stated up front because a proof is worth what its fakes are worth:
//
//   - REAL: the signed Events API delivery, the production router, the production bridge, the real
//     api.Admitter, PostgreSQL, the idempotency reservation, the run-birth rule, the stored responses.input,
//     and the file-host allow-list (the URLs below are genuine https://files.slack.com addresses; only the
//     transport is local, because a 127.0.0.1 host is REFUSED — see the adapter's unit tests).
//   - FAKE: the bytes Slack would serve, and the object store. The object store's real semantics — a
//     caller-chosen idempotent id, the NULL-then-attach ordering, tenant-scoped reads, retention reach — are
//     proven against real S3 + real Postgres in
//     apps/control-plane/internal/artifacts/inbound_image_component_test.go. What THIS file proves is the
//     admission: what gets fetched, what the run's input says, and in what order.

// componentPNG is a real 1x1 PNG. It has to be a real one: the fetch SNIFFS the bytes and refuses anything
// that is not an image, so a fixture serving "fake png" would prove the refusal, not the happy path.
var componentPNG = []byte{
	0x89, 0x50, 0x4E, 0x47, 0x0D, 0x0A, 0x1A, 0x0A, 0x00, 0x00, 0x00, 0x0D, 0x49, 0x48, 0x44, 0x52,
	0x00, 0x00, 0x00, 0x01, 0x00, 0x00, 0x00, 0x01, 0x08, 0x06, 0x00, 0x00, 0x00, 0x1F, 0x15, 0xC4,
	0x89, 0x00, 0x00, 0x00, 0x0A, 0x49, 0x44, 0x41, 0x54, 0x78, 0x9C, 0x63, 0x00, 0x01, 0x00, 0x00,
	0x05, 0x00, 0x01, 0x0D, 0x0A, 0x2D, 0xB4, 0x00, 0x00, 0x00, 0x00, 0x49, 0x45, 0x4E, 0x44, 0xAE,
	0x42, 0x60, 0x82,
}

// fileFetch is one request the fake file host saw.
type fileFetch struct {
	url   string
	auth  string
	query string
}

// fakeSlackFileHost stands in for files.slack.com as a Doer. It answers whatever URL it is given and RECORDS
// the request, so a test can assert both what was fetched and — the load-bearing half — exactly where the bot
// token went.
type fakeSlackFileHost struct {
	mu      sync.Mutex
	fetches []fileFetch
	content []byte
	status  int // 0 ⇒ 200
}

func (h *fakeSlackFileHost) Do(r *http.Request) (*http.Response, error) {
	h.mu.Lock()
	h.fetches = append(h.fetches, fileFetch{url: r.URL.String(), auth: r.Header.Get("Authorization"), query: r.URL.RawQuery})
	status, content := h.status, h.content
	h.mu.Unlock()
	if status == 0 {
		status = http.StatusOK
	}
	return &http.Response{StatusCode: status, Body: io.NopCloser(strings.NewReader(string(content))), Header: http.Header{}}, nil
}

func (h *fakeSlackFileHost) seen() []fileFetch {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]fileFetch(nil), h.fetches...)
}

// inboundWrite is one artifact the bridge asked to persist.
type inboundWrite struct {
	org, project, id string
	mediaType        string
	size             int
	provenance       map[string]any
}

// recordingInboundArtifacts stands in for the object store and records the ORDER of the two calls, which is
// the whole ordering argument: the write has to happen before the admission (so the row exists before the
// run's dispatch outbox commits) and the attach after it (so retention can reach the bytes).
type recordingInboundArtifacts struct {
	mu       sync.Mutex
	order    []string
	writes   []inboundWrite
	attaches map[string]string
	failNext bool
	// The OUTBOUND half (E22 T5): artifacts runs produced, and what the upload leg asked for. One fake serves
	// both directions because one artifacts.Writer does in production.
	runArtifacts map[string]runArtifact
	reads        []string
	readErr      error
}

func (r *recordingInboundArtifacts) WriteInboundArtifact(_ context.Context, project, id string, content []byte, mediaType string, provenance map[string]any) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.failNext {
		r.failNext = false
		return fmt.Errorf("object store is unavailable")
	}
	r.order = append(r.order, "write:"+id)
	r.writes = append(r.writes, inboundWrite{project: project, id: id, mediaType: mediaType, size: len(content), provenance: provenance})
	return nil
}

func (r *recordingInboundArtifacts) AttachArtifactRun(_ context.Context, _, artifactID, runID string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.attaches == nil {
		r.attaches = map[string]string{}
	}
	r.order = append(r.order, "attach:"+artifactID)
	r.attaches[artifactID] = runID
	return nil
}

// runArtifact is one artifact a RUN produced, as the outbound leg reads them back (E22 T5). It is keyed by
// run as well as id because that pairing IS the guard under test: an artifact belonging to another run must
// not be publishable into this thread.
type runArtifact struct {
	runID   string
	content []byte
	// size overrides len(content) so an over-ceiling artifact can be expressed without allocating one. The
	// production store reads the size off the ROW for exactly this reason — it refuses before it reads.
	size int64
}

// ReadRunArtifact stands in for artifacts.Writer.ReadRunArtifact and enforces the same three keys: the
// tenant, the run, and the ceiling. The guard against a REAL Postgres row lives in the artifacts component
// suite; this fake exists so the Slack suite can prove the PUMP's behaviour without an object store.
func (r *recordingInboundArtifacts) ReadRunArtifact(_ context.Context, project, runID, artifactID string, maxBytes int64) ([]byte, int64, bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.reads = append(r.reads, artifactID)
	if r.readErr != nil {
		return nil, 0, false, r.readErr
	}
	art, ok := r.runArtifacts[org+"/"+project+"/"+artifactID]
	if !ok || art.runID != runID {
		return nil, 0, false, nil // unknown, another tenant's, or another run's: one indistinguishable miss
	}
	size := art.size
	if size == 0 {
		size = int64(len(art.content))
	}
	if size > maxBytes {
		return nil, size, true, nil // exists, too big, no bytes — the seam's documented over-ceiling answer
	}
	return art.content, size, true, nil
}

// putRunArtifact seeds one artifact a run produced.
func (r *recordingInboundArtifacts) putRunArtifact(org, project, runID, artifactID string, content []byte, size int64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.runArtifacts == nil {
		r.runArtifacts = map[string]runArtifact{}
	}
	r.runArtifacts[org+"/"+project+"/"+artifactID] = runArtifact{runID: runID, content: content, size: size}
}

// readIDs reports which artifact ids the upload leg asked for, so a test can prove it asked for NONE.
func (r *recordingInboundArtifacts) readIDs() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.reads...)
}

func (r *recordingInboundArtifacts) snapshot() ([]string, []inboundWrite, map[string]string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	attaches := map[string]string{}
	for k, v := range r.attaches {
		attaches[k] = v
	}
	return append([]string(nil), r.order...), append([]inboundWrite(nil), r.writes...), attaches
}

// eventFileShare builds a signed-ready file_share delivery carrying `files`.
func (f *slackFixture) eventFileShare(eventID, channel, ts, text string, files ...map[string]any) []byte {
	inner := map[string]any{
		"type": "message", "subtype": "file_share", "user": "Umapped",
		"channel": channel, "ts": ts, "text": text, "files": files,
	}
	raw, _ := json.Marshal(map[string]any{
		"type": "event_callback", "team_id": f.team, "api_app_id": "A0001",
		"event_id": eventID, "event_time": 1700000000, "event": inner,
	})
	return raw
}

// slackFile is one file object as Slack publishes it (the subset the mapping reads).
func slackFile(id, name, mime string, size int) map[string]any {
	return map[string]any{
		"id": id, "name": name, "mimetype": mime, "filetype": strings.TrimPrefix(mime, "image/"), "size": size,
		"url_private_download": "https://files.slack.com/files-pri/T1-" + id + "/download/" + name,
	}
}

// storedInput reads the one stored run input for this fixture's tenant.
func (f *slackFixture) storedInput(t *testing.T) string {
	t.Helper()
	var input string
	if err := f.pool.QueryRow(storage.WithSystemScope(context.Background()),
		`SELECT input::text FROM responses WHERE organization_id=$1 AND project_id=$2 ORDER BY created_at LIMIT 1`,
		f.org, f.project).Scan(&input); err != nil {
		t.Fatalf("read the stored run input: %v", err)
	}
	return input
}

// TestSlackSharedImageBecomesAnImageRefInTheRunInput is the end-to-end admission proof: a DM carrying a
// screenshot is fetched with the bot token, persisted, and the stored run input is the CONTENT ARRAY naming
// it — through the shipped route, against real PostgreSQL.
//
// It also pins the ORDER, which is the part a reader would otherwise have to take on trust: write, then
// attach. Reversed, the artifact row would not exist when the model step reads it (the admission commits the
// dispatch outbox in its own transaction); with the attach missing, retention would never reach the bytes.
func TestSlackSharedImageBecomesAnImageRefInTheRunInput(t *testing.T) {
	f := newSlackFixture(t)

	// A DM, so the run-birth rule admits it without a mention (Slack's own channel_type is the authority).
	body := f.eventFileShare("EvImg1", "D90", "1700000090.000100", "ne yazıyor burada",
		slackFile("F1", "shot.png", "image/png", len(componentPNG)))
	body = withChannelType(body, "im")
	f.deliver(t, body, time.Now(), "", "").Body.Close()

	if got := f.runCount(t); got != 1 {
		t.Fatalf("runs = %d, want 1", got)
	}

	// THE FETCH: one request, to the file's real Slack URL, with the bot token in the header and nowhere else.
	fetches := f.fileHost.seen()
	if len(fetches) != 1 {
		t.Fatalf("file fetches = %d (%v), want exactly 1", len(fetches), fetches)
	}
	if fetches[0].auth != "Bearer "+string(f.botToken) {
		t.Fatalf("Authorization = %q, want the bot token as a bearer header", fetches[0].auth)
	}
	if strings.Contains(fetches[0].url, string(f.botToken)) || fetches[0].query != "" {
		t.Fatalf("fetch url = %q — the token must never ride the URL", fetches[0].url)
	}

	// THE ARTIFACT: written once, with the SNIFFED media type and the real byte count.
	order, writes, attaches := f.artifacts.snapshot()
	if len(writes) != 1 {
		t.Fatalf("artifact writes = %d (%v), want 1", len(writes), writes)
	}
	w := writes[0]
	if w.org != f.org || w.project != f.project {
		t.Fatalf("artifact written into %s/%s, want the connection's own tenant %s/%s", w.org, w.project, f.org, f.project)
	}
	if w.mediaType != "image/png" || w.size != len(componentPNG) {
		t.Fatalf("artifact = %s/%d bytes, want image/png and %d", w.mediaType, w.size, len(componentPNG))
	}
	if w.provenance["slack_file_id"] != "F1" || w.provenance["source"] != "slack" {
		t.Fatalf("provenance = %v, want it to trace back to Slack file F1", w.provenance)
	}

	// THE ORDER: write before attach, and the attach names the run that was actually born.
	if len(order) != 2 || !strings.HasPrefix(order[0], "write:") || !strings.HasPrefix(order[1], "attach:") {
		t.Fatalf("call order = %v, want the write before the attach", order)
	}
	var bornRun string
	if err := f.pool.QueryRow(storage.WithSystemScope(context.Background()),
		`SELECT id FROM runs WHERE organization_id=$1 AND project_id=$2`, f.org, f.project).Scan(&bornRun); err != nil {
		t.Fatalf("read the born run: %v", err)
	}
	if attaches[w.id] != bornRun {
		t.Fatalf("artifact attached to run %q, want the born run %q", attaches[w.id], bornRun)
	}

	// THE INPUT: a content array whose first item is the human's words and whose second names the artifact.
	input := f.storedInput(t)
	var items []map[string]any
	if err := json.Unmarshal([]byte(input), &items); err != nil {
		t.Fatalf("responses.input is not a content array: %s", input)
	}
	if len(items) != 2 {
		t.Fatalf("input items = %d: %s", len(items), input)
	}
	if items[0]["type"] != "input_text" || items[0]["text"] != "ne yazıyor burada" {
		t.Fatalf("first item = %v, want the human's words: %s", items[0], input)
	}
	if items[1]["type"] != "image_ref" || items[1]["artifact_id"] != w.id {
		t.Fatalf("second item = %v, want image_ref %s: %s", items[1], w.id, input)
	}
	// The bytes are NOT in the input, and cannot be: run.start carries this value to the engine over a 1 MiB
	// frame. And no scope, no filename, no URL either.
	for _, leaked := range []string{"base64", "data:image", "shot.png", "files.slack.com", "F1", f.team, "D90", "Umapped", string(f.botToken)} {
		if strings.Contains(input, leaked) {
			t.Fatalf("responses.input carries %q: %s", leaked, input)
		}
	}
}

// TestSlackImageRedeliveryReplaysOntoTheSameRunAndArtifact is SLK-002 with an image in it, and it is the
// reason the artifact id is DERIVED rather than minted: the id rides the input, the input is hashed into the
// idempotency reservation, and a fresh id on the retry would make the reservation report a CONFLICT instead of
// replaying. One run, one artifact id, however many times Slack delivers it.
func TestSlackImageRedeliveryReplaysOntoTheSameRunAndArtifact(t *testing.T) {
	f := newSlackFixture(t)
	body := withChannelType(f.eventFileShare("EvImg2", "D91", "1700000091.000100", "bak",
		slackFile("F7", "shot.png", "image/png", len(componentPNG))), "im")

	for i := range 3 {
		resp := f.deliver(t, body, time.Now(), fmt.Sprint(i), "http_timeout")
		resp.Body.Close()
		if resp.StatusCode/100 != 2 {
			t.Fatalf("delivery %d = HTTP %d, want a 2xx (a redelivery must replay, not conflict)", i, resp.StatusCode)
		}
	}

	if got := f.runCount(t); got != 1 {
		t.Fatalf("runs = %d after 3 deliveries, want 1", got)
	}
	_, writes, attaches := f.artifacts.snapshot()
	ids := map[string]bool{}
	for _, w := range writes {
		ids[w.id] = true
	}
	if len(ids) != 1 {
		t.Fatalf("artifact ids across the redeliveries = %v, want exactly one derived id", ids)
	}
	// The attach ran ONCE, on the original: a replay's runID was never inserted into `runs`, so attaching to
	// it would violate the artifacts.run_id foreign key.
	if len(attaches) != 1 {
		t.Fatalf("attaches = %v, want exactly one (the replays must not re-attach)", attaches)
	}
}

// TestSlackNonImageAndOversizeFilesAreRefusedVisibly proves every refusal reaches the PROMPT. A silent drop
// is the defect this whole leg exists to fix: the user is told the model cannot see something, with no
// indication that anything was even skipped.
func TestSlackNonImageAndOversizeFilesAreRefusedVisibly(t *testing.T) {
	f := newSlackFixture(t)
	body := withChannelType(f.eventFileShare("EvImg3", "D92", "1700000092.000100", "bunlara bak",
		slackFile("F1", "notes.pdf", "application/pdf", 100),     // not an image: never fetched
		slackFile("F2", "huge.png", "image/png", 50<<20),         // declares 50 MiB: refused before the fetch
		slackFile("F3", "a.png", "image/png", len(componentPNG)), // fetched
		slackFile("F4", "b.png", "image/png", len(componentPNG)), // fetched
		slackFile("F5", "c.png", "image/png", len(componentPNG)), // fetched
		slackFile("F6", "d.png", "image/png", len(componentPNG)), // past the per-message cap of 3
	), "im")
	f.deliver(t, body, time.Now(), "", "").Body.Close()

	// Only the declared images under the cap were fetched — the pdf and the 50 MiB claim cost no transfer.
	fetches := f.fileHost.seen()
	if len(fetches) != 3 {
		t.Fatalf("file fetches = %d (%v), want 3: the pdf is not fetched, the oversize claim is refused first, and the cap stops the rest", len(fetches), fetches)
	}
	for _, fetch := range fetches {
		if strings.Contains(fetch.url, "notes.pdf") || strings.Contains(fetch.url, "huge.png") {
			t.Fatalf("fetched %q — a non-image and an over-size file must never be transferred", fetch.url)
		}
	}

	input := f.storedInput(t)
	var items []map[string]any
	if err := json.Unmarshal([]byte(input), &items); err != nil {
		t.Fatalf("input is not a content array: %s", input)
	}
	if len(items) != 4 {
		t.Fatalf("input items = %d, want the text plus 3 image refs: %s", len(items), input)
	}
	text, _ := items[0]["text"].(string)
	if !strings.Contains(text, "3 further files") || !strings.Contains(text, "not visible") {
		t.Fatalf("input text = %q, want it to say 3 further files were not attached", text)
	}
	// The prompt says HOW MANY, never WHICH: a filename is uploader-controlled text.
	for _, leaked := range []string{"notes.pdf", "huge.png", "F1", "F2", "F6"} {
		if strings.Contains(input, leaked) {
			t.Fatalf("input carries %q — the refusal is counted, not itemised: %s", leaked, input)
		}
	}
}

// A file the fetch cannot GET must not fail the admission: the run is born, and the input says an image is
// missing. Refusing the message instead would let a Slack user wedge their own conversation with one upload.
func TestSlackAFailedFetchStillBirthsTheRunAndSaysSo(t *testing.T) {
	f := newSlackFixture(t)
	f.fileHost.status = http.StatusForbidden
	body := withChannelType(f.eventFileShare("EvImg4", "D93", "1700000093.000100", "bak",
		slackFile("F1", "shot.png", "image/png", len(componentPNG))), "im")
	f.deliver(t, body, time.Now(), "", "").Body.Close()

	if got := f.runCount(t); got != 1 {
		t.Fatalf("runs = %d, want 1 — a fetch failure must not swallow the message", got)
	}
	if _, writes, _ := f.artifacts.snapshot(); len(writes) != 0 {
		t.Fatalf("artifact writes = %v, want none when the fetch failed", writes)
	}
	// With nothing attached the input is a plain STRING again, and it says a file could not be attached.
	input := f.storedInput(t)
	if !strings.HasPrefix(input, `"`) {
		t.Fatalf("input = %s, want a bare JSON string when no image was attached", input)
	}
	if !strings.Contains(input, "could not be attached") {
		t.Fatalf("input = %s, want it to say the file could not be attached", input)
	}
}

// THE REGRESSION GUARD, and it is why the image leg is mounted on every fixture: a delivery with NO files
// must touch neither the file host nor the object store, and must store the same bare string it always did.
func TestSlackAFileLessDeliveryFetchesNothing(t *testing.T) {
	f := newSlackFixture(t)
	f.deliver(t, f.eventText(t, "EvImg5", "app_mention", "Umapped", "C94", "1700000094.000100", "",
		"<@"+f.botUser+"> merhaba"), time.Now(), "", "").Body.Close()

	if fetches := f.fileHost.seen(); len(fetches) != 0 {
		t.Fatalf("file fetches = %v, want none for a message with no files", fetches)
	}
	if order, _, _ := f.artifacts.snapshot(); len(order) != 0 {
		t.Fatalf("object-store calls = %v, want none", order)
	}
	if got := f.storedInput(t); got != `"merhaba"` {
		t.Fatalf("responses.input = %s, want the unchanged bare string \"merhaba\"", got)
	}
}

// TestSlackEditingACaptionKeepsTheImageAndDeletingTheTurnTakesItAway is where E20's image leg meets ff67139's
// retraction: an image is part of a TURN, so what happens to the turn happens to the image. The two verbs pull
// in opposite directions and both are asserted through the SHIPPED history query, which is what the provider
// is actually assembled from — a column read would prove a flag was written, not that a picture stopped (or
// kept) reaching the model.
//
//   - AN EDIT changes the words and nothing else. The file is still in the thread and the human can still see
//     it, so a rewrite that blanked the content array would make the model blind to the very thing being
//     discussed.
//   - A DELETION takes the whole turn, image included. This one matters most: model_dispatch resolves
//     image_refs across the WHOLE conversation, so a retracted screenshot left in history would be re-sent to
//     the provider on every later turn in the thread.
func TestSlackEditingACaptionKeepsTheImageAndDeletingTheTurnTakesItAway(t *testing.T) {
	f := newSlackFixture(t)
	const shot, followUp = "1700000096.000100", "1700000096.000200"

	// A DM carrying a screenshot, then a SECOND turn in the same thread: history is "everything before this
	// response", so without a later turn there is no vantage point to prove anything from.
	f.deliver(t, withChannelType(f.eventFileShare("EvImg6", "D96", shot, "bunda ne hata var",
		slackFile("F6", "shot.png", "image/png", len(componentPNG))), "im"), time.Now(), "", "").Body.Close()
	f.answerRuns(t, "ilk cevap")
	f.deliver(t, withChannelType(f.eventText(t, "EvImg7", "message", "Umapped", "D96", followUp, shot,
		"peki şimdi?"), "im"), time.Now(), "", "").Body.Close()
	f.answerRuns(t, "ikinci cevap")

	_, writes, _ := f.artifacts.snapshot()
	if len(writes) != 1 {
		t.Fatalf("artifact writes = %d, want the one screenshot", len(writes))
	}
	artifact := writes[0].id
	ids, inputs := f.responseIDs(t)
	if len(ids) != 2 {
		t.Fatalf("the thread holds %v, want two turns", inputs)
	}
	session, second := f.threadSessionID(t), ids[1]

	// POSITIVE CONTROL, without which everything below is vacuous: the image IS in the history the second run
	// was shown, named by the artifact the fetch wrote.
	prior := f.modelHistory(t, session, second)
	if len(prior) != 1 || !strings.Contains(string(prior[0].Input), artifact) {
		t.Fatalf("the second run's history = %v, want the first turn carrying image_ref %s", prior, artifact)
	}

	// 1. THE EDIT. The caption is superseded; the image_ref survives it.
	f.deliver(t, mustJSON(map[string]any{
		"type": "event_callback", "team_id": f.team, "event_id": "EvImg8",
		"event": map[string]any{"type": "message", "subtype": "message_changed", "channel": "D96",
			"channel_type": "im",
			"message":      map[string]any{"user": "Umapped", "ts": shot, "thread_ts": shot, "text": "hangi renk yanlış"}},
	}), time.Now(), "", "").Body.Close()

	if n := f.runCount(t); n != 2 {
		t.Fatalf("the edit brought the run total to %d, want 2 — a correction supersedes a turn", n)
	}
	prior = f.modelHistory(t, session, second)
	if len(prior) != 1 {
		t.Fatalf("history after the edit = %v, want the one superseded turn", prior)
	}
	var items []map[string]any
	if err := json.Unmarshal(prior[0].Input, &items); err != nil {
		t.Fatalf("the superseded turn is no longer a content array (%s) — the edit took the image out with the words", prior[0].Input)
	}
	if len(items) != 2 || items[0]["text"] != "(edited) hangi renk yanlış" {
		t.Fatalf("the superseded turn = %v, want the corrected caption as item 0", items)
	}
	if items[1]["type"] != "image_ref" || items[1]["artifact_id"] != artifact {
		t.Fatalf("the superseded turn's second item = %v, want image_ref %s — editing a caption does not un-share the file", items[1], artifact)
	}

	// 2. THE DELETION. The turn goes, and the image goes with it.
	f.deliver(t, mustJSON(map[string]any{
		"type": "event_callback", "team_id": f.team, "event_id": "EvImg9",
		"event": map[string]any{"type": "message", "subtype": "message_deleted", "channel": "D96",
			"channel_type":     "im",
			"previous_message": map[string]any{"user": "Umapped", "ts": shot, "thread_ts": shot, "text": "hangi renk yanlış"}},
	}), time.Now(), "", "").Body.Close()

	if n := f.runCount(t); n != 2 {
		t.Fatalf("the deletion brought the run total to %d, want 2 — a deletion answers nothing", n)
	}
	if prior := f.modelHistory(t, session, second); len(prior) != 0 {
		t.Fatalf("history after the deletion = %v, want nothing — the image must leave with the turn that carried it", prior)
	}
	// The bytes are NOT deleted, and that is deliberate: the artifact belongs to a run that really happened and
	// leaves through retention (§22.2), which reaches it because the admission attached it to that run.
	if _, _, attaches := f.artifacts.snapshot(); attaches[artifact] == "" {
		t.Fatalf("artifact %s is unattached after the retraction — retention would never reach the bytes", artifact)
	}
}

// withChannelType stamps channel_type onto an already-built inner event, so a fixture body can be a DM
// (Slack's own field is the ONLY authority for that — never the "D…" id prefix).
func withChannelType(body []byte, channelType string) []byte {
	var envelope map[string]any
	if err := json.Unmarshal(body, &envelope); err != nil {
		panic(err)
	}
	envelope["event"].(map[string]any)["channel_type"] = channelType
	out, err := json.Marshal(envelope)
	if err != nil {
		panic(err)
	}
	return out
}
