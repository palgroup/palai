import type { Palai } from "../client.ts";
import { callArgs, enc, listPath, type CallOptions, type ListParams, type ListView, type Page } from "./shared.ts";

// The machine-fleet admin surface (E24/E28): pools a Mac enrolls into, the enrolment keys that admit
// one, and the enrolled machines themselves. Requires an operator key (see PalaiAdmin).
//
// NO GENERATED TYPE EXISTS FOR THESE YET — `src/generated/types.ts` has no runner / runner-pool /
// runner-pool-key entry (`grep -in runner src/generated/types.ts` finds nothing) and this tree carries
// no generator invocation to run one. The projections below are typed by hand against the exact shape
// `apps/control-plane/api/runners.go` renders (runnerView / runnerPoolView / poolKeyView) rather than
// invented into the generated file, which stays untouched.

// --- projections ----------------------------------------------------------------------------------

// RunnerPool is one pool's read projection (list/create/patch all render this same shape). `waiting`
// is present only on a deployment that wired the runner gateway — its absence and a literal 0 are
// different answers (see runners.go's RunnerPoolItem.Waiting).
export interface RunnerPool {
  id: string;
  object: string;
  name: string;
  posture: string;
  os: string;
  arch: string;
  strict_enrollment: boolean;
  created_at: string;
  waiting?: number;
}

export interface RunnerPoolCreateParams {
  name: string;
  posture: string;
  os?: string;
  arch?: string;
  strict_enrollment?: boolean;
}

// RunnerPoolPatchParams carries the ONE field a pool can be patched through. It is required, not
// optional: the route 400s on a missing value rather than assuming one, because a pool's posture is
// fixed at creation and there is no default to silently keep.
export interface RunnerPoolPatchParams {
  strict_enrollment: boolean;
}

// RunnerPoolKey is one enrolment key's projection. `key` is present ONLY on the mint response — the
// one time the plaintext value is disclosed, never on a list or a revoke. `enrolled_runners_still_running`
// is present only on a revoke, naming the machines that key already admitted (none of which is stopped
// by revoking it).
export interface RunnerPoolKey {
  id: string;
  object: string;
  pool_id: string;
  key_prefix: string;
  created_at: string;
  expires_at?: string;
  revoked_at?: string;
  last_used_at?: string;
  key?: string;
  enrolled_runners_still_running?: RunnerPoolKeyEnrollment[];
}

export interface RunnerPoolKeyEnrollment {
  id: string;
  label: string;
  runner_dns: string;
  state: string;
  pool_id: string;
  enrolled_at: string;
}

export interface RunnerPoolKeyMintParams {
  expires_at?: string;
}

// Runner is one enrolled machine's read projection. Every optional field below is omitted by the
// server rather than sent as a zero value — see runners.go's runnerView for why (a machine that has
// never reported carries no config_revision at all, not a `0`).
export interface Runner {
  id: string;
  object: string;
  pool_id: string;
  label: string;
  runner_dns: string;
  public_key_sha256: string;
  state: string;
  os: string;
  arch: string;
  posture: string;
  capacity: number;
  created_at: string;
  cert_not_after?: string;
  enrolled_at?: string;
  last_seen_at?: string;
  active_leases?: number;
  config_revision?: number;
  config_applied?: Record<string, string>;
  config_reported_at?: string;
}

// DesiredConfig is the per-pool or per-machine desired-configuration document. `desired` is `null`
// when nobody has decided anything for this scope yet — a real, distinct answer from "nothing to
// show", not an absent field (see runners.go's desiredForScope).
export interface DesiredConfig {
  object: string;
  plane: string;
  scope_id: string;
  desired: unknown;
}

// --- RunnerPools -----------------------------------------------------------------------------------

// RunnerPools administers the pool a Mac enrolls into: what it IS (posture/os/arch), decided once at
// creation, and whether enrolling into it needs a human (strict_enrollment), which may change anytime.
// Requires the `provision` capability.
export class RunnerPools {
  // The route family this class covers, read by the fleet coverage sweep
  // (test/fleet-coverage.test.ts) — not by any request. Every entry here has a matching
  // `this.#client.request(...)` call below; a route added here with no such call would make the sweep
  // claim coverage the SDK does not have.
  static readonly routes = [
    "GET /v1/runner-pools",
    "POST /v1/runner-pools",
    "PATCH /v1/runner-pools/{pool_id}",
    "GET /v1/runner-pools/{pool_id}/desired",
  ] as const;

  #client: Palai;
  constructor(client: Palai) {
    this.#client = client;
  }

  // create opens one pool (201). posture has no default — a pool created without one would place runs
  // somewhere nobody described.
  async create(params: RunnerPoolCreateParams, options: CallOptions = {}): Promise<RunnerPool> {
    const r = await this.#client.request<RunnerPool>("POST", "/v1/runner-pools", { body: params, ...callArgs(options) });
    return r.body;
  }

  // list returns a tenant-scoped page of pools with the shared cursor + basic filters.
  async list(params: ListParams = {}, options: CallOptions = {}): Promise<Page<RunnerPool>> {
    const r = await this.#client.request<Page<RunnerPool>>("GET", listPath("/v1/runner-pools", params), callArgs(options));
    return r.body;
  }

  // patch flips strict_enrollment — the only field a pool can be patched through. `posture` is
  // deliberately not among them: a machine inherits its pool's posture at enrolment, so moving a
  // populated pool's posture would retroactively change what its machines already ARE.
  async patch(poolID: string, params: RunnerPoolPatchParams, options: CallOptions = {}): Promise<RunnerPool> {
    const r = await this.#client.request<RunnerPool>("PATCH", `/v1/runner-pools/${enc(poolID)}`, { body: params, ...callArgs(options) });
    return r.body;
  }

  // getDesired reads the pool-scoped desired-configuration document.
  async getDesired(poolID: string, options: CallOptions = {}): Promise<DesiredConfig> {
    const r = await this.#client.request<DesiredConfig>("GET", `/v1/runner-pools/${enc(poolID)}/desired`, callArgs(options));
    return r.body;
  }
}

// --- RunnerPoolKeys --------------------------------------------------------------------------------

// RunnerPoolKeys mints and revokes a pool's enrolment keys. There is no route that reads a key's value
// back — nothing stores it to return — so the value appears exactly once, in the mint response.
// Requires the `provision` capability.
export class RunnerPoolKeys {
  static readonly routes = [
    "POST /v1/runner-pools/{pool_id}/keys",
    "GET /v1/runner-pools/{pool_id}/keys",
    "POST /v1/runner-pool-keys/{key_id}/revoke",
  ] as const;

  #client: Palai;
  constructor(client: Palai) {
    this.#client = client;
  }

  // mint creates an enrolment key for a pool (201). `expiresAt` is optional; a key minted with none
  // never expires. A caller who loses the value mints another and revokes this one.
  async mint(poolID: string, params: RunnerPoolKeyMintParams = {}, options: CallOptions = {}): Promise<RunnerPoolKey> {
    const args = callArgs(options);
    const r = await this.#client.request<RunnerPoolKey>(
      "POST",
      `/v1/runner-pools/${enc(poolID)}/keys`,
      params.expires_at === undefined ? args : { body: { expires_at: params.expires_at }, ...args },
    );
    return r.body;
  }

  // list returns a pool's keys as metadata only — never a value, never a digest.
  async list(poolID: string, options: CallOptions = {}): Promise<ListView<RunnerPoolKey>> {
    const r = await this.#client.request<ListView<RunnerPoolKey>>("GET", `/v1/runner-pools/${enc(poolID)}/keys`, callArgs(options));
    return r.body;
  }

  // revoke closes a key and returns the machines it already admitted, none of which is stopped by
  // revoking it.
  async revoke(keyID: string, options: CallOptions = {}): Promise<RunnerPoolKey> {
    const r = await this.#client.request<RunnerPoolKey>("POST", `/v1/runner-pool-keys/${enc(keyID)}/revoke`, callArgs(options));
    return r.body;
  }
}

// --- Runners ---------------------------------------------------------------------------------------

// Runners reads the enrolled-machine registry and drives its lifecycle. The three lifecycle verbs
// (cordon/resume/revoke) and the desired-config read require `provision`; `approve` is gated on its
// own `approve` capability instead — admitting a machine is a DECISION, not administration (see
// runners.go's approveRunner for why the two capabilities stay independent).
export class Runners {
  static readonly routes = [
    "GET /v1/runners",
    "GET /v1/runners/{runner_id}",
    "GET /v1/runners/{runner_id}/desired",
    "POST /v1/runners/{runner_id}/approve",
    "POST /v1/runners/{runner_id}/cordon",
    "POST /v1/runners/{runner_id}/resume",
    "POST /v1/runners/{runner_id}/revoke",
  ] as const;

  #client: Palai;
  constructor(client: Palai) {
    this.#client = client;
  }

  // list returns a tenant-scoped page of enrolled machines with the shared cursor + basic filters.
  async list(params: ListParams = {}, options: CallOptions = {}): Promise<Page<Runner>> {
    const r = await this.#client.request<Page<Runner>>("GET", listPath("/v1/runners", params), callArgs(options));
    return r.body;
  }

  // retrieve reads one machine; an absent or foreign id is a 404.
  async retrieve(runnerID: string, options: CallOptions = {}): Promise<Runner> {
    const r = await this.#client.request<Runner>("GET", `/v1/runners/${enc(runnerID)}`, callArgs(options));
    return r.body;
  }

  // getDesired reads the machine-scoped desired-configuration document.
  async getDesired(runnerID: string, options: CallOptions = {}): Promise<DesiredConfig> {
    const r = await this.#client.request<DesiredConfig>("GET", `/v1/runners/${enc(runnerID)}/desired`, callArgs(options));
    return r.body;
  }

  // approve admits one machine out of a strict pool's waiting room.
  async approve(runnerID: string, options: CallOptions = {}): Promise<Runner> {
    const r = await this.#client.request<Runner>("POST", `/v1/runners/${enc(runnerID)}/approve`, callArgs(options));
    return r.body;
  }

  // cordon takes a machine out of service without decommissioning it.
  async cordon(runnerID: string, options: CallOptions = {}): Promise<Runner> {
    const r = await this.#client.request<Runner>("POST", `/v1/runners/${enc(runnerID)}/cordon`, callArgs(options));
    return r.body;
  }

  // resume returns a cordoned machine to service.
  async resume(runnerID: string, options: CallOptions = {}): Promise<Runner> {
    const r = await this.#client.request<Runner>("POST", `/v1/runners/${enc(runnerID)}/resume`, callArgs(options));
    return r.body;
  }

  // occupancies reads ONE machine's session history, newest first: what it is holding now and what it
  // held before. It is the question an operator asks before unplugging a Mac.
  //
  // ‼️ THE CURSOR IS A PAIR AND BOTH HALVES TRAVEL TOGETHER. Two holds can begin in the same clock tick,
  // so a page ordered on time alone drops or repeats rows between requests; the control plane refuses a
  // half cursor by name rather than guessing, and this signature makes the pair unsplittable by typing
  // it as one object.
  async occupancies(
    runnerID: string,
    page: { limit?: number; startingBefore?: { at: string; id: string } } = {},
    options: CallOptions = {},
  ): Promise<ListView<MachineOccupancy>> {
    const query = new URLSearchParams();
    if (page.limit !== undefined) query.set("limit", String(page.limit));
    if (page.startingBefore) {
      query.set("starting_before", page.startingBefore.at);
      query.set("starting_before_id", page.startingBefore.id);
    }
    const suffix = query.size > 0 ? `?${query.toString()}` : "";
    const r = await this.#client.request<ListView<MachineOccupancy>>(
      "GET",
      `/v1/runners/${enc(runnerID)}/occupancies${suffix}`,
      callArgs(options),
    );
    return r.body;
  }

  // revoke decommissions a machine. Irreversible.
  async revoke(runnerID: string, options: CallOptions = {}): Promise<Runner> {
    const r = await this.#client.request<Runner>("POST", `/v1/runners/${enc(runnerID)}/revoke`, callArgs(options));
    return r.body;
  }
}

// MachineOccupancy is one hold as the machine-detail view reads it.
//
// `releasedAt` and `releaseReason` are OPTIONAL because an open hold has neither, and the control plane
// omits them rather than sending empty strings: "still running" and "released for a reason nobody
// recorded" are different answers, and a blank would collapse them. `billedSeconds` is present on both —
// an open hold bills the time it has held so far, because "in progress" is not "free".
export interface MachineOccupancy {
  object: "machine_occupancy";
  id: string;
  session_id: string;
  started_at: string;
  last_activity_at: string;
  billed_seconds: number;
  released_at?: string;
  release_reason?: string;
}

// --- Fleet -------------------------------------------------------------------------------------------

// Fleet is the machine-fleet surface PalaiAdmin exposes, grouped as the three nouns an operator
// actually thinks in — a pool, a key, a machine — rather than fourteen flat methods on PalaiAdmin
// itself.
export class Fleet {
  readonly pools: RunnerPools;
  readonly poolKeys: RunnerPoolKeys;
  readonly runners: Runners;

  constructor(client: Palai) {
    this.pools = new RunnerPools(client);
    this.poolKeys = new RunnerPoolKeys(client);
    this.runners = new Runners(client);
  }
}
