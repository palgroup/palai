// Code generated from the canonical execution and common schemas; DO NOT EDIT.
//
// The SDK's typed transport surface: fixed-field construction/validation views of the
// canonical objects, with open unions kept lossless via an index signature. Identifier
// types are inlined as string aliases so this file is self-contained.

export type AttemptId = string;
export type CommandId = string;
export type EventId = string;
export type MessageId = string;
export type OpaqueId = string;
export type OrganizationId = string;
export type ProjectId = string;
export type RepositoryBindingId = string;
export type RequestId = string;
export type ResponseId = string;
export type RunId = string;
export type SessionId = string;

export interface Case {
  checksum: string;
  db_assertions: string[];
  id: string;
  image_digest: string;
  mtls_enroll: string;
  proof_class: string;
  provider_request_id: string;
  run_id: string;
  status: string;
  terminal: Record<string, unknown>;
  usage: Record<string, unknown>;
}

export interface Command {
  applied_sequence?: number | null;
  created_at: string;
  delivery?: string;
  id: CommandId;
  kind: string;
  object: string;
  result?: Record<string, unknown>;
  session_id: SessionId;
  status: string;
}

export interface CommandCreateRequest {
  command_id: CommandId;
  delivery?: string;
  immediate?: boolean;
  kind: string;
  message?: string;
  model?: string;
  tools?: string[];
}

// open union: unknown fields and unknown type values survive a round-trip (ADR-0002, spec API-009).
export interface ContentItem {
  type: string;
  [key: string]: unknown;
}

export interface Event {
  attempt_id?: AttemptId;
  data: Record<string, unknown>;
  datacontenttype?: string;
  id: EventId;
  project_id?: ProjectId;
  run_id?: RunId;
  sequence: number;
  session_id?: SessionId;
  source: string;
  specversion: string;
  subject?: string;
  time: string;
  type: string;
}

export interface LocalLiveEvidenceManifest {
  api_version: string;
  captured_at: string;
  cases: Record<string, unknown>[];
  git_sha: string;
  migration: string;
  release: string;
}

export interface MCPConnection {
  created_at?: string;
  disabled?: boolean;
  id: OpaqueId;
  name: string;
  object: string;
  organization_id?: OrganizationId;
  project_id?: ProjectId;
  transport: string;
  trust_level?: string;
}

export interface Message {
  content: ContentItem[];
  created_at: string;
  delivery?: string;
  id: MessageId;
  role: string;
  source_ref?: OpaqueId;
  visibility?: string;
}

export interface Page {
  data: unknown[];
  has_more: boolean;
  next_cursor?: string | null;
  previous_cursor?: string | null;
}

export interface PageParams {
  after?: string;
  before?: string;
  limit?: number;
}

export interface PreparationReceipt {
  base_commit: string;
  branch?: string;
  prepared_at?: string;
  requested_ref?: string;
  tree_hash: string;
}

export interface Problem {
  code: string;
  context?: Record<string, unknown>;
  detail?: string;
  field_errors?: Record<string, unknown>[];
  instance?: string;
  request_id: RequestId;
  retryable?: boolean;
  status: number;
  title: string;
  type: string;
}

export interface RepositoryBinding {
  allowed_operations?: string[];
  clone_url: string;
  connection_ref?: string;
  created_at?: string;
  data_classification?: string;
  default_branch: string;
  id: RepositoryBindingId;
  object: string;
  organization_id?: OrganizationId;
  policy?: Record<string, unknown>;
  project_id?: ProjectId;
  provider: string;
  region_constraint?: string;
  repository_identity: string;
}

export interface ResourceEnvelope {
  created_at: string;
  id: OpaqueId;
  labels?: Record<string, string>;
  metadata?: Record<string, unknown>;
  object: string;
  organization_id?: OrganizationId;
  project_id?: ProjectId;
  revision?: number;
  updated_at?: string;
}

export interface Response {
  created_at: string;
  error?: Problem | null;
  id: ResponseId;
  labels?: Record<string, string>;
  metadata?: Record<string, unknown>;
  model: string;
  object: string;
  organization_id?: OrganizationId;
  output: ContentItem[];
  project_id?: ProjectId;
  revision?: number;
  run_id?: RunId;
  session_id?: SessionId;
  status: string;
  updated_at?: string;
  usage: Usage;
}

export interface ResponseCreateRequest {
  agent_revision_id?: string | null;
  background?: boolean;
  budget?: Record<string, unknown>;
  callback?: Record<string, unknown>;
  capabilities?: string[];
  context?: Record<string, unknown>;
  delegation?: Record<string, unknown>;
  engine?: string | null;
  input: string | ContentItem[];
  instructions?: string;
  max_output_tokens?: number;
  max_tool_calls?: number;
  metadata?: Record<string, unknown>;
  model?: string;
  output?: Record<string, unknown>;
  parallel_tool_calls?: boolean;
  previous_response_id?: string | null;
  repository?: Record<string, unknown>;
  run_template_revision_id?: string | null;
  session_id?: string | null;
  skills?: string[];
  store?: boolean;
  stream?: boolean;
  tool_choice?: string;
  tool_sets?: string[];
  tools?: Record<string, unknown>[];
  workspace?: Record<string, unknown>;
}

export interface Session {
  created_at: string;
  id: SessionId;
  metadata?: Record<string, unknown>;
  object: string;
  organization_id?: OrganizationId;
  project_id?: ProjectId;
  status: string;
}

export interface Skill {
  created_at?: string;
  digest?: string;
  id: OpaqueId;
  name?: string;
  object: string;
  organization_id?: OrganizationId;
  project_id?: ProjectId;
  revision_number?: number;
  scan_findings?: Record<string, unknown>[];
  skill_id?: OpaqueId;
  source_url?: string;
  state?: string;
}

export interface Tool {
  canonical_name: string;
  created_at?: string;
  id: OpaqueId;
  model_visible_name: string;
  object: string;
  organization_id?: OrganizationId;
  project_id?: ProjectId;
}

export interface ToolHTTPCallback {
  operation_id: string;
  problem?: Record<string, unknown>;
  protocol: string;
  result?: Record<string, unknown>;
  tool_call_id: string;
}

export interface ToolHTTPInvoke {
  arguments: Record<string, unknown>;
  attempt_id: string;
  callback: Record<string, unknown>;
  deadline: string;
  protocol: string;
  request_hash: string;
  run_id: string;
  tool_call_id: string;
  tool_revision: string;
}

export interface ToolSet {
  created_at?: string;
  digest?: string;
  id: OpaqueId;
  object: string;
  organization_id?: OrganizationId;
  project_id?: ProjectId;
  revision_number: number;
  set: string;
}

export interface Usage {
  cost?: Record<string, unknown>;
  input_tokens: number;
  output_tokens: number;
  tool_calls?: number;
  total_tokens?: number;
}
