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
  /**
   * The page's FIRST SENTENCE — what this screen is for, in one line, above everything it renders.
   *
   * It is REQUIRED for the same reason `readyTestId` is. Every page in this console used to open with a
   * panel or with a wall of equally-weighted notes, and the only <h1> on any of them was "Palai Console" in
   * the header — repeated on all eleven screens, so the largest text on the page never said which page it
   * was. A console reads as monotone when everything is shown at equal weight; a lead sentence is the
   * cheapest thing that is not equal weight, and putting it here means a new page cannot ship without one.
   */
  lead: string;
}

export const CONSOLE_ROUTES: readonly ConsoleRoute[] = [
  { path: "/", label: "Admin", readyTestId: "panel-organizations", lead: "The configuration this deployment is actually running, read straight from the public API: the organizations and projects, the keys that reach them, the model wiring behind them, and the agents they run." },
  { path: "/runs", label: "Live runs", readyTestId: "run-button", lead: "Start a run and watch it happen — the canonical event stream as it arrives, the approval it parks on, and whatever it leaves behind." },
  // E25 T4. Its readiness signal is the LIST panel rather than a form, for the same reason "/" uses
  // panel-organizations: the forms on this page render their final markup synchronously (no data feeds them
  // except the environment picker, and BOTH of its states — a select, or the "create one first" note — are
  // real states worth scanning), while the panel is the one part that can still be a spinner.
  { path: "/environments", label: "Environments", readyTestId: "panel-environments", lead: "The named KEY=value sets an agent's shell commands run against. A value is written here and never read back." },
  // E25 T5. The readiness signal is the list PANEL, and it is the honest one on both profiles: the fixture
  // parks five gated calls while a compose stack can park none (DIV-UI-006), so on the real profile this panel
  // renders its EMPTY state — which is a state worth scanning, because an empty queue plus the page's scope
  // sentence is the difference between "nothing is waiting" and "nothing of this KIND is waiting".
  { path: "/approvals", label: "Approvals", readyTestId: "panel-approvals", lead: "Gated tool calls waiting for an answer. Read the arguments, then decide: the decision binds to the request hash the row carries and to nothing else." },
  // E25 T6. The readiness signal is the LIST panel on both, for the same reason the two rows above use one:
  // the forms render synchronously and the panel is the only part that can still be a spinner. Both
  // collections are EMPTY on a bootstrap stack — nothing creates a repository binding or an agent without
  // Slack — so on the real profile these scans cover the empty state, which is the state a first-day
  // operator meets and the one the create forms sit above.
  { path: "/repositories", label: "Repositories", readyTestId: "panel-repository-bindings", lead: "The repositories a coding run can attach a workspace to. Registering a binding checks nothing — the first thing that exercises one is a run." },
  // `panel-agent-profiles`, NOT `panel-agents`: "/" already carries a panel by that name (it is where list
  // truncation is visible — pagination.spec.ts drives it), and two pages answering one testid is how a spec
  // ends up asserting against whichever page it happened to be on.
  { path: "/agents", label: "Agents", readyTestId: "panel-agent-profiles", lead: "An agent is a name with a lineage of immutable revisions. A revision is drafted here and published once; publishing is what makes a run pinned to it reproducible, and it cannot be undone." },
  // E25 T7. `panel-mcp-connections` is the FIRST panel on the page and the one that can still be a spinner;
  // the forms below it render synchronously. It is empty on every bootstrap stack (nothing registers an MCP
  // connection without an operator), so on both profiles this scan covers the empty state — which is the
  // state a first-day operator meets, and the one the registration form sits above.
  { path: "/tools", label: "Tools", readyTestId: "panel-mcp-connections", lead: "What an upstream MCP server offers, and what this deployment lets a model actually call. Discovery leaves drafts; approving one is a publish, and pinning it into a set is what puts it in front of a model." },
  // E25 T8 — THE OBSERVABILITY HALF, AND IT IS THREE ROUTES FOR SIX SCREENS (feature list §7 O1/O2/O4/O5/
  // O6/O9). §T8 says each screen adds a row here; three of the six cannot have a row of their own and that
  // is structural rather than a saving: O2 (a past run's event stream) and O6 (that run's artifacts) are
  // both keyed by a RUN ID, and a row here must be a path this list's own axe loop can `goto` on a stack
  // that may hold no runs at all. A `/history/[id]` route would be a declared row with no scannable path,
  // which is exactly the unscanned-page hole this file exists to close. So they live on /history as the
  // drill-down of the run the operator selected, and each still gets its own panel testid and its own leg.
  //
  // `panel-runs` is the list, and it is the honest readiness signal on both profiles: a bootstrap stack
  // whose console has never started a run answers GET /v1/responses with an empty page, so on the real
  // profile this scan covers the empty state — which is the state a first-day operator meets.
  { path: "/history", label: "Run history", readyTestId: "panel-runs", lead: "Every run this project has started and what each one wrote — the event journal replayed from the record rather than remembered, and the files it produced." },
  // O4/O5, THE READ HALF ONLY (the write half is E26). `panel-usage-meters` is the first of four panels and
  // the one that can still be a spinner. All four are EMPTY on a fresh stack and that is the point of the
  // scan: an empty metering screen is a rendered SENTENCE here, never a blank region (DIV-UI-002 measured
  // how thin the real surface is; a console that answers thinness with whitespace is unreadable).
  { path: "/usage", label: "Usage", readyTestId: "panel-usage-meters", lead: "What has been spent in this scope and what caps it. This is a read surface: nothing on it sets, raises or removes a limit." },
  // O9. The tiers are shown as GET /v1/capabilities returns them — no re-derivation, no prettifying, no
  // console-side opinion about what "preview" ought to mean. `palai up` prints this to a terminal
  // (up.go capabilityRows) and until now the console did not show it at all.
  { path: "/capabilities", label: "Capabilities", readyTestId: "panel-capabilities", lead: "What this deployment advertises to every client and at which tier — exactly as GET /v1/capabilities answers it, with no console-side opinion about what a tier ought to mean." },
];
