"use client";

import { NameCell, Panel, type Column } from "@/components/Panel";
import { CopyButton, shortId, Stamp } from "@/components/Session";

// THE MISSING LINK, AND ONLY ONE OF THREE WAS MISSING.
//
// The owner's ask was: define an MCP server in the admin panel, assign it to an agent, and have the agent
// reach it when it comes up on a Mac or a Linux box. Measured 2026-08-05, two of those three already
// worked — `/agents/[id]` has read `/mcp-connections` and written `revision-mcp-connection` for some time,
// and at run time extensions/lookup.go makes the revision's `mcp_connections` a capability CEILING while
// the bearer is redeemed from its `secret_ref` at request time.
//
// What did not exist was the screen that CREATES one. So the live stack had zero connections, the agent
// screen rendered an empty picker, and nothing anywhere said why. This page is that half.
//
// WHAT THIS SCREEN DELIBERATELY DOES NOT DO, because the industry's answer is worth refusing explicitly:
// Cursor ships a `cursor://…/mcp/install?config=<base64>` deeplink that carries an ENTIRE server
// configuration inline behind one consent modal — and that modal truncates its own argument preview.
// Research on it (2026-08-05) lands on "a UX gate, not admission control". A deeplink is an IDE's answer
// to editing JSON on disk; the definition lives on the server here, so the panel IS the install flow.

/** One row of GET /v1/mcp-connections, as extensions.Connection serialises it. */
interface ConnectionRow extends Record<string, unknown> {
  id?: string;
  name?: string;
  transport?: string;
  secret_ref?: string;
  trust_level?: string;
  disabled?: boolean;
  created_at?: string;
}

const columns: Column<ConnectionRow>[] = [
  {
    header: "Name",
    sort: (r) => String(r.name ?? ""),
    render: (r) => <NameCell name={String(r.name ?? "")} id={shortId(String(r.id ?? ""))} />,
  },
  {
    header: "Transport",
    sort: (r) => String(r.transport ?? ""),
    // stdio and http are not two flavours of the same thing: one starts a process on the machine holding
    // the run's lease, the other dials a URL. An operator deciding whether a server is safe to assign is
    // deciding mostly about this cell.
    render: (r) => <code>{String(r.transport ?? "—")}</code>,
  },
  {
    // THE REF, NEVER THE VALUE — the same asymmetry the model-connection table states, and for the same
    // reason: the credential is written server-side and readable through no route, this console included.
    header: "Credential",
    sort: (r) => String(r.secret_ref ?? ""),
    render: (r) =>
      String(r.secret_ref ?? "") === "" ? (
        <span className="cell-none">— none (unauthenticated)</span>
      ) : (
        <code>{String(r.secret_ref)}</code>
      ),
  },
  {
    // Trust is what decides whether this server's OUTPUT is announced to the model as untrusted data.
    // Everything a remote MCP server returns is a third party's text, and a tool description is text too.
    header: "Trust",
    sort: (r) => String(r.trust_level ?? ""),
    render: (r) => <span className="name">{String(r.trust_level ?? "—")}</span>,
  },
  {
    header: "Status",
    sort: (r) => (r.disabled === true ? "disabled" : "enabled"),
    // Stated in TEXT, not by colour alone — the rule Status.tsx and Panel.tsx already follow.
    render: (r) =>
      r.disabled === true ? <span className="cell-none">disabled</span> : <span className="name">enabled</span>,
  },
  {
    header: "ID",
    sort: (r) => String(r.id ?? ""),
    render: (r) => (
      <span className="cell-name">
        <code>{String(r.id ?? "")}</code>
        <CopyButton value={String(r.id ?? "")} label="connection ID" testId="copy-mcp-connection-id" />
      </span>
    ),
  },
  {
    header: "Created",
    sort: (r) => String(r.created_at ?? ""),
    render: (r) => <Stamp iso={String(r.created_at ?? "")} />,
  },
];

export default function MCPPage() {
  return (
    <Panel<ConnectionRow>
      title="MCP connections"
      testId="panel-mcp-connections"
      fetchPath="/mcp-connections"
      // THE NOTE CARRIES THE ONE FACT NO CELL CAN SHOW: a connection on this screen grants nothing by
      // itself. A run reaches a server only when the agent revision it is pinned to NAMES that connection,
      // which is done on the agent's own screen — so an operator who creates one here and expects a run to
      // pick it up would otherwise wait for something that is working exactly as designed.
      note="A connection is a definition, not a grant: a run can call a server only when its agent revision names this connection (set that on the agent's screen). The credential is a REF — the value is written server-side and readable through no route, this console included."
      columns={columns}
      // The empty state is the reason this screen shipped before its create form. A deployment with no
      // connections already renders an empty picker on the agent screen and says nothing; naming the gap is
      // the part that stops it being a silent skip.
      emptyNote="No MCP connections yet. An MCP server gives an agent tools it does not ship with — an issue tracker, a wiki, an internal API. Define one here, then name it on an agent's revision to let that agent's runs call it."
    />
  );
}
