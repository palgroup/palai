"use client";

import { usePathname } from "next/navigation";
import { useEffect, useState, type ReactNode } from "react";

import { Select } from "@/components/ui/Select";
import { apiGet } from "@/lib/api";
import { CONSOLE_ROUTES, NAV_GROUPS } from "@/lib/routes";
import { rememberedProject, rememberProject } from "@/lib/scope";

// THE SHELL — a sidebar with grouped sections, a scope readout, and a page that says what it is.
//
// What was here: a flat top bar of twelve links in epic order, and a page title. Twelve peers is a list, not
// an architecture — nothing said which screens belong together, which one you start from, or which scope you
// were looking at. lib/routes.ts now carries a `group` per route and NAV_GROUPS carries their order, so the
// nav is DERIVED from the same table the axe sweep reads. A route in a group nobody renders would be an
// unlinked page, so the partition is asserted below rather than assumed.
//
// THE SCOPE BLOCK IS BUILT FOR A TENANT COUNT THIS DEPLOYMENT DOES NOT HAVE YET, and it says so instead of
// pretending. See <Scope /> for what it does and — more importantly — what it does not.

/** navGroupId is the DOM id a group's heading and its list share. Kept in one place so they cannot drift. */
const navGroupId = (group: string) => `nav-group-${group.toLowerCase()}`;

/**
 * Shell frames every page: the sidebar, and the <main> the skip link targets.
 *
 * `/login` gets NO sidebar, and that is a decision rather than an omission: the whole console behind that
 * door answers 401 without a session, so a nav rendered there offers twelve links that all refuse. The front
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
    <div className="app-shell">
      {/* TWO ELEMENTS, AND THE INNER ONE IS WHY. The <header> is the grid CELL — it carries the rail's fill
          and its edge, and a grid cell stretches to the row's height, so the rail reaches the bottom of a
          twelve-screen page instead of stopping at the fold. The sticky viewport-height box has to be the
          INNER one: `position: sticky` on the cell itself would make the cell 100vh tall and leave the fill
          hanging in mid-page. */}
      {/* SCOPE MOVED TO THE FOOT OF THE RAIL (E30 visual identity), and the reason is what it IS rather than
          how it looked. It is a READOUT — the org and project every screen is reading — and it was the first
          thing on every page, above the navigation, which is to say the console opened with a standing
          caveat instead of with the twelve places an operator can go. A readout belongs where a readout
          belongs: at the foot, permanently visible, and out of the way of the thing being used. */}
      <header className="sidebar">
        <div className="sidebar-inner">
          <Brand />
          <Nav />
          <Scope />
        </div>
      </header>
      <main id="main" className="content">
        <PageHeader />
        {children}
      </main>
    </div>
  );
}

/**
 * Brand is the wordmark, and it is now ONLY the wordmark.
 *
 * THE MARK IS GONE, AND IT IS THE ONE THING THIS PASS TOOK OFF ON THE WAY OUT. It was three bars at rising
 * heights with the tallest in the accent, and the comment that shipped with it described exactly what it was:
 * "a signal meter, which is what a control plane is looking at". That was the right idea drawn in the wrong
 * place. components/LaneStrip.tsx is now a signal meter that carries real frames at three scales, in the same
 * palette, twenty pixels below this — and a console cannot have two signal meters where one of them is
 * decoration. Keeping both would have been the accent problem again in miniature: a coloured thing in the
 * chrome, competing with the coloured thing that means something.
 *
 * What is left is five letters and a divider, set in IBM Plex Sans at the weight and tracking a wordmark is
 * set at rather than typeset at. That is enough; it was always the part that named the product.
 */
function Brand({ standalone = false }: { standalone?: boolean }) {
  return (
    <a className={standalone ? "brand brand-standalone" : "brand"} href="/">
      <span className="brand-word">palai</span>
      <span className="brand-kind">Console</span>
    </a>
  );
}

/**
 * scopeLabel is the row's own name, and it treats an EMPTY STRING as absent.
 *
 * `??` does not, which is the whole bug: `String(row.display_name ?? row.id)` keeps a `display_name` of ""
 * because "" is neither null nor undefined — so a deployment whose projects carry no display name rendered a
 * blank line under "SCOPE", and, when there was more than one of them, a dropdown of blank options. That is
 * what "it reads broken" was: not a styling problem, a fallback that never fired.
 */
function scopeLabel(row: ScopeRow | undefined): string {
  if (row === undefined) return "—";
  const name = String(row.display_name ?? "").trim();
  if (name !== "") return name;
  const id = String(row.id ?? "").trim();
  return id === "" ? "—" : id;
}

interface ScopeRow {
  id?: string;
  display_name?: string;
}

/**
 * Scope names the organisation and project every screen in this console is reading.
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
 * The structure is what survives into the SaaS product: this component is the one place the console decides
 * what "current scope" means, and a screen that gains a project-scoped read reads it from lib/scope.ts
 * rather than growing a picker of its own.
 *
 * A failure is SILENT and that is deliberate: on the un-configured console (no operator password) every relay
 * read is a 401, and a shell that rendered an alert on every page would put a role="alert" on twelve screens
 * that are simply closed.
 */
function Scope() {
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
        /* closed door or an unreachable upstream — the shell states nothing rather than alarming twelve pages */
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

  function choose(id: string) {
    setProject(id);
    rememberProject(id);
  }

  const org = orgs?.[0];
  const chosen = projects?.find((p) => p.id === project) ?? projects?.[0];
  const many = (projects?.length ?? 0) > 1;

  // THE BOX IS GONE AND THE READING IS NAMED. What was here was a bordered, rounded card holding four lines
  // of small text with no hierarchy — an org name, an org id, a project name, a project id — none of which
  // said which was which. It is now a two-row readout with each row's ROLE in the gutter, which costs the
  // same four lines and is the difference between a readout and a stack.
  return (
    <div className="scope" data-testid="scope">
      <p className="micro-label">Scope</p>
      <p className="scope-line">
        <span className="scope-role">Org</span>
        <span className="scope-name">{scopeLabel(org)}</span>
        <span className="scope-id">{org === undefined ? "" : String(org.id ?? "")}</span>
      </p>
      {many ? (
        <p className="scope-line">
          <span className="scope-role">Project</span>
          <Select
            className="scope-select"
            label="Project"
            testId="scope-project-select"
            value={project}
            onValueChange={choose}
            options={(projects ?? []).map((p) => ({ value: String(p.id), label: scopeLabel(p) }))}
          />
        </p>
      ) : (
        <p className="scope-line">
          <span className="scope-role">Project</span>
          <span className="scope-name">{scopeLabel(chosen)}</span>
          <span className="scope-id">{chosen === undefined ? "" : String(chosen.id ?? "")}</span>
        </p>
      )}
    </div>
  );
}

/**
 * Nav renders one link per declared route, grouped, marking the current one for machines AND for eyes.
 *
 * THE PARTITION IS CHECKED. A route whose group is not in NAV_GROUPS would be scanned by axe and linked from
 * nowhere — an unlinked page, which is exactly half of the hole lib/routes.ts exists to close — so the
 * leftover is computed and rendered as a visible group rather than dropped on the floor.
 */
export function Nav() {
  const pathname = usePathname();
  const placed = new Set<string>();
  const groups = NAV_GROUPS.map((group) => {
    const rows = CONSOLE_ROUTES.filter((r) => r.group === group);
    for (const r of rows) placed.add(r.path);
    return { group, rows };
  });
  const orphans = CONSOLE_ROUTES.filter((r) => !placed.has(r.path));

  return (
    <nav aria-label="Primary">
      {[...groups, ...(orphans.length === 0 ? [] : [{ group: "Ungrouped", rows: orphans }])].map(({ group, rows }) =>
        rows.length === 0 ? null : (
          <div className="nav-group" key={group}>
            {/* A <p>, NOT a heading. The sidebar sits before <main> in the DOM, so an <h2> here would put two
                second-level headings above the page's own <h1> on every screen — a heading outline that reads
                backwards. `aria-labelledby` gives the list its name without inventing a document level. */}
            <p className="micro-label nav-group-label" id={navGroupId(group)}>
              {group}
            </p>
            <ul aria-labelledby={navGroupId(group)}>
              {rows.map((route) => (
                <li key={route.path}>
                  <a
                    href={route.path}
                    // NOT COLOUR ALONE. `aria-current` is the machine-readable half — it is what a screen
                    // reader announces — and the stylesheet marks the same link with weight, a filled
                    // background AND a solid rail down its left edge, so the current page is legible without
                    // colour vision.
                    aria-current={pathname === route.path ? "page" : undefined}
                  >
                    {route.label}
                  </a>
                </li>
              ))}
            </ul>
          </div>
        ),
      )}
    </nav>
  );
}

/**
 * PageHeader renders the page's group, its title and its first sentence.
 *
 * With no props it reads the current route out of lib/routes.ts, which is how the layout renders it for
 * every declared page. `/login` is deliberately NOT in that list — it sits outside the session gate, so the
 * generated axe loop cannot reach it — and it passes its own, which is also why an unknown path renders
 * NOTHING rather than a blank heading.
 *
 * The eyebrow is the route's GROUP, and it is the sidebar's answer repeated where the eye already is: a
 * screen deep in a scroll should not require a glance across the window to say which part of the console it
 * belongs to.
 */
export function PageHeader({ title, lead }: { title?: string; lead?: ReactNode }) {
  const pathname = usePathname();
  const route = CONSOLE_ROUTES.find((r) => r.path === pathname);
  const heading = title ?? route?.label;
  const sentence = lead ?? route?.lead;
  if (heading === undefined) return null;
  return (
    <div className="page-header">
      {title === undefined && route !== undefined ? (
        <p className="micro-label page-eyebrow" data-testid="page-eyebrow">
          {route.group}
        </p>
      ) : null}
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
