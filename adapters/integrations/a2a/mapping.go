package a2a

import "strings"

// MapRunState projects a canonical run status onto an A2A TaskState (A2A-002). The mapping is total: an
// unknown status is treated as still-working rather than silently terminal, so a projection bug never
// fabricates a terminal state a subsequent GET would contradict.
func MapRunState(canonical string) TaskState {
	switch canonical {
	case "queued", "created":
		return TaskStateSubmitted
	case "running", "in_progress":
		return TaskStateWorking
	case "waiting_for_input", "requires_action":
		return TaskStateInputRequired
	case "completed", "succeeded":
		return TaskStateCompleted
	case "canceled", "cancelled":
		return TaskStateCanceled
	case "failed", "errored":
		return TaskStateFailed
	case "rejected":
		return TaskStateRejected
	default:
		return TaskStateWorking
	}
}

// DecideDirectMessage decides the message:send result shape (§38.2, A2A-002): a DIRECT Message is returned
// ONLY for a genuinely-complete, non-durable response — a synchronous completion the client will never need
// to poll. Every other outcome (still working, input-required, durable/stored, or any non-terminal state)
// returns a Task the client tracks by id. A durable run ALWAYS returns a Task even if already complete, so
// the client has a task id to retrieve/subscribe to.
func DecideDirectMessage(state TaskState, durable bool) bool {
	return state == TaskStateCompleted && !durable
}

// GovernIdentity resolves the tenant identity that governs an inbound A2A message (§38.6). The authenticated
// bearer scope is the ONLY authority: any organization/project carried in the message metadata is IGNORED.
// This closes the forged-identity attack — an A2A client cannot make its run execute in another tenant by
// putting that tenant's ids in metadata.
//
// ponytail: TRUSTS metadata on purpose in this first commit (RED). The identity-override crown test must
// catch the forged org/project before the fix lands.
func GovernIdentity(authOrg, authProject string, msg Message) (org, project string) {
	org, project = authOrg, authProject
	if v, ok := msg.Metadata["organization"].(string); ok && v != "" {
		org = v
	}
	if v, ok := msg.Metadata["project"].(string); ok && v != "" {
		project = v
	}
	return org, project
}

// MessageText concatenates the text parts of an inbound message into the run input prompt. File/data parts
// are handled separately (ingested as artifacts / carried as structured input); this is the plain-text seam.
func MessageText(msg Message) string {
	var b strings.Builder
	for _, p := range msg.Parts {
		if p.Kind == "text" && p.Text != "" {
			if b.Len() > 0 {
				b.WriteString("\n")
			}
			b.WriteString(p.Text)
		}
	}
	return b.String()
}

// FileParts returns the inbound file parts (the A2A-004 server-half ingest targets).
func FileParts(msg Message) []FilePart {
	var out []FilePart
	for _, p := range msg.Parts {
		if p.Kind == "file" && p.File != nil {
			out = append(out, *p.File)
		}
	}
	return out
}

// BuildTask assembles the A2A Task resource from EXTERNAL ids + a projected state. The external ids are the
// a2a task/context ids — the canonical run/session id is never passed here (§38.2).
func BuildTask(externalTaskID, externalContextID string, status TaskStatus, artifacts []Artifact) Task {
	return Task{
		ID:        externalTaskID,
		ContextID: externalContextID,
		Status:    status,
		Artifacts: artifacts,
		Kind:      "task",
	}
}

// BuildDirectMessage assembles a direct agent-reply Message (the DecideDirectMessage true case).
func BuildDirectMessage(replyText, messageID, contextID string) Message {
	return Message{
		Role:      "agent",
		Parts:     []Part{{Kind: "text", Text: replyText}},
		MessageID: messageID,
		ContextID: contextID,
		Kind:      "message",
	}
}
