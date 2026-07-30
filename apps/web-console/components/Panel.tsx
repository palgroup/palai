"use client";

import { useEffect, useState, type ReactNode } from "react";

import { apiGet, RelayError } from "@/lib/api";

export interface Column<Row> {
  header: string;
  render: (row: Row) => ReactNode;
}

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
export function Panel<Row extends Record<string, unknown>>({
  title,
  testId,
  fetchPath,
  columns,
  note,
}: {
  title: string;
  testId: string;
  fetchPath: string;
  columns: Column<Row>[];
  note?: string;
}) {
  const [rows, setRows] = useState<Row[] | null>(null);
  const [error, setError] = useState<string | null>(null);
  // truncated: the server said rows remain. cursor: the position to continue from, when it gave one.
  const [truncated, setTruncated] = useState(false);
  const [cursor, setCursor] = useState<string | null>(null);
  const [loadingMore, setLoadingMore] = useState(false);

  useEffect(() => {
    let live = true;
    apiGet<Page<Row>>(fetchPath)
      .then((body) => {
        if (!live) return;
        setRows(body.data ?? []);
        setTruncated(body.has_more === true);
        setCursor(body.next_cursor ?? null);
      })
      .catch((err: unknown) => {
        if (live) setError(err instanceof RelayError ? err.problem.detail : "failed to load");
      });
    return () => {
      live = false;
    };
  }, [fetchPath]);

  // loadMore continues from the SERVER's cursor. The console never computes an offset: a real cursor is an
  // HMAC'd keyset position bound to the tenant (api/pagination.go encodeCursor), so a client-side guess is
  // rejected as a foreign cursor.
  async function loadMore() {
    if (cursor === null) return;
    setLoadingMore(true);
    try {
      const sep = fetchPath.includes("?") ? "&" : "?";
      const body = await apiGet<Page<Row>>(`${fetchPath}${sep}after=${encodeURIComponent(cursor)}`);
      setRows((current) => [...(current ?? []), ...(body.data ?? [])]);
      setTruncated(body.has_more === true);
      setCursor(body.next_cursor ?? null);
    } catch (err: unknown) {
      setError(err instanceof RelayError ? err.problem.detail : "failed to load more");
    } finally {
      setLoadingMore(false);
    }
  }

  return (
    <section className="panel" data-testid={testId} aria-labelledby={`${testId}-h`}>
      <h2 id={`${testId}-h`}>{title}</h2>
      {note ? <p className="muted">{note}</p> : null}
      {error ? (
        <p role="alert" data-testid={`${testId}-error`}>
          Error: {error}
        </p>
      ) : rows === null ? (
        <p data-testid={`${testId}-loading`}>Loading…</p>
      ) : rows.length === 0 ? (
        <p data-testid={`${testId}-empty`}>None yet.</p>
      ) : (
        <>
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
              {rows.map((row, i) => (
                <tr key={String(row.id ?? row.name ?? i)}>
                  {columns.map((c) => (
                    <td key={c.header}>{c.render(row)}</td>
                  ))}
                </tr>
              ))}
            </tbody>
          </table>
          {/* The cut, in WORDS. Not a colour, not a greyed arrow, not silence. */}
          {truncated ? (
            <p data-testid={`${testId}-more`}>
              Showing {rows.length} rows — more are available.{" "}
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
