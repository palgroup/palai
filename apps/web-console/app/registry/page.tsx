"use client";

import { useRef, useState } from "react";

import { NameCell, Panel, type Column } from "@/components/Panel";
import { ResourceForm } from "@/components/ResourceForm";
import { SecretField, takeSecret } from "@/components/SecretField";
import { CopyButton, shortId, Stamp } from "@/components/Session";
import { Button } from "@/components/ui/Button";
import { apiSend, RelayError } from "@/lib/api";

// MODEL WIRING — and since E29 this screen WRITES, which is the product's headline promise finally reaching
// the panel.
//
// WHAT WAS MEASURED (2026-07-31, `PROSE MEASURE /registry`): three tables carrying FOUR header cells between
// them — Provider + Secret ref on the first, and ONE column each on the other two. A one-column table is a
// list with a border round it: it cannot be sorted, it cannot be scanned down, and the id it does carry was
// a 36-character hash printed whole. Alongside them, 593 characters of paragraph over 773 characters of
// page — more prose than content.
//
// WHAT IT IS NOW: an id cell that is readable AND copyable, the one or two attributes that identify the row,
// and the stamp that says how old it is.
//
// THE SENTENCE THIS FILE USED TO CARRY WAS "NOTHING ON THIS SCREEN WRITES, and that is the API's shape
// rather than a simplification: /v1 mounts no console-reachable create for a model connection". That was
// true when it was written and it is why it is quoted rather than deleted: `POST /v1/model-connections` was
// mounted the whole time (api/router.go), and the claim was made about the CONSOLE's reach and then read as
// a claim about the API's shape. The screen was the missing half, not the route.
//
// THE OWNER'S ASK, IN THEIR WORDS: "bu self-host, burada adam gidip llm providerlar set edebilmesi lazım —
// openai, anthropic vs, custom OpenAI-compatible bir şeyler koyabilecek, sonra bu llm'leri agentlarında
// istediği gibi kullanabilecek, kendi resourcelarını kullanıyor yani." Three shapes, and the third is why
// migration 000051 added a column: a custom endpoint is a property of the CONNECTION, not of the
// deployment's dotenv file.
//
// IT IS A TWO-CALL FLOW AND THE STATUS SAYS WHICH HALF RAN. A connection stores a REF, never a value, so the
// credential is sealed through POST /v1/secret-refs first and the connection then NAMES it. Two calls means
// two ways to fail, and a status that only said "done" would leave an operator unable to tell a rejected
// key name from a rejected endpoint. The credential itself never enters React state — see SecretField.
//
// `created_at` IS ON ALL THREE REAL PROJECTIONS and is rendered from all three (internal/store/
// model_routes.go modelConnectionView/modelRouteView write it unconditionally; knowledge/views.go's
// knowledgeBaseView carries it too). The deterministic fixture carries it on the knowledge base alone, so on
// the fake profile the other two render the em dash Stamp gives an absent time — which is the honest cell
// for a row whose stamp did not arrive, and not a defect in this screen.

/** IdCell is this console's row identity: a readable prefix that links nowhere, and the whole value one
 *  click away. Ours are `<prefix>_` + a hash, and nobody reads the middle of a hash — what a reader needs is
 *  to recognise the row again and to be able to copy the value EXACTLY. There is no detail route for any of
 *  these three collections (no /v1 read of a single route or base is mounted), so the id is not a link
 *  here: a link to a page that cannot exist is worse than a value that can be copied. */
function IdCell({ id, label }: { id: string; label: string }) {
  if (id === "") return <span className="cell-none">— none</span>;
  return (
    <span className="cell-id-group">
      <code className="cell-id" title={id}>
        {shortId(id)}
      </code>
      <CopyButton value={id} label={label} testId={`copy-${id}`} />
    </span>
  );
}

/** created is the column all three share, declared once so the three tables cannot describe it differently. */
const created: Column<Record<string, unknown>> = {
  header: "Created",
  sort: (r) => String(r.created_at ?? ""),
  render: (r) => <Stamp iso={typeof r.created_at === "string" ? r.created_at : null} />,
};

/** ConnectionRow is GET /v1/model-connections' row as modelConnectionView projects it. `base_url` and the
 *  two verification fields are omitted rather than nulled when absent (the view writes them only when set),
 *  so every one of them is optional here — a required field would be a lie the type checker enforced. */
interface ConnectionRow extends Record<string, unknown> {
  id?: string;
  provider?: string;
  secret_ref?: string;
  base_url?: string;
  verified_at?: string;
  verification_outcome?: string;
}

// THE FAMILIES, AND THEY ARE THE WIRE NAMES. `provider-one` / `provider-two` are what every existing
// model_connections row carries and what Broker.Route dispatches on; the label is what a human reads. The
// canonical list lives in packages/model-broker/families.go and the store validates against it, so a value
// this picker offers is a value the broker can dial — that pairing is the whole reason the list is declared
// once in Go rather than three times.
//
// ponytail: the list is duplicated here rather than fetched. There is no /v1 route that serves the family
// catalogue, and inventing one for a three-row constant would be a route to maintain for a value that
// changes when a Go adapter is written. The cost is named: adding a fourth family means editing this array,
// and the create form's own refusal (the store's 400) is what stops a stale console from writing a family
// the broker cannot reach.
const FAMILIES = [
  { value: "provider-one", label: "OpenAI", endpoint: false },
  { value: "provider-two", label: "Anthropic", endpoint: false },
  { value: "openai-compatible", label: "Custom (OpenAI-compatible)", endpoint: true },
] as const;

/** outcomeWords renders a probe verdict as a SENTENCE. Never a bare colour or glyph: the three states an
 *  operator must tell apart are accepted / rejected / not checked, and `not_probed` in particular must read
 *  as "nothing was checked" rather than as a neutral-looking pass. */
function outcomeWords(outcome: string): string {
  switch (outcome) {
    case "credential_accepted":
      return "accepted — the endpoint answered and did not reject this credential";
    case "credential_rejected":
      return "rejected — the key is wrong, expired or revoked";
    case "unreachable":
      return "unreachable — no answer from the endpoint (name resolution, connection, TLS or timeout)";
    case "transient":
      return "inconclusive — a rate limit or a server error, which says nothing about the credential";
    case "not_probed":
      return "not checked — nothing was verified";
    default:
      return outcome;
  }
}

const detail = (err: unknown, fallback: string) => (err instanceof RelayError ? err.problem.detail : fallback);

export default function RegistryPage() {
  const [provider, setProvider] = useState<string>(FAMILIES[0].value);
  const [secretRefName, setSecretRefName] = useState("");
  const [endpoint, setEndpoint] = useState("");
  // THE CREDENTIAL IS A DOM NODE AND NOTHING ELSE. No useState, deliberately — see SecretField's header.
  const secretRef = useRef<HTMLInputElement | null>(null);
  const [createError, setCreateError] = useState("");
  const [createStatus, setCreateStatus] = useState("");
  const [creating, setCreating] = useState(false);
  const [reloadKey, setReloadKey] = useState(0);

  const [verifyStatus, setVerifyStatus] = useState("");
  const [verifyError, setVerifyError] = useState("");
  const [verifying, setVerifying] = useState("");

  const needsEndpoint = FAMILIES.find((f) => f.value === provider)?.endpoint === true;

  // createConnection seals the credential, then names it. The two calls are reported separately because they
  // fail separately: a refused secret write is a name problem, a refused connection is a family or endpoint
  // problem, and one merged "could not create" would send an operator to re-read the wrong field.
  async function createConnection() {
    setCreating(true);
    setCreateError("");
    setCreateStatus("");
    // Read and CLEAR in one call, before any await: the value exists as one local for the duration of one
    // fetch and is copied nowhere — not into state, not into a status message, not into a log.
    const value = takeSecret(secretRef);
    try {
      await apiSend<{ name?: string; version?: number }>("POST", "/secret-refs", { name: secretRefName, value });
    } catch (err: unknown) {
      setCreateError(`the credential could not be sealed: ${detail(err, "the secret ref was refused")}`);
      setCreating(false);
      return;
    }
    try {
      const body = await apiSend<ConnectionRow>("POST", "/model-connections", {
        provider,
        secret_ref: secretRefName,
        // The field is sent only when the family takes one — the store refuses a base_url on a family that
        // has its own endpoint, and sending "" would be sending the field.
        ...(needsEndpoint ? { base_url: endpoint } : {}),
      });
      // The status names the REF and the connection id. Never the value, and never a masked stand-in for it
      // either: `****` next to a name is how a reader learns to expect a reveal control.
      setCreateStatus(
        `Connection ${body.id ?? "?"} now reaches ${String(body.provider ?? provider)} with the credential sealed under ${secretRefName}. ` +
          "The key cannot be read back — from this console or any route.",
      );
      setSecretRefName("");
      setEndpoint("");
      setReloadKey((n) => n + 1);
    } catch (err: unknown) {
      // THE CREDENTIAL SURVIVES THIS FAILURE and the operator is told so. The secret ref was written before
      // the connection was refused, so the bytes are sealed under a name that now binds nothing — saying
      // "creation failed" without that would leave a live credential the operator does not know exists.
      setCreateError(
        `${detail(err, "the connection was refused")} — the credential was already sealed under ${secretRefName}, ` +
          "so fix the field above and submit again with the same ref name (it will be versioned, not duplicated).",
      );
    } finally {
      setCreating(false);
    }
  }

  // verify runs the REAL credential probe (POST /v1/model-connections/{id}/verify). It is a network call
  // from the control plane to the provider, so it is slow enough to need a pending state and honest enough
  // to report a verdict this console must not soften.
  async function verify(id: string) {
    setVerifying(id);
    setVerifyError("");
    setVerifyStatus("Checking…");
    try {
      const body = await apiSend<{ outcome?: string; detail?: string; endpoint?: string; status?: number }>(
        "POST",
        `/model-connections/${encodeURIComponent(id)}/verify`,
      );
      // The SERVER's own sentence, not a paraphrase. The control plane is what measured this and it says
      // what it did and did not check; a console that re-worded it would be free to make it sound better.
      setVerifyStatus(`${shortId(id)}: ${outcomeWords(String(body.outcome ?? ""))}. ${String(body.detail ?? "")}`);
      setReloadKey((n) => n + 1);
    } catch (err: unknown) {
      setVerifyStatus("");
      setVerifyError(detail(err, "the connection could not be verified"));
    } finally {
      setVerifying("");
    }
  }

  return (
    <>
      <Panel<ConnectionRow>
        title="Model connections"
        testId="panel-model-connections"
        fetchPath="/model-connections"
        reloadKey={reloadKey}
        // THE ONE SENTENCE THAT SURVIVES AS A NOTE, because it is the only claim on this screen a reader
        // cannot infer from a cell: the Secret ref column holds a NAME, and no route anywhere reads the value
        // it points at back. The second half is what a verify verdict does NOT establish, said next to the
        // button that produces one — an operator reading "accepted" must not conclude their model id is
        // valid, and a caveat kept only in an API response body is a caveat nobody reads.
        note="A connection binds a provider to a secret REF — a name. The value behind it is written server-side and is readable through no route, this console included. Verify asks the endpoint whether it accepts the credential; it does NOT check that the route's model id exists, nor that a quota remains."
        columns={[
          { header: "ID", sort: (r) => String(r.id ?? ""), render: (r) => <IdCell id={String(r.id ?? "")} label="connection ID" /> },
          { header: "Provider", sort: (r) => String(r.provider ?? ""), render: (r) => <NameCell name={String(r.provider ?? "")} id="" /> },
          {
            // THE REF, NOT THE VALUE, and the column header is where that is said now — it used to be a
            // sentence above the table repeating what the cell already shows.
            header: "Secret ref",
            sort: (r) => String(r.secret_ref ?? ""),
            render: (r) => (String(r.secret_ref ?? "") === "" ? <span className="cell-none">— none</span> : <code>{String(r.secret_ref)}</code>),
          },
          {
            // THE ADDRESS READS BACK AND THE CREDENTIAL DOES NOT, and the asymmetry is the design rather
            // than an inconsistency: an operator must be able to see which of two connections points at
            // their own model server. The write path vetted this URL through packages/egress before a row
            // existed, and userinfo-bearing forms are refused there.
            header: "Endpoint",
            sort: (r) => String(r.base_url ?? ""),
            render: (r) =>
              String(r.base_url ?? "") === "" ? <span className="cell-none">— the family&apos;s own</span> : <code>{String(r.base_url)}</code>,
          },
          {
            header: "Last verified",
            sort: (r) => String(r.verified_at ?? ""),
            render: (r) => {
              const outcome = String(r.verification_outcome ?? "");
              if (outcome === "") return <span className="cell-none">— never</span>;
              return (
                <span>
                  {outcomeWords(outcome)} <Stamp iso={typeof r.verified_at === "string" ? r.verified_at : null} />
                </span>
              );
            },
          },
          created,
          {
            header: "Verify",
            render: (r) => {
              const id = String(r.id ?? "");
              if (id === "") return null;
              return (
                <Button testId="connection-verify-button" onClick={() => void verify(id)} disabled={verifying !== ""}>
                  {verifying === id ? "Checking…" : "Verify"}
                </Button>
              );
            },
          },
        ]}
        emptyNote={
          <>
            <p className="empty-title">No model connections yet</p>
            <p className="empty-body">
              A connection is how this deployment reaches a provider: it binds a provider name to a secret
              REF, never to a value. Until one exists every run falls back to the deployment default — the
              key in the stack&apos;s own <code>.env.local</code>, or the deterministic fake adapter when
              there is none. Use the form below.
            </p>
          </>
        }
      />

      {/* The verdict lives outside the table because it is about ONE action the operator just took, and a
          cell that changed under them would be a verdict they have to go and find. */}
      {verifyError !== "" ? (
        <p role="alert" data-testid="connection-verify-error">
          Error: {verifyError}
        </p>
      ) : null}
      {verifyStatus === "" ? null : (
        <p data-testid="connection-verify-status">
          <span className="glyph" aria-hidden="true">
            ✔
          </span>{" "}
          {verifyStatus}
        </p>
      )}

      <ResourceForm
        title="Add a provider connection"
        testId="connection-create"
        note={
          <>
            <strong>Two things happen when you submit.</strong> The credential is sealed under the ref name
            you give — <code>POST /v1/secret-refs</code>, write-only, versioned if the name already exists —
            and the connection is then created NAMING that ref. The key is never stored on the connection and
            is readable through no route. Bind the connection to a project with a model route to make runs
            use it; until then it is configuration that nothing dispatches to.
          </>
        }
        fields={[
          {
            name: "connection-provider",
            label: "Provider",
            kind: "select",
            required: true,
            value: provider,
            onChange: setProvider,
            options: FAMILIES.map((f) => ({ value: f.value, label: f.label })),
            hint: "OpenAI and Anthropic each have an endpoint of their own. Choose the custom family to name your own.",
            testId: "connection-provider-select",
          },
          {
            name: "connection-secret-ref",
            label: "Secret ref name",
            required: true,
            value: secretRefName,
            onChange: setSecretRefName,
            hint: "What this credential is called. This name — never the key — is what the connection stores and what every screen shows.",
            testId: "connection-secret-ref-input",
          },
          // THE ENDPOINT FIELD EXISTS ONLY FOR THE FAMILY THAT TAKES ONE, and it is REQUIRED by it. Both
          // halves are refusals the store makes, mirrored here so the operator meets them at the field
          // rather than in a 400: a base_url on a family with its own endpoint is refused, and a custom
          // connection with a BLANK endpoint would fall back to api.openai.com — shipping an operator's
          // prompts to OpenAI under a key minted for their private server, with nothing saying so.
          ...(needsEndpoint
            ? [
                {
                  name: "connection-endpoint",
                  label: "Endpoint",
                  required: true,
                  value: endpoint,
                  onChange: setEndpoint,
                  hint: "The full chat-completions URL, e.g. http://127.0.0.1:11434/v1/chat/completions. A private or loopback address is allowed — your own vLLM or Ollama is the point of this family.",
                  testId: "connection-endpoint-input",
                },
              ]
            : []),
        ]}
        submitLabel="Add connection"
        submittingLabel="Adding…"
        submitTestId="connection-create-button"
        submitting={creating}
        error={createError}
        status={createStatus}
        onSubmit={createConnection}
      >
        <SecretField
          inputRef={secretRef}
          id="field-connection-secret"
          label="API key"
          testId="connection-secret-input"
          hint="Paste it — pasting is not blocked, and a 40-character credential retyped by eye is a typo in a value nobody can read back to check. The field clears when you submit."
        />
      </ResourceForm>

      <Panel
        title="Model routes"
        testId="panel-model-routes"
        fetchPath="/model-routes"
        columns={[
          { header: "ID", sort: (r) => String(r.id ?? ""), render: (r) => <IdCell id={String(r.id ?? "")} label="route ID" /> },
          {
            header: "Name",
            sort: (r) => String(r.name ?? ""),
            render: (r) => <NameCell name={String(r.name ?? "")} id="" />,
          },
          created,
        ]}
        emptyNote={
          <>
            <p className="empty-title">No model routes yet</p>
            <p className="empty-body">
              A route is a name a run can ask for instead of a model. With none, runs use the deployment
              default; a per-project override is created through the provisioning API or{" "}
              <code>palai model route</code>, and this screen is where it becomes visible.
            </p>
          </>
        }
      />

      <Panel
        title="Knowledge bases"
        testId="panel-knowledge-bases"
        fetchPath="/knowledge-bases"
        columns={[
          { header: "ID", sort: (r) => String(r.id ?? ""), render: (r) => <IdCell id={String(r.id ?? "")} label="knowledge base ID" /> },
          {
            header: "Name",
            // `name`, NOT `display_name`. The real projection is knowledge/views.go's knowledgeBaseView and it
            // carries `name`; the fixture said `display_name` until E25 T2 and nothing caught it, because a
            // bootstrap stack's collection is empty and the conformance sweep skips an item comparison when
            // either side has no row.
            sort: (r) => String(r.name ?? ""),
            render: (r) => <NameCell name={String(r.name ?? "")} id="" />,
          },
          created,
        ]}
        emptyNote={
          <>
            <p className="empty-title">No knowledge bases yet</p>
            <p className="empty-body">
              A knowledge base is what a retrieval tool searches. Having none is not an error — most
              deployments never create one — and a base is created and indexed outside this console.
            </p>
          </>
        }
      />
    </>
  );
}
