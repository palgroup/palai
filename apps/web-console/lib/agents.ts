// The agent surface's row shapes and the one derivation both agent screens make from them.
//
// WHAT THE LIST ACTUALLY CARRIES, measured rather than assumed — and it is the reason this file exists:
//
//   grep -n '"id": it.ID, "object": "agent", "name": it.Name' apps/control-plane/internal/store/agents.go
//     → 2 hits (2026-07-31): GetAgentProfile and ListAgentProfiles
//
// Three fields. `created_at` IS selected by storage/queries/agents.sql's ListAgentProfiles and IS carried in
// api.ListRow — and api/pagination.go's renderPage puts only `row.Body` on the wire (`page.Data[i] =
// row.Body`), so the timestamp is used to mint a cursor and is never serialised. There is therefore NO
// Created column and NO Last-updated column on this console's agents table, and their absence is a fact
// about the API rather than a gap in this screen. The reference console has both; ours cannot, and inventing
// one from `id` ordering would be the exact shape of claim this tree keeps finding in its own code.
//
// SO THE LINEAGE COLUMNS COME FROM A SECOND READ, one per row, and that cost is stated where it is paid
// (app/agents/page.tsx). A profile is a NAME; whether a run can be pinned to it is a fact about its
// REVISIONS, which live behind GET /v1/agents/{id}/revisions. A table that showed only what the list
// endpoint answers would be two columns — an id nobody can read and a name — which is the "list pretending
// to be a table" this pass exists to remove.

/** AgentRow is GET /v1/agents' row, field for field. */
export interface AgentRow extends Record<string, unknown> {
  id?: string;
  name?: string;
}

/** AgentRevision is GET /v1/agents/{id}/revisions' row, field for field (store/agents.go ListAgentRevisions). */
export interface AgentRevision extends Record<string, unknown> {
  id?: string;
  revision_number?: number;
  model?: string;
  tools?: string[] | null;
  tool_sets?: string[] | null;
  mcp_connections?: string[] | null;
  environment?: string;
  instructions?: string;
  status?: string;
}

/**
 * Lineage is what a row's revisions say about the row, and every field is derived from a revision that was
 * actually returned.
 *
 * `state` is deliberately three words and not two. "published" and "draft" describe a REVISION; a LINEAGE can
 * also be in a state no revision is in — created and never revised — and that is the state a freshly created
 * agent is in for as long as it takes to write one. Folding it into "draft" would say a draft exists.
 */
export interface Lineage {
  count: number;
  /** The newest revision, published or not. The list is served newest-first. */
  newest: AgentRevision | null;
  /** The newest PUBLISHED revision — the only kind a run can be pinned to. */
  published: AgentRevision | null;
  state: "published" | "draft only" | "no revisions";
  /** The server said there are more than this page holds. */
  truncated: boolean;
}

/**
 * lineageOf reads a revision page into the two facts an operator asks of an agent: can a run be pinned to it,
 * and what does the newest revision run.
 *
 * ORDER IS THE SERVER'S. storage/queries/agents.sql's ListAgentRevisions is `ORDER BY created_at DESC, id
 * DESC` — newest first — so `rows[0]` is the newest and the first published row is the newest published one.
 * Nothing here re-sorts: a client that re-derived an order the server already has would be a second opinion
 * about which revision is current, and a run is pinned by id rather than by whichever one a console called
 * newest.
 */
export function lineageOf(rows: AgentRevision[], truncated = false): Lineage {
  const published = rows.find((r) => r.status === "published") ?? null;
  return {
    count: rows.length,
    newest: rows[0] ?? null,
    published,
    state: published !== null ? "published" : rows.length > 0 ? "draft only" : "no revisions",
    truncated,
  };
}

/** modelOf is the revision's model as a cell: an empty string means "inherit", which is not the same as none. */
export function modelOf(revision: AgentRevision | null): string {
  if (revision === null) return "—";
  const model = String(revision.model ?? "");
  return model === "" ? "— inherited" : model;
}

/** revisionLabel is `#3` — the number an operator reads, from the field the API sends. */
export function revisionLabel(revision: AgentRevision | null): string {
  if (revision === null) return "—";
  return revision.revision_number === undefined ? String(revision.id ?? "—") : `#${String(revision.revision_number)}`;
}
