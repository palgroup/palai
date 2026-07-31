"use client";

import { Tabs as BaseTabs } from "@base-ui/react/tabs";
import type { ReactNode } from "react";

// THE TAB STRIP, LIFTED RATHER THAN REWRITTEN (E29 component layer).
//
// MEASURED BEFORE THIS FILE EXISTED, on d8ca934b (2026-07-31):
//   grep -rn 'role="tablist"' --include='*.tsx' components app | wc -l → 1
//
// One tablist in the whole console — the Transcript/Debug pair on the session detail — and unlike the row
// menu it was already CORRECT: a roving tabindex, arrow keys, aria-selected, aria-controls. So this is not a
// fix. It is the same pattern moved out of a page component and into the layer, so the second tab strip in
// this console is a call rather than a second copy of the APG.
//
// TWO THINGS THE LIBRARY ADDS THAT THE HAND-ROLLED VERSION DID NOT HAVE:
//
//   `inert` on the hidden panel — Base UI sets it beside `hidden` (tabs/panel/TabsPanel.mjs writes
//   `inert: inertValue(!open)` and `hidden` in the same props object; cited rather than measured, unlike the
//   Dialog claims in components/ui/Dialog.tsx, because nothing in this suite reads it). The old markup set
//   `hidden` alone, which takes the panel out of the accessibility tree but does not stop a POINTER or a
//   programmatic focus reaching what is inside it.
//   Home/End. The old handler answered ArrowLeft and ArrowRight only; the APG's tab pattern also specifies
//   Home and End, and a two-tab strip is the one case where nobody notices they are missing.
//
// `keepMounted` IS PASSED BY THE CALLER AND IT IS NOT COSMETIC. Base UI unmounts a hidden panel by default.
// The transcript panel owns the type filter, the search box and the Rendered/Raw choice as React state, so an
// unmounted panel loses all three every time the reader looks at Debug — which is the behaviour the `hidden`
// attribute was giving for free and which would have regressed silently.

export interface TabDef<T extends string> {
  id: T;
  label: string;
}

export function Tabs<T extends string>({
  label,
  tabs,
  value,
  onValueChange,
  children,
}: {
  /** The tablist's accessible name — required, because "tabs" alone names nothing on a page with two. */
  label: string;
  tabs: TabDef<T>[];
  value: T;
  onValueChange: (value: T) => void;
  /** The panels, as <TabPanel value=…> children. */
  children: ReactNode;
}) {
  return (
    <BaseTabs.Root value={value} onValueChange={(next) => onValueChange(next as T)}>
      {/* activateOnFocus — BASE UI DEFAULTS IT TO FALSE AND THE STRIP THIS REPLACES DID NOT. The old
          handler called chooseTab() straight out of ArrowLeft/ArrowRight, so an arrow both moved and
          selected; Base UI's default moves focus and waits for Enter or Space (the APG calls these "manual
          activation" and lists BOTH as conforming). tests/sessions.spec.ts asserts the first — "press
          ArrowRight, the next tab is selected AND focused" — so this is behaviour being PRESERVED, not a
          preference. It is also the right one for two tabs whose panels are already loaded: manual
          activation exists for panels that are expensive to render, and these are not. */}
      <BaseTabs.List className="tabs" aria-label={label} activateOnFocus>
        {tabs.map((tab) => (
          <BaseTabs.Tab key={tab.id} value={tab.id} className="tab" data-testid={`tab-${tab.id}`}>
            {tab.label}
          </BaseTabs.Tab>
        ))}
      </BaseTabs.List>
      {children}
    </BaseTabs.Root>
  );
}

export function TabPanel({ value, children }: { value: string; children: ReactNode }) {
  return (
    <BaseTabs.Panel value={value} keepMounted>
      {children}
    </BaseTabs.Panel>
  );
}
