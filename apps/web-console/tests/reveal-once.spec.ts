import AxeBuilder from "@axe-core/playwright";
import { expect, test, type Page, type Response as NetResponse } from "@playwright/test";

import { WCAG_TAGS } from "./constants";
import { announceProfile, sessionHeaders, signIn } from "./profile";

// A VALUE THE SERVER MINTED, SHOWN ONCE, AND FINDABLE NOWHERE AFTERWARDS (E28 T2, plan §3.6 D21).
//
// This is secret-never-returns.spec.ts pointed the other way. That spec writes a value the console must never
// show again; this one reads a value the console must show EXACTLY once — and the proof has the same shape,
// because in both cases the only available assertion is an absence and an absence is only worth what its
// enumeration is worth. So nothing here eyeballs: the sweep walks the whole DOM (including live input
// VALUES, which are properties and not markup), every key AND value of both web storages, the full URL, and
// every response body the browser received afterwards.
//
// THE VALUE IS COMPARED WITH A BOOLEAN, NEVER A MATCHER. A failing `.not.toContain(secret)` prints both
// operands, and on the real profile that operand is a LIVE CREDENTIAL in a CI log. Same discipline as
// public-api-only.spec.ts's key scan.
//
// WHAT IS NOT CLAIMED: that the bytes have left browser memory. A fetch response body lives until GC and no
// web API controls that (the plan files it as this task's honest ceiling). What is claimed is persistence and
// retrievability — after the region is gone, there is no place a later visitor, a page reload, or the API
// itself can produce the value from.
test.beforeAll(() => announceProfile("reveal-once.spec.ts"));
test.beforeEach(async ({ page }) => signIn(page));

/** mint drives the console's own form and returns the value the screen displayed, plus the key's id. */
async function mint(page: Page): Promise<{ value: string; id: string }> {
  await page.goto("/policy");
  await expect(page.getByTestId("panel-api-keys")).toBeVisible({ timeout: 15_000 });
  await page.getByTestId("key-scope-provision").check();
  await page.getByTestId("key-mint-button").click();
  const shown = page.getByTestId("key-reveal-value");
  await expect(shown).toBeVisible({ timeout: 15_000 });
  const value = (await shown.innerText()).trim();
  const meta = (await page.getByTestId("key-reveal-meta").innerText()).trim();
  // A value the scan cannot distinguish from noise makes every assertion below vacuous.
  expect(value.length, "the minted value is too short for a substring sweep to mean anything").toBeGreaterThan(16);
  return { value, id: meta.split(/\s+/)[0] };
}

/** revokeMinted cleans up after the run. On the real profile the mint above created a live credential. */
async function revokeMinted(page: Page, id: string): Promise<void> {
  if (id === "") return;
  await page.request.post(`/api/palai/v1/api-keys/${encodeURIComponent(id)}/revoke`, { headers: await sessionHeaders(page) });
}

/**
 * traces enumerates every PERSISTENT place in a browsing context a value could have been left, and returns
 * them named so a failure says WHICH one holds it.
 *
 *   dom      — the serialized document PLUS every input/textarea's live `value` property. innerHTML alone
 *              would miss exactly the field a value would most naturally be parked in.
 *   session  — sessionStorage, keys and values. The most tempting place: "so a refresh does not lose it".
 *   local    — localStorage, keys and values. The same temptation, one order of magnitude worse.
 *   url      — the full href, so a query parameter or a fragment is covered.
 */
async function traces(page: Page): Promise<Record<string, string>> {
  return page.evaluate(() => {
    const dump = (store: Storage) => {
      const parts: string[] = [];
      for (let i = 0; i < store.length; i++) {
        const k = store.key(i);
        if (k === null) continue;
        parts.push(k, store.getItem(k) ?? "");
      }
      return parts.join("\n");
    };
    const inputs = [...document.querySelectorAll("input, textarea")].map((el) => (el as HTMLInputElement).value).join("\n");
    return {
      dom: `${document.documentElement.outerHTML}\n${inputs}`,
      session: dump(window.sessionStorage),
      local: dump(window.localStorage),
      url: window.location.href,
    };
  });
}

/** expectNoTrace asserts the value is in none of the enumerated places, and names the place if it is. */
function expectNoTrace(found: Record<string, string>, value: string, when: string): void {
  for (const [place, haystack] of Object.entries(found)) {
    expect(haystack.includes(value), `the minted key is still in ${place} ${when}`).toBe(false);
  }
}

/**
 * reportSite prints one row of the E28 exit gate's key-scan ledger (plan §T4 group (c)), in the shape
 * tests/uat/fleet-console/journey_test.go diffs against the bundle's carried numbers.
 *
 * IT PRINTS THE BYTE COUNT AND THE PROBE, NOT ONLY THE ZERO, and that is the whole reason it exists rather
 * than the gate trusting the ledger. A scan over an empty haystack reports zero hits and so does a scan whose
 * haystack was never read — this tree measured that twice, over compressed bytes in E14 T7 and over
 * JSON-escaped ones in E20 T4 — so each site names a harmless token the SAME scan found in the SAME bytes.
 * The probe is never the value: finding that would be the failure, not the control.
 */
function reportSite(site: string, haystack: string, value: string, probe: string): void {
  const hits = haystack.split(value).length - 1;
  const probeFound = probe !== "" && haystack.includes(probe);
  expect(probeFound, `the ${site} scan found no probe (${probe}) — a haystack nothing was located in is a haystack nobody has shown was read`).toBe(true);
  console.log(`KEY VALUE SCAN — site=${site} bytes=${haystack.length} hits=${hits} probe=${probe} probe_found=${probeFound}`);
}

test("a minted key survives nowhere: not the DOM, not either storage, not the URL, not a later response", async ({ page }) => {
  const bodies: NetResponse[] = [];
  const { value, id } = await mint(page);

  // THE SCAN CAN SEE THE PAGE. Before asserting an absence, prove the haystack is real: the value IS in the
  // DOM right now, which is the one moment it is allowed to be.
  const whileShown = await traces(page);
  expect(whileShown.dom.includes(value), "the value is not in the DOM while it is being shown — the sweep would be vacuous").toBe(true);
  // And it is nowhere persistent EVEN NOW. A value parked in storage "just until it is copied" is a value
  // that outlives the tab.
  expect(whileShown.session.includes(value), "the value was written to sessionStorage while it was on screen").toBe(false);
  expect(whileShown.local.includes(value), "the value was written to localStorage while it was on screen").toBe(false);
  expect(whileShown.url.includes(value), "the value was put in the URL").toBe(false);

  // 1. DISMISSED — the region is gone and so is the value.
  await page.getByTestId("key-reveal-dismiss").click();
  await expect(page.getByTestId("key-reveal")).toHaveCount(0);
  expectNoTrace(await traces(page), value, "after the region was dismissed");

  // 2. RELOADED — a fresh document, built from whatever the console can still reach.
  page.on("response", (resp) => bodies.push(resp));
  await page.reload();
  await expect(page.getByTestId("panel-api-keys")).toBeVisible({ timeout: 15_000 });
  const after = await traces(page);
  expectNoTrace(after, value, "after a page reload");
  // The exit gate's ledger rows (plan §T4 group (c)) for the three in-document sites. Each probe is a token
  // that is ALLOWED to be in that haystack: the key's own id for the rebuilt DOM, and the path for the URL.
  reportSite("dom", after.dom, value, id);
  reportSite("url", after.url, value, "/policy");

  // WEB STORAGE NEEDS A SEEDED PROBE RATHER THAN A FOUND ONE, AND THE REASON IS A MEASUREMENT: this console
  // writes NOTHING to either storage, so the dump is empty — and an empty haystack reports zero hits exactly
  // like a clean one. There is no naturally-occurring token to point at, so the control is the sweep finding
  // something the test PUT there: if `dump` can see this, it could have seen the value.
  const storageProbe = "palai-console-scan-control";
  await page.evaluate((probe) => {
    window.sessionStorage.setItem(probe, "a harmless value, written by the test to prove this scan reads storage");
    window.localStorage.setItem(probe, "the same, in the other storage");
  }, storageProbe);
  const seeded = await traces(page);
  expect(seeded.session.includes(value), "the minted key is in sessionStorage").toBe(false);
  expect(seeded.local.includes(value), "the minted key is in localStorage").toBe(false);
  reportSite("web-storage", `${seeded.session}\n${seeded.local}`, value, storageProbe);
  await page.evaluate((probe) => {
    window.sessionStorage.removeItem(probe);
    window.localStorage.removeItem(probe);
  }, storageProbe);
  // The key EXISTS — its id is on the screen. So the reload rendered the collection this value belongs to,
  // and "the value is absent" is a statement about the value rather than about an empty page.
  await expect(page.getByTestId("panel-api-keys")).toContainText(id);

  // 3. EVERY RESPONSE BODY the reload produced, which includes the console's own GET /v1/api-keys.
  //
  // THE BODIES ARE DECODED BEFORE THEY ARE SCANNED, which `resp.text()` does — Playwright applies the
  // content encoding — and it is stated rather than assumed because a raw-byte sweep over a gzipped body can
  // never fail: deflate bit-packs literals, so the plaintext is not IN the bytes to find. This tree measured
  // that in E14 T7 and paid for it again in E20 T4 over JSON escaping.
  let scanned = 0;
  let sawKeyID = 0;
  let bodyBytes = 0;
  for (const resp of bodies) {
    let body: string;
    try {
      body = await resp.text();
    } catch {
      continue;
    }
    scanned += 1;
    bodyBytes += body.length;
    if (body.includes(id)) sawKeyID += 1;
    expect(body.includes(value), `the minted value came back in a response body from ${resp.url()}`).toBe(false);
  }
  expect(scanned, "no response bodies were retained — this arm would be vacuous").toBeGreaterThan(0);
  expect(
    sawKeyID,
    "no response body carried the key's ID, so this scan has not been shown to read the api-keys body at all — " +
      "and its zero would be a statement about the haystack rather than about the secret",
  ).toBeGreaterThan(0);
  // The two remaining ledger rows. `response-body` is what the MINT itself answered plus everything since;
  // `later-response` is the one the other four sites pass over — a value can be gone from the DOM and still
  // be served back by a LIST call, which is the server behaviour `poolKeyView(item, false)` exists to
  // prevent and which the browser half has to mirror rather than assume.
  console.log(`KEY VALUE SCAN — site=response-body bytes=${bodyBytes} hits=0 probe=${id} probe_found=${sawKeyID > 0}`);
  const later = await page.request.get("/api/palai/v1/api-keys", { headers: await sessionHeaders(page) });
  const laterBody = await later.text();
  expect(laterBody.includes(id), "the LIST call did not carry the key's id, so scanning it says nothing about the value").toBe(true);
  expect(laterBody.includes(value), "the api-keys LIST call served the minted value back").toBe(false);
  console.log(`KEY VALUE SCAN — site=later-response bytes=${laterBody.length} hits=0 probe=${id} probe_found=true`);

  await revokeMinted(page, id);
});

test("the value is readable and copyable by hand in a context with no clipboard API", async ({ page }) => {
  // W3: Clipboard.writeText is secure-context only. A console reached over plain http:// — a posture this
  // tree supports, since compose's edge TLS is in the production.yml overlay and not the base profile — has
  // no navigator.clipboard at all, and the value must still be gettable there. The API is removed BEFORE any
  // script runs, so the component sees the same world that browser does.
  await page.addInitScript(() => {
    Object.defineProperty(navigator, "clipboard", { get: () => undefined, configurable: true });
  });

  const { value, id } = await mint(page);
  expect(value.length).toBeGreaterThan(16);

  // No button that cannot work, and a sentence in its place saying what to do instead.
  await expect(page.getByTestId("key-reveal-copy")).toHaveCount(0);
  await expect(page.getByTestId("key-reveal-manual")).toBeVisible();
  await expect(page.getByTestId("key-reveal-manual")).toContainText(/select/i);

  // AND THE VALUE IS SELECTABLE — the computed style, not the source. `user-select: none` anywhere up the
  // ancestor chain would turn the fallback path into a transcription task, which is the cost WCAG 2.2
  // §3.3.8's rationale names.
  const selectable = await page.getByTestId("key-reveal-value").evaluate((el) => {
    const style = getComputedStyle(el);
    return { userSelect: style.userSelect, webkit: style.webkitUserSelect, pointer: style.pointerEvents };
  });
  expect(selectable.userSelect, "the value carries user-select: none").not.toBe("none");
  expect(selectable.webkit, "the value carries -webkit-user-select: none").not.toBe("none");
  expect(selectable.pointer, "the value cannot be pointed at, so it cannot be selected with a mouse").not.toBe("none");

  // AND NOTHING CANCELS A COPY. A `copy` handler calling preventDefault is the other way to block this, and
  // it is invisible to a style check.
  const copyBlocked = await page.getByTestId("key-reveal-value").evaluate((el) => {
    const event = new ClipboardEvent("copy", { bubbles: true, cancelable: true });
    el.dispatchEvent(event);
    return event.defaultPrevented;
  });
  expect(copyBlocked, "a handler cancelled the copy event").toBe(false);

  // The value can genuinely be read out of the document by a selection, which is what "copy it by hand" is.
  const selected = await page.getByTestId("key-reveal-value").evaluate((el) => {
    const range = document.createRange();
    range.selectNodeContents(el);
    const selection = window.getSelection();
    selection?.removeAllRanges();
    selection?.addRange(range);
    return selection?.toString() ?? "";
  });
  expect(selected.trim() === value, "selecting the value's node did not yield the value").toBe(true);

  await revokeMinted(page, id);
});

test("a refused clipboard write is shown, not swallowed", async ({ page }) => {
  // NotAllowedError is what writeText throws when the clipboard is denied (MDN). Swallowing it leaves an
  // operator believing they hold a credential they do not — and on a one-time value that is discovered after
  // the screen is gone.
  await page.addInitScript(() => {
    Object.defineProperty(navigator, "clipboard", {
      get: () => ({
        writeText: () => Promise.reject(new DOMException("Write permission denied.", "NotAllowedError")),
      }),
      configurable: true,
    });
  });

  const { id } = await mint(page);
  await page.getByTestId("key-reveal-copy").click();
  const error = page.getByTestId("key-reveal-copy-error");
  await expect(error).toBeVisible();
  await expect(error).toContainText("NotAllowedError");
  // The value is STILL on screen. An error that also removed the one copy of a one-time credential would be
  // worse than the failure it reports.
  await expect(page.getByTestId("key-reveal-value")).toBeVisible();
  await expect(page.getByTestId("key-reveal-copied")).toHaveCount(0);

  await revokeMinted(page, id);
});

test("the arrival of a one-time value is announced, and the region is axe-clean", async ({ page }) => {
  const { id } = await mint(page);

  // A live region, and it says the value ARRIVED without saying the value. An announcement carrying the
  // credential would read it aloud in whatever room the operator is in.
  const announce = page.getByTestId("key-reveal-announce");
  await expect(announce).toHaveAttribute("role", "status");
  await expect(announce).toContainText(/not be shown again/i);
  const spoken = await announce.innerText();
  const value = (await page.getByTestId("key-reveal-value").innerText()).trim();
  expect(spoken.includes(value), "the status region announces the credential itself").toBe(false);

  // And the sentence the server's own behaviour implies, on screen.
  await expect(page.getByTestId("key-reveal-once-note")).toContainText(/never shown again/i);

  const results = await new AxeBuilder({ page }).withTags(WCAG_TAGS).analyze();
  expect(results.violations, JSON.stringify(results.violations, null, 2)).toEqual([]);
  expect(results.passes.length + results.violations.length + results.incomplete.length).toBeGreaterThan(0);

  await revokeMinted(page, id);
});
