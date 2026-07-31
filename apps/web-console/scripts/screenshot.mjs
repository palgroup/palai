// Drives the BUILT console and writes one full-page screenshot per route, in one colour scheme.
//
// It exists because the thing this pass changed cannot be read off a diff: an information architecture and a
// visual identity are judged by looking at them, and "looks right on the machine the author was using" is
// exactly the claim this tree does not accept. So the images are produced by a script anyone can re-run,
// against the same fake upstream the suite uses, at a pinned viewport and device scale.
//
// It signs in through the REAL login form with the REAL password, like every spec does — no forged cookie.
//
//   node tests/fake-control-plane.mjs &                         # FAKE_UPSTREAM_PORT=<upstream>
//   PALAI_CONSOLE_PASSWORD_HASH=$(printf %s "$PW" | node scripts/hash-password.mjs | cut -d= -f2-) \
//     PALAI_API_KEY=… PALAI_BASE_URL=http://127.0.0.1:<upstream> pnpm exec next start -p <port>
//   node scripts/screenshot.mjs <out-dir> <light|dark> / /agents /registry
//
// PALAI_CONSOLE_ORIGIN and PALAI_CONSOLE_PASSWORD override the loopback defaults below.
import { chromium } from "@playwright/test";

const ORIGIN = process.env.PALAI_CONSOLE_ORIGIN ?? "http://127.0.0.1:3200";
// The suite's own operator password (tests/constants.ts). It opens a loopback fixture this repo starts and
// stops; the point is that the door is real, not that this password is secret.
const PASSWORD = process.env.PALAI_CONSOLE_PASSWORD ?? "console-proof-operator-password-DO-NOT-REUSE-4c1f9a2b";

const [out, scheme = "light", ...routes] = process.argv.slice(2);
if (out === undefined || routes.length === 0) {
  console.error("usage: node scripts/screenshot.mjs <out-dir> <light|dark> <route>…");
  process.exit(2);
}

const browser = await chromium.launch();
// 1440x1000 at 2x: a laptop the console is actually read on, and a retina capture so the 11px micro-labels
// are legible in the image rather than only on the screen.
const context = await browser.newContext({ viewport: { width: 1440, height: 1000 }, deviceScaleFactor: 2, colorScheme: scheme });
const page = await context.newPage();

await page.goto(`${ORIGIN}/login`);
await page.getByTestId("password-input").fill(PASSWORD);
await page.getByTestId("login-button").click();
await page.getByTestId("ceiling-note").waitFor({ timeout: 20_000 });

for (const route of routes) {
  // The pointer is parked in the corner first: it stays wherever the last click left it, and a row under it
  // renders its hover state — which then reads as a selected row in a still image.
  await page.mouse.move(4, 4);
  await page.goto(ORIGIN + route);
  await page.waitForLoadState("networkidle");
  const name = route === "/" ? "overview" : route.replace(/\//g, "");
  await page.screenshot({ path: `${out}/${name}-${scheme}.png`, fullPage: true });
  console.log(`shot ${name} ${scheme}`);
}

await browser.close();
