import { readFileSync } from "node:fs";
import { join } from "node:path";

import { expect, test } from "@playwright/test";

import {
  classifyCommand,
  exitCodeOf,
  outputTextOf,
  parseBuild,
  parseSimulator,
  parseTest,
  readIOSOutput,
  shellCommandOf,
} from "@/lib/ios-output";

// =============================================================================================
// THE PARSERS, AGAINST BYTES THE REAL TOOLS PRODUCED.
//
// Every fixture in tests/fixtures/ was captured by running `xcodebuild` and `xcrun simctl` on this
// machine (Xcode 26.6, build 17F113) against a real iOS Simulator destination on 2026-08-02. They
// are trimmed to the lines a parser looks at — the full build was 24,708 bytes and the full test run
// 51,234 — but every retained line is verbatim.
//
// WHY THAT MATTERS MORE THAN USUAL HERE. A parser written against a plausible-looking format does
// not fail when it is wrong; it matches nothing and returns an empty, confident result. The screen
// then shows a build with no errors while the build underneath is failing. Hand-written fixtures
// would reproduce whatever the author imagined the format to be, and the test would agree with the
// author rather than with Xcode.
// =============================================================================================

const fixture = (name: string) => readFileSync(join(__dirname, "fixtures", name), "utf8");

test.describe("classifying the command", () => {
  // Classification is from the COMMAND, not the output — a build whose output happens to contain
  // "Test Suite" (a compiler error inside a test file) must not render as a test run.
  test("reads the verb rather than the output", () => {
    expect(classifyCommand("xcodebuild -scheme PalaiDemo build")).toBe("build");
    expect(classifyCommand("xcodebuild -scheme PalaiDemo test")).toBe("test");
    expect(classifyCommand("xcrun simctl boot 'iPhone 17 Pro'")).toBe("simulator");
    expect(classifyCommand("git -C repo status")).toBe("shell");
  });

  // `-list` produces no BUILD SUCCEEDED marker. Classified as a build it would render as a FAILED
  // build every single time, which is the exact shape of a confidently wrong rendering.
  test("an xcodebuild query is not a build", () => {
    expect(classifyCommand("xcodebuild -list")).toBe("shell");
    expect(classifyCommand("xcodebuild -showsdks")).toBe("shell");
    expect(classifyCommand("xcodebuild -version")).toBe("shell");
  });
});

test.describe("xcodebuild build", () => {
  test("a real successful build reports success and no diagnostics", () => {
    const report = parseBuild(fixture("xcodebuild-build-ok.txt"));
    expect(report.succeeded).toBe(true);
    expect(report.errorCount).toBe(0);
    expect(report.diagnostics).toHaveLength(0);
  });

  // THE ONE THE SCREEN EXISTS FOR: a failing build, with the file and line that caused it. The
  // fixture is the real Swift type error xcodebuild emitted.
  test("a real failing build names the file, the line and the reason", () => {
    const report = parseBuild(fixture("xcodebuild-build-fail.txt"));
    expect(report.succeeded).toBe(false);
    expect(report.errorCount).toBe(1);

    const [first] = report.diagnostics;
    expect(first.fileName).toBe("Greeter.swift");
    expect(first.line).toBe(5);
    expect(first.column).toBe(49);
    expect(first.severity).toBe("error");
    // The message contains colons and quotes; a parser that split on ":" would truncate it here.
    expect(first.message).toBe("cannot convert return expression of type 'String' to return type 'Int'");
  });

  // The marker is authoritative over the diagnostic count, because a build can fail with NO
  // parseable diagnostic at all — a linker error, a missing scheme. Inferring success from "no
  // errors found" would draw those as green.
  test("a failure with no parseable diagnostic is still a failure", () => {
    const report = parseBuild("** BUILD FAILED **\nld: symbol(s) not found for architecture arm64\n");
    expect(report.succeeded).toBe(false);
    expect(report.diagnostics).toHaveLength(0);
  });

  test("a build that succeeded with warnings is still a success", () => {
    const report = parseBuild(
      "/tmp/x/Sources/A.swift:3:9: warning: variable 'y' was never used\n** BUILD SUCCEEDED **\n",
    );
    expect(report.succeeded).toBe(true);
    expect(report.warningCount).toBe(1);
    expect(report.errorCount).toBe(0);
  });

  // xcodebuild repeats a diagnostic once per target that hit it. Listing it twice reads as two
  // problems, and an operator counts rows.
  test("the same diagnostic twice is one problem", () => {
    const line = "/tmp/x/Sources/A.swift:5:49: error: cannot convert return expression\n";
    const report = parseBuild(line + line + "** BUILD FAILED **\n");
    expect(report.diagnostics).toHaveLength(1);
    expect(report.errorCount).toBe(1);
  });

  test("errors sort above warnings so the first row is why it failed", () => {
    const report = parseBuild(
      "/tmp/x/A.swift:1:1: warning: unused\n/tmp/x/B.swift:2:2: error: broken\n** BUILD FAILED **\n",
    );
    expect(report.diagnostics[0].severity).toBe("error");
  });
});

test.describe("xcodebuild test", () => {
  test("a real test run reports every case and the totals", () => {
    const report = parseTest(fixture("xcodebuild-test-ok.txt"));
    expect(report.succeeded).toBe(true);
    expect(report.executed).toBe(2);
    expect(report.failures).toBe(0);

    expect(report.cases).toHaveLength(2);
    expect(report.cases.map((c) => c.name)).toEqual(["testGreetsByName", "testGreetsEmpty"]);
    // The suite arrives as `PalaiDemoTests.GreeterTests`; the class is what a reader recognises.
    expect(report.cases[0].suite).toBe("GreeterTests");
    expect(report.cases[0].status).toBe("passed");
  });

  // xcodebuild prints "Executed N tests" once per suite AND again for "All tests", each a running
  // total. Taking the FIRST would report one suite's numbers as the run's.
  test("the totals are the run's, not the first suite's", () => {
    const report = parseTest(
      [
        "Test Case '-[Pkg.ATests testOne]' passed (0.001 seconds).",
        "\t Executed 1 test, with 0 failures (0 unexpected) in 0.001 (0.002) seconds",
        "Test Case '-[Pkg.BTests testTwo]' failed (0.003 seconds).",
        "\t Executed 3 tests, with 1 failure (0 unexpected) in 0.004 (0.005) seconds",
        "** TEST FAILED **",
      ].join("\n"),
    );
    expect(report.executed).toBe(3);
    expect(report.failures).toBe(1);
    expect(report.succeeded).toBe(false);
  });

  // The real output indents that line with a TAB. A parser that matched without trimming finds
  // nothing and reports a run of zero tests.
  test("the Executed line is found despite its leading tab", () => {
    const report = parseTest("\t Executed 7 tests, with 2 failures (0 unexpected) in 1.5 (1.6) seconds");
    expect(report.executed).toBe(7);
    expect(report.failures).toBe(2);
  });

  test("a failing case is recorded as failed", () => {
    const report = parseTest("Test Case '-[Pkg.ATests testBad]' failed (0.02 seconds).\n** TEST FAILED **");
    expect(report.cases[0].status).toBe("failed");
    expect(report.succeeded).toBe(false);
  });
});

test.describe("simctl", () => {
  test("a real device list reports each device and its state", () => {
    const report = parseSimulator("xcrun simctl list devices", fixture("simctl-list.txt"));
    expect(report.devices.length).toBeGreaterThan(0);

    const named = report.devices.find((d) => d.name === "iPhone 17 Pro");
    expect(named).toBeDefined();
    // A real UDID, not a placeholder — the regex requires the 36-character form.
    expect(named?.udid).toMatch(/^[0-9A-F-]{36}$/i);
    expect(["Booted", "Shutdown"]).toContain(named?.state);
  });

  // `bootstatus` CONTAINS `boot`. Checked in the wrong order every bootstatus call reports itself
  // as a boot, and the screen says a simulator was started when nothing was.
  test("bootstatus is not read as a boot", () => {
    expect(parseSimulator("xcrun simctl bootstatus 'iPhone 17 Pro'", "").action).toBe("bootstatus");
    expect(parseSimulator("xcrun simctl boot 'iPhone 17 Pro'", "").action).toBe("boot");
  });

  // A successful `simctl boot` prints NOTHING, so "did it boot" is unanswerable from its output.
  // Reporting booted:true there would be inventing an observation.
  test("a silent boot does not claim a booted device", () => {
    const report = parseSimulator("xcrun simctl boot 'iPhone 17 Pro'", "");
    expect(report.booted).toBe(false);
    expect(report.devices).toHaveLength(0);
  });

  test("a bootstatus that finished is a booted device", () => {
    const report = parseSimulator(
      "xcrun simctl bootstatus 'iPhone 17 Pro'",
      "[2026-08-02 15:05:05 +0000] Status=4294967295, isTerminal=YES, Elapsed=00:08.\n\tFinished\n",
    );
    expect(report.booted).toBe(true);
  });
});

test.describe("reading a tool call", () => {
  test("the raw output always survives the parse", () => {
    const raw = fixture("xcodebuild-build-fail.txt");
    const report = readIOSOutput("xcodebuild -scheme PalaiDemo build", raw, 65);
    expect(report.kind).toBe("build");
    // A rendering must never be the only copy: every parser here can miss, and the terminal view
    // is what makes a miss visible rather than silent.
    expect(report.output).toBe(raw);
    expect(report.exitCode).toBe(65);
  });

  test("the command line is read from the shell tool's own argument", () => {
    expect(shellCommandOf({ command: "xcodebuild build" })).toBe("xcodebuild build");
    expect(shellCommandOf({ nothing: 1 })).toBe("");
    expect(shellCommandOf(null)).toBe("");
  });

  // A non-zero exit is a RESULT FIELD, not a transport failure — this tree's own finding, and the
  // most interesting answer this screen renders.
  test("a non-zero exit code is read as an answer", () => {
    expect(exitCodeOf({ exit_code: 65 })).toBe(65);
    expect(exitCodeOf({ exit_code: 0 })).toBe(0);
    expect(exitCodeOf({})).toBeNull();
  });

  // xcodebuild writes diagnostics to stdout and its own failures to stderr; showing one drops half
  // of every interesting build.
  test("stdout and stderr are both kept", () => {
    const text = outputTextOf({ stdout: "** BUILD FAILED **", stderr: "xcodebuild: error: no scheme" });
    expect(text).toContain("** BUILD FAILED **");
    expect(text).toContain("no scheme");
  });
});

test.describe("the shell tool's REAL argument shape", () => {
  // MEASURED on a live run 2026-08-02. `palai.workspace.shell` sends an ARGV ARRAY, and the first
  // version of shellCommandOf looked only for a `command` string — the shape the fake invented. So
  // against the live control plane it returned "", the detail part rendered nothing, and every iOS
  // card silently vanished. No error, no blank card: no card.
  test("an argv array from the live shell tool yields the command", () => {
    const live = {
      argv: [
        "bash",
        "-c",
        'cd repo && xcodebuild -scheme PalaiDemo -destination "platform=iOS Simulator,name=iPhone 17 Pro" build',
      ],
    };
    const command = shellCommandOf(live);
    expect(command).toContain("xcodebuild");
    // And it must CLASSIFY, which is the property that actually mattered — a command the classifier
    // never sees renders as nothing at all.
    expect(classifyCommand(command)).toBe("build");
  });

  // `bash -c` puts the script in the element after the flag. Joining the whole array would classify
  // on a string starting with "bash" and put that on the terminal header, which is not what the
  // model asked to run.
  test("the -c script is unwrapped rather than joined", () => {
    expect(shellCommandOf({ argv: ["bash", "-c", "xcrun simctl list devices"] })).toBe(
      "xcrun simctl list devices",
    );
    // A plain argv with no -c is joined, because there is no inner script to unwrap.
    expect(shellCommandOf({ argv: ["git", "status", "--short"] })).toBe("git status --short");
  });

  test("an empty or malformed argv falls through rather than inventing a command", () => {
    expect(shellCommandOf({ argv: [] })).toBe("");
    expect(shellCommandOf({ argv: [1, 2] })).toBe("");
    // The string form still works — a registry tool wrapping the shell may pass one.
    expect(shellCommandOf({ command: "xcodebuild build" })).toBe("xcodebuild build");
  });

  // The live result carries these seven keys; the renderer must read the two it needs off the real
  // shape rather than off the one the fixture happened to have.
  test("the live result shape yields its exit code and output", () => {
    const live = { stderr: "", stdout: "** BUILD SUCCEEDED **", exit_code: 0, timed_out: false, truncated: false, oom_killed: false, duration_ms: 38016 };
    expect(exitCodeOf(live)).toBe(0);
    expect(outputTextOf(live)).toContain("** BUILD SUCCEEDED **");
  });
});
