import type { Palai } from "../client.ts";
import { callArgs, enc, type CallOptions, type ListView } from "./shared.ts";

// The tenancy provisioning projections (spec §39.2, E13 T2). No canonical schema generates them, so
// they are open: identity fields plus an index signature.
export interface Organization {
  id: string;
  object: string;
  display_name?: string;
  [key: string]: unknown;
}
// OrganizationCreated is the ONE place a fresh tenant's admin key plaintext is disclosed (creation opens
// a new tenant with its default project + admin key). Every later read renders metadata only.
export interface OrganizationCreated extends Organization {
  default_project_id: string;
  admin_api_key: ApiKeyCreated;
}
export interface Project {
  id: string;
  object: string;
  organization_id?: string;
  display_name?: string;
  config_policy?: unknown;
  [key: string]: unknown;
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

export interface OrganizationCreateParams {
  display_name: string;
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

// Organizations administers tenants (spec §39.2). Creation is the one cross-tenant op — it provisions a
// SECOND tenant with no restart. Requires a key with the `provision` capability.
export class Organizations {
  #client: Palai;
  constructor(client: Palai) {
    this.#client = client;
  }
  // create opens a new tenant (201); the response discloses the tenant's admin key plaintext once.
  async create(params: OrganizationCreateParams, options: CallOptions = {}): Promise<OrganizationCreated> {
    const r = await this.#client.request<OrganizationCreated>("POST", "/v1/organizations", { body: params, ...callArgs(options) });
    return r.body;
  }
  async list(options: CallOptions = {}): Promise<ListView<Organization>> {
    const r = await this.#client.request<ListView<Organization>>("GET", "/v1/organizations", callArgs(options));
    return r.body;
  }
  async retrieve(organizationID: string, options: CallOptions = {}): Promise<Organization> {
    const r = await this.#client.request<Organization>("GET", `/v1/organizations/${enc(organizationID)}`, callArgs(options));
    return r.body;
  }
}

// Projects administers projects within the caller's organization, including the §14 config_policy
// write-path — the first API that makes the resolver's project layer reachable.
export class Projects {
  #client: Palai;
  constructor(client: Palai) {
    this.#client = client;
  }
  async create(params: ProjectCreateParams, options: CallOptions = {}): Promise<Project> {
    const r = await this.#client.request<Project>("POST", "/v1/projects", { body: params, ...callArgs(options) });
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
