"use client";

import { useParams } from "next/navigation";
import { useEffect, useRef, useState } from "react";

import { Button } from "@/components/ui/Button";
import { FormDialog } from "@/components/FormDialog";
import { ResourceForm } from "@/components/ResourceForm";
import { SecretField, takeSecret } from "@/components/SecretField";
import { CopyButton, shortId, Stamp } from "@/components/Session";
import { apiGet, apiSend, RelayError } from "@/lib/api";
import { operationsOf, type BindingRow, type SecretRefRow } from "@/lib/repositories";

// ONE REPOSITORY BINDING — the whole record, including the four fields a list cannot carry.
//
// WHY THIS ROUTE EXISTS. The list shows what tells two bindings apart (id, repository, provider, branch,
// connection, created); the record also holds a CLONE URL, a data classification, a region constraint and a
// pass-through `policy` object, and putting ten columns on a table is how a table stops being read. Those
// four were on no screen at all before this page — the console wrote them and never showed them back, which
// is the write-and-pray shape §2 forbids.
//
// AND IT IS WHERE A BINDING IS REPAIRED, which is what this page could not do until E30.
//
// It used to carry the sentence "there is no way to change or remove a binding", and that sentence was
// TRUE and was the whole problem: measured on a live stack, 20 bindings held no connection_ref and nothing
// anywhere could give one to any of them. An operator whose run failed with "whether a private repository
// needs a connection_ref naming a token in this deployment's secret store" was being told exactly what to
// do on a screen that could not do it. Registering a second binding was the only move, which is why 8 of
// those 20 were duplicates of one repository.
//
// WHAT IS EDITABLE HERE IS THE CREDENTIAL AND NOTHING ELSE, and that is the API's shape rather than this
// page's restraint: PUT /v1/repository-bindings/{id}/connection is a narrow sub-resource, because provider,
// repository_identity and clone_url are what preparation receipts have already asserted about past runs and
// a mutable identity would falsify them. The binding can also be ARCHIVED — retired from the list and
// refused at run admission — which is not a delete: the row and its receipts survive.
//
// THERE ARE NO TABS HERE, and lib/routes.ts therefore declares no second tab for it: one record, one view.
// A tab strip over a single <dl> would be chrome around a thing that has no second side.

const detail = (err: unknown, fallback: string) => (err instanceof RelayError ? err.problem.detail : fallback);

/** Value renders a field that may be ABSENT on the wire, saying which absence it is. */
function Value({ value, absent }: { value: unknown; absent: string }) {
  if (typeof value !== "string" || value === "") return <span className="cell-none">{absent}</span>;
  return <code>{value}</code>;
}

export default function RepositoryBindingPage() {
  const params = useParams<{ id: string }>();
  const bindingID = Array.isArray(params.id) ? (params.id[0] ?? "") : (params.id ?? "");

  const [binding, setBinding] = useState<BindingRow | null>(null);
  const [error, setError] = useState("");

  useEffect(() => {
    if (bindingID === "") return;
    let live = true;
    apiGet<BindingRow>(`/repository-bindings/${encodeURIComponent(bindingID)}`)
      .then((row) => {
        if (live) setBinding(row);
      })
      .catch((err: unknown) => {
        // The SERVER's sentence: a 404 for an id that was never here and a 401 for a closed door are
        // different facts, and "the binding could not be loaded" is neither of them.
        if (live) setError(detail(err, "the repository binding could not be read"));
      });
    return () => {
      live = false;
    };
  }, [bindingID]);

  const operations = binding === null ? "" : operationsOf(binding);
  const policy = binding?.policy;
  const archived = typeof binding?.archived_at === "string" && binding.archived_at !== "";

  // --- THE REPAIR STATE (E30). Same two-call shape the register flow uses, same boundary. -------------
  const [connOpen, setConnOpen] = useState(false);
  const [connMode, setConnMode] = useState<"existing" | "new" | "none">("new");
  const [connRef, setConnRef] = useState("");
  const [newRefName, setNewRefName] = useState("");
  // The credential is a DOM node and nothing else — components/SecretField.tsx carries the argument.
  const secretRef = useRef<HTMLInputElement | null>(null);
  const [connError, setConnError] = useState("");
  const [connStatus, setConnStatus] = useState("");
  const [saving, setSaving] = useState(false);
  const [archiveOpen, setArchiveOpen] = useState(false);
  const [busy, setBusy] = useState(false);

  const [refs, setRefs] = useState<SecretRefRow[]>([]);
  const [reload, setReload] = useState(0);
  useEffect(() => {
    let live = true;
    apiGet<{ data?: SecretRefRow[] }>("/secret-refs")
      .then((body) => {
        if (live) setRefs(body.data ?? []);
      })
      .catch(() => {
        // An operator who cannot list secret refs can still SEAL one — the `new` mode does not depend on
        // the read — so a failed list leaves the picker out rather than the whole control.
      });
    return () => {
      live = false;
    };
  }, [reload]);
  const refOptions = refs
    .filter((r) => typeof r.name === "string" && r.name !== "")
    .map((r) => ({ value: String(r.name), label: `${String(r.name)}${r.version === undefined ? "" : ` (version ${String(r.version)})`}` }));

  /** reread pulls the binding back from the API rather than patching local state from a response body. */
  async function reread() {
    const row = await apiGet<BindingRow>(`/repository-bindings/${encodeURIComponent(bindingID)}`);
    setBinding(row);
  }

  async function saveConnection() {
    setSaving(true);
    setConnError("");
    setConnStatus("");
    // READ AND CLEAR BEFORE ANY await AND BEFORE ANY EARLY RETURN, the same discipline the register flow
    // keeps: a refusal on the name must not leave the token in a DOM node on a screen the operator has just
    // been told to go and fix something on.
    const token = connMode === "new" ? takeSecret(secretRef) : "";
    let ref = connMode === "existing" ? connRef : "";
    let sealed = false;
    if (connMode === "new") {
      const name = newRefName.trim();
      if (name === "" || token === "") {
        setConnError(
          name === ""
            ? "give the credential a name — the binding stores the name rather than the token."
            : "the credential is empty. Paste the token this repository should be cloned with; nothing was sent.",
        );
        setSaving(false);
        return;
      }
      try {
        await apiSend("POST", "/secret-refs", { name, value: token });
      } catch (err: unknown) {
        setConnError(`the credential could not be sealed: ${detail(err, "the secret ref was refused")}. The binding was not changed.`);
        setSaving(false);
        return;
      }
      ref = name;
      sealed = true;
      setReload((n) => n + 1);
    }
    try {
      // THE NARROW SUB-RESOURCE. Not a PATCH on the binding — see the header.
      await apiSend("PUT", `/repository-bindings/${encodeURIComponent(bindingID)}/connection`, { connection_ref: ref });
      await reread();
      setConnStatus(
        ref === ""
          ? "The connection was detached. This binding now clones with no credential of its own, which only works for a public repository."
          : `This binding now clones under the credential sealed as ${ref}. The token cannot be read back — from this console or any route.`,
      );
      setConnOpen(false);
      setNewRefName("");
    } catch (err: unknown) {
      // The credential OUTLIVES a refused binding write, and saying only "it failed" would leave a live
      // credential the operator does not know exists.
      setConnError(
        sealed
          ? `${detail(err, "the binding could not be updated")} — the credential was already sealed under ${ref}, so fix this and choose it under an existing credential rather than sealing a second copy.`
          : detail(err, "the binding could not be updated"),
      );
    } finally {
      setSaving(false);
    }
  }

  async function flipArchive(retire: boolean) {
    setBusy(true);
    setConnError("");
    setConnStatus("");
    try {
      await apiSend("POST", `/repository-bindings/${encodeURIComponent(bindingID)}/${retire ? "archive" : "unarchive"}`);
      await reread();
      setConnStatus(
        retire
          ? "Archived. It is out of the pickers and NEW RUNS NAMING IT ARE REFUSED; nothing that already ran was touched, and its preparation receipts are unchanged."
          : "Restored. This binding accepts runs again.",
      );
      setArchiveOpen(false);
    } catch (err: unknown) {
      setConnError(detail(err, retire ? "the binding could not be archived" : "the binding could not be restored"));
    } finally {
      setBusy(false);
    }
  }

  return (
    <>
      <nav aria-label="Breadcrumb" className="breadcrumb">
        <ol>
          <li>
            <a href="/repositories">Repositories</a>
          </li>
          <li aria-current="page">
            <CopyButton value={bindingID} label="binding ID" testId="breadcrumb-binding">
              <code>{shortId(bindingID)}</code>
            </CopyButton>
          </li>
        </ol>
      </nav>

      {error === "" ? null : (
        <p role="alert" className="form-error" data-testid="binding-error">
          <span className="glyph" aria-hidden="true">
            ✖︎
          </span>{" "}
          {error}
        </p>
      )}

      <h1 className="page-title" data-testid="binding-title">
        {binding === null ? <code>{shortId(bindingID)}</code> : <code>{String(binding.repository_identity ?? bindingID)}</code>}
      </h1>

      {/* THE READINESS SIGNAL (lib/routes.ts names `panel-binding-record`) is this section, and it renders in
          BOTH settled states and in neither pending one — a handle over a spinner reads as a state the page
          is not in, which is the rule components/Panel.tsx wrote down. */}
      {binding === null && error === "" ? (
        <p className="loading">Loading…</p>
      ) : (
        <section className="panel" data-testid="panel-binding-record" aria-labelledby="binding-record-h">
          <div className="panel-head">
            <h2 id="binding-record-h">Binding</h2>
          </div>

          {/* THE RETIRED BANNER IS A STATEMENT ABOUT RUNS, not about a list. An operator who reads
              "archived" as "hidden" would be surprised by the next refusal; this says what actually
              changed. */}
          {!archived ? null : (
            <p className="form-status" data-testid="binding-archived-note">
              <span className="glyph" aria-hidden="true">⏸</span>{" "}
              <strong>Archived</strong> — this binding is out of the pickers and{" "}
              <strong>new runs naming it are refused</strong>. Nothing that already ran was affected and its
              preparation receipts are unchanged. Restore it to accept runs again.
            </p>
          )}
          {connError === "" ? null : (
            <p role="alert" className="form-error" data-testid="binding-connection-error">
              <span className="glyph" aria-hidden="true">✖</span> {connError}
            </p>
          )}
          {connStatus === "" ? null : (
            <p className="form-status" data-testid="binding-connection-status">
              <span className="glyph" aria-hidden="true">✔</span> {connStatus}
            </p>
          )}
          <dl data-testid="binding-record">
            <dt>Provider</dt>
            <dd>
              <Value value={binding?.provider} absent="— not set" />
            </dd>
            <dt>Repository identity</dt>
            <dd>
              <Value value={binding?.repository_identity} absent="— not set" />
            </dd>
            <dt>Clone URL</dt>
            {/* NOT A LINK. It is fetched by the control plane, never followed by a reader, and an operator's
                browser is the one place this URL should not be opened from — a console that turned an
                operator-supplied URL into a click target on its own origin is the shape of defect this tree
                already fixed once for artifact downloads. */}
            <dd>
              <Value value={binding?.clone_url} absent="— not set" />
            </dd>
            <dt>Default branch</dt>
            <dd>
              <Value value={binding?.default_branch} absent="— not set" />
            </dd>
            <dt>Connection ref</dt>
            <dd data-testid="binding-connection-ref">
              <span data-testid="binding-connection-value">
                {typeof binding?.connection_ref === "string" && binding.connection_ref !== "" ? (
                  <code>{binding.connection_ref}</code>
                ) : (
                  <span className="cell-none">— none (a public repository)</span>
                )}
              </span>{" "}
              {/* THE CONTROL IS BESIDE THE FIELD IT CHANGES. An operator looking for "how do I give this a
                  token" looks at the token row, not at a toolbar. It is hidden while the binding is
                  archived: nothing can run against a retired binding, so the API refuses the write too, and
                  a control that always 404s is worse than no control. */}
              {archived ? null : (
                <Button testId="binding-connection-open" onClick={() => setConnOpen(true)}>
                  {typeof binding?.connection_ref === "string" && binding.connection_ref !== "" ? "Change credential" : "Add a credential"}
                </Button>
              )}
            </dd>
            <dt>Allowed operations</dt>
            <dd data-testid="binding-operations">
              {operations === "" ? <span className="cell-none">— none declared</span> : operations}
            </dd>
            <dt>Data classification</dt>
            <dd>
              <Value value={binding?.data_classification} absent="— none recorded" />
            </dd>
            <dt>Region constraint</dt>
            <dd>
              <Value value={binding?.region_constraint} absent="— none recorded" />
            </dd>
            <dt>Created</dt>
            <dd>
              <Stamp iso={typeof binding?.created_at === "string" ? binding.created_at : null} />
            </dd>
          </dl>

          <h3>Policy</h3>
          {/* AS THE API RETURNED IT. The console passes this object through verbatim on the way in and it has
              no business narrowing it on the way out either: it cannot know which keys this deployment
              reads, so pretty-printing the JSON is the whole of what it can honestly do. */}
          {policy === undefined || policy === null ? (
            <p className="cell-none" data-testid="binding-policy-none">
              — none set. The API takes an open object and this binding names no keys.
            </p>
          ) : (
            <pre className="code" data-testid="binding-policy">
              {JSON.stringify(policy, null, 2)}
            </pre>
          )}

          <p className="muted" data-testid="binding-correction-note">
            <strong>The credential is the only field that can change.</strong> The provider, the repository
            identity and the clone URL are fixed for the life of the binding — every preparation receipt
            already written against it says which repository a past run cloned, and letting the identity move
            would make those receipts wrong with nothing to show it had happened. A binding registered
            against the wrong repository is superseded by registering another and pointing runs at that one;
            this one can then be archived so it stops appearing in pickers.
          </p>
          <h3>Lifecycle</h3>
          <p className="muted">
            {archived
              ? "This binding is retired. Restoring it puts it back in the pickers and lets runs name it again."
              : "Archiving retires a binding without deleting it: the row stays, its preparation receipts stay, and it stops appearing in pickers. New runs naming it are refused."}
          </p>
          <p>
            {archived ? (
              <Button testId="binding-unarchive" disabled={busy} onClick={() => void flipArchive(false)}>
                {busy ? "Restoring…" : "Restore binding"}
              </Button>
            ) : (
              <Button testId="binding-archive" disabled={busy} onClick={() => setArchiveOpen(true)}>
                Archive binding
              </Button>
            )}
          </p>
        </section>
      )}

      {!connOpen ? null : (
        <FormDialog
          label="Set this binding's credential"
          testId="binding-connection-dialog"
          onClose={() => {
            setConnOpen(false);
            setConnError("");
          }}
        >
          <ResourceForm
            title="Set this binding's credential"
            testId="binding-connection"
            note={
              <span>
                The binding stores the NAME of a stored secret and never the token itself. Changing it takes
                effect on the <strong>next run</strong> that names this binding; nothing already running is
                touched, and nothing is verified here — a wrong token shows up as a git authentication
                refusal inside that run.
              </span>
            }
            fields={[
              {
                name: "binding-connection-mode",
                label: "Credential",
                kind: "select",
                value: connMode,
                onChange: (v) => setConnMode(v === "existing" || v === "none" ? v : "new"),
                options: [
                  { value: "new", label: "Seal a new credential now" },
                  ...(refOptions.length === 0 ? [] : [{ value: "existing", label: "Use a credential already sealed" }]),
                  // DETACHING IS OFFERED because it is a real move — the repository became public, or the
                  // token was retired — and leaving it out would be the same dead end one step along.
                  { value: "none", label: "None — detach, this is a public repository" },
                ],
                testId: "binding-connection-mode",
              },
              ...(connMode !== "existing"
                ? []
                : [
                    {
                      name: "binding-connection-pick",
                      label: "Connection ref (a secret HANDLE)",
                      kind: "select" as const,
                      value: connRef,
                      onChange: setConnRef,
                      options: refOptions,
                      placeholder: "Choose a credential…",
                      testId: "binding-connection-select",
                      hint: "Every secret ref in the organization — model-provider keys and environment values included. Pick the one that is a Git credential.",
                    },
                  ]),
              ...(connMode !== "new"
                ? []
                : [
                    {
                      name: "binding-connection-name",
                      label: "Name for this credential",
                      required: true,
                      value: newRefName,
                      onChange: setNewRefName,
                      testId: "binding-connection-name-input",
                      hint: "Re-using an existing name ROTATES that credential rather than creating a second one.",
                    },
                  ]),
            ]}
            submitLabel="Save credential"
            submittingLabel="Saving…"
            submitTestId="binding-connection-save"
            submitting={saving}
            error={connError}
            onSubmit={saveConnection}
            actions={
              <Button
                testId="binding-connection-cancel"
                onClick={() => {
                  setConnOpen(false);
                  setConnError("");
                }}
              >
                Cancel
              </Button>
            }
          >
            {connMode !== "new" ? null : (
              <SecretField
                inputRef={secretRef}
                id="field-binding-detail-token"
                label="Credential"
                testId="binding-connection-token-input"
                hint="A personal or fine-grained access token with read access to this repository. It is sealed on submit and is retrievable from nowhere afterwards."
              />
            )}
          </ResourceForm>
        </FormDialog>
      )}

      {!archiveOpen ? null : (
        <FormDialog
          label="Archive this binding"
          testId="binding-archive-dialog"
          onClose={() => setArchiveOpen(false)}
        >
          <ResourceForm
            title="Archive this binding"
            testId="binding-archive-confirm-form"
            // THE DIALOG SAYS BOTH HALVES, and it has to. An operator who reads "archive" as "delete" will
            // never click it; one who reads it as cosmetic will be surprised when their runs start being
            // refused. Neither sentence alone is the truth.
            note={
              <span>
                <strong>Nothing is deleted and this can be undone.</strong> The binding stays, and so does
                every preparation receipt that records what past runs cloned through it.{" "}
                <strong>New runs naming this binding will be refused</strong> until it is restored.
              </span>
            }
            fields={[]}
            submitLabel="Archive binding"
            submittingLabel="Archiving…"
            submitTestId="binding-archive-confirm"
            submitting={busy}
            error=""
            onSubmit={() => void flipArchive(true)}
            actions={
              <Button testId="binding-archive-cancel" onClick={() => setArchiveOpen(false)}>
                Cancel
              </Button>
            }
          />
        </FormDialog>
      )}
    </>
  );
}
