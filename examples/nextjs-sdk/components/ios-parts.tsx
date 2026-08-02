"use client";

import {
  CheckCircle2,
  CircleDot,
  Hammer,
  Smartphone,
  TerminalSquare,
  TriangleAlert,
  XCircle,
} from "lucide-react";

import {
  Terminal,
  TerminalContent,
  TerminalCopyButton,
  TerminalHeader,
  TerminalStatus,
  TerminalTitle,
} from "@/components/ai-elements/terminal";
import {
  TestResults,
  TestResultsContent,
  TestResultsHeader,
  TestResultsSummary,
} from "@/components/ai-elements/test-results";
import { Badge } from "@/components/ui/badge";
import { Collapsible, CollapsibleContent, CollapsibleTrigger } from "@/components/ui/collapsible";
import type { BuildReport, IOSReport, SimulatorReport, TestReport } from "@/lib/ios-output";

// =============================================================================================
// DRAWING AN iOS RUN.
//
// The owner asked to WATCH the agent build an iOS project. A build that emits 24,708 bytes and a
// test run that emits 51,234 are not something to paste into a chat bubble, so each kind of work
// gets the shape that answers the question a human actually has about it:
//
//   a build       -> did it compile, and if not, WHICH FILE AND LINE
//   a test run    -> how many ran, how many failed, and which ones
//   the simulator -> which device, and is it up
//   anything else -> a terminal, honestly labelled
//
// THE RAW OUTPUT IS ALWAYS ONE CLICK AWAY, on every one of them. Each parser in lib/ios-output.ts
// can miss — Xcode changes its wording, a toolchain differs, a diagnostic arrives in a shape the
// regex does not know — and a summary with no way back to the bytes turns every miss into a screen
// that quietly shows nothing. The disclosure is what makes a parse failure visible instead.
// =============================================================================================

export function IOSPart({ report }: { report: IOSReport }) {
  switch (report.kind) {
    case "build":
      return <BuildPart report={report as BuildReport & IOSReport} />;
    case "test":
      return <TestPart report={report as TestReport & IOSReport} />;
    case "simulator":
      return <SimulatorPart report={report as SimulatorReport & IOSReport} />;
    default:
      return <ShellPart report={report} />;
  }
}

// BuildPart answers "did it compile", and when it did not, leads with the file and line.
function BuildPart({ report }: { report: BuildReport & IOSReport }) {
  const ok = report.succeeded;
  return (
    <div
      data-testid="ios-build"
      data-succeeded={ok ? "true" : "false"}
      className="overflow-hidden rounded-lg border border-border"
    >
      <div className="flex items-center gap-2 border-border border-b bg-muted/40 px-3 py-2">
        <Hammer className="size-3.5 shrink-0 text-muted-foreground" aria-hidden />
        <span className="font-medium text-[13px]">xcodebuild</span>
        {ok ? (
          <Badge variant="secondary" className="gap-1 text-[11px]" data-testid="ios-build-status">
            <CheckCircle2 className="size-3" aria-hidden />
            BUILD SUCCEEDED
          </Badge>
        ) : (
          <Badge variant="destructive" className="gap-1 text-[11px]" data-testid="ios-build-status">
            <XCircle className="size-3" aria-hidden />
            BUILD FAILED
          </Badge>
        )}
        {/* Counts are rendered only when non-zero: "0 warnings" is noise on a clean build, and a
            failing build's row should carry its error count and nothing competing with it. */}
        {report.errorCount > 0 ? (
          <span className="text-[11px] text-muted-foreground">
            {report.errorCount} error{report.errorCount === 1 ? "" : "s"}
          </span>
        ) : null}
        {report.warningCount > 0 ? (
          <span className="text-[11px] text-muted-foreground">
            {report.warningCount} warning{report.warningCount === 1 ? "" : "s"}
          </span>
        ) : null}
      </div>

      {report.diagnostics.length > 0 ? (
        <ul className="divide-y divide-border" data-testid="ios-build-diagnostics">
          {report.diagnostics.map((d, i) => (
            <li key={`${d.file}:${d.line}:${d.column}:${i}`} className="flex gap-2 px-3 py-2">
              {d.severity === "error" ? (
                <XCircle className="mt-0.5 size-3.5 shrink-0 text-destructive" aria-hidden />
              ) : (
                <TriangleAlert className="mt-0.5 size-3.5 shrink-0 text-muted-foreground" aria-hidden />
              )}
              <div className="min-w-0">
                {/* The basename and the line are the whole point of this row — an operator scanning a
                    failure is looking for WHERE, and a full DerivedData path pushes it off screen. The
                    full path is on the title attribute for anyone who needs it. */}
                <p className="font-mono text-[12px]" data-testid="ios-build-location" title={d.file}>
                  {d.fileName}:{d.line}:{d.column}
                </p>
                <p className="break-words text-[12px] text-muted-foreground">{d.message}</p>
              </div>
            </li>
          ))}
        </ul>
      ) : null}

      {/* A FAILING BUILD WITH NOTHING PARSED IS THE CASE THIS EXISTS FOR. A linker error, a missing
          scheme, or a diagnostic in a shape the regex does not know all land here — and without this
          line the card would say "BUILD FAILED" over an empty body and look broken rather than
          informative. It points at the disclosure that does have the answer. */}
      {!ok && report.diagnostics.length === 0 ? (
        <p className="px-3 py-2 text-[12px] text-muted-foreground" data-testid="ios-build-unparsed">
          The build failed without a diagnostic this screen could read — open the output below for
          what xcodebuild actually said.
        </p>
      ) : null}

      <RawOutput report={report} />
    </div>
  );
}

// TestPart answers "how many ran and how many failed", then names the failures.
function TestPart({ report }: { report: TestReport & IOSReport }) {
  const passed = report.executed - report.failures;
  return (
    <div data-testid="ios-test" data-succeeded={report.succeeded ? "true" : "false"}>
      {/* MEASURED, not assumed: TestResultsSummary takes NO count props — it reads them from
          TestResultsContext, which `TestResults` provides from its own `summary` prop. Passing
          passed/failed/total to the summary (the obvious-looking call) renders an empty header,
          because the context is undefined and the component returns null. */}
      <TestResults
        summary={{
          passed,
          failed: report.failures,
          skipped: 0,
          total: report.executed,
          duration: typeof report.seconds === "number" ? Math.round(report.seconds * 1000) : undefined,
        }}
      >
        <TestResultsHeader>
          <TestResultsSummary />
        </TestResultsHeader>
        <TestResultsContent>
          <ul className="divide-y divide-border">
            {report.cases.map((c, i) => (
              <li
                key={`${c.suite}.${c.name}-${i}`}
                className="flex items-center gap-2 px-3 py-1.5"
                data-testid="ios-test-case"
                data-status={c.status}
              >
                {c.status === "passed" ? (
                  <CheckCircle2 className="size-3.5 shrink-0 text-muted-foreground" aria-hidden />
                ) : (
                  <XCircle className="size-3.5 shrink-0 text-destructive" aria-hidden />
                )}
                <span className="font-mono text-[12px]">{c.name}</span>
                <span className="text-[11px] text-muted-foreground">{c.suite}</span>
                {typeof c.seconds === "number" ? (
                  <span className="ml-auto text-[11px] text-muted-foreground tabular-nums">
                    {c.seconds.toFixed(3)}s
                  </span>
                ) : null}
              </li>
            ))}
          </ul>
        </TestResultsContent>
      </TestResults>
      <RawOutput report={report} />
    </div>
  );
}

// SimulatorPart answers "which device, and is it up".
function SimulatorPart({ report }: { report: SimulatorReport & IOSReport }) {
  return (
    <div data-testid="ios-simulator" className="overflow-hidden rounded-lg border border-border">
      <div className="flex items-center gap-2 border-border border-b bg-muted/40 px-3 py-2">
        <Smartphone className="size-3.5 shrink-0 text-muted-foreground" aria-hidden />
        <span className="font-medium text-[13px]">simctl</span>
        {report.action ? (
          <Badge variant="secondary" className="text-[11px]" data-testid="ios-sim-action">
            {report.action}
          </Badge>
        ) : null}
        {report.booted ? (
          <Badge variant="secondary" className="gap-1 text-[11px]" data-testid="ios-sim-booted">
            <CircleDot className="size-3" aria-hidden />
            Booted
          </Badge>
        ) : null}
      </div>

      {report.devices.length > 0 ? (
        <ul className="divide-y divide-border" data-testid="ios-sim-devices">
          {report.devices.map((d) => (
            <li key={d.udid} className="flex items-center gap-2 px-3 py-1.5" data-state={d.state}>
              <span className="text-[12px]">{d.name}</span>
              <span className="font-mono text-[10px] text-muted-foreground">{d.udid.slice(0, 8)}…</span>
              <span
                className={
                  d.state === "Booted"
                    ? "ml-auto text-[11px] text-foreground"
                    : "ml-auto text-[11px] text-muted-foreground"
                }
              >
                {d.state}
              </span>
            </li>
          ))}
        </ul>
      ) : (
        // A successful `simctl boot` prints NOTHING. Saying so is the honest rendering: the
        // alternative is an empty card that reads as "no devices", which is a different claim.
        <p className="px-3 py-2 text-[12px] text-muted-foreground" data-testid="ios-sim-silent">
          {report.exitCode === 0
            ? "simctl completed with no output, which is what a successful boot looks like."
            : "simctl listed no devices."}
        </p>
      )}
      <RawOutput report={report} />
    </div>
  );
}

// ShellPart is the fallback, and it is labelled as a plain command rather than dressed up as iOS
// work — a `git status` rendered under an Xcode hammer would be a small lie told forty times.
function ShellPart({ report }: { report: IOSReport }) {
  const failed = report.exitCode !== null && report.exitCode !== 0;
  return (
    // Terminal takes its text as a PROP and provides it through context; TerminalContent renders it
    // from there rather than from children. Passing the output as a child compiles only because
    // TerminalContent accepts children, and then shows nothing.
    <Terminal data-testid="ios-shell" output={report.output || "(no output)"} autoScroll={false}>
      <TerminalHeader>
        <TerminalTitle>
          <TerminalSquare className="mr-1.5 inline size-3.5" aria-hidden />
          {report.command || "shell"}
        </TerminalTitle>
        <TerminalStatus data-testid="ios-shell-status">
          {failed ? `exit ${report.exitCode}` : "exit 0"}
        </TerminalStatus>
        <TerminalCopyButton />
      </TerminalHeader>
      <TerminalContent />
    </Terminal>
  );
}

// RawOutput is the disclosure every summary above carries.
//
// It is COLLAPSED by default and it is always present. Collapsed, because 24,708 bytes of build log
// is not what a chat is for; always present, because the summary above it is derived and every
// derivation can be wrong. The label carries the exit code, which is the one fact on this card that
// came from the tool rather than from a regex.
function RawOutput({ report }: { report: IOSReport }) {
  return (
    <Collapsible>
      <CollapsibleTrigger
        data-testid="ios-raw-toggle"
        className="flex w-full items-center gap-2 border-border border-t px-3 py-1.5 text-[11px] text-muted-foreground hover:bg-muted/40"
      >
        <TerminalSquare className="size-3" aria-hidden />
        <span className="font-mono">{report.command}</span>
        <span className="ml-auto tabular-nums">
          {report.exitCode === null ? "no exit code" : `exit ${report.exitCode}`}
        </span>
      </CollapsibleTrigger>
      <CollapsibleContent>
        <pre
          data-testid="ios-raw-output"
          className="max-h-64 overflow-auto bg-muted/30 px-3 py-2 font-mono text-[11px] leading-relaxed"
        >
          {report.output || "(no output)"}
        </pre>
      </CollapsibleContent>
    </Collapsible>
  );
}
