// Code generated from the canonical CanonicalIdentifiers schema; DO NOT EDIT.

export type ArtifactId = string;
export type AttemptId = string;
export type CommandId = string;
export type EventId = string;
export type FrameId = string;
export type MessageId = string;
export type ModelRequestId = string;
export type OpaqueId = string;
export type OrganizationId = string;
export type ProjectId = string;
export type RequestId = string;
export type ResponseId = string;
export type RunId = string;
export type SessionId = string;
export type ToolCallId = string;
export type WorkspaceId = string;

export const artifactIdPattern = /^art_[A-Za-z0-9_-]+$/;
export const attemptIdPattern = /^att_[A-Za-z0-9_-]+$/;
export const commandIdPattern = /^cmd_[A-Za-z0-9_-]+$/;
export const eventIdPattern = /^evt_[A-Za-z0-9_-]+$/;
export const frameIdPattern = /^frm_[A-Za-z0-9_-]+$/;
export const messageIdPattern = /^msg_[A-Za-z0-9_-]+$/;
export const modelRequestIdPattern = /^mreq_[A-Za-z0-9_-]+$/;
export const opaqueIdPattern = /^[a-z][a-z0-9]{1,11}_[A-Za-z0-9_-]+$/;
export const organizationIdPattern = /^org_[A-Za-z0-9_-]+$/;
export const projectIdPattern = /^prj_[A-Za-z0-9_-]+$/;
export const requestIdPattern = /^req_[A-Za-z0-9_-]+$/;
export const responseIdPattern = /^resp_[A-Za-z0-9_-]+$/;
export const runIdPattern = /^run_[A-Za-z0-9_-]+$/;
export const sessionIdPattern = /^ses_[A-Za-z0-9_-]+$/;
export const toolCallIdPattern = /^tcall_[A-Za-z0-9_-]+$/;
export const workspaceIdPattern = /^wksp_[A-Za-z0-9_-]+$/;

export function isArtifactId(value: string): boolean {
  return artifactIdPattern.test(value);
}
export function isAttemptId(value: string): boolean {
  return attemptIdPattern.test(value);
}
export function isCommandId(value: string): boolean {
  return commandIdPattern.test(value);
}
export function isEventId(value: string): boolean {
  return eventIdPattern.test(value);
}
export function isFrameId(value: string): boolean {
  return frameIdPattern.test(value);
}
export function isMessageId(value: string): boolean {
  return messageIdPattern.test(value);
}
export function isModelRequestId(value: string): boolean {
  return modelRequestIdPattern.test(value);
}
export function isOpaqueId(value: string): boolean {
  return opaqueIdPattern.test(value);
}
export function isOrganizationId(value: string): boolean {
  return organizationIdPattern.test(value);
}
export function isProjectId(value: string): boolean {
  return projectIdPattern.test(value);
}
export function isRequestId(value: string): boolean {
  return requestIdPattern.test(value);
}
export function isResponseId(value: string): boolean {
  return responseIdPattern.test(value);
}
export function isRunId(value: string): boolean {
  return runIdPattern.test(value);
}
export function isSessionId(value: string): boolean {
  return sessionIdPattern.test(value);
}
export function isToolCallId(value: string): boolean {
  return toolCallIdPattern.test(value);
}
export function isWorkspaceId(value: string): boolean {
  return workspaceIdPattern.test(value);
}
