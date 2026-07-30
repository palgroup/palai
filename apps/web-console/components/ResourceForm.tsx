"use client";

import type { ReactNode } from "react";

// THE FORM DISCIPLINE, ESTABLISHED ONCE (E25 T2, plan §3.5 N11). E25 adds six configuration forms across
// T4/T5/T6 and every one of them owes the same four things. They are written here so those tasks inherit them
// instead of re-deriving them five times — and so a reviewer checks one file rather than six:
//
//   1. A PROGRAMMATIC LABEL (htmlFor/id). WCAG 2.2 §3.3.2 Labels or Instructions (A) — "Labels or
//      instructions are provided when content requires user input" — and §1.3.1 / §4.1.2. Placement is not a
//      label; a placeholder is not a label. `id` is derived from the field name so it cannot be forgotten.
//   2. A role="alert" ERROR REGION, so a refusal is announced without moving focus. WCAG §3.3.1 Error
//      Identification (A): the error is "described to the user IN TEXT".
//   3. STATUS IN TEXT, NEVER COLOUR ALONE — a glyph plus a word, the same rule Status.tsx and Panel.tsx
//      already follow (a red border says nothing to a screen reader and nothing to a colourblind operator).
//   4. EVERY CONTROL KEYBOARD-REACHABLE: real <input>/<textarea>/<button> elements in document order, no
//      tabindex, no div-with-onClick. tests/a11y.spec.ts presses Tab until it reaches a control, so anything
//      dropped out of the tab order fails rather than merely looking fine.
//
// Paste is NEVER blocked and autocomplete is never "off" for a credential field: WCAG 2.2 §3.3.8 Accessible
// Authentication counts password-manager support and copy/paste as the mechanisms that SATISFY it, and MDN
// records that browsers ignore autocomplete="off" for password fields anyway. T4's secret field passes
// autoComplete="new-password" through this component for exactly that reason.
//
// ponytail: text / password / textarea / select fields, one submit, one optional extra action. No validation
// DSL and no field-level error map. The `select` arm arrived with its first caller in E25 T4 (the environment
// picker) exactly as this comment said it should, and T6 reuses it on the agent-revision form rather than
// hand-rolling a second dropdown. A form component that grows options nobody calls is how the discipline it
// exists to enforce stops being read.
//
// A `select` WITH NO OPTIONS IS NOT RENDERED AT ALL, and the caller says what stands in its place (T4's
// `emptyNote`). That is a rule rather than a nicety: an empty dropdown is a control that cannot be
// satisfied, and the tempting alternative — degrade to a free-text box — invites an operator to type an id
// that does not exist, which then fails at admission with a refusal about something else entirely.

export interface FormOption {
  value: string;
  label: string;
}

export interface FormField {
  /** Field name; also the source of the control's id, so the label can never be orphaned. */
  name: string;
  label: string;
  value: string;
  onChange: (value: string) => void;
  kind?: "text" | "password" | "textarea" | "select";
  /** Never "off" for a credential — see the header. */
  autoComplete?: string;
  required?: boolean;
  /** Rendered under the control as a programmatic description, for the "or instructions" half of §3.3.2. */
  hint?: string;
  testId?: string;
  /** kind "select" only. An EMPTY list renders `emptyNote` instead of the control — see the header. */
  options?: FormOption[];
  /** kind "select" only: what to render when there is nothing to choose from. */
  emptyNote?: ReactNode;
  /** kind "select" only: the placeholder row's label, when one is wanted. */
  placeholder?: string;
}

export function ResourceForm({
  title,
  fields,
  submitLabel,
  submittingLabel,
  submitTestId,
  onSubmit,
  submitting = false,
  error = "",
  status = "",
  testId,
  note,
  actions,
  children,
}: {
  title: string;
  fields: FormField[];
  submitLabel: string;
  submittingLabel?: string;
  submitTestId?: string;
  onSubmit: () => void | Promise<void>;
  submitting?: boolean;
  /** A refusal, in the server's own words where there is one. Rendered in the role="alert" region. */
  error?: string;
  /** What just happened, in TEXT. */
  status?: string;
  testId?: string;
  note?: ReactNode;
  /** An extra control (a cancel, an abort) rendered beside the submit, still in the tab order. */
  actions?: ReactNode;
  /**
   * A field this component deliberately does NOT own, rendered after `fields` and still inside the form, so
   * it stays in document order and therefore in tab order. Its one caller is E25 T4's SecretField: that field
   * is UNCONTROLLED on purpose (the secret lives in the DOM node and nowhere else), which is the opposite of
   * FormField's controlled value/onChange contract — so it is passed in rather than modelled here. A
   * `kind: "secret"` arm would have meant putting a credential through this component's state contract.
   */
  children?: ReactNode;
}) {
  return (
    <section className="panel" data-testid={testId} aria-labelledby={`${title.replace(/\W+/g, "-").toLowerCase()}-h`}>
      <h2 id={`${title.replace(/\W+/g, "-").toLowerCase()}-h`}>{title}</h2>
      {note ? <p className="muted">{note}</p> : null}
      <form
        data-testid={testId ? `${testId}-form` : undefined}
        onSubmit={(event) => {
          // A plain fetch, not a Server Action: a Server Action is a second write path the public-API-only
          // network proof cannot see (§3.5 N8), and preventing the default keeps the role="alert" refusal on
          // screen — a full page load has nothing left to announce.
          event.preventDefault();
          void onSubmit();
        }}
      >
        {fields.map((field) => {
          const id = `field-${field.name}`;
          const describedBy = field.hint ? `${id}-hint` : undefined;
          // An options-less select renders its caller's note INSTEAD of a control, and no label either: a
          // label pointing at nothing is an axe violation and a lie to a screen reader.
          if (field.kind === "select" && (field.options ?? []).length === 0) {
            return (
              <p key={field.name} className="muted" data-testid={field.testId === undefined ? undefined : `${field.testId}-empty`}>
                {field.emptyNote}
              </p>
            );
          }
          return (
            <div key={field.name}>
              <label htmlFor={id}>{field.label}</label>
              {field.kind === "select" ? (
                <select
                  id={id}
                  name={field.name}
                  required={field.required}
                  value={field.value}
                  aria-describedby={describedBy}
                  onChange={(e) => field.onChange(e.target.value)}
                  data-testid={field.testId}
                >
                  {field.placeholder === undefined ? null : <option value="">{field.placeholder}</option>}
                  {(field.options ?? []).map((option) => (
                    <option key={option.value} value={option.value}>
                      {option.label}
                    </option>
                  ))}
                </select>
              ) : field.kind === "textarea" ? (
                <textarea
                  id={id}
                  name={field.name}
                  rows={2}
                  required={field.required}
                  value={field.value}
                  aria-describedby={describedBy}
                  onChange={(e) => field.onChange(e.target.value)}
                  data-testid={field.testId}
                />
              ) : (
                <input
                  id={id}
                  name={field.name}
                  type={field.kind === "password" ? "password" : "text"}
                  autoComplete={field.autoComplete}
                  required={field.required}
                  value={field.value}
                  aria-describedby={describedBy}
                  onChange={(e) => field.onChange(e.target.value)}
                  data-testid={field.testId}
                />
              )}
              {field.hint ? (
                <p className="muted" id={`${id}-hint`}>
                  {field.hint}
                </p>
              ) : null}
            </div>
          );
        })}
        {children}
        {error === "" ? null : (
          <p role="alert" className="form-error" data-testid={testId ? `${testId}-error` : undefined}>
            <span className="glyph" aria-hidden="true">
              ✖
            </span>{" "}
            {error}
          </p>
        )}
        {status === "" ? null : (
          <p data-testid={testId ? `${testId}-status` : undefined}>
            <span className="glyph" aria-hidden="true">
              ✔
            </span>{" "}
            {status}
          </p>
        )}
        <p>
          <button type="submit" disabled={submitting} data-testid={submitTestId}>
            {submitting ? (submittingLabel ?? `${submitLabel}…`) : submitLabel}
          </button>
          {actions ? <> {actions}</> : null}
        </p>
      </form>
    </section>
  );
}
