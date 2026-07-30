"use client";

import { useCallback, useEffect, useRef, useState } from "react";

import { Panel } from "@/components/Panel";
import { ResourceForm } from "@/components/ResourceForm";
import { SecretField, takeSecret } from "@/components/SecretField";
import { apiGet, apiSend, RelayError } from "@/lib/api";

// THE ENVIRONMENT CONSOLE — the field that is written and never seen again (E25 T4, plan §T4).
//
// THIS IS THE SCREEN THIS PLAN'S OWN ANCESTOR REFUSED, and the refusal is worth reading before the code
// (plan §3.6 D1). phase-23 measured four extra exposures a value-taking form adds — the browser process, the
// DOM, autofill, and "the real issue: an origin that today has no identity" — and concluded the console
// should never take a value at all. Three of the four are now NAMED by the `new-password` token
// (components/SecretField.tsx) and the fourth is what E25 T1's login closed. What that plan never measured is
// that the un-readability is TRACEABLE IN THE TREE: exactly two queries touch `ciphertext`
// (storage/ciphertext_sweep_test.go pins the pair), the one that decrypts is reachable from no /v1 route, and
// the DB itself carries `REVOKE UPDATE, DELETE` on secret_refs. A form that round-trips a value is a
// different design from a field that never renders back — this is the second, and tests/secret-never-returns
// .spec.ts is what makes that a measurement rather than a description.
//
// WHAT THIS SCREEN CAN NEVER SHOW: a value. There is no reveal control, and its absence is asserted (the DOM
// is enumerated and no control offers to unmask anything) rather than eyeballed. What it shows is an
// INVENTORY: key names, versions, and when each version was written.
//
// ROTATE IS THE SAME ROUTE AS CREATE, because secret_refs is append-only (plan §3.6 D13): there is no
// in-place update that would make a first write and a later one different operations. One button, one path,
// and the version number is how the operator sees which happened.
//
// REMOVING A KEY REMOVES THE BINDING, NOT THE BYTES, and the confirmation says so in those words. A delete
// button that does not delete what a human thinks it deletes is worse than no button — an operator who
// believes a credential is gone will not go and revoke it at the service that issued it.

interface EnvironmentRow extends Record<string, unknown> {
  id?: string;
  name?: string;
  description?: string;
  key_count?: number;
}

interface EnvironmentKey {
  key: string;
  version: number;
  updated_at?: string;
}

// EnvironmentDetail is GET /v1/environments/{id} as identity/environments.go actually projects it. There is
// no `value` on the key view and no parameter that would add one — the field set is pinned by a Go test
// (environments_view_test.go), so this type cannot drift into asking for something the API refuses to send.
interface EnvironmentDetail {
  id?: string;
  name?: string;
  keys?: EnvironmentKey[];
}

const detail = (err: unknown, fallback: string) => (err instanceof RelayError ? err.problem.detail : fallback);

export default function EnvironmentsPage() {
  // The environments the LIST rendered. The picker is built from these — see Panel's onRows.
  const [environments, setEnvironments] = useState<EnvironmentRow[]>([]);
  const [reloadKey, setReloadKey] = useState(0);

  const [newName, setNewName] = useState("");
  const [newDescription, setNewDescription] = useState("");
  const [createError, setCreateError] = useState("");
  const [createStatus, setCreateStatus] = useState("");
  const [creating, setCreating] = useState(false);

  const [selected, setSelected] = useState("");
  const [keyName, setKeyName] = useState("");
  // THE SECRET IS A DOM NODE AND NOTHING ELSE. No useState here, deliberately — see SecretField's header.
  const secretRef = useRef<HTMLInputElement | null>(null);
  const [writeError, setWriteError] = useState("");
  const [writeStatus, setWriteStatus] = useState("");
  const [writing, setWriting] = useState(false);

  const [keys, setKeys] = useState<EnvironmentKey[] | null>(null);
  const [keysError, setKeysError] = useState("");
  const [unbindStatus, setUnbindStatus] = useState("");

  // loadKeys reads the selected environment's key NAMES, versions and update times. Called on selection and
  // after every write, so the version an operator just moved is the version they see.
  const loadKeys = useCallback(async (id: string) => {
    if (id === "") {
      setKeys(null);
      return;
    }
    try {
      const body = await apiGet<EnvironmentDetail>(`/environments/${encodeURIComponent(id)}`);
      setKeys(body.keys ?? []);
      setKeysError("");
    } catch (err: unknown) {
      setKeys(null);
      setKeysError(detail(err, "the environment could not be read"));
    }
  }, []);

  useEffect(() => {
    void loadKeys(selected);
  }, [selected, loadKeys]);

  async function createEnvironment() {
    setCreating(true);
    setCreateError("");
    setCreateStatus("");
    try {
      const body = await apiSend<EnvironmentRow>("POST", "/environments", {
        name: newName,
        description: newDescription,
      });
      setCreateStatus(`Created ${body.name ?? newName} (${body.id ?? "?"}). It holds no keys yet.`);
      setNewName("");
      setNewDescription("");
      setReloadKey((n) => n + 1);
      if (body.id !== undefined) setSelected(body.id);
    } catch (err: unknown) {
      setCreateError(detail(err, "the environment could not be created"));
    } finally {
      setCreating(false);
    }
  }

  // writeValue is CREATE AND ROTATE. The value is read out of the DOM node and the node is cleared in the
  // same call (takeSecret), so it exists as one local string for the duration of one fetch and is copied
  // nowhere — not into state, not into a log, not into a status message.
  async function writeValue() {
    setWriting(true);
    setWriteError("");
    setWriteStatus("");
    const value = takeSecret(secretRef);
    try {
      const body = await apiSend<EnvironmentKey>("POST", `/environments/${encodeURIComponent(selected)}/values`, {
        key: keyName,
        value,
      });
      // The STATUS names the key and the version — never the value, and never a masked stand-in for it
      // either: `****` next to a key name is how a reader learns to expect a reveal control.
      setWriteStatus(`${body.key} is now at version ${body.version}. The value cannot be read back.`);
      setKeyName("");
      await loadKeys(selected);
      setReloadKey((n) => n + 1); // the key COUNT on the list row moved
    } catch (err: unknown) {
      setWriteError(detail(err, "the value could not be written"));
    } finally {
      setWriting(false);
    }
  }

  // unbind removes the MEMBERSHIP row. The confirmation is a native dialog on purpose.
  //
  // ponytail: window.confirm, not a modal component. It is keyboard-operable, screen-reader-announced and
  // focus-trapped by the browser for free — every property a hand-rolled dialog has to re-earn and that axe
  // cannot check for us. Upgrade path: if this screen ever needs a confirmation with more than one sentence
  // and a yes/no, build the dialog then.
  async function unbind(key: string) {
    const answer = window.confirm(
      `Remove ${key} from this environment?\n\n` +
        "This removes the BINDING, not the stored value. secret_refs is append-only (the database grants no " +
        "DELETE), so the sealed versions are retained for audit and become unreachable — nothing names them " +
        "afterwards and no run receives this key again. If the credential itself must die, revoke it at the " +
        "service that issued it.",
    );
    if (!answer) return;
    setUnbindStatus("");
    try {
      const body = await apiSend<{ key?: string; note?: string }>("DELETE", `/environments/${encodeURIComponent(selected)}/values/${encodeURIComponent(key)}`);
      // The server's OWN words about what was removed. The console does not paraphrase a claim about whether
      // bytes still exist.
      setUnbindStatus(body.note ?? `${body.key ?? key}: the binding was removed.`);
      await loadKeys(selected);
      setReloadKey((n) => n + 1);
    } catch (err: unknown) {
      setKeysError(detail(err, "the key could not be unbound"));
    }
  }

  const options = environments
    .filter((e) => typeof e.id === "string" && e.id !== "")
    .map((e) => ({ value: String(e.id), label: `${String(e.name ?? e.id)} (${String(e.id)})` }));
  const selectedName = environments.find((e) => e.id === selected)?.name ?? selected;

  return (
    <>
      <p className="muted" data-testid="env-scope-note">
        An environment is a named group of <code>KEY=value</code> pairs an agent&apos;s shell commands
        receive. It is scoped to the <strong>organization</strong>, not to a project — the same scope{" "}
        <code>secret_refs</code> has — so every project in this organization sees these environments and an
        agent in any of them can be bound to one. There is no per-environment permission: any API key that can
        provision can read every key NAME here and write every value.
      </p>
      <p className="muted" data-testid="env-writeonly-note">
        <strong>A value cannot be read back once written.</strong> This screen shows key names, versions and
        update times — never a value, and there is no button that would reveal one. If you lose a value, write
        a new one (that is what <em>rotate</em> is). Giving an agent a secret is the agent having that secret:
        put here only credentials you would give to the person the agent stands in for. See{" "}
        <code>docs/operations/environments.md</code>.
      </p>

      <Panel<EnvironmentRow>
        title="Environments"
        testId="panel-environments"
        fetchPath="/environments"
        reloadKey={reloadKey}
        onRows={setEnvironments}
        note="Key COUNTS only. Select an environment below to see its key names and versions."
        columns={[
          { header: "ID", render: (r) => String(r.id ?? "") },
          { header: "Name", render: (r) => String(r.name ?? "") },
          { header: "Description", render: (r) => String(r.description ?? "") },
          { header: "Keys", render: (r) => String(r.key_count ?? 0) },
        ]}
      />

      <div id="environment-create">
        <ResourceForm
          title="Create an environment"
          testId="environment-create"
          note="It starts with no keys — create the group, then fill it. A name must be unique in this organization."
          fields={[
            {
              name: "environment-name",
              label: "Name",
              required: true,
              value: newName,
              onChange: setNewName,
              hint: "For example: production, staging, jira-readonly.",
              testId: "env-name-input",
            },
            {
              name: "environment-description",
              label: "Description",
              kind: "textarea",
              value: newDescription,
              onChange: setNewDescription,
              hint: "What this group is for. Optional, and never a place for a value.",
              testId: "env-description-input",
            },
          ]}
          submitLabel="Create environment"
          submitTestId="env-create-button"
          submitting={creating}
          error={createError}
          status={createStatus}
          onSubmit={createEnvironment}
        />
      </div>

      <ResourceForm
        title="Write a value (this is also how you rotate one)"
        testId="environment-value"
        note={
          <>
            <strong>Create and rotate are the same operation</strong> — <code>secret_refs</code> is
            append-only, so writing a key that already exists adds a new version rather than replacing one.
            Key names must match <code>^[A-Z][A-Z0-9_]*$</code> and may not start with <code>PALAI_</code>;
            names the sandbox reserves (<code>PATH</code>, <code>HOME</code>, and others that depend on the
            posture) are refused again at execution time, before any process starts.
          </>
        }
        fields={[
          {
            name: "environment",
            label: "Environment",
            kind: "select",
            required: true,
            value: selected,
            onChange: setSelected,
            options,
            placeholder: "Choose an environment…",
            testId: "value-environment-select",
            // NO FREE-TEXT FALLBACK. An empty list is a refusal with a way forward, never a box to type an id
            // into: a typo'd environment id is accepted by this form, refused at admission, and reported as a
            // failure about a run rather than about a field.
            emptyNote: (
              <>
                <strong>Create an environment first.</strong> There is nothing to write a value into yet.{" "}
                <a href="#environment-create">Go to the create form</a>.
              </>
            ),
          },
          {
            name: "environment-key",
            label: "Key name",
            required: true,
            value: keyName,
            onChange: setKeyName,
            hint: "Upper snake case, e.g. JIRA_TOKEN. This name is what the agent's shell sees.",
            testId: "value-key-input",
          },
        ]}
        submitLabel="Write value"
        submittingLabel="Writing…"
        submitTestId="value-write-button"
        submitting={writing}
        error={writeError}
        status={writeStatus}
        onSubmit={writeValue}
      >
        <SecretField
          inputRef={secretRef}
          label="Value"
          testId="value-secret-input"
          hint="Paste it — pasting is not blocked, and a 40-character credential retyped by eye is a typo in a value nobody can read back to check. The field clears when you submit."
        />
      </ResourceForm>

      <section className="panel" data-testid="panel-environment-keys" aria-labelledby="env-keys-h">
        <h2 id="env-keys-h">Keys in {selected === "" ? "the selected environment" : String(selectedName)}</h2>
        <p className="muted">
          Names, versions and update times. <strong>No values</strong> — this is the whole of what the API
          returns, not a redacted view of something larger.
        </p>
        {keysError !== "" ? (
          <p role="alert" data-testid="env-keys-error">
            Error: {keysError}
          </p>
        ) : null}
        {unbindStatus === "" ? null : (
          <p data-testid="env-unbind-status">
            <span className="glyph" aria-hidden="true">
              ✔
            </span>{" "}
            {unbindStatus}
          </p>
        )}
        {selected === "" ? (
          <p data-testid="env-keys-none-selected">No environment selected.</p>
        ) : keys === null ? (
          <p data-testid="env-keys-loading">Loading…</p>
        ) : keys.length === 0 ? (
          <p data-testid="env-keys-empty">This environment holds no keys yet.</p>
        ) : (
          <table>
            <thead>
              <tr>
                <th scope="col">Key</th>
                <th scope="col">Version</th>
                <th scope="col">Last written</th>
                <th scope="col">Remove binding</th>
              </tr>
            </thead>
            <tbody>
              {keys.map((k) => (
                <tr key={k.key}>
                  <td>{k.key}</td>
                  <td>{k.version}</td>
                  <td>{k.updated_at ?? "—"}</td>
                  <td>
                    {/* The label says BINDING, like the dialog and like the API's own response note. */}
                    <button type="button" data-testid={`unbind-${k.key}`} onClick={() => void unbind(k.key)}>
                      Unbind {k.key}
                    </button>
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        )}
      </section>
    </>
  );
}
