"use client";

import { useEffect, useState } from "react";

import { Panel } from "@/components/Panel";
import { ResourceForm } from "@/components/ResourceForm";
import { apiGet, apiSend, RelayError } from "@/lib/api";

// THE REPOSITORY BINDING SCREEN (E25 T6, plan §T6).
//
// WHY THIS IS ONE OF THE BLOCKING PAGES, measured rather than asserted: Palai's promise is an agent that
// writes code, and a coding run attaches its workspace through the `repository` field of POST /v1/responses,
// which names a REPOSITORY BINDING. `palai up` creates none — without Slack a fresh stack holds zero — so
// until this page existed a self-hosting operator's only way to get one was `curl`. The write route has
// existed since E09 T10 and the read halves since E13 T4 (api/router.go:42-46); what was missing was a form.
//
// THE CREDENTIAL IS NOT ON THIS SCREEN AND CANNOT BE. `connection_ref` is an OPAQUE HANDLE — a secret_refs
// NAME — and it is CHOSEN from GET /v1/secret-refs rather than typed. That is a structural property of the
// API rather than a courtesy of this page: RepositoryBindingCreate carries ConnectionRef and no credential
// field at all (api/repository_bindings.go:28-39), and the read side of secret_refs projects
// {name, version, updated_at} with no value (identity/secrets.go). So the strongest thing this page can leak
// is the NAME of a credential, which is the name an operator chose.
//
// AND IT IS NOT A FREE-TEXT BOX EVEN WHEN THE LIST IS EMPTY (the T4 rule, ResourceForm's select arm): a
// typo'd ref is accepted by a form, and then fails at CLONE TIME inside a run with a refusal about git
// authentication — as far from the field that caused it as a refusal can get.
//
// THREE CEILINGS ARE ON THE SCREEN, because each one is a thing an operator would otherwise assume:
//   1. Registering a binding does NOT prove the repository is reachable. Nothing is cloned here, no
//      credential is exercised, and no permission is checked. The first proof is a run.
//   2. There is no DELETE and no PATCH (api/router.go:42-61 mounts three GETs and one POST). This console
//      creates and reads; it does not correct.
//   3. `policy` is passed through as JSON because the API takes an open object and the console has no
//      business narrowing it — which also means the console cannot tell an operator whether a key in it
//      means anything.

interface BindingRow extends Record<string, unknown> {
  id?: string;
  provider?: string;
  repository_identity?: string;
  clone_url?: string;
  default_branch?: string;
  connection_ref?: string;
  allowed_operations?: unknown;
}

interface SecretRefRow {
  name?: string;
  version?: number;
}

const detail = (err: unknown, fallback: string) => (err instanceof RelayError ? err.problem.detail : fallback);

/** csv splits a comma-separated field into a trimmed list, dropping empties. The API takes an array. */
const csv = (raw: string): string[] =>
  raw
    .split(",")
    .map((s) => s.trim())
    .filter((s) => s !== "");

export default function RepositoriesPage() {
  const [reloadKey, setReloadKey] = useState(0);

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
      setStatus(
        `Registered ${String(body.repository_identity ?? identity)} as ${String(body.id ?? "?")}. ` +
          "Nothing has been cloned: the first time this binding is exercised is a run that names it.",
      );
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

  return (
    <>
      <p className="muted" data-testid="binding-reachability-note">
        A repository binding is the object a coding run attaches its workspace through. <strong>Registering
        one does not prove the repository exists, is reachable, or that the credential behind{" "}
        <code>connection_ref</code> can read it</strong> — nothing is cloned on this screen and no permission
        is checked. The first thing that exercises a binding is a run, and that is where a wrong provider,
        a wrong identity or a revoked credential shows up.
      </p>
      <details className="notes" data-testid="repositories-standing-notes">
        <summary>A binding cannot be changed or removed from here</summary>
      <p className="muted" data-testid="binding-correction-note">
        <strong>There is no way to change or remove a binding from here.</strong> The API mounts a create and
        two reads and no PATCH or DELETE, so this console creates and reads; it does not correct. A binding
        registered wrongly is superseded by registering another and pointing runs at that one.
      </p>
      </details>

      <Panel<BindingRow>
        title="Repository bindings"
        testId="panel-repository-bindings"
        fetchPath="/repository-bindings"
        reloadKey={reloadKey}
        note="Provider + repository identity are the AUTHORITATIVE identity; a display name or a URL is not trusted as one. The connection ref is a handle — no credential is on this surface."
        columns={[
          { header: "ID", render: (r) => <code>{String(r.id ?? "")}</code> },
          { header: "Provider", render: (r) => String(r.provider ?? "") },
          { header: "Repository", render: (r) => <code>{String(r.repository_identity ?? "")}</code> },
          { header: "Default branch", render: (r) => String(r.default_branch ?? "") },
          { header: "Connection ref", render: (r) => (r.connection_ref ? <code>{String(r.connection_ref)}</code> : "— none (public)") },
          { header: "Allowed operations", render: (r) => (Array.isArray(r.allowed_operations) ? r.allowed_operations.join(", ") : "") },
        ]}
      />

      <ResourceForm
        title="Register a repository binding"
        testId="repository-binding"
        note={
          <>
            <code>clone_url</code> must be an <code>http(s)</code> URL — a <code>file:</code> or local path is
            refused by the API, because on a collapsed single-host deployment it would let one tenant point a
            clone at another tenant&apos;s workspace.
          </>
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
                <strong>This organization has no secret refs.</strong> A private repository needs one first:
                write it with <code>palai secret create</code>, or write an environment value on the{" "}
                <a href="/environments">Environments</a> page (that creates a secret ref too). A public
                repository needs none — leave this binding without a connection.
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
        status={status}
        onSubmit={create}
      />
    </>
  );
}
