"use client";

import { Select as BaseSelect } from "@base-ui/react/select";
import type { SVGProps } from "react";

// THE LISTBOX THIS CONSOLE DID NOT HAVE (E29 component layer).
//
// MEASURED BEFORE THIS FILE EXISTED, on d8ca934b (2026-07-31):
//
//   grep -rn '<select' --include='*.tsx' components app | wc -l   → 7
//   grep -rn 'role="listbox"' --include='*.tsx' components app | wc -l → 0
//   grep -rn 'role="option"'  --include='*.tsx' components app | wc -l → 0
//
// Seven native `<select>`s and no listbox anywhere: every dropdown in this console was drawn by the operating
// system, which is why they look like nothing else on the page and why app/globals.css had to paint a chevron
// into `background-image` with `appearance: none` to get within sight of the design. That stopgap is gone with
// this file — there is no `<select>` left for it to apply to.
//
// WHAT BASE UI SUPPLIES AND WHAT WE SUPPLY. @base-ui/react@1.6.0 gives the BEHAVIOUR — role="combobox" on the
// trigger, role="listbox" on the popup, role="option" per item, typeahead, Home/End/arrow navigation, the
// dismissal and focus rules, and Floating UI positioning. It ships no styles at all, so every pixel below
// comes from app/globals.css §8's tokens.
//
// THE PACKAGE NAME MOVED AND THIS IS THE CURRENT ONE. `@base-ui-components/react` stopped at 1.0.0-rc.0;
// the library shipped 1.0.0 under `@base-ui/react` on 2025-12-11 and is at 1.6.0 (2026-06-18). Checked with
// `npm view @base-ui/react version` rather than remembered.
//
// THE TRIGGER IS A NATIVE <button>, AND THAT IS LOAD-BEARING RATHER THAN INCIDENTAL. tests/contrast.spec.ts
// sweeps `document.querySelectorAll("input, select, textarea, button")` and scores each control's boundary
// against SC 1.4.11's 3:1. A trigger rendered as a <div> — which is what several headless libraries do —
// would leave every dropdown in this console OUTSIDE the one measurement that judges it, and the sweep would
// stay green while losing seven controls. Select.Trigger in 1.6.0 renders `<button>` (verified in
// select/trigger/SelectTrigger.d.mts: "Renders a `<button>` element"), so the sweep still sees all seven.
//
// THE ACCESSIBLE NAME COMES FROM ONE OF TWO PLACES AND NEITHER IS COMPOSED HERE.
//
//   A TOOLBAR FILTER has no visible label — the seven native selects carried `aria-label="Status"` and the
//   panel head has no room for a word per control — so the caller passes `label` and it becomes aria-label.
//   A FORM FIELD is wrapped in components/ui/Field.tsx, and Base UI's Field.Label registers its own id on
//   the field context; SelectTrigger reads it (select/trigger/SelectTrigger.mjs merges `aria-labelledby`
//   from resolveAriaLabelledBy). So the label association is a REGISTRATION rather than a string this file
//   builds, which is the same property components/ui/Field.tsx exists for.
//
// A `labelledBy` PROP WAS WRITTEN HERE AND IS GONE. It composed `"<label-id> <trigger-id>"`, the APG's
// select-only-combobox pattern, so the name would read "Environment, staging" rather than "Environment".
// It never had a caller: every labelled Select in this console is inside a Field, where Base UI has already
// done the association, and passing a second aria-labelledby would have overwritten it with a hand-built
// string. The narrower name is the library's considered choice and it is what a native `<select>` produced
// here too. That the name EXISTS is measured rather than assumed: axe's `aria-input-field-name` (wcag2a) is
// in tests/constants.ts's tag set and runs over every route that renders one of these forms.

export interface SelectOption {
  value: string;
  label: string;
}

export function Select({
  value,
  onValueChange,
  options,
  label,
  id,
  name,
  required,
  disabled,
  testId,
  className,
}: {
  value: string;
  onValueChange: (value: string) => void;
  options: SelectOption[];
  /** The accessible name, for a control with NO visible label. A field inside <Field> leaves this unset. */
  label?: string;
  id?: string;
  name?: string;
  required?: boolean;
  disabled?: boolean;
  testId?: string;
  className?: string;
}) {
  return (
    <BaseSelect.Root
      items={options}
      value={value}
      onValueChange={(next) => onValueChange(next === null ? "" : String(next))}
      name={name}
      required={required}
      disabled={disabled}
    >
      {/* `data-value` ON THE TRIGGER AND ON EVERY ITEM, AND IT IS NOT DECORATION. A native <select> put its
          value in the DOM twice — `option[value]` and the element's own `.value` — and the whole suite read
          it: `selectOption(value)` at 19 call sites and `inputValue()` at three more. Base UI puts the value
          nowhere a test can see it: an item is a `<div role="option">` with a label, and the trigger renders
          the label too. Without this, every one of those call sites would have to address a control by the
          COPY on its rows, which couples the tests to display text and cannot express "the empty choice" at
          all. So the value stays addressable, and tests/profile.ts's chooseOption is the one reader. */}
      <BaseSelect.Trigger
        id={id}
        className={className === undefined ? "ui-select-trigger" : `ui-select-trigger ${className}`}
        aria-label={label}
        data-testid={testId}
        data-value={value}
      >
        <BaseSelect.Value className="ui-select-value" />
        <BaseSelect.Icon className="ui-select-icon" render={<Chevron />} />
      </BaseSelect.Trigger>
      <BaseSelect.Portal>
        {/* alignItemWithTrigger={false} — the popup drops BELOW the control instead of overlaying it with the
            selected row under the pointer. The overlay behaviour is the macOS native one, and reproducing it
            is the opposite of this file's purpose: it puts the popup on top of the trigger the sweep just
            measured, and it reads as an OS control, which is the thing being replaced. */}
        <BaseSelect.Positioner className="ui-select-positioner" sideOffset={4} alignItemWithTrigger={false}>
          <BaseSelect.Popup className="ui-select-popup">
            {options.map((option) => (
              <BaseSelect.Item key={option.value} value={option.value} className="ui-select-item" data-value={option.value}>
                {/* THE INDICATOR IS A GLYPH, NOT A COLOUR — the rule components/Status.tsx already follows.
                    `[data-selected]` also carries a background, and that is the redundant third layer rather
                    than the carrier: remove the colour and the check still says which row is chosen. It is
                    aria-hidden because role="option" already exposes the state as aria-selected, and a
                    screen reader announcing "tick, selected" says it twice. */}
                <span className="ui-select-check" aria-hidden="true">
                  <BaseSelect.ItemIndicator keepMounted={false}>✓</BaseSelect.ItemIndicator>
                </span>
                <BaseSelect.ItemText className="ui-select-item-text">{option.label}</BaseSelect.ItemText>
              </BaseSelect.Item>
            ))}
          </BaseSelect.Popup>
        </BaseSelect.Positioner>
      </BaseSelect.Portal>
    </BaseSelect.Root>
  );
}

/**
 * The chevron, as an inline SVG.
 *
 * It is the same path the stylesheet used to paint into `background-image` on every native select, moved into
 * the markup where it can take `currentColor`. THE DATA URI HAD A COLOUR BAKED INTO IT AND IT WAS THE WRONG
 * ONE: `stroke='%23b0b4ba'` (app/globals.css:1006 on d8ca934b) is the literal at line 260 of the same file —
 * `--slate-11` as redefined INSIDE the `prefers-color-scheme: dark` block. So both schemes drew the dark
 * scheme's chevron: 9.06:1 against --bg-page in dark and 2.03:1 in light, where the token it was copied from
 * measures 5.79:1 (contrast.spec.ts's own formula, re-run 2026-07-31). That is the class of bug an inline SVG
 * removes rather than fixes: a colour inside a URL cannot follow a token, and no rule in this repository can
 * measure it.
 */
function Chevron(props: SVGProps<SVGSVGElement>) {
  return (
    <svg {...props} width="10" height="6" viewBox="0 0 10 6" aria-hidden="true" focusable="false">
      <path d="M1 1l4 4 4-4" fill="none" stroke="currentColor" strokeWidth="1.5" strokeLinecap="round" strokeLinejoin="round" />
    </svg>
  );
}
