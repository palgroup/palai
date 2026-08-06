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

// AgentRevisionCreateParams is the revision body, passed through as the caller wrote it.
//
// IT IS OPEN BECAUSE THE SERVER OWNS THE SHAPE. The control plane hands this body to the agent service
// unparsed (api/agents.go: `CreateAgentRevision(ctx, scope, agentID, raw)`), so the schema lives there
// and a closed interface here would be a second, quietly diverging copy of it — the SDK would start
// refusing fields the plane accepts. `instructions` and `model_route_id` are named because every caller
// sends them and a bare index signature gives an author nothing to autocomplete.
export interface AgentRevisionCreateParams {
  instructions?: string;
  model_route_id?: string;
  [key: string]: unknown;
}

// Agents is the /v1/agents resource: author a profile and its revisions, read them back, and publish.
//
// ‼️ THE AUTHORING WRITES WERE ABSENT AND THE COMMENT HERE SAID SO — "out of this parity task's scope".
// That was true of the task and false of the product: with only list/retrieve/publish, an SDK holder
// could publish a revision they had no way to create, on a profile they had no way to open. The plane
// has had POST /v1/agents and POST /v1/agents/{id}/revisions the whole time; what was missing was the
// half of the SDK that reaches them, and "a customer creates an agent and opens a session with the key
// we minted for them" is not a sentence the SDK could complete.
export class Agents {
  #client: Palai;
  constructor(client: Palai) {
    this.#client = client;
  }

  // create opens a new agent-profile lineage. The profile is an IDENTITY and carries no behaviour: what
  // an agent does lives on its revisions, which is why this takes a name and nothing else.
  async create(params: { name: string }, options: CallOptions = {}): Promise<AgentProfile> {
    const r = await this.#client.request<AgentProfile>("POST", "/v1/agents", { body: params, ...callArgs(options) });
    return r.body;
  }

  // createRevision adds a DRAFT revision to a profile (201). A draft steers no run — publishRevision is
  // the transition that makes it the one a session resolves — so authoring and arming stay two calls a
  // reader can tell apart.
  //
  // The 201 carries the id and NO Location header, and that is the plane's deliberate choice rather than
  // an omission: `/v1/agent-revisions/{id}` is not a mounted address, and a correct-looking wrong URL is
  // worse than none. Read it from the returned body.
  async createRevision(
    agentID: string,
    params: AgentRevisionCreateParams,
    options: CallOptions = {},
  ): Promise<AgentRevision> {
    const r = await this.#client.request<AgentRevision>(
      "POST",
      `/v1/agents/${enc(agentID)}/revisions`,
      { body: params, ...callArgs(options) },
    );
    return r.body;
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
