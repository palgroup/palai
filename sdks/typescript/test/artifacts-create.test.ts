import assert from "node:assert/strict";
import { test } from "node:test";
import { Palai } from "../src/index.ts";

// A screenshot reaching a model is the one direction this SDK could not go. `artifacts.download` has
// existed since E09 and `create` did not, so a browser app — the only kind with a file picker — could
// show what the agent produced and never send what the operator was looking at.
test("create posts the raw bytes and declares no media type", async () => {
  let seen: { method: string | undefined; path: string; body: unknown; contentType: string | null } = { method: undefined, path: "", body: undefined, contentType: null };
  const client = new Palai({
    apiKey: "sk-test",
    baseURL: "http://127.0.0.1:1",
    fetch: async (url: string | Request | URL, init?: RequestInit) => {
      seen = {
        method: init?.method,
        path: new URL(String(url)).pathname,
        body: init?.body,
        contentType: new Headers(init?.headers).get("Content-Type"),
      };
      return new Response(JSON.stringify({ id: "art_1", object: "artifact", media_type: "image/png" }), {
        status: 200,
        headers: { "Content-Type": "application/json" },
      });
    },
  });

  const artifact = await client.artifacts.create(new Uint8Array([0x89, 0x50, 0x4e, 0x47]));

  assert.equal(artifact.id, "art_1");
  assert.equal(seen.method, "POST");
  assert.equal(seen.path, "/v1/artifacts");
  // ‼️ THE BYTES GO UNWRAPPED. A JSON envelope here would be a body the control plane sniffs as JSON and
  // refuses as a media type outside its allow-list — the upload failing for a reason the caller cannot
  // see from either side.
  assert.ok(seen.body instanceof Uint8Array, "the body must be the raw bytes");
  // AND NO Content-Type, because the server sniffs. A declared type is a claim the server ignores, which
  // is a lie in a function signature.
  assert.equal(seen.contentType, null);
});

test("create refuses an empty upload before the round trip", async () => {
  const client = new Palai({ apiKey: "sk-test", baseURL: "http://127.0.0.1:1" });
  await assert.rejects(() => client.artifacts.create(new Uint8Array()), /no bytes/);
});
