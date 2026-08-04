// THE FAKE-VS-REAL CONFORMANCE SWEEP — the crown of E19 T7, the plan's D15 closure.
//
// E17 T10 built this console against a fake /v1 upstream and its own exit gate then proved a fake CAN
// diverge from the real contract, leaving the question "how is fixture drift caught?" answered only by a
// reviewer's attention. This binds it to a test. The sweep takes the very objects
// tests/fake-control-plane.mjs dispatches from — `ROUTES` and `SCRIPTED_EVENTS`, imported, not
// re-described — and diffs them against the surface it gathers from a RUNNING REAL control plane. Both
// failure directions the plan requires are here: a route the fixture serves that the real router does not
// register FAILS, and a route both serve where the real one answers a different shape or status FAILS. The
// event arm is the same discipline applied to the stream, which is where E17 T10's own invented approval
// event lived — and where this sweep found five more inventions.
//
// Anything that differs must be written down in tests/divergences.mjs; anything written down that has
// STOPPED differing fails too. The ledger cannot rot in either direction.
//
// WHY node:test AND NOT PLAYWRIGHT: this compares two HTTP servers. No browser, no console, and the
// fixture's contract objects are plain ESM a .mjs test imports directly. node:test is stdlib.
//
// ---------------------------------------------------------------------------------------------------
// HOW THE REAL SURFACE IS GATHERED — the load-bearing mechanism, and the reason this is a measurement
// rather than a document review.
//
// Go's net/http ServeMux answers a request whose PATH matches a registered pattern but whose METHOD does
// not with 405 plus an `Allow` header naming the methods it does serve, and answers a path matching NO
// pattern with a bare 404. One OPTIONS request per pattern therefore reads the real router's method set for
// that path straight out of the running process — no debug endpoint, no reflection, no source parsing.
// Crucially it does NOT confuse "this route is not registered" with "this resource does not exist": a
// resource 404 is produced by a HANDLER, which means the path matched, which means OPTIONS on it returns
// 405+Allow. Only an unregistered pattern gives 404 with no Allow. (Verified against the real router's
// actual shape, including its top-level `/` fallthrough, before this sweep was written.)
// ---------------------------------------------------------------------------------------------------
//
// NO CREDENTIAL ANYWHERE: the bootstrap key arrives as PALAI_API_KEY and is never printed, never
// interpolated into a failure message, never written to disk.
import assert from "node:assert/strict";
import { spawn, spawnSync } from "node:child_process";
import { after, before, describe, it } from "node:test";
import { setTimeout as delay } from "node:timers/promises";
import { fileURLToPath } from "node:url";

import { DIVERGENCES, find } from "./divergences.mjs";
import { ROUTES, SCRIPTED_EVENTS } from "./fake-control-plane.mjs";

// --- The real stack, from the environment. ABSENCE IS A FAILURE, NEVER A SKIP. -------------------------
//
// A sweep that quietly skips when the stack is down is worse than no sweep: it reports green for the exact
// condition it exists to detect. This throws at load time, naming what is missing and how to supply it.
const REAL_BASE = (process.env.PALAI_BASE_URL ?? "").trim().replace(/\/+$/, "");
const REAL_KEY = (process.env.PALAI_API_KEY ?? "").trim();
if (REAL_BASE === "" || REAL_KEY === "") {
  throw new Error(
    "the fake-vs-real conformance sweep requires a RUNNING real control plane and REFUSES to skip: set " +
      "PALAI_BASE_URL (from $PALAI_HOME/config.json .base_url) and PALAI_API_KEY (read from " +
      "$PALAI_HOME/api-key into the environment — never passed on a command line). Bring the stack up with " +
      "`PALAI_DISPATCH_WORKERS=1 PALAI_MODEL_PROVIDER=fake palai local up`.",
  );
}

const FAKE_PORT = Number(process.env.SWEEP_FAKE_PORT ?? 3299);
const FAKE_BASE = `http://127.0.0.1:${FAKE_PORT}`;
const FAKE_KEY = "sweep-fixture-token";
const PROBE = "zzsweepprobe";
const RUN_BUDGET_MS = Number(process.env.SWEEP_RUN_BUDGET_MS ?? 180_000);
const REPO_ROOT = fileURLToPath(new URL("../../../", import.meta.url));

// observed accumulates the ledger ids this run re-derived, so the staleness arm can demand that every row
// was actually seen. A row nobody observes is either closed (delete it) or decoration (worse).
const observed = new Set();

// seeded holds the ids seedBothStacks created on each side, so a wildcard PATH can be made concrete with a
// row that actually exists (E25 T6). `{agent_id}` used to be filled with a probe token, which on a real stack
// is an unknown profile answering an empty page — an item comparison over nothing.
const seeded = { real: {}, fixture: {} };

// The model the seeded revision pins. Distinctive, so the DIV-UI-007 arm can say something about whether it
// travelled — and never equal to the deployment default, which is what would make that arm vacuous.
const SEEDED_REVISION_MODEL = "sweep-pinned-by-revision";

// requireLedgerRow is the single gate every arm funnels through: a difference with no ledger row FAILS with
// the difference spelled out, which is the D15 requirement in one function.
function requireLedgerRow(kind, subject, detail) {
  const row = find(kind, subject);
  assert.ok(
    row !== undefined,
    `UNRECORDED FAKE-VS-REAL DIVERGENCE [${kind}] ${subject}\n  ${detail}\n\n` +
      "Every difference between tests/fake-control-plane.mjs and the real control plane must be written down " +
      "in tests/divergences.mjs with its detail and its owner. That is D15: fixture drift is caught by this " +
      "test, not by a reviewer noticing.",
  );
  observed.add(row.id);
  return row;
}

// --- HTTP helpers. -------------------------------------------------------------------------------------
const realFetch = (path, init = {}) =>
  fetch(`${REAL_BASE}${path}`, { ...init, headers: { authorization: `Bearer ${REAL_KEY}`, ...(init.headers ?? {}) } });
const fakeFetch = (path, init = {}) =>
  fetch(`${FAKE_BASE}${path}`, { ...init, headers: { authorization: `Bearer ${FAKE_KEY}`, ...(init.headers ?? {}) } });

/** concretePath substitutes a probe token for each {wildcard} so a pattern becomes a requestable path. */
const concretePath = (pattern) => pattern.replace(/\{[a-z_]+\}/g, PROBE);

/**
 * seededPath is concretePath with the ids seedBothStacks actually created substituted in, per side (E25 T6).
 * A wildcard with no seed still gets the probe token — the only thing that changes is that a route whose
 * item shape CAN be compared now is.
 *
 * A PATTERN-SCOPED KEY WINS OVER THE BARE NAME, and that is not generality for its own sake: `{revision_id}`
 * is the ONE OVERLOADED WILDCARD in this table (E25 T7). An agent revision and a tool-set revision both use
 * the name, and a flat map handed GET /v1/tool-sets/{set}/revisions/{revision_id} an AGENT revision id —
 * which the real route correctly 404s, which makes arm 3 `continue` past it, which means the comparison this
 * seed exists to enable would have been skipped IN SILENCE. Exactly the failure mode T6 found with the probe
 * token, one wildcard later. Scoped keys are written as `<pattern>|<name>`.
 */
const seededPath = (pattern, side) =>
  pattern.replace(/\{([a-z_]+)\}/g, (_m, name) => {
    const scoped = seeded[side][`${pattern}|${name}`];
    const value = typeof scoped === "string" ? scoped : seeded[side][name];
    return typeof value === "string" ? encodeURIComponent(value) : PROBE;
  });

/** allowProbe reads a running router's method set for one path pattern — see the header for why this works. */
async function allowProbe(base, pattern, key) {
  const res = await fetch(`${base}${concretePath(pattern)}`, { method: "OPTIONS", headers: { authorization: `Bearer ${key}` } });
  await res.body?.cancel().catch(() => {});
  if (res.status === 404) return { registered: false, methods: [] };
  const allow = res.headers.get("allow") ?? "";
  return { registered: true, methods: allow.split(",").map((m) => m.trim().toUpperCase()).filter(Boolean) };
}

/**
 * keyShape reduces a JSON value to its STRUCTURE — sorted key names, recursively, arrays folded to their
 * first element. Values are deliberately ignored: a real stack's ids and timestamps differ from a fixture's
 * by design and that is not drift. What IS drift is a key one side has and the other does not.
 */
function keyShape(value) {
  if (Array.isArray(value)) return value.length === 0 ? "[]" : `[${keyShape(value[0])}]`;
  if (value === null || typeof value !== "object") return typeof value;
  return `{${Object.keys(value).sort().map((k) => `${k}:${keyShape(value[k])}`).join(",")}}`;
}

/**
 * journaledByProductionCode re-derives whether ANY non-test production Go file carries this event-type
 * literal. It is what separates an INVENTED event (the E17 T10 class — the fixture made the name up) from
 * an UNEXERCISED one (a real event this tier cannot reach). Conflating those would let five real inventions
 * hide behind a coverage excuse, so the distinction is re-measured rather than trusted to the ledger's prose.
 *
 * tests/ and examples/ are excluded because they are where fixtures live — the very thing under audit.
 * sdks/ is excluded for the same reason: sdks/go/runner/main.go carries event literals inside a fake SSE
 * fixture string, which is not a journaling site.
 */
function journaledByProductionCode(type) {
  const out = spawnSync("git", ["grep", "-l", "-F", `"${type}"`, "--", "*.go"], { cwd: REPO_ROOT, encoding: "utf8" });
  return out.stdout
    .split("\n")
    .filter((f) => f !== "" && !f.endsWith("_test.go") && !/^(tests|examples|sdks)\//.test(f));
}

// --- The fixture, started by the sweep itself. ----------------------------------------------------------
// The sweep spawns the fake rather than assuming a Playwright webServer left one running: it must answer
// "is this table a surface that is actually served?" on its own, and probing the running fixture the SAME
// way it probes the real router is the only honest way to ask.
let fake;
before(async () => {
  const script = fileURLToPath(new URL("./fake-control-plane.mjs", import.meta.url));
  fake = spawn(process.execPath, [script], {
    env: { ...process.env, FAKE_UPSTREAM_PORT: String(FAKE_PORT) },
    stdio: ["ignore", "ignore", "inherit"],
  });
  let up = false;
  for (let i = 0; i < 100 && !up; i++) {
    try {
      up = (await fetch(`${FAKE_BASE}/healthz`)).ok;
    } catch {
      /* not up yet */
    }
    if (!up) await delay(50);
  }
  if (!up) throw new Error(`the fixture did not come up on ${FAKE_BASE}`);
  await seedBothStacks();
});

// --- SEEDING BOTH SIDES (E25 T2, extended by T4) --------------------------------------------------------
//
// The item-shape arm can only compare a collection that has a row on BOTH sides, and a bootstrap stack seeds
// exactly two: projects, api-keys (it seeded organizations too until A.2 Task 6 dropped them). So this sweep
// has been comparing that handful of item shapes and
// PROVING ENVELOPES for the rest — which is honest but thin, and it is why the knowledge-base fixture row
// carried `display_name` (the real projection's field is `name`) for months with nothing to catch it.
//
// The seed creates one knowledge base, because /v1/knowledge-bases is mounted UNCONDITIONALLY (main.go's
// WithKnowledge needs no external key material, unlike secret-refs' master key), which makes the floor below
// a fixed number rather than a stack-dependent one. THE FLOOR RISES WITH EVERY LATER E25 TASK: T3/T4 add
// environments, T7 adds tool revisions, and each seeds its own collection here and raises the assert. A
// baseline that never rises is a baseline nobody notices going stale.
//
// ABSENCE IS A FAILURE, NEVER A SKIP: a seed that cannot be created throws, because a sweep that quietly
// compares fewer collections reports green for exactly the drift it exists to find. The seed is idempotent
// only in the sense that it does not need to be: rows accumulate on a test stack, and one is enough.
// E25 T4 ALSO SEEDS THE FIXTURE, AND ONLY FOR ENVIRONMENTS. Every other fixture collection is a static row;
// the environment surface is STATEFUL there (the console rotates and unbinds, which a static row cannot
// express) and it therefore starts EMPTY, on purpose — that is what lets the browser suite meet a console
// with no environments. So the sweep creates one on each side rather than one, which is also the honest
// symmetry: the shape being compared is the shape a CREATE returns on both.
async function seedBothStacks() {
  const res = await realFetch("/v1/knowledge-bases", {
    method: "POST",
    headers: { "content-type": "application/json" },
    body: JSON.stringify({ name: `conformance-sweep-seed-${Date.now()}` }),
  });
  const body = await res.json().catch(() => ({}));
  assert.ok(
    res.status === 200 || res.status === 201,
    `the sweep could not seed a knowledge base on the real stack: POST /v1/knowledge-bases returned ${res.status} ` +
      `(${body.code ?? "?"}). The item-shape arm needs a row on BOTH sides to compare anything, so a failed seed ` +
      "is a sweep that would pass by comparing nothing. The bootstrap key holds every capability implicitly " +
      "(api/middleware/auth.go HasScope on an empty scope set), so this is a real failure, not a permission gap.",
  );

  // AND IT WRITES ONE VALUE, WHICH SEEDS A SECOND COLLECTION. An environment value IS a secret_refs row
  // under the derived name `env:<id>:<key>`, and secret_refs is EMPTY on a bootstrap stack — so the item arm
  // has never once compared a secret-ref shape, and the fixture's row was missing `updated_at` for months
  // with nothing able to notice. That is not a hypothetical: this seed's first run found it.
  //
  // The value is a SENTINEL, not a credential: it opens nothing, it is never printed, and it is written in a
  // request BODY (never a path, never an argument), which is the same discipline the route itself enforces.
  const name = `sweep-env-${Date.now()}`;
  const create = {
    method: "POST",
    headers: { "content-type": "application/json" },
    body: JSON.stringify({ name, description: "conformance sweep seed" }),
  };
  const write = {
    method: "POST",
    headers: { "content-type": "application/json" },
    body: JSON.stringify({ key: "SWEEP_SEED_TOKEN", value: "conformance-sweep-sentinel-not-a-credential" }),
  };
  for (const [label, doFetch] of [
    ["real", realFetch],
    ["fixture", fakeFetch],
  ]) {
    const created = await doFetch("/v1/environments", create);
    const createdBody = await created.json().catch(() => ({}));
    assert.ok(
      created.status === 200 || created.status === 201,
      `the sweep could not seed an environment on the ${label} side: POST /v1/environments returned ` +
        `${created.status} (${createdBody.code ?? "?"}). The environment routes mount only when a secret master ` +
        "key is configured (main.go WithEnvironments); compose passes PALAI_SECRET_MASTER_KEY_FILE, so an " +
        "unmounted family on the real side means the stack was brought up without it and the item-shape floor " +
        "below would drop silently.",
    );
    const wrote = await doFetch(`/v1/environments/${encodeURIComponent(createdBody.id)}/values`, write);
    const wroteBody = await wrote.json().catch(() => ({}));
    assert.ok(
      wrote.status === 200 || wrote.status === 201,
      `the sweep could not seed an environment VALUE on the ${label} side: POST /v1/environments/{id}/values ` +
        `returned ${wrote.status} (${wroteBody.code ?? "?"}). Without it the secret-refs item shape is compared ` +
        "against an empty real collection, which is a pass over nothing.",
    );
  }

  // E25 T6 SEEDS THREE MORE COLLECTIONS, and one of them needs a REAL ID in the path.
  //
  // repository-bindings and agents are EMPTY on a bootstrap stack — nothing creates either without Slack, and
  // that is the whole reason T6's console pages exist — so their item shapes had never been compared. The
  // agents row was missing `name` on the fixture side for exactly that long.
  //
  // agent-revisions is the interesting one: its pattern carries a wildcard, and arm 3 has always substituted a
  // PROBE token, which on a real stack is an unknown profile and answers an EMPTY page — so the comparison
  // silently did nothing. `seededPathIDs` below is what fixes it, and it took closing DIV-SHP-005 to make the
  // fix reachable at all: while the envelopes differed, arm 3 skipped the item comparison for this route by
  // design.
  for (const [label, doFetch] of [
    ["real", realFetch],
    ["fixture", fakeFetch],
  ]) {
    const binding = await doFetch("/v1/repository-bindings", {
      method: "POST",
      headers: { "content-type": "application/json" },
      body: JSON.stringify({
        provider: "github",
        repository_identity: `palai-example/sweep-seed-${Date.now()}`,
        clone_url: "https://github.com/palai-example/sweep-seed.git",
        default_branch: "main",
        allowed_operations: ["clone"],
      }),
    });
    const bindingBody = await binding.json().catch(() => ({}));
    assert.ok(
      binding.status === 200 || binding.status === 201,
      `the sweep could not seed a repository binding on the ${label} side: POST /v1/repository-bindings ` +
        `returned ${binding.status} (${bindingBody.code ?? "?"}). The route mounts only when the store is wired ` +
        "(api/router.go:42), and an unmounted family means the item-shape floor below would drop silently.",
    );

    const agent = await doFetch("/v1/agents", {
      method: "POST",
      headers: { "content-type": "application/json" },
      body: JSON.stringify({ name: `sweep-seed-agent-${Date.now()}` }),
    });
    const agentBody = await agent.json().catch(() => ({}));
    assert.ok(
      agent.status === 200 || agent.status === 201,
      `the sweep could not seed an agent on the ${label} side: POST /v1/agents returned ${agent.status} ` +
        `(${agentBody.code ?? "?"})`,
    );
    const revision = await doFetch(`/v1/agents/${encodeURIComponent(agentBody.id)}/revisions`, {
      method: "POST",
      headers: { "content-type": "application/json" },
      body: JSON.stringify({ model: SEEDED_REVISION_MODEL }),
    });
    const revisionBody = await revision.json().catch(() => ({}));
    assert.ok(
      revision.status === 200 || revision.status === 201,
      `the sweep could not seed an agent revision on the ${label} side: POST /v1/agents/{id}/revisions ` +
        `returned ${revision.status} (${revisionBody.code ?? "?"}). Without it the revision item shape is ` +
        "compared against an empty page, which is the pass-over-nothing this seed exists to end.",
    );
    // PUBLISHED, because the DIV-UI-007 arm below needs a revision a run can be pinned to — and because a
    // published row is the one whose `status` field the item comparison should be reading.
    const publish = await doFetch(
      `/v1/agents/${encodeURIComponent(agentBody.id)}/revisions/${encodeURIComponent(revisionBody.id)}/publish`,
      { method: "POST", headers: { "content-type": "application/json" }, body: "{}" },
    );
    assert.ok(
      publish.status === 200 || publish.status === 201,
      `the sweep could not publish the seeded revision on the ${label} side: ${publish.status}`,
    );
    await publish.body?.cancel().catch(() => {});

    seeded[label] = { agent_id: agentBody.id, revision_id: revisionBody.id };
  }

  // E25 T7 SEEDS THREE MORE COLLECTIONS: tools, tool-revisions and tool-sets. All three are EMPTY on a
  // bootstrap stack — built-in tools are code-defined and deliberately NOT in the registry
  // (000024_tools.up.sql says so), so nothing puts a row in any of them without an operator — which is why
  // the two E25 T7 read routes had never had a shape compared at all.
  //
  // THE SEED USES THE MANUAL REGISTRY PATH, NOT DISCOVERY, and that is deliberate on BOTH sides: discovery
  // needs an upstream MCP server, a hermetic stack must not dial one, and this seed exists to compare
  // PROJECTIONS rather than to re-prove the discovery chain (which the component tier proves against a real
  // router — see DIV-UI-008). A control_plane revision and an mcp revision have the same projection.
  for (const [label, doFetch] of [
    ["real", realFetch],
    ["fixture", fakeFetch],
  ]) {
    const json = (body) => ({ method: "POST", headers: { "content-type": "application/json" }, body: JSON.stringify(body) });
    const tool = await doFetch("/v1/tools", json({ canonical_name: `sweep.seed.probe${Date.now()}` }));
    const toolBody = await tool.json().catch(() => ({}));
    assert.ok(
      tool.status === 200 || tool.status === 201,
      `the sweep could not seed a tool on the ${label} side: POST /v1/tools returned ${tool.status} ` +
        `(${toolBody.code ?? "?"}). The registry routes mount only when the store is wired (api/router.go), ` +
        "and an unmounted family means the item-shape floor below would drop silently.",
    );
    const revision = await doFetch(`/v1/tools/${encodeURIComponent(toolBody.id)}/revisions`, json({
      executor: "control_plane",
      description: "conformance sweep seed — a registry projection, not a discovered tool",
      input_schema: { type: "object" },
    }));
    const revisionBody = await revision.json().catch(() => ({}));
    assert.ok(
      revision.status === 200 || revision.status === 201,
      `the sweep could not seed a tool revision on the ${label} side: POST /v1/tools/{id}/revisions returned ` +
        `${revision.status} (${revisionBody.code ?? "?"}). Without it GET /v1/tools/{tool_id}/revisions is ` +
        "compared against an empty page on one side, which is the pass-over-nothing this seed exists to end.",
    );
    // PUBLISHED, because a set may pin only published revisions — and because `status` and the two approval
    // columns are fields whose PUBLISHED value is the one worth comparing.
    const publish = await doFetch(
      `/v1/tools/${encodeURIComponent(toolBody.id)}/revisions/${encodeURIComponent(revisionBody.id)}/publish`,
      json({ approval_required: true, approval_label: "conformance sweep seed" }),
    );
    assert.ok(publish.status === 200, `the sweep could not publish the seeded tool revision on the ${label} side: ${publish.status}`);
    await publish.body?.cancel().catch(() => {});

    const setName = "sweep-seed";
    const setRevision = await doFetch(`/v1/tool-sets/${setName}/revisions`, json({ tools: [{ tool_revision_id: revisionBody.id }] }));
    const setBody = await setRevision.json().catch(() => ({}));
    assert.ok(
      setRevision.status === 200 || setRevision.status === 201,
      `the sweep could not seed a tool-set revision on the ${label} side: POST /v1/tool-sets/{set}/revisions ` +
        `returned ${setRevision.status} (${setBody.code ?? "?"})`,
    );
    const publishSet = await doFetch(`/v1/tool-sets/${setName}/revisions/${encodeURIComponent(setBody.id)}/publish`, json({}));
    assert.ok(publishSet.status === 200, `the sweep could not publish the seeded set revision on the ${label} side: ${publishSet.status}`);
    await publishSet.body?.cancel().catch(() => {});

    seeded[label].tool_id = toolBody.id;
    seeded[label].set = setName;
    // SCOPED, because `{revision_id}` already means the AGENT revision above — see seededPath.
    seeded[label]["/v1/tool-sets/{set}/revisions/{revision_id}|revision_id"] = setBody.id;
    // The THIRD meaning of `{revision_id}` in this table, and the one that makes the tool revision's own
    // Location resolvable. Without this scoped key the pattern would inherit the agent revision's id, the
    // real route would correctly 404, and arm 3 would `continue` past it in silence.
    seeded[label]["/v1/tools/{tool_id}/revisions/{revision_id}|revision_id"] = revisionBody.id;
  }

  // E28 T2 SEEDS NO ROW — IT SEEDS AN ID, and that is the whole difference between a compared route and a
  // skipped one. `GET /v1/projects/{project_id}` is the route the policy screen reads its `config_policy`
  // from, and arm 3 substitutes a PROBE token into every wildcard it has no seed for — which on a real stack
  // is an unknown project, answers 404, and makes the arm `continue` in silence. That is the trap E25 T6
  // found on agent revisions, and this route walks straight into it: the bootstrap project ALREADY exists, so
  // there is nothing to create and it would have been easy to assume the comparison was happening.
  //
  // Each side gets ITS OWN id, because the two stacks are different databases. The real one is READ rather
  // than written down: DIV-SHP-002 records that the fixture's project id (`proj_local`) is off by a letter
  // from the one identity/store.go seeds (`prj_local`), which is exactly the kind of constant that goes stale.
  for (const [label, doFetch] of [
    ["real", realFetch],
    ["fixture", fakeFetch],
  ]) {
    const list = await doFetch("/v1/projects");
    const listBody = await list.json().catch(() => ({}));
    const id = listBody.data?.[0]?.id;
    assert.ok(
      typeof id === "string" && id !== "",
      `the sweep could not read a project id on the ${label} side: GET /v1/projects returned ${list.status} ` +
        "with no first row. Every stack has a bootstrap project, so this is a real failure — and without the " +
        "id the project DETAIL route is compared against a probe token, which 404s and passes by comparing " +
        "nothing.",
    );
    seeded[label].project_id = id;
  }

  // E28 T3 SEEDS A POOL ID AND ONE ENROLMENT KEY ON EACH SIDE, and it is one seed for two purposes.
  //
  // The id first, for the same reason the project id above is read rather than written down: every stack has
  // a pool — a tenant is seeded one at birth (`InsertDefaultRunnerPool`) — so there is nothing to create, and
  // `GET /v1/runner-pools/{pool_id}/keys` would otherwise be probed with a token that is not a pool on either
  // side. The fixture's `pool_mac` does not exist on a real stack and the real bootstrap's id is not written
  // anywhere here, so both are READ.
  //
  // Then the key, because that collection is EMPTY on every bootstrap stack: nothing mints a pool key without
  // an operator, so its item shape had never been compared. It is minted and LEFT — not revoked afterwards —
  // because a revoked row carries `revoked_at` and an un-revoked one omits it, so revoking one side's would
  // manufacture the shape difference this arm exists to find. It authenticates nothing that is not already
  // reachable: the caller already holds a `provision` key, which is what mints it.
  for (const [label, doFetch] of [
    ["real", realFetch],
    ["fixture", fakeFetch],
  ]) {
    const pools = await doFetch("/v1/runner-pools");
    const poolBody = await pools.json().catch(() => ({}));
    const poolID = poolBody.data?.[0]?.id;
    assert.ok(
      typeof poolID === "string" && poolID !== "",
      `the sweep could not read a runner pool id on the ${label} side: GET /v1/runner-pools returned ` +
        `${pools.status} with no first row. Every tenant is seeded a default pool at birth, so this is a ` +
        "real failure — and without the id the key route is compared against a probe token, which answers " +
        "an empty list and passes by comparing nothing.",
    );
    seeded[label].pool_id = poolID;

    const key = await doFetch(`/v1/runner-pools/${encodeURIComponent(poolID)}/keys`, {
      method: "POST",
      headers: { "content-type": "application/json" },
      body: "{}",
    });
    const keyBody = await key.json().catch(() => ({}));
    assert.ok(
      key.status === 200 || key.status === 201,
      `the sweep could not mint a pool enrolment key on the ${label} side: ` +
        `POST /v1/runner-pools/{pool_id}/keys returned ${key.status} (${keyBody.code ?? "?"}). Without a row ` +
        "on both sides the key listing's item shape is compared against an empty list, which is the " +
        "pass-over-nothing this seed exists to end.",
    );
    // NOTHING IS RECORDED ABOUT THE VALUE, not even its length: the response carries a live enrolment
    // credential, and a sweep that printed one in an assertion message would be the leak this suite's other
    // arms exist to deny.
  }

  // E25 T8 SEEDS A RUN ON EACH SIDE, which populates TWO collections at once and is why it is one seed
  // rather than two: GET /v1/responses gets its first row, and so does GET /v1/usage/ledger — a run settles
  // `run.admitted` inside the ADMISSION transaction (coordinator/usage.go), so the ledger row exists the
  // moment the create returns and does not wait for the run to finish.
  //
  // THE REAL SIDE REUSES realRun(), which is memoized: four later arms already need a real run, and driving
  // a second one would double the slowest thing this sweep does for nothing. The fixture side is a plain
  // POST — its run history is stateful for the same reason its environment surface is (the console's history
  // screen must be able to open a run the browser suite just drove), so it starts empty and this fills it.
  // Both sides record their ids, so `/v1/responses/{response_id}/artifacts` is compared against a run that
  // EXISTS rather than against the probe id — which 404s on the real side and made that route's envelope
  // uncomparable. It still adds no ITEM comparison (a compose run leaves no artifact behind), and that is
  // itself worth having measured rather than assumed.
  const real = await realRun();
  seeded.real.response_id = real.responseID;
  seeded.real.session_id = real.sessionID;
  const fixtureRun = await fakeFetch("/v1/responses", {
    method: "POST",
    headers: { "content-type": "application/json", "idempotency-key": `sweep-${Date.now()}` },
    body: JSON.stringify({ input: "E25 T8 conformance sweep — seed one run so history and the ledger have a row" }),
  });
  const fixtureRunBody = await fixtureRun.json().catch(() => ({}));
  assert.ok(
    fixtureRun.status === 200 || fixtureRun.status === 202,
    `the sweep could not seed a run on the fixture side: POST /v1/responses returned ${fixtureRun.status}`,
  );
  seeded.fixture.response_id = fixtureRunBody.id;
  seeded.fixture.session_id = fixtureRunBody.session_id;
}
after(() => fake?.kill());

describe("fake-vs-real conformance sweep (D15)", { concurrency: 1 }, () => {
  it("the real control plane is reachable and authenticates the sweep's bearer", async () => {
    const res = await realFetch("/v1/capabilities");
    await res.body?.cancel().catch(() => {});
    assert.equal(
      res.status,
      200,
      `GET /v1/capabilities on the real stack returned ${res.status}. The sweep cannot gather a real surface ` +
        "without an authenticated running stack (check PALAI_BASE_URL / PALAI_API_KEY — the key is never printed)",
    );
  });

  // ARM 1 — the fixture's table IS its surface. Without this, ROUTES could describe routes the fixture does
  // not serve, and every later arm would be auditing a document instead of a server.
  //
  // IT ASKS THE DISPATCHER DIRECTLY NOW, and the change is a STRENGTHENING rather than a relaxation.
  //
  // This arm asserted `status !== 404`, which is a PROXY for "a row matched" and is wrong in one direction:
  // a handler that IS dispatched and answers a legitimate application 404 — a write to a sub-resource of an
  // id that does not exist, which is most of the id-bearing write patterns in the table — emits exactly the
  // same `sendProblem(404, "not_found")` as a pattern nothing routes at all. So the arm failed on correctly
  // served routes, and because it asserts inside a loop it named only the FIRST: on `main` that was
  // `PUT /v1/repository-bindings/{binding_id}/connection`, and adding the model-route write path only
  // changed which one got named. Two real instances, one visible, and the invisible one would have come
  // back the moment the other was fixed.
  //
  // `x-fixture-dispatched` carries the PATTERN the dispatcher matched, so this now checks the claim itself,
  // and checks something the old form could not: that the row which matched is the row being probed. A
  // pattern in the table that no compiled row serves emits no header and still fails — which is the defect
  // this arm exists for, and it is now the ONLY thing that fails it.
  it("every row of the fixture's route table is genuinely served by the fixture", async () => {
    for (const route of ROUTES) {
      const res = await fakeFetch(concretePath(route.pattern), {
        method: route.method,
        body: route.method === "POST" ? "{}" : undefined,
      });
      const dispatched = res.headers.get("x-fixture-dispatched");
      await res.body?.cancel().catch(() => {});
      assert.equal(
        dispatched,
        route.pattern,
        `the table declares ${route.method} ${route.pattern} but the fixture dispatched ` +
          `${dispatched === null ? "NOTHING" : dispatched} for it — the table is not the dispatcher, so nothing ` +
          "this sweep concludes about the fixture is trustworthy",
      );
    }
  });

  // ARM 2 — ROUTE SURFACE. A route the fixture serves that the real router does not register is the
  // invented-event class in its HTTP form. FAILS unless the ledger already carries it.
  it("every (method, path-pattern) the fixture serves is registered by the RUNNING real router", async () => {
    for (const route of ROUTES) {
      const subject = `${route.method} ${route.pattern}`;
      const probe = await allowProbe(REAL_BASE, route.pattern, REAL_KEY);
      if (!probe.registered) {
        requireLedgerRow("route", subject, "the real router 404s this path with NO Allow header — no pattern is registered for it");
        continue;
      }
      if (!probe.methods.includes(route.method)) {
        requireLedgerRow(
          "route",
          subject,
          `the real router serves this path but not this method (Allow: ${probe.methods.join(", ") || "<none>"})`,
        );
      }
    }
  });

  // ARM 3 — RESPONSE SHAPE on the routes both serve.
  //
  // ENVELOPE and ITEM are compared separately, and the distinction is not cosmetic. A list ENVELOPE is a
  // contract fact that holds even over zero rows, so it is always compared. An ITEM shape can only be
  // compared when BOTH sides actually have a row: a fresh bootstrap stack has no model connections, no
  // routes and no knowledge bases, and folding "the real list is empty" into "the shapes disagree" would
  // produce a false divergence for every unpopulated collection — which then has to be excused in the
  // ledger, which is how a ledger stops meaning anything. The honest consequence is stated rather than
  // hidden: on a fresh stack this arm proves ENVELOPES everywhere and ITEMS only for the three collections
  // the bootstrap seeds (projects, api-keys).
  it("the JSON shapes of the routes both serve agree, or the difference is on the ledger", async () => {
    // Only GETs that need no prior state, so the comparison is about SHAPE and never about test setup.
    const comparable = ROUTES.filter((r) => r.method === "GET" && !r.pattern.endsWith("/events") && !r.pattern.endsWith("/content"));
    assert.ok(comparable.length >= 10, `expected the fixture to serve the console's read surfaces, got ${comparable.length}`);

    let itemsCompared = 0;
    // WHICH collections were compared, printed rather than counted (E25 T6). A bare number told nobody which
    // one had dropped out, and T6 spent a sweep run discovering by elimination that GET /v1/agents is not on
    // this list and structurally cannot be (DIV-SHP-004). A floor whose members are invisible is a floor
    // nobody can raise deliberately.
    const comparedSubjects = [];
    for (const route of comparable) {
      const subject = `${route.method} ${route.pattern}`;
      // SEEDED IDS, NOT A PROBE, for the patterns a seed created a row under (E25 T6). Arms 1 and 2 keep the
      // probe deliberately — they ask whether a PATTERN is registered, and a real id would not test that — but
      // an item comparison over `/v1/agents/zzsweepprobe/revisions` is a comparison against an empty page on
      // the real side, which is how the fixture's revision shape stayed wrong. Each side gets ITS OWN id: the
      // two stacks are different databases and neither knows the other's rows.
      const [path, fakePath] = [seededPath(route.pattern, "real"), seededPath(route.pattern, "fixture")];
      const [realRes, fakeRes] = await Promise.all([realFetch(path), fakeFetch(fakePath)]);
      if (realRes.status !== 200) {
        // Arm 2 already established whether the pattern exists; a non-200 here is an unmounted capability or
        // an unknown probe id, neither of which is a shape fact.
        await realRes.body?.cancel().catch(() => {});
        await fakeRes.body?.cancel().catch(() => {});
        continue;
      }
      const [real, fk] = [await realRes.json(), await fakeRes.json()];

      const envelope = (v) => Object.keys(v ?? {}).sort().join(",");
      if (envelope(real) !== envelope(fk)) {
        requireLedgerRow("shape", subject, `list envelope: real {${envelope(real)}} vs fixture {${envelope(fk)}}`);
        continue; // an envelope difference subsumes the item comparison for this route
      }
      const [realItem, fakeItem] = [real.data?.[0], fk.data?.[0]];
      if (realItem === undefined || fakeItem === undefined) continue; // no row on one side — nothing to compare
      itemsCompared += 1;
      comparedSubjects.push(subject);
      if (keyShape(realItem) !== keyShape(fakeItem)) {
        requireLedgerRow("shape", subject, `list item: real ${keyShape(realItem)}\n  fixture ${keyShape(fakeItem)}`);
      }
    }
    // eslint-disable-next-line no-console -- the membership IS the evidence; a bare count hides which one left.
    console.log(`ITEM SHAPES COMPARED — ${itemsCompared}: ${comparedSubjects.join(", ")}`);

    // THE FLOOR, AND IT RISES EVERY TASK (E25 T2, raised by T4 and again by T6). It was 3 — the three
    // collections a bootstrap stack seeds — then 4 once this sweep began seeding a knowledge base, then 6 when
    // T4's seed created an ENVIRONMENT and wrote one VALUE into it (a SECRET_REFS row), and it is now 8.
    //
    // T6 SEEDED THREE COLLECTIONS AND THE FLOOR ROSE BY TWO, and that gap is the measurement rather than a
    // shortfall. The seeds are a REPOSITORY BINDING, an AGENT and a published AGENT REVISION — three
    // collections empty on every bootstrap stack, because nothing without Slack creates any of them. Two
    // became comparable; `GET /v1/agents` did NOT, and structurally cannot: the fixture's agents collection
    // must exceed one page for truncation to be observable at all (DIV-UI-005/DIV-SHP-004), so its first page
    // carries a minted `next_cursor` the real stack's short list omits, and an envelope difference SUBSUMES
    // the item comparison. The eight that must hold (organizations was among them until A.2 Task 6):
    // projects, api-keys, knowledge-bases,
    // environments, secret-refs, repository-bindings, agent-revisions. T7 seeds tool revisions and raises it
    // again; it must never fall.
    //
    // WHAT THE T6 RAISE FOUND, because a floor that rises is only worth the drift it catches: the fixture's
    // agent row was missing `name`; its REVISION row said `published: true` where the real projection says
    // `status: "published"` and carried none of agent_id / revision_number / mcp_connections / environment /
    // instructions; and it served `tools: []` / `mcp_connections: []` where a revision naming neither has
    // `null` on the real wire (store/agents.go marshals a Go nil slice). All three were invisible for the same
    // reason — DIV-SHP-005 recorded an envelope difference on that route — so closing that row was part of
    // raising this number rather than a separate tidy-up.
    //
    // E25 T5 DID NOT RAISE IT, AND THAT IS A MEASUREMENT RATHER THAN AN OMISSION (§2 says every page-opening task
    // seeds its collection and raises this number). GET /v1/approvals is comparable here and its ENVELOPE is
    // compared — both sides answer {data, has_more} — but its ITEM shape cannot be, because the real side cannot
    // be seeded: a row in `tool_approvals` is created only when a gated tool call PARKS, no /v1 route parks one
    // (E23 T9's surface reads the queue and decides on it), and a compose run reaches no tool call at all
    // (DIV-UNX-001: the fake adapter is hardcoded with no ToolCalls and no env knob). So the honest floor for T5
    // is the one below, unchanged, with the reason written down — and the first task that can seed a row on the
    // real side raises it. A baseline nudged up by a seed that does not exist would be worse than one that holds.
    //
    // E25 T7 RAISES IT TO 11. Its seed creates a TOOL, a published TOOL REVISION and a published TOOL-SET
    // REVISION — three collections empty on every bootstrap stack, because built-in tools are code-defined
    // and deliberately absent from the registry (000024_tools.up.sql). All three become comparable, and one
    // of them is a route this epic just wrote: GET /v1/tools/{tool_id}/revisions had no shape to compare
    // because it did not exist. GET /v1/tool-sets/{set}/revisions/{revision_id} is compared too, but as an
    // ENVELOPE rather than an item — it is a single resource, not a page, so `data[0]` is undefined and it
    // does not raise this count. Its whole key set is still diffed, which for a detail projection is the
    // stronger of the two checks. The eleven: projects, api-keys, knowledge-bases,
    // environments, secret-refs, repository-bindings, agent-revisions, tools, tool-revisions, tool-sets.
    //
    // E25 T8 RAISES IT TO 13, AND T7's ELEVEN HAD NEVER ACTUALLY BEEN REACHED. T7 wrote the number above
    // from the seed it added, but that seed asserted on `POST /v1/tools returned 404` — the fixture served
    // no manual registry write path at all — so `pnpm sweep` failed in seedBothStacks from the moment T7
    // landed and not one arm of it ran. The three collections T7 counted became comparable only once the
    // fixture gained those two routes (tests/fake-control-plane.mjs says what was missing and why nothing
    // caught it). The number was therefore aspirational when it was written and is measured now.
    //
    // T8's OWN TWO are GET /v1/responses and GET /v1/usage/ledger, from one seed rather than two: a run
    // settles `run.admitted` inside the ADMISSION transaction (coordinator/usage.go), so creating a run puts
    // a row in both collections at once. Both are empty on a bootstrap stack — a console that never started
    // a run leaves no response and therefore no ledger entry — which is why the two routes O1 and O4 are
    // built on had never had an item shape compared. GET /v1/usage does NOT raise it and cannot: it answers
    // totals under `meters`, not a `data` page, so it has no item to compare. The thirteen: the eleven above
    // plus responses and usage-ledger.
    // E28 T3 RAISES IT TO 16, AND THE NUMBER ABOVE WAS TWO SHORT BEFORE IT ADDED ANYTHING.
    //
    // MEASURED, and the measurement corrected two guesses in one run (2026-07-31, real compose stack):
    //
    //   runner-pools    — was ALREADY comparable and had been all along, because every tenant is seeded a
    //                     default pool at birth. The thirteen the paragraphs above enumerate never named it,
    //                     so that enumeration was stale by one and the assertion under it was passing with
    //                     room to spare. This is why the membership is PRINTED: a bare count could not have
    //                     said so, and reading it is what found this.
    //   runner-pool-keys — genuinely new, and the seed above is what makes it comparable: the collection is
    //                     empty on every bootstrap stack, since nothing mints a pool key without an operator.
    //   runners         — ALSO already comparable, and the reason this comment does not say otherwise is that
    //                     a first draft of DIV-UI-009 did. It claimed a compose stack enrols no machines,
    //                     reasoning from the runner plane being a listener a host agent dials. deploy/compose
    //                     starts a `runner` SERVICE and `palai local up` mints it a token: the real side has
    //                     one active machine, and this arm compares its row. A ledger is worth exactly the
    //                     truth of its lines, and this is the line the sweep corrected.
    //
    // THE FLOOR FELL FROM 16 TO 15, AND A FALLING FLOOR NEEDS A REASON OR IT IS JUST A LOWERED BAR. The
    // reason is not a seed that stopped landing — it is that `organizations` STOPPED EXISTING. A.2 Task 6
    // unmounted GET /v1/organizations on the real stack and this pass removed it from the fixture, so there
    // is no collection left to compare rather than a comparison that is being skipped. Every other member
    // is untouched, and the rule stands for them: it must never fall again without a line like this one.
    //
    // The fifteen, as the run prints them: projects, api-keys, runner-pools, runner-pool-keys,
    // runners, secret-refs, knowledge-bases, agent-revisions, tools, tool-revisions, tool-sets,
    // repository-bindings, environments, responses, usage-ledger.
    assert.ok(
      itemsCompared >= 15,
      `only ${itemsCompared} collections had a row on BOTH sides (${comparedSubjects.join(", ")}), so this arm ` +
        "compared almost no item shapes — the bootstrap seeds projects/api-keys and this sweep " +
        "seeds a knowledge base, an environment, one environment value (which is a secret_refs row), a " +
        "repository binding, an agent, a published agent revision, a tool, a published tool revision, a " +
        "published tool-set revision, a RUN (which settles a usage ledger row in the same transaction) and a " +
        "POOL ENROLMENT KEY — while the bootstrap ALSO seeds a runner pool and `palai local up` enrols one " +
        "MACHINE. Fifteen of those sixteen can be compared; GET /v1/agents cannot, because its envelope " +
        "differs irreducibly (see DIV-SHP-004). Fewer than fifteen means either the real stack is not seeded " +
        "or a seed did not land, and this arm would pass vacuously",
    );
  });

  // The one read that needs a real id: the terminal Response projection the console retrieves after the
  // stream ends. Compared against the fixture's, which is the shape the console was written to.
  it("the terminal Response projection has the same shape on both", async () => {
    const { responseID } = await realRun();
    const [realRes, fakeRes] = await Promise.all([
      realFetch(`/v1/responses/${encodeURIComponent(responseID)}`),
      fakeFetch("/v1/responses/resp_console_0001"),
    ]);
    assert.equal(realRes.status, 200, `GET /v1/responses/{id} on the real stack returned ${realRes.status}`);
    const [real, fk] = [await realRes.json(), await fakeRes.json()];
    if (keyShape(real) !== keyShape(fk)) {
      requireLedgerRow("shape", "GET /v1/responses/{response_id}", `real ${keyShape(real)}\n  fixture ${keyShape(fk)}`);
    }
  });

  // ARM 4 — STATUS and command semantics. The same request must be answered the same way, or the fixture is
  // teaching the console a contract the real API does not honour.
  it("POST /v1/responses without an Idempotency-Key is answered the same way by both", async () => {
    const init = { method: "POST", headers: { "content-type": "application/json" }, body: JSON.stringify({ input: "sweep — idempotency-key probe" }) };
    const [real, fk] = await Promise.all([realFetch("/v1/responses", init), fakeFetch("/v1/responses", init)]);
    const [realBody, fakeBody] = [await real.json().catch(() => ({})), await fk.json().catch(() => ({}))];
    if (real.status !== fk.status) {
      requireLedgerRow("status", "POST /v1/responses", `real ${real.status} (${realBody.code ?? "?"}) vs fixture ${fk.status}`);
    }
  });

  it("an approve command is projected the same way by both", async () => {
    const { sessionID } = await realRun();
    const init = {
      method: "POST",
      headers: { "content-type": "application/json" },
      body: JSON.stringify({ command_id: `cmd_sweep_${Date.now()}`, kind: "approve" }),
    };
    const [real, fk] = await Promise.all([
      realFetch(`/v1/sessions/${encodeURIComponent(sessionID)}/commands`, init),
      fakeFetch("/v1/sessions/ses_console_0001/commands", init),
    ]);
    const [realBody, fakeBody] = [await real.json(), await fk.json()];
    if (keyShape(realBody) !== keyShape(fakeBody)) {
      requireLedgerRow(
        "shape",
        "POST /v1/sessions/{session_id}/commands",
        `real ${keyShape(realBody)} status=${realBody.status}\n  fixture ${keyShape(fakeBody)} status=${fakeBody.status}`,
      );
    }
  });

  // ARM 5 — THE EVENT STREAM. This is where E17 T10's invented approval event lived, and it is the arm with
  // the most to say: it drives a REAL run on the REAL stack to terminal, reads the types and `data` key sets
  // it actually journals, and diffs them against the fixture's scripted stream.
  it("the fixture's scripted event vocabulary and payload keys match a REAL run's journal", async () => {
    const { events } = await realRun();
    assert.ok(
      events.size > 0,
      "a real run journaled NO events — the stack is not dispatching. PALAI_DISPATCH_WORKERS=1 and " +
        "PALAI_MODEL_PROVIDER=fake must be set BEFORE `palai local up`; the compose default is queued-only, " +
        "and a queued-only stack would let this whole arm pass vacuously",
    );

    // The fixture's vocabulary, folded to type -> the union of its data keys across all scripted rows.
    const scripted = new Map();
    for (const [type, data] of SCRIPTED_EVENTS) {
      scripted.set(type, new Set([...(scripted.get(type) ?? []), ...Object.keys(data)]));
    }

    for (const [type, fixtureKeys] of scripted) {
      const realKeys = events.get(type);
      if (realKeys === undefined) {
        // Absent from a real run. Which KIND of absence it is gets re-derived, never assumed.
        const sites = journaledByProductionCode(type);
        const kind = sites.length === 0 ? "event" : "unexercised";
        const why =
          sites.length === 0
            ? "and NO production Go file journals this literal — the fixture invented it (the E17 T10 class)"
            : `but it IS journaled by ${sites.join(", ")} — a real event this tier cannot reach`;
        // Look the row up by SUBJECT across BOTH absence kinds, then check the kind separately. Looking it
        // up by (kind, subject) would make the classification check unreachable — the lookup would already
        // have enforced it, and a misfiled row would be reported as "unrecorded", which is the wrong
        // diagnosis for the wrong reason. A misclassification deserves to say so: an invention filed as a
        // coverage gap is precisely how five real ones stayed hidden behind one known instance.
        const row = DIVERGENCES.find((d) => (d.kind === "event" || d.kind === "unexercised") && d.subject === type);
        assert.ok(
          row !== undefined,
          `UNRECORDED FAKE-VS-REAL DIVERGENCE [${kind}] ${type}\n  scripted by the fixture, absent from a ` +
            `real run's stream, ${why}\n\nEvery difference must be written down in tests/divergences.mjs.`,
        );
        assert.equal(
          row.kind,
          kind,
          `${type} is filed in the ledger as "${row.kind}" but the tree says "${kind}". Production journaling ` +
            `sites = [${sites.join(", ") || "NONE"}]. "event" means the fixture INVENTED the name; ` +
            '"unexercised" means the name is real and this tier cannot reach it. Filing an invention as a ' +
            "coverage gap is how the E17 T10 class hides.",
        );
        observed.add(row.id);
        continue;
      }
      const fixtureOnly = [...fixtureKeys].filter((k) => !realKeys.has(k)).sort();
      const realOnly = [...realKeys].filter((k) => !fixtureKeys.has(k)).sort();
      if (fixtureOnly.length > 0 || realOnly.length > 0) {
        requireLedgerRow(
          "data",
          type,
          `fixture-only keys [${fixtureOnly.join(", ") || "-"}], real-only keys [${realOnly.join(", ") || "-"}]`,
        );
      }
    }
  });

  // ARM 6 — the mechanical evidence behind the ui rows, so a spec skipped on the real profile is skipped for
  // a reason this sweep re-derives rather than a reason someone typed.
  it("the ui-row evidence holds: no approval on a real run, honestly two lanes, no hostile artifact, no upstream introspection", async () => {
    const { events } = await realRun();
    const types = [...events.keys()].sort();

    assert.ok(
      !types.some((t) => t.startsWith("approval.")),
      `a real compose run DID journal an approval event (${types.join(", ")}) — DIV-UI-001 is stale and the ` +
        "approval specs must be un-skipped on the real profile",
    );
    requireLedgerRow("ui", "the approval journey (UI-002 authoritative detail, approve, deny)", "a real compose run reaches terminal with no approval.* event");

    assert.ok(
      !types.some((t) => t.startsWith("usage.") || t.startsWith("artifact.") || t.startsWith("tool_call.") || t.startsWith("attempt.")),
      `a real compose run journaled a tool/recovery/usage/artifact event (${types.join(", ")}) — DIV-UI-002 is stale`,
    );
    requireLedgerRow(
      "ui",
      "the lane-separated timeline, recovery lane, live usage and mid-stream artifact reveal",
      `a real run journals only [${types.join(", ")}]`,
    );

    const evil = await realFetch("/v1/artifacts/art_evil/content");
    await evil.body?.cancel().catch(() => {});
    assert.equal(evil.status, 404, `the real stack served /v1/artifacts/art_evil/content with ${evil.status} — DIV-UI-003 is stale`);
    requireLedgerRow("ui", "the hostile-artifact download hardening suite", "the hostile artifact fixture does not exist on a real stack");

    const introspect = await realFetch("/__introspect");
    await introspect.body?.cancel().catch(() => {});
    assert.equal(introspect.status, 404, `the real stack served /__introspect with ${introspect.status} — DIV-UI-004 is stale`);
    requireLedgerRow("ui", "the upstream half of the public-API-only proof (/__introspect)", "no upstream introspection on a real control plane");

    // DIV-UI-005 (E25 T2): the truncation proof needs a collection LARGER than one page, and a bootstrap stack
    // has none. Re-derived from the envelope the real API actually returns rather than assumed: has_more false
    // means there is no second page and therefore nothing for a "load more" control to fetch. If a real
    // collection ever does exceed a page, this fails and tests/pagination.spec.ts must stop skipping on real.
    const agents = await realFetch("/v1/agents");
    assert.equal(agents.status, 200, `GET /v1/agents on the real stack returned ${agents.status}`);
    const agentsPage = await agents.json();
    assert.equal(
      agentsPage.has_more,
      false,
      `the real agents collection reports has_more=${agentsPage.has_more} — it EXCEEDS one page, so DIV-UI-005 is ` +
        "stale and the list-truncation spec can run on the real profile",
    );
    requireLedgerRow(
      "ui",
      "the list-truncation proof over a collection larger than one page",
      `a real stack's agents list holds ${agentsPage.data?.length ?? 0} row(s) with has_more=false — no page is ever truncated`,
    );

    // DIV-UI-006 (E25 T5): the tool-approval QUEUE renders on the real profile — the route is mounted
    // unconditionally and answers 200 — but it cannot hold a ROW, because a tool approval is created only when a
    // gated tool call parks and nothing on a compose stack can park one. Re-derived, not assumed, and derived
    // AFTER a real run has reached terminal: if a real run ever does park a gated call, this fails, the row is
    // stale, and the queue's decision legs must stop skipping on the real profile.
    const approvals = await realFetch("/v1/approvals");
    assert.equal(
      approvals.status,
      200,
      `GET /v1/approvals on the real stack returned ${approvals.status}. It is mounted UNCONDITIONALLY (main.go ` +
        "api.WithApprovals(repo)) and the bootstrap key holds the `approve` capability implicitly (an empty scope " +
        "set satisfies Scope.HasScope), so anything else means the console's approval screen has no real upstream " +
        "and DIV-UI-006 understates the problem",
    );
    const queue = await approvals.json();
    assert.deepEqual(
      queue.data ?? [],
      [],
      `the real tool-approval queue holds ${queue.data?.length ?? 0} row(s) — a compose run PARKED a gated call, so ` +
        "DIV-UI-006 is stale and tests/approval-queue.spec.ts must stop skipping its decision legs on the real profile",
    );
    requireLedgerRow(
      "ui",
      "the tool-approval queue over a PARKED gated tool call",
      `GET /v1/approvals answers 200 with ${queue.data?.length ?? 0} row(s) after a real run reached terminal, and the ` +
        `envelope is {${Object.keys(queue).sort().join(",")}} — the screen is real on this profile, the rows cannot be`,
    );

    // DIV-UI-008 (E25 T7): the tools screen RENDERS on the real profile — its panels, its pickers and its
    // ceiling notes are all real there, and this sweep's own seed proves both new read routes answer. What
    // cannot exist is a DISCOVERED tool revision, because discovery dials an upstream MCP server and a
    // hermetic stack has none and must not acquire one. Re-derived from the connection collection rather
    // than from prose: it is EMPTY, and nothing on a bootstrap stack fills it — `palai up` registers no
    // connection, and SLACK_AGENT_MCP names one an operator already made. If a compose stack ever ships with
    // a connection registered, this fails, the row is stale, and the discovery legs must stop skipping.
    const conns = await realFetch("/v1/mcp-connections");
    assert.equal(conns.status, 200, `GET /v1/mcp-connections on the real stack returned ${conns.status} — the family is unmounted, which DIV-UI-008 does not claim`);
    const connPage = await conns.json();
    assert.deepEqual(
      connPage.data ?? [],
      [],
      `the real stack holds ${connPage.data?.length ?? 0} MCP connection(s) — something on a bootstrap stack now ` +
        "registers one, so DIV-UI-008 is stale and tests/mcp-tools.spec.ts must stop skipping its discovery legs",
    );
    requireLedgerRow(
      "ui",
      "registering an MCP connection, DISCOVERING it, and approving what it found",
      `GET /v1/mcp-connections answers 200 with ${connPage.data?.length ?? 0} row(s) — there is nothing to discover ` +
        "on a hermetic stack, while both E25 T7 read routes are seeded and compared by this same sweep",
    );

    // DIV-UI-009 (E28 T3): the fleet screen RENDERS on the real profile with a REAL machine in it — compose
    // starts a `runner` service and `palai local up` mints it a token — and this arm is why that row says so.
    // Its first draft claimed a compose stack enrols no machines at all; the first run of this sweep answered
    // one row and the claim died. What is genuinely unreachable there is narrower: a SECOND machine, a
    // `pending` one, and a non-zero live count. Each is re-derived, so a stack that ever grows a second runner
    // or a strict pool at boot fails here rather than leaving three skips standing on a stale sentence.
    const fleet = await realFetch("/v1/runners");
    assert.equal(fleet.status, 200, `GET /v1/runners on the real stack returned ${fleet.status} — the registry is unmounted, which DIV-UI-009 does not claim`);
    const machines = (await fleet.json()).data ?? [];
    assert.ok(
      machines.length < 2,
      `the real stack holds ${machines.length} machines — the concurrency notice's condition (two or more ` +
        "enrolled) is now reachable there, so DIV-UI-009 is stale and tests/fleet.spec.ts must stop skipping it",
    );
    const pending = machines.filter((m) => m.state === "pending");
    assert.deepEqual(
      pending.map((m) => m.id),
      [],
      `the real stack holds ${pending.length} PENDING machine(s) — a waiting room can be populated there, so ` +
        "DIV-UI-009 is stale and the admission legs must stop skipping on the real profile",
    );
    // THE LIVE COUNTS ARE ZERO ON AN IDLE STACK, and that is the third unreachable state — read off the
    // machine's own single read and the pool page rather than reasoned about. Note what is NOT asserted here:
    // that no pool is strict. tests/fleet.spec.ts CREATES strict pools on this same stack, so a sweep run
    // after a real-profile browser run would fail on its own leftovers — an assertion a sibling suite can
    // invalidate is a false alarm waiting to happen, and the operative fact is the absence of a PENDING
    // MACHINE, which is asserted above and which no console action can produce.
    const realPools = (await (await realFetch("/v1/runner-pools")).json()).data ?? [];
    const queued = realPools.filter((p) => typeof p.waiting === "number" && p.waiting > 0);
    assert.deepEqual(
      queued.map((p) => p.id),
      [],
      `a real pool reports a non-zero queue depth (${queued.map((p) => `${p.id}=${p.waiting}`).join(", ")}), so the ` +
        "non-zero rendering is reachable on the real profile and DIV-UI-009 is stale",
    );
    requireLedgerRow(
      "ui",
      "a fleet with a SECOND machine, a machine WAITING to be admitted, and a non-zero live count",
      `GET /v1/runners answers 200 with ${machines.length} row(s), state(s) {${[...new Set(machines.map((m) => m.state))].join(",")}}, ` +
        `over ${realPools.length} pool(s) whose queue depths are all zero — the screen and its machine table are ` +
        "real on this profile; a second machine, a pending one and a non-zero live count are not",
    );
  });

  // DIV-UI-007 (E25 T6) — THE PIN TRAVELS, AND ITS MODEL CANNOT BE SEEN.
  //
  // Its own arm, because it needs a SECOND real run and a pinned one. Both halves are re-derived from that run
  // rather than from the row's prose: the DRAFT refusal proves the field reaches admission at all on this
  // profile (a 409 can only come from the server resolving the id), and the terminal projection's model proves
  // the observation is impossible here (main.go's fake adapter answers with a constant). If a compose stack
  // ever echoes the model it was asked for, the second assertion fails, the row is stale, and the crown leg in
  // tests/config-journey.spec.ts must stop skipping on the real profile.
  it("the ui-007 evidence holds: a pinned revision reaches admission, and its model cannot be observed on compose", async () => {
    const { agent_id: agentID, revision_id: revisionID } = seeded.real;
    assert.ok(
      typeof revisionID === "string" && revisionID !== "",
      "the seed created no agent revision on the real side, so this arm would prove nothing",
    );

    // A DRAFT is created fresh (the seeded one is published) so the 409 is about a draft rather than about a
    // re-publish.
    const draft = await realFetch(`/v1/agents/${encodeURIComponent(agentID)}/revisions`, {
      method: "POST",
      headers: { "content-type": "application/json" },
      body: JSON.stringify({ model: SEEDED_REVISION_MODEL }),
    });
    const draftBody = await draft.json();
    assert.ok(draft.status === 200 || draft.status === 201, `seeding a draft revision returned ${draft.status}`);
    const refused = await realFetch("/v1/responses", {
      method: "POST",
      headers: { "content-type": "application/json", "idempotency-key": `sweep-draft-${Date.now()}` },
      body: JSON.stringify({ input: "sweep — a draft pin must be refused", agent_revision_id: draftBody.id }),
    });
    const refusal = await refused.json().catch(() => ({}));
    assert.equal(
      refused.status,
      409,
      `POST /v1/responses pinned to a DRAFT revision returned ${refused.status} (${refusal.code ?? "?"}), want 409 ` +
        "revision_not_published. This is the half of the pin that DOES run on the real profile, and if it stopped " +
        "working the console's honest-refusal leg would be asserting a fixture behaviour",
    );
    assert.equal(refusal.code, "revision_not_published", `the draft refusal's code is ${refusal.code}`);

    // AND THE PUBLISHED PIN RUNS — to terminal, with a model that is NOT the one the revision names.
    const created = await realFetch("/v1/responses", {
      method: "POST",
      headers: { "content-type": "application/json", "idempotency-key": `sweep-pinned-${Date.now()}` },
      body: JSON.stringify({ input: "sweep — a published pin must run", agent_revision_id: revisionID }),
    });
    assert.ok(created.status === 200 || created.status === 202, `a pinned admission returned ${created.status}`);
    const run = await created.json();

    // Read the canonical stream to its close, which is the run's terminal — the same idiom realRun uses.
    const stream = await realFetch(`/v1/sessions/${encodeURIComponent(run.session_id)}/events`, {
      headers: { accept: "text/event-stream" },
      signal: AbortSignal.timeout(RUN_BUDGET_MS),
    });
    assert.equal(stream.status, 200, `the pinned run's event stream returned ${stream.status}`);
    const reader = stream.body.getReader();
    try {
      for (;;) {
        const { done } = await reader.read();
        if (done) break;
      }
    } finally {
      await reader.cancel().catch(() => {});
    }

    const terminal = await realFetch(`/v1/responses/${encodeURIComponent(run.id)}`);
    const projection = await terminal.json();
    assert.equal(terminal.status, 200, `retrieving the pinned run returned ${terminal.status}`);
    assert.notEqual(
      projection.model,
      SEEDED_REVISION_MODEL,
      `the pinned run's terminal projection reports model=${projection.model}, which IS the revision's model — the ` +
        "compose adapter has started echoing the model it was asked for, DIV-UI-007 is stale, and the crown leg of " +
        "tests/config-journey.spec.ts must stop skipping on the real profile",
    );
    requireLedgerRow(
      "ui",
      "a run pinned to a published agent revision reporting THAT revision's model",
      `a real run pinned to a revision naming ${SEEDED_REVISION_MODEL} reached ${projection.status} reporting ` +
        `model=${projection.model} — while the DRAFT pin was refused ${refused.status} ${refusal.code}, so the field ` +
        "does reach admission and only its observable effect is missing",
    );
  });

  // THE STALENESS DIRECTION. A ledger that only grows is a ledger nobody trusts: a row whose divergence has
  // been CLOSED must fail, or the fixture stays permanently excused for something it no longer does.
  it("no ledger row has gone stale — every row was re-observed against the running real stack", () => {
    const unobserved = DIVERGENCES.filter((d) => !observed.has(d.id)).map((d) => `${d.id} [${d.kind}] ${d.subject}`);
    assert.deepEqual(
      unobserved,
      [],
      "these divergence-ledger rows were NOT re-observed against the running real stack. Either the difference " +
        "has been CLOSED — delete the row, and un-skip whatever cites it — or the sweep no longer checks it, " +
        "which is worse, because the row is now decoration:\n  " + unobserved.join("\n  "),
    );
  });
});

// realRun creates one real run, reads its canonical stream to terminal, and returns the ids plus
// type -> set of observed `data` keys. Memoized: four arms need it and a compose run is not cheap.
let cached;
function realRun() {
  cached ??= (async () => {
    const create = await realFetch("/v1/responses", {
      method: "POST",
      headers: { "content-type": "application/json", "idempotency-key": `sweep-${Date.now()}` },
      body: JSON.stringify({ input: "E19 T7 conformance sweep — drive a real run to terminal" }),
    });
    assert.ok(create.status === 200 || create.status === 202, `POST /v1/responses returned ${create.status}`);
    const created = await create.json();
    assert.ok(created.session_id, `the real create returned no session_id (keys: ${Object.keys(created).sort().join(", ")})`);

    const stream = await realFetch(`/v1/sessions/${encodeURIComponent(created.session_id)}/events`, {
      headers: { accept: "text/event-stream" },
      signal: AbortSignal.timeout(RUN_BUDGET_MS),
    });
    assert.equal(stream.status, 200, `GET /v1/sessions/{id}/events returned ${stream.status}`);

    // The canonical stream replays from sequence 0 and the server CLOSES it on the run's terminal event, so
    // reading to end-of-body is reading the whole journal — no polling, no arbitrary cutoff.
    const events = new Map();
    const reader = stream.body.getReader();
    const decoder = new TextDecoder();
    let buffer = "";
    try {
      for (;;) {
        const { done, value } = await reader.read();
        if (done) break;
        buffer += decoder.decode(value, { stream: true });
        let boundary = buffer.indexOf("\n\n");
        while (boundary !== -1) {
          const dataLine = buffer.slice(0, boundary).split("\n").find((l) => l.startsWith("data:"));
          buffer = buffer.slice(boundary + 2);
          if (dataLine !== undefined) {
            const ev = JSON.parse(dataLine.slice(5).trim());
            events.set(ev.type, new Set([...(events.get(ev.type) ?? []), ...Object.keys(ev.data ?? {})]));
          }
          boundary = buffer.indexOf("\n\n");
        }
      }
    } finally {
      await reader.cancel().catch(() => {});
    }
    return { responseID: created.id, sessionID: created.session_id, events };
  })();
  return cached;
}
