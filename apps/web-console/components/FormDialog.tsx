"use client";

import { useEffect, useRef, type ReactNode } from "react";

// A CREATE FORM BEHIND ITS OWN BUTTON — the shell, not the form (page-parity pass).
//
// WHY IT EXISTS AT ALL. Measured on 2026-07-31 against the reference console: their list screens carry a
// title, one sentence, a `+ Create X` button and a table. Ours stacked the create form, and on /agents three
// more forms after it, down the same page — so the screen had no primary subject and the table it was named
// after was the fourth thing on it. A create form is a MODE, and the control that enters that mode is the
// button; a form that is always open is a form the operator scrolls past on every visit.
//
// IT IS NOT ConfirmDestructive AND IT DOES NOT REPLACE IT. That component is role="alertdialog" with a
// review block and a Cancel/Confirm pair, and its own header says when it is justified (WCAG 2.2 SC 3.3.4
// leg 3, for an action that cannot be reversed). This one is role="dialog": nothing here is destructive, and
// an alertdialog announces its message as an alert, which is wrong for "here is a form".
//
// THE ARIA CONTRACT IS THE W3C ARIA APG's DIALOG PATTERN, and it is the same list ConfirmDestructive
// re-earns: role="dialog", aria-modal="true", an accessible NAME, focus moved in on open and back on close,
// Escape closes, and Tab cannot leave. https://www.w3.org/WAI/ARIA/apg/patterns/dialog-modal/
//
// THE NAME IS aria-label RATHER THAN aria-labelledby, and that is a deliberate weakening of a house habit.
// Everything this dialog wraps is a components/ResourceForm.tsx, which derives its own heading id from its
// TITLE STRING (`${title.replace(/\W+/g, "-").toLowerCase()}-h`) — so an aria-labelledby here would be a
// pointer into another component's derived id, silently broken by a wording change, and a dangling
// aria-labelledby produces an EMPTY accessible name rather than an error. The label is stated where it
// cannot rot. The heading inside is still the visible one and still says the same words.
//
// TODO(component-layer): this is the Dialog primitive. When components/ui/Dialog lands, this file should
// become a thin arrangement over it — the focus trap, the backdrop and the Escape handler below are exactly
// what a primitive owns, and there should not be two of them in this tree for long. Its second and third
// callers are already named: /environments' create form, and whatever /repositories does with its own.
//
// ponytail: rendered INLINE with a pointer-blocking backdrop, like ConfirmDestructive — no portal, no scroll
// lock, no `inert` on the rest of the document. `aria-modal="true"` is what the APG specifies for constraining
// a screen reader and the backdrop is what keeps a click from moving focus out from under the trap. Upgrade
// path: portal it when a dialog first has to open from inside an overflow-hidden ancestor.

const FOCUSABLE =
  'a[href], button:not([disabled]), input:not([disabled]), select:not([disabled]), textarea:not([disabled]), [tabindex]:not([tabindex="-1"])';

export function FormDialog({
  /** The dialog's accessible name. Say the same words the heading inside says. */
  label,
  onClose,
  testId,
  children,
}: {
  label: string;
  onClose: () => void;
  testId: string;
  children: ReactNode;
}) {
  const dialog = useRef<HTMLDivElement | null>(null);

  useEffect(() => {
    // Focus moves IN on open and BACK on close. Returning it is not decoration: a keyboard operator who
    // cancels and lands at the top of the document has lost the list they were working in.
    //
    // THE FIRST FIELD, not the first control, and that is the difference from ConfirmDestructive: there the
    // safe end of a destructive choice is where focus belongs, here the operator opened a form in order to
    // type into it. The APG describes a CHOICE of initial focus and names "the first focusable element" as
    // the common one for a form dialog.
    const previous = document.activeElement instanceof HTMLElement ? document.activeElement : null;
    dialog.current?.querySelector<HTMLElement>(FOCUSABLE)?.focus();
    return () => previous?.focus();
  }, []);

  return (
    <>
      <div className="dialog-backdrop" data-testid={`${testId}-backdrop`} onClick={onClose} />
      <div
        ref={dialog}
        role="dialog"
        aria-modal="true"
        aria-label={label}
        className="dialog"
        data-testid={testId}
        onKeyDown={(event) => {
          if (event.key === "Escape") {
            event.stopPropagation();
            onClose();
            return;
          }
          if (event.key !== "Tab") return;
          // THE TRAP. It wraps rather than merely blocking — a Tab that does nothing reads as a broken page —
          // and the focusable set is READ AT KEYPRESS rather than cached, so a control that appears while the
          // dialog is open (a submit swapping to its busy label, a role="alert" refusal) is in the cycle.
          const nodes = [...(dialog.current?.querySelectorAll<HTMLElement>(FOCUSABLE) ?? [])].filter(
            (el) => el.offsetParent !== null || el === document.activeElement,
          );
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
        {children}
      </div>
    </>
  );
}
