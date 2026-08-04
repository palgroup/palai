import type { Palai } from "../client.ts";
import { callArgs, enc, type CallOptions, type ListView } from "./shared.ts";

// The tenancy provisioning projections (spec §39.2, E13 T2). No canonical schema generates them, so
// they are open: identity fields plus an index signature.
export interface Project {
  id: string;
  object: string;
  organization_id?: string;
  display_name?: string;
  config_policy?: unknown;
  [key: string]: unknown;
}
// ProjectCreated is the other place a fresh tenant's admin key plaintext is disclosed: since A.2 T6,
// creating a project OPENS a tenant (project + service principal + admin key), the job organization
// creation used to hold alone. Every later read renders metadata only.
export interface ProjectCreated extends Project {
  admin_api_key: ApiKeyCreated;
}
export interface ApiKey {
  id: string;
  object: string;
  organization_id?: string;
  project_id?: string;
  scopes?: string[];
  [key: string]: unknown;
}
// ApiKeyCreated carries the plaintext key, returned exactly once at creation and never on a read.
export interface ApiKeyCreated extends ApiKey {
  key: string;
}

export interface ProjectCreateParams {
  display_name: string;
}
export interface ProjectPolicyParams {
  config_policy: unknown;
}
export interface ApiKeyCreateParams {
  project_id: string;
  scopes?: string[];
  expires_at?: string;
}

// Projects administers the caller's projects, including the §14 config_policy write-path — the first API
// that makes the resolver's project layer reachable.
//
// AN `Organizations` RESOURCE SAT ABOVE THIS ONE UNTIL A.2 TASK 6, with create/list/retrieve against
// /v1/organizations. That task unmounted all three routes, so every one of its methods returned 404 — a
// client method that cannot succeed is worse than an absent one, because it advertises a capability the
// server does not have. Projects.create already does what Organizations.create did: it OPENS a tenant and
// discloses its admin key once. The Go SDK never grew the resource, so removing it here also ends a
// three-SDK asymmetry rather than creating one.
export class Projects {
  #client: Palai;
  constructor(client: Palai) {
    this.#client = client;
  }
  // create OPENS a tenant (201) and the response discloses its admin key plaintext once. It requires a key
  // with the `system` capability, not merely `provision` — opening a tenant is not an operation on the
  // caller's own tenant.
  async create(params: ProjectCreateParams, options: CallOptions = {}): Promise<ProjectCreated> {
    const r = await this.#client.request<ProjectCreated>("POST", "/v1/projects", { body: params, ...callArgs(options) });
    return r.body;
  }
  async list(options: CallOptions = {}): Promise<ListView<Project>> {
    const r = await this.#client.request<ListView<Project>>("GET", "/v1/projects", callArgs(options));
    return r.body;
  }
  async retrieve(projectID: string, options: CallOptions = {}): Promise<Project> {
    const r = await this.#client.request<Project>("GET", `/v1/projects/${enc(projectID)}`, callArgs(options));
    return r.body;
  }
  // updatePolicy writes the project-layer config_policy (strict schema, unknown-field reject).
  async updatePolicy(projectID: string, params: ProjectPolicyParams, options: CallOptions = {}): Promise<Project> {
    const r = await this.#client.request<Project>("PATCH", `/v1/projects/${enc(projectID)}`, { body: params, ...callArgs(options) });
    return r.body;
  }
}

// ApiKeys mints and revokes project-scoped keys. A key's plaintext is disclosed only on creation.
export class ApiKeys {
  #client: Palai;
  constructor(client: Palai) {
    this.#client = client;
  }
  async create(params: ApiKeyCreateParams, options: CallOptions = {}): Promise<ApiKeyCreated> {
    const r = await this.#client.request<ApiKeyCreated>("POST", "/v1/api-keys", { body: params, ...callArgs(options) });
    return r.body;
  }
  async list(options: CallOptions = {}): Promise<ListView<ApiKey>> {
    const r = await this.#client.request<ListView<ApiKey>>("GET", "/v1/api-keys", callArgs(options));
    return r.body;
  }
  async retrieve(keyID: string, options: CallOptions = {}): Promise<ApiKey> {
    const r = await this.#client.request<ApiKey>("GET", `/v1/api-keys/${enc(keyID)}`, callArgs(options));
    return r.body;
  }
  // revoke is naturally idempotent (revoked_at is monotonic).
  async revoke(keyID: string, options: CallOptions = {}): Promise<ApiKey> {
    const r = await this.#client.request<ApiKey>("POST", `/v1/api-keys/${enc(keyID)}/revoke`, callArgs(options));
    return r.body;
  }
}
