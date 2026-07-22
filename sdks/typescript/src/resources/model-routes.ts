import type { Palai } from "../client.ts";
import { callArgs, enc, type CallOptions } from "./shared.ts";

// The DB-backed model-routing write projections (spec §27.2/§27.6, E13 T8). No canonical schema
// generates them, so they are open: identity fields plus an index signature.
export interface ModelConnection {
  id: string;
  object: string;
  [key: string]: unknown;
}
export interface ModelRoute {
  id: string;
  object: string;
  [key: string]: unknown;
}
export interface ModelRouteRevision {
  id: string;
  object: string;
  [key: string]: unknown;
}

// A connection binds a provider family to a secret REF, never a value — a request that inlines a
// credential is rejected by the server's strict decode.
export interface ModelConnectionCreateParams {
  provider: string;
  secret_ref: string;
}
export interface ModelRouteCreateParams {
  name: string;
}
export interface ModelRouteRevisionCreateParams {
  model: string;
  connection_id: string;
}

// ModelRoutes is the model-routing admin write surface (E13 T8, MCI-006): a project binds its own
// provider connection and publishes route revisions, so two projects on one stack run different models
// on different credentials. Requires a key with the `provision` capability. Read-back of routes is not
// on main, so this client is write-only by design (honest naming: no list/get method is offered).
export class ModelRoutes {
  #client: Palai;
  constructor(client: Palai) {
    this.#client = client;
  }

  // createConnection binds a provider family to a secret-ref handle (201).
  async createConnection(params: ModelConnectionCreateParams, options: CallOptions = {}): Promise<ModelConnection> {
    const r = await this.#client.request<ModelConnection>("POST", "/v1/model-connections", { body: params, ...callArgs(options) });
    return r.body;
  }

  // createRoute opens a named route alias for the project (201).
  async createRoute(params: ModelRouteCreateParams, options: CallOptions = {}): Promise<ModelRoute> {
    const r = await this.#client.request<ModelRoute>("POST", "/v1/model-routes", { body: params, ...callArgs(options) });
    return r.body;
  }

  // createRevision adds a DRAFT revision to a route (201) — a draft never steers a run; publishing does.
  async createRevision(routeID: string, params: ModelRouteRevisionCreateParams, options: CallOptions = {}): Promise<ModelRouteRevision> {
    const r = await this.#client.request<ModelRouteRevision>("POST", `/v1/model-routes/${enc(routeID)}/revisions`, { body: params, ...callArgs(options) });
    return r.body;
  }

  // publishRevision makes a draft revision the project's routed target. Re-publishing is idempotent.
  async publishRevision(routeID: string, revisionID: string, options: CallOptions = {}): Promise<ModelRouteRevision> {
    const r = await this.#client.request<ModelRouteRevision>("POST", `/v1/model-routes/${enc(routeID)}/revisions/${enc(revisionID)}/publish`, callArgs(options));
    return r.body;
  }
}
