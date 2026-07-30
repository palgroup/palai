"use client";

import { usePathname } from "next/navigation";
import type { ReactNode } from "react";

import { CONSOLE_ROUTES } from "@/lib/routes";

// THE PAGE'S OWN IDENTITY, WHICH ELEVEN SCREENS DID NOT HAVE (console design pass).
//
// Before this, the only <h1> in the console was "Palai Console" in the header — the same six characters on
// every page, so the largest text on screen never said which screen it was, and every page opened straight
// into a panel or into a stack of equally-weighted grey paragraphs. That is what makes an admin console read
// as monotone: not the palette, but everything being shown at the same weight with no answer to "where am I
// and what is this for".
//
// So the h1 is the PAGE and the brand is demoted to chrome. The heading level of every panel is unchanged —
// they are <h2>s inside <section>s, which is exactly right one level under a page title, and not one of them
// had to move.
//
// BOTH HALVES READ lib/routes.ts, which already drives the nav and the axe sweep. A page cannot ship without
// a title and a lead sentence for the same structural reason it cannot ship without a `readyTestId`: the
// list is the one place a route is declared, and tests/a11y.spec.ts asserts every declared row carries all
// three.

/** Nav renders one link per declared route, marking the current one for machines AND for eyes. */
export function Nav() {
  const pathname = usePathname();
  return (
    <nav aria-label="Primary">
      {CONSOLE_ROUTES.map((route) => (
        <a
          key={route.path}
          href={route.path}
          // NOT COLOUR ALONE. `aria-current` is the machine-readable half — it is what a screen reader
          // announces — and the stylesheet marks the same link with weight and a filled background, so the
          // current page is legible without colour vision.
          aria-current={pathname === route.path ? "page" : undefined}
        >
          {route.label}
        </a>
      ))}
    </nav>
  );
}

/**
 * PageHeader renders the page title and its first sentence.
 *
 * With no props it reads the current route out of lib/routes.ts, which is how the layout renders it for
 * every declared page. `/login` is deliberately NOT in that list — it sits outside the session gate, so the
 * generated axe loop cannot reach it — and it passes its own, which is also why an unknown path renders
 * NOTHING rather than a blank heading.
 */
export function PageHeader({ title, lead }: { title?: string; lead?: ReactNode }) {
  const pathname = usePathname();
  const route = CONSOLE_ROUTES.find((r) => r.path === pathname);
  const heading = title ?? route?.label;
  const sentence = lead ?? route?.lead;
  if (heading === undefined) return null;
  return (
    <div className="page-header">
      <h1 className="page-title" data-testid="page-title">
        {heading}
      </h1>
      {sentence === undefined ? null : (
        <p className="page-lead" data-testid="page-lead">
          {sentence}
        </p>
      )}
    </div>
  );
}
