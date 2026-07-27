package providerone

import (
	"encoding/json"
	"strings"
	"testing"

	modelbroker "github.com/palgroup/palai/packages/model-broker"
)

// wireBody renders a request the way Execute would and decodes the messages array back.
func wireBody(t *testing.T, req modelbroker.Request) []map[string]any {
	t.Helper()
	raw, _, err := buildBody(req)
	if err != nil {
		t.Fatalf("buildBody: %v", err)
	}
	var body struct {
		Messages []map[string]any `json:"messages"`
	}
	if err := json.Unmarshal(raw, &body); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	return body.Messages
}

// A turn carrying an image must reach the provider in the MULTI-PART content shape, with the bytes
// base64'd into a data URL beside the text.
//
// CONTRACT: https://developers.openai.com/api/docs/api-reference/chat/create (checked 2026-07-27) — a user
// message's content may be an array of parts; an image part is {"type":"image_url","image_url":{"url":…}}
// and the url may be a `data:<media-type>;base64,<bytes>` URI.
//
// THE BYTES, NOT A URL, and that is the load-bearing half: nothing in this body may be an address the
// PROVIDER dereferences. The image is already control-plane-side (spec §24), and handing OpenAI a
// files.slack.com URL would both leak the workspace's file location and make a third party fetch on our
// behalf with whatever credential that URL happened to embed.
func TestBuildBodyRendersAnImageAsAContentPart(t *testing.T) {
	messages := wireBody(t, modelbroker.Request{
		Model: "gpt-4o-mini",
		Messages: []modelbroker.Message{{
			Role:    "user",
			Content: "ne yazıyor burada",
			Images:  []modelbroker.Image{{MediaType: "image/png", Data: []byte{0x89, 'P', 'N', 'G'}}},
		}},
	})
	if len(messages) != 1 {
		t.Fatalf("messages = %d, want 1", len(messages))
	}
	parts, ok := messages[0]["content"].([]any)
	if !ok {
		t.Fatalf("content = %#v, want an array of parts", messages[0]["content"])
	}
	if len(parts) != 2 {
		t.Fatalf("parts = %d (%#v), want the text part plus the image part", len(parts), parts)
	}
	text, _ := parts[0].(map[string]any)
	if text["type"] != "text" || text["text"] != "ne yazıyor burada" {
		t.Fatalf("first part = %#v, want the text part carrying the human's words", text)
	}
	image, _ := parts[1].(map[string]any)
	if image["type"] != "image_url" {
		t.Fatalf("second part type = %v, want image_url", image["type"])
	}
	ref, _ := image["image_url"].(map[string]any)
	url, _ := ref["url"].(string)
	if want := "data:image/png;base64,iVBORw=="; url != want {
		t.Fatalf("image url = %q, want %q", url, want)
	}
}

// An image-only turn (a file shared with no comment) must still be a valid message: the parts array
// carries the image and NO empty text part — a provider rejects a zero-length text part.
func TestBuildBodyOmitsTheTextPartWhenTheTurnHasNoWords(t *testing.T) {
	messages := wireBody(t, modelbroker.Request{
		Messages: []modelbroker.Message{{Role: "user", Images: []modelbroker.Image{{MediaType: "image/jpeg", Data: []byte("jpg")}}}},
	})
	parts, ok := messages[0]["content"].([]any)
	if !ok {
		t.Fatalf("content = %#v, want an array of parts", messages[0]["content"])
	}
	if len(parts) != 1 {
		t.Fatalf("parts = %#v, want the image part alone", parts)
	}
	if part, _ := parts[0].(map[string]any); part["type"] != "image_url" {
		t.Fatalf("part = %#v, want the image part", part)
	}
}

// THE REGRESSION GUARD, and it is why images ride a separate field rather than a re-typed Content: a turn
// with no images must render the STRING content it always did, so every text-only run's provider request is
// byte-identical to the pre-vision one.
func TestBuildBodyLeavesATextOnlyTurnUnchanged(t *testing.T) {
	messages := wireBody(t, modelbroker.Request{
		Messages: []modelbroker.Message{{Role: "user", Content: "merhaba"}},
	})
	if got := messages[0]["content"]; got != "merhaba" {
		t.Fatalf("content = %#v, want the bare string \"merhaba\"", got)
	}
}

// An image's bytes must never appear in the body as anything but the base64 data URL — no filename, no
// raw byte array, no second copy under another key.
func TestBuildBodyCarriesTheImageExactlyOnce(t *testing.T) {
	raw, _, err := buildBody(modelbroker.Request{
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
