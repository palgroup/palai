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
// exactly three: organizations, projects, api-keys. So this sweep has been comparing three item shapes and
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
  it("every row of the fixture's route table is genuinely served by the fixture", async () => {
    for (const route of ROUTES) {
      const res = await fakeFetch(concretePath(route.pattern), {
        method: route.method,
        body: route.method === "POST" ? "{}" : undefined,
      });
      await res.body?.cancel().catch(() => {});
      assert.notEqual(
        res.status,
        404,
        `the table declares ${route.method} ${route.pattern} but the fixture 404s it — the table is not the ` +
          "dispatcher, so nothing this sweep concludes about the fixture is trustworthy",
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
  // the bootstrap seeds (organizations, projects, api-keys).
  it("the JSON shapes of the routes both serve agree, or the difference is on the ledger", async () => {
    // Only GETs that need no prior state, so the comparison is about SHAPE and never about test setup.
    const comparable = ROUTES.filter((r) => r.method === "GET" && !r.pattern.endsWith("/events") && !r.pattern.endsWith("/content"));
    assert.ok(comparable.length >= 10, `expected the fixture to serve the console's read surfaces, got ${comparable.length}`);

    let itemsCompared = 0;
    for (const route of comparable) {
      const subject = `${route.method} ${route.pattern}`;
      const path = concretePath(route.pattern);
      const [realRes, fakeRes] = await Promise.all([realFetch(path), fakeFetch(path)]);
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
      if (keyShape(realItem) !== keyShape(fakeItem)) {
        requireLedgerRow("shape", subject, `list item: real ${keyShape(realItem)}\n  fixture ${keyShape(fakeItem)}`);
      }
    }
    // THE FLOOR, AND IT RISES EVERY TASK (E25 T2, raised by T4). It was 3 — the three collections a bootstrap
    // stack seeds — then 4 once this sweep began seeding a knowledge base, and it is now 6: T4's seed creates
    // an ENVIRONMENT and writes one VALUE into it, and the value is a SECRET_REFS row, so two collections that
    // were empty on every bootstrap stack now have one each. The six that must hold: organizations, projects,
    // api-keys, knowledge-bases, environments, secret-refs. T7 seeds tool revisions and raises it again; it
    // must never fall.
    assert.ok(
      itemsCompared >= 6,
      `only ${itemsCompared} collections had a row on BOTH sides, so this arm compared almost no item shapes — ` +
        "the bootstrap seeds organizations/projects/api-keys and this sweep seeds a knowledge base, an " +
        "environment and one environment value (which is a secret_refs row), so fewer than six means either " +
        "the real stack is not seeded or a seed did not land, and this arm would pass vacuously",
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
