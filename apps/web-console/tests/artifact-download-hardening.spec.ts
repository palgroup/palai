import { test, expect } from "@playwright/test";

import { NEXT_PORT } from "./constants";

const APP_ORIGIN = `http://127.0.0.1:${NEXT_PORT}`;

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
test.describe("artifact download hardening (untrusted bytes on a trusted origin)", () => {
  test("a hostile artifact is neutralized: octet-stream, nosniff, forced attachment, sanitized filename", async ({ request }) => {
    const res = await request.get(`${APP_ORIGIN}/api/palai/v1/artifacts/art_evil/content`);
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

  test("a benign artifact keeps its real filename and type while still being hardened", async ({ request }) => {
    const res = await request.get(`${APP_ORIGIN}/api/palai/v1/artifacts/art_1/content`);
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
