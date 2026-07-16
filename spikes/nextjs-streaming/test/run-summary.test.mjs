import assert from "node:assert/strict";
import { existsSync, mkdtempSync, readFileSync, rmSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import test from "node:test";
import {
  createOutcomeRecorder,
  expectedOutcomeNames,
  finalizeRunObservation,
  writeRunObservation,
} from "./run-summary.mjs";

test("outcome recorder requires each exact observed result once", () => {
  const incomplete = createOutcomeRecorder();
  incomplete.observe(expectedOutcomeNames[0], "observed");
  assert.throws(() => incomplete.complete(), /missing observed outcome/i);
  assert.throws(
    () => incomplete.observe(expectedOutcomeNames[0], "again"),
    /duplicate observed outcome/i,
  );
  assert.throws(
    () => incomplete.observe("unknown.observation", "observed"),
    /unexpected observed outcome/i,
  );

  const complete = createOutcomeRecorder();
  for (const name of expectedOutcomeNames) {
    complete.observe(name, `${name} observed`);
  }
  assert.deepEqual(
    complete.complete().map(({ name, passed }) => ({ name, passed })),
    expectedOutcomeNames.map((name) => ({ name, passed: true })),
  );
});

test("run summary becomes visible only after a successful process result", () => {
  const buildRoot = mkdtempSync(join(tmpdir(), "palai-run-summary-"));
  const environment = {
    PALAI_SPIKE_GIT_COMMIT: "a".repeat(40),
    PALAI_SPIKE_INVOCATION_ID: "run-12345678",
    PALAI_SPIKE_ITERATION: "1",
    PALAI_SPIKE_SOURCE_TREE: "b".repeat(40),
  };
  const outcomes = createOutcomeRecorder();
  for (const name of expectedOutcomeNames) {
    outcomes.observe(name, `${name} observed`);
  }

  try {
    const paths = writeRunObservation(
      {
        buildContract: { schema_version: 2 },
        metrics: {
          abortToUpstreamCloseMs: 4.5,
          timeToFirstFrameMs: 100.25,
        },
        outcomes: outcomes.complete(),
        targets: {
          build_output: ["scanned"],
          downstream_response: ["scanned"],
          next_server_log: ["scanned"],
          server_bundle: ["scanned"],
          source_file: ["scanned"],
          source_map: ["scanned"],
          static_chunk: ["scanned"],
        },
      },
      { buildRoot, environment },
    );
    assert.equal(existsSync(paths.observationPath), true);
    assert.equal(existsSync(paths.summaryPath), false);
    assert.throws(
      () => finalizeRunObservation(1, { buildRoot, environment }),
      /successful test process/i,
    );
    assert.equal(existsSync(paths.summaryPath), false);

    finalizeRunObservation(0, { buildRoot, environment });
    assert.equal(existsSync(paths.observationPath), false);
    const summary = JSON.parse(readFileSync(paths.summaryPath, "utf8"));
    assert.deepEqual(summary.process_result, { completed: true, exit_code: 0 });
    assert.equal(summary.git_commit, environment.PALAI_SPIKE_GIT_COMMIT);
    assert.equal(summary.source_tree, environment.PALAI_SPIKE_SOURCE_TREE);
    assert.equal(summary.invocation_id, environment.PALAI_SPIKE_INVOCATION_ID);
    assert.equal(summary.iteration, 1);
  } finally {
    rmSync(buildRoot, { force: true, recursive: true });
  }
});
