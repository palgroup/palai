package knowledge

import "testing"

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
