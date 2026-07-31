"use client";

import { useId, useState } from "react";

import { apiSend, RelayError } from "@/lib/api";
import { absoluteTime, relativeTime, type NameSource, type SessionRow } from "@/lib/sessions";

// The four pieces both session screens render, in one file so the list and the transcript cannot disagree
// about what a name, an agent, a token pair or a timestamp looks like.

/**
 * SessionName is the row identity, and it makes ONE distinction the API added a whole field for.
 *
 * `name_source` is `operator` | `derived` | `none`, and the schema says why the console needs it: "presenting
 * a derived cut as the session's name is a claim nobody made". A derived label is the first eighty runes of
 * the first prompt — on the live stack, twenty-four sessions in a row all read "Push the release branch."
 * because that is what was typed — and an operator who renames one is not correcting a mistake, they are
 * making a claim for the first time. The marker is what tells the two apart at a glance.
 */
export function SessionName({ name, source }: { name: string; source: NameSource }) {
  const named = name.trim() !== "";
  return (
    <span className="session-name">
      <span className="name" data-unnamed={named ? undefined : "true"}>
        {named ? name : "— unnamed"}
      </span>
      {source === "derived" ? (
        <span className="name-mark" title="Derived from this session's first prompt — nobody chose it">
          auto
        </span>
      ) : null}
    </span>
  );
}

/**
 * AgentChips renders the DISTINCT agent profiles this session's runs pinned — plural, because that is what
 * the field is.
 *
 * EMPTY IS THE COMMON CASE AND IT IS NOT AN ERROR. Measured against the live stack on 2026-07-31, every
 * session on it carries `agents: []`: a run pinned to no agent profile, or pinned to a run TEMPLATE (which
 * carries executable config and deliberately does not impersonate an agent identity), contributes nothing
 * here. So the cell says which of the two nothings it is rather than going blank.
 */
export function AgentChips({ agents }: { agents: string[] }) {
  if (agents.length === 0) return <span className="cell-none">— none pinned</span>;
  return (
    <ul className="chip-list">
      {agents.map((agent) => (
        <li key={agent}>
          <span className="chip" data-kind="agent">
            <AgentGlyph />
            {agent}
          </span>
        </li>
      ))}
    </ul>
  );
}

/**
 * AgentGlyph is the chip's icon, drawn here for the reason components/Chrome.tsx draws the wordmark here: an
 * icon set is a dependency, a font request and a CSP entry, and this console has none of the three. Two
 * strokes — a head and a shoulder line — at the text's own colour, aria-hidden because the chip's text names
 * the agent and "icon" is noise in a screen reader.
 */
function AgentGlyph() {
  return (
    <svg className="chip-glyph" viewBox="0 0 12 12" width="11" height="11" aria-hidden="true" focusable="false">
      <circle cx="6" cy="4" r="2.1" fill="none" stroke="currentColor" strokeWidth="1.2" />
      <path d="M1.9 10.6a4.3 4.3 0 0 1 8.2 0" fill="none" stroke="currentColor" strokeWidth="1.2" strokeLinecap="round" />
    </svg>
  );
}

/**
 * Stamp is a timestamp as a reader wants it and as a machine needs it, in one element: the relative form is
 * what is on screen, the ISO form is the `datetime` attribute and the title.
 *
 * A NULL IS AN EM DASH IN A PLAIN SPAN, not an empty <time>. `first_activity_at` is null for a session that
 * has never run, and <time> with no datetime is invalid markup describing a moment that does not exist.
 */
export function Stamp({ iso }: { iso: string | null | undefined }) {
  if (iso === null || iso === undefined || iso === "") return <span className="cell-none">—</span>;
  return (
    <time dateTime={iso} title={absoluteTime(iso)}>
      {relativeTime(iso)}
    </time>
  );
}

/**
 * RenameSession is the one WRITE on either screen: PATCH /v1/sessions/{id}, through the relay, with no key.
 *
 * IT IS DELIBERATELY NOT A FORM ELEMENT, and that is a security decision this console already made once.
 * CON-013: a form element with no `method="post"` submits as GET before React hydrates and puts every named
 * field in the address bar — an operator's password once reached a real browser history that way. The guard
 * written after it (tests/auth.spec.ts) asserts that exactly ONE file in the whole tree contains a form
 * element, because that one carries the attribute; the match is deliberately blunt enough that a COMMENT
 * mentioning the tag trips it too, which is why this paragraph spells the word out and quotes no markup.
 * components/ResourceForm.tsx is a `<section class="panel">` with an `<h2>`, which is the wrong shape for a
 * control that has to live inside a table cell — so rather than add a second one and widen that allow-list,
 * this is a labelled input and two buttons. There is no native submit here at all, so the hazard the
 * attribute defends against does not exist; Enter is wired by hand, which is the one thing the element would
 * otherwise have given for free.
 *
 * ResourceForm's four disciplines are kept verbatim: a programmatic label, a role="alert" refusal in the
 * SERVER's own words (a 400 for an over-long name and a 404 for a reaped session say different things), text
 * status rather than colour, and real controls in document order.
 */
export function RenameSession({
  sessionId,
  name,
  onDone,
  onCancel,
  testId,
}: {
  sessionId: string;
  name: string;
  onDone: (row: SessionRow) => void;
  onCancel: () => void;
  testId: string;
}) {
  const inputId = useId();
  const [value, setValue] = useState(name);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState("");

  async function submit() {
    setBusy(true);
    setError("");
    try {
      const row = await apiSend<SessionRow>("PATCH", `/sessions/${encodeURIComponent(sessionId)}`, { name: value });
      onDone(row);
    } catch (err: unknown) {
      setError(err instanceof RelayError ? err.problem.detail : "the rename did not reach the API");
    } finally {
      setBusy(false);
    }
  }

  return (
    <div className="rename" role="group" aria-label="Rename this session" data-testid={testId}>
      <label htmlFor={inputId}>Name</label>
      <div className="rename-row">
        <input
          id={inputId}
          type="text"
          value={value}
          maxLength={200}
          autoComplete="off"
          data-testid={`${testId}-input`}
          onChange={(e) => setValue(e.target.value)}
          // Enter saves and Escape abandons, by hand, because there is no form to do it. Without these the
          // control would be mouse-only in practice — a keyboard operator would type a name and find nothing
          // to press.
          onKeyDown={(e) => {
            if (e.key === "Enter") void submit();
            if (e.key === "Escape") onCancel();
          }}
        />
        <button type="button" className="primary" disabled={busy} aria-busy={busy ? "true" : undefined} data-testid={`${testId}-save`} onClick={() => void submit()}>
          {busy ? "…" : "Save"}
        </button>
        <button type="button" onClick={onCancel} data-testid={`${testId}-cancel`}>
          Cancel
        </button>
      </div>
      {error === "" ? null : (
        <p role="alert" className="form-error" data-testid={`${testId}-error`}>
          <span className="glyph" aria-hidden="true">
            ✖︎
          </span>{" "}
          {error}
        </p>
      )}
    </div>
  );
}
