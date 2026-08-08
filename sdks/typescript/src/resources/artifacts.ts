import type { Palai } from "../client.ts";
import { callArgs, enc, type CallOptions, type DownloadOptions, type ListView } from "./shared.ts";

// Artifact is an artifact's metadata projection (spec §22.6). No canonical schema generates it, so it
// is open: the identity fields plus an index signature, so classification/integrity fields the server
// adds survive a round-trip.
export interface Artifact {
  id: string;
  object: string;
  [key: string]: unknown;
}

// ArtifactDownload is an authenticated byte download. `stream` is the object's body (never buffered
// through control-plane memory); `bytes()` drains it into one Uint8Array for convenience; the two are
// mutually exclusive (a fetch body is single-use). contentDigest is the RFC 9530 Content-Digest for
// byte-integrity verification against the workspace copy.
export interface ArtifactDownload {
  stream: ReadableStream<Uint8Array>;
  contentDigest: string | null;
  contentType: string;
  contentLength: number | null;
  bytes(): Promise<Uint8Array>;
}

// Artifacts is the artifact retrieval resource (spec §22.6, E13 T5): the never-opened READ half of the
// E09 write-path — metadata, an authenticated streaming download, and a run-scoped list. A wrong-tenant
// or unknown id is an indistinguishable 404 (NotFoundError), so the surface leaks no cross-tenant
// existence.
export class Artifacts {
  #client: Palai;
  constructor(client: Palai) {
    this.#client = client;
  }

  /**
   * create uploads bytes and returns the artifact whose id a run's `image_ref` content item names.
   *
   * ‼️ THIS IS HOW A SCREENSHOT REACHES A MODEL, and its absence was the gap: the Go SDK has had this
   * since E09 and the TypeScript one did not, so a browser app — the only kind that has a file picker —
   * could download artifacts and never send one. The demo's chat could show what the agent produced and
   * not what the operator was looking at.
   *
   * NO MEDIA TYPE IS SENT, deliberately. The server sniffs the bytes (http.DetectContentType) and
   * refuses anything outside its allow-list, so a caller-declared type would be a claim the server
   * ignores — a lie in a function signature. A caller who knows what it is holding still learns what the
   * server decided, from the `media_type` on the artifact that comes back.
   */
  async create(content: Uint8Array, options: CallOptions = {}): Promise<Artifact> {
    if (content.byteLength === 0) {
      // Refused before the round trip: the server answers 400 for an empty body, and paying a request to
      // be told what the caller already knows is a request nobody needed.
      throw new TypeError("artifacts.create: the content carried no bytes");
    }
    const result = await this.#client.request<Artifact>("POST", "/v1/artifacts", {
      ...callArgs(options),
      bytes: content,
    });
    return result.body;
  }

  // retrieve returns an artifact's metadata; a foreign/unknown id is a 404.
  async retrieve(artifactID: string, options: CallOptions = {}): Promise<Artifact> {
    const result = await this.#client.request<Artifact>("GET", `/v1/artifacts/${enc(artifactID)}`, callArgs(options));
    return result.body;
  }

  // download opens the authenticated byte stream for an artifact. HONEST CEILING: this is a direct
  // authenticated download — the object's bytes stream straight from the object store through the
  // control-plane; a pre-signed URL + expiry policy is E13-H. The SSE primitive in stream.ts does NOT
  // fit here (it frames an event stream, not raw bytes), so this reads the raw response body instead.
  async download(artifactID: string, options: DownloadOptions = {}): Promise<ArtifactDownload> {
    const response = await this.#client.openDownload(`/v1/artifacts/${enc(artifactID)}/content`, options);
    const contentLength = response.headers.get("Content-Length");
    return {
      stream: response.body ?? emptyStream(),
      contentDigest: response.headers.get("Content-Digest"),
      contentType: response.headers.get("Content-Type") ?? "application/octet-stream",
      contentLength: contentLength === null ? null : Number(contentLength),
      bytes: async () => new Uint8Array(await response.arrayBuffer()),
    };
  }

  // listForResponse lists the artifacts a response's run produced. A known run with no artifacts is an
  // empty list, not a miss; an unknown/foreign response id is a 404.
  async listForResponse(responseID: string, options: CallOptions = {}): Promise<ListView<Artifact>> {
    const result = await this.#client.request<ListView<Artifact>>(
      "GET",
      `/v1/responses/${enc(responseID)}/artifacts`,
      callArgs(options),
    );
    return result.body;
  }
}

function emptyStream(): ReadableStream<Uint8Array> {
  return new ReadableStream<Uint8Array>({
    start(controller) {
      controller.close();
    },
  });
}
