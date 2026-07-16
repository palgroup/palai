package statemachines

// RunState is a Run lifecycle state (spec §22.3).
type RunState string

// RunCommand drives a Run state transition (spec §22.3).
type RunCommand string

const (
	RunQueued         RunState = "queued"
	RunProvisioning   RunState = "provisioning"
	RunRunning        RunState = "running"
	RunWaiting        RunState = "waiting"
	RunCompleted      RunState = "completed"
	RunFailed         RunState = "failed"
	RunCanceled       RunState = "canceled"
	RunTimedOut       RunState = "timed_out"
	RunBudgetExceeded RunState = "budget_exceeded"
)

const (
	RunCmdProvision     RunCommand = "provision"
	RunCmdStart         RunCommand = "start"
	RunCmdWait          RunCommand = "wait"
	RunCmdResume        RunCommand = "resume"
	RunCmdComplete      RunCommand = "complete"
	RunCmdFail          RunCommand = "fail"
	RunCmdCancel        RunCommand = "cancel"
	RunCmdTimeout       RunCommand = "timeout"
	RunCmdExhaustBudget RunCommand = "exhaust_budget"
)

// RunTable is the Run transition table (spec §22.3).
var RunTable = []Transition[RunState, RunCommand]{
	{RunQueued, RunCmdProvision, RunProvisioning, "run.provisioning.v1"},
	{RunProvisioning, RunCmdStart, RunRunning, "run.running.v1"},
	{RunRunning, RunCmdWait, RunWaiting, "run.waiting.v1"},
	{RunWaiting, RunCmdResume, RunRunning, "run.running.v1"},
	{RunRunning, RunCmdComplete, RunCompleted, "run.completed.v1"},
	{RunQueued, RunCmdCancel, RunCanceled, "run.canceled.v1"},
	{RunProvisioning, RunCmdCancel, RunCanceled, "run.canceled.v1"},
	{RunRunning, RunCmdCancel, RunCanceled, "run.canceled.v1"},
	{RunWaiting, RunCmdCancel, RunCanceled, "run.canceled.v1"},
	{RunQueued, RunCmdFail, RunFailed, "run.failed.v1"},
	{RunProvisioning, RunCmdFail, RunFailed, "run.failed.v1"},
	{RunRunning, RunCmdFail, RunFailed, "run.failed.v1"},
	{RunWaiting, RunCmdFail, RunFailed, "run.failed.v1"},
	{RunRunning, RunCmdTimeout, RunTimedOut, "run.timed_out.v1"},
	{RunWaiting, RunCmdTimeout, RunTimedOut, "run.timed_out.v1"},
	{RunRunning, RunCmdExhaustBudget, RunBudgetExceeded, "run.budget_exceeded.v1"},
	{RunWaiting, RunCmdExhaustBudget, RunBudgetExceeded, "run.budget_exceeded.v1"},
}
