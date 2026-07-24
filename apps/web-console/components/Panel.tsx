"use client";

import { useEffect, useState, type ReactNode } from "react";

import { apiGet, RelayError } from "@/lib/api";

export interface Column<Row> {
  header: string;
  render: (row: Row) => ReactNode;
}

// Panel is the generic admin-list surface: it fetches a /v1 list through the relay on mount and renders
// a labelled table. Every admin section (organizations, projects, keys, connections, routes, secret-refs,
// agents, knowledge bases) is one Panel + a column spec — no per-endpoint boilerplate. Loading / empty /
// error states are TEXT (never color-only), so a screen reader and a colorblind user both get the state.
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

  useEffect(() => {
    let live = true;
    apiGet<{ data?: Row[] }>(fetchPath)
      .then((body) => {
        if (live) setRows(body.data ?? []);
      })
      .catch((err: unknown) => {
        if (live) setError(err instanceof RelayError ? err.problem.detail : "failed to load");
      });
    return () => {
      live = false;
    };
  }, [fetchPath]);

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
      )}
    </section>
  );
}
