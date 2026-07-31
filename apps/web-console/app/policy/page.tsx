"use client";

import { useCallback, useEffect, useState } from "react";

import { ConfirmDestructive } from "@/components/ConfirmDestructive";
import { Panel } from "@/components/Panel";
import { Picker, type PickerOption } from "@/components/Picker";
import { ResourceForm } from "@/components/ResourceForm";
import { RevealOnce } from "@/components/RevealOnce";
import { apiGet, apiSend, RelayError } from "@/lib/api";

// THE POLICY SCREEN — AND EVERY FIELD ON IT IS SENT EVERY TIME (E28 T2, plan §3.6 D9).
//
// PATCH /v1/projects/{project_id} IS AN ASSIGNMENT. identity/store.go:269-275 marshals the decoded
// configPolicyInput and hands the bytes to UpdateProjectConfigPolicy; there is no merge and no patch, and the
// four list fields are nil-able Go slices — so a request naming only `pool` stores `approvers: null`. And
// `HIL-P11` measured that an empty approver list is PERMISSIVE. The most innocent button an admin console can
// carry — "put this project on the Mac pool" — therefore opens the approval gate to every principal in the
// project, silently, and the operator's own action succeeds while it happens.
//
// The tree already knew this in a COMMENT (cmd/cli/internal/admin/admin.go:241-244, verbatim: "the realistic
// accident is setting --pool and silently dropping --approvers") and in a runbook paragraph
// (docs/operations/runner-fleet.md:70). A comment is not a control. THE FORM IS THE CONTROL: it reads the
// current document first, shows all five fields, and sends all five. tests/policy.spec.ts asserts both the
// stored outcome and the request BODY, because a server that merged would make the outcome assertion pass
// over a form that still only sent one field.
//
// AND THE FORM DOES NOT HIDE THE ASSIGNMENT — it states it. A screen that quietly repaired the semantics
// would leave an operator who then uses `palai project set-policy` with a wrong mental model, and that CLI
// call has no read-first step to save them.
//
// THE SECOND HALF IS THE KEYS THAT REACH THIS POLICY, and the two belong on one page for a measured reason:
// docs/operations/console.md §3's narrow-key recipe explains why `approve` is NOT covered by `provision` —
// "a key that could provision could add ITSELF to the approver list and then approve" — which is a sentence
// about the approver field on THIS page. Minting the key and naming its principal in `approvers` is one task,
// and it was two tools.

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

export default function PolicyPage() {
  const [projects, setProjects] = useState<ProjectRow[]>([]);
  const [pools, setPools] = useState<PoolRow[]>([]);
  const [project, setProject] = useState("");
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
  const [scopeProvision, setScopeProvision] = useState(false);
  const [scopeApprove, setScopeApprove] = useState(false);
  const [expiresOn, setExpiresOn] = useState("");
  const [mintError, setMintError] = useState("");
  const [minting, setMinting] = useState(false);
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

  // The project list feeds the picker directly rather than through a Panel + onRows. MEASURED, so nobody
  // reads a truncation ceiling into it that is not there: ListProjects (storage/queries/projects.sql) carries
  // no LIMIT and the handler returns a plain {object, data} list — this collection is never cut, exactly like
  // /v1/environments. A page of projects is also not what this screen is for.
  useEffect(() => {
    let live = true;
    Promise.all([apiGet<{ data?: ProjectRow[] }>("/projects"), apiGet<{ data?: PoolRow[] }>("/runner-pools")])
      .then(([projectBody, poolBody]) => {
        if (!live) return;
        const rows = projectBody.data ?? [];
        setProjects(rows);
        setPools(poolBody.data ?? []);
        // Select the first project so the screen opens showing a REAL policy. A form that opens empty invites
        // a save over a document nobody read.
        setProject((current) => (current === "" ? String(rows[0]?.id ?? "") : current));
      })
      .catch((err: unknown) => {
        if (live) setLoadError(detail(err, "the projects and pools could not be read"));
      });
    return () => {
      live = false;
    };
  }, []);

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

      {/* THE ASSIGNMENT SENTENCE STAYS OPEN. It changes what you DO on this screen every single time you use
          it, which is the same test the environments page applied to decide what collapses. */}
      <p className="muted" data-testid="policy-assignment-note">
        <strong>This form writes the WHOLE policy.</strong> The five fields you see here are, after you save,
        the entirety of the project&apos;s <code>config_policy</code> — <code>PATCH /v1/projects/{"{id}"}</code>{" "}
        replaces the stored document rather than merging into it. That is why the form reads the current policy
        before it shows you anything: a field you cannot see is a field you did not send.
      </p>

      <Picker
        id="policy-project"
        label="Project"
        value={project}
        onChange={setProject}
        options={projectOptions}
        testId="policy-project-select"
        hint="Both halves of this page act on this project: its policy document, and the keys that reach it."
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
        note={
          <>
            Empty means <em>unset</em>, and unset is not the same as restrictive: an empty{" "}
            <code>allowed_tools</code> lets the deployment default decide, and an empty <code>approvers</code>{" "}
            permits everyone. Lists are comma-separated, exactly as{" "}
            <code>palai admin project set-policy</code> takes them.
          </>
        }
        fields={[
          {
            name: "policy-allowed-models",
            label: "Allowed models",
            value: allowedModels,
            onChange: setAllowedModels,
            hint: "Model names a run in this project may ask for. Empty leaves the deployment's own routing in charge.",
            testId: "policy-allowed-models-input",
          },
          {
            name: "policy-allowed-tools",
            label: "Allowed tools",
            value: allowedTools,
            onChange: setAllowedTools,
            hint: "The ceiling: a tool not named here cannot be granted by an agent revision or a tool set.",
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
            hint: "Principals allowed to decide this project's approvals: a principal id, key:<api_key_id>, or slack:<team>:<user>.",
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
          <p className="status" data-glyph="warn" data-testid="policy-approvers-permissive">
            <span className="glyph" aria-hidden="true">
              !
            </span>{" "}
            <strong>An empty approver list permits every key in this project to approve.</strong> It is not a
            closed gate — <code>ConfigPolicy.ApproverAllowed</code> admits every principal when the list is
            unconfigured (<code>HIL-P11</code>). Name at least one principal if the approval gate is meant to
            decide anything.
          </p>
        ) : null}
      </ResourceForm>

      <Panel<APIKeyRow>
        title="API keys"
        testId="panel-api-keys"
        fetchPath="/api-keys"
        reloadKey={keyReload}
        onRows={setKeyRows}
        note="Metadata only. A key's value is returned by the create call and by nothing else — there is no route that reads one back."
        columns={[
          { header: "ID", render: (r) => <code>{String(r.id ?? "")}</code> },
          { header: "Project", render: (r) => <code>{String(r.project_id ?? "")}</code> },
          {
            header: "Capabilities",
            render: (r) =>
              (r.scopes ?? []).length === 0 ? (
                // THE EMPTY SET IS THE DANGEROUS ONE and it must not render as a blank cell: middleware's
                // Scope.HasScope returns true for EVERY capability when the set is empty.
                <strong>every capability</strong>
              ) : (
                <>{(r.scopes ?? []).join(", ")}</>
              ),
          },
          { header: "Revoked", render: (r) => (r.revoked_at === null || r.revoked_at === undefined ? "—" : String(r.revoked_at)) },
          {
            header: "Revoke",
            render: (r) =>
              r.revoked_at === null || r.revoked_at === undefined ? (
                <button type="button" data-testid={`revoke-${String(r.id ?? "")}`} onClick={() => setRevoking(r)}>
                  Revoke {String(r.id ?? "")}
                </button>
              ) : (
                <>—</>
              ),
          },
        ]}
      />

      {revokeStatus === "" ? null : (
        <p data-testid="key-revoke-status">
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

      <ResourceForm
        title="Mint an API key"
        testId="key-mint"
        note={
          <>
            The value is shown <strong>once</strong>, here, and is retrievable from nowhere afterwards.{" "}
            <code>docs/operations/console.md</code> §3 explains why the console wants a key holding{" "}
            <code>provision</code> and <code>approve</code> and nothing else.
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
            <code>provision</code> — organizations, projects, keys, model wiring, environments, tools, and{" "}
            <strong>this policy form</strong>
          </label>
          <p className="muted" id="key-scope-provision-hint">
            Everything on the admin surface. A key with this can rewrite the approver list above.
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

      {/* THE CEILING THIS SCREEN CANNOT CLOSE (CON-P2). Minting a narrow key here does not narrow the key the
          console itself is holding — that one is in the server process's environment. */}
      <p className="muted" data-testid="policy-console-key-note">
        <strong>This console&apos;s own key may still hold every capability</strong> — minting a narrow key
        here does not change it. The relay presents whatever <code>PALAI_API_KEY</code> the console process was
        started with, so narrowing that one means restarting the console with a key minted here
        (<code>CON-P2</code>).
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
              <dd>{String(keyRows.filter((k) => k.revoked_at === null || k.revoked_at === undefined).length - 1)} still live after this one</dd>
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
