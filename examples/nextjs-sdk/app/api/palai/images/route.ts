import { getPalaiClient } from "@/lib/palai";

// POST /api/palai/images — relay a screenshot the operator dropped into the chat.
//
// ‼️ THE BYTES CROSS THIS PROCESS AND THE KEY NEVER LEAVES IT, which is the whole server-relay stance
// this example exists to demonstrate. The browser posts the file here; this handler hands it to the SDK
// with the server-side credential and returns ONLY the artifact id. A browser that could upload directly
// would be a browser holding a Palai key.
//
// WHAT THE ID IS FOR: a run's input names it in an `image_ref` content item, and the admission attaches
// the artifact to the run it creates. That attachment is also what brings the object inside retention's
// reach — an artifact whose run_id stays NULL is a row no purge can see — so an id that is uploaded and
// never named is a leak of exactly one object.
//
// NO MEDIA TYPE IS DECLARED. The control plane sniffs the bytes and refuses anything outside its
// allow-list, so a type from the browser would be a claim the server ignores. What comes back is what
// the server decided.
export async function POST(request: Request): Promise<Response> {
  const raw = await request.arrayBuffer();
  const bytes = new Uint8Array(raw);
  if (bytes.byteLength === 0) {
    return problem(400, "invalid_request", "the upload carried no bytes");
  }

  try {
    const artifact = await getPalaiClient().artifacts.create(bytes);
    // Only the id and what the server decided about the bytes. The Artifact type carries an index
    // signature for forward compatibility, so these two are read by name rather than spread — a spread
    // would forward whatever a future control plane adds, to a browser, unreviewed.
    return Response.json({
      artifactId: artifact.id,
      mediaType: typeof artifact["media_type"] === "string" ? artifact["media_type"] : null,
      sizeBytes: typeof artifact["size_bytes"] === "number" ? artifact["size_bytes"] : null,
    });
  } catch (err) {
    // The control plane's own refusal is the useful one — an unsupported media type, an object over the
    // cap — so its message is forwarded rather than replaced with a generic failure the operator cannot
    // act on. It is a Palai error and never a provider one, so nothing sensitive rides it.
    const detail = err instanceof Error ? err.message : "the upload could not be relayed";
    return problem(502, "upstream_error", detail);
  }
}

function problem(status: number, code: string, detail: string): Response {
  return new Response(
    JSON.stringify({ type: `https://docs.palai.dev/problems/${code}`, title: code, status, detail }),
    { status, headers: { "Content-Type": "application/problem+json" } },
  );
}
