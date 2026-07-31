"use client";

import { useEffect, useState } from "react";

import { ConfirmDestructive } from "@/components/ConfirmDestructive";
import { Panel } from "@/components/Panel";
import { Picker, type PickerOption } from "@/components/Picker";
import { ResourceForm } from "@/components/ResourceForm";
import { RevealOnce } from "@/components/RevealOnce";
import { Status } from "@/components/Status";
import { apiGet, apiSend, RelayError } from "@/lib/api";

// THE FLEET SCREEN — POOLS, KEYS, MACHINES, THE WAITING ROOM, AND THE THREE THINGS IT REFUSES TO IMPLY
// (E28 T3, plan §T3).
//
// WHAT WAS MEASURED BEFORE IT. E24 shipped the whole read surface and E24 T6 shipped the waiting room's door
// — `POST /v1/runners/{runner_id}/approve`, correctly gated on `approve` rather than `provision` — and
// `grep -rc "runner-pools\|/v1/runners\|runner_pool" apps/web-console/{app,lib,components}` answered 0 in
// every file. E24 handed the screens to E25 as "free once console auth lands" and E25's plan was written
// before E24 existed, so neither plan carried the other's row and the surface fell between them. E28 T1 then
// found the half underneath: a pool could not be CREATED at all, so the state this page shows could not be
// produced either.
//
// FOUR SECTIONS, IN THE ORDER AN OPERATOR ACQUIRES THEM: a pool exists, a key admits machines into it,
// machines are in it, and one of them is asking to be let in.
//
// THREE THINGS THIS PAGE WILL NOT INVENT.
//
//   1. NO `healthy`, NO up/down, NO derived liveness. api/runners.go:26-29, verbatim: "last_seen_at records
//      the last time the machine AUTHENTICATED (enrol, connect, renew). Nothing polls and nothing expires a
//      row, so a stale stamp means 'has not authenticated since', NOT 'is down'. The item deliberately
//      carries no `healthy` field, because there is nothing behind one." A dot rendered from that stamp is a
//      status the server does not have, and an operator would act on it.
//   2. NO FLATTENING OF A POINTER. A pool's `waiting` and a machine's `active_leases` are `*int64` on
//      purpose: "could not ask the gateway" and "nothing is waiting" / "serving nothing" are different
//      answers, and the second of each is the one that means it is safe to unplug the Mac. The JSON key is
//      absent for the first, and this page renders that absence as a SENTENCE rather than a zero.
//   3. NO PRETENDING THIS IS THE WHOLE QUEUE. A gated TOOL CALL is not here and cannot be — it parks on
//      /approvals — and a pending MACHINE is not there and cannot be, because api/runners.go:520-521 says
//      why: "A MACHINE ENROLMENT HAS NO REQUEST HASH TO BIND TO", while the /v1/approvals decision route
//      requires one. Both screens name the other's.
//
// AND IT WRITES THREE CEILINGS, all of them measured gap rows rather than caution: `FLT-P15` (a Mac runs the
// ENGINE and not the tools), the control plane's concurrency bound (`dispatchWorkerFleetWarning`'s sentence),
// and `FLT-P12`/`FLT-P4` (with the waiting room closed, the pool key is the whole admission control).

interface PoolRow extends Record<string, unknown> {
  id?: string;
  name?: string;
  posture?: string;
  os?: string;
  arch?: string;
  strict_enrollment?: boolean;
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

  const [keyPool, setKeyPool] = useState("");
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
  // RunnerDetail and is set only after GET /v1/runners/{runner_id} returns. See revokeRunner below.
  const [revokingRunner, setRevokingRunner] = useState<RunnerDetail | null>(null);
  const [runnerRevokeBusy, setRunnerRevokeBusy] = useState(false);

  const [admitStatus, setAdmitStatus] = useState("");
  const [admitErrors, setAdmitErrors] = useState<Record<string, string>>({});

  // The key panel opens on a REAL pool. A picker that opens empty invites a mint against nothing, and every
  // stack has at least one pool — the tenant is seeded one at birth.
  useEffect(() => {
    setKeyPool((current) => (current === "" ? String(pools[0]?.id ?? "") : current));
  }, [pools]);

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
      {/* WHAT THIS PAGE DOES NOT HOLD, FIRST, for the reason /approvals says it first: a queue whose scope is
          unstated teaches an operator that everything pending is on one screen. */}
      <p className="muted" data-testid="fleet-approvals-scope-note">
        <strong>This screen holds machines, not tool calls.</strong> A gated tool call parks a RUN and waits
        on <a href="/approvals">Approvals</a>; it is never here. The two are separate on purpose and the
        reason is the server&apos;s: a machine enrolment has <strong>no request hash to bind to</strong> —
        there are no arguments, no parked call, and the certificate was issued before anybody was asked — so
        it cannot ride the approval route, whose decision requires one.
      </p>

      {/* THE STANDING FACTS ABOUT THIS DEPLOYMENT'S FLEET. Collapsed for the reason /approvals collapses its
          three: they are true on every visit and unchanged by anything on screen, and four paragraphs of
          equal weight before the first panel is how a screen stops being read. Still text, still in the DOM,
          still keyboard-reachable, and the summary says what is inside. */}
      {/* FLT-P15 STAYS OPEN, and the test for that is /approvals': it changes what you CONCLUDE from
          everything else on the page, every single visit. "Three active Macs" plus this sentence and "three
          active Macs" without it are different screens. */}
      <p className="muted" data-testid="fleet-remote-execution-note">
        <strong>
          A machine in a pool runs the ENGINE of a run placed there. Every tool still runs in the control
          plane&apos;s own process.
        </strong>{" "}
        A lease offer carries an image digest — that is the engine — and the shell commands, the file writes
        and the git calls happen where the control plane is. So a Mac in this list does not run{" "}
        <code>xcodebuild</code> unless the control plane is on it, and enrolling more Macs does not change
        that (<code>FLT-P15</code>: remote execution was deferred and has never shipped). Read this before
        anything else here, because it bounds what all of it is worth.
      </p>

      <details className="notes" data-testid="fleet-standing-notes">
        <summary>What a pool key alone can do, and what an admission actually admits</summary>
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
      </details>

      {/* --- 1. THE POOLS ------------------------------------------------------------------------------ */}
      <Panel<PoolRow>
        title="Runner pools"
        testId="panel-runner-pools"
        fetchPath="/runner-pools"
        reloadKey={poolReload}
        onRows={setPools}
        note="A pool is a POSTURE plus the shape of machine expected in it. A machine inherits its pool's posture when it enrols, which is why a pool's posture cannot be changed afterwards — it would retroactively change what the machines already in it are."
        columns={[
          { header: "ID", render: (r) => <code>{String(r.id ?? "")}</code> },
          { header: "Name", render: (r) => String(r.name ?? "") },
          { header: "Posture", render: (r) => <code>{String(r.posture ?? "")}</code> },
          {
            header: "Shape",
            render: (r) =>
              String(r.os ?? "") === "" && String(r.arch ?? "") === "" ? (
                <>— any</>
              ) : (
                <code>{`${String(r.os ?? "any")}/${String(r.arch ?? "any")}`}</code>
              ),
          },
          {
            header: "Enrolment",
            render: (r) => (
              <>
                <Status value={r.strict_enrollment === true ? "waiting room" : "open"} testId={`pool-mode-${String(r.id ?? "")}`} />{" "}
                <button
                  type="button"
                  data-testid={`pool-strict-${String(r.id ?? "")}`}
                  onClick={() => void setStrict(r, r.strict_enrollment !== true)}
                >
                  {r.strict_enrollment === true ? "Open enrolment" : "Require approval"}
                </button>
              </>
            ),
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
        ]}
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

      <ResourceForm
        title="Create a runner pool"
        testId="pool-create"
        note={
          <>
            Until this form existed a tenant had exactly one pool, forever: the only statement that wrote a
            pool row wrote its name, its posture and its enrolment mode as literals, so a rented-Mac pool
            could not exist and the waiting room could not be switched on.
          </>
        }
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
        hint="Enrolment keys belong to one pool. Minting here admits machines into THAT pool and no other."
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
              <strong>This pool has no enrolment key.</strong> Nothing can join it until one is minted: a
              machine enrols by presenting a key and a certificate request.
            </>
          }
          columns={[
            { header: "ID", render: (r) => <code>{String(r.id ?? "")}</code> },
            { header: "Prefix", render: (r) => <code>{String(r.key_prefix ?? "")}</code> },
            { header: "Created", render: (r) => String(r.created_at ?? "") },
            { header: "Expires", render: (r) => (r.expires_at === undefined ? "— never" : String(r.expires_at)) },
            { header: "Last used", render: (r) => (r.last_used_at === undefined ? "— never" : String(r.last_used_at)) },
            {
              header: "Revoke",
              render: (r) =>
                r.revoked_at === undefined ? (
                  <button type="button" data-testid={`revoke-poolkey-${String(r.id ?? "")}`} onClick={() => setRevokingKey(r)}>
                    Revoke {String(r.id ?? "")}
                  </button>
                ) : (
                  <>revoked {String(r.revoked_at)}</>
                ),
            },
          ]}
        />
      )}

      {keyRevoked === null ? null : (
        // THE COUNT, AND THE SENTENCE THAT MAKES IT MEAN SOMETHING. api/runners.go names the field
        // `enrolled_runners_still_running` rather than `enrolled_runners` for exactly this: an operator not
        // shown them reads "revoked" as "removed" and believes one call decommissioned a fleet.
        <p role="status" className="status" data-glyph="warn" data-testid="poolkey-revoke-status">
          <span className="glyph" aria-hidden="true">
            !
          </span>{" "}
          <strong>
            {String(keyRevoked.id ?? "")} is revoked and {String(revokedKeyMachines.length)} machine(s) it
            already admitted are still running.
          </strong>{" "}
          Revoking a key stops the NEXT machine from joining with it; it does not stop the ones that already
          did.{" "}
          {revokedKeyMachines.length === 0
            ? "This key had admitted nothing, so nothing survives it."
            : `Decommission each of them separately if that is what you meant: ${revokedKeyMachines.map((m) => `${String(m.id ?? "")} (${String(m.label ?? "no label")})`).join(", ")}.`}
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
        note="Every machine that has enrolled, whatever it is doing now. A revoked identity stays listed — decommissioning is a fact worth keeping visible."
        emptyNote={
          <>
            <strong>No machine has enrolled.</strong> A machine joins by dialling the runner plane with a pool
            key and a certificate request; nothing on this screen or anywhere else in the public API can put
            one here.
          </>
        }
        columns={[
          { header: "ID", render: (r) => <code>{String(r.id ?? "")}</code> },
          { header: "Pool", render: (r) => <code>{String(r.pool_id ?? "")}</code> },
          { header: "Label", render: (r) => String(r.label ?? "— none") },
          { header: "State", render: (r) => <Status value={String(r.state ?? "unknown")} testId={`runner-state-${String(r.id ?? "")}`} /> },
          { header: "Shape", render: (r) => <code>{`${String(r.os ?? "?")}/${String(r.arch ?? "?")}`}</code> },
          { header: "Posture", render: (r) => <code>{String(r.posture ?? "")}</code> },
          { header: "Last seen", render: (r) => (r.last_seen_at === undefined ? "— never" : String(r.last_seen_at)) },
          {
            header: "Lifecycle",
            render: (r) =>
              r.state === "revoked" ? (
                <>— decommissioned</>
              ) : (
                <>
                  {r.state === "cordoned" ? (
                    <button type="button" data-testid={`runner-resume-${String(r.id ?? "")}`} onClick={() => void move(r, "resume")}>
                      Resume {String(r.id ?? "")}
                    </button>
                  ) : (
                    <button type="button" data-testid={`runner-cordon-${String(r.id ?? "")}`} onClick={() => void move(r, "cordon")}>
                      Cordon {String(r.id ?? "")}
                    </button>
                  )}{" "}
                  <button type="button" className="danger" data-testid={`runner-revoke-${String(r.id ?? "")}`} onClick={() => void openRevoke(r)}>
                    Revoke {String(r.id ?? "")}
                  </button>
                </>
              ),
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
      {runners.length < 2 ? null : (
        <p className="status" data-glyph="warn" data-testid="fleet-concurrency-note">
          <span className="glyph" aria-hidden="true">
            !
          </span>{" "}
          <strong>
            {String(runners.length)} machines are enrolled, and concurrent runs are bounded by the control
            plane, not the fleet.
          </strong>{" "}
          Concurrency is the smaller of <code>PALAI_DISPATCH_WORKERS</code> and the fleet&apos;s lease slots,
          and that setting defaults to <strong>1</strong> — at which point the extra machines park and are
          never reached. <strong>This console cannot read it:</strong> it is read by the control-plane process
          and no <code>/v1</code> route publishes it, so this notice is shown on the machine count alone.
          Check it where the stack is configured, and raise <code>PALAI_RUNNER_CONCURRENCY</code> with it.
        </p>
      )}

      {/* --- 4. THE WAITING ROOM ----------------------------------------------------------------------- */}
      <section className="panel" data-testid="waiting-room" aria-labelledby="waiting-room-h">
        <h2 id="waiting-room-h">Waiting room</h2>
        <p className="muted">
          Machines that presented a valid enrolment key into a pool with strict enrolment and are waiting for
          a human. Admitting one needs the <code>approve</code> capability, which <code>provision</code>{" "}
          deliberately does not cover — a key that could provision could add itself to the approver list it is
          about to be checked against. This list is derived from the table above, so a truncation notice there
          applies here too.
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
          <p className="empty" data-testid="waiting-room-empty">
            <strong>No machine is waiting.</strong> That is what an open pool always looks like: with strict
            enrolment off, a machine holding a valid key joins without asking, so an empty room here does not
            mean nothing joined.
          </p>
        ) : (
          <ul>
            {waitingRoom.map((row) => {
              const id = String(row.id ?? "");
              const refusal = admitErrors[id] ?? "";
              return (
                <li key={id}>
                  <code>{id}</code> — {String(row.label ?? "no label")}, asking to join{" "}
                  <code>{String(row.pool_id ?? "")}</code> as{" "}
                  <code>{`${String(row.os ?? "?")}/${String(row.arch ?? "?")}`}</code>{" "}
                  <button type="button" data-testid={`admit-${id}`} onClick={() => void admit(row)}>
                    Admit {id}
                  </button>
                  {refusal === "" ? null : (
                    <p role="alert" className="form-error" data-testid={`admit-${id}-error`}>
                      <span className="glyph" aria-hidden="true">
                        ✖
                      </span>{" "}
                      {/* THREE REFUSALS, THREE NEXT ACTIONS. The server distinguishes them; flattening them
                          into one sentence sends an operator to the wrong file. */}
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
                </li>
              );
            })}
          </ul>
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
