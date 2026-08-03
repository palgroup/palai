"use client";

import { useEffect, useState } from "react";

import { FormDialog } from "@/components/FormDialog";
import { ResourceForm, type FormField } from "@/components/ResourceForm";
import { Button } from "@/components/ui/Button";
import { Stamp } from "@/components/Session";
import { apiGet, apiSend, RelayError } from "@/lib/api";
import { DESIRED_HELP, type DeploymentBody, type DeploymentSetting } from "@/lib/deployment";

// CONFIGURING A POOL, CONFIGURING ONE MACHINE, AND SAYING WHAT THE MACHINE DID WITH IT.
//
// THE THREE SCOPES ARE THE WHOLE SUBJECT. `control_plane` is the process every project shares and it is
// configured on /deployment; `runner_pool` is every machine in one pool; `runner_machine` is ONE machine,
// laid over its pool's document key by key with the machine winning
// (internal/store/deployment_desired.go's DesiredSettingsForMachine). This file is the second and third.
//
// ONE WRITE ROUTE FOR ALL THREE, and that is the server's decision rather than a convenience: the write is
// PUT /v1/deployment/desired carrying `plane` and `scope_id`, so every scope goes through one validator.
// The READS are split — GET /v1/runner-pools/{id}/desired and GET /v1/runners/{id}/desired — because
// GET /v1/deployment reports THIS PROCESS's environment and a machine's document belongs to the surface
// that reports that machine (api/runners.go's desiredForScope carries the argument).
//
// WHICH FIELDS EXIST IS THE SERVER'S DECISION, NEVER THIS FILE'S — the same rule
// app/deployment/DesiredConfig.tsx:35-38 states, and it is not a style: the catalogue is an allow-list that
// drops every filesystem path structurally, so a console carrying its own copy of it would be a second
// opinion about a security boundary, one that keeps offering a control after the server has withdrawn it.
// There is no list of setting names anywhere below.
//
// AND BOTH FORMS OFFER THE **SAME** SET, which looks like a bug and is the model. `runner_pool` and
// `runner_machine` differ in SCOPE and not in READER: both documents are read by cmd/runner, so
// deployment_desired.go:201 compares `readerOf(plane)` rather than the plane string, and its own comment
// says why a strict equality would be wrong — it would refuse PALAI_RUNNER_CONCURRENCY, catalogued
// `runner_pool`, in the machine document that exists precisely to set it per machine. The selector here is
// therefore the catalogue's plane (`runner_pool`) for both forms.

/** The plane strings, spelled once. api/deployment.go:113-146 declares all three. */
export const PLANE_RUNNER_POOL = "runner_pool";
export const PLANE_RUNNER_MACHINE = "runner_machine";

/** One desired document, exactly as api/deployment_desired.go's DesiredDocument serialises. */
export interface DesiredDocument {
  revision?: number;
  plane?: string;
  scope_id?: string;
  settings?: Record<string, string>;
  written_at?: string;
  written_by?: string;
}

/**
 * The read routes' envelope.
 *
 * `desired: null` IS A REAL ANSWER AND NOT AN ERROR: nobody has written a document for that scope, which is
 * the state every pool and every machine is in until an operator saves one. The form renders empty fields
 * for it. A screen that treated it as a failure would report a fault for the normal case.
 */
export interface DesiredEnvelope {
  object?: string;
  plane?: string;
  scope_id?: string;
  desired?: DesiredDocument | null;
}

/** The runner-plane rows this console may write, chosen by the SERVER's two flags and nothing else. */
function writableRunnerSettings(body: DeploymentBody | null): DeploymentSetting[] {
  return (body?.settings ?? []).filter((row) => row.writable === true && row.plane === PLANE_RUNNER_POOL);
}

const detail = (err: unknown, fallback: string) => (err instanceof RelayError ? err.problem.detail : fallback);

/**
 * ScopedConfigForm is the body of both dialogs: read the catalogue, read this scope's document, write the
 * whole document back.
 *
 * THE WRITE REPLACES. A key that is PRESENT means the operator decided this setting; a key that is ABSENT
 * means "whatever this machine was started with". So an empty field is OMITTED rather than sent as "" —
 * the server refuses "" outright (deployment_desired.go's validateDesiredValue: "an empty string would be
 * stored, exported, and read by the process as unset, which is a third state that looks like the second"),
 * and clearing a field is how an operator gives a setting back.
 *
 * IT DOES NOT VALIDATE, and the refusal it shows is the server's own sentence. The control plane refuses a
 * value its own reader would silently coerce and names the setting when it does; a friendlier sentence
 * written here would know less and drift.
 */
function ScopedConfigForm({
  plane,
  scopeId,
  title,
  testId,
  subject,
  onClose,
  onSaved,
}: {
  plane: string;
  scopeId: string;
  /** The same words the dialog's accessible name says — see components/FormDialog.tsx. */
  title: string;
  testId: string;
  /** What this scope IS, in the words the row showed: a pool's name, a machine's label. */
  subject: string;
  onClose: () => void;
  onSaved: (revision: number) => void;
}) {
  const readPath = plane === PLANE_RUNNER_POOL ? `/runner-pools/${encodeURIComponent(scopeId)}/desired` : `/runners/${encodeURIComponent(scopeId)}/desired`;

  const [catalogue, setCatalogue] = useState<DeploymentBody | null>(null);
  const [savedDoc, setSavedDoc] = useState<DesiredDocument | null>(null);
  const [loaded, setLoaded] = useState(false);
  const [loadError, setLoadError] = useState("");
  const [edits, setEdits] = useState<Record<string, string>>({});
  const [saving, setSaving] = useState(false);
  const [error, setError] = useState("");

  useEffect(() => {
    let live = true;
    void Promise.all([apiGet<DeploymentBody>("/deployment"), apiGet<DesiredEnvelope>(readPath)])
      .then(([body, envelope]) => {
        if (!live) return;
        const saved = envelope.desired ?? null;
        setCatalogue(body);
        setSavedDoc(saved);
        // PREFILLED FROM THE DOCUMENT AND FROM NOTHING ELSE. The effective value a machine is RUNNING is not
        // available here and must not be substituted: this process holds no copy of a runner-plane variable
        // (the catalogue reports `observable: false` for exactly that reason), so prefilling from anything
        // but the saved document would put a number in the field that nobody wrote.
        const current: Record<string, string> = {};
        for (const row of writableRunnerSettings(body)) {
          const value = saved?.settings?.[row.name];
          if (typeof value === "string") current[row.name] = value;
        }
        setEdits(current);
        setLoaded(true);
      })
      .catch((err: unknown) => {
        if (live) setLoadError(detail(err, "this scope's configuration could not be read"));
      });
    return () => {
      live = false;
    };
  }, [readPath]);

  const writable = writableRunnerSettings(catalogue);
  // KEYS THE DOCUMENT CARRIES THAT THIS FORM HAS NO FIELD FOR. It happens when a control plane withdraws a
  // setting from the allow-list after a document was written with it, and it MATTERS because the write
  // replaces: saving would drop them. Naming them is the only honest option — silently resubmitting a value
  // the server now refuses would fail the save, and silently dropping it would delete a setting the operator
  // never touched.
  const orphaned = Object.keys(savedDoc?.settings ?? {}).filter((name) => !writable.some((row) => row.name === name));

  const save = async () => {
    setSaving(true);
    setError("");
    try {
      const settings: Record<string, string> = {};
      for (const [name, value] of Object.entries(edits)) {
        if (value.trim() !== "") settings[name] = value.trim();
      }
      const saved = await apiSend<{ revision?: number }>("PUT", "/deployment/desired", { plane, scope_id: scopeId, settings });
      onSaved(typeof saved.revision === "number" ? saved.revision : 0);
    } catch (err: unknown) {
      // THE DIALOG STAYS OPEN. Closing on a refusal throws away everything the operator typed and leaves a
      // screen with no error on it, which is the refusal announced to nobody.
      setError(detail(err, "the configuration could not be saved"));
    } finally {
      setSaving(false);
    }
  };

  const fields: FormField[] = writable.map((row) => ({
    name: row.name,
    label: row.name,
    value: edits[row.name] ?? "",
    onChange: (value) => setEdits((prev) => ({ ...prev, [row.name]: value })),
    hint: `${DESIRED_HELP[row.value_grammar ?? ""] ?? "a value this machine's own reader parses"}. Leave it empty to use ${row.default}.`,
    testId: `${testId}-${row.name}`,
  }));

  return (
    <ResourceForm
      title={title}
      testId={testId}
      fields={fields}
      submitLabel="Save"
      submittingLabel="Saving…"
      submitTestId={`${testId}-save`}
      submitting={saving}
      error={error === "" ? loadError : error}
      onSubmit={() => void save()}
      note={
        <>
          <strong>{subject}</strong>. This writes a document that <strong>replaces</strong> the one saved for
          this scope; a field left empty is left out of it, which hands that setting back to whatever the
          machine was started with.
        </>
      }
      caveat={{
        summary: "What happens to a machine when this is saved, and when",
        body: (
          <>
            <p className="muted">
              Nothing is restarted and nothing is confirmed. A machine <strong>asks</strong> for its
              configuration on a schedule (about every 30 seconds unless <code>PALAI_SETTINGS_INTERVAL</code>{" "}
              says otherwise on that machine) and applies what it is given, so a value saved here is a value
              that has been <strong>decided</strong>, not one that is running.{" "}
              <strong>Saved is not the same as running.</strong> The Configuration column on the machine list
              is where each machine says what it actually did with it.
            </p>
            <p className="muted">
              A machine document is laid over its pool&apos;s, key by key, with the machine winning. The
              overlay is resolved when a machine asks rather than frozen when this is saved, so a later pool
              edit still reaches a machine that has been configured individually.
            </p>
          </>
        ),
      }}
      actions={
        <Button testId={`${testId}-cancel`} onClick={onClose}>
          Cancel
        </Button>
      }
    >
      {loaded ? (
        <p className="muted" data-testid={`${testId}-revision`}>
          {savedDoc === null ? (
            <>
              Nothing has been saved for this scope. Its machines run on whatever they were started with, and
              this console is not in control of it.
            </>
          ) : (
            <>
              Revision {savedDoc.revision} for {savedDoc.plane ?? plane}, saved by {savedDoc.written_by} at{" "}
              {savedDoc.written_at}.
            </>
          )}
        </p>
      ) : (
        <p role="status" className="muted" data-testid={`${testId}-loading`}>
          Reading which settings this deployment declares writable, and what is saved for this scope.
        </p>
      )}
      {loaded && writable.length === 0 ? (
        <p className="muted" data-testid={`${testId}-none-writable`}>
          This deployment declares <strong>no writable machine setting</strong>. That is the catalogue&apos;s
          answer rather than this screen&apos;s, so there is nothing to fill in here and saving would only
          clear whatever is already stored.
        </p>
      ) : null}
      {orphaned.length === 0 ? null : (
        <p className="muted" data-testid={`${testId}-orphaned`}>
          The saved document also carries {orphaned.join(", ")}, which this deployment no longer declares
          writable. Saving <strong>drops</strong> those, because the write replaces the document rather than
          merging into it.
        </p>
      )}
    </ResourceForm>
  );
}

/**
 * PoolConfigDialog configures every machine in ONE pool.
 *
 * IT IS A SEPARATE MOUNT FROM THE MACHINE ONE RATHER THAN A PROP, and the reason is the accessible name.
 * The two dialogs write to different scopes and a screen reader is told which by the dialog's NAME — a
 * single mount with a computed label would put the two under one string, and tests/constants.ts's
 * FORM_DIALOGS asserts a dialog's name is exactly what it declares. Two names, two rows, two scans.
 */
export function PoolConfigDialog({
  poolId,
  poolName,
  onClose,
  onSaved,
}: {
  poolId: string;
  poolName: string;
  onClose: () => void;
  onSaved: (revision: number) => void;
}) {
  return (
    <FormDialog label="Configure this runner pool" testId="pool-config-dialog" onClose={onClose}>
      <ScopedConfigForm
        plane={PLANE_RUNNER_POOL}
        scopeId={poolId}
        title="Configure this runner pool"
        testId="pool-config"
        subject={`Every machine in ${poolName === "" ? poolId : poolName} (${poolId})`}
        onClose={onClose}
        onSaved={onSaved}
      />
    </FormDialog>
  );
}

/** MachineConfigDialog configures ONE machine, over its pool's document. */
export function MachineConfigDialog({
  runnerId,
  runnerLabel,
  onClose,
  onSaved,
}: {
  runnerId: string;
  runnerLabel: string;
  onClose: () => void;
  onSaved: (revision: number) => void;
}) {
  return (
    <FormDialog label="Configure this machine" testId="machine-config-dialog" onClose={onClose}>
      <ScopedConfigForm
        plane={PLANE_RUNNER_MACHINE}
        scopeId={runnerId}
        title="Configure this machine"
        testId="machine-config"
        subject={`${runnerLabel === "" ? "This machine" : runnerLabel} alone (${runnerId})`}
        onClose={onClose}
        onSaved={onSaved}
      />
    </FormDialog>
  );
}

/** The machine's own report, as api/runners.go's runnerView serialises it — ALL THREE keys or none. */
export interface ConfigReport {
  config_revision?: number;
  config_applied?: Record<string, string>;
  config_reported_at?: string;
}

/**
 * MachineConfigState is the Configuration column: what THIS machine says it did with what was saved for it.
 *
 * THE ABSENCE IS THE MESSAGE, and it is the first branch for that reason. api/runners.go renders
 * `config_revision` / `config_applied` / `config_reported_at` only when the machine has actually reported —
 * "a screen that showed `config_revision: 0` would render that as a number, and an operator reads a number
 * as an answer. Absence they have to ask about." Every runner enrolled before the settings poll existed is
 * in this state and so is every machine that has not come back since.
 *
 * THE VERDICTS ARE THE MACHINE'S AND THIS COLUMN DOES NOT ENUMERATE THEM. packages/runner/settings.go
 * declares two constants — `applied` and `not_read` — and states that `pending_restart` is deliberately
 * absent because nothing writes it. But the constants are not the whole vocabulary: applySettings
 * (packages/runner/serve.go:333) also writes `refused: not a positive integer`, and it is REACHABLE from
 * this console — the control plane's integer grammar admits `0` (validateDesiredValue refuses only a
 * negative) while cmd/runner refuses anything below 1. So the branch below is `applied` versus
 * EVERYTHING ELSE, and everything else is printed in the machine's own words. A console that mapped a
 * closed set of verdicts to sentences would render that state as nothing at all.
 *
 * `saved` IS max(pool document revision, machine document revision), which is what the machine compares
 * against: DesiredSettingsForMachine returns the higher of the two that contributed, "a CHANGE DETECTOR
 * rather than a citation". `savedKnown` is false when either read failed, and then this column says nothing
 * about currency rather than guessing — a wrong "up to date" is the one answer that would be acted on.
 */
export function MachineConfigState({
  runner,
  saved,
  savedKnown,
  testId,
}: {
  runner: ConfigReport;
  saved: number;
  savedKnown: boolean;
  testId: string;
}) {
  const reportedAt = typeof runner.config_reported_at === "string" ? runner.config_reported_at : "";
  if (reportedAt === "") {
    return (
      <span className="cell-none" data-testid={testId} data-config-state="never-reported">
        — never reported. This machine has never said what configuration it is running, so nothing in this
        row is confirmed on it.
      </span>
    );
  }

  const revision = typeof runner.config_revision === "number" ? runner.config_revision : 0;
  const verdicts = Object.entries(runner.config_applied ?? {}).sort(([a], [b]) => a.localeCompare(b));
  const notApplied = verdicts.filter(([, verdict]) => verdict !== "applied");
  const appliedNames = verdicts.filter(([, verdict]) => verdict === "applied").map(([name]) => name);
  const behind = savedKnown && saved > revision;
  const state = behind ? "behind" : notApplied.length > 0 ? "not-applied" : appliedNames.length > 0 ? "applied" : "nothing-saved";

  return (
    <span data-testid={testId} data-config-state={state}>
      {notApplied.length > 0 ? (
        <>
          Revision {revision} reached it and{" "}
          {notApplied.map(([name, verdict], index) => (
            <span key={name}>
              {index === 0 ? "" : ", "}
              <code>{name}</code>{" "}
              {verdict === "not_read" ? (
                <>was NOT read: this runner build has no reader for it, so that saved value will do nothing here, restart or not</>
              ) : (
                <>was not applied, and the machine said: {verdict}</>
              )}
            </span>
          ))}
          .{appliedNames.length === 0 ? "" : ` Applied: ${appliedNames.join(", ")}.`}
        </>
      ) : appliedNames.length > 0 ? (
        <>
          Revision {revision} applied: {appliedNames.join(", ")}.
        </>
      ) : (
        <>Reported in, and has been sent nothing to apply. It runs on whatever it was started with.</>
      )}{" "}
      {behind ? (
        <>
          Revision {saved} was saved after that and has <strong>not reached it yet</strong>: a machine asks
          for its configuration about every 30 seconds, so this is normally a wait rather than a fault.{" "}
        </>
      ) : null}
      <span className="cell-none">
        Reported <Stamp iso={reportedAt} />
      </span>
    </span>
  );
}
