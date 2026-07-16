import { randomBytes } from "node:crypto";
import { once } from "node:events";
import {
  chmodSync,
  existsSync,
  mkdirSync,
  readFileSync,
  readdirSync,
  rmSync,
  statSync,
  writeFileSync,
} from "node:fs";
import { connect, createServer } from "node:net";
import { dirname, join } from "node:path";
import { fileURLToPath } from "node:url";
import { spawn } from "node:child_process";

const projectRoot = dirname(dirname(fileURLToPath(import.meta.url)));
const buildDirectory = join(projectRoot, ".build");
const buildCapturePath = join(buildDirectory, "next-build.json");
const buildContractPath = join(buildDirectory, "build-contract.json");
const secretPath = join(buildDirectory, "runtime-secret");
const nextBinary = join(projectRoot, "node_modules", ".bin", "next");
const typeScriptBinary = join(projectRoot, "node_modules", ".bin", "tsc");
const commandOutputLimitBytes = 1024 * 1024;
const serverLogLimitBytes = 256 * 1024;

export const routePath = join(projectRoot, "app", "api", "relay", "route.ts");

export function hasProductionBuild() {
  return (
    existsSync(buildCapturePath) &&
    existsSync(buildContractPath) &&
    existsSync(secretPath)
  );
}

export function readProductionBuild() {
  return {
    buildCapture: JSON.parse(readFileSync(buildCapturePath, "utf8")),
    buildContract: JSON.parse(readFileSync(buildContractPath, "utf8")),
    secret: readFileSync(secretPath, "utf8").trim(),
  };
}

export async function captureProductionBuild() {
  rmSync(buildDirectory, { force: true, recursive: true });
  mkdirSync(buildDirectory, { recursive: true });
  const secret = `palai-runtime-${randomBytes(32).toString("hex")}`;
  writeFileSync(secretPath, `${secret}\n`, { mode: 0o600 });
  chmodSync(secretPath, 0o600);

  const buildContract = await verifyTypeScript7Gate();
  const capture = await runProcess(nextBinary, ["build"], {
    ...process.env,
    NEXT_TELEMETRY_DISABLED: "1",
    PALAI_SPIKE_API_KEY: secret,
    PALAI_SPIKE_UPSTREAM_URL: "http://127.0.0.1:9/stream",
  });
  writeFileSync(
    buildCapturePath,
    `${JSON.stringify({ stderr: capture.stderr, stdout: capture.stdout }, null, 2)}\n`,
  );
  process.stdout.write(capture.stdout.replaceAll(secret, "[redacted]"));
  process.stderr.write(capture.stderr.replaceAll(secret, "[redacted]"));
  ensure(
    !capture.stdout.includes(secret) && !capture.stderr.includes(secret),
    "runtime credential leaked into captured next build output",
  );
  ensure(capture.code === 0, "next build failed");
  ensure(
    capture.stdout.includes("Detected @typescript/native-preview") &&
      capture.stdout.includes("Skipping validation of types"),
    "Next did not use the explicit TypeScript 7 compatibility boundary",
  );
  writeFileSync(
    buildContractPath,
    `${JSON.stringify(buildContract, null, 2)}\n`,
  );
}

async function verifyTypeScript7Gate() {
  const nextVersion = await runProcess(nextBinary, ["--version"], process.env);
  ensure(nextVersion.code === 0, "Next version check failed");
  const actualNextVersion = nextVersion.stdout.trim().replace(/^Next\.js v/, "");
  ensure(actualNextVersion === "16.2.10", "unexpected Next version");
  const reactVersion = packageVersion("react");
  const reactDOMVersion = packageVersion("react-dom");
  const serverOnlyVersion = packageVersion("server-only");
  ensure(reactVersion === "19.2.7", "unexpected React version");
  ensure(reactDOMVersion === "19.2.7", "unexpected ReactDOM version");
  ensure(serverOnlyVersion === "0.0.1", "unexpected server-only version");

  const version = await runProcess(typeScriptBinary, ["--version"], process.env);
  ensure(version.code === 0, "TypeScript version check failed");
  const actualTypeScriptVersion = version.stdout.trim().replace(/^Version /, "");
  ensure(actualTypeScriptVersion === "7.0.2", "unexpected TypeScript version");

  const probePath = join(buildDirectory, "typecheck-probe.ts");
  writeFileSync(probePath, 'const mustBeString: string = 1;\n');
  let negative;
  try {
    negative = await runProcess(
      typeScriptBinary,
      ["--ignoreConfig", "--noEmit", "--pretty", "false", probePath],
      process.env,
    );
  } finally {
    rmSync(probePath, { force: true });
  }
  ensure(
    negative.code !== 0 &&
      `${negative.stdout}\n${negative.stderr}`.includes("typecheck-probe.ts"),
    "TypeScript 7 negative typecheck probe was not rejected",
  );

  const project = await runProcess(
    typeScriptBinary,
    ["--noEmit", "--pretty", "false"],
    process.env,
  );
  process.stdout.write(project.stdout);
  process.stderr.write(project.stderr);
  ensure(project.code === 0, "TypeScript 7 project typecheck failed");

  return {
    next_version: actualNextVersion,
    next_legacy_typescript_api_bypassed: true,
    react_dom_version: reactDOMVersion,
    react_version: reactVersion,
    schema_version: 1,
    server_only_version: serverOnlyVersion,
    typescript_negative_probe_rejected: true,
    typescript_project_typecheck_passed: true,
    typescript_version: actualTypeScriptVersion,
  };
}

function packageVersion(name) {
  const path = join(projectRoot, "node_modules", name, "package.json");
  return JSON.parse(readFileSync(path, "utf8")).version;
}

export async function startNextServer({ secret, upstreamURL }) {
  const port = await reservePort();
  const child = spawn(
    nextBinary,
    ["start", "--hostname", "127.0.0.1", "--port", String(port)],
    {
      cwd: projectRoot,
      env: {
        ...process.env,
        NEXT_TELEMETRY_DISABLED: "1",
        PALAI_SPIKE_API_KEY: secret,
        PALAI_SPIKE_UPSTREAM_URL: upstreamURL,
      },
      stdio: ["ignore", "pipe", "pipe"],
    },
  );
  const capture = newCapture();
  child.stdout.setEncoding("utf8");
  child.stderr.setEncoding("utf8");
  child.stdout.on("data", (chunk) => {
    appendCapture(capture, "stdout", chunk, serverLogLimitBytes, () => {
      child.kill("SIGTERM");
    });
  });
  child.stderr.on("data", (chunk) => {
    appendCapture(capture, "stderr", chunk, serverLogLimitBytes, () => {
      child.kill("SIGTERM");
    });
  });
  try {
    await waitForListening(child, port, capture, secret);
  } catch (error) {
    await terminateChild(child);
    throw error;
  }

  return {
    arguments: ["start", "--hostname", "127.0.0.1", "--port"],
    capture,
    async stop() {
      const graceful = await terminateChild(child);
      ensure(graceful, "next start required SIGKILL during cleanup");
    },
    url: `http://127.0.0.1:${port}`,
  };
}

export function collectSecretScanTargets({
  buildCapture,
  downstreamResponses,
  nextServer,
}) {
  const sourcePaths = [
    routePath,
    join(projectRoot, "lib", "server-client.ts"),
    join(projectRoot, "next.config.ts"),
    join(projectRoot, "package.json"),
  ];
  const sourceMapPaths = listFiles(join(projectRoot, ".next")).filter((path) =>
    path.endsWith(".map"),
  );
  const serverBundlePaths = listFiles(join(projectRoot, ".next", "server"));
  const staticChunkPaths = listFiles(join(projectRoot, ".next", "static"));
  return {
    build_output: [buildCapture.stdout, buildCapture.stderr],
    downstream_response: downstreamResponses,
    next_server_log: [nextServer.capture.stdout, nextServer.capture.stderr],
    server_bundle: serverBundlePaths.map((path) => readFileSync(path, "utf8")),
    source_file: sourcePaths.map((path) => readFileSync(path, "utf8")),
    source_map: sourceMapPaths.map((path) => readFileSync(path, "utf8")),
    static_chunk: staticChunkPaths.map((path) => readFileSync(path, "utf8")),
  };
}

export function writeRunSummary({ assertionCount, buildContract, metrics, targets }) {
  const iteration = process.env.PALAI_SPIKE_ITERATION ?? "1";
  ensure(/^\d+$/.test(iteration), "spike iteration must be numeric");
  const scanTargets = Object.fromEntries(
    Object.entries(targets).map(([category, values]) => [category, values.length]),
  );
  const summary = {
    abort_to_upstream_close_ms: roundMetric(metrics.abortToUpstreamCloseMs),
    assertion_count: assertionCount,
    build_contract: buildContract,
    capture_limits: {
      command_output_bytes_per_stream: commandOutputLimitBytes,
      next_server_log_bytes_per_stream: serverLogLimitBytes,
    },
    production_server: "next start",
    scan_targets: scanTargets,
    schema_version: 1,
    time_to_first_frame_ms: roundMetric(metrics.timeToFirstFrameMs),
  };
  writeFileSync(
    join(buildDirectory, `run-${iteration}.json`),
    `${JSON.stringify(summary, null, 2)}\n`,
  );
}

function listFiles(root) {
  if (!existsSync(root)) {
    return [];
  }
  const paths = [];
  for (const entry of readdirSync(root, { withFileTypes: true })) {
    const path = join(root, entry.name);
    if (entry.isDirectory()) {
      paths.push(...listFiles(path));
    } else if (entry.isFile() && statSync(path).size >= 0) {
      paths.push(path);
    }
  }
  return paths.sort();
}

async function reservePort() {
  const server = createServer();
  server.listen(0, "127.0.0.1");
  await once(server, "listening");
  const address = server.address();
  ensure(address !== null && typeof address !== "string", "failed to reserve a TCP port");
  server.close();
  await once(server, "close");
  return address.port;
}

async function waitForListening(child, port, capture, secret) {
  const deadline = Date.now() + 15_000;
  for (;;) {
    if (child.exitCode !== null || child.signalCode !== null) {
      const safeOutput = `${capture.stdout}\n${capture.stderr}`.replaceAll(
        secret,
        "[redacted]",
      );
      throw new Error(`next start exited before listening\n${safeOutput}`);
    }
    if (await canConnect(port)) {
      return;
    }
    if (Date.now() >= deadline) {
      throw new Error("next start did not listen within 15 seconds");
    }
    await new Promise((resolve) => setTimeout(resolve, 25));
  }
}

async function terminateChild(child) {
  if (child.exitCode !== null || child.signalCode !== null) {
    return true;
  }
  let exited = once(child, "exit");
  child.kill("SIGTERM");
  try {
    await withTimeout(exited, 5_000, "child did not stop after SIGTERM");
    return true;
  } catch {
    if (child.exitCode === null && child.signalCode === null) {
      exited = once(child, "exit");
      child.kill("SIGKILL");
      await withTimeout(exited, 5_000, "child could not be reaped after SIGKILL");
    }
    return false;
  }
}

function canConnect(port) {
  return new Promise((resolve) => {
    const probe = connect({ host: "127.0.0.1", port });
    probe.setTimeout(250);
    probe.on("connect", () => {
      probe.destroy();
      resolve(true);
    });
    probe.on("error", () => {
      probe.destroy();
      resolve(false);
    });
    probe.on("timeout", () => {
      probe.destroy();
      resolve(false);
    });
  });
}

async function runProcess(command, arguments_, environment) {
  const child = spawn(command, arguments_, {
    cwd: projectRoot,
    env: environment,
    stdio: ["ignore", "pipe", "pipe"],
  });
  const capture = newCapture();
  let signalOverflow;
  const overflow = new Promise((resolve) => {
    signalOverflow = resolve;
  });
  child.stdout.setEncoding("utf8");
  child.stderr.setEncoding("utf8");
  child.stdout.on("data", (chunk) => {
    appendCapture(capture, "stdout", chunk, commandOutputLimitBytes, () => {
      signalOverflow();
    });
  });
  child.stderr.on("data", (chunk) => {
    appendCapture(capture, "stderr", chunk, commandOutputLimitBytes, () => {
      signalOverflow();
    });
  });
  const outcome = await Promise.race([
    once(child, "exit").then(([code]) => ({ code, type: "exit" })),
    once(child, "error").then(([error]) => ({ error, type: "error" })),
    overflow.then(() => ({ type: "overflow" })),
  ]);
  if (outcome.type === "overflow") {
    await terminateChild(child);
    throw new Error("child process exceeded its configured output limit");
  }
  if (outcome.type === "error") {
    throw outcome.error;
  }
  ensure(!capture.overflow, "child process exceeded its configured output limit");
  return { ...capture, code: outcome.code };
}

function newCapture() {
  return {
    overflow: false,
    stderr: "",
    stderrBytes: 0,
    stdout: "",
    stdoutBytes: 0,
  };
}

function appendCapture(capture, stream, chunk, limit, onOverflow) {
  if (capture.overflow) {
    return;
  }
  const byteCount = Buffer.byteLength(chunk);
  const bytesKey = `${stream}Bytes`;
  if (capture[bytesKey] + byteCount > limit) {
    capture.overflow = true;
    onOverflow();
    return;
  }
  capture[stream] += chunk;
  capture[bytesKey] += byteCount;
}

export function withTimeout(promise, milliseconds, message) {
  let timer;
  const timeout = new Promise((_, reject) => {
    timer = setTimeout(() => reject(new Error(message)), milliseconds);
  });
  return Promise.race([promise, timeout]).finally(() => clearTimeout(timer));
}

function roundMetric(value) {
  return Math.round(value * 1_000) / 1_000;
}

function ensure(condition, message) {
  if (!condition) {
    throw new Error(message);
  }
}
