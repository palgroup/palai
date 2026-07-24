// Package a2a is the A2A 1.0 (Agent2Agent) HTTP+JSON server projection (spec §38, E17 Task 2). It exposes a
// published AgentRevision as an A2A 1.0 interface — the Agent Card discovery surface plus the message/task
// lifecycle — WITHOUT leaking anything internal (provider model name, tool inventory, tenant inventory): the
// card is a projection of explicitly-published safe fields (A2A-001). Inbound A2A messages map to canonical
// runs through the SAME admission the /v1/responses path uses (the adapter invents NO run identity, §38.2);
// A2A task/context ids are stored as an EXTERNAL ref beside the canonical run/session id, which is never
// replaced by an A2A-supplied id. Push notifications expose only the CONFIG CRUD surface — delivery is an
// honest ceiling.
//
// Honest ceilings (spec §5, §6): JWS/JCS Agent Card signing is v0-OUT ("when trust policy requires"); push
// DELIVERY is not wired (only pushNotificationConfigs CRUD, tokens read-redacted — the card advertises push
// only when a Pusher exists); multi-turn continuation of an input-required task is not correlated yet; and
// the only interop proof here is a loopback/fake generic client driving this server — a FOREIGN A2A peer is
// the §6 operator leg, so the capability stays "preview" and this package never claims otherwise.
package a2a

// The A2A 1.0 protocol version this server speaks, advertised on every Agent Card interface and checked on
// version-negotiation. It is an EXACT advertisement (A2A-001) — the card names the version and binding it
// actually serves, never a superset.
const (
	ProtocolVersion = "1.0"
	// HTTPJSONBinding is the transport binding this server implements (the A2A "HTTP+JSON/REST" binding, spec
	// §11). JSONRPC/gRPC bindings are NOT served and are deliberately not advertised.
	HTTPJSONBinding = "HTTP+JSON"
)

// TaskState is the A2A 1.0 task lifecycle state (spec §TaskState). The JSON values are the canonical
// lowercase-hyphen forms the HTTP+JSON binding uses; MapRunState projects a canonical run status onto one.
type TaskState string

const (
	TaskStateSubmitted     TaskState = "submitted"
	TaskStateWorking       TaskState = "working"
	TaskStateInputRequired TaskState = "input-required"
	TaskStateCompleted     TaskState = "completed"
	TaskStateCanceled      TaskState = "canceled"
	TaskStateFailed        TaskState = "failed"
	TaskStateRejected      TaskState = "rejected"
	// TaskStateUnknown is the honest projection of a task whose canonical run record is no longer resolvable
	// — retention-reaped, or otherwise gone (M-1). It is a valid A2A 1.0 state; using it stops a client from
	// polling a phantom "working" task forever while never fabricating a completed/failed outcome we can't see.
	TaskStateUnknown TaskState = "unknown"
)

// Terminal reports whether a state is a lifecycle end — the streaming terminal-consistency anchor (A2A-002):
// the final stream frame carries a terminal state with final=true, and a subsequent tasks/{id} GET returns
// the SAME terminal state.
func (s TaskState) Terminal() bool {
	switch s {
	case TaskStateCompleted, TaskStateCanceled, TaskStateFailed, TaskStateRejected:
		return true
	}
	return false
}

// Part is one piece of a Message or Artifact. kind discriminates: "text" (Text set), "file" (File set),
// "data" (Data set). The A2A 1.0 shape uses a `kind` discriminator per part.
type Part struct {
	Kind string         `json:"kind"`
	Text string         `json:"text,omitempty"`
	File *FilePart      `json:"file,omitempty"`
	Data map[string]any `json:"data,omitempty"`
}

// FilePart carries an inline file (base64 Bytes) or a URI. An inbound file part is ingested + scanned +
// stored as an artifact (the A2A-004 server half); the raw bytes never become a privileged instruction.
type FilePart struct {
	Name     string `json:"name,omitempty"`
	MimeType string `json:"mimeType,omitempty"`
	Bytes    string `json:"bytes,omitempty"` // base64
	URI      string `json:"uri,omitempty"`
}

// Message is an A2A message (inbound request or a direct agent reply). Role is "user" (inbound) or "agent"
// (reply). MessageID is the client-supplied idempotency anchor (used as the run idempotency key). ContextID
// is an EXTERNAL reference carried onto the task (it never replaces a canonical session id, §38.2). Metadata
// is opaque and, critically, can NOT carry identity: any org/project inside it is IGNORED (§38.6, the
// authenticated bearer scope governs).
//
// HONEST CEILING (§6): an inbound TaskID (a follow-up to an input-required task) is NOT yet correlated —
// multi-turn continuation that resumes a waiting run through the SAME canonical run is later work; today a
// taskId'd message:send admits a fresh run. The field is parsed but deliberately not acted on, and message:
// send does not claim otherwise.
type Message struct {
	Role      string         `json:"role"`
	Parts     []Part         `json:"parts"`
	MessageID string         `json:"messageId,omitempty"`
	TaskID    string         `json:"taskId,omitempty"`
	ContextID string         `json:"contextId,omitempty"`
	Metadata  map[string]any `json:"metadata,omitempty"`
	Kind      string         `json:"kind,omitempty"` // "message" on a reply
}

// TaskStatus is a task's current state + timestamp (RFC-3339).
type TaskStatus struct {
	State     TaskState `json:"state"`
	Timestamp string    `json:"timestamp,omitempty"`
	// Message is an optional status detail — e.g. the input-required prompt (A2A-003).
	Message *Message `json:"message,omitempty"`
}

// Artifact is an A2A task artifact (an output). ArtifactID is the canonical artifact id; Parts carry the
// content or a reference. A file output references the stored artifact by id — bytes are fetched over the
// authenticated artifact API, not inlined into every task read.
type Artifact struct {
	ArtifactID string `json:"artifactId"`
	Name       string `json:"name,omitempty"`
	Parts      []Part `json:"parts"`
}

// Task is the A2A 1.0 task resource. ID is the EXTERNAL A2A task id (never the canonical run id); ContextID
// is the external context id (never the canonical session id). Status is the projected lifecycle state.
type Task struct {
	ID        string     `json:"id"`
	ContextID string     `json:"contextId"`
	Status    TaskStatus `json:"status"`
	Artifacts []Artifact `json:"artifacts,omitempty"`
	History   []Message  `json:"history,omitempty"`
	Kind      string     `json:"kind"` // always "task"
}

// StatusUpdateEvent / ArtifactUpdateEvent are the streaming frames (spec §11 current 1.0 representation: the
// member name is the discriminator). Final marks the terminal frame (A2A-002 terminal consistency).
type StatusUpdateEvent struct {
	StatusUpdate statusUpdate `json:"statusUpdate"`
}
type statusUpdate struct {
	TaskID    string     `json:"taskId"`
	ContextID string     `json:"contextId"`
	Status    TaskStatus `json:"status"`
	Final     bool       `json:"final"`
}
type ArtifactUpdateEvent struct {
	ArtifactUpdate artifactUpdate `json:"artifactUpdate"`
}
type artifactUpdate struct {
	TaskID   string   `json:"taskId"`
	Artifact Artifact `json:"artifact"`
}

// NewStatusUpdate / NewArtifactUpdate build the discriminated stream frames.
func NewStatusUpdate(taskID, contextID string, status TaskStatus, final bool) StatusUpdateEvent {
	return StatusUpdateEvent{statusUpdate{TaskID: taskID, ContextID: contextID, Status: status, Final: final}}
}
func NewArtifactUpdate(taskID string, a Artifact) ArtifactUpdateEvent {
	return ArtifactUpdateEvent{artifactUpdate{TaskID: taskID, Artifact: a}}
}

// PushNotificationConfig is an A2A push-notification target for a task's asynchronous updates: URL is the
// destination, Token an optional bearer the receiver checks. HONEST posture (A2A-003, M-3): only the
// CONFIG CRUD exists — the token is stored in the task's push_configs JSONB and REDACTED on every read
// (server.redactPush), with RLS confining the row. It is NOT a secret_ref handle, and DELIVERY is not wired
// (see Pusher). A secret_ref indirection + real signed delivery are later hardening (§6).
type PushNotificationConfig struct {
	ID    string `json:"id"`
	URL   string `json:"url"`
	Token string `json:"token,omitempty"`
}
