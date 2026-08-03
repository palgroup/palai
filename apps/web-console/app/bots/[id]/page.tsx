"use client";

import { useParams } from "next/navigation";
import { useEffect, useRef, useState } from "react";

import { FormDialog } from "@/components/FormDialog";
import { ResourceForm } from "@/components/ResourceForm";
import { SecretField, takeSecret } from "@/components/SecretField";
import { CopyButton, shortId, Stamp } from "@/components/Session";
import { Badge } from "@/components/ui/Badge";
import { Button } from "@/components/ui/Button";
import { Field, FieldControl } from "@/components/ui/Field";
import { apiGet, apiSend, RelayError } from "@/lib/api";
import { type ManifestPrompt } from "@/lib/botManifest";
import { type BotRow, namedHandles } from "@/lib/bots";
import { channelHandle, channelLabel, channelOf, csvList } from "@/lib/channels";

// ONE BOT — and the two steps between "a row exists" and "a person can talk to it" (2026-08-03 plan, Task 12).
//
// THE FLOW THIS SCREEN IS THE MIDDLE OF, in the owner's words: choose the channel, receive a manifest, go and
// install it, come back and paste the tokens, continue, then test from the channel itself. /bots does the
// choosing; this page hands over the manifest and takes the credentials back. The live test is Task 13.
//
// WHY THE MANIFEST COMES FIRST AND THE TOKENS SECOND: the tokens do not exist yet. A signing secret and a bot
// token are minted BY creating the app, so a screen that asked for them first would be asking for values the
// operator cannot have — and /bots' create dialog is deliberately willing to register a bot with none, for
// exactly that reason. The order here is the order the work happens in.
//
// `kind === "slack"` APPEARS NOWHERE ON THIS PAGE, the same rule /bots keeps. Every channel-specific word —
// what the document is called, where it is pasted, which values it carries and what they are capped at — is a
// row in lib/channels.ts, and this page renders whatever the chosen row declares. A channel with no manifest
// concept renders no manifest section; a channel with no adapter renders neither, and says so.
//
// NOTHING THE MANIFEST FORM HOLDS IS STORED, and that is a decision rather than an omission. `config` is a
// contract with a READER — lib/channels.ts names one for every key it writes, because a key nothing consumes
// is a key that lies about being configuration — and no reader anywhere consumes an app name or a suggested
// prompt: they are Slack's copy of the document once it is pasted. So the wizard's fields live for as long as
// the screen does, and re-opening it re-derives the same draft from the bot's own name.
//
// THE CREDENTIAL WRITE IS A MERGE AND THE CONTROL PLANE'S IS AN ASSIGNMENT, which is the one thing on this
// page that would silently destroy data if it were got wrong. `PATCH /v1/bots/{id}` sets
// `config = COALESCE($8, config)` (storage/queries/bots.sql UpdateBot): a document that is present REPLACES
// the stored one whole. Measured against the live stack on 2026-08-03, bot_7bc2…'s config carries five keys
// this console has never heard of — the registry's opacity is real and other writers use it — so the write
// below starts from the row as it was READ and overlays only what this screen owns. A console that sent just
// the handles would have deleted whatever else was in there.

const detail = (err: unknown, fallback: string) => (err instanceof RelayError ? err.problem.detail : fallback);

/** Value renders a field that may be ABSENT on the wire, saying which absence it is. */
function Value({ value, absent }: { value: unknown; absent: string }) {
  if (typeof value !== "string" || value === "") return <span className="cell-none">{absent}</span>;
  return <code>{value}</code>;
}

/** prefill reads a stored `config` value back into its box: a `list` field is an ARRAY on the wire. */
function prefill(value: unknown): string {
  if (Array.isArray(value)) return value.map((v) => String(v)).join(", ");
  return typeof value === "string" ? value : "";
}

export default function BotPage() {
  const params = useParams<{ id: string }>();
  const botID = Array.isArray(params.id) ? (params.id[0] ?? "") : (params.id ?? "");

  const [bot, setBot] = useState<BotRow | null>(null);
  const [error, setError] = useState("");

  // The manifest draft's own state. Seeded from the channel's declaration and the bot's name when the row
  // arrives, and never re-seeded after that — a re-read following a save must not throw away an edit the
  // operator has made but not yet pasted.
  const [manifestValues, setManifestValues] = useState<Record<string, string>>({});
  const [prompts, setPrompts] = useState<ManifestPrompt[]>([]);

  // The channel's own `config` fields, prefilled from the row so an operator returning to fix ONE of them is
  // not asked to retype the rest.
  const [channelValues, setChannelValues] = useState<Record<string, string>>({});
  const [credOpen, setCredOpen] = useState(false);
  const [credError, setCredError] = useState("");
  const [credStatus, setCredStatus] = useState("");
  const [saving, setSaving] = useState(false);

  // THE CREDENTIALS ARE DOM NODES AND NOTHING ELSE — no useState, one ref per slot, keyed by slot because the
  // SET of slots belongs to the channel. components/SecretField.tsx carries the whole argument.
  const secretRefs = useRef<Record<string, { current: HTMLInputElement | null }>>({});
  const secretRef = (field: string) => (secretRefs.current[field] ??= { current: null });

  useEffect(() => {
    if (botID === "") return;
    let live = true;
    apiGet<BotRow>(`/bots/${encodeURIComponent(botID)}`)
      .then((row) => {
        if (!live) return;
        setBot(row);
        const channel = channelOf(String(row.kind ?? ""));
        setManifestValues(
          Object.fromEntries(
            (channel?.manifest?.fields ?? []).map((f) => [f.name, f.fromBotName === true ? String(row.name ?? "") : (f.initial ?? "")]),
          ),
        );
        setPrompts((channel?.manifest?.prompts?.initial ?? []).map((p) => ({ ...p })));
        setChannelValues(Object.fromEntries((channel?.form?.fields ?? []).map((f) => [f.name, prefill(row.config?.[f.name])])));
      })
      .catch((err: unknown) => {
        // The SERVER's sentence: a 404 for an id that was never here and a 401 for a closed door are
        // different facts, and "the bot could not be loaded" is neither of them.
        if (live) setError(detail(err, "the bot could not be read"));
      });
    return () => {
      live = false;
    };
  }, [botID]);

  const channel = channelOf(String(bot?.kind ?? ""));
  const manifest = channel?.manifest;
  const form = channel?.form;
  const promptGroup = manifest?.prompts;
  const draft = manifest?.build(manifestValues, prompts);

  /** reread pulls the row back from the API rather than patching local state from a response body. */
  async function reread() {
    const row = await apiGet<BotRow>(`/bots/${encodeURIComponent(botID)}`);
    setBot(row);
    setChannelValues(Object.fromEntries((channelOf(String(row.kind ?? ""))?.form?.fields ?? []).map((f) => [f.name, prefill(row.config?.[f.name])])));
  }

  async function saveCredentials() {
    setSaving(true);
    setCredError("");
    setCredStatus("");

    // READ AND CLEAR EVERY CREDENTIAL FIRST, BEFORE ANY await AND BEFORE ANY VALIDATION THAT CAN RETURN
    // EARLY. takeSecret() reads and resets in one call, so the bytes are locals for the life of this function
    // and are copied nowhere. Every early return below would otherwise leave tokens sitting in DOM nodes on a
    // screen the operator has just been told to go and fix something on. The cost is retyping on a refusal,
    // and it is the trade components/SecretField.tsx argues for.
    const slots = form?.secrets ?? [];
    const values = slots.map((slot) => takeSecret(secretRef(slot.field)));

    if (bot === null || form === undefined) {
      setCredError("this bot's channel has no credentials this console can seal. Nothing was sent.");
      setSaving(false);
      return;
    }
    // A GUARD, NOT THE MESSAGE THE OPERATOR READS — and saying which is the point of this comment.
    //
    // The field carries `required`, so the browser's own constraint validation refuses the submit before this
    // function runs at all: on a hydrated page this branch is unreachable, and what an operator sees is
    // Chrome's own bubble. It stays because of what an empty value would DO rather than because it is nice to
    // have — the handle is `${prefix}${scope}`, so an empty scope seals every bot in the deployment under the
    // same three names, and one bot would silently rotate another's token.
    const missing = form.fields.find((f) => f.required === true && (channelValues[f.name] ?? "").trim() === "");
    if (missing !== undefined) {
      setCredError(`${missing.label} is required, and every credential below is sealed under a name derived from it. Nothing was sent.`);
      setSaving(false);
      return;
    }

    // SEAL, then NAME. One call per slot that carries a value; an empty slot is not sealed and its handle is
    // left exactly as the row already had it — a handle that names nothing fails SILENTLY at every consumer,
    // which is the failure cmd/cli/internal/stack/up.go's slot table was written to close.
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
        setCredError(
          `the ${slot.label.toLowerCase()} could not be sealed: ${detail(err, "the secret ref was refused")}. ` +
            (sealedNames.length === 0
              ? "The bot was not changed and nothing was stored."
              : `The bot was not changed. Already sealed: ${sealedNames.join(", ")} — re-submitting the same values rotates them rather than creating duplicates.`),
        );
        setSaving(false);
        return;
      }
      handles[slot.field] = handle;
      sealedNames.push(handle);
    }

    // THE WHOLE DOCUMENT, STARTING FROM THE ONE THAT IS STORED. See the header: the API assigns rather than
    // merges, so anything this console does not understand has to be carried across by this line or it is
    // deleted.
    const config: Record<string, unknown> = { ...(bot.config ?? {}) };
    for (const field of form.fields) {
      const raw = (channelValues[field.name] ?? "").trim();
      if (raw === "") continue;
      config[field.name] = field.list === true ? csvList(raw) : raw;
    }
    // HANDLES, NEVER VALUES. Nothing at the edge would stop a token going in here — `config` is opaque by
    // design — so this line is the boundary, and tests/bot-manifest.spec.ts reads the wire rather than this
    // comment.
    Object.assign(config, handles);

    try {
      await apiSend("PATCH", `/bots/${encodeURIComponent(botID)}`, { config });
      await reread();
      setCredStatus(
        (sealedNames.length === 0
          ? "Saved. No credential was sealed this time, so this bot connects with whatever it was already named. "
          : `Saved. ${sealedNames.length === 1 ? "The credential is" : "The credentials are"} sealed in this deployment's secret store as ${sealedNames.join(", ")} — named here, readable from nowhere, and on no machine's disk. `) +
          "Nothing has been contacted: the relay process reads this row at startup, so a running one has to be restarted before it sees this.",
      );
      setCredOpen(false);
    } catch (err: unknown) {
      // THE CREDENTIALS SURVIVE A REFUSED WRITE AND THE OPERATOR IS TOLD SO, BY NAME. The seals happened
      // first, so the values are sealed under handles the row does not yet reference; reporting only "it
      // failed" would leave live credentials the operator does not know exist.
      setCredError(
        `${detail(err, "the bot could not be updated")}` +
          (sealedNames.length === 0
            ? ""
            : ` — the credentials were already sealed as ${sealedNames.join(", ")}, so they are safe: fix the field above and submit again with the same values. (Re-sealing a name is a rotation, not a duplicate.)`),
      );
    } finally {
      setSaving(false);
    }
  }

  const kind = String(bot?.kind ?? "");
  const named = namedHandles(bot?.config, (form?.secrets ?? []).map((s) => s.field));

  return (
    <>
      <nav aria-label="Breadcrumb" className="breadcrumb">
        <ol>
          <li>
            <a href="/bots">Bots</a>
          </li>
          <li aria-current="page">
            <CopyButton value={botID} label="bot ID" testId="breadcrumb-bot">
              <code>{shortId(botID)}</code>
            </CopyButton>
          </li>
        </ol>
      </nav>

      {error === "" ? null : (
        <p role="alert" className="form-error" data-testid="bot-error">
          <span className="glyph" aria-hidden="true">
            ✖︎
          </span>{" "}
          {error}
        </p>
      )}

      <h1 className="page-title" data-testid="bot-title">
        {bot === null ? <code>{shortId(botID)}</code> : String(bot.name ?? botID)}
      </h1>

      {/* THE READINESS SIGNAL (lib/routes.ts names `panel-bot-record`) renders in BOTH settled states and in
          neither pending one — a record over a spinner reads as a state the page is not in. */}
      {bot === null && error === "" ? (
        <p className="loading">Loading…</p>
      ) : (
        <section className="panel" data-testid="panel-bot-record" aria-labelledby="bot-record-h">
          <div className="panel-head">
            <h2 id="bot-record-h">Bot</h2>
          </div>
          <dl data-testid="bot-record">
            <dt>Channel</dt>
            <dd data-testid="bot-channel">{channelLabel(kind)}</dd>
            <dt>Agent revision</dt>
            <dd>
              <Value value={bot?.agent_revision_id} absent="— not pinned; runs start with this deployment's default" />
            </dd>
            <dt>Repository</dt>
            <dd>
              <Value value={bot?.repository_binding_id} absent="— none; this bot cannot attach a workspace to a run" />
            </dd>
            <dt>Credentials</dt>
            {/* "NAMED", NEVER "WORKING". `config` carries handles, and whether the store can still redeem one
                is not visible from here — lib/bots.ts carries that argument. */}
            <dd data-testid="bot-credential-count">
              {(form?.secrets ?? []).length === 0 ? (
                <span className="cell-none">— this console seals no credentials for this channel</span>
              ) : (
                `${String(named)} of ${String((form?.secrets ?? []).length)} named`
              )}
            </dd>
            <dt>State</dt>
            <dd>
              {bot?.disabled === true ? (
                <Badge tone="warn" glyph="⊘" title="disabled">
                  disabled — will not start
                </Badge>
              ) : (
                <Badge tone="ok" glyph="✔︎" title="registered">
                  registered
                </Badge>
              )}
            </dd>
            <dt>Created</dt>
            <dd>
              <Stamp iso={typeof bot?.created_at === "string" ? bot.created_at : null} />
            </dd>
          </dl>
          {channel === undefined ? (
            // POST /v1/bots accepts ANY kind by design, so a row this console has no channel for is a state
            // that really happens — and saying nothing would leave an operator on a page with no controls and
            // no explanation.
            <p className="muted" data-testid="bot-unknown-channel">
              This console offers no form for the channel <code>{kind}</code>. The row is real and the control
              plane stores it, including its whole <code>config</code> document; what is missing is a screen
              here that knows what that channel asks for.
            </p>
          ) : null}
        </section>
      )}

      {manifest === undefined || bot === null ? null : (
        <section className="panel" data-testid="panel-bot-manifest" aria-labelledby="bot-manifest-h">
          <div className="panel-head">
            <h2 id="bot-manifest-h">Step 1 — {manifest.label}</h2>
          </div>
          <p className="muted">{manifest.lead}</p>
          <ol data-testid="bot-manifest-where">
            <li>
              Open{" "}
              <a href={manifest.href} target="_blank" rel="noreferrer">
                {manifest.href}
              </a>{" "}
              in a new tab.
            </li>
            <li>
              <code>{manifest.where}</code>
            </li>
            <li>Paste the document below, create the app, then install it into the workspace.</li>
          </ol>

          {manifest.fields.map((field) => {
            const value = manifestValues[field.name] ?? "";
            const over = [...value].length - field.limit;
            return (
              <div key={field.name}>
                <Field label={field.label} hint={field.hint}>
                  <FieldControl
                    id={`field-manifest-${field.name}`}
                    name={`manifest-${field.name}`}
                    type="text"
                    multiline={field.multiline === true}
                    rows={field.multiline === true ? 4 : undefined}
                    value={value}
                    onChange={(e) => setManifestValues((prev) => ({ ...prev, [field.name]: e.target.value }))}
                    data-testid={field.testId}
                  />
                </Field>
                {/* THE COUNT IS PLAIN TEXT AND NOT A LIVE REGION. It changes on every keystroke, and a region
                    that announces each one is a field nobody can type in. It is what makes the refusal below
                    predictable rather than a surprise at the end. */}
                <p className="muted" data-testid={`${field.testId}-count`}>
                  {String([...value].length)} of {String(field.limit)} characters
                  {over > 0 ? ` — ${String(over)} too many` : ""}
                </p>
              </div>
            );
          })}

          {promptGroup === undefined ? null : (
            <div data-testid="bot-manifest-prompts">
              <h3>{promptGroup.label}</h3>
              <p className="muted">{promptGroup.note}</p>
              {prompts.map((prompt, i) => (
                // Keyed by position because position IS the identity here: a prompt has no id, and the array
                // is rebuilt on every edit, so a row's controls always carry the value at their own index.
                <div key={i}>
                  <Field label={`${promptGroup.titleLabel} ${String(i + 1)}`}>
                    <FieldControl
                      id={`field-manifest-prompt-title-${String(i)}`}
                      name={`manifest-prompt-title-${String(i)}`}
                      type="text"
                      value={prompt.title}
                      onChange={(e) => setPrompts((prev) => prev.map((p, j) => (j === i ? { ...p, title: e.target.value } : p)))}
                      data-testid={`manifest-prompt-title-${String(i)}`}
                    />
                  </Field>
                  <Field label={`${promptGroup.messageLabel} ${String(i + 1)}`}>
                    <FieldControl
                      id={`field-manifest-prompt-message-${String(i)}`}
                      name={`manifest-prompt-message-${String(i)}`}
                      type="text"
                      value={prompt.message}
                      onChange={(e) => setPrompts((prev) => prev.map((p, j) => (j === i ? { ...p, message: e.target.value } : p)))}
                      data-testid={`manifest-prompt-message-${String(i)}`}
                    />
                  </Field>
                  <Button testId={`manifest-prompt-remove-${String(i)}`} onClick={() => setPrompts((prev) => prev.filter((_, j) => j !== i))}>
                    Remove prompt {String(i + 1)}
                  </Button>
                </div>
              ))}
              <p>
                <Button testId="manifest-prompt-add" onClick={() => setPrompts((prev) => [...prev, { title: "", message: "" }])}>
                  {promptGroup.addLabel}
                </Button>
              </p>
            </div>
          )}

          {draft === undefined || draft.refusals.length > 0 ? (
            // NO DOCUMENT AT ALL WHILE ANYTHING IS WRONG WITH IT, which is the point of a refusal rather than
            // a truncation: half a manifest that Slack rejects at paste time costs the operator the trip.
            <div className="form-error" data-testid="bot-manifest-refusal">
              <p>
                <span className="glyph" aria-hidden="true">
                  ✖
                </span>{" "}
                <strong>No manifest yet.</strong> Slack would refuse this one, so it is not offered:
              </p>
              <ul>
                {(draft?.refusals ?? []).map((refusal) => (
                  <li key={refusal}>{refusal}</li>
                ))}
              </ul>
            </div>
          ) : (
            <>
              <p>
                <CopyButton value={draft.yaml} label="app manifest" testId="bot-manifest-copy">
                  Copy manifest
                </CopyButton>
              </p>
              <pre className="code" data-testid="bot-manifest">
                {draft.yaml}
              </pre>
            </>
          )}
        </section>
      )}

      {form === undefined || bot === null ? null : (
        <section className="panel" data-testid="panel-bot-credentials" aria-labelledby="bot-credentials-h">
          <div className="panel-head">
            <h2 id="bot-credentials-h">Step 2 — {channel?.label ?? "Channel"} credentials</h2>
          </div>
          <p className="muted">
            What the app gives you once it exists. Each value is sealed into this deployment&apos;s secret
            store and this bot&apos;s row keeps only the NAME it was sealed under.
          </p>
          {credStatus === "" ? null : (
            <p className="form-status" data-testid="bot-credentials-status">
              <span className="glyph" aria-hidden="true">
                ✔
              </span>{" "}
              {credStatus}
            </p>
          )}
          <dl data-testid="bot-credential-handles">
            {form.secrets.map((slot) => {
              const held = prefill(bot.config?.[slot.field]);
              return (
                <div key={slot.field}>
                  <dt>{slot.label}</dt>
                  <dd>{held === "" ? <span className="cell-none">— none named</span> : <code>{held}</code>}</dd>
                </div>
              );
            })}
          </dl>
          <p>
            <Button variant="primary" testId="bot-credentials-open" onClick={() => setCredOpen(true)}>
              {named === 0 ? "Add credentials" : "Change credentials"}
            </Button>
          </p>
        </section>
      )}

      {/* THE FORM IS BEHIND A DIALOG, AND THAT IS A MEASUREMENT RATHER THAN A STYLE (Task 12).
          tests/auth.spec.ts sweeps every form the console SERVES and asserts method="post" on each, deriving
          the expected count from `<ResourceForm` mounts in `app/**\/page.tsx` minus the ones behind a
          `<FormDialog`. It fetches the paths lib/routes.ts declares — and a dynamic segment is not one, so a
          form served from `/bots/[id]` would be swept by nothing at all. That guard failed on the first draft
          of this page, naming exactly that.
          A dialog is also what app/repositories/[id] does with the one credential it takes, for the same
          operator reason: a detail page is a record, and the control that changes a value belongs beside the
          value it changes. tests/constants.ts declares this row, so the axe and contrast sweeps open it. */}
      {!credOpen || form === undefined || bot === null ? null : (
        <FormDialog
          label="Set this bot's credentials"
          testId="bot-credentials-dialog"
          onClose={() => {
            setCredOpen(false);
            setCredError("");
          }}
        >
          <ResourceForm
            title="Set this bot's credentials"
            testId="bot-credentials"
            note={
              <span>
                What the app gave you once it existed. Every value is sealed by <code>POST /v1/secret-refs</code>{" "}
                under a name derived from the workspace, and this bot&apos;s row then stores only that NAME —
                nothing here is readable back, from this console or any route.
              </span>
            }
            caveat={{
              summary: "What saving this does, and what it does not do",
              body: (
                <p className="muted">
                  <strong>Nothing is verified and no workspace is contacted.</strong> A wrong signing secret is
                  not a refusal here; it is a callback the relay cannot tell from a forged one, later. A
                  credential left empty is not sealed and its handle is left exactly as the row already had it,
                  so this form can be used to rotate one token without retyping the other two. The relay process
                  reads this row <strong>at startup</strong>, so a running relay keeps using what it read until
                  it is restarted.
                </p>
              ),
            }}
            fields={(form.fields ?? []).map((field) => ({
              name: field.name,
              label: field.label,
              required: field.required,
              hint: field.hint,
              testId: field.testId,
              value: channelValues[field.name] ?? "",
              onChange: (value: string) => setChannelValues((prev) => ({ ...prev, [field.name]: value })),
            }))}
            submitLabel="Save credentials"
            submittingLabel="Saving…"
            submitTestId="bot-credentials-save"
            submitting={saving}
            error={credError}
            onSubmit={saveCredentials}
            actions={
              <Button
                testId="bot-credentials-cancel"
                onClick={() => {
                  setCredOpen(false);
                  setCredError("");
                }}
              >
                Cancel
              </Button>
            }
          >
            {/* THE CREDENTIALS ARE CHILDREN RATHER THAN FormFields, which is ResourceForm's own rule: a
                FormField is a controlled value/onChange pair, and putting a secret through it would make the
                secret React state. They render INSIDE the form and after the fields, so they stay in document
                order and therefore in tab order. */}
            {form.secrets.map((slot) => {
              // WHAT THIS SLOT IS ALREADY NAMED, shown so "leave it empty" is a decision rather than a guess.
              // It is the HANDLE — a name, never a value; the value is unreadable from here and from every
              // route, which is the whole point of the two-call flow.
              const held = prefill(bot.config?.[slot.field]);
              return (
                <SecretField
                  key={slot.field}
                  inputRef={secretRef(slot.field)}
                  id={`field-detail-${slot.field}`}
                  label={slot.label}
                  testId={slot.testId}
                  required={false}
                  hint={`${slot.hint} Leave it empty to keep the name this row already carries${held === "" ? " (none yet)" : ` (${held})`}.`}
                />
              );
            })}
          </ResourceForm>
        </FormDialog>
      )}
    </>
  );
}
