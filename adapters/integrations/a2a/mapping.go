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
// bearer scope is the ONLY authority: the message argument is accepted so the contract is explicit that its
// metadata is examined and DISCARDED — any organization/project inside it is IGNORED. This closes the
// forged-identity attack: an A2A client cannot make its run execute in another tenant by putting that
// tenant's ids in metadata.
func GovernIdentity(authOrg, authProject string, _ Message) (org, project string) {
	return authOrg, authProject
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
