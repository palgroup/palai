import {
  existsSync,
  mkdirSync,
  readFileSync,
  renameSync,
  unlinkSync,
  writeFileSync,
} from "node:fs";
import { dirname, join } from "node:path";
import { fileURLToPath } from "node:url";

const projectRoot = dirname(dirname(fileURLToPath(import.meta.url)));
const defaultBuildRoot = join(projectRoot, ".build");
const scanCategories = [
  "build_output",
  "downstream_response",
  "next_server_log",
  "server_bundle",
  "source_file",
  "source_map",
  "static_chunk",
];
const contextVariables = [
  "PALAI_SPIKE_GIT_COMMIT",
  "PALAI_SPIKE_INVOCATION_ID",
  "PALAI_SPIKE_ITERATION",
  "PALAI_SPIKE_SOURCE_TREE",
];

export const expectedOutcomeNames = [
  "abort.explicit_cancel_not_called",
  "abort.upstream_transport_prompt",
  "harness.output_capture_bounded",
  "reconnect.last_event_id_exact",
  "runtime.next_start",
  "secret.scan_targets_clean",
  "secret.upstream_authorization_only",
  "stream.first_frame_unbuffered",
  "stream.ordered_canonical_frames",
  "toolchain.exact_runtime_versions",
  "toolchain.typescript7_effective_gate",
  "upstream.error_response_redacted",
];

export function createOutcomeRecorder() {
  const expected = new Set(expectedOutcomeNames);
  const observed = new Map();
  return {
    complete() {
      for (const name of expectedOutcomeNames) {
        ensure(observed.has(name), `missing observed outcome: ${name}`);
      }
      return expectedOutcomeNames.map((name) => observed.get(name));
    },
    observe(name, detail) {
      ensure(expected.has(name), `unexpected observed outcome: ${name}`);
      ensure(!observed.has(name), `duplicate observed outcome: ${name}`);
      ensure(
        typeof detail === "string" && detail.trim() !== "",
        "outcome detail is required",
      );
      observed.set(name, { detail, name, passed: true });
    },
  };
}

export function hasRunContext(environment = process.env) {
  const present = contextVariables.filter((name) =>
    environment[name] !== undefined
  );
  if (present.length === 0) {
    return false;
  }
  ensure(
    present.length === contextVariables.length,
    "run context is incomplete",
  );
  return true;
}

export function writeRunObservation(
  { buildContract, metrics, outcomes, targets },
  { buildRoot = defaultBuildRoot, environment = process.env } = {},
) {
  const context = readRunContext(environment);
  validateOutcomes(outcomes);
  const abortMS = requiredMetric(
    metrics?.abortToUpstreamCloseMs,
    "abort latency",
  );
  const firstFrameMS = requiredMetric(
    metrics?.timeToFirstFrameMs,
    "first-frame latency",
  );
  ensure(abortMS < 500, "abort latency exceeded its bound");
  const scanTargets = {};
  ensure(
    targets !== null && typeof targets === "object",
    "secret scan targets are required",
  );
  ensure(
    Object.keys(targets).length === scanCategories.length,
    "secret scan target categories changed",
  );
  for (const category of scanCategories) {
    ensure(
      Array.isArray(targets[category]) && targets[category].length > 0,
      `empty scan target: ${category}`,
    );
    scanTargets[category] = targets[category].length;
  }

  const paths = runPaths(buildRoot, context);
  mkdirSync(paths.runDirectory, { recursive: true });
  ensure(!existsSync(paths.observationPath), "run observation already exists");
  ensure(
    !existsSync(paths.summaryPath),
    "finalized run summary already exists",
  );
  const observation = {
    abort_to_upstream_close_ms: roundMetric(abortMS),
    build_contract: buildContract,
    capture_limits: {
      command_output_bytes_per_stream: 1024 * 1024,
      next_server_log_bytes_per_stream: 256 * 1024,
    },
    git_commit: context.gitCommit,
    invocation_id: context.invocationID,
    iteration: context.iteration,
    outcomes,
    production_server: "next start",
    scan_targets: scanTargets,
    schema_version: 2,
    source_tree: context.sourceTree,
    time_to_first_frame_ms: roundMetric(firstFrameMS),
  };
  writeJSON(paths.observationPath, observation);
  return paths;
}

export function finalizeRunObservation(
  exitCode,
  { buildRoot = defaultBuildRoot, environment = process.env } = {},
) {
  ensure(
    exitCode === 0,
    "a successful test process is required to finalize evidence",
  );
  const context = readRunContext(environment);
  const paths = runPaths(buildRoot, context);
  ensure(existsSync(paths.observationPath), "run observation is missing");
  ensure(
    !existsSync(paths.summaryPath),
    "finalized run summary already exists",
  );
  const observation = JSON.parse(readFileSync(paths.observationPath, "utf8"));
  ensure(
    observation.git_commit === context.gitCommit,
    "run observation commit changed",
  );
  ensure(
    observation.source_tree === context.sourceTree,
    "run observation tree changed",
  );
  ensure(
    observation.invocation_id === context.invocationID,
    "run observation ID changed",
  );
  ensure(
    observation.iteration === context.iteration,
    "run observation iteration changed",
  );
  const summary = {
    ...observation,
    process_result: { completed: true, exit_code: exitCode },
  };
  const temporaryPath = `${paths.summaryPath}.tmp`;
  writeJSON(temporaryPath, summary);
  renameSync(temporaryPath, paths.summaryPath);
  unlinkSync(paths.observationPath);
  return paths.summaryPath;
}

function readRunContext(environment) {
  ensure(hasRunContext(environment), "run context is required");
  const gitCommit = environment.PALAI_SPIKE_GIT_COMMIT;
  const invocationID = environment.PALAI_SPIKE_INVOCATION_ID;
  const iteration = environment.PALAI_SPIKE_ITERATION;
  const sourceTree = environment.PALAI_SPIKE_SOURCE_TREE;
  ensure(/^[0-9a-f]{40}$/.test(gitCommit), "run commit is invalid");
  ensure(
    /^[A-Za-z0-9][A-Za-z0-9._-]{7,127}$/.test(invocationID),
    "invocation ID is invalid",
  );
  ensure(/^[1-9][0-9]*$/.test(iteration), "run iteration is invalid");
  ensure(/^[0-9a-f]{40}$/.test(sourceTree), "run source tree is invalid");
  return { gitCommit, invocationID, iteration: Number(iteration), sourceTree };
}

function runPaths(buildRoot, context) {
  const runDirectory = join(buildRoot, "runs", context.invocationID);
  return {
    observationPath: join(
      runDirectory,
      `observation-${context.iteration}.json`,
    ),
    runDirectory,
    summaryPath: join(runDirectory, `run-${context.iteration}.json`),
  };
}

function validateOutcomes(outcomes) {
  ensure(Array.isArray(outcomes), "observed outcomes are required");
  ensure(
    outcomes.length === expectedOutcomeNames.length,
    "observed outcome set is incomplete",
  );
  for (let index = 0; index < expectedOutcomeNames.length; index += 1) {
    const outcome = outcomes[index];
    ensure(
      outcome?.name === expectedOutcomeNames[index],
      "observed outcome order or name changed",
    );
    ensure(
      outcome.passed === true,
      `observed outcome did not pass: ${outcome?.name}`,
    );
    ensure(
      typeof outcome.detail === "string" && outcome.detail.trim() !== "",
      "outcome detail is required",
    );
  }
}

function requiredMetric(value, name) {
  ensure(
    Number.isFinite(value) && value >= 0,
    `${name} is required and must be finite`,
  );
  return value;
}

function roundMetric(value) {
  return Math.round(value * 1_000) / 1_000;
}

function writeJSON(path, value) {
  writeFileSync(path, `${JSON.stringify(value, null, 2)}\n`, { mode: 0o600 });
}

function ensure(condition, message) {
  if (!condition) {
    throw new Error(message);
  }
}
