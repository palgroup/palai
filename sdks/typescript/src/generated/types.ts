// Code generated from the canonical execution and common schemas; DO NOT EDIT.
//
// The SDK's typed transport surface: fixed-field construction/validation views of the
// canonical objects, with open unions kept lossless via an index signature. Identifier
// types are inlined as string aliases so this file is self-contained.

export type AttemptId = string;
export type EventId = string;
export type MessageId = string;
export type OpaqueId = string;
export type OrganizationId = string;
export type ProjectId = string;
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
  session_id?: string | null;
  skills?: string[];
  store?: boolean;
  stream?: boolean;
  tool_choice?: string;
  tool_sets?: string[];
  tools?: Record<string, unknown>[];
  workspace?: Record<string, unknown>;
}

export interface Usage {
  cost?: Record<string, unknown>;
  input_tokens: number;
  output_tokens: number;
  tool_calls?: number;
  total_tokens?: number;
}
