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

  // THE CARD IS OPENED BEFORE THE ABSENCE IS ASSERTED, and that ordering is the assertion. ToolContent
  // is a Radix CollapsibleContent, which UNMOUNTS while closed — so `toHaveCount(0)` against a shut
  // card is true of every card ever rendered, including one still full of the old apology. This tree
  // has the same defect on record from an axe sweep that scanned routes with every dialog closed and
  // reported a cleaner number while covering less.
  await page.getByTestId("chat-tool-header").first().click();
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

test("the file tree states that it can only ever show what the run changed", async ({ page }) => {
  await page.goto(CHAT);
  // Before any run there is nothing, and the reason is on screen rather than implied by emptiness.
  await expect(page.getByTestId("tree-empty")).toContainText("no route that enumerates a workspace");
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
