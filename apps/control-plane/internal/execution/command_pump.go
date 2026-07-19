package execution

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/palgroup/palai/packages/coordinator"
)

// pumpCommands delivers a run's queued send_message commands at a safe boundary (spec §9.2,
// §22.4). It is the orchestrator's correlate-commit-dispatch role applied to commands: it never
// rewrites the engine loop — it reads the pending set, marks each command applied (durably,
// with the applied_sequence journaled in command.applied.v1), and sends the message.deliver
// frame the engine folds into the next model request.
//
// Deliver-once and fence discipline both fall out of ApplyCommand: its state transition is
// single-winner (WHERE state='queued'), so a redelivered boundary re-reads an already-drained
// set, and it runs under the run-terminal guard, so a canceled run's stale attempt cannot
// deliver — the same guard the model/tool commits use. A command another attempt already
// claimed returns ErrCommandNotPending and is skipped.
//
// ponytail: applied-but-not-folded message loss — after ApplyCommand commits, the delivered
// message lives only in the engine subprocess's memory until the next model request folds it,
// but run.start history is only prior-RESPONSE outputs, so a crash/reclaim between apply and
// fold gives the fresh attempt no redelivery and command.applied.v1 claims an effect that is
// lost. T4 pause/resume hits this on the NORMAL path (resume = new attempt from the journal),
// so the fix (a durable message row, or applied-undelivered redelivery at attempt start) is a
// hard T4 prerequisite, not a T2 concern.
func (o *Orchestrator) pumpCommands(ctx context.Context, st *attemptState) error {
	pending, err := o.spine.PendingSendMessageCommands(ctx, st.tenant, string(st.attempt.RunID))
	if err != nil {
		return err
	}
	for _, cmd := range pending {
		_, err := o.spine.ApplyCommand(ctx, st.tenant, st.sessionID, st.responseID, string(st.attempt.RunID), cmd.ID)
		if errors.Is(err, coordinator.ErrCommandNotPending) {
			continue // another boundary already delivered it
		}
		if err != nil {
			return err
		}
		// The applied_sequence is journaled in command.applied.v1 (ApplyCommand); the engine
		// only needs the message and its delivery mode to fold it in at the input boundary.
		frame := o.frame(st, "message.deliver", map[string]any{
			"command_id": cmd.ID,
			"delivery":   cmd.Delivery,
			"message":    decodeMessage(cmd.Payload),
		}, "")
		if err := st.ch.Send(ctx, frame); err != nil {
			return err
		}
	}
	return nil
}

// decodeMessage reads the send_message text from a command payload ({"message": "..."}). A
// malformed payload yields the empty string; the API validates a non-empty message at accept.
func decodeMessage(payload []byte) string {
	var body struct {
		Message string `json:"message"`
	}
	_ = json.Unmarshal(payload, &body)
	return body.Message
}
