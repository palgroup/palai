import { createHash, randomBytes } from "node:crypto";
import { once } from "node:events";
import {
  chmodSync,
  existsSync,
  mkdirSync,
  readdirSync,
  readFileSync,
  rmSync,
  statSync,
  writeFileSync,
} from "node:fs";
import { connect, createServer } from "node:net";
import { dirname, join, relative } from "node:path";
import { fileURLToPath } from "node:url";
import { spawn } from "node:child_process";
import {
  appendCapture,
  newCapture,
  runProcess,
  signalProcessTree,
  supportsProcessGroups,
  terminateChild,
  waitForClose,
} from "./process-lifecycle.mjs";

const projectRoot = dirname(dirname(fileURLToPath(import.meta.url)));
const repositoryRoot = dirname(dirname(projectRoot));
const buildDirectory = join(projectRoot, ".build");
const buildCapturePath = join(buildDirectory, "next-build.json");
const buildContractPath = join(buildDirectory, "build-contract.json");
const secretPath = join(buildDirectory, "runtime-secret");
const nextDirectory = join(projectRoot, ".next");
const nextBuildIDPath = join(nextDirectory, "BUILD_ID");
const nextBinary = join(projectRoot, "node_modules", ".bin", "next");
const typeScriptBinary = join(projectRoot, "node_modules", ".bin", "tsc");
const serverLogLimitBytes = 256 * 1024;
const buildCommandDeadlineMS = 120_000;
const typeScriptCommandDeadlineMS = 30_000;
const toolCommandDeadlineMS = 15_000;

export const routePath = join(projectRoot, "app", "api", "relay", "route.ts");

export function hasProductionBuild() {
  if (
    !existsSync(buildCapturePath) ||
    !existsSync(buildContractPath) ||
    !existsSync(secretPath) ||
    !existsSync(nextBuildIDPath)
  ) {
    return false;
  }
  try {
    const contract = JSON.parse(readFileSync(buildContractPath, "utf8"));
    return contract.schema_version === 2 &&
      buildIdentityMatches(contract, currentBuildIdentity());
  } catch {
    return false;
  }
}

export function readProductionBuild() {
  ensure(hasProductionBuild(), "production build is stale or incomplete");
  return {
    buildCapture: JSON.parse(readFileSync(buildCapturePath, "utf8")),
    buildContract: JSON.parse(readFileSync(buildContractPath, "utf8")),
    secret: readFileSync(secretPath, "utf8").trim(),
  };
}

export async function captureProductionBuild() {
  rmSync(buildDirectory, { force: true, recursive: true });
  rmSync(nextDirectory, { force: true, recursive: true });
  mkdirSync(buildDirectory, { recursive: true });
  const secret = `palai-runtime-${randomBytes(32).toString("hex")}`;
  writeFileSync(secretPath, `${secret}\n`, { mode: 0o600 });
  chmodSync(secretPath, 0o600);

  const sourceFingerprint = computeSourceFingerprint();
  const buildContract = await verifyTypeScript7Gate();
  const capture = await runProcess(
    nextBinary,
    ["build"],
    {
      ...process.env,
      NEXT_TELEMETRY_DISABLED: "1",
      PALAI_SPIKE_API_KEY: secret,
      PALAI_SPIKE_UPSTREAM_URL: "http://127.0.0.1:9/stream",
    },
    buildCommandDeadlineMS,
  );
  writeFileSync(
    buildCapturePath,
    `${
      JSON.stringify(
        { stderr: capture.stderr, stdout: capture.stdout },
        null,
        2,
      )
    }\n`,
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
  ensure(existsSync(nextBuildIDPath), "next build did not produce BUILD_ID");
  const completedBuildContract = {
    ...buildContract,
    next_build_id: readFileSync(nextBuildIDPath, "utf8").trim(),
    source_fingerprint: sourceFingerprint,
  };
  writeFileSync(
    buildContractPath,
    `${JSON.stringify(completedBuildContract, null, 2)}\n`,
  );
}

async function verifyTypeScript7Gate() {
  const nextVersion = await runProcess(
    nextBinary,
    ["--version"],
    process.env,
    toolCommandDeadlineMS,
  );
  ensure(nextVersion.code === 0, "Next version check failed");
  const actualNextVersion = nextVersion.stdout.trim().replace(
    /^Next\.js v/,
    "",
  );
  ensure(actualNextVersion === "16.2.10", "unexpected Next version");
  const reactVersion = packageVersion("react");
  const reactDOMVersion = packageVersion("react-dom");
  const serverOnlyVersion = packageVersion("server-only");
  ensure(reactVersion === "19.2.7", "unexpected React version");
  ensure(reactDOMVersion === "19.2.7", "unexpected ReactDOM version");
  ensure(serverOnlyVersion === "0.0.1", "unexpected server-only version");

  const version = await runProcess(
    typeScriptBinary,
    ["--version"],
    process.env,
    toolCommandDeadlineMS,
  );
  ensure(version.code === 0, "TypeScript version check failed");
  const actualTypeScriptVersion = version.stdout.trim().replace(
    /^Version /,
    "",
  );
  ensure(actualTypeScriptVersion === "7.0.2", "unexpected TypeScript version");

  const probePath = join(buildDirectory, "typecheck-probe.ts");
  writeFileSync(probePath, "const mustBeString: string = 1;\n");
  let negative;
  try {
    negative = await runProcess(
      typeScriptBinary,
      ["--ignoreConfig", "--noEmit", "--pretty", "false", probePath],
      process.env,
      typeScriptCommandDeadlineMS,
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
    typeScriptCommandDeadlineMS,
  );
  process.stdout.write(project.stdout);
  process.stderr.write(project.stderr);
  ensure(project.code === 0, "TypeScript 7 project typecheck failed");

  return {
    next_version: actualNextVersion,
    next_legacy_typescript_api_bypassed: true,
    react_dom_version: reactDOMVersion,
    react_version: reactVersion,
    schema_version: 2,
    server_only_version: serverOnlyVersion,
    typescript_negative_probe_rejected: true,
    typescript_project_typecheck_passed: true,
    typescript_version: actualTypeScriptVersion,
  };
}

export function buildIdentityMatches(contract, expected) {
  return (
    typeof contract === "object" &&
    contract !== null &&
    /^[0-9a-f]{64}$/.test(contract.source_fingerprint) &&
    contract.source_fingerprint === expected.sourceFingerprint &&
    typeof contract.next_build_id === "string" &&
    contract.next_build_id.length > 0 &&
    contract.next_build_id === expected.nextBuildID
  );
}

function currentBuildIdentity() {
  return {
    nextBuildID: readFileSync(nextBuildIDPath, "utf8").trim(),
    sourceFingerprint: computeSourceFingerprint(),
  };
}

function computeSourceFingerprint() {
  const paths = [
    ...listFiles(join(projectRoot, "app")),
    ...listFiles(join(projectRoot, "lib")),
    join(projectRoot, "next.config.ts"),
    join(projectRoot, "package.json"),
    join(projectRoot, "tsconfig.json"),
    join(repositoryRoot, "pnpm-lock.yaml"),
    join(repositoryRoot, "pnpm-workspace.yaml"),
  ].sort();
  const digest = createHash("sha256");
  for (const path of paths) {
    ensure(
      existsSync(path),
      `build input is missing: ${relative(repositoryRoot, path)}`,
    );
    digest.update(relative(repositoryRoot, path));
    digest.update("\0");
    digest.update(readFileSync(path));
    digest.update("\0");
  }
  return digest.digest("hex");
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
      detached: supportsProcessGroups,
    },
  );
  const closed = waitForClose(child);
  let spawnError;
  child.once("error", (error) => {
    spawnError = error;
  });
  const capture = newCapture();
  child.stdout.setEncoding("utf8");
  child.stderr.setEncoding("utf8");
  child.stdout.on("data", (chunk) => {
    appendCapture(capture, "stdout", chunk, serverLogLimitBytes, () => {
      signalProcessTree(child, "SIGTERM");
    });
  });
  child.stderr.on("data", (chunk) => {
    appendCapture(capture, "stderr", chunk, serverLogLimitBytes, () => {
      signalProcessTree(child, "SIGTERM");
    });
  });
  try {
    await waitForListening(child, port, capture, secret, () => spawnError);
  } catch (error) {
    await terminateChild(child, closed);
    throw error;
  }

  return {
    arguments: ["start", "--hostname", "127.0.0.1", "--port"],
    capture,
    async stop() {
      const graceful = await terminateChild(child, closed);
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
    path.endsWith(".map")
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
  ensure(
    address !== null && typeof address !== "string",
    "failed to reserve a TCP port",
  );
  server.close();
  await once(server, "close");
  return address.port;
}

async function waitForListening(child, port, capture, secret, getSpawnError) {
  const deadline = Date.now() + 15_000;
  for (;;) {
    if (getSpawnError() !== undefined) {
      throw getSpawnError();
    }
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

function ensure(condition, message) {
  if (!condition) {
    throw new Error(message);
  }
}
