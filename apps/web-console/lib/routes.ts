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
  // E25 T4. Its readiness signal is the LIST panel rather than a form, for the same reason "/" uses
  // panel-organizations: the forms on this page render their final markup synchronously (no data feeds them
  // except the environment picker, and BOTH of its states — a select, or the "create one first" note — are
  // real states worth scanning), while the panel is the one part that can still be a spinner.
  { path: "/environments", label: "Environments", readyTestId: "panel-environments" },
  // E25 T5. The readiness signal is the list PANEL, and it is the honest one on both profiles: the fixture
  // parks five gated calls while a compose stack can park none (DIV-UI-006), so on the real profile this panel
  // renders its EMPTY state — which is a state worth scanning, because an empty queue plus the page's scope
  // sentence is the difference between "nothing is waiting" and "nothing of this KIND is waiting".
  { path: "/approvals", label: "Approvals", readyTestId: "panel-approvals" },
];
