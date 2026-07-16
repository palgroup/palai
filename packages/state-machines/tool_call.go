package statemachines

// ToolCallState is a tool-call lifecycle state (spec §26.7).
type ToolCallState string

// ToolCallCommand drives a tool-call state transition (spec §26.7).
type ToolCallCommand string

const (
	ToolCallProposed             ToolCallState = "proposed"
	ToolCallPolicyCheck          ToolCallState = "policy_check"
	ToolCallApprovalPending      ToolCallState = "approval_pending"
	ToolCallReady                ToolCallState = "ready"
	ToolCallLeased               ToolCallState = "leased"
	ToolCallExecuting            ToolCallState = "executing"
	ToolCallCompleted            ToolCallState = "completed"
	ToolCallFailed               ToolCallState = "failed"
	ToolCallCanceled             ToolCallState = "canceled"
	ToolCallUncertain            ToolCallState = "uncertain"
	ToolCallReconciledCompleted  ToolCallState = "reconciled_completed"
	ToolCallReconciledNotApplied ToolCallState = "reconciled_not_applied"
	ToolCallManualResolution     ToolCallState = "manual_resolution"
)

const (
	ToolCallCmdCheckPolicy         ToolCallCommand = "check_policy"
	ToolCallCmdRequireApproval     ToolCallCommand = "require_approval"
	ToolCallCmdApprove             ToolCallCommand = "approve"
	ToolCallCmdMarkReady           ToolCallCommand = "mark_ready"
	ToolCallCmdLease               ToolCallCommand = "lease"
	ToolCallCmdExecute             ToolCallCommand = "execute"
	ToolCallCmdComplete            ToolCallCommand = "complete"
	ToolCallCmdFail                ToolCallCommand = "fail"
	ToolCallCmdCancel              ToolCallCommand = "cancel"
	ToolCallCmdMarkUncertain       ToolCallCommand = "mark_uncertain"
	ToolCallCmdReconcileCompleted  ToolCallCommand = "reconcile_completed"
	ToolCallCmdReconcileNotApplied ToolCallCommand = "reconcile_not_applied"
	ToolCallCmdEscalate            ToolCallCommand = "escalate"
)

// ToolCallTable is the tool-call transition table (spec §26.7). policy_check
// reaches ready directly via mark_ready, or through approval_pending via
// require_approval then approve. executing resolves to completed, failed, or
// uncertain; an uncertain call must be reconciled to reconciled_completed,
// reconciled_not_applied, or manual_resolution. cancel fires from every
// non-terminal state on the proposed→executing path (uncertain excluded).
var ToolCallTable = []Transition[ToolCallState, ToolCallCommand]{
	{ToolCallProposed, ToolCallCmdCheckPolicy, ToolCallPolicyCheck, "tool_call.policy_check.v1"},
	{ToolCallPolicyCheck, ToolCallCmdRequireApproval, ToolCallApprovalPending, "tool_call.approval_pending.v1"},
	{ToolCallApprovalPending, ToolCallCmdApprove, ToolCallReady, "tool_call.ready.v1"},
	{ToolCallPolicyCheck, ToolCallCmdMarkReady, ToolCallReady, "tool_call.ready.v1"},
	{ToolCallReady, ToolCallCmdLease, ToolCallLeased, "tool_call.leased.v1"},
	{ToolCallLeased, ToolCallCmdExecute, ToolCallExecuting, "tool_call.executing.v1"},

	{ToolCallExecuting, ToolCallCmdComplete, ToolCallCompleted, "tool_call.completed.v1"},
	{ToolCallExecuting, ToolCallCmdFail, ToolCallFailed, "tool_call.failed.v1"},
	{ToolCallExecuting, ToolCallCmdMarkUncertain, ToolCallUncertain, "tool_call.uncertain.v1"},

	{ToolCallUncertain, ToolCallCmdReconcileCompleted, ToolCallReconciledCompleted, "tool_call.reconciled_completed.v1"},
	{ToolCallUncertain, ToolCallCmdReconcileNotApplied, ToolCallReconciledNotApplied, "tool_call.reconciled_not_applied.v1"},
	{ToolCallUncertain, ToolCallCmdEscalate, ToolCallManualResolution, "tool_call.manual_resolution.v1"},

	{ToolCallProposed, ToolCallCmdCancel, ToolCallCanceled, "tool_call.canceled.v1"},
	{ToolCallPolicyCheck, ToolCallCmdCancel, ToolCallCanceled, "tool_call.canceled.v1"},
	{ToolCallApprovalPending, ToolCallCmdCancel, ToolCallCanceled, "tool_call.canceled.v1"},
	{ToolCallReady, ToolCallCmdCancel, ToolCallCanceled, "tool_call.canceled.v1"},
	{ToolCallLeased, ToolCallCmdCancel, ToolCallCanceled, "tool_call.canceled.v1"},
	{ToolCallExecuting, ToolCallCmdCancel, ToolCallCanceled, "tool_call.canceled.v1"},
}

// SuccessfulToolStates reports the terminal tool-call states whose results enter
// context as successful (spec §26.7): only completed and reconciled_completed.
func SuccessfulToolStates() map[ToolCallState]bool {
	return map[ToolCallState]bool{
		ToolCallCompleted:           true,
		ToolCallReconciledCompleted: true,
	}
}
