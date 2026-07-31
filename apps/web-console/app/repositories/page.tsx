"use client";

import { useEffect, useState } from "react";

import { FormDialog } from "@/components/FormDialog";
import { Panel, type Column } from "@/components/Panel";
import { ResourceForm } from "@/components/ResourceForm";
import { CopyButton, shortId, Stamp } from "@/components/Session";
import { apiGet, apiSend, RelayError } from "@/lib/api";
import type { BindingRow, SecretRefRow } from "@/lib/repositories";

// THE REPOSITORY BINDING LIST (E25 T6; page-parity pass).
//
// WHY THIS IS ONE OF THE BLOCKING PAGES, measured rather than asserted: Palai's promise is an agent that
// writes code, and a coding run attaches its workspace through the `repository` field of POST /v1/responses,
// which names a REPOSITORY BINDING. `palai up` creates none — without Slack a fresh stack holds zero — so
// until this page existed a self-hosting operator's only way to get one was `curl`.
//
// WHAT THE PAGE-PARITY PASS CHANGED, and the measurement that prompted it (built console, 2026-07-31):
// 2197 characters of <main>, 1650 of them grey prose, and an eight-field registration form open on the page
// under two standing paragraphs. The list itself was fine — six columns, all fields — with a 36-character id
// in the first one and a row that went nowhere.
//
// So: the form is a dialog behind `+ Register binding`, the id cell is a short form with a copy button and a
// link, and the four fields that did NOT fit a list (clone URL, classification, region constraint, and the
// pass-through policy object) are on the binding's own page — which is also where the "you cannot change
// this" sentence now lives, because that is the page an operator goes to looking for an edit control.
//
// THE CREDENTIAL IS NOT ON THIS SCREEN AND CANNOT BE. `connection_ref` is an OPAQUE HANDLE — a secret_refs
// NAME — chosen from GET /v1/secret-refs rather than typed. That is a structural property of the API rather
// than a courtesy of this page: RepositoryBindingCreate carries ConnectionRef and no credential field at all
// (api/repository_bindings.go:28-39), and the read side of secret_refs projects {name, version, updated_at}
// with no value (identity/secrets.go). The strongest thing this page can leak is the NAME of a credential.
//
// AND IT IS NOT A FREE-TEXT BOX EVEN WHEN THE LIST IS EMPTY (the T4 rule, ResourceForm's select arm): a
// typo'd ref is accepted by a form, and then fails at CLONE TIME inside a run with a refusal about git
// authentication — as far from the field that caused it as a refusal can get.

const detail = (err: unknown, fallback: string) => (err instanceof RelayError ? err.problem.detail : fallback);

/** csv splits a comma-separated field into a trimmed list, dropping empties. The API takes an array. */
const csv = (raw: string): string[] =>
  raw
    .split(",")
    .map((s) => s.trim())
    .filter((s) => s !== "");

export default function RepositoriesPage() {
  const [reloadKey, setReloadKey] = useState(0);
  const [menu, setMenu] = useState("");
  const [open, setOpen] = useState(false);

  const [provider, setProvider] = useState("github");
  const [identity, setIdentity] = useState("");
  const [cloneURL, setCloneURL] = useState("");
  const [defaultBranch, setDefaultBranch] = useState("main");
  const [connectionRef, setConnectionRef] = useState("");
  const [operations, setOperations] = useState("");
  const [classification, setClassification] = useState("");
  const [region, setRegion] = useState("");
  const [policy, setPolicy] = useState("");

  const [error, setError] = useState("");
  const [status, setStatus] = useState("");
  const [creating, setCreating] = useState(false);

  // The secret-ref NAMES the connection picker offers. Read once: this page does not write secret refs, so
  // the list cannot move underneath it.
  const [refs, setRefs] = useState<SecretRefRow[]>([]);
  useEffect(() => {
    let live = true;
    apiGet<{ data?: SecretRefRow[] }>("/secret-refs")
      .then((body) => {
        if (live) setRefs(body.data ?? []);
      })
      .catch(() => {
        // A failed read leaves the picker empty, which renders the "no secret refs" note. That is the honest
        // outcome: an operator who cannot read the list cannot choose from it either.
      });
    return () => {
      live = false;
    };
  }, []);

  async function create() {
    setCreating(true);
    setError("");
    setStatus("");
    // The policy object is parsed HERE rather than sent as a string, because the API's field is an object and
    // a string would be a 400 about a type instead of about the JSON the operator wrote.
    let parsedPolicy: Record<string, unknown> | undefined;
    if (policy.trim() !== "") {
      try {
        const value: unknown = JSON.parse(policy);
        if (typeof value !== "object" || value === null || Array.isArray(value)) {
          throw new Error("not an object");
        }
        parsedPolicy = value as Record<string, unknown>;
      } catch {
        setError("policy must be a JSON object, for example {\"require_approval\": true}. Nothing was sent.");
        setCreating(false);
        return;
      }
    }
    try {
      const body = await apiSend<BindingRow>("POST", "/repository-bindings", {
        provider,
        repository_identity: identity,
        clone_url: cloneURL,
        default_branch: defaultBranch,
        // Sent only when one was CHOSEN. An empty string is a value the API would have to interpret, and a
        // public repository genuinely has no connection.
        ...(connectionRef === "" ? {} : { connection_ref: connectionRef }),
        allowed_operations: csv(operations),
        ...(parsedPolicy === undefined ? {} : { policy: parsedPolicy }),
        data_classification: classification,
        region_constraint: region,
      });
      // THE STATUS STAYS ON THE LIST rather than travelling with a navigation, and that is the difference
      // from /agents' create: an agent with no revisions is a lineage the operator must go and configure, so
      // landing on it is the next step; a binding is COMPLETE the moment it is registered and there is
      // nothing to do to it — the API mounts no PATCH. What is worth saying is what did NOT happen.
      setStatus(
        `Registered ${String(body.repository_identity ?? identity)} as ${String(body.id ?? "?")}. ` +
          "Nothing has been cloned: the first time this binding is exercised is a run that names it.",
      );
      setOpen(false);
      setIdentity("");
      setCloneURL("");
      setReloadKey((n) => n + 1);
    } catch (err: unknown) {
      setError(detail(err, "the repository binding could not be registered"));
    } finally {
      setCreating(false);
    }
  }

  const refOptions = refs
    .filter((r) => typeof r.name === "string" && r.name !== "")
    .map((r) => ({ value: String(r.name), label: `${String(r.name)}${r.version === undefined ? "" : ` (version ${String(r.version)})`}` }));

  const columns: Column<BindingRow>[] = [
    {
      header: "ID",
      sort: (r) => String(r.id ?? ""),
      render: (r) => (
        <span className="cell-id-group">
          <a
            className="cell-id"
            href={`/repositories/${encodeURIComponent(String(r.id ?? ""))}`}
            title={String(r.id ?? "")}
            data-testid="binding-link"
          >
            {shortId(String(r.id ?? ""))}
          </a>
          <CopyButton value={String(r.id ?? "")} label="binding ID" testId="binding-copy-id" />
        </span>
      ),
    },
    {
      // PROVIDER + IDENTITY ARE THE AUTHORITATIVE NAME, and they are one cell because they are one fact: a
      // display name or a URL is not trusted as an identity anywhere in this system.
      header: "Repository",
      sort: (r) => `${String(r.provider ?? "")} ${String(r.repository_identity ?? "")}`,
      render: (r) => (
        <a className="cell-name-link" href={`/repositories/${encodeURIComponent(String(r.id ?? ""))}`} data-testid="binding-identity-link">
          <code>{String(r.repository_identity ?? "")}</code>
        </a>
      ),
    },
    { header: "Provider", sort: (r) => String(r.provider ?? ""), render: (r) => String(r.provider ?? "") },
    { header: "Default branch", render: (r) => String(r.default_branch ?? "") },
    {
      header: "Connection",
      // "— none (public)" rather than an em dash: a binding with no connection is a PUBLIC repository, which
      // is a decision somebody made, not a field somebody forgot.
      render: (r) => (r.connection_ref ? <code>{String(r.connection_ref)}</code> : <span className="cell-none">— none (public)</span>),
    },
    {
      // A CREATED COLUMN EXISTS HERE AND NOT ON /agents, and the difference is the projection rather than the
      // screen: contracts.RepositoryBinding carries `created_at` on the wire, while an agent's timestamp
      // never leaves the cursor (lib/agents.ts holds that measurement).
      header: "Created",
      sort: (r) => String(r.created_at ?? ""),
      render: (r) => <Stamp iso={typeof r.created_at === "string" ? r.created_at : null} />,
    },
    {
      header: "",
      // ONE ITEM, AND IT IS THE API RATHER THAN THE DESIGN. api/router.go:44-46 mounts a create and two
      // reads: no PATCH, no DELETE. A second entry here would have to be a control that refuses.
      render: (r) => {
        const id = String(r.id ?? "");
        return (
          <div className="row-menu">
            <button
              type="button"
              className="row-menu-toggle"
              aria-expanded={menu === id}
              aria-controls={`menu-${id}`}
              aria-label={`Actions for binding ${id}`}
              data-testid="binding-menu"
              onClick={() => setMenu(menu === id ? "" : id)}
              onKeyDown={(e) => {
                if (e.key === "Escape") setMenu("");
              }}
            >
              <span aria-hidden="true">⋯</span>
            </button>
            {menu === id ? (
              <div className="row-menu-panel" id={`menu-${id}`}>
                <a href={`/repositories/${encodeURIComponent(id)}`} data-testid="binding-menu-open">
                  Open binding
                </a>
              </div>
            ) : null}
          </div>
        );
      },
    },
  ];

  return (
    <>
      {status === "" ? null : (
        <p className="form-status" data-testid="repository-binding-status">
          <span className="glyph" aria-hidden="true">
            ✔
          </span>{" "}
          {status}
        </p>
      )}

      <Panel<BindingRow>
        title="Repository bindings"
        testId="panel-repository-bindings"
        fetchPath="/repository-bindings"
        reloadKey={reloadKey}
        columns={columns}
        matchOn={(r) => `${String(r.id ?? "")} ${String(r.repository_identity ?? "")} ${String(r.provider ?? "")}`}
        filterLabel="Search bindings by repository or ID"
        filterPlaceholder="Repository or ID…"
        action={
          <button type="button" className="primary" data-testid="binding-create-open" onClick={() => setOpen(true)}>
            + Register binding
          </button>
        }
        emptyNote={
          <>
            <p className="empty-title" data-testid="binding-empty-title">
              No repository bindings yet
            </p>
            <p className="empty-body">
              A binding is the object a coding run attaches its workspace through — the provider, the
              repository&apos;s own identity, and the handle of the credential that reaches it.
            </p>
            <button type="button" className="primary" data-testid="binding-create-open-empty" onClick={() => setOpen(true)}>
              Register one
            </button>
          </>
        }
      />

      {open ? (
        <FormDialog
          label="Register a repository binding"
          testId="binding-create-dialog"
          onClose={() => {
            setOpen(false);
            setError("");
          }}
        >
          <ResourceForm
            title="Register a repository binding"
            testId="repository-binding"
            // THE CEILING IS ON THE CONTROL THAT CAUSES IT. This sentence used to be the first paragraph of
            // the page, read on every visit by an operator who was only looking at the list; it belongs at
            // the moment somebody is about to register one, which is here.
            note={
              <span data-testid="binding-reachability-note">
                <strong>
                  Registering a binding does not prove the repository exists, is reachable, or that the
                  credential behind <code>connection_ref</code> can read it
                </strong>{" "}
                — nothing is cloned here and no permission is checked. The first thing that exercises a
                binding is a run, and that is where a wrong provider, a wrong identity or a revoked credential
                shows up. <code>clone_url</code> must be an <code>http(s)</code> URL: a <code>file:</code> or
                local path is refused by the API, because on a collapsed single-host deployment it would let
                one tenant point a clone at another tenant&apos;s workspace.
              </span>
            }
            fields={[
              {
                name: "binding-provider",
                label: "Provider",
                required: true,
                value: provider,
                onChange: setProvider,
                hint: "The provider id, e.g. github. Together with the repository identity this is what the platform treats as the repository's name.",
                testId: "binding-provider-input",
              },
              {
                name: "binding-identity",
                label: "Repository identity",
                required: true,
                value: identity,
                onChange: setIdentity,
                hint: "The provider's own identity for the repository, e.g. owner/name.",
                testId: "binding-identity-input",
              },
              {
                name: "binding-clone-url",
                label: "Clone URL",
                required: true,
                value: cloneURL,
                onChange: setCloneURL,
                hint: "https://… — this is fetched by the control plane, never by the agent's sandbox.",
                testId: "binding-clone-url-input",
              },
              {
                name: "binding-default-branch",
                label: "Default branch",
                value: defaultBranch,
                onChange: setDefaultBranch,
                hint: "The branch a run starts from when it names no other.",
                testId: "binding-default-branch-input",
              },
              {
                name: "binding-connection-ref",
                label: "Connection ref (a secret HANDLE)",
                kind: "select",
                value: connectionRef,
                onChange: setConnectionRef,
                options: refOptions,
                placeholder: "None — a public repository",
                testId: "binding-connection-select",
                hint: "The NAME of a stored secret, never its value. The credential itself is written by `palai secret create` or by writing an environment value; this screen only points at one.",
                // NO FREE-TEXT FALLBACK, and the empty state has a way forward rather than a box.
                emptyNote: (
                  <>
                    <strong>This organization has no secret refs.</strong> A private repository needs one
                    first: write it with <code>palai secret create</code>, or write an environment value on
                    the <a href="/environments">Environments</a> page (that creates a secret ref too). A
                    public repository needs none — leave this binding without a connection.
                  </>
                ),
              },
              {
                name: "binding-operations",
                label: "Allowed operations",
                value: operations,
                onChange: setOperations,
                hint: "Comma-separated, e.g. clone, push. This is a CEILING on what a run may do with the repository, not a grant.",
                testId: "binding-operations-input",
              },
              {
                name: "binding-classification",
                label: "Data classification",
                value: classification,
                onChange: setClassification,
                hint: "Free-form, recorded on the binding. The platform stores it; it does not enforce it.",
                testId: "binding-classification-input",
              },
              {
                name: "binding-region",
                label: "Region constraint",
                value: region,
                onChange: setRegion,
                hint: "Free-form, recorded on the binding.",
                testId: "binding-region-input",
              },
              {
                name: "binding-policy",
                label: "Policy (JSON object)",
                kind: "textarea",
                value: policy,
                onChange: setPolicy,
                hint: 'Optional, passed through verbatim, e.g. {"require_approval": true}. The console does not validate the keys — it cannot know which ones this deployment reads.',
                testId: "binding-policy-input",
              },
            ]}
            submitLabel="Register binding"
            submittingLabel="Registering…"
            submitTestId="binding-create-button"
            submitting={creating}
            error={error}
            onSubmit={create}
            actions={
              <button
                type="button"
                data-testid="binding-create-cancel"
                onClick={() => {
                  setOpen(false);
                  setError("");
                }}
              >
                Cancel
              </button>
            }
          />
        </FormDialog>
      ) : null}
    </>
  );
}
