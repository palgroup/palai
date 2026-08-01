"use client";

import { DeploymentNotice } from "@/components/DeploymentNotice";
import { NameCell, Panel } from "@/components/Panel";
import { mutabilityLabel, type DeploymentSetting } from "@/lib/deployment";

// WHAT THIS MACHINE IS RUNNING WITH.
//
// Measured on main at cf0efd63 (2026-08-01):
//
//   grep -oE 'PALAI_[A-Z_]+' deploy/compose/compose.yaml | sort -u   -> 24 settings
//   of those, readable from any /v1 route                            -> 0
//
// Every knob that decides what this deployment DOES lived in container environment and no screen could
// show one of them. This is that screen, and it is a READ surface — see MUTABILITY below for why there is
// no edit control on it, which is a finding rather than a limitation.
//
// IT IS NOT /policy AND THE DISTINCTION IS THE POINT. /policy writes a PROJECT's config_policy document
// (a JSONB column on the projects table, storage/migrations/000005_config_revisions.up.sql:16), behind a
// project picker, replacing the whole document on save. Nothing on this page is a property of a project:
// a dispatch worker count is a property of the process every project on this deployment shares. Putting a
// machine-wide switch behind a project picker would make it look per-project to the one reader who most
// needs to know it is not.
//
// THE ORDER IS THE API'S AND IS NOT SORTED HERE. api/deployment.go's catalogue puts the two settings that
// silently change what the product does first, because alphabetical order is where PALAI_DISPATCH_WORKERS
// was already hiding — between the CA paths and the engine image. The column offers a sort, so a reader who
// wants the dump can have it; what they get on arrival is the order somebody decided.
export default function DeploymentPage() {
  return (
    <>
      {/* EVERY warning, above the table. A row in a table is not a warning: the value that cost an evening
          was visible in `docker inspect` all along, and being visible is what it had already failed at. */}
      <DeploymentNotice path="/deployment" />

      <Panel<DeploymentSetting>
        title="Effective configuration"
        testId="panel-deployment-settings"
        fetchPath="/deployment"
        selectRows={(body) => (Array.isArray(body.settings) ? (body.settings as DeploymentSetting[]) : [])}
        // ONE SENTENCE, AND IT IS THE ONE THE TABLE CANNOT CARRY: the scope of the answer. Every row is the
        // CONTROL-PLANE process's own environment. PALAI_RUNNER_CONCURRENCY is absent for that reason and
        // not by oversight — it is set on the runner container, and cmd/cli/internal/stack/upgrade.go
        // records what happens to a reader that forgets: "reading a runner-scoped var off the
        // control-plane container always misses it".
        note="These are the control-plane process's own settings. A runner-scoped setting (PALAI_RUNNER_CONCURRENCY) is not shown here because this process holds no copy of it — a confident wrong answer would be worse than the absence. What this deployment can observe about a machine's capacity is on Fleet."
        // ONLY THE GROUP RIDES `.id`, AND THAT IS A MEASUREMENT RATHER THAN A LAYOUT PREFERENCE.
        //
        // The first draft put `change_with` and the reader citation in `.id`, which is the console's
        // IDENTIFIER slot: `white-space: nowrap`, `max-width: 24rem`, `text-overflow: ellipsis`
        // (app/globals.css:944). Two consequences, and the second is the one axe caught. A remedy sentence
        // rendered there is TRUNCATED — the operator sees "recreate the control-plane with the new…" and
        // learns nothing, which is the `docker inspect` evening with an ellipsis. And three nowrap cells at
        // 24rem give the table a 72rem floor, so `.panel`'s `overflow-x: auto` turned the section into a
        // SCROLLABLE REGION with no focusable content: axe-core `scrollable-region-focusable`, serious,
        // wcag2a SC 2.1.1 — content a keyboard user cannot reach. The stylesheet already states the rule
        // this broke, on `pre` (globals.css:525): a block WRAPS rather than scrolls.
        //
        // So prose wraps as prose, and the two machine strings that have no spaces to break at are `<code>`,
        // which carries `overflow-wrap: anywhere` (globals.css:522). No stylesheet change: the rules needed
        // were already there and the markup was asking for the wrong ones.
        columns={[
          {
            header: "Setting",
            sort: (row) => row.name,
            render: (row) => <NameCell name={row.name} id={row.group} />,
          },
          {
            header: "Value",
            sort: (row) => row.value,
            render: (row) => <ValueCell row={row} />,
          },
          {
            header: "Changing it",
            sort: (row) => row.mutability,
            render: (row) => (
              <span className="cell-name">
                <span className="name">{mutabilityLabel(row.mutability)}</span>
                <span className="muted">{row.change_with}</span>
              </span>
            ),
          },
          {
            header: "What it does",
            render: (row) => (
              <span className="cell-name">
                <span className="muted">{row.effect}</span>
                {/* THE CITATION. An operator reading "this cannot be changed live" is entitled to ask who
                    says so; this is the answer, and it is checked rather than typed — the Go test
                    TestEveryCatalogueCitationResolvesToARealReader parses the named file and asserts the
                    named function's source actually mentions the variable. */}
                <span className="muted">
                  read by <code>{row.reader_func}</code> in <code>{row.reader_file}</code>
                </span>
              </span>
            ),
          },
        ]}
        filterLabel="Filter settings by name, group or value"
        filterPlaceholder="Setting, group or value…"
        matchOn={(row) => `${row.name} ${row.group} ${row.value} ${row.effect}`}
        emptyNote={
          <>
            <p className="empty-title">This deployment reported no configuration</p>
            <p className="empty-body">
              GET /v1/deployment answered with an empty catalogue. Every setting on this screen is declared
              in the control plane's own source, so an empty answer is a statement about the binary that is
              running rather than about this deployment&apos;s configuration — a stack with nothing set would
              still report every setting, as unset with its default beside it.
            </p>
          </>
        }
      />
    </>
  );
}

// ValueCell renders what the process holds, or — when the variable is unset — what it uses instead.
//
// "UNSET" ALONE IS THE WRONG ANSWER AND IT IS THE ONE A NAIVE TABLE GIVES. Unset PALAI_DISPATCH_WORKERS runs
// ONE worker; unset PALAI_S3_ENDPOINT means there is no object store at all and half a dozen subsystems are
// off. Those are opposite facts wearing the same word, so the fallback is rendered rather than left to a
// reader who would have to know the code to tell them apart.
function ValueCell({ row }: { row: DeploymentSetting }) {
  if (!row.set) {
    return (
      <span className="cell-name">
        <span className="name" data-unnamed="true">
          — unset
        </span>
        <span className="muted">uses: {row.default}</span>
      </span>
    );
  }
  return (
    <span className="cell-name">
      <code>{row.value}</code>
      {/* A path is a HANDLE and this cell says which cells are handles, because that is the whole of the
          credential rule here: the master key's PATH is what makes the secret store's posture visible, and
          a path is not a key. GET /v1/deployment returns no credential value at all — the catalogue is an
          allow-list, so a variable nobody has decided about is invisible rather than published. */}
      {row.kind === "path" ? <span className="muted">a filesystem path — never the contents</span> : null}
    </span>
  );
}
