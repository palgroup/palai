import type { MCPConnection, RepositoryBinding, Tool, ToolSet } from "../generated/types.ts";
import type { Palai } from "../client.ts";
import { callArgs, enc, listPath, type CallOptions, type ListParams, type Page } from "./shared.ts";

// The T4 read/LIST surfaces that carry no writes (spec §T4). Each is the same thin list+get pair over
// the shared opaque cursor, so they are co-located rather than split into four near-empty files — same
// pattern as responses.ts, just grouped. Every row is born under RLS, tenant-confined by the verified
// key; a foreign cursor is a 400 the server rejects (never a silently-empty page).

// Trigger is the automation-trigger read projection (spec §20.2.2). No canonical schema generates it,
// so it is open: identity fields plus an index signature.
export interface Trigger {
  id: string;
  object: string;
  [key: string]: unknown;
}

// RepositoryBindings: the external repositories a project's coding sessions attach (spec §30.1).
export class RepositoryBindings {
  #client: Palai;
  constructor(client: Palai) {
    this.#client = client;
  }
  async list(params: ListParams = {}, options: CallOptions = {}): Promise<Page<RepositoryBinding>> {
    const r = await this.#client.request<Page<RepositoryBinding>>("GET", listPath("/v1/repository-bindings", params), callArgs(options));
    return r.body;
  }
  async retrieve(bindingID: string, options: CallOptions = {}): Promise<RepositoryBinding> {
    const r = await this.#client.request<RepositoryBinding>("GET", `/v1/repository-bindings/${enc(bindingID)}`, callArgs(options));
    return r.body;
  }
}

// Tools: the extensibility tool lineages + the named tool-sets (spec §20.2, §28.2-28.4).
export class Tools {
  #client: Palai;
  constructor(client: Palai) {
    this.#client = client;
  }
  async list(params: ListParams = {}, options: CallOptions = {}): Promise<Page<Tool>> {
    const r = await this.#client.request<Page<Tool>>("GET", listPath("/v1/tools", params), callArgs(options));
    return r.body;
  }
  async retrieve(toolID: string, options: CallOptions = {}): Promise<Tool> {
    const r = await this.#client.request<Tool>("GET", `/v1/tools/${enc(toolID)}`, callArgs(options));
    return r.body;
  }
  // listSets pages the named tool-sets (GET /v1/tool-sets).
  async listSets(params: ListParams = {}, options: CallOptions = {}): Promise<Page<ToolSet>> {
    const r = await this.#client.request<Page<ToolSet>>("GET", listPath("/v1/tool-sets", params), callArgs(options));
    return r.body;
  }
}

// MCPConnections: the admin-registered upstream MCP servers (spec §28.13-28.14). Read-only here — the
// register/discover writes are the admin-only management surface, not this parity task.
export class MCPConnections {
  #client: Palai;
  constructor(client: Palai) {
    this.#client = client;
  }
  async list(params: ListParams = {}, options: CallOptions = {}): Promise<Page<MCPConnection>> {
    const r = await this.#client.request<Page<MCPConnection>>("GET", listPath("/v1/mcp-connections", params), callArgs(options));
    return r.body;
  }
  async retrieve(connectionID: string, options: CallOptions = {}): Promise<MCPConnection> {
    const r = await this.#client.request<MCPConnection>("GET", `/v1/mcp-connections/${enc(connectionID)}`, callArgs(options));
    return r.body;
  }
}

// Triggers: the automation triggers read surface (spec §20.2.2).
export class Triggers {
  #client: Palai;
  constructor(client: Palai) {
    this.#client = client;
  }
  async list(params: ListParams = {}, options: CallOptions = {}): Promise<Page<Trigger>> {
    const r = await this.#client.request<Page<Trigger>>("GET", listPath("/v1/triggers", params), callArgs(options));
    return r.body;
  }
  async retrieve(triggerID: string, options: CallOptions = {}): Promise<Trigger> {
    const r = await this.#client.request<Trigger>("GET", `/v1/triggers/${enc(triggerID)}`, callArgs(options));
    return r.body;
  }
}
