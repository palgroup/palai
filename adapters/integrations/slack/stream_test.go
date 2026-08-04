package slack

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
)

// recordingPeer is a Slack Web API stand-in that keeps every call's URL and body, so a test asserts the wire
// shape against the PUBLISHED argument list rather than against our own builder.
type recordingPeer struct {
	bodies []string
	urls   []string
	auths  []string
	// reply is the envelope every call answers with (default: an ok with a ts).
	reply string
}

func (p *recordingPeer) Do(r *http.Request) (*http.Response, error) {
	body, _ := io.ReadAll(r.Body)
	p.bodies = append(p.bodies, string(body))
	p.urls = append(p.urls, r.URL.String())
	p.auths = append(p.auths, r.Header.Get("Authorization"))
	reply := p.reply
	if reply == "" {
		reply = `{"ok":true,"channel":"C1","ts":"1700000000.000100"}`
	}
	return &http.Response{StatusCode: http.StatusOK, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(reply))}, nil
}

func (p *recordingPeer) decode(t *testing.T, i int) map[string]any {
	t.Helper()
	var got map[string]any
	if err := json.Unmarshal([]byte(p.bodies[i]), &got); err != nil {
		t.Fatalf("decode call %d body %q: %v", i, p.bodies[i], err)
	}
	return got
}

// S9: recipient_user_id and recipient_team_id are "Required when streaming to channels", and every Slack run
// this tree admits IS a channel thread. Missing either one is FAIL-CLOSED — no call is made at all, so the
// caller falls back to a plain post instead of burning a Tier-2 request on a request Slack will refuse with
// missing_recipient_user_id.
func TestStartStreamRefusesWithoutARecipient(t *testing.T) {
	peer := &recordingPeer{}
	for _, missing := range []StreamStart{
		{Channel: "C1", ThreadTS: "1.1", RecipientTeamID: "T1"},
		{Channel: "C1", ThreadTS: "1.1", RecipientUserID: "U1"},
		{Channel: "C1", ThreadTS: "1.1"},
		{Channel: "C1", RecipientUserID: "U1", RecipientTeamID: "T1"}, // thread_ts is REQUIRED
	} {
		if _, err := StartStream(context.Background(), peer, "https://slack.test/api", []byte("xoxb"), missing); err == nil {
			t.Fatalf("StartStream(%+v) succeeded; a channel stream without a recipient (or a thread) must refuse", missing)
		}
	}
	if len(peer.bodies) != 0 {
		t.Fatalf("fail-closed made %d call(s) to Slack, want 0", len(peer.bodies))
	}
}

func TestStartStreamCarriesTheDocumentedArguments(t *testing.T) {
	peer := &recordingPeer{}
	ts, err := StartStream(context.Background(), peer, "https://slack.test/api", []byte("xoxb-token"), StreamStart{
		Channel: "C1", ThreadTS: "1700000000.000100",
		RecipientUserID: "U1", RecipientTeamID: "T1", MarkdownText: "working…",
	})
	if err != nil {
		t.Fatalf("StartStream: %v", err)
	}
	if ts != "1700000000.000100" {
		t.Fatalf("returned ts = %q, want the ts Slack assigned the streaming message", ts)
	}
	if peer.urls[0] != "https://slack.test/api/chat.startStream" {
		t.Fatalf("posted to %q, want chat.startStream", peer.urls[0])
	}
	if peer.auths[0] != "Bearer xoxb-token" {
		t.Fatalf("auth = %q, want the token on the Authorization header only", peer.auths[0])
	}
	got := peer.decode(t, 0)
	for field, want := range map[string]any{
		"channel": "C1", "thread_ts": "1700000000.000100",
		"recipient_user_id": "U1", "recipient_team_id": "T1", "markdown_text": "working…",
	} {
		if got[field] != want {
			t.Fatalf("startStream body %s = %v, want %v (body %q)", field, got[field], want, peer.bodies[0])
		}
	}
}

// S8: markdown_text is capped at 12,000 characters, and the cut is NOT silent — a reader who cannot tell a
// truncated answer from a complete one has been told something false.
func TestStreamTextIsTruncatedVisibly(t *testing.T) {
	long := strings.Repeat("ü", MaxStreamMarkdown+500) // multi-byte on purpose: the limit is CHARACTERS
	got := TruncateMarkdown(long)
	if n := len([]rune(got)); n != MaxStreamMarkdown {
		t.Fatalf("truncated to %d characters, want exactly %d", n, MaxStreamMarkdown)
	}
	if !strings.HasSuffix(got, "…") {
		t.Fatalf("truncated text does not end with the ellipsis marker: %q", got[len(got)-8:])
	}
	if short := TruncateMarkdown("fits"); short != "fits" {
		t.Fatalf("text within the limit was altered: %q", short)
	}
}

// S11: the user can stop a stream from Slack's own UI. That is a DELIVERY fact, not run control — the caller
// needs the code to tell it apart from a transport failure, so the API error is typed rather than a string.
func TestAppendStreamSurfacesStoppedByUser(t *testing.T) {
	peer := &recordingPeer{reply: `{"ok":false,"error":"stopped_by_user"}`}
	err := AppendStream(context.Background(), peer, "https://slack.test/api", []byte("xoxb"), "C1", "1.1", "more")
	if err == nil {
		t.Fatal("AppendStream on a stopped stream returned nil; the caller must be able to stop appending")
	}
	if code := APIErrorCode(err); code != CodeStoppedByUser {
		t.Fatalf("APIErrorCode = %q, want %q — an untyped error cannot be told from a transport failure", code, CodeStoppedByUser)
	}
	if code := APIErrorCode(context.Canceled); code != "" {
		t.Fatalf("APIErrorCode on a non-API error = %q, want empty", code)
	}
}

// S12 is an UNRESOLVED VENDOR CONTRADICTION and the conservative reading is enforced by the SIGNATURES, not
// by a comment: chat.stopStream takes blocks, chat.appendStream cannot be given any. This test pins the
// asymmetry so widening it later is a deliberate act with a live measurement behind it.
func TestOnlyStopStreamCarriesBlocks(t *testing.T) {
	peer := &recordingPeer{}
	if err := AppendStream(context.Background(), peer, "https://slack.test/api", []byte("x"), "C1", "1.1", "step done"); err != nil {
		t.Fatalf("AppendStream: %v", err)
	}
	if _, ok := peer.decode(t, 0)["blocks"]; ok {
		t.Fatalf("appendStream body carried blocks: %q", peer.bodies[0])
	}
	if peer.urls[0] != "https://slack.test/api/chat.appendStream" {
		t.Fatalf("append posted to %q, want chat.appendStream", peer.urls[0])
	}

	blocks := json.RawMessage(`[{"type":"section","text":{"type":"mrkdwn","text":"done"}}]`)
	if err := StopStream(context.Background(), peer, "https://slack.test/api", []byte("x"), "C1", "1.1", "the answer", blocks); err != nil {
		t.Fatalf("StopStream: %v", err)
	}
	stop := peer.decode(t, 1)
	if peer.urls[1] != "https://slack.test/api/chat.stopStream" {
		t.Fatalf("stop posted to %q, want chat.stopStream", peer.urls[1])
	}
	if stop["markdown_text"] != "the answer" {
		t.Fatalf("stopStream markdown_text = %v, want the final text", stop["markdown_text"])
	}
	if _, ok := stop["blocks"]; !ok {
		t.Fatalf("stopStream dropped the blocks it is documented to accept: %q", peer.bodies[1])
	}
	// A nil blocks argument omits the field entirely rather than sending a null Slack would reject.
	if err := StopStream(context.Background(), peer, "https://slack.test/api", []byte("x"), "C1", "1.1", "plain", nil); err != nil {
		t.Fatalf("StopStream without blocks: %v", err)
	}
	if _, ok := peer.decode(t, 2)["blocks"]; ok {
		t.Fatalf("stopStream sent a blocks field for a nil argument: %q", peer.bodies[2])
	}
}

// The 12,000-character cap is applied by the CALLS, not only by the exported helper — a caller that forgets
// TruncateMarkdown must not be able to hand Slack an over-long field.
func TestStreamCallsTruncateTheirOwnText(t *testing.T) {
	peer := &recordingPeer{}
	long := strings.Repeat("a", MaxStreamMarkdown+10)
	if _, err := StartStream(context.Background(), peer, "https://slack.test/api", []byte("x"), StreamStart{
		Channel: "C1", ThreadTS: "1.1", RecipientUserID: "U1", RecipientTeamID: "T1", MarkdownText: long,
	}); err != nil {
		t.Fatalf("StartStream: %v", err)
	}
	if err := AppendStream(context.Background(), peer, "https://slack.test/api", []byte("x"), "C1", "1.1", long); err != nil {
		t.Fatalf("AppendStream: %v", err)
	}
	if err := StopStream(context.Background(), peer, "https://slack.test/api", []byte("x"), "C1", "1.1", long, nil); err != nil {
		t.Fatalf("StopStream: %v", err)
	}
	for i := range peer.bodies {
		text, _ := peer.decode(t, i)["markdown_text"].(string)
		if n := len([]rune(text)); n != MaxStreamMarkdown {
			t.Fatalf("call %d sent %d characters, want the 12,000 cap applied at the call", i, n)
		}
	}
}

// A BROADCAST IS AN ACTION, and E20 T1 is the first task to put model-authored text (a task's title) on a
// path of its own into a workspace. `<!channel>` notifies everyone present; nothing in a run's output gets to
// decide that, and it is one prompt injection away.
//
// The check is on the ONE function every streaming call routes its text through and on ThreadReply, which is
// the other — so a future call site inherits the defence instead of having to remember it.
func TestModelTextCannotBroadcast(t *testing.T) {
	hostile := "done <!channel> and <!here> and <!subteam^S1> cc <@U0BADBAD>"
	got := NeutralizeBroadcasts(hostile)
	for _, live := range []string{"<!channel>", "<!here>", "<!subteam^", "<@U0BADBAD>"} {
		if strings.Contains(got, live) {
			t.Fatalf("neutralized text still carries a live %s: %q", live, got)
		}
	}
	// Escaped, not deleted: the reader still sees what the model wrote.
	for _, want := range []string{"&lt;!channel", "&lt;@U0BADBAD"} {
		if !strings.Contains(got, want) {
			t.Fatalf("neutralized text dropped the token instead of escaping it (want %q): %q", want, got)
		}
	}
	if plain := NeutralizeBroadcasts("an ordinary answer with a < and an @"); plain != "an ordinary answer with a < and an @" {
		t.Fatalf("ordinary text was altered: %q", plain)
	}

	// Both outbound paths, not just the helper: the streaming calls and the plain thread reply.
	peer := &recordingPeer{}
	if err := AppendStream(context.Background(), peer, "https://slack.test/api", []byte("x"), "C1", "1.1", hostile); err != nil {
		t.Fatalf("AppendStream: %v", err)
	}
	if text, _ := peer.decode(t, 0)["markdown_text"].(string); strings.Contains(text, "<!channel>") {
		t.Fatalf("a streamed line reached Slack with a live broadcast: %q", text)
	}
	// DECODED, and the decode is the whole assertion. Reading the raw marshalled bytes for `<!channel>` is
	// a check that CANNOT FAIL: encoding/json escapes `<`, `>` and `&` by default, so a live broadcast is
	// spelled `<!channel>` in the body and the substring is absent whether or not
	// NeutralizeBroadcasts ran at all. This leg passed on hostile input for exactly that reason.
	var reply map[string]any
	if err := json.Unmarshal(ThreadReply("C1", "1.1", hostile, "resp_1"), &reply); err != nil {
		t.Fatalf("decode ThreadReply: %v", err)
	}
	if text, _ := reply["markdown_text"].(string); strings.Contains(text, "<!channel>") {
		t.Fatalf("a posted reply reached Slack with a live broadcast: %q", text)
	}
}

// TestTaskUpdateChunkCarriesTheDocumentedShape pins the `task_update` chunk against the shape the LIVE API
// accepts, which is the flat one from the method reference — NOT the nested {"task":{…}} form the agents
// guide prints. The nested form is answered `invalid_arguments` ("failed to match exactly one allowed
// schema [json-pointer:/chunks/0]"), measured 2026-08-04; see TaskUpdateChunk's own contract note.
//
// The distinction this test really guards is the pair of key names, because getting either wrong fails at
// the wire and nowhere earlier: `id` (not `task_id`, which is the BLOCK's spelling) and a `details` STRING
// (not the block's rich_text object).
func TestTaskUpdateChunkCarriesTheDocumentedShape(t *testing.T) {
	chunk := TaskUpdateChunk(Task{
		ID: "tc_1", Title: "Reading files", Status: "in_progress", Detail: "palai.workspace.file",
	})
	// Round-trip through JSON: what Slack receives is the encoded form, so the assertion is made against
	// that rather than against the Go map a caller happens to hold.
	raw, err := json.Marshal(chunk)
	if err != nil {
		t.Fatalf("marshal chunk: %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("decode chunk: %v", err)
	}
	for field, want := range map[string]any{
		"type": "task_update", "id": "tc_1", "title": "Reading files",
		"status": "in_progress", "details": "palai.workspace.file",
	} {
		if got[field] != want {
			t.Fatalf("chunk.%s = %v, want %v — the whole chunk was %s", field, got[field], want, raw)
		}
	}
	// The BLOCK's spellings must not appear: sending them is what the live API refuses.
	if _, present := got["task"]; present {
		t.Fatalf("the chunk nests its fields under `task`, which the live API refuses: %s", raw)
	}
	if _, present := got["task_id"]; present {
		t.Fatalf("the chunk uses the block's `task_id` rather than the chunk's `id`: %s", raw)
	}
	// A task with nothing to title it is not a card Slack can draw, so it is refused rather than sent.
	if TaskUpdateChunk(Task{Status: "done"}) != nil {
		t.Fatal("a task with neither title nor id produced a chunk; want nil")
	}
	// An empty detail is OMITTED, and that is load-bearing rather than tidy: `details` APPENDS across
	// updates of one id, so a caller that has nothing new to add must send no key at all — sending "" or
	// repeating the old value is how a card ends up reading `palai.workspace.filepalai.workspace.file`.
	done := TaskUpdateChunk(Task{ID: "tc_1", Title: "Reading files", Status: "done"})
	if _, present := done["details"]; present {
		t.Fatalf("a task with no new detail still sent `details`: %v", done)
	}
	if done["status"] != "complete" {
		t.Fatalf("status = %v, want complete", done["status"])
	}
}

// TestMarkdownChunkUsesTheTextKey: the markdown chunk's field is `text`. The agents guide's example calls it
// `markdown_text` and the live API refuses that, which is the same page disagreement as the task shape and
// fails in exactly the same invisible way — at the wire, on a call that looks right.
func TestMarkdownChunkUsesTheTextKey(t *testing.T) {
	chunk := MarkdownChunk("Projede toplam 7 Swift dosyası var")
	if chunk["type"] != "markdown_text" {
		t.Fatalf("chunk type = %v, want markdown_text", chunk["type"])
	}
	if chunk["text"] != "Projede toplam 7 Swift dosyası var" {
		t.Fatalf("chunk.text = %v, want the text", chunk["text"])
	}
	if _, present := chunk["markdown_text"]; present {
		t.Fatalf("the chunk carries a `markdown_text` key, which the live API refuses: %v", chunk)
	}
	// The chunk path is not a way around the budget or the broadcast rule.
	if got := MarkdownChunk("ping <!channel>")["text"].(string); strings.Contains(got, "<!channel>") {
		t.Fatalf("a markdown chunk carried a live broadcast: %q", got)
	}
	if got := MarkdownChunk(strings.Repeat("x", MaxStreamMarkdown+500))["text"].(string); len([]rune(got)) > MaxStreamMarkdown {
		t.Fatalf("a markdown chunk carried %d characters, over the %d budget", len([]rune(got)), MaxStreamMarkdown)
	}
}

// TestATaskCardCannotBroadcast: a card's title and output are strings a MODEL had a hand in (the detail
// carries a tool's own progress message), so they go through the same defusing every other outbound string
// does. The assertion is made on the DECODED payload — see the note in TestModelTextCannotBroadcast for why
// a raw-bytes check here would be vacuous.
func TestATaskCardCannotBroadcast(t *testing.T) {
	raw, err := json.Marshal(TaskUpdateChunk(Task{
		ID: "tc_1", Title: "ping <!channel>", Status: "in_progress", Detail: "cc <@U0BADBAD> <!here>",
	}))
	if err != nil {
		t.Fatalf("marshal chunk: %v", err)
	}
	var got struct {
		Title   string `json:"title"`
		Details string `json:"details"`
	}
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("decode chunk: %v", err)
	}
	for _, live := range []string{"<!channel>", "<!here>", "<@U0BADBAD>"} {
		if strings.Contains(got.Title, live) || strings.Contains(got.Details, live) {
			t.Fatalf("a task card carried a live %s (title %q, details %q)", live, got.Title, got.Details)
		}
	}
}

// TestStartStreamPicksTheStreamMode pins the measured mode split, which is the thing about this API that
// fails latest and most confusingly: chat.startStream decides FOR THE STREAM'S WHOLE LIFE whether it speaks
// text or chunks, and the penalty for getting it wrong lands on a later, unrelated-looking call —
// `streaming_mode_mismatch` on an append, or on the close, which leaves the message streaming forever.
//
// So the assertion is that the opening call sends one form and NOT the other. A start that sent both would
// look fine here and in the docs, and would still have picked a mode.
func TestStartStreamPicksTheStreamMode(t *testing.T) {
	peer := &recordingPeer{}
	// Chunk mode: asked for by wanting task cards at all.
	if _, err := StartStream(context.Background(), peer, "https://slack.test/api", []byte("x"), StreamStart{
		Channel: "C1", ThreadTS: "1.1", RecipientUserID: "U1", RecipientTeamID: "T1",
		MarkdownText: "opening words\n", TaskDisplayMode: TaskDisplayModePlan,
	}); err != nil {
		t.Fatalf("StartStream: %v", err)
	}
	chunked := peer.decode(t, 0)
	if got := chunked["task_display_mode"]; got != "plan" {
		t.Fatalf("task_display_mode = %v, want plan", got)
	}
	if _, present := chunked["markdown_text"]; present {
		t.Fatalf("a chunk-mode start also sent markdown_text, which opens a TEXT stream and makes every "+
			"later task card fail: %v", chunked)
	}
	chunks, _ := chunked["chunks"].([]any)
	if len(chunks) != 1 {
		t.Fatalf("a chunk-mode start sent %v for chunks, want the opening text as one markdown chunk", chunked["chunks"])
	}
	if first, _ := chunks[0].(map[string]any); first["type"] != "markdown_text" || first["text"] != "opening words\n" {
		t.Fatalf("the opening chunk = %v, want the opening text as a markdown_text chunk", chunks[0])
	}

	// Text mode: no display mode asked for, so nothing changes for the callers that were here first.
	if _, err := StartStream(context.Background(), peer, "https://slack.test/api", []byte("x"), StreamStart{
		Channel: "C1", ThreadTS: "1.1", RecipientUserID: "U1", RecipientTeamID: "T1", MarkdownText: "working…",
	}); err != nil {
		t.Fatalf("StartStream: %v", err)
	}
	text := peer.decode(t, 1)
	if _, present := text["task_display_mode"]; present {
		t.Fatal("an unset display mode was sent as a field; want it omitted")
	}
	if text["markdown_text"] != "working…" {
		t.Fatalf("a text-mode start sent markdown_text = %v, want the text", text["markdown_text"])
	}
	if _, present := text["chunks"]; present {
		t.Fatalf("a text-mode start also sent chunks: %v", text)
	}
}

// TestAnEmptyOpeningSendsNoChunkAtAll is the property relay.Run depends on to stop prefixing every finished
// answer with a stale progress word.
//
// IT IS ABOUT WHAT IS ABSENT, so it asserts absence rather than an empty string: sending
// {"type":"markdown_text","text":""} would also "open with nothing" and would still be a body chunk, and the
// live API answers ok to both — so a test that only checked the text was empty could not tell the two apart.
// The measured behaviour (2026-08-04, workspace T0AMPM5JX8U) is that a start with NO chunks key keeps a
// message with `text:""` and no blocks at all.
func TestAnEmptyOpeningSendsNoChunkAtAll(t *testing.T) {
	peer := &recordingPeer{}
	if _, err := StartStream(context.Background(), peer, "https://slack.test/api", []byte("x"), StreamStart{
		Channel: "C1", ThreadTS: "1.1", RecipientUserID: "U1", RecipientTeamID: "T1",
		TaskDisplayMode: TaskDisplayModePlan,
	}); err != nil {
		t.Fatalf("StartStream: %v", err)
	}
	opened := peer.decode(t, 0)
	if _, present := opened["chunks"]; present {
		t.Fatalf("an empty opening still sent chunks (%v) — whatever opens the body STAYS in the body, above "+
			"the answer, for as long as the message exists", opened["chunks"])
	}
	if _, present := opened["markdown_text"]; present {
		t.Fatalf("an empty opening sent markdown_text (%v), which would open a TEXT stream and make every "+
			"later task card fail", opened["markdown_text"])
	}
	if opened["task_display_mode"] != "plan" {
		t.Fatalf("task_display_mode = %v, want plan — the mode must survive the empty opening, since it is "+
			"the only call that can set it", opened["task_display_mode"])
	}
}

// TestStopStreamChunksClosesAChunkStream: the mode split reaches the CLOSING call, and this is the half that
// matters most — chat.stopStream with markdown_text on a chunk stream is refused, so a stream closed the
// ordinary way does not close at all and renders as permanently "streaming" (SLK-P2).
func TestStopStreamChunksClosesAChunkStream(t *testing.T) {
	peer := &recordingPeer{}
	if err := StopStreamChunks(context.Background(), peer, "https://slack.test/api", []byte("x"), "C1", "1.1",
		[]map[string]any{MarkdownChunk("the last words")}); err != nil {
		t.Fatalf("StopStreamChunks: %v", err)
	}
	if !strings.HasSuffix(peer.urls[0], "/chat.stopStream") {
		t.Fatalf("posted to %q, want chat.stopStream", peer.urls[0])
	}
	body := peer.decode(t, 0)
	if _, present := body["markdown_text"]; present {
		t.Fatalf("the closing call carried markdown_text, which a chunk stream refuses: %v", body)
	}
	if chunks, _ := body["chunks"].([]any); len(chunks) != 1 {
		t.Fatalf("chunks = %v, want the final text as one chunk", body["chunks"])
	}
	// Nothing left to say still CLOSES — the call is made, with no chunks. A close skipped because there was
	// no final text is a message stuck streaming.
	if err := StopStreamChunks(context.Background(), peer, "https://slack.test/api", []byte("x"), "C1", "1.1", nil); err != nil {
		t.Fatalf("StopStreamChunks(nil): %v", err)
	}
	if len(peer.bodies) != 2 {
		t.Fatalf("a close with no final text made %d call(s) total, want 2 — it must still close", len(peer.bodies))
	}
	if _, present := peer.decode(t, 1)["chunks"]; present {
		t.Fatalf("a close with nothing to add still sent a chunks key: %v", peer.decode(t, 1))
	}
}

// TestAppendStreamChunksSendsNoBodyText is the SLK-P6 property at the wire: a step travels as a chunk, and
// the message body is not touched. A markdown_text of "" riding alongside would append an empty line to the
// answer and, worse, make the call ambiguous about what it is appending.
func TestAppendStreamChunksSendsNoBodyText(t *testing.T) {
	peer := &recordingPeer{}
	chunk := TaskUpdateChunk(Task{ID: "tc_1", Title: "Reading files", Status: "complete"})
	if err := AppendStreamChunks(context.Background(), peer, "https://slack.test/api", []byte("x"),
		"C1", "1.1", []map[string]any{chunk}); err != nil {
		t.Fatalf("AppendStreamChunks: %v", err)
	}
	if !strings.HasSuffix(peer.urls[0], "/chat.appendStream") {
		t.Fatalf("posted to %q, want chat.appendStream", peer.urls[0])
	}
	body := peer.decode(t, 0)
	if _, present := body["markdown_text"]; present {
		t.Fatalf("a chunks-only append carried markdown_text: %v", body)
	}
	chunks, _ := body["chunks"].([]any)
	if len(chunks) != 1 {
		t.Fatalf("chunks = %v, want exactly the one task update", body["chunks"])
	}
	// No chunks is no call at all, rather than a request Slack has nothing to do with.
	if err := AppendStreamChunks(context.Background(), peer, "https://slack.test/api", []byte("x"),
		"C1", "1.1", nil); err != nil {
		t.Fatalf("AppendStreamChunks(nil): %v", err)
	}
	if len(peer.bodies) != 1 {
		t.Fatalf("an empty chunk list made %d call(s), want 1 (the earlier one only)", len(peer.bodies))
	}
}

// TestAFinishedCallNeverRendersAsPending: TaskStatus falls back to `pending` for a status it has not been
// taught, which is the right default for an UNKNOWN one and the wrong answer for a call that ended. A
// failed tool rendered `pending` is a card that spins forever in a finished message.
func TestAFinishedCallNeverRendersAsPending(t *testing.T) {
	for ours, want := range map[string]string{"done": "complete", "failed": "error", "canceled": "error"} {
		if got := TaskStatus(ours); got != want {
			t.Fatalf("TaskStatus(%q) = %q, want %q", ours, got, want)
		}
	}
	if got := TaskStatus("something-new"); got != "pending" {
		t.Fatalf("TaskStatus of an unknown status = %q, want the fail-closed pending", got)
	}
}
