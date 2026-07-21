package statemachines

// TriggerDeliveryState is a TriggerDelivery lifecycle state (spec §20.2.2, E11 Task 2). A delivery is
// born `received` and advances received → authenticated → deduplicated → mapped → admitted → run_created,
// with rejected/duplicate/failed/deferred/skipped branches.
type TriggerDeliveryState string

// TriggerDeliveryCommand drives a TriggerDelivery state transition.
type TriggerDeliveryCommand string

const (
	TriggerDeliveryReceived      TriggerDeliveryState = "received"
	TriggerDeliveryAuthenticated TriggerDeliveryState = "authenticated"
	TriggerDeliveryDeduplicated  TriggerDeliveryState = "deduplicated"
	TriggerDeliveryMapped        TriggerDeliveryState = "mapped"
	TriggerDeliveryAdmitted      TriggerDeliveryState = "admitted"
	TriggerDeliveryRunCreated    TriggerDeliveryState = "run_created"
	TriggerDeliveryRejected      TriggerDeliveryState = "rejected"
	TriggerDeliveryDuplicate     TriggerDeliveryState = "duplicate"
	TriggerDeliveryFailed        TriggerDeliveryState = "failed"
	TriggerDeliveryDeferred      TriggerDeliveryState = "deferred"
	TriggerDeliverySkipped       TriggerDeliveryState = "skipped"
)

const (
	TriggerDeliveryCmdAuthenticate  TriggerDeliveryCommand = "authenticate"
	TriggerDeliveryCmdReject        TriggerDeliveryCommand = "reject"
	TriggerDeliveryCmdDeduplicate   TriggerDeliveryCommand = "deduplicate"
	TriggerDeliveryCmdMarkDuplicate TriggerDeliveryCommand = "mark_duplicate"
	TriggerDeliveryCmdMap           TriggerDeliveryCommand = "map"
	TriggerDeliveryCmdFail          TriggerDeliveryCommand = "fail"
	TriggerDeliveryCmdAdmit         TriggerDeliveryCommand = "admit"
	TriggerDeliveryCmdDefer         TriggerDeliveryCommand = "defer"
	TriggerDeliveryCmdSkip          TriggerDeliveryCommand = "skip"
	TriggerDeliveryCmdCreateRun     TriggerDeliveryCommand = "create_run"
)

// TriggerDeliveryTable is the TriggerDelivery transition table (spec §20.2.2). The happy path is
// authenticate → deduplicate → map → admit → create_run. A failed authentication rejects; a dedupe hit
// marks the delivery a duplicate (linked to its canonical original, AUT-001); a mapping-schema failure
// fails WITHOUT a run (AUT-003 — no billable run). At the concurrency gate a mapped delivery either
// admits, defers (queue/singleton — resumed by the delivery-reconciler), skips, or rejects (per policy).
//
// CONSCIOUS DECISION — `skipped` is a SEPARATE terminal state (AUT-005 honest naming). drop_if_running
// and a coalesce-subsumed delivery RECORD `skipped` with a reason; burying that in `rejected` would
// conflate a policy skip (nothing was wrong) with a rejection (auth/policy denial), so the two stay
// distinct terminals.
//
// `deferred` is NOT terminal: the reconciler resumes it (deferred → admit) once its gate opens, or skips
// it (deferred → skip) when a coalesce survivor subsumes it.
var TriggerDeliveryTable = []Transition[TriggerDeliveryState, TriggerDeliveryCommand]{
	{TriggerDeliveryReceived, TriggerDeliveryCmdAuthenticate, TriggerDeliveryAuthenticated, "trigger.delivery.authenticated.v1"},
	{TriggerDeliveryReceived, TriggerDeliveryCmdReject, TriggerDeliveryRejected, "trigger.delivery.rejected.v1"},

	{TriggerDeliveryAuthenticated, TriggerDeliveryCmdDeduplicate, TriggerDeliveryDeduplicated, "trigger.delivery.deduplicated.v1"},
	{TriggerDeliveryAuthenticated, TriggerDeliveryCmdMarkDuplicate, TriggerDeliveryDuplicate, "trigger.delivery.duplicate.v1"},

	{TriggerDeliveryDeduplicated, TriggerDeliveryCmdMap, TriggerDeliveryMapped, "trigger.delivery.mapped.v1"},
	{TriggerDeliveryDeduplicated, TriggerDeliveryCmdFail, TriggerDeliveryFailed, "trigger.delivery.failed.v1"},

	{TriggerDeliveryMapped, TriggerDeliveryCmdAdmit, TriggerDeliveryAdmitted, "trigger.delivery.admitted.v1"},
	{TriggerDeliveryMapped, TriggerDeliveryCmdDefer, TriggerDeliveryDeferred, "trigger.delivery.deferred.v1"},
	{TriggerDeliveryMapped, TriggerDeliveryCmdSkip, TriggerDeliverySkipped, "trigger.delivery.skipped.v1"},
	{TriggerDeliveryMapped, TriggerDeliveryCmdReject, TriggerDeliveryRejected, "trigger.delivery.rejected.v1"},
	{TriggerDeliveryMapped, TriggerDeliveryCmdFail, TriggerDeliveryFailed, "trigger.delivery.failed.v1"},

	{TriggerDeliveryDeferred, TriggerDeliveryCmdAdmit, TriggerDeliveryAdmitted, "trigger.delivery.admitted.v1"},
	{TriggerDeliveryDeferred, TriggerDeliveryCmdSkip, TriggerDeliverySkipped, "trigger.delivery.skipped.v1"},

	{TriggerDeliveryAdmitted, TriggerDeliveryCmdCreateRun, TriggerDeliveryRunCreated, "trigger.delivery.run_created.v1"},
	{TriggerDeliveryAdmitted, TriggerDeliveryCmdFail, TriggerDeliveryFailed, "trigger.delivery.failed.v1"},
}
