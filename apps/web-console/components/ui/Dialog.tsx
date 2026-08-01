"use client";

import { Dialog as BaseDialog } from "@base-ui/react/dialog";
import { useRef, type ReactNode, type RefObject } from "react";

// THE MODAL, ONCE (E29 component layer).
//
// MEASURED BEFORE THIS FILE EXISTED, on d8ca934b (2026-07-31):
//   grep -rn 'role="dialog"' --include='*.tsx' components app | wc -l → 0
//
// Zero — and yet the console has a modal: components/ConfirmDestructive.tsx, which carries `role="alertdialog"`
// and 90 lines re-earning what a dialog is. Its own header lists what it had to write by hand and what it
// gave up: "rendered INLINE, with a backdrop that blocks pointer events. No portal, no scroll lock, no
// `inert` on the rest of the document", and then names the upgrade path — "if a dialog ever has to open over
// a scrolling region or from inside an overflow-hidden ancestor, portal it then".
//
// This is that portal. WHAT THE LIBRARY ACTUALLY DOES WAS MEASURED ON THE RUNNING DIALOG (2026-07-31,
// /policy, key-revoke-dialog) rather than read off its documentation, because "a mechanism is named" is not
// "a mechanism operates":
//
//   portal          the popup is a direct child of <body>, outside every overflow ancestor.  MEASURED
//   scroll lock     document.body inline overflow: hidden while open, back to visible on close.  MEASURED
//   focus trap      Tab cycles inside; the overrun lands on Base UI's own aria-hidden focus guard
//                   (`[data-base-ui-focus-guard][data-type="inside"]`), which bounces it back.  MEASURED
//   focus restore   Escape returns focus to the control that opened the dialog.  MEASURED
//   containment     every OTHER child of <body> gets aria-hidden="true" while open, and none after.  MEASURED
//
// AND ONE THING IT DOES NOT DO, WHICH AN EARLIER DRAFT OF THIS COMMENT CLAIMED: it does not set `inert` on
// the rest of the document. Measured: `main.inert === false`, `nav.inert === false`, and no element outside
// the portal carries the attribute — the containment is aria-hidden, which is a screen-reader mechanism and
// not a pointer one. The pointer is stopped by the backdrop, exactly as it was before. Naming `inert` here
// would have been the shape of error this repository keeps finding: a capability written down because the
// library has it, on a path where it is not switched on.
//
// What ConfirmDestructive keeps is everything the library cannot know: which of SC 3.3.4's three legs
// applies, and the review region that satisfies the word "reviewing" inside leg 3.
//
// THE TAB WRAP IS OURS AND IT IS SYNCHRONOUS, WHICH THE LIBRARY'S IS NOT — MEASURED, and it is the one
// place this primitive adds behaviour rather than borrowing it.
//
// Base UI contains Tab with a pair of focus-guard <span>s around the popup: tabbing past the last control
// focuses a guard, whose onFocus calls `enqueueFocus(...)`, which is
// `requestAnimationFrame(() => el.focus())` (floating-ui-react/utils/enqueueFocus.mjs — `sync` defaults to
// false). So the redirect lands ONE FRAME after the key. Measured on the running dialog with 20 Tab presses
// at the speed the test suite presses them: focus reached the guard, then `document.body`, then — twice in
// forty presses — `.skip-link` and `.brand`, which are real controls on the page behind an open
// alertdialog. Shift+Tab reached `key-mint-button` three times.
//
// A human tabbing cannot outrun a frame and would never see this; a key held on auto-repeat (~30ms) can,
// and tests/policy.spec.ts already presses at that speed on purpose — its own words: "twenty presses is
// nine full cycles — enough that a trap which only holds for one lap fails here". That test passed against
// the hand-rolled dialog this primitive replaced, because that trap ran in the keydown handler.
//
// So the wrap below is not a workaround for a test. It is the property the test was written to protect,
// restored — and it now lives in ONE file instead of in every caller that opens a modal. Focus never
// reaches a guard, so the library's rAF redirect never fires and the two cannot fight.
const FOCUSABLE = 'a[href], button:not([disabled]), input:not([disabled]), select:not([disabled]), textarea:not([disabled]), [tabindex]:not([tabindex="-1"])';

// `alert` PICKS THE ROLE AND THE DISMISSAL TOGETHER, because they are one decision rather than two. An
// alertdialog interrupts to say something is about to be destroyed; letting a stray click outside it count as
// an answer is the failure mode that role exists to prevent. https://www.w3.org/WAI/ARIA/apg/patterns/alertdialog/
//
// FOCUS STARTS WHERE THE CALLER SAYS AND NO ACCESSIBILITY REQUIREMENT IS CLAIMED FOR IT — that sentence is
// ConfirmDestructive's, carried across verbatim in substance: the APG describes a CHOICE of initial focus and
// publishes no normative preference for the least destructive action. `initialFocus` is a ref rather than a
// default so the claim stays the caller's.

export function Dialog({
  open,
  onOpenChange,
  title,
  description,
  children,
  actions,
  testId,
  alert = false,
  initialFocus,
}: {
  open: boolean;
  onOpenChange: (open: boolean) => void;
  title: string;
  /** What aria-describedby points at, so it is READ. Keep it to what the reader must decide on. */
  description: ReactNode;
  /** The body between the description and the actions — a review region, a form. */
  children?: ReactNode;
  actions: ReactNode;
  testId: string;
  /** role="alertdialog" plus no pointer dismissal. See the header. */
  alert?: boolean;
  initialFocus?: RefObject<HTMLElement | null>;
}) {
  const popup = useRef<HTMLDivElement | null>(null);

  return (
    <BaseDialog.Root open={open} onOpenChange={onOpenChange} modal disablePointerDismissal={alert}>
      <BaseDialog.Portal>
        <BaseDialog.Backdrop className="dialog-backdrop" data-testid={`${testId}-backdrop`} />
        {/* aria-modal IS WRITTEN HERE AND THE LIBRARY DOES NOT WRITE IT. Base UI constrains a screen reader by
            marking every OTHER child of <body> aria-hidden="true" (floating-ui-react's markOthers, called
            with `ariaHidden` and nothing else — measured, see the header), which is a mechanism rather than
            an attribute and is why it omits this one. The APG's dialog pattern still names aria-modal="true"
            as part of the role's contract, ConfirmDestructive lists it as one of four non-negotiables, and
            tests/policy.spec.ts asserts BOTH — the attribute here, and the marking on <main>'s own ancestor,
            which is the half that actually does the work. */}
        <BaseDialog.Popup
          ref={popup}
          className="dialog"
          // `"dialog"` RATHER THAN `undefined`, AND THE NON-ALERT PATH HAD NEVER BEEN RENDERED UNTIL NOW.
          //
          // This read `alert ? "alertdialog" : undefined`, on the assumption that Base UI supplies
          // `role="dialog"` itself when we do not. MEASURED, 2026-08-01, on the first two non-alert callers
          // (components/McpCatalogue.tsx and components/AgentTemplates.tsx) — it does not:
          //
          //   <div data-open="" id="_R_2j4av5ulb_" tabindex="-1" data-base-ui-focusable=""
          //        aria-modal="true" data-testid="catalogue-dialog" class="dialog"
          //        aria-labelledby="base-ui-_r_7_" aria-describedby="base-ui-_r_8_">
          //
          // No role at all — so `aria-modal="true"` sat on a plain <div>, which axe-core reports as
          // `aria-allowed-attr`, impact CRITICAL: "ARIA attribute is not allowed: aria-modal="true"".
          //
          // IT WAS INVISIBLE BECAUSE THE BRANCH WAS DEAD. components/ConfirmDestructive.tsx was the only
          // consumer of this primitive and it passes `alert` unconditionally, so every render this file had
          // ever produced took the "alertdialog" arm. The default arm — the one a caller gets by NOT asking
          // for an alert, which is what the tree tells new code to do — shipped broken and no scan could
          // have caught it, because no scan had a non-alert dialog to open. Both new callers failed on
          // their first axe scan.
          role={alert ? "alertdialog" : "dialog"}
          aria-modal="true"
          data-testid={testId}
          initialFocus={initialFocus}
          onKeyDown={(event) => {
            if (event.key !== "Tab") return;
            // THE FOCUSABLE SET IS READ AT KEYPRESS, not cached, so a control that appears while the dialog
            // is open (a busy state swapping a button) is in the cycle. This sentence and this filter are
            // carried over verbatim from the hand-rolled trap in components/ConfirmDestructive.tsx, which
            // is the implementation this replaces and the one the shipped assertions were written against.
            const nodes = [...(popup.current?.querySelectorAll<HTMLElement>(FOCUSABLE) ?? [])].filter(
              (el) => el.offsetParent !== null || el === document.activeElement,
            );
            if (nodes.length === 0) return;
            const [first, last] = [nodes[0], nodes[nodes.length - 1]];
            const active = document.activeElement;
            const inside = popup.current?.contains(active) === true;
            if (event.shiftKey && (active === first || !inside)) {
              event.preventDefault();
              last.focus();
            } else if (!event.shiftKey && (active === last || !inside)) {
              event.preventDefault();
              first.focus();
            }
          }}
        >
          <BaseDialog.Title>{title}</BaseDialog.Title>
          <BaseDialog.Description data-testid={`${testId}-message`}>{description}</BaseDialog.Description>
          {children}
          <p className="dialog-actions">{actions}</p>
        </BaseDialog.Popup>
      </BaseDialog.Portal>
    </BaseDialog.Root>
  );
}
