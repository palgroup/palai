"use client";

import type { ComponentPropsWithRef } from "react";

// THE BUTTON, ONCE (E29 component layer).
//
// MEASURED BEFORE THIS FILE EXISTED: `grep -rn '<button' --include='*.tsx' components app | wc -l` → 39
// (2026-07-31, d8ca934b). THIRTY-EIGHT of those are elements and one is a sentence in a comment
// (components/ResourceForm.tsx:18, "real <input>/<textarea>/<button> elements in document order") — the
// distinction is written down because the count is the argument, and a script that rewrote all 39 would have
// edited that comment too. One did, and this is where it was caught.
//
// Thirty-eight hand-rolled buttons, each repeating `type="button"` and each reaching for a paint by writing a
// bare class name — so "what are this console's button variants" had no answer anywhere in the tree, and
// changing one had thirty-eight places to land.
//
// WHAT THIS DOES NOT DO IS REDRAW THEM. app/globals.css §8 already styles the bare `button` element and
// already carries `.primary` and `.danger`; this component is the VOCABULARY over those rules, not a second
// set of them. So the paint is unchanged by construction — a migrated call site renders the same classes it
// rendered before — and the win is that the vocabulary is now typed, `type` defaults to "button", and the
// next paint change lands in one file.
//
// THERE IS NO `ghost` VARIANT, AND THE REASON IS A MEASUREMENT RATHER THAN A PREFERENCE.
//
//   A ghost button — no border, no fill, paint only on hover — is what the reference console uses for its
//   quiet row actions. tests/contrast.spec.ts measures EVERY <button> on every route and scores it
//   `Math.max(border ?? 0, fill ?? 0)` against the surface behind it, requiring 3:1 for SC 1.4.11. A
//   borderless, unfilled button scores 0 and reddens the sweep. Nor does a quiet FILL rescue it: the two
//   candidates in this token system are --bg-inset (slate-3) and --bg-hover (slate-4), which measure 1.11:1
//   and 1.19:1 against --bg-page in light — contrast.spec.ts's own formula, re-run 2026-07-31 against
//   --border-control's 3.69:1. app/globals.css says the same thing from the other end — "the first usable
//   neutral for an interactive control border is step 10".
//
//   So this console's quiet buttons are quiet in GEOMETRY and never in boundary: .copy-button,
//   .row-menu-toggle and .detail-close all keep --border-control. `className` passes those three classes
//   through unchanged.
//
// THERE IS NO `size` PROP EITHER, AND THAT IS A MEASUREMENT RATHER THAN AN OVERSIGHT. One was written here,
// on the assumption that those three quiet controls were one shape under three names. Read off
// app/globals.css they are three shapes: .copy-button is 24px tall with `padding: 0 var(--space-2)`,
// .row-menu-toggle is 1.75rem/28px with the same padding, and .detail-close is 24px with
// `padding: 0 var(--space-3)` plus a `margin-left: auto`. A `size="sm"` covering them would have been a
// FOURTH geometry that no caller wanted — precisely the defect this layer exists to remove. Consolidating
// the three is a decision about those screens; this file's job was to find out whether they are already one,
// and they are not.
//
// AND `type="submit"` STAYS COUPLED TO THE PRIMARY PAINT, which is a pre-existing rule this file inherits
// rather than introduces: globals.css §8 paints `button[type="submit"]` with the accent because "every form
// on this surface has exactly one <button type="submit"> and it is the thing the form is for". A
// `<Button variant="secondary" type="submit">` would therefore still render primary. Nothing calls that
// combination today; if one ever does, the rule is what has to move, not this component.

export type ButtonVariant = "primary" | "secondary" | "destructive";

const VARIANT_CLASS: Record<ButtonVariant, string> = {
  primary: "primary",
  secondary: "",
  destructive: "danger",
};

export function Button({
  variant = "secondary",
  type = "button",
  className,
  testId,
  ...rest
  // `ref` rides in `rest`: React 19 passes it as an ordinary prop to a function component, so there is no
  // forwardRef here and no wrapper to unwrap. ConfirmDestructive uses it to say which control opens focused.
}: Omit<ComponentPropsWithRef<"button">, "className"> & {
  variant?: ButtonVariant;
  className?: string;
  /** data-testid, spelled the way every other component in this tree spells it. */
  testId?: string;
}) {
  const classes = ["ui-button", VARIANT_CLASS[variant], className ?? ""]
    .filter((c) => c !== "")
    .join(" ");
  return <button {...rest} type={type} className={classes} data-testid={testId} />;
}
