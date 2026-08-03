"use client";

import { useEffect, useRef, useState } from "react";

import { FormDialog } from "@/components/FormDialog";
import { NameCell, Panel, type Column } from "@/components/Panel";
import { ResourceForm, type FormField } from "@/components/ResourceForm";
import { SecretField, takeSecret } from "@/components/SecretField";
import { CopyButton, shortId, Stamp } from "@/components/Session";
import { Badge } from "@/components/ui/Badge";
import { Button } from "@/components/ui/Button";
import { type AgentRevision, type AgentRow, lineageOf } from "@/lib/agents";
import { apiGet, apiSend, RelayError } from "@/lib/api";
import { type BotBindingRow, type BotRow, namedHandles } from "@/lib/bots";
import { CHANNELS, channelHandle, channelLabel, channelOf, csvList } from "@/lib/channels";

// THE BOTS SCREEN — one row per relay process this deployment can run (2026-08-03 plan, Task 11).
//
// WHAT A BOT IS, in the owner's own words: "bot ekranı yazarsın botlar listelenir, new bot'a tıklarsın
// slack liste açılır slack çıkar ileride whatsapp telegram x falan da yaparız çünkü" — you write a bot
// screen, bots are listed, you click new bot, a channel list opens and Slack appears; later there will be
// WhatsApp, Telegram, X. That last clause is the whole design constraint, and it is why the channel is
// CHOSEN FROM A LIST rather than assumed: `kind === "slack"` appears nowhere on this screen, and every
// channel-specific fact lives in lib/channels.ts. Adding a channel is a row there.
//
// THIS SCREEN REPLACES app/integrations, AND THE REPLACEMENT IS A DIFFERENT ARCHITECTURE RATHER THAN A
// RENAME. /integrations wrote `slack_connections`, which the control plane's own in-process Slack adapter
// consumes; this writes `integration_bots` (migration 000061), a registry the control plane never
// interprets and a SEPARATE relay process (apps/slack-bot) reads at startup through the SDK. Two screens
// for one operator intent is the thing the owner has refused, so there is one — and the shipped
// in-process path keeps running off the row it already has until Task 14 removes it.
//
// WHAT CARRIED OVER FROM THAT SCREEN, DELIBERATELY AND IN FULL: the two-phase secret flow (seal with
// POST /v1/secret-refs, then name the handle), up.go's handle NAMES, the published-revision resolution,
// and the four properties tests/slack-workspace.spec.ts pinned — which are now tests/bots.spec.ts's.
//
// WHAT IS WEAKER HERE THAN THERE, and it is the one thing on this page that has to be said plainly:
// POST /v1/slack-connections could not be handed a raw credential — the body declared no field for one,
// so DisallowUnknownFields refused it at the edge. POST /v1/bots CAN be: `config` is opaque, which is
// exactly the property that lets a new channel arrive without a control-plane change. The boundary is
// gone and only this page's discipline is left, so tests/bots.spec.ts asserts on the WIRE that no
// credential ever reaches the registration, rather than trusting that it cannot.
//
// WHAT THIS SCREEN DOES NOT OFFER, because nothing reads it: `principal_id`. The column exists on the row
// and the SDK carries it, and `grep -rn 'principal_id\|PrincipalID' apps/slack-bot sdks/go/bots.go` →
// ONE hit, the SDK's own struct field (2026-08-03). The relay authenticates with its own API key and the
// deciding principal on an approval is `key:<api_key_id>` (relay/approvals.go), so a principal chosen
// here would be a value nothing redeems. The approver list below is the boundary that IS read.

const detail = (err: unknown, fallback: string) => (err instanceof RelayError ? err.problem.detail : fallback);

// THE CHANNEL THE DIALOG OPENS ON, and it is a default rather than a blank on purpose — for two reasons,
// one of them a recorded defect in this tree.
//
// The operator reason: with one connectable channel, an empty selector asks a question that has one
// answer. The list still shows what is coming, and changing it is one click.
//
// The measurement reason: the credential fields belong to the CHOSEN channel, so on a blank selector they
// do not exist in the DOM — and the generated sweeps (tests/a11y.spec.ts, tests/contrast.spec.ts) open
// this dialog and judge what is in it. That is precisely the shape recorded on 2026-08-01, when five
// FormDialogs took 144 controls out of the contrast sweep and it reported a CLEANER number while covering
// less. Opening on a channel keeps every control this form can render in front of both sweeps.
const DEFAULT_CHANNEL = CHANNELS.find((c) => c.enabled)?.id ?? "";

export default function BotsPage() {
  const [reloadKey, setReloadKey] = useState(0);
  const [open, setOpen] = useState(false);

  const [kind, setKind] = useState(DEFAULT_CHANNEL);
  const [botName, setBotName] = useState("");
  const [agentID, setAgentID] = useState("");
  const [bindingID, setBindingID] = useState("");
  // The chosen channel's own fields, keyed by the `config` key each one writes. One map rather than a
  // useState per field, because the FIELD SET is the channel's and changes when the channel does.
  const [channelValues, setChannelValues] = useState<Record<string, string>>({});

  const [error, setError] = useState("");
  const [status, setStatus] = useState("");
  const [creating, setCreating] = useState(false);

  // THE CREDENTIALS ARE DOM NODES AND NOTHING ELSE — no useState, deliberately, one ref object per slot.
  // components/SecretField.tsx carries the full argument; the short form is that a controlled input makes
  // the secret React state, which every re-render closes over and which a Server-Component boundary can
  // serialize into a flight payload. The refs are keyed by slot because the SET of slots is the channel's:
  // a fixed array would be a channel-shaped assumption in the one file that must not hold one.
  const secretRefs = useRef<Record<string, { current: HTMLInputElement | null }>>({});
  const secretRef = (field: string) => (secretRefs.current[field] ??= { current: null });

  const channel = channelOf(kind);
  const form = channel?.form;

  // WHAT THE TWO RUN-TARGET PICKERS CHOOSE FROM. Both are read from the deployment rather than typed: the
  // relay puts `agent_revision_id` and `repository_binding_id` straight onto its Responses.Create call
  // (relay/inbound.go createResponse), so a typo in either is a bot that registers cleanly and then fails
  // on its first message with a refusal about something else.
  const [agents, setAgents] = useState<AgentRow[]>([]);
  const [bindings, setBindings] = useState<BotBindingRow[]>([]);
  useEffect(() => {
    let live = true;
    void apiGet<{ data?: AgentRow[] }>("/agents")
      .then((body) => {
        if (live) setAgents(body.data ?? []);
      })
      .catch(() => {
        // An unreadable list leaves the picker empty, which renders its own note rather than a dead control.
      });
    void apiGet<{ data?: BotBindingRow[] }>("/repository-bindings")
      .then((body) => {
        if (live) setBindings(body.data ?? []);
      })
      .catch(() => {});
    return () => {
      live = false;
    };
  }, []);

  async function create() {
    setCreating(true);
    setError("");
    setStatus("");

    // READ AND CLEAR EVERY CREDENTIAL FIRST, BEFORE ANY await AND BEFORE ANY VALIDATION THAT CAN RETURN
    // EARLY. takeSecret() reads and resets in one call, so the bytes exist as locals for the duration of
    // this function and are copied nowhere. Every early return below would otherwise leave tokens sitting
    // in DOM nodes on a screen the operator has just been told to go and fix something on.
    //
    // The cost is real and chosen, the same trade SecretField argues: a refused submit means retyping them.
    const slots = form?.secrets ?? [];
    const values = slots.map((slot) => takeSecret(secretRef(slot.field)));

    if (channel === undefined || !channel.enabled || form === undefined) {
      setError("choose a channel this bot speaks on. Nothing was sent.");
      setCreating(false);
      return;
    }
    const name = botName.trim();
    if (name === "") {
      setError("give the bot a name. It is unique within this project and it is how you will find it here. Nothing was sent.");
      setCreating(false);
      return;
    }
    const missing = form.fields.find((f) => f.required === true && (channelValues[f.name] ?? "").trim() === "");
    if (missing !== undefined) {
      setError(`${missing.label} is required for a ${channel.label} bot, and every credential below is sealed under a name derived from it. Nothing was sent.`);
      setCreating(false);
      return;
    }

    // THE AGENT'S PUBLISHED REVISION, RESOLVED HERE RATHER THAN ASKED FOR. The registry's field is a
    // REVISION id, and published is not a nicety: admission verifies published_at before pinning a run, so
    // a draft would register a bot that answers every message with a refusal. Asking an operator to pick a
    // revision number would be asking them to know that rule.
    let revisionID = "";
    if (agentID !== "") {
      try {
        const body = await apiGet<{ data?: AgentRevision[] }>(`/agents/${encodeURIComponent(agentID)}/revisions`);
        const published = lineageOf(body.data ?? []).published;
        if (published === null) {
          setError(
            "that agent has no PUBLISHED revision, and a bot can only be pinned to one — admission checks it before " +
              "starting a run, so pinning a draft would create a bot that refuses every message. Publish a revision on " +
              "the agent's own page first, or leave the agent empty. Nothing was sent.",
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
    }

    // SEAL. One call per slot that carries a value; a slot left empty is not sealed and its handle is not
    // written, so a handle never names a credential the operator does not have — up.go's rule, and the
    // reason is that an unresolvable handle fails SILENTLY at every consumer.
    const scope = (channelValues[form.handleKey] ?? "").trim();
    const handles: Record<string, string> = {};
    const sealedNames: string[] = [];
    for (const [i, slot] of form.secrets.entries()) {
      const value = values[i];
      if (value === "") continue;
      const handle = channelHandle(slot.prefix, scope);
      try {
        await apiSend("POST", "/secret-refs", { name: handle, value });
      } catch (err: unknown) {
        // A refused seal is reported as its own failure rather than folded into the registration's — and it
        // names what has ALREADY been sealed, because a failure on the second of three leaves the first one
        // live under a handle that binds nothing.
        setError(
          `the ${slot.label.toLowerCase()} could not be sealed: ${detail(err, "the secret ref was refused")}. ` +
            (sealedNames.length === 0
              ? "No bot was created and nothing was stored."
              : `No bot was created. Already sealed: ${sealedNames.join(", ")} — re-submitting the same values rotates ` +
                "them rather than creating duplicates."),
        );
        setCreating(false);
        return;
      }
      handles[slot.field] = handle;
      sealedNames.push(handle);
    }

    // THE CHANNEL'S DOCUMENT. Every key here is one the channel table declares and names a reader for; the
    // control plane stores it verbatim and never looks inside (migration 000061: `json`, not `jsonb`, so
    // not even the key order is reshaped).
    const config: Record<string, unknown> = {};
    for (const field of form.fields) {
      const raw = (channelValues[field.name] ?? "").trim();
      if (raw === "") continue;
      config[field.name] = field.list === true ? csvList(raw) : raw;
    }
    // HANDLES, NEVER VALUES. Nothing at the edge would stop a token going in here — `config` is opaque by
    // design — so this line is the boundary, and tests/bots.spec.ts reads the wire rather than this comment.
    Object.assign(config, handles);

    try {
      const body = await apiSend<{ id?: string }>("POST", "/bots", {
        name,
        kind: channel.id,
        // OMITTED WHEN EMPTY rather than sent as "", matching what the relay does with the same two fields
        // on the way out (relay/inbound.go createResponse) — an absent field reads as "not chosen", which
        // is what it means, on a row whose columns default to ''.
        ...(revisionID === "" ? {} : { agent_revision_id: revisionID }),
        ...(bindingID === "" ? {} : { repository_binding_id: bindingID }),
        config,
      });
      setStatus(
        `Bot ${name} is registered as ${String(body.id ?? "?")}. ` +
          (sealedNames.length === 0
            ? "No credential was sealed, so nothing can connect yet — open the bot and add its tokens. "
            : `Its credentials are sealed in this deployment's secret store as ${sealedNames.join(", ")} — named here, ` +
              "readable from nowhere, and on no machine's disk. ") +
          `Nothing has been contacted: a bot is a row until a relay process is started with PALAI_BOT_ID=${String(body.id ?? "?")}.`,
      );
      setOpen(false);
      setBotName("");
      setChannelValues({});
      setReloadKey((n) => n + 1);
    } catch (err: unknown) {
      // THE CREDENTIALS SURVIVE THIS FAILURE AND THE OPERATOR IS TOLD SO, BY NAME. The seals were written
      // before the registration was refused, so the values are sealed under handles that now bind nothing —
      // reporting only "the bot could not be created" would leave live credentials the operator does not
      // know exist. This is /registry's and /repositories' sentence, on the surface that now has its shape.
      setError(
        `${detail(err, "the bot could not be created")}` +
          (sealedNames.length === 0
            ? ""
            : ` — the credentials were already sealed as ${sealedNames.join(", ")}, so they are safe: fix the field ` +
              "above and submit again with the same values. (Re-sealing a name is a rotation, not a duplicate.)"),
      );
    } finally {
      setCreating(false);
    }
  }

  // THE CHANNEL LIST, AS THE CONTROL SEES IT. A channel with no adapter is a row that is SHOWN and cannot
  // be chosen, with the reason in its own label — a roadmap rather than a promise, and never a hidden thing.
  const channelOptions = CHANNELS.map((c) => ({
    value: c.id,
    label: c.enabled ? c.label : `${c.label} — ${String(c.note ?? "not available")}`,
    disabled: !c.enabled,
  }));

  const agentOptions = agents
    .filter((a) => typeof a.id === "string" && a.id !== "")
    .map((a) => ({ value: String(a.id), label: `${String(a.name ?? "(unnamed)")} (${shortId(String(a.id))})` }));

  // LIVE BINDINGS ONLY. An archived binding still resolves, and offering one would offer a repository whose
  // operator has already been told it is retired.
  const bindingOptions = bindings
    .filter((b) => typeof b.id === "string" && b.id !== "" && (b.archived_at === undefined || b.archived_at === null))
    .map((b) => ({ value: String(b.id), label: `${String(b.repository_identity ?? "(unnamed)")} (${shortId(String(b.id))})` }));

  const columns: Column<BotRow>[] = [
    {
      header: "Bot",
      sort: (r) => String(r.name ?? ""),
      render: (r) => (
        <span className="cell-id-group">
          <NameCell name={String(r.name ?? "")} id={shortId(String(r.id ?? ""))} />
          <CopyButton value={String(r.id ?? "")} label="bot ID" testId="bot-copy-id" />
        </span>
      ),
    },
    {
      // THE RAW KIND WHEN THIS CONSOLE HAS NO WORD FOR IT. POST /v1/bots accepts any kind by design, so a
      // cell that rendered an unknown one as blank would make this console the thing that breaks the rule.
      header: "Channel",
      sort: (r) => String(r.kind ?? ""),
      render: (r) => <span>{channelLabel(String(r.kind ?? ""))}</span>,
    },
    {
      header: "Credentials",
      render: (r) => {
        const slots = channelOf(String(r.kind ?? ""))?.form?.secrets ?? [];
        if (slots.length === 0) return <span className="cell-none">—</span>;
        const named = namedHandles(r.config, slots.map((s) => s.field));
        return named === 0 ? (
          <span className="cell-none">— none named</span>
        ) : (
          <span>
            {named} of {slots.length} named
          </span>
        );
      },
    },
    {
      header: "Agent revision",
      render: (r) =>
        String(r.agent_revision_id ?? "") === "" ? (
          <span className="cell-none">— not pinned</span>
        ) : (
          <code title={String(r.agent_revision_id)}>{shortId(String(r.agent_revision_id))}</code>
        ),
    },
    {
      header: "Repository",
      render: (r) =>
        String(r.repository_binding_id ?? "") === "" ? (
          <span className="cell-none">— none</span>
        ) : (
          <code title={String(r.repository_binding_id)}>{shortId(String(r.repository_binding_id))}</code>
        ),
    },
    {
      // A DISABLED BOT IS NOT A COSMETIC STATE: the relay process REFUSES TO START against a disabled row
      // ("enable it in the admin panel before starting this process", apps/slack-bot/main.go). It gets a
      // glyph and a word rather than a colour, which is components/Status.tsx's rule.
      header: "State",
      render: (r) =>
        r.disabled === true ? (
          <Badge tone="warn" glyph="⊘" title="disabled">
            disabled — will not start
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

  const channelFields: FormField[] = (form?.fields ?? []).map((field) => ({
    name: field.name,
    label: field.label,
    required: field.required,
    hint: field.hint,
    testId: field.testId,
    value: channelValues[field.name] ?? "",
    onChange: (value: string) => setChannelValues((prev) => ({ ...prev, [field.name]: value })),
  }));

  return (
    <>
      {status === "" ? null : (
        <p className="form-status" data-testid="bot-create-status">
          <span className="glyph" aria-hidden="true">
            ✔
          </span>{" "}
          {status}
        </p>
      )}

      <Panel<BotRow>
        title="Bots"
        testId="panel-bots"
        pageTitle
        fetchPath="/bots"
        reloadKey={reloadKey}
        columns={columns}
        matchOn={(r) => `${String(r.id ?? "")} ${String(r.name ?? "")} ${String(r.kind ?? "")}`}
        filterLabel="Search bots by name, ID or channel"
        filterPlaceholder="Bot, ID or channel…"
        action={
          <Button variant="primary" testId="bot-create-open" onClick={() => setOpen(true)}>
            + New bot
          </Button>
        }
        emptyNote={
          <>
            <p className="empty-title" data-testid="bot-empty-title">
              No bots yet
            </p>
            <p className="empty-body">
              A bot lets people in a chat workspace start runs by mentioning it, and decide the approvals
              those runs park on. Creating one registers a row and seals its credentials into this
              deployment&apos;s secret store — they are not written to any machine&apos;s disk.
            </p>
            <Button variant="primary" testId="bot-create-open-empty" onClick={() => setOpen(true)}>
              New bot
            </Button>
          </>
        }
      />

      {open ? (
        <FormDialog
          label="Create a bot"
          testId="bot-create-dialog"
          onClose={() => {
            setOpen(false);
            setError("");
          }}
        >
          <ResourceForm
            title="Create a bot"
            testId="bot-create"
            note={
              <span data-testid="bot-reachability-note">
                <strong>Nothing here is checked against the channel: no credential is verified, no workspace
                is contacted, and no permission is tested</strong> — creating a bot writes a row, and the
                first thing that exercises it is a relay process started with its id.
              </span>
            }
            caveat={{
              summary: "Where these credentials go, and what runs the bot",
              body: (
                <p className="muted">
                  Each value is sealed by <code>POST /v1/secret-refs</code> under a name derived from the
                  workspace it belongs to, and the bot row then stores only that NAME. Nothing here is
                  readable back — not from this console and not from any route. The row itself is inert: a
                  separate relay process reads it at startup (<code>PALAI_BOT_ID</code>) and is what opens
                  the connection, so a bot created here does nothing until that process is running.
                </p>
              ),
            }}
            fields={[
              {
                name: "bot-channel",
                label: "Channel",
                kind: "select",
                required: true,
                value: kind,
                onChange: (value) => {
                  setKind(value);
                  // The FIELDS belong to the channel, so their values do too — carrying a Slack workspace
                  // id into another channel's form would submit a value the operator never typed there.
                  setChannelValues({});
                },
                options: channelOptions,
                testId: "bot-channel-select",
                emptyNote: <span className="muted">This console offers no channels, which is a build fault rather than a deployment state.</span>,
                hint: "Where people talk to this bot. Channels with no adapter yet are listed and cannot be chosen.",
              },
              {
                name: "bot-name",
                label: "Name",
                required: true,
                value: botName,
                onChange: setBotName,
                testId: "bot-name-input",
                hint: "Unique within this project — a second bot by the same name is refused rather than created alongside it.",
              },
              {
                name: "bot-agent",
                label: "Agent",
                kind: "select",
                value: agentID,
                onChange: setAgentID,
                options: agentOptions,
                placeholder: "No agent — use this deployment's default",
                testId: "bot-agent-select",
                emptyNote: (
                  <span className="muted">
                    There are no agents yet, so this bot will run whatever a run started from /runs runs.
                  </span>
                ),
                manage: { href: "/agents", label: "Agents" },
                hint: "The bot pins to this agent's newest PUBLISHED revision, resolved when you submit; an agent whose revisions are all drafts is refused rather than pinned. Left empty, the relay omits the field and the run starts with this deployment's default.",
              },
              {
                name: "bot-repository",
                label: "Repository",
                kind: "select",
                value: bindingID,
                onChange: setBindingID,
                options: bindingOptions,
                placeholder: "No repository — the bot cannot write code",
                testId: "bot-repository-select",
                emptyNote: (
                  <span className="muted">
                    No repository binding is registered, so this bot can answer questions but cannot attach a
                    workspace to a run.
                  </span>
                ),
                manage: { href: "/repositories", label: "Repositories" },
                hint: "Attached to every run this bot starts. Leaving it empty is a deliberate non-coding bot, not an omission.",
              },
              ...channelFields,
            ]}
            submitLabel="Create bot"
            submittingLabel="Creating…"
            submitTestId="bot-create-button"
            submitting={creating}
            error={error}
            onSubmit={create}
            actions={
              <Button
                testId="bot-create-cancel"
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
            {(form?.secrets ?? []).map((slot) => (
              <SecretField
                key={slot.field}
                inputRef={secretRef(slot.field)}
                id={`field-${slot.field}`}
                label={slot.label}
                testId={slot.testId}
                required={slot.required === true}
                hint={slot.hint}
              />
            ))}
          </ResourceForm>
        </FormDialog>
      ) : null}
    </>
  );
}
