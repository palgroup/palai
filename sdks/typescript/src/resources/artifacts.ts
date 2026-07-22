import type { Palai } from "../client.ts";
import { callArgs, enc, type CallOptions, type ListView } from "./shared.ts";

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

  // retrieve returns an artifact's metadata; a foreign/unknown id is a 404.
  async retrieve(artifactID: string, options: CallOptions = {}): Promise<Artifact> {
    const result = await this.#client.request<Artifact>("GET", `/v1/artifacts/${enc(artifactID)}`, callArgs(options));
    return result.body;
  }

  // download opens the authenticated byte stream for an artifact. HONEST CEILING: this is a direct
  // authenticated download — the object's bytes stream straight from the object store through the
  // control-plane; a pre-signed URL + expiry policy is E13-H. The SSE primitive in stream.ts does NOT
  // fit here (it frames an event stream, not raw bytes), so this reads the raw response body instead.
  async download(artifactID: string, options: CallOptions = {}): Promise<ArtifactDownload> {
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
