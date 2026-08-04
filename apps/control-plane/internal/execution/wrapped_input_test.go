package execution

import (
	"testing"

	modelbroker "github.com/palgroup/palai/packages/model-broker"
)

// resolveOne is an image resolver that returns one fixed image for any id.
func resolveOne(t *testing.T) imageResolver {
	t.Helper()
	return func(string) (modelbroker.Image, bool, error) {
		return modelbroker.Image{MediaType: "image/png", Data: []byte{0x89, 'P', 'N', 'G'}}, true, nil
	}
}

// THE REGRESSION THIS FILE EXISTS FOR, measured live on 2026-08-04. The public API accepts an input that
// is an array of MESSAGES, and the reference engine appends the whole input as ONE user turn's content
// (context.py's start()). The result is a content array holding message objects, decodeContentParts keys
// on an item's `type`, a message has none — so every item matched no case and was silently dropped. The
// turn reached the provider with no text and no image, and Anthropic answered 400 for the empty text
// block the adapter then rendered. Both the words and the picture were gone before the adapter ran.
func TestDecodeMessagesUnwrapsAnEngineWrappedMessageArray(t *testing.T) {
	raw := []any{map[string]any{
		"role": "user",
		"content": []any{
			map[string]any{
				"role": "user",
				"content": []any{
					map[string]any{"type": "input_text", "text": "Bu görseldeki rengi TEK kelimeyle söyle."},
					map[string]any{"type": "image_ref", "artifact_id": "art_x"},
				},
			},
		},
	}}

	messages, err := decodeMessages(raw, resolveOne(t))
	if err != nil {
		t.Fatalf("decodeMessages: %v", err)
	}
	if len(messages) != 1 {
		t.Fatalf("messages = %d (%+v), want the wrapped turn expanded to one", len(messages), messages)
	}
	if messages[0].Role != "user" {
		t.Errorf("role = %q, want user", messages[0].Role)
	}
	if messages[0].Content != "Bu görseldeki rengi TEK kelimeyle söyle." {
		t.Errorf("content = %q, want the human's words to survive the unwrap", messages[0].Content)
	}
	if len(messages[0].Images) != 1 {
		t.Fatalf("images = %d, want the image_ref resolved rather than dropped", len(messages[0].Images))
	}
}

// A wrapped array carrying SEVERAL turns must expand into several messages with their roles intact —
// flattening them into one user turn would lose which side of the conversation said what.
func TestDecodeMessagesKeepsRolesWhenUnwrapping(t *testing.T) {
	raw := []any{map[string]any{
		"role": "user",
		"content": []any{
			map[string]any{"role": "user", "content": "merhaba"},
			map[string]any{"role": "assistant", "content": "selam"},
			map[string]any{"role": "user", "content": "nasılsın"},
		},
	}}

	messages, err := decodeMessages(raw, resolveOne(t))
	if err != nil {
		t.Fatalf("decodeMessages: %v", err)
	}
	if len(messages) != 3 {
		t.Fatalf("messages = %+v, want three turns", messages)
	}
	for i, want := range []struct{ role, content string }{
		{"user", "merhaba"}, {"assistant", "selam"}, {"user", "nasılsın"},
	} {
		if messages[i].Role != want.role || messages[i].Content != want.content {
			t.Errorf("message %d = %q/%q, want %q/%q", i, messages[i].Role, messages[i].Content, want.role, want.content)
		}
	}
}

// THE OTHER HALF, and the one that keeps the unwrap from becoming its own bug: a genuine CONTENT array —
// the shape the Slack bridge sends, and the shape that has always worked — must pass through untouched.
// A discriminator that mistook content items for messages would break the only path that was working.
func TestDecodeMessagesLeavesARealContentArrayAlone(t *testing.T) {
	raw := []any{map[string]any{
		"role": "user",
		"content": []any{
			map[string]any{"type": "input_text", "text": "buradaki ekran görüntüsünde ne yazıyor?"},
			map[string]any{"type": "image_ref", "artifact_id": "art_y"},
		},
	}}

	messages, err := decodeMessages(raw, resolveOne(t))
	if err != nil {
		t.Fatalf("decodeMessages: %v", err)
	}
	if len(messages) != 1 {
		t.Fatalf("messages = %+v, want the single Slack-shaped turn", messages)
	}
	if messages[0].Content != "buradaki ekran görüntüsünde ne yazıyor?" || len(messages[0].Images) != 1 {
		t.Fatalf("content/images = %q/%d, want the Slack shape decoded as it always was",
			messages[0].Content, len(messages[0].Images))
	}
}

// A mixed array — one message object beside one content item — is NOT a wrapped conversation, and
// re-interpreting it would be a guess. It must be left alone.
func TestDecodeMessagesLeavesAMixedArrayAlone(t *testing.T) {
	raw := []any{map[string]any{
		"role": "user",
		"content": []any{
			map[string]any{"role": "user", "content": "merhaba"},
			map[string]any{"type": "input_text", "text": "ek"},
		},
	}}

	messages, err := decodeMessages(raw, resolveOne(t))
	if err != nil {
		t.Fatalf("decodeMessages: %v", err)
	}
	if len(messages) != 1 {
		t.Fatalf("messages = %+v, want the ambiguous turn left as one", messages)
	}
	if messages[0].Content != "ek" {
		t.Errorf("content = %q, want only the recognised content item decoded", messages[0].Content)
	}
}

// An object carrying BOTH a `type` and a `role` is not a message, and this pins the `type` half of the
// discriminator. It exists because a perturbation deleting that half stayed GREEN: for every ordinary
// content item the `role` check already refuses the array, so the two guards overlap everywhere except
// here. The property held under that perturbation — but nothing was asserting this branch, so nothing
// would have noticed if it stopped holding.
func TestDecodeMessagesTreatsATypedObjectAsAContentItem(t *testing.T) {
	raw := []any{map[string]any{
		"role": "user",
		"content": []any{
			map[string]any{"type": "input_text", "role": "user", "content": "x", "text": "gerçek metin"},
		},
	}}

	messages, err := decodeMessages(raw, resolveOne(t))
	if err != nil {
		t.Fatalf("decodeMessages: %v", err)
	}
	if len(messages) != 1 {
		t.Fatalf("messages = %+v, want the turn left alone", messages)
	}
	if messages[0].Content != "gerçek metin" {
		t.Fatalf("content = %q, want the item decoded as the content item its `type` declares it to be",
			messages[0].Content)
	}
}

// A permanent provider rejection must be classified so the run loop can end the run instead of feeding
// the retry ladder a request the provider has already answered. Measured live: a 400 repeated once a
// second, forever, and the run never left in_progress.
func TestPermanentProviderRejectionClassification(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
		want   bool
	}{
		{"malformed request", 400, true},
		{"rejected credential", 401, true},
		{"forbidden", 403, true},
		{"unknown model", 404, true},
		{"unprocessable", 422, true},
		{"request timeout invites a retry", 408, false},
		{"rate limited invites a retry", 429, false},
		{"server error may differ in a moment", 500, false},
		{"overloaded may differ in a moment", 529, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := isPermanentProviderRejection(&modelbroker.SanitizedError{Code: "x", Status: tc.status})
			if got != tc.want {
				t.Fatalf("status %d classified permanent=%v, want %v", tc.status, got, tc.want)
			}
		})
	}
	if isPermanentProviderRejection(nil) {
		t.Error("a nil error must not be classified as a permanent rejection")
	}
}
