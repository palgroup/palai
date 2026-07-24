// Server entrypoint (the default export condition). It exposes the API-key client, whose
// module chain imports the server-only guard, so bundling this for the browser fails loud.
// Browser code resolves ./index.browser instead (the "browser" export condition), which
// carries no credential path.

export { Palai, APIVersion } from "./client.ts";
export type { PalaiOptions, RequestOptions, ApiResult } from "./client.ts";

export { Responses } from "./resources/responses.ts";
export type { CreateOptions, RetrieveOptions, StreamOptions } from "./resources/responses.ts";

// The E13 T10 core-parity resources. Every one routes through the API-key client above, so importing
// any of them marks a module server-only — none is re-exported from ./index.browser (the browser
// entrypoint carries no credential path; that is the browser-direct-token DROP, enforced positively).
export { Sessions, SessionCommands } from "./resources/sessions.ts";
export type { SteerParams, InterruptParams } from "./resources/sessions.ts";
export { Agents } from "./resources/agents.ts";
export type { AgentProfile, AgentRevision } from "./resources/agents.ts";
export { Artifacts } from "./resources/artifacts.ts";
export type { Artifact, ArtifactDownload } from "./resources/artifacts.ts";
export { RepositoryBindings, Tools, MCPConnections, Triggers } from "./resources/reads.ts";
export type { Trigger } from "./resources/reads.ts";
export { SecretRefs } from "./resources/secret-refs.ts";
export type { SecretRef, SecretRefCreateParams, SecretRefRotateParams } from "./resources/secret-refs.ts";
export { ModelRoutes } from "./resources/model-routes.ts";
// ModelConnection/ModelRoute/ModelRouteRevision are now the generated contract types, exported once via
// the `export type * from ./generated/types.ts` below (E16 T1: no more hand-written open interfaces).
export type {
  ModelConnectionCreateParams,
  ModelRouteCreateParams,
  ModelRouteRevisionCreateParams,
} from "./resources/model-routes.ts";
export { Organizations, Projects, ApiKeys } from "./resources/provisioning.ts";
export type {
  Organization,
  OrganizationCreated,
  Project,
  ApiKey,
  ApiKeyCreated,
  OrganizationCreateParams,
  ProjectCreateParams,
  ProjectPolicyParams,
  ApiKeyCreateParams,
} from "./resources/provisioning.ts";
export type { CallOptions, DownloadOptions, ListParams, Page, ListView } from "./resources/shared.ts";

export { ResponseStream, isTerminalEvent, parseEventStream, fullJitterBackoff, delay } from "./stream.ts";
export type { SSEFrame, StreamTransport, ResponseStreamInit, StreamStart } from "./stream.ts";

// The E17 T8 external-orchestrator kit: the §35.1 five-step contract as thin helpers over the client,
// keeping the external workflow id and Palai's canonical run id SEPARATE (docs/orchestrator-kit.md). It
// imports the server-only client path, so it is never re-exported from ./index.browser.
export { Orchestrator, workflowIdempotencyKey, isTerminalStatus } from "./orchestrator.ts";
export type { WorkflowRun, StartOptions, WaitOptions, RunActivityOptions } from "./orchestrator.ts";

export * from "./errors.ts";
export type * from "./generated/types.ts";
