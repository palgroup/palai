"use client";

import { useCallback, useEffect, useState } from "react";

import { Menu } from "@/components/ui/Menu";
import { Button } from "@/components/ui/Button";
import { ConfirmDestructive } from "@/components/ConfirmDestructive";
import { FormDialog } from "@/components/FormDialog";
import { Panel } from "@/components/Panel";
import { Picker, type PickerOption } from "@/components/Picker";
import { ResourceForm } from "@/components/ResourceForm";
import { RevealOnce } from "@/components/RevealOnce";
import { CopyButton, shortId, Stamp } from "@/components/Session";
import { Status } from "@/components/Status";
import { apiGet, apiSend, RelayError } from "@/lib/api";
import { rememberedProject, rememberProject } from "@/lib/scope";
import { useQueryParam } from "@/lib/urlState";

// THE POLICY SCREEN — AND EVERY FIELD ON IT IS SENT EVERY TIME (E28 T2, plan §3.6 D9).
//
// PATCH /v1/projects/{project_id} IS AN ASSIGNMENT. identity/store.go:269-275 marshals the decoded
// configPolicyInput and hands the bytes to UpdateProjectConfigPolicy; there is no merge and no patch, and the
// four list fields are nil-able Go slices — so a request naming only `pool` stores `approvers: null`. And
// `HIL-P11` measured that an empty approver list is PERMISSIVE. The most innocent button an admin console can
// carry — "put this project on the Mac pool" — therefore opens the approval gate to every principal in the
// project, silently, and the operator's own action succeeds while it happens. THE FORM IS THE CONTROL: it
// reads the current document first, shows all five fields, and sends all five.
//
// WHAT THIS PASS CHANGED, MEASURED FIRST (2026-07-31, `PROSE MEASURE /policy`): 3235 characters of page, of
// which 2486 sat in FIFTEEN paragraphs longer than sixty characters — and ONE table, five header cells, one
// row. The screen was an essay with a form in it.
//
//   THE KEYS ARE A TABLE NOW. An id you can read and copy, the project, the capability set as chips, when it
//   expires, whether it is live, and a row-end ⋯ carrying the one action a key has. `revoked_at` used to be
//   a raw timestamp in a column called "Revoked" whose usual value was an em dash; the status of a key is a
//   STATE, and it reads as one.
//   THE SELECTED PROJECT IS IN THE URL. `/policy?project=prj_…` — the pattern app/sessions/[id]/page.tsx
//   landed. The back button undoes a selection, a project's policy can be sent to somebody as a link, and
//   the overview's project rows now link straight here. lib/scope.ts is still written, so the shell's scope
//   picker and this screen can never show different projects.
//   THE PARAGRAPHS THAT SURVIVED ARE THE TWO THAT CHANGE WHAT YOU DO: the whole-document warning above the
//   form, and CON-P2 under the mint. The rest moved to docs/operations/console.md §3b, which already carried
//   most of it in more detail, and to the hints under the fields they describe.
//
// THE MINT IS A DIALOG NOW, AND THE TWO GUARDS THAT HELD IT HERE WERE ANSWERED RATHER THAN LOWERED.
// components/FormDialog.tsx landed in the page-parity pass, so there is a primitive to move into.
// tests/auth.spec.ts's served-form sweep no longer carries a hand-typed floor: it DERIVES the expected count
// per route as ResourceForm mounts minus FormDialog mounts, so a form moving behind a dialog self-adjusts
// instead of turning a number red. tests/reveal-once.spec.ts's mint() gained ONE `.click()` to open the
// dialog — a driver change, not a weakening: every assertion in that file is the one it was, and its five
// probes still report `probe_found=true`, which is what makes its zeros mean something.

interface ProjectRow {
  id?: string;
  display_name?: string;
}

interface PoolRow {
  id?: string;
  name?: string;
  posture?: string;
  strict_enrollment?: boolean;
}

interface ConfigPolicy {
  allowed_models?: string[] | null;
  allowed_tools?: string[] | null;
  default_tools?: string[] | null;
  approvers?: string[] | null;
  pool?: string;
}

interface APIKeyRow extends Record<string, unknown> {
  id?: string;
  project_id?: string;
  scopes?: string[];
  expires_at?: string | null;
  created_at?: string | null;
  revoked_at?: string | null;
}

/** MintedKey is the CREATE response. `key` is set here and on no read — identity/store.go apiKeyView. */
interface MintedKey {
  id?: string;
  project_id?: string;
  scopes?: string[];
  key?: string;
}

const detail = (err: unknown, fallback: string) => (err instanceof RelayError ? err.problem.detail : fallback);

/** csv splits the operator's comma-separated text the way cmd/cli/internal/admin/admin.go's `csv` does. */
const csv = (raw: string): string[] =>
  raw
    .split(",")
    .map((v) => v.trim())
    .filter((v) => v !== "");

/** joinList renders a stored list back into the field. A null list and an empty list both render empty — which
 *  is honest, because the API stores `null` for both and the resolver reads them the same way. */
const joinList = (list: string[] | null | undefined): string => (list ?? []).join(", ");

/** live is the one predicate this screen has about a key, and it is used by the table, the dialog's review
 *  and the revoke control — so the three can never disagree about what "revoked" means. */
const live = (row: { revoked_at?: string | null }): boolean => row.revoked_at === null || row.revoked_at === undefined;

export default function PolicyPage() {
  // THE SELECTED PROJECT IS IN THE ADDRESS BAR. `replace` rather than `push`, because this is also written
  // on ARRIVAL when the URL names no project — see lib/urlState.ts.
  const [project, setProject] = useQueryParam("project", "replace");

  const [projects, setProjects] = useState<ProjectRow[]>([]);
  const [pools, setPools] = useState<PoolRow[]>([]);
  const [loadError, setLoadError] = useState("");

  // THE FIVE FIELDS. They are five pieces of state and one submit, deliberately: a "changed fields" set is
  // the shape that produces a partial write, and this endpoint has no partial write.
  const [allowedModels, setAllowedModels] = useState("");
  const [allowedTools, setAllowedTools] = useState("");
  const [defaultTools, setDefaultTools] = useState("");
  const [approvers, setApprovers] = useState("");
  const [pool, setPool] = useState("");
  const [policyError, setPolicyError] = useState("");
  const [policyStatus, setPolicyStatus] = useState("");
  const [saving, setSaving] = useState(false);

  const [keyReload, setKeyReload] = useState(0);
  const [keyRows, setKeyRows] = useState<APIKeyRow[]>([]);
  // THE OPEN ROW MENU, BY ID. A menu ITEM does not close it: components/ConfirmDestructive.tsx returns focus
  // to the element that was focused when it opened, and an element removed from the DOM in the same click
  // cannot receive it — Escape would then drop focus to <body>, which tests/policy.spec.ts asserts against.
  const [scopeProvision, setScopeProvision] = useState(false);
  const [scopeApprove, setScopeApprove] = useState(false);
  const [expiresOn, setExpiresOn] = useState("");
  const [mintError, setMintError] = useState("");
  const [minting, setMinting] = useState(false);
  /** The mint dialog. A create form is a MODE, and the control that enters it is the panel's own button. */
  const [mintOpen, setMintOpen] = useState(false);
  // THE MINTED VALUE LIVES HERE AND NOWHERE ELSE, for as long as the region is on screen. This is a client
  // component, so this state is never serialized into an RSC flight payload; dismissal drops the reference.
  // What is NOT claimed is that the bytes leave browser memory — a fetch response body lives until GC and no
  // web API controls that. What IS claimed, and proven by tests/reveal-once.spec.ts, is that the value
  // reaches no storage, no URL, no later response body, and no DOM node after dismissal.
  const [minted, setMinted] = useState<MintedKey | null>(null);

  const [revoking, setRevoking] = useState<APIKeyRow | null>(null);
  const [revokeBusy, setRevokeBusy] = useState(false);
  const [revokeStatus, setRevokeStatus] = useState("");
  const [revokeError, setRevokeError] = useState("");

  /** choose writes the selection to the address bar AND to lib/scope.ts, so the shell's picker follows. */
  const choose = useCallback(
    (id: string) => {
      rememberProject(id);
      setProject(id);
    },
    [setProject],
  );

  // The project list feeds the picker directly rather than through a Panel + onRows. MEASURED, so nobody
  // reads a truncation ceiling into it that is not there: ListProjects (storage/queries/projects.sql) carries
  // no LIMIT and the handler returns a plain {object, data} list — this collection is never cut, exactly like
  // /v1/environments. A page of projects is also not what this screen is for.
  useEffect(() => {
    let cancelled = false;
    Promise.all([apiGet<{ data?: ProjectRow[] }>("/projects"), apiGet<{ data?: PoolRow[] }>("/runner-pools")])
      .then(([projectBody, poolBody]) => {
        if (cancelled) return;
        setProjects(projectBody.data ?? []);
        setPools(poolBody.data ?? []);
      })
      .catch((err: unknown) => {
        if (!cancelled) setLoadError(detail(err, "the projects and pools could not be read"));
      });
    return () => {
      cancelled = true;
    };
  }, []);

  // THE DEFAULT SELECTION, ONCE THE COLLECTION IS KNOWN. A form that opens empty invites a save over a
  // document nobody read, so the screen picks: whatever the URL names, else the shell's remembered scope,
  // else the first row. AN ID THIS DEPLOYMENT DOES NOT HAVE IS IGNORED rather than selected, so neither a
  // stale session value nor a hand-typed query string can open the form on nothing.
  useEffect(() => {
    if (projects.length === 0) return;
    if (projects.some((r) => r.id === project)) return;
    const remembered = rememberedProject();
    choose(projects.some((r) => r.id === remembered) ? remembered : String(projects[0]?.id ?? ""));
  }, [projects, project, choose]);

  // loadPolicy is the READ-FIRST step, and it is the whole mechanism. Every field the form will send is
  // filled from the stored document, so an operator who changes one has explicitly kept the other four.
  const loadPolicy = useCallback(async (id: string) => {
    if (id === "") return;
    setPolicyStatus("");
    try {
      const body = await apiGet<{ config_policy?: ConfigPolicy | null }>(`/projects/${encodeURIComponent(id)}`);
      const policy = body.config_policy ?? {};
      setAllowedModels(joinList(policy.allowed_models));
      setAllowedTools(joinList(policy.allowed_tools));
      setDefaultTools(joinList(policy.default_tools));
      setApprovers(joinList(policy.approvers));
      setPool(policy.pool ?? "");
      setPolicyError("");
    } catch (err: unknown) {
      setPolicyError(detail(err, "the project's policy could not be read"));
    }
  }, []);

  useEffect(() => {
    void loadPolicy(project);
  }, [project, loadPolicy]);

  async function savePolicy() {
    setSaving(true);
    setPolicyError("");
    setPolicyStatus("");
    try {
      // ALL FIVE KEYS, ALWAYS. `pool` is sent as "" when none is chosen, which the server's `omitempty` drops
      // on marshal — so "no pool" stores an absent key rather than an empty string, and the console does not
      // have to model that difference.
      const body = await apiSend<{ config_policy?: ConfigPolicy | null }>("PATCH", `/projects/${encodeURIComponent(project)}`, {
        config_policy: {
          allowed_models: csv(allowedModels),
          allowed_tools: csv(allowedTools),
          default_tools: csv(defaultTools),
          approvers: csv(approvers),
          pool,
        },
      });
      // The response is the STORED document (the route re-reads the project after the write), so the form is
      // refilled from what was saved rather than from what was typed.
      const saved = body.config_policy ?? {};
      setAllowedModels(joinList(saved.allowed_models));
      setAllowedTools(joinList(saved.allowed_tools));
      setDefaultTools(joinList(saved.default_tools));
      setApprovers(joinList(saved.approvers));
      setPool(saved.pool ?? "");
      setPolicyStatus(
        `The policy for ${project} is now exactly these five fields. ` +
          `${(saved.approvers ?? []).length === 0 ? "It names no approvers, so every principal in this project may approve." : `It names ${String((saved.approvers ?? []).length)} approver(s).`}`,
      );
    } catch (err: unknown) {
      setPolicyError(detail(err, "the policy could not be written"));
    } finally {
      setSaving(false);
    }
  }

  async function mintKey() {
    setMinting(true);
    setMintError("");
    setMinted(null);
    const scopes = [scopeProvision ? "provision" : "", scopeApprove ? "approve" : ""].filter((s) => s !== "");
    try {
      // `expires_at` is sent only when the operator chose a date. A `<input type="date">` yields YYYY-MM-DD;
      // the API takes RFC 3339, and the END of the chosen day is the reading that does not surprise anyone.
      const body = await apiSend<MintedKey>("POST", "/api-keys", {
        project_id: project,
        scopes,
        ...(expiresOn === "" ? {} : { expires_at: `${expiresOn}T23:59:59Z` }),
      });
      setMinted(body);
      setMintOpen(false);
      setKeyReload((n) => n + 1);
    } catch (err: unknown) {
      setMintError(detail(err, "the key could not be minted"));
    } finally {
      setMinting(false);
    }
  }

  async function revoke(row: APIKeyRow) {
    setRevokeBusy(true);
    setRevokeError("");
    setRevokeStatus("");
    try {
      await apiSend("POST", `/api-keys/${encodeURIComponent(String(row.id))}/revoke`);
      setRevokeStatus(`${String(row.id)} is revoked. Anything holding it is now refused with 401.`);
      setKeyReload((n) => n + 1);
    } catch (err: unknown) {
      setRevokeError(detail(err, "the key could not be revoked"));
    } finally {
      setRevokeBusy(false);
      setRevoking(null);
    }
  }

  const projectOptions: PickerOption[] = projects
    .filter((p) => typeof p.id === "string" && p.id !== "")
    .map((p) => ({ value: String(p.id), label: `${String(p.display_name === undefined || p.display_name === "" ? p.id : p.display_name)} (${String(p.id)})` }));
  const poolOptions: PickerOption[] = pools
    .filter((p) => typeof p.id === "string" && p.id !== "")
    .map((p) => ({
      value: String(p.id),
      label: `${String(p.name ?? p.id)} — ${String(p.posture ?? "?")}${p.strict_enrollment === true ? ", strict enrolment" : ""} (${String(p.id)})`,
    }));
  const approversEmpty = csv(approvers).length === 0;

  return (
    <>
      {loadError === "" ? null : (
        <p role="alert" className="form-error" data-testid="policy-load-error">
          <span className="glyph" aria-hidden="true">
            ✖
          </span>{" "}
          {loadError}
        </p>
      )}

      <Picker
        id="policy-project"
        label="Project"
        value={project}
        onChange={choose}
        options={projectOptions}
        testId="policy-project-select"
        hint="Both halves of this page act on this project: its policy document, and the keys that reach it. The choice is in the address bar, so it survives a reload and can be sent to somebody."
        emptyNote={
          <>
            <strong>No projects.</strong> There is nothing to configure yet — create one with{" "}
            <code>palai admin project create</code>.
          </>
        }
      />

      <ResourceForm
        title="Project policy"
        testId="policy"
        // THE ASSIGNMENT SENTENCE IS THE FORM'S OWN NOTE RATHER THAN A PARAGRAPH BESIDE IT. It changes what
        // you DO every single time you use this control — the test this console applies to decide what
        // collapses — and as a sibling it drifted onto the wrong form the first time the page was reordered.
        note={
          <span data-testid="policy-assignment-note">
            {/* THE EXPLICIT SPACES ARE LOAD-BEARING, and tests/rendered-copy.spec.ts is why they are written
                out: the same sentence lost the same space the FIRST time this note was authored, on this
                page, and the guard added after that caught the next move too. A text child that starts on
                the line after a closing tag has its leading whitespace trimmed by the JSX transform — which
                is exactly the shape of edit this split is, so every boundary here is explicit. */}
            <strong>This form writes the WHOLE policy.</strong>{" "}
            The five fields below are, after you save, the entirety of this project&apos;s{" "}
            <code>config_policy</code>.
          </span>
        }
        caveat={{
          summary: "Why the form reads the policy first, and what a field you cannot see means",
          body: (
            <p className="muted">
              <code>PATCH /v1/projects/{"{id}"}</code> replaces the stored document rather than merging into
              it, which is why the form reads the current policy before it shows you anything. A field you
              cannot see is a field you did not send.
            </p>
          ),
        }}
        fields={[
          {
            name: "policy-allowed-models",
            label: "Allowed models",
            value: allowedModels,
            onChange: setAllowedModels,
            hint: "Comma-separated model names a run in this project may ask for. Empty means UNSET, which leaves the deployment's own routing in charge — not restricted.",
            testId: "policy-allowed-models-input",
          },
          {
            name: "policy-allowed-tools",
            label: "Allowed tools",
            value: allowedTools,
            onChange: setAllowedTools,
            hint: "The ceiling: a tool not named here cannot be granted by an agent revision or a tool set. Empty lets the deployment default decide.",
            testId: "policy-allowed-tools-input",
          },
          {
            name: "policy-default-tools",
            label: "Default tools",
            value: defaultTools,
            onChange: setDefaultTools,
            hint: "What a run gets when nothing else names a tool set.",
            testId: "policy-default-tools-input",
          },
          {
            name: "policy-approvers",
            label: "Approvers",
            value: approvers,
            onChange: setApprovers,
            hint: "Principals allowed to decide this project's approvals: a principal id, key:<api_key_id>, or slack:<team>:<user>. Empty PERMITS EVERYONE.",
            testId: "policy-approvers-input",
          },
          {
            name: "policy-pool",
            label: "Runner pool",
            kind: "select",
            value: pool,
            onChange: setPool,
            options: poolOptions,
            placeholder: "Inherit this tenant's default pool",
            testId: "policy-pool-select",
            hint: "Where this project's runs are placed. One pool, because a project's runs go to one posture.",
            // A ONE-POOL FLEET STILL GETS A DROPDOWN. Every deployment before E28 T1 had exactly one pool,
            // and a free-text fallback here would accept a pool id that does not exist — refused several
            // steps later as a failure about a RUN rather than about this field.
            emptyNote: (
              <>
                <strong>No runner pools are readable.</strong> Runs will be placed in this tenant&apos;s
                default pool. Create one with <code>palai pool create</code>.
              </>
            ),
          },
        ]}
        submitLabel="Save the whole policy"
        submittingLabel="Saving…"
        submitTestId="policy-save-button"
        submitting={saving}
        error={policyError}
        status={policyStatus}
        onSubmit={savePolicy}
      >
        {/* HIL-P11, WHERE THE OPERATOR CAN ACT ON IT. Writing a ceiling on screen does not close it; what it
            prevents is an empty box reading as a locked door. */}
        {approversEmpty ? (
          // TODO(component-layer): Callout — see app/fleet/page.tsx. `.status` is a nowrap PILL and this
          // is a paragraph; `.form-status` is the block-shaped sibling that wraps.
          <p className="form-status" data-glyph="warn" data-testid="policy-approvers-permissive">
            <span className="glyph" aria-hidden="true">
              !
            </span>{" "}
            <span>
            <strong>An empty approver list permits every key in this project to approve.</strong> It is not a
            closed gate — <code>ConfigPolicy.ApproverAllowed</code> admits every principal when the list is
            unconfigured (<code>HIL-P11</code>). Name at least one principal if the approval gate is meant to
            decide anything.
            </span>
          </p>
        ) : null}
      </ResourceForm>

      {/* THE KEYS, UNDER THE POLICY THEY REACH — the table first and the mint form after it, because a
          create control belongs below the collection it adds to rather than above it. */}
      <Panel<APIKeyRow>
        title="API keys"
        testId="panel-api-keys"
        fetchPath="/api-keys"
        reloadKey={keyReload}
        onRows={setKeyRows}
        note="Metadata only. A key's value is returned by the create call and by nothing else — there is no route that reads one back."
        action={
          <button type="button" className="primary" data-testid="key-mint-open" onClick={() => setMintOpen(true)}>
            + Mint key
          </button>
        }
        columns={[
          {
            header: "ID",
            sort: (r) => String(r.id ?? ""),
            render: (r) => (
              <span className="cell-id-group">
                <code className="cell-id" title={String(r.id ?? "")}>
                  {shortId(String(r.id ?? ""))}
                </code>
                <CopyButton value={String(r.id ?? "")} label="API key ID" testId={`copy-${String(r.id ?? "")}`} />
              </span>
            ),
          },
          { header: "Project", sort: (r) => String(r.project_id ?? ""), render: (r) => <code>{String(r.project_id ?? "")}</code> },
          {
            header: "Capabilities",
            // NO SORT: a capability set is a SET, and sorting a list of keys by the alphabetical first of
            // their scopes is a control that produces motion and no information (Column's own rule).
            render: (r) =>
              (r.scopes ?? []).length === 0 ? (
                // THE EMPTY SET IS THE DANGEROUS ONE and it must not render as a blank cell:
                // middleware's Scope.HasScope returns true for EVERY capability when the set is empty.
                <span className="chip" data-kind="danger">
                  <strong>every capability</strong>
                </span>
              ) : (
                <ul className="chip-list">
                  {(r.scopes ?? []).map((scope) => (
                    <li key={scope}>
                      <span className="chip">{scope}</span>
                    </li>
                  ))}
                </ul>
              ),
          },
          {
            header: "Expires",
            sort: (r) => String(r.expires_at ?? ""),
            render: (r) =>
              r.expires_at === null || r.expires_at === undefined ? <span className="cell-none">— never</span> : <Stamp iso={r.expires_at} />,
          },
          {
            // A STATE, NOT A TIMESTAMP IN A COLUMN CALLED "Revoked" whose usual value was an em dash. The
            // stamp is still readable — it is the pill's title — but what a reader needs at a glance is
            // whether this credential still opens the door.
            header: "Status",
            sort: (r) => (live(r) ? "live" : "revoked"),
            render: (r) => (
              <span title={live(r) ? "This key still authenticates." : `Revoked ${String(r.revoked_at)}`}>
                <Status value={live(r) ? "live" : "revoked"} testId={`key-state-${String(r.id ?? "")}`} />
              </span>
            ),
          },
          {
            header: "",
            render: (r) => {
              const id = String(r.id ?? "");
              if (!live(r)) return <span className="cell-none">—</span>;
              return (
                <div className="row-menu">
                  <Menu
                    label={`Actions for API key ${id}`}
                    trigger={<span aria-hidden="true">⋯</span>}
                    triggerClassName="row-menu-toggle"
                    triggerTestId={`key-menu-${id}`}
                    // ONE ITEM, AND THAT IS THE API RATHER THAN THE DESIGN. A key's whole write surface after
                    // creation is POST /v1/api-keys/{id}/revoke — there is no rename, no re-scope and no
                    // un-revoke — so a second entry here would be a control that refuses.
                    items={[{ label: `Revoke ${id}`, testId: `revoke-${id}`, danger: true, onSelect: () => setRevoking(r) }]}
                  />
                </div>
              );
            },
          },
        ]}
        emptyNote={
          <>
            <p className="empty-title">No API keys</p>
            <p className="empty-body">
              An API key is what a client presents to reach this deployment, and its capabilities are what
              that client may then do. This list cannot be empty on a running stack — a bootstrap seeds the
              admin key — so an empty table here means the read did not reach the collection it named.
            </p>
          </>
        }
      />

      {revokeStatus === "" ? null : (
        <p role="status" className="form-status" data-testid="key-revoke-status">
          <span className="glyph" aria-hidden="true">
            ✔
          </span>{" "}
          {revokeStatus}
        </p>
      )}
      {revokeError === "" ? null : (
        <p role="alert" className="form-error" data-testid="key-revoke-error">
          <span className="glyph" aria-hidden="true">
            ✖
          </span>{" "}
          {revokeError}
        </p>
      )}

      {/* THE CREATE FORM IS A MODE, AND THE PANEL'S BUTTON IS THE CONTROL THAT ENTERS IT. It sat open under
          the table on every visit, so an operator who came to READ the key list scrolled past a credential
          minter to do it. The dialog closes on success, which leaves the one-time value's reveal region as
          the only thing on screen — the one moment this page has that must not be scrolled past. */}
      {mintOpen ? (
        <FormDialog
          label="Mint an API key"
          testId="key-mint-dialog"
          onClose={() => {
            setMintOpen(false);
            setMintError("");
          }}
        >
          <ResourceForm
            title="Mint an API key"
            testId="key-mint"
            note={
              <>
                The value is shown <strong>once</strong>, here, and is retrievable from nowhere afterwards. It is
                minted into the project selected above; <code>docs/operations/console.md</code> §3 is the recipe
                for the narrow key this console wants.
              </>
            }
            fields={[
              {
                name: "key-expires",
                label: "Expires on (optional)",
                kind: "date",
                value: expiresOn,
                onChange: setExpiresOn,
                hint: "The key stops authenticating at the end of this day, UTC. Leave it empty for a key that never expires.",
                testId: "key-expires-input",
              },
            ]}
            submitLabel="Mint key"
            submittingLabel="Minting…"
            submitTestId="key-mint-button"
            submitting={minting}
            error={mintError}
            onSubmit={mintKey}
          >
            {/* CHECKBOXES IN A GROUPED FIELDSET, which is the grouping a screen reader reads as one question.
                ResourceForm's `fields` are single controls with one value; a capability set is neither. */}
            <fieldset data-testid="key-scopes">
              <legend>Capabilities</legend>
              <p className="muted" id="key-scopes-hint">
                <strong>Selecting nothing mints an UNLIMITED key.</strong> An empty capability set holds every
                capability (<code>middleware.Scope.HasScope</code>) — it is not a key that can do nothing, it is
                the bootstrap key&apos;s power in a new credential.
              </p>
              <label htmlFor="key-scope-provision">
                <input
                  id="key-scope-provision"
                  type="checkbox"
                  checked={scopeProvision}
                  data-testid="key-scope-provision"
                  aria-describedby="key-scope-provision-hint"
                  onChange={(e) => setScopeProvision(e.target.checked)}
                />{" "}
                <code>provision</code> — the whole admin surface, including <strong>this policy form</strong>
              </label>
              <p className="muted" id="key-scope-provision-hint">
                Organizations, projects, keys, model wiring, environments and tools. A key with this can rewrite
                the approver list above.
              </p>
              <label htmlFor="key-scope-approve">
                <input
                  id="key-scope-approve"
                  type="checkbox"
                  checked={scopeApprove}
                  data-testid="key-scope-approve"
                  aria-describedby="key-scope-approve-hint"
                  onChange={(e) => setScopeApprove(e.target.checked)}
                />{" "}
                <code>approve</code> — deciding a parked tool call, and admitting a machine into a strict pool
              </label>
              <p className="muted" id="key-scope-approve-hint">
                Deliberately <em>not</em> covered by <code>provision</code>: a key that could provision could add
                itself to the approver list and then approve. Holding this alone is what makes an approver an
                approver rather than an administrator.
              </p>
            </fieldset>
          </ResourceForm>
        </FormDialog>
      ) : null}


      {/* THE CEILING THIS SCREEN CANNOT CLOSE (CON-P2). Minting a narrow key here does not narrow the key the
          console itself is holding — that one is in the server process's environment. */}
      <p className="muted" data-testid="policy-console-key-note">
        <strong>This console&apos;s own key may still hold every capability</strong> — minting a narrow key
        here does not change it. The relay presents whatever <code>PALAI_API_KEY</code> the console process was
        started with, so narrowing that one means restarting the console with a key minted here
        (<code>CON-P2</code>). <code>docs/operations/console.md</code> §3b is the long form of this screen.
      </p>

      {minted === null ? null : (
        <RevealOnce
          testId="key-reveal"
          label="New API key"
          announcement="A new API key was created. Its value is on screen now and will not be shown again."
          value={String(minted.key ?? "")}
          meta={
            <>
              <code>{String(minted.id ?? "")}</code> for <code>{String(minted.project_id ?? "")}</code> —{" "}
              {(minted.scopes ?? []).length === 0 ? "EVERY capability" : (minted.scopes ?? []).join(", ")}
            </>
          }
          onDismiss={() => setMinted(null)}
        />
      )}

      {revoking === null ? null : (
        <ConfirmDestructive
          testId="key-revoke-dialog"
          title="Revoke this API key?"
          message={
            <>
              A revoked key never comes back. Anything still presenting it — a script, a CI job, another
              console — starts being refused with <code>401</code> the moment you confirm, and there is no undo:
              the replacement is a NEW key with a new value.
            </>
          }
          review={
            // THE "REVIEWING" HALF OF SC 3.3.4 LEG 3, as DATA. A dialog that only asks "are you sure?" has
            // confirmed nothing — what makes this leg satisfiable is that the operator can see WHICH key and
            // what it opens before the irreversible step.
            <dl>
              <dt>Key</dt>
              <dd>
                <code>{String(revoking.id ?? "")}</code>
              </dd>
              <dt>Project</dt>
              <dd>
                <code>{String(revoking.project_id ?? "")}</code>
              </dd>
              <dt>Capabilities</dt>
              <dd>{(revoking.scopes ?? []).length === 0 ? "EVERY capability" : (revoking.scopes ?? []).join(", ")}</dd>
              <dt>Keys left in this organization</dt>
              <dd>{String(keyRows.filter(live).length - 1)} still live after this one</dd>
            </dl>
          }
          confirmLabel="Revoke it"
          busy={revokeBusy}
          onConfirm={() => void revoke(revoking)}
          onCancel={() => setRevoking(null)}
        />
      )}
    </>
  );
}
