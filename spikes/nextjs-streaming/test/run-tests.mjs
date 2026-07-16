import {
  captureProductionBuild,
  hasProductionBuild,
} from "./production-harness.mjs";
import { runProcess } from "./process-lifecycle.mjs";
import { finalizeRunObservation, hasRunContext } from "./run-summary.mjs";

if (!hasProductionBuild()) {
  await captureProductionBuild();
}

const result = await runProcess(
  process.execPath,
  [
    "--test",
    "--test-concurrency=1",
    "test/production-harness.test.mjs",
    "test/run-summary.test.mjs",
    "test/relay.test.mjs",
  ],
  process.env,
  60_000,
);
process.stdout.write(result.stdout);
process.stderr.write(result.stderr);

if (result.code === 0 && hasRunContext()) {
  finalizeRunObservation(result.code);
}
if (result.code !== 0) {
  process.exitCode = result.code ?? 1;
}
