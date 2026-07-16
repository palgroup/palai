package statemachines

// ResponseState is a Response lifecycle state (spec §8.3).
type ResponseState string

// ResponseCommand drives a Response state transition (spec §8.3).
type ResponseCommand string

const (
	ResponseQueued             ResponseState = "queued"
	ResponseProvisioning       ResponseState = "provisioning"
	ResponseInProgress         ResponseState = "in_progress"
	ResponseWaitingForTool     ResponseState = "waiting_for_tool"
	ResponseWaitingForApproval ResponseState = "waiting_for_approval"
	ResponseWaitingForInput    ResponseState = "waiting_for_input"
	ResponseCompleted          ResponseState = "completed"
	ResponseFailed             ResponseState = "failed"
	ResponseCanceled           ResponseState = "canceled"
	ResponseTimedOut           ResponseState = "timed_out"
	ResponseBudgetExceeded     ResponseState = "budget_exceeded"
)

const (
	ResponseCmdProvision       ResponseCommand = "provision"
	ResponseCmdStart           ResponseCommand = "start"
	ResponseCmdRequestTool     ResponseCommand = "request_tool"
	ResponseCmdRequestApproval ResponseCommand = "request_approval"
	ResponseCmdRequestInput    ResponseCommand = "request_input"
	ResponseCmdResume          ResponseCommand = "resume"
	ResponseCmdComplete        ResponseCommand = "complete"
	ResponseCmdFail            ResponseCommand = "fail"
	ResponseCmdCancel          ResponseCommand = "cancel"
	ResponseCmdTimeout         ResponseCommand = "timeout"
	ResponseCmdExhaustBudget   ResponseCommand = "exhaust_budget"
)

// ResponseTable is the Response transition table (spec §8.3). A response is born
// queued. Each waiting_for_* state returns only to in_progress via resume.
// complete fires only from in_progress. exhaust_budget fires from in_progress and
// the three waiting states; timeout adds queued; cancel and fail add queued and
// provisioning (spec §20.12 queue-deadline).
var ResponseTable = []Transition[ResponseState, ResponseCommand]{
	{ResponseQueued, ResponseCmdProvision, ResponseProvisioning, "response.provisioning.v1"},
	{ResponseProvisioning, ResponseCmdStart, ResponseInProgress, "response.in_progress.v1"},

	{ResponseInProgress, ResponseCmdRequestTool, ResponseWaitingForTool, "response.waiting_for_tool.v1"},
	{ResponseInProgress, ResponseCmdRequestApproval, ResponseWaitingForApproval, "response.waiting_for_approval.v1"},
	{ResponseInProgress, ResponseCmdRequestInput, ResponseWaitingForInput, "response.waiting_for_input.v1"},

	{ResponseWaitingForTool, ResponseCmdResume, ResponseInProgress, "response.in_progress.v1"},
	{ResponseWaitingForApproval, ResponseCmdResume, ResponseInProgress, "response.in_progress.v1"},
	{ResponseWaitingForInput, ResponseCmdResume, ResponseInProgress, "response.in_progress.v1"},

	{ResponseInProgress, ResponseCmdComplete, ResponseCompleted, "response.completed.v1"},

	{ResponseQueued, ResponseCmdFail, ResponseFailed, "response.failed.v1"},
	{ResponseProvisioning, ResponseCmdFail, ResponseFailed, "response.failed.v1"},
	{ResponseInProgress, ResponseCmdFail, ResponseFailed, "response.failed.v1"},
	{ResponseWaitingForTool, ResponseCmdFail, ResponseFailed, "response.failed.v1"},
	{ResponseWaitingForApproval, ResponseCmdFail, ResponseFailed, "response.failed.v1"},
	{ResponseWaitingForInput, ResponseCmdFail, ResponseFailed, "response.failed.v1"},

	{ResponseQueued, ResponseCmdCancel, ResponseCanceled, "response.canceled.v1"},
	{ResponseProvisioning, ResponseCmdCancel, ResponseCanceled, "response.canceled.v1"},
	{ResponseInProgress, ResponseCmdCancel, ResponseCanceled, "response.canceled.v1"},
	{ResponseWaitingForTool, ResponseCmdCancel, ResponseCanceled, "response.canceled.v1"},
	{ResponseWaitingForApproval, ResponseCmdCancel, ResponseCanceled, "response.canceled.v1"},
	{ResponseWaitingForInput, ResponseCmdCancel, ResponseCanceled, "response.canceled.v1"},

	{ResponseQueued, ResponseCmdTimeout, ResponseTimedOut, "response.timed_out.v1"},
	{ResponseInProgress, ResponseCmdTimeout, ResponseTimedOut, "response.timed_out.v1"},
	{ResponseWaitingForTool, ResponseCmdTimeout, ResponseTimedOut, "response.timed_out.v1"},
	{ResponseWaitingForApproval, ResponseCmdTimeout, ResponseTimedOut, "response.timed_out.v1"},
	{ResponseWaitingForInput, ResponseCmdTimeout, ResponseTimedOut, "response.timed_out.v1"},

	{ResponseInProgress, ResponseCmdExhaustBudget, ResponseBudgetExceeded, "response.budget_exceeded.v1"},
	{ResponseWaitingForTool, ResponseCmdExhaustBudget, ResponseBudgetExceeded, "response.budget_exceeded.v1"},
	{ResponseWaitingForApproval, ResponseCmdExhaustBudget, ResponseBudgetExceeded, "response.budget_exceeded.v1"},
	{ResponseWaitingForInput, ResponseCmdExhaustBudget, ResponseBudgetExceeded, "response.budget_exceeded.v1"},
}
