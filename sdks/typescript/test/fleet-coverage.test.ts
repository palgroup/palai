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
