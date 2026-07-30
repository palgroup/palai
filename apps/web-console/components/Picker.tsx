"use client";

import type { ReactNode } from "react";

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
}) {
  if (options.length === 0) {
    return (
      <p className="muted" data-testid={testId === undefined ? undefined : `${testId}-empty`}>
        {emptyNote}
      </p>
    );
  }
  const describedBy = hint === undefined ? undefined : `${id}-hint`;
  return (
    <div>
      <label htmlFor={id}>{label}</label>
      <select
        id={id}
        name={name}
        required={required}
        value={value}
        aria-describedby={describedBy}
        onChange={(e) => onChange(e.target.value)}
        data-testid={testId}
      >
        {placeholder === undefined ? null : <option value="">{placeholder}</option>}
        {options.map((option) => (
          <option key={option.value} value={option.value}>
            {option.label}
          </option>
        ))}
      </select>
      {hint === undefined ? null : (
        <p className="muted" id={`${id}-hint`}>
          {hint}
        </p>
      )}
    </div>
  );
}
