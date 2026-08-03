"use client";

import { useState } from "react";

import { DeploymentNotice } from "@/components/DeploymentNotice";
import { NameCell, Panel } from "@/components/Panel";
import { mutabilityLabel, type DeploymentSetting } from "@/lib/deployment";

import { DesiredConfig } from "./DesiredConfig";

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
// numberRemedies assigns each DISTINCT `change_with` a 1-based number, so the column can point at a
// sentence printed ONCE below the table instead of repeating it on every row that carries it.
//
// WHAT THIS REPLACED AND WHY, because the old design was right for a catalogue that no longer exists. It
// suppressed the single STRICT-MAJORITY sentence — correct when 29 of 35 rows shared one and repeating it
// buried the six that said something else, one of which says a stored secret rotates LIVE. Measured
// 2026-08-03 the catalogue is 20 / 12 / 2 and 6 rows with no remedy, across FOUR groups: no majority, so
// the flag suppressed nothing and all twenty identical sentences printed. The premise ended; the intent —
// say each sentence once, keep the exceptions visible — is what this preserves for any number of groups.
//
// THE ORDER IS TOTAL AND COMPUTED OVER ALL ROWS. Descending count first so the commonest remedy is [1],
// then the sentence itself to break a tie: two groups of equal size must not swap numbers between renders.
// And it runs in `selectRows`, which is the only place with the WHOLE set in hand — numbering the FILTERED
// rows would renumber the list as an operator types, and a row's [2] would mean a different sentence than
// the one it meant a keystroke ago.
export function numberRemedies(rows: DeploymentSetting[]): DeploymentSetting[] {
  const counts = new Map<string, number>();
  for (const row of rows) {
    if (row.change_with !== "") counts.set(row.change_with, (counts.get(row.change_with) ?? 0) + 1);
  }
  const ordered = [...counts.entries()]
    .sort((a, b) => (b[1] - a[1] !== 0 ? b[1] - a[1] : a[0].localeCompare(b[0])))
    .map(([sentence]) => sentence);
  return rows.map((row) => ({
    ...row,
    change_with_index: ordered.indexOf(row.change_with) + 1,
    change_with_count: counts.get(row.change_with) ?? 0,
  }));
}

// remedyList is the footnote's content: every distinct remedy, in numbering order, stated ONCE.
//
// It is derived from the rows the panel actually loaded rather than written here, which is the whole
// property this screen now asserts — a sentence typed into this file would be a second copy able to drift
// from the catalogue, and the Go guard TestTheDeploymentPageHardcodesNoRemedySentence forbids exactly that.
export function remedyList(rows: DeploymentSetting[]): { index: number; sentence: string; count: number }[] {
  const byIndex = new Map<number, { index: number; sentence: string; count: number }>();
  for (const row of rows) {
    const index = row.change_with_index ?? 0;
    if (index === 0 || (row.change_with_count ?? 0) < 2) continue;
    const seen = byIndex.get(index);
    if (seen === undefined) byIndex.set(index, { index, sentence: row.change_with, count: 1 });
    else seen.count += 1;
  }
  return [...byIndex.values()].sort((a, b) => a.index - b.index);
}

export default function DeploymentPage() {
  // ONE COUNTER, TWO READERS. Saving a desired value changes what BOTH the panel above and the table below
  // report — the table's `desired`/`drift` columns come from the same GET /v1/deployment the panel reads —
  // so a save that refreshed only the form would leave the table claiming the old document until a reload.
  // That is the shape of a console disagreeing with itself about the machine it is showing.
  const [reloadKey, setReloadKey] = useState(0);
  // THE ROWS, SO THE FOOTNOTE CAN BE DERIVED FROM THEM. Panel's `note` is a static prop that renders before
  // any row is fetched; `footnote` is the slot documented as row-derived, and `onRows` is how a page gets
  // the set to derive it from. That pairing is why the remedy sentences are never typed into this file.
  const [rows, setRows] = useState<DeploymentSetting[]>([]);
  const remedies = remedyList(rows);

  return (
    <>
      {/* EVERY warning, above the table. A row in a table is not a warning: the value that cost an evening
          was visible in `docker inspect` all along, and being visible is what it had already failed at. */}
      <DeploymentNotice path="/deployment" reloadKey={reloadKey} />

      {/* DESIRED BEFORE EFFECTIVE, and the order is the answer to "why is this machine running this?".
          The table below says what IS; this says what was ASKED FOR and whether the two agree. A reader who
          meets the table first has no way to know a save is pending. */}
      <DesiredConfig reloadKey={reloadKey} onSaved={() => setReloadKey((n) => n + 1)} />

      <Panel<DeploymentSetting>
        reloadKey={reloadKey}
        title="Effective configuration"
        testId="panel-deployment-settings"
        fetchPath="/deployment"
        selectRows={(body) => numberRemedies(Array.isArray(body.settings) ? (body.settings as DeploymentSetting[]) : [])}
        onRows={setRows}
        // ONE SENTENCE, AND IT IS THE ONE THE TABLE CANNOT CARRY: the scope of the answer. Every row is the
        // CONTROL-PLANE process's own environment. PALAI_RUNNER_CONCURRENCY is absent for that reason and
        // not by oversight — it is set on the runner container, and cmd/cli/internal/stack/upgrade.go
        // records what happens to a reader that forgets: "reading a runner-scoped var off the
        // control-plane container always misses it".
        note={
          <>
            These are the control-plane process&apos;s own settings. A runner-scoped setting (PALAI_RUNNER_CONCURRENCY) is
            not shown here because this process holds no copy of it — a confident wrong answer would be worse than the
            absence. What this deployment can observe about a machine&apos;s capacity is on Fleet. Changing anything
            marked <span className="name">Needs a bring-up</span> means replacing the process with the new value. HOW
            differs by row and the ways are numbered in the Changing it column; each one is spelled out once beneath
            the table.
          </>
        }
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
            // DESIRED AGAINST EFFECTIVE, IN THE SAME ROW. The whole cost of the configuration being
            // invisible was that the operator had to go somewhere else to find it; a desired value on a
            // different screen from the effective one reproduces that with two screens instead of a
            // `docker inspect`. Drift is stated IN TEXT ("pending a bring-up"), never by colour alone —
            // the rule Status.tsx and Panel.tsx already follow.
            header: "Saved for this machine",
            sort: (row) => (row.desired_set === true ? row.desired ?? "" : ""),
            render: (row) => <DesiredCell row={row} />,
          },
          {
            header: "Changing it",
            sort: (row) => row.mutability,
            // THE COLUMN POINTS AT A REMEDY; IT DOES NOT REPEAT ONE. Measured against the running catalogue
            // rather than assumed — the command, so the next reader re-measures instead of trusting this:
            //
            //   curl -s .../v1/deployment | jq -r '.settings[].change_with' | sort | uniq -c | sort -rn
            //     2026-08-03 -> 20 / 12 / 2, and 6 rows with no remedy at all. FOUR groups, no majority.
            //
            // The sentences themselves are deliberately NOT quoted here. This file may not contain one: the
            // Go guard TestTheDeploymentPageHardcodesNoRemedySentence forbids it, because a copy in this file
            // is a copy that can drift from the catalogue — and the comment this replaced proved the point by
            // doing exactly that. It recorded "29 identical sentences of 35" from a catalogue that has since
            // become 20 of 40, and it went on describing a suppression rule the code no longer had.
            //
            // WHAT THE OLD RULE WAS FOR IS PRESERVED. Repeating one sentence twenty-nine times buried the six
            // rows that said something ELSE, and those six are the valuable ones — PALAI_SECRET_MASTER_KEY_FILE's
            // says a stored secret rotates LIVE and only the master key's LOCATION needs a bring-up, which is
            // the opposite of what a reader skimming a wall of identical text takes away. Suppressing only the
            // strict majority achieved that while a majority existed. Numbering every distinct remedy achieves
            // it for any number of groups, and gets stronger as the catalogue grows instead of harder to hold.
            //
            // The numbering is computed in `selectRows` above, over ALL rows rather than the filtered set, so
            // a filter cannot renumber the list as an operator types. It is deliberately not done by giving
            // Panel's `render` a second argument: a column that needs its siblings is a property of THIS
            // screen and does not belong in the shared table.
            render: (row) => (
              <span className="cell-name">
                <span className="name">{mutabilityLabel(row.mutability)}</span>
                {(row.change_with_index ?? 0) === 0 ? null : (row.change_with_count ?? 0) === 1 ? (
                  <span className="muted">{row.change_with}</span>
                ) : (
                  <span className="muted">Way {row.change_with_index}, below</span>
                )}
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
        // SPANS, NOT AN <ol>, AND THAT IS THE CONTAINER'S RULE RATHER THAN A STYLE PREFERENCE: Panel renders
        // `footnote` inside a <p> (components/Panel.tsx:522), so a list or a nested <p> is invalid nesting the
        // browser silently repairs into broken DOM. A <span> with `display: block` is valid there and reads
        // the same.
        //
        // ONLY THE SHARED ONES. A remedy carried by a SINGLE row is stated on that row, because a group of one
        // IS its row — and those are the rows worth reading. Sending them to a footnote would satisfy "once"
        // while doing the very thing the original suppression existed to prevent: burying the exception.
        //
        // EACH REMEDY, ONCE. Derived from the loaded rows (never typed here, so it cannot drift from the
        // catalogue), numbered to match the Changing it column, and rendered as an ordered LIST rather than a
        // paragraph — a reader looking up "Way 3" is scanning, and a list is the element that supports it.
        // The count is stated because "12 settings" is what tells an operator whether the way they are
        // reading is the common one or the exception, which is what the old majority notice conveyed by
        // being printed at all.
        footnote={
          remedies.length === 0 ? undefined : (
            <>
              <span className="remedy-way">Ways to change a setting on this deployment:</span>
              {remedies.map((remedy) => (
                <span className="remedy-way" key={remedy.index}>
                  <span className="name">Way {remedy.index}</span>{" "}
                  <span className="muted">
                    ({remedy.count} {remedy.count === 1 ? "setting" : "settings"})
                  </span>{" "}
                  {remedy.sentence}
                </span>
              ))}
            </>
          )
        }
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
  // A SETTING THIS PROCESS DOES NOT READ IS NOT "UNSET", and rendering it that way would be the sharpest
  // possible version of this screen's own founding mistake: two opposite facts wearing one word. Unset
  // PALAI_RUNNER_POOL on a runner means the machine took the default pool; "unset" HERE would mean the
  // control plane holds no copy of a variable it never reads — a statement about the wrong computer.
  if (row.observable === false) {
    return (
      <span className="cell-name">
        <span className="name" data-unnamed="true">
          — read on the machines
        </span>
        <span className="muted">
          not this process&apos;s to report. A runner that was given nothing uses: {row.default}
        </span>
      </span>
    );
  }
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

// DesiredCell renders what was SAVED for this machine against what the process holds.
//
// FOUR STATES AND EACH ONE IS A DIFFERENT SENTENCE, because collapsing any two of them is how a screen
// starts lying about a machine:
//
//   drift      — saved, and the process is not running it. A bring-up is pending, and the cell says the
//                word "pending" rather than relying on a colour a screen reader does not read.
//   agreed     — saved, and the process is running it. This is the state an operator wants to confirm.
//   writable   — nothing saved, but something COULD be. The cell says so, so the empty space reads as an
//                offer rather than as an absence of capability.
//   refused    — the catalogue will not take a value for this setting, and the REASON is the cell. This is
//                twenty-four of the thirty-five rows, and printing the reason here is what stops an
//                operator concluding the console is unfinished.
function DesiredCell({ row }: { row: DeploymentSetting }) {
  if (row.desired_set === true) {
    return (
      <span className="cell-name">
        <code>{row.desired}</code>
        {row.drift === true ? (
          <span className="name">pending a bring-up — this process holds {row.set ? `“${row.value}”` : "no value"}</span>
        ) : (
          <span className="muted">this process is running it</span>
        )}
      </span>
    );
  }
  if (row.writable === true) {
    return (
      <span className="cell-name">
        <span className="name" data-unnamed="true">
          — not saved
        </span>
        <span className="muted">can be saved here</span>
      </span>
    );
  }
  return (
    <span className="cell-name">
      <span className="name" data-unnamed="true">
        — not settable here
      </span>
      <span className="muted">{row.not_writable_because}</span>
    </span>
  );
}
