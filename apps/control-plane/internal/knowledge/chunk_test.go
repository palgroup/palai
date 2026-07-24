package knowledge

import (
	"strings"
	"testing"
	"unicode/utf8"
)

// TestChunkDocumentIsDeterministicWithVerifiableOffsets is the chunker's honest check: chunking is a pure
// function (same input -> same chunks) and every chunk's [ByteStart,ByteEnd) slice of the original bytes
// equals its Content — the invariant the citation-offset proof (KNO-001 / T11 verifier) recomputes.
func TestChunkDocumentIsDeterministicWithVerifiableOffsets(t *testing.T) {
	doc := "The quick brown fox.\n\nJumps over the lazy dog.\n\nA third short paragraph."

	first := chunkDocument(doc, defaultMaxChunkBytes)
	second := chunkDocument(doc, defaultMaxChunkBytes)

	if len(first) != 3 {
		t.Fatalf("chunkDocument produced %d chunks, want 3 paragraphs", len(first))
	}
	if len(first) != len(second) {
		t.Fatalf("chunker is not deterministic: %d vs %d chunks", len(first), len(second))
	}
	for i := range first {
		if first[i] != second[i] {
			t.Fatalf("chunk %d differs between runs: %+v vs %+v", i, first[i], second[i])
		}
		// The load-bearing citation invariant: the byte offsets recover the exact chunk content.
		if got := doc[first[i].ByteStart:first[i].ByteEnd]; got != first[i].Content {
			t.Fatalf("chunk %d offsets [%d,%d) recover %q, want content %q",
				i, first[i].ByteStart, first[i].ByteEnd, got, first[i].Content)
		}
		if first[i].Ordinal != i {
			t.Fatalf("chunk %d has ordinal %d, want %d", i, first[i].Ordinal, i)
		}
	}
}

// TestChunkDocumentHardSplitsAnOversizedParagraph proves no chunk is unbounded: a single paragraph longer
// than maxBytes is split, and the pieces still tile the original bytes contiguously.
func TestChunkDocumentHardSplitsAnOversizedParagraph(t *testing.T) {
	doc := "aaaaaaaaaa bbbbbbbbbb cccccccccc" // 31 bytes, no blank lines
	chunks := chunkDocument(doc, 10)
	if len(chunks) < 3 {
		t.Fatalf("oversized paragraph split into %d chunks, want >=3", len(chunks))
	}
	var joined string
	for _, c := range chunks {
		if c.ByteEnd-c.ByteStart > 10 {
			t.Fatalf("chunk exceeds maxBytes: %+v", c)
		}
		if got := doc[c.ByteStart:c.ByteEnd]; got != c.Content {
			t.Fatalf("hard-split chunk offsets recover %q, want %q", got, c.Content)
		}
		joined += c.Content
	}
	if joined != doc {
		t.Fatalf("hard-split chunks do not tile the document: %q vs %q", joined, doc)
	}
}

// TestChunkDocumentNeverSplitsAUTF8Rune is the Turkish-critical invariant (SF-2): a hard split that would
// land mid-rune walks back to a rune boundary so EVERY chunk is valid UTF-8. Postgres rejects invalid UTF-8
// text params (SQLSTATE 22021), so a mid-rune split fails the ingest and makes Turkish/CJK/emoji prose
// unindexable. Offsets stay byte-true (the citation invariant) and the pieces still tile the document.
func TestChunkDocumentNeverSplitsAUTF8Rune(t *testing.T) {
	doc := strings.Repeat("ğ", 40) // 80 bytes of 2-byte runes, no blank lines
	chunks := chunkDocument(doc, 15) // 15 lands mid-rune on every boundary (each rune is 2 bytes)
	if len(chunks) < 2 {
		t.Fatalf("oversized 2-byte-rune paragraph split into %d chunks, want >=2", len(chunks))
	}
	var joined string
	for _, c := range chunks {
		if !utf8.ValidString(c.Content) {
			t.Fatalf("chunk %d is invalid UTF-8 (mid-rune split): %q", c.Ordinal, c.Content)
		}
		if got := doc[c.ByteStart:c.ByteEnd]; got != c.Content {
			t.Fatalf("chunk %d offsets [%d,%d) recover %q, want %q", c.Ordinal, c.ByteStart, c.ByteEnd, got, c.Content)
		}
		joined += c.Content
	}
	if joined != doc {
		t.Fatalf("rune-safe chunks do not tile the document")
	}

	// Pathological: maxBytes smaller than a single rune must still emit each whole rune, never infinite-loop.
	tiny := chunkDocument("ğşç😀", 1) // three 2-byte runes + one 4-byte emoji
	if len(tiny) != 4 {
		t.Fatalf("sub-rune maxBytes produced %d chunks, want 4 whole runes", len(tiny))
	}
	for _, c := range tiny {
		if !utf8.ValidString(c.Content) || utf8.RuneCountInString(c.Content) != 1 {
			t.Fatalf("sub-rune chunk is not exactly one valid rune: %q", c.Content)
		}
	}
}
