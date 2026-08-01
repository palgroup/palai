"use client";

import type { ReactNode } from "react";

import { Field } from "@/components/ui/Field";
import { Select } from "@/components/ui/Select";

// ONE SELECT, ONE RULE (E25 T6).
//
// The rule is T4's and it is the reason this file exists rather than a fourth copy of it: A SELECT WITH NO
// OPTIONS IS NOT RENDERED AT ALL, and the caller says what stands in its place. An empty dropdown is a
// control that cannot be satisfied, and the tempting alternative — degrade to a free-text box — invites an
// operator to type an id that does not exist, which then fails several steps later with a refusal about
// something else entirely. A label pointing at nothing is also an axe violation and a lie to a screen reader.
//
// It was written inside ResourceForm's select arm, where it belongs for a FORM field. E25 T6 added three
// standalone pickers that are not form fields — an agent chooser on /agents, and the agent + revision pins on
// /runs — and re-deriving a five-line rule three times is how the fourth copy gets it wrong. ResourceForm's
// select arm now delegates here, so there is exactly one implementation and the `${testId}-empty` contract
// every spec reads is the same one in both places.
//
// E29 COMPONENT LAYER — THE RULE IS UNCHANGED AND THE CONTROL IS NOT. What was a native <select> is now
// components/ui/Select.tsx over @base-ui/react, and the label/hint wiring is components/ui/Field.tsx. The
// options-less rule above, the `${testId}-empty` contract and the `field-<name>` id all survive verbatim;
// what changes is that the dropdown is drawn by this stylesheet rather than by the operating system.
export interface PickerOption {
  value: string;
  label: string;
}

export function Picker({
  id,
  name,
  label,
  value,
  onChange,
  options,
  placeholder,
  emptyNote,
  testId,
  hint,
  required,
  manage,
}: {
  /** The control's DOM id; the label points at it and the hint is derived from it. */
  id: string;
  name?: string;
  label: string;
  value: string;
  onChange: (value: string) => void;
  options: PickerOption[];
  /** The placeholder row's label, when an empty choice is meaningful. Omitted = no empty row. */
  placeholder?: string;
  /** What is rendered INSTEAD of the control when there is nothing to choose from. */
  emptyNote: ReactNode;
  testId?: string;
  hint?: ReactNode;
  required?: boolean;
  /**
   * THE SCREEN THAT MANAGES WHAT THIS PICKER CHOOSES FROM, linked beside the label.
   *
   * MEASURED on the reference's Create-session dialog: every picker in it carries `Manage <resource> ↗` at
   * the right end of its label row. It is strictly more than this component's `emptyNote` and the difference
   * is not a matter of degree — `emptyNote` is rendered ONLY when the collection is empty, so an operator
   * whose list has four environments and needs a fifth is offered nothing at all. That is the more common
   * case, and it was the one with no answer on screen.
   *
   * `emptyNote` STAYS AND IS NOT REPLACED. It says why an empty list is empty, in this deployment's own
   * terms, and a bare link cannot: "nothing registers an MCP connection without an operator" is a fact, not
   * a destination. When both are present the empty arm renders the sentence AND the link.
   */
  manage?: { href: string; label: string };
}) {
  // THE PLACEHOLDER IS A ROW, NOT A PROP, and it was already: the native version prepended
  // `<option value="">`. Keeping it an option means "no choice yet" is a value the listbox can be ON, which
  // is what makes it selectable with the keyboard and readable to a screen reader.
  const rows = placeholder === undefined ? options : [{ value: "", label: placeholder }, ...options];
  // The link is one element in two places — the label row when there is a control, and the end of the note
  // when there is not — so a rename of the destination cannot leave one of the two pointing at the old page.
  const manageLink =
    manage === undefined ? undefined : (
      <a
        className="field-manage"
        href={manage.href}
        data-testid={testId === undefined ? undefined : `${testId}-manage`}
      >
        {manage.label}
        <span aria-hidden="true"> ↗</span>
      </a>
    );
  if (options.length === 0) {
    return (
      <p className="muted" data-testid={testId === undefined ? undefined : `${testId}-empty`}>
        {emptyNote}
        {manageLink === undefined ? null : <> {manageLink}</>}
      </p>
    );
  }
  return (
    <Field label={label} hint={hint} action={manageLink}>
      <Select id={id} name={name} required={required} value={value} onValueChange={onChange} options={rows} testId={testId} />
    </Field>
  );
}
