//go:build live

// The live proof for the message-array input form. It is deliberately NOT a provider-tier test: the
// defect was never in the adapter, it was in this package's decoder, and a test that hand-builds a
// modelbroker.Request cannot see it. This one drives the REAL decodeMessages over the REAL engine wrap
// and hands the result to the REAL provider-two adapter against the REAL Anthropic API.
//
// It reproduces resp_76e1d12c45f584ce9dd99c686f29e750 (2026-08-04), which failed
// `400 invalid_request_error` once a second forever: an input in the public API's message-array form,
// wrapped by the reference engine into one user turn, decoded into a turn with no text and no image.
package execution

import (
	"bytes"
	"context"
	"encoding/json"
	"image"
	"image/color"
	"image/png"
	"os"
	"strings"
	"testing"
	"time"

	providertwo "github.com/palgroup/palai/adapters/models/provider_two"
	"github.com/palgroup/palai/packages/contracts"
	modelbroker "github.com/palgroup/palai/packages/model-broker"
)

// anthropicKeyEnv is the variable name AS IT APPEARS in .env.local (the misspelling is the file's).
const anthropicKeyEnv = "ANTROPHIC_API_KEY"

// orangePNG renders the 80x80 flat-orange PNG the live reproduction used. Generated rather than
// committed as a fixture so what the model is asked to name is reviewable in the diff — the assertion
// cannot drift from an image nobody can read.
func orangePNG(t *testing.T) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, 80, 80))
	orange := color.RGBA{R: 0xFF, G: 0x8C, B: 0x00, A: 0xFF}
	for y := range 80 {
		for x := range 80 {
			img.Set(x, y, orange)
		}
	}
	var out bytes.Buffer
	if err := png.Encode(&out, img); err != nil {
		t.Fatalf("encode png: %v", err)
	}
	return out.Bytes()
}

// TestLiveWrappedMessageArrayReachesTheModel is CASE=wrapped-message-array. The input is the exact shape
// the failing response carried, put through the exact wrap engines/reference/.../context.py applies, and
// the model is asked a question ONLY the pixels can answer.
//
// A colour is a weaker needle than the vision leg's four digits — a model guessing has a real chance of
// saying orange. That is deliberate and the assertion is built for it: the run this reproduces did not
// give a wrong answer, it gave NO answer, and what has to be proved here is that the content arrives at
// all. The input-token floor is the half that proves the image specifically: 42 tokens with the picture,
// well under 20 without it.
func TestLiveWrappedMessageArrayReachesTheModel(t *testing.T) {
	secret := os.Getenv(anthropicKeyEnv)
	if secret == "" {
		t.Fatalf("%s is unset; the live tier loads it from .env.local at runtime", anthropicKeyEnv)
	}
	pixels := orangePNG(t)

	// The response's own input, verbatim from the failing row.
	const inputJSON = `[{"role":"user","content":[` +
		`{"text":"Bu görseldeki rengi TEK kelimeyle söyle.","type":"input_text"},` +
		`{"type":"image_ref","artifact_id":"art_20970f0f14f2f14fc65907c22c109d30"}]}]`
	var input any
	if err := json.Unmarshal([]byte(inputJSON), &input); err != nil {
		t.Fatalf("decode input: %v", err)
	}

	// THE ENGINE WRAP, reproduced exactly: context.py's start() appends the whole input as one user turn.
	wrapped := []any{map[string]any{"role": "user", "content": input}}

	messages, err := decodeMessages(wrapped, func(string) (modelbroker.Image, bool, error) {
		return modelbroker.Image{MediaType: "image/png", Data: pixels}, true, nil
	})
	if err != nil {
		t.Fatalf("decodeMessages: %v", err)
	}

	// Before the fix this was one turn with Content "" and no images — the empty text block Anthropic 400'd.
	if len(messages) != 1 {
		t.Fatalf("messages = %+v, want the wrapped turn expanded to one", messages)
	}
	if messages[0].Content == "" {
		t.Fatal("the turn reached the provider with NO TEXT — the wrap swallowed the words again")
	}
	if len(messages[0].Images) != 1 {
		t.Fatalf("images = %d, want the image_ref resolved — the wrap swallowed the picture again",
			len(messages[0].Images))
	}

	res, err := providertwo.Adapter{}.Execute(context.Background(), modelbroker.Request{
		ModelRequestID: contracts.ModelRequestID("mreq_live_wrapped"),
		RouteRevision:  1,
		ModelStepID:    "step-1",
		Model:          "claude-sonnet-5",
		Messages:       messages,
		Deadline:       time.Now().Add(60 * time.Second),
	}, secret, nil)
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	if res.Error != nil {
		// The message now carries the provider's own explanation, redacted — the whole point of this fix.
		t.Fatalf("provider rejected the request: code=%s status=%d message=%s",
			res.Error.Code, res.Error.Status, res.Error.Message)
	}

	if res.Usage.InputTokens < 30 {
		t.Fatalf("input tokens = %d, too few to have carried the image — the picture never left", res.Usage.InputTokens)
	}
	answer := strings.ToLower(res.Output)
	if !strings.Contains(answer, "turuncu") && !strings.Contains(answer, "orange") {
		t.Fatalf("output = %q, want the colour only the pixels describe", res.Output)
	}
	if strings.Contains(res.Output, secret) {
		t.Fatal("the credential leaked into the output")
	}

	t.Logf("live PASS case=wrapped-message-array message_id=%s… model=%s usage(input=%d output=%d) answer=%q",
		res.ProviderRequestID[:min(len(res.ProviderRequestID), 16)], res.Model,
		res.Usage.InputTokens, res.Usage.OutputTokens, res.Output)
}
