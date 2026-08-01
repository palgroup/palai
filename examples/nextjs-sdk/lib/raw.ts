// server-only for exactly the reason lib/palai.ts is: this module reads the API key. It is a
// SECOND module that touches the credential, so it carries the same build-time guard — a raw
// fetch is still a credentialed call, and "raw" must not come to mean "unguarded".
import "server-only";

// APIVersion is the dated contract the raw half sends. It is a literal rather than an import from
// @palai/sdk on purpose: the raw path exists to show what a caller writes WITHOUT the SDK, and a
// raw path that imported the SDK to learn its own header would be demonstrating nothing.
//
// It must stay in step with sdks/typescript/src/client.ts APIVersion by hand. That duplication is
// the honest cost of not depending on the SDK, and naming it here is better than pretending the
// cost does not exist — a caller who hand-rolls HTTP inherits exactly this maintenance.
export const RawAPIVersion = "2026-07-16";

// rawBaseURL is the control-plane origin the raw half posts to — the SAME PALAI_BASE_URL the SDK
// client uses, so the two halves are talking to one deployment and any difference between them is
// the transport rather than the target.
export function rawBaseURL(): string {
  return requiredEnv("PALAI_BASE_URL").replace(/\/+$/, "");
}

// rawHeaders is every header the SDK would have set for you. Writing them out is the point: the
// bearer credential, the dated contract, and the content type. The Idempotency-Key is NOT here
// because it must be minted per create rather than per request — the caller adds it, and the
// compare route omits it deliberately when asked so the 400 is demonstrable.
export function rawHeaders(): Record<string, string> {
  return {
    Authorization: `Bearer ${requiredEnv("PALAI_API_KEY")}`,
    "API-Version": RawAPIVersion,
    "Content-Type": "application/json",
  };
}

function requiredEnv(name: string): string {
  const value = process.env[name]?.trim();
  if (!value) {
    throw new Error(
      `${name} is required and is read server-side only. Set it before starting the app; ` +
        "it must never be exposed to the browser (no NEXT_PUBLIC_ prefix).",
    );
  }
  return value;
}
