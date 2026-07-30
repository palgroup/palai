"use client";

import type { RefObject } from "react";

// THE ONE FIELD THAT TAKES A SECRET, AND ITS DESIGN IS PLAN §3.5 N17 (E25 T4).
//
// `type="password"` + `autocomplete="new-password"`, and the token is the whole point — `off` would be
// WRONG, not merely weaker. MDN, verbatim, on `new-password`: "A new password. When creating a new account
// or changing passwords, this should be used for an 'Enter your new password' or 'Confirm new password'
// field… This may be used by the browser both to AVOID ACCIDENTALLY FILLING IN AN EXISTING PASSWORD and to
// offer assistance in creating a secure password." And on `off`, the same page: "In most modern browsers,
// setting autocomplete to "off" will not prevent a password manager from asking the user if they would like
// to save username and password information, or from automatically filling in those values in a site's login
// form" — with the advice to use it "only where genuinely necessary (such as for CAPTCHA or one-time token
// fields)". An environment value is EXACTLY the case `new-password` names: the risk is the browser dropping
// the operator's saved console password into this box and sealing it as a credential an agent will use.
// (https://developer.mozilla.org/en-US/docs/Web/HTML/Reference/Attributes/autocomplete)
//
// COPY-PASTE IS NOT BLOCKED, and that is deliberate (WCAG 2.2 §3.3.8 Accessible Authentication, plan §3.5
// N12). A real credential is 40 random characters; blocking paste means retyping it by eye, which is the
// cognitive test the criterion exists to forbid — and it produces typos in a value nobody can read back to
// check. There is no onPaste handler here and there must never be one.
//
// WHAT IS NOT CLAIMED (plan §3.5 N19, filed as CON-P5): whether a browser offers to SAVE this value to a
// password manager was NOT measured. MDN documents `new-password`'s effect on FILLING; its sentence about
// save offers is scoped to "a site's login form", and no vendor publishes its password-manager heuristics.
// So `new-password` is written for the reason MDN supports BY NAME, and the save offer is left named as an
// accepted exposure in docs/operations/environments.md rather than quietly assumed away.
//
// THE FIELD IS UNCONTROLLED, AND THIS IS THE SECURITY PROPERTY RATHER THAN A REACT PREFERENCE. A controlled
// input means the secret is React state: it lives in the component tree, it is a value every re-render
// closes over, and on a Server-Component boundary it is exactly the kind of thing that ends up serialized
// into a flight payload. Here the bytes exist in ONE place — the DOM node — and takeSecret() below is the
// only way to get them out, which reads and CLEARS in a single call so a caller cannot read without
// resetting. Nothing copies the value anywhere; there is no state, no ref cache, no log line.
export function SecretField({
  inputRef,
  id = "field-secret-value",
  label,
  hint,
  testId,
  required = true,
}: {
  /** The DOM node is the only place the value lives. Read it with takeSecret(). */
  inputRef: RefObject<HTMLInputElement | null>;
  id?: string;
  label: string;
  hint?: string;
  testId?: string;
  required?: boolean;
}) {
  const hintId = `${id}-hint`;
  return (
    <div>
      {/* A programmatic label, like every other field on this surface (ResourceForm's rule 1). */}
      <label htmlFor={id}>{label}</label>
      <input
        ref={inputRef}
        id={id}
        name="value"
        type="password"
        autoComplete="new-password"
        // The two attributes a browser reads as "this is a fresh credential, do not help me": no
        // capitalisation, no spellcheck sending it anywhere.
        autoCapitalize="off"
        spellCheck={false}
        required={required}
        aria-describedby={hint ? hintId : undefined}
        data-testid={testId}
        // NO defaultValue and NO value: an uncontrolled field with no initial bytes. NO onPaste: see above.
      />
      {hint ? (
        <p className="muted" id={hintId}>
          {hint}
        </p>
      ) : null}
    </div>
  );
}

// takeSecret reads the typed value and RESETS the field in the same call — the reset is not the caller's
// discipline to remember.
//
// It clears on every submit, success or failure, and the cost of that is real and chosen: a refused write
// means retyping. The alternative — clear only on success — leaves the secret sitting in a DOM node after a
// 400, on a screen the operator may then walk away from, which is the leak this file exists to avoid. The
// returned string is a local in the caller's submit function and dies with it.
export function takeSecret(inputRef: RefObject<HTMLInputElement | null>): string {
  const el = inputRef.current;
  if (el === null) return "";
  const value = el.value;
  el.value = "";
  return value;
}
