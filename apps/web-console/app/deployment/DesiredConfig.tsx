"use client";

import { useCallback, useEffect, useState } from "react";

import { ResourceForm, type FormField } from "@/components/ResourceForm";
import { FormDialog } from "@/components/FormDialog";
import { Button } from "@/components/ui/Button";
import { apiGet, apiSend, RelayError } from "@/lib/api";
import { DESIRED_HELP, type DeploymentBody, type DeploymentSetting } from "@/lib/deployment";

// THE FLEET COUNTS, read so the scope sentence below can be a NUMBER rather than a hedge. "No setting here
// is per-machine" is a claim a reader has no way to size; "…and this deployment has 3 machines in 17
// pools" is the same claim with the thing it is about in it. A failed read renders the sentence without
// the counts rather than blocking the panel — a diagnostic that cannot read its own source says less, not
// something wrong.
interface FleetCounts {
  runners: number | null;
  pools: number | null;
}

// THE DESIRED CONFIGURATION — what this machine SHOULD be running with, saved on the machine.
//
// THIS IS AN EDIT SURFACE ON A SCREEN WHOSE OWN HEADER SAYS THERE IS NO EDIT CONTROL, so the distinction
// has to be exact or this is the defect the screen exists to expose.
//
//   The table below is EFFECTIVE configuration: what the control-plane PROCESS holds right now. Thirty-two
//   of its thirty-five rows are read from the process environment, which is fixed at exec, so a control
//   that claimed to change one of them live would be "declared, and nothing happens".
//
//   This panel is DESIRED configuration: a document stored on the machine that the NEXT BRING-UP reads and
//   turns into the process's environment (cmd/cli/internal/stack/desired.go). Saving here changes nothing
//   about the running process and this panel never pretends otherwise — the button says "Save", the notice
//   says what has to happen next, and the row shows desired against effective until they agree.
//
// WHICH FIELDS EXIST IS THE SERVER'S DECISION, NEVER THIS FILE'S. The fields are built from the rows the
// API marks `writable`. There is no list of setting names here and there must not be: the allow-list drops
// every filesystem path structurally, and a console carrying its own copy of it would be a second opinion
// about a security boundary — one that keeps offering a control after the server has withdrawn it.
//
// AND THE REFUSAL IS THE SERVER'S TOO. This panel does not validate. The control plane refuses a value its
// own reader would silently coerce (`10min` is not a Go duration; it would become the default and the panel
// would show a wall time the process is not running), and its refusal names the setting and says why — so
// it is shown verbatim in the alert region rather than replaced with a friendlier sentence that knows less.

interface SaveResult {
  revision?: number;
}

export function DesiredConfig({ reloadKey, onSaved }: { reloadKey: number; onSaved: () => void }) {
  const [body, setBody] = useState<DeploymentBody | null>(null);
  const [fleet, setFleet] = useState<FleetCounts>({ runners: null, pools: null });
  const [loadError, setLoadError] = useState("");
  const [open, setOpen] = useState(false);
  // `edits` is the whole document being composed, keyed by setting name. It starts as what the server holds,
  // so a save that touches one field does not silently drop the other ten — the write REPLACES the document.
  const [edits, setEdits] = useState<Record<string, string>>({});
  const [saving, setSaving] = useState(false);
  const [error, setError] = useState("");
  const [status, setStatus] = useState("");

  const load = useCallback(async () => {
    try {
      setBody(await apiGet<DeploymentBody>("/deployment"));
      setLoadError("");
    } catch (err) {
      setLoadError(err instanceof RelayError ? err.problem.detail : "the deployment configuration could not be read");
    }
  }, []);

  useEffect(() => {
    void load();
  }, [load, reloadKey]);

  // The fleet counts are read ONCE and never with the deployment read: they answer a different question
  // (how many machines is this NOT configuring) and a failure to answer it must not take the panel down.
  useEffect(() => {
    let live = true;
    const count = async (path: string) => {
      const page = await apiGet<{ data?: unknown[] }>(path);
      return Array.isArray(page.data) ? page.data.length : null;
    };
    void Promise.all([count("/runners"), count("/runner-pools")])
      .then(([runners, pools]) => {
        if (live) setFleet({ runners, pools });
      })
      .catch(() => {
        /* the scope sentence renders without its counts; see FleetCounts */
      });
    return () => {
      live = false;
    };
  }, []);

  const settings = body?.settings ?? [];
  const writable = settings.filter((row) => row.writable === true);
  const desired = body?.desired ?? null;
  const drifted = desired?.drifted ?? [];

  const openDialog = () => {
    const current: Record<string, string> = {};
    for (const row of writable) {
      if (row.desired_set === true) current[row.name] = row.desired ?? "";
    }
    setEdits(current);
    setError("");
    setStatus("");
    setOpen(true);
  };

  const save = async () => {
    setSaving(true);
    setError("");
    setStatus("");
    try {
      // EVERY EMPTY FIELD IS OMITTED, NOT SENT AS "". That is the document's own contract: a key that is
      // PRESENT means the operator decided this setting; a key that is ABSENT means "whatever this
      // deployment's own default is". The server refuses "" outright rather than storing a third state
      // that looks like the second — so clearing a field here is how an operator gives a setting back.
      const payload: Record<string, string> = {};
      for (const [name, value] of Object.entries(edits)) {
        if (value.trim() !== "") payload[name] = value.trim();
      }
      const saved = await apiSend<SaveResult>("PUT", "/deployment/desired", { settings: payload });
      setStatus(
        `Saved as revision ${saved.revision ?? "?"}. Nothing on this machine has changed yet — run \`palai up\` on it to apply.`,
      );
      setOpen(false);
      await load();
      onSaved();
    } catch (err) {
      // The server's own sentence. It names the setting and says why, which is the whole reason this panel
      // does not write its own.
      setError(err instanceof RelayError ? err.problem.detail : "the desired configuration could not be saved");
    } finally {
      setSaving(false);
    }
  };

  const fields: FormField[] = writable.map((row) => ({
    name: row.name,
    label: row.name,
    value: edits[row.name] ?? "",
    onChange: (value) => setEdits((prev) => ({ ...prev, [row.name]: value })),
    hint: `${DESIRED_HELP[row.value_grammar ?? ""] ?? "a value this deployment's own reader parses"} — leave empty to use ${effectiveOrDefault(row)}`,
    testId: `desired-${row.name}`,
  }));

  return (
    <section className="panel" data-testid="panel-desired-config" aria-labelledby="desired-config-h">
      <header className="panel-head">
        <h2 id="desired-config-h">Desired configuration</h2>
        <Button testId="desired-config-edit" onClick={openDialog} disabled={writable.length === 0}>
          Edit desired configuration
        </Button>
      </header>

      {/* WHAT IT CONFIGURES, IN THE FIRST SENTENCE. It used to say "what this machine should be running
          with", which is wrong in the direction that matters: there are three scopes in this product —
          the control-plane PROCESS, the POOL a machine belongs to, and the RUNNER — and this is the first.
          A reader who takes "machine" literally is wrong about which machines a change reaches. */}
      <p className="muted" data-testid="desired-config-scope-sentence">
        The <strong>control-plane process&apos;s</strong> configuration, stored on the machine that runs it. One process,
        shared by every project and every machine on this deployment
        {fleet.runners === null || fleet.pools === null ? null : (
          <>
            {" "}
            — currently {fleet.runners} machine{fleet.runners === 1 ? "" : "s"} in {fleet.pools} pool
            {fleet.pools === 1 ? "" : "s"}
          </>
        )}
        . <strong>Nothing here is per-machine.</strong> Saving changes nothing about the running process either: every
        setting below is read from that process&apos;s environment, which is fixed when it starts.{" "}
        <code>palai up</code> on the machine reads this document and starts the process with it.
      </p>

      {/* AND WHAT IT IS NOT, named rather than left as an absence. An operator who came here for
          per-machine configuration and finds none must learn that the axis exists elsewhere — otherwise
          they conclude the product cannot do it. PALAI_RUNNER_CONCURRENCY is the one genuinely per-machine
          knob and the control plane holds no copy of it, which is why it is not even listed below. */}
      <p className="muted" data-testid="desired-config-not-per-machine">
        To configure <strong>machines</strong>, use a runner pool on Fleet — a pool&apos;s posture and enrolment are
        enforced by the registry. The one per-machine knob, <code>PALAI_RUNNER_CONCURRENCY</code>, lives on the runner
        container itself: this process holds no copy of it, so it is not on this screen at all rather than reported as
        unset.
      </p>

      {loadError === "" ? null : (
        <p role="alert" className="form-error" data-testid="desired-config-load-error">
          {loadError}
        </p>
      )}

      {/* NO DOCUMENT IS ITS OWN SENTENCE, and it names what IS in control. A blank panel here would read as
          "nothing is configured", when the truth is that something else is configuring it. */}
      {desired === null ? (
        <p className="muted" data-testid="desired-config-absent">
          Nothing has been saved for this machine. Its settings come from the compose file and the environment it was
          started with, exactly as they did before this panel existed — so what is running is whatever the last
          bring-up was handed, and this console is not in control of it.
        </p>
      ) : (
        <>
          <p className="muted" data-testid="desired-config-revision">
            Revision {desired.revision} for the {desired.plane ?? "control_plane"} scope, saved by {desired.written_by} at{" "}
            {desired.written_at}.
          </p>
          {desired.pending ? (
            <p role="status" className="form-status" data-testid="desired-config-pending">
              A bring-up is pending: {drifted.length} setting{drifted.length === 1 ? "" : "s"} — {drifted.join(", ")} —
              differ from what this process is running. Run <code>palai up</code> on the machine.{" "}
              <code>docker compose up -d --force-recreate control-plane</code> will NOT apply it: compose reads the
              shell that runs it, not this document.
            </p>
          ) : (
            <p role="status" className="form-status" data-testid="desired-config-applied">
              This machine is running the configuration that was saved for it.
            </p>
          )}
        </>
      )}

      {/* THE COUNT IS STATED, because it is the one thing a form cannot show: twenty-four of the thirty-five
          settings below are NOT writable from here, each for a reason the table carries in its own row. An
          operator who does not find the setting they came for should learn that it is refused rather than
          conclude the console is incomplete. */}
      <p className="muted" data-testid="desired-config-scope">
        {writable.length} of {settings.length} settings can be saved here. The rest are refused — a filesystem path, an
        image reference, the address this API is reached on, or a destination this deployment sends a credential to —
        and each row below says which.
      </p>

      {open ? (
        <FormDialog label="Edit desired configuration" testId="desired-config-dialog" onClose={() => setOpen(false)}>
          <ResourceForm
            title="Edit desired configuration"
            testId="desired-config-form"
            fields={fields}
            submitLabel="Save"
            submittingLabel="Saving…"
            submitTestId="desired-config-save"
            submitting={saving}
            error={error}
            onSubmit={() => void save()}
            note={
              <>
                This writes a document to the machine. It takes effect on the next <code>palai up</code> — nothing here
                changes the running process. An empty field means &quot;use this deployment&apos;s own default&quot;.
              </>
            }
            actions={
              <Button testId="desired-config-cancel" onClick={() => setOpen(false)}>
                Cancel
              </Button>
            }
          />
        </FormDialog>
      ) : null}

      {status === "" ? null : (
        <p role="status" className="form-status" data-testid="desired-config-status">
          {status}
        </p>
      )}
    </section>
  );
}

// effectiveOrDefault is what an empty field falls back to, in the words the API already uses. It prefers the
// process's OWN value over the catalogue's default sentence, because "leave empty to use 4" is a fact about
// this machine and "leave empty to use 1" is a fact about the code.
function effectiveOrDefault(row: DeploymentSetting): string {
  return row.set ? `the value this process holds (${row.value})` : row.default;
}
