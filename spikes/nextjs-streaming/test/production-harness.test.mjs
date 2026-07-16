import assert from "node:assert/strict";
import { once } from "node:events";
import { mkdtempSync, readFileSync, rmSync } from "node:fs";
import { createServer } from "node:http";
import { tmpdir } from "node:os";
import { join } from "node:path";
import test from "node:test";
import { buildIdentityMatches } from "./production-harness.mjs";
import {
  fetchWithDeadline,
  readFirstFrame,
  readRemaining,
  runProcess,
  withTimeout,
} from "./process-lifecycle.mjs";

test("build identity rejects stale source and Next build fingerprints", () => {
  const expected = {
    nextBuildID: "next-build-123",
    sourceFingerprint: "a".repeat(64),
  };
  assert.equal(
    buildIdentityMatches(
      {
        next_build_id: expected.nextBuildID,
        source_fingerprint: expected.sourceFingerprint,
      },
      expected,
    ),
    true,
  );
  assert.equal(
    buildIdentityMatches(
      {
        next_build_id: expected.nextBuildID,
        source_fingerprint: "b".repeat(64),
      },
      expected,
    ),
    false,
  );
  assert.equal(
    buildIdentityMatches(
      {
        next_build_id: "stale-next-build",
        source_fingerprint: expected.sourceFingerprint,
      },
      expected,
    ),
    false,
  );
});

test("runProcess drains inherited stdio after the child exits", async () => {
  const tailDelayMS = 120;
  const descendant =
    `setTimeout(() => process.stdout.write("late-tail\\n"), ${tailDelayMS});`;
  const parent = [
    'const { spawn } = require("node:child_process");',
    `const child = spawn(process.execPath, ["-e", ${
      JSON.stringify(descendant)
    }], {`,
    '  stdio: ["ignore", "inherit", "inherit"],',
    "});",
    "child.unref();",
  ].join("\n");

  const startedAt = performance.now();
  const result = await runProcess(
    process.execPath,
    ["-e", parent],
    process.env,
  );

  assert.equal(result.code, 0);
  assert.match(result.stdout, /late-tail/);
  assert.ok(
    performance.now() - startedAt >= tailDelayMS - 30,
    "capture returned before inherited stdout drained",
  );
});

test("readRemaining cancels a stream that stalls after its first frame", async () => {
  let markClosed;
  const requestClosed = new Promise((resolve) => {
    markClosed = resolve;
  });
  const firstFrame = "id: fixture-1\ndata: first\n\n";
  const server = createServer((request, response) => {
    request.once("close", markClosed);
    response.writeHead(200, { "Content-Type": "text/event-stream" });
    response.write(firstFrame);
  });
  server.listen(0, "127.0.0.1");
  await once(server, "listening");
  const address = server.address();
  assert.ok(address !== null && typeof address !== "string");

  try {
    const response = await fetchWithDeadline(
      `http://127.0.0.1:${address.port}/stalled-remainder`,
      {},
      500,
    );
    const reader = response.body.getReader();
    const first = await readFirstFrame(reader, 500);
    assert.equal(first.frame, firstFrame);
    const startedAt = performance.now();
    await assert.rejects(
      readRemaining(reader, first.remainder, 100),
      /remaining SSE stream deadline/i,
    );
    assert.ok(
      performance.now() - startedAt < 700,
      "remaining-stream deadline was not bounded",
    );
    await withTimeout(requestClosed, 500, "stalled stream socket stayed open");
  } finally {
    server.closeAllConnections();
    server.close();
    await once(server, "close");
  }
});

test("stream deadlines fail closed when immediate reads starve timers", async () => {
  for (
    const [name, read] of [
      ["first", (reader) => readFirstFrame(reader, 1)],
      ["remaining", (reader) => readRemaining(reader, "", 1)],
    ]
  ) {
    let cancelled = false;
    let remainingReads = 100_000;
    const reader = {
      cancel() {
        cancelled = true;
        return Promise.resolve();
      },
      read() {
        if (remainingReads-- > 0) {
          return Promise.resolve({ done: false, value: new Uint8Array([97]) });
        }
        return Promise.resolve({ done: true, value: undefined });
      },
    };

    await assert.rejects(
      read(reader),
      new RegExp(`${name} SSE .*deadline`, "i"),
    );
    assert.equal(
      cancelled,
      true,
      `${name} reader was not cancelled at its deadline`,
    );
  }
});

test("readFirstFrame cancels a stream that never emits a frame", async () => {
  let markClosed;
  const requestClosed = new Promise((resolve) => {
    markClosed = resolve;
  });
  const server = createServer((request, response) => {
    request.once("close", markClosed);
    response.writeHead(200, { "Content-Type": "text/event-stream" });
    response.flushHeaders();
  });
  server.listen(0, "127.0.0.1");
  await once(server, "listening");
  const address = server.address();
  assert.ok(address !== null && typeof address !== "string");

  try {
    const response = await fetchWithDeadline(
      `http://127.0.0.1:${address.port}/silent-frame`,
      {},
      500,
    );
    const startedAt = performance.now();
    await assert.rejects(
      readFirstFrame(response.body.getReader(), 100),
      /first SSE frame deadline/i,
    );
    assert.ok(
      performance.now() - startedAt < 700,
      "first-frame deadline was not bounded",
    );
    await withTimeout(
      requestClosed,
      500,
      "timed-out stream socket stayed open",
    );
  } finally {
    server.closeAllConnections();
    server.close();
    await once(server, "close");
  }
});

test("runProcess enforces its deadline and reaps the process tree", async () => {
  const directory = mkdtempSync(join(tmpdir(), "palai-process-deadline-"));
  const pidPath = join(directory, "pids.json");
  const descendant = "setInterval(() => {}, 10_000);";
  const fixture = [
    'const { spawn } = require("node:child_process");',
    'const { writeFileSync } = require("node:fs");',
    `const child = spawn(process.execPath, ["-e", ${
      JSON.stringify(descendant)
    }], { stdio: "ignore" });`,
    "writeFileSync(process.env.PID_PATH, JSON.stringify([process.pid, child.pid]));",
    "setInterval(() => {}, 10_000);",
  ].join("\n");
  let pids = [];
  const watchdog = setTimeout(() => {
    try {
      pids = JSON.parse(readFileSync(pidPath, "utf8"));
      for (const pid of pids) {
        try {
          process.kill(pid, "SIGKILL");
        } catch {
          // The desired deadline cleanup may have already reaped it.
        }
      }
    } catch {
      // The assertion below reports a missing fixture receipt.
    }
  }, 2_000);

  try {
    const startedAt = performance.now();
    await assert.rejects(
      runProcess(
        process.execPath,
        ["-e", fixture],
        { ...process.env, PID_PATH: pidPath },
        500,
      ),
      /deadline/i,
    );
    assert.ok(
      performance.now() - startedAt < 1_500,
      "deadline was not fail-closed",
    );
    pids = JSON.parse(readFileSync(pidPath, "utf8"));
    for (const pid of pids) {
      assert.throws(() => process.kill(pid, 0), { code: "ESRCH" });
    }
  } finally {
    clearTimeout(watchdog);
    for (const pid of pids) {
      try {
        process.kill(pid, "SIGKILL");
      } catch {
        // Already reaped.
      }
    }
    rmSync(directory, { force: true, recursive: true });
  }
});

test("fetchWithDeadline aborts a request that never receives headers", async () => {
  let markClosed;
  const requestClosed = new Promise((resolve) => {
    markClosed = resolve;
  });
  const server = createServer((request) => {
    request.once("close", markClosed);
  });
  server.listen(0, "127.0.0.1");
  await once(server, "listening");
  const address = server.address();
  assert.ok(address !== null && typeof address !== "string");

  try {
    const startedAt = performance.now();
    await assert.rejects(
      fetchWithDeadline(`http://127.0.0.1:${address.port}/silent`, {}, 100),
      /fetch acquisition deadline/i,
    );
    assert.ok(
      performance.now() - startedAt < 700,
      "fetch deadline was not bounded",
    );
    await withTimeout(requestClosed, 500, "timed-out fetch socket stayed open");
  } finally {
    server.closeAllConnections();
    server.close();
    await once(server, "close");
  }
});
