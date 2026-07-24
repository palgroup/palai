import { getPalaiClient } from "@/lib/palai";
import { isPublicApiPath, problem, relayError } from "@/lib/relay";

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
  const path = await upstreamPath(ctx, request);
  if (path === null) return problem(400, "invalid_request", "only /v1/* public-API paths are relayed");

  if (isArtifactDownload(path)) {
    try {
      const upstream = await getPalaiClient().openDownload(path);
      // Stream the object's bytes straight back; preserve the integrity + type headers the browser needs
      // to verify + name the download. No key, no upstream URL — just the object body.
      const headers = new Headers({ "Cache-Control": "no-store" });
      for (const h of ["Content-Type", "Content-Length", "Content-Digest", "Content-Disposition"]) {
        const v = upstream.headers.get(h);
        if (v !== null) headers.set(h, v);
      }
      return new Response(upstream.body, { status: upstream.status, headers });
    } catch (err) {
      return relayError(err);
    }
  }
  return relayJSON("GET", path, undefined);
}

export async function POST(request: Request, ctx: { params: Promise<{ path: string[] }> }): Promise<Response> {
  const path = await upstreamPath(ctx, request);
  if (path === null) return problem(400, "invalid_request", "only /v1/* public-API paths are relayed");
  return relayJSON("POST", path, await readBody(request));
}

export async function PATCH(request: Request, ctx: { params: Promise<{ path: string[] }> }): Promise<Response> {
  const path = await upstreamPath(ctx, request);
  if (path === null) return problem(400, "invalid_request", "only /v1/* public-API paths are relayed");
  return relayJSON("PATCH", path, await readBody(request));
}

export async function DELETE(request: Request, ctx: { params: Promise<{ path: string[] }> }): Promise<Response> {
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
