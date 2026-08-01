import { expect, test, type Page } from "@playwright/test";

import { announceProfile, signIn } from "./profile";

// THE DEPLOYMENT SCREEN, AND THE EVENING IT WAS WRITTEN AGAINST (machine-config).
//
// Measured on main at cf0efd63 (2026-08-01):
//
//   grep -oE 'PALAI_[A-Z_]+' deploy/compose/compose.yaml | sort -u   -> 24 settings
//   of those, readable from any /v1 route                            -> 0
//
// A stack was brought up with `make local-up`, which takes PALAI_DISPATCH_WORKERS=0 from
// deploy/compose/compose.yaml:82. The console accepted five runs and every one sat at run.queued.v1
// forever. Nothing on any screen said the deployment had no dispatcher; the value was eventually found
// with `docker inspect`.
//
// BOTH PROFILES RUN EVERY LEG HERE, and the blocking-warning leg is the one that needed thinking about.
// The real profile's stack is brought up with PALAI_DISPATCH_WORKERS=1 (tests/divergences.mjs records the
// invocation), so a fixture scripting a dispatch-off body would prove a console behaviour the real stack
// contradicts — the fake-diverges-from-real trap this suite has already been bitten by. So the blocking arm
// rewrites the response AT THE BROWSER BOUNDARY: those are the bytes a dispatch-off control plane sends,
// GET /v1/deployment is the same route on both sides, and what is being asserted is this console's
// rendering, which is true on both profiles or on neither.
test.beforeAll(() => announceProfile("deployment.spec.ts"));
test.beforeEach(async ({ page }) => signIn(page));

/** A dispatch-off body, in api/deployment.go's exact shape. */
const DISPATCH_OFF = {
  object: "deployment",
  settings: [
    {
      name: "PALAI_DISPATCH_WORKERS",
      group: "execution",
      value: "0",
      set: true,
      default: "1",
      kind: "value",
      effect: "How many durable dispatch workers run. ZERO IS NOT 'SLOWER', IT IS OFF.",
      mutability: "bring_up",
      change_with: "recreate the control-plane with the new value (`palai up`)",
      reader_file: "apps/control-plane/api/deployment.go",
      reader_func: "DispatchWorkers",
    },
  ],
  warnings: [
    {
      code: "dispatch_workers_zero",
      severity: "blocking",
      headline: "This deployment has no dispatcher — submitted runs are accepted and never executed.",
      detail:
        "PALAI_DISPATCH_WORKERS is 0, so startDispatch returns before it builds a worker. POST /v1/responses still returns 201 and the run stays at run.queued.v1 forever.",
      remedy: "Recreate the control-plane with PALAI_DISPATCH_WORKERS=1 or higher.",
      settings: ["PALAI_DISPATCH_WORKERS"],
    },
  ],
};

async function serveDispatchOff(page: Page) {
  await page.route("**/api/palai/v1/deployment", (route) =>
    route.request().method() === "GET" ? route.fulfill({ status: 200, json: DISPATCH_OFF }) : route.fallback(),
  );
}

// THE LEG THIS WHOLE TASK EXISTS FOR. /runs promises "Start a run and watch it happen"; with no dispatcher
// nothing happens. The banner has to be on THAT screen, above the control, and it has to say what changes
// it — a warning that names a fault and not its remedy sends the reader to a search engine.
test("a deployment with no dispatcher says so on the screen that offers to start a run", async ({ page }) => {
  await serveDispatchOff(page);
  await page.goto("/runs");

  const notice = page.getByTestId("deployment-warning-dispatch_workers_zero");
  await expect(notice).toBeVisible({ timeout: 15_000 });
  await expect(notice).toContainText("accepted and never executed");
  await expect(notice).toContainText("PALAI_DISPATCH_WORKERS");
  // THE REMEDY, asserted separately: the headline is the diagnosis and this is the treatment, and a banner
  // that carries only the first is the `docker inspect` evening in a coloured box.
  await expect(notice).toContainText("PALAI_DISPATCH_WORKERS=1");
  // BLOCKING IS A DIFFERENT BAND AND A DIFFERENT WORD. The severity is written out because colour alone
  // cannot carry it (UI-001, §47.5).
  await expect(notice).toHaveAttribute("data-severity", "blocking");
  await expect(notice).toContainText("Blocking:");

  // IT IS ABOVE THE PROMPT. A warning under the button it is about is read after the mistake.
  const noticeBox = await notice.boundingBox();
  const promptBox = await page.getByTestId("prompt-input").boundingBox();
  expect(noticeBox, "the notice rendered with no box").not.toBeNull();
  expect(promptBox, "the prompt rendered with no box").not.toBeNull();
  expect(noticeBox!.y).toBeLessThan(promptBox!.y);
});

// THE OTHER DIRECTION, AND IT IS WHAT KEEPS THE BANNER FROM BECOMING WALLPAPER. Both profiles run their
// stack with a dispatcher, so this is the unintercepted read: the blocking notice must be ABSENT while the
// page is otherwise fully rendered.
test("a deployment with a dispatcher raises no blocking notice", async ({ page }) => {
  await page.goto("/runs");
  await expect(page.getByTestId("prompt-input")).toBeVisible({ timeout: 15_000 });
  // Wait for the deployment read to have landed before concluding anything from an absence: asserting
  // "not visible" against a page that has not finished fetching is an assertion about timing.
  await expect(page.getByTestId("deployment-warning-model_provider_fake")).toBeVisible({ timeout: 15_000 });
  await expect(page.getByTestId("deployment-warning-dispatch_workers_zero")).toHaveCount(0);
});

// THE READ SURFACE ITSELF. Three properties, and each of them is a way the screen could have been useless
// while looking complete.
test("the deployment screen reports each setting's value, its fallback and what changes it", async ({ page }) => {
  await page.goto("/deployment");
  await expect(page.getByTestId("panel-deployment-settings")).toBeVisible({ timeout: 15_000 });

  const table = page.getByTestId("panel-deployment-settings").locator("table");
  await expect(table.getByText("PALAI_DISPATCH_WORKERS", { exact: true })).toBeVisible();

  // 1. AN UNSET SETTING SAYS WHAT THE PROCESS USES INSTEAD. "unset" alone is the wrong answer and it is the
  //    one a naive table gives: unset PALAI_DISPATCH_WORKERS runs ONE worker, while unset PALAI_S3_ENDPOINT
  //    means there is no object store at all. Opposite facts, same word.
  const unsetRow = table.locator("tr", { hasText: "PALAI_SANDBOX_IMAGE" });
  await expect(unsetRow).toContainText("— unset");
  await expect(unsetRow).toContainText("uses:");

  // 2. THE MUTABILITY IS ON THE SCREEN, AND SO IS THE THING THAT DOES CHANGE IT. A screen that says a value
  //    cannot be changed here and does not say what changes it has told the operator to go and find out.
  await expect(unsetRow).toContainText("Needs a bring-up");
  await expect(unsetRow).toContainText("palai up");

  // 3. A SETTING WITH A LIVE OVERRIDE IS NOT CALLED "NEEDS A BRING-UP". A project that publishes a model
  //    route dispatches through it with no restart, so labelling the env default the same way as a genuine
  //    restart-only setting would send an operator on an outage a POST would have avoided.
  const providerRow = table.locator("tr", { hasText: "PALAI_MODEL_PROVIDER" });
  await expect(providerRow).toContainText("overridable live");
  await expect(providerRow).toContainText("/v1/model-routes");
});

// THE CREDENTIAL RULE, ON THE SCREEN. The master key's PATH is what makes the secret store's posture
// visible, and the screen says which cells are handles — a reader who cannot tell a path from a value
// cannot tell a safe screen from a leaking one. The absence of the key itself is proven server-side, where
// the allow-list lives (apps/control-plane/api/deployment_test.go).
test("a path-valued setting is shown as a path and labelled as one", async ({ page }) => {
  await page.goto("/deployment");
  await expect(page.getByTestId("panel-deployment-settings")).toBeVisible({ timeout: 15_000 });
  const keyRow = page.getByTestId("panel-deployment-settings").locator("tr", { hasText: "PALAI_SECRET_MASTER_KEY_FILE" });
  await expect(keyRow).toContainText("/run/secrets/master_key");
  await expect(keyRow).toContainText("never the contents");
});
