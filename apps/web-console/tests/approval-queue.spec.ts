import { readdirSync, readFileSync } from "node:fs";
import { resolve } from "node:path";

import { expect, test, type Page } from "@playwright/test";

import { IS_REAL } from "./constants";
import { announceProfile, signIn, skipOnReal } from "./profile";

// CON-005 — THE APPROVAL QUEUE, AND THE NAME OF WHAT IT DOES NOT SHOW (E25 T5, plan §T5).
//
// THE RED THIS FILE WAS WRITTEN AS: a gated tool call parks, GET /v1/approvals returns it, and NO console page
// shows it. Every leg below failed on "no such page" — `page.goto("/approvals")` answered 404 and the panel
// never existed — which is the whole reason this task exists at all. E23 T9 wrote the backend half (the list
// route, two decision routes, a new `approve` capability, the request hash made mandatory at the edge); what
// was missing was a screen, and a hole nobody can see is a hole that fails closed until a reaper releases it.
//
// WHAT RUNS WHERE, AND WHY IT IS NOT A CHOICE. GET /v1/approvals is mounted unconditionally on a compose stack
// and answers 200 with an EMPTY page, so the queue's SENTENCES — its scope, its principal, its gates, its
// polling period, and its honest empty state — are asserted on BOTH profiles. A ROW cannot exist there: a tool
// approval is created only when a gated tool call parks, and no /v1 route parks one (the surface E23 T9 opened
// reads and decides, it cannot fill). DIV-UI-006 records that with its measurement, and the conformance sweep
// re-derives it from a real run on every sweep.
//
// THE THREE MECHANISMS BELOW ARE DELIBERATELY DIFFERENT, because they prove different things:
//   - the ROTATING row (`apvl_console_drift`) makes the 409 come from the one-shot binding genuinely failing,
//   - the STRIPPED request (a browser-boundary rewrite of the console's own POST) makes the 400 come from the
//     server's edge rule, with the console's own request as the input,
//   - the per-row canned 404/403 are SYNTHESIS, named as such in the fixture: the real API keys those to an id
//     and to a principal, and the console's key is fixed for the life of its process. Their control-plane half
//     is proven against a real store at the component tier
//     (apps/control-plane/internal/execution/http_tool_approval_component_test.go).
test.beforeAll(() => announceProfile("approval-queue.spec.ts"));
test.beforeEach(async ({ page }) => signIn(page));

// THE EXPECTED BYTES ARE WRITTEN HERE RATHER THAN IMPORTED FROM THE FIXTURE, and not by preference: Playwright
// transpiles what a spec imports to CJS, and an `.mjs` is loaded as ESM by Node, so importing the fixture from a
// spec fails at load ("exports is not defined in ES module scope") — the conformance sweep can import it because
// node:test runs native ESM. Independent expectations are the better shape anyway: if the fixture's rendering
// changes, this file has to change deliberately rather than agreeing with it automatically.
const APPROVAL_ARGS_JIRA = `{
  "labels": [
    "&lt;@U0THERS>"
  ],
  "project": "OPS",
  "summary": "<img src=x onerror=window.__approval_xss_executed=true>"
}`;
const APPROVAL_ARGS_CUT = `{
  "body": "the first 8000 bytes of a very long patch"
… truncated: 8000 of 9214 bytes shown; the full arguments are on the tool call this button is bound to`;

// The fixture's queue is STATEFUL and an answered row LEAVES it, so the rows a spec READS and the rows a spec
// ANSWERS have to be different rows — the first draft of this file read and decided the same one and every leg
// after the approve failed on "no such row", which was the fixture's state and not the console's behaviour.
const DECIDABLE = "apvl_console_0001";
// The two answerable rows are addressed by PREFIX, never by a fixed id: the fixture RE-PARKS one after it is
// answered, under a new id, so a fixed id here would make these legs depend on how many times the suite has run
// against a reused fixture — and the disappearance of the exact row that was answered is itself an assertion.
const APPROVE_PREFIX = "apvl_console_approve";
const DENY_PREFIX = "apvl_console_deny";
const CUT = "apvl_console_0002";
const DRIFT = "apvl_console_drift";
const GONE = "apvl_console_gone";
const LOCKED = "apvl_console_locked";

/** relayPosts records every decision the BROWSER actually sent, so the wire can be read rather than trusted. */
function relayPosts(page: Page): { url: string; body: string }[] {
  const posts: { url: string; body: string }[] = [];
  page.on("request", (request) => {
    if (request.method() === "POST" && request.url().includes("/api/palai/v1/approvals/")) {
      posts.push({ url: request.url(), body: request.postData() ?? "" });
    }
  });
  return posts;
}

/** renderedIds lists the parked rows the page is actually showing, in the order it shows them. */
async function renderedIds(page: Page): Promise<string[]> {
  return page
    .getByTestId(/^tool-approval-facts-/)
    .evaluateAll((nodes) => nodes.map((n) => (n.getAttribute("data-testid") ?? "").replace("tool-approval-facts-", "")));
}

/** pickOpen returns the id of a parked row to answer. An absent one THROWS rather than skipping the leg. */
async function pickOpen(page: Page, prefix: string): Promise<string> {
  const ids = await renderedIds(page);
  const id = ids.find((candidate) => candidate.startsWith(prefix));
  if (id === undefined) throw new Error(`no parked row whose id starts with "${prefix}" — the queue holds [${ids.join(", ")}]`);
  return id;
}

/** openQueue signs-in state is already present; this waits for the list to have actually rendered. */
async function openQueue(page: Page): Promise<void> {
  await page.goto("/approvals");
  await expect(page.getByTestId("panel-approvals")).toBeVisible({ timeout: 15_000 });
}

test("the approval queue shows a parked gated tool call with the server's own arguments, verbatim", async ({ page }) => {
  skipOnReal("DIV-UI-006");
  await openQueue(page);

  // THE SERVER'S SCREEN, NOT A SECOND ONE. This is byte equality against the fixture's own bytes — the same
  // bytes slack.DeriveApprovalDisplay produces (canonical JSON, keys sorted at every level by encoding/json,
  // two-space indent, HTML escaping OFF). A console that re-parsed and re-serialized would pass a "contains
  // the summary" assertion and fail this one, which is the point: two renderings of one ledger row must be
  // byte-identical or a diff between them means nothing.
  await expect(page.getByTestId(`tool-approval-arguments-${DECIDABLE}`)).toHaveText(APPROVAL_ARGS_JIRA);
  await expect(page.getByTestId(`tool-approval-identity-${DECIDABLE}`)).toHaveText("mcp:jira:create_issue");
  await expect(page.getByTestId(`tool-approval-operator-label-${DECIDABLE}`)).toContainText("Files a ticket");
  await expect(page.getByTestId(`tool-approval-request-hash-${DECIDABLE}`)).toHaveText("sha256:1c0ffee1");
  await expect(page.getByTestId(`tool-approval-expires-${DECIDABLE}`)).toContainText("2026-07-30T12:25:00Z");

  // THE SLACK-SHAPED ESCAPE IS SHOWN, NOT REPAIRED. Both surfaces share the ONE derivation, and that derivation
  // runs slack.NeutralizeBroadcasts over every string and key (`<@` -> `&lt;@`, slack/stream.go:163). So the
  // console's screen is Slack-flavoured whether it wants to be or not, and the honest console renders the
  // escape it was given rather than un-escaping it back into a mention. This is what "render the screen the
  // server computed" costs, measured rather than described.
  await expect(page.getByTestId(`tool-approval-arguments-${DECIDABLE}`)).toContainText("&lt;@U0THERS>");

  // THE CUT IS THE SERVER'S ADMISSION AND THE CONSOLE REPEATS IT. `truncated` is a field, not an inference: the
  // console does not measure the string itself, and the warning says the hash binds to ALL the bytes.
  await expect(page.getByTestId(`tool-approval-arguments-${CUT}`)).toHaveText(APPROVAL_ARGS_CUT);
  await expect(page.getByTestId(`tool-approval-truncated-${CUT}`)).toContainText("CUT");
  await expect(page.getByTestId(`tool-approval-truncated-${DECIDABLE}`)).toHaveCount(0);

  // A MISSING OPERATOR LABEL IS A SENTENCE, NOT A BLANK — slack.NoOperatorLabel, verbatim off the wire.
  await expect(page.getByTestId(`tool-approval-operator-label-${CUT}`)).toHaveText("(no operator label)");
  // AND A MISSING DEADLINE IS A SENTENCE TOO. `expires_at` is omitempty; a console printing "Invalid Date" here
  // would be showing its own bug as a fact about the gate.
  await expect(page.getByTestId(`tool-approval-expires-${CUT}`)).toContainText("no deadline");
});

test("the decision carries the request hash the row displayed, and nothing on the page asks for one", async ({ page }) => {
  skipOnReal("DIV-UI-006");
  const posts = relayPosts(page);
  await openQueue(page);

  // NOTHING ASKS FOR THE HASH, and this is enumerated rather than eyeballed (the T4 rule). No hidden input
  // anywhere — a hidden field is the tempting way to carry it and it is a field a page can be made to submit
  // with someone else's value — and every editable control on the page is a deny REASON, one per row.
  expect(await page.locator('input[type="hidden"]').count(), "a hidden field on an approval screen is a value the operator cannot see and cannot check").toBe(0);

  // The expected control set is DERIVED FROM THE ROWS THAT RENDERED rather than typed here: this queue is
  // stateful across the specs in one run, and a hardcoded list would make this assertion a statement about test
  // ORDER. What is claimed is per-row: every parked call has exactly one editable control, and it is its reason.
  const rendered = await renderedIds(page);
  expect(rendered.length, "the queue rendered no rows, so everything after this would be vacuous").toBeGreaterThan(2);
  const controls = await page.locator("input, textarea, select").evaluateAll((nodes) => nodes.map((n) => n.getAttribute("data-testid") ?? "<no testid>"));
  expect(controls.sort(), "the only editable controls on the approval queue are the per-row deny reasons").toEqual(rendered.map((id) => `tool-approval-reason-${id}`).sort());
  expect(controls.some((c) => /hash/i.test(c)), "a control asked the operator for a request hash").toBe(false);

  const target = await pickOpen(page, APPROVE_PREFIX);
  const displayed = await page.getByTestId(`tool-approval-request-hash-${target}`).innerText();
  await page.getByTestId(`tool-approval-approve-${target}`).click();
  // The confirmation is read from the PAGE, not the row: the row is gone by the time the refetch lands, and a
  // message inside it would go with it. Reading it here is what caught that.
  await expect(page.getByTestId("approvals-decision-status")).toContainText("approved", { timeout: 15_000 });

  // THE WIRE. The hash the browser sent is the hash the row DISPLAYED — carried out of the row's own data.
  expect(posts.length, "the console sent no decision at all").toBe(1);
  const sent = JSON.parse(posts[0].body) as Record<string, unknown>;
  expect(posts[0].url).toContain(`/approvals/${target}/approve`);
  expect(sent.request_hash).toBe(displayed);
  // AND NOTHING ELSE RIDES AN APPROVE. api.ApprovalDecision decodes with DisallowUnknownFields and has no
  // `approver` field on purpose — the principal is stamped server-side from the verified key — so a body that
  // grew a field would be a 400 against the real API and a console trying to name its own approver.
  expect(Object.keys(sent).sort(), "an approve body carries the binding and nothing else").toEqual(["request_hash"]);

  // ANSWERED MEANS GONE. There is no status field on this projection to read instead: the row leaving the queue
  // is the whole of what "it was answered" looks like.
  await expect(page.getByTestId(`tool-approval-facts-${target}`)).toHaveCount(0, { timeout: 15_000 });
  await expect(page.getByTestId(`tool-approval-facts-${CUT}`)).toBeVisible();
});

test("a decision stripped of its request hash authorizes nothing and the screen says the hash was missing", async ({ page }) => {
  skipOnReal("DIV-UI-006");
  // THE CONSOLE'S OWN REQUEST, WITH THE BINDING REMOVED AT THE BOUNDARY. This is how the 400 branch is reached
  // without a client-side guard standing in front of it: api/approvals.go:200-204 refuses the decision at the
  // edge, and the console must report THAT rather than a shrug. The console deliberately does not pre-check an
  // empty hash — a guard there would make this refusal unreachable and therefore its rendering untested.
  await page.route("**/api/palai/v1/approvals/**", async (route) => {
    const request = route.request();
    if (request.method() !== "POST") return route.fallback();
    const body = JSON.parse(request.postData() ?? "{}") as Record<string, unknown>;
    delete body.request_hash;
    await route.continue({ postData: JSON.stringify(body) });
  });

  await openQueue(page);
  await page.getByTestId(`tool-approval-approve-${DECIDABLE}`).click();

  const error = page.getByTestId(`tool-approval-${DECIDABLE}-error`);
  await expect(error).toBeVisible({ timeout: 15_000 });
  await expect(error).toContainText("carried no request hash");
  await expect(error).toContainText("authorized nothing");
  // The row is STILL THERE: a refused decision must not look like an answered one.
  await expect(page.getByTestId(`tool-approval-facts-${DECIDABLE}`)).toBeVisible();
});

test("an approval whose arguments changed is refused as no-longer-decidable, and the screen says which refusal it was", async ({ page }) => {
  skipOnReal("DIV-UI-006");
  await openQueue(page);

  // The drift row hands out a binding and then moves on — the fixture rotates its arguments and its hash on
  // every serve, so whatever this page is holding is the PREVIOUS call. That is the one-shot binding: the
  // decision authorizes nothing rather than authorizing something else.
  const held = await page.getByTestId(`tool-approval-request-hash-${DRIFT}`).innerText();
  await page.getByTestId(`tool-approval-approve-${DRIFT}`).click();

  const error = page.getByTestId(`tool-approval-${DRIFT}-error`);
  await expect(error).toBeVisible({ timeout: 15_000 });
  await expect(error).toContainText("can no longer be decided");
  await expect(error).toContainText("the arguments changed");
  await expect(error, "the 409 sentence must say that NOTHING was authorized").toContainText("Nothing was");
  // Still parked, and the next read shows a hash that is not the one that was held — the arguments really did
  // move, which is what made the refusal real rather than canned.
  await expect(page.getByTestId(`tool-approval-facts-${DRIFT}`)).toBeVisible();
  await page.reload();
  await expect(page.getByTestId(`tool-approval-request-hash-${DRIFT}`)).not.toHaveText(held);
});

test("the typed refusals get different sentences and none of them is a generic failure", async ({ page }) => {
  skipOnReal("DIV-UI-006");
  await openQueue(page);

  // Three causes, three next actions for the operator: reload the queue (404), fix the approver list (403), or
  // read the arguments again because they changed (409). api/approvals.go:211-221 already distinguishes them;
  // the console must not flatten them back together.
  const sentences: Record<string, string> = {};
  for (const id of [GONE, LOCKED, DRIFT]) {
    await page.getByTestId(`tool-approval-approve-${id}`).click();
    const error = page.getByTestId(`tool-approval-${id}-error`);
    await expect(error).toBeVisible({ timeout: 15_000 });
    sentences[id] = await error.innerText();
  }

  expect(sentences[GONE]).toContain("no longer exists");
  expect(sentences[GONE], "404 is unknown OR foreign, indistinguishable on purpose — the screen must not imply it exists elsewhere").toContain("indistinguishable");
  expect(sentences[LOCKED]).toContain("approver list");
  expect(sentences[LOCKED], "the two gates are independent and the fix is different for each").toContain("capability");
  expect(sentences[DRIFT]).toContain("can no longer be decided");

  const all = [sentences[GONE], sentences[LOCKED], sentences[DRIFT]];
  expect(new Set(all).size, `three typed refusals produced ${new Set(all).size} distinct sentence(s):\n${all.join("\n---\n")}`).toBe(3);
  for (const sentence of all) {
    expect(/something went wrong|unexpected error|an error occurred|try again later/i.test(sentence), `a typed refusal was flattened into a generic failure: ${sentence}`).toBe(false);
    // Each one is the console's OWN sentence rather than an echo of the server's: the fixture's `detail` for
    // all three is deliberately bland, so a console that just printed it would produce three near-identical
    // shrugs and this assertion would fail on the words that name the operator's next action.
    expect(sentence.length, "a refusal that says less than the server's own detail is not a screen text").toBeGreaterThan(80);
  }
});

test("a deny sends the operator's own reason verbatim, and a deny with no reason never leaves the browser", async ({ page }) => {
  skipOnReal("DIV-UI-006");
  const posts = relayPosts(page);
  await openQueue(page);

  // A DENY WITH NO REASON IS REFUSED BY THE CONSOLE ITSELF, and that is the console's own rule rather than the
  // API's (api.ApprovalDecision.Reason is optional). It is what closes `HIL-P10` on this surface: on Slack a
  // typed deny reason reaches nothing and the model is handed a constant sentence, while here the field is
  // wired to ApprovalDecision.Reason — so the ceiling is closed by making the operator write the sentence.
  const target = await pickOpen(page, DENY_PREFIX);
  const displayed = await page.getByTestId(`tool-approval-request-hash-${target}`).innerText();
  await page.getByTestId(`tool-approval-deny-${target}`).click();
  await expect(page.getByTestId(`tool-approval-${target}-error`)).toContainText("A denial needs a reason");
  expect(posts.length, "an empty denial reached the control plane — the model would have been handed nothing").toBe(0);

  // VERBATIM means verbatim: a colon, a newline and a quote all survive, because this text is what the MODEL is
  // handed as the answer to what it asked for.
  const reason = 'Do not file this: "OPS" is world-readable.\nUse the OPS-INTERNAL project instead.';
  await page.getByTestId(`tool-approval-reason-${target}`).fill(reason);
  await page.getByTestId(`tool-approval-deny-${target}`).click();
  await expect(page.getByTestId("approvals-decision-status")).toContainText("denied", { timeout: 15_000 });

  expect(posts.length).toBe(1);
  const sent = JSON.parse(posts[0].body) as Record<string, unknown>;
  expect(posts[0].url).toContain(`/approvals/${target}/deny`);
  expect(sent.reason, "the reason reaches the API exactly as it was written").toBe(reason);
  expect(sent.request_hash, "the deny carries the row's own binding too").toBe(displayed);
  expect(Object.keys(sent).sort()).toEqual(["reason", "request_hash"]);
  await expect(page.getByTestId(`tool-approval-facts-${target}`)).toHaveCount(0, { timeout: 15_000 });
});

test("the queue says what it does NOT cover while a publication approval is parked in a live run", async ({ page }) => {
  // DIV-UI-001: a compose run reaches no approval.requested.v1 at all, so there is no publication approval to
  // park on the real profile. The SENTENCE is asserted on both profiles by the test below this one; what needs
  // the fixture is the half that makes it a measurement rather than a claim — a publication approval genuinely
  // parked, in another tab, while this queue does not show it.
  skipOnReal("DIV-UI-001");

  await page.goto("/runs");
  await page.getByTestId("run-button").click();
  await expect(page.getByTestId("approval-panel")).toBeVisible({ timeout: 15_000 });
  const publicationHash = await page.getByTestId("approval-request-hash").innerText();
  expect(publicationHash, "the publication approval carries the binding this queue must not claim to cover").not.toBe("");

  // A SECOND TAB in the same browser context — the run's stream stays open, so the publication really is still
  // waiting while the queue is read. Closing the first page would have made this "a queue that does not show an
  // approval nobody is waiting on", which proves nothing.
  const queue = await page.context().newPage();
  try {
    await queue.goto("/approvals");
    await expect(queue.getByTestId("panel-approvals")).toBeVisible({ timeout: 15_000 });

    // IT IS NOT IN THE LIST — checked by the publication's own facts, not by counting rows.
    const listed = await queue.getByTestId("panel-approvals").innerText();
    expect(listed.includes(publicationHash), "a publication approval appeared in the TOOL approval queue").toBe(false);
    expect(listed.includes("push_branch"), "the publication's operation appeared in the tool approval queue").toBe(false);
    expect(listed.includes("pub_1"), "the publication's id appeared in the tool approval queue").toBe(false);

    // AND THE PAGE SAYS SO, with the way to the surface that does show it. A silently empty half is the failure
    // mode this sentence exists to prevent: an operator who learns "approvals live here" will miss the ones
    // that do not.
    const scope = queue.getByTestId("approvals-scope-note");
    await expect(scope).toContainText("Publication approvals are not here");
    await expect(scope).toContainText("tool approvals only");
    await expect(scope.locator('a[href="/runs"]')).toHaveCount(1);
    await expect(scope, "an empty queue must not read as 'nothing is waiting'").toContainText("does not mean nothing is waiting");
  } finally {
    await queue.close();
  }

  // The publication is STILL parked — the queue read nothing and decided nothing on the other surface.
  await expect(page.getByTestId("approval-panel")).toBeVisible();
  await expect(page.getByTestId("terminal-status")).toHaveCount(0);
});

test("the queue names the key a decision is recorded against, and the two gates in front of it", async ({ page }) => {
  // BOTH PROFILES. Nothing here needs a parked call: these are the sentences that bound what this screen means,
  // and the real profile is exactly where an operator reads them.
  await openQueue(page);

  const principal = page.getByTestId("approvals-principal-note");
  await expect(principal).toContainText("recorded against the console's API key");
  await expect(principal).toContainText("key:<api_key_id>");
  await expect(principal, "HIL-P2: a principal is an account on a surface, never a human").toContainText("no user identity");

  const gates = page.getByTestId("approvals-gates-note");
  await expect(gates).toContainText("approve");
  await expect(gates).toContainText("config_policy.approvers");
  await expect(gates, "HIL-P11: both gates are permissive when unset, and E25 does not change that posture").toContainText("permissive when unset");
});

test("the queue renders on both profiles, states its polling period, and re-reads the list on that period", async ({ page }) => {
  // 10s of waiting plus a build-cold first paint; the budget is generous on purpose so a slow machine reports a
  // real failure rather than a timeout.
  test.setTimeout(90_000);

  let reads = 0;
  page.on("request", (request) => {
    if (request.method() === "GET" && request.url().includes("/api/palai/v1/approvals")) reads += 1;
  });
  await openQueue(page);

  // THE HONEST STATE OF THE QUEUE ON THIS PROFILE, pinned in BOTH directions rather than tolerated in either:
  // the fixture parks five calls, and a compose stack can park none (DIV-UI-006), so the real profile must show
  // the EMPTY state — which is itself the thing worth asserting, because an empty list plus a scope sentence is
  // the difference between "nothing is waiting" and "nothing of this KIND is waiting".
  if (IS_REAL) {
    await expect(page.getByTestId("panel-approvals-empty")).toBeVisible();
    expect(await page.getByTestId(/^tool-approval-facts-/).count()).toBe(0);
  } else {
    // Not an exact count: the specs above this one ANSWER two of the fixture's rows and an answered row leaves
    // the queue, so a number here would be an assertion about test order rather than about the console.
    expect(await page.getByTestId(/^tool-approval-facts-/).count(), "the fixture parks gated calls and none of them rendered").toBeGreaterThan(2);
    await expect(page.getByTestId("panel-approvals-empty")).toHaveCount(0);
  }

  // The period is read off the DOM and the sentence is checked against it, so the prose cannot drift from the
  // timer: both come from one exported constant.
  const note = page.getByTestId("approvals-poll-note");
  const declared = Number(await note.getAttribute("data-poll-ms"));
  expect(declared).toBeGreaterThan(0);
  await expect(note).toContainText(`every ${declared / 1000} seconds`);
  await expect(note, "the ceiling is that there is no push — say it").toContainText("no push notification");

  const before = reads;
  expect(before, "the queue did not read the list at all").toBeGreaterThan(0);
  // eslint-disable-next-line no-console -- the count IS the evidence that the timer runs.
  console.log(`APPROVAL QUEUE POLL — ${before} read(s) on load, waiting ${declared}ms for the next`);
  await expect.poll(() => reads, { timeout: declared + 20_000, intervals: [500] }).toBeGreaterThan(before);
});

test("the model-authored arguments render as text and the payload never executes", async ({ page }) => {
  skipOnReal("DIV-UI-006");
  await openQueue(page);

  // The arguments are the ONE piece of model-authored prose on this screen, and the fixture's own row carries
  // active markup: `<img src=x onerror=window.__approval_xss_executed=true>`. React's default escaping is the
  // whole defence, so it is attacked rather than asserted about — the payload is IN the page and must be inert.
  await expect(page.getByTestId(`tool-approval-arguments-${DECIDABLE}`)).toContainText("<img src=x onerror=");
  expect(await page.locator(`[data-testid="tool-approval-arguments-${DECIDABLE}"] img`).count(), "the model's markup became an ELEMENT").toBe(0);
  expect(
    await page.evaluate(() => (window as unknown as { __approval_xss_executed?: boolean }).__approval_xss_executed),
    "a payload in a parked call's arguments EXECUTED on the console's origin — the origin that holds the operator session and drives the relay",
  ).toBeUndefined();
});

// --- THE ABSENCE, IN THE SOURCE (both profiles) ---------------------------------------------------------
//
// A tree that renders untrusted bytes must be able to say it renders them as TEXT, and "we do not use
// dangerouslySetInnerHTML" is a claim about every file rather than about the one being reviewed. So every .tsx
// under app/ and components/ is TOKENIZED and the identifier is hunted in the token stream.
//
// IT CANNOT BE A TEXT SCAN, and this file's own comments are why: the sentence above contains the identifier,
// and so do three comments in components/ApprovalRow.tsx. A grep would fail on the prose that promises the
// absence — which is exactly how T4's first paste check failed, and this tree's substring comparisons have now
// been defeated five times. Tokens have no comments in them.
//
// TWO THINGS MAKE THE SCAN TRUSTWORTHY RATHER THAN HOPEFUL:
//   1. IT IS PROVEN ON SYNTHETIC INPUT FIRST — a source that USES the prop must be found, and a source that
//      only MENTIONS it (in a comment and in a string) must not be. A scanner nobody tested is a guard nobody
//      tested.
//   2. EVERY FILE'S BRACKET BALANCE MUST COME OUT AT ZERO. A mis-scanned string or template swallows source,
//      and a swallowed region is where an occurrence would hide; relay-gate.spec.ts was caught by exactly that,
//      reporting one method of five. Here a desync THROWS.
//
// The regex re-scan relay-gate.spec.ts needs is deliberately NOT done: in JSX a `/` after `<` is a closing tag,
// and re-scanning it as a regular expression would swallow the rest of the line. That is the opposite mistake,
// and the balance check is what would catch either.
const FORBIDDEN_PROP = ["dangerously", "SetInnerHTML"].join("");

async function identifiers(source: string, label: string): Promise<Set<string>> {
  const ast = await import("typescript/unstable/ast");
  const kinds = ast.SyntaxKind as unknown as Record<string, number | undefined>;
  const K: Record<string, number> = {};
  for (const name of ["EndOfFile", "Identifier", "OpenBraceToken", "CloseBraceToken", "OpenParenToken", "CloseParenToken", "OpenBracketToken", "CloseBracketToken", "TemplateHead", "TemplateTail"]) {
    const value = kinds[name];
    if (typeof value !== "number") throw new Error(`SyntaxKind.${name} does not exist in this TypeScript — the scan cannot be trusted until it is remapped`);
    K[name] = value;
  }
  const scanner = ast.createScanner(true, ast.LanguageVariant.JSX, source);
  const found = new Set<string>();
  const stack: ("brace" | "template")[] = [];
  let braces = 0;
  let parens = 0;
  let brackets = 0;
  for (let i = 0; ; i++) {
    if (i > 400_000) throw new Error(`the token scan of ${label} never reached EndOfFile`);
    let kind = scanner.scan();
    if (kind === K.EndOfFile) break;
    if (kind === K.TemplateHead) stack.push("template");
    else if (kind === K.OpenBraceToken) {
      stack.push("brace");
      braces++;
    } else if (kind === K.CloseBraceToken) {
      if (stack[stack.length - 1] === "template") {
        kind = scanner.reScanTemplateToken(false);
        if (kind === K.TemplateTail) stack.pop();
      } else {
        stack.pop();
        braces--;
      }
    } else if (kind === K.OpenParenToken) parens++;
    else if (kind === K.CloseParenToken) parens--;
    else if (kind === K.OpenBracketToken) brackets++;
    else if (kind === K.CloseBracketToken) brackets--;
    if (kind === K.Identifier) found.add(scanner.getTokenText());
  }
  if (braces !== 0 || parens !== 0 || brackets !== 0) {
    throw new Error(`the token scan of ${label} desynced: {}=${braces} ()=${parens} []=${brackets} — a string, template or regex literal was mis-scanned, and a swallowed region is where an occurrence would hide`);
  }
  return found;
}

function tsxFiles(dir: string): string[] {
  const out: string[] = [];
  for (const entry of readdirSync(dir, { withFileTypes: true })) {
    const full = resolve(dir, entry.name);
    if (entry.isDirectory()) out.push(...tsxFiles(full));
    else if (entry.name.endsWith(".tsx")) out.push(full);
  }
  return out;
}

test("no console source renders untrusted bytes as markup — the forbidden prop is in no token stream", async () => {
  // (1) The scan is proven on synthetic input before it is trusted on real input.
  const uses = await identifiers(`export const X = () => <p ${FORBIDDEN_PROP}={{ __html: evil }} />;`, "<synthetic: uses the prop>");
  expect(uses.has(FORBIDDEN_PROP), "the scan cannot even find the prop when it IS used — every result below would be meaningless").toBe(true);
  const mentions = await identifiers(`// we never use ${FORBIDDEN_PROP} here\nexport const Y = () => <p title={"${FORBIDDEN_PROP}"} />;\n`, "<synthetic: only mentions it>");
  expect(mentions.has(FORBIDDEN_PROP), "the scan reported a comment and a string literal as usage — this is the substring defeat this tree has shipped five times").toBe(false);

  // (2) Every .tsx the console renders from.
  const files = [...tsxFiles(resolve(process.cwd(), "app")), ...tsxFiles(resolve(process.cwd(), "components"))];
  expect(files.length, "the walk found fewer .tsx files than this console has — a guard that scans nothing passes").toBeGreaterThanOrEqual(10);
  const offenders: string[] = [];
  for (const file of files) {
    if ((await identifiers(readFileSync(file, "utf8"), file)).has(FORBIDDEN_PROP)) offenders.push(file.replace(process.cwd(), "."));
  }
  // eslint-disable-next-line no-console -- the count IS the evidence.
  console.log(`UNTRUSTED-RENDER AUDIT — ${files.length} .tsx file(s) tokenized, ${offenders.length} using the forbidden prop`);
  expect(offenders, "a console page or component renders raw HTML; every byte on the approval screen is model-authored or MCP-authored").toEqual([]);
});
