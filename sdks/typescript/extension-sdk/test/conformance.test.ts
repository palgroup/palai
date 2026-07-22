// The TypeScript leg of the shared tool-sdk conformance corpus (spec §28.23,
// TOL-018). It runs the SAME JSON fixtures the Go and Python legs run, so a
// polyglot drift in the four server-side surfaces fails this test. Rides the only
// framework this repo's TS uses: node:test under --experimental-strip-types.
import { test } from "node:test";
import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import { fileURLToPath } from "node:url";

import {
  defineTool,
  sign,
  verify,
  syncResult,
  syncProblem,
  callback,
  callbackProblem,
  IdempotencyStore,
} from "../src/index.ts";

const corpusDir = new URL("../../../../tests/conformance/tool-sdk/corpus/", import.meta.url);

function load(name: string): { vectors: any[] } {
  return JSON.parse(readFileSync(fileURLToPath(new URL(name, corpusDir)), "utf8"));
}

test("schema-emit canonical bytes", () => {
  const { vectors } = load("schema-emit.json");
  assert.ok(vectors.length > 0, "empty schema-emit corpus");
  for (const v of vectors) {
    assert.equal(defineTool(v.definition), v.canonical, v.name);
  }
});

test("signature verify matches corpus + sign parity", () => {
  const { vectors } = load("signature-verify.json");
  assert.ok(vectors.length > 0, "empty signature-verify corpus");
  for (const v of vectors) {
    const secret = Buffer.from(v.secret, "utf8");
    const body = Buffer.from(v.body, "utf8");
    const got = verify(secret, v.webhook_id, v.timestamp, body, v.signature, v.now, v.tolerance_seconds);
    assert.equal(got, v.expect, v.name);
    if (v.expect_signature) {
      assert.equal(sign(secret, v.webhook_id, v.timestamp, body), v.expect_signature, v.name);
    }
  }
});

test("result-normalize canonical bytes", () => {
  const { vectors } = load("result-normalize.json");
  assert.ok(vectors.length > 0, "empty result-normalize corpus");
  for (const v of vectors) {
    let got: string;
    if (v.kind === "sync" && v.outcome === "result") got = syncResult(v.payload);
    else if (v.kind === "sync" && v.outcome === "problem") got = syncProblem(v.payload);
    else if (v.kind === "callback" && v.outcome === "result") got = callback(v.operation_id, v.tool_call_id, v.payload);
    else if (v.kind === "callback" && v.outcome === "problem") got = callbackProblem(v.operation_id, v.tool_call_id, v.payload);
    else throw new Error(`${v.name}: unknown kind/outcome ${v.kind}/${v.outcome}`);
    assert.equal(got, v.canonical, v.name);
  }
});

test("idempotency store replay + conflict", () => {
  const s = new IdempotencyStore();
  assert.equal(s.classify("tc", "h1").outcome, "fresh");
  const stored = '{"result":{"ok":true}}';
  s.store("tc", "h1", stored);
  const replay = s.classify("tc", "h1");
  assert.equal(replay.outcome, "replay");
  assert.equal(replay.response, stored);
  assert.equal(s.classify("tc", "h2").outcome, "conflict");
});
