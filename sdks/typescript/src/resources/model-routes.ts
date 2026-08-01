import type { ModelConnection, ModelRoute, ModelRouteRevision } from "../generated/types.ts";
import type { Palai } from "../client.ts";
import { callArgs, enc, type CallOptions, type ListView } from "./shared.ts";

// The projections are the generated contract types (ModelConnection/ModelRoute/ModelRouteRevision) — no
// more hand-written open interfaces (E16 T1 retires the E13 T10 `[key: string]: unknown` shape). The
// request params below shape a write body, so they stay local.

// A connection binds a provider family to a secret REF, never a value — a request that inlines a
// credential is rejected by the server's strict decode.
export interface ModelConnectionCreateParams {
  provider: string;
  secret_ref: string;
  /**
   * This connection's OWN endpoint (E29). REQUIRED by the `openai-compatible` family — that family has no
   * endpoint of its own, and an empty one would dial the OpenAI API with a key minted for something else —
   * and refused on families that do have one. Omit for `provider-one` / `provider-two`.
   */
  base_url?: string;
}

/**
 * One credential probe's finding (E29). `outcome` is the verdict; `not_probed` means NOTHING was checked
 * and must never be rendered as a pass. `checked` is the server's own sentence about what a probe does not
 * establish — the model id's validity above all — carried in the payload so a UI cannot overstate it.
 */
export interface ModelConnectionVerification {
  object: string;
  connection_id: string;
  provider: string;
  outcome: "credential_accepted" | "credential_rejected" | "unreachable" | "transient" | "not_probed";
  status?: number;
  endpoint?: string;
  detail?: string;
  checked?: string;
}
export interface ModelRouteCreateParams {
  name: string;
}
export interface ModelRouteRevisionCreateParams {
  model: string;
  connection_id: string;
}

// ModelRoutes is the model-routing admin surface (E13 T8 write + E16 T1 read-back, MCI-006): a project
// binds its own provider connection and publishes route revisions, so two projects on one stack run
// different models on different credentials — AND reads them back. The list methods return the admin
// ListView envelope ({object:"list", data:[…]}) — a full, small, tenant-scoped set, no cursor. Requires a
// key with the `provision` capability. A connection projection carries the secret REF name only, never a
// value.
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

  // verifyConnection performs a REAL credential probe against the connection's endpoint (E29): the control
  // plane redeems this connection's own credential and asks the endpoint whether it accepts it. It resolves
  // (200) for every outcome it reached a verdict on, INCLUDING a rejected credential — the request
  // succeeded and the verdict is in the body, because a 4xx would blame the caller's request for the
  // operator's key. Only an unknown connection rejects, with a 404.
  async verifyConnection(connectionID: string, options: CallOptions = {}): Promise<ModelConnectionVerification> {
    const r = await this.#client.request<ModelConnectionVerification>("POST", `/v1/model-connections/${enc(connectionID)}/verify`, callArgs(options));
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

  // --- E16 T1 read-back (the E13 T10 write-only gap) ------------------------------------------------

  // listConnections returns the project's connections (secret REF names only, never values).
  async listConnections(options: CallOptions = {}): Promise<ListView<ModelConnection>> {
    const r = await this.#client.request<ListView<ModelConnection>>("GET", "/v1/model-connections", callArgs(options));
    return r.body;
  }

  // getConnection reads one connection by id; an absent/foreign id is a 404.
  async getConnection(connectionID: string, options: CallOptions = {}): Promise<ModelConnection> {
    const r = await this.#client.request<ModelConnection>("GET", `/v1/model-connections/${enc(connectionID)}`, callArgs(options));
    return r.body;
  }

  // listRoutes returns the project's route aliases.
  async listRoutes(options: CallOptions = {}): Promise<ListView<ModelRoute>> {
    const r = await this.#client.request<ListView<ModelRoute>>("GET", "/v1/model-routes", callArgs(options));
    return r.body;
  }

  // getRoute reads one route alias by id; an absent/foreign id is a 404.
  async getRoute(routeID: string, options: CallOptions = {}): Promise<ModelRoute> {
    const r = await this.#client.request<ModelRoute>("GET", `/v1/model-routes/${enc(routeID)}`, callArgs(options));
    return r.body;
  }

  // listRevisions returns a route's revisions (each with its derived `published` flag).
  async listRevisions(routeID: string, options: CallOptions = {}): Promise<ListView<ModelRouteRevision>> {
    const r = await this.#client.request<ListView<ModelRouteRevision>>("GET", `/v1/model-routes/${enc(routeID)}/revisions`, callArgs(options));
    return r.body;
  }

  // getRevision reads one revision of a route; an absent/foreign route or revision is a 404.
  async getRevision(routeID: string, revisionID: string, options: CallOptions = {}): Promise<ModelRouteRevision> {
    const r = await this.#client.request<ModelRouteRevision>("GET", `/v1/model-routes/${enc(routeID)}/revisions/${enc(revisionID)}`, callArgs(options));
    return r.body;
  }
}
