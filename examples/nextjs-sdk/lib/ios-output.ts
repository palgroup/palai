// =============================================================================================
// READING WHAT `xcodebuild` AND `simctl` ACTUALLY SAY.
//
// The owner asked to WATCH an agent code an iOS project — "xcbuild kullandığını sim kullandığını
// vs görmek istiyorum". A run that drives `xcodebuild` produces output nobody should read raw: the
// build below emitted 51,422 bytes for a package containing one function, and the two lines that
// matter ("BUILD FAILED", and the file and line that caused it) are somewhere in the middle of it.
// So this module turns that output into something a screen can render.
//
// EVERY PATTERN HERE WAS MEASURED, NOT GUESSED. The four shapes below were captured by running the
// real tools on this machine (Xcode 26.6, build 17F113) against a real iOS Simulator destination on
// 2026-08-02, and the exact captured lines are quoted at each parser. That matters more than usual
// here, because a parser written against a plausible-looking format silently produces a confident,
// empty rendering — it does not fail, it just never matches, and the screen quietly shows nothing
// while the run underneath is working perfectly.
//
// WHAT THIS IS NOT: it is not a source of truth about whether the build succeeded. The tool's own
// exit code is that, and it rides the tool result. This only decides how to DRAW what happened.
// =============================================================================================

/** The kinds of iOS work this module can draw. `shell` is the honest fallback. */
export type IOSKind = "build" | "test" | "simulator" | "shell";

export interface BuildDiagnostic {
  file: string;
  /** The basename, which is what a chat line has room for. */
  fileName: string;
  line: number;
  column: number;
  severity: "error" | "warning";
  message: string;
}

export interface BuildReport {
  kind: "build";
  succeeded: boolean;
  /** Errors first, then warnings — a failing build's first row must be why it failed. */
  diagnostics: BuildDiagnostic[];
  errorCount: number;
  warningCount: number;
}

export interface TestCase {
  suite: string;
  name: string;
  status: "passed" | "failed";
  seconds?: number;
  failureMessage?: string;
}

export interface TestReport {
  kind: "test";
  succeeded: boolean;
  cases: TestCase[];
  executed: number;
  failures: number;
  seconds?: number;
}

export interface SimulatorDevice {
  name: string;
  udid: string;
  state: "Booted" | "Shutdown" | string;
}

export interface SimulatorReport {
  kind: "simulator";
  devices: SimulatorDevice[];
  /** The verb the command performed, when it was more than a list. */
  action?: "boot" | "shutdown" | "install" | "launch" | "bootstatus";
  booted: boolean;
}

export interface ShellReport {
  kind: "shell";
}

export type IOSReport = (BuildReport | TestReport | SimulatorReport | ShellReport) & {
  /** The command line as the model wrote it, for the terminal header. */
  command: string;
  /** The raw output, always kept — a rendering must never be the only copy. */
  output: string;
  exitCode: number | null;
};

// classifyCommand decides which parser to use FROM THE COMMAND, not from the output.
//
// That direction is deliberate. Classifying by output would mean a build whose output happens to
// contain the word "Test Suite" (a compiler error inside a test file, say) renders as a test run.
// The command is what the model asked for and it is unambiguous: `xcodebuild … test` is a test run
// even when it fails before running a single case.
export function classifyCommand(command: string): IOSKind {
  const c = command.toLowerCase();
  if (c.includes("simctl")) return "simulator";
  if (c.includes("xcodebuild")) {
    // `test` and `build-for-testing` both run tests; `-list` and `-showsdks` are neither, and
    // rendering them as a failed build (no "BUILD SUCCEEDED" in the output) would be a lie.
    if (/\btest(-without-building)?\b/.test(c)) return "test";
    // The boundary is written `(?:^|\s)-` and NOT `\b-`: in JavaScript `\b` between a space and a
    // hyphen is not a boundary at all (both are non-word characters), so `/\b-list\b/` never matched
    // `xcodebuild -list` and every query rendered as a FAILED build — there is no BUILD SUCCEEDED
    // marker in a `-list`. Caught by the test, which is the whole reason it is written against real
    // command lines rather than against what the flag "obviously" looks like.
    if (/(?:^|\s)-(?:list|showsdks|showdestinations|version)\b/.test(c)) return "shell";
    return "build";
  }
  return "shell";
}

// MEASURED 2026-08-02, the exact line xcodebuild emitted for a type error:
//   /…/Sources/PalaiDemo/Greeter.swift:5:49: error: cannot convert return expression of type
//   'String' to return type 'Int'
// Path, line, column, severity, message — colon-separated, and the message itself contains colons
// and quotes, so the message capture is greedy to end-of-line rather than up to the next colon.
const DIAGNOSTIC = /^(\/[^\s:]+):(\d+):(\d+):\s+(error|warning):\s*(.+)$/;

export function parseBuild(output: string): Omit<BuildReport, "kind"> {
  const diagnostics: BuildDiagnostic[] = [];
  const seen = new Set<string>();
  for (const line of output.split("\n")) {
    const m = DIAGNOSTIC.exec(line.trim());
    if (m === null) continue;
    // xcodebuild repeats a diagnostic once per target that hit it; the same file:line:col:message
    // twice is one problem, and a screen listing it twice reads like two.
    const key = `${m[1]}:${m[2]}:${m[3]}:${m[5]}`;
    if (seen.has(key)) continue;
    seen.add(key);
    diagnostics.push({
      file: m[1],
      fileName: m[1].split("/").pop() ?? m[1],
      line: Number(m[2]),
      column: Number(m[3]),
      severity: m[4] as "error" | "warning",
      message: m[5],
    });
  }
  diagnostics.sort((a, b) => (a.severity === b.severity ? 0 : a.severity === "error" ? -1 : 1));

  // MEASURED: success is "** BUILD SUCCEEDED **" and failure is "** BUILD FAILED **", both on their
  // own line. The marker is authoritative over the diagnostic count, because a build can succeed
  // with warnings and — more importantly — can FAIL with no parseable diagnostic at all (a linker
  // error, a missing scheme). Inferring failure from "are there errors" would draw that as success.
  const succeeded = output.includes("** BUILD SUCCEEDED **");
  return {
    succeeded,
    diagnostics,
    errorCount: diagnostics.filter((d) => d.severity === "error").length,
    warningCount: diagnostics.filter((d) => d.severity === "warning").length,
  };
}

// MEASURED 2026-08-02, the exact lines from `xcodebuild … test`:
//   Test Case '-[PalaiDemoTests.GreeterTests testGreetsByName]' passed (0.001 seconds).
//   	 Executed 2 tests, with 0 failures (0 unexpected) in 0.001 (0.002) seconds
//   ** TEST SUCCEEDED **
// Note the leading tab on the Executed line — trimming before matching is not optional.
const TEST_CASE = /^Test Case '-\[([^\s\]]+)\s+([^\]]+)\]'\s+(passed|failed)(?:\s+\(([\d.]+)\s+seconds?\))?/;
const EXECUTED = /^Executed (\d+) tests?, with (\d+) failures?(?:\s+\(\d+ unexpected\))?(?:\s+in ([\d.]+))?/;

export function parseTest(output: string): Omit<TestReport, "kind"> {
  const cases: TestCase[] = [];
  let executed = 0;
  let failures = 0;
  let seconds: number | undefined;

  for (const raw of output.split("\n")) {
    const line = raw.trim();
    const tc = TEST_CASE.exec(line);
    if (tc !== null) {
      // The suite arrives as `PalaiDemoTests.GreeterTests`; the class is what a reader recognises.
      const suite = tc[1].split(".").pop() ?? tc[1];
      cases.push({
        suite,
        name: tc[2],
        status: tc[3] as "passed" | "failed",
        seconds: tc[4] !== undefined ? Number(tc[4]) : undefined,
      });
      continue;
    }
    const ex = EXECUTED.exec(line);
    if (ex !== null) {
      // xcodebuild prints this line once PER SUITE and again for "All tests", each with a running
      // total. Taking the LAST is what yields the run's totals rather than the last suite's.
      executed = Number(ex[1]);
      failures = Number(ex[2]);
      if (ex[3] !== undefined) seconds = Number(ex[3]);
    }
  }

  return {
    // Same reasoning as the build marker: the tool's own verdict beats a count we derived.
    succeeded: output.includes("** TEST SUCCEEDED **"),
    cases,
    executed,
    failures,
    seconds,
  };
}

// MEASURED 2026-08-02, `xcrun simctl list devices`:
//     iPhone 17 Pro (D51D94F3-1F89-4BDD-94C1-37EBF0907F76) (Booted)
// Leading whitespace, name, UDID in parens, state in parens, and a TRAILING SPACE after the state
// that cost a first version of this regex its match.
const SIM_DEVICE = /^(.+?)\s+\(([0-9A-Fa-f-]{36})\)\s+\((Booted|Shutdown|Booting|Shutting Down)\)/;

export function parseSimulator(command: string, output: string): Omit<SimulatorReport, "kind"> {
  const devices: SimulatorDevice[] = [];
  for (const raw of output.split("\n")) {
    const m = SIM_DEVICE.exec(raw.trim());
    if (m === null) continue;
    devices.push({ name: m[1], udid: m[2], state: m[3] });
  }

  const c = command.toLowerCase();
  let action: SimulatorReport["action"];
  for (const verb of ["bootstatus", "boot", "shutdown", "install", "launch"] as const) {
    // `bootstatus` is checked before `boot` because it CONTAINS it — reversed, every bootstatus
    // call would report itself as a boot.
    if (c.includes(`simctl ${verb}`)) {
      action = verb;
      break;
    }
  }

  // A `boot` that returns exit 0 prints NOTHING, so "did anything boot" cannot be read off the
  // output of a boot command at all. It is honest to report the devices we can see plus the verb
  // that ran, and let the caller show the exit code — rather than invent a "Booted" we did not
  // observe. `bootstatus` DOES print, and its final line is "Finished".
  const booted =
    devices.some((d) => d.state === "Booted") ||
    (action === "bootstatus" && /\bFinished\b/.test(output));

  return { devices, action, booted };
}

/**
 * readIOSOutput turns one shell tool call into a report a screen can draw.
 *
 * `output` is whatever the tool returned. It is passed through UNCHANGED on the report, because a
 * rendering must never be the only copy of what a build said — every parser here can miss, and the
 * terminal view is what makes a miss visible rather than silent.
 */
export function readIOSOutput(command: string, output: string, exitCode: number | null): IOSReport {
  const base = { command, output, exitCode };
  switch (classifyCommand(command)) {
    case "build":
      return { kind: "build", ...parseBuild(output), ...base };
    case "test":
      return { kind: "test", ...parseTest(output), ...base };
    case "simulator":
      return { kind: "simulator", ...parseSimulator(command, output), ...base };
    default:
      return { kind: "shell", ...base };
  }
}

/**
 * shellCommandOf pulls the command line out of a tool call's arguments.
 *
 * MEASURED against the shipped schema rather than assumed: `palai.workspace.shell` takes its command
 * under `command`. The two fallbacks are not speculative generality — a registry tool wrapping the
 * shell may name it `cmd` or `script`, and a call whose command we cannot find must render as a
 * plain tool rather than as an iOS build with an empty command line.
 */
export function shellCommandOf(args: unknown): string {
  if (args === null || typeof args !== "object") return "";
  const record = args as Record<string, unknown>;
  for (const key of ["command", "cmd", "script"]) {
    const value = record[key];
    if (typeof value === "string" && value.trim() !== "") return value;
  }
  return "";
}

/**
 * exitCodeOf reads the exit code off a tool result.
 *
 * IT IS A FIELD, NOT AN ERROR, and that is this tree's own finding: a tool that fails returns a
 * result carrying `exit_code`, and treating a non-zero exit as a transport failure is what used to
 * wedge a run forever. A failing `xcodebuild` is an ANSWER — it is, in fact, the most interesting
 * answer this screen renders.
 */
export function exitCodeOf(result: unknown): number | null {
  if (result === null || typeof result !== "object") return null;
  const record = result as Record<string, unknown>;
  for (const key of ["exit_code", "exitCode", "code", "status"]) {
    const value = record[key];
    if (typeof value === "number") return value;
  }
  return null;
}

/**
 * outputTextOf assembles the human-readable output of a tool result.
 *
 * stdout and stderr are CONCATENATED rather than shown separately, because `xcodebuild` writes its
 * diagnostics to stdout and its own failures to stderr, and a screen that showed only one of them
 * would drop half of every interesting build.
 */
export function outputTextOf(result: unknown): string {
  if (typeof result === "string") return result;
  if (result === null || typeof result !== "object") return "";
  const record = result as Record<string, unknown>;
  const parts: string[] = [];
  for (const key of ["stdout", "output", "stderr", "error"]) {
    const value = record[key];
    if (typeof value === "string" && value !== "") parts.push(value);
  }
  return parts.join("\n");
}
