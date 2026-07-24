import { test, expect } from "@playwright/test";

import { laneFor } from "../lib/timeline";

// The runnable check for the §47.2 lane table: if the mapping drifts (a tool event landing in the model
// lane, an approval folded into progress, a recovery transition mislabelled), the console's separation is
// broken and this fails. No browser needed — a pure-function assertion in the same `playwright test` run.
test("laneFor sorts canonical event types into the §47.2 lanes", () => {
  expect(laneFor("message.accepted.v1")).toBe("message");
  expect(laneFor("model_step.delta.v1")).toBe("model_step");
  expect(laneFor("tool_call.completed.v1")).toBe("tool");
  expect(laneFor("child.completed.v1")).toBe("tool");
  expect(laneFor("approval.requested.v1")).toBe("approval");
  expect(laneFor("approval.approved.v1")).toBe("approval");
  expect(laneFor("attempt.recovering.v1")).toBe("recovery");
  expect(laneFor("recovery.proof.v1")).toBe("recovery");
  expect(laneFor("workspace.restored.v1")).toBe("recovery");
  expect(laneFor("usage.updated.v1")).toBe("usage");
  expect(laneFor("response.in_progress.v1")).toBe("progress");
  expect(laneFor("run.completed.v1")).toBe("terminal");
  expect(laneFor("response.failed.v1")).toBe("terminal");
});
