"use client";

import { useEffect, useState } from "react";

import { Panel } from "@/components/Panel";
import { Picker } from "@/components/Picker";
import { ResourceForm } from "@/components/ResourceForm";
import { RevisePublish } from "@/components/RevisePublish";
import { apiGet, apiSend, RelayError } from "@/lib/api";

// THE AGENT LINEAGE SCREEN — create → SELECT AN ENVIRONMENT → publish (E25 T6, plan §T6).
//
// A LINEAGE, NOT A RESOURCE. An agent profile is a name; its executable configuration lives in IMMUTABLE
// revisions, and a run is pinned to a PUBLISHED one (spec §10). That is why this page has three parts rather
// than one form: the lineage, the draft, and the publish. It is also why there is no edit control anywhere on
// it — 000019_agents.up.sql has no UPDATE path for a revision's config and the API mounts no PATCH.
//
// THE ENVIRONMENT FIELD IS WHY THIS TASK AND T3 ARE ONE SENTENCE. T3 built the pipe that carries an
// environment's KEY=value pairs into an agent's shell commands, and it could only prove it against a
// hand-written revision row. This dropdown is the operator's end of that pipe: choose an environment here,
// publish, and the shell of every run pinned to this revision receives those keys. The proof that the keys
// actually arrive is a component test over the SAME HTTP routes this page calls
// (apps/control-plane/internal/execution/console_environment_run_component_test.go) — a browser cannot see a
// subprocess's environment, and pretending otherwise would be the kind of claim this tree writes down instead.
//
// AND IT IS A SELECT, NEVER A TEXT BOX (the T4 rule, ResourceForm's select arm): an environment id typed by
// hand is accepted by this form and then refused at PUBLISH with a 400 about a field the operator has already
// stopped looking at. With no environments the control is not rendered at all — a refusal with a link, which
// is the honest shape of "there is nothing to choose".
//
// WHAT IS DELIBERATELY ABSENT UNTIL T7: `tool_sets` and `mcp_connections`. The revision body accepts both and
// this page renders NEITHER, because there is nothing in this deployment to put in them yet — the two read
// routes that would list a tool revision and a tool set's contents do not exist (plan §3.6 D14), which is
// T7's whole reason for being. An empty dropdown is a control that cannot be satisfied, so the absence is
// stated in a sentence instead.

interface AgentRow extends Record<string, unknown> {
  id?: string;
  name?: string;
}

interface EnvironmentRow {
  id?: string;
  name?: string;
}

const detail = (err: unknown, fallback: string) => (err instanceof RelayError ? err.problem.detail : fallback);

/** csv splits a comma-separated field into a trimmed list. `tools` is an array on the wire. */
const csv = (raw: string): string[] =>
  raw
    .split(",")
    .map((s) => s.trim())
    .filter((s) => s !== "");

export default function AgentsPage() {
  const [agents, setAgents] = useState<AgentRow[]>([]);
  const [reloadKey, setReloadKey] = useState(0);
  const [selected, setSelected] = useState("");

  const [newName, setNewName] = useState("");
  const [createError, setCreateError] = useState("");
  const [createStatus, setCreateStatus] = useState("");
  const [creating, setCreating] = useState(false);

  const [model, setModel] = useState("");
  const [instructions, setInstructions] = useState("");
  const [tools, setTools] = useState("");
  const [environment, setEnvironment] = useState("");

  const [environments, setEnvironments] = useState<EnvironmentRow[]>([]);
  useEffect(() => {
    let live = true;
    apiGet<{ data?: EnvironmentRow[] }>("/environments")
      .then((body) => {
        if (live) setEnvironments(body.data ?? []);
      })
      .catch(() => {
        // An unreadable list leaves the picker absent, which renders the "create one first" note. An operator
        // who cannot read environments cannot bind one either, so that is the truthful screen.
      });
    return () => {
      live = false;
    };
  }, []);

  async function createAgent() {
    setCreating(true);
    setCreateError("");
    setCreateStatus("");
    try {
      const body = await apiSend<AgentRow>("POST", "/agents", { name: newName });
      setCreateStatus(`Created ${String(body.name ?? newName)} (${String(body.id ?? "?")}). It has no revisions yet, so nothing can be pinned to it.`);
      setNewName("");
      setReloadKey((n) => n + 1);
      // SELECT WHAT WAS JUST MADE. Without this the operator's next step is to find their own agent in a list
      // of near-identical names, which on an accumulated stack is the worst kind of small friction.
      if (typeof body.id === "string" && body.id !== "") setSelected(body.id);
    } catch (err: unknown) {
      setCreateError(detail(err, "the agent could not be created"));
    } finally {
      setCreating(false);
    }
  }

  const agentOptions = agents
    .filter((a) => typeof a.id === "string" && a.id !== "")
    .map((a) => ({ value: String(a.id), label: `${String(a.name ?? a.id)} (${String(a.id)})` }));
  const environmentOptions = environments
    .filter((e) => typeof e.id === "string" && e.id !== "")
    .map((e) => ({ value: String(e.id), label: `${String(e.name ?? e.id)} (${String(e.id)})` }));

  return (
    <>
      <p className="muted" data-testid="agent-lineage-note">
        An agent is a <strong>name with a lineage of immutable revisions</strong>. A run is pinned to a{" "}
        <strong>published</strong> revision, and publishing cannot be undone — so a revision is superseded,
        never edited. There is no PATCH and no DELETE on this surface: this console creates and reads.
      </p>

      <Panel<AgentRow>
        title="Agents"
        testId="panel-agent-profiles"
        fetchPath="/agents"
        reloadKey={reloadKey}
        onRows={setAgents}
        note="One row per lineage. Select one below to read its revisions and add another."
        columns={[
          { header: "ID", render: (r) => String(r.id ?? "") },
          { header: "Name", render: (r) => String(r.name ?? "") },
        ]}
      />

      <ResourceForm
        title="Create an agent"
        testId="agent-create"
        note="A name and nothing else — the executable configuration is a revision, which is the next form."
        fields={[
          {
            name: "agent-name",
            label: "Name",
            required: true,
            value: newName,
            onChange: setNewName,
            hint: "For example: deployer, docs-writer, release-bot.",
            testId: "agent-name-input",
          },
        ]}
        submitLabel="Create agent"
        submitTestId="agent-create-button"
        submitting={creating}
        error={createError}
        status={createStatus}
        onSubmit={createAgent}
      />

      {/* NOT a ResourceForm, and the reason is the one that component's own header gives: it exists so a FORM
          does not re-derive labels, an alert region and a submit. This is a single navigational select with
          no submission — wrapping it would have meant a button that does nothing. The rule that matters (an
          options-less select is not rendered at all) comes from Picker, which is where it now lives. */}
      <section className="panel" data-testid="agent-choose" aria-labelledby="agent-choose-h">
        <h2 id="agent-choose-h">Choose an agent</h2>
        <p className="muted">The revision form and the lineage below both act on this agent.</p>
        <Picker
          id="agent-select"
          label="Agent"
          value={selected}
          onChange={setSelected}
          options={agentOptions}
          placeholder="Choose an agent…"
          testId="agent-select"
          emptyNote={
            <>
              <strong>Create an agent first.</strong> There is no lineage to revise yet.
            </>
          }
        />
      </section>

      <RevisePublish
        title="Create a revision"
        testId="agent-revision"
        note={
          <>
            A draft is created first and <strong>published as a separate step</strong>, because a run can only
            be pinned to a published revision. An environment chosen here is what the agent&apos;s{" "}
            <strong>shell commands</strong> receive as <code>KEY=value</code> — never something the model is
            shown, and never part of a prompt.
          </>
        }
        createPath={selected === "" ? "" : `/agents/${encodeURIComponent(selected)}/revisions`}
        listPath={selected === "" ? "" : `/agents/${encodeURIComponent(selected)}/revisions`}
        publishPath={(revisionID) => `/agents/${encodeURIComponent(selected)}/revisions/${encodeURIComponent(revisionID)}/publish`}
        emptyNote={
          <>
            <strong>Choose an agent above first.</strong> A revision belongs to one lineage, so there is
            nothing to create or read until one is selected.
          </>
        }
        onCreated={() => {
          setModel("");
          setInstructions("");
          setTools("");
        }}
        fields={[
          {
            name: "revision-model",
            label: "Model",
            value: model,
            onChange: setModel,
            hint: "Overrides the project route and the deployment default for runs pinned to this revision. Leave empty to inherit.",
            testId: "revision-model-input",
          },
          {
            name: "revision-instructions",
            label: "Instructions",
            kind: "textarea",
            value: instructions,
            onChange: setInstructions,
            hint: "The system instructions this agent runs with.",
            testId: "revision-instructions-input",
          },
          {
            name: "revision-tools",
            label: "Tools",
            value: tools,
            onChange: setTools,
            hint: "Comma-separated tool names. This is a CEILING on what a run pinned to this revision may call, not a grant — a name here that the project does not grant stays unavailable.",
            testId: "revision-tools-input",
          },
          {
            name: "revision-environment",
            label: "Environment",
            kind: "select",
            value: environment,
            onChange: setEnvironment,
            options: environmentOptions,
            placeholder: "None — this agent's shell gets no extra keys",
            testId: "revision-environment-select",
            hint: "Its KEY=value pairs reach every shell command of every run pinned to this revision. A value written into an environment can never be read back — see the Environments page.",
            // NO FREE-TEXT FALLBACK. Publishing a revision that names a missing environment is refused with a
            // 400 (store/agents.go, automation.ErrEnvironmentNotFound), which is a refusal about a field the
            // operator typed several steps earlier.
            emptyNote: (
              <>
                <strong>Create an environment first.</strong> There is nothing to bind, and an id cannot be
                typed here on purpose. <a href="/environments">Go to Environments</a>.
              </>
            ),
          },
        ]}
        buildBody={() => ({
          // Only what was FILLED IN. An empty string on `model` would pin the revision to a model named "",
          // and an empty `environment` is the no-environment case the store reads as "inherit nothing".
          ...(model === "" ? {} : { model }),
          ...(instructions === "" ? {} : { instructions }),
          ...(tools === "" ? {} : { tools: csv(tools) }),
          ...(environment === "" ? {} : { environment }),
        })}
        columns={[
          { header: "Model", render: (rev) => String(rev.model ?? "— inherited") },
          {
            header: "Environment",
            render: (rev) => (
              <span data-testid={`revision-environment-${String(rev.id ?? "")}`}>{rev.environment ? String(rev.environment) : "— none"}</span>
            ),
          },
          { header: "Tools", render: (rev) => (Array.isArray(rev.tools) ? rev.tools.join(", ") : "— inherited") },
        ]}
      />

      <p className="muted" data-testid="revision-t7-note">
        <strong>This form does not offer tool sets and MCP connections yet</strong>, and that is an absence
        rather than an oversight: the revision body accepts both, but nothing in this deployment can list a
        tool revision or a tool set&apos;s contents, so there would be nothing to choose from. An empty
        dropdown is a control that cannot be satisfied. Until those read routes exist, a revision&apos;s
        external capability ceiling is set outside this console.
      </p>
    </>
  );
}
