package statemachines

import (
	"errors"
	"testing"
)

// TestTriggerDeliveryTransitions pins the TriggerDelivery table's happy path and its documented
// branches (spec §20.2.2, AUT-001/003/005). The exhaustive terminal-monotonicity + one-event-per-row
// properties are covered by the shared property suite (allTables registers this table); here we assert
// the specific edges the ingestion pipeline relies on.
func TestTriggerDeliveryTransitions(t *testing.T) {
	step := func(from TriggerDeliveryState, cmd TriggerDeliveryCommand, wantTo TriggerDeliveryState, wantEvent string) {
		t.Helper()
		to, event, err := Apply(from, cmd, TriggerDeliveryTable)
		if err != nil {
			t.Fatalf("Apply(%s, %s) error = %v", from, cmd, err)
		}
		if to != wantTo || event != wantEvent {
			t.Fatalf("Apply(%s, %s) = (%s, %q), want (%s, %q)", from, cmd, to, event, wantTo, wantEvent)
		}
	}

	// Happy path: received → authenticated → deduplicated → mapped → admitted → run_created.
	step(TriggerDeliveryReceived, TriggerDeliveryCmdAuthenticate, TriggerDeliveryAuthenticated, "trigger.delivery.authenticated.v1")
	step(TriggerDeliveryAuthenticated, TriggerDeliveryCmdDeduplicate, TriggerDeliveryDeduplicated, "trigger.delivery.deduplicated.v1")
	step(TriggerDeliveryDeduplicated, TriggerDeliveryCmdMap, TriggerDeliveryMapped, "trigger.delivery.mapped.v1")
	step(TriggerDeliveryMapped, TriggerDeliveryCmdAdmit, TriggerDeliveryAdmitted, "trigger.delivery.admitted.v1")
	step(TriggerDeliveryAdmitted, TriggerDeliveryCmdCreateRun, TriggerDeliveryRunCreated, "trigger.delivery.run_created.v1")

	// Branches: dedupe hit → duplicate (AUT-001); mapping schema failure → failed WITHOUT a run
	// (AUT-003); the concurrency gate → deferred, then resumed to admitted by the reconciler (AUT-004);
	// a drop_if_running / coalesce-subsumed delivery → skipped (AUT-005 honest naming).
	step(TriggerDeliveryAuthenticated, TriggerDeliveryCmdMarkDuplicate, TriggerDeliveryDuplicate, "trigger.delivery.duplicate.v1")
	step(TriggerDeliveryDeduplicated, TriggerDeliveryCmdFail, TriggerDeliveryFailed, "trigger.delivery.failed.v1")
	step(TriggerDeliveryMapped, TriggerDeliveryCmdDefer, TriggerDeliveryDeferred, "trigger.delivery.deferred.v1")
	step(TriggerDeliveryDeferred, TriggerDeliveryCmdAdmit, TriggerDeliveryAdmitted, "trigger.delivery.admitted.v1")
	step(TriggerDeliveryMapped, TriggerDeliveryCmdSkip, TriggerDeliverySkipped, "trigger.delivery.skipped.v1")
	step(TriggerDeliveryDeferred, TriggerDeliveryCmdSkip, TriggerDeliverySkipped, "trigger.delivery.skipped.v1")
	step(TriggerDeliveryMapped, TriggerDeliveryCmdReject, TriggerDeliveryRejected, "trigger.delivery.rejected.v1")

	// A terminal rejects every command.
	if _, _, err := Apply(TriggerDeliveryRunCreated, TriggerDeliveryCmdCreateRun, TriggerDeliveryTable); !errors.Is(err, ErrInvalidState) {
		t.Fatalf("terminal run_created accepted create_run, want ErrInvalidState (got %v)", err)
	}
}
