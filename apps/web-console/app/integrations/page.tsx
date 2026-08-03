"use client";

import { useEffect, useRef, useState } from "react";

import { FormDialog } from "@/components/FormDialog";
import { Panel, type Column } from "@/components/Panel";
import { ResourceForm } from "@/components/ResourceForm";
import { SecretField, takeSecret } from "@/components/SecretField";
import { CopyButton, shortId, Stamp } from "@/components/Session";
import { Badge } from "@/components/ui/Badge";
import { Button } from "@/components/ui/Button";
import { apiGet, apiSend, RelayError } from "@/lib/api";
import { type AgentRevision, type AgentRow, lineageOf } from "@/lib/agents";
import { type ApiKeyRow, csvList, SLACK_SECRET_SLOTS, slackHandle, type SlackConnectionRow } from "@/lib/slack";

// SLACK — THE OTHER HALF OF "A FRESH MACHINE NEEDS NO FILE".
//
// WHAT WAS MEASURED, 2026-08-03:
//
//   grep -rn 'slack-connections' apps/web-console/app apps/web-console/lib   ->   0 hits
//   GET /v1/slack-connections on the live stack (127.0.0.1:60351)            ->   1 row, team T0AMPM5JX8U
//
// One workspace registered, and no screen in a nineteen-route console could have made it. It was made by
// `palai up`, reading SLACK_TEAM_ID, SLACK_SIGNING_SECRET, SLACK_BOT_TOKEN and SLACK_APP_TOKEN out of
// .env.local. That is the arrangement this branch exists to end: the owner has said three times that runtime
// configuration must come from the panel and not from a file on a machine.
//
// THE MECHANISM WAS ALREADY BUILT AND HAD NO DOOR — the same shape repo-token-path found for repository
// credentials a day earlier. POST /v1/slack-connections has existed since E19 T9, values are write-only
// behind POST /v1/secret-refs, and the registration body accepts *_ref HANDLES only: api/slack_connections.go
// declares slackRegistrationBody with no signing_secret, bot_token or app_token field at all, so
// DisallowUnknownFields turns an inline credential into a 400 at the boundary. What did not exist was a
// screen. This is it, and it is the two-phase shape /registry and /repositories already use — seal, then
// name — because that is the only shape the API accepts.
//
// THE HANDLE NAMES ARE up.go's. See lib/slack.ts's SLACK_SECRET_SLOTS for why copying them is the point
// rather than a shortcut: a console that named its handles differently would make a panel-registered
// workspace and a CLI-registered workspace resolve to different secrets, which is the PAIRING failure that
// table was written to close, re-opened from the other side.
//
// WHY IT IS ITS OWN ROUTE RATHER THAN A PANEL ON /tools. /tools is what a MODEL may call; a Slack workspace
// is a way for a HUMAN to reach this deployment. They are different questions, and the second one is about
// to have siblings — the queue connections surface (POST /v1/queue-connections) is the same shape and has
// the same missing screen.

const detail = (err: unknown, fallback: string) => (err instanceof RelayError ? err.problem.detail : fallback);

export default function IntegrationsPage() {
  const [reloadKey, setReloadKey] = useState(0);
  const [open, setOpen] = useState(false);

  const [teamID, setTeamID] = useState("");
  const [botUserID, setBotUserID] = useState("");
  const [agentID, setAgentID] = useState("");
  const [principalID, setPrincipalID] = useState("");
  const [approvers, setApprovers] = useState("");
  const [channels, setChannels] = useState("");

  // THE THREE CREDENTIALS ARE DOM NODES AND NOTHING ELSE — no useState, deliberately, and one ref per slot
  // so the array below stays parallel to SLACK_SECRET_SLOTS. components/SecretField.tsx carries the full
  // argument; the short form is that a controlled input makes the secret React state, which every re-render
  // closes over and which a Server-Component boundary can serialize into a flight payload.
  const signingRef = useRef<HTMLInputElement | null>(null);
  const botRef = useRef<HTMLInputElement | null>(null);
  const appRef = useRef<HTMLInputElement | null>(null);
  const secretRefs = [signingRef, botRef, appRef];

  const [error, setError] = useState("");
  const [status, setStatus] = useState("");
  const [creating, setCreating] = useState(false);

  // WHAT THE TWO RUN-TARGET PICKERS CHOOSE FROM. Both are read from the deployment rather than typed,
  // because vetSlackDefaultPolicy requires agent_revision_id AND principal_id and says why: "a binding that
  // has not been told what to run, or as whom, admits nothing". A typo in either is a workspace that
  // registers cleanly and then silently refuses every message.
  const [agents, setAgents] = useState<AgentRow[]>([]);
  const [keys, setKeys] = useState<ApiKeyRow[]>([]);
  useEffect(() => {
    let live = true;
    void apiGet<{ data?: AgentRow[] }>("/agents")
      .then((body) => {
        if (live) setAgents(body.data ?? []);
      })
      .catch(() => {
        // An unreadable list leaves the picker empty, which renders its own note rather than a dead control.
      });
    void apiGet<{ data?: ApiKeyRow[] }>("/api-keys")
      .then((body) => {
        if (live) setKeys(body.data ?? []);
      })
      .catch(() => {});
    return () => {
      live = false;
    };
  }, []);

  async function connect() {
    setCreating(true);
    setError("");
    setStatus("");

    // READ AND CLEAR ALL THREE CREDENTIALS FIRST, BEFORE ANY await AND BEFORE ANY VALIDATION THAT CAN RETURN
    // EARLY. takeSecret() reads and resets in one call, so the bytes exist as locals for the duration of this
    // function and are copied nowhere. Every early return below would otherwise leave three tokens sitting in
    // DOM nodes on a screen the operator has just been told to go and fix something on.
    //
    // The cost is real and chosen, the same trade SecretField argues: a refused submit means retyping them.
    const values = secretRefs.map((ref) => takeSecret(ref));
    const team = teamID.trim();

    if (team === "" || values[0] === "") {
      setError(
        team === ""
          ? "the workspace ID is required. It is the `T…` id on App → Basic Information, and every credential handle is named after it. Nothing was sent."
          : "the signing secret is required: it is what verifies that a callback really came from Slack, and the API refuses a registration without it. Nothing was sent.",
      );
      setCreating(false);
      return;
    }
    if (agentID === "" || principalID === "") {
      setError(
        agentID === ""
          ? "choose the agent this workspace runs. A binding that has not been told what to run admits nothing. Nothing was sent."
          : "choose the principal these runs are attributed to. A binding that has not been told who to run as admits nothing. Nothing was sent.",
      );
      setCreating(false);
      return;
    }

    // THE AGENT'S PUBLISHED REVISION, RESOLVED HERE RATHER THAN ASKED FOR. The policy field is a REVISION id,
    // and published is not a nicety: admission verifies published_at before pinning a run, so a draft would
    // register a workspace that admits nothing — today's silent failure with one more step in front of it.
    // Asking an operator to pick a revision number would be asking them to know that rule.
    let revisionID: string;
    try {
      const body = await apiGet<{ data?: AgentRevision[] }>(`/agents/${encodeURIComponent(agentID)}/revisions`);
      const published = lineageOf(body.data ?? []).published;
      if (published === null) {
        setError(
          `that agent has no PUBLISHED revision, and a workspace can only be pinned to one — admission checks it before ` +
            `starting a run, so registering against a draft would create a workspace that silently admits nothing. ` +
            `Publish a revision on the agent's own page first. Nothing was sent.`,
        );
        setCreating(false);
        return;
      }
      revisionID = String(published.id ?? "");
    } catch (err: unknown) {
      setError(`that agent's revisions could not be read: ${detail(err, "the request was refused")}. Nothing was sent.`);
      setCreating(false);
      return;
    }

    // SEAL. One call per slot that carries a value; a slot left empty is not sealed and its handle is not
    // registered, so a handle is never registered for a credential the operator does not have — up.go's rule,
    // and the reason is that an unresolvable handle fails SILENTLY at every consumer.
    const handles: Record<string, string> = {};
    const sealedNames: string[] = [];
    for (const [i, slot] of SLACK_SECRET_SLOTS.entries()) {
      const value = values[i];
      if (value === "") continue;
      const name = slackHandle(slot.prefix, team);
      try {
        await apiSend("POST", "/secret-refs", { name, value });
      } catch (err: unknown) {
        // A refused seal is reported as its own failure rather than folded into the registration's — and it
        // names what has ALREADY been sealed, because a failure on the second of three leaves the first one
        // live under a handle that binds nothing.
        setError(
          `the ${slot.label.toLowerCase()} could not be sealed: ${detail(err, "the secret ref was refused")}. ` +
            (sealedNames.length === 0
              ? "No workspace was registered and nothing was stored."
              : `No workspace was registered. Already sealed: ${sealedNames.join(", ")} — re-submitting the same values ` +
                "rotates them rather than creating duplicates."),
        );
        setCreating(false);
        return;
      }
      handles[slot.field] = name;
      sealedNames.push(name);
    }

    try {
      const body = await apiSend<{ id?: string }>("POST", "/slack-connections", {
        team_id: team,
        // HANDLES, NEVER VALUES. The API has no field for a raw credential, so this object structurally
        // cannot carry one — and the console does not attempt it, because a 400 an operator has to decode is
        // a worse outcome than a form that cannot produce one.
        ...handles,
        ...(botUserID.trim() === "" ? {} : { bot_user_id: botUserID.trim() }),
        // BOTH SCOPES ARE ABSENT-BY-DEFAULT, and the two defaults are OPPOSITE — see the field hints.
        ...(csvList(channels).length === 0 ? {} : { allowed_channels: csvList(channels) }),
        ...(csvList(approvers).length === 0 ? {} : { allowed_users: csvList(approvers) }),
        default_policy: { agent_revision_id: revisionID, principal_id: principalID },
      });
      setStatus(
        `Slack workspace ${team} is registered as ${String(body.id ?? "?")}. ` +
          `Its credentials are sealed in this deployment's secret store as ${sealedNames.join(", ")} — named here, ` +
          "readable from nowhere, and on no machine's disk. " +
          (handles.app_token_ref === undefined
            ? "No app-level token was given, so Socket Mode stays dormant: this workspace receives events only if Slack can reach this deployment over HTTP. "
            : "") +
          (csvList(approvers).length === 0
            ? "It names NO approvers, and approval is deny-by-default — nobody in this workspace can approve a gated tool call until you add some. "
            : "") +
          "Nothing has been called: the first thing that exercises this is a message in Slack.",
      );
      setOpen(false);
      setTeamID("");
      setBotUserID("");
      setApprovers("");
      setChannels("");
      setReloadKey((n) => n + 1);
    } catch (err: unknown) {
      // THE CREDENTIALS SURVIVE THIS FAILURE AND THE OPERATOR IS TOLD SO, BY NAME. The seals were written
      // before the registration was refused, so the values are sealed under handles that now bind nothing —
      // reporting only "the registration failed" would leave live credentials the operator does not know
      // exist. This is /registry's and /repositories' sentence, on the surface that now has the same shape.
      setError(
        `${detail(err, "the workspace could not be registered")}` +
          (sealedNames.length === 0
            ? ""
            : ` — the credentials were already sealed as ${sealedNames.join(", ")}, so they are safe: fix the field ` +
              "above and submit again with the same values. (Re-sealing a name is a rotation, not a duplicate.)"),
      );
    } finally {
      setCreating(false);
    }
  }

  const agentOptions = agents
    .filter((a) => typeof a.id === "string" && a.id !== "")
    .map((a) => ({ value: String(a.id), label: `${String(a.name ?? "(unnamed)")} (${shortId(String(a.id))})` }));

  // LIVE KEYS ONLY. A revoked key's principal is still a principal the store would accept, but offering it
  // would be offering an identity whose operator has already been told it is gone.
  //
  // BOTH ABSENT SHAPES COUNT AS LIVE, and that is measured rather than defensive. The live control plane
  // OMITS `revoked_at` on a key that is not revoked (GET /v1/api-keys on 127.0.0.1:60351: key_local carries
  // no such field, and a revoked sibling carries it as a timestamp), while the deterministic fixture writes
  // `revoked_at: null`. A `=== undefined` test alone is therefore right on one profile and silently empties
  // the picker on the other — a control that renders nothing at all, which reads as "this deployment has no
  // principals" rather than as a bug.
  //
  // Two shapes for one fact is worth flagging as an API observation rather than absorbing quietly: the
  // projection would be easier to consume if it omitted the field on both sides or nulled it on both.
  const principalOptions = keys
    .filter((k) => typeof k.principal_id === "string" && k.principal_id !== "" && (k.revoked_at === undefined || k.revoked_at === null))
    .map((k) => ({ value: String(k.principal_id), label: `${String(k.id ?? "")} — ${String(k.principal_id)}` }));

  const columns: Column<SlackConnectionRow>[] = [
    {
      header: "ID",
      sort: (r) => String(r.id ?? ""),
      render: (r) => (
        <span className="cell-id-group">
          <code className="cell-id" title={String(r.id ?? "")}>
            {shortId(String(r.id ?? ""))}
          </code>
          <CopyButton value={String(r.id ?? "")} label="connection ID" testId="slack-copy-id" />
        </span>
      ),
    },
    {
      header: "Workspace",
      sort: (r) => String(r.team_id ?? ""),
      render: (r) => <code>{String(r.team_id ?? "")}</code>,
    },
    {
      header: "Bot user",
      render: (r) => (String(r.bot_user_id ?? "") === "" ? <span className="cell-none">— not set</span> : <code>{String(r.bot_user_id)}</code>),
    },
    {
      // A DISABLED CONNECTION IS NOT A COSMETIC STATE: it admits nothing. It gets a word rather than a
      // colour, because "disabled" and "registered" are the difference between a workspace that answers and
      // one that does not.
      header: "State",
      // The glyph is required and it is the point: components/Status.tsx's header records that the meaning
      // lives in the GLYPH and the WORD, with colour as a redundant third layer. `⊘` and `✔︎` are the two
      // that classifier already uses, in text presentation.
      render: (r) =>
        r.disabled === true ? (
          <Badge tone="warn" glyph="⊘" title="disabled">
            disabled — admits nothing
          </Badge>
        ) : (
          <Badge tone="ok" glyph="✔︎" title="registered">
            registered
          </Badge>
        ),
    },
    {
      header: "Created",
      sort: (r) => String(r.created_at ?? ""),
      render: (r) => <Stamp iso={typeof r.created_at === "string" ? r.created_at : null} />,
    },
  ];

  return (
    <>
      {status === "" ? null : (
        <p className="form-status" data-testid="slack-connect-status">
          <span className="glyph" aria-hidden="true">
            ✔
          </span>{" "}
          {status}
        </p>
      )}

      <Panel<SlackConnectionRow>
        title="Slack workspaces"
        testId="panel-slack-connections"
        pageTitle
        fetchPath="/slack-connections"
        reloadKey={reloadKey}
        columns={columns}
        matchOn={(r) => `${String(r.id ?? "")} ${String(r.team_id ?? "")}`}
        filterLabel="Search workspaces by ID or team"
        filterPlaceholder="Workspace or ID…"
        action={
          <Button variant="primary" testId="slack-connect-open" onClick={() => setOpen(true)}>
            + Connect a workspace
          </Button>
        }
        emptyNote={
          <>
            <p className="empty-title" data-testid="slack-empty-title">
              No Slack workspaces connected
            </p>
            <p className="empty-body">
              Connecting one lets people in a Slack workspace start runs by mentioning the bot, and decide the
              approvals those runs park on. The credentials are sealed into this deployment&apos;s secret
              store — they are not written to any machine&apos;s disk and they travel with the deployment.
            </p>
            <Button variant="primary" testId="slack-connect-open-empty" onClick={() => setOpen(true)}>
              Connect a workspace
            </Button>
          </>
        }
      />

      {open ? (
        <FormDialog
          label="Connect a Slack workspace"
          testId="slack-connect-dialog"
          onClose={() => {
            setOpen(false);
            setError("");
          }}
        >
          <ResourceForm
            title="Connect a Slack workspace"
            testId="slack-connect"
            note={
              <span data-testid="slack-reachability-note">
                <strong>
                  Nothing here is checked against Slack: no credential is verified, no workspace is contacted,
                  and no permission is tested
                </strong>{" "}
                — registering builds the binding, and the first thing that exercises it is a real message. A
                wrong signing secret shows up as every callback being refused; a wrong bot token as a bot that
                receives and cannot reply.
              </span>
            }
            caveat={{
              summary: "Where these credentials go, and where to find each one",
              body: (
                <p className="muted">
                  Each value is sealed by <code>POST /v1/secret-refs</code> under a name derived from the
                  workspace ID, and the workspace registration then stores only that NAME. The registration
                  API has no field for a raw credential at all, so a value cannot reach it even by mistake.
                  Nothing here is readable back — not from this console and not from any route. These are the
                  same handle names <code>palai up</code> uses, so a workspace connected here and one
                  connected by the CLI resolve to the same secrets rather than two copies.
                </p>
              ),
            }}
            fields={[
              {
                name: "slack-team",
                label: "Workspace ID",
                required: true,
                value: teamID,
                onChange: setTeamID,
                testId: "slack-team-input",
                hint: "The `T…` id from App → Basic Information. Every credential below is sealed under a name derived from it, so it must be the real workspace id rather than a label.",
              },
              {
                name: "slack-agent",
                label: "Agent to run",
                kind: "select",
                required: true,
                value: agentID,
                onChange: setAgentID,
                options: agentOptions,
                placeholder: "Choose an agent…",
                testId: "slack-agent-select",
                emptyNote: (
                  <span className="muted">
                    There are no agents yet. A workspace must be told what to run before it can admit
                    anything — create one on the Agents screen and publish a revision first.
                  </span>
                ),
                manage: { href: "/agents", label: "Agents" },
                hint: "This workspace pins to the agent's newest PUBLISHED revision, resolved when you submit. An agent whose revisions are all drafts is refused rather than registered, because admission checks published_at before starting a run.",
              },
              {
                name: "slack-principal",
                label: "Run as principal",
                kind: "select",
                required: true,
                value: principalID,
                onChange: setPrincipalID,
                options: principalOptions,
                placeholder: "Choose a principal…",
                testId: "slack-principal-select",
                emptyNote: <span className="muted">No live API key was readable, so there is no principal to attribute these runs to.</span>,
                manage: { href: "/policy", label: "Policy & keys" },
                hint: "Who runs started from Slack are attributed to, listed by the API key that carries each principal. This is an identity for accounting and policy — it is not how a Slack user is authenticated.",
              },
              {
                name: "slack-bot-user",
                label: "Bot user ID",
                value: botUserID,
                onChange: setBotUserID,
                testId: "slack-bot-user-input",
                hint: "Optional, `U…`. It is how the bot recognises mentions of itself; without it a mention may not be matched.",
              },
              {
                name: "slack-approvers",
                label: "Approvers",
                value: approvers,
                onChange: setApprovers,
                testId: "slack-approvers-input",
                hint: "Comma-separated Slack user ids (U…). Approval is DENY-BY-DEFAULT: leave this empty and nobody in the workspace can approve a gated tool call.",
              },
              {
                name: "slack-channels",
                label: "Allowed channels",
                value: channels,
                onChange: setChannels,
                testId: "slack-channels-input",
                // The OPPOSITE default to the field above, and saying so is the point: two adjacent list
                // fields whose empty state means opposite things is exactly where an operator guesses wrong.
                hint: "Comma-separated channel ids. Empty means NO restriction — every channel the bot is in, which is the production default. This is the opposite of Approvers above.",
              },
            ]}
            submitLabel="Connect workspace"
            submittingLabel="Connecting…"
            submitTestId="slack-connect-button"
            submitting={creating}
            error={error}
            onSubmit={connect}
            actions={
              <Button
                testId="slack-connect-cancel"
                onClick={() => {
                  setOpen(false);
                  setError("");
                }}
              >
                Cancel
              </Button>
            }
          >
            {/* THE CREDENTIALS ARE CHILDREN RATHER THAN FormFields, and that is ResourceForm's own rule
                rather than a shortcut: FormField's contract is a controlled value/onChange pair, and putting
                a secret through it would make the secret React state. They still render INSIDE the form and
                after the fields, so they stay in document order and therefore in tab order. */}
            {SLACK_SECRET_SLOTS.map((slot, i) => (
              <SecretField
                key={slot.field}
                inputRef={secretRefs[i]}
                id={`field-${slot.field}`}
                label={slot.label}
                testId={slot.testId}
                required={slot.required}
                hint={slot.hint}
              />
            ))}
          </ResourceForm>
        </FormDialog>
      ) : null}
    </>
  );
}
