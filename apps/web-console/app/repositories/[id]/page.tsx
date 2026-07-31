"use client";

import { useParams } from "next/navigation";
import { useEffect, useState } from "react";

import { CopyButton, shortId, Stamp } from "@/components/Session";
import { apiGet, RelayError } from "@/lib/api";
import { operationsOf, type BindingRow } from "@/lib/repositories";

// ONE REPOSITORY BINDING — the whole record, including the four fields a list cannot carry.
//
// WHY THIS ROUTE EXISTS. The list shows what tells two bindings apart (id, repository, provider, branch,
// connection, created); the record also holds a CLONE URL, a data classification, a region constraint and a
// pass-through `policy` object, and putting ten columns on a table is how a table stops being read. Those
// four were on no screen at all before this page — the console wrote them and never showed them back, which
// is the write-and-pray shape §2 forbids.
//
// AND IT IS WHERE "YOU CANNOT CHANGE THIS" BELONGS. That sentence was the second paragraph of the list
// screen, read by every operator who came to look at the list; the moment it is needed is when somebody
// opens one binding looking for an edit control. api/router.go:44-46 mounts a create and two reads — no
// PATCH, no DELETE — so the honest thing this page can do is say so where the control would have been.
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
              {typeof binding?.connection_ref === "string" && binding.connection_ref !== "" ? (
                <code>{binding.connection_ref}</code>
              ) : (
                <span className="cell-none">— none (a public repository)</span>
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
            <strong>There is no way to change or remove a binding.</strong> The API mounts a create and two
            reads and no PATCH or DELETE, so this console creates and reads; it does not correct. A binding
            registered wrongly is superseded by registering another and pointing runs at that one.
          </p>
        </section>
      )}
    </>
  );
}
