import { expect, test } from "@playwright/test";

import { API_KEY, UPSTREAM_PORT } from "./constants";

// =============================================================================================
// THE /chat SURFACES, DRIVEN THROUGH THE THINGS A HUMAN TOUCHES.
//
// This tree found TEN forms whose tests posted to the endpoint and never submitted the form, and the
// sharpest one had no `method`, so every moment before hydration put a password in the URL. So every
// test below clicks the real control and fills the real field; not one of them calls /api/* directly
// except to read back what the form caused.
//
// The deterministic upstream is tests/fake-control-plane.mjs, whose coding block replays payloads
// MEASURED on the live control plane on 2026-08-02 — including the three absences that matter:
// a tool frame with no name, a publication approval frame with no approval_id and no destination, and
// an approval.approved.v1 with no publication.published.v1 after it.
// =============================================================================================

const CHAT = "/chat";

// EVERY TEST OWNS ITS STARTING TURN COUNT. The fake's coding session holds two runs — a chat session
// is many runs, and a one-run journal cannot reproduce the defect that dropped every turn after the
// first — so which run a create is answered with depends on how many creates that session has seen.
// Without this, test N would be measuring how many of its neighbours sent a message.
test.beforeEach(async ({ request }) => {
  await request.post(`http://127.0.0.1:${UPSTREAM_PORT}/__reset-coding`, {
    headers: { Authorization: "Bearer reset" },
  });
});

async function pickFirstRepo(page: import("@playwright/test").Page) {
  await page.getByTestId("repo-option").first().waitFor();
  await page.getByTestId("repo-option").first().click();
}

async function send(page: import("@playwright/test").Page, text: string) {
  await page.getByTestId("chat-input").fill(text);
  await page.getByTestId("chat-input").press("Enter");
}

test("the repository list is read through the app's own route and names the identity each binding publishes as", async ({ page }) => {
  await page.goto(CHAT);

  const options = page.getByTestId("repo-option");
  await expect(options).toHaveCount(2);

  // A binding WITH a connection_ref shows that ref; one WITHOUT shows "deployment App" rather than a
  // blank. An empty connection_ref is a different named identity, not a missing value, and a blank
  // would read as "nobody checked".
  await expect(options.nth(0)).toContainText("acme/with-credential");
  await expect(options.nth(0)).toContainText("gh-pat-acme");
  await expect(options.nth(1)).toContainText("acme/no-credential");
  await expect(options.nth(1)).toContainText("deployment App");

  // Picking one is what makes the turn a coding session; the centre panel names it.
  await options.nth(0).click();
  await expect(page.getByTestId("workspace-title")).toHaveText("acme/with-credential");
});

test("the bind form SUBMITS — and it carries method=post, so a pre-hydration submit cannot put the clone URL in the address bar", async ({ page }) => {
  await page.goto(CHAT);
  await page.getByTestId("repo-bind-toggle").click();

  // THE ATTRIBUTE CHECK IS THE POINT, and it is asserted on the DOM rather than in the source. A form
  // with no method defaults to GET: every field, including a clone URL that can embed a token, would
  // land in a history entry each time JavaScript had not yet attached.
  const form = page.getByTestId("bind-form");
  await expect(form).toHaveAttribute("method", /post/i);
  await expect(form).toHaveAttribute("action", "/api/palai/bindings");

  await page.getByTestId("bind-clone-url").fill("https://example.invalid/acme/from-the-form.git");
  await page.getByTestId("bind-branch").fill("main");
  await page.getByTestId("bind-connection-ref").fill("gh-pat-from-form");
  await page.getByTestId("bind-submit").click();

  // The new binding appears in the list AND is selected, which is what makes the next turn use it.
  await expect(page.getByTestId("repo-option")).toHaveCount(3);
  await expect(page.getByTestId("workspace-title")).toHaveText("acme/from-the-form");

  // And the upstream actually received it, with owner/repo DERIVED from the clone URL rather than
  // left empty — a pull request needs it, and a wrong one opens against a repository nobody meant.
  const seen = await page.request.get(`http://127.0.0.1:${UPSTREAM_PORT}/__introspect-coding`, {
    headers: { Authorization: `Bearer ${API_KEY}` },
  });
  const body = (await seen.json()) as { createdBindings: { repository_identity: string; connection_ref?: string }[] };
  expect(body.createdBindings).toHaveLength(1);
  expect(body.createdBindings[0].repository_identity).toBe("acme/from-the-form");
  expect(body.createdBindings[0].connection_ref).toBe("gh-pat-from-form");
});

test("the bind form refuses a non-http clone URL next to the field, without a round trip", async ({ page }) => {
  // THE BASELINE IS READ, NOT ASSUMED. The fake upstream keeps its created bindings for the life of
  // the process, so a literal "there are 2 rows" assertion here passes or fails on which tests ran
  // before it rather than on anything this one did. That is the shape of a test that measures its
  // neighbour; the count is taken before and compared after.
  const introspect = async () =>
    ((await page.request.get(`http://127.0.0.1:${UPSTREAM_PORT}/__introspect-coding`, {
      headers: { Authorization: `Bearer ${API_KEY}` },
    })) .json() as Promise<{ createdBindings: unknown[] }>);

  await page.goto(CHAT);
  const before = (await introspect()).createdBindings.length;

  await page.getByTestId("repo-bind-toggle").click();
  await page.getByTestId("bind-clone-url").fill("git@github.com:acme/ssh.git");
  await page.getByTestId("bind-submit").click();
  await expect(page.getByTestId("bind-error")).toContainText("http(s)");

  // The refusal happened in this app's route: the control plane was never asked.
  expect((await introspect()).createdBindings.length).toBe(before);
});

test("the credential form SUBMITS, posts only {name,value}, and the token never appears in the page afterwards", async ({ page }) => {
  await page.goto(CHAT);
  await page.getByTestId("secret-toggle").click();

  const form = page.getByTestId("secret-form");
  await expect(form).toHaveAttribute("method", /post/i);
  await expect(form).toHaveAttribute("action", "/api/palai/secret-refs");
  // The field is a password field, so it is not shoulder-readable and not autofilled into a log.
  await expect(page.getByTestId("secret-value")).toHaveAttribute("type", "password");

  const token = "ghp-SENTINEL-must-not-be-rendered-9f2c";
  await page.getByTestId("secret-name").fill("gh-pat-from-form");
  await page.getByTestId("secret-value").fill(token);
  await page.getByTestId("secret-submit").click();

  // WAIT ON THE RECEIPT THE OPERATOR SEES, not on a timer. The earlier version of this test read the
  // upstream immediately and raced the POST — it passed alone and failed in the suite, which is a
  // test measuring its own timing rather than the product. The receipt also had to be MADE reachable:
  // the form used to collapse itself on success, destroying the only confirmation that ever exists
  // (the control plane's 201 body is metadata only).
  await expect(page.getByTestId("secret-stored")).toContainText("gh-pat-from-form");

  const seen = await page.request.get(`http://127.0.0.1:${UPSTREAM_PORT}/__introspect-coding`, {
    headers: { Authorization: `Bearer ${API_KEY}` },
  });
  expect(((await seen.json()) as { createdSecrets: string[] }).createdSecrets).toContain("gh-pat-from-form");

  // THE VALUE IS GONE FROM THE SCREEN. The 201 body is metadata only, so there is nothing to echo —
  // this asserts the demo did not decide to be helpful and show it back.
  await expect(page.locator("body")).not.toContainText(token);
});

// THIS TEST USED TO ASSERT THE OPPOSITE, and rewriting it rather than deleting it is the point.
//
// It read: "the screen says the name is not carried", and it was correct — the tool frames carried
// {run_id, replay_class, tool_call_id} and nothing else, so the card said so in as many words rather
// than printing a label the stream never held. E30 T2 put `tool_name` on both frames, so the sentence
// the old assertion demanded is now the wrong thing for the screen to say.
//
// THE PROPERTY IT GUARDED HAS NOT CHANGED: the card must never lie about the name. ToolHeader renders
// an EMPTY SPAN when handed nothing usable, which reads as a rendering bug, and a placeholder reads as
// a tool actually called that. So this asserts the real name now, and the companion below keeps the
// other half — a frame WITHOUT a name must still say so, which is what a run started before this
// deployment upgraded looks like.
test("a tool call renders as the AI Elements tool component AND carries its real name", async ({ page }) => {
  await page.goto(CHAT);
  await pickFirstRepo(page);
  await send(page, "add a contributing guide");

  const tool = page.getByTestId("chat-tool");
  await expect(tool.first()).toBeVisible({ timeout: 30_000 });

  await expect(page.getByTestId("chat-tool-header").first()).toContainText("palai.workspace.shell");

  // OPENING THE CARD SHOWS WHAT THE CALL RAN. UAT opened one and found three identifiers under a
  // heading that says PARAMETERS — one of them the tool name already printed on the header it had just
  // clicked. It was reported as a fixture defect; measuring said otherwise, because the live and the
  // deterministic paths render this block from the same component with the same fields. The card was
  // thin on both. A card that opens onto its own id teaches an operator not to open cards.
  await page.getByTestId("chat-tool-header").first().click();
  // THE TIMEOUT IS THE SYNTAX HIGHLIGHTER, NOT THE PRODUCT, and it is worth naming because the failure
  // it produced was thoroughly misleading. CodeBlock highlights asynchronously, so when the arguments
  // replace the identifier fallback the card renders the NEW footer beside the OLD highlighted body —
  // a state that looks like the args branch printing identifiers, which is impossible in the source.
  // Measured by hand: the settled card holds the argv. Waiting for the re-highlight is waiting on a
  // real render, not smoothing over a race in what is being asserted.
  // ASSERTED ON THE ARGUMENTS BLOCK ITSELF, not on the whole card. Against the card, this matched the
  // stale identifier JSON for the full timeout while a hand-driven browser — same ordering, same
  // fixture — showed the argv settled in the DOM. Rather than widen the timeout again on a difference
  // I could not explain, the assertion now names the one element it is about. The card's own text is
  // a concatenation of a syntax-highlighted <pre> under `content-visibility: auto`, a header and a
  // footer, and matching a command line inside that is asking a broad locator a narrow question.
  await expect(page.getByTestId("chat-tool-args").first()).toContainText("git -C repo add CONTRIBUTING.md", {
    timeout: 15_000,
  });
  // And once the arguments are known, the identifier fallback is NOT also shown — two "parameters"
  // blocks would be the card hedging rather than answering.
  await expect(page.getByTestId("chat-tool-args-pending")).toHaveCount(0);

  // THE CARD IS ALREADY OPEN, and that ordering is the assertion. ToolContent is a Radix
  // CollapsibleContent, which UNMOUNTS while closed — so `toHaveCount(0)` against a shut card is true
  // of every card ever rendered, including one still full of the old apology. This tree has the same
  // defect on record from an axe sweep that scanned routes with every dialog closed and reported a
  // cleaner number while covering less. (It used to click here a second time, which would now SHUT the
  // card the assertion above opened — a toggle clicked twice is a closed card, and every absence below
  // it would have gone vacuous.)
  await expect(page.getByTestId("chat-tool").first()).toContainText("tcall_coding_1");

  // NOW the absence means something: the content is mounted, and the apology is not in it. Leaving it
  // would be the screen excusing a gap that no longer exists, which is its own kind of lie.
  await expect(page.getByTestId("chat-tool-name-gap")).toHaveCount(0);

  // And what the frame carries is still shown, so the label is checkable rather than asserted.
  await expect(tool.first()).toContainText("tcall_coding_1");
  await expect(tool.first()).toContainText("irreversible");
});

// THE NON-VACUITY HALF, and without it the test above is satisfied by a build that simply deleted the
// fallback. A frame with no `tool_name` — a run that started before this deployment took E30 T2, or a
// stack that has not — must still be drawn honestly rather than as a tool named "".
test("a frame that carries no tool name still says so", async ({ page }) => {
  await page.goto(CHAT);
  await pickFirstRepo(page);
  await send(page, "unnamed tool please");

  // Same reason as above: the sentence lives inside the collapsible, so the card is opened first.
  await expect(page.getByTestId("chat-tool-header").first()).toBeVisible({ timeout: 30_000 });
  await page.getByTestId("chat-tool-header").first().click();

  const gap = page.getByTestId("chat-tool-name-gap").first();
  await expect(gap).toBeVisible();
  await expect(gap).toContainText("tool_name");
});

test("the approval renders the destination AND the identity, both of which the frame does not carry", async ({ page }) => {
  await page.goto(CHAT);
  await pickFirstRepo(page);
  await send(page, "push it");

  const approval = page.getByTestId("chat-approval");
  await expect(approval).toBeVisible({ timeout: 30_000 });

  // WHERE the write goes. None of these four are on approval.requested.v1; they come from the
  // server-side join to GET /v1/approvals, which is the whole reason that join exists.
  await expect(page.getByTestId("approval-remote")).toHaveText("http://127.0.0.1:8177/demo-target.git");
  await expect(page.getByTestId("approval-branch")).toHaveText("agent/ws_fake/run_fake");
  await expect(page.getByTestId("approval-base")).toHaveText("main");
  await expect(page.getByTestId("approval-head")).toHaveText("4f4f89e3b2d083f46a9f539fc82220f5e1417630");

  // AS WHOM. This is the field commit e8802630 added, and the demo before this branch had no way to
  // show it because the frame carries no credential at all.
  await expect(page.getByTestId("approval-credential")).toContainText("this repository binding's own credential");
  await expect(page.getByTestId("approval-credential")).toContainText("demo-local-token");

  // The join succeeded, so the "could not be read" warning must NOT be showing.
  await expect(page.getByTestId("approval-join-gap")).toHaveCount(0);
});

test("pressing Approve applies through the HTTP path, forwarding the one-shot request hash", async ({ page }) => {
  await page.goto(CHAT);
  await pickFirstRepo(page);
  await send(page, "push it");

  await expect(page.getByTestId("chat-approval")).toBeVisible({ timeout: 30_000 });
  await page.getByTestId("chat-approve").click();
  await expect(page.getByTestId("approval-outcome")).toContainText("Approved");
  // AND IT DOES NOT PROMISE THE PUSH. The line used to say the durable command applies and the parked
  // run wakes — three promises, one of which this screen cannot observe — and the operator then read
  // further down that the push was NOT confirmed. Reassurance followed by a retraction is worse than
  // either alone, because the reassurance is what they act on.
  await expect(page.getByTestId("approval-outcome")).toContainText("separate");
  await expect(page.getByTestId("approval-outcome")).not.toContainText("the durable command applies");

  // THE DECISION REACHED THE CONTROL PLANE. The fake refuses any body whose request_hash does not
  // match the one the run emitted — "an approval id alone authorizes nothing" — so a decision that
  // registers here is a decision that carried the binding.
  const seen = await page.request.get(`http://127.0.0.1:${UPSTREAM_PORT}/__introspect-coding`, {
    headers: { Authorization: `Bearer ${API_KEY}` },
  });
  expect(((await seen.json()) as { approvalDecision: string }).approvalDecision).toBe("approved");
});

test("an approval that APPLIED but never published says the push is not confirmed", async ({ page }) => {
  await page.goto(CHAT);
  await pickFirstRepo(page);
  await send(page, "push it");

  // The scripted stream emits approval.approved.v1 and then reaches its terminal WITHOUT a
  // publication.published.v1 — exactly what a stack with no configured publisher does. The screen
  // must not let "approved" read as "pushed".
  const notice = page.getByTestId("chat-notice").filter({ hasText: "THE PUSH IS NOT CONFIRMED" });
  await expect(notice).toBeVisible({ timeout: 30_000 });
  await expect(notice).toContainText("PALAI_GITHUB_APP_ID");
  await expect(notice).toContainText("connection_ref");
});

test("the workspace panel fills the file tree, the diff and the shell transcript from the run's artifacts", async ({ page }) => {
  await page.goto(CHAT);
  await pickFirstRepo(page);
  await send(page, "add a contributing guide");

  await expect(page.getByTestId("file-tree")).toBeVisible({ timeout: 30_000 });
  await expect(page.getByTestId("file-tree")).toContainText("CONTRIBUTING.md");
  await expect(page.getByTestId("file-diff-name")).toHaveText("repo/CONTRIBUTING.md");
  await expect(page.getByTestId("file-diff")).toContainText("Be kind.");

  // The transcript parser must find THREE commands and keep each exit code with its own command —
  // including the non-zero one, whose styling is the difference between "it ran" and "it worked".
  const commands = page.getByTestId("shell-command");
  await expect(commands).toHaveCount(3);
  await expect(page.getByTestId("shell-cmd").nth(0)).toHaveText("git -C repo add CONTRIBUTING.md");
  await expect(page.getByTestId("shell-exit").nth(0)).toContainText("exit 0");
  await expect(page.getByTestId("shell-cmd").nth(2)).toHaveText("git -C repo push");
  await expect(page.getByTestId("shell-exit").nth(2)).toContainText("exit 128");

  // The panel names the bytes it read, and repeats Palai's own mislabel rather than tidying it away.
  await expect(page.getByTestId("workspace-evidence")).toContainText("art_patch_0001");
  await expect(page.getByTestId("workspace-mislabel")).toContainText("test-result");
});

// BUILD ARTEFACTS DO NOT BURY THE RUN'S OWN WORK — AND THEY ARE NOT SILENTLY DROPPED EITHER.
//
// MEASURED 2026-08-02: `swift build` inside the clone wrote 40-odd files under `.build/`, the tree
// presented every one as the run's work, and the diff pane opened on a compiler MODULE CACHE. The
// panel that exists to say what the AGENT did was showing what the COMPILER did.
//
// The clone carries no `.gitignore` (verified by cloning it), so the changeset is right to count them
// and this is the screen's problem. Both halves are asserted, because each alone is satisfiable by a
// wrong build: showing the edit first is satisfied by a filter that DROPS the artefacts, and counting
// them is satisfied by a tree that still buries the edit under forty rows.
test("build artefacts are collapsed behind a stated rule, and the run's own edit is what opens", async ({ page }) => {
  await page.goto(CHAT);
  await pickFirstRepo(page);
  await send(page, "add a contributing guide");
  await expect(page.getByTestId("file-tree")).toBeVisible({ timeout: 30_000 });

  // The authored file is in the tree and is the one selected — not the first path in the changeset.
  await expect(page.getByTestId("file-diff-name")).toHaveText("repo/CONTRIBUTING.md");

  // The artefacts are COUNTED and the rule is on screen. A silent filter would fail this.
  const artefacts = page.getByTestId("tree-artefacts");
  await expect(artefacts).toBeVisible();
  await expect(artefacts).toContainText("build artefact");
  await expect(artefacts).toContainText(".build/");
  await expect(artefacts).toContainText(".gitignore");

  // And they are REACHABLE. "Collapsed" has to mean collapsed; a count with no way to see the rows
  // would be the same silent drop wearing a number.
  await expect(page.getByTestId("file-tree")).not.toContainText(".build/artefact-one.txt");
  await page.getByTestId("tree-artefacts-toggle").click();
  await expect(page.getByTestId("file-tree")).toContainText(".build/artefact-one.txt");
});

// THE SWITCH SAYS WHAT IT GATES. A build completing with both switches OFF is correct and reads as a
// broken control; the screen has to name the rule rather than leave the operator to infer it.
test("the auto-approve panel says which calls the switch actually gates", async ({ page }) => {
  await page.goto(CHAT);
  await page.getByTestId("auto-approve-summary").click();
  const scope = page.getByTestId("auto-approve-scope");
  await expect(scope).toBeVisible();
  await expect(scope).toContainText("approval_required");
  await expect(scope).toContainText("a build runs either way");
});

// THE PANEL IS COLLAPSED BY DEFAULT, AND THE ONE LINE THAT REMAINS STATES THE STATE.
//
// The owner's verdict on the old block: "şurayı kaldır aq sikeceğim bir sike de yaramıyor". Three cards
// reading OFF / OFF / "Nothing is armed" sat above the conversation on every turn — the top of the
// column spent saying nothing is happening. The feature is not deleted; the SPLIT is the safety
// property this demo exists to show. It is one line until somebody asks.
//
// BOTH HALVES ARE ASSERTED: that the switches are NOT rendered while collapsed (a "collapse" that only
// hides visually still costs the column), and that the summary names the state rather than being a
// decorative chevron.
test("the auto-approve panel is one line until it is asked for, and that line names the state", async ({ page }) => {
  await page.goto(CHAT);

  const summary = page.getByTestId("auto-approve-summary");
  await expect(summary).toBeVisible();
  await expect(summary).toContainText("Auto-approve");
  await expect(summary).toContainText("a session opens on the first turn");

  // Collapsed means UNMOUNTED, not merely invisible.
  await expect(page.getByTestId("auto-approve-tools")).toHaveCount(0);
  await expect(page.getByTestId("auto-approve-publications")).toHaveCount(0);

  await summary.click();
  await expect(page.getByTestId("auto-approve-tools")).toBeVisible();
  await expect(page.getByTestId("auto-approve-publications")).toBeVisible();
});

// THE SUMMARY NAMES THE HALVES — C16 on the surface an operator actually sees, since the panel is shut
// by default and this line is the whole control most of the time.
test("the collapsed line names WHICH half is armed", async ({ page }) => {
  await page.goto(CHAT);
  await pickFirstRepo(page);
  await send(page, "add a contributing guide");
  await expect(page.getByTestId("chat-run").first()).toContainText("completed", { timeout: 30_000 });

  const summary = page.getByTestId("auto-approve-summary");
  await expect(summary).toContainText("nothing armed");

  await summary.click();
  await page.getByTestId("auto-approve-tools").click();
  await expect(page.getByTestId("auto-approve-tools")).toHaveAttribute("aria-checked", "true");

  // The one line an operator reads at a glance distinguishes "builds armed, pushes ask" from "both".
  await expect(summary).toContainText("build commands armed · pushes ask");
  await expect(summary).not.toContainText("pushes armed");
});

// THE STATE LINE NAMES WHICH HALF IS ARMED. Arming builds while pushes stay off is the safety property
// the whole two-switch design exists for, and it is worth nothing if the screen renders the same
// sentence for "tools armed", "pushes armed" and "both armed" — three different exposures. The old line
// said only "Approvals are answered automatically under key:…", which is true of all three.
//
// THE UNARMED HALF IS ASSERTED TOO, because that is the half an operator is trusting.
test("the armed state names WHICH half is armed, and says the other still waits", async ({ page }) => {
  await page.goto(CHAT);
  await pickFirstRepo(page);
  await send(page, "add a contributing guide");
  await expect(page.getByTestId("chat-run").first()).toContainText("completed", { timeout: 30_000 });

  await page.getByTestId("auto-approve-summary").click();
  const state = page.getByTestId("auto-approve-state");

  await page.getByTestId("auto-approve-tools").click();
  await expect(state).toContainText("Gated tool calls are armed");
  await expect(state).toContainText("Pushes and pull requests still park the run");
  // The sentence must not be the both-armed one — that is the confusion this test exists for.
  await expect(state).not.toContainText("BOTH");

  await page.getByTestId("auto-approve-publications").click();
  await expect(state).toContainText("BOTH gated tool calls AND pushes are armed");
  await expect(state).toContainText("Nothing waits for a human.");
});

test("the file tree states that it can only ever show what the run changed", async ({ page }) => {
  await page.goto(CHAT);
  // Before any run there is nothing, and the reason is on screen rather than implied by emptiness.
  await expect(page.getByTestId("tree-empty")).toContainText("no route that enumerates a workspace");
});

// =============================================================================================
// THE THREE DEFECTS MEASURED ON THE LIVE STACK ON 2026-08-02, EACH WITH THE TEST THAT WOULD HAVE
// CAUGHT IT. All three were reported by the owner as one sentence: "çok fazla bug var".
// =============================================================================================

// =============================================================================================
// THE AGENT. The owner asked: "bir agent mı tanımladık biz? ios agent'ı gibi bir şey? adminde agent
// üzerinden o agent'ı mı run ediyoruz? bu sistem promptu nereden alıyor şu an?"
//
// The answer was no on every clause, and nothing on the screen said so. `agent_id` was a branch in
// the chat route that the client never took, so every turn ran with no pinned config; the only
// steering was a string constant in a React file.
// =============================================================================================

// THE STEERING IS ON A PUBLISHED REVISION, AND THE RUN IS PINNED TO IT.
//
// This asserts the whole chain in one turn, because each link alone is satisfiable by a build that
// does the wrong thing: a profile can exist and be unused, a revision can exist and be a DRAFT, and a
// create can carry an agent_id that names an agent with no instructions at all — which is exactly
// what the live stack's pre-existing `demo-coder` was (published, tool ceiling, instructions "").
test("the turn runs as a published agent, and the steering lives on its revision", async ({ page }) => {
  await page.goto(CHAT);
  await pickFirstRepo(page);
  await send(page, "add a contributing guide");
  await expect(page.getByTestId("chat-run").first()).toContainText("completed", { timeout: 30_000 });

  const seen = await page.request.get(`http://127.0.0.1:${UPSTREAM_PORT}/__introspect-coding`, {
    headers: { Authorization: `Bearer ${API_KEY}` },
  });
  const body = (await seen.json()) as {
    codingAgentIds: string[];
    codingInstructions: string[];
    agentProfiles: { id: string; name: string }[];
    agentRevisions: Record<string, { id: string; status: string; instructions: string; tools: unknown; model: string }[]>;
  };

  // 1. A profile exists, under the name the demo resolves on — not an id anybody typed.
  const profile = body.agentProfiles.find((p) => p.name === "ios-coder");
  expect(profile, "the demo must resolve or create the ios-coder profile").toBeDefined();

  // 2. Its revision is PUBLISHED and carries the instructions. A draft would steer nothing.
  const revisions = body.agentRevisions[profile!.id] ?? [];
  const published = revisions.filter((r) => r.status === "published");
  expect(published).toHaveLength(1);
  expect(published[0].instructions).toContain("swift build --package-path repo");
  expect(published[0].model).toBe("claude-sonnet-5");

  // 3. AND `tools` IS NULL, which is the difference between an agent that can work and one that
  //    cannot. automation/agents.go:60 — a non-nil set, EVEN EMPTY, is a ceiling the resolver
  //    intersects with the project grant. The live stack's other agent carries such a ceiling.
  expect(published[0].tools, "a revision tools list is a CEILING, not a grant — it must stay nil").toBeNull();

  // 4. The run was actually pinned to it. Without this the three above describe an agent nobody uses.
  expect(body.codingAgentIds[0]).toBe(profile!.id);

  // 5. And the steering is NOT also sent on the request. resolveInstructionLayers COMPOSES layer 3
  //    with layer 5, so sending both would put the same paragraph in the conversation twice.
  expect(body.codingInstructions[0]).toBe("");
});

// AND THE SCREEN SAYS SO. The owner had to ASK which agent was running, which is the report that the
// screen was not answering it.
test("the screen names the agent the session runs as", async ({ page }) => {
  await page.goto(CHAT);
  await pickFirstRepo(page);
  await send(page, "add a contributing guide");

  const agentLine = page.getByTestId("chat-agent");
  await expect(agentLine).toBeVisible({ timeout: 30_000 });
  await expect(agentLine).toContainText("ios-coder");
  await expect(agentLine).toContainText("aprof_");
});

// DEFECT ONE: THE INTERNAL PROMPT WAS PUT IN THE OPERATOR'S MOUTH.
//
// Measured on screen: the operator typed "selam" — five characters — and their own message bubble
// rendered 470, a paragraph about `./repo`, `git -C repo` and `user.email=agent@palai.local`. It was an
// instruction to the model, shown as though they had written it.
//
// BOTH HALVES ARE ASSERTED AND THE SECOND IS THE ONE THAT MATTERS. "The bubble is clean" is satisfied
// just as well by deleting the hint entirely, which would break every coding turn — so this also reads
// what the create actually carried.
test("the operator's bubble holds ONLY what the operator typed — and the hint still reaches the model", async ({ page }) => {
  await page.goto(CHAT);
  await pickFirstRepo(page);
  await send(page, "selam");

  const bubble = page.getByTestId("chat-message-user").first();
  await expect(bubble).toBeVisible();
  await expect(bubble).toHaveText("selam");

  // Not one word of the internal instruction is on the page as something the operator said.
  await expect(bubble).not.toContainText("git -C repo");
  await expect(bubble).not.toContainText("agent@palai.local");

  // AND THE STEERING STILL REACHED THE MODEL — by a different road than it used to. The create
  // carries `agent_id`, and the agent's published revision carries the text. Reading BOTH is what
  // makes this test survive the move: `input` is exactly what was typed, and the steering exists.
  const seen = await page.request.get(`http://127.0.0.1:${UPSTREAM_PORT}/__introspect-coding`, {
    headers: { Authorization: `Bearer ${API_KEY}` },
  });
  const body = (await seen.json()) as {
    codingInputs: string[];
    codingAgentIds: string[];
    agentProfiles: { id: string; name: string }[];
    agentRevisions: Record<string, { status: string; instructions: string }[]>;
  };
  expect(body.codingInputs[0]).toBe("selam");

  const profile = body.agentProfiles.find((p) => p.name === "ios-coder")!;
  expect(body.codingAgentIds[0]).toBe(profile.id);
  const published = (body.agentRevisions[profile.id] ?? []).find((r) => r.status === "published")!;
  expect(published.instructions).toContain("git -C repo");
  expect(published.instructions).toContain("--package-path repo");
});

// DEFECT TWO: THE HINT ONLY EVER RODE TURN ONE.
//
// The old code gated it on `messages.length === 0`, so by turn three the model no longer knew where the
// clone was. Measured cost on one live run: eight tool calls to do one build, six of them `swift build`
// against a directory with no Package.swift.
//
// THE CARRIER CHANGED AND THE PROPERTY DID NOT. The pin is a RUN-SPECIFIC field — `agent_id` is per
// response, not per session — so "pinned once" and "pinned every turn" are genuinely different wire
// traffic, and this reads the wire. This is UAT C4 ("what the model was told does not decay") in its
// checkable form: the config the run resolves is attached to EVERY run, so a later turn cannot lose
// it the way a first-message hint did.
test("the agent is pinned on EVERY turn, not just the first", async ({ page }) => {
  await page.goto(CHAT);
  await pickFirstRepo(page);

  await send(page, "selam");
  await expect(page.getByTestId("chat-run").first()).toContainText("completed", { timeout: 30_000 });

  await send(page, "build al projeyi");
  await expect(page.getByTestId("chat-run").nth(1)).toContainText("completed", { timeout: 30_000 });

  const seen = await page.request.get(`http://127.0.0.1:${UPSTREAM_PORT}/__introspect-coding`, {
    headers: { Authorization: `Bearer ${API_KEY}` },
  });
  const body = (await seen.json()) as {
    codingInputs: string[];
    codingAgentIds: string[];
    agentProfiles: { id: string; name: string }[];
  };
  expect(body.codingInputs).toEqual(["selam", "build al projeyi"]);

  const profile = body.agentProfiles.find((p) => p.name === "ios-coder")!;
  expect(body.codingAgentIds).toHaveLength(2);
  // The SECOND one is the assertion. A build that pinned the agent only when opening a session — the
  // shape the old `messages.length === 0` gate had — would have "" here.
  expect(body.codingAgentIds).toEqual([profile.id, profile.id]);
});

// DEFECT THREE, AND IT IS THE ONE THAT MADE THE DEMO LOOK BROKEN: EVERY TURN AFTER THE FIRST RENDERED
// NOTHING AT ALL.
//
// Measured on the live stack, session ses_0438bd21b5b6562890fabbec865c10ea. Turn two ("build al
// projeyi") showed "run queued", "run completed" and nothing else. The run had made SIX tool calls and
// written a full answer — both readable afterwards on GET /v1/responses/resp_47c7eb59…{,/tool-calls}.
//
// Cause: one session, many runs. The adapter opened the journal at cursor 0; the server replayed run
// ONE, hit its `run.completed.v1`, and closed the connection by design (api/events.go:149, whose own
// comment says "an LP session carries a single run"). The pump read someone else's terminal as its own.
//
// THIS TEST IS THE REASON THE FAKE GREW A SECOND RUN AND AN `after_sequence` CURSOR. Against the old
// fake — which replayed from the start whatever cursor it was given, and never held a second run —
// this assertion could not have failed, and so could not have passed for the right reason either.
test("a SECOND turn renders its own tool calls and its own answer", async ({ page }) => {
  await page.goto(CHAT);
  await pickFirstRepo(page);

  await send(page, "add a contributing guide");
  await expect(page.getByTestId("chat-run").first()).toContainText("completed", { timeout: 30_000 });
  const firstTurnTools = await page.getByTestId("chat-tool").count();
  expect(firstTurnTools).toBeGreaterThan(0);

  await send(page, "build al projeyi");
  await expect(page.getByTestId("chat-run").nth(1)).toContainText("completed", { timeout: 30_000 });

  // THE SECOND TURN'S OWN CALL IS ON SCREEN. Run two makes exactly one, and it is a different command
  // from anything run one ran, so this cannot be satisfied by run one's cards still being visible.
  const secondMessage = page.getByTestId("chat-message-ai").nth(1);
  await expect(secondMessage.getByTestId("chat-tool")).toHaveCount(1);
  await expect(secondMessage.getByTestId("chat-tool-header")).toContainText("palai.workspace.shell");

  // AND ITS OUTPUT WAS READ. The ledger join is what carries the command line and the exit code; a
  // second turn whose join never fired would show the card and none of this.
  await expect(secondMessage.getByTestId("chat-tool-detail")).toBeVisible();
  await expect(secondMessage).toContainText("swift build --package-path repo");

  // AND ITS ANSWER. This is what the operator actually asked for and what the old build dropped.
  await expect(secondMessage.getByTestId("chat-ai-text")).toContainText("Build complete");
});

// THE NON-VACUITY HALF OF THE JOIN. A card whose ledger read fails must SAY the output could not be
// read rather than draw an empty successful build — and the sentence has to name how hard it tried, so
// a wrong retry bound is visible on the surface instead of hidden behind a longer wait.
test("a tool call whose ledger row never lands says so, and says how many attempts it made", async ({ page }) => {
  await page.goto(CHAT);
  await pickFirstRepo(page);
  // The fake answers this response's tool-calls read with a row list that does not contain the call id
  // the frames carried, which is exactly what an uncommitted ledger row looks like from here.
  await send(page, "unjoinable tool please");

  const unjoined = page.getByTestId("chat-tool-detail-unjoined").first();
  await expect(unjoined).toBeVisible({ timeout: 30_000 });
  await expect(unjoined).toContainText("could not be read");
  await expect(unjoined).toContainText("10 attempts");
});

// THE STOP CONTROL. palcore has one and so does this; a turn an operator cannot interrupt is a turn
// they have to wait out. Aborting the browser's request must NOT cancel the run — disconnect is not
// cancellation, which is the property the /page proof already makes about its own transport.
test("a turn in flight can be stopped from the chat", async ({ page }) => {
  await page.goto(CHAT);
  await pickFirstRepo(page);
  await send(page, "add a contributing guide");

  const stop = page.getByTestId("chat-stop");
  await expect(stop).toBeVisible({ timeout: 30_000 });
  await stop.click();

  // The send button comes back, which is the operator-visible statement that the turn is over.
  await expect(page.getByTestId("chat-send")).toBeVisible();
  await expect(page.getByTestId("chat-message-ai").first()).toContainText("[Cancelled]");
});

test("no Palai credential reaches the browser on any /chat surface", async ({ page }) => {
  const leaked: string[] = [];
  page.on("request", (req) => {
    const headers = req.headers();
    if (Object.values(headers).some((v) => v.includes(API_KEY))) leaked.push(`header:${req.url()}`);
    if ((req.postData() ?? "").includes(API_KEY)) leaked.push(`body:${req.url()}`);
    // Every browser request must go to this app's own origin.
    if (req.url().includes(`:${UPSTREAM_PORT}`) && !req.url().includes("__introspect")) leaked.push(`upstream:${req.url()}`);
  });

  await page.goto(CHAT);
  await pickFirstRepo(page);
  await send(page, "add a contributing guide");
  await expect(page.getByTestId("file-tree")).toBeVisible({ timeout: 30_000 });

  expect(leaked).toEqual([]);
  await expect(page.locator("body")).not.toContainText(API_KEY);
  const html = await page.content();
  expect(html).not.toContain(API_KEY);
});
