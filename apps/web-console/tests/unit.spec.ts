import { test, expect } from "@playwright/test";

import { refusalText } from "../components/ApprovalRow";
import { SecretField } from "../components/SecretField";
import {
  compactTokens,
  elapsedStamp,
  eventState,
  eventSubject,
  formatDuration,
  isFailureEvent,
  LANE_LABEL,
  positionOf,
  relativeTime,
} from "../lib/sessions";
import { WARNING_SCREENS, warningsFor } from "../lib/deployment";
import { CONSOLE_ROUTES } from "../lib/routes";
import { LANE_ORDER, positionAt, spanMark, staveOf, windowOf } from "../lib/strip";
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

// PASTE IS NOT BLOCKED, AND THE CHECK READS THE COMPONENT'S PROPS (E25 T4, plan §3.5 N12 / WCAG 2.2 §3.3.8).
//
// It cannot be a DOM assertion: a paste handler leaves no attribute behind, so a rendered field that silently
// swallows Ctrl-V looks identical to one that does not. THE FIRST DRAFT OF THIS TEST SCANNED THE SOURCE TEXT
// FOR "onPaste" AND FAILED ON THE COMPONENT'S OWN COMMENT SAYING IT HAS NONE — the same defeat this tree has
// shipped four times with substring comparisons, arriving here as a false positive instead of a false green.
// So the element tree is walked and the input's ACTUAL props are read: a comment cannot be a prop.
//
// Blocking paste on a credential field means retyping forty random characters by eye into a value nobody can
// read back to verify — the cognitive test 3.3.8 exists to forbid, and a source of undetectable typos.
//
// It also pins the token, because `off` is the WRONG answer rather than a weaker one: browsers ignore it on
// password fields and MDN reserves it for CAPTCHA / one-time-token fields, while `new-password` is documented
// to prevent an EXISTING password being autofilled — here, the operator's own console password being dropped
// into a box whose contents an agent will then use as a credential.
test("SecretField does not block paste or copy, and asks for new-password rather than off", () => {
  const input = findNode(SecretField({ inputRef: { current: null }, label: "Value", testId: "value-secret-input" }), "input");
  // A shape change in the transform must FAIL rather than pass over nothing.
  expect(input, "no <input> was found in SecretField's element tree — this assertion would be vacuous").not.toBe(undefined);
  const props = input as Record<string, unknown>;

  expect(props.onPaste, "SecretField blocks paste — WCAG 2.2 §3.3.8 counts paste as a mechanism that SATISFIES it").toBe(undefined);
  expect(props.onCopy, "SecretField interferes with copy").toBe(undefined);
  expect(props.onCut, "SecretField interferes with cut").toBe(undefined);
  expect(props.onKeyDown, "SecretField intercepts keys — the only reason to would be to block one").toBe(undefined);
  expect(props.autoComplete).toBe("new-password");
  expect(props.type).toBe("password");
  // NO value and NO defaultValue: the secret is a DOM node, never React state. Either prop would put a
  // credential into the component tree and into every render's closure.
  expect(props.value, "SecretField is controlled — the secret would be React state").toBe(undefined);
  expect(props.defaultValue, "SecretField seeds the field with bytes").toBe(undefined);
});

/**
 * findNode walks a JSX element tree and returns the PROPS of the first node of `tag`.
 *
 * It reads `type`/`props`/`children` only, which both React elements and Playwright's JSX transform expose —
 * this file is a .ts, so the .tsx components it imports are compiled by whichever transform is active, and
 * the caller asserts that a node was actually found so a transform change cannot make this pass over nothing.
 */
function findNode(node: unknown, tag: string): Record<string, unknown> | undefined {
  if (Array.isArray(node)) {
    for (const child of node) {
      const hit = findNode(child, tag);
      if (hit !== undefined) return hit;
    }
    return undefined;
  }
  if (node === null || typeof node !== "object") return undefined;
  const el = node as { type?: unknown; props?: Record<string, unknown> };
  if (el.type === tag) return el.props ?? {};
  return el.props === undefined ? undefined : findNode(el.props.children, tag);
}

// THE FOUR TYPED REFUSALS OF THE APPROVAL SURFACE, AS A PURE FUNCTION (E25 T5, plan §T5).
//
// The browser suite drives three of these end to end (a 404, a 403 from the approver list, and a 409 from a
// binding that genuinely went stale). The fourth — `insufficient_scope` — is NOT drivable from a browser: the
// console holds ONE key for the life of its process, and the same capability gates reading this queue and
// deciding on it, so a key that could render the page can always decide on it. Asserting it here is the honest
// alternative to a fixture that pretends otherwise: the MAPPING is what the console owns, and this is that
// mapping, over the exact `code` strings api/approvals.go writes.
//
// AND THE PLAN'S §T5 SAYS THREE REFUSALS. It is FOUR: 403 arrives with two independent codes, from two
// independent gates (`authorize()` on the key's `approve` capability; the project's approver list), and the
// operator's fix is different for each. Collapsing them would be the same flattening this test exists to refuse,
// one level up.
test("the approval queue gives every typed refusal its own sentence, and none of them is a generic failure", () => {
  const sentences = new Map<string, string>();
  for (const [code, status] of [
    ["invalid_request", 400],
    ["insufficient_scope", 403],
    ["not_an_approver", 403],
    ["not_found", 404],
    ["approval_not_decidable", 409],
  ] as const) {
    // The detail is deliberately BLAND, exactly as the fixture serves it: a console that echoed the server's
    // prose would produce five near-identical shrugs and fail the distinctness assertion below.
    sentences.set(code, refusalText({ code, status, detail: "refused" }));
  }

  expect(sentences.get("invalid_request")).toContain("carried no request hash");
  expect(sentences.get("insufficient_scope")).toContain("approve");
  expect(sentences.get("not_an_approver")).toContain("approver list");
  expect(sentences.get("not_found")).toContain("no longer exists");
  expect(sentences.get("approval_not_decidable")).toContain("can no longer be decided");

  const all = [...sentences.values()];
  expect(new Set(all).size, "two typed refusals produced the same sentence").toBe(all.length);
  for (const sentence of all) {
    expect(/something went wrong|unexpected error|an error occurred/i.test(sentence), `a typed refusal was flattened: ${sentence}`).toBe(false);
    expect(sentence).not.toBe("refused");
  }

  // A code this console has never met still names the code and the status rather than shrugging — the last line
  // of api/approvals.go's problem space is "some other refusal", and that has to read as one too.
  const unknown = refusalText({ code: "teapot_error", status: 418, detail: "the server is a teapot" });
  expect(unknown).toContain("teapot_error");
  expect(unknown).toContain("418");
  expect(unknown).toContain("the server is a teapot");
});

// THE SESSION SCREENS' DERIVATIONS (E29), driven with no browser.
//
// Every value on the two session screens that is not a field verbatim is one of these functions: a relative
// stamp, a compact token figure, a duration, an elapsed offset, a scrubber position. They live in
// lib/sessions.ts precisely so they can be driven here — a derivation written inline in JSX is a derivation
// whose edge cases are only ever exercised by whatever the fixture happens to contain.
test("relativeTime reads like a list, and a null is an em dash rather than an Invalid Date", () => {
  const now = Date.parse("2026-07-31T12:00:00Z");
  expect(relativeTime("2026-07-31T11:59:40Z", now)).toBe("just now");
  expect(relativeTime("2026-07-31T11:30:00Z", now)).toBe("30 minutes ago");
  expect(relativeTime("2026-07-31T02:00:00Z", now)).toBe("10 hours ago");
  expect(relativeTime("2026-07-29T12:00:00Z", now)).toBe("2 days ago");
  // A session whose row says it never ran, and one whose timestamp is unreadable, are the SAME thing to a
  // reader — a value that is not there — and neither may render as NaN or "Invalid Date".
  expect(relativeTime(null, now)).toBe("—");
  expect(relativeTime("", now)).toBe("—");
  expect(relativeTime("not a timestamp", now)).toBe("—");
});

test("compactTokens keeps small counts exact and only abbreviates past a thousand", () => {
  // 44 and 161 are what a fake-provider run actually meters on this stack; "0.0k" is not a smaller way to
  // write 44, which is why the cut is at a thousand rather than lower.
  expect(compactTokens(44)).toBe("44");
  expect(compactTokens(999)).toBe("999");
  expect(compactTokens(1000)).toBe("1k");
  expect(compactTokens(22_500)).toBe("22.5k");
  expect(compactTokens(1_250_000)).toBe("1.3M");
});

test("formatDuration distinguishes a session that lasted zero from one that never ran", () => {
  expect(formatDuration(19_900)).toBe("19.9s");
  expect(formatDuration(940)).toBe("940ms");
  expect(formatDuration(124_000)).toBe("2m 04s");
  expect(formatDuration(4_320_000)).toBe("1h 12m");
  // THE DISTINCTION session.json spells out: null is "a session with no duration, not one lasting zero".
  expect(formatDuration(0)).toBe("0ms");
  expect(formatDuration(null)).toBe("—");
});

test("elapsedStamp is the transcript's offset column, floored at the journal's first frame", () => {
  expect(elapsedStamp(0)).toBe("0:00:00");
  expect(elapsedStamp(4_000)).toBe("0:00:04");
  expect(elapsedStamp(3_671_000)).toBe("1:01:11");
  // A frame timestamped before the span's start is a clock disagreement, not a negative elapsed time.
  expect(elapsedStamp(-5_000)).toBe("0:00:00");
});

test("positionOf never places a scrubber mark outside its own track", () => {
  const span = { start: 1000, end: 5000 };
  expect(positionOf("1970-01-01T00:00:01.000Z", span)).toBe(0);
  expect(positionOf("1970-01-01T00:00:03.000Z", span)).toBe(50);
  expect(positionOf("1970-01-01T00:00:05.000Z", span)).toBe(100);
  // A ZERO-LENGTH SPAN IS THE INTERESTING ONE: every frame of a fast failure shares one timestamp, and a
  // division by zero would put every mark at `left: NaN%`, which is to say nowhere.
  expect(positionOf("1970-01-01T00:00:01.000Z", { start: 1000, end: 1000 })).toBe(0);
  expect(positionOf("", span)).toBe(0);
  expect(positionOf("1970-01-01T00:00:00.000Z", null)).toBe(0);
});

test("isFailureEvent is the error pill's predicate — terminal is not the same as failed", () => {
  expect(isFailureEvent("run.failed.v1")).toBe(true);
  expect(isFailureEvent("run.canceled.v1")).toBe(true);
  expect(isFailureEvent("response.timed_out.v1")).toBe(true);
  expect(isFailureEvent("approval.denied.v1")).toBe(true);
  // run.completed.v1 IS terminal and is the good outcome; a pill on it would mark every finished run as an
  // error, which is exactly what "terminal" would have meant if it were the predicate.
  expect(isFailureEvent("run.completed.v1")).toBe(false);
  expect(isFailureEvent("model_step.created.v1")).toBe(false);
});

test("eventSubject reads the identifier a frame carries, and admits when there is none", () => {
  // The two shapes the live journal actually serves, measured 2026-07-31.
  expect(eventSubject({ run_id: "run_1", state: "queued" })).toBe("run_1");
  expect(eventSubject({ model_request_id: "mreq_1", run_id: "run_1" })).toBe("mreq_1");
  expect(eventState({ run_id: "run_1", state: "queued" })).toBe("queued");
  expect(eventState({ model_request_id: "mreq_1" })).toBe("");
  // A frame with nothing to identify gets an empty string, which the cell renders as an em dash. There is no
  // fallback to a made-up label: "the frame carries no subject" is a fact worth showing.
  expect(eventSubject({})).toBe("");
  expect(eventSubject(undefined)).toBe("");
});

test("every §47.2 lane has a word, so a badge can never render undefined", () => {
  // A lane laneFor can return with no entry here would render as literally nothing on the transcript's most
  // repeated element. The keys are compared as a SET rather than spot-checked.
  const lanes = ["message", "model_step", "tool", "approval", "recovery", "usage", "progress", "terminal"];
  expect(Object.keys(LANE_LABEL).sort()).toEqual([...lanes].sort());
  // And no lane wears a speaker's name — the transcript labels lanes, not authors.
  expect(Object.values(LANE_LABEL).filter((v) => /^(user|assistant|human|system)$/i.test(v))).toEqual([]);
});

// THE WARNING-TO-SCREEN MAP CANNOT NAME A SCREEN THAT DOES NOT EXIST (machine-config).
//
// GET /v1/deployment raises a warning when a configured value changes what the product DOES; the API names
// no console path, because which screen would LIE is the only part of the question the console can answer.
// That makes WARNING_SCREENS a join maintained by hand, and a hand-maintained join between two lists is
// exactly where a renamed route silently unhooks a banner — the page keeps rendering, the warning simply
// stops appearing, and nothing fails. This is the check that makes it fail.
test("every screen the deployment warnings map to is a route this console serves", () => {
  const declared = new Set(CONSOLE_ROUTES.map((r) => r.path));
  const codes = Object.keys(WARNING_SCREENS);
  expect(codes.length, "an empty map means no warning reaches any screen").toBeGreaterThan(0);
  for (const code of codes) {
    const screens = WARNING_SCREENS[code] ?? [];
    expect(screens.length, `${code} maps to no screen, so it can never be shown`).toBeGreaterThan(0);
    for (const path of screens) {
      expect(declared, `${code} names ${path}, which CONSOLE_ROUTES does not declare`).toContain(path);
    }
    // EVERY warning reaches /deployment, whatever else it reaches. That screen's whole subject is the
    // configuration, so a warning it did not carry would be a warning with no home when the screen it was
    // aimed at is not the one the operator is on.
    expect(screens, `${code} does not reach /deployment`).toContain("/deployment");
  }
});

// warningsFor selects by SCREEN, and the negative half is the load-bearing one: a banner repeated on every
// page is chrome, and a reader stops seeing chrome.
test("warningsFor gives a screen only the warnings that screen is responsible for", () => {
  const dispatch = { code: "dispatch_workers_zero", severity: "blocking", headline: "", detail: "", remedy: "", settings: [] };
  const unknown = { code: "a_code_no_screen_claims", severity: "advisory", headline: "", detail: "", remedy: "", settings: [] };
  expect(warningsFor("/runs", [dispatch, unknown])).toEqual([dispatch]);
  expect(warningsFor("/deployment", [dispatch, unknown])).toEqual([dispatch]);
  // /history shows a dispatch-off run as `queued`, which is the truth — the screen reports a state rather
  // than promising a result, so it carries no banner.
  expect(warningsFor("/history", [dispatch, unknown])).toEqual([]);
});

// --- THE LANE STRIP'S ARITHMETIC (E30 visual identity). ---------------------------------------------------
// The strip is the console's signature and every mark on it is a percentage, so every percentage is driven
// here with no browser. Two of the three cases below are the ones that put a mark NOWHERE rather than in the
// wrong place, which is the failure mode a screenshot does not catch.

test("positionAt clamps to its own track, and a zero-width window is 0 rather than NaN", () => {
  const w = { start: 1000, end: 2000 };
  expect(positionAt(1000, w)).toBe(0);
  expect(positionAt(1500, w)).toBe(50);
  expect(positionAt(2000, w)).toBe(100);
  // OUTSIDE THE WINDOW IS CLAMPED, NOT DROPPED. A frame whose stamp is outside the span its own journal
  // reported is a real thing (a clock skew between a runner and the control plane), and `left: -40%` puts it
  // off the screen where nobody can see that it happened.
  expect(positionAt(0, w)).toBe(0);
  expect(positionAt(9999, w)).toBe(100);
  // The two ways a window has no width. Every frame of a replayed fixture shares one timestamp, and a
  // division there yields NaN — `left: NaN%` is a mark that renders nowhere at all.
  expect(positionAt(1500, { start: 1500, end: 1500 })).toBe(0);
  expect(positionAt(Number.NaN, w)).toBe(0);
});

test("windowOf ignores what it cannot parse, and answers null rather than a window of NaNs", () => {
  expect(windowOf(["2026-08-01T00:00:00Z", "2026-08-01T00:00:10Z"])).toEqual({ start: Date.parse("2026-08-01T00:00:00Z"), end: Date.parse("2026-08-01T00:00:10Z") });
  // A session that never ran carries `first_activity_at: null` — absent, not zero — and a window that let a
  // NaN in would place every OTHER row's bar against it.
  expect(windowOf(["2026-08-01T00:00:00Z", null, undefined, ""])).toEqual({ start: Date.parse("2026-08-01T00:00:00Z"), end: Date.parse("2026-08-01T00:00:00Z") });
  expect(windowOf([null, undefined, ""])).toBeNull();
  expect(windowOf([])).toBeNull();
});

test("staveOf keeps LANE_ORDER, drops the lanes a journal never used, and counts what it kept", () => {
  const w = { start: 0, end: 100 };
  const frames = [
    { key: "3", lane: "terminal" as const, time: new Date(100).toISOString(), failure: false },
    { key: "1", lane: "progress" as const, time: new Date(0).toISOString(), failure: false },
    { key: "2", lane: "tool" as const, time: new Date(50).toISOString(), failure: true },
    { key: "4", lane: "progress" as const, time: new Date(25).toISOString(), failure: false },
  ];
  const stave = staveOf(frames, w);
  // THE ORDER IS THE STAVE'S, NOT THE JOURNAL'S. The frames above arrive out of order on purpose: a reader
  // learns the channel positions once, so they must not move when a run happens to emit its lanes in a
  // different order.
  expect(stave.map((c) => c.lane)).toEqual(["progress", "tool", "terminal"]);
  expect(stave.map((c) => c.marks.length)).toEqual([2, 1, 1]);
  // A lane with no frames gets no channel — six empty tracks on every run is six rows saying nothing.
  expect(stave.some((c) => c.lane === "approval")).toBe(false);
  // The failure survives onto the mark, because it is what makes the tick full height.
  expect(stave.find((c) => c.lane === "tool")?.marks[0].failure).toBe(true);
  expect(stave.find((c) => c.lane === "progress")?.marks[0].failure).toBeUndefined();
  // No window is no stave, rather than a stave whose marks are all at zero.
  expect(staveOf(frames, null)).toEqual([]);
});

test("LANE_ORDER gives every lane a channel — a lane with no place on the stave would draw nowhere", () => {
  // The same set assertion the LANE_LABEL test makes, for the same reason and against the other table: a lane
  // laneFor can return but LANE_ORDER does not name is a lane whose frames staveOf silently discards.
  expect([...LANE_ORDER].sort()).toEqual(Object.keys(LANE_LABEL).sort());
});

test("spanMark is absent for a session that never ran, and never runs off its own track", () => {
  const w = { start: Date.parse("2026-08-01T00:00:00Z"), end: Date.parse("2026-08-01T01:00:00Z") };
  const mark = spanMark("2026-08-01T00:15:00Z", "2026-08-01T00:45:00Z", w);
  expect(mark).toEqual({ key: "span", at: 25, width: 50 });
  // A session with no first_activity_at gets NO BAR. A zero-width bar at the window's start would claim it
  // ran for an instant when it opened, which is a different fact from "it has never run".
  expect(spanMark(null, null, w)).toBeNull();
  expect(spanMark(undefined, "2026-08-01T00:45:00Z", w)).toBeNull();
  // A session that started and has not stopped: last is absent, so the bar is the start with no width — a
  // position rather than a duration, which is what is actually known.
  expect(spanMark("2026-08-01T00:30:00Z", null, w)).toEqual({ key: "span", at: 50, width: 0 });
  // The width can never push the bar past the end of the track, whatever the stamps say.
  expect(spanMark("2026-08-01T00:30:00Z", "2027-01-01T00:00:00Z", w)).toEqual({ key: "span", at: 50, width: 50 });
  expect(spanMark("2026-08-01T00:30:00Z", null, null)).toBeNull();
});
