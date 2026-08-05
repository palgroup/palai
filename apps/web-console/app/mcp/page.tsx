"use client";

import { useRef, useState } from "react";

import { NameCell, Panel, type Column } from "@/components/Panel";
import { ResourceForm } from "@/components/ResourceForm";
import { SecretField, takeSecret } from "@/components/SecretField";
import { CopyButton, shortId, Stamp } from "@/components/Session";
import { Button } from "@/components/ui/Button";
import { apiSend, RelayError } from "@/lib/api";

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

function connectionColumns(
  onDiscover: (id: string) => void,
  discovering: string,
): Column<ConnectionRow>[] {
  return [
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
    {
      // THE ACTION THAT DIALS. It sits in the row rather than above the table because it acts on ONE
      // connection, and an operator with several needs to see which one they are about to exercise.
      header: "Tools",
      render: (r) => (
        <Button
          variant="secondary"
          onClick={() => onDiscover(String(r.id ?? ""))}
          disabled={discovering !== ""}
          testId="mcp-discover-button"
        >
          {discovering === String(r.id ?? "") ? "Discovering…" : "Discover tools"}
        </Button>
      ),
    },
  ];
}

/** detail pulls the server's own sentence out of a relay error, falling back to `fallback`. */
function detail(err: unknown, fallback: string): string {
  if (err instanceof RelayError && err.problem.detail !== "") return err.problem.detail;
  return fallback;
}

export default function MCPPage() {
  const [name, setName] = useState("");
  const [url, setUrl] = useState("");
  const [secretRefName, setSecretRefName] = useState("");
  // THE CREDENTIAL IS A DOM NODE AND NOTHING ELSE. No useState, deliberately — see SecretField's header.
  const secretRef = useRef<HTMLInputElement | null>(null);
  const [createError, setCreateError] = useState("");
  const [createStatus, setCreateStatus] = useState("");
  const [creating, setCreating] = useState(false);
  const [reloadKey, setReloadKey] = useState(0);

  // DISCOVERY IS A SEPARATE, DELIBERATE ACT — it dials the server. Creating a connection writes a row and
  // reaches nothing; this is the first moment the URL and the credential are actually exercised, which is
  // why its verdict is worth a line of its own rather than being folded into "created".
  const [discovering, setDiscovering] = useState("");
  const [discoverStatus, setDiscoverStatus] = useState("");
  const [discoverError, setDiscoverError] = useState("");

  async function discover(id: string) {
    setDiscovering(id);
    setDiscoverStatus("Asking the server what it offers…");
    setDiscoverError("");
    try {
      const body = await apiSend<{ new_revisions?: string[]; unchanged?: string[]; rejected?: string[] }>(
        "POST",
        `/mcp-connections/${encodeURIComponent(id)}/discover`,
      );
      const fresh = body.new_revisions ?? [];
      const same = body.unchanged ?? [];
      const refused = body.rejected ?? [];
      // THE REJECTED ARE NAMED, and they are the reason this reports three numbers instead of one. A tool
      // the platform refused is not a tool the operator can assign, and a summary that only counted
      // successes would leave them looking for it on the agent screen.
      setDiscoverStatus(
        `${fresh.length} new, ${same.length} unchanged, ${refused.length} refused.` +
          (fresh.length > 0 ? ` New: ${fresh.join(", ")}.` : "") +
          (refused.length > 0 ? ` Refused: ${refused.join(", ")} — these cannot be assigned.` : "") +
          " A discovered tool still reaches a run only through an agent revision that names this connection.",
      );
      setReloadKey((n) => n + 1);
    } catch (err: unknown) {
      // The dial is where a wrong URL, an unreachable host or a rejected credential ACTUALLY surfaces, so
      // the server's own sentence is carried rather than replaced with a generic failure.
      setDiscoverError(detail(err, "the server could not be reached"));
      setDiscoverStatus("");
    } finally {
      setDiscovering("");
    }
  }

  // createConnection seals the credential, then names it — two calls reported SEPARATELY because they fail
  // separately. A refused secret write is a name problem; a refused connection is a URL problem. One merged
  // "could not create" sends an operator to re-read the wrong field.
  async function createConnection() {
    setCreating(true);
    setCreateError("");
    setCreateStatus("");

    // Read and CLEAR in one call, before any await: the value exists as one local for the duration of one
    // fetch and is copied nowhere — not into state, not into a status message, not into a log.
    const value = takeSecret(secretRef);
    const wantsCredential = value !== "" || secretRefName !== "";
    if (wantsCredential && (value === "" || secretRefName === "")) {
      setCreateError(
        "a credential needs BOTH a ref name and a value — the name is what the connection stores, the value is what is sealed behind it.",
      );
      setCreating(false);
      return;
    }
    if (wantsCredential) {
      try {
        await apiSend<{ name?: string; version?: number }>("POST", "/secret-refs", { name: secretRefName, value });
      } catch (err: unknown) {
        setCreateError(`the credential could not be sealed: ${detail(err, "the secret ref was refused")}`);
        setCreating(false);
        return;
      }
    }

    try {
      const body = await apiSend<ConnectionRow>("POST", "/mcp-connections", {
        name,
        // HTTP ONLY, and the omission is the decision. A stdio connection starts a PROCESS on the machine
        // holding the run's lease — a different threat surface, and one the ecosystem is moving off:
        // Anthropic names streamable HTTP the recommended remote transport and marks SSE deprecated.
        // Existing stdio rows still list and still work; this form does not mint new ones.
        transport: "http",
        config: { url },
        ...(wantsCredential ? { secret_ref: secretRefName } : {}),
      });
      setCreateStatus(
        `Connection ${String(body.id ?? "?")} now points at ${url}` +
          (wantsCredential ? ` with the credential sealed under ${secretRefName}` : " with no credential") +
          ". It grants nothing yet: name it on an agent's revision to let that agent's runs call it.",
      );
      setName("");
      setUrl("");
      setSecretRefName("");
      setReloadKey((n) => n + 1);
    } catch (err: unknown) {
      // THE CREDENTIAL SURVIVES THIS FAILURE and the operator is told so. The secret was sealed before the
      // connection was refused, so the bytes now sit under a name that binds nothing — reporting only
      // "creation failed" would leave a live credential the operator does not know exists.
      setCreateError(
        `${detail(err, "the connection was refused")}` +
          (wantsCredential
            ? ` — the credential was already sealed under ${secretRefName}, so fix the field above and submit again with the same ref name (it will be versioned, not duplicated).`
            : ""),
      );
    } finally {
      setCreating(false);
    }
  }

  return (
    <>
      <ResourceForm
        title="Add an MCP server"
        testId="mcp-create-form"
        caveat={{
          summary: "what this does and does not grant",
          body: (
            <p>
              A connection is a <strong>definition</strong>, not a grant. A run reaches this server only when
              the agent revision it is pinned to names this connection — set that on the agent&apos;s own
              screen. Until then it is configuration nothing dispatches to.
            </p>
          ),
        }}
        fields={[
          {
            name: "mcp-name",
            label: "Name",
            required: true,
            value: name,
            onChange: setName,
            hint: "What this server is called here. It also namespaces its tools, so a `search` tool from `jira` reaches the model as `jira__search` and cannot collide with another server's.",
            testId: "mcp-name-input",
          },
          {
            name: "mcp-url",
            label: "URL",
            required: true,
            value: url,
            onChange: setUrl,
            hint: "The server's streamable-HTTP endpoint, e.g. https://mcp.example.com/mcp. It is vetted before the row exists: an internal address, an http downgrade, or a URL carrying a credential is refused.",
            testId: "mcp-url-input",
          },
          {
            name: "mcp-secret-ref",
            label: "Credential ref name",
            value: secretRefName,
            onChange: setSecretRefName,
            hint: "Leave both this and the field below empty for a server that needs no credential. This name — never the value — is what the connection stores and every screen shows.",
            testId: "mcp-secret-ref-input",
          },
        ]}
        submitLabel="Add server"
        submittingLabel="Adding…"
        submitTestId="mcp-create-button"
        submitting={creating}
        error={createError}
        status={createStatus}
        onSubmit={createConnection}
      >
        <SecretField
          inputRef={secretRef}
          id="field-mcp-secret"
          label="Credential"
          testId="mcp-secret-input"
          hint="The bearer token this server expects, if it expects one. It is sealed server-side and read back by no route, this console included. The field clears when you submit."
        />
      </ResourceForm>

      {discoverStatus === "" ? null : (
        <p className="form-status" data-testid="mcp-discover-status">
          <span className="glyph" aria-hidden="true">
            ✔
          </span>{" "}
          {discoverStatus}
        </p>
      )}
      {discoverError === "" ? null : (
        <p className="form-error" role="alert" data-testid="mcp-discover-error">
          {discoverError}
        </p>
      )}

      <Panel<ConnectionRow>
        title="MCP connections"
        testId="panel-mcp-connections"
        fetchPath="/mcp-connections"
        reloadKey={reloadKey}
        // THE NOTE CARRIES THE ONE FACT NO CELL CAN SHOW: a connection grants nothing by itself. An operator
        // who creates one here and expects a run to pick it up would otherwise be waiting for something
        // working exactly as designed.
        note="A connection is a definition, not a grant: a run can call a server only when its agent revision names this connection (set that on the agent's screen). The credential is a REF — the value is written server-side and readable through no route, this console included."
        columns={connectionColumns(discover, discovering)}
        emptyNote="No MCP connections yet. An MCP server gives an agent tools it does not ship with — an issue tracker, a wiki, an internal API. Define one above, then name it on an agent's revision to let that agent's runs call it."
      />
    </>
  );
}
