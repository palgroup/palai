import { test } from "node:test";
import assert from "node:assert/strict";

import { Palai } from "../src/client.ts";
import * as browserEntry from "../src/index.browser.ts";

// --- shared test double -------------------------------------------------------------

interface Call {
  url: string;
  method: string;
  headers: Record<string, string>;
  body: string | undefined;
}

// recordingFetch captures each request and returns whatever the handler produces, so a test
// can assert on the exact method/URL/headers/body the client sent. It mirrors the double in
// responses.test.ts (kept local so each suite stays self-contained).
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

function json(status: number, body: unknown, headers: Record<string, string> = {}): globalThis.Response {
  return new globalThis.Response(status === 204 ? null : JSON.stringify(body), {
    status,
    headers: { "content-type": "application/json", ...headers },
  });
}

function newClient(fetchImpl: typeof fetch): Palai {
  return new Palai({ apiKey: "sk-test", baseURL: "http://palai.test", fetch: fetchImpl, backoffBaseMs: 1, backoffMaxMs: 2 });
}

function page(data: unknown[], nextCursor: string | null = null): unknown {
  return { data, has_more: nextCursor !== null, next_cursor: nextCursor, previous_cursor: null };
}

// Every request must carry the server-side credential and the dated API version — the positive
// proof that these resources route through the server-only client, never a browser token.
function assertAuthenticated(call: Call): void {
  assert.equal(call.headers["Authorization"], "Bearer sk-test");
  assert.equal(call.headers["API-Version"], "2026-07-16");
}

// --- browser-direct-token DROP: positive enforcement --------------------------------

test("the browser entrypoint exposes no credentialed client — the browser-direct-token DROP", () => {
  // Everything a browser needs to narrow an error is present…
  assert.equal(typeof browserEntry.PalaiAPIError, "function");
  // …but the API-key client constructor is NOT reachable from the browser entrypoint. A browser
  // that wanted a direct token has nothing to construct: the credential path stays server-only.
  assert.equal((browserEntry as Record<string, unknown>)["Palai"], undefined);
  assert.equal((browserEntry as Record<string, unknown>)["Sessions"], undefined);
  assert.equal((browserEntry as Record<string, unknown>)["Artifacts"], undefined);
});

// --- sessions -----------------------------------------------------------------------

test("sessions.create posts and returns the session handle", async () => {
  const { fetch: f, calls } = recordingFetch(() => json(201, { id: "ses_1", object: "session", status: "open" }));
  const session = await newClient(f).sessions.create();
  assert.equal(session.id, "ses_1");
  assert.equal(calls[0]?.method, "POST");
  assert.ok(calls[0]?.url.endsWith("/v1/sessions"));
  assertAuthenticated(calls[0]!);
});

test("sessions.retrieve gets by id", async () => {
  const { fetch: f, calls } = recordingFetch(() => json(200, { id: "ses_1", object: "session", status: "open" }));
  const session = await newClient(f).sessions.retrieve("ses_1");
  assert.equal(session.id, "ses_1");
  assert.ok(calls[0]?.url.endsWith("/v1/sessions/ses_1"));
});

test("sessions.list passes cursor + filters and returns a page", async () => {
  const { fetch: f, calls } = recordingFetch(() => json(200, page([{ id: "ses_1", object: "session" }], "cur_2")));
  const result = await newClient(f).sessions.list({ limit: 10, after: "cur_1", status: "open", createdAfter: "2026-07-01T00:00:00Z" });
  assert.equal(result.data.length, 1);
  assert.equal(result.has_more, true);
  assert.equal(result.next_cursor, "cur_2");
  const url = new URL(calls[0]!.url);
  assert.equal(url.pathname, "/v1/sessions");
  assert.equal(url.searchParams.get("limit"), "10");
  assert.equal(url.searchParams.get("after"), "cur_1");
  assert.equal(url.searchParams.get("status"), "open");
  assert.equal(url.searchParams.get("created_after"), "2026-07-01T00:00:00Z");
});

test("sessions.commands.steer posts a send_message steer with the message and a command id", async () => {
  const { fetch: f, calls } = recordingFetch(() => json(202, { id: "cmd_1", object: "command", kind: "send_message", status: "queued", session_id: "ses_1", created_at: "t" }));
  const command = await newClient(f).sessions.commands.steer("ses_1", { message: "focus on the failing test" });
  assert.equal(command.id, "cmd_1");
  assert.equal(calls[0]?.method, "POST");
  assert.ok(calls[0]?.url.endsWith("/v1/sessions/ses_1/commands"));
  const sent = JSON.parse(calls[0]!.body ?? "{}");
  assert.equal(sent.kind, "send_message");
  assert.equal(sent.delivery, "steer");
  assert.equal(sent.message, "focus on the failing test");
  assert.ok(typeof sent.command_id === "string" && sent.command_id.length > 0, "steer must mint a command_id");
});

test("sessions.commands.interrupt posts a send_message interrupt and honors an explicit command id", async () => {
  const { fetch: f, calls } = recordingFetch(() => json(202, { id: "cmd_2", object: "command", kind: "send_message", status: "queued", session_id: "ses_1", created_at: "t" }));
  await newClient(f).sessions.commands.interrupt("ses_1", { message: "stop now", commandId: "cmd_explicit" });
  const sent = JSON.parse(calls[0]!.body ?? "{}");
  assert.equal(sent.delivery, "interrupt");
  assert.equal(sent.message, "stop now");
  assert.equal(sent.command_id, "cmd_explicit");
});

// --- agents -------------------------------------------------------------------------

test("agents.list / retrieve / listRevisions / publishRevision hit the right routes", async () => {
  const { fetch: f, calls } = recordingFetch((call) => {
    if (call.method === "POST") return json(200, { id: "arev_1", object: "agent_revision", state: "published" });
    if (call.url.includes("/revisions")) return json(200, page([{ id: "arev_1", object: "agent_revision" }]));
    if (call.url.match(/\/agents\/[^/]+$/)) return json(200, { id: "agt_1", object: "agent" });
    return json(200, page([{ id: "agt_1", object: "agent" }]));
  });
  const client = newClient(f);

  const list = await client.agents.list({ limit: 5 });
  assert.equal(list.data[0]!.id, "agt_1");
  const profile = await client.agents.retrieve("agt_1");
  assert.equal(profile.id, "agt_1");
  const revisions = await client.agents.listRevisions("agt_1");
  assert.equal(revisions.data[0]!.id, "arev_1");
  const published = await client.agents.publishRevision("agt_1", "arev_1");
  assert.equal(published.id, "arev_1");

  assert.ok(calls[0]!.url.includes("/v1/agents?"));
  assert.ok(calls[1]!.url.endsWith("/v1/agents/agt_1"));
  assert.ok(calls[2]!.url.startsWith("http://palai.test/v1/agents/agt_1/revisions"));
  assert.equal(calls[3]!.method, "POST");
  assert.ok(calls[3]!.url.endsWith("/v1/agents/agt_1/revisions/arev_1/publish"));
});

// --- artifacts ----------------------------------------------------------------------

test("artifacts.retrieve returns metadata; listForResponse lists a run's artifacts", async () => {
  const { fetch: f, calls } = recordingFetch((call) => {
    if (call.url.includes("/responses/")) return json(200, { object: "list", data: [{ id: "art_1", object: "artifact" }] });
    return json(200, { id: "art_1", object: "artifact", size_bytes: 3 });
  });
  const client = newClient(f);
  const meta = await client.artifacts.retrieve("art_1");
  assert.equal(meta.id, "art_1");
  assert.ok(calls[0]!.url.endsWith("/v1/artifacts/art_1"));

  const listed = await client.artifacts.listForResponse("resp_1");
  assert.equal(listed.data[0]!.id, "art_1");
  assert.ok(calls[1]!.url.endsWith("/v1/responses/resp_1/artifacts"));
});

test("artifacts.download streams bytes and surfaces the Content-Digest", async () => {
  const bytes = new Uint8Array([1, 2, 3, 4]);
  const { fetch: f, calls } = recordingFetch(
    () =>
      new globalThis.Response(bytes, {
        status: 200,
        headers: { "Content-Type": "application/octet-stream", "Content-Digest": "sha-256=:abc:", "Content-Length": "4" },
      }),
  );
  const download = await newClient(f).artifacts.download("art_1");
  assert.equal(download.contentDigest, "sha-256=:abc:");
  assert.equal(download.contentType, "application/octet-stream");
  assert.equal(download.contentLength, 4);
  const drained = await download.bytes();
  assert.deepEqual([...drained], [1, 2, 3, 4]);
  assert.ok(calls[0]!.url.endsWith("/v1/artifacts/art_1/content"));
  assertAuthenticated(calls[0]!);
});

test("artifacts.download maps a 404 to a typed NotFoundError", async () => {
  const { NotFoundError } = await import("../src/errors.ts");
  const { fetch: f } = recordingFetch(() => json(404, { type: "t", title: "gone", status: 404, code: "not_found", request_id: "r" }));
  await assert.rejects(newClient(f).artifacts.download("missing"), (e: unknown) => e instanceof NotFoundError);
});

// --- T4 read/LIST surfaces ----------------------------------------------------------

test("the T4 list/get surfaces each page and get through the shared cursor", async () => {
  const { fetch: f, calls } = recordingFetch((call) =>
    call.url.match(/\/(tools|mcp-connections|repository-bindings|triggers|responses|tool-sets)\/[^/?]+$/)
      ? json(200, { id: "x_1", object: "thing" })
      : json(200, page([{ id: "x_1", object: "thing" }])),
  );
  const client = newClient(f);

  await client.responses.list({ status: "completed" });
  await client.repositoryBindings.list();
  await client.repositoryBindings.retrieve("rb_1");
  await client.tools.list();
  await client.tools.retrieve("tool_1");
  await client.tools.listSets();
  await client.mcpConnections.list();
  await client.mcpConnections.retrieve("mcp_1");
  await client.triggers.list();
  await client.triggers.retrieve("trg_1");

  const paths = calls.map((c) => new URL(c.url).pathname);
  assert.deepEqual(paths, [
    "/v1/responses",
    "/v1/repository-bindings",
    "/v1/repository-bindings/rb_1",
    "/v1/tools",
    "/v1/tools/tool_1",
    "/v1/tool-sets",
    "/v1/mcp-connections",
    "/v1/mcp-connections/mcp_1",
    "/v1/triggers",
    "/v1/triggers/trg_1",
  ]);
  for (const c of calls) assertAuthenticated(c);
});
