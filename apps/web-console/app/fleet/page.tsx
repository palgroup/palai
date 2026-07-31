"use client";

import { useEffect, useState } from "react";

import { ConfirmDestructive } from "@/components/ConfirmDestructive";
import { NameCell, Panel } from "@/components/Panel";
import { Picker, type PickerOption } from "@/components/Picker";
import { ResourceForm } from "@/components/ResourceForm";
import { RevealOnce } from "@/components/RevealOnce";
import { CopyButton, shortId, Stamp } from "@/components/Session";
import { Status } from "@/components/Status";
import { apiGet, apiSend, RelayError } from "@/lib/api";
import { useQueryParam } from "@/lib/urlState";

// THE FLEET SCREEN — POOLS, KEYS, MACHINES, THE WAITING ROOM, AND THE THREE THINGS IT REFUSES TO IMPLY.
//
// WHAT THIS PASS CHANGED, MEASURED FIRST (2026-07-31, `PROSE MEASURE /fleet`): 6646 characters of page, of
// which 4635 sat in EIGHTEEN paragraphs over sixty characters. It was the longest screen in the console and
// most of it was prose. The three ceilings that changed what an operator CONCLUDES stayed exactly as they
// were — FLT-P15 open at the top, FLT-P4/P12/P13 one disclosure down, the concurrency notice on the machine
// count — and everything that merely described a column moved into the column, into an empty state, or into
// docs/operations/console.md §3c which already carried the long form.
//
// FOUR TABLES INSTEAD OF TWO. The waiting room was a `<ul>` of sentences with a button glued to each; it is
// a table with the same columns as the machine list above it, because it holds the same kind of row and an
// operator comparing "what is in" against "what is asking to get in" was comparing a table to a paragraph.
//
// THE SELECTED POOL IS IN THE ADDRESS BAR (`/fleet?pool=pool_mac`) — the pattern app/sessions/[id]/page.tsx
// landed. Both the key table and the mint act on it, and until now the choice existed only in React state:
// unlinkable, lost on reload, and invisible to the back button.
//
// THREE THINGS THIS PAGE WILL NOT INVENT.
//
//   1. NO `healthy`, NO up/down, NO derived liveness. api/runners.go:26-29, verbatim: "last_seen_at records
//      the last time the machine AUTHENTICATED (enrol, connect, renew). Nothing polls and nothing expires a
//      row, so a stale stamp means 'has not authenticated since', NOT 'is down'. The item deliberately
//      carries no `healthy` field, because there is nothing behind one." A dot rendered from that stamp is a
//      status the server does not have, and an operator would act on it. The Last seen column is a relative
//      Stamp, which is the honest form of a stamp with no polling behind it, and the note above the table
//      says what it means.
//   2. NO FLATTENING OF A POINTER. A pool's `waiting` and a machine's `active_leases` are `*int64` on
//      purpose: "could not ask the gateway" and "nothing is waiting" / "serving nothing" are different
//      answers, and the second of each is the one that means it is safe to unplug the Mac. The JSON key is
//      absent for the first, and this page renders that absence as a SENTENCE rather than a zero.
//   3. NO PRETENDING THIS IS THE WHOLE QUEUE. A gated TOOL CALL is not here and cannot be — it parks on
//      /approvals — and a pending MACHINE is not there and cannot be, because api/runners.go:520-521 says
//      why: "A MACHINE ENROLMENT HAS NO REQUEST HASH TO BIND TO", while the /v1/approvals decision route
//      requires one. Both screens name the other's.
//
// THE TWO FORMS ARE STILL ON THE PAGE, AND THAT IS A BLOCKED MOVE RATHER THAN A CHOICE.
// TODO(component-layer): Dialog — "Create a runner pool" and "Mint an enrolment key" both want to be dialogs
// behind a primary action in their panel's head. tests/auth.spec.ts asserts the served form sweep sees at
// least EIGHT forms and `grep -rc "<ResourceForm" app` is 10 today; a dialog-mounted form is not in the
// server-rendered HTML, so moving these two plus /policy's mint takes that count to 7.

interface PoolRow extends Record<string, unknown> {
  id?: string;
  name?: string;
  posture?: string;
  os?: string;
  arch?: string;
  strict_enrollment?: boolean;
  created_at?: string;
  /** ABSENT when the gateway could not be asked. `undefined` and `0` are different answers — see the header. */
  waiting?: number;
}

interface RunnerRow extends Record<string, unknown> {
  id?: string;
  pool_id?: string;
  label?: string;
  state?: string;
  os?: string;
  arch?: string;
  posture?: string;
  last_seen_at?: string;
}

/** RunnerDetail is the SINGLE read, and `active_leases` exists on it and on no listing. */
interface RunnerDetail extends RunnerRow {
  active_leases?: number;
}

interface PoolKeyRow extends Record<string, unknown> {
  id?: string;
  pool_id?: string;
  key_prefix?: string;
  created_at?: string;
  expires_at?: string;
  revoked_at?: string;
  last_used_at?: string;
}

/** MintedPoolKey is the CREATE response. `key` is set here and on no read — api/runners.go poolKeyView. */
interface MintedPoolKey {
  id?: string;
  pool_id?: string;
  key_prefix?: string;
  key?: string;
}

interface StillRunning {
  id?: string;
  label?: string;
  state?: string;
  pool_id?: string;
}

interface RevokedPoolKey {
  id?: string;
  enrolled_runners_still_running?: StillRunning[];
}

const detail = (err: unknown, fallback: string) => (err instanceof RelayError ? err.problem.detail : fallback);
const code = (err: unknown) => (err instanceof RelayError ? err.problem.code : "");

/** shape is the os/arch pair every fleet table carries, rendered once so the three cannot disagree. */
function Shape({ os, arch }: { os?: string; arch?: string }) {
  if (String(os ?? "") === "" && String(arch ?? "") === "") return <span className="cell-none">— any</span>;
  return <code>{`${String(os === undefined || os === "" ? "any" : os)}/${String(arch === undefined || arch === "" ? "any" : arch)}`}</code>;
}

/** IdCell is the reference console's identity cell: a readable prefix, the whole value in `title` and on the
 *  clipboard. Machine and pool ids are hashes; nobody reads the middle of a hash. */
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

export default function FleetPage() {
  const [poolReload, setPoolReload] = useState(0);
  const [pools, setPools] = useState<PoolRow[]>([]);

  const [poolName, setPoolName] = useState("");
  const [posture, setPosture] = useState("sandboxed-linux");
  const [poolOS, setPoolOS] = useState("");
  const [poolArch, setPoolArch] = useState("");
  const [poolStrict, setPoolStrict] = useState(false);
  const [creating, setCreating] = useState(false);
  const [createError, setCreateError] = useState("");
  const [createStatus, setCreateStatus] = useState("");
  const [strictStatus, setStrictStatus] = useState("");
  const [strictError, setStrictError] = useState("");

  // THE POOL THE KEY HALF ACTS ON, IN THE ADDRESS BAR. `replace` rather than `push`, because it is also
  // written on ARRIVAL when the URL names no pool — see lib/urlState.ts.
  const [keyPool, setKeyPool] = useQueryParam("pool", "replace");
  const [keyReload, setKeyReload] = useState(0);
  const [minting, setMinting] = useState(false);
  const [mintError, setMintError] = useState("");
  // THE MINTED VALUE LIVES HERE AND NOWHERE ELSE, for as long as the region is on screen — the same posture
  // /policy takes with an API key, and for the same reason: the server keeps no copy, so this response is the
  // only thing in the world that has it.
  const [minted, setMinted] = useState<MintedPoolKey | null>(null);
  const [revokingKey, setRevokingKey] = useState<PoolKeyRow | null>(null);
  const [keyRevokeBusy, setKeyRevokeBusy] = useState(false);
  const [keyRevoked, setKeyRevoked] = useState<RevokedPoolKey | null>(null);
  const [keyRevokeError, setKeyRevokeError] = useState("");

  const [runnerReload, setRunnerReload] = useState(0);
  const [runners, setRunners] = useState<RunnerRow[]>([]);
  const [lifecycleError, setLifecycleError] = useState("");
  // THE DIALOG'S SUBJECT IS THE SINGLE READ'S ANSWER, not the list row — which is why this state holds a
  // RunnerDetail and is set only after GET /v1/runners/{runner_id} returns. See openRevoke below.
  const [revokingRunner, setRevokingRunner] = useState<RunnerDetail | null>(null);
  const [runnerRevokeBusy, setRunnerRevokeBusy] = useState(false);
  /** The open row menu, by id. One at a time, because two open menus over a table is two answers to "what
   *  can I do to this row" and only one of them is about the row under the pointer.
   *
   *  A MENU ITEM DOES NOT CLOSE IT, and that is a focus requirement rather than a preference:
   *  components/ConfirmDestructive.tsx returns focus to the element that was focused when it opened, and an
   *  element removed from the DOM in the same click cannot receive it — Escape would then drop focus to
   *  <body>, which is the one outcome a modal's cancel must not have. The toggle and Escape close it. */
  const [menu, setMenu] = useState("");

  const [admitStatus, setAdmitStatus] = useState("");
  const [admitErrors, setAdmitErrors] = useState<Record<string, string>>({});

  // The key panel opens on a REAL pool. A picker that opens empty invites a mint against nothing, and every
  // stack has at least one pool — the tenant is seeded one at birth. A pool id this deployment does not have
  // is IGNORED rather than selected, so a hand-typed query string cannot open the mint on nothing.
  useEffect(() => {
    if (pools.length === 0) return;
    if (pools.some((p) => p.id === keyPool)) return;
    setKeyPool(String(pools[0]?.id ?? ""));
  }, [pools, keyPool, setKeyPool]);

  async function createPool() {
    setCreating(true);
    setCreateError("");
    setCreateStatus("");
    try {
      const body = await apiSend<PoolRow>("POST", "/runner-pools", {
        name: poolName,
        posture,
        os: poolOS,
        arch: poolArch,
        strict_enrollment: poolStrict,
      });
      setCreateStatus(
        `${String(body.name ?? poolName)} exists as ${String(body.id ?? "")} with posture ${String(body.posture ?? posture)}. ` +
          (body.strict_enrollment === true
            ? "Enrolling into it needs a human: a machine that presents a key waits here until it is admitted."
            : "Enrolling into it needs only a key — anything holding one joins without being asked about."),
      );
      setPoolName("");
      setPoolReload((n) => n + 1);
    } catch (err: unknown) {
      setCreateError(detail(err, "the pool could not be created"));
    } finally {
      setCreating(false);
    }
  }

  // setStrict switches an EXISTING pool's waiting room. It is reversible in one click, so it takes no
  // confirmation at all — SC 3.3.4's leg 1, and a dialog in front of a toggle is noise that teaches an
  // operator to dismiss dialogs.
  async function setStrict(pool: PoolRow, strict: boolean) {
    setStrictError("");
    setStrictStatus("");
    try {
      const body = await apiSend<PoolRow>("PATCH", `/runner-pools/${encodeURIComponent(String(pool.id))}`, {
        strict_enrollment: strict,
      });
      setStrictStatus(
        body.strict_enrollment === true
          ? `${String(body.id ?? "")} now holds new enrolments for a human. Machines already in it are unaffected.`
          : `${String(body.id ?? "")} now admits any machine holding a valid pool key, without asking.`,
      );
      setPoolReload((n) => n + 1);
    } catch (err: unknown) {
      setStrictError(detail(err, "the pool's enrolment mode could not be changed"));
    }
  }

  async function mintKey() {
    setMinting(true);
    setMintError("");
    setMinted(null);
    try {
      setMinted(await apiSend<MintedPoolKey>("POST", `/runner-pools/${encodeURIComponent(keyPool)}/keys`));
      setKeyReload((n) => n + 1);
    } catch (err: unknown) {
      setMintError(detail(err, "the enrolment key could not be minted"));
    } finally {
      setMinting(false);
    }
  }

  async function revokeKey(row: PoolKeyRow) {
    setKeyRevokeBusy(true);
    setKeyRevokeError("");
    setKeyRevoked(null);
    try {
      setKeyRevoked(await apiSend<RevokedPoolKey>("POST", `/runner-pool-keys/${encodeURIComponent(String(row.id))}/revoke`));
      setKeyReload((n) => n + 1);
    } catch (err: unknown) {
      setKeyRevokeError(detail(err, "the key could not be revoked"));
    } finally {
      setKeyRevokeBusy(false);
      setRevokingKey(null);
    }
  }

  // cordon/resume go through the NATIVE dialog and that is the published criterion rather than a shortcut:
  // WCAG 2.2 SC 3.3.4 is satisfied by any ONE of three legs, and a cordon satisfies leg 1 verbatim — resume
  // undoes it (execution/runner_gateway.go:324). window.confirm is keyboard-operable, screen-reader-announced
  // and focus-trapped by the browser, every property components/ConfirmDestructive.tsx has to re-earn.
  async function move(row: RunnerRow, action: "cordon" | "resume") {
    const question =
      action === "cordon"
        ? `Cordon ${String(row.id ?? "")} (${String(row.label ?? "no label")})?\n\n` +
          "It stops being offered new leases. Whatever it is running now keeps running — a cordon is not a " +
          "kill — and Resume puts it back in rotation."
        : `Resume ${String(row.id ?? "")} (${String(row.label ?? "no label")})?\n\n` +
          "It starts being offered leases again.";
    if (!window.confirm(question)) return;
    setLifecycleError("");
    try {
      await apiSend("POST", `/runners/${encodeURIComponent(String(row.id))}/${action}`);
      setRunnerReload((n) => n + 1);
    } catch (err: unknown) {
      setLifecycleError(detail(err, `the machine could not be ${action}ed`));
    }
  }

  /**
   * openRevoke READS THE MACHINE BEFORE IT ASKS ANYTHING, and that is the mechanism rather than a nicety.
   *
   * SC 3.3.4's only available leg for an irreversible action is Confirmed, and the word "reviewing" inside it
   * is a requirement: the operator must be able to see what is about to die. Half of that is `active_leases`
   * — how many sessions this machine is serving RIGHT NOW — and api/runners.go:49-59 puts it on the SINGLE
   * read and NOT on the listing, because "a page of them would be a page of separate instants presented as
   * one". So the dialog cannot be opened from the row: it is opened from the answer.
   *
   * A FAILED READ OPENS NOTHING. A dialog that appeared anyway would be reviewing a machine nobody looked at.
   */
  async function openRevoke(row: RunnerRow) {
    setLifecycleError("");
    try {
      setRevokingRunner(await apiGet<RunnerDetail>(`/runners/${encodeURIComponent(String(row.id))}`));
    } catch (err: unknown) {
      setLifecycleError(
        `${String(row.id ?? "")} could not be read, so there is nothing to review and the revocation was not offered: ${detail(err, "the machine could not be read")}`,
      );
    }
  }

  async function revokeRunner(row: RunnerDetail) {
    setRunnerRevokeBusy(true);
    setLifecycleError("");
    try {
      await apiSend("POST", `/runners/${encodeURIComponent(String(row.id))}/revoke`);
      setRunnerReload((n) => n + 1);
    } catch (err: unknown) {
      setLifecycleError(detail(err, "the machine could not be revoked"));
    } finally {
      setRunnerRevokeBusy(false);
      setRevokingRunner(null);
    }
  }

  /**
   * admit is the waiting room's door, and its three refusals get three sentences.
   *
   * The server distinguishes them and the console must not flatten them back together, because the FIX is
   * different for each: `insufficient_scope` is a key that needs `approve` minted into it, and
   * `approver_not_authorized` is a project whose `config_policy.approvers` does not name this key's
   * principal. Sending an operator to the wrong one of those costs an afternoon.
   */
  async function admit(row: RunnerRow) {
    const id = String(row.id ?? "");
    setAdmitStatus("");
    setAdmitErrors((current) => ({ ...current, [id]: "" }));
    try {
      await apiSend("POST", `/runners/${encodeURIComponent(id)}/approve`);
      setAdmitStatus(`${id} (${String(row.label ?? "no label")}) was admitted and may now take leases in ${String(row.pool_id ?? "its pool")}.`);
      setRunnerReload((n) => n + 1);
    } catch (err: unknown) {
      setAdmitErrors((current) => ({ ...current, [id]: code(err) === "" ? "unknown" : code(err) }));
    }
  }

  const poolOptions: PickerOption[] = pools
    .filter((p) => typeof p.id === "string" && p.id !== "")
    .map((p) => ({
      value: String(p.id),
      label: `${String(p.name ?? p.id)} — ${String(p.posture ?? "?")}${p.strict_enrollment === true ? ", strict enrolment" : ""} (${String(p.id)})`,
    }));
  const waitingRoom = runners.filter((r) => r.state === "pending");
  const revokedKeyMachines = keyRevoked?.enrolled_runners_still_running ?? [];

  return (
    <>
      {/* FLT-P15 STAYS OPEN AND STAYS FIRST. It changes what you CONCLUDE from everything else on the page,
          every single visit: "three active Macs" plus this sentence and "three active Macs" without it are
          different screens. */}
      <p className="muted" data-testid="fleet-remote-execution-note">
        <strong>
          A machine in a pool runs the ENGINE of a run placed there. Every tool still runs in the control
          plane&apos;s own process.
        </strong>{" "}
        A lease offer carries an image digest — that is the engine — and the shell commands, the file writes
        and the git calls happen where the control plane is. So a Mac in this list does not run{" "}
        <code>xcodebuild</code> unless the control plane is on it, and enrolling more Macs does not change
        that (<code>FLT-P15</code>: remote execution was deferred and has never shipped).
      </p>

      {/* THE STANDING FACTS, COLLAPSED. They are true on every visit and unchanged by anything on screen —
          the test this console applies to decide what collapses. Still text, still in the DOM, still
          keyboard-reachable, and the summary says what is inside. */}
      <details className="notes" data-testid="fleet-standing-notes">
        <summary>What a pool key alone can do, what an admission admits, and which queue is not here</summary>
        <p className="muted" data-testid="fleet-strict-note">
          <strong>With enrolment open, whoever holds a pool key is a machine in that pool.</strong> Nothing
          attests what an enrolling host actually is — the posture it declares is compared against the
          pool&apos;s, not verified — so the defences are the key&apos;s secrecy and how fast you revoke it
          (<code>FLT-P4</code>). Switching a pool to strict enrolment is what puts a human in front of the
          next machine (<code>FLT-P12</code>); it does not re-ask about the ones already in.
        </p>
        <p className="muted" data-testid="fleet-approval-scope-note">
          <strong>An admission admits an ENROLMENT, not a machine.</strong> The same Mac asks again after
          every reboot, because what was approved was this certificate rather than this hardware
          (<code>FLT-P13</code>).
        </p>
        {/* THE SCOPE NOTE MOVED IN HERE WITH THEM (it was the first paragraph on the page). It is the same
            kind of fact — permanently true, unchanged by anything on screen — and tests/fleet.spec.ts reads
            it by testid and by its link, both of which a details element keeps in the document. */}
        <p className="muted" data-testid="fleet-approvals-scope-note">
          <strong>This screen holds machines, not tool calls.</strong> A gated tool call parks a RUN and waits
          on <a href="/approvals">Approvals</a>; it is never here. The reason is the server&apos;s: a machine
          enrolment has <strong>no request hash to bind to</strong> — no arguments, no parked call, and the
          certificate was issued before anybody was asked — so it cannot ride the approval route, whose
          decision requires one.
        </p>
      </details>

      {/* --- 1. THE POOLS ------------------------------------------------------------------------------ */}
      <Panel<PoolRow>
        title="Runner pools"
        testId="panel-runner-pools"
        fetchPath="/runner-pools"
        reloadKey={poolReload}
        onRows={setPools}
        columns={[
          { header: "ID", sort: (r) => String(r.id ?? ""), render: (r) => <IdCell id={String(r.id ?? "")} label="pool ID" /> },
          {
            header: "Name",
            sort: (r) => String(r.name ?? ""),
            // THE NAME IS A LINK TO THIS PAGE WITH THE POOL NAMED, which is the only "detail" a pool has in
            // this API: there is no GET /v1/runner-pools/{id}, so the row's destination is the key table
            // below, scoped to it. An ANCHOR rather than a button, deliberately — it is a real address a
            // reader can copy, open in a second tab, or send to somebody.
            render: (r) => (
              <a className="cell-name-link" href={`/fleet?pool=${encodeURIComponent(String(r.id ?? ""))}`} data-testid={`pool-select-${String(r.id ?? "")}`}>
                <NameCell name={String(r.name ?? "")} id="" />
              </a>
            ),
          },
          { header: "Posture", sort: (r) => String(r.posture ?? ""), render: (r) => <code>{String(r.posture ?? "")}</code> },
          { header: "Shape", render: (r) => <Shape os={r.os} arch={r.arch} /> },
          {
            header: "Enrolment",
            sort: (r) => (r.strict_enrollment === true ? "waiting room" : "open"),
            render: (r) => <Status value={r.strict_enrollment === true ? "waiting room" : "open"} testId={`pool-mode-${String(r.id ?? "")}`} />,
          },
          {
            // THE POINTER, UNFLATTENED. See the header: absence is "the gateway could not be asked" and 0 is
            // "nothing is waiting", and only one of those means the pool is keeping up.
            header: "Waiting",
            render: (r) => (
              <span
                data-testid={`pool-waiting-${String(r.id ?? "")}`}
                data-waiting-known={r.waiting === undefined ? "false" : "true"}
              >
                {r.waiting === undefined ? "— the gateway could not be asked" : `${String(r.waiting)} queued with no free machine`}
              </span>
            ),
          },
          { header: "Created", sort: (r) => String(r.created_at ?? ""), render: (r) => <Stamp iso={r.created_at} /> },
          {
            header: "",
            render: (r) => {
              const id = String(r.id ?? "");
              const strict = r.strict_enrollment === true;
              return (
                <div className="row-menu">
                  <button
                    type="button"
                    className="row-menu-toggle"
                    aria-expanded={menu === `pool-${id}`}
                    aria-controls={`menu-pool-${id}`}
                    aria-label={`Actions for pool ${id}`}
                    data-testid={`pool-menu-${id}`}
                    onClick={() => setMenu(menu === `pool-${id}` ? "" : `pool-${id}`)}
                    onKeyDown={(e) => {
                      if (e.key === "Escape") setMenu("");
                    }}
                  >
                    <span aria-hidden="true">⋯</span>
                  </button>
                  {menu === `pool-${id}` ? (
                    <div className="row-menu-panel" id={`menu-pool-${id}`}>
                      {/* TWO ITEMS, AND THAT IS THE API: a pool's whole write surface after creation is
                          PATCH /v1/runner-pools/{id} with `strict_enrollment`, and there is no delete.
                          Selecting it is not a write; it is where the keys below are read from. */}
                      <button
                        type="button"
                        data-testid={`pool-strict-${id}`}
                        onClick={() => void setStrict(r, !strict)}
                      >
                        {strict ? "Open enrolment" : "Require approval"}
                      </button>
                      <button
                        type="button"
                        data-testid={`pool-keys-${id}`}
                        onClick={() => setKeyPool(id)}
                      >
                        Show enrolment keys
                      </button>
                    </div>
                  ) : null}
                </div>
              );
            },
          },
        ]}
        emptyNote={
          <>
            <p className="empty-title">No runner pools</p>
            <p className="empty-body">
              A pool is a POSTURE plus the shape of machine expected in it, and it is what a run is placed
              into. Every tenant is seeded one at birth, so an empty table here means the read did not reach
              the collection it named.
            </p>
          </>
        }
      />

      {strictStatus === "" ? null : (
        <p role="status" className="form-status" data-testid="pool-strict-status">
          <span className="glyph" aria-hidden="true">
            ✔
          </span>{" "}
          {strictStatus}
        </p>
      )}
      {strictError === "" ? null : (
        <p role="alert" className="form-error" data-testid="pool-strict-error">
          <span className="glyph" aria-hidden="true">
            ✖
          </span>{" "}
          {strictError}
        </p>
      )}

      {/* TODO(component-layer): Dialog — this is the pool panel's primary action wearing a section. See the
          header for the shipped guard that holds it here. */}
      <ResourceForm
        title="Create a runner pool"
        testId="pool-create"
        fields={[
          {
            name: "pool-name",
            label: "Name",
            value: poolName,
            onChange: setPoolName,
            required: true,
            hint: "Unique within this project. A second pool with the same name is refused rather than created.",
            testId: "pool-name-input",
          },
          {
            name: "pool-posture",
            label: "Posture",
            kind: "select",
            value: posture,
            onChange: setPosture,
            options: [
              { value: "sandboxed-linux", label: "sandboxed-linux — a container the control plane isolates" },
              { value: "unsandboxed-host", label: "unsandboxed-host — a real machine, e.g. a rented Mac" },
            ],
            testId: "pool-posture-select",
            hint: "Decided once and never afterwards: a machine inherits it at enrolment, so changing it would change what the machines already here are.",
            emptyNote: <>No postures are available, which cannot happen — this list is fixed in the schema.</>,
          },
          {
            name: "pool-os",
            label: "Operating system (optional)",
            value: poolOS,
            onChange: setPoolOS,
            hint: "The shape this pool expects, e.g. darwin. Leave it empty to accept any.",
            testId: "pool-os-input",
          },
          {
            name: "pool-arch",
            label: "Architecture (optional)",
            value: poolArch,
            onChange: setPoolArch,
            hint: "e.g. arm64. Leave it empty to accept any.",
            testId: "pool-arch-input",
          },
        ]}
        submitLabel="Create pool"
        submittingLabel="Creating…"
        submitTestId="pool-create-button"
        submitting={creating}
        error={createError}
        status={createStatus}
        onSubmit={createPool}
      >
        <fieldset data-testid="pool-strict-fieldset">
          <legend>Enrolment</legend>
          <label htmlFor="pool-strict-input">
            <input
              id="pool-strict-input"
              type="checkbox"
              checked={poolStrict}
              data-testid="pool-strict-input"
              aria-describedby="pool-strict-hint"
              onChange={(e) => setPoolStrict(e.target.checked)}
            />{" "}
            Require a human to admit each machine
          </label>
          <p className="muted" id="pool-strict-hint">
            With this off, any machine holding a valid pool key joins immediately. With it on, it waits in the
            room below until somebody admits it — and admitting takes the <code>approve</code> capability,
            which <code>provision</code> deliberately does not cover.
          </p>
        </fieldset>
      </ResourceForm>

      {/* --- 2. THE KEYS ------------------------------------------------------------------------------- */}
      <Picker
        id="poolkey-pool"
        label="Pool"
        value={keyPool}
        onChange={(value) => {
          setKeyPool(value);
          setMinted(null);
          setKeyRevoked(null);
        }}
        options={poolOptions}
        testId="poolkey-pool-select"
        hint="Enrolment keys belong to one pool: minting here admits machines into THAT pool and no other. The choice is in the address bar, so it survives a reload and can be sent to somebody."
        emptyNote={
          <>
            <strong>No pools are readable.</strong> Create one above before minting an enrolment key.
          </>
        }
      />

      {keyPool === "" ? null : (
        <Panel<PoolKeyRow>
          title="Enrolment keys"
          testId="panel-runner-pool-keys"
          fetchPath={`/runner-pools/${encodeURIComponent(keyPool)}/keys`}
          reloadKey={keyReload}
          note="Metadata only. A key's value exists in the mint response and nowhere else — the store keeps no copy, so there is no route that reads one back."
          emptyNote={
            <>
              <p className="empty-title">This pool has no enrolment key</p>
              <p className="empty-body">
                Nothing can join it until one is minted: a machine enrols by presenting a key and a
                certificate request, and no other path into a pool exists.
              </p>
            </>
          }
          columns={[
            { header: "ID", sort: (r) => String(r.id ?? ""), render: (r) => <IdCell id={String(r.id ?? "")} label="pool key ID" /> },
            { header: "Prefix", sort: (r) => String(r.key_prefix ?? ""), render: (r) => <code>{String(r.key_prefix ?? "")}</code> },
            {
              // A STATE, NOT A COLUMN CALLED "Revoke" WITH A BUTTON IN IT. The row's action moved to the ⋯;
              // what belongs in a column is what the row IS.
              header: "Status",
              sort: (r) => (r.revoked_at === undefined ? "live" : "revoked"),
              render: (r) => (
                <span title={r.revoked_at === undefined ? "This key still admits machines." : `Revoked ${String(r.revoked_at)}`}>
                  <Status value={r.revoked_at === undefined ? "live" : "revoked"} testId={`poolkey-state-${String(r.id ?? "")}`} />
                </span>
              ),
            },
            { header: "Created", sort: (r) => String(r.created_at ?? ""), render: (r) => <Stamp iso={r.created_at} /> },
            {
              header: "Expires",
              sort: (r) => String(r.expires_at ?? ""),
              render: (r) => (r.expires_at === undefined ? <span className="cell-none">— never</span> : <Stamp iso={r.expires_at} />),
            },
            {
              header: "Last used",
              sort: (r) => String(r.last_used_at ?? ""),
              render: (r) => (r.last_used_at === undefined ? <span className="cell-none">— never</span> : <Stamp iso={r.last_used_at} />),
            },
            {
              header: "",
              render: (r) => {
                const id = String(r.id ?? "");
                if (r.revoked_at !== undefined) return <span className="cell-none">—</span>;
                return (
                  <div className="row-menu">
                    <button
                      type="button"
                      className="row-menu-toggle"
                      aria-expanded={menu === `poolkey-${id}`}
                      aria-controls={`menu-poolkey-${id}`}
                      aria-label={`Actions for enrolment key ${id}`}
                      data-testid={`poolkey-menu-${id}`}
                      onClick={() => setMenu(menu === `poolkey-${id}` ? "" : `poolkey-${id}`)}
                      onKeyDown={(e) => {
                        if (e.key === "Escape") setMenu("");
                      }}
                    >
                      <span aria-hidden="true">⋯</span>
                    </button>
                    {menu === `poolkey-${id}` ? (
                      <div className="row-menu-panel" id={`menu-poolkey-${id}`}>
                        <button
                          type="button"
                          className="danger"
                          data-testid={`revoke-poolkey-${id}`}
                          onClick={() => setRevokingKey(r)}
                        >
                          Revoke {id}
                        </button>
                      </div>
                    ) : null}
                  </div>
                );
              },
            },
          ]}
        />
      )}

      {keyRevoked === null ? null : (
        // THE COUNT, AND THE SENTENCE THAT MAKES IT MEAN SOMETHING. api/runners.go names the field
        // `enrolled_runners_still_running` rather than `enrolled_runners` for exactly this: an operator not
        // shown them reads "revoked" as "removed" and believes one call decommissioned a fleet.
        // eslint-disable-next-line -- see the TODO above the concurrency notice below.
        <p role="status" className="form-status" data-glyph="warn" data-testid="poolkey-revoke-status">
          <span className="glyph" aria-hidden="true">
            !
          </span>{" "}
          <span>
          <strong>
            {String(keyRevoked.id ?? "")} is revoked and {String(revokedKeyMachines.length)} machine(s) it
            already admitted are still running.
          </strong>{" "}
          Revoking a key stops the NEXT machine from joining with it; it does not stop the ones that already
          did.{" "}
          {revokedKeyMachines.length === 0
            ? "This key had admitted nothing, so nothing survives it."
            : `Decommission each of them separately if that is what you meant: ${revokedKeyMachines.map((m) => `${String(m.id ?? "")} (${String(m.label ?? "no label")})`).join(", ")}.`}
          </span>
        </p>
      )}
      {keyRevokeError === "" ? null : (
        <p role="alert" className="form-error" data-testid="poolkey-revoke-error">
          <span className="glyph" aria-hidden="true">
            ✖
          </span>{" "}
          {keyRevokeError}
        </p>
      )}

      {/* TODO(component-layer): Dialog — the key panel's primary action, same as above. */}
      <ResourceForm
        title="Mint an enrolment key"
        testId="poolkey-mint"
        note={
          <>
            The value is shown <strong>once</strong>, here, and is retrievable from nowhere afterwards. It is
            what a machine presents when it first dials the runner plane, so it belongs in that machine&apos;s
            configuration and nowhere else.
          </>
        }
        fields={[]}
        submitLabel="Mint key"
        submittingLabel="Minting…"
        submitTestId="poolkey-mint-button"
        submitting={minting}
        error={mintError}
        onSubmit={mintKey}
      />

      {minted === null ? null : (
        <RevealOnce
          testId="poolkey-reveal"
          label="New enrolment key"
          announcement="A new pool enrolment key was created. Its value is on screen now and will not be shown again."
          value={String(minted.key ?? "")}
          meta={
            <>
              <code>{String(minted.id ?? "")}</code> admits machines into{" "}
              <code>{String(minted.pool_id ?? "")}</code>
            </>
          }
          onDismiss={() => setMinted(null)}
        />
      )}

      {/* --- 3. THE MACHINES --------------------------------------------------------------------------- */}
      <p className="muted" data-testid="runner-last-seen-note">
        <strong>
          <code>Last seen</code> is the last time the machine AUTHENTICATED — enrolled, connected or renewed
          its certificate.
        </strong>{" "}
        Nothing polls it and nothing expires a row, so a stale stamp means &quot;has not authenticated
        since&quot;, <strong>not &quot;is down&quot;</strong>. There is no health column on this table and
        that is deliberate: the API carries no such field, and a badge with nothing behind it is worse than a
        column that is not there.
      </p>

      <Panel<RunnerRow>
        title="Machines"
        testId="panel-runners"
        fetchPath="/runners"
        reloadKey={runnerReload}
        onRows={setRunners}
        // A REVOKED IDENTITY STAYS LISTED, and that is worth one sentence because a list that dropped them
        // would make a decommissioning invisible the moment it succeeded.
        note="Every machine that has enrolled, whatever it is doing now — a revoked identity stays in the table, because decommissioning is a fact worth keeping visible."
        emptyNote={
          <>
            <p className="empty-title">No machine has enrolled</p>
            <p className="empty-body">
              A machine joins by dialling the runner plane with a pool key and a certificate request. Nothing
              on this screen or anywhere else in the public API can put one here, so an empty table is the
              normal shape of a deployment whose runs are placed by the control plane&apos;s own workers.
            </p>
          </>
        }
        columns={[
          { header: "ID", sort: (r) => String(r.id ?? ""), render: (r) => <IdCell id={String(r.id ?? "")} label="machine ID" /> },
          {
            header: "Label",
            sort: (r) => String(r.label ?? ""),
            render: (r) => <NameCell name={String(r.label ?? "")} id="" />,
          },
          {
            header: "State",
            sort: (r) => String(r.state ?? ""),
            render: (r) => <Status value={String(r.state ?? "unknown")} testId={`runner-state-${String(r.id ?? "")}`} />,
          },
          { header: "Pool", sort: (r) => String(r.pool_id ?? ""), render: (r) => <code>{String(r.pool_id ?? "")}</code> },
          { header: "Shape", render: (r) => <Shape os={r.os} arch={r.arch} /> },
          { header: "Posture", sort: (r) => String(r.posture ?? ""), render: (r) => <code>{String(r.posture ?? "")}</code> },
          {
            header: "Last seen",
            sort: (r) => String(r.last_seen_at ?? ""),
            render: (r) => (r.last_seen_at === undefined ? <span className="cell-none">— never</span> : <Stamp iso={r.last_seen_at} />),
          },
          {
            header: "",
            // A PENDING MACHINE IS OFFERED NO CORDON, and that is the server's shape rather than tidiness:
            // `cordon` and `resume` answer 404 for a machine in the waiting room, because a cordon would
            // erase the fact that nobody had admitted it and the resume after it would then look
            // legitimate. What a waiting machine CAN be given is the other answer — revoke refuses the
            // enrolment — so that control stays.
            render: (r) => {
              const id = String(r.id ?? "");
              if (r.state === "revoked") return <span className="cell-none">— decommissioned</span>;
              return (
                <div className="row-menu">
                  <button
                    type="button"
                    className="row-menu-toggle"
                    aria-expanded={menu === `runner-${id}`}
                    aria-controls={`menu-runner-${id}`}
                    aria-label={`Actions for machine ${id}`}
                    data-testid={`runner-menu-${id}`}
                    onClick={() => setMenu(menu === `runner-${id}` ? "" : `runner-${id}`)}
                    onKeyDown={(e) => {
                      if (e.key === "Escape") setMenu("");
                    }}
                  >
                    <span aria-hidden="true">⋯</span>
                  </button>
                  {menu === `runner-${id}` ? (
                    <div className="row-menu-panel" id={`menu-runner-${id}`}>
                      {r.state === "pending" ? null : r.state === "cordoned" ? (
                        <button
                          type="button"
                          data-testid={`runner-resume-${id}`}
                          onClick={() => void move(r, "resume")}
                        >
                          Resume {id}
                        </button>
                      ) : (
                        <button
                          type="button"
                          data-testid={`runner-cordon-${id}`}
                          onClick={() => void move(r, "cordon")}
                        >
                          Cordon {id}
                        </button>
                      )}
                      <button
                        type="button"
                        className="danger"
                        data-testid={`runner-revoke-${id}`}
                        onClick={() => void openRevoke(r)}
                      >
                        Revoke {id}
                      </button>
                    </div>
                  ) : null}
                </div>
              );
            },
          },
        ]}
      />

      {lifecycleError === "" ? null : (
        <p role="alert" className="form-error" data-testid="runner-lifecycle-error">
          <span className="glyph" aria-hidden="true">
            ✖
          </span>{" "}
          {lifecycleError}
        </p>
      )}

      {/* THE CONCURRENCY CEILING, ON THE HALF OF ITS CONDITION THIS CONSOLE CAN OBSERVE. `palai up` prints
          this when PALAI_DISPATCH_WORKERS=1 AND at least two machines are enrolled. The worker count is read
          by the control-plane PROCESS and appears on no /v1 route, so the console cannot check it and says
          so — a screen that implied it had read the value would be claiming a measurement it never took. */}
      {/* TODO(component-layer): Callout — a WARNING BLOCK. `.status` is a PILL (app/globals.css: inline-flex,
          fixed --pill-h, `white-space: nowrap`), and a four-sentence paragraph wearing it does not wrap:
          measured 2026-07-31 on the fake profile, this notice rendered as ONE line and took the full-page
          screenshot from 1440 to 3296 CSS pixels wide. `.form-status` is the block-shaped sibling and wraps,
          so it is what these three carry until a warn-toned block exists; `data-glyph="warn"` stays on them
          as the hook. */}
      {runners.length < 2 ? null : (
        <p className="form-status" data-glyph="warn" data-testid="fleet-concurrency-note">
          <span className="glyph" aria-hidden="true">
            !
          </span>{" "}
          <span>
          <strong>
            {String(runners.length)} machines are enrolled, and concurrent runs are bounded by the control
            plane, not the fleet.
          </strong>{" "}
          Concurrency is the smaller of <code>PALAI_DISPATCH_WORKERS</code> and the fleet&apos;s lease slots,
          and that setting defaults to <strong>1</strong> — at which point the extra machines park and are
          never reached. <strong>This console cannot read it:</strong> it is read by the control-plane process
          and no <code>/v1</code> route publishes it, so this notice is shown on the machine count alone.
          Check it where the stack is configured, and raise <code>PALAI_RUNNER_CONCURRENCY</code> with it.
          </span>
        </p>
      )}

      {/* --- 4. THE WAITING ROOM ----------------------------------------------------------------------- */}
      {/* IT IS A TABLE NOW, WITH THE MACHINE LIST'S OWN COLUMNS. It was a `<ul>` of sentences with a button
          glued to the end of each, so an operator comparing what is IN the fleet against what is asking to
          get in was comparing a table to a paragraph. It is still derived from the rows above rather than
          from a second fetch, so a truncation notice there applies here too — which is the whole reason it
          is not a Panel of its own. */}
      <section className="panel" data-testid="waiting-room" aria-labelledby="waiting-room-h">
        <div className="panel-head">
          <h2 id="waiting-room-h">Waiting room</h2>
          <span className="panel-count">
            {waitingRoom.length} {waitingRoom.length === 1 ? "machine" : "machines"}
          </span>
        </div>
        <p className="muted">
          Machines that presented a valid enrolment key into a pool with strict enrolment. Admitting one takes
          the <code>approve</code> capability, which <code>provision</code> deliberately does not cover. This
          table is derived from the machine list above rather than from a second read, so a truncation notice
          there applies here too.
        </p>
        {admitStatus === "" ? null : (
          <p role="status" className="form-status" data-testid="admit-status">
            <span className="glyph" aria-hidden="true">
              ✔
            </span>{" "}
            {admitStatus}
          </p>
        )}
        {waitingRoom.length === 0 ? (
          <div className="empty" data-testid="waiting-room-empty">
            <p className="empty-title">No machine is waiting</p>
            <p className="empty-body">
              That is what an open pool always looks like: with strict enrolment off, a machine holding a
              valid key joins without asking, so an empty room here does not mean nothing joined.
            </p>
          </div>
        ) : (
          <table>
            <thead>
              <tr>
                <th scope="col">ID</th>
                <th scope="col">Label</th>
                <th scope="col">Asking to join</th>
                <th scope="col">Shape</th>
                <th scope="col">Waiting since</th>
                <th scope="col">Admit</th>
              </tr>
            </thead>
            <tbody>
              {waitingRoom.map((row) => {
                const id = String(row.id ?? "");
                const refusal = admitErrors[id] ?? "";
                return (
                  <tr key={id}>
                    <td>
                      <IdCell id={id} label="machine ID" />
                    </td>
                    <td>
                      <NameCell name={String(row.label ?? "")} id="" />
                    </td>
                    <td>
                      <code>{String(row.pool_id ?? "")}</code>
                    </td>
                    <td>
                      <Shape os={row.os} arch={row.arch} />
                    </td>
                    <td>{row.last_seen_at === undefined ? <span className="cell-none">— never</span> : <Stamp iso={row.last_seen_at} />}</td>
                    <td>
                      {/* SC 2.5.3 Label in Name: the accessible name must CONTAIN the visible one, so the
                          machine is APPENDED to "Admit" rather than replacing it with an aria-label. Seven
                          buttons each reading a 16-character hash is a column of noise; seven buttons all
                          reading "Admit" are indistinguishable to a screen reader. This is both. */}
                      <button type="button" className="primary" data-testid={`admit-${id}`} onClick={() => void admit(row)}>
                        Admit
                        <span className="sr-only"> {id}</span>
                      </button>
                      {refusal === "" ? null : (
                        <p role="alert" className="form-error" data-testid={`admit-${id}-error`}>
                          <span className="glyph" aria-hidden="true">
                            ✖
                          </span>{" "}
                          {/* THREE REFUSALS, THREE NEXT ACTIONS. The server distinguishes them; flattening
                              them into one sentence sends an operator to the wrong file. */}
                          {refusal === "approver_not_authorized" ? (
                            <>
                              <strong>This key is not in the project&apos;s approver list.</strong> The key holds
                              the <code>approve</code> capability and the project&apos;s{" "}
                              <code>config_policy.approvers</code> does not name the principal it resolves to. Add{" "}
                              <code>key:&lt;api_key_id&gt;</code> to that list on <a href="/policy">Policy</a>, or
                              admit this machine with a key that is already on it.
                            </>
                          ) : refusal === "insufficient_scope" ? (
                            <>
                              <strong>
                                This key has no <code>approve</code> capability.
                              </strong>{" "}
                              That is a different gate from the approver list and a different fix: mint a key
                              holding <code>approve</code> and start the console with it. It is deliberately not
                              covered by <code>provision</code>, so an administrator is not an approver by
                              default.
                            </>
                          ) : refusal === "not_found" ? (
                            <>
                              <strong>No such machine is waiting for approval.</strong> Three causes are{" "}
                              <strong>indistinguishable</strong> here and the server refuses to say which — it is
                              not yours, it does not exist, or it is no longer admissible because it was cordoned
                              or revoked. Re-read the list before deciding which happened.
                            </>
                          ) : (
                            <>
                              The admission was refused with <code>{refusal}</code>, which this screen has no
                              sentence for. That is a gap in this page rather than a description of what happened.
                            </>
                          )}
                        </p>
                      )}
                    </td>
                  </tr>
                );
              })}
            </tbody>
          </table>
        )}
      </section>

      {revokingKey === null ? null : (
        <ConfirmDestructive
          testId="poolkey-revoke-dialog"
          title="Revoke this enrolment key?"
          message={
            <>
              A revoked enrolment key never comes back, and the machines it already admitted{" "}
              <strong>keep running</strong> — revoking is how you stop the NEXT one, not how you decommission
              the last. The replacement is a new key with a new value, which every machine still using this
              one will need.
            </>
          }
          review={
            <dl>
              <dt>Key</dt>
              <dd>
                <code>{String(revokingKey.id ?? "")}</code>
              </dd>
              <dt>Pool</dt>
              <dd>
                <code>{String(revokingKey.pool_id ?? "")}</code>
              </dd>
              <dt>Last used</dt>
              <dd>{revokingKey.last_used_at === undefined ? "never — no machine has enrolled with it" : String(revokingKey.last_used_at)}</dd>
            </dl>
          }
          confirmLabel="Revoke it"
          busy={keyRevokeBusy}
          onConfirm={() => void revokeKey(revokingKey)}
          onCancel={() => setRevokingKey(null)}
        />
      )}

      {revokingRunner === null ? null : (
        <ConfirmDestructive
          testId="runner-revoke-dialog"
          title="Revoke this machine's identity?"
          message={
            <>
              A revoked runner identity is <strong>decommissioned, not paused</strong>: nothing un-revokes it,
              and neither Cordon nor Resume brings it back. If you meant &quot;stop giving it work for now&quot;,
              cancel and cordon it instead. The machine can only return by enrolling again as a{" "}
              <strong>new</strong> identity, with a valid pool key.
            </>
          }
          review={
            // THE REVIEW IS THE SINGLE READ'S ANSWER (SC 3.3.4 leg 3's "reviewing"), and the lease count is
            // the only reason that read happens. A page of them would be a page of separate instants.
            <dl>
              <dt>Machine</dt>
              <dd>
                <code>{String(revokingRunner.id ?? "")}</code> — {String(revokingRunner.label ?? "no label")}
              </dd>
              <dt>Pool</dt>
              <dd>
                <code>{String(revokingRunner.pool_id ?? "")}</code>
              </dd>
              <dt>Leases it is serving right now</dt>
              <dd data-testid="runner-revoke-leases">
                {revokingRunner.active_leases === undefined
                  ? "the gateway could not be asked, so this is unknown rather than none"
                  : `${String(revokingRunner.active_leases)} lease(s), read live just now`}
              </dd>
            </dl>
          }
          confirmLabel="Revoke it"
          busy={runnerRevokeBusy}
          onConfirm={() => void revokeRunner(revokingRunner)}
          onCancel={() => setRevokingRunner(null)}
        />
      )}
    </>
  );
}
