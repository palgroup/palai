import { test } from "node:test";
import assert from "node:assert/strict";

import { Palai } from "../src/client.ts";
import { GoneError, NotFoundError, PalaiAPIError } from "../src/errors.ts";

// --- test doubles -----------------------------------------------------------------

interface Call {
  url: string;
  method: string;
  headers: Record<string, string>;
  body: string | undefined;
}

// recordingFetch captures each request and returns whatever the handler produces, so a
// test can assert on the exact headers/URL the client sent and script per-attempt replies.
function recordingFetch(
  handler: (call: Call, attempt: number) => globalThis.Response,
): { fetch: typeof fetch; calls: Call[] } {
  const calls: Call[] = [];
  const fetchImpl = (async (input: unknown, init?: RequestInit) => {
    const call: Call = {
      url: String(input),
      method: init?.method ?? "GET",
      headers: (init?.headers ?? {}) as Record<string, string>,
      body: typeof init?.body === "string" ? init.body : undefined,
    };
    calls.push(call);
    return handler(call, calls.length);
  }) as unknown as typeof fetch;
  return { fetch: fetchImpl, calls };
}

function json(status: number, body: unknown, headers: Record<string, string> = {}): globalThis.Response {
  return new globalThis.Response(JSON.stringify(body), {
    status,
    headers: { "content-type": "application/json", ...headers },
  });
}

function newClient(fetchImpl: typeof fetch, overrides: Record<string, unknown> = {}): Palai {
  return new Palai({
    apiKey: "sk-test",
    baseURL: "http://palai.test",
    fetch: fetchImpl,
    backoffBaseMs: 1,
    backoffMaxMs: 2,
    ...overrides,
  });
}

const queuedResponse = {
  id: "resp_1",
  object: "response",
  status: "queued",
  model: "",
  output: [],
  usage: { input_tokens: 0, output_tokens: 0 },
  session_id: "ses_1",
  run_id: "run_1",
};

// --- configuration ----------------------------------------------------------------

test("explicit constructor options win over the environment", async () => {
  await withEnvAsync({ PALAI_API_KEY: "env-key", PALAI_BASE_URL: "http://env.test" }, async () => {
    const { fetch: fetchImpl, calls } = recordingFetch(() => json(200, { ...queuedResponse, status: "completed" }));
    const client = new Palai({ apiKey: "opt-key", baseURL: "http://opt.test", fetch: fetchImpl });
    await client.responses.retrieve("resp_1");
    assert.equal(calls[0]?.headers["Authorization"], "Bearer opt-key");
    assert.ok(calls[0]?.url.startsWith("http://opt.test/"), `url = ${calls[0]?.url}`);
  });
});

test("the client reads only the Palai environment, never an unrelated provider's key", async () => {
  await withEnvAsync({ OPENAI_API_KEY: "sk-openai", ANTHROPIC_API_KEY: "sk-anthropic" }, async () => {
    // With no PALAI_API_KEY, construction fails rather than silently borrowing another key.
    assert.throws(() => new Palai({ fetch: recordingFetch(() => json(200, {})).fetch }), /API key is required/);
  });
  await withEnvAsync({ PALAI_API_KEY: "palai-env-key", OPENAI_API_KEY: "sk-openai" }, async () => {
    const { fetch: fetchImpl, calls } = recordingFetch(() => json(200, { ...queuedResponse, status: "completed" }));
    const client = new Palai({ fetch: fetchImpl });
    await client.responses.retrieve("resp_1");
    assert.equal(calls[0]?.headers["Authorization"], "Bearer palai-env-key");
  });
});

// --- create -----------------------------------------------------------------------

test("create sends the required headers and returns the 202 response handle", async () => {
  const { fetch: fetchImpl, calls } = recordingFetch(() =>
    json(202, queuedResponse, { Location: "/v1/responses/resp_1", "Request-Id": "req_abc" }),
  );
  const client = newClient(fetchImpl);
  const response = await client.responses.create({ input: "do the work" });

  assert.equal(response.id, "resp_1");
  assert.equal(response.session_id, "ses_1");
  const call = calls[0];
  assert.ok(call);
  assert.equal(call.method, "POST");
  assert.ok(call.url.endsWith("/v1/responses"));
  assert.equal(call.headers["Authorization"], "Bearer sk-test");
  assert.equal(call.headers["API-Version"], "2026-07-16");
  assert.equal(call.headers["Content-Type"], "application/json");
  assert.ok((call.headers["Idempotency-Key"] ?? "").length > 0, "create must send an Idempotency-Key");
  assert.deepEqual(JSON.parse(call.body ?? "{}"), { input: "do the work" });
});

// --- retrieve ---------------------------------------------------------------------

test("retrieve returns the typed terminal response with model and no error", async () => {
  const { fetch: fetchImpl } = recordingFetch(() =>
    json(200, {
      id: "resp_1",
      object: "response",
      status: "completed",
      model: "fake-model-v2",
      output: [{ type: "output_text", text: "12" }],
      usage: { input_tokens: 5, output_tokens: 3, total_tokens: 8 },
    }),
  );
  const response = await newClient(fetchImpl).responses.retrieve("resp_1");
  assert.equal(response.status, "completed");
  assert.equal(response.model, "fake-model-v2");
  assert.equal(response.error, undefined);
  assert.equal(response.output[0]?.type, "output_text");
});

test("retrieve surfaces the problem-shaped error of a failed response", async () => {
  const { fetch: fetchImpl } = recordingFetch(() =>
    json(200, {
      id: "resp_1",
      object: "response",
      status: "failed",
      model: "",
      output: [],
      usage: { input_tokens: 0, output_tokens: 0 },
      error: {
        type: "https://docs.palai.dev/problems/internal_error",
        title: "Internal error",
        status: 500,
        code: "internal_error",
        request_id: "req_x",
        detail: "the run failed during execution",
      },
    }),
  );
  const response = await newClient(fetchImpl).responses.retrieve("resp_1");
  assert.equal(response.status, "failed");
  assert.equal(response.error?.code, "internal_error");
  assert.equal(response.error?.title, "Internal error");
});

test("retrieve maps 404 and 410 to typed RFC 9457 errors", async () => {
  const notFound = recordingFetch(() =>
    json(404, { type: "t", title: "Not found", status: 404, code: "not_found", request_id: "req_404" }, {
      "content-type": "application/problem+json",
    }),
  );
  await assert.rejects(newClient(notFound.fetch).responses.retrieve("missing"), (error: unknown) => {
    assert.ok(error instanceof NotFoundError);
    assert.equal(error.status, 404);
    assert.equal(error.code, "not_found");
    assert.equal(error.requestId, "req_404");
    assert.equal(error.retryable, false);
    return true;
  });

  const gone = recordingFetch(() =>
    json(410, { type: "t", title: "Gone", status: 410, code: "retention_expired", request_id: "req_410" }),
  );
  await assert.rejects(newClient(gone.fetch).responses.retrieve("purged"), (error: unknown) => {
    assert.ok(error instanceof GoneError);
    assert.equal(error.code, "retention_expired");
    return true;
  });
});

test("an unknown problem code is preserved on the typed error (open enum)", async () => {
  const { fetch: fetchImpl } = recordingFetch(() =>
    json(400, { type: "t", title: "Nope", status: 400, code: "some_future_code", request_id: "req_1" }),
  );
  await assert.rejects(newClient(fetchImpl).responses.retrieve("x"), (error: unknown) => {
    assert.ok(error instanceof PalaiAPIError);
    assert.equal(error.code, "some_future_code");
    return true;
  });
});

test("unknown response status and unknown fields survive retrieve (open world)", async () => {
  const { fetch: fetchImpl } = recordingFetch(() =>
    json(200, {
      id: "resp_1",
      object: "response",
      status: "provisioning_beta",
      model: "m",
      output: [],
      usage: { input_tokens: 0, output_tokens: 0 },
      surprise_field: "kept",
    }),
  );
  const response = await newClient(fetchImpl).responses.retrieve("resp_1");
  assert.equal(response.status, "provisioning_beta");
  assert.equal((response as unknown as Record<string, unknown>)["surprise_field"], "kept");
});

// --- cancel -----------------------------------------------------------------------

test("cancel posts to the cancel subpath", async () => {
  const { fetch: fetchImpl, calls } = recordingFetch(() => new globalThis.Response(null, { status: 202 }));
  await newClient(fetchImpl).responses.cancel("resp_1");
  assert.equal(calls[0]?.method, "POST");
  assert.ok(calls[0]?.url.endsWith("/v1/responses/resp_1/cancel"));
});

// --- server-only guard ------------------------------------------------------------

test("the API-key credential module refuses to load in a browser bundle", async () => {
  const globals = globalThis as { window?: unknown };
  globals.window = {};
  try {
    await assert.rejects(
      // A unique query forces a fresh module evaluation so the top-level guard runs again.
      import(`../src/server-only.ts?browser-check=${Date.now()}`),
      /browser/i,
    );
  } finally {
    delete globals.window;
  }
});

// withEnvAsync is withEnv for async bodies.
async function withEnvAsync(patch: Record<string, string | undefined>, fn: () => Promise<void>): Promise<void> {
  const saved: Record<string, string | undefined> = {};
  for (const key of ["PALAI_API_KEY", "PALAI_BASE_URL", "PALAI_PROJECT", "OPENAI_API_KEY", "ANTHROPIC_API_KEY"]) {
    saved[key] = process.env[key];
    delete process.env[key];
  }
  for (const [key, value] of Object.entries(patch)) {
    if (value !== undefined) {
      process.env[key] = value;
    }
  }
  try {
    await fn();
  } finally {
    for (const [key, value] of Object.entries(saved)) {
      if (value === undefined) {
        delete process.env[key];
      } else {
        process.env[key] = value;
      }
    }
  }
}
