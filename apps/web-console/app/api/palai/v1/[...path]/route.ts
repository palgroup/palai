import { getPalaiClient } from "@/lib/palai";
import { isPublicApiPath, problem, relayError } from "@/lib/relay";
import { requireSession } from "@/lib/session";

// The SDK's server path uses node:crypto, so this runs on the Node runtime; force-dynamic keeps it out
// of the static build (the credential is never present at build time).
export const runtime = "nodejs";
export const dynamic = "force-dynamic";

// This catch-all is the ONE data relay for the whole admin + live surface. Mounted at
// /api/palai/v1/[...path], it reconstructs the upstream path from the browser URL — so the ONLY thing
// the browser can address through it is a /v1/* public-API route (§47.6). It forwards through the
// @palai/sdk client, which carries the Bearer server-side, retries idempotently, and maps failures to
// stable RFC 9457 problems. The API key never rides the response; a metadata read returns metadata only
// (secret-ref VALUES are write-only server-side and never appear here).
//
// Two shapes: a JSON endpoint goes through client.request (parsed body re-serialized to the browser);
// an artifact byte download (/v1/artifacts/{id}/content) streams through client.openDownload, so the
// object never buffers through relay memory. Everything else is JSON.
//
// THE IDENTITY GATE IS HERE, IN EACH METHOD, AND NOWHERE ELSE (E25 T1, §2). Not in a proxy.ts: the vendor's
// own post-CVE guidance says middleware "should not be your only line of defense" and a mis-scoped matcher
// "can silently remove Proxy coverage" (GHSA-f82v-jwr5-mffw, CVSS 9.1). Not in app/layout.tsx: Partial
// Rendering does not re-run a layout on every navigation, so a check there is not a check. This function
// family is the single place every data path passes through, which is what makes one line per method the
// whole defense rather than one of several. tests/relay-gate.spec.ts counts the methods and the gates and
// requires the two numbers to be equal, so a sixth export cannot join without one.

async function upstreamPath(ctx: { params: Promise<{ path: string[] }> }, request: Request): Promise<string | null> {
  const { path } = await ctx.params;
  const search = new URL(request.url).search;
  const candidate = `/v1/${path.map(encodeURIComponent).join("/")}${search}`;
  return isPublicApiPath(candidate) ? candidate : null;
}

// isArtifactDownload matches the one binary endpoint: GET /v1/artifacts/{id}/content.
function isArtifactDownload(path: string): boolean {
  return /^\/v1\/artifacts\/[^/]+\/content(\?|$)/.test(path);
}

// artifactID pulls the id out of the download path — the fallback filename when an upstream name is
// missing or sanitizes away to nothing.
function artifactID(path: string): string {
  return path.match(/^\/v1\/artifacts\/([^/]+)\/content/)?.[1] ?? "artifact";
}

// activeTypePattern matches the content types a browser RENDERS as a document or script. Artifact bytes are
// UNTRUSTED (a run's own output, an ingested source, an A2A-pushed file, a worker result) and they are served
// from the console's OWN origin — the origin that holds the operator session and can drive this relay, which
// carries the server-side Bearer. So an artifact allowed to render here is stored XSS with full public-API
// reach: exactly the privileged backchannel §47.6 exists to deny. These types are coerced, never served.
const activeTypePattern = /^(?:text\/html|application\/xhtml\+xml|image\/svg\+xml|application\/xml|text\/xml)\b/i;

// safeFilename reduces an upstream filename to a bare token: last path segment only (no traversal), and only
// characters that cannot break out of the quoted header value or inject a new header. An upstream name is a
// convenience for the operator, never something the relay trusts verbatim.
function safeFilename(disposition: string | null, fallback: string): string {
  const raw = disposition?.match(/filename\*?=(?:UTF-8''|")?([^";]+)/i)?.[1] ?? "";
  const base = raw.split(/[\\/]/).pop() ?? "";
  const cleaned = base
    .replace(/[^A-Za-z0-9._-]/g, "")
    .replace(/^\.+/, "")
    .slice(0, 128);
  return cleaned === "" ? fallback : cleaned;
}

// downloadHeaders hardens the one response that carries untrusted bytes on a trusted origin, REGARDLESS of
// what the upstream said: the type is coerced away from active content, sniffing is off (so the coercion
// cannot be second-guessed), the disposition always DOWNLOADS instead of rendering, the filename is
// sanitized, and the response denies every content source as defense in depth. Only the integrity/length
// headers an operator actually verifies against are passed through.
function downloadHeaders(upstream: Headers, path: string): Headers {
  const upstreamType = upstream.get("Content-Type") ?? "application/octet-stream";
  const headers = new Headers({
    "Cache-Control": "no-store",
    "X-Content-Type-Options": "nosniff",
    "Content-Security-Policy": "default-src 'none'; sandbox",
    "Content-Type": activeTypePattern.test(upstreamType) ? "application/octet-stream" : upstreamType,
    "Content-Disposition": `attachment; filename="${safeFilename(upstream.get("Content-Disposition"), artifactID(path))}"`,
  });
  for (const h of ["Content-Length", "Content-Digest"]) {
    const v = upstream.get(h);
    if (v !== null) headers.set(h, v);
  }
  return headers;
}

async function relayJSON(method: string, path: string, body: unknown): Promise<Response> {
  try {
    const result = await getPalaiClient().request<unknown>(method, path, body === undefined ? {} : { body });
    return new Response(JSON.stringify(result.body), {
      status: result.status,
      headers: { "Content-Type": "application/json; charset=utf-8", "Cache-Control": "no-store" },
    });
  } catch (err) {
    return relayError(err);
  }
}

export async function GET(request: Request, ctx: { params: Promise<{ path: string[] }> }): Promise<Response> {
  const refused = requireSession(request);
  if (refused !== null) return refused;

  const path = await upstreamPath(ctx, request);
  if (path === null) return problem(400, "invalid_request", "only /v1/* public-API paths are relayed");

  if (isArtifactDownload(path)) {
    try {
      const upstream = await getPalaiClient().openDownload(path);
      // Stream the object's bytes straight back (never buffered here) under HARDENED headers: the bytes are
      // untrusted and this is the console's own origin, so the relay decides how they are labelled — see
      // downloadHeaders. No key, no upstream URL — just the object body.
      return new Response(upstream.body, { status: upstream.status, headers: downloadHeaders(upstream.headers, path) });
    } catch (err) {
      return relayError(err);
    }
  }
  return relayJSON("GET", path, undefined);
}

export async function POST(request: Request, ctx: { params: Promise<{ path: string[] }> }): Promise<Response> {
  const refused = requireSession(request);
  if (refused !== null) return refused;

  const path = await upstreamPath(ctx, request);
  if (path === null) return problem(400, "invalid_request", "only /v1/* public-API paths are relayed");
  return relayJSON("POST", path, await readBody(request));
}

export async function PATCH(request: Request, ctx: { params: Promise<{ path: string[] }> }): Promise<Response> {
  const refused = requireSession(request);
  if (refused !== null) return refused;

  const path = await upstreamPath(ctx, request);
  if (path === null) return problem(400, "invalid_request", "only /v1/* public-API paths are relayed");
  return relayJSON("PATCH", path, await readBody(request));
}

export async function DELETE(request: Request, ctx: { params: Promise<{ path: string[] }> }): Promise<Response> {
  const refused = requireSession(request);
  if (refused !== null) return refused;

  const path = await upstreamPath(ctx, request);
  if (path === null) return problem(400, "invalid_request", "only /v1/* public-API paths are relayed");
  return relayJSON("DELETE", path, undefined);
}

// readBody tolerates an empty body (a command/publish POST may carry none) — an empty string parses to
// undefined so the relay sends no JSON body rather than a malformed one.
async function readBody(request: Request): Promise<unknown> {
  const text = await request.text();
  if (text.trim() === "") return undefined;
  try {
    return JSON.parse(text);
  } catch {
    return undefined;
  }
}
