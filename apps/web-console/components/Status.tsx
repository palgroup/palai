import { Badge, type BadgeTone } from "@/components/ui/Badge";

// Status renders a run/approval status as a GLYPH + WORD, never color alone (UI-001, §47.5). The glyph
// and the text both carry the meaning; the color is a redundant third layer. A screen reader reads the word;
// a colorblind user reads the glyph + word; remove either and the chip means nothing, which is the property
// that keeps it conforming.
//
// THE COLOUR HINT IS NOW REAL, AND IT WAS NOT BEFORE. The old comment on this file promised that "the class
// only adds a redundant color hint" while app/globals.css carried NO [data-status] rule at all — the claim
// was true of the behaviour and false about the code. Rather than correct the sentence downward, the hint is
// added: `data-glyph` names the CLASSIFICATION this component already computed, so the stylesheet colours a
// chip from the same decision that chose the glyph. One classifier, not two that can drift apart.
//
// THE VARIATION SELECTOR IS NOT DECORATION. Unicode's own emoji-data.txt gives U+2714 HEAVY CHECK MARK and
// U+2716 HEAVY MULTIPLICATION X the `Emoji` and `Extended_Pictographic` properties, and neither appears in
// `Emoji_Presentation` — so the DEFAULT presentation is text, but a platform is free to pick a colour emoji
// glyph. That would reintroduce colour as a carrier on the one element built to avoid it, and it would
// change the glyph's advance width, which is what makes a column of statuses stop lining up. U+FE0E (VS15)
// pins text presentation. `○` (U+25CB), `⊘` (U+2298) and `•` (U+2022) carry no emoji property and are left
// exactly as they are.
//
// E29 COMPONENT LAYER — THE PILL MOVED, THE CLASSIFIER DID NOT. The markup is now components/ui/Badge.tsx
// and this file keeps `classify`, which is the half that is DOMAIN knowledge: "restored" is an ok and "lost"
// is a danger are facts about run lifecycles, and a primitive that knew them would not be a primitive. Not a
// token, a class or a pixel changes — Badge renders the same `<span className="status" data-glyph>` this
// function has always produced.
export function Status({ value, testId }: { value: string; testId?: string }) {
  const { glyph, kind } = classify(value);
  return (
    <Badge tone={kind} glyph={glyph} title={value} testId={testId}>
      {value}
    </Badge>
  );
}

/** classify is the ONE mapping from a status word to its glyph and its colour band. */
function classify(value: string): { glyph: string; kind: BadgeTone } {
  const v = value.toLowerCase();
  if (v.includes("complete") || v.includes("approved") || v.includes("succeed") || v.includes("restored")) {
    return { glyph: "✔︎", kind: "ok" }; // heavy check, text presentation
  }
  if (v.includes("fail") || v.includes("denied") || v.includes("error") || v.includes("lost")) {
    return { glyph: "✖︎", kind: "danger" }; // heavy multiply, text presentation
  }
  if (v.includes("wait") || v.includes("pending") || v.includes("recover") || v.includes("stream")) {
    return { glyph: "○", kind: "info" }; // open circle
  }
  if (v.includes("cancel") || v.includes("expired") || v.includes("timed")) {
    return { glyph: "⊘", kind: "warn" }; // circled slash
  }
  return { glyph: "•", kind: "neutral" }; // bullet
}
