package knowledge

import (
	"strings"
	"unicode/utf8"
)

// chunkerRevision pins the deterministic chunking algorithm. It is recorded on every document_revision so a
// re-chunk is reproducible and an algorithm change is a NEW revision, never a silent reinterpretation of
// stored offsets (§25.15.2 "deterministic chunk, parser/chunker revision pinned").
const chunkerRevision = "para-v1"

// defaultMaxChunkBytes bounds a single chunk so an FTS row and its ts_rank stay reasonable. A paragraph
// longer than this is hard-split at the boundary.
const defaultMaxChunkBytes = 1500

// Chunk is one deterministic slice of a document: its ordinal, the [ByteStart,ByteEnd) offsets into the
// document's bytes (stable citation offsets — the verifier recomputes Content as document[ByteStart:ByteEnd]),
// and the slice content the FTS tsvector is built from.
type Chunk struct {
	Ordinal   int
	ByteStart int
	ByteEnd   int
	Content   string
}

// chunkDocument splits content into deterministic paragraph chunks with exact byte offsets. Paragraphs are
// separated by blank lines; a paragraph longer than maxBytes is hard-split at the last UTF-8 rune boundary
// at or before maxBytes, so no chunk is unbounded AND no chunk is invalid UTF-8 (a mid-rune split would fail
// the ingest — Postgres rejects invalid UTF-8 text with SQLSTATE 22021). It is a PURE function of (content,
// maxBytes) — the same input always yields the same chunks,
// which is what makes a re-ingest reproducible and a citation offset verifiable.
//
// ponytail: one text chunker for all three parsers (text/markdown/code) — office/PDF parsers and
// language-aware code splitting are §5/deferred; the parser pin is recorded per revision so a smarter
// chunker lands as chunkerRevision "para-v2" without reinterpreting old offsets.
func chunkDocument(content string, maxBytes int) []Chunk {
	if maxBytes <= 0 {
		maxBytes = defaultMaxChunkBytes
	}
	var chunks []Chunk
	ordinal := 0
	// Walk the raw bytes so ByteStart/ByteEnd are byte offsets (what the DB stores and citations reference),
	// splitting paragraphs on blank lines while carrying each paragraph's absolute start offset.
	for _, seg := range splitParagraphs(content) {
		text := strings.TrimSpace(seg.text)
		if text == "" {
			continue
		}
		// The trimmed content may start after seg.start; find its real offset so citations point at the
		// non-whitespace bytes.
		start := seg.start + strings.Index(seg.text, text)
		for off := 0; off < len(text); {
			end := off + maxBytes
			if end >= len(text) {
				end = len(text)
			} else if !utf8.RuneStart(text[end]) {
				// A hard split at maxBytes would land inside a multibyte rune (Turkish/CJK/emoji). Postgres
				// rejects invalid UTF-8 text params (22021), so walk the boundary BACK to the rune start.
				for end > off && !utf8.RuneStart(text[end]) {
					end--
				}
				// A single rune wider than maxBytes: emit the whole rune rather than loop forever.
				if end == off {
					_, size := utf8.DecodeRuneInString(text[off:])
					end = off + size
				}
			}
			chunks = append(chunks, Chunk{
				Ordinal:   ordinal,
				ByteStart: start + off,
				ByteEnd:   start + end,
				Content:   text[off:end],
			})
			ordinal++
			off = end
		}
	}
	return chunks
}

// paragraph is a raw segment of the document and its absolute byte start offset.
type paragraph struct {
	start int
	text  string
}

// splitParagraphs breaks content on blank-line boundaries ("\n\n"), preserving each segment's absolute byte
// start so offsets stay anchored to the original bytes.
func splitParagraphs(content string) []paragraph {
	var out []paragraph
	start := 0
	for i := 0; i+1 < len(content); i++ {
		if content[i] == '\n' && content[i+1] == '\n' {
			out = append(out, paragraph{start: start, text: content[start:i]})
			start = i + 1
		}
	}
	out = append(out, paragraph{start: start, text: content[start:]})
	return out
}
