"use client";

import { Field as BaseField } from "@base-ui/react/field";
import type { ComponentPropsWithoutRef, ReactNode } from "react";

// THE FOUR PARTS OF A FORM FIELD, ASSEMBLED ONCE (E29 component layer).
//
// components/ResourceForm.tsx assembles them by hand — a <label htmlFor>, a control, a <p className="muted">
// hint, and an `aria-describedby` computed from the control's id — and components/Picker.tsx assembles the
// same four again for the select arm. Two copies of a wiring rule is how the third one loses the
// aria-describedby: it is a string that has to MATCH an id somewhere else in the markup, and nothing in the
// tree can tell you when it stops matching.
//
// BASE UI'S FIELD IS WHAT REMOVES THE STRING. Field.Root carries a context; Field.Label reads the control's
// id out of it and writes its own `for`; Field.Description registers itself and the control's
// `aria-describedby` is composed from the registration. So the association is a REFERENCE rather than a
// convention, and a renamed field cannot silently orphan its label.
//
// THE ID CONTRACT IS THIS REPOSITORY'S AND IT SURVIVES. `field-<name>` is not decoration: tests/
// secret-never-returns.spec.ts asserts `label[for="field-environment"]` has count 0 when the environment
// collection is empty — the claim being that an unsatisfiable control is ABSENT rather than degraded into a
// free-text box. Passing `id` explicitly to the control keeps that assertion about the real markup; letting
// Base UI generate the id would have made it pass because the selector matches nothing anywhere.
//
// THERE IS NO PER-FIELD ERROR PART, AND THE ABSENCE IS DELIBERATE. Every form in this console shows ONE
// refusal, in the server's own words, in one `role="alert"` region — ResourceForm's header says why, and no
// caller has field-level validation to report. Field.Error exists in the library and is not wrapped here:
// a fourth part with no caller is the defect this layer was built to remove, not an omission.

export function Field({
  label,
  hint,
  action,
  children,
}: {
  label: ReactNode;
  /** The "or instructions" half of WCAG §3.3.2, wired as the control's description rather than as prose. */
  hint?: ReactNode;
  /**
   * A link on the LABEL'S OWN LINE, to the screen that manages what this field chooses from.
   *
   * MEASURED ON THE REFERENCE'S Create-session DIALOG, 2026-08-01: every picker in it carries a link at the
   * right end of its label row — `Manage agents ↗`, `Manage environments ↗`, `Manage credential vaults ↗`,
   * at 12px/17px in the link colour. That is how it answers "the list is empty, now what" — and, just as
   * importantly, "the list has the wrong thing in it, now what", which is a question our `emptyNote` cannot
   * be asked because it is only rendered when the list is empty.
   *
   * It is a FIFTH part in a file whose own header says a part with no caller is the defect this layer exists
   * to remove. It has callers: see components/Picker.tsx's `manage`.
   */
  action?: ReactNode;
  /** The control. It must be a Base UI field part (FieldControl, or a Select) for the wiring to reach it. */
  children: ReactNode;
}) {
  return (
    <BaseField.Root>
      {action === undefined ? (
        <BaseField.Label>{label}</BaseField.Label>
      ) : (
        <div className="field-label-row">
          <BaseField.Label>{label}</BaseField.Label>
          {action}
        </div>
      )}
      {children}
      {hint === undefined ? null : <BaseField.Description className="muted">{hint}</BaseField.Description>}
    </BaseField.Root>
  );
}

/**
 * The text control. `render` is how Base UI swaps the element, so a textarea is the same component with a
 * different tag rather than a second branch with a second copy of the wiring.
 *
 * `type` IS PASSED THROUGH RATHER THAN DEFAULTED, and app/globals.css is why: its form rule selects
 * `input[type="text"]`, and an `<input>` with no type ATTRIBUTE is a text input by the HTML spec's missing-
 * value default but matches no attribute selector. A field that quietly lost its border is the shape
 * tests/contrast.spec.ts exists to catch, and this is the line that keeps it from having to.
 */
export function FieldControl({
  multiline = false,
  rows,
  type,
  ...rest
}: ComponentPropsWithoutRef<"input"> & { multiline?: boolean; rows?: number }) {
  // A <textarea> has no `type` and an <input> has no `rows`; React warns about either. The branch is here,
  // once, rather than at each of the twelve ResourceForm call sites.
  return multiline ? (
    <BaseField.Control {...rest} render={<textarea rows={rows} />} />
  ) : (
    <BaseField.Control {...rest} type={type} />
  );
}
