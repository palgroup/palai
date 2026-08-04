//go:build live

// The VISION live smoke (E20). It is the only test in the tree that can settle the claim the whole image
// leg exists to make — that a REAL model REALLY READS an image a run carried — and it settles it the only
// way that is not self-confirming: by rendering text nobody has told the model, sending the PIXELS through
// the shipped provider-one adapter, and asserting the answer contains those exact characters.
//
// WHY A GENERATED IMAGE RATHER THAN A COMMITTED FIXTURE. A binary fixture is a file a reviewer cannot read:
// "the image says PALAI 7429" would be a claim in a comment rather than a fact in the diff. The glyph
// bitmaps below ARE the image, in source, so what the model is asked to read is reviewable — and the
// assertion cannot drift from the fixture, because the fixture is derived from the same string.
//
// WHAT THE ASSERTION IS WORTH. "7429" is four digits: a model that could not see the image would have one
// chance in ten thousand of naming it, and no amount of guessing from the prompt would help — the prompt
// deliberately says nothing about the content. A colour or a shape could be inferred from a lucky default;
// a specific number cannot.
//
// It also asserts the credential is absent from every captured surface, the same discipline every case in
// this tier follows.
package provider_test

import (
	"bytes"
	"context"
	"image"
	"image/color"
	"image/png"
	"os"
	"strings"
	"testing"
	"time"

	providerone "github.com/palgroup/palai/adapters/models/provider_one"
	"github.com/palgroup/palai/packages/contracts"
	modelbroker "github.com/palgroup/palai/packages/model-broker"
)

// visionSecret is the string the rendered image says and the answer must contain. The digits are the real
// assertion (see the package comment); the word is there so a partially-legible render is distinguishable
// from an unread one.
const visionSecret = "PALAI 7429"

// visionDigits is the part of visionSecret a correct answer must reproduce exactly.
const visionDigits = "7429"

// glyphs is a 5x7 bitmap font covering exactly the characters visionSecret needs. Hand-coded rather than
// pulled from a font package: adding a dependency to draw ten characters is not a trade worth making, and
// these rows are readable as the letters they draw.
var glyphs = map[rune][7]string{
	'P': {"11110", "10001", "10001", "11110", "10000", "10000", "10000"},
	'A': {"01110", "10001", "10001", "11111", "10001", "10001", "10001"},
	'L': {"10000", "10000", "10000", "10000", "10000", "10000", "11111"},
	'I': {"11111", "00100", "00100", "00100", "00100", "00100", "11111"},
	'7': {"11111", "00001", "00010", "00100", "01000", "01000", "01000"},
	'4': {"00010", "00110", "01010", "10010", "11111", "00010", "00010"},
	'2': {"01110", "10001", "00001", "00010", "00100", "01000", "11111"},
	'9': {"01110", "10001", "10001", "01111", "00001", "00001", "01110"},
	' ': {"00000", "00000", "00000", "00000", "00000", "00000", "00000"},
}

// visionScale blows each font pixel up to a block this many device pixels wide, and the value is MEASURED
// against the real model rather than picked for looking good.
//
// The instinct is "bigger is more legible", and for a scaled bitmap font it is exactly wrong. At scale 16 the
// image is unmistakable to a human and gpt-4o-mini read "PALATI 7232" — it saw the text and misread it,
// because a 5x7 glyph blown up 16x is a mosaic of large squares rather than a letterform. Scaling DOWN fixed
// it: at 5, 6 and 7 the model returned "PALAI 7429" exactly, three consecutive runs at 6. It is also the
// cheapest — 8.5k input tokens against 19.9k at 16.
//
// So: do not raise this to make the picture nicer. If this test ever starts misreading, the fix is a smaller
// scale or a better font, not a bigger one.
const visionScale = 6

// renderTextPNGAt draws text as large black blocks on white and encodes it as a PNG. Stdlib only.
//
// The scale is a PARAMETER rather than the package constant because each provider's own measured value is
// the right one for it: provider-one reads this font best at 6 (see visionScale) and Anthropic, which bills
// and downscales images by an entirely different rule, at 12 (see visionScaleTwo). One shared renderer, one
// measured number per family.
func renderTextPNGAt(t *testing.T, text string, scale int) []byte {
	t.Helper()
	const glyphW, glyphH, gap = 5, 7, 1
	pad := scale * 2
	width := len(text)*(glyphW+gap)*scale + 2*pad
	height := glyphH*scale + 2*pad
	img := image.NewRGBA(image.Rect(0, 0, width, height))
	for y := range height {
		for x := range width {
			img.Set(x, y, color.White)
		}
	}
	for i, r := range text {
		rows, ok := glyphs[r]
		if !ok {
			t.Fatalf("no glyph for %q — the font covers only the characters visionSecret uses", r)
		}
		originX := pad + i*(glyphW+gap)*scale
		for row, bits := range rows {
			for col, bit := range bits {
				if bit != '1' {
					continue
				}
				for dy := range scale {
					for dx := range scale {
						img.Set(originX+col*scale+dx, pad+row*scale+dy, color.Black)
					}
				}
			}
		}
	}
	var out bytes.Buffer
	if err := png.Encode(&out, img); err != nil {
		t.Fatalf("encode png: %v", err)
	}
	return out.Bytes()
}

// TestLiveProviderOneReadsTextInAnImage is CASE=vision-reads-image: one real streamed completion whose user
// turn carries a generated PNG, answering the owner's own question — "ne yazıyor burada" — about an image
// only the pixels describe.
//
// The prompt says NOTHING about what the image contains, deliberately. A prompt that mentioned digits, or a
// number, or the word PALAI would let a model produce the right answer without looking; this one leaves the
// image as the only source of the four digits in the answer.
func TestLiveProviderOneReadsTextInAnImage(t *testing.T) {
	secret := os.Getenv(credentialEnv)
	if secret == "" {
		t.Fatalf("%s is unset; the live tier loads it from .env.local at runtime", credentialEnv)
	}

	pixels := renderTextPNGAt(t, visionSecret, visionScale)
	if len(pixels) < 200 {
		t.Fatalf("rendered image is %d bytes, which is too small to be a real render", len(pixels))
	}

	broker := modelbroker.New(modelbroker.Config{
		Adapters: map[string]modelbroker.ModelAdapter{"provider-one": providerone.Adapter{}},
		Secrets:  modelbroker.EnvResolver{"provider-one": credentialEnv},
	})

	req := modelbroker.Request{
		ModelRequestID: contracts.ModelRequestID("mreq_live_vision"),
		RouteRevision:  1,
		ModelStepID:    "step-1",
		Model:          liveModel(),
		Messages: []modelbroker.Message{{
			Role: "user",
			// The owner's question, verbatim, plus the instruction that makes the answer machine-checkable.
			Content: "ne yazıyor burada? Reply with only the text you can read, nothing else.",
			Images:  []modelbroker.Image{{MediaType: "image/png", Data: pixels}},
		}},
		Deadline: time.Now().Add(90 * time.Second),
		// AN IMAGE IS EXPENSIVE, and this number is MEASURED. The first run reserved 2000 tokens — the figure
		// every other case in this tier uses — and was refused with "14196 tokens over the 2000 reserved". This
		// render costs ~8.5k input tokens; a larger one measured 19.9k. The mini models bill image tiles at a
		// far higher multiplier than the full-size ones.
		//
		// Read those numbers before raising execution.maxRunImages: at ~8-20k tokens per screenshot, a handful
		// of images in one thread is a five-figure prompt. That is what the caps are for, and why they are low.
		Reservation: modelbroker.Reservation{MaxTotalTokens: 30000},
		Secret:      modelbroker.SecretRef("provider-one"),
	}

	var streamed bytes.Buffer
	res, err := broker.Route(context.Background(), "provider-one", req, func(d modelbroker.Delta) {
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
	if !strings.HasPrefix(res.ProviderRequestID, "chatcmpl") {
		t.Errorf("provider request id %q is not a real chat completion id", res.ProviderRequestID)
	}
	if len(res.Deltas) == 0 {
		t.Error("no streamed deltas were captured")
	}
	if res.Attempts != 1 {
		t.Errorf("attempts = %d, want exactly 1 (no hidden provider retry)", res.Attempts)
	}
	// Vision costs input tokens: an image that never reached the provider would bill like a bare sentence.
	// The one-line prompt alone is well under 100 prompt tokens and this render measured 8524. The floor is
	// deliberately loose — it catches an image that was DROPPED, it does not pin a tokenizer.
	if res.Usage.InputTokens < 1000 {
		t.Errorf("input tokens = %d, too few for a prompt carrying an image — was the image dropped from the request?", res.Usage.InputTokens)
	}

	// The credential must be absent from every captured surface. The value is used only as an opaque needle
	// and is never printed.
	for name, surface := range map[string]string{
		"output":              res.Output,
		"streamed text":       streamed.String(),
		"provider request id": res.ProviderRequestID,
		"model":               res.Model,
	} {
		if strings.Contains(surface, secret) {
			t.Fatalf("the credential leaked into the %s", name)
		}
	}
}
