package statemachines

// CommandState is a Command lifecycle state. Spec §22.4 describes commands as
// durable resources whose acceptance means "durably queued, not applied" and
// whose applied_sequence marks where they took effect, but it does not
// enumerate a state set. This is the minimal machine derived from that
// acceptance/apply/expiry language; future command kinds must extend it
// additively rather than redefine these rows.
type CommandState string

// CommandCommand drives a Command state transition (spec §22.4, derived).
type CommandCommand string

const (
	CommandQueued   CommandState = "queued"
	CommandApplying CommandState = "applying"
	CommandApplied  CommandState = "applied"
	CommandRejected CommandState = "rejected"
	CommandExpired  CommandState = "expired"
)

const (
	CommandCmdApply       CommandCommand = "apply"
	CommandCmdFinishApply CommandCommand = "finish_apply"
	CommandCmdReject      CommandCommand = "reject"
	CommandCmdExpire      CommandCommand = "expire"
)

// CommandTable is the Command transition table (spec §22.4, derived). A command
// is born queued (command.accepted.v1 is its birth event, not a transition);
// apply begins application, and applying resolves to applied, rejected, or
// expired.
var CommandTable = []Transition[CommandState, CommandCommand]{
	{CommandQueued, CommandCmdApply, CommandApplying, "command.applying.v1"},
	{CommandApplying, CommandCmdFinishApply, CommandApplied, "command.applied.v1"},
	{CommandApplying, CommandCmdReject, CommandRejected, "command.rejected.v1"},
	{CommandApplying, CommandCmdExpire, CommandExpired, "command.expired.v1"},
}
