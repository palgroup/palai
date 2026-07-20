// Code generated from the canonical CanonicalIdentifiers schema; DO NOT EDIT.
package contracts

import "regexp"

// ArtifactID is an opaque, prefixed, URL-safe identifier (schema $def artifact_id).
type ArtifactID string

// AttemptID is an opaque, prefixed, URL-safe identifier (schema $def attempt_id).
type AttemptID string

// CommandID is an opaque, prefixed, URL-safe identifier (schema $def command_id).
type CommandID string

// EventID is an opaque, prefixed, URL-safe identifier (schema $def event_id).
type EventID string

// FrameID is an opaque, prefixed, URL-safe identifier (schema $def frame_id).
type FrameID string

// MessageID is an opaque, prefixed, URL-safe identifier (schema $def message_id).
type MessageID string

// ModelRequestID is an opaque, prefixed, URL-safe identifier (schema $def model_request_id).
type ModelRequestID string

// OpaqueID is an opaque, prefixed, URL-safe identifier (schema $def opaque_id).
type OpaqueID string

// OrganizationID is an opaque, prefixed, URL-safe identifier (schema $def organization_id).
type OrganizationID string

// ProjectID is an opaque, prefixed, URL-safe identifier (schema $def project_id).
type ProjectID string

// RepositoryBindingID is an opaque, prefixed, URL-safe identifier (schema $def repository_binding_id).
type RepositoryBindingID string

// RequestID is an opaque, prefixed, URL-safe identifier (schema $def request_id).
type RequestID string

// ResponseID is an opaque, prefixed, URL-safe identifier (schema $def response_id).
type ResponseID string

// RunID is an opaque, prefixed, URL-safe identifier (schema $def run_id).
type RunID string

// SessionID is an opaque, prefixed, URL-safe identifier (schema $def session_id).
type SessionID string

// ToolCallID is an opaque, prefixed, URL-safe identifier (schema $def tool_call_id).
type ToolCallID string

// WorkspaceID is an opaque, prefixed, URL-safe identifier (schema $def workspace_id).
type WorkspaceID string

var (
	artifactIDPattern          = regexp.MustCompile(`^art_[A-Za-z0-9_-]+$`)
	attemptIDPattern           = regexp.MustCompile(`^att_[A-Za-z0-9_-]+$`)
	commandIDPattern           = regexp.MustCompile(`^cmd_[A-Za-z0-9_-]+$`)
	eventIDPattern             = regexp.MustCompile(`^evt_[A-Za-z0-9_-]+$`)
	frameIDPattern             = regexp.MustCompile(`^frm_[A-Za-z0-9_-]+$`)
	messageIDPattern           = regexp.MustCompile(`^msg_[A-Za-z0-9_-]+$`)
	modelRequestIDPattern      = regexp.MustCompile(`^mreq_[A-Za-z0-9_-]+$`)
	opaqueIDPattern            = regexp.MustCompile(`^[a-z][a-z0-9]{1,11}_[A-Za-z0-9_-]+$`)
	organizationIDPattern      = regexp.MustCompile(`^org_[A-Za-z0-9_-]+$`)
	projectIDPattern           = regexp.MustCompile(`^prj_[A-Za-z0-9_-]+$`)
	repositoryBindingIDPattern = regexp.MustCompile(`^repo_[A-Za-z0-9_-]+$`)
	requestIDPattern           = regexp.MustCompile(`^req_[A-Za-z0-9_-]+$`)
	responseIDPattern          = regexp.MustCompile(`^resp_[A-Za-z0-9_-]+$`)
	runIDPattern               = regexp.MustCompile(`^run_[A-Za-z0-9_-]+$`)
	sessionIDPattern           = regexp.MustCompile(`^ses_[A-Za-z0-9_-]+$`)
	toolCallIDPattern          = regexp.MustCompile(`^tcall_[A-Za-z0-9_-]+$`)
	workspaceIDPattern         = regexp.MustCompile(`^wksp_[A-Za-z0-9_-]+$`)
)

// Valid reports whether id matches the canonical artifact_id pattern.
func (id ArtifactID) Valid() bool { return artifactIDPattern.MatchString(string(id)) }

// Valid reports whether id matches the canonical attempt_id pattern.
func (id AttemptID) Valid() bool { return attemptIDPattern.MatchString(string(id)) }

// Valid reports whether id matches the canonical command_id pattern.
func (id CommandID) Valid() bool { return commandIDPattern.MatchString(string(id)) }

// Valid reports whether id matches the canonical event_id pattern.
func (id EventID) Valid() bool { return eventIDPattern.MatchString(string(id)) }

// Valid reports whether id matches the canonical frame_id pattern.
func (id FrameID) Valid() bool { return frameIDPattern.MatchString(string(id)) }

// Valid reports whether id matches the canonical message_id pattern.
func (id MessageID) Valid() bool { return messageIDPattern.MatchString(string(id)) }

// Valid reports whether id matches the canonical model_request_id pattern.
func (id ModelRequestID) Valid() bool { return modelRequestIDPattern.MatchString(string(id)) }

// Valid reports whether id matches the canonical opaque_id pattern.
func (id OpaqueID) Valid() bool { return opaqueIDPattern.MatchString(string(id)) }

// Valid reports whether id matches the canonical organization_id pattern.
func (id OrganizationID) Valid() bool { return organizationIDPattern.MatchString(string(id)) }

// Valid reports whether id matches the canonical project_id pattern.
func (id ProjectID) Valid() bool { return projectIDPattern.MatchString(string(id)) }

// Valid reports whether id matches the canonical repository_binding_id pattern.
func (id RepositoryBindingID) Valid() bool { return repositoryBindingIDPattern.MatchString(string(id)) }

// Valid reports whether id matches the canonical request_id pattern.
func (id RequestID) Valid() bool { return requestIDPattern.MatchString(string(id)) }

// Valid reports whether id matches the canonical response_id pattern.
func (id ResponseID) Valid() bool { return responseIDPattern.MatchString(string(id)) }

// Valid reports whether id matches the canonical run_id pattern.
func (id RunID) Valid() bool { return runIDPattern.MatchString(string(id)) }

// Valid reports whether id matches the canonical session_id pattern.
func (id SessionID) Valid() bool { return sessionIDPattern.MatchString(string(id)) }

// Valid reports whether id matches the canonical tool_call_id pattern.
func (id ToolCallID) Valid() bool { return toolCallIDPattern.MatchString(string(id)) }

// Valid reports whether id matches the canonical workspace_id pattern.
func (id WorkspaceID) Valid() bool { return workspaceIDPattern.MatchString(string(id)) }
