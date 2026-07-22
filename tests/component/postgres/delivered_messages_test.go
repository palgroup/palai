//go:build component

package postgres

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/palgroup/palai/packages/contracts"
	"github.com/palgroup/palai/packages/coordinator"
	statemachines "github.com/palgroup/palai/packages/state-machines"

	"github.com/palgroup/palai/storage"
)

// decodeSeededMessage reads the {"message": "..."} text a command payload carries.
func decodeSeededMessage(t *testing.T, payload []byte) string {
	t.Helper()
	var body struct {
		Message string `json:"message"`
	}
	if err := json.Unmarshal(payload, &body); err != nil {
		t.Fatalf("decode command payload %q error = %v", payload, err)
	}
	return body.Message
}

// seedQueuedSendMessage inserts a queued send_message command for a run — the boundary pump's input.
func seedQueuedSendMessage(t *testing.T, cs *coordinator.Store, tenant coordinator.Tenant, sessionID, runID, delivery, message string) string {
	t.Helper()
	cmdID := newID("cmd")
	exec(t, cs.Pool(),
		`INSERT INTO commands (id, organization_id, project_id, session_id, run_id, kind, delivery, payload, state)
		 VALUES ($1, $2, $3, $4, $5, 'send_message', $6, jsonb_build_object('message', $7::text), 'queued')`,
		cmdID, tenant.Organization, tenant.Project, sessionID, runID, delivery, message)
	return cmdID
}

type deliveredRow struct {
	boundary string
	seq      int64
	fold     string
}

func readDeliveredMessage(t *testing.T, cs *coordinator.Store, tenant coordinator.Tenant, commandID string) (deliveredRow, bool) {
	t.Helper()
	var r deliveredRow
	err := cs.Pool().QueryRow(storage.WithSystemScope(context.Background()),
		`SELECT coalesce(boundary_request_id, ''), applied_sequence, fold_state
		 FROM delivered_messages WHERE organization_id = $1 AND project_id = $2 AND command_id = $3`,
		tenant.Organization, tenant.Project, commandID).Scan(&r.boundary, &r.seq, &r.fold)
	if err != nil {
		return deliveredRow{}, false
	}
	return r, true
}

// TestApplyCommandRecordsDurableDeliveredMessageAtomically proves the E10 Task 2 write path (spec
// §26.9): applying a boundary send_message journals command.applied.v1 AND a durable
// delivered_messages row in one transaction, so an applied command always has its row (variant-1's
// "applied lie" is closed). The row carries the boundary the message folds at and the applied
// sequence; a re-apply is a single-winner no-op that leaves exactly one row.
func TestApplyCommandRecordsDurableDeliveredMessageAtomically(t *testing.T) {
	cs := openHarness(t)
	ctx := context.Background()
	tenant, sessionID, runID := seedRun(t, cs.Pool())
	cmdID := seedQueuedSendMessage(t, cs, tenant, sessionID, runID, "steer", "also do Y")

	seq, err := cs.ApplyCommand(ctx, tenant, sessionID, "", runID, cmdID, "mr_step2")
	if err != nil {
		t.Fatalf("ApplyCommand() error = %v", err)
	}
	if seq <= 0 {
		t.Fatalf("ApplyCommand() sequence = %d, want > 0", seq)
	}

	row, ok := readDeliveredMessage(t, cs, tenant, cmdID)
	if !ok {
		t.Fatal("applied command has no delivered_messages row — command.applied.v1 is a lie")
	}
	if row.boundary != "mr_step2" {
		t.Fatalf("delivered_messages.boundary_request_id = %q, want mr_step2", row.boundary)
	}
	if row.seq != seq {
		t.Fatalf("delivered_messages.applied_sequence = %d, want %d (the applied_sequence)", row.seq, seq)
	}
	if row.fold != "delivered" {
		t.Fatalf("delivered_messages.fold_state = %q, want delivered (not yet folded)", row.fold)
	}

	// Re-applying the same command is a single-winner no-op (WHERE state='queued'), so no second row.
	if _, err := cs.ApplyCommand(ctx, tenant, sessionID, "", runID, cmdID, "mr_step2"); err != coordinator.ErrCommandNotPending {
		t.Fatalf("re-ApplyCommand() error = %v, want ErrCommandNotPending", err)
	}
	var count int
	if err := cs.Pool().QueryRow(storage.WithSystemScope(ctx),
		`SELECT count(*) FROM delivered_messages WHERE command_id = $1`, cmdID).Scan(&count); err != nil {
		t.Fatalf("count delivered rows error = %v", err)
	}
	if count != 1 {
		t.Fatalf("delivered rows after re-apply = %d, want 1", count)
	}
}

// TestCommitModelResultFoldsDeliveredMessages proves the delivered -> folded transition (spec §26.9):
// a message delivered at a prior boundary is marked folded when the following model step commits, in
// the SAME transaction as the result. This is the honest record that separates variant-1 (crash
// before this commit) from R1 (crash after).
func TestCommitModelResultFoldsDeliveredMessages(t *testing.T) {
	cs := openHarness(t)
	ctx := context.Background()
	tenant, sessionID, runID := seedRun(t, cs.Pool())
	cmdID := seedQueuedSendMessage(t, cs, tenant, sessionID, runID, "queue", "also do Y")

	if _, err := cs.ApplyCommand(ctx, tenant, sessionID, "", runID, cmdID, "mr_step1"); err != nil {
		t.Fatalf("ApplyCommand() error = %v", err)
	}
	if row, _ := readDeliveredMessage(t, cs, tenant, cmdID); row.fold != "delivered" {
		t.Fatalf("before commit, fold_state = %q, want delivered", row.fold)
	}

	// The next model step commits: it folded the delivered message into the request it just answered.
	if err := cs.CommitModelRequest(ctx, tenant, sessionID, "", runID, "mr_step2", "model_step.created.v1", []byte(`{}`)); err != nil {
		t.Fatalf("CommitModelRequest() error = %v", err)
	}
	if _, err := cs.CommitModelResult(ctx, tenant, sessionID, "", runID, "mr_step2", []byte(`{"output":"ok"}`), "model_step.completed.v1", []byte(`{}`), contracts.Usage{}); err != nil {
		t.Fatalf("CommitModelResult() error = %v", err)
	}

	if row, _ := readDeliveredMessage(t, cs, tenant, cmdID); row.fold != "folded" {
		t.Fatalf("after commit, fold_state = %q, want folded", row.fold)
	}
}

// TestRedeliverBoundaryMessagesReturnsBoundaryRowsInCanonicalOrder proves the redelivery read (spec
// §26.9): the messages recorded at one boundary come back joined to their command's content/delivery,
// in applied_sequence order, and a different boundary returns none — so a fresh attempt refolds each
// message at exactly its original boundary.
func TestRedeliverBoundaryMessagesReturnsBoundaryRowsInCanonicalOrder(t *testing.T) {
	cs := openHarness(t)
	ctx := context.Background()
	tenant, sessionID, runID := seedRun(t, cs.Pool())

	// Two messages delivered at boundary mr_step2 (in order), one at mr_step5.
	firstID := seedQueuedSendMessage(t, cs, tenant, sessionID, runID, "queue", "first at 2")
	secondID := seedQueuedSendMessage(t, cs, tenant, sessionID, runID, "steer", "second at 2")
	otherID := seedQueuedSendMessage(t, cs, tenant, sessionID, runID, "queue", "at 5")
	if _, err := cs.ApplyCommand(ctx, tenant, sessionID, "", runID, firstID, "mr_step2"); err != nil {
		t.Fatalf("ApplyCommand(first) error = %v", err)
	}
	if _, err := cs.ApplyCommand(ctx, tenant, sessionID, "", runID, secondID, "mr_step2"); err != nil {
		t.Fatalf("ApplyCommand(second) error = %v", err)
	}
	if _, err := cs.ApplyCommand(ctx, tenant, sessionID, "", runID, otherID, "mr_step5"); err != nil {
		t.Fatalf("ApplyCommand(other) error = %v", err)
	}

	at2, err := cs.RedeliverBoundaryMessages(ctx, tenant, runID, "mr_step2")
	if err != nil {
		t.Fatalf("RedeliverBoundaryMessages(mr_step2) error = %v", err)
	}
	if len(at2) != 2 {
		t.Fatalf("boundary mr_step2 returned %d messages, want 2", len(at2))
	}
	if at2[0].CommandID != firstID || at2[1].CommandID != secondID {
		t.Fatalf("boundary mr_step2 order = [%s %s], want canonical [%s %s]", at2[0].CommandID, at2[1].CommandID, firstID, secondID)
	}
	if at2[0].Delivery != "queue" || at2[1].Delivery != "steer" {
		t.Fatalf("delivery modes = [%s %s], want [queue steer] (joined from the command)", at2[0].Delivery, at2[1].Delivery)
	}
	if got := decodeSeededMessage(t, at2[0].Payload); got != "first at 2" {
		t.Fatalf("first payload message = %q, want %q (content ref resolves)", got, "first at 2")
	}

	at5, err := cs.RedeliverBoundaryMessages(ctx, tenant, runID, "mr_step5")
	if err != nil {
		t.Fatalf("RedeliverBoundaryMessages(mr_step5) error = %v", err)
	}
	if len(at5) != 1 || at5[0].CommandID != otherID {
		t.Fatalf("boundary mr_step5 = %+v, want just %s", at5, otherID)
	}

	// A boundary with nothing recorded returns no rows (never a spurious redelivery).
	none, err := cs.RedeliverBoundaryMessages(ctx, tenant, runID, "mr_step9")
	if err != nil {
		t.Fatalf("RedeliverBoundaryMessages(mr_step9) error = %v", err)
	}
	if len(none) != 0 {
		t.Fatalf("empty boundary returned %d messages, want 0", len(none))
	}
}

// TestQueuedMessageOnTerminalStepNotLost pins the E10 T7 ENG-012 fork-3 behavior BY NAME (spec §22.4,
// §9.2): a send_message queued during a run that then TERMINATES on a step with no delivery boundary is
// NOT silently dropped. It stays queued (not expired like other commands), a warning.raised.v1 marks
// that it will carry, and CarrySessionSendMessages re-scopes it to the next run so that run's ordinary
// boundary pump delivers it at its first input boundary — no injection into the terminated response, no
// forced extra step (the rejected alternative). change_config's cross-run carry is unchanged.
func TestQueuedMessageOnTerminalStepNotLost(t *testing.T) {
	cs := openHarness(t)
	ctx := context.Background()
	pool := cs.Pool()
	tenant, sessionID, run1 := seedRun(t, pool)
	respID := newID("resp")
	exec(t, pool, `INSERT INTO responses (id, organization_id, project_id, session_id, state) VALUES ($1,$2,$3,$4,'in_progress')`,
		respID, tenant.Organization, tenant.Project, sessionID)
	exec(t, pool, `UPDATE runs SET state='running', response_id=$2 WHERE id=$1`, run1, respID)

	// A message queued mid-run, plus a non-carry command (a pause) that DOES expire on terminal.
	msgID := seedQueuedSendMessage(t, cs, tenant, sessionID, run1, "queue", "please also do Y")
	pauseID := newID("cmd")
	exec(t, pool, `INSERT INTO commands (id, organization_id, project_id, session_id, run_id, kind, state) VALUES ($1,$2,$3,$4,$5,'pause','queued')`,
		pauseID, tenant.Organization, tenant.Project, sessionID, run1)

	// The run terminates on a final step (no boundary pumped the message).
	if _, err := cs.ApplyRunTransition(ctx, tenant, run1, statemachines.RunCmdComplete); err != nil {
		t.Fatalf("ApplyRunTransition(complete) error = %v", err)
	}

	// The send_message SURVIVES queued (not lost); the pause expired (ordinary lifecycle).
	var msgState, pauseState string
	if err := pool.QueryRow(storage.WithSystemScope(ctx), `SELECT state FROM commands WHERE id=$1`, msgID).Scan(&msgState); err != nil {
		t.Fatalf("read send_message state error = %v", err)
	}
	if err := pool.QueryRow(storage.WithSystemScope(ctx), `SELECT state FROM commands WHERE id=$1`, pauseID).Scan(&pauseState); err != nil {
		t.Fatalf("read pause state error = %v", err)
	}
	if msgState != "queued" {
		t.Fatalf("send_message after terminal = %q, want queued (not lost, fork 3)", msgState)
	}
	if pauseState != "expired" {
		t.Fatalf("pause after terminal = %q, want expired (ordinary sweep)", pauseState)
	}
	// A warning.raised.v1 tells the user it will carry (not a silent drop).
	var warns int
	if err := pool.QueryRow(storage.WithSystemScope(ctx),
		`SELECT count(*) FROM events WHERE session_id=$1 AND type='warning.raised.v1' AND payload->>'code'='message_carried_to_next_response'`,
		sessionID).Scan(&warns); err != nil {
		t.Fatalf("count carry warnings error = %v", err)
	}
	if warns != 1 {
		t.Fatalf("carry warnings = %d, want 1 (the surviving message is visibly warned)", warns)
	}

	// The next response opens a fresh run; the carry re-scopes the queued message to it, so that run's
	// ordinary pump delivers it at its first input boundary.
	run2 := newID("run")
	exec(t, pool, `INSERT INTO runs (id, organization_id, project_id, session_id, state) VALUES ($1,$2,$3,$4,'running')`,
		run2, tenant.Organization, tenant.Project, sessionID)
	carried, err := cs.CarrySessionSendMessages(ctx, tenant, sessionID, run2)
	if err != nil {
		t.Fatalf("CarrySessionSendMessages error = %v", err)
	}
	if carried != 1 {
		t.Fatalf("carried %d messages, want 1", carried)
	}
	var carriedRun, carriedState string
	if err := pool.QueryRow(storage.WithSystemScope(ctx), `SELECT run_id, state FROM commands WHERE id=$1`, msgID).Scan(&carriedRun, &carriedState); err != nil {
		t.Fatalf("read carried message error = %v", err)
	}
	if carriedRun != run2 || carriedState != "queued" {
		t.Fatalf("carried message = {run:%q state:%q}, want {%s queued} (deliverable by run2's pump)", carriedRun, carriedState, run2)
	}
	// The pending set for run2 now includes the carried message — the ordinary boundary pump will deliver it.
	pending, err := cs.PendingBoundaryCommands(ctx, tenant, run2)
	if err != nil {
		t.Fatalf("PendingBoundaryCommands(run2) error = %v", err)
	}
	if len(pending) != 1 || pending[0].ID != msgID {
		t.Fatalf("run2 pending = %+v, want the carried message %s", pending, msgID)
	}
}

// TestInterruptDeliveryDurableAcrossReclaim proves the ENG-012 interrupt half (spec §26.9, E10 T7): an
// interrupt-delivered message used to live ONLY in the engine subprocess's memory (InterruptModelStep
// wrote no delivered_messages row), so a crash after the fold dropped it — the command drained
// single-winner, run.start carries prior responses only, nothing redelivered it. InterruptModelStep now
// journals the SAME durable row keyed by the aborted step's boundary, so a reclaiming attempt redelivers
// it exactly once at that boundary — and interleaves with a boundary-delivered message by
// applied_sequence (never inside the reconstructed step). A re-interrupt is a single-winner no-op.
func TestInterruptDeliveryDurableAcrossReclaim(t *testing.T) {
	cs := openHarness(t)
	ctx := context.Background()
	tenant, sessionID, runID := seedRun(t, cs.Pool())

	// A message queued during the outage and delivered by the in-flight-abort watcher (an interrupt
	// fold), aborting model step mr_step2.
	interruptID := seedQueuedSendMessage(t, cs, tenant, sessionID, runID, "interrupt", "stop and do Z")
	iseq, err := cs.InterruptModelStep(ctx, tenant, sessionID, "", runID, interruptID, "mr_step2", "model_step.interrupted.v1", []byte(`{"output":"partial"}`))
	if err != nil {
		t.Fatalf("InterruptModelStep() error = %v", err)
	}
	if iseq <= 0 {
		t.Fatalf("InterruptModelStep() sequence = %d, want > 0", iseq)
	}

	// The interrupt fold is now DURABLE: a delivered_messages row keyed by the aborted step's boundary.
	row, ok := readDeliveredMessage(t, cs, tenant, interruptID)
	if !ok {
		t.Fatal("interrupt fold wrote no delivered_messages row — the ENG-012 outage loss is not closed")
	}
	if row.boundary != "mr_step2" || row.seq != iseq {
		t.Fatalf("interrupt delivered row = {boundary:%q seq:%d}, want {mr_step2 %d}", row.boundary, row.seq, iseq)
	}

	// A boundary-delivered message applied at the SAME step interleaves with the interrupt one by
	// applied_sequence: the interrupt folded first (lower seq), the boundary message after.
	boundaryID := seedQueuedSendMessage(t, cs, tenant, sessionID, runID, "queue", "then do Y at boundary")
	if _, err := cs.ApplyCommand(ctx, tenant, sessionID, "", runID, boundaryID, "mr_step2"); err != nil {
		t.Fatalf("ApplyCommand(boundary) error = %v", err)
	}

	redeliver, err := cs.RedeliverBoundaryMessages(ctx, tenant, runID, "mr_step2")
	if err != nil {
		t.Fatalf("RedeliverBoundaryMessages(mr_step2) error = %v", err)
	}
	if len(redeliver) != 2 {
		t.Fatalf("boundary mr_step2 redelivered %d messages, want 2 (interrupt + boundary interleaved)", len(redeliver))
	}
	if redeliver[0].CommandID != interruptID || redeliver[1].CommandID != boundaryID {
		t.Fatalf("redelivery order = [%s %s], want [interrupt %s, boundary %s] by applied_sequence",
			redeliver[0].CommandID, redeliver[1].CommandID, interruptID, boundaryID)
	}
	if redeliver[0].Delivery != "interrupt" {
		t.Fatalf("interrupt-delivered redelivery mode = %q, want interrupt (joined from the command)", redeliver[0].Delivery)
	}
	if got := decodeSeededMessage(t, redeliver[0].Payload); got != "stop and do Z" {
		t.Fatalf("interrupt payload message = %q, want %q (content ref resolves)", got, "stop and do Z")
	}

	// A re-interrupt (the reclaim re-walks the boundary) is a single-winner no-op: the command already
	// applied, so ErrCommandNotPending and no second durable row.
	if _, err := cs.InterruptModelStep(ctx, tenant, sessionID, "", runID, interruptID, "mr_step2", "model_step.interrupted.v1", []byte(`{}`)); err != coordinator.ErrCommandNotPending {
		t.Fatalf("re-InterruptModelStep() error = %v, want ErrCommandNotPending", err)
	}
	var count int
	if err := cs.Pool().QueryRow(storage.WithSystemScope(ctx), `SELECT count(*) FROM delivered_messages WHERE command_id = $1`, interruptID).Scan(&count); err != nil {
		t.Fatalf("count interrupt delivered rows error = %v", err)
	}
	if count != 1 {
		t.Fatalf("interrupt delivered rows after re-interrupt = %d, want 1 (single-winner)", count)
	}
}
