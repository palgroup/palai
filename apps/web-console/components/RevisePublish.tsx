"use client";

import { useCallback, useEffect, useState, type ReactNode } from "react";

import { ResourceForm, type FormField } from "@/components/ResourceForm";
import { apiGet, apiSend, RelayError } from "@/lib/api";

// REVISE AND PUBLISH, ONCE (E25 T6, plan §T6: "the revise-and-publish component is GENERIC and T7 must not
// rewrite it").
//
// This tree has three publishable lineages and they share one shape, which is why this is a component rather
// than a page section: create a DRAFT, read the lineage back with each revision's state, publish one, and
// live with the fact that publishing is IRREVERSIBLE (000019_agents.up.sql sets published_at once and there
// is no route that unsets it). What differs between them is the FIELDS on the draft, the COLUMNS on the
// list, and the three paths — so those are the props, and nothing else is.
//
// WHAT T7 MOUNTS THROUGH IT, named so the next reader does not re-derive the shape: a tool revision and a
// tool-set revision are the same create → list → publish, with `fields` carrying an executor and an
// approval_required toggle and `columns` carrying a digest. It needs no new prop, and if it does, that is a
// prop the caller was going to hand-roll anyway.
//
// THE PUBLISH REFUSAL IS THE SERVER'S OWN SENTENCE, not a category. api/agents.go answers a revision naming
// an environment this organization does not have with a 400 (store/agents.go turns
// automation.ErrEnvironmentNotFound into BadField, deliberately NOT a 404 — the revision id in the path is
// real, so pointing the operator at a missing revision would send them looking for the wrong thing). A
// console that flattened that into "publish failed" would send them looking for the wrong thing too.
//
// ponytail: one draft form, one list, one publish button per draft row. No revision diff (components/
// AgentDiff.tsx already renders one and its ceiling is written there), no bulk publish, no per-field
// validation. Upgrade path: if a lineage ever needs to publish several revisions atomically, that is a
// server-side transaction and a different control.

export interface Revision extends Record<string, unknown> {
  id?: string;
  revision_number?: number;
  status?: string;
}

export interface RevisionColumn {
  header: string;
  render: (revision: Revision) => ReactNode;
}

export function RevisePublish({
  title,
  testId,
  note,
  fields,
  buildBody,
  onCreated,
  createPath,
  listPath,
  publishPath,
  columns,
  emptyNote,
  submitLabel = "Create draft revision",
}: {
  title: string;
  testId: string;
  note?: ReactNode;
  /** The draft's own fields. The caller owns their state; this component owns the form's discipline. */
  fields: FormField[];
  /** The create body, built by the caller because only the caller knows what its fields mean. */
  buildBody: () => Record<string, unknown>;
  /** Called after a successful create with the id the server minted, so a page can clear its inputs. */
  onCreated?: (revision: Revision) => void;
  /** POST target for a draft. "" means there is no parent selected — see `emptyNote`. */
  createPath: string;
  /** GET target for the lineage. "" means the same. */
  listPath: string;
  /** POST target for a publish, from the revision's own id. */
  publishPath: (revisionID: string) => string;
  columns: RevisionColumn[];
  /** What stands in place of the whole surface when there is no parent to revise. */
  emptyNote: ReactNode;
  submitLabel?: string;
}) {
  const [revisions, setRevisions] = useState<Revision[] | null>(null);
  // TRUNCATION IS VISIBLE HERE TOO (§2). This list does not use components/Panel.tsx — it renders its own
  // table because each row carries a publish control — so it would have inherited the exact bug T2 fixed
  // there: api/pagination.go's defaultPageLimit is 20, and a lineage with a twenty-first revision would have
  // looked as though it had twenty. The cut is stated in words. There is no "load more": what an operator
  // publishes or pins is the NEWEST revision and this list is newest-first, so the honest minimum is saying
  // that older ones exist. ponytail: add the continuation if anyone ever needs to publish an old draft.
  const [truncated, setTruncated] = useState(false);
  const [listError, setListError] = useState("");
  const [formError, setFormError] = useState("");
  const [status, setStatus] = useState("");
  // The id the last create or publish touched, carried on the status element so a spec (and an operator
  // reading a lineage of near-identical rows) can tell WHICH revision the sentence is about.
  const [statusID, setStatusID] = useState("");
  const [busy, setBusy] = useState(false);

  const load = useCallback(async (path: string) => {
    if (path === "") {
      setRevisions(null);
      return;
    }
    try {
      const body = await apiGet<{ data?: Revision[]; has_more?: boolean }>(path);
      setRevisions(body.data ?? []);
      setTruncated(body.has_more === true);
      setListError("");
    } catch (err: unknown) {
      setRevisions(null);
      setListError(err instanceof RelayError ? err.problem.detail : "the revisions could not be read");
    }
  }, []);

  useEffect(() => {
    void load(listPath);
  }, [listPath, load]);

  async function create() {
    setBusy(true);
    setFormError("");
    setStatus("");
    try {
      const body = await apiSend<Revision>("POST", createPath, buildBody());
      setStatusID(String(body.id ?? ""));
      setStatus(`Revision ${String(body.id ?? "?")} created as a draft. Publish it before a run can be pinned to it.`);
      onCreated?.(body);
      await load(listPath);
    } catch (err: unknown) {
      setFormError(err instanceof RelayError ? err.problem.detail : "the revision could not be created");
    } finally {
      setBusy(false);
    }
  }

  async function publish(revisionID: string) {
    setFormError("");
    setStatus("");
    try {
      await apiSend<Revision>("POST", publishPath(revisionID));
      setStatusID(revisionID);
      setStatus(`Revision ${revisionID} is published. Publishing cannot be undone — supersede it with a new revision instead.`);
      await load(listPath);
    } catch (err: unknown) {
      // The SERVER's words. A publish is refused for reasons the console cannot tell apart on its own (an
      // environment that no longer exists, a revision outside this project), and each has a different fix.
      setFormError(err instanceof RelayError ? err.problem.detail : "the revision could not be published");
    }
  }

  if (listPath === "" || createPath === "") {
    return (
      <section className="panel" data-testid={`${testId}-none`} aria-labelledby={`${testId}-none-h`}>
        <h2 id={`${testId}-none-h`}>{title}</h2>
        <p className="muted">{emptyNote}</p>
      </section>
    );
  }

  return (
    <>
      <ResourceForm
        title={title}
        testId={testId}
        note={note}
        fields={fields}
        submitLabel={submitLabel}
        submitTestId={`${testId}-create-button`}
        submitting={busy}
        error={formError}
        onSubmit={create}
      />

      {/* The status lives OUTSIDE the form, because a publish is not a form submission and its sentence has
          to survive the form's own state. `data-revision-id` is the id in machine-readable form. */}
      {status === "" ? null : (
        <p data-testid={`${testId}-status`} data-revision-id={statusID}>
          <span className="glyph" aria-hidden="true">
            ✔
          </span>{" "}
          {status}
        </p>
      )}

      <section className="panel" data-testid={`panel-${testId}s`} aria-labelledby={`${testId}s-h`}>
        <h2 id={`${testId}s-h`}>Revisions</h2>
        <p className="muted">
          Newest first. <strong>A draft can be changed only by superseding it</strong> — there is no PATCH on
          this surface — and <strong>publishing is permanent</strong>: a published revision can be superseded
          but never un-published, which is what makes a run pinned to one reproducible.
        </p>
        {listError !== "" ? (
          <p role="alert" data-testid={`panel-${testId}s-error`}>
            Error: {listError}
          </p>
        ) : revisions === null ? (
          <p data-testid={`panel-${testId}s-loading`}>Loading…</p>
        ) : revisions.length === 0 ? (
          <p data-testid={`panel-${testId}s-empty`}>No revisions yet.</p>
        ) : (
          <table>
            <thead>
              <tr>
                <th scope="col">Revision</th>
                <th scope="col">State</th>
                {columns.map((c) => (
                  <th key={c.header} scope="col">
                    {c.header}
                  </th>
                ))}
                <th scope="col">Publish</th>
              </tr>
            </thead>
            <tbody>
              {revisions.map((rev, i) => {
                const id = String(rev.id ?? i);
                const published = rev.status === "published";
                return (
                  <tr key={id}>
                    <td>
                      {id}
                      {/* A TEMPLATE STRING RATHER THAN JSX TEXT, and the reason is a real finding rather than
                          taste: this read `(#{String(...)})` as JSX text, and a bare `#` before `{` makes the
                          raw TypeScript scanner emit a zero-width PrivateIdentifier forever — the token audit
                          in tests/approval-queue.spec.ts caught it as "never reached EndOfFile", which is
                          exactly the swallowed-region desync that guard exists to refuse. */}
                      {rev.revision_number === undefined ? null : ` (#${String(rev.revision_number)})`}
                    </td>
                    {/* STATE IN TEXT, never a colour: draft and published are the difference between config
                        that can still change and config that cannot. */}
                    <td data-testid={`revision-state-${id}`}>{published ? "published" : "draft"}</td>
                    {columns.map((c) => (
                      <td key={c.header}>{c.render(rev)}</td>
                    ))}
                    <td>
                      {published ? (
                        <span>— already published</span>
                      ) : (
                        <button type="button" data-testid={`publish-${id}`} onClick={() => void publish(id)}>
                          Publish {id}
                        </button>
                      )}
                    </td>
                  </tr>
                );
              })}
            </tbody>
          </table>
        )}
        {truncated ? (
          <p data-testid={`panel-${testId}s-more`}>
            Showing the {revisions?.length ?? 0} newest revisions — <strong>older ones exist and are not
            listed</strong>. Publishing and pinning act on recent revisions, so this list does not continue;
            read the rest through the API.
          </p>
        ) : null}
      </section>
    </>
  );
}
