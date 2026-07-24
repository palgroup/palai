import type { Metadata } from "next";
import type { ReactNode } from "react";

import "./globals.css";

export const metadata: Metadata = {
  title: "Palai Console",
  description: "Open-core admin + live-run console — public API only.",
};

// The a11y shell (UI-001): a document language, a skip link as the first Tab stop, a landmark header
// with keyboard-reachable nav, and a single <main> landmark the skip link targets. Everything below is
// framed by these landmarks so a screen-reader/keyboard user can navigate the regions (axe: landmarks,
// bypass-block, html-has-lang).
export default function RootLayout({ children }: { children: ReactNode }) {
  return (
    <html lang="en">
      <body>
        <a className="skip-link" href="#main">
          Skip to main content
        </a>
        <header className="app-header">
          <h1>Palai Console</h1>
          <nav aria-label="Primary">
            <a href="/">Admin</a>
            <a href="/runs">Live runs</a>
          </nav>
        </header>
        <main id="main">{children}</main>
      </body>
    </html>
  );
}
