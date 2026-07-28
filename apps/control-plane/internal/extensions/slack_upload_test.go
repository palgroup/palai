package extensions

import (
	"strings"
	"testing"
)

// THE MODEL'S URL IS A NAME. This is the function that turns a reference a model wrote into something we look
// up in our OWN store, and everything it refuses is a thing that would otherwise be a lookup key we did not
// choose. It never dials anything, so the risk is not SSRF — it is naming the wrong artifact, and a
// near-miss that resolves is worse than one that does not.
func TestArtifactIDIsRecognisedByShapeAndNearMissesAreRefused(t *testing.T) {
	const id = "art_0123456789abcdef0123456789abcdef"
	for _, ref := range []string{
		"https://palai.example/v1/artifacts/" + id + "/content", // the retrieval route this tree serves
		"https://palai.example/v1/artifacts/" + id,
		id,                             // a model that names the thing directly
		"see " + id + " for the video", // prose around it
		"https://palai.example/x?artifact=" + id,
	} {
		if got := artifactIDFromRef(ref); got != id {
			t.Fatalf("artifactIDFromRef(%q) = %q, want %q", ref, got, id)
		}
	}
	for _, ref := range []string{
		"",
		"https://evil.test/v1/artifacts/art_short/content",
		"art_0123456789abcdef0123456789abcde",             // 31 hex: one short
		"art_0123456789abcdef0123456789abcdefa",           // 33 hex: one long, so it is NOT an id
		"xart_0123456789abcdef0123456789abcdef",           // the tail of a longer token
		"art_0123456789abcdef0123456789abcdeZ",            // not hex
		"art_0123456789ABCDEF0123456789ABCDEF",            // upper case: ids are minted lower
		"art_0123456789abcdef0123456789abcdef_evil",       // longer token again
		"https://palai.example/v1/artifacts/art_/content", // nothing after the prefix
	} {
		if got := artifactIDFromRef(ref); got != "" {
			t.Fatalf("artifactIDFromRef(%q) = %q, want nothing — a near-miss must not resolve", ref, got)
		}
	}
}

// ONLY A file_ref NAMES AN UPLOAD, and it is the renderer's own parser that decides what a file_ref is — so a
// shape the renderer treats as inert text cannot become an upload either.
func TestArtifactRefsComeOnlyFromFileRefsInOrderAndDeduplicated(t *testing.T) {
	const a = "art_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	const b = "art_bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	answer := `[
	  {"type":"text","text":"here is ` + a + ` in prose, which names nothing"},
	  {"type":"file_ref","url":"https://palai.example/v1/artifacts/` + b + `/content","label":"the recording"},
	  {"type":"file_ref","url":"https://palai.example/v1/artifacts/` + a + `/content","label":"the screenshot"},
	  {"type":"file_ref","url":"https://palai.example/v1/artifacts/` + a + `/content","label":"again"},
	  {"type":"video","url":"https://palai.example/v1/artifacts/` + a + `/content"}
	]`
	refs := artifactRefs(answer, 3)
	if len(refs) != 2 {
		t.Fatalf("artifactRefs found %d ref(s) (%+v), want 2 — prose, a repeat and an unknown variant name nothing", len(refs), refs)
	}
	if refs[0].id != b || refs[0].label != "the recording" {
		t.Fatalf("ref 0 = %+v, want the first file_ref in the model's own order", refs[0])
	}
	if refs[1].id != a || refs[1].label != "the screenshot" {
		t.Fatalf("ref 1 = %+v, want the second file_ref with its FIRST label", refs[1])
	}

	// Plain prose is not an upload instruction, whatever it contains.
	if refs := artifactRefs("the file is at https://palai.example/v1/artifacts/"+a+"/content", 3); len(refs) != 0 {
		t.Fatalf("prose naming an artifact produced %d upload(s), want 0", len(refs))
	}

	// The cap bounds what a model can ask for.
	var many []string
	for i := 0; i < 8; i++ {
		many = append(many, `{"type":"file_ref","url":"https://palai.example/v1/artifacts/art_`+
			strings.Repeat(string(rune('a'+i)), 32)+`/content"}`)
	}
	if refs := artifactRefs("["+strings.Join(many, ",")+"]", 3); len(refs) != 3 {
		t.Fatalf("a model asking for 8 uploads got %d, want the cap of 3", len(refs))
	}
}

// A REFUSAL IS SAID OUT LOUD. An artifact too big to publish, or one that could not be read, appears in the
// answer as a sentence — "not uploaded" plus "not mentioned" is a silent drop, which is the one thing this
// leg must never be.
func TestUploadNoteSaysWhatWasNotAttached(t *testing.T) {
	if note := uploadNote(0, 0); note != "" {
		t.Fatalf("nothing was refused and the answer still carried %q", note)
	}
	one := uploadNote(1, 0)
	if !strings.Contains(one, "8 MiB") || !strings.Contains(one, "one file") {
		t.Fatalf("the over-ceiling note = %q, want it to name the ceiling in the units a human reads", one)
	}
	if !strings.Contains(uploadNote(3, 0), "3 files") {
		t.Fatalf("the plural over-ceiling note = %q", uploadNote(3, 0))
	}
	both := uploadNote(1, 2)
	if !strings.Contains(both, "over the") || !strings.Contains(both, "2 files this run referred to") {
		t.Fatalf("a mixed refusal = %q, want both reasons", both)
	}
	// It carries no artifact id: an id in a channel is noise to every human who reads it, and this note is
	// entirely OURS — a fixed sentence and an integer, with no field of any payload in it.
	if strings.Contains(both, "art_") {
		t.Fatalf("the note leaked an artifact id: %q", both)
	}
}
