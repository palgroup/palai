import { expect, test } from "@playwright/test";

import { NEXT_PORT, UPSTREAM_PORT } from "./constants";

// =============================================================================================
// THE SCREEN, DRIVEN.
//
// Every test here uses the CHAT — it types into the form and presses send — rather than posting to
// /api/chat and asserting on the response. That is not style. This tree found ten forms whose tests
// went to the endpoint and never submitted the form, and one of them was a login whose `method` was
// missing, so the password went into the URL every time JavaScript had not hydrated. A test that
// proves a route works proves nothing about the human path to it.
//
// The upstream is tests/fake-control-plane.mjs, whose iOS tool results are BYTES CAPTURED FROM THE
// REAL TOOLS (tests/fixtures/*.txt, from Xcode 26.6 against a real iOS Simulator destination). So
// what these assertions read on screen is what `xcodebuild` actually printed.
// =============================================================================================

// send drives the FORM: fill the real textarea, press the real submit button. `Enter` would also
// work but the button is the affordance an operator sees, and a submit button that stopped
// submitting would pass a keyboard-only test.
async function send(page: import("@playwright/test").Page, text: string) {
  await page.getByTestId("chat-input").fill(text);
  await page.getByTestId("chat-send").click();
}

test.describe("the chat renders iOS work", () => {
  // THE SAME RESET THE SIBLING DESCRIBE ALREADY OWNS, AND IT BECAME LOAD-BEARING ON 2026-08-02.
  //
  // The fake's coding session now holds TWO runs, because that is what a chat session is and because a
  // single-run journal could not reproduce the defect that dropped every turn after the first. Which
  // run a create is answered with depends on how many creates that session has seen — so a test that
  // does not own its starting turn count is measuring how many tests ran before it. This is the same
  // class of coupling the auto-approve block found in its own state, one field over.
  test.beforeEach(async ({ page, request }) => {
    await request.post(`http://127.0.0.1:${UPSTREAM_PORT}/__reset-coding`, {
      headers: { Authorization: "Bearer reset" },
    });
    await page.goto("/chat");
    await send(page, "build the iOS app and run its tests");
    // The run's terminal part is the signal that the whole scripted stream has been consumed;
    // waiting on it rather than on a timeout is what keeps this deterministic.
    await expect(page.getByTestId("chat-run").last()).toContainText("completed", { timeout: 30_000 });
  });

  // THE GAP THAT USED TO BE HERE. Before E30 T2 the frames carried no tool name and this card said
  // so in as many words. Now it must be LABELLED — and the old "name is unavailable" text must be
  // gone, because leaving it would be the screen apologising for a gap that no longer exists.
  test("a tool call is named on screen", async ({ page }) => {
    const headers = page.getByTestId("chat-tool-header");
    await expect(headers.first()).toBeVisible();
    await expect(headers.first()).toContainText("palai.workspace.shell");
    await expect(page.getByTestId("chat-tool-name-gap")).toHaveCount(0);
  });

  // A FAILING BUILD, WITH THE FILE AND LINE. This is the whole point of item 3: the fixture is the
  // real Swift type error xcodebuild emitted, inside 288 bytes of real build output.
  test("a failing xcodebuild shows BUILD FAILED with the failing file and line", async ({ page }) => {
    const build = page.getByTestId("ios-build");
    await expect(build).toHaveCount(1);
    await expect(build).toHaveAttribute("data-succeeded", "false");
    await expect(build.getByTestId("ios-build-status")).toContainText("BUILD FAILED");

    const location = build.getByTestId("ios-build-location").first();
    await expect(location).toContainText("Greeter.swift:5:49");
    await expect(build.getByTestId("ios-build-diagnostics")).toContainText(
      "cannot convert return expression of type 'String' to return type 'Int'",
    );
  });

  // A TEST RUN, with its counts and its cases — read off 934 bytes of real `xcodebuild test` output.
  test("a passing test run shows the counts and the cases", async ({ page }) => {
    const tests = page.getByTestId("ios-test");
    await expect(tests).toHaveCount(1);
    await expect(tests).toHaveAttribute("data-succeeded", "true");
    await expect(tests).toContainText("2 passed");

    const cases = tests.getByTestId("ios-test-case");
    await expect(cases).toHaveCount(2);
    await expect(cases.first()).toContainText("testGreetsByName");
    await expect(cases.first()).toHaveAttribute("data-status", "passed");
  });

  // THE SIMULATOR, with the device and its state — off real `simctl list devices` output.
  test("simctl shows the devices and their state", async ({ page }) => {
    const sim = page.getByTestId("ios-simulator");
    await expect(sim).toHaveCount(1);
    await expect(sim.getByTestId("ios-sim-devices")).toContainText("iPhone 17 Pro");
    // `list` is a verb like any other — a card with no verb reads as though the screen could not
    // tell what ran, which is a different (and wrong) statement from "this listed the devices".
    await expect(sim.getByTestId("ios-sim-action")).toContainText("list");
  });

  // THE SUMMARY IS NEVER THE ONLY COPY. Every parser here can miss, and a rendering with no way back
  // to the bytes turns a miss into a screen that quietly shows nothing. The disclosure has to open
  // and it has to contain what the tool actually printed.
  test("the raw output is one click away and holds what the tool printed", async ({ page }) => {
    const build = page.getByTestId("ios-build");
    await expect(build.getByTestId("ios-raw-output")).toBeHidden();
    await build.getByTestId("ios-raw-toggle").click();
    const raw = build.getByTestId("ios-raw-output");
    await expect(raw).toBeVisible();
    await expect(raw).toContainText("** BUILD FAILED **");
    // The exit code is the one fact on the card that came from the tool rather than from a regex.
    await expect(build.getByTestId("ios-raw-toggle")).toContainText("exit 65");
  });

  // A NON-SHELL TOOL MUST NOT BE DRESSED AS iOS WORK. `git -C repo add` is a shell call and renders
  // as a terminal; nothing in the run should have invented an Xcode card for it.
  test("an ordinary command renders as a terminal, not as a build", async ({ page }) => {
    const shells = page.getByTestId("ios-shell");
    await expect(shells.first()).toBeVisible();
    await expect(shells.first()).toContainText("git -C repo add CONTRIBUTING.md");
    // Exactly one build card, one test card, one simulator card — the git call did not become a
    // fourth. A classifier that fell through to "build" would show four.
    await expect(page.getByTestId("ios-build")).toHaveCount(1);
  });
});

test.describe("arming the session", () => {
  // THE FAKE'S STANDING AUTHORIZATION IS MODULE STATE ON ONE SHARED SERVER, so a test that armed the
  // session left it armed for the next. Two specs here passed or failed purely on the order they ran
  // in until this reset was added — a test whose result depends on its neighbours is measuring the
  // suite rather than the product. Each one now OWNS its starting condition.
  test.beforeEach(async ({ page, request }) => {
    await request.post(`http://127.0.0.1:${UPSTREAM_PORT}/__reset-coding`, {
      headers: { Authorization: "Bearer reset" },
    });
    await page.goto("/chat");
    // THE PANEL IS COLLAPSED NOW, and every assertion below lives inside a Radix CollapsibleContent
    // which UNMOUNTS while closed. Without this open, `toBeDisabled` and `toHaveAttribute` would be
    // asserting against elements that are not in the DOM at all — the exact vacuity this tree keeps
    // finding in sweeps that scan routes with every dialog shut and report a cleaner number.
    await page.getByTestId("auto-approve-summary").click();
  });

  // A session does not exist until the first turn opens one. A dead control that silently does
  // nothing when pressed is worse than one that says why it is not ready.
  test("before a session exists the controls say so and are disabled", async ({ page }) => {
    await expect(page.getByTestId("auto-approve-tools")).toBeDisabled();
    await expect(page.getByTestId("auto-approve-publications")).toBeDisabled();
    await expect(page.getByTestId("auto-approve-state")).toContainText("Send a message first");
  });

  // THE SPLIT, ON THE SCREEN, DRIVEN BY A CLICK. This is the assertion the whole two-column design
  // exists to make: arming the build commands must leave the push switch OFF.
  test("arming build commands does not arm pushes", async ({ page }) => {
    await send(page, "build the iOS app");
    await expect(page.getByTestId("chat-run").last()).toContainText("completed", { timeout: 30_000 });

    const tools = page.getByTestId("auto-approve-tools");
    const publications = page.getByTestId("auto-approve-publications");
    await expect(tools).toHaveAttribute("aria-checked", "false");

    await tools.click();
    await expect(tools).toHaveAttribute("aria-checked", "true");
    await expect(publications).toHaveAttribute(
      "aria-checked",
      "false",
      // If this ever flips, an operator who wanted to stop confirming xcodebuild has been given
      // unattended writes to their repository.
    );
  });

  // The two halves are independent in BOTH directions, and without this the test above is satisfied
  // by a publication switch that never works at all.
  test("pushes can be armed on their own", async ({ page }) => {
    await send(page, "build the iOS app");
    await expect(page.getByTestId("chat-run").last()).toContainText("completed", { timeout: 30_000 });

    await page.getByTestId("auto-approve-publications").click();
    await expect(page.getByTestId("auto-approve-publications")).toHaveAttribute("aria-checked", "true");
    await expect(page.getByTestId("auto-approve-tools")).toHaveAttribute("aria-checked", "false");
  });

  // WHOSE AUTHORITY IT IS. "Approved automatically" is an anonymous sentence; the screen has to name
  // the principal the decision is made as, because that is what makes it auditable — and it is the
  // principal the project's approver policy is checked against.
  test("an armed session names the principal it decides under", async ({ page }) => {
    await send(page, "build the iOS app");
    await expect(page.getByTestId("chat-run").last()).toContainText("completed", { timeout: 30_000 });

    await page.getByTestId("auto-approve-tools").click();
    const state = page.getByTestId("auto-approve-state");
    await expect(state).toContainText("key:demo-operator");
    // And it says the rows are still there. An auto-approval that skipped the approvals surface
    // would be indistinguishable, from every screen a human looks at, from no gate at all.
    await expect(state).toContainText("decided row");
  });

  // The toggle has to work in both directions or the screen is lying in one of them.
  test("a session can be disarmed again", async ({ page }) => {
    await send(page, "build the iOS app");
    await expect(page.getByTestId("chat-run").last()).toContainText("completed", { timeout: 30_000 });

    const tools = page.getByTestId("auto-approve-tools");
    await tools.click();
    await expect(tools).toHaveAttribute("aria-checked", "true");
    await tools.click();
    await expect(tools).toHaveAttribute("aria-checked", "false");
    await expect(page.getByTestId("auto-approve-state")).toContainText("Nothing is armed");
    // AND THE COLLAPSED LINE AGREES. It is the only part an operator sees by default, so a
    // summary that drifted from the panel would be the whole control lying at a glance.
    await expect(page.getByTestId("auto-approve-summary")).toContainText("nothing armed");
  });

  // NO KEY IN THE BROWSER, on the route this feature added. The credential is server-side; every
  // request the page makes goes to /api/*.
  test("arming the session never sends a Palai key from the browser", async ({ page }) => {
    const offBox: string[] = [];
    page.on("request", (r) => {
      const url = r.url();
      // Derived from the shared constant rather than hard-coded: a literal here would keep passing
      // if the suite's port moved, by scanning for requests to a host nothing is talking to.
      if (!url.startsWith(`http://127.0.0.1:${NEXT_PORT}`)) offBox.push(url);
      const auth = r.headers().authorization;
      if (auth !== undefined) offBox.push(`AUTH HEADER on ${url}`);
    });

    await send(page, "build the iOS app");
    await expect(page.getByTestId("chat-run").last()).toContainText("completed", { timeout: 30_000 });
    await page.getByTestId("auto-approve-tools").click();
    await expect(page.getByTestId("auto-approve-tools")).toHaveAttribute("aria-checked", "true");

    expect(offBox).toEqual([]);
  });
});
