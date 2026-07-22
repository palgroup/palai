import type { Palai } from "../client.ts";
import { callArgs, enc, listPath, type CallOptions, type ListParams, type Page } from "./shared.ts";

// AgentProfile / AgentRevision are the automation-agent management projections (spec §20.2.1, §10).
// No canonical schema generates them, so they are open interfaces: the fixed identity fields plus an
// index signature, so unknown server fields survive a round-trip (the SDK's open-world stance).
export interface AgentProfile {
  id: string;
  object: string;
  [key: string]: unknown;
}
export interface AgentRevision {
  id: string;
  object: string;
  [key: string]: unknown;
}

// Agents is the /v1/agents read + publish resource. It exposes the read side (list/get + a profile's
// revisions) and the publish transition — the authoring writes (create profile/revision) are the E11
// management surface, out of this parity task's scope.
export class Agents {
  #client: Palai;
  constructor(client: Palai) {
    this.#client = client;
  }

  // list returns a tenant-scoped page of agent-profile lineages.
  async list(params: ListParams = {}, options: CallOptions = {}): Promise<Page<AgentProfile>> {
    const result = await this.#client.request<Page<AgentProfile>>("GET", listPath("/v1/agents", params), callArgs(options));
    return result.body;
  }

  // retrieve reads one agent-profile lineage; a foreign/unknown id is a 404.
  async retrieve(agentID: string, options: CallOptions = {}): Promise<AgentProfile> {
    const result = await this.#client.request<AgentProfile>("GET", `/v1/agents/${enc(agentID)}`, callArgs(options));
    return result.body;
  }

  // listRevisions pages one profile's revisions. The cursor is profile-scoped server-side, so a
  // cursor from one profile does not validate on another.
  async listRevisions(agentID: string, params: ListParams = {}, options: CallOptions = {}): Promise<Page<AgentRevision>> {
    const result = await this.#client.request<Page<AgentRevision>>(
      "GET",
      listPath(`/v1/agents/${enc(agentID)}/revisions`, params),
      callArgs(options),
    );
    return result.body;
  }

  // publishRevision publishes a draft revision, making it the profile's active config.
  async publishRevision(agentID: string, revisionID: string, options: CallOptions = {}): Promise<AgentRevision> {
    const result = await this.#client.request<AgentRevision>(
      "POST",
      `/v1/agents/${enc(agentID)}/revisions/${enc(revisionID)}/publish`,
      callArgs(options),
    );
    return result.body;
  }
}
