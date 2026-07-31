"use client";

import { useEffect, useRef, type ReactNode } from "react";

// THE CONFIRMATION FOR WHAT CANNOT BE UNDONE — AND `window.confirm` STAYS (E28 T2, plan §3.5 W1/W2/W5).
//
// READ THIS BEFORE CONVERTING ANYTHING TO IT. The next person here will want to replace every window.confirm
// in the console with this component. That would be wrong, and the threshold is published rather than a
// preference:
//
//   WCAG 2.2 SC 3.3.4 Error Prevention (Legal, Financial, Data) applies to pages that "modify or delete
//   user-controllable data in data storage systems", and it asks for ONE of three legs — 1. Reversible,
//   2. Checked, 3. Confirmed ("a mechanism is available for REVIEWING, confirming, and correcting
//   information before finalizing the submission").
//   https://www.w3.org/WAI/WCAG22/Understanding/error-prevention-legal-financial-data.html
//
//   `cordon` / `resume` / `unbind` satisfy leg 1. A cordon is undone by a resume, verbatim in
//   execution/runner_gateway.go:324, and unbinding an environment key removes a binding an operator can
//   re-create. For those, a `window.confirm` is not a compromise — it is keyboard-operable,
//   screen-reader-announced and focus-trapped BY THE BROWSER, every property this file has to re-earn and
//   that axe cannot check for us (app/environments/page.tsx:150-155 wrote that down first).
//
//   `revoke` cannot satisfy leg 1: runner_gateway.go:328, verbatim, "a revoked runner identity is
//   decommissioned, not paused", and :381-394 "a revoke is ONE-WAY: once revoked, neither flag comes back".
//   Leg 2 is vacuous — there is no data entered to check. So leg 3 is the only one left, and the word
//   "reviewing" inside it is a REQUIREMENT rather than a description: the dialog must show what is about to
//   die. That is more than one sentence and a yes/no, which is precisely the upgrade threshold the
//   environments page named, so this component is justified for revoke AND FOR NOTHING ELSE YET.
//
// THE FOUR ARIA REQUIREMENTS ARE THE W3C ARIA APG's, and once a hand-rolled dialog is justified they are not
// negotiable: role="alertdialog" on the container, aria-modal="true", aria-labelledby pointing at the title,
// aria-describedby pointing at the message, plus the modal-dialog KEYBOARD contract — Tab cannot leave, and
// Escape cancels. https://www.w3.org/WAI/ARIA/apg/patterns/alertdialog/
//
// FOCUS STARTS ON CANCEL, AND NO ACCESSIBILITY REQUIREMENT IS CLAIMED FOR THAT (plan §3.5 W5, UNCONFIRMED).
// The APG describes a CHOICE of "the element that will receive focus when the dialog opens" and publishes no
// normative sentence preferring the least destructive action; that was searched for and not found. This
// matches what `window.confirm` already does in this console, and "do not change the behaviour" is the
// cheapest way to make no claim. Filed as FLC-P5.
//
// ponytail: rendered INLINE, with a backdrop that blocks pointer events. No portal, no scroll lock, no
// `inert` on the rest of the document — `aria-modal="true"` is what the APG specifies for constraining a
// screen reader, and the backdrop is what keeps a click from moving focus out from under the Tab trap.
// Upgrade path: if a dialog ever has to open over a scrolling region or from inside an overflow-hidden
// ancestor, portal it then.

const FOCUSABLE = 'a[href], button:not([disabled]), input:not([disabled]), select:not([disabled]), textarea:not([disabled]), [tabindex]:not([tabindex="-1"])';

export function ConfirmDestructive({
  title,
  /** The alert message. It is what aria-describedby points at, so it is READ — keep it to what will be lost. */
  message,
  /** The REVIEW half of SC 3.3.4 leg 3: what is about to be destroyed, as data rather than as prose. */
  review,
  confirmLabel,
  onConfirm,
  onCancel,
  testId,
  busy = false,
}: {
  title: string;
  message: ReactNode;
  review?: ReactNode;
  confirmLabel: string;
  onConfirm: () => void;
  onCancel: () => void;
  testId: string;
  busy?: boolean;
}) {
  const dialog = useRef<HTMLDivElement | null>(null);
  const cancel = useRef<HTMLButtonElement | null>(null);

  useEffect(() => {
    // Focus moves IN on open and BACK on close. Returning it is not decoration: a keyboard user who cancels
    // and lands at the top of the document has lost the row they were working on.
    const previous = document.activeElement instanceof HTMLElement ? document.activeElement : null;
    cancel.current?.focus();
    return () => previous?.focus();
  }, []);

  return (
    <>
      <div className="dialog-backdrop" data-testid={`${testId}-backdrop`} />
      <div
        ref={dialog}
        role="alertdialog"
        aria-modal="true"
        aria-labelledby={`${testId}-title`}
        aria-describedby={`${testId}-message`}
        className="dialog"
        data-testid={testId}
        onKeyDown={(event) => {
          if (event.key === "Escape") {
            event.stopPropagation();
            onCancel();
            return;
          }
          if (event.key !== "Tab") return;
          // THE TRAP. It wraps rather than merely blocking, because a Tab that does nothing reads as a broken
          // page; the focusable set is READ AT KEYPRESS, not cached, so a control that appears while the
          // dialog is open (a busy state swapping a button) is in the cycle.
          const nodes = [...(dialog.current?.querySelectorAll<HTMLElement>(FOCUSABLE) ?? [])].filter((el) => el.offsetParent !== null || el === document.activeElement);
          if (nodes.length === 0) return;
          const [first, last] = [nodes[0], nodes[nodes.length - 1]];
          const active = document.activeElement;
          const inside = dialog.current?.contains(active) === true;
          if (event.shiftKey && (active === first || !inside)) {
            event.preventDefault();
            last.focus();
          } else if (!event.shiftKey && (active === last || !inside)) {
            event.preventDefault();
            first.focus();
          }
        }}
      >
        <h2 id={`${testId}-title`}>{title}</h2>
        <p id={`${testId}-message`} data-testid={`${testId}-message`}>
          {message}
        </p>
        {review === undefined ? null : (
          <div className="dialog-review" data-testid={`${testId}-review`}>
            {review}
          </div>
        )}
        <p className="dialog-actions">
          {/* CANCEL IS FIRST IN DOCUMENT ORDER as well as focused first, so the tab cycle starts at the safe
              end for a user who never sees the focus ring. */}
          <button type="button" ref={cancel} data-testid={`${testId}-cancel`} onClick={onCancel}>
            Cancel
          </button>{" "}
          <button type="button" className="danger" data-testid={`${testId}-confirm`} disabled={busy} onClick={onConfirm}>
            {busy ? `${confirmLabel}…` : confirmLabel}
          </button>
        </p>
      </div>
    </>
  );
}
