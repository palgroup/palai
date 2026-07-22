package automation

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/palgroup/palai/storage"
)

// Trigger callbacks (spec §20.2.2, §21.6, §32.1, E11 Task 6). A callback is a POST-run delivery: once a
// triggered run reaches terminal, its response projection is shaped through the SAME bounded mapping
// language the input uses (no second language) and delivered to a registered webhook endpoint over T4's
// signed egress-safe pump — a normal webhook_deliveries row. The callback has its OWN terminal state
// (callback_state), so a callback that dead-letters never corrupts the run result (AUT-011 link-half).
//
// The sweep is the reconciler's third step (delivery_reconciler.go Tick), NOT a separate loop: (a) it is a
// trigger_deliveries remnant scan — the reconciler's job; (b) the HTTP half is already the supervised
// webhook-pump. So callback code lives here and the reconciler gains one coordination line.

// callbackDue is one run-terminal delivery whose callback is not yet armed — the sweep's unit of work.
type callbackDue struct {
	deliveryID    string
	org           string
	project       string
	sessionID     string
	responseID    string
	runID         string
	triggerID     string
	endpointID    string
	outputMapping []byte
	status        string
	output        []byte
}

// sweepCallbacks runs one callback pass: arm the callback of every run-terminal delivery whose revision
// names an endpoint (shape the output, enqueue a signed webhook delivery), then mirror the pump's terminal
// state back onto callback_state. Both halves are set-based / ON CONFLICT idempotent, so a re-sweep is a
// no-op. It is the reconciler's third Tick step. limit bounds the arm batch.
func (s *TriggerStore) sweepCallbacks(ctx context.Context, limit int, log func(string, ...any)) error {
	ctx = storage.WithSystemScope(ctx) // cross-tenant sweep: the catalogue query spans every tenant by construction
	if log == nil {
		log = func(string, ...any) {}
	}
	if limit <= 0 {
		limit = 100
	}
	if err := s.armDueCallbacks(ctx, limit, log); err != nil {
		return err
	}
	// Mirror the pump's terminal delivery state onto callback_state (pending → delivered/dead). A missed
	// mirror just resumes next tick — the webhook_deliveries row is the source of truth.
	if _, err := s.pool.Exec(ctx, storage.Query("MirrorCallbackState")); err != nil {
		return fmt.Errorf("mirror callback state: %w", err)
	}
	return nil
}

// armDueCallbacks shapes + enqueues the callback of each run-terminal delivery. A poison row (a shaping
// failure) is dead-lettered in place and skipped, never returned — one bad callback must not wedge the
// sweep behind a supervisor restart (the M2 remnant-sweep discipline).
func (s *TriggerStore) armDueCallbacks(ctx context.Context, limit int, log func(string, ...any)) error {
	ctx = storage.WithSystemScope(ctx) // cross-tenant sweep: the catalogue query spans every tenant by construction
	rows, err := s.pool.Query(ctx, storage.Query("CallbackDueDeliveries"), limit)
	if err != nil {
		return fmt.Errorf("scan callback-due deliveries: %w", err)
	}
	var due []callbackDue
	for rows.Next() {
		var d callbackDue
		if err := rows.Scan(&d.deliveryID, &d.org, &d.project, &d.sessionID, &d.responseID, &d.runID,
			&d.triggerID, &d.endpointID, &d.outputMapping, &d.status, &d.output); err != nil {
			rows.Close()
			return err
		}
		due = append(due, d)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}

	for _, d := range due {
		if err := s.armCallback(ctx, d); err != nil {
			log("delivery-reconciler: arm callback %s: %v", d.deliveryID, err)
		}
	}
	return nil
}

// armCallback shapes one delivery's callback output and enqueues a signed webhook delivery + marks the
// callback pending in ONE tx. A shaping failure (a schema-invalid output mapping) dead-letters the callback
// WITHOUT enqueuing — the run result stays intact; only the callback has its own dead terminal.
func (s *TriggerStore) armCallback(ctx context.Context, d callbackDue) error {
	mapping, err := CompileMapping(d.outputMapping, nil)
	if err != nil {
		// A revision that stored a bad output mapping would have been rejected at revise; treat a compile
		// error here as a callback failure, not a run failure.
		return s.deadCallback(ctx, d, "callback output mapping is invalid: "+err.Error())
	}
	shaped, err := mapping.Apply(callbackSource(d))
	if err != nil {
		return s.deadCallback(ctx, d, "callback output mapping failed: "+err.Error())
	}
	envelope := buildCallbackEnvelope("cb:"+d.deliveryID, d.deliveryID, shaped)

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	// The callback rides T4's outbound pump as a normal webhook_deliveries row. ON CONFLICT(endpoint_id,
	// event_id) makes a re-arm a no-op, so a crash between enqueue and the state mark never double-delivers.
	// ponytail: an enqueue error is left transient — logged and retried next sweep — NOT dead-lettered. The
	// one structural "endpoint gone" case (an FK violation on endpoint_id) is unreachable: the endpoint
	// cannot be deleted while a pinned revision references it (trigger_revisions.callback_endpoint_id is a
	// RESTRICT FK). Every other enqueue error is a transient DB blip that SHOULD retry; a persistent one
	// means the DB is down, which stalls the whole sweep, not just this callback. Add terminal-error
	// classification here only if a reachable non-retryable enqueue error ever appears.
	if _, err := tx.Exec(ctx, storage.Query("InsertDelivery"),
		newID("whd"), d.org, d.project, d.endpointID, d.sessionID, "cb:"+d.deliveryID, "trigger.callback.v1", envelope); err != nil {
		return fmt.Errorf("enqueue callback delivery: %w", err)
	}
	if _, err := tx.Exec(ctx, storage.Query("ArmDeliveryCallback"), d.deliveryID, d.org, d.project); err != nil {
		return fmt.Errorf("arm callback: %w", err)
	}
	return tx.Commit(ctx)
}

// deadCallback marks a callback dead (its own terminal) without enqueuing, recording why. The run result is
// untouched — the delivery state stays run_created.
func (s *TriggerStore) deadCallback(ctx context.Context, d callbackDue, reason string) error {
	if _, err := s.pool.Exec(ctx, storage.Query("DeadDeliveryCallback"), d.deliveryID, d.org, d.project, "callback: "+reason); err != nil {
		return fmt.Errorf("dead-letter callback: %w", err)
	}
	return nil
}

// callbackSource is the payload the output mapping is applied to: the terminal run's projection. The
// mapping selects from these fields (e.g. select "output", select "status") in the SAME language the input
// mapping uses.
func callbackSource(d callbackDue) map[string]any {
	var output any
	if len(d.output) > 0 {
		_ = json.Unmarshal(d.output, &output)
	}
	return map[string]any{
		"status":      d.status,
		"output":      output,
		"response_id": d.responseID,
		"run_id":      d.runID,
		"session_id":  d.sessionID,
		"trigger_id":  d.triggerID,
		"delivery_id": d.deliveryID,
	}
}

// buildCallbackEnvelope wraps the shaped output in the same CloudEvents-compatible envelope the webhook
// pump signs, typed trigger.callback.v1 (an externally-visible envelope type that is NEVER journaled — it
// bypasses fan-out and is delivered directly). data is the shaped output.
func buildCallbackEnvelope(eventID, deliveryID string, shaped []byte) []byte {
	var data any
	if len(shaped) > 0 {
		_ = json.Unmarshal(shaped, &data)
	}
	body, _ := json.Marshal(map[string]any{
		"specversion": "1.0",
		"id":          eventID,
		"type":        "trigger.callback.v1",
		"source":      "/v1/trigger-deliveries/" + deliveryID,
		"data":        data,
	})
	return body
}
