import { execFileSync } from "node:child_process";
import { readdirSync, readFileSync } from "node:fs";
import { resolve } from "node:path";

// NOTHING IN THIS FILE MAY RUN AT MODULE SCOPE, and nothing may import `@playwright/test`.
// playwright.config.ts imports this module at CONFIG LOAD — before any test context exists — so a top-level
// call here executes on every single run before collection, and a `@playwright/test` import there fails the
// whole run outright. Export functions and plain data; let the caller decide when to pay. (Recorded after
// `formDialogMountCount` moved here from a spec: the next value somebody wants will be tempting to compute
// once at the top, and that is the shape to refuse.)
//
// Shared between the Playwright config (which injects them into the servers) and the tests (which scan
// for the credential + assert the relay origin).
//
// TWO PROFILES, ONE SPEC SET (E19 T7). `PALAI_CONSOLE_PROFILE` selects which /v1 upstream the built console
// is pointed at, and nothing else about the run changes — the same spec files execute against both:
//
//   fake (default) — tests/fake-control-plane.mjs on a loopback port, started by the Playwright webServer.
//                    Deterministic, fast, Docker-free. This layer is NOT going away: it is the only place a
//                    hostile artifact, a scripted recovery, or a paused approval can be produced on demand.
//   real           — a compose control plane. PALAI_BASE_URL and PALAI_API_KEY come from the ENVIRONMENT
//                    ($PALAI_HOME/config.json .base_url and $PALAI_HOME/api-key, read by the caller), never
//                    from a command line and never committed.
//
// ABSENCE IS A FAILURE, NEVER A SKIP. Asking for the real profile without a real stack throws here, which
// fails the whole run loudly at config load. A per-test skip in that situation is the exact trap this
// repo has now been caught by eight times: a suite that reports green for the condition it exists to detect.
export type ConsoleProfile = "fake" | "real";

export const PROFILE: ConsoleProfile = (process.env.PALAI_CONSOLE_PROFILE ?? "fake") === "real" ? "real" : "fake";
export const IS_REAL = PROFILE === "real";

export const NEXT_PORT = Number(process.env.PALAI_CONSOLE_PORT ?? 3200);
export const UPSTREAM_PORT = Number(process.env.FAKE_UPSTREAM_PORT ?? 3201);

// A SECOND CONSOLE, IDENTICAL EXCEPT FOR THE ONE THING BEING PROVEN (E25 T1). The fail-closed claim is
// "a console with no PALAI_CONSOLE_PASSWORD_HASH does not serve", and the only honest way to assert it is
// against a real process that has none. This port carries the same build, the same API key and the same
// upstream — the hash is the single difference, so nothing else can explain the refusals.
export const UNCONFIGURED_PORT = Number(process.env.PALAI_CONSOLE_UNCONFIGURED_PORT ?? 3202);

// The operator password for both profiles. It is a test credential in the same sense FAKE_API_KEY is: it
// opens a loopback fixture that this repo starts and stops, and it sits three lines from its own hash — the
// point is that the door is real, not that this password is secret.
export const CONSOLE_PASSWORD = "console-proof-operator-password-DO-NOT-REUSE-4c1f9a2b";

// consolePasswordHash runs the SHIPPED script to produce the hash the server is given. Nothing is hardcoded
// and nothing is duplicated: scripts/hash-password.mjs is the only writer of the format, lib/session.ts is
// the only reader, and this call is what binds them — if the script's output ever stops being something the
// console accepts, every sign-in in the suite fails. It is a function rather than a constant so only the
// Playwright config pays for the derivation.
export function consolePasswordHash(): string {
  const line = execFileSync("node", ["scripts/hash-password.mjs"], { input: CONSOLE_PASSWORD, encoding: "utf8" }).trim();
  const prefix = "PALAI_CONSOLE_PASSWORD_HASH=";
  if (!line.startsWith(prefix)) throw new Error(`scripts/hash-password.mjs printed something unexpected: ${line.slice(0, 40)}…`);
  return line.slice(prefix.length);
}

// THE AXE TAG SET, IN ONE PLACE (E25 T2, plan §3.6 D16). Every axe scan in this suite uses these tags, and
// the list is here rather than inline because it was inline in four places and all four said WCAG 2.0.
//
// axe-core's own tag table defines `wcag2a`/`wcag2aa` as "WCAG 2.0 Level A"/"Level AA"; `wcag21a`,
// `wcag21aa` and `wcag22aa` are SEPARATE tags and were not included. So the console's accessibility evidence
// covered WCAG 2.0 only, and 2.4.11 Focus Not Obscured, 3.3.7 Redundant Entry and 3.3.8 Accessible
// Authentication — all new in 2.2, all criteria a FORM is judged by — sat entirely outside it, on a surface
// E25 is adding six forms to.
//
// What widening this proves is "axe-clean with the WCAG 2.0+2.1+2.2 tags", NOT "accessible": Deque's own
// figure is that axe finds "on average 57% of WCAG issues", and no scanner judges whether an error message
// MEANS anything. That gap is §6 operator leg 1 and it does not narrow here.
export const WCAG_TAGS = ["wcag2a", "wcag2aa", "wcag21a", "wcag21aa", "wcag22aa"];

// The fake profile's API key is a distinctive sentinel so the browser-surface secret scan is meaningful:
// this exact string is the server-only credential, and it must appear in NO browser surface (request
// headers, URLs, bodies, source maps, static chunks). The relay is its only holder. On the real profile the
// SAME scan runs against the REAL bootstrap key — a stronger assertion, since a leak would be a live
// credential. The scans compare with a boolean rather than a matcher so a failure never prints the key.
const FAKE_API_KEY = "palai-sk-console-proof-DO-NOT-LEAK-1a2b3c4d5e6f7a8b";

function realEnv(name: string): string {
  const value = process.env[name]?.trim();
  if (!value) {
    throw new Error(
      `PALAI_CONSOLE_PROFILE=real requires ${name}, and this run has none. The real profile drives the built ` +
        "console against a RUNNING compose control plane; without it there is nothing to prove, and skipping " +
        "would report green for the absence itself. Bring the stack up with `PALAI_DISPATCH_WORKERS=1 " +
        "PALAI_MODEL_PROVIDER=fake palai local up`, then export PALAI_BASE_URL=$(… config.json .base_url) and " +
        "PALAI_API_KEY=$(cat $PALAI_HOME/api-key). Never pass the key as an argument.",
    );
  }
  return value;
}

export const UPSTREAM = IS_REAL ? realEnv("PALAI_BASE_URL").replace(/\/+$/, "") : `http://127.0.0.1:${UPSTREAM_PORT}`;
export const API_KEY = IS_REAL ? realEnv("PALAI_API_KEY") : FAKE_API_KEY;

// THE CREATE DIALOGS THE SWEEPS OPEN. Declared HERE rather than in a spec because TWO specs need the
// same five rows — tests/a11y.spec.ts scans each dialog with axe, tests/contrast.spec.ts measures the
// controls inside it — and Playwright refuses a spec-to-spec import outright ("test file X should not
// import test file Y"). A second hand-typed copy is what a declaration table exists to prevent.
//
// THE CREATE DIALOGS, AND THEY WERE SCANNED BY NOTHING AT ALL UNTIL THIS LOOP.
//
// The generated axe loop scans a route AS IT LOADS. A components/FormDialog.tsx renders only after a click,
// so every create form that moved behind a `+ Create` button left the accessibility evidence the moment it
// moved — five forms, on four routes, none of them scanned. It is the same hole this file already documents
// in tests/a11y.spec.ts for the transcript's second tab ("`hidden` takes the whole Debug panel out of the
// accessibility tree, so the first scan reports a clean bill of health for markup it did not see"), and a
// modal is the worse case of it: a dialog owns the focus trap, the accessible name and the Escape contract,
// which is exactly the surface axe has rules for.
//
// THE LIST IS DECLARED AND THEN CHECKED AGAINST THE SOURCE, so a sixth dialog cannot ship unscanned. The
// coverage test at the bottom of tests/a11y.spec.ts walks app/**/page.tsx for `<FormDialog` mounts and fails if the
// count does not match the rows here — the same shape as the route coverage assertion, and for the same
// reason: a surface nobody scans must be a red test rather than a thing somebody remembers.
export const FORM_DIALOGS: { route: string; open: string; dialog: string; label: string; dynamic?: string; rowMenu?: string }[] = [
  { route: "/agents", open: "agent-create-open", dialog: "agent-create-dialog", label: "Create an agent" },
  { route: "/repositories", open: "binding-create-open", dialog: "binding-create-dialog", label: "Register a repository binding" },
  { route: "/policy", open: "key-mint-open", dialog: "key-mint-dialog", label: "Mint an API key" },
  { route: "/fleet", open: "pool-create-open", dialog: "pool-create-dialog", label: "Create a runner pool" },
  { route: "/fleet", open: "poolkey-mint-open", dialog: "poolkey-mint-dialog", label: "Mint an enrolment key" },
  { route: "/deployment", open: "desired-config-edit", dialog: "desired-config-dialog", label: "Edit desired configuration" },
  // The route publish (panel-credentials): the control that turns a stored connection into the credential
  // runs actually use. Its picker and model field do not exist until the dialog is open, which is exactly
  // the shape that walked out of the contrast sweep once already.
  { route: "/registry", open: "route-publish-open", dialog: "route-publish-dialog", label: "Point runs at a connection" },
  // The bot registration (panel-bots). Three credential fields that do not exist in the DOM until the dialog
  // is open — the largest single block of secret-bearing controls this console has, and therefore the one
  // most worth scanning OPEN rather than at rest. They belong to the CHOSEN channel, which is why
  // app/bots/page.tsx opens the dialog on one rather than on a blank selector: on a blank one they would not
  // be rendered at all and both sweeps would cover less while reporting a cleaner number.
  { route: "/bots", open: "bot-create-open", dialog: "bot-create-dialog", label: "Create a bot" },
  // THE TWO ROW-MENU DIALOGS (machine-config), AND `rowMenu` IS WHAT MAKES THEM DECLARABLE AT ALL.
  //
  // Every row above names a control that is VISIBLE when its route loads — a panel head's `+ Create X`. A
  // per-row action is not: components/ui/Menu.tsx portals its popup to <body> on click, so `pool-config-…`
  // does not exist in the document until the row's `⋯` has been opened, and a loop that went straight to
  // `getByTestId(open)` would time out on a control that is not missing but unrendered.
  //
  // BOTH TESTIDS ARE PREFIXES BECAUSE A ROW'S CONTROLS ARE KEYED BY ITS ID, and the ids are not constants:
  // the fake seeds `pool_default` / `pool_mac` while a compose stack mints a fresh hash per pool and per
  // machine. profile.ts's openDeclaredDialog takes the FIRST row offering the control, which is the same
  // claim tests/fleet.spec.ts's firstMachineWith already makes and the reason it runs on both profiles.
  {
    route: "/fleet",
    rowMenu: "pool-menu-",
    open: "pool-config-",
    dialog: "pool-config-dialog",
    label: "Configure this runner pool",
  },
  {
    route: "/fleet",
    rowMenu: "runner-menu-",
    open: "machine-config-",
    dialog: "machine-config-dialog",
    label: "Configure this machine",
  },
  // THE FIRST TWO ROWS ON A DYNAMIC ROUTE (E30), and `dynamic` is what makes that expressible. Every row
  // above names a path the loop can `goto` directly; a binding's own page is keyed by an id, so this names
  // the DYNAMIC_CONSOLE_ROUTES pattern instead and the loop resolves it through concreteDynamicPath —
  // which CREATES a binding when the collection is empty rather than skipping, the same refusal-to-skip
  // that route already carries.
  //
  // Both openers exist only on a LIVE binding: an archived one accepts neither a credential (the API
  // refuses the write) nor a second archive. That is not a hazard for the sweep, because the default list
  // hides archived rows and concreteDynamicPath samples the first row of it, so the binding it resolves is
  // always one whose controls are present.
  {
    route: "/repositories/[id]",
    dynamic: "/repositories/[id]",
    open: "binding-connection-open",
    dialog: "binding-connection-dialog",
    label: "Set this binding's credential",
  },
  {
    route: "/repositories/[id]",
    dynamic: "/repositories/[id]",
    open: "binding-archive",
    dialog: "binding-archive-dialog",
    label: "Archive this binding",
  },
];

/**
 * THE DIALOGS BUILT ON THE `components/ui/Dialog` PRIMITIVE, which the list above structurally cannot hold.
 *
 * THE HOLE THIS CLOSES WAS MEASURED, 2026-08-01, on 3f53c5f9 before the catalogue existed:
 *
 *   grep -rn '<Dialog[ >]' --include='*.tsx' app components | grep -v components/ui/Dialog.tsx | wc -l → 1
 *   grep -rn '<FormDialog' --include='*.tsx' app components | grep -v components/FormDialog.tsx | wc -l → 5
 *
 * FORM_DIALOGS describes those five and `formDialogMountCount()` counts them, so a sixth FormDialog cannot
 * ship unscanned. NOTHING COUNTED THE OTHER ONE. `formDialogMountCount()` walks `app/**\/page.tsx` for the
 * string `<FormDialog`, and it is blind on BOTH axes to the primitive: wrong component name, wrong
 * directory. A `<Dialog>` mounted inside `components/` is invisible to it, and the tree's own guidance is to
 * prefer the primitive — so every dialog written from here on would have landed in the blind spot, and the
 * guard would have stayed green while doing it.
 *
 * The one pre-existing primitive mount, components/ConfirmDestructive.tsx, IS scanned — by a hand-written
 * test ("axe-core reports zero violations on /fleet with a destructive dialog OPEN", tests/a11y.spec.ts).
 * That is why it is exempt below rather than a row here: it is a generic wrapper opened from many screens,
 * not a dialog belonging to one route, and the bespoke test already opens it where it matters.
 *
 * A new component that mounts `<Dialog` and appears in neither list fails the coverage assertion at the
 * bottom of tests/a11y.spec.ts. That is the whole point — the same shape as the FormDialog guard, on the
 * component the tree is being told to use.
 */
export const PRIMITIVE_DIALOGS: { route: string; open: string; dialog: string; label: string }[] = [
  // THE ⌘K NAVIGATOR (E31 shell parity), and it is the first row here that belongs to the SHELL rather than
  // to a route. Its `route` is "/" only because the sweeps need somewhere to open it from; the control is in
  // the rail and is therefore on all fifteen. That asymmetry is worth stating: a shell dialog scanned on one
  // route is scanned everywhere, while a route dialog scanned on the wrong route is scanned nowhere.
  { route: "/", open: "command-search-open", dialog: "command-search-dialog", label: "Search console" },
  { route: "/tools", open: "catalogue-open", dialog: "catalogue-dialog", label: "Known MCP servers" },
  // Opening a template dialog needs a CARD, so the trigger is one template's own button rather than a
  // generic "open" — the gallery has no single opener and inventing one for the sweep would be a control
  // that exists only for the sweep.
  { route: "/agents", open: "template-open-incident-commander", dialog: "template-dialog", label: "Incident commander" },
];

/**
 * Components that mount the primitive but are covered by a NAMED bespoke scan instead of the generated loop.
 * Each entry must say which test opens it, because an exemption whose reason is not written down is how a
 * list like this rots into a suppression.
 */
export const PRIMITIVE_DIALOG_EXEMPT: { file: string; scannedBy: string }[] = [
  {
    file: "ConfirmDestructive.tsx",
    scannedBy: 'tests/a11y.spec.ts — "axe-core reports zero violations on /fleet with a destructive dialog OPEN"',
  },
];

/**
 * primitiveDialogMountCount is how many FILES mount the Dialog primitive, walked rather than declared.
 *
 * It counts FILES, not occurrences, because the unit that needs a scan is the dialog a component owns —
 * AgentTemplates.tsx mounts one `<Dialog` and shows three different templates through it, and that is one
 * surface to scan, not three. Node builtins only, for the reason `formDialogMountCount` gives: this module
 * is imported at Playwright config load, before any test context exists.
 */
export function primitiveDialogMountCount(): number {
  const root = resolve(process.cwd(), "components");
  let files = 0;
  for (const entry of readdirSync(root, { recursive: true, withFileTypes: true })) {
    if (!entry.isFile() || !entry.name.endsWith(".tsx")) continue;
    const path = resolve(entry.parentPath ?? root, entry.name);
    // The primitive itself DEFINES `<BaseDialog.…>`; it never mounts `<Dialog`, so it needs no exclusion by
    // name. Checking the content rather than the filename is what keeps a rename from silently zeroing this.
    if (/<Dialog[\s>]/.test(readFileSync(path, "utf8"))) files += 1;
  }
  return files;
}

/**
 * formDialogMountCount is how many create dialogs the SOURCE actually mounts, walked rather than declared.
 *
 * IT LIVES BESIDE THE LIST IT CHECKS, AND THAT IS THE WHOLE POINT. It was inline in tests/a11y.spec.ts, so
 * tests/contrast.spec.ts — which opens the same five dialogs to measure the controls inside them — was
 * complete only by virtue of ANOTHER FILE running an assertion it could not see. A sixth dialog would have
 * reddened the axe loop and left the contrast sweep quietly short, and nothing in contrast.spec.ts would have
 * said so. Both call this now, so each sweep is complete on its own terms.
 *
 * `<FormDialog` is the right thing to count because that is exactly the surface that does not exist until
 * somebody clicks: a route walk cannot see it, and every sweep that walks routes therefore has to be told it
 * is there.
 *
 * IT WALKED `app/**\/page.tsx` UNTIL E29 AND THAT WAS THE SAME BLIND SPOT PRIMITIVE_DIALOGS WAS WRITTEN
 * ABOUT, one axis over. That comment records the primitive escaping on the wrong COMPONENT NAME and the
 * wrong DIRECTORY; this one escaped on the wrong FILENAME. A dialog owned by a route lives just as happily
 * in `app/deployment/DesiredConfig.tsx` as in `app/deployment/page.tsx` — it is the same click, on the same
 * route, equally invisible to a route walk — and the counter would have stayed at five while a sixth
 * shipped unscanned. Measured on this branch before the widening:
 *
 *   grep -rln '<FormDialog[ >]' --include='page.tsx' app | wc -l         -> 5   (what it counted)
 *   grep -rn  '<FormDialog[ >]' --include='*.tsx' app components         -> 6   (what exists)
 *
 * So it now walks every `.tsx` under `app/` AND `components/`, which is what primitiveDialogMountCount has
 * always done for the primitive — the two counters no longer disagree about what a mount is. FormDialog.tsx
 * itself DEFINES the component and never mounts it, so it needs no exclusion by name (the same reasoning the
 * primitive's walk records); the check is on content, which keeps a rename from silently zeroing this.
 *
 * NODE BUILTINS ONLY, deliberately. playwright.config.ts imports this module at CONFIG LOAD, before any test
 * context exists, so a `@playwright/test` import here would take the whole run down at collection — the same
 * failure class as the spec-to-spec import that sent FORM_DIALOGS to this file in the first place.
 *
 * AND IT IS A FUNCTION RATHER THAN A CONST, WHICH IS THE HALF THE PARAGRAPH ABOVE DOES NOT COVER. The
 * tempting shape for a value that never changes during a run is `export const FORM_DIALOG_MOUNTS =
 * countThem()` — and at module scope that walks the whole app tree on EVERY config load, including the ones
 * that have nothing to do with these two specs, and takes the run down before collection if it ever throws
 * on a permission or a half-written file. A caller pays for the walk when it needs the number; nobody else
 * pays for it at all. See the file header: this module must stay side-effect-free.
 */
export function formDialogMountCount(): number {
  let mounted = 0;
  for (const dir of ["app", "components"]) {
    const root = resolve(process.cwd(), dir);
    for (const entry of readdirSync(root, { recursive: true, withFileTypes: true })) {
      if (!entry.isFile() || !entry.name.endsWith(".tsx")) continue;
      mounted += (readFileSync(resolve(entry.parentPath ?? root, entry.name), "utf8").match(/<FormDialog[\s>]/g) ?? []).length;
    }
  }
  return mounted;
}
