"use client";

import { usePathname } from "next/navigation";
import { createContext, useContext, useEffect, useMemo, useRef, useState, type ReactNode } from "react";

import { Dialog } from "@/components/ui/Dialog";
import { Select } from "@/components/ui/Select";
import { apiGet } from "@/lib/api";
import { CONSOLE_ROUTES, NAV_GROUPS } from "@/lib/routes";
import { rememberedProject, rememberProject } from "@/lib/scope";

// THE SHELL — the rail, measured against the reference console rather than designed (E31 shell parity).
//
// WHAT WAS HERE: a rail carrying four FLAT groups under uppercase micro-labels, an accent-filled active item,
// and a scope readout at the foot. The owner's verdict on it was "hiç bir şey anlaşılmıyor menülerden" —
// nothing is legible from the menus — and the instruction was "şu menülerin birebir aynısını yap".
//
// SO EVERY NUMBER BELOW WAS READ OFF THE REFERENCE'S OWN RAIL with getComputedStyle / getBoundingClientRect
// on 2026-08-01, and the two that contradict design-reference/measured-2026-08-01.md are called out where
// they land. In summary, and each is a `grep`-able fact in app/globals.css §5:
//
//   rail 256px on a surface DARKER than the page      — the doc has this and our tree had it backwards
//   item box 36px, gap 4px  → the 40px pitch the doc reports; the item itself is not 40 tall
//   item pad 0 8px, gap 12px, radius 8px, icon 20x20
//   CHILD items: padding-left 40px and NO icon, which lands a child's label at the same x as a group
//                header's label (8 pad + 20 icon + 12 gap). That alignment is why it reads as a tree.
//   top-level links and group headers: label w500. CHILDREN: w400. OPEN group header: w580.
//                THE DOC SAYS w400 FOR ALL OF THEM AND THAT IS WRONG — measured on all 32 of their items.
//   active item: background rgba(255,255,255,0.05), colour #fff, weight UNCHANGED.
//                The doc says "a background block", which is right; what it does not say is that the fill is
//                a surface rather than an accent and that the weight does not move. Ours was an accent fill
//                + a 2px accent rail + medium weight, i.e. three marks where the reference has one.
//
// AND ONE THING WE KEEP THAT THE REFERENCE DOES NOT NEED: a non-colour mark on the current item. The
// reference distinguishes it by fill and by text colour, both of which are colour. app/globals.css keeps a
// shape — see `.rail-item[aria-current="page"]` — because `aria-current` is announced but nothing on the
// screen would say "here" to a reader who cannot separate #fff from rgb(195,194,183).

/** navGroupId is the DOM id a group's heading and its list share. Kept in one place so they cannot drift. */
const navGroupId = (group: string) => `nav-group-${group.toLowerCase()}`;

// --- THE PAGE'S OWN LIST, PUBLISHED UPWARDS -------------------------------------------------------------
//
// THE PROBLEM, VISIBLE ON EIGHT SCREENS: `/sessions` rendered `<h1>Sessions</h1>` and then, forty pixels
// lower, `<h2>Sessions</h2>` with the row count and the create button beside it. The page said its own name
// twice and the only thing between the two was a sentence. Measured on the reference: ONE title, the count
// badge on the same line (`API keys ⁴`), the primary action at the far right of that line, and the search +
// filter row directly under it.
//
// The h1 is rendered by the SHELL (from lib/routes.ts) and the count is known only to the PANEL, which is
// two levels down inside `children`. This context is the seam between them and it is deliberately tiny: a
// panel that is the page's subject publishes its count and its action, and PageHeader renders them on the
// title line. Nothing else passes through it.
//
// A SECOND CLAIMANT IS A DEFECT AND IT SAYS SO. Two panels both calling themselves the page's subject would
// silently give the title line whichever rendered last; `ListChromeProvider` counts them and the console
// warns rather than picking. (It is a warning and not a throw because a broken title line must not blank a
// working screen.)
// THE COUNT CROSSES AS A STRING AND THE ACTION AS A NODE, AND THAT ASYMMETRY IS THE WHOLE DESIGN.
//
// A React element is a new object on every render, so anything that compares published chrome by identity
// never settles — measured as a hard render loop that took every page to Next's "This page couldn't load"
// screen. `countLabel` is a plain string, so the provider can ask "is this the same claim as last time?" and
// answer honestly; the action rides along and is simply re-captured whenever the label moves.
interface ListChrome {
  /** The badge's text, e.g. "20 rows" or "3 of 20 rows". A STRING, for the reason above. */
  countLabel?: string;
  /** The badge's data-testid, so the page's badge answers to the panel's own name. */
  countTestId?: string;
  action?: ReactNode;
}
const ListChromeContext = createContext<{
  chrome: ListChrome | null;
  publish: (id: string, chrome: ListChrome | null) => void;
} | null>(null);

/**
 * usePageList lets a Panel put its count and its primary action on the page's own title line.
 *
 * TWO EFFECTS, AND THE FIRST ONE HAS NO DEPENDENCY ARRAY ON PURPOSE. It runs after every render and hands
 * the current claim down; the provider drops it on the floor when the label is unchanged (returning the same
 * state object, which React bails out of), so the steady state is one no-op call per render and no update.
 * A dependency array would have to list the action node, which is exactly the identity that never settles.
 *
 * The second effect owns the UNPUBLISH, and it is separate because its dependencies genuinely are just the
 * identity: a cleanup on the first effect would fire after every render and thrash the title line.
 */
export function usePageList(id: string, chrome: ListChrome | null, enabled: boolean) {
  const ctx = useContext(ListChromeContext);
  const publish = ctx?.publish;
  useEffect(() => {
    if (publish === undefined || !enabled || chrome === null) return;
    publish(id, chrome);
  });
  useEffect(() => {
    if (publish === undefined || !enabled) return;
    return () => {
      publish(id, null);
    };
  }, [publish, id, enabled]);
}

/**
 * Shell frames every page: the sidebar, and the <main> the skip link targets.
 *
 * `/login` gets NO sidebar, and that is a decision rather than an omission: the whole console behind that
 * door answers 401 without a session, so a nav rendered there offers fifteen links that all refuse. The front
 * door is one screen with one field on it.
 */
export function Shell({ children }: { children: ReactNode }) {
  const pathname = usePathname();
  const bare = pathname === "/login";
  if (bare) {
    return (
      <div className="app-shell app-shell-bare">
        <main id="main" className="content content-bare">
          <Brand standalone />
          {children}
        </main>
      </div>
    );
  }
  return (
    <ListChromeProvider>
      <div className="app-shell">
        {/* TWO ELEMENTS, AND THE INNER ONE IS WHY. The <header> is the grid CELL — it carries the rail's fill
            and its edge, and a grid cell stretches to the row's height, so the rail reaches the bottom of a
            twelve-screen page instead of stopping at the fold. The sticky viewport-height box has to be the
            INNER one: `position: sticky` on the cell itself would make the cell 100vh tall and leave the fill
            hanging in mid-page. */}
        <header className="sidebar">
          <div className="sidebar-inner">
            <Brand />
            <Workspace />
            <CommandSearch />
            <Nav />
            <RailFoot />
          </div>
        </header>
        {/* THE PAGE IS A PANEL ON THE RAIL, NOT A COLUMN BESIDE IT. Measured: the reference's main region
            carries `border-radius: 12px 0 0 12px` over a darker rail, which is what makes the rail read as
            BEHIND the page rather than as a second column of equal standing. It costs one radius. */}
        <main id="main" className="content">
          <PageHeader />
          {children}
        </main>
      </div>
    </ListChromeProvider>
  );
}

/** ListChromeProvider holds whatever the page's own list published for the title line. See usePageList. */
function ListChromeProvider({ children }: { children: ReactNode }) {
  const [claims, setClaims] = useState<Record<string, ListChrome>>({});
  // `publish` IS STABLE FOR THE LIFETIME OF THE PROVIDER, AND THAT IS LOAD-BEARING RATHER THAN TIDY.
  //
  // Written the obvious way — inside the same `useMemo` as `chrome`, keyed on `claims` — it gets a new
  // identity every time a claim lands. usePageList's effect depends on it, so the effect re-runs, publishes
  // again, produces a new `claims` object, and the loop never settles: measured as a hard render loop that
  // took every page in the console to Next's "This page couldn't load" screen on the first click.
  //
  // It closes over NOTHING (the updater below is the functional form), so a `useRef`-held constant is
  // correct rather than merely cheap.
  const publish = useRef((id: string, chrome: ListChrome | null) => {
    setClaims((was) => {
      if (chrome === null) {
        if (!(id in was)) return was;
        const next = { ...was };
        delete next[id];
        return next;
      }
      // THE SAME CLAIM AS LAST TIME IS NOT AN UPDATE. Returning `was` unchanged is what React bails out of,
      // and it is what turns "publish on every render" into one no-op call rather than a loop. The comparison
      // is on the LABEL because that is the only part of a claim that is comparable — see ListChrome.
      if (was[id]?.countLabel === chrome.countLabel) return was;
      return { ...was, [id]: chrome };
    });
  }).current;
  const value = useMemo(() => ({ chrome: Object.values(claims)[0] ?? null, publish }), [claims, publish]);
  const claimants = Object.keys(claims);
  if (claimants.length > 1) {
    // eslint-disable-next-line no-console -- a title line that silently picks one of two is worse than a warning.
    console.warn(`two panels claim this page's title line (${claimants.join(", ")}); showing the first. Exactly one panel per page may set pageTitle.`);
  }
  return <ListChromeContext.Provider value={value}>{children}</ListChromeContext.Provider>;
}

/**
 * Brand is the wordmark, and it is the console's ONLY piece of pure identity.
 *
 * The mark is drawn here rather than fetched: three bars at rising heights, the tallest in the accent. It
 * costs one inline <svg>, no network request, no font file and no entry in any CSP — the same reasoning that
 * keeps system-ui as the typeface. It is aria-hidden because the word beside it already names the product;
 * an alt text of "logo" is noise in a screen reader.
 */
function Brand({ standalone = false }: { standalone?: boolean }) {
  return (
    <a className={standalone ? "brand brand-standalone" : "brand"} href="/">
      <svg className="brand-mark" viewBox="0 0 18 18" width="18" height="18" aria-hidden="true" focusable="false">
        <rect x="0" y="10" width="4" height="8" rx="1.5" fill="var(--brand-mark-quiet)" />
        <rect x="7" y="5" width="4" height="13" rx="1.5" fill="var(--brand-mark-quiet)" />
        <rect x="14" y="0" width="4" height="18" rx="1.5" fill="var(--brand-mark-live)" />
      </svg>
      <span className="brand-word">palai</span>
      <span className="brand-kind">Console</span>
    </a>
  );
}

// --- THE ICONS ------------------------------------------------------------------------------------------
//
// One 20x20 stroke glyph per top-level row, drawn here for the reason the wordmark is drawn here: no icon
// font, no sprite request, no CSP entry. They take `currentColor`, so they follow the item's own state
// (muted → #fff on the current page) with no second rule.
//
// A CHILD ROW HAS NO ICON, and that is the reference's own arrangement rather than a saving: fifteen glyphs
// down a rail is a column of decoration, and what the eye needs at depth two is the INDENT. See
// `.rail-item[data-depth="2"]`, whose 40px left padding lands a child's label under its parent's label.
const ICON: Record<string, ReactNode> = {
  // Overview — four panes, which is what a dashboard is.
  Overview: (
    <>
      <rect x="3" y="3" width="6.5" height="6.5" rx="1.5" />
      <rect x="10.5" y="3" width="6.5" height="6.5" rx="1.5" />
      <rect x="3" y="10.5" width="6.5" height="6.5" rx="1.5" />
      <rect x="10.5" y="10.5" width="6.5" height="6.5" rx="1.5" />
    </>
  ),
  // Usage — three bars at rising heights. Deliberately the same idea as the wordmark: this console's one
  // recurring picture is a meter.
  Usage: (
    <>
      <path d="M3.5 16.5v-4" />
      <path d="M10 16.5V6" />
      <path d="M16.5 16.5v-7" />
    </>
  ),
  // Build — a wrench. The things under it are what a run is assembled FROM.
  Build: (
    <path d="M13.2 3.4a4.2 4.2 0 0 0-5 5.4l-5 5a1.6 1.6 0 0 0 2.3 2.3l5-5a4.2 4.2 0 0 0 5.4-5l-2.4 2.4-2-2z" />
  ),
  // Operate — a play mark in a ring: something is running, and you are watching it.
  Operate: (
    <>
      <circle cx="10" cy="10" r="7.2" />
      <path d="M8.4 7.2 13 10l-4.6 2.8z" />
    </>
  ),
  // Manage — sliders. Settings with positions, which is what every screen under it holds.
  Manage: (
    <>
      <path d="M3 6.5h10M16 6.5h1" />
      <path d="M3 13.5h1M7 13.5h10" />
      <circle cx="14.5" cy="6.5" r="2" />
      <circle cx="5.5" cy="13.5" r="2" />
    </>
  ),
  // The foot's two rows.
  Documentation: (
    <>
      <path d="M3.5 4.5h4.2A2.3 2.3 0 0 1 10 6.8v9.7a2 2 0 0 0-2-1.5H3.5z" />
      <path d="M16.5 4.5h-4.2A2.3 2.3 0 0 0 10 6.8v9.7a2 2 0 0 1 2-1.5h4.5z" />
    </>
  ),
  Org: (
    <>
      <path d="M4 17V5.5a1 1 0 0 1 1-1h6a1 1 0 0 1 1 1V17" />
      <path d="M12 9h3.2a1 1 0 0 1 1 1V17" />
      <path d="M6.5 8h3M6.5 11.5h3" />
    </>
  ),
};

/** RailIcon wraps one glyph in the 20x20 box every row reserves. */
function RailIcon({ name }: { name: string }) {
  const glyph = ICON[name];
  if (glyph === undefined) return <span className="rail-icon" aria-hidden="true" />;
  return (
    <svg
      className="rail-icon"
      viewBox="0 0 20 20"
      width="20"
      height="20"
      aria-hidden="true"
      focusable="false"
      fill="none"
      stroke="currentColor"
      strokeWidth="1.4"
      strokeLinecap="round"
      strokeLinejoin="round"
    >
      {glyph}
    </svg>
  );
}

interface ScopeRow {
  id?: string;
  display_name?: string;
}

/**
 * scopeLabel is the row's own name, and it treats an EMPTY STRING as absent.
 *
 * `??` does not, which is the whole of "the SCOPE box reads broken": `String(row.display_name ?? row.id)`
 * keeps a `display_name` of `""` because `""` is neither null nor undefined — so a deployment whose projects
 * carry no display name rendered a blank line under the label, and, with more than one of them, a dropdown of
 * blank options. It was never a styling problem; it was a fallback that could not fire.
 */
function scopeLabel(row: ScopeRow | undefined): string {
  if (row === undefined) return "—";
  const name = String(row.display_name ?? "").trim();
  if (name !== "") return name;
  const id = String(row.id ?? "").trim();
  return id === "" ? "—" : id;
}

/**
 * useScope reads the organisation and the project list once, for the two ends of the rail.
 *
 * IT IS ONE HOOK FOR TWO COMPONENTS BECAUSE IT IS ONE PAIR OF READS. The project picker sits at the TOP of
 * the rail and the org card at the FOOT — measured, that is where the reference puts its workspace picker and
 * its account card — and having each fetch its own copy would double two requests on every page load to render
 * two halves of one fact.
 *
 * A failure is SILENT and that is deliberate: on the un-configured console (no operator password) every relay
 * read is a 401, and a shell that rendered an alert on every page would put a role="alert" on fifteen screens
 * that are simply closed.
 */
function useScope() {
  const [orgs, setOrgs] = useState<ScopeRow[] | null>(null);
  const [projects, setProjects] = useState<ScopeRow[] | null>(null);
  const [project, setProject] = useState("");

  useEffect(() => {
    let live = true;
    apiGet<{ data?: ScopeRow[] }>("/organizations")
      .then((body) => {
        if (live) setOrgs(body.data ?? []);
      })
      .catch(() => {
        /* closed door or an unreachable upstream — the shell states nothing rather than alarming fifteen pages */
      });
    apiGet<{ data?: ScopeRow[] }>("/projects")
      .then((body) => {
        if (!live) return;
        const rows = body.data ?? [];
        setProjects(rows);
        const remembered = rememberedProject();
        const known = rows.some((r) => r.id === remembered);
        setProject(known ? remembered : String(rows[0]?.id ?? ""));
      })
      .catch(() => {
        /* same rule */
      });
    return () => {
      live = false;
    };
  }, []);

  return { orgs, projects, project, setProject };
}

/**
 * Workspace is the project picker, at the TOP of the rail where the reference puts its workspace control.
 *
 * IT IS A READOUT FIRST AND A CONTROL SECOND, AND THE ORDER IS THE HONEST ONE. This console holds ONE
 * server-side API key, and every /v1 read is scoped by that key's tenant — there is no request parameter on
 * /v1/agents, /v1/responses or /v1/usage that narrows a read to a project. A dropdown that changed what those
 * screens fetched would therefore be a control that does nothing, which is the exact shape of defect this
 * tree keeps finding in its own code. So the picker RENDERS ONLY WHEN THERE IS A CHOICE — more than one row
 * in the collection — and until then the scope is stated, not offered.
 *
 * AND IT IS NOT DECORATIVE, WHICH IS WHY lib/scope.ts EXISTS. `/policy` is the one screen that takes a
 * project as a parameter — it writes that project's whole configuration document and mints keys against it —
 * and it now OPENS on whichever project was chosen here. That is a small effect and it is a real one, which
 * is the difference between a control and an ornament. Its own picker writes back through the same module,
 * so the two can never disagree.
 *
 * THE ONE-PROJECT ARM IS A <div> AND NOT A DISABLED <button>. A disabled control is a control that says "you
 * may do this later"; there is no later here, because a second project is created by the API and not by this
 * screen. It carries the picker's geometry so the rail does not reflow when a second project appears.
 */
function Workspace() {
  const { projects, project, setProject } = useScope();
  const chosen = projects?.find((p) => p.id === project) ?? projects?.[0];
  const many = (projects?.length ?? 0) > 1;

  function choose(id: string) {
    setProject(id);
    rememberProject(id);
  }

  return (
    <div className="rail-workspace" data-testid="scope">
      {many ? (
        <Select
          className="workspace-select"
          label="Project"
          testId="scope-project-select"
          value={project}
          onValueChange={choose}
          options={(projects ?? []).map((p) => ({ value: String(p.id), label: scopeLabel(p) }))}
        />
      ) : (
        <div className="workspace-static" data-testid="scope-project-static">
          <span className="workspace-mark" aria-hidden="true" />
          {/* The role is named for a screen reader and not repeated on screen: the rail already establishes
              that this is the scope, and a visible "Project:" prefix on a one-project deployment is a word
              that never changes. */}
          <span className="sr-only">Project:</span>
          <span className="workspace-name">{scopeLabel(chosen)}</span>
        </div>
      )}
    </div>
  );
}

/**
 * RailFoot is Documentation and the organisation card, in the reference's own order at the foot of the rail.
 *
 * THE ORG CARD IS A READOUT AND NOT A BUTTON. The reference's is a menu opener (account, billing, sign out);
 * ours has nowhere to go — the console has one server-side credential and no account model — so rendering a
 * button here would be a control that does nothing, which is the defect this file's neighbours are full of
 * comments about. It carries the card's geometry and none of its affordance.
 */
function RailFoot() {
  const { orgs } = useScope();
  const org = orgs?.[0];
  return (
    <div className="rail-foot">
      <a className="rail-item" href="https://github.com/palgroup/palai#readme" data-testid="rail-documentation">
        <RailIcon name="Documentation" />
        <span className="rail-label">Documentation</span>
        {/* The arrow is the standing convention for "this leaves the console", and it is aria-hidden because
            the accessible name below says the same thing in words. */}
        <span className="rail-external" aria-hidden="true">
          ↗
        </span>
        <span className="sr-only">(opens in a new tab)</span>
      </a>
      <div className="rail-org" data-testid="rail-org">
        <span className="rail-org-mark" aria-hidden="true">
          <RailIcon name="Org" />
        </span>
        <span className="rail-org-text">
          <span className="rail-org-name">{scopeLabel(org)}</span>
          <span className="rail-org-sub">{org === undefined ? "—" : String(org.id ?? "—")}</span>
        </span>
      </div>
    </div>
  );
}

/**
 * CommandSearch is the ⌘K row, and it OPENS SOMETHING — which is the whole reason it is 90 lines rather than
 * a styled div.
 *
 * The reference carries a `Search Console… ⌘K` control under its workspace picker and the brief asks for it.
 * A row that looked like that and did nothing would be this repository's most-repeated defect shipped on
 * purpose (`/runs`'s `Agent (optional)` picker sent nothing; `CreateSlackConnection` had no prod caller). So
 * it is a real navigator over the ONE list that can honestly back it: lib/routes.ts, the same table the nav
 * and the axe sweep read. Type `env`, get Environments.
 *
 * WHAT IT IS NOT: a search over the deployment's DATA. /v1 offers no `?q=` on any collection —
 * components/Panel.tsx's filter box carries that measurement and states its own scope for the same reason —
 * so a palette that offered to find a session by name would be a box that fails to find rows that exist.
 * It searches screens, it says "Screens" above its results, and it stops there.
 */
function CommandSearch() {
  const [open, setOpen] = useState(false);
  const [query, setQuery] = useState("");
  const input = useRef<HTMLInputElement | null>(null);

  useEffect(() => {
    const onKey = (event: KeyboardEvent) => {
      if (event.key.toLowerCase() !== "k" || !(event.metaKey || event.ctrlKey)) return;
      event.preventDefault();
      setOpen((was) => !was);
    };
    window.addEventListener("keydown", onKey);
    return () => window.removeEventListener("keydown", onKey);
  }, []);

  // The match is over the label AND the lead, so "spend" finds Usage and "machine" finds Fleet — the words an
  // operator has are about the JOB, and a label-only match makes the operator guess our nouns.
  const hits = useMemo(() => {
    const q = query.trim().toLowerCase();
    if (q === "") return CONSOLE_ROUTES;
    return CONSOLE_ROUTES.filter((r) => `${r.label} ${r.group} ${r.lead}`.toLowerCase().includes(q));
  }, [query]);

  return (
    <>
      <button
        type="button"
        className="rail-search"
        data-testid="command-search-open"
        onClick={() => {
          setOpen(true);
        }}
      >
        <svg className="rail-icon" viewBox="0 0 20 20" width="20" height="20" aria-hidden="true" focusable="false" fill="none" stroke="currentColor" strokeWidth="1.4" strokeLinecap="round">
          <circle cx="9" cy="9" r="5.5" />
          <path d="m13.2 13.2 3.3 3.3" />
        </svg>
        <span className="rail-search-label">Search console…</span>
        {/* The shortcut is rendered as two <kbd>s because that is what they are; `aria-hidden` keeps a screen
            reader from spelling "command K" after a button already named "Search console". */}
        <span className="rail-search-keys" aria-hidden="true">
          <kbd>⌘</kbd>
          <kbd>K</kbd>
        </span>
      </button>
      <Dialog
        open={open}
        onOpenChange={setOpen}
        title="Search console"
        description="Every screen this console has, by name or by what it is for."
        testId="command-search-dialog"
        initialFocus={input}
        actions={
          <button
            type="button"
            onClick={() => {
              setOpen(false);
            }}
            data-testid="command-search-close"
          >
            Close
          </button>
        }
      >
        <div className="command-search">
          <label className="sr-only" htmlFor="command-search-input">
            Filter screens
          </label>
          <input
            id="command-search-input"
            ref={input}
            type="search"
            placeholder="Screen name, or what you are trying to do…"
            data-testid="command-search-input"
            value={query}
            onChange={(e) => {
              setQuery(e.target.value);
            }}
          />
          {hits.length === 0 ? (
            <p className="empty" data-testid="command-search-empty">
              No screen matches <strong>{query}</strong>. This searches SCREENS, not the data on them — /v1
              offers no text search on any collection.
            </p>
          ) : (
            <ul className="command-hits" data-testid="command-search-hits">
              {hits.map((route) => (
                <li key={route.path}>
                  <a href={route.path} data-testid={`command-hit-${route.path === "/" ? "overview" : route.path.slice(1)}`}>
                    <span className="command-hit-label">{route.label}</span>
                    <span className="command-hit-group">{route.group}</span>
                    <span className="command-hit-lead">{route.lead}</span>
                  </a>
                </li>
              ))}
            </ul>
          )}
        </div>
      </Dialog>
    </>
  );
}

/** OPEN_GROUPS_KEY is where a reader's own open/closed choices survive a navigation. */
const OPEN_GROUPS_KEY = "palai.console.navGroups";

/**
 * Nav renders the rail's two top-level links and its three collapsible sections.
 *
 * THE PARTITION IS CHECKED. A route whose group is not in NAV_GROUPS would be scanned by axe and linked from
 * nowhere — an unlinked page, which is exactly half of the hole lib/routes.ts exists to close — so the
 * leftover is computed and rendered as a visible group rather than dropped on the floor.
 *
 * A GROUP IS A NATIVE <details>, AND THAT IS TWO DECISIONS AT ONCE.
 *
 *   1. It is the element for a disclosure, it needs no JavaScript, and it carries the expanded state for a
 *      screen reader without an aria-expanded anybody has to remember to update. app/globals.css already uses
 *      it for `details.notes` and says so: "the native element for exactly this, and it costs no JavaScript".
 *   2. IT IS NOT A <button>, WHICH MATTERS HERE FOR A REASON WORTH WRITING DOWN. tests/contrast.spec.ts scores
 *      every <button> as `max(border, fill) >= 3` for SC 1.4.11, and components/ui/Button.tsx records the
 *      consequence: "a borderless, unfilled button scores 0 and reddens the sweep… this console's quiet
 *      buttons are quiet in GEOMETRY and never in boundary". The reference's group headers have neither a
 *      border nor a fill — they are text — and SC 1.4.11 agrees with the reference: where a control is
 *      identified only by its text, it is SC 1.4.3 at 4.5:1 that judges it, and 1.4.11 adds nothing.
 *      A <summary> is therefore not an evasion of the sweep, it is the element whose contrast obligation is
 *      the one that actually applies — and tests/shell.spec.ts measures that obligation directly, in both
 *      colour schemes, so the coverage is added rather than sidestepped.
 *
 * THE OPEN STATE IS ROUTE-DERIVED ON THE SERVER AND READER-OVERRIDDEN ON THE CLIENT, in that order, because
 * a value read from localStorage during render is a hydration mismatch. First paint opens the group holding
 * the current page (which is what a reader who just navigated wants); an effect then applies whatever they
 * last chose. Both are correct at the moment they run and they cannot disagree with the server.
 */
export function Nav() {
  const pathname = usePathname();
  const placed = new Set<string>();
  const groups = NAV_GROUPS.map(({ name, collapsible }) => {
    const rows = CONSOLE_ROUTES.filter((r) => r.group === name);
    for (const r of rows) placed.add(r.path);
    return { group: name as string, collapsible, rows };
  });
  const orphans = CONSOLE_ROUTES.filter((r) => !placed.has(r.path));
  const sections = [...groups, ...(orphans.length === 0 ? [] : [{ group: "Ungrouped", collapsible: true, rows: orphans }])];

  // Route-derived: the group holding the current page. Computed on every render so a navigation into a
  // closed group opens it — which is what a reader who followed a link expects to see.
  const activeGroup = sections.find((s) => s.rows.some((r) => r.path === pathname))?.group;
  const [overrides, setOverrides] = useState<Record<string, boolean>>({});

  useEffect(() => {
    try {
      const raw = window.localStorage.getItem(OPEN_GROUPS_KEY);
      if (raw !== null) setOverrides(JSON.parse(raw) as Record<string, boolean>);
    } catch {
      /* a browser with storage denied simply gets the route-derived default, which is a usable rail */
    }
  }, []);

  function toggle(group: string, next: boolean) {
    setOverrides((was) => {
      const merged = { ...was, [group]: next };
      try {
        window.localStorage.setItem(OPEN_GROUPS_KEY, JSON.stringify(merged));
      } catch {
        /* same rule: the rail still works, the choice simply does not survive a reload */
      }
      return merged;
    });
  }

  return (
    <nav aria-label="Primary" className="rail-nav">
      <ul className="rail-list">
        {sections.map(({ group, collapsible, rows }) =>
          rows.length === 0 ? null : !collapsible ? (
            // A TOP-LEVEL LINK, NOT A SECTION OF ONE. The reference opens with two of these (Dashboard, API
            // keys) and never draws a header over a single row; lib/routes.ts's NAV_GROUPS comment carries
            // the measurement and the reason.
            rows.map((route) => (
              <li key={route.path}>
                <a
                  className="rail-item"
                  data-depth="1"
                  href={route.path}
                  aria-current={pathname === route.path ? "page" : undefined}
                >
                  <RailIcon name={group} />
                  <span className="rail-label">{route.label}</span>
                </a>
              </li>
            ))
          ) : (
            <li key={group}>
              <details
                className="rail-group"
                open={overrides[group] ?? group === activeGroup}
                onToggle={(e) => {
                  toggle(group, (e.currentTarget as HTMLDetailsElement).open);
                }}
              >
                <summary className="rail-item rail-summary" data-depth="1" id={navGroupId(group)}>
                  <RailIcon name={group} />
                  <span className="rail-label">{group}</span>
                  {/* The chevron is drawn rather than left to the UA marker: `list-style: none` on the
                      summary removes the triangle (see globals.css §5), and a disclosure with no visible
                      affordance is a disclosure nobody opens. It rotates on [open]. */}
                  <svg className="rail-chevron" viewBox="0 0 12 12" width="12" height="12" aria-hidden="true" focusable="false" fill="none" stroke="currentColor" strokeWidth="1.6" strokeLinecap="round" strokeLinejoin="round">
                    <path d="m3 4.5 3 3 3-3" />
                  </svg>
                </summary>
                <ul aria-labelledby={navGroupId(group)}>
                  {rows.map((route) => (
                    <li key={route.path}>
                      <a
                        className="rail-item"
                        data-depth="2"
                        href={route.path}
                        // NOT COLOUR ALONE. `aria-current` is the machine-readable half — it is what a screen
                        // reader announces — and the stylesheet marks the same link with a fill, full-strength
                        // text AND a shape, so the current page is legible without colour vision.
                        aria-current={pathname === route.path ? "page" : undefined}
                      >
                        <span className="rail-label">{route.label}</span>
                      </a>
                    </li>
                  ))}
                </ul>
              </details>
            </li>
          ),
        )}
      </ul>
    </nav>
  );
}

/**
 * PageHeader renders the page's title and its first sentence.
 *
 * With no props it reads the current route out of lib/routes.ts, which is how the layout renders it for
 * every declared page. `/login` is deliberately NOT in that list — it sits outside the session gate, so the
 * generated axe loop cannot reach it — and it passes its own, which is also why an unknown path renders
 * NOTHING rather than a blank heading.
 *
 * THE EYEBROW IS GONE (E31 shell parity). It printed the route's GROUP above the title on every screen, on
 * the argument that "a screen deep in a scroll should not require a glance across the window". Measured, the
 * reference prints no such thing — its page header is a title and one sentence, full stop — and with the rail
 * now carrying a collapsible tree whose open section IS the answer, the eyebrow was the same word twice on
 * one screen. It also read badly the moment a group became a single top-level link: "USAGE / Usage".
 */
export function PageHeader({ title, lead }: { title?: string; lead?: ReactNode }) {
  const pathname = usePathname();
  const route = CONSOLE_ROUTES.find((r) => r.path === pathname);
  const chrome = useContext(ListChromeContext)?.chrome ?? null;
  const heading = title ?? route?.label;
  const sentence = lead ?? route?.lead;
  if (heading === undefined) return null;
  return (
    <div className="page-header">
      {/* THE TITLE LINE: name, count, action — measured on the reference, where a list page reads
          `API keys ⁴` with `+ Create key` at the far right of the same line. The count and the action come
          from the page's own list through usePageList; a page with no single list simply has neither, and
          the line is the title alone exactly as it was. */}
      <div className="page-title-row">
        <h1 className="page-title" data-testid="page-title">
          {heading}
        </h1>
        {chrome?.countLabel === undefined ? null : (
          <span className="panel-count" data-testid={chrome.countTestId}>
            {chrome.countLabel}
          </span>
        )}
        {chrome?.action === undefined ? null : <div className="page-action">{chrome.action}</div>}
      </div>
      {sentence === undefined ? null : (
        <p className="page-lead" data-testid="page-lead">
          {sentence}
        </p>
      )}
    </div>
  );
}
