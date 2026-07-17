import { test } from "node:test";
import assert from "node:assert/strict";

import { Palai } from "../src/client.ts";
import { fullJitterBackoff } from "../src/stream.ts";
import { InternalServerError, InvalidRequestError, PalaiConnectionError } from "../src/errors.ts";

function json(status: number, body: unknown): globalThis.Response {
  return new globalThis.Response(JSON.stringify(body), {
    status,
    headers: { "content-type": "application/json" },
  });
}

function problem(status: number, code: string): unknown {
  return { type: "t", title: code, status, code, request_id: "req_1" };
}

const queued = {
  id: "resp_1",
  object: "response",
  status: "queued",
  model: "",
  output: [],
  usage: { input_tokens: 0, output_tokens: 0 },
  session_id: "ses_1",
  run_id: "run_1",
};

function headerOf(init: RequestInit | undefined, name: string): string | undefined {
  const headers = (init?.headers ?? {}) as Record<string, string>;
  return headers[name];
}

// --- backoff schedule -------------------------------------------------------------

test("fullJitterBackoff stays within [0, min(max, base*2^attempt)] and caps at max", () => {
  const base = 100;
  const max = 5_000;
  for (let attempt = 0; attempt < 8; attempt += 1) {
    const ceiling = Math.min(max, base * 2 ** attempt);
    for (let sample = 0; sample < 200; sample += 1) {
      const wait = fullJitterBackoff(attempt, base, max);
      assert.ok(wait >= 0 && wait <= ceiling, `attempt ${attempt}: ${wait} not in [0, ${ceiling}]`);
    }
  }
  // The exponential ceiling saturates at max, and a non-positive base disables backoff.
  assert.ok(fullJitterBackoff(20, base, max) <= max);
  assert.equal(fullJitterBackoff(3, 0, max), 0);
});

// --- idempotent retry -------------------------------------------------------------

test("a retryable failure is retried with the SAME idempotency key until it succeeds", async () => {
  const keys: Array<string | undefined> = [];
  let attempt = 0;
  const fetchImpl = (async (_input: unknown, init?: RequestInit) => {
    attempt += 1;
    keys.push(headerOf(init, "Idempotency-Key"));
    if (attempt <= 2) {
      return json(503, problem(503, "capacity_unavailable"));
    }
    return json(202, queued);
  }) as unknown as typeof fetch;

  const client = new Palai({
    apiKey: "sk-test",
    baseURL: "http://palai.test",
    fetch: fetchImpl,
    maxRetries: 3,
    backoffBaseMs: 1,
    backoffMaxMs: 2,
  });
  const response = await client.responses.create({ input: "hi" });

  assert.equal(response.id, "resp_1");
  assert.equal(keys.length, 3, "two 503s then a success is three attempts");
  assert.equal(new Set(keys).size, 1, "the idempotency key must be identical across retries");
  assert.ok((keys[0] ?? "").length > 0);
});

test("a non-retryable status is thrown immediately without a retry", async () => {
  let attempts = 0;
  const fetchImpl = (async () => {
    attempts += 1;
    return json(400, problem(400, "invalid_request"));
  }) as unknown as typeof fetch;

  const client = new Palai({
    apiKey: "sk-test",
    baseURL: "http://palai.test",
    fetch: fetchImpl,
    maxRetries: 3,
    backoffBaseMs: 1,
    backoffMaxMs: 2,
  });
  await assert.rejects(client.responses.retrieve("x"), (error: unknown) => error instanceof InvalidRequestError);
  assert.equal(attempts, 1, "a 400 must not be retried");
});

test("a persistent network error retries to the ceiling then throws a connection error", async () => {
  let attempts = 0;
  const fetchImpl = (async () => {
    attempts += 1;
    throw new TypeError("network unreachable");
  }) as unknown as typeof fetch;

  const client = new Palai({
    apiKey: "sk-test",
    baseURL: "http://palai.test",
    fetch: fetchImpl,
    maxRetries: 2,
    backoffBaseMs: 1,
    backoffMaxMs: 2,
  });
  await assert.rejects(client.responses.retrieve("x"), (error: unknown) => error instanceof PalaiConnectionError);
  assert.equal(attempts, 3, "the initial attempt plus two retries");
});

test("the total deadline bounds retries below a large maxRetries", async () => {
  let attempts = 0;
  const fetchImpl = (async () => {
    attempts += 1;
    return json(503, problem(503, "capacity_unavailable"));
  }) as unknown as typeof fetch;

  const client = new Palai({
    apiKey: "sk-test",
    baseURL: "http://palai.test",
    fetch: fetchImpl,
    maxRetries: 100,
    timeoutMs: 25,
    backoffBaseMs: 5,
    backoffMaxMs: 10,
  });
  await assert.rejects(client.responses.retrieve("x"), (error: unknown) => error instanceof InternalServerError);
  assert.ok(attempts < 100, `made ${attempts} attempts; the 25ms deadline must stop well before maxRetries`);
});
