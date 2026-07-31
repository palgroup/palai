"use client";

import { useRef, type ReactNode } from "react";

import { Button } from "@/components/ui/Button";
import { Dialog } from "@/components/ui/Dialog";

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
//   screen-reader-announced and focus-trapped BY THE BROWSER, every property this file used to have to
//   re-earn (app/environments/page.tsx:150-155 wrote that down first).
//
//   `revoke` cannot satisfy leg 1: runner_gateway.go:328, verbatim, "a revoked runner identity is
//   decommissioned, not paused", and :381-394 "a revoke is ONE-WAY: once revoked, neither flag comes back".
//   Leg 2 is vacuous — there is no data entered to check. So leg 3 is the only one left, and the word
//   "reviewing" inside it is a REQUIREMENT rather than a description: the dialog must show what is about to
//   die. That is more than one sentence and a yes/no, which is precisely the upgrade threshold the
//   environments page named, so this component is justified for revoke AND FOR NOTHING ELSE YET.
//
// E29 COMPONENT LAYER — THE HAND-ROLLED DIALOG IS GONE AND THE JUDGEMENT ABOVE IS NOT. What this file used
// to carry, and no longer does: a 20-line Tab trap that re-read the focusable set on every keypress, an
// Escape handler, a focus-restore effect, and a FOCUSABLE selector string. components/ui/Dialog.tsx over
// @base-ui/react supplies all four, and pays the upgrade note this header used to carry ("if a dialog ever
// has to open over a scrolling region or from inside an overflow-hidden ancestor, portal it then").
//
// TWO OF THE THREE THINGS THE OLD HEADER LISTED AS GIVEN UP ARE NOW PAID, AND THE THIRD IS STILL OWED. The
// list was "no portal, no scroll lock, no `inert` on the rest of the document". The portal and the scroll
// lock are measured in components/ui/Dialog.tsx's header; `inert` is NOT set by the library, and this file
// does not claim it. What replaces it is aria-hidden on every other child of <body> — screen-reader
// containment rather than pointer containment — and the pointer is still stopped by the backdrop, exactly
// as it was.
//
// THE FOUR ARIA REQUIREMENTS ARE THE W3C ARIA APG's AND ARE UNCHANGED: role="alertdialog" on the container,
// aria-modal="true", aria-labelledby pointing at the title, aria-describedby pointing at the message, plus
// the modal-dialog KEYBOARD contract. https://www.w3.org/WAI/ARIA/apg/patterns/alertdialog/ The first two are
// written by the primitive; the second two are Dialog.Title and Dialog.Description registering their own
// generated ids, which is stricter than the hand-built `${testId}-title` strings this file used to compose —
// tests/policy.spec.ts resolves both pointers to real elements, and a composed string can go stale.
//
// FOCUS STARTS ON CANCEL, AND NO ACCESSIBILITY REQUIREMENT IS CLAIMED FOR THAT (plan §3.5 W5, UNCONFIRMED).
// The APG describes a CHOICE of "the element that will receive focus when the dialog opens" and publishes no
// normative sentence preferring the least destructive action; that was searched for and not found. This
// matches what `window.confirm` already does in this console, and "do not change the behaviour" is the
// cheapest way to make no claim. Filed as FLC-P5. It is passed EXPLICITLY rather than left to the library's
// first-tabbable default: the default would silently follow document order, so adding a control above Cancel
// would move it with nothing to say so.

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
  const cancel = useRef<HTMLButtonElement | null>(null);

  return (
    // `open` is a constant because MOUNTING is how every caller controls this component
    // (`{revoking === null ? null : <ConfirmDestructive …>}`), and that contract is unchanged. Any close —
    // Escape, the close of the last focusable, a programmatic one — arrives here as onOpenChange(false) and
    // means exactly what the old Escape handler meant: cancel.
    <Dialog
      open
      onOpenChange={(next) => {
        if (!next) onCancel();
      }}
      alert
      testId={testId}
      title={title}
      description={message}
      initialFocus={cancel}
      actions={
        <>
          {/* CANCEL IS FIRST IN DOCUMENT ORDER as well as focused first, so the tab cycle starts at the safe
              end for a user who never sees the focus ring. */}
          <Button ref={cancel} testId={`${testId}-cancel`} onClick={onCancel}>
            Cancel
          </Button>{" "}
          <Button variant="destructive" testId={`${testId}-confirm`} disabled={busy} onClick={onConfirm}>
            {busy ? `${confirmLabel}…` : confirmLabel}
          </Button>
        </>
      }
    >
      {review === undefined ? null : (
        <div className="dialog-review" data-testid={`${testId}-review`}>
          {review}
        </div>
      )}
    </Dialog>
  );
}
