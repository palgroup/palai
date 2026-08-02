"use client";

import { useEffect, useRef, useState } from "react";

import { Button } from "@/components/ui/Button";
import { Menu } from "@/components/ui/Menu";
import { FormDialog } from "@/components/FormDialog";
import { Panel, type Column } from "@/components/Panel";
import { ResourceForm } from "@/components/ResourceForm";
import { SecretField, takeSecret } from "@/components/SecretField";
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
// THE CREDENTIAL IS NEVER A FIELD OF THE BINDING, AND THAT BOUNDARY IS THE API'S RATHER THAN THIS PAGE'S.
// `connection_ref` is an OPAQUE HANDLE — a secret_refs NAME. RepositoryBindingCreate carries ConnectionRef
// and no credential field at all (api/repository_bindings.go:31-41), and the read side of secret_refs
// projects {name, version, updated_at} with no value (identity/secrets.go:173-211). So the binding create
// this page issues can only ever carry a NAME, and nothing this screen does can change that.
//
// WHAT THIS PAGE DOES NOW DO IS TAKE THE VALUE ONCE, ON ITS WAY TO POST /v1/secret-refs (the token flow
// below). The sentence that used to be here — "the credential is not on this screen and cannot be" — was
// true of the binding create and FALSE as a description of the screen the moment sealing moved here, so it
// is worth being exact about which half survived: the value is never a field of a binding, and it IS, for
// the duration of one submit, a DOM node on this page. It is never React state, never a status message,
// never a URL, and takeSecret() clears it on every submit whether that submit succeeded or not.
//
// WHY IT MOVED HERE AT ALL, measured on the live stack 2026-08-02:
//   curl 'http://127.0.0.1:60351/v1/repository-bindings?limit=100' -> 20 rows, connection_ref absent on all 20.
// Nothing had ever used the private-repository path, and the reason was reachable: an operator who wanted one
// had to leave for /registry — a page for MODEL-PROVIDER connections — seal a secret there under a form whose
// every other field is about a model family, and come back. The two API calls stay two calls, because sealing
// and naming genuinely fail separately and one merged "could not create" sends an operator to re-read the
// wrong field. What was removed is the detour, not the boundary.
//
// AND THE PICKER IS NOT A FREE-TEXT BOX EVEN WHEN THE LIST IS EMPTY (the T4 rule, ResourceForm's select arm):
// a typo'd ref is accepted by a form, and then fails at CLONE TIME inside a run with a refusal about git
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
  const [open, setOpen] = useState(false);

  const [provider, setProvider] = useState("github");
  const [identity, setIdentity] = useState("");
  const [cloneURL, setCloneURL] = useState("");
  const [defaultBranch, setDefaultBranch] = useState("main");
  const [connectionRef, setConnectionRef] = useState("");
  // THE CREDENTIAL MODE. "none" is a public repository, "existing" points at a secret already sealed, "new"
  // seals one here. It is a select rather than a checkbox because the three states are not a toggle: "no
  // credential" is a DECISION somebody made about a public repository, and a control that reads as off/on
  // would make it look like a field somebody forgot.
  const [connectionMode, setConnectionMode] = useState<"none" | "existing" | "new">("none");
  const [newRefName, setNewRefName] = useState("");
  // THE CREDENTIAL IS A DOM NODE AND NOTHING ELSE — no useState, deliberately. components/SecretField.tsx
  // carries the argument; the short form is that a controlled input makes the secret React state, which every
  // re-render closes over and which a Server-Component boundary can serialize into a flight payload.
  const secretRef = useRef<HTMLInputElement | null>(null);
  const [operations, setOperations] = useState("");
  const [classification, setClassification] = useState("");
  const [region, setRegion] = useState("");
  const [policy, setPolicy] = useState("");

  const [error, setError] = useState("");
  const [status, setStatus] = useState("");
  const [creating, setCreating] = useState(false);

  // The secret-ref NAMES the connection picker offers.
  //
  // IT IS RE-READ, AND THE REASON IS THE FEATURE ABOVE. This used to say "read once: this page does not write
  // secret refs, so the list cannot move underneath it" — a sentence that stopped being true in the same
  // commit that gave the page a seal control. A stale list here is not cosmetic: an operator who seals
  // `deploy-token`, then registers a SECOND binding against the same repository, would not find the ref they
  // just created and would seal it twice under two names.
  const [refs, setRefs] = useState<SecretRefRow[]>([]);
  const [refsKey, setRefsKey] = useState(0);
  useEffect(() => {
    let live = true;
    apiGet<{ data?: SecretRefRow[] }>("/secret-refs")
      .then((body) => {
        if (live) setRefs(body.data ?? []);
      })
      .catch(() => {
        // A failed read leaves the picker empty, which renders the "no secret refs" note. That is the honest
        // outcome: an operator who cannot read the list cannot choose from it either — and the `new` mode is
        // still open to them, because sealing does not depend on being able to list.
      });
    return () => {
      live = false;
    };
  }, [refsKey]);

  async function create() {
    setCreating(true);
    setError("");
    setStatus("");
    // READ AND CLEAR THE CREDENTIAL FIRST, BEFORE ANY await AND BEFORE ANY VALIDATION THAT CAN RETURN EARLY.
    // takeSecret() reads and resets in one call, so the bytes exist as ONE local for the duration of this
    // function and are copied nowhere. Doing it here rather than at the seal call is deliberate: every early
    // return below (a bad policy, a missing name) would otherwise leave the token sitting in the DOM node on
    // a screen the operator has just been told to go and fix something on.
    //
    // The cost is real and chosen, the same trade components/SecretField.tsx already argues: a refused submit
    // means retyping the token.
    const token = connectionMode === "new" ? takeSecret(secretRef) : "";
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

    // THE REF THE BINDING WILL NAME. For "new" it is minted by sealing; for "existing" it is the picked name;
    // for "none" it stays empty and the binding carries no connection at all.
    let ref = connectionMode === "existing" ? connectionRef : "";
    // Whether a credential is now sealed under `ref` that was NOT there before. It decides what a later
    // binding failure has to say — see the catch below.
    let sealed = false;
    if (connectionMode === "new") {
      const name = newRefName.trim();
      // Refused HERE rather than at the API, because both of these produce a 400 whose text is about a body
      // field an operator never typed. Nothing has been sent at this point.
      if (name === "" || token === "") {
        setError(
          name === ""
            ? "give the credential a name — it is the handle the binding will point at, and the binding stores the name rather than the token."
            : "the credential is empty. Paste the token you want this repository cloned with; nothing was sent.",
        );
        setCreating(false);
        return;
      }
      try {
        // SEAL. The value crosses here and nowhere else: the response projects {name, version, updated_at}.
        await apiSend<{ name?: string; version?: number }>("POST", "/secret-refs", { name, value: token });
      } catch (err: unknown) {
        // A refused SEAL is a name problem, and it is reported as its own failure rather than folded into the
        // binding's — /registry's createConnection makes the same split for the same reason.
        setError(`the credential could not be sealed: ${detail(err, "the secret ref was refused")}. No binding was registered.`);
        setCreating(false);
        return;
      }
      ref = name;
      sealed = true;
      // The picker's list is now stale by exactly this row. Bumping the key re-reads it.
      setRefsKey((n) => n + 1);
    }

    try {
      const body = await apiSend<BindingRow>("POST", "/repository-bindings", {
        provider,
        repository_identity: identity,
        clone_url: cloneURL,
        default_branch: defaultBranch,
        // THE NAME, NEVER THE VALUE — and `ref` is a name in all three modes: picked, just-sealed, or empty.
        // Sent only when there is one. An empty string is a value the API would have to interpret, and a
        // public repository genuinely has no connection.
        ...(ref === "" ? {} : { connection_ref: ref }),
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
          // THE REF BY NAME, NEVER THE VALUE, AND NEVER A MASKED STAND-IN FOR IT: `****` beside a name is how
          // a reader learns to expect a reveal control, and there is none because there is nothing to reveal.
          (ref === ""
            ? ""
            : `It clones under the credential sealed as ${ref}, which cannot be read back from this console or any route. `) +
          "Nothing has been cloned: the first time this binding is exercised is a run that names it.",
      );
      setOpen(false);
      setIdentity("");
      setCloneURL("");
      setNewRefName("");
      setConnectionMode("none");
      setReloadKey((n) => n + 1);
    } catch (err: unknown) {
      // THE CREDENTIAL SURVIVES THIS FAILURE AND THE OPERATOR IS TOLD SO. The secret ref was written before
      // the binding was refused, so the bytes are sealed under a name that now binds nothing — reporting only
      // "the binding was refused" would leave a live credential the operator does not know exists. This is
      // /registry's sentence, on the surface that now has the same two-call shape.
      setError(
        sealed
          ? `${detail(err, "the repository binding could not be registered")} — the credential was already sealed under ` +
              `${ref}, so it is safe to fix the field above and submit again: choose "${ref}" under an existing ` +
              "credential rather than sealing a second copy. (Re-sealing the same name is a rotation, not a duplicate.)"
          : detail(err, "the repository binding could not be registered"),
      );
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
            <Menu
              label={`Actions for binding ${id}`}
              trigger={<span aria-hidden="true">⋯</span>}
              triggerClassName="row-menu-toggle"
              triggerTestId="binding-menu"
              items={[{ label: "Open binding", href: `/repositories/${encodeURIComponent(id)}`, testId: "binding-menu-open" }]}
            />
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
        pageTitle
        fetchPath="/repository-bindings"
        reloadKey={reloadKey}
        columns={columns}
        matchOn={(r) => `${String(r.id ?? "")} ${String(r.repository_identity ?? "")} ${String(r.provider ?? "")}`}
        filterLabel="Search bindings by repository or ID"
        filterPlaceholder="Repository or ID…"
        action={
          <Button variant="primary" testId="binding-create-open" onClick={() => setOpen(true)}>
            + Register binding
          </Button>
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
            <Button variant="primary" testId="binding-create-open-empty" onClick={() => setOpen(true)}>
              Register one
            </Button>
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
                // THE MODE COMES BEFORE THE CONTROLS IT DECIDES, so the credential fields do not exist in the
                // DOM until an operator has said they want one. That is why the axe scan in
                // tests/repository-token.spec.ts selects this mode before it runs: a sweep of /repositories at
                // rest cannot see controls that are not rendered, and this console has already lost 144
                // controls from a contrast sweep exactly that way.
                name: "binding-connection-mode",
                label: "Repository access",
                kind: "select",
                value: connectionMode,
                onChange: (v) => setConnectionMode(v === "existing" || v === "new" ? v : "none"),
                options: [
                  { value: "none", label: "None — a public repository" },
                  // Offered only when there is something to choose. A mode that leads to an empty picker is a
                  // dead end an operator has to back out of.
                  ...(refOptions.length === 0 ? [] : [{ value: "existing", label: "Use a credential already sealed" }]),
                  { value: "new", label: "Seal a new credential now" },
                ],
                testId: "binding-connection-mode",
                hint: "A private repository clones under a token this platform holds for you. Sealing it here stores it in the platform's secret store — it is not placed on any machine, and it travels with the deployment.",
              },
              ...(connectionMode !== "existing"
                ? []
                : [
                    {
                      name: "binding-connection-ref",
                      label: "Connection ref (a secret HANDLE)",
                      kind: "select" as const,
                      value: connectionRef,
                      onChange: setConnectionRef,
                      options: refOptions,
                      placeholder: "Choose a credential…",
                      testId: "binding-connection-select",
                      // NO `manage` LINK, AND THE OMISSION IS MEASURED. This field used to carry
                      // `{href: "/", label: "Manage secret refs"}` — the dashboard, which manages nothing of
                      // the sort. `ls app/` names nineteen routes and NOT ONE of them is a secret-ref screen:
                      // the only surfaces that write a secret ref are /registry (a model-provider connection),
                      // /environments (a value under the derived name `env:<id>:<key>`), this control, and the
                      // CLI (`palai secret create|list|get|rotate`, cmd/cli/internal/admin/admin.go:134). A
                      // link that promises a management screen there is not is the same defect as a comment
                      // claiming a property the code lacks, in a form the reader can click.
                      hint: "The NAME of a stored secret, never its value. This list is every secret ref in the organization — model-provider keys and environment values included — so pick the one that is a Git credential. There is no screen that lists them by purpose; `palai secret list` is the other way to see them.",
                    },
                  ]),
              ...(connectionMode !== "new"
                ? []
                : [
                    {
                      name: "binding-connection-name",
                      label: "Name for this credential",
                      required: true,
                      value: newRefName,
                      onChange: setNewRefName,
                      testId: "binding-connection-name-input",
                      hint: "The handle the binding points at, e.g. github-palgroup. Re-using an existing name ROTATES that credential rather than creating a second one.",
                    },
                  ]),
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
            // THE CREDENTIAL FIELD IS PASSED AS A CHILD RATHER THAN AS A FormField, and that is ResourceForm's
            // own rule rather than a shortcut: FormField's contract is a controlled value/onChange pair, and
            // putting a secret through it would make the secret React state. It still renders INSIDE the form
            // and after the fields, so it stays in document order and therefore in tab order.
            actions={
              <Button
                testId="binding-create-cancel"
                onClick={() => {
                  setOpen(false);
                  setError("");
                }}
              >
                Cancel
              </Button>
            }
          >
            {connectionMode !== "new" ? null : (
              <SecretField
                inputRef={secretRef}
                id="field-binding-connection-token"
                label="Credential"
                testId="binding-connection-token-input"
                hint="A personal or fine-grained access token with read access to this repository. It is sealed on submit and is retrievable from nowhere afterwards — not from this console, not from any route. A wrong one shows up as a git authentication refusal inside the first run that names this binding."
              />
            )}
          </ResourceForm>
        </FormDialog>
      ) : null}
    </>
  );
}
