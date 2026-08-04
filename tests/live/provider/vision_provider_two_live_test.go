//go:build live

// The Anthropic VISION live smoke. It is the round trip the provider-two adapter's image refusal named as
// its own precondition: that adapter carried a guard whose comment said the image block "would be an
// UNPROVEN wire shape in a tree whose vision claim rests on a real round trip, and there is no Anthropic
// round trip behind it", and whose upgrade path was "add the image block to wireMessages and delete this
// guard in the same change that adds a real Anthropic vision round trip". This file is that change's half
// of the bargain.
//
// It settles the claim the same way the provider-one leg does, which is the only way that is not
// self-confirming: render text nobody has told the model, send the PIXELS through the shipped provider-two
// adapter, and assert the answer contains those exact characters. The glyph bitmaps live in
// vision_live_test.go, in source, so what the model is asked to read is reviewable in the diff rather than
// asserted in a comment — and the assertion cannot drift from the fixture, because both derive from the
// same string.
//
// WHAT THE ASSERTION IS WORTH. "7429" is four digits: a model that could not see the image would have one
// chance in ten thousand of naming it, and the prompt deliberately says nothing about the content.
package provider_test

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	providertwo "github.com/palgroup/palai/adapters/models/provider_two"
	"github.com/palgroup/palai/packages/contracts"
	modelbroker "github.com/palgroup/palai/packages/model-broker"
)

// visionModelTwo is the model this case runs against, and the default is chosen to match the DEPLOYMENT
// rather than to be cheap: prj_local's published `default` route is claude-sonnet-5 over a provider-two
// connection (measured 2026-08-04), which is the exact path an operator's Slack screenshot travels. Proving
// vision on a different Anthropic model would prove the wire shape while leaving the operator's own route
// unproven.
//
//	$ psql … -tAc "select rr.config->>'model', c.provider from model_route_revisions rr
//	    join model_routes r on r.id = rr.route_id join model_connections c
//	      on c.id = rr.config->>'connection_id'
//	    where r.project_id = 'prj_local' and r.name = 'default' order by rr.revision desc limit 1"
//	→ claude-sonnet-5 | provider-two          (2026-08-04)
func visionModelTwo() string {
	if m := os.Getenv("PALAI_LIVE_MODEL_TWO"); m != "" {
		return m
	}
	return "claude-sonnet-5"
}

// visionScaleTwo blows each font pixel up to a block this many device pixels wide, and it is MEASURED
// against the real model rather than shared with the provider-one leg.
//
// It differs from that leg's 6 because the two providers charge and downscale images by completely
// different rules, so the value tuned against one says nothing about the other. Anthropic views an image in
// 28x28-pixel patches — an image costs ceil(w/28) * ceil(h/28) visual tokens
// (https://platform.claude.com/docs/en/build-with-claude/vision, checked 2026-08-04) — so a 5x7 glyph must
// be large enough that its strokes are not lost inside one patch. At 12, each glyph is 60x84 device pixels
// and the whole render is 768x108, which is 28 x 4 = 112 visual tokens: legible AND cheap.
//
// Raising this is not free and not obviously better: the provider-one leg records a real case where scaling
// UP made the model misread ("PALATI 7232" at 16). If this test ever starts misreading, measure before
// reaching for a bigger number.
const visionScaleTwo = 12

// visionMinInputTokensTwo is the floor that catches an image DROPPED from the request, and it is MEASURED —
// the provider-one leg's floor of 1000 would be nonsense here.
//
// The two providers price the same picture an order of magnitude apart: provider-one's leg measured 8524
// input tokens for its render because the mini models bill image tiles at a high multiplier, while this
// render costs ~112 visual tokens under Anthropic's patch rule. The whole request measures 175 input tokens
// (2026-08-04, identical across three consecutive runs); with the image dropped from the body the same
// prompt measures 32, which is the number this floor actually has to separate from. 100 sits between them
// with room on both sides: it catches a dropped image, it does not pin a tokenizer.
const visionMinInputTokensTwo = 100

// TestLiveProviderTwoReadsTextInAnImage is CASE=vision-reads-image for the SECOND provider: one real
// streamed Anthropic message whose user turn carries a generated PNG, answering the owner's own question —
// "ne yazıyor burada" — about an image only the pixels describe.
//
// The prompt says NOTHING about what the image contains, deliberately. A prompt that mentioned digits, or a
// number, or the word PALAI would let a model produce the right answer without looking; this one leaves the
// image as the only source of the four digits in the answer.
func TestLiveProviderTwoReadsTextInAnImage(t *testing.T) {
	secret := os.Getenv(anthropicCredentialEnv)
	if secret == "" {
		t.Fatalf("%s is unset; the live tier loads it from .env.local at runtime", anthropicCredentialEnv)
	}

	pixels := renderTextPNGAt(t, visionSecret, visionScaleTwo)
	if len(pixels) < 200 {
		t.Fatalf("rendered image is %d bytes, which is too small to be a real render", len(pixels))
	}

	broker := modelbroker.New(modelbroker.Config{
		Adapters: map[string]modelbroker.ModelAdapter{"provider-two": providertwo.Adapter{}},
		Secrets:  modelbroker.EnvResolver{"provider-two": anthropicCredentialEnv},
	})

	req := modelbroker.Request{
		ModelRequestID: contracts.ModelRequestID("mreq_live_two_vision"),
		RouteRevision:  1,
		ModelStepID:    "step-1",
		Model:          visionModelTwo(),
		Messages: []modelbroker.Message{{
			Role: "user",
			// The owner's question, verbatim, plus the instruction that makes the answer machine-checkable.
			Content: "ne yazıyor burada? Reply with only the text you can read, nothing else.",
			Images:  []modelbroker.Image{{MediaType: "image/png", Data: pixels}},
		}},
		Deadline:    time.Now().Add(90 * time.Second),
		Reservation: modelbroker.Reservation{MaxTotalTokens: 4000},
		Secret:      modelbroker.SecretRef("provider-two"),
	}

	var streamed bytes.Buffer
	res, err := broker.Route(context.Background(), "provider-two", req, func(d modelbroker.Delta) {
		streamed.WriteString(d.Text)
	})
	if err != nil {
		t.Fatalf("route: %v", err)
	}
	if res.Error != nil {
		// Status class and stable code only — never the raw body, which can echo a credential prefix.
		t.Fatalf("provider returned a sanitized error: code=%s status=%d", res.Error.Code, res.Error.Status)
	}
	if err := res.Validate(); err != nil {
		t.Fatalf("live result is not canonical: %v", err)
	}

	// USAGE FIRST, and the ORDER is the point rather than a style choice. Vision costs input tokens, so an
	// image that never reached the provider bills like a bare sentence — and if the digits were checked
	// first, a dropped image would fail as "the model did not read the image", which reads as a MODEL
	// problem and sends a reader to the prompt or the render. It is neither: it is the REQUEST, and this
	// assertion is the one that says so. (Checked 2026-08-04 by dropping the image block in the adapter:
	// this fires first, reporting 32 input tokens against the floor of 100.)
	if res.Usage.InputTokens < visionMinInputTokensTwo {
		t.Fatalf("input tokens = %d, below the floor of %d — the image never reached the provider; look at the request body, not at the model",
			res.Usage.InputTokens, visionMinInputTokensTwo)
	}

	// THE ASSERTION THIS FILE EXISTS FOR: the four digits came back, and they could only have come from the
	// pixels.
	if !strings.Contains(res.Output, visionDigits) {
		t.Fatalf("the model did not read the image: output = %q, want it to contain %q (the image says %q)",
			res.Output, visionDigits, visionSecret)
	}
	// The word too, upper or lower case — a model that read the digits but not the letters has read the
	// image, so this is an Error rather than a Fatal: it says the render is marginal without pretending the
	// round trip failed.
	if !strings.Contains(strings.ToUpper(res.Output), "PALAI") {
		t.Errorf("output = %q read the digits but not the word — the render may be marginal", res.Output)
	}

	// A REAL round trip, not a replayed or fabricated one.
	if !strings.HasPrefix(res.ProviderRequestID, "msg") {
		t.Errorf("provider request id %q is not a real Anthropic message id", res.ProviderRequestID)
	}
	if len(res.Deltas) == 0 {
		t.Error("no streamed deltas were captured")
	}
	if res.Attempts != 1 {
		t.Errorf("attempts = %d, want exactly 1 (no hidden provider retry)", res.Attempts)
	}

	// The credential must be absent from every captured surface. The value is used only as an opaque needle
	// and is never printed.
	resultJSON, err := json.Marshal(res)
	if err != nil {
		t.Fatalf("marshal result: %v", err)
	}
	for name, surface := range map[string][]byte{
		"streamed deltas":  streamed.Bytes(),
		"canonical result": resultJSON,
	} {
		if bytes.Contains(surface, []byte(secret)) {
			t.Fatalf("the credential leaked into the %s", name) // never echo the value
		}
	}

	// Safe evidence only: an id prefix, token counts, the finish reason, and the answer the pixels produced.
	t.Logf("live PASS provider=provider-two case=vision-reads-image message_id=%s… model=%s finish=%s usage(input=%d output=%d total=%d) answer=%q",
		safePrefix(res.ProviderRequestID), res.Model, res.FinishReason,
		res.Usage.InputTokens, res.Usage.OutputTokens, res.Usage.TotalTokens, res.Output)
}
