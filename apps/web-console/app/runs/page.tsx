"use client";

import { useEffect, useRef, useState } from "react";

import { Button } from "@/components/ui/Button";
import { ApprovalPanel, type PendingApproval } from "@/components/ApprovalPanel";
import { DeploymentNotice } from "@/components/DeploymentNotice";
import { Picker } from "@/components/Picker";
import { Status } from "@/components/Status";
import { Timeline, type Frame } from "@/components/Timeline";
import { apiGet, artifactHref, RelayError, streamRun } from "@/lib/api";

interface ToolRow {
  seq: number;
  type: string;
  name: string | null;
  arguments: unknown;
  result: unknown;
}
interface RecoveryRow {
  seq: number;
  type: string;
  attempt: unknown;
  detail: unknown;
}
interface Usage {
  input_tokens: number | null;
  output_tokens: number | null;
  total_tokens: number | null;
  tool_calls: number | null;
}
interface ArtifactRow {
  id: string;
}
interface AgentRow {
  id: string;
  name?: string;
}
/** A session this run can be appended to. Only ACTIVE ones can be — see the picker's own note. */
interface SessionRow {
  id: string;
  title?: string;
  status?: string;
  state?: string;
}

interface RevisionRow {
  id: string;
  revision_number?: number;
  model?: string;
  status?: string;
}

// The §47.2 live surface: start a run, watch its canonical timeline sorted into lanes, act on an approval
// (the exact-detail panel), see recovery/attempt transitions, read usage, and download artifacts — all
// through the /v1/* relay. The API key never reaches this component; every call is a same-origin relay fetch.
export default function RunsPage() {
  const [prompt, setPrompt] = useState("Push the release branch.");
  const [status, setStatus] = useState<string>("idle");
  const [frames, setFrames] = useState<Frame[]>([]);
  const [streamText, setStreamText] = useState("");
  const [tools, setTools] = useState<ToolRow[]>([]);
  const [recovery, setRecovery] = useState<RecoveryRow[]>([]);
  const [usage, setUsage] = useState<Usage | null>(null);
  const [pending, setPending] = useState<PendingApproval | null>(null);
  const [sessionId, setSessionId] = useState<string>("");
  // --- SINGLE SHOT, OR A TURN IN A CONVERSATION -----------------------------------------------------------
  //
  // `chainTo` is the session this run will APPEND to. "" is a single shot: the admission mints a fresh
  // session and the model sees nothing before this prompt. app/api/palai/stream/route.ts carries the two
  // code references and the reason the console could never do the second one — `session_id` has been on the
  // wire contract the whole time and this page only ever read the id back off the `meta` frame.
  //
  // `sessionId` (above) is still the RESULT — the session the run that just ran belongs to. The two are
  // deliberately separate: reusing one field for "what I asked for" and "what I got" is how a control ends
  // up silently continuing whichever session happened to be on screen.
  const [chainTo, setChainTo] = useState<string>("");
  const [sessions, setSessions] = useState<SessionRow[] | null>(null);
  const [terminal, setTerminal] = useState<{ status: string; model: string | null; output: unknown } | null>(null);
  const [artifacts, setArtifacts] = useState<ArtifactRow[]>([]);
  const [running, setRunning] = useState(false);

  // THE OPTIONAL PIN (E25 T6). Both default to "", which is the unpinned run this page has always started —
  // the relay omits the field entirely then, so the upstream body is bit-identical to before.
  const [agents, setAgents] = useState<AgentRow[] | null>(null);
  const [agentId, setAgentId] = useState("");
  // The §22.7 output contract as the operator typed it, and the parse refusal shown beside the box.
  // The RAW TEXT is state, not a parsed object: re-serialising the operator's document would lose
  // their formatting on every keystroke, and the text is what the relay is given anyway.
  const [outputSchema, setOutputSchema] = useState("");
  const [schemaError, setSchemaError] = useState("");
  const [revisions, setRevisions] = useState<RevisionRow[]>([]);
  // The pickers read page ONE of each collection. Both are newest-first and neither offers a continuation, so
  // the cut is said in words rather than left to look like the whole list (§2). What an operator pins is a
  // recent revision, which is why a "load more" is not what the honesty costs here.
  const [agentsTruncated, setAgentsTruncated] = useState(false);
  const [revisionsTruncated, setRevisionsTruncated] = useState(false);
  const [revisionId, setRevisionId] = useState("");
  // The run's own refusal, announced rather than only rendered. A 409 for a draft revision means NO RUN
  // STARTED, which is a different thing from a run that failed — and the difference has to be on the screen.
  const [runError, setRunError] = useState("");

  const responseIdRef = useRef<string>("");
  const abortRef = useRef<AbortController | null>(null);

  useEffect(() => {
    let live = true;
    apiGet<{ data?: AgentRow[]; has_more?: boolean }>("/agents")
      .then((body) => {
        if (!live) return;
        setAgents(body.data ?? []);
        setAgentsTruncated(body.has_more === true);
      })
      .catch(() => {
        // An unreadable list leaves the picker absent and the note in its place. A run with no pin still
        // works, which is why this is not fatal to the page.
        if (live) setAgents([]);
      });
    return () => {
      live = false;
    };
  }, []);

  // THE SESSIONS THIS RUN COULD CONTINUE, and only the ones it actually could.
  //
  // `?status=active` is a SERVER filter (api/pagination.go's beginList parses it), not a client-side
  // narrowing, and it is the honest list: admission refuses a non-active session with a 409
  // (`SessionConflict`, packages/coordinator/store.go:754) and an unknown or foreign one with a 404. A
  // picker that offered a closed session would be offering a choice the server answers with a refusal —
  // the exact shape components/Picker.tsx exists to prevent.
  //
  // It is fetched ONCE, on mount, and refreshed after a run reaches its terminal (see `run()`): a run that
  // opens a new session makes that session the newest row here, and an operator's next act is often to
  // continue the thing they just started.
  const [sessionsReloadKey, setSessionsReloadKey] = useState(0);
  useEffect(() => {
    let live = true;
    apiGet<{ data?: SessionRow[] }>("/sessions?status=active")
      .then((body) => {
        if (live) setSessions(body.data ?? []);
      })
      .catch(() => {
        // Same rule as the agents list: an unreadable collection leaves the picker absent and its note in
        // place. A single shot still works, which is what makes this non-fatal.
        if (live) setSessions([]);
      });
    return () => {
      live = false;
    };
  }, [sessionsReloadKey]);

  useEffect(() => {
    setRevisionId("");
    if (agentId === "") {
      setRevisions([]);
      return;
    }
    let live = true;
    apiGet<{ data?: RevisionRow[]; has_more?: boolean }>(`/agents/${encodeURIComponent(agentId)}/revisions`)
      .then((body) => {
        if (!live) return;
        setRevisions(body.data ?? []);
        setRevisionsTruncated(body.has_more === true);
      })
      .catch(() => {
        if (live) setRevisions([]);
      });
    return () => {
      live = false;
    };
  }, [agentId]);

  function reset() {
    setStatus("streaming");
    setFrames([]);
    setStreamText("");
    setTools([]);
    setRecovery([]);
    setUsage(null);
    setPending(null);
    setSessionId("");
    setTerminal(null);
    setArtifacts([]);
    setRunError("");
    responseIdRef.current = "";
  }

  function onFrame(f: Record<string, unknown>) {
    const type = f.type as string | undefined;
    if (type === "status") {
      setStatus(String(f.status));
      return;
    }
    if (type === "meta") {
      setSessionId(String(f.sessionId ?? ""));
      responseIdRef.current = String(f.responseId ?? "");
      return;
    }
    if (type === "final") {
      setStatus(String(f.status));
      setTerminal({ status: String(f.status), model: (f.model as string) ?? null, output: f.output });
      if (f.usage) setUsage(f.usage as Usage);
      return;
    }
    if (type === "error") {
      setStatus("error");
      // THE REFUSAL, IN THE SERVER'S OWN WORDS — and, for the two refusals that happen BEFORE anything is
      // created, what did not happen. api/responses.go answers a draft pin with 409 `revision_not_published`
      // and an unknown pin with 404, both decided before the idempotency reserve, so there is no run, no
      // session and no idempotency record. The distinction is not cosmetic: an operator who read only
      // "failed" would go looking for a run that does not exist, and the failure mode this sentence rules out
      // — a silent fall back to some other configuration — would be worse than either.
      const detail = String(f.detail ?? "the run could not be started");
      const code = String(f.code ?? "");
      const admissionRefusal = code === "revision_not_published" || code === "not_found";
      setRunError(
        admissionRefusal
          ? `${detail} — this run did not start: no run and no session were created, and no default ` +
              "configuration was substituted for the revision you chose. Publish the revision on the Agents " +
              "page, or clear the revision to run with the project's own configuration."
          : detail,
      );
      // AND NO "TERMINAL RESULT" PANEL FOR A RUN THAT HAS NO TERMINAL. An admission refusal happens before
      // anything is created, so there is no run whose end state could be reported — a panel headed "Terminal
      // result" over it would be exactly the silent-lie shape the rest of this console is built to avoid.
      // Every OTHER error still fills it, unchanged: a run that started and then failed does have a terminal.
      if (!admissionRefusal) setTerminal({ status: "error", model: null, output: f.detail });
      return;
    }

    // A lane-tagged canonical event: record it for the timeline, then fan out to its lane view.
    setFrames((prev) => [...prev, f as Frame]);
    const lane = f.lane as string | undefined;
    if (lane === "model_step" && typeof f.text === "string") {
      setStreamText((prev) => prev + f.text);
    } else if (lane === "tool") {
      const t = f.tool as Record<string, unknown>;
      setTools((prev) => [...prev, { seq: Number(f.sequence), type: String(type), name: (t?.name as string) ?? null, arguments: t?.arguments, result: t?.result }]);
    } else if (lane === "approval") {
      const a = f.approval as PendingApproval & { resolution: string | null };
      if (a.resolution === null) {
        setPending(a);
      } else {
        setPending(null); // resolved — the authoritative detail is no longer actionable
      }
    } else if (lane === "recovery") {
      const rec = f.recovery as Record<string, unknown>;
      setRecovery((prev) => [...prev, { seq: Number(f.sequence), type: String(type), attempt: rec?.attempt, detail: rec?.detail }]);
    } else if (lane === "usage") {
      setUsage(f.usage as Usage);
    }
  }

  async function run() {
    // Parse before anything else — before reset(), so a syntax error does not clear the previous
    // run's timeline, and before setRunning, so the button does not flicker into a disabled state for
    // a request that was never sent. An empty box is not an error: it is the default.
    if (outputSchema.trim() !== "") {
      let parsed: unknown;
      try {
        parsed = JSON.parse(outputSchema);
      } catch (err) {
        setSchemaError(`The output schema is not valid JSON: ${(err as Error).message}`);
        return;
      }
      if (parsed === null || typeof parsed !== "object" || Array.isArray(parsed)) {
        setSchemaError("The output schema must be a JSON object, for example {\"type\":\"object\", ...}.");
        return;
      }
    }
    setSchemaError("");
    reset();
    setRunning(true);
    const controller = new AbortController();
    abortRef.current = controller;
    try {
      await streamRun(prompt, onFrame, controller.signal, revisionId, outputSchema, chainTo);
      // The run reached its terminal — refresh the sessions this page can continue, because the run just
      // either opened one or added a turn to one, and offering yesterday's list is offering a stale choice.
      setSessionsReloadKey((n) => n + 1);
      // Load the artifacts it produced (through the relay).
      const rid = responseIdRef.current;
      if (rid !== "") {
        try {
          const list = await apiGet<{ data?: ArtifactRow[] }>(`/responses/${encodeURIComponent(rid)}/artifacts`);
          setArtifacts(list.data ?? []);
        } catch {
          /* an empty/absent artifact list is not fatal to the timeline */
        }
      }
    } catch (err) {
      if (!controller.signal.aborted) {
        setStatus("error");
        const detail = err instanceof RelayError ? err.problem.detail : "stream failed";
        setTerminal({ status: "error", model: null, output: detail });
        setRunError(detail);
      }
    } finally {
      setRunning(false);
    }
  }

  function abort() {
    abortRef.current?.abort();
    setStatus("aborted");
    setRunning(false);
  }

  return (
    <>
      {/* THE DEPLOYMENT'S OWN WARNING, ABOVE THE BUTTON THAT WOULD OTHERWISE LIE (machine-config).
          This screen's lead promises "Start a run and watch it happen". Measured 2026-08-01: a stack
          brought up with `make local-up` takes PALAI_DISPATCH_WORKERS=0 (compose.yaml:82), so five runs
          were accepted here and every one sat at run.queued.v1 forever with nothing on any screen saying
          why. It goes ABOVE the prompt rather than beside the result, because the point is to be read
          before the button is pressed. */}
      <DeploymentNotice path="/runs" />

      <section className="panel" aria-labelledby="run-h">
        <h2 id="run-h">Start a run</h2>
        {/* WHAT THIS RUN IS, DECIDED BEFORE THE PROMPT IS TYPED — because it is what the prompt MEANS.
            "single shot bir şey görmüyorum": this page offered a prompt, a model and an agent, and nothing
            said whether the model would see anything before this message. It could not have: `session_id`
            was never sent, so every run opened a fresh session and the answer was always "single shot".

            THE PICKER IS ABSENT WHEN THERE IS NOTHING TO CONTINUE, which is components/Picker.tsx's rule and
            the right one here: on a deployment with no active session the choice does not exist, and a
            disabled radio pair explaining that would be two controls where one sentence does. */}
        <div className="run-mode" data-testid="run-mode">
          {/* THE PICKER IS ABSENT WHEN THERE IS NOTHING TO CONTINUE, which is components/Picker.tsx's rule
              and the right one here: on a deployment with no active session the choice does not exist, and a
              disabled control explaining that would be a control where a sentence does.

              THE SENTENCE BELOW IS NOT INSIDE THAT BRANCH, and that separation is deliberate rather than
              tidy: it is the thing that answers "single shot bir şey görmüyorum", so it must be on the
              screen in EVERY state — including the two where no picker renders (no active session at all,
              and a session armed by the Continue button on a stack whose list has not caught up). A note
              that disappears exactly when the situation is unusual is a note that is missing when it is
              most needed. */}
          {sessions === null || sessions.length === 0 ? null : (
            <Picker
              id="run-chain-select"
              label="Conversation"
              value={chainTo}
              onChange={setChainTo}
              options={sessions.map((row) => ({
                value: row.id,
                label: `${row.title === undefined || row.title === "" ? "— unnamed" : row.title} (${row.id})`,
              }))}
              placeholder="Single shot — start a new conversation"
              testId="run-chain-select"
              manage={{ href: "/sessions", label: "Manage sessions" }}
              emptyNote={<>No active session to continue.</>}
            />
          )}
          {/* Both halves are what the CODE does rather than what it is for: packages/coordinator/store.go:742
              mints a session when none is requested and appends when one is, and only an ACTIVE session may
              be appended to (a non-active one is `SessionConflict`, a 409). */}
          {sessions === null ? null : (
            <p className="muted" data-testid="run-mode-note">
              {chainTo === "" ? (
                <>
                  <strong>Single shot.</strong> One request, one response — this run opens a NEW session and
                  the model sees nothing before this prompt.
                  {sessions.length === 0 ? " No session is active yet, so there is nothing to continue." : ""}
                </>
              ) : (
                <>
                  <strong>A turn in a conversation.</strong> This run appends to <code>{chainTo}</code>, so
                  the model sees that session&apos;s earlier turns. The session must still be active — a
                  closed one is refused with a 409 and no run is created.
                </>
              )}
            </p>
          )}
        </div>

        <label htmlFor="prompt-input">Prompt</label>
        <textarea id="prompt-input" data-testid="prompt-input" rows={2} value={prompt} onChange={(e) => setPrompt(e.target.value)} />

        {/* THE OPTIONAL PIN (E25 T6). Two pickers, because a revision belongs to one agent and that is the
            shape the API has: GET /v1/agents, then GET /v1/agents/{id}/revisions. Neither degrades to a
            free-text box, and with no agents at all the controls are absent rather than empty.

            A DRAFT IS OFFERED AND LABELLED RATHER THAN HIDDEN. Filtering drafts out would have made the
            server's 409 unreachable from this console, and an operator who cannot see their draft cannot tell
            why the agent they just configured is not running. What they see is the reason. */}
        {/* Nothing while the list is still loading: rendering the empty note first would state "no agents
            yet" about a collection nobody has read, and axe scans this page in whatever state it finds. */}
        {agents === null ? null : (
          <Picker
            id="run-agent-select"
            label="Agent (optional)"
            value={agentId}
            onChange={setAgentId}
            options={agents.map((a) => ({ value: a.id, label: a.name === undefined || a.name === "" ? a.id : `${a.name} (${a.id})` }))}
            placeholder="None — use the project's configuration"
            testId="run-agent-select"
            manage={{ href: "/agents", label: "Manage agents" }}
            emptyNote={
              <>
                <strong>No agents yet</strong>, so this run uses the project&apos;s own configuration.{" "}
                <a href="/agents">Create an agent</a> to pin a run to a published revision.
              </>
            }
          />
        )}
        {agentId === "" ? null : (
          <Picker
            id="run-revision-select"
            label="Revision (optional)"
            value={revisionId}
            onChange={setRevisionId}
            options={revisions.map((r) => ({
              value: r.id,
              label:
                `${r.id}${r.revision_number === undefined ? "" : ` (#${String(r.revision_number)})`}` +
                ` — ${r.status === "published" ? "published" : "draft, cannot be run until published"}` +
                `${r.model === undefined || r.model === "" ? "" : ` — model ${r.model}`}`,
            }))}
            placeholder="None — use the project's configuration"
            testId="run-revision-select"
            manage={{ href: "/agents", label: "Manage agents" }}
            hint="Only a PUBLISHED revision can be run. A draft is refused by the server, which is why it is listed rather than hidden."
            emptyNote={
              <>
                <strong>This agent has no revisions yet.</strong> <a href="/agents">Create one</a> and publish
                it before a run can be pinned to it.
              </>
            }
          />
        )}

        {agentsTruncated ? (
          <p data-testid="run-agent-select-more">
            Showing the {(agents ?? []).length} newest agents — <strong>older ones exist and are not offered
            here</strong>. Pin an older one through the API.
          </p>
        ) : null}
        {revisionsTruncated ? (
          <p data-testid="run-revision-select-more">
            Showing the {revisions.length} newest revisions — <strong>older ones exist and are not offered
            here</strong>.
          </p>
        ) : null}

        {/* THE OUTPUT CONTRACT (spec §22.7). Optional, and empty means exactly what it has always
            meant: free-form text, nothing validated.

            THE PARSE HAPPENS BEFORE SUBMIT, AND THE REFUSAL IS SHOWN. A malformed schema posted to
            the API comes back as a 400 whose detail is about JSON syntax, which is a slow and
            confusing way to learn about a missing brace in a box that is still on screen. The check
            below is a convenience for the operator, NOT the guarantee: the relay parses it again and
            the API refuses any schema it cannot enforce. A console that treated its own parse as the
            guarantee would be the same defect this screen's feature exists to fix.

            role="alert" and a glyph-plus-word, per the ResourceForm discipline: a refusal is
            announced without moving focus, and never signalled by colour alone. */}
        <label htmlFor="output-schema-input">Output JSON Schema (optional)</label>
        <p id="output-schema-hint" className="hint">
          Leave empty for free-form text. With a schema, the model is <strong>constrained</strong> to it and
          the answer is <strong>validated</strong> before the run is called completed — a run whose output does
          not satisfy it fails rather than returning prose. Draft 2020-12; <code>$ref</code>, <code>oneOf</code>{" "}
          and other keywords this server cannot enforce are refused rather than silently ignored.
        </p>
        <textarea
          id="output-schema-input"
          data-testid="output-schema-input"
          aria-describedby="output-schema-hint"
          rows={4}
          spellCheck={false}
          value={outputSchema}
          placeholder={'{"type":"object","properties":{"city":{"type":"string"}},"required":["city"]}'}
          onChange={(e) => {
            setOutputSchema(e.target.value);
            if (schemaError !== "") setSchemaError("");
          }}
        />
        {schemaError === "" ? null : (
          <p role="alert" className="form-error" data-testid="output-schema-error">
            <span className="glyph" aria-hidden="true">
              ✖
            </span>{" "}
            {schemaError}
          </p>
        )}

        {runError === "" ? null : (
          <p role="alert" className="form-error" data-testid="run-error">
            <span className="glyph" aria-hidden="true">
              ✖
            </span>{" "}
            {runError}
          </p>
        )}
        <p>
          <Button testId="run-button" onClick={run} disabled={running}>
            Run
          </Button>{" "}
          <Button testId="abort-button" onClick={abort} disabled={!running}>
            Abort
          </Button>{" "}
          <Status value={status} testId="status" />
        </p>
      </section>

      {pending && sessionId ? <ApprovalPanel approval={pending} sessionId={sessionId} onResolved={() => setPending(null)} /> : null}

      <section className="panel" aria-labelledby="message-h">
        <h2 id="message-h">Assistant message</h2>
        <p data-testid="stream-text">{streamText}</p>
      </section>

      <section className="panel" aria-labelledby="tools-h">
        <h2 id="tools-h">Tool &amp; subagent activity</h2>
        {tools.length === 0 ? (
          <p className="muted">None yet.</p>
        ) : (
          <ul data-testid="tool-activity">
            {tools.map((t, i) => (
              <li key={i}>
                <strong>{t.type}</strong> {t.name ?? ""}
                {t.arguments ? <pre className="code">{JSON.stringify(t.arguments)}</pre> : null}
                {t.result !== null && t.result !== undefined ? <span data-testid="tool-result"> → {JSON.stringify(t.result)}</span> : null}
              </li>
            ))}
          </ul>
        )}
      </section>

      <section className="panel" aria-labelledby="recovery-h">
        <h2 id="recovery-h">Recovery &amp; attempts</h2>
        {recovery.length === 0 ? (
          <p className="muted">No recovery transitions.</p>
        ) : (
          <ul data-testid="recovery-display">
            {recovery.map((r, i) => (
              <li key={i}>
                <Status value={r.type} /> {r.detail ? String(r.detail) : ""}
              </li>
            ))}
          </ul>
        )}
      </section>

      <section className="panel" aria-labelledby="usage-h">
        <h2 id="usage-h">Usage</h2>
        <p data-testid="usage">{usage ? `total ${usage.total_tokens ?? 0} tokens, ${usage.tool_calls ?? 0} tool calls` : "—"}</p>
      </section>

      {terminal ? (
        <section className="panel" aria-labelledby="terminal-h">
          <h2 id="terminal-h">Terminal result</h2>
          <p>
            <Status value={terminal.status} testId="terminal-status" />
          </p>
          <p data-testid="model">Model: {terminal.model ?? "—"}</p>
          <pre className="code" data-testid="final-output">
            {JSON.stringify(terminal.output, null, 2)}
          </pre>
          {/* THE SESSION THIS RUN BELONGS TO, AND THE ONE CONTROL THAT MAKES THE DISTINCTION LEGIBLE.
              Reading "sess_…" on the screen taught an operator nothing; being able to send the next prompt
              INTO it is what shows that a run is a turn rather than a shot. It sets the picker above rather
              than starting anything, so the operator still types the prompt and still presses the button —
              a one-click "continue" would be a second way to start a run, and this page has one. */}
          {sessionId === "" ? null : (
            <p className="run-continue" data-testid="run-continue-row">
              This run is a turn in session <code data-testid="terminal-session">{sessionId}</code>.{" "}
              {chainTo === sessionId ? (
                <span data-testid="run-continue-armed">
                  The next run continues it — type a prompt above and start it.
                </span>
              ) : (
                <button
                  type="button"
                  data-testid="run-continue"
                  onClick={() => {
                    setChainTo(sessionId);
                  }}
                >
                  Continue this conversation
                </button>
              )}
            </p>
          )}
        </section>
      ) : null}

      <section className="panel" aria-labelledby="artifacts-h">
        <h2 id="artifacts-h">Artifacts</h2>
        {artifacts.length === 0 ? (
          <p className="muted">No artifacts.</p>
        ) : (
          <ul data-testid="artifact-list">
            {artifacts.map((a) => (
              <li key={a.id}>
                <a data-testid="artifact-download" href={artifactHref(a.id)} download>
                  {a.id}
                </a>
              </li>
            ))}
          </ul>
        )}
      </section>

      <Timeline frames={frames} />
    </>
  );
}
