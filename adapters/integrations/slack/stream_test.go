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
