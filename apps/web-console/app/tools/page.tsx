"use client";

import { useEffect, useState } from "react";

import { Button } from "@/components/ui/Button";
import { McpCatalogue } from "@/components/McpCatalogue";
import { Panel } from "@/components/Panel";
import { Picker } from "@/components/Picker";
import { ResourceForm } from "@/components/ResourceForm";
import { RevisePublish, type Revision } from "@/components/RevisePublish";
import { apiGet, apiSend, RelayError } from "@/lib/api";

// THE MCP CONNECTION AND TOOL REGISTRATION SCREEN (E25 T7, plan §T7).
//
// THIS IS A REGISTRATION SCREEN. IT IS NOT AN APPROVAL SCREEN — and the difference is not a matter of
// emphasis, it is a difference in what is on the wire (plan §3.6 D23):
//
//   THIS screen decides, ONCE PER TOOL and before any run exists, whether a tool is advertised to a model
//   at all and whether calling it will park for a human. The untrusted `description` an MCP server wrote IS
//   shown here, because it is precisely what is being decided about: those words enter a model's context.
//
//   THE APPROVAL SCREEN is /approvals (E25 T5), and it answers a DIFFERENT question at a different time —
//   "this specific call, with these specific arguments, right now". The server's `description` is not on
//   that surface at all: api/approvals.go's PendingApproval carries six ledger fields and four screen
//   fields, and its own comment says "WHAT IS NOT HERE is as deliberate as what is: no MCP `description`,
//   no server-supplied `title`, no model prose outside the arguments." The one human sentence there is the
//   `approval_label` an operator types HERE, which is why this form asks for it.
//
// So neither screen is missing anything. A reader who expects a description on /approvals is looking for a
// field that was deliberately kept off the wire; a reader who expects this page to be an approval queue is
// looking for a decision that has already been made by the time a run parks.
//
// EVERY BYTE OF A DESCRIPTION IS AN ATTACKER'S TEXT. It is rendered as TEXT: React escaping, no markup
// interpreted, no link made clickable, and `dangerouslySetInnerHTML` nowhere in this console (a token scan
// of every .tsx proves that rather than a promise — tests/approval-queue.spec.ts). A fixture description
// carrying a <script> and an <a> is driven through this page in tests/mcp-tools.spec.ts, and the assertion
// is that no element and no navigation came of it.
//
// FOUR CEILINGS, ALL ON THE SCREEN, because each is something an operator would otherwise assume:
//   1. TWO READ ROUTES ARE NOT AN APPROVAL FLOW. Nothing records WHO published a revision. The decision is
//      immutable and visible, but it is not attributed.
//   2. `discover` DIALS A REAL SERVER. On the deterministic profile that is a fixture (§6 leg 3).
//   3. NO DELETE AND NO PATCH for an MCP connection. This console registers and reads; it does not correct.
//   4. HIL-P5 STANDS: publishing a write tool WITHOUT `approval_required` silently skips the gate. The
//      console makes that VISIBLE without preventing it — see the note on the publish control.

interface ConnectionRow extends Record<string, unknown> {
  id?: string;
  name?: string;
  transport?: string;
  trust_level?: string;
  disabled?: boolean;
}

interface ToolRow extends Record<string, unknown> {
  id?: string;
  canonical_name?: string;
  model_visible_name?: string;
}

interface SecretRefRow {
  name?: string;
  version?: number;
}

interface DiscoveryResult {
  new_revisions?: string[];
  unchanged?: string[];
  rejected?: string[];
}

/** One operator answer per revision: the gate, and the sentence a human reads at 2am. */
interface Declaration {
  required: boolean;
  label: string;
}

const detail = (err: unknown, fallback: string) => (err instanceof RelayError ? err.problem.detail : fallback);

// THE DEFAULT IS ON, FOR EVERY TOOL, AND THAT IS A CORRECTION TO THE PLAN RATHER THAN AN OVERSHOOT.
// §T7 asks for "approval_required defaulted ON for a WRITE tool" — but this console CANNOT KNOW which tool
// is a write tool, and that is not a gap in the console, it is the whole reason HIL-P5 exists. The MCP
// specification says verbatim that "clients MUST consider tool annotations to be untrusted unless they come
// from trusted servers", our client does not decode `annotations` at all, and a server declaring its own
// tool harmless is exactly the claim that cannot be taken on faith. A console that guessed would be
// classifying on the strength of a name.
//
// So the default is ON for everything, which is the only implementable form of the plan's requirement and a
// strict superset of it: a write tool arrives gated, and UN-GATING is the deliberate click — on a read tool
// too, where it is correct and cheap.
const DEFAULT_DECLARATION: Declaration = { required: true, label: "" };

export default function ToolsPage() {
  const [reloadConnections, setReloadConnections] = useState(0);
  const [reloadTools, setReloadTools] = useState(0);

  const [connections, setConnections] = useState<ConnectionRow[]>([]);
  const [tools, setTools] = useState<ToolRow[]>([]);

  // --- register a connection -----------------------------------------------------------------------
  const [name, setName] = useState("");
  const [url, setUrl] = useState("");
  const [secretRef, setSecretRef] = useState("");
  const [registerError, setRegisterError] = useState("");
  const [registerStatus, setRegisterStatus] = useState("");
  const [registering, setRegistering] = useState(false);

  // --- discover ------------------------------------------------------------------------------------
  const [discoverTarget, setDiscoverTarget] = useState("");
  const [discovering, setDiscovering] = useState(false);
  const [discoverError, setDiscoverError] = useState("");
  const [discovered, setDiscovered] = useState<DiscoveryResult | null>(null);

  // --- approve (publish) ---------------------------------------------------------------------------
  const [selectedTool, setSelectedTool] = useState("");
  const [declarations, setDeclarations] = useState<Record<string, Declaration>>({});

  // --- pin into a set ------------------------------------------------------------------------------
  const [toolSetName, setToolSetName] = useState("");
  const [pins, setPins] = useState<string[]>([]);
  const [contentsOf, setContentsOf] = useState<{ id: string; pins: string[]; status: string } | null>(null);
  const [contentsError, setContentsError] = useState("");

  const [refs, setRefs] = useState<SecretRefRow[]>([]);
  useEffect(() => {
    let live = true;
    apiGet<{ data?: SecretRefRow[] }>("/secret-refs")
      .then((body) => {
        if (live) setRefs(body.data ?? []);
      })
      .catch(() => {
        // An unreadable list leaves the picker absent, which renders the "create one first" note. An
        // operator who cannot read secret refs cannot point a connection at one either.
      });
    return () => {
      live = false;
    };
  }, []);

  const declarationFor = (id: string): Declaration => declarations[id] ?? DEFAULT_DECLARATION;
  const setDeclaration = (id: string, patch: Partial<Declaration>) =>
    setDeclarations((current) => ({ ...current, [id]: { ...declarationFor(id), ...patch } }));

  async function register() {
    setRegistering(true);
    setRegisterError("");
    setRegisterStatus("");
    try {
      const body = await apiSend<ConnectionRow>("POST", "/mcp-connections", {
        name,
        transport: "http",
        config: { url },
        // Sent only when one was CHOSEN. A public MCP server genuinely needs no credential, and an empty
        // string is a value the API would have to interpret.
        ...(secretRef === "" ? {} : { secret_ref: secretRef }),
      });
      setRegisterStatus(
        `Registered ${String(body.name ?? name)} as ${String(body.id ?? "?")}. Nothing has been dialled yet — ` +
          "Discover below is the first call that reaches the server.",
      );
      setReloadConnections((n) => n + 1);
      if (typeof body.id === "string" && body.id !== "") setDiscoverTarget(body.id);
    } catch (err: unknown) {
      setRegisterError(detail(err, "the MCP connection could not be registered"));
    } finally {
      setRegistering(false);
    }
  }

  async function discover() {
    setDiscovering(true);
    setDiscoverError("");
    setDiscovered(null);
    try {
      const body = await apiSend<DiscoveryResult>("POST", `/mcp-connections/${encodeURIComponent(discoverTarget)}/discover`);
      setDiscovered(body);
      setReloadTools((n) => n + 1);
    } catch (err: unknown) {
      setDiscoverError(detail(err, "the connection could not be discovered"));
    } finally {
      setDiscovering(false);
    }
  }

  async function showContents(revision: Revision) {
    setContentsError("");
    setContentsOf(null);
    const id = String(revision.id ?? "");
    const set = String(revision.set ?? "");
    try {
      const body = await apiGet<{ tools?: { tool_revision_id?: string }[]; status?: string }>(
        `/tool-sets/${encodeURIComponent(set)}/revisions/${encodeURIComponent(id)}`,
      );
      setContentsOf({
        id,
        status: String(body.status ?? ""),
        pins: (body.tools ?? []).map((p) => String(p.tool_revision_id ?? "")),
      });
    } catch (err: unknown) {
      setContentsError(detail(err, "the set's contents could not be read"));
    }
  }

  const connectionOptions = connections
    .filter((c) => typeof c.id === "string" && c.id !== "")
    .map((c) => ({ value: String(c.id), label: `${String(c.name ?? c.id)} (${String(c.id)})` }));
  const toolOptions = tools
    .filter((t) => typeof t.id === "string" && t.id !== "")
    .map((t) => ({ value: String(t.id), label: `${String(t.canonical_name ?? t.id)} (${String(t.id)})` }));
  const refOptions = refs
    .filter((r) => typeof r.name === "string" && r.name !== "")
    .map((r) => ({ value: String(r.name), label: `${String(r.name)}${r.version === undefined ? "" : ` (version ${String(r.version)})`}` }));

  return (
    <>
      <p className="muted" data-testid="tools-registration-note">
        <strong>This is a registration screen, not the approval queue.</strong> Here you decide, once per
        tool and before any run exists, whether a tool is advertised to a model at all and whether calling it
        will stop for a human. The <strong>server&apos;s own description is shown here on purpose</strong>{" "}
        — it is what you are deciding about, because those words go into a model&apos;s context. The{" "}
        <a href="/approvals">approval queue</a>{" "}
        answers a different question later (this call, these
        arguments, now) and the server&apos;s description is <strong>not on that screen at all</strong>; the
        only human sentence there is the label you write below.
      </p>
      {/* The registration note above says WHICH decision this screen is, which is the thing an operator gets
          wrong; these two are standing properties of the surface. Collapsed, not removed. */}
      <details className="notes" data-testid="tools-standing-notes">
        <summary>The descriptions below are untrusted text, and nothing here can be edited or removed</summary>
      <p className="muted" data-testid="tools-untrusted-note">
        <strong>Every description below was written by the upstream server, and it is untrusted text.</strong>{" "}
        It is displayed exactly as bytes — no markup is interpreted and no link is made clickable — so a
        description containing HTML shows you the HTML. Read it as evidence about what the tool claims to do,
        never as an instruction.
      </p>
      <p className="muted" data-testid="tools-correction-note">
        <strong>There is no way to change or remove a connection from here</strong>: the API mounts a create,
        two reads and a discover, and no PATCH or DELETE. Publishing a revision is likewise permanent — a
        gate can be added or removed only by publishing a NEW revision and re-pinning the set, which is what
        makes an approved tool auditable. And <strong>nothing records who published</strong>: the decision is
        immutable and visible, but it is not attributed to a person.
      </p>
      </details>

      <Panel<ConnectionRow>
        title="MCP connections"
        testId="panel-mcp-connections"
        fetchPath="/mcp-connections"
        reloadKey={reloadConnections}
        onRows={setConnections}
        note="An admin-registered upstream MCP server. The credential is NOT here and cannot be: a connection carries a secret_ref HANDLE, and the list projection has no field for a value."
        columns={[
          { header: "ID", render: (r) => <code>{String(r.id ?? "")}</code> },
          { header: "Name", render: (r) => String(r.name ?? "") },
          { header: "Transport", render: (r) => String(r.transport ?? "") },
          { header: "Trust level", render: (r) => String(r.trust_level ?? "") },
          { header: "Disabled", render: (r) => (r.disabled === true ? "yes" : "no") },
        ]}
      />

      <ResourceForm
        title="Register an MCP connection"
        testId="mcp-connection"
        note={
          <>
            HTTP transport only on this screen. A <code>stdio</code> connection needs a pinned image digest
            and an argv, which is a deployment decision rather than a form field — register one with the API
            if you need it. Registering dials nothing: <strong>Discover</strong> is the first call that
            reaches the server.
            <br />
            {/* THE DIRECTORY, so the first question on this form is "which server" rather than "what is its
                URL". It also answers the question this form cannot: whether the server this deployment is
                about to be pointed at can be authenticated at all. */}
            Not sure of the URL, or whether a server can be connected here at all?{" "}
            <McpCatalogue
              onPick={(entry) => {
                setName(entry.id);
                setUrl(entry.url);
                setRegisterError("");
                setRegisterStatus(
                  `Filled in ${entry.name}. Nothing has been registered yet — check the name and the ` +
                    "credential, then submit.",
                );
              }}
            />
          </>
        }
        fields={[
          {
            name: "mcp-name",
            label: "Name",
            required: true,
            value: name,
            onChange: setName,
            hint: "The connection namespace. A discovered tool becomes mcp.<name>.<tool> and is model-visible as <name>__<tool>, so two servers' identically-named tools never collide.",
            testId: "mcp-name-input",
          },
          {
            name: "mcp-url",
            label: "Server URL",
            required: true,
            value: url,
            onChange: setUrl,
            hint: "https://… — checked against the egress gate at registration and again before every dial. A URL that resolves to a private address is refused.",
            testId: "mcp-url-input",
          },
          {
            name: "mcp-secret-ref",
            label: "Secret ref (a credential HANDLE)",
            kind: "select",
            value: secretRef,
            onChange: setSecretRef,
            options: refOptions,
            placeholder: "None — an unauthenticated server",
            testId: "mcp-secret-select",
            hint: "The NAME of a stored secret, never its value. Palai resolves it at request time and sends it as the Authorization header; store the whole header value, scheme included.",
            // NO FREE-TEXT FALLBACK (the T4 rule): a typo'd ref does not fail here, it fails at DISCOVER
            // with `http status 401`, which reads as a credential the server rejected rather than a
            // credential that was never found.
            emptyNote: (
              <>
                <strong>This organization has no secret refs.</strong> Write one with{" "}
                <code>palai secret create</code>, or write an environment value on the{" "}
                <a href="/environments">Environments</a> page (that creates a secret ref too). A server that
                needs no credential needs none of this — register the connection without one.
              </>
            ),
          },
        ]}
        submitLabel="Register connection"
        submittingLabel="Registering…"
        submitTestId="mcp-create-button"
        submitting={registering}
        error={registerError}
        status={registerStatus}
        onSubmit={register}
      />

      <section className="panel" data-testid="mcp-discover" aria-labelledby="mcp-discover-h">
        <h2 id="mcp-discover-h">Discover a connection&apos;s tools</h2>
        <p className="muted">
          This is the <strong>only control on this page that contacts the upstream server</strong>. Each tool
          it finds becomes a <strong>DRAFT</strong> revision — nothing is advertised to a model until you
          publish it below. Re-discovering a tool whose description or schema CHANGED creates a new draft and
          leaves the published one alone, so a server cannot alter what a model sees by editing its own text.
        </p>
        <Picker
          id="discover-connection"
          label="Connection"
          value={discoverTarget}
          onChange={setDiscoverTarget}
          options={connectionOptions}
          placeholder="Choose a connection…"
          testId="discover-connection-select"
          emptyNote={
            <>
              <strong>Register a connection first.</strong> There is nothing to discover yet.
            </>
          }
        />
        {discoverError === "" ? null : (
          <p role="alert" data-testid="discover-error">
            <span className="glyph" aria-hidden="true">
              ✖
            </span>{" "}
            {discoverError}
          </p>
        )}
        {discovered === null ? null : (
          <p data-testid="discover-result">
            <span className="glyph" aria-hidden="true">
              ✔
            </span>{" "}
            New drafts: {(discovered.new_revisions ?? []).join(", ") || "none"}. Unchanged:{" "}
            {(discovered.unchanged ?? []).join(", ") || "none"}. Rejected (a name collision):{" "}
            {(discovered.rejected ?? []).join(", ") || "none"}.
          </p>
        )}
        <p>
          <Button
            testId="discover-button"
            disabled={discoverTarget === "" || discovering}
            onClick={() => void discover()}
          >
            {discovering ? "Discovering…" : "Discover tools"}
          </Button>
        </p>
      </section>

      <Panel<ToolRow>
        title="Tools"
        testId="panel-tools"
        fetchPath="/tools"
        reloadKey={reloadTools}
        onRows={setTools}
        note="One row per tool lineage, discovered or registered. The model calls a tool by its model-visible name; the canonical name is what an operator reads."
        columns={[
          { header: "ID", render: (r) => <code>{String(r.id ?? "")}</code> },
          { header: "Canonical name", render: (r) => <code>{String(r.canonical_name ?? "")}</code> },
          { header: "Model-visible name", render: (r) => <code>{String(r.model_visible_name ?? "")}</code> },
        ]}
      />

      <section className="panel" data-testid="tool-choose" aria-labelledby="tool-choose-h">
        <h2 id="tool-choose-h">Choose a tool to approve</h2>
        <p className="muted">Its revisions are listed below, with what each one would put in front of a model.</p>
        <Picker
          id="tool-select"
          label="Tool"
          value={selectedTool}
          onChange={setSelectedTool}
          options={toolOptions}
          placeholder="Choose a tool…"
          testId="tool-select"
          emptyNote={
            <>
              <strong>Discover a connection first.</strong> There are no tools to approve — a tool lineage is
              created by discovery, not by this form.
            </>
          }
        />
      </section>

      <RevisePublish
        title="Tool revisions"
        testId="tool-revision"
        // NO CREATE FORM. A discovered revision is created by DISCOVERY; a hand-written one is an API call,
        // and offering a box for it on the screen whose job is approving somebody ELSE's text would be
        // offering the one thing this screen is not for.
        createPath=""
        listPath={selectedTool === "" ? "" : `/tools/${encodeURIComponent(selectedTool)}/revisions`}
        publishPath={(rev) => `/tools/${encodeURIComponent(selectedTool)}/revisions/${encodeURIComponent(String(rev.id ?? ""))}/publish`}
        // THE OPERATOR'S DECLARATION RIDES THE PUBLISH — no second ceremony, exactly as E23 T5 built it.
        publishBody={(rev) => {
          const decl = declarationFor(String(rev.id ?? ""));
          return { approval_required: decl.required, approval_label: decl.label };
        }}
        emptyNote={
          <>
            <strong>Choose a tool above first.</strong> A revision belongs to one lineage, so there is nothing
            to read until one is selected.
          </>
        }
        listNote={
          <>
            <strong>Publishing is the approval.</strong> Until a revision is published its description reaches
            no model. A published revision is <strong>advertised</strong> to any run whose agent revision
            names a set that pins it — and <strong>Palai does not decide which tool is dangerous</strong>: the
            gate below is yours to set, it defaults ON, and turning it OFF for a tool that writes means the
            agent will call it with no human in the loop.
          </>
        }
        columns={[
          { header: "Executor", render: (rev) => String(rev.executor ?? "") },
          {
            header: "Description (written by the server — untrusted)",
            render: (rev) => (
              // TEXT. React escapes it; nothing here interprets markup and nothing makes a URL clickable.
              <span data-testid={`tool-revision-description-${String(rev.id ?? "")}`}>{String(rev.description ?? "— none")}</span>
            ),
          },
          {
            header: "Input schema",
            render: (rev) => (
              <code data-testid={`tool-revision-schema-${String(rev.id ?? "")}`}>{JSON.stringify(rev.input_schema ?? null)}</code>
            ),
          },
          {
            header: "Human approval before each call",
            render: (rev) => {
              const id = String(rev.id ?? "");
              if (rev.status === "published") {
                return (
                  <span data-testid={`tool-revision-gate-${id}`}>
                    {rev.approval_required === true
                      ? `required — “${String(rev.approval_label ?? "") || "(no operator label)"}”`
                      : "NOT required — this tool runs with no human in the loop"}
                  </span>
                );
              }
              return (
                <>
                  <label htmlFor={`gate-${id}`}>Require approval</label>{" "}
                  <input
                    id={`gate-${id}`}
                    type="checkbox"
                    checked={declarationFor(id).required}
                    data-testid={`gate-toggle-${id}`}
                    onChange={(e) => setDeclaration(id, { required: e.target.checked })}
                  />
                  <label htmlFor={`gate-label-${id}`}>Operator label</label>
                  <input
                    id={`gate-label-${id}`}
                    type="text"
                    value={declarationFor(id).label}
                    data-testid={`gate-label-${id}`}
                    onChange={(e) => setDeclaration(id, { label: e.target.value })}
                    aria-describedby={`gate-label-${id}-hint`}
                  />
                  <span className="muted" id={`gate-label-${id}-hint`}>
                    The only human sentence on the approval screen, ≤300 characters. Write it for the person
                    who reads it at 2am. Left empty, that screen says “(no operator label)”.
                  </span>
                </>
              );
            },
          },
          {
            header: "Pin",
            render: (rev) => {
              const id = String(rev.id ?? "");
              if (rev.status !== "published") return <span>— publish it first</span>;
              if (pins.includes(id)) return <span data-testid={`pinned-${id}`}>pinned below</span>;
              return (
                <Button testId={`pin-${id}`} onClick={() => setPins((current) => [...current, id])}>
                  Add to the set
                </Button>
              );
            },
          },
        ]}
      />

      <section className="panel" data-testid="tool-set-pins" aria-labelledby="tool-set-pins-h">
        <h2 id="tool-set-pins-h">Pins for the next set revision</h2>
        <p className="muted">
          A tool set is an <strong>exact pin list</strong>: it names revision ids, not tools, so a set keeps
          granting the revision you approved even after a re-discovery creates a newer draft. Only PUBLISHED
          revisions can be pinned — the API refuses a draft pin outright. Pins collected here survive
          switching between tools above, so one set can span several connections.
        </p>
        {pins.length === 0 ? (
          <p data-testid="tool-set-pins-empty">
            <strong>Nothing pinned yet.</strong> Publish a revision above, then use “Add to the set”.
          </p>
        ) : (
          <ul data-testid="tool-set-pins-list">
            {pins.map((id) => (
              <li key={id}>
                {id}{" "}
                <Button testId={`unpin-${id}`} onClick={() => setPins((c) => c.filter((p) => p !== id))}>
                  Remove {id}
                </Button>
              </li>
            ))}
          </ul>
        )}
        <div>
          <label htmlFor="set-name">Set name</label>
          <input
            id="set-name"
            type="text"
            value={toolSetName}
            data-testid="set-name-input"
            aria-describedby="set-name-hint"
            onChange={(e) => setToolSetName(e.target.value)}
          />
          <p className="muted" id="set-name-hint">
            A set is named directly — there is no separate set object to create first. Publishing a revision
            of a name that already exists adds a revision to it.
          </p>
        </div>
      </section>

      <RevisePublish
        title="Create a tool-set revision"
        testId="tool-set-revision"
        note={
          <>
            The draft pins the ids above. Publish it, and then a run reaches these tools only if its agent
            revision names <strong>both</strong> this set and — for an MCP tool — the connection it came
            from. Either one missing fails quietly: without the set the tool is never advertised, without the
            connection it resolves to nothing.
          </>
        }
        createPath={toolSetName === "" ? "" : `/tool-sets/${encodeURIComponent(toolSetName)}/revisions`}
        listPath="/tool-sets"
        // FROM THE ROW, not from `setName`: this list shows every set in the project, so publishing the row
        // you clicked means reading its own set off it.
        publishPath={(rev) =>
          `/tool-sets/${encodeURIComponent(String(rev.set ?? ""))}/revisions/${encodeURIComponent(String(rev.id ?? ""))}/publish`
        }
        buildBody={() => ({ tools: pins.map((id) => ({ tool_revision_id: id })) })}
        // RevisePublish refetches its own list after a create, so the pins are all this has to clear.
        onCreated={() => setPins([])}
        submitLabel="Create set revision"
        emptyNote={<>Name a set above first.</>}
        listNote={
          <>
            Every set in this project, newest first — not only the one named above. <strong>Use “Show
            contents”</strong> to read which tool revisions a set revision actually grants: the list itself
            shows a digest, and a digest tells you two sets differ without telling you how.
          </>
        }
        columns={[
          { header: "Set", render: (rev) => String(rev.set ?? "") },
          { header: "Digest", render: (rev) => <code>{String(rev.digest ?? "")}</code> },
          {
            header: "Contents",
            render: (rev) => (
              <Button testId={`contents-${String(rev.id ?? "")}`} onClick={() => void showContents(rev)}>
                Show contents
              </Button>
            ),
          },
        ]}
      />

      <section className="panel" data-testid="tool-set-contents" aria-labelledby="tool-set-contents-h">
        <h2 id="tool-set-contents-h">Set contents</h2>
        {contentsError !== "" ? (
          <p role="alert" data-testid="tool-set-contents-error">
            Error: {contentsError}
          </p>
        ) : contentsOf === null ? (
          <p data-testid="tool-set-contents-empty">
            Choose “Show contents” on a set revision above to read the exact tool revisions it grants.
          </p>
        ) : (
          <>
            <p data-testid="tool-set-contents-summary" data-revision-id={contentsOf.id}>
              {contentsOf.id} is {contentsOf.status} and pins {contentsOf.pins.length} tool revision
              {contentsOf.pins.length === 1 ? "" : "s"}.
            </p>
            <ul data-testid="tool-set-contents-list">
              {contentsOf.pins.map((id) => (
                <li key={id}>{id}</li>
              ))}
            </ul>
          </>
        )}
      </section>

      <p className="muted" data-testid="tools-grant-note">
        <strong>A published set grants nothing on its own.</strong> Bind it to an agent on the{" "}
        <a href="/agents">Agents</a> page: a revision names its tool sets and its MCP connections, and a run
        is pinned to a published revision.
      </p>
    </>
  );
}
