import { expect, test, type Page } from "@playwright/test";

import { AGENT_TEMPLATES, buildRevisionBody, parseTemplateYAML } from "../lib/agentTemplates";
import { AUTH_LABEL, isConnectable, MCP_CATALOGUE } from "../lib/mcpCatalogue";
import { announceProfile, signIn } from "./profile";

// THE MCP CATALOGUE AND THE AGENT TEMPLATE GALLERY.
//
// TWO FEATURES, ONE FILE, because they share the property being proven: each one shows an operator something
// this deployment CANNOT do, and the value of both rests entirely on that half being right. A catalogue that
// listed Slack as connectable, or a template that named a field the API rejects, would be a screen that
// lies — and a lying screen is worse than an absent one, because it is discovered at the end of a
// registration rather than at the start.
//
// THE PARSER LEGS RUN HERE RATHER THAN UNDER `node --test`, AND THAT IS A DELIBERATE PLACEMENT. The
// repository's own rule: a check that the shipped invocation does not name never runs. `pnpm sweep` — the
// only `node --test` entry point — is invoked by scripts/uat/fleet-console ONLY inside the
// `RUN_CONSOLE_REAL` arm (line 258), so a parser test living there would be skipped on every default run
// and the gate would stay green. `pnpm exec playwright test` (line 119) passes no testMatch and no --grep,
// so playwright.config.ts collects this file automatically. The legs below therefore ride the tier that
// always runs.

announceProfile("mcp-catalogue-templates.spec.ts");

// ─────────────────────────────────────────────────────────────────────────────────────────────────────────
// THE CATALOGUE'S OWN CONSISTENCY — no browser needed, and it fails at the data rather than at a screenshot
// ─────────────────────────────────────────────────────────────────────────────────────────────────────────

test("every catalogue entry is classified, and only Bearer/Basic/no-credential entries are offered", () => {
  expect(MCP_CATALOGUE.length, "the catalogue is empty — this suite would be judging nothing").toBeGreaterThan(20);

  const ids = MCP_CATALOGUE.map((e) => e.id);
  expect(new Set(ids).size, `duplicate catalogue id: ${ids.join(", ")}`).toBe(ids.length);

  for (const entry of MCP_CATALOGUE) {
    // EVERY ROW CITES SOMETHING. A classification with no source is the shape this repository keeps finding
    // — a number written down once and never re-derivable.
    expect(entry.evidence, `${entry.id} has no evidence URL`).toMatch(/^https:\/\//);
    expect(entry.quote.length, `${entry.id} has no supporting quote`).toBeGreaterThan(10);
    expect(AUTH_LABEL[entry.auth], `${entry.id} has an unlabelled auth bucket`).toBeTruthy();

    if (isConnectable(entry)) {
      // A connectable entry must be one of the three buckets the transport can actually render. This is the
      // assertion that would fail if someone added an `oauth` row to the ready group by hand.
      expect(["none", "bearer", "basic"], `${entry.id} is offered but its auth is ${entry.auth}`).toContain(entry.auth);
      expect(entry.url, `${entry.id} has no https URL`).toMatch(/^https:\/\//);
    } else {
      // AND A REFUSED ENTRY MUST SAY WHY, in a sentence. A greyed row with no reason sends an operator to
      // read our source to find out what happened to their integration.
      expect(entry.refusal, `${entry.id} is refused with no reason on the screen`).toBeTruthy();
      expect((entry.refusal ?? "").length, `${entry.id}'s refusal is too short to be a reason`).toBeGreaterThan(30);
    }
  }
});

test("the servers shipped evidence says we cannot reach are refused, each for its own recorded reason", () => {
  const by = (id: string) => {
    const entry = MCP_CATALOGUE.find((e) => e.id === id);
    expect(entry, `the catalogue has no ${id} row`).toBeDefined();
    return entry!;
  };

  // SLACK — evidence/releases/runner-fleet-0.1.0/manifest.json records this verbatim: "mcp.slack.com accepts
  // only confidential OAuth 2.0 with a USER token (M2), and Palai has no authorization-code flow for MCP
  // connections at all". A catalogue that offered it would contradict a shipped release bundle.
  expect(by("slack").auth).toBe("oauth");
  expect(isConnectable(by("slack"))).toBe(false);

  // SENTRY — AND THIS IS THE ONE A DOCS SKIM GETS WRONG. Sentry DOES document a headless static token, which
  // reads exactly like GitHub's or Linear's. It rides a `Sentry-Bearer` scheme, and
  // adapters/integrations/mcp/http.go:70 recognises `Bearer ` and `Basic ` only — so authorizationHeader()
  // would send `Bearer Sentry-Bearer <token>`. It is refused for a DIFFERENT reason than Slack, and the two
  // must not collapse into one "unsupported" bucket: building an OAuth flow would fix Slack and not this.
  expect(by("sentry").auth).toBe("customScheme");
  expect(by("sentry").refusal).toContain("Sentry-Bearer");

  // ATLASSIAN — the counter-example that keeps the refusals honest. The same shipped manifest records that
  // its "API-token path WORKS TODAY", and this tree drives a real Basic credential through the transport in
  // apps/control-plane/internal/extensions/mcp_jira_component_test.go. If everything were refused this
  // screen would be useless; this row is why the classification is a measurement and not a policy.
  expect(by("atlassian").auth).toBe("basic");
  expect(isConnectable(by("atlassian"))).toBe(true);

  // AND THE CATALOGUE IS NOT MOSTLY-REFUSED. A directory whose useful half is a rounding error is a
  // different product than the one asked for, and this asserts the shape of what shipped.
  const ready = MCP_CATALOGUE.filter(isConnectable).length;
  // eslint-disable-next-line no-console -- the split IS the feature; a bare pass says nothing about it.
  console.log(
    `MCP CATALOGUE — ${String(ready)}/${String(MCP_CATALOGUE.length)} connectable today; refused: ` +
      MCP_CATALOGUE.filter((e) => !isConnectable(e))
        .map((e) => `${e.id}(${e.auth})`)
        .join(", "),
  );
  expect(ready, "fewer than half the catalogue is connectable").toBeGreaterThan(MCP_CATALOGUE.length / 2);
});

// ─────────────────────────────────────────────────────────────────────────────────────────────────────────
// THE PARSER — the half that decides whether a pasted template becomes a revision or a 400
// ─────────────────────────────────────────────────────────────────────────────────────────────────────────

test("every shipped template parses, and neither name nor description reaches the revision body", () => {
  expect(AGENT_TEMPLATES.length).toBeGreaterThan(0);
  for (const t of AGENT_TEMPLATES) {
    const { template, errors } = parseTemplateYAML(t.yaml);
    expect(errors, `${t.id} does not parse: ${JSON.stringify(errors)}`).toEqual([]);
    expect(template, `${t.id} produced no template`).not.toBeNull();

    // THE BLOCK SCALAR KEPT ITS SHAPE. `system: |-` is the one construct here whose failure is silent — a
    // parser that flattened it would still produce a valid revision, with the instructions run together.
    expect(template!.system, `${t.id}'s system prompt lost its line breaks`).toContain("\n");
    expect(template!.name.length, `${t.id} has no name`).toBeGreaterThan(0);

    // THE MAPPING, ASSERTED. `name` belongs to the profile (a different call) and `description` belongs to
    // nothing at all — agent_profiles has no such column (storage/migrations/000019_agents.up.sql:16-25).
    // Sending either would be a DisallowUnknownFields 400 from DecodeRevisionInput.
    const body = buildRevisionBody(template!);
    expect(Object.keys(body), `${t.id} leaks name/description into the revision body`).not.toContain("name");
    expect(Object.keys(body)).not.toContain("description");
    // And every key that IS sent is a RevisionInput field (automation/agents.go:76-88).
    for (const key of Object.keys(body)) {
      expect(
        ["model", "instructions", "tools", "tool_sets", "mcp_connections", "environment"],
        `${t.id} sends \`${key}\`, which RevisionInput has no field for`,
      ).toContain(key);
    }
  }
});

test("the fields Palai has no counterpart for are refused by NAME, with the line number", () => {
  // The three keys from the reference console's template that this tree cannot honour. Each must fail at the
  // editor rather than as a 400 two round trips later — and must name the field, so the operator learns
  // which one rather than that "something" was wrong.
  for (const field of ["metadata", "permission_policy", "mcp_servers"]) {
    const { template, errors } = parseTemplateYAML(`name: Probe\n${field}: whatever\n`);
    expect(template, `\`${field}\` was accepted — it has no counterpart and would be a 400`).toBeNull();
    expect(errors.some((e) => e.line === 2 && e.message.includes(field)), `\`${field}\` was not named on line 2: ${JSON.stringify(errors)}`).toBe(true);
  }

  // A NAMELESS TEMPLATE IS REFUSED HERE, because POST /v1/agents refuses it there (store/agents.go
  // MissingName) — and a profile that fails to create takes the revision with it.
  expect(parseTemplateYAML("model: claude-opus-4-8\n").template).toBeNull();

  // EVERY ERROR CARRIES A REAL LINE. A line number of 0, or past the end, is a gutter that highlights
  // nothing — the failure mode that makes numbered errors worse than unnumbered ones.
  const messy = "name: Probe\nnope: 1\n  bad-indent: 2\ntools: not-a-list\n";
  const { errors } = parseTemplateYAML(messy);
  expect(errors.length).toBeGreaterThan(2);
  for (const e of errors) {
    expect(e.line, `an error points at line ${e.line}, outside the document`).toBeGreaterThan(0);
    expect(e.line).toBeLessThanOrEqual(messy.split("\n").length);
  }
});

// ─────────────────────────────────────────────────────────────────────────────────────────────────────────
// THE SCREENS
// ─────────────────────────────────────────────────────────────────────────────────────────────────────────

/** openCatalogue signs in, lands on /tools, and opens the directory. */
async function openCatalogue(page: Page): Promise<void> {
  await signIn(page);
  await page.goto("/tools");
  await expect(page.getByTestId("panel-mcp-connections")).toBeVisible();
  await page.getByTestId("catalogue-open").click();
  await expect(page.getByTestId("catalogue-dialog")).toBeVisible();
}

test("the catalogue offers a Use control for a connectable server and NONE for a refused one", async ({ page }) => {
  await openCatalogue(page);

  // FOCUS IS ASSERTED, NOT ASSUMED. A dialog opened from a MENU does not hold focus for one frame — the menu
  // restores focus to its trigger as it closes (measured on /policy: t0 outside, 16ms inside). This one opens
  // from an ordinary button, which is exactly why it should be checked: the claim "so it is fine" is the kind
  // this tree keeps finding false.
  await expect
    .poll(async () => page.evaluate(() => document.querySelector('[data-testid="catalogue-dialog"]')?.contains(document.activeElement) ?? false))
    .toBe(true);

  // GitHub is a static-token server, so it gets a button.
  await expect(page.getByTestId("catalogue-entry-github")).toBeVisible();
  await expect(page.getByTestId("catalogue-use-github")).toBeVisible();

  // Slack and Sentry are listed — PRESENT, so the gap is visible — and offer no way to try.
  for (const id of ["slack", "sentry"]) {
    await expect(page.getByTestId(`catalogue-entry-${id}`), `${id} is not listed at all`).toBeVisible();
    await expect(page.getByTestId(`catalogue-refusal-${id}`)).toBeVisible();
    await expect(page.getByTestId(`catalogue-use-${id}`), `${id} offers a Use button it cannot honour`).toHaveCount(0);
  }

  // THE REFUSAL IS ON THE SCREEN, not only in a data file. The reason an operator reads must be the reason
  // the classification rests on.
  await expect(page.getByTestId("catalogue-refusal-sentry")).toContainText("Sentry-Bearer");
  await expect(page.getByTestId("catalogue-refusal-slack")).toContainText("OAuth");
});

test("choosing a catalogue entry fills the registration form and registers nothing", async ({ page }) => {
  await openCatalogue(page);
  await page.getByTestId("catalogue-use-deepwiki").click();

  // The dialog closes and the form behind it now carries the entry — the whole point of the picker.
  await expect(page.getByTestId("catalogue-dialog")).toHaveCount(0);
  await expect(page.getByTestId("mcp-url-input")).toHaveValue("https://mcp.deepwiki.com/mcp");
  await expect(page.getByTestId("mcp-name-input")).toHaveValue("deepwiki");

  // AND NOTHING WAS REGISTERED. Picking is not committing: the connection list is untouched until the
  // operator submits, which matters because registering is what a discovery later dials.
  await expect(page.getByTestId("mcp-connection-status")).toContainText("Nothing has been registered yet");
});

test("a template opens as editable YAML, and a field with no counterpart is refused by line before anything is created", async ({ page }) => {
  await signIn(page);
  await page.goto("/agents");
  await expect(page.getByTestId("panel-agent-templates")).toBeVisible();

  await page.getByTestId("template-open-incident-commander").click();
  await expect(page.getByTestId("template-dialog")).toBeVisible();

  const editor = page.getByTestId("template-yaml");
  await expect(editor).toBeVisible();
  await expect(editor).toHaveValue(/name: Incident commander/);

  // THE EDITOR IS EDITABLE, and the errors are the parser's rather than the server's. Appending a
  // `permission_policy` — the reference console's field, and the one this tree deliberately has no
  // counterpart for — must fail HERE.
  await editor.fill("name: Probe agent\npermission_policy: always_allow\n");
  await page.getByTestId("template-apply").click();

  await expect(page.getByTestId("template-errors")).toBeVisible();
  await expect(page.getByTestId("template-errors")).toContainText("Line 2");
  await expect(page.getByTestId("template-errors")).toContainText("permission_policy");
  // NOTHING WAS CREATED — the dialog is still open and the console has not navigated to an agent page.
  await expect(page.getByTestId("template-dialog")).toBeVisible();
  expect(page.url()).toContain("/agents");
  expect(page.url()).not.toMatch(/\/agents\/[a-z]/);
});
