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
  // A SIXTH BAND, AND IT IS THE ONE THIS CLASSIFIER WAS MISSING (E30 visual identity).
  //
  // `active`, `running`, `provisioning` and `queued` matched NOTHING above and fell through to `neutral`, so
  // on a list where nineteen of twenty sessions are active, the column that says what a session is DOING was
  // twenty identical grey pills. That is the exact defect this console keeps finding in its own code — a
  // signal that is shipped, rendered, and carries no information — and it survived because grey is not wrong,
  // it is merely silent.
  //
  // The band is the LIFECYCLE lane's cyan (app/globals.css --live-bg), which is not a coincidence and not a
  // free choice: a session that is open and a journal frame that moves a run's state machine are the same
  // fact at two scales, so they wear one hue. `▸` is U+25B8, which carries no Unicode emoji property — the
  // same requirement the variation selectors above exist to enforce, met here by picking a character that
  // never needed one.
  //
  // ORDER MATTERS AND THIS BRANCH IS SECOND ON PURPOSE. It sits after the `complete` arm so that a status
  // word carrying both (there is none today, and "queued_complete" would be one tomorrow) reads as finished
  // rather than as running — an ending outranks a beginning.
  if (v.includes("active") || v.includes("running") || v.includes("provision") || v.includes("queued") || v.includes("in_progress")) {
    return { glyph: "▸", kind: "live" }; // black right-pointing small triangle, no emoji property
  }
  // `revoked` IS A DANGER AND IT WAS GREY. A key list where "live" and "revoked" are the same colour is a
  // list where the one row that matters looks like the other nineteen — the same defect as `active` above,
  // on the screen where the consequence is largest. It joins the danger arm rather than getting one of its
  // own because a revoked credential and a failed run are the same kind of fact: this thing is over.
  if (v.includes("fail") || v.includes("denied") || v.includes("error") || v.includes("lost") || v.includes("revoked")) {
    return { glyph: "✖︎", kind: "danger" }; // heavy multiply, text presentation
  }
  if (v.includes("wait") || v.includes("pending") || v.includes("recover") || v.includes("stream")) {
    return { glyph: "○", kind: "info" }; // open circle
  }
  if (v.includes("cancel") || v.includes("expired") || v.includes("timed")) {
    return { glyph: "⊘", kind: "warn" }; // circled slash
  }
  // PAUSED IS A HOLD AND IT IS THE SAME BAND AS A CANCELLATION, which is a claim worth being explicit about:
  // both are a run that is NOT going to make progress until somebody does something, and amber is this
  // console's word for that everywhere else (the approval lane, the "waiting on you" card). It was falling
  // through to neutral, which said a paused session and a closed one are the same kind of thing.
  // `‖` is U+2016 DOUBLE VERTICAL LINE, which carries no Unicode emoji property.
  if (v.includes("pause") || v.includes("hold")) {
    return { glyph: "‖", kind: "warn" };
  }
  return { glyph: "•", kind: "neutral" }; // bullet
}

/**
 * statusTone is classify's colour band alone, for the surfaces that need the BAND without the pill.
 *
 * It exists so a session's activity bar and its status pill cannot disagree. They did: the bar took its hue
 * from a second table keyed on the same status word, so `paused` was amber on the strip and grey in the pill
 * in the same table row — two answers to one question, from two places that each looked correct on its own.
 * There is one classifier again, and app/sessions/page.tsx maps its six bands onto lanes rather than
 * re-deciding what a word means.
 */
export function statusTone(value: string): BadgeTone {
  return classify(value).kind;
}
