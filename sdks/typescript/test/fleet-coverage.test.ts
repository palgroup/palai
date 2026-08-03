import { test } from "node:test";
import assert from "node:assert/strict";
import { readFileSync } from "node:fs";

import { PalaiAdmin } from "../src/admin-client.ts";

// collectRoutesFrom builds a throwaway PalaiAdmin (a fake key, a fetch that throws if ever called —
// no network happens here) and reads the route tables its REAL fleet resources declare
// (RunnerPools.routes, RunnerPoolKeys.routes, Runners.routes) off admin.fleet's actual instances. That
// is deliberate: it walks the object graph PalaiAdmin really wires, not a list copy-pasted into this
// test, so a resource that declared routes but was never hung off `fleet` would still fail here.
function collectRoutesFrom(AdminCtor: typeof PalaiAdmin): string[] {
  const admin = new AdminCtor({
    apiKey: "sk-test",
    baseURL: "http://palai.test",
    fetch: (() => {
      throw new Error("fleet-coverage: no network call is expected");
    }) as unknown as typeof fetch,
  });
  const pools = admin.fleet.pools.constructor as unknown as { routes: readonly string[] };
  const poolKeys = admin.fleet.poolKeys.constructor as unknown as { routes: readonly string[] };
  const runners = admin.fleet.runners.constructor as unknown as { routes: readonly string[] };
  return [...pools.routes, ...poolKeys.routes, ...runners.routes];
}

// SDK, filo rota ailesinin TAMAMINI kapsar. Test rotaları sabit yazmaz: router'dan çıkarılan
// listeyi okur ve her biri için bir metot arar. Yarın eklenen bir rota bu testi KIRAR — ve kırması
// gerekir, çünkü kapsanmayan bir rota Cloud backend'inin ham HTTP'ye düşmesi demektir.
test("every fleet route has an admin SDK method", () => {
  const routes = readFileSync(new URL("./fixtures/fleet-routes.txt", import.meta.url), "utf8")
    .trim()
    .split("\n")
    .filter(Boolean);
  // VAKUMLULUK: fixture bir gün boş gelirse test sıfır rota denetler ve yeşil kalır.
  assert.ok(routes.length > 0, "fleet-routes fixture is empty — this test would assert nothing");

  const covered = collectRoutesFrom(PalaiAdmin);
  const missing = routes.filter((r) => !covered.includes(r));
  assert.deepEqual(missing, [], `fleet routes with no admin SDK method: ${missing.join(", ")}`);
});

// THE OTHER DIRECTION, and it is a DIFFERENT failure. The sweep above proves every shipped route has
// an entry in a `routes` table. It proves nothing about whether that entry is backed by anything: a
// string typed into `routes` and never implemented passes it, and passes it while making the whole
// sweep read as coverage the SDK does not have.
//
// fleet.ts already asserts the invariant in prose — "Every entry here has a matching
// `this.#client.request(...)` call below" — which is the shape this tree keeps finding: a comment
// claiming a property at the line a reader would audit it, with nothing enforcing it.
//
// This DRIVES the methods rather than reading the source. Two reasons, and the second is the one that
// decided it. (a) A source scan proves a `request(…)` call exists; driving proves the method exists,
// is reachable off `admin.fleet`, and builds the URL the table claims — the surface, not the
// mechanism. (b) sdks/typescript pins typescript 7.0.2, whose package exports exactly `version` and
// `versionMajorMinor`: `createSourceFile`, `ScriptTarget` and `forEachChild` are all `undefined`
// (measured 2026-08-03), so there is no compiler API here to parse with in the first place.
const PLACEHOLDER_ID = "id_placeholder";

// Every method is invoked TWICE because the fleet classes take two argument shapes — `(id, params,
// options)` for the parameterised routes and `(params, options)` for the collection ones — and this
// sweep must not encode which is which. Calls are expected to fail (the fake body satisfies no
// projection); the URL is recorded before that, which is all this test reads.
async function routesReachedBy(resource: object, seen: Set<string>, calls: { method: string; url: string }[]): Promise<void> {
  const proto = Object.getPrototypeOf(resource) as object;
  for (const name of Object.getOwnPropertyNames(proto)) {
    if (name === "constructor") continue;
    const fn = (resource as Record<string, unknown>)[name];
    if (typeof fn !== "function") continue;
    for (const args of [[PLACEHOLDER_ID, {}, {}], [{}, {}]]) {
      calls.length = 0;
      try {
        await (fn as (...a: unknown[]) => Promise<unknown>).apply(resource, args);
      } catch {
        // The request was still built and recorded — that is the observation.
      }
      for (const c of calls) {
        const path = new URL(c.url).pathname
          .split("/")
          .map((seg) => (seg === PLACEHOLDER_ID ? "*" : seg))
          .join("/");
        seen.add(`${c.method} ${path}`);
      }
    }
  }
}

test("every declared fleet route is reached by a method that really builds it", async () => {
  const calls: { method: string; url: string }[] = [];
  const admin = new PalaiAdmin({
    apiKey: "sk-test",
    baseURL: "http://palai.test",
    fetch: (async (url: string | URL | Request, init?: RequestInit) => {
      calls.push({ method: init?.method ?? "GET", url: String(url) });
      return new Response("{}", { status: 200, headers: { "content-type": "application/json" } });
    }) as unknown as typeof fetch,
  });

  const reached = new Set<string>();
  for (const resource of [admin.fleet.pools, admin.fleet.poolKeys, admin.fleet.runners]) {
    await routesReachedBy(resource, reached, calls);
  }

  // VACUITY: a sweep that reached nothing would report every route as unbacked, which reads as a
  // catastrophe rather than as a broken harness. Fail on the harness first, in its own words.
  assert.ok(reached.size > 0, "no fleet method issued a request at all — the sweep is broken, this test is VACUOUS");

  const unbacked = collectRoutesFrom(PalaiAdmin).filter((route) => {
    const [method, path] = route.split(" ");
    return !reached.has(`${method} ${path!.replace(/\{[^}]+\}/g, "*")}`);
  });
  assert.deepEqual(
    unbacked,
    [],
    `routes declared with no method behind them: ${unbacked.join(", ")}`,
  );
});
