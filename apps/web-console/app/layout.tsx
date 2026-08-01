import type { Metadata } from "next";
import type { ReactNode } from "react";

import { Shell } from "@/components/Chrome";

import "./globals.css";

export const metadata: Metadata = {
  title: "Palai Console",
  description: "Open-core admin + live-run console — public API only.",
};

// The a11y shell (UI-001): a document language, a skip link as the first Tab stop, a landmark header
// with keyboard-reachable nav, and a single <main> landmark the skip link targets. Everything below is
// framed by these landmarks so a screen-reader/keyboard user can navigate the regions (axe: landmarks,
// bypass-block, html-has-lang).
//
// The nav links are DERIVED from lib/routes.ts (E25 T2) — the same list tests/a11y.spec.ts scans. They were
// two hand-written anchors, which is one half of how a new page could reach production both unlinked and
// unscanned.
//
// THE BRAND IS NO LONGER THE h1 (console design pass). "Palai Console" was the only h1 on every one of the
// eleven pages, which meant the biggest text on screen never said which page you were on and no page had a
// first sentence. It is chrome — a link home — and components/Chrome.tsx renders the PAGE's title and lead
// inside <main>, from the same route table the nav and the axe sweep already read. No panel heading moved:
// they are <h2>s, which is the correct level directly under a page title.
//
// THE FRAME ITSELF NOW LIVES IN components/Chrome.tsx, and the reason is `/login`. The shell has to render a
// SIDEBAR on twelve screens and no navigation at all on the front door, which is a decision about the current
// path — something a Server Component cannot see without threading it. `children` is still whatever the route
// rendered on the server; only the frame around it is a client boundary.
export default function RootLayout({ children }: { children: ReactNode }) {
  return (
    <html lang="en">
      <body>
        <a className="skip-link" href="#main">
          Skip to main content
        </a>
        <Shell>{children}</Shell>
      </body>
    </html>
  );
}
