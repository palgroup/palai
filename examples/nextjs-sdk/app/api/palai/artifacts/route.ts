import { getPalaiClient } from "@/lib/palai";
import { problem, relayError } from "@/lib/relay";

// The SDK's server path uses node:crypto, so this runs on the Node runtime; force-dynamic keeps it
// out of the static build (the credential is never present at build time).
export const runtime = "nodejs";
export const dynamic = "force-dynamic";

// GET relays an authenticated artifact download to the browser: /api/palai/artifacts?id=art_123. The
// SDK opens the authenticated byte stream with the server-side key and the handler pipes the bytes
// straight through — the credential never reaches the browser, which receives only the object and its
// Content-Digest (RFC 9530) to verify byte-integrity. A browser disconnect aborts request.signal,
// which closes the upstream stream.
export async function GET(request: Request): Promise<Response> {
  const id = new URL(request.url).searchParams.get("id");
  if (id === null || id === "") {
    return problem(400, "invalid_request", "an 'id' query parameter is required");
  }

  const client = getPalaiClient();
  try {
    const download = await client.artifacts.download(id, { signal: request.signal });
    const headers = new Headers({ "Content-Type": download.contentType, "Cache-Control": "no-store" });
    if (download.contentDigest !== null) {
      headers.set("Content-Digest", download.contentDigest);
    }
    if (download.contentLength !== null) {
      headers.set("Content-Length", String(download.contentLength));
    }
    return new Response(download.stream, { headers });
  } catch (err) {
    return relayError(err);
  }
}
