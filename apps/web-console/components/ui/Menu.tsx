"use client";

import { Menu as BaseMenu } from "@base-ui/react/menu";
import type { ReactNode } from "react";

// THE FIRST MENU IN THIS CONSOLE (E29 component layer).
//
// MEASURED BEFORE THIS FILE EXISTED, on d8ca934b (2026-07-31):
//   grep -rn 'role="menu"' --include='*.tsx' components app | wc -l → 0
//
// There WAS a menu on screen — the `⋯` at the end of every session row — and it was a <div> of buttons with
// `aria-expanded`/`aria-controls` on the toggle. That markup is announced as "expanded button" followed by
// loose buttons: a screen reader is never told it is IN a menu, how many items it holds, or which one it is
// on. Arrow keys did nothing, Escape only worked while the toggle itself still had focus, and a click
// anywhere else left it open.
//
// AND THE PANEL WAS CLIPPED, which app/globals.css wrote down as a constraint it could not get past: "`.panel`
// scrolls horizontally, so an absolutely-positioned menu inside it is clipped by its own container". The
// workaround was to keep the panel IN FLOW, which makes the row grow taller when the menu opens. Base UI
// portals the popup to the document body and anchors it with Floating UI, so the clip is not worked around —
// it is gone, and the row no longer moves.
//
// SIX ROW MENUS, NOT ONE. The count went up during the rebase onto 81f7dbcf rather than down: page-parity and
// page-parity-govern each moved their row actions into a `⋯` while this branch was in flight, and both wrote
// the same hand-rolled `.row-menu-panel` again — on /agents, /repositories, /policy and three on /fleet. That
// is the argument for a layer stated by the tree rather than by me: the fourth copy of a pattern gets written
// while you are deleting the third. All six are this component now.
//
// A DANGER ITEM TAKES --danger-text AND NOT --danger-text-inline, and the difference is measured rather than
// aesthetic. The inline red (step 11) is what `.form-error` uses on the page surface, where it reads 5.08:1;
// on a HIGHLIGHTED menu row (--bg-hover) it reads 4.27:1, under SC 1.4.3's 4.5. Step 12 clears every surface
// this popup can sit on — 12.13 / 10.18 light, 13.81 / 10.56 dark (contrast.spec.ts's own formula, 2026-08-01).
// Nothing in this repository would have caught that: the boundary sweep does not match a `<div role="menuitem">`
// and axe only sees one if a scan OPENS the popup, which tests/a11y.spec.ts now does.
//
// AND THE COLOUR IS NEVER THE CARRIER — the item reads "Revoke <id>", which is the word this console's rule
// requires. Remove the tint and the item still says what it does.

export interface MenuAction {
  label: string;
  /** A navigation — renders Menu.LinkItem, i.e. a real <a> a middle-click and a screen reader both understand. */
  href?: string;
  /** An action. Exactly one of `href` / `onSelect`. */
  onSelect?: () => void;
  /** The destructive tone. See the header: it is --danger-text, not the inline red. */
  danger?: boolean;
  // THERE IS NO `disabled`. One was written and no caller passed it: all six menus REMOVE an item that does
  // not apply (a pending machine gets no lifecycle row, a revoked key gets no menu at all) rather than
  // showing a dead one, which is this console's existing rule — Picker refuses to render a select with no
  // options rather than rendering an empty one. And Menu.LinkItem has no such prop anyway: an <a> cannot be
  // disabled, so it would have been a prop that worked on half the items.
  testId?: string;
}

export function Menu({
  label,
  trigger,
  items,
  triggerClassName,
  triggerTestId,
  popupTestId,
}: {
  /** The trigger's accessible name — the `⋯` glyph carries none, so this is the whole of it. */
  label: string;
  /** What the trigger renders. A glyph here must be aria-hidden; `label` is what is announced. */
  trigger: ReactNode;
  items: MenuAction[];
  triggerClassName?: string;
  triggerTestId?: string;
  popupTestId?: string;
}) {
  return (
    <BaseMenu.Root>
      {/* `ui-button` RATHER THAN A `ui-menu-trigger` OF ITS OWN. Menu.Trigger renders a native <button>, so
          in this console it IS a button and takes the vocabulary every other one takes; a second class name
          would have needed a rule, and the rule it needed was already written. The geometry (28px square,
          the `⋯` lane) stays the caller's `.row-menu-toggle`. */}
      <BaseMenu.Trigger
        className={triggerClassName === undefined ? "ui-button" : `ui-button ${triggerClassName}`}
        aria-label={label}
        data-testid={triggerTestId}
      >
        {trigger}
      </BaseMenu.Trigger>
      <BaseMenu.Portal>
        <BaseMenu.Positioner className="ui-menu-positioner" sideOffset={4} align="end">
          <BaseMenu.Popup className="ui-menu-popup" data-testid={popupTestId}>
            {items.map((item) =>
              // A LINK IS AN <a>, NOT A BUTTON THAT NAVIGATES. Two of the six callers are navigations
              // (/agents' Revisions and Compare, /repositories' Open binding), and rendering those as items
              // with an onClick would take away middle-click, copy-link and the "link" a screen reader
              // announces. Menu.LinkItem is the same row with the right element under it.
              item.href === undefined ? (
                <BaseMenu.Item
                  key={item.label}
                  className="ui-menu-item"
                  data-danger={item.danger === true ? "true" : undefined}
                  data-testid={item.testId}
                  onClick={item.onSelect}
                >
                  {item.label}
                </BaseMenu.Item>
              ) : (
                <BaseMenu.LinkItem
                  key={item.label}
                  href={item.href}
                  className="ui-menu-item"
                  data-danger={item.danger === true ? "true" : undefined}
                  data-testid={item.testId}
                >
                  {item.label}
                </BaseMenu.LinkItem>
              ),
            )}
          </BaseMenu.Popup>
        </BaseMenu.Positioner>
      </BaseMenu.Portal>
    </BaseMenu.Root>
  );
}
