"use client";

import { Fragment, useEffect, useRef, useState, type ReactNode } from "react";

import { apiGet, RelayError } from "@/lib/api";

export interface Column<Row> {
  header: string;
  render: (row: Row) => ReactNode;
  /**
   * The value this column sorts on. Providing it is what makes the column SORTABLE — a column with no
   * ordering that makes sense (a set of scopes, a rendered chip) simply does not offer one.
   */
  sort?: (row: Row) => string | number;
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
}: {
  title: string;
  testId: string;
  fetchPath: string;
  columns: Column<Row>[];
  note?: ReactNode;
  reloadKey?: number;
  onRows?: (rows: Row[]) => void;
  selectRows?: (body: Record<string, unknown>) => Row[];
  emptyNote?: ReactNode;
}) {
  const [rows, setRows] = useState<Row[] | null>(null);
  const [error, setError] = useState<string | null>(null);
  // truncated: the server said rows remain. cursor: the position to continue from, when it gave one.
  const [truncated, setTruncated] = useState(false);
  const [cursor, setCursor] = useState<string | null>(null);
  const [loadingMore, setLoadingMore] = useState(false);
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

  // loadMore continues from the SERVER's cursor. The console never computes an offset: a real cursor is an
  // HMAC'd keyset position bound to the tenant (api/pagination.go encodeCursor), so a client-side guess is
  // rejected as a foreign cursor.
  async function loadMore() {
    if (cursor === null) return;
    setLoadingMore(true);
    try {
      const sep = fetchPath.includes("?") ? "&" : "?";
      const body = await apiGet<Page<Row>>(`${fetchPath}${sep}after=${encodeURIComponent(cursor)}`);
      // Appended outside the state updater, not inside it: an updater must stay pure (React may call it
      // twice), and onRows is a side effect. `rows` is current here — loadMore only runs from a click, behind
      // the loadingMore guard.
      const next = [...(rows ?? []), ...(selectRef.current?.(body as Record<string, unknown>) ?? body.data ?? [])];
      setRows(next);
      setTruncated(body.has_more === true);
      setCursor(body.next_cursor ?? null);
      onRowsRef.current?.(next);
    } catch (err: unknown) {
      setError(err instanceof RelayError ? err.problem.detail : "failed to load more");
    } finally {
      setLoadingMore(false);
    }
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
  if (rows === null && error === null) {
    return (
      <section className="panel" data-testid={`${testId}-loading`} aria-labelledby={`${testId}-h`} aria-busy="true">
        <div className="panel-head">
          <h2 id={`${testId}-h`}>{title}</h2>
        </div>
        {note ? <p className="muted">{note}</p> : null}
        <p className="loading">Loading…</p>
      </section>
    );
  }

  // Settled. `rows` is still nullable to the compiler because a fetch that REJECTED leaves it null, and that
  // arm renders the error rather than a list — so the empty list is the honest reading for everything below.
  const settled: Row[] = rows ?? [];
  // The sortable columns, in the order they are declared — so the select reads like the table.
  const sortable = columns.filter((c) => c.sort !== undefined);
  // The filter matches the row AS THE API RETURNED IT rather than as this panel rendered it: a render is a
  // ReactNode and cannot be searched, and matching only the rendered columns would silently fail to find a
  // row by a field that is on the wire but not in this panel's column list. The scope is stated below.
  const matches = (row: Row) => JSON.stringify(row).toLowerCase().includes(query.trim().toLowerCase());
  const filtered = query.trim() === "" ? settled : settled.filter(matches);
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
  const tools = total >= TOOLS_FROM;

  return (
    <section className="panel" data-testid={testId} aria-labelledby={`${testId}-h`}>
      <div className="panel-head">
        <h2 id={`${testId}-h`}>{title}</h2>
        {/* THE COUNT, IN THE HEADING. "How many are there" is the first question asked of every list on this
            surface and the answer used to be "count the rows yourself". When a filter is narrowing the list
            it says BOTH numbers, so a filter that hides everything is legible as a filter rather than as an
            empty collection. */}
        {error !== null ? null : (
          <span className="panel-count" data-testid={`${testId}-count`}>
            {shown === total ? `${String(total)} ${total === 1 ? "row" : "rows"}` : `${String(shown)} of ${String(total)} rows`}
          </span>
        )}
        {tools && error === null ? (
          <div className="panel-tools">
            <input
              type="search"
              // A visible label on every list toolbar would be six words of chrome per panel; the accessible
              // name names the LIST as well as the action, because "Filter" alone is ambiguous the moment a
              // screen carries four of these.
              aria-label={`Filter ${title}`}
              placeholder="Filter…"
              data-testid={`${testId}-filter`}
              value={query}
              onChange={(e) => setQuery(e.target.value)}
            />
            {sortable.length === 0 ? null : (
              <select aria-label={`Order ${title}`} data-testid={`${testId}-sort`} value={order} onChange={(e) => setOrder(e.target.value)}>
                <option value={AS_SERVED}>Order as served</option>
                {sortable.map((c, i) => (
                  <Fragment key={c.header}>
                    <option value={`${String(i)}:asc`}>{c.header} A → Z</option>
                    <option value={`${String(i)}:desc`}>{c.header} Z → A</option>
                  </Fragment>
                ))}
              </select>
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
        <p className="empty" data-testid={`${testId}-empty`}>{emptyNote ?? "None yet."}</p>
      ) : (
        <>
          {/* A FILTER THAT MATCHED NOTHING IS NOT AN EMPTY COLLECTION, and it does not get the empty
              collection's testid or its sentence. Conflating the two is how an operator concludes a resource
              was deleted when they have simply mistyped it. */}
          {shown === 0 ? (
            <p className="empty" data-testid={`${testId}-no-match`}>
              No row here matches <strong>{query}</strong>. {total} {total === 1 ? "row is" : "rows are"} loaded
              {truncated ? " and the server has more" : ""}; clear the filter to see them.
            </p>
          ) : (
            <table>
              <thead>
                <tr>
                  {columns.map((c) => (
                    <th key={c.header} scope="col">
                      {c.header}
                    </th>
                  ))}
                </tr>
              </thead>
              <tbody>
                {ordered.map((row, i) => (
                  <tr key={String(row.id ?? row.name ?? i)}>
                    {columns.map((c) => (
                      <td key={c.header}>{c.render(row)}</td>
                    ))}
                  </tr>
                ))}
              </tbody>
            </table>
          )}
          {/* The cut, in WORDS. Not a colour, not a greyed arrow, not silence. */}
          {truncated ? (
            <p className="table-more" data-testid={`${testId}-more`}>
              Showing {settled.length} rows — more are available.{" "}
              {cursor === null ? "The server returned no cursor to continue from." : null}
            </p>
          ) : null}
          {truncated && cursor !== null ? (
            <button type="button" data-testid={`${testId}-load-more`} onClick={() => void loadMore()} disabled={loadingMore}>
              {loadingMore ? "Loading…" : "Load more"}
            </button>
          ) : null}
        </>
      )}
    </section>
  );
}
