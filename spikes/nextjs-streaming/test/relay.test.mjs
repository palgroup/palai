import assert from "node:assert/strict";
import { existsSync } from "node:fs";
import test from "node:test";
import {
  FIRST_FRAME,
  RESUMED_FRAME,
  startFakeUpstream,
  TERMINAL_DELAY_MS,
  TERMINAL_FRAME,
} from "./fake-upstream.mjs";
import {
  captureProductionBuild,
  collectSecretScanTargets,
  hasProductionBuild,
  readProductionBuild,
  routePath,
  startNextServer,
} from "./production-harness.mjs";
import {
  fetchWithDeadline,
  readFirstFrame,
  readRemaining,
  withTimeout,
} from "./process-lifecycle.mjs";
import {
  createOutcomeRecorder,
  hasRunContext,
  writeRunObservation,
} from "./run-summary.mjs";

const fetchDeadlineMS = 5_000;
const streamDeadlineMS = 5_000;

if (process.argv.includes("--build")) {
  await captureProductionBuild();
} else {
  test(
    "Next.js production relay contract",
    { timeout: 30_000 },
    runRelayContract,
  );
}

async function runRelayContract(context) {
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
  const outcomes = createOutcomeRecorder();
  requireCondition(
    buildContract.next_version === "16.2.10" &&
      buildContract.react_version === "19.2.7" &&
      buildContract.react_dom_version === "19.2.7" &&
      buildContract.server_only_version === "0.0.1",
    "exact runtime versions were not observed",
  );
  outcomes.observe(
    "toolchain.exact_runtime_versions",
    "next=16.2.10 react=19.2.7 react_dom=19.2.7 server_only=0.0.1",
  );
  requireCondition(
    buildContract.typescript_version === "7.0.2" &&
      buildContract.typescript_negative_probe_rejected === true &&
      buildContract.typescript_project_typecheck_passed === true &&
      buildContract.next_legacy_typescript_api_bypassed === true,
    "TypeScript 7 effective gate was not observed",
  );
  outcomes.observe(
    "toolchain.typescript7_effective_gate",
    "typescript=7.0.2 negative_probe=rejected project=passed legacy_loader=bypassed",
  );
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
        const response = await fetchWithDeadline(
          `${nextServer.url}/api/relay`,
          { cache: "no-store" },
          fetchDeadlineMS,
        );
        requireSSE(response);
        const headers = serializeResponseHead(response);
        const reader = response.body.getReader();
        const first = await readFirstFrame(reader, streamDeadlineMS);
        metrics.timeToFirstFrameMs = performance.now() - startedAt;
        const request = fakeUpstream.requests.at(-1);
        requireCondition(
          request !== undefined,
          "upstream request was not observed",
        );
        requireCondition(
          request.terminalWrittenAt === null,
          "first downstream frame was buffered until the terminal event",
        );
        outcomes.observe(
          "stream.first_frame_unbuffered",
          "first frame arrived before the delayed terminal write",
        );
        requireCondition(
          first.frame === FIRST_FRAME,
          "first canonical SSE frame changed in transit",
        );
        const remainder = await readRemaining(
          reader,
          first.remainder,
          streamDeadlineMS,
        );
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
        outcomes.observe(
          "stream.ordered_canonical_frames",
          "canonical first and terminal frames arrived byte-for-byte in order",
        );
        downstreamResponses.push(headers + body);
      },
    );

    await context.test(
      "browser abort closes upstream transport without explicit cancellation",
      async () => {
        fakeUpstream.setMode("hold");
        const downstreamAbort = new AbortController();
        const response = await fetchWithDeadline(
          `${nextServer.url}/api/relay`,
          {
            cache: "no-store",
            signal: downstreamAbort.signal,
          },
          fetchDeadlineMS,
        );
        requireSSE(response);
        const headers = serializeResponseHead(response);
        const reader = response.body.getReader();
        const first = await readFirstFrame(reader, streamDeadlineMS);
        requireCondition(
          first.frame === FIRST_FRAME,
          "abort fixture did not deliver its initial frame",
        );
        const request = fakeUpstream.requests.at(-1);
        requireCondition(
          request !== undefined,
          "abort request was not observed",
        );
        const abortedAt = performance.now();
        downstreamAbort.abort();
        await assert.rejects(
          withTimeout(
            reader.read(),
            1_000,
            "downstream reader did not reject after abort",
          ),
          { name: "AbortError" },
        );
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
        outcomes.observe(
          "abort.upstream_transport_prompt",
          "upstream transport closed within 500ms",
        );
        requireCondition(
          fakeUpstream.cancelCalls === 0,
          "local transport abort invoked the explicit cancellation endpoint",
        );
        outcomes.observe(
          "abort.explicit_cancel_not_called",
          "explicit cancellation endpoint calls remained zero",
        );
        downstreamResponses.push(headers + first.frame + first.remainder);
      },
    );

    await context.test("forwards Last-Event-ID exactly on reconnect", async () => {
      fakeUpstream.setMode("resume");
      const lastEventID = "event-001:segment/9";
      const response = await fetchWithDeadline(
        `${nextServer.url}/api/relay`,
        {
          cache: "no-store",
          headers: { "Last-Event-ID": lastEventID },
        },
        fetchDeadlineMS,
      );
      requireSSE(response);
      const headers = serializeResponseHead(response);
      const body = await readRemaining(
        response.body.getReader(),
        "",
        streamDeadlineMS,
      );
      const request = fakeUpstream.requests.at(-1);
      requireCondition(
        request !== undefined,
        "reconnect request was not observed",
      );
      requireCondition(
        request.lastEventId === lastEventID,
        "Last-Event-ID changed between downstream and upstream",
      );
      requireCondition(
        body === RESUMED_FRAME,
        "resumed canonical SSE frame changed in transit",
      );
      outcomes.observe(
        "reconnect.last_event_id_exact",
        "Last-Event-ID reached the upstream unchanged",
      );
      downstreamResponses.push(headers + body);
    });

    await context.test(
      "returns a generic credential-free response for upstream failures",
      async () => {
        fakeUpstream.setMode("error");
        const response = await fetchWithDeadline(
          `${nextServer.url}/api/relay`,
          { cache: "no-store" },
          fetchDeadlineMS,
        );
        const headers = serializeResponseHead(response);
        requireCondition(
          response.body !== null,
          "error response did not have a body",
        );
        const body = await readRemaining(
          response.body.getReader(),
          "",
          streamDeadlineMS,
        );
        requireCondition(
          response.status === 502,
          "upstream failure was not mapped to 502",
        );
        requireCondition(
          body === "Upstream stream unavailable\n",
          "upstream failure details reached the downstream response",
        );
        outcomes.observe(
          "upstream.error_response_redacted",
          "upstream credential echo was replaced by the generic 502 response",
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
        outcomes.observe(
          "secret.upstream_authorization_only",
          "runtime credential appeared only in upstream Authorization",
        );
      },
    );

    requireCondition(
      nextServer.arguments.join(" ") === "start --hostname 127.0.0.1 --port",
      "integration tests did not launch next start",
    );
    outcomes.observe(
      "runtime.next_start",
      "integration server launched with next start",
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
      outcomes.observe(
        "harness.output_capture_bounded",
        "build and next-start output stayed within per-stream byte bounds",
      );
      const targets = collectSecretScanTargets({
        buildCapture,
        downstreamResponses,
        nextServer,
      });
      for (const [category, values] of Object.entries(targets)) {
        requireCondition(
          values.length > 0,
          `secret scan category is empty: ${category}`,
        );
        for (const value of values) {
          requireCondition(
            !value.includes(secret),
            `runtime credential leaked into scan category: ${category}`,
          );
        }
      }
      outcomes.observe(
        "secret.scan_targets_clean",
        "all required build, response, log, bundle, source, map, and chunk targets were clean",
      );
      const observedOutcomes = outcomes.complete();
      if (hasRunContext()) {
        writeRunObservation({
          buildContract,
          metrics,
          outcomes: observedOutcomes,
          targets,
        });
      }
    },
  );
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

function requireSSE(response) {
  requireCondition(response.status === 200, "relay did not return HTTP 200");
  requireCondition(
    response.body !== null,
    "relay response did not have a stream body",
  );
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
