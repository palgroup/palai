package statemachines

// AttemptState is an Attempt lifecycle state (spec §22.3).
type AttemptState string

// AttemptCommand drives an Attempt state transition (spec §22.3).
type AttemptCommand string

const (
	AttemptAssigned  AttemptState = "assigned"
	AttemptStarting  AttemptState = "starting"
	AttemptActive    AttemptState = "active"
	AttemptDraining  AttemptState = "draining"
	AttemptSucceeded AttemptState = "succeeded"
	AttemptFailed    AttemptState = "failed"
	AttemptLost      AttemptState = "lost"
	AttemptPreempted AttemptState = "preempted"
)

const (
	AttemptCmdStart    AttemptCommand = "start"
	AttemptCmdActivate AttemptCommand = "activate"
	AttemptCmdDrain    AttemptCommand = "drain"
	AttemptCmdSucceed  AttemptCommand = "succeed"
	AttemptCmdFail     AttemptCommand = "fail"
	AttemptCmdLose     AttemptCommand = "lose"
	AttemptCmdPreempt  AttemptCommand = "preempt"
)

// AttemptTable is the Attempt transition table (spec §22.3). fail, lose, and
// preempt terminate from every non-terminal state; succeed only from draining.
var AttemptTable = []Transition[AttemptState, AttemptCommand]{
	{AttemptAssigned, AttemptCmdStart, AttemptStarting, "attempt.starting.v1"},
	{AttemptStarting, AttemptCmdActivate, AttemptActive, "attempt.active.v1"},
	{AttemptActive, AttemptCmdDrain, AttemptDraining, "attempt.draining.v1"},
	{AttemptDraining, AttemptCmdSucceed, AttemptSucceeded, "attempt.succeeded.v1"},

	{AttemptAssigned, AttemptCmdFail, AttemptFailed, "attempt.failed.v1"},
	{AttemptStarting, AttemptCmdFail, AttemptFailed, "attempt.failed.v1"},
	{AttemptActive, AttemptCmdFail, AttemptFailed, "attempt.failed.v1"},
	{AttemptDraining, AttemptCmdFail, AttemptFailed, "attempt.failed.v1"},

	{AttemptAssigned, AttemptCmdLose, AttemptLost, "attempt.lost.v1"},
	{AttemptStarting, AttemptCmdLose, AttemptLost, "attempt.lost.v1"},
	{AttemptActive, AttemptCmdLose, AttemptLost, "attempt.lost.v1"},
	{AttemptDraining, AttemptCmdLose, AttemptLost, "attempt.lost.v1"},

	{AttemptAssigned, AttemptCmdPreempt, AttemptPreempted, "attempt.preempted.v1"},
	{AttemptStarting, AttemptCmdPreempt, AttemptPreempted, "attempt.preempted.v1"},
	{AttemptActive, AttemptCmdPreempt, AttemptPreempted, "attempt.preempted.v1"},
	{AttemptDraining, AttemptCmdPreempt, AttemptPreempted, "attempt.preempted.v1"},
}
