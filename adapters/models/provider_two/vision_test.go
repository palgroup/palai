package providertwo

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	modelbroker "github.com/palgroup/palai/packages/model-broker"
)

// wireBody renders a request the way Execute would and decodes the system + messages back out.
func wireBody(t *testing.T, req modelbroker.Request) (string, []map[string]any) {
	t.Helper()
	raw, _, err := Adapter{}.buildBody(req)
	if err != nil {
		t.Fatalf("buildBody: %v", err)
	}
	var body struct {
		System   string           `json:"system"`
		Messages []map[string]any `json:"messages"`
	}
	if err := json.Unmarshal(raw, &body); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	return body.System, body.Messages
}

// blocks pulls one message's content array out as decoded JSON objects.
func blocks(t *testing.T, message map[string]any) []map[string]any {
	t.Helper()
	raw, ok := message["content"].([]any)
	if !ok {
		t.Fatalf("content = %#v, want an array of blocks", message["content"])
	}
	out := make([]map[string]any, 0, len(raw))
	for i, item := range raw {
		block, ok := item.(map[string]any)
		if !ok {
			t.Fatalf("block %d = %#v, want an object", i, item)
		}
		out = append(out, block)
	}
	return out
}

// A user turn carrying an image must reach Anthropic as an image content block holding the BYTES, with the
// image placed BEFORE the words.
//
// CONTRACT: https://platform.claude.com/docs/en/build-with-claude/vision (checked 2026-08-04) — the block
// is {"type":"image","source":{"type":"base64","media_type":…,"data":…}}, and the same page's advice is
// that "Claude works best when images come before text".
//
// THE BYTES, NOT A URL, is the load-bearing half: the endpoint would also take {"type":"url"}, and nothing
// in this body may be an address the PROVIDER dereferences. The image is already control-plane-side (spec
// §24), and handing Anthropic a files.slack.com URL would both leak the workspace's file location and make
// a third party fetch on our behalf with whatever credential that URL happened to embed.
func TestBuildBodyRendersAnImageBeforeTheText(t *testing.T) {
	_, messages := wireBody(t, modelbroker.Request{
		Model: "claude-sonnet-5",
		Messages: []modelbroker.Message{{
			Role:    "user",
			Content: "ne yazıyor burada",
			Images:  []modelbroker.Image{{MediaType: "image/png", Data: []byte{0x89, 'P', 'N', 'G'}}},
		}},
	})
	if len(messages) != 1 {
		t.Fatalf("messages = %d, want 1", len(messages))
	}
	if messages[0]["role"] != "user" {
		t.Fatalf("role = %v, want user", messages[0]["role"])
	}
	parts := blocks(t, messages[0])
	if len(parts) != 2 {
		t.Fatalf("blocks = %d (%#v), want the image block plus the text block", len(parts), parts)
	}
	if parts[0]["type"] != "image" {
		t.Fatalf("first block type = %v, want image — the image must come before the text", parts[0]["type"])
	}
	source, _ := parts[0]["source"].(map[string]any)
	if source["type"] != "base64" {
		t.Errorf("source type = %v, want base64", source["type"])
	}
	if source["media_type"] != "image/png" {
		t.Errorf("media_type = %v, want image/png", source["media_type"])
	}
	if want := "iVBORw=="; source["data"] != want {
		t.Errorf("data = %v, want %q", source["data"], want)
	}
	if _, addressed := source["url"]; addressed {
		t.Error("the source carries a url — the provider must never be asked to dereference an address")
	}
	if parts[1]["type"] != "text" || parts[1]["text"] != "ne yazıyor burada" {
		t.Errorf("second block = %#v, want the text block carrying the human's words", parts[1])
	}
}

// An image-only turn — a file shared with no comment, the ordinary Slack case — must carry the image and NO
// empty text block: the endpoint rejects a zero-length text block, so the pre-vision code's unconditional
// {"type":"text","text":""} would have 400'd every wordless screenshot.
func TestBuildBodyOmitsTheTextBlockWhenTheTurnHasNoWords(t *testing.T) {
	_, messages := wireBody(t, modelbroker.Request{
		Messages: []modelbroker.Message{{Role: "user", Images: []modelbroker.Image{{MediaType: "image/jpeg", Data: []byte("jpg")}}}},
	})
	parts := blocks(t, messages[0])
	if len(parts) != 1 {
		t.Fatalf("blocks = %#v, want the image block alone", parts)
	}
	if parts[0]["type"] != "image" {
		t.Fatalf("block = %#v, want the image block", parts[0])
	}
}

// THE REGRESSION GUARD, and it is why images ride a separate field rather than a re-typed Content: a turn
// with no images must render the SAME single text block it always did, so every text-only run's provider
// request is byte-identical to the pre-vision one.
func TestBuildBodyLeavesATextOnlyTurnUnchanged(t *testing.T) {
	_, messages := wireBody(t, modelbroker.Request{
		Messages: []modelbroker.Message{{Role: "user", Content: "merhaba"}},
	})
	if messages[0]["role"] != "user" {
		t.Fatalf("role = %v, want user", messages[0]["role"])
	}
	parts := blocks(t, messages[0])
	if len(parts) != 1 || parts[0]["type"] != "text" || parts[0]["text"] != "merhaba" {
		t.Fatalf("content = %#v, want exactly one text block saying \"merhaba\"", parts)
	}
}

// An image's bytes must appear in the body ONCE, as base64, and never in the raw.
func TestBuildBodyCarriesTheImageExactlyOnce(t *testing.T) {
	raw, _, err := Adapter{}.buildBody(modelbroker.Request{
		Messages: []modelbroker.Message{{Role: "user", Images: []modelbroker.Image{{MediaType: "image/png", Data: []byte("SECRETBYTES")}}}},
	})
	if err != nil {
		t.Fatalf("buildBody: %v", err)
	}
	if n := strings.Count(string(raw), "U0VDUkVUQllURVM="); n != 1 {
		t.Fatalf("the base64 image appears %d times in the body, want exactly 1: %s", n, raw)
	}
	if strings.Contains(string(raw), "SECRETBYTES") {
		t.Fatalf("the body carries the RAW image bytes beside the encoded copy: %s", raw)
	}
}

// A format Anthropic does not accept must be MARKED, never dropped in silence — the whole reason this
// family refused images at all before it could send them. http.DetectContentType (the sniffer behind every
// MediaType that reaches here) emits image/bmp, and execution's resolver admits anything `image/`, so this
// is a reachable inbound case rather than a defensive one.
func TestBuildBodySaysSoWhenItCannotReadTheFormat(t *testing.T) {
	_, messages := wireBody(t, modelbroker.Request{
		Messages: []modelbroker.Message{{
			Role:    "user",
			Content: "bunu oku",
			Images:  []modelbroker.Image{{MediaType: "image/bmp", Data: []byte("BM-bitmap")}},
		}},
	})
	parts := blocks(t, messages[0])
	for _, block := range parts {
		if block["type"] == "image" {
			t.Fatalf("an image/bmp reached the wire as an image block: %#v", block)
		}
	}
	if len(parts) != 1 || parts[0]["type"] != "text" {
		t.Fatalf("blocks = %#v, want a single text block", parts)
	}
	text, _ := parts[0]["text"].(string)
	if !strings.Contains(text, "bunu oku") {
		t.Errorf("text = %q, want it to keep the human's words", text)
	}
	if !strings.Contains(text, unreadableImageNote) {
		t.Errorf("text = %q, want it to carry %q — an unsent image must never be silent", text, unreadableImageNote)
	}
}

// The note is per TURN, not per image: repeating one identical sentence tells the model nothing the first
// one did not, and this is the assertion that keeps a five-screenshot message from carrying five copies.
func TestBuildBodyNotesUnreadableImagesOncePerTurn(t *testing.T) {
	_, messages := wireBody(t, modelbroker.Request{
		Messages: []modelbroker.Message{{
			Role: "user",
			Images: []modelbroker.Image{
				{MediaType: "image/bmp", Data: []byte("one")},
				{MediaType: "image/vnd.microsoft.icon", Data: []byte("two")},
			},
		}},
	})
	parts := blocks(t, messages[0])
	text, _ := parts[len(parts)-1]["text"].(string)
	if n := strings.Count(text, unreadableImageNote); n != 1 {
		t.Fatalf("the note appears %d times in %q, want exactly 1", n, text)
	}
}

// A supported image beside an unreadable one must still be SENT. The note says one was withheld; it must
// not cost the model the picture it could have read.
func TestBuildBodyStillSendsTheReadableImageBesideAnUnreadableOne(t *testing.T) {
	_, messages := wireBody(t, modelbroker.Request{
		Messages: []modelbroker.Message{{
			Role: "user",
			Images: []modelbroker.Image{
				{MediaType: "image/bmp", Data: []byte("bitmap")},
				{MediaType: "image/webp", Data: []byte("webp")},
			},
		}},
	})
	parts := blocks(t, messages[0])
	images := 0
	for _, block := range parts {
		if block["type"] == "image" {
			images++
			source, _ := block["source"].(map[string]any)
			if source["media_type"] != "image/webp" {
				t.Errorf("media_type = %v, want the webp that Anthropic accepts", source["media_type"])
			}
		}
	}
	if images != 1 {
		t.Fatalf("image blocks = %d, want the one readable image to survive its unreadable neighbour", images)
	}
}

// An image on a turn whose Anthropic shape has nowhere to put one (here: system) must be reported in the
// system text rather than vanishing. Roles are an unconstrained string in the message contract
// (protocols/schemas/execution/message.json), so a caller can put an image_ref on any turn it likes.
func TestBuildBodyReportsAnImageItCannotPlaceOnANonUserTurn(t *testing.T) {
	system, messages := wireBody(t, modelbroker.Request{
		Messages: []modelbroker.Message{
			{Role: "system", Content: "sen bir asistansın", Images: []modelbroker.Image{{MediaType: "image/png", Data: []byte("png")}}},
			{Role: "user", Content: "merhaba"},
		},
	})
	if !strings.Contains(system, "sen bir asistansın") {
		t.Errorf("system = %q, want it to keep the operator's own words", system)
	}
	if !strings.Contains(system, unplaceableImageNote) {
		t.Errorf("system = %q, want it to carry %q", system, unplaceableImageNote)
	}
	for _, message := range messages {
		for _, block := range blocks(t, message) {
			if block["type"] == "image" {
				t.Fatalf("a system-turn image reached a message as an image block: %#v", block)
			}
		}
	}
}

// A conversation with no images anywhere must leave the system string exactly as it was — the note is a
// consequence of an image, never a decoration on every request.
func TestBuildBodyLeavesTheSystemStringAloneWithoutImages(t *testing.T) {
	system, _ := wireBody(t, modelbroker.Request{
		Messages: []modelbroker.Message{
			{Role: "system", Content: "sen bir asistansın"},
			{Role: "user", Content: "merhaba"},
		},
	})
	if system != "sen bir asistansın" {
		t.Fatalf("system = %q, want it unchanged", system)
	}
}

// THE PROOF THE REFUSAL IS GONE, at the level the refusal lived at. Until 2026-08-04 Execute returned
// ErrImageUnsupported before it built a request or touched the credential, so a run carrying a screenshot
// failed outright. This drives the real Execute against a fake endpoint and asserts the request ARRIVED,
// carrying the image — a check on buildBody alone could not tell a deleted guard from a live one.
func TestExecuteSendsAnImageInsteadOfRefusingIt(t *testing.T) {
	var received []byte
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		received, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, "data: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_live\",\"model\":\"claude-sonnet-5\",\"usage\":{\"input_tokens\":9,\"output_tokens\":0}}}\n\n")
		io.WriteString(w, "data: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"gördüm\"}}\n\n")
		io.WriteString(w, "data: {\"type\":\"message_stop\"}\n\n")
	}))
	defer server.Close()

	res, err := Adapter{BaseURL: server.URL}.Execute(context.Background(), modelbroker.Request{
		Model: "claude-sonnet-5",
		Messages: []modelbroker.Message{{
			Role:    "user",
			Content: "ne yazıyor burada",
			Images:  []modelbroker.Image{{MediaType: "image/png", Data: []byte("PIXELS")}},
		}},
	}, "k", nil)
	if err != nil {
		t.Fatalf("Execute refused a request carrying an image: %v", err)
	}
	if res.Error != nil {
		t.Fatalf("sanitized error: code=%s status=%d", res.Error.Code, res.Error.Status)
	}
	if res.Output != "gördüm" {
		t.Errorf("output = %q, want the streamed answer", res.Output)
	}
	if !strings.Contains(string(received), "\"type\":\"image\"") {
		t.Fatalf("the request that reached the endpoint carries no image block: %s", received)
	}
	if !strings.Contains(string(received), "UElYRUxT") {
		t.Fatalf("the request carries no base64 of the image bytes: %s", received)
	}
}
