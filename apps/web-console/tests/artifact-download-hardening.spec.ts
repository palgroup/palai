import { test, expect } from "@playwright/test";

import { NEXT_PORT } from "./constants";
import { announceProfile, sessionHeaders, signIn, skipOnReal } from "./profile";

const APP_ORIGIN = `http://127.0.0.1:${NEXT_PORT}`;

test.beforeAll(() => announceProfile("artifact-download-hardening.spec.ts"));

// Artifact BYTES are untrusted — a run's own output, an ingested knowledge source, an A2A-pushed file, a
// capability-worker result — and so are the headers the object store replays alongside them. The console
// relay serves those bytes from the console's OWN origin, which is also the origin that holds the operator's
// session and can drive the relay (the API key lives server-side, so anything running on this origin can
// call the whole public API as the signed-in operator). Passing an upstream `Content-Type: text/html` or an
// `inline` disposition straight through would therefore turn any uploadable artifact into stored XSS with
// full API reach — the console's entire "no privileged backchannel" posture defeated by a rendered file.
//
// So the relay must harden the download branch regardless of what the upstream says: never serve active
// content on this origin, never sniff, always download rather than render, and never trust an upstream
// filename. This suite drives the hostile fixture (/v1/artifacts/art_evil/content: text/html + inline +
// a traversal/CRLF filename) through the real relay and pins each guarantee.
//
// E19 T7 — FAKE-PROFILE ONLY, and for two reasons the sweep measured rather than assumed. First, the
// hostile artifact has to be SYNTHESISED: no real object store will serve active content with an `inline`
// disposition and a traversal filename on demand, and manufacturing an attacker is exactly what a fixture
// is for. Second, on a compose stack the artifact routes are not even registered — compose runs a seaweedfs
// object-store, publishes it, healthchecks it and depends_on it, then never passes PALAI_S3_ENDPOINT to the
// control plane, so `artifacts != nil` is false and the whole retrieval surface is unmounted (DIV-RTE-003).
// The hardening pinned below is real relay code on both profiles; only its adversarial INPUT is fixture-only.
test.describe("artifact download hardening (untrusted bytes on a trusted origin)", () => {
  // The artifact download rides the gated relay now (E25 T1), so these two tests sign in first and carry
  // the session EXPLICITLY (sessionHeaders): an APIRequestContext withholds a `Secure` cookie over
  // loopback HTTP even when the browser in the same context sends it, so without the header both the
  // hostile and the benign case would come back a uniform 401 and prove nothing about hardening.
  test.beforeEach(async ({ page }) => {
    skipOnReal("DIV-UI-003");
    await signIn(page);
  });

  test("a hostile artifact is neutralized: octet-stream, nosniff, forced attachment, sanitized filename", async ({ page }) => {
    const res = await page.request.get(`${APP_ORIGIN}/api/palai/v1/artifacts/art_evil/content`, { headers: await sessionHeaders(page) });
    expect(res.status()).toBe(200);
    const headers = res.headers();

    // 1. The active content type is coerced — the browser is never told to render this as a document.
    expect(headers["content-type"], "an active upstream type must be coerced to a download type").toBe(
      "application/octet-stream",
    );

    // 2. nosniff — so a coerced type cannot be second-guessed back into text/html by content sniffing.
    expect(headers["x-content-type-options"]).toBe("nosniff");

    // 3. The disposition is forced to attachment: the upstream said `inline`, which would have rendered.
    const disposition = headers["content-disposition"] ?? "";
    expect(disposition.startsWith("attachment;"), `disposition must force download, got: ${disposition}`).toBe(true);

    // 4. The upstream filename is never echoed raw: no traversal segments, no CR/LF header injection.
    expect(disposition).not.toContain("..");
    expect(disposition).not.toContain("/");
    expect(disposition).not.toMatch(/[\r\n]/);

    // 5. Defense in depth: even if a client ignored all of the above, the response denies every source.
    expect(headers["content-security-policy"] ?? "").toContain("default-src 'none'");

    // The bytes themselves are still delivered intact — hardening is about how they are LABELLED, not
    // about censoring the artifact an operator legitimately asked for.
    expect(await res.text()).toContain("<script>");
  });

  test("a benign artifact keeps its real filename and type while still being hardened", async ({ page }) => {
    const res = await page.request.get(`${APP_ORIGIN}/api/palai/v1/artifacts/art_1/content`, { headers: await sessionHeaders(page) });
    expect(res.status()).toBe(200);
    const headers = res.headers();

    // A safe type survives (text/plain is not active content), and the operator still gets the real name —
    // sanitizing must not degrade the honest case into an opaque blob.
    expect(headers["content-type"]).toContain("text/plain");
    expect(headers["x-content-type-options"]).toBe("nosniff");
    expect(headers["content-disposition"]).toBe('attachment; filename="release-notes.txt"');
    // The integrity header the operator verifies against is still passed through.
    expect(headers["content-digest"]).toContain("sha-256=");
  });
});
