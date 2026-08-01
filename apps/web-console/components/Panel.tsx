"use client";

import { useEffect, useRef, useState, type ReactNode } from "react";

import { usePageList } from "@/components/Chrome";
import { Select } from "@/components/ui/Select";
import { apiGet, RelayError } from "@/lib/api";

export interface Column<Row> {
  header: string;
  render: (row: Row) => ReactNode;
  /**
   * The value this column sorts on. Providing it is what makes the column SORTABLE — a column with no
   * ordering that makes sense (a set of scopes, a rendered chip) simply does not offer one.
   */
  sort?: (row: Row) => string | number;
  /**
   * A NUMERIC column: right-aligned, tabular figures, header included.
   *
   * It is the one rule this console took verbatim from a published design system — Shopify Polaris, "Numeric
   * cells and titles should be right aligned" — and it was unreachable from a caller until now, because the
   * stylesheet's `td.num` selector needs the class on the CELL and Panel writes every cell itself. A column
   * of figures that do not share a decimal position cannot be compared by eye, which is the only thing a
   * column of figures is for.
   */
  numeric?: boolean;
}

/** NameCell is the name-first row identity: the human name leads, the id stays under it, complete.
 *
 * THE ID USED TO BE THE WHOLE CELL. Projects rendered as `prj_f89cbd4f78ce3e8d51fb5ea49a8bcfff`, agents as
 * `aprof_239203512c3f88a6433ab3a976165ad8` — the console showing the operator the one part of the row they
 * cannot read, while the display name it had already fetched went unrendered. On a stack carrying eighteen
 * test fixtures and one real agent, that is the difference between a list and a wall.
 *
 * A ROW WITH NO NAME SAYS SO. The real projections do not all carry one (a knowledge base created by the API
 * may have an empty `name`), and a blank cell reads as a rendering fault rather than as a fact about the
 * record.
 */
export function NameCell({ name, id }: { name: string; id: string }) {
  const named = name.trim() !== "";
  return (
    <span className="cell-name">
      <span className="name" data-unnamed={named ? undefined : "true"}>
        {named ? name : "— unnamed"}
      </span>
      {id === "" ? null : <span className="id">{id}</span>}
    </span>
  );
}

/** ORDER AS SERVED is the default and it is not merely a placeholder: the API's own order is meaningful
 *  (newest first on every paginated collection), and a console that re-sorted on arrival would hide it. */
const AS_SERVED = "";

/** A list is given tools when it is long enough to need them. Below this, a filter box over four rows is
 *  furniture — and every control added here is a control the contrast sweep and the tab order both pay for. */
const TOOLS_FROM = 8;

// Page is the /v1 list envelope, as api/pagination.go's renderPage actually writes it: `data`, `has_more`,
// and a `next_cursor` MINTED only when has_more is true. `previous_cursor` is deliberately not here — see
// below.
interface Page<Row> {
  data?: Row[];
  has_more?: boolean;
  next_cursor?: string;
}

// Panel is the generic admin-list surface: it fetches a /v1 list through the relay on mount and renders
// a labelled table. Every admin section (organizations, projects, keys, connections, routes, secret-refs,
// agents, knowledge bases) is one Panel + a column spec — no per-endpoint boilerplate. Loading / empty /
// error states are TEXT (never color-only), so a screen reader and a colorblind user both get the state.
//
// TRUNCATION IS VISIBLE, AND THIS WAS A BUG (E25 T2, plan §3.6 D18 / feature list O12). This component used
// to read `body.data` with `has_more` absent from its type, while api/pagination.go's defaultPageLimit is 20:
// so every list here showed at most twenty rows and said nothing, and the twenty-first row did not appear to
// exist. A silent cut is a lie. Now the cut is stated in TEXT and a "load more" control continues the list
// with the server's own cursor.
//
// THERE IS NO "PREVIOUS" CONTROL, AND THAT IS A MEASUREMENT: beginList REFUSES `?before=` with a 400
// ("backward pagination is not supported", api/pagination.go:179) and renderPage never populates
// contracts.Page's `previous_cursor`. A back button would be a control that cannot work — the fixture offers
// a previous_cursor the real API never mints (DIV-SHP-004), which is exactly the trap.
//
// ponytail: forward-only, append-in-place, no page-number UI and no cursor history. The API is forward-only,
// so a page model would be inventing state the server does not have. Upgrade path: if `?before=` is ever
// implemented, keep the cursors this already carries and add a backward control THEN.
// TWO PROPS ARRIVED IN E25 T4, both because a WRITE surface changes what a list holds while the operator is
// looking at it:
//
//   reloadKey — bump it and this refetches from page one. A console that creates a resource and then shows a
//               stale list has taught the operator to reload the browser to find out whether their write
//               landed, which is how a UI stops being believed.
//   onRows    — the rows this panel actually RENDERED, handed to the page. T4's environment picker is built
//               from them rather than from a second fetch of the same collection, so the dropdown can never
//               offer something the list did not show, and whatever this panel says about truncation is
//               therefore true of the picker as well. (Measured, so nobody reads a ceiling into it that is
//               not there: GET /v1/environments does NOT go through renderPage — storage/queries/
//               environments.sql's ListEnvironments carries no LIMIT and the handler returns a plain
//               {object, data} list — so that collection is never cut. The wiring is what makes the picker
//               inherit the notice for any collection that IS paginated.)
// TWO MORE ARRIVED IN E25 T8, both because the observability reads are not all `{data: [...]}` lists:
//
//   selectRows — maps a response BODY to the rows to render, defaulting to `body.data`. GET /v1/usage
//                carries its per-meter totals under `meters` (metering/store.go summaryView) and GET
//                /v1/capabilities answers a capability -> tier MAP rather than a list at all. Without this
//                each would have needed its own component with its own loading / empty / error states —
//                three more places for a blank region to hide, on the pages added precisely because a
//                blank region is what the console showed before.
//   emptyNote  — replaces the generic "None yet." where the emptiness MEANS something. "None yet." over a
//                budget list says nothing about whether this deployment caps spending, and on a read-only
//                screen with no control to change it, a sentence that does not say so is a blank region
//                with two words on it.
// AND FOUR MORE IN E29, all so the SESSIONS screen is this component rather than a second table:
//
//   action     — the list's own primary action, in the head, at the end of the line. A collection an
//                operator can CREATE a row in has that control beside its name; a screen that offered no way
//                to make the first one is what an empty state with no action looks like.
//   tools      — extra controls in the panel's toolbar, rendered BEFORE the filter box. The session filters
//                (status, created) are server-side query parameters rather than a narrowing of what is on
//                screen, so they cannot be the generic filter and must not look like it.
//                Passing them also LIFTS THE EIGHT-ROW FLOOR: a status filter that narrowed the collection to
//                three rows would otherwise take its own control off the screen with it, which is a trap
//                rather than a saving.
//   filterLabel / filterPlaceholder / matchOn — the generic box matches the whole row as JSON, which is the
//                right default and the wrong behaviour for a list whose search is scoped to one field. A
//                caller that says which field it searches gets the box named after it, so the control's
//                promise and its behaviour are the same thing.
export function Panel<Row extends Record<string, unknown>>({
  title,
  testId,
  fetchPath,
  columns,
  note,
  reloadKey = 0,
  onRows,
  selectRows,
  emptyNote,
  action,
  tools,
  filterLabel,
  filterPlaceholder,
  matchOn,
  narrow,
  footnote,
  pageTitle = false,
}: {
  title: string;
  testId: string;
  fetchPath: string;
  columns: Column<Row>[];
  note?: ReactNode;
  /**
   * A sentence UNDER the table, about the rows it just drew.
   *
   * It exists because of a shape this console had fifteen copies of on one screen: a cell whose honest value
   * is a whole explanation. `— no revision pinned` was correct in every one of those cells and, repeated down
   * a column, it was a wall of text saying nothing — the fact is about the COLLECTION ("almost no run here
   * pinned a revision"), not about each row, and a fact about the collection belongs under it once.
   *
   * `note` is the other half of the pair and is not the same thing: a note is a standing caveat about what
   * the collection IS, printed whether or not any row was returned. A footnote is derived from the rows and
   * disappears when they stop making it true.
   */
  footnote?: ReactNode;
  reloadKey?: number;
  onRows?: (rows: Row[]) => void;
  selectRows?: (body: Record<string, unknown>) => Row[];
  emptyNote?: ReactNode;
  action?: ReactNode;
  tools?: ReactNode;
  filterLabel?: string;
  filterPlaceholder?: string;
  matchOn?: (row: Row) => string;
  /**
   * An EXTRA client-side predicate, applied with the filter box and counted the same way ("N of M rows").
   *
   * It exists for a filter the SERVER cannot do. GET /v1/sessions accepts ?status= and the created_at bounds
   * and nothing else (api/pagination.go beginList), while `agents` is an aggregate over the session's runs —
   * so an agent control either narrows what is on screen or does not exist. It narrows, and the row count
   * says so, which is the same honesty the filter box already carries.
   */
  narrow?: (row: Row) => boolean;
  /**
   * THIS PANEL *IS* THE PAGE, so its heading, its count and its action belong to the page's title line.
   *
   * MEASURED, ON EIGHT OF OUR SCREENS: `/sessions` rendered `<h1>Sessions</h1>` and then `<h2>Sessions</h2>`
   * with the count and the create button beside it, forty pixels lower, with one sentence in between. The
   * page said its own name twice. The reference has ONE title, the count badge on the same line (`API keys ⁴`)
   * and the primary action at the far right of it — components/Chrome.tsx's usePageList is the seam that
   * gets the count from here to there.
   *
   * The <h2> does not disappear from the accessibility tree, it stops being DRAWN twice: the section takes
   * `aria-label` instead of `aria-labelledby`, so the region is still named for a screen reader and the name
   * is still the same word.
   *
   * EXACTLY ONE PANEL PER PAGE MAY SET IT. A second claimant is a defect and ListChromeProvider warns rather
   * than silently choosing.
   */
  pageTitle?: boolean;
}) {
  const [rows, setRows] = useState<Row[] | null>(null);
  const [error, setError] = useState<string | null>(null);
  // truncated: the server said rows remain. cursor: the position to continue from, when it gave one.
  const [truncated, setTruncated] = useState(false);
  const [cursor, setCursor] = useState<string | null>(null);
  const [loadingMore, setLoadingMore] = useState(false);
  // THE TRAIL: the `after` value that produced each page from the first to the one on screen, in order.
  // `trail[0]` is always `null` (page one is the unparameterised read) and `trail.length` is the page
  // number. See `back()` for why this is the honest way to build a backward control against an API whose
  // backward pagination is a 400.
  const [trail, setTrail] = useState<(string | null)[]>([null]);
  // THE FILTER AND THE ORDER ARE CLIENT-SIDE, OVER THE ROWS THIS PANEL HAS ACTUALLY FETCHED, and the UI says
  // so in words. /v1 offers no `?q=` on any collection and no `?order=` on any either, so a box that claimed
  // to search the collection would be a box that silently fails to find the twenty-first row. What it does
  // instead is narrow what is on screen and state its own scope.
  const [query, setQuery] = useState("");
  const [order, setOrder] = useState(AS_SERVED);
  // A ref, so a caller passing an inline arrow does not re-trigger the fetch on every render. The effect
  // depends on the DATA inputs (path, reload signal) and nothing else.
  const onRowsRef = useRef(onRows);
  onRowsRef.current = onRows;
  // Same reason as onRows: a page passing an inline `(b) => b.meters` must not re-trigger the fetch on
  // every render. The effect depends on the DATA inputs and nothing else.
  const selectRef = useRef(selectRows);
  selectRef.current = selectRows;

  useEffect(() => {
    let live = true;
    apiGet<Page<Row>>(fetchPath)
      .then((body) => {
        if (!live) return;
        const next = selectRef.current?.(body as Record<string, unknown>) ?? body.data ?? [];
        setRows(next);
        setTruncated(body.has_more === true);
        setCursor(body.next_cursor ?? null);
        onRowsRef.current?.(next);
      })
      .catch((err: unknown) => {
        if (live) setError(err instanceof RelayError ? err.problem.detail : "failed to load");
      });
    return () => {
      live = false;
    };
  }, [fetchPath, reloadKey]);

  // --- PAGING --------------------------------------------------------------------------------------------
  //
  // IT IS A `‹ ›` PAIR NOW AND IT WAS A "Load more" BUTTON, and the change is not cosmetic — the two are
  // different models. Load-more APPENDED, so page four was 80 rows in one scroll and "which page am I on"
  // had no answer; measured on the reference, a list ends in two 32x32 arrow buttons at the bottom LEFT and
  // shows one page at a time.
  //
  // THE BACKWARD ARROW DOES NOT USE BACKWARD PAGINATION, and that distinction is the whole of why it is
  // allowed to exist. `beginList` REFUSES `?before=` with a 400 ("backward pagination is not supported",
  // api/pagination.go:179) and `renderPage` never populates `previous_cursor` — so a control that asked the
  // server to go back would be a control that cannot work, which is exactly what tests/pagination.spec.ts
  // was written to keep out of this console.
  //
  // What `back()` does instead is REPLAY: the console remembers the cursor it used to reach each page, and
  // going back re-issues that same forward request. Every request this panel makes is still `?after=` with a
  // value the SERVER minted, page one is still the unparameterised read, and `?before=` still never leaves
  // the browser. tests/pagination.spec.ts asserts all three, and the absence assertion it carries is now
  // about the WIRE (no `?before=` request) rather than about the absence of a button, because the wire is
  // the property the API's refusal is actually about.
  //
  // A REPLAY IS NOT A CACHE, deliberately: the row may have changed since, and showing a remembered copy of
  // page one would be a console that lies about the present to save a request.
  async function goto(after: string | null, nextTrail: (string | null)[]) {
    setLoadingMore(true);
    try {
      const sep = fetchPath.includes("?") ? "&" : "?";
      const url = after === null ? fetchPath : `${fetchPath}${sep}after=${encodeURIComponent(after)}`;
      const body = await apiGet<Page<Row>>(url);
      // REPLACED, not appended. Assigned outside the state updater because an updater must stay pure (React
      // may call it twice) and onRows is a side effect.
      const next = selectRef.current?.(body as Record<string, unknown>) ?? body.data ?? [];
      setRows(next);
      setTruncated(body.has_more === true);
      setCursor(body.next_cursor ?? null);
      setTrail(nextTrail);
      onRowsRef.current?.(next);
    } catch (err: unknown) {
      setError(err instanceof RelayError ? err.problem.detail : "failed to load the next page");
    } finally {
      setLoadingMore(false);
    }
  }
  /** forward continues from the SERVER's cursor and records it as this page's own `after`. */
  async function forward() {
    if (cursor === null) return;
    await goto(cursor, [...trail, cursor]);
  }
  /** back re-issues the forward request that produced the previous page. See the note above. */
  async function back() {
    if (trail.length < 2) return;
    const nextTrail = trail.slice(0, -1);
    await goto(nextTrail[nextTrail.length - 1], nextTrail);
  }

  // A PANEL THAT HAS LOADED NOTHING IS NOT THE PANEL, AND IT DOES NOT ANSWER TO THE PANEL'S NAME.
  //
  // This component used to emit `data-testid={testId}` from the first paint, before the fetch above had
  // settled — so `getByTestId("panel-api-keys")` became visible over a region holding the word "Loading…" and
  // nothing else. That is the same defect this file already refuses one line down for a different pair (a
  // filter that matched nothing is not the empty collection and does not get its testid): a handle that reads
  // as a state the thing is not in.
  //
  // IT WAS NOT A THEORY, IT WAS FOUR CALL SITES AND TWO MEASUREMENTS. Three specs already worked around it by
  // hand — observability.spec.ts twice and approval-queue.spec.ts once, each waiting for `${testId}-loading`
  // to reach count 0 after waiting for the panel, and observability.spec.ts says why in its own words: "a
  // spinner is neither of the two states and would let this pass over a panel that had rendered nothing at
  // all". The fourth site took the natural reading instead, and reveal-once.spec.ts's DOM sweep scanned a
  // document its key had not been rendered into yet — an absence proved against an empty haystack, which is
  // the one failure that spec exists to make impossible.
  //
  // AND IT REACHED THE ACCESSIBILITY EVIDENCE. lib/routes.ts requires a `readyTestId` per route precisely
  // because "axe on a page still rendering Loading… scans a spinner and reports a clean bill of health for
  // markup it never saw" — and almost every route names a PANEL as that signal:
  //   `grep -o 'readyTestId: "panel-[a-z-]*"' lib/routes.ts | wc -l` → 12 of 13 routes (2026-07-31),
  //   the exception being /runs, which names `run-button`.
  // Measured at the instant tests/a11y.spec.ts analyzes, on the fake profile: "/" scanned four loading
  // panels, /agents two, /environments one, /fleet one — and on three of those the still-loading panel WAS
  // the route's own declared readiness signal. Withholding the name is what makes that field mean what it
  // says. /fleet's remaining one is a different mechanism and is NOT closed here: panel-runner-pool-keys
  // mounts only once a pool is selected, so its fetch has not begun when the signal fires.
  //
  // THE MARKUP BELOW IS WHAT THIS COMPONENT ALREADY RENDERED WHILE PENDING — the head with its heading, the
  // note, and the same "Loading…" paragraph — so nothing moves on the screen when the rows arrive that did
  // not move before. `aria-busy` is the machine-readable half of the word.
  //
  // NO FIXED HEIGHT IS RESERVED HERE, AND THAT IS A MEASUREMENT RATHER THAN AN OMISSION. A panel's height IS
  // its row count, and the row count is exactly what has not arrived: measured on the fake profile, the same
  // component resolves to 154px with one row (panel-organizations) and 1379px with twenty (panel-agents).
  // A reserve sized for either is wrong by about 1200px for the other, and sizing for the larger replaces a
  // panel that grows with a blank region that collapses — a worse jump, and a worse one to read.
  // Settled. `rows` is still nullable to the compiler because a fetch that REJECTED leaves it null, and that
  // arm renders the error rather than a list — so the empty list is the honest reading for everything below.
  const settled: Row[] = rows ?? [];
  // The sortable columns, in the order they are declared — so the listbox reads like the table.
  const sortable = columns.filter((c) => c.sort !== undefined);
  // The order rows, flattened. `AS_SERVED` first, then two per sortable column — the same list the two
  // <option> elements per column used to produce, and the same `<index>:<direction>` value `ordered` parses.
  const orderOptions = [
    { value: AS_SERVED, label: "Order as served" },
    ...sortable.flatMap((c, i) => [
      { value: `${String(i)}:asc`, label: `${c.header} A → Z` },
      { value: `${String(i)}:desc`, label: `${c.header} Z → A` },
    ]),
  ];
  // The filter matches the row AS THE API RETURNED IT rather than as this panel rendered it: a render is a
  // ReactNode and cannot be searched, and matching only the rendered columns would silently fail to find a
  // row by a field that is on the wire but not in this panel's column list. The scope is stated below.
  const matches = (row: Row) => (matchOn?.(row) ?? JSON.stringify(row)).toLowerCase().includes(query.trim().toLowerCase());
  const kept = narrow === undefined ? settled : settled.filter(narrow);
  const filtered = query.trim() === "" ? kept : kept.filter(matches);
  const ordered =
    order === AS_SERVED
      ? filtered
      : (() => {
          const [index, direction] = order.split(":");
          const by = sortable[Number(index)]?.sort;
          if (by === undefined) return filtered;
          const sign = direction === "desc" ? -1 : 1;
          return [...filtered].sort((a, b) => {
            const va = by(a);
            const vb = by(b);
            if (typeof va === "number" && typeof vb === "number") return (va - vb) * sign;
            return String(va).localeCompare(String(vb), undefined, { numeric: true }) * sign;
          });
        })();

  const shown = ordered.length;
  const total = settled.length;
  // A caller that brought its own controls always gets the toolbar: those controls are how the collection was
  // narrowed in the first place, and hiding them under the eight-row floor would remove the only way back.
  const showTools = total >= TOOLS_FROM || tools !== undefined;

  // THE COUNT, IN THE HEADING. "How many are there" is the first question asked of every list on this
  // surface and the answer used to be "count the rows yourself". When a filter is narrowing the list it says
  // BOTH numbers, so a filter that hides everything is legible as a filter rather than as an empty
  // collection.
  const countLabel =
    error !== null
      ? undefined
      : shown === total
        ? `${String(total)} ${total === 1 ? "row" : "rows"}`
        : `${String(shown)} of ${String(total)} rows`;
  const countBadge =
    countLabel === undefined ? undefined : (
      <span className="panel-count" data-testid={`${testId}-count`}>
        {countLabel}
      </span>
    );

  // PUBLISHED TO THE PAGE'S TITLE LINE, and it is a HOOK, so it is called unconditionally — which is why
  // the whole derivation above moved up here with it, out of the settled arm. A panel that has not settled
  // publishes nothing at all: a badge reading "0 rows" over a list that is still arriving is a number the
  // operator would read as an answer, which is the same defect as a testid that appears over a spinner.
  usePageList(
    testId,
    rows === null ? null : { countLabel, countTestId: `${testId}-count`, action },
    pageTitle,
  );

  if (rows === null && error === null) {
    return (
      <section
        className="panel"
        data-testid={`${testId}-loading`}
        aria-busy="true"
        {...(pageTitle ? { "aria-label": title } : { "aria-labelledby": `${testId}-h` })}
      >
        {pageTitle ? null : (
          <div className="panel-head">
            <h2 id={`${testId}-h`}>{title}</h2>
          </div>
        )}
        {note ? <p className="muted">{note}</p> : null}
        <p className="loading">Loading…</p>
      </section>
    );
  }


  return (
    <section
      className="panel"
      data-testid={testId}
      {...(pageTitle ? { "aria-label": title } : { "aria-labelledby": `${testId}-h` })}
    >
      <div className="panel-head" data-page-title={pageTitle ? "true" : undefined}>
        {pageTitle ? null : <h2 id={`${testId}-h`}>{title}</h2>}
        {pageTitle ? null : countBadge}
        {pageTitle || action === undefined ? null : <div className="panel-action">{action}</div>}
        {showTools && error === null ? (
          <div className="panel-tools" data-wrap={action === undefined ? undefined : "true"}>
            {tools}
            <input
              type="search"
              // A visible label on every list toolbar would be six words of chrome per panel; the accessible
              // name names the LIST as well as the action, because "Filter" alone is ambiguous the moment a
              // screen carries four of these.
              aria-label={filterLabel ?? `Filter ${title}`}
              placeholder={filterPlaceholder ?? "Filter…"}
              data-testid={`${testId}-filter`}
              value={query}
              onChange={(e) => setQuery(e.target.value)}
            />
            {sortable.length === 0 ? null : (
              <Select label={`Order ${title}`} testId={`${testId}-sort`} value={order} onValueChange={setOrder} options={orderOptions} />
            )}
            {/* The scope of the box, stated. Over a collection the server cut at twenty, a filter that says
                nothing will fail to find a row that exists and look like an answer. */}
            <p className="panel-tools-note">
              {truncated
                ? `Filtering the ${String(total)} rows loaded — more remain on the server.`
                : `Filtering all ${String(total)} rows.`}
            </p>
          </div>
        ) : null}
      </div>
      {note ? <p className="muted">{note}</p> : null}
      {error !== null ? (
        <p role="alert" className="form-error" data-testid={`${testId}-error`}>
          Error: {error}
        </p>
      ) : settled.length === 0 ? (
        // A <div>, not a <p> (E29): the measured empty state is a TITLE plus a sentence saying what the
        // thing is FOR, and a title cannot be a paragraph nested in a paragraph. A caller passing a bare
        // string renders exactly as before.
        <div className="empty" data-testid={`${testId}-empty`}>
          {emptyNote ?? "None yet."}
        </div>
      ) : (
        <>
          {/* A FILTER THAT MATCHED NOTHING IS NOT AN EMPTY COLLECTION, and it does not get the empty
              collection's testid or its sentence. Conflating the two is how an operator concludes a resource
              was deleted when they have simply mistyped it. */}
          {shown === 0 ? (
            <div className="empty" data-testid={`${testId}-no-match`}>
              {query.trim() === "" ? (
                // NARROWED BY A TOOLBAR CONTROL RATHER THAN BY THE BOX, which is a state the message above
                // could not describe: it named a query, and with `narrow` in play there is none to name.
                <>No row here matches the filters above.</>
              ) : (
                <>
                  No row here matches <strong>{query}</strong>.
                </>
              )}{" "}
              {total} {total === 1 ? "row is" : "rows are"} loaded
              {truncated ? " and the server has more" : ""}; clear the filter to see them.
            </div>
          ) : (
            <table>
              <thead>
                <tr>
                  {columns.map((c) => (
                    <th key={c.header} scope="col" className={c.numeric === true ? "num" : undefined}>
                      {c.header}
                    </th>
                  ))}
                </tr>
              </thead>
              <tbody>
                {ordered.map((row, i) => (
                  <tr key={String(row.id ?? row.name ?? i)}>
                    {columns.map((c) => (
                      <td key={c.header} className={c.numeric === true ? "num" : undefined}>
                        {c.render(row)}
                      </td>
                    ))}
                  </tr>
                ))}
              </tbody>
            </table>
          )}
          {/* The collection's own footnote, printed once under the rows that make it true and only when a
              row was actually drawn — a sentence about twenty rows under an empty table is a sentence about
              nothing. */}
          {footnote === undefined || shown === 0 ? null : (
            <p className="table-more" data-testid={`${testId}-footnote`}>
              {footnote}
            </p>
          )}
          {/* THE PAGER: two arrows at the bottom left, measured. It renders only when there IS more than one
              page — a pair of dead arrows under a four-row table is chrome saying "there might be more".

              THE CUT IS STILL STATED IN WORDS beside them, which is the property the old "Load more" note
              carried and the one that must not be lost: a greyed arrow is a colour, and this console's rule
              is that a truncation is a sentence. It now says WHICH page as well, because with a replacing
              pager "20 rows" alone stops meaning "the first 20". */}
          {truncated || trail.length > 1 ? (
            <div className="pager" data-testid={`${testId}-pager`}>
              <button
                type="button"
                className="pager-arrow"
                data-testid={`${testId}-page-back`}
                onClick={() => void back()}
                disabled={loadingMore || trail.length < 2}
                aria-label={`Show the previous page of ${title}`}
              >
                <span aria-hidden="true">‹</span>
              </button>
              <button
                type="button"
                className="pager-arrow"
                data-testid={`${testId}-page-next`}
                onClick={() => void forward()}
                disabled={loadingMore || !truncated || cursor === null}
                aria-label={`Show the next page of ${title}`}
              >
                <span aria-hidden="true">›</span>
              </button>
              <p className="pager-note" data-testid={`${testId}-more`}>
                {loadingMore ? "Loading…" : `Page ${String(trail.length)} — ${String(settled.length)} rows`}
                {truncated ? ", more are available" : ", the last page"}
                {truncated && cursor === null ? ". The server returned no cursor to continue from." : "."}
              </p>
            </div>
          ) : null}
        </>
      )}
    </section>
  );
}
