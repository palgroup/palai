package statemachines

// SessionState is a Session lifecycle state (spec §22.1).
type SessionState string

// SessionCommand drives a Session state transition (spec §22.1).
type SessionCommand string

const (
	SessionActive  SessionState = "active"
	SessionPaused  SessionState = "paused"
	SessionClosing SessionState = "closing"
	SessionClosed  SessionState = "closed"
	SessionDeleted SessionState = "deleted"
)

const (
	SessionCmdPause       SessionCommand = "pause"
	SessionCmdResume      SessionCommand = "resume"
	SessionCmdClose       SessionCommand = "close"
	SessionCmdFinishClose SessionCommand = "finish_close"
	SessionCmdDelete      SessionCommand = "delete"
)

// SessionTable is the Session transition table (spec §22.1). A Session is born
// active (created is not a state), and a terminal run never closes it: every row
// here is driven by an explicit session command.
var SessionTable = []Transition[SessionState, SessionCommand]{
	{SessionActive, SessionCmdPause, SessionPaused, "session.paused.v1"},
	{SessionPaused, SessionCmdResume, SessionActive, "session.active.v1"},
	{SessionActive, SessionCmdClose, SessionClosing, "session.closing.v1"},
	{SessionPaused, SessionCmdClose, SessionClosing, "session.closing.v1"},
	{SessionClosing, SessionCmdFinishClose, SessionClosed, "session.closed.v1"},
	{SessionClosed, SessionCmdDelete, SessionDeleted, "session.deleted.v1"},
}
