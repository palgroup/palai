import type { Palai } from "../client.ts";
import { callArgs, enc, type CallOptions, type ListView } from "./shared.ts";

// SecretRef is a secret-ref's metadata projection (spec §39.x, E13 T3). The VALUE is write-only — it
// never appears in any response — so this type carries only name/version/object (plus an index
// signature for updated_at and any metadata the server adds).
export interface SecretRef {
  name: string;
  version: number;
  object: string;
  [key: string]: unknown;
}

export interface SecretRefCreateParams {
  name: string;
  value: string;
}
export interface SecretRefRotateParams {
  value: string;
}

// SecretRefs is the restart-less secret-ref write-path (spec §39.x, E13 T3, SEC-002/MCI-002): a tenant
// POSTs a write-only value the resolver reads fresh, so a rotation takes effect with no restart; reads
// return metadata only. Requires a key with the `provision` capability.
export class SecretRefs {
  #client: Palai;
  constructor(client: Palai) {
    this.#client = client;
  }

  // create stores a new secret under a name (201). The response is metadata only — the value never
  // comes back.
  async create(params: SecretRefCreateParams, options: CallOptions = {}): Promise<SecretRef> {
    const r = await this.#client.request<SecretRef>("POST", "/v1/secret-refs", { body: params, ...callArgs(options) });
    return r.body;
  }

  // list returns the tenant's secret-ref metadata (names/versions), never values.
  async list(options: CallOptions = {}): Promise<ListView<SecretRef>> {
    const r = await this.#client.request<ListView<SecretRef>>("GET", "/v1/secret-refs", callArgs(options));
    return r.body;
  }

  // retrieve reads one secret-ref's metadata by name; an unknown name is a 404.
  async retrieve(name: string, options: CallOptions = {}): Promise<SecretRef> {
    const r = await this.#client.request<SecretRef>("GET", `/v1/secret-refs/${enc(name)}`, callArgs(options));
    return r.body;
  }

  // rotate inserts a new version for an existing name; rotating a never-created name is a 404. The new
  // value takes effect with no restart.
  async rotate(name: string, params: SecretRefRotateParams, options: CallOptions = {}): Promise<SecretRef> {
    const r = await this.#client.request<SecretRef>("POST", `/v1/secret-refs/${enc(name)}/rotate`, { body: params, ...callArgs(options) });
    return r.body;
  }
}
