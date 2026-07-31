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

/**
 * NAV_GROUPS is the sidebar's section order, and it is the console's information architecture.
 *
 * The nav was a FLAT ROW OF TWELVE LINKS in a top bar, which is a list rather than a structure: every screen
 * was offered at the same weight, in the order the epics happened to add them, so nothing said which screens
 * belong together or which one an operator starts from. Twelve peers is also past the point where a reader
 * scans rather than reads.
 *
 * The grouping answers "what am I doing", and it is deliberately a VERB per group rather than a noun: you
 * BUILD the things a run needs, you OPERATE the runs, you GOVERN what they may spend and reach. "Overview"
 * is its own group of one because a dashboard is not a peer of the screens it summarises.
 *
 * IT IS ORDERED, AND THE ORDER IS THE NAV'S. A route naming a group that is not in this list would be
 * unreachable in the sidebar while still being scanned by axe — an unlinked page, which is half of the hole
 * this file exists to close — so components/Chrome.tsx asserts the partition covers every route.
 */
export const NAV_GROUPS = ["Overview", "Build", "Operate", "Govern"] as const;
export type NavGroup = (typeof NAV_GROUPS)[number];

export interface ConsoleRoute {
  /** The browser path, exactly as the nav links it and the scan visits it. */
  path: string;
  /** The nav link's text. */
  label: string;
  /** The sidebar section this screen belongs to. Every route carries one; see NAV_GROUPS. */
  group: NavGroup;
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
  // "Overview", not "Admin", and the rename is the page's: this was a twelve-screen scroll of every registry
  // the public API answers, stacked at equal weight with no answer to "what is this deployment doing". The
  // registries that nothing else needed here moved to /registry; what stays is what an overview is FOR.
  //
  // FOUR COLLECTIONS STAY ON THIS PAGE BECAUSE SHIPPED SPECS BIND THEM HERE, and that is recorded rather than
  // worked around: tests/public-api-only.spec.ts reads `panel-organizations` and `panel-secret-refs` on "/",
  // and tests/pagination.spec.ts drives `panel-agents` here — three files this pass is not allowed to edit,
  // plus tests/profile.ts's sign-in landing. So organizations, projects, secret refs and agents are rendered
  // as the overview's own registry strip; model connections, model routes and knowledge bases, which no spec
  // pins to this path, are on /registry where they belong.
  { path: "/", label: "Overview", group: "Overview", readyTestId: "panel-organizations", lead: "What this deployment is running right now — the scope you are in, what it holds, and what is waiting on you." },
  // E29. FIRST IN "Operate", because a session is the thing every other row on this surface belongs to: a run
  // has a session, a response has a session, an approval is raised inside one. Until now the console could
  // list RESPONSES (/history) and had no screen for the container they live in, so "what has this deployment
  // been asked to do" had no answer and "who asked it twice" had none either.
  { path: "/sessions", label: "Sessions", group: "Operate", readyTestId: "panel-sessions", lead: "Every conversation this project has held, newest first — what it was called, what it ran, and what it cost." },
  { path: "/runs", label: "Live runs", group: "Operate", readyTestId: "run-button", lead: "Start a run and watch it happen — the canonical event stream as it arrives, the approval it parks on, and whatever it leaves behind." },
  // E25 T4. Its readiness signal is the LIST panel rather than a form, for the same reason "/" uses
  // panel-organizations: the forms on this page render their final markup synchronously (no data feeds them
  // except the environment picker, and BOTH of its states — a select, or the "create one first" note — are
  // real states worth scanning), while the panel is the one part that can still be a spinner.
  { path: "/environments", label: "Environments", group: "Build", readyTestId: "panel-environments", lead: "The named KEY=value sets an agent's shell commands run against. A value is written here and never read back." },
  // E25 T5. The readiness signal is the list PANEL, and it is the honest one on both profiles: the fixture
  // parks five gated calls while a compose stack can park none (DIV-UI-006), so on the real profile this panel
  // renders its EMPTY state — which is a state worth scanning, because an empty queue plus the page's scope
  // sentence is the difference between "nothing is waiting" and "nothing of this KIND is waiting".
  { path: "/approvals", label: "Approvals", group: "Operate", readyTestId: "panel-approvals", lead: "Gated tool calls waiting for an answer. Read the arguments, then decide: the decision binds to the request hash the row carries and to nothing else." },
  // E25 T6. The readiness signal is the LIST panel on both, for the same reason the two rows above use one:
  // the forms render synchronously and the panel is the only part that can still be a spinner. Both
  // collections are EMPTY on a bootstrap stack — nothing creates a repository binding or an agent without
  // Slack — so on the real profile these scans cover the empty state, which is the state a first-day
  // operator meets and the one the create forms sit above.
  // THE LEAD IS UNCHANGED and that is deliberate: it already carries the ceiling in one sentence, which is
  // what the page-parity pass wants a lead to do. The two paragraphs that stood under it moved to the
  // controls they govern — the reachability note into the create dialog, the "cannot be changed" note onto
  // the binding's own page, where an operator goes looking for the edit control that does not exist.
  { path: "/repositories", label: "Repositories", group: "Build", readyTestId: "panel-repository-bindings", lead: "The repositories a coding run can attach a workspace to. Registering a binding checks nothing — the first thing that exercises one is a run." },
  // `panel-agent-profiles`, NOT `panel-agents`: "/" already carries a panel by that name (it is where list
  // truncation is visible — pagination.spec.ts drives it), and two pages answering one testid is how a spec
  // ends up asserting against whichever page it happened to be on.
  // THE LEAD IS ONE SENTENCE AGAIN (page-parity pass). It used to be three clauses carrying the whole
  // publish-is-irreversible rule, on a screen that then repeated it in a paragraph under the title and again
  // in the revision panel's note. A lead says what the screen IS; the rule belongs where the publish control
  // is, which is app/agents/[id] and docs/operations/console.md §4c.
  { path: "/agents", label: "Agents", group: "Build", readyTestId: "panel-agent-profiles", lead: "The agents this project can run — one row per lineage, with the revisions a run can be pinned to." },
  // E25 T7. `panel-mcp-connections` is the FIRST panel on the page and the one that can still be a spinner;
  // the forms below it render synchronously. It is empty on every bootstrap stack (nothing registers an MCP
  // connection without an operator), so on both profiles this scan covers the empty state — which is the
  // state a first-day operator meets, and the one the registration form sits above.
  { path: "/tools", label: "Tools", group: "Build", readyTestId: "panel-mcp-connections", lead: "What an upstream MCP server offers, and what this deployment lets a model actually call. Discovery leaves drafts; approving one is a publish, and pinning it into a set is what puts it in front of a model." },
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
  { path: "/history", label: "Run history", group: "Operate", readyTestId: "panel-runs", lead: "Every run this project has started and what each one wrote — the event journal replayed from the record rather than remembered, and the files it produced." },
  // O4/O5, THE READ HALF ONLY (the write half is E26). `panel-usage-meters` is the first of four panels and
  // the one that can still be a spinner. All four are EMPTY on a fresh stack and that is the point of the
  // scan: an empty metering screen is a rendered SENTENCE here, never a blank region (DIV-UI-002 measured
  // how thin the real surface is; a console that answers thinness with whitespace is unreadable).
  { path: "/usage", label: "Usage", group: "Govern", readyTestId: "panel-usage-meters", lead: "What has been spent in this scope and what caps it. This is a read surface: nothing on it sets, raises or removes a limit." },
  // O9. The tiers are shown as GET /v1/capabilities returns them — no re-derivation, no prettifying, no
  // console-side opinion about what "preview" ought to mean. `palai up` prints this to a terminal
  // (up.go capabilityRows) and until now the console did not show it at all.
  { path: "/capabilities", label: "Capabilities", group: "Govern", readyTestId: "panel-capabilities", lead: "What this deployment advertises to every client and at which tier — exactly as GET /v1/capabilities answers it, with no console-side opinion about what a tier ought to mean." },
  // E28 T2. The readiness signal is the KEY panel rather than the policy form, for the reason /environments
  // and /repositories use a list: the forms render their markup synchronously, and the panel is the one part
  // that can still be a spinner. It is `panel-api-keys` specifically because that collection is non-empty on
  // every stack — a bootstrap seeds the admin key — so the scan meets rendered rows on both profiles.
  { path: "/policy", label: "Policy & keys", group: "Govern", readyTestId: "panel-api-keys", lead: "A project's whole configuration policy, and the keys that reach it. Saving here writes the policy document ENTIRELY — the five fields on this screen are what the project will have afterwards, and a value you cannot see is one you did not send." },
  // E28 T3. The readiness signal is the POOL panel — the first thing on the page, the only collection that is
  // non-empty on every stack (a tenant is seeded one pool at birth), and the one every section below it
  // depends on: the key panel picks from it and the machines carry its id. Every other panel here renders its
  // EMPTY state on a real stack, because nothing in the public API can enrol a machine (DIV-UI-009), and
  // those empty states are the fleet screen a first-day operator actually meets.
  { path: "/fleet", label: "Fleet", group: "Operate", readyTestId: "panel-runner-pools", lead: "The machines this deployment can place a run on: the pools they join, the keys that let them in, and the ones waiting to be admitted. A machine here runs a run's ENGINE — every tool still runs in the control plane's own process." },
  // THE THREE REGISTRIES THAT CAME OFF "/" (console design pass). They are read-only projections of how a
  // model is reached, and they were the middle of a twelve-screen scroll on the landing page — which is
  // where a deployment's least-consulted configuration should not be. `panel-model-connections` is the
  // first panel and the one that can still be a spinner. All three are empty on a bootstrap stack, so on
  // the real profile this scan covers their empty states.
  { path: "/registry", label: "Model wiring", group: "Govern", readyTestId: "panel-model-connections", lead: "How a model is reached from this deployment: the provider connections, the routes that name them, and the knowledge bases a run can retrieve from. This is a read surface — every row here is created by the API or the CLI." },
];

/**
 * A route with a DYNAMIC SEGMENT — a screen keyed by a resource id, which CONSOLE_ROUTES structurally cannot
 * hold and which was therefore, until now, a page with no accessibility coverage at all.
 *
 * THE HOLE IS RECORDED IN THIS FILE'S OWN HISTORY. The E25 T8 comment on `/history` says a `/history/[id]`
 * row "would be a declared row with no scannable path, which is exactly the unscanned-page hole this file
 * exists to close", and the conclusion drawn was to avoid dynamic routes entirely — three screens folded onto
 * one path. That works until a screen genuinely IS a resource: a session transcript is about ONE session, its
 * breadcrumb names that session, and a query parameter would be the same dynamic segment wearing a disguise.
 *
 * So the list is declared, and what it declares is how to MAKE the path concrete: `sampleFrom` is the /v1
 * collection whose first row fills the segment, and `create` is what to POST when that collection is empty.
 * tests/a11y.spec.ts resolves each row through the relay and scans the result — and because `create` exists,
 * an empty collection produces a row rather than a skip. That is the whole point: a deployment with no
 * sessions must not be a deployment whose transcript screen is silently unscanned.
 *
 * These rows are NOT in the sidebar and must not be: a link to one particular session is not navigation.
 * components/Chrome.tsx reads CONSOLE_ROUTES only, which is why the two lists are separate rather than one
 * list with a flag.
 */
export interface DynamicConsoleRoute {
  /** The Next route pattern, exactly as the directory declares it — the identity a coverage assertion uses. */
  pattern: string;
  /** What this screen is, for a failure message. */
  label: string;
  /** The data-testid whose visibility means the page has rendered its content (same contract as above). */
  readyTestId: string;
  /** The /v1 collection path (after /v1) whose rows can fill the segment. */
  sampleFrom: string;
  /** The body to POST to `sampleFrom` when it holds no row. An empty object is a valid create here. */
  create: Record<string, unknown>;
  /** The concrete browser path for one row's id. */
  build: (id: string) => string;
  /**
   * A SECOND SCREEN BEHIND A TAB, declared so the axe loop can open it.
   *
   * A scan of the page as it loads never looks at the other tab: `hidden` takes the whole panel out of the
   * accessibility tree, so the first scan reports a clean bill of health for markup it did not see — the
   * same shape as scanning a page still rendering "Loading…", one interaction later. tests/a11y.spec.ts used
   * to hard-code `tab-debug` and `session-debug` inside its dynamic loop, which meant the SECOND dynamic
   * route to exist would have run the session transcript's assertions against a page that has neither. The
   * pair is data now, and a tabbed screen that omits it simply gets no second scan — visibly, here, rather
   * than by a locator failing on the wrong page.
   */
  secondTab?: { tabTestId: string; panelTestId: string; url: RegExp };
}

export const DYNAMIC_CONSOLE_ROUTES: readonly DynamicConsoleRoute[] = [
  {
    pattern: "/sessions/[id]",
    label: "Session transcript",
    // THE CHIP ROW, NOT THE TRANSCRIPT SECTION — measured, 2026-08-01. `session-transcript` is the <section>
    // the transcript renders SYNCHRONOUSLY, so it became visible while the chip row above it was still a
    // "Loading…" paragraph: a scan firing on that signal analyses a page whose summary has not arrived. A
    // probe that polls every 20ms for each declared signal and records what is still loading at the instant
    // it appears reported `STILL LOADING: loading` for this route and `clean` for the other sixteen.
    //
    // It did not fail, and that is the uncomfortable half: tests/a11y.spec.ts follows the signal with a
    // `waitForLoadState("networkidle")`, so the chips had landed by the time axe looked. This file's own
    // comments call networkidle "a proxy for the condition" and the dynamic loop says so twice — a wrong
    // signal compensated for by a proxy is a green that belongs to the harness.
    readyTestId: "session-chips",
    sampleFrom: "/sessions",
    create: {},
    build: (id) => `/sessions/${encodeURIComponent(id)}`,
    secondTab: { tabTestId: "tab-debug", panelTestId: "session-debug", url: /segment=debug/ },
  },
  // THE AGENT LINEAGE (page-parity pass). It is a resource screen for the same reason the transcript is: the
  // revision draft, the publish and the diff are all about ONE agent, and while they lived on /agents that
  // screen carried a picker whose only job was to re-ask which row the operator had already chosen.
  //
  // `create` CARRIES A NAME, because POST /v1/agents refuses a nameless one with a 400 ("name is required",
  // store/agents.go MissingName) — an empty body here would make the empty-collection arm fail rather than
  // create, which is exactly the silent skip this list exists to prevent. The name says where the row came
  // from, since on a real stack it persists.
  {
    pattern: "/agents/[id]",
    label: "Agent lineage",
    // THE CHIP ROW, not the revisions panel, and that is a measurement rather than a preference:
    // components/RevisePublish.tsx emits `panel-agent-revisions` from its FIRST paint with the word
    // "Loading…" inside it, so a scan waiting on that name would look at a spinner. The chip row renders
    // only once the lineage read has settled, in either direction.
    readyTestId: "agent-chips",
    sampleFrom: "/agents",
    create: { name: "axe-coverage-probe" },
    build: (id) => `/agents/${encodeURIComponent(id)}`,
    secondTab: { tabTestId: "tab-compare", panelTestId: "panel-agent-diff", url: /segment=compare/ },
  },
  // THE BINDING RECORD (page-parity pass). Four of its nine fields — the clone URL, the data classification,
  // the region constraint and the pass-through policy object — were on NO screen: the console wrote them and
  // never showed them back. They do not fit a list, so they are here.
  //
  // `create` carries the three fields POST /v1/repository-bindings requires, and the clone URL is an
  // `https:` one because the scheme gate is checked FIRST (api/repository_bindings.go) — a `file:` probe
  // would make the empty-collection arm fail rather than create. NO `secondTab`: one record, one view, and a
  // tab strip over a single definition list would be chrome around a thing with no second side.
  {
    pattern: "/repositories/[id]",
    label: "Repository binding",
    readyTestId: "panel-binding-record",
    sampleFrom: "/repository-bindings",
    create: {
      provider: "github",
      repository_identity: "palai-example/axe-coverage-probe",
      clone_url: "https://example.invalid/palai-example/axe-coverage-probe.git",
    },
    build: (id) => `/repositories/${encodeURIComponent(id)}`,
  },
];
