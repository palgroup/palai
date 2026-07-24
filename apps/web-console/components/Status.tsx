// Status renders a run/approval status as a GLYPH + WORD, never color alone (UI-001, §47.5). The glyph
// and the text both carry the meaning; the class only adds a redundant color hint. A screen reader reads
// the word; a colorblind user reads the glyph + word.
export function Status({ value, testId }: { value: string; testId?: string }) {
  const glyph = glyphFor(value);
  return (
    <span className="status" data-testid={testId} data-status={value}>
      <span className="glyph" aria-hidden="true">
        {glyph}
      </span>
      <span>{value}</span>
    </span>
  );
}

function glyphFor(value: string): string {
  const v = value.toLowerCase();
  if (v.includes("complete") || v.includes("approved") || v.includes("succeed") || v.includes("restored")) return "✔"; // heavy check
  if (v.includes("fail") || v.includes("denied") || v.includes("error") || v.includes("lost")) return "✖"; // heavy multiply
  if (v.includes("wait") || v.includes("pending") || v.includes("recover") || v.includes("stream")) return "○"; // open circle
  if (v.includes("cancel") || v.includes("expired") || v.includes("timed")) return "⊘"; // circled slash
  return "•"; // bullet
}
