"use client";

import { useEffect, useState, type ReactNode } from "react";

import { Button } from "@/components/ui/Button";

// THE OTHER DIRECTION FROM SecretField (E28 T2, plan §3.6 D21).
//
// SecretField handles a value the HUMAN writes and the screen never shows again. This handles a value the
// SERVER mints and the human must copy ONCE. Reading them as two shapes of one component is the mistake the
// plan measured: SecretField is an `<input type="password">` with `autocomplete="new-password"` whose bytes
// are read out of the DOM node by takeSecret() — and all three of those are INAPPLICABLE here, because a
// minted key arrives in a fetch response body and is therefore already in JavaScript memory before any
// component exists. What the two share is exactly one rule, and it is the load-bearing one:
//
//   THE VALUE NEVER ENTERS REACT STATE. It arrives as a prop, it is rendered into one DOM node, and this
//   component copies it NOWHERE — no useState, no useRef cache, no effect that reads it, no log line, no
//   sessionStorage, no URL. The one useEffect below carries an announcement string and is written so that
//   putting the value in it would be an obvious edit.
//
// It is the browser's mirror of the server's own behaviour: poolKeyView(item, true) is used by the mint route
// and by nothing else (api/runners.go:282 vs :298,:320), and CreateAPIKey sets apiKeyView.Key on the create
// response alone (identity/store.go). The screen shows the value once and cannot bring it back, because the
// API cannot either.
//
// WHAT IS *NOT* CLAIMED, and the plan names it as this component's honest ceiling: the bytes are not gone from
// BROWSER MEMORY. A fetch response body lives until GC and no web API controls that. What is claimed and
// proven (tests/reveal-once.spec.ts) is that the value is written to no PERSISTENT place — not the DOM after
// dismissal, not either storage, not the URL, not a later response body — and that it cannot be retrieved.

/**
 * canCopy is W3 rendered as a branch: Clipboard.writeText is a SECURE-CONTEXT API. MDN, verbatim: "Writing to
 * the clipboard can only be done in a secure context." The console over `pnpm dev` on localhost is one; an
 * operator reaching `http://<host>:3000` is NOT, and that posture is supported in this tree (compose's edge
 * TLS lives in the production.yml overlay, not in the base profile). So the button is CONDITIONAL and the
 * fallback is the carrying path rather than a courtesy.
 *
 * It is read during render rather than resolved in an effect. That is correct here and the reason is
 * structural: this component only ever mounts in response to a click, so it is never part of a prerender and
 * there is no server-rendered markup for it to disagree with. If a caller ever renders it during SSR, the
 * button would be absent on the server pass and present after hydration — a mismatch React repairs, and a
 * reason for that caller to reconsider rather than for this to grow a `useEffect`.
 */
function canCopy(): boolean {
  return typeof navigator !== "undefined" && typeof navigator.clipboard?.writeText === "function";
}

export function RevealOnce({
  value,
  label,
  announcement,
  testId,
  meta,
  onDismiss,
}: {
  /** The minted value. It is rendered and NOTHING else — see the header. */
  value: string;
  /** The heading, e.g. "New API key". */
  label: string;
  /** What the status region says. It must NEVER contain the value; it is announced, and announcements are read aloud. */
  announcement: string;
  testId: string;
  /** Non-secret identity for the thing that was minted — an id, a scope list — so the operator knows WHICH one this is. */
  meta?: ReactNode;
  /** Removing the region. The value does not come back; the caller must not keep it either. */
  onDismiss: () => void;
}) {
  // The copy button's REFUSAL, not the value. `NotAllowedError` is what writeText throws when the clipboard is
  // denied (MDN), and swallowing it leaves an operator believing they hold a credential they do not — which on
  // a one-time value means discovering it after the screen is gone.
  const [copyError, setCopyError] = useState("");
  const [copied, setCopied] = useState(false);
  // THE ANNOUNCEMENT IS SET ONE TICK AFTER MOUNT, ON PURPOSE. A live region that arrives already populated is
  // an INSERTION rather than an UPDATE, and support for announcing insertions varies by screen reader; a
  // region that mounts empty and then changes is the shape every implementation reads. This is the only
  // effect in this file and it carries a fixed string the caller wrote — never `value`.
  const [live, setLive] = useState("");
  useEffect(() => setLive(announcement), [announcement]);

  const headingID = `${testId}-h`;
  return (
    <section className="panel reveal-once" data-testid={testId} aria-labelledby={headingID}>
      <h2 id={headingID}>{label}</h2>
      {/* THE ANNOUNCEMENT. A one-time credential that appears silently is one a screen-reader user scrolls
          past; role="status" is the polite live region for exactly this. */}
      <p role="status" className="muted" data-testid={`${testId}-announce`}>
        {live}
      </p>
      {meta === undefined ? null : (
        <p className="muted" data-testid={`${testId}-meta`}>
          {meta}
        </p>
      )}
      {/* ALWAYS SELECTABLE (W4). No `user-select: none`, no onCopy/onCut/onContextMenu/onSelectStart handler,
          and there must never be one: WCAG 2.2 §3.3.8's RATIONALE — "would force the user to transcribe
          information" — does not depend on the direction of travel. The criterion itself is about
          authentication FIELDS and this claims no conformance to it; what it takes from it is the reason. */}
      <p>
        <code className="reveal-value" data-testid={`${testId}-value`}>
          {value}
        </code>
      </p>
      {canCopy() ? (
        <p>
          <Button
            testId={`${testId}-copy`}
            onClick={() => {
              setCopyError("");
              setCopied(false);
              // The value goes to the clipboard and to no local binding: no `const copied = value` above, and
              // the promise's rejection carries no value either.
              navigator.clipboard.writeText(value).then(
                () => setCopied(true),
                (err: unknown) => setCopyError(err instanceof Error ? `${err.name}: ${err.message}` : "the clipboard refused the write"),
              );
            }}
          >
            Copy
          </Button>
          {copied ? (
            <span className="form-status" data-testid={`${testId}-copied`}>
              {" "}
              <span className="glyph" aria-hidden="true">
                ✔
              </span>{" "}
              Copied.
            </span>
          ) : null}
        </p>
      ) : (
        // W3's fallback, and it is the carrying path on any console reached over plain HTTP.
        <p className="muted" data-testid={`${testId}-manual`}>
          <strong>Select the value above and copy it.</strong> This browser offers no clipboard API to this
          page — writing to the clipboard requires a secure context (HTTPS), and this console is not being
          served over one.
        </p>
      )}
      {copyError === "" ? null : (
        <p role="alert" className="form-error" data-testid={`${testId}-copy-error`}>
          <span className="glyph" aria-hidden="true">
            ✖
          </span>{" "}
          The clipboard refused the write ({copyError}). Select the value above and copy it by hand — do not
          dismiss this until you hold it.
        </p>
      )}
      {/* THE SENTENCE, IN THE SERVER'S OWN TERMS. api/runners.go:255-256, verbatim: "The value is in the
          response body and nowhere else, so a caller that loses it mints another and revokes this one." */}
      <p data-testid={`${testId}-once-note`}>
        <strong>This value is never shown again</strong> — if you lose it, mint a new one and revoke this. It
        is in this one place on the screen and in no storage, no URL and no later response.
      </p>
      <p>
        <Button testId={`${testId}-dismiss`} onClick={onDismiss}>
          I have it — dismiss
        </Button>
      </p>
    </section>
  );
}
