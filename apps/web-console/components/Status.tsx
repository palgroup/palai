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
  // `revoked` JOINED THIS ARM AND IT USED TO BE GREY. On /policy a live key and a REVOKED one rendered in
  // the same neutral band, so the one row that matters looked like the other nineteen. It belongs here
  // rather than in a band of its own because a revoked credential and a failed run are the same kind of
  // fact: this thing is over, and nothing further will come of it.
  if (v.includes("fail") || v.includes("denied") || v.includes("error") || v.includes("lost") || v.includes("revoked")) {
    return { glyph: "✖︎", kind: "danger" }; // heavy multiply, text presentation
  }
  if (v.includes("wait") || v.includes("pending") || v.includes("recover") || v.includes("stream")) {
    return { glyph: "○", kind: "info" }; // open circle
  }
  // PAUSED IS A HOLD, AND IT IS THE SAME BAND AS A CANCELLATION ON PURPOSE: both are a thing that will make
  // no further progress until somebody does something. It was falling through to neutral, which said a
  // paused session and a closed one are the same kind of thing. `‖` is U+2016 DOUBLE VERTICAL LINE, which
  // carries no Unicode emoji property — the requirement the variation selectors above exist to meet, met
  // here by picking a character that never needed one.
  if (v.includes("cancel") || v.includes("expired") || v.includes("timed")) {
    return { glyph: "⊘", kind: "warn" }; // circled slash
  }
  if (v.includes("pause") || v.includes("hold")) {
    return { glyph: "‖", kind: "warn" };
  }
  // THE SIXTH BAND, WHICH IS WHAT THE TODO HERE ASKED FOR. `active`, `running`, `provisioning` and `queued`
  // matched nothing above and fell through to neutral, so on a list where nineteen of twenty sessions are
  // active the column that says what a session is DOING was twenty identical grey pills.
  //
  // THE HUE IS TEAL AND IT COULD NOT BE TAKEN FROM THE REFERENCE — app/globals.css's --_teal-3 carries the
  // measurement in full, and the short form is that the reference records no hue for a running state at all
  // (its pill's only measured colour is a NEUTRAL 5%-white surface) and none of its four chromatic families
  // is unclaimed. Teal is 41° from `ok`'s grass and 34° from `info`'s blue, which are the two readings this
  // pill sits beside in the same column.
  //
  // `▸` IS U+25B8 BLACK RIGHT-POINTING SMALL TRIANGLE, which carries no Unicode emoji property — so it needs
  // no variation selector, the same requirement `○`, `⊘` and `•` already meet by construction.
  //
  // IT IS LAST, AND THAT ORDER IS A DECISION. `queued` lands here while `pending` lands in `info` one arm
  // up, and the two words are not synonyms in this API: a RUNNER is `pending` admission, which is waiting on
  // a human, and a RUN is `queued`, which is the machine already holding it. An ending outranks a beginning
  // for the same reason — a word carrying both reads as finished.
  if (v.includes("active") || v.includes("running") || v.includes("provision") || v.includes("queued") || v.includes("in_progress")) {
    return { glyph: "▸", kind: "live" };
  }
  return { glyph: "•", kind: "neutral" }; // bullet
}
