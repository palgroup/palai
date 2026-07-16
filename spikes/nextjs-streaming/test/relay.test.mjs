import assert from "node:assert/strict";
import { existsSync } from "node:fs";
import test from "node:test";
import {
  FIRST_FRAME,
  RESUMED_FRAME,
  TERMINAL_FRAME,
  TERMINAL_DELAY_MS,
  startFakeUpstream,
} from "./fake-upstream.mjs";
import {
  captureProductionBuild,
  collectSecretScanTargets,
  hasProductionBuild,
  readProductionBuild,
  routePath,
  startNextServer,
  withTimeout,
  writeRunSummary,
} from "./production-harness.mjs";

if (process.argv.includes("--build")) {
  await captureProductionBuild();
} else {
  test("Next.js production relay contract", async (context) => {
    requireCondition(
      existsSync(routePath),
      "relay route is missing; production implementation has not been created",
    );
    requireCondition(
      hasProductionBuild(),
      "production build evidence is missing; run the build before the tests",
    );

    const { buildCapture, buildContract, secret } = readProductionBuild();
    requireCondition(secret.length >= 48, "runtime build credential is invalid");
    const fakeUpstream = await startFakeUpstream();
    let nextServer;
    try {
      nextServer = await startNextServer({
        secret,
        upstreamURL: fakeUpstream.url,
      });
    } catch (error) {
      await fakeUpstream.stop();
      throw error;
    }
    const downstreamResponses = [];
    const metrics = {
      abortToUpstreamCloseMs: 0,
      timeToFirstFrameMs: 0,
    };

    try {
      await context.test(
        "streams ordered canonical frames before the delayed terminal event",
        async () => {
          fakeUpstream.setMode("complete");
          const startedAt = performance.now();
          const response = await fetch(`${nextServer.url}/api/relay`, {
            cache: "no-store",
          });
          requireSSE(response);
          const headers = serializeResponseHead(response);
          const reader = response.body.getReader();
          const first = await readFirstFrame(reader);
          metrics.timeToFirstFrameMs = performance.now() - startedAt;
          const request = fakeUpstream.requests.at(-1);
          requireCondition(request !== undefined, "upstream request was not observed");
          requireCondition(
            request.terminalWrittenAt === null,
            "first downstream frame was buffered until the terminal event",
          );
          requireCondition(
            first.frame === FIRST_FRAME,
            "first canonical SSE frame changed in transit",
          );
          const remainder = await readRemaining(reader, first.remainder);
          const body = first.frame + remainder;
          requireCondition(
            body === FIRST_FRAME + TERMINAL_FRAME,
            "ordered canonical SSE bytes changed in transit",
          );
          requireCondition(
            request.terminalWrittenAt !== null &&
              request.terminalWrittenAt - request.firstWrittenAt >=
                TERMINAL_DELAY_MS - 25,
            "fake terminal event was not deliberately delayed",
          );
          downstreamResponses.push(headers + body);
        },
      );

      await context.test(
        "browser abort closes upstream transport without explicit cancellation",
        async () => {
          fakeUpstream.setMode("hold");
          const downstreamAbort = new AbortController();
          const response = await fetch(`${nextServer.url}/api/relay`, {
            cache: "no-store",
            signal: downstreamAbort.signal,
          });
          requireSSE(response);
          const headers = serializeResponseHead(response);
          const reader = response.body.getReader();
          const first = await readFirstFrame(reader);
          requireCondition(
            first.frame === FIRST_FRAME,
            "abort fixture did not deliver its initial frame",
          );
          const request = fakeUpstream.requests.at(-1);
          requireCondition(request !== undefined, "abort request was not observed");
          const abortedAt = performance.now();
          downstreamAbort.abort();
          await assert.rejects(reader.read(), { name: "AbortError" });
          const closedAt = await withTimeout(
            request.closed,
            1_000,
            "upstream transport remained open after downstream abort",
          );
          metrics.abortToUpstreamCloseMs = closedAt - abortedAt;
          requireCondition(
            metrics.abortToUpstreamCloseMs >= 0 &&
              metrics.abortToUpstreamCloseMs < 500,
            "upstream transport did not close promptly after downstream abort",
          );
          requireCondition(
            fakeUpstream.cancelCalls === 0,
            "local transport abort invoked the explicit cancellation endpoint",
          );
          downstreamResponses.push(headers + first.frame + first.remainder);
        },
      );

      await context.test("forwards Last-Event-ID exactly on reconnect", async () => {
        fakeUpstream.setMode("resume");
        const lastEventID = "event-001:segment/9";
        const response = await fetch(`${nextServer.url}/api/relay`, {
          cache: "no-store",
          headers: { "Last-Event-ID": lastEventID },
        });
        requireSSE(response);
        const headers = serializeResponseHead(response);
        const body = await response.text();
        const request = fakeUpstream.requests.at(-1);
        requireCondition(request !== undefined, "reconnect request was not observed");
        requireCondition(
          request.lastEventId === lastEventID,
          "Last-Event-ID changed between downstream and upstream",
        );
        requireCondition(
          body === RESUMED_FRAME,
          "resumed canonical SSE frame changed in transit",
        );
        downstreamResponses.push(headers + body);
      });

      await context.test(
        "returns a generic credential-free response for upstream failures",
        async () => {
          fakeUpstream.setMode("error");
          const response = await fetch(`${nextServer.url}/api/relay`, {
            cache: "no-store",
          });
          const headers = serializeResponseHead(response);
          const body = await response.text();
          requireCondition(response.status === 502, "upstream failure was not mapped to 502");
          requireCondition(
            body === "Upstream stream unavailable\n",
            "upstream failure details reached the downstream response",
          );
          downstreamResponses.push(headers + body);
        },
      );

      await context.test(
        "sends the credential only as upstream Authorization",
        () => {
          requireCondition(
            fakeUpstream.requests.length === 4,
            "unexpected upstream request count",
          );
          for (const request of fakeUpstream.requests) {
            requireCondition(
              request.authorization === `Bearer ${secret}`,
              "upstream authorization did not match the runtime credential",
            );
            const locations = secretLocationsInRequest(request, secret);
            requireCondition(
              locations.length === 1 && locations[0] === "authorization",
              "runtime credential appeared outside upstream Authorization",
            );
          }
        },
      );

      requireCondition(
        nextServer.arguments.join(" ") === "start --hostname 127.0.0.1 --port",
        "integration tests did not launch next start",
      );
    } finally {
      await Promise.all([nextServer.stop(), fakeUpstream.stop()]);
    }

    await context.test(
      "keeps the runtime credential out of responses, builds, sources, maps, chunks, and logs",
      () => {
        requireCondition(
          !nextServer.capture.overflow,
          "next start exceeded its configured log capture limit",
        );
        const targets = collectSecretScanTargets({
          buildCapture,
          downstreamResponses,
          nextServer,
        });
        for (const [category, values] of Object.entries(targets)) {
          requireCondition(values.length > 0, `secret scan category is empty: ${category}`);
          for (const value of values) {
            requireCondition(
              !value.includes(secret),
              `runtime credential leaked into scan category: ${category}`,
            );
          }
        }
        writeRunSummary({
          assertionCount: 9,
          buildContract,
          metrics,
          targets,
        });
      },
    );
  });
}

function secretLocationsInRequest(request, secret) {
  const locations = [];
  if (request.authorization?.includes(secret)) {
    locations.push("authorization");
  }
  if (request.method.includes(secret)) {
    locations.push("method");
  }
  if (request.url.includes(secret)) {
    locations.push("url");
  }
  for (let index = 0; index < request.rawHeaders.length; index += 2) {
    const name = request.rawHeaders[index]?.toLowerCase();
    const value = request.rawHeaders[index + 1] ?? "";
    if (name !== "authorization" && value.includes(secret)) {
      locations.push(`header:${name}`);
    }
  }
  return locations;
}

async function readFirstFrame(reader) {
  const decoder = new TextDecoder();
  let text = "";
  while (!text.includes("\n\n")) {
    const { done, value } = await reader.read();
    requireCondition(!done, "SSE stream ended before its first frame");
    text += decoder.decode(value, { stream: true });
  }
  const boundary = text.indexOf("\n\n") + 2;
  return { frame: text.slice(0, boundary), remainder: text.slice(boundary) };
}

async function readRemaining(reader, initial = "") {
  const decoder = new TextDecoder();
  let text = initial;
  for (;;) {
    const { done, value } = await reader.read();
    if (done) {
      return text + decoder.decode();
    }
    text += decoder.decode(value, { stream: true });
  }
}

function requireSSE(response) {
  requireCondition(response.status === 200, "relay did not return HTTP 200");
  requireCondition(response.body !== null, "relay response did not have a stream body");
  requireCondition(
    response.headers.get("content-type") === "text/event-stream; charset=utf-8",
    "relay did not return the canonical SSE content type",
  );
  requireCondition(
    response.headers.get("cache-control") === "no-cache, no-transform",
    "relay response was not marked no-cache/no-transform",
  );
}

function serializeResponseHead(response) {
  return `${response.status}\n${JSON.stringify([...response.headers])}\n`;
}

function requireCondition(condition, message) {
  if (!condition) {
    throw new Error(message);
  }
}
