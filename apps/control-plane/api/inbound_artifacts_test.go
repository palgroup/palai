package api

import (
	"encoding/json"
	"strings"
	"testing"
)

// decodeInput parses a wire-shaped `input` exactly as the create handler receives it — through
// json.Unmarshal into `any`, so the walk under test meets map[string]any / []any and float64, not Go
// literals a test author chose. A fixture built as a Go value would let the walk pass over shapes the
// decoder never actually produces.
func decodeInput(t *testing.T, raw string) any {
	t.Helper()
	var v any
	if err := json.Unmarshal([]byte(raw), &v); err != nil {
		t.Fatalf("decode fixture: %v", err)
	}
	return v
}

// TestInboundArtifactIDsFindsImageRefsInEveryInputShape is the test that decides whether an ingested
// screenshot is ever reachable by retention: the ids this returns are the ones the admission attaches to
// the run, and an id it misses is a row whose run_id stays NULL forever — bytes no §22.2 sweep can see.
//
// It runs the SHAPES rather than one of them, because `input` is `any` on the wire and all of these are
// accepted today: a bare string (the text-only turn), a flat content array (what the Slack relay sends and
// what the in-process bridge sent before it), and an array of messages each holding a content array (the
// assembled-conversation shape execution.decodeMessages consumes). A walk that understood only one would
// silently attach nothing for the others.
func TestInboundArtifactIDsFindsImageRefsInEveryInputShape(t *testing.T) {
	for _, tc := range []struct {
		name  string
		input string
		want  []string
	}{
		{
			name:  "a text-only turn names nothing",
			input: `"just words"`,
		},
		{
			name:  "a flat content array — the shape the Slack relay sends",
			input: `[{"type":"input_text","text":"fix this"},{"type":"image_ref","artifact_id":"art_a"}]`,
			want:  []string{"art_a"},
		},
		{
			name:  "messages each holding a content array",
			input: `[{"role":"user","content":[{"type":"input_text","text":"hi"},{"type":"image_ref","artifact_id":"art_b"}]}]`,
			want:  []string{"art_b"},
		},
		{
			name:  "several images keep their order",
			input: `[{"type":"image_ref","artifact_id":"art_1"},{"type":"image_ref","artifact_id":"art_2"}]`,
			want:  []string{"art_1", "art_2"},
		},
		{
			name:  "the same id twice attaches once",
			input: `[{"type":"image_ref","artifact_id":"art_x"},{"type":"image_ref","artifact_id":"art_x"}]`,
			want:  []string{"art_x"},
		},
		{
			name: "a non-image ref is not taken",
			// file_ref and audio_ref carry artifact_id too (content.json declares both). They are NOT
			// images and this walk must not attach them — it exists for the image ingest, and widening it
			// by accident would attach artifacts whose lifecycle another writer owns.
			input: `[{"type":"file_ref","artifact_id":"art_file"},{"type":"audio_ref","artifact_id":"art_audio"}]`,
		},
		{
			name:  "an image_ref with no artifact_id contributes nothing",
			input: `[{"type":"image_ref"}]`,
		},
		{
			name: "an artifact_id that is not a string is not passed on",
			// A number here would otherwise travel into a query parameter as the wrong type. The walk takes
			// a non-empty STRING or nothing.
			input: `[{"type":"image_ref","artifact_id":42},{"type":"image_ref","artifact_id":""}]`,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := inboundArtifactIDs(decodeInput(t, tc.input))
			if len(got) != len(tc.want) {
				t.Fatalf("inboundArtifactIDs() = %v, want %v", got, tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Fatalf("inboundArtifactIDs() = %v, want %v", got, tc.want)
				}
			}
		})
	}
}

// TestInboundArtifactIDsIsBounded pins the two bounds that keep untrusted request content from turning one
// admission into unbounded work inside a transaction other requests queue behind.
//
// Both are measured against a body a client can actually send, not against a Go value: the depth fixture is
// real nested JSON, so if the depth guard were removed this would recurse rather than merely return a
// different answer.
func TestInboundArtifactIDsIsBounded(t *testing.T) {
	var many strings.Builder
	many.WriteString(`[`)
	for i := 0; i < maxInboundArtifactRefs*3; i++ {
		if i > 0 {
			many.WriteString(",")
		}
		many.WriteString(`{"type":"image_ref","artifact_id":"art_`)
		many.WriteString(string(rune('a' + i%26)))
		many.WriteString(string(rune('a' + (i/26)%26)))
		many.WriteString(string(rune('a' + (i/676)%26)))
		many.WriteString(`"}`)
	}
	many.WriteString(`]`)
	if got := inboundArtifactIDs(decodeInput(t, many.String())); len(got) > maxInboundArtifactRefs {
		t.Fatalf("inboundArtifactIDs() returned %d ids, want at most %d", len(got), maxInboundArtifactRefs)
	}

	// Deeper than the walk's own limit, with the image at the very bottom. The assertion is that this
	// RETURNS — a walk with no depth bound would keep going until the stack ended.
	deep := strings.Repeat(`{"a":`, 200) + `{"type":"image_ref","artifact_id":"art_deep"}` + strings.Repeat(`}`, 200)
	if got := inboundArtifactIDs(decodeInput(t, deep)); len(got) != 0 {
		t.Fatalf("inboundArtifactIDs() reached past the depth bound and returned %v", got)
	}
}
