// THE ROUTE TABLE — the ONE source of the console's navigation and of its accessibility sweep (E25 T2).
//
// It exists because of a measured hole (plan §3.6 D17): Playwright collects a new `*.spec.ts` automatically
// (playwright.config.ts defines no `testMatch`), so a new SPEC cannot be forgotten — but tests/a11y.spec.ts
// used to write the routes it scanned by HAND, and app/layout.tsx wrote its nav links by hand too. A new page
// therefore got ZERO axe coverage and nothing said so. E25 adds seven pages.
//
// Both consumers now DERIVE from this list: app/layout.tsx renders one nav link per row, and
// tests/a11y.spec.ts generates one axe scan per row plus a coverage assertion that every row was scanned. So
// an unscanned page is not a discipline problem, it is structurally impossible — adding a row here adds a
// test, and the a11y spec's test COUNT rises with the list's length.
//
// `readyTestId` is REQUIRED, and that is the point rather than an inconvenience: a page must say what
// "finished loading" means for it, because axe on a page still showing "Loading…" scans a spinner and reports
// a clean bill of health for markup it never saw. It is the data-testid the scan waits for before analyzing.
export interface ConsoleRoute {
  /** The browser path, exactly as the nav links it and the scan visits it. */
  path: string;
  /** The nav link's text. */
  label: string;
  /** The data-testid whose visibility means "this page has rendered its content". */
  readyTestId: string;
}

export const CONSOLE_ROUTES: readonly ConsoleRoute[] = [
  { path: "/", label: "Admin", readyTestId: "panel-organizations" },
  { path: "/runs", label: "Live runs", readyTestId: "run-button" },
];
