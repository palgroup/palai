import { test } from "node:test";
import assert from "node:assert/strict";
import { readFileSync } from "node:fs";

import * as adminEntry from "../src/index.admin.ts";
import * as browserEntry from "../src/index.browser.ts";

// @palai/sdk/admin AYRI bir giriştir ve TARAYICIYA ait değildir: operatör yüzeyi bir sunucu
// yüzeyidir ve müşteri tarayıcısına paketlenmesi bir kimlik sızıntısıdır.
test("admin entrypoint is server-only and exports PalaiAdmin", () => {
  assert.equal(typeof adminEntry.PalaiAdmin, "function");

  const pkg = JSON.parse(readFileSync(new URL("../package.json", import.meta.url), "utf8"));
  assert.ok(pkg.exports["./admin"], "package.json exports must declare ./admin");
  assert.equal(
    pkg.exports["./admin"].browser,
    undefined,
    "./admin must carry NO browser condition — one would let a bundler move the operator client into the customer bundle",
  );
});

// index.browser HİÇBİR admin sembolü taşımaz. Bu POZİTİF olarak denetlenir: "eklemedik" demek
// yetmez, iki modülün export ettiği isimler kesiştirilir.
test("the browser entrypoint carries no admin symbol", () => {
  const leaked = Object.keys(adminEntry).filter((k) => k in browserEntry);
  assert.deepEqual(leaked, [], `browser entrypoint leaked admin symbols: ${leaked.join(", ")}`);
});
