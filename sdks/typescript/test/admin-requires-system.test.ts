import { test } from "node:test";
import assert from "node:assert/strict";

import { PalaiAdmin } from "../src/admin-client.ts";

// --- shared test double (mirrors test/resources.test.ts's recordingFetch idiom) -----

interface Call {
  url: string;
  method: string;
  headers: Record<string, string>;
  body: string | undefined;
}

function recordingFetch(handler: (call: Call) => globalThis.Response): { fetch: typeof fetch; calls: Call[] } {
  const calls: Call[] = [];
  const fetchImpl = (async (input: unknown, init?: RequestInit) => {
    calls.push({
      url: String(input),
      method: init?.method ?? "GET",
      headers: (init?.headers ?? {}) as Record<string, string>,
      body: typeof init?.body === "string" ? init.body : undefined,
    });
    return handler(calls[calls.length - 1]!);
  }) as unknown as typeof fetch;
  return { fetch: fetchImpl, calls };
}

function json(status: number, body: unknown): globalThis.Response {
  return new globalThis.Response(JSON.stringify(body), { status, headers: { "content-type": "application/json" } });
}

// fakeControlPlane is a TEST DOUBLE for A.1's `system`-capability gate: it reads the Authorization
// header on every call and 403s a key whose token does not carry `requireCapability`. It is not a
// re-implementation of the real control plane's problem body — only enough of RFC 9457 shape for
// these assertions — and passing here is NOT proof the boundary holds against a REAL control plane:
// A.1 T2 gated `POST /v1/organizations`, but the fleet routes (A.7 T2) are gated by a change landing
// concurrently with this task and may not be merged yet. This test drives the SDK's shape of the
// boundary against a fake, nothing more.
function fakeControlPlane(opts: { requireCapability: string }): { url: string; fetch: typeof fetch } {
  const { fetch: fetchImpl } = recordingFetch((call) => {
    const key = (call.headers["Authorization"] ?? "").replace(/^Bearer\s+/, "");
    // Capability is read off the key's last TWO hyphen-delimited tokens ("with-<cap>" / "no-<cap>"),
    // never a raw substring test. A substring check would misread "tenant-key-no-system": the tail
    // "no-system" itself contains the run of characters "system", so `.includes("system")` would
    // WRONGLY grant the very capability the key is named to lack.
    const tokens = key.split("-");
    const hasCapability = tokens.at(-2) === "with" && tokens.at(-1) === opts.requireCapability;
    if (!hasCapability) {
      return json(403, { type: "about:blank", title: "forbidden", status: 403, code: "capability_denied", request_id: "r" });
    }
    return json(201, { id: "prj_1", object: "project", display_name: "x" });
  });
  return { url: "http://palai.test", fetch: fetchImpl };
}

// PalaiAdmin bir MÜŞTERİ anahtarıyla kurulursa çağrılar 403 döner. Bu bir tip kontrolü değil, bir TEL
// kontrolüdür: sahte sunucu Authorization başlığını okur ve `system` taşımayan anahtara 403 döndürür —
// yani test A.1'in kapısının ŞEKLİNİ sürer.
test("PalaiAdmin with a tenant key is refused by the server", async () => {
  const srv = fakeControlPlane({ requireCapability: "system" });

  const admin = new PalaiAdmin({ apiKey: "tenant-key-no-system", baseURL: srv.url, fetch: srv.fetch });
  await assert.rejects(
    () => admin.projects.create({ display_name: "x" }),
    (err: unknown) => (err as { status?: number }).status === 403,
    "a tenant key must be refused 403 by the admin client",
  );

  const ok = new PalaiAdmin({ apiKey: "platform-key-with-system", baseURL: srv.url, fetch: srv.fetch });
  const created = await ok.projects.create({ display_name: "x" });
  assert.ok(created, "a platform key must be accepted");
});

// The grouping is IDENTITY, not a copy: admin.organizations/projects/apiKeys must be instances of the
// exact same classes provisioning.ts exports at the root entrypoint — proving Task 3 grouped rather
// than duplicated a single method.
test("admin.organizations/projects/apiKeys are the SAME classes as the root entrypoint's", async () => {
  const { Organizations, Projects, ApiKeys } = await import("../src/resources/provisioning.ts");
  const admin = new PalaiAdmin({
    apiKey: "platform-key-with-system",
    baseURL: "http://palai.test",
    fetch: (() => {
      throw new Error("identity test: no network call is expected");
    }) as unknown as typeof fetch,
  });
  assert.ok(admin.organizations instanceof Organizations);
  assert.ok(admin.projects instanceof Projects);
  assert.ok(admin.apiKeys instanceof ApiKeys);
});
