"use client";

import { useCallback, useEffect, useState, type ReactNode } from "react";

import { Button } from "@/components/ui/Button";
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
// T7 NEEDED THREE, AND EACH ONE IS A PROP THE CALLER WOULD HAVE HAND-ROLLED — which is the sentence above
// being cashed rather than contradicted. They are generalisations of this component, not additions to it:
//
//   1. `publishPath` TAKES THE REVISION, not its id. A tool-set revision's publish route names the SET
//      (/v1/tool-sets/{set}/revisions/{id}/publish) and the list shows every set in the project, so the
//      path cannot be closed over — it has to be read off the row. (Measured while writing this: the
//      publish HANDLER ignores the {set} segment and keys on the revision id alone, so closing over one
//      set's name would have WORKED and quietly published another set's draft under the wrong path. A
//      caller that happens to work is worse than one that reads correctly.)
//   2. `publishBody` — an OPTIONAL body on the publish. E23 T5 made publishing a tool revision carry the
//      operator's approval declaration, and a publish with no body is the shipped bodyless publish, so this
//      is `undefined` for both agent lineages and unchanged for them.
//   3. `createPath === ""` NOW MEANS "no create form here", with the list and its publish controls still
//      rendered. A discovered MCP tool revision is created by DISCOVERY, not by a form: offering an
//      operator a hand-written revision box on the approval screen would be offering the one thing that
//      screen is not for. `listPath === ""` still means "no parent selected" and renders `emptyNote`.
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
  fields = [],
  buildBody,
  onCreated,
  onChanged,
  createPath,
  listPath,
  publishPath,
  publishBody,
  columns,
  emptyNote,
  listNote,
  submitLabel = "Create draft revision",
}: {
  title: string;
  testId: string;
  note?: ReactNode;
  /** The draft's own fields. The caller owns their state; this component owns the form's discipline. */
  fields?: FormField[];
  /** The create body, built by the caller because only the caller knows what its fields mean. */
  buildBody?: () => Record<string, unknown>;
  /** Called after a successful create with the id the server minted, so a page can clear its inputs. */
  onCreated?: (revision: Revision) => void;
  /**
   * Called after ANY change this component landed — a create OR a publish — once its own list has been
   * reloaded. It exists because a page that renders a SUMMARY of this lineage beside it (app/agents/[id]
   * reads the same collection for its chip row) has no other way to learn that a publish happened.
   *
   * IT IS NOT `onCreated` WITH A WIDER NAME, and the difference is a bug this component shipped with. A
   * publish reloaded the table here and told nobody, so the summary above it kept saying "draft only" over a
   * table whose top row read "published" — one screen contradicting itself, which is the failure a summary
   * always invites and the reason a summary needs a signal rather than a second guess. Found by the leg that
   * asserts the two agree (tests/config-journey.spec.ts), on the first run after the summary existed.
   */
  onChanged?: () => void;
  /** POST target for a draft. "" means THIS LINEAGE IS NOT CREATED FROM HERE — no form is rendered. */
  createPath: string;
  /** GET target for the lineage. "" means there is no parent selected — see `emptyNote`. */
  listPath: string;
  /** POST target for a publish, read off the ROW (a set revision's path names its set). */
  publishPath: (revision: Revision) => string;
  /** An OPTIONAL publish body, per row. `undefined` is the shipped bodyless publish. */
  publishBody?: (revision: Revision) => Record<string, unknown> | undefined;
  columns: RevisionColumn[];
  /** Extra prose above the revisions table, for what is true of THIS lineage and not of every one. */
  listNote?: ReactNode;
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
    // CLEARED FIRST, and this is a correctness fix rather than a flicker (E25 T7). `load` replaces the rows
    // when the fetch returns, so switching to another lineage left the PREVIOUS one's revisions on screen
    // under the new one's heading until the response landed — and a publish control rendered against the
    // wrong lineage is a click that publishes something the operator was not looking at. It was found by a
    // spec that switched tools and read the row it was already showing. "Loading…" is the honest state.
    setRevisions(null);
    void load(listPath);
  }, [listPath, load]);

  async function create() {
    setBusy(true);
    setFormError("");
    setStatus("");
    try {
      const body = await apiSend<Revision>("POST", createPath, buildBody?.() ?? {});
      setStatusID(String(body.id ?? ""));
      setStatus(`Revision ${String(body.id ?? "?")} created as a draft. Publish it before a run can be pinned to it.`);
      onCreated?.(body);
      await load(listPath);
      onChanged?.();
    } catch (err: unknown) {
      setFormError(err instanceof RelayError ? err.problem.detail : "the revision could not be created");
    } finally {
      setBusy(false);
    }
  }

  async function publish(revision: Revision) {
    const revisionID = String(revision.id ?? "");
    setFormError("");
    setStatus("");
    try {
      await apiSend<Revision>("POST", publishPath(revision), publishBody?.(revision));
      setStatusID(revisionID);
      setStatus(`Revision ${revisionID} is published. Publishing cannot be undone — supersede it with a new revision instead.`);
      await load(listPath);
      onChanged?.();
    } catch (err: unknown) {
      // The SERVER's words. A publish is refused for reasons the console cannot tell apart on its own (an
      // environment that no longer exists, a revision outside this project), and each has a different fix.
      setFormError(err instanceof RelayError ? err.problem.detail : "the revision could not be published");
    }
  }

  if (listPath === "") {
    return (
      <section className="panel" data-testid={`${testId}-none`} aria-labelledby={`${testId}-none-h`}>
        <h2 id={`${testId}-none-h`}>{title}</h2>
        <p className="muted">{emptyNote}</p>
      </section>
    );
  }

  return (
    <>
      {createPath === "" ? null : (
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
      )}

      {/* A REFUSAL NEEDS SOMEWHERE TO LAND WHEN THERE IS NO FORM. `formError` is where a failed PUBLISH goes,
          and with `createPath === ""` the ResourceForm that used to render it is not on the page — so a
          server's 400 about a draft pin or a too-long approval label would have been swallowed entirely.
          Same role="alert" region, same words, rendered here instead. */}
      {createPath !== "" || formError === "" ? null : (
        <p role="alert" className="form-error" data-testid={`${testId}-error`}>
          <span className="glyph" aria-hidden="true">
            ✖
          </span>{" "}
          {formError}
        </p>
      )}

      {/* The status lives OUTSIDE the form, because a publish is not a form submission and its sentence has
          to survive the form's own state. `data-revision-id` is the id in machine-readable form. */}
      {status === "" ? null : (
        <p className="form-status" data-testid={`${testId}-status`} data-revision-id={statusID}>
          <span className="glyph" aria-hidden="true">
            ✔
          </span>{" "}
          {status}
        </p>
      )}

      {/* THE NAME IS WITHHELD UNTIL THE ROWS SETTLE, which is components/Panel.tsx's rule applied to the one
          panel in this console that was not following it (found by page-parity).

          This section emitted `panel-<testId>s` from its FIRST PAINT with "Loading…" inside it, so anything
          waiting on the panel got a truthy answer about a region holding a spinner. That is the defect
          tests/reveal-once.spec.ts failed on this morning, and the one the popup and dialog scans in
          tests/a11y.spec.ts exist to stop: a container that answers to its name before it has content makes
          every downstream measurement look clean while looking at nothing. An axe scan arriving here would
          have reported zero violations for a table it never saw.

          ONE SECTION, NOT TWO. Panel.tsx returns a separate pending section because it returns EARLY; doing
          that here would put a second `id="<testId>s-h"` in the document for aria-labelledby to point at,
          which trades this defect for an ambiguous accessible name. Swapping the testid on the one section
          buys the same property — the wait cannot be satisfied early — and moves nothing on screen, which is
          the other half of Panel's rule. `aria-busy` is the machine-readable half of the word. */}
      <section
        className="panel"
        data-testid={listError === "" && revisions === null ? `panel-${testId}s-loading` : `panel-${testId}s`}
        aria-labelledby={`${testId}s-h`}
        aria-busy={listError === "" && revisions === null ? "true" : undefined}
      >
        <h2 id={`${testId}s-h`}>Revisions</h2>
        {/* ONE SENTENCE (page-parity pass). This was three: newest-first, no PATCH, and publishing being
            permanent — all true, all standing background, and all repeated by the create form's own note two
            panels up. What an operator needs at the moment they look at this table is the ORDER, because it
            decides which row is current; the irreversibility is stated on the control that causes it (the
            publish button's own status sentence says "Publishing cannot be undone") and the rest is in
            docs/operations/console.md §4c. */}
        <p className="muted">Newest first — the top row is what a new pin resolves to.</p>
        {listNote ? <p className="muted">{listNote}</p> : null}
        {listError !== "" ? (
          <p role="alert" className="form-error" data-testid={`panel-${testId}s-error`}>
            Error: {listError}
          </p>
        ) : revisions === null ? (
          <p className="loading">Loading…</p>
        ) : revisions.length === 0 ? (
          <p className="empty" data-testid={`panel-${testId}s-empty`}>No revisions yet.</p>
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
                        <Button testId={`publish-${id}`} onClick={() => void publish(rev)}>
                          Publish {id}
                        </Button>
                      )}
                    </td>
                  </tr>
                );
              })}
            </tbody>
          </table>
        )}
        {truncated ? (
          <p className="table-more" data-testid={`panel-${testId}s-more`}>
            Showing the {revisions?.length ?? 0} newest revisions — <strong>older ones exist and are not
            listed</strong>. Publishing and pinning act on recent revisions, so this list does not continue;
            read the rest through the API.
          </p>
        ) : null}
      </section>
    </>
  );
}
