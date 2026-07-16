import { spawn } from "node:child_process";
import { dirname } from "node:path";
import { fileURLToPath } from "node:url";

const projectRoot = dirname(dirname(fileURLToPath(import.meta.url)));
const commandOutputLimitBytes = 1024 * 1024;
const defaultCommandDeadlineMS = 30_000;
const processCleanupDeadlineMS = 5_000;

export const supportsProcessGroups = process.platform !== "win32";

export async function runProcess(
  command,
  arguments_,
  environment,
  deadlineMS = defaultCommandDeadlineMS,
) {
  ensure(
    Number.isFinite(deadlineMS) && deadlineMS > 0,
    "child process deadline must be positive",
  );
  const child = spawn(command, arguments_, {
    cwd: projectRoot,
    env: environment,
    stdio: ["ignore", "pipe", "pipe"],
    detached: supportsProcessGroups,
  });
  const closed = waitForClose(child);
  const capture = newCapture();
  let signalOverflow;
  const overflow = new Promise((resolve) => {
    signalOverflow = resolve;
  });
  child.stdout.setEncoding("utf8");
  child.stderr.setEncoding("utf8");
  child.stdout.on("data", (chunk) => {
    appendCapture(
      capture,
      "stdout",
      chunk,
      commandOutputLimitBytes,
      signalOverflow,
    );
  });
  child.stderr.on("data", (chunk) => {
    appendCapture(
      capture,
      "stderr",
      chunk,
      commandOutputLimitBytes,
      signalOverflow,
    );
  });
  let timer;
  const deadline = new Promise((resolve) => {
    timer = setTimeout(() => resolve({ type: "deadline" }), deadlineMS);
  });
  let signalSpawnError;
  const spawnError = new Promise((resolve) => {
    signalSpawnError = resolve;
  });
  const handleSpawnError = (error) =>
    signalSpawnError({ error, type: "error" });
  child.once("error", handleSpawnError);
  let outcome;
  try {
    outcome = await Promise.race([
      closed.then(({ code }) => ({ code, type: "close" })),
      spawnError,
      overflow.then(() => ({ type: "overflow" })),
      deadline,
    ]);
  } finally {
    clearTimeout(timer);
    child.removeListener("error", handleSpawnError);
  }
  if (outcome.type !== "close") {
    await terminateChild(child, closed);
    if (outcome.type === "overflow") {
      throw new Error("child process exceeded its configured output limit");
    }
    if (outcome.type === "deadline") {
      throw new Error(`child process exceeded its ${deadlineMS}ms deadline`);
    }
    throw outcome.error;
  }
  ensure(
    !capture.overflow,
    "child process exceeded its configured output limit",
  );
  return { ...capture, code: outcome.code };
}

export async function fetchWithDeadline(
  input,
  options = {},
  deadlineMS = 5_000,
) {
  ensure(
    Number.isFinite(deadlineMS) && deadlineMS > 0,
    "fetch acquisition deadline must be positive",
  );
  const deadlineController = new AbortController();
  const signal = options.signal
    ? AbortSignal.any([options.signal, deadlineController.signal])
    : deadlineController.signal;
  return withTimeout(
    fetch(input, { ...options, signal }),
    deadlineMS,
    `fetch acquisition deadline exceeded after ${deadlineMS}ms`,
    () =>
      deadlineController.abort(
        new Error("fetch acquisition deadline exceeded"),
      ),
  );
}

export async function readFirstFrame(reader, deadlineMS = 5_000) {
  ensure(
    Number.isFinite(deadlineMS) && deadlineMS > 0,
    "first SSE frame deadline must be positive",
  );
  const decoder = new TextDecoder();
  const deadlineAt = performance.now() + deadlineMS;
  const deadlineMessage =
    `first SSE frame deadline exceeded after ${deadlineMS}ms`;
  let text = "";
  while (!text.includes("\n\n")) {
    const remainingMS = remainingDeadlineMS(
      reader,
      deadlineAt,
      deadlineMessage,
    );
    const { done, value } = await readStreamChunk(
      reader,
      remainingMS,
      deadlineMessage,
    );
    ensure(!done, "SSE stream ended before its first frame");
    text += decoder.decode(value, { stream: true });
  }
  const boundary = text.indexOf("\n\n") + 2;
  return { frame: text.slice(0, boundary), remainder: text.slice(boundary) };
}

export async function readRemaining(reader, initial = "", deadlineMS = 5_000) {
  ensure(
    Number.isFinite(deadlineMS) && deadlineMS > 0,
    "remaining SSE stream deadline must be positive",
  );
  const decoder = new TextDecoder();
  const deadlineAt = performance.now() + deadlineMS;
  const deadlineMessage =
    `remaining SSE stream deadline exceeded after ${deadlineMS}ms`;
  let text = initial;
  for (;;) {
    const remainingMS = remainingDeadlineMS(
      reader,
      deadlineAt,
      deadlineMessage,
    );
    const { done, value } = await readStreamChunk(
      reader,
      remainingMS,
      deadlineMessage,
    );
    if (done) {
      return text + decoder.decode();
    }
    text += decoder.decode(value, { stream: true });
  }
}

export async function terminateChild(child, closed) {
  if (!supportsProcessGroups || child.pid === undefined) {
    return terminateDirectChild(child, closed);
  }

  // POSIX has no stable process-group handle. Capture the detached leader's
  // PGID once and use it only during this bounded cleanup window.
  const processGroupID = child.pid;
  signalProcessGroup(processGroupID, "SIGTERM");
  const leaderClosed = await closesWithin(closed, processCleanupDeadlineMS);
  if (leaderClosed && !processGroupExists(processGroupID)) {
    return true;
  }

  signalProcessGroup(processGroupID, "SIGKILL");
  const [leaderGone, processGroupGone] = await Promise.all([
    closesWithin(closed, processCleanupDeadlineMS),
    waitForProcessGroupExit(processGroupID, processCleanupDeadlineMS),
  ]);
  ensure(
    processGroupGone,
    `child process group ${processGroupID} could not be proven gone after SIGKILL`,
  );
  ensure(leaderGone, "child could not be closed after process-group SIGKILL");
  return false;
}

async function terminateDirectChild(child, closed) {
  signalProcessTree(child, "SIGTERM");
  try {
    await withTimeout(
      closed,
      processCleanupDeadlineMS,
      "child did not close after SIGTERM",
    );
    return true;
  } catch {
    signalProcessTree(child, "SIGKILL");
    await withTimeout(
      closed,
      processCleanupDeadlineMS,
      "child could not be closed after SIGKILL",
    );
    return false;
  }
}

export function waitForClose(child) {
  return new Promise((resolve) => {
    child.once("close", (code, signal) => resolve({ code, signal }));
  });
}

export function signalProcessTree(child, signal) {
  if (child.pid === undefined) {
    return false;
  }
  if (!supportsProcessGroups) {
    try {
      child.kill(signal);
      return true;
    } catch (error) {
      if (error?.code === "ESRCH") {
        return false;
      }
      throw error;
    }
  }
  return signalProcessGroup(child.pid, signal);
}

function signalProcessGroup(processGroupID, signal) {
  try {
    process.kill(-processGroupID, signal);
    return true;
  } catch (error) {
    if (error?.code === "ESRCH") {
      return false;
    }
    throw error;
  }
}

function processGroupExists(processGroupID) {
  try {
    process.kill(-processGroupID, 0);
    return true;
  } catch (error) {
    if (error?.code === "ESRCH") {
      return false;
    }
    if (error?.code === "EPERM") {
      return true;
    }
    throw error;
  }
}

async function waitForProcessGroupExit(processGroupID, deadlineMS) {
  const deadlineAt = performance.now() + deadlineMS;
  while (processGroupExists(processGroupID)) {
    const remainingMS = deadlineAt - performance.now();
    if (remainingMS <= 0) {
      return false;
    }
    await new Promise((resolve) =>
      setTimeout(resolve, Math.min(25, remainingMS))
    );
  }
  return true;
}

async function closesWithin(closed, deadlineMS) {
  try {
    await withTimeout(closed, deadlineMS, "child close deadline exceeded");
    return true;
  } catch {
    return false;
  }
}

export function newCapture() {
  return {
    overflow: false,
    stderr: "",
    stderrBytes: 0,
    stdout: "",
    stdoutBytes: 0,
  };
}

export function appendCapture(capture, stream, chunk, limit, onOverflow) {
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

export function withTimeout(promise, milliseconds, message, onTimeout) {
  let timer;
  const timeout = new Promise((_, reject) => {
    timer = setTimeout(() => {
      reject(new Error(message));
      onTimeout?.();
    }, milliseconds);
  });
  return Promise.race([promise, timeout]).finally(() => clearTimeout(timer));
}

function readStreamChunk(reader, deadlineMS, message) {
  return withTimeout(reader.read(), deadlineMS, message, () => {
    void reader.cancel(new Error(message)).catch(() => undefined);
  });
}

function remainingDeadlineMS(reader, deadlineAt, message) {
  const remainingMS = deadlineAt - performance.now();
  if (remainingMS <= 0) {
    void reader.cancel(new Error(message)).catch(() => undefined);
    throw new Error(message);
  }
  return remainingMS;
}

function ensure(condition, message) {
  if (!condition) {
    throw new Error(message);
  }
}
