import { test } from "node:test";
import assert from "node:assert/strict";

import { Palai } from "../src/client.ts";

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

function newClient(fetchImpl: typeof fetch): Palai {
  return new Palai({ apiKey: "sk-admin", baseURL: "http://palai.test", fetch: fetchImpl, backoffBaseMs: 1, backoffMaxMs: 2 });
}

// --- secret refs (T3): write-only value, metadata reads, rotate --------------------

test("secretRefs.create sends name+value and gets metadata back; the value never leaves the SDK", async () => {
  const { fetch: f, calls } = recordingFetch(() => json(201, { name: "provider-one", version: 1, object: "secret_ref" }));
  const ref = await newClient(f).secretRefs.create({ name: "provider-one", value: "sk-upstream-abc" });
  assert.equal(ref.name, "provider-one");
  assert.equal(ref.version, 1);
  assert.equal(calls[0]?.method, "POST");
  assert.ok(calls[0]?.url.endsWith("/v1/secret-refs"));
  assert.deepEqual(JSON.parse(calls[0]!.body ?? "{}"), { name: "provider-one", value: "sk-upstream-abc" });
  // The credential rides the Authorization header, never a browser token.
  assert.equal(calls[0]?.headers["Authorization"], "Bearer sk-admin");
});

test("secretRefs list/get/rotate hit the right routes", async () => {
  const { fetch: f, calls } = recordingFetch((call) =>
    call.url.endsWith("/v1/secret-refs") && call.method === "GET"
      ? json(200, { object: "list", data: [{ name: "provider-one", version: 2, object: "secret_ref" }] })
      : json(200, { name: "provider-one", version: 2, object: "secret_ref" }),
  );
  const client = newClient(f);

  const listed = await client.secretRefs.list();
  assert.equal(listed.data[0]!.name, "provider-one");
  await client.secretRefs.retrieve("provider-one");
  const rotated = await client.secretRefs.rotate("provider-one", { value: "sk-upstream-def" });
  assert.equal(rotated.version, 2);

  assert.equal(calls[1]?.method, "GET");
  assert.ok(calls[1]?.url.endsWith("/v1/secret-refs/provider-one"));
  assert.equal(calls[2]?.method, "POST");
  assert.ok(calls[2]?.url.endsWith("/v1/secret-refs/provider-one/rotate"));
  assert.deepEqual(JSON.parse(calls[2]!.body ?? "{}"), { value: "sk-upstream-def" });
});

// --- model routes (T8) --------------------------------------------------------------

test("modelRoutes drives connection + route + revision + publish with reference-only bodies", async () => {
  const { fetch: f, calls } = recordingFetch(() => json(201, { id: "mx_1", object: "model_route" }));
  const client = newClient(f);

  await client.modelRoutes.createConnection({ provider: "provider-one", secret_ref: "openai-a" });
  await client.modelRoutes.createRoute({ name: "default" });
  await client.modelRoutes.createRevision("mroute_1", { model: "gpt-4o-mini", connection_id: "mconn_1" });
  await client.modelRoutes.publishRevision("mroute_1", "mrev_1");

  assert.ok(calls[0]!.url.endsWith("/v1/model-connections"));
  assert.deepEqual(JSON.parse(calls[0]!.body ?? "{}"), { provider: "provider-one", secret_ref: "openai-a" });
  assert.ok(calls[1]!.url.endsWith("/v1/model-routes"));
  assert.ok(calls[2]!.url.endsWith("/v1/model-routes/mroute_1/revisions"));
  assert.equal(calls[3]!.method, "POST");
  assert.ok(calls[3]!.url.endsWith("/v1/model-routes/mroute_1/revisions/mrev_1/publish"));
});

// --- tenancy provisioning (T2) ------------------------------------------------------

test("organizations.create provisions a second tenant and returns its one-time admin key", async () => {
  const { fetch: f, calls } = recordingFetch(() =>
    json(201, {
      id: "org_2",
      object: "organization",
      display_name: "acme",
      default_project_id: "prj_2",
      admin_api_key: { id: "key_2", object: "api_key", key: "sk-live-once", scopes: [] },
    }),
  );
  const org = await newClient(f).organizations.create({ display_name: "acme" });
  assert.equal(org.id, "org_2");
  assert.equal(org.default_project_id, "prj_2");
  assert.equal(org.admin_api_key.key, "sk-live-once");
  assert.ok(calls[0]?.url.endsWith("/v1/organizations"));
  assert.deepEqual(JSON.parse(calls[0]!.body ?? "{}"), { display_name: "acme" });
});

test("projects create/list/get and the config_policy PATCH write-path", async () => {
  const { fetch: f, calls } = recordingFetch((call) =>
    call.url.endsWith("/v1/projects") && call.method === "GET"
      ? json(200, { object: "list", data: [{ id: "prj_1", object: "project" }] })
      : json(200, { id: "prj_1", object: "project", config_policy: { allowed_models: ["m"] } }),
  );
  const client = newClient(f);

  await client.projects.create({ display_name: "p" });
  const listed = await client.projects.list();
  assert.equal(listed.data[0]!.id, "prj_1");
  await client.projects.retrieve("prj_1");
  const patched = await client.projects.updatePolicy("prj_1", { config_policy: { allowed_models: ["m"] } });
  assert.deepEqual(patched.config_policy, { allowed_models: ["m"] });

  assert.equal(calls[3]?.method, "PATCH");
  assert.ok(calls[3]?.url.endsWith("/v1/projects/prj_1"));
  assert.deepEqual(JSON.parse(calls[3]!.body ?? "{}"), { config_policy: { allowed_models: ["m"] } });
});

test("apiKeys create/list/get/revoke; the plaintext key appears only on create", async () => {
  const { fetch: f, calls } = recordingFetch((call) =>
    call.method === "POST" && call.url.endsWith("/v1/api-keys")
      ? json(201, { id: "key_1", object: "api_key", key: "sk_secret", scopes: [] })
      : json(200, { id: "key_1", object: "api_key", scopes: [] }),
  );
  const client = newClient(f);

  const created = await client.apiKeys.create({ project_id: "prj_1", scopes: ["run"] });
  assert.equal(created.key, "sk_secret");
  const read = await client.apiKeys.retrieve("key_1");
  assert.equal((read as Record<string, unknown>)["key"], undefined, "a read must not carry the plaintext key");
  await client.apiKeys.list();
  await client.apiKeys.revoke("key_1");

  assert.deepEqual(JSON.parse(calls[0]!.body ?? "{}"), { project_id: "prj_1", scopes: ["run"] });
  assert.equal(calls[3]?.method, "POST");
  assert.ok(calls[3]?.url.endsWith("/v1/api-keys/key_1/revoke"));
});
