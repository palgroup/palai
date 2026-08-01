"use client";

import { useRef, useState } from "react";

import { Button } from "@/components/ui/Button";
import { Dialog } from "@/components/ui/Dialog";
import { apiSend, RelayError } from "@/lib/api";
import {
  AGENT_TEMPLATES,
  buildRevisionBody,
  parseTemplateYAML,
  type AgentTemplate,
  type ParseError,
} from "@/lib/agentTemplates";

// THE TEMPLATE GALLERY — a worked agent, as YAML, that an operator can read and edit before it exists.
//
// WHAT "USE TEMPLATE" ACTUALLY DOES, because the button hides two calls and one of them is irreversible in
// a way the other is not:
//
//   POST /v1/agents                    → the profile lineage, named by the template's `name`
//   POST /v1/agents/{id}/revisions     → a DRAFT revision carrying the executable config
//
// The draft is NOT published. Publishing is a separate, irreversible act on the agent's own page
// (000019_agents.up.sql sets published_at once and no route unsets it), and a template that published its
// own output would make a gallery click the most consequential control in this console.
//
// IF THE SECOND CALL FAILS THE FIRST HAS ALREADY HAPPENED, and this component says so rather than pretending
// the pair was atomic. There is no transaction across two public API calls and this console must not invent
// one: what it can do is name the agent that now exists with no revision, so the operator knows what to go
// and look at instead of creating it a second time.
//
// THE EDITOR IS A TEXTAREA WITH A GUTTER, not a code editor. Line numbers exist because the parser reports
// errors BY LINE, and a numbered error against unnumbered text is a puzzle. There is no syntax highlighting
// and no autocomplete: both want a parser running per keystroke over a grammar lib/agentTemplates.ts
// deliberately does not implement.

const detail = (err: unknown, fallback: string) =>
  err instanceof RelayError ? err.problem.detail : fallback;

function Editor({
  value,
  onChange,
  errors,
}: {
  value: string;
  onChange: (next: string) => void;
  errors: readonly ParseError[];
}) {
  const gutter = useRef<HTMLDivElement | null>(null);
  const lines = value.split("\n");
  const bad = new Set(errors.map((e) => e.line));

  return (
    <div className="yaml-editor">
      {/* aria-hidden: the numbers are a visual index into text the textarea already exposes. A screen
          reader reading "1 2 3 4" before the content would be reading furniture. The errors below carry
          their line numbers IN WORDS for exactly this reason. */}
      <div className="yaml-gutter" ref={gutter} aria-hidden="true">
        {lines.map((_, n) => (
          <span key={n} className={bad.has(n + 1) ? "yaml-line yaml-line-bad" : "yaml-line"}>
            {n + 1}
          </span>
        ))}
      </div>
      <textarea
        className="yaml-text"
        data-testid="template-yaml"
        aria-label="Template YAML"
        spellCheck={false}
        value={value}
        rows={Math.min(Math.max(lines.length, 12), 28)}
        onChange={(e) => onChange(e.target.value)}
        onScroll={(e) => {
          if (gutter.current !== null) gutter.current.scrollTop = e.currentTarget.scrollTop;
        }}
      />
    </div>
  );
}

export function AgentTemplates({ onCreated }: { onCreated: (agentId: string) => void }) {
  const [chosen, setChosen] = useState<AgentTemplate | null>(null);
  const [draft, setDraft] = useState("");
  const [errors, setErrors] = useState<readonly ParseError[]>([]);
  const [failure, setFailure] = useState("");
  const [busy, setBusy] = useState(false);

  function open(template: AgentTemplate) {
    setChosen(template);
    setDraft(template.yaml);
    setErrors([]);
    setFailure("");
  }

  function close() {
    setChosen(null);
    setErrors([]);
    setFailure("");
  }

  async function apply() {
    const { template, errors: found } = parseTemplateYAML(draft);
    setErrors(found);
    setFailure("");
    if (template === null) return;

    setBusy(true);
    let agentId = "";
    try {
      const profile = await apiSend<{ id?: string }>("POST", "/agents", { name: template.name });
      agentId = String(profile.id ?? "");
      if (agentId === "") {
        setFailure("The agent was created but the API returned no id, so its revision could not be written.");
        return;
      }
      await apiSend("POST", `/agents/${encodeURIComponent(agentId)}/revisions`, buildRevisionBody(template));
      onCreated(agentId);
    } catch (err: unknown) {
      setFailure(
        agentId === ""
          ? detail(err, "the agent could not be created")
          : // THE HALF-DONE CASE, NAMED. The profile exists; the revision does not.
            `The agent "${template.name}" was created, but its first revision was refused: ` +
              `${detail(err, "the revision could not be written")}. The agent is on the list below with no ` +
              "revisions — open it and write one there rather than creating it again.",
      );
    } finally {
      setBusy(false);
    }
  }

  return (
    <section className="panel" data-testid="panel-agent-templates" aria-labelledby="agent-templates-h">
      <h2 id="agent-templates-h">Start from a template</h2>
      <p className="muted">
        A worked agent you can read before it exists. Choosing one opens its YAML — edit it, then create the
        agent and its first <strong>draft</strong> revision in one step. Nothing is published: that stays a
        deliberate act on the agent&apos;s own page.
      </p>
      <p className="muted">
        A template <strong>names</strong> MCP connections, it does not create them. Register those on the{" "}
        <a href="/tools">Tools</a> page first — every template says which ones it wants and why.
      </p>

      <ul className="template-gallery">
        {AGENT_TEMPLATES.map((t) => (
          <li key={t.id} className="template-card" data-testid={`template-card-${t.id}`}>
            <h3>{t.title}</h3>
            <p className="muted">{t.summary}</p>
            <p className="template-integrations">
              {t.integrations.map((name) => (
                <code key={name}>{name}</code>
              ))}
            </p>
            <Button variant="secondary" testId={`template-open-${t.id}`} onClick={() => open(t)}>
              Open {t.title}
            </Button>
          </li>
        ))}
      </ul>

      <Dialog
        open={chosen !== null}
        onOpenChange={(next) => {
          if (!next) close();
        }}
        testId="template-dialog"
        title={chosen === null ? "Template" : chosen.title}
        description={
          <>
            Creates an agent named by the <code>name</code> field, plus one <strong>draft</strong> revision.
            Fields with no counterpart here — a permission policy, an inline server URL, arbitrary metadata —
            are not accepted and the editor will say so by line.
          </>
        }
        actions={
          <>
            <Button variant="secondary" testId="template-cancel" onClick={close}>
              Cancel
            </Button>
            <Button variant="primary" testId="template-apply" disabled={busy} onClick={apply}>
              {busy ? "Creating…" : "Use template"}
            </Button>
          </>
        }
      >
        <div className="template-body">
          <Editor value={draft} onChange={setDraft} errors={errors} />

          {errors.length > 0 ? (
            <div className="template-errors" role="alert" data-testid="template-errors">
              <p>
                <strong>
                  {errors.length === 1 ? "One problem" : `${errors.length} problems`} — nothing was created.
                </strong>
              </p>
              <ul>
                {errors.map((e, n) => (
                  <li key={`${e.line}-${n}`}>
                    <strong>Line {e.line}:</strong> {e.message}
                  </li>
                ))}
              </ul>
            </div>
          ) : null}

          {failure !== "" ? (
            <p className="template-errors" role="alert" data-testid="template-failure">
              {failure}
            </p>
          ) : null}
        </div>
      </Dialog>
    </section>
  );
}
