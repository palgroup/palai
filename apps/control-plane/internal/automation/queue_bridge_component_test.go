//go:build component

// Component tests for the E19 T6 queue BRIDGES against REAL PostgreSQL + the REAL coordinator admission
// spine. E17 T7 already proved the queue ADAPTER's durable-delivery contract (redelivery, backpressure,
// dead-letter, loss-less outbox) against a scratch side effect; these prove the same cruxes when the side
// effect is an actual RUN, plus the two the adapter tier could not reach:
//
//	AUT-009  a lost-ack redelivery admits ONE run (not one scratch row) — the admission's own idempotency
//	         reservation is the anchor, and it holds even when the receipt is lost
//	AUT-010  a flood applies backpressure and reports depth; NOTHING is dropped and every published
//	         message eventually becomes exactly one run
//	AUT-013  the bridge adds no retry of its own — the queue is the single retry owner
//	§34.3    a poison message dead-letters instead of blocking the stream
//	§34.5    a run's terminal transaction commits the outbound delivery, so a down publisher loses nothing
//	         and the recovered publisher delivers exactly once
//	§2/§38.6 a payload cannot select a tenant or a run target, and a DISABLED connection admits nothing
//
// Honest ceiling: no broker PRODUCT runs here (E17 §6 leg 5). The reference adapter's tables ARE the
// broker, so what is proven is the BRIDGE over durable Postgres, not SQS/PubSub/Kafka semantics.
package automation

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/palgroup/palai/adapters/integrations/queue"
	"github.com/palgroup/palai/packages/coordinator"
	statemachines "github.com/palgroup/palai/packages/state-machines"
	"github.com/palgroup/palai/storage"
)

// --- harness ---

// seedQueuePublishedRevision creates and publishes an agent revision in scope — the run target an inbound
// connection's config must pin (the bridge fails closed without one).
func seedQueuePublishedRevision(t *testing.T, pool *pgxpool.Pool, org, project string) string {
	t.Helper()
	ctx := context.Background()
	agents := New(pool)
	profileID, err := agents.CreateProfile(ctx, org, project, randID("profile"))
	if err != nil {
		t.Fatalf("CreateProfile error = %v", err)
	}
	rev, err := agents.CreateRevision(ctx, org, project, profileID, []byte(`{"model":"gpt-4o-mini","instructions":"handle the queued message"}`))
	if err != nil {
		t.Fatalf("CreateRevision error = %v", err)
	}
	if _, _, err := agents.PublishRevision(ctx, org, project, rev.ID); err != nil {
		t.Fatalf("PublishRevision error = %v", err)
	}
	return rev.ID
}

// wiredQueueBridge returns a bridge over the real spine, plus the spine and pool for assertions.
func wiredQueueBridge(t *testing.T, cfg QueueBridgeConfig) (*QueueBridge, *QueueStore, *coordinator.Store, *pgxpool.Pool) {
	t.Helper()
	spine := componentSpine(t)
	store := NewQueueStore(spine.Pool())
	return NewQueueBridge(store, spine, cfg, t.Logf), store, spine, spine.Pool()
}

// inboundConnConfig is the run target an inbound binding pins, in the shape the admin surface writes.
func inboundConnConfig(revision, principal string) []byte {
	return []byte(fmt.Sprintf(`{"agent_revision_id":%q,"principal_id":%q}`, revision, principal))
}

// queueEnvelope is a §21.7 inbound envelope as a producer would publish it. sourceTenant is the field that
// MUST be inert — it is what a hostile producer would use to try to move the run to another tenant.
func queueEnvelope(source, sourceTenant, eventID, data string) []byte {
	body, _ := json.Marshal(map[string]any{
		"source": source, "source_tenant": sourceTenant, "source_event_id": eventID,
		"data": json.RawMessage(data),
	})
	return body
}

// sweptConn re-reads a connection through the bridge's OWN catalogue scan, so a test drives exactly the
// row the production loop would. A connection missing from the sweep is a real outcome (disabled).
func sweptConn(t *testing.T, store *QueueStore, direction, connID string) (queueConn, bool) {
	t.Helper()
	conns, err := store.sweepConnections(context.Background(), direction)
	if err != nil {
		t.Fatalf("sweepConnections error = %v", err)
	}
	for _, c := range conns {
		if c.id == connID {
			return c, true
		}
	}
	return queueConn{}, false
}

// countRuns counts the responses born in a tenant — the "how many runs did this produce" assertion.
func countRuns(t *testing.T, pool *pgxpool.Pool, org, project string) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(storage.ScopeToTenant(context.Background(), org, project),
		`SELECT count(*) FROM responses WHERE organization_id = $1 AND project_id = $2`, org, project).Scan(&n); err != nil {
		t.Fatalf("count runs error = %v", err)
	}
	return n
}

// runPinnedRevision reads the agent revision the single run in scope was pinned to.
func runPinnedRevision(t *testing.T, pool *pgxpool.Pool, org, project string) string {
	t.Helper()
	var rev *string
	if err := pool.QueryRow(storage.ScopeToTenant(context.Background(), org, project),
		`SELECT agent_revision_id FROM runs WHERE organization_id = $1 AND project_id = $2`, org, project).Scan(&rev); err != nil {
		t.Fatalf("read run revision error = %v", err)
	}
	if rev == nil {
		return ""
	}
	return *rev
}

func queueMessageState(t *testing.T, pool *pgxpool.Pool, org, project, connID, key string) string {
	t.Helper()
	var state string
	if err := pool.QueryRow(storage.ScopeToTenant(context.Background(), org, project),
		`SELECT state FROM queue_messages WHERE queue_connection_id = $1 AND idempotency_key = $2`, connID, key).Scan(&state); err != nil {
		t.Fatalf("read queue message state error = %v", err)
	}
	return state
}

// --- the guarantees ---

// TestQueueBridgeLostAckRedeliversToExactlyOneRun is the AUT-009/AUT-013 WIRED leg. The first delivery
// admits a run and then its ACK IS LOST (the disposition never reaches the broker), which is the exact
// crash window the §34.2 ack ordering exists for. The lease expires, the REAL bridge redelivers — and the
// admission's idempotency reservation collapses it onto the same response instead of minting a second run.
//
// This is stronger than the adapter-tier leg: there, an in-transaction receipt made the effect single. Here
// the receipt from the first attempt is NOT what saves us (the run is born before it), the admission's own
// key is — which is the property the bridge deliberately relies on and must therefore be pinned.
func TestQueueBridgeLostAckRedeliversToExactlyOneRun(t *testing.T) {
	bridge, store, _, pool := wiredQueueBridge(t, QueueBridgeConfig{})
	ctx := context.Background()
	org, project, _ := seedSession(t, pool)
	principal := seedPrincipal(t, pool, org, project)
	revision := seedQueuePublishedRevision(t, pool, org, project)
	connID := mustCreateQueueConn(t, store, org, project, QueueConnectionInput{
		Name: "orders", Direction: "inbound", Config: inboundConnConfig(revision, principal)})

	q, err := store.InboundQueue(ctx, org, project, connID)
	if err != nil {
		t.Fatalf("InboundQueue error = %v", err)
	}
	if err := q.Publish(ctx, "msg-1", queueEnvelope("orders.v1", "", "evt-1", `{"order":7}`)); err != nil {
		t.Fatalf("Publish error = %v", err)
	}

	c, ok := sweptConn(t, store, "inbound", connID)
	if !ok {
		t.Fatal("the enabled inbound connection is missing from the bridge sweep")
	}
	target, err := bridge.runTarget(ctx, c)
	if err != nil {
		t.Fatalf("runTarget error = %v", err)
	}

	// Attempt 1: the bridge's real handler runs (run + receipt commit) but the ACK is lost.
	lossy := func(ctx context.Context, m queue.Message) (queue.Disposition, error) {
		disp, err := bridge.handler(c, target)(ctx, m)
		if err == nil && disp == queue.Ack {
			return queue.Retry, nil // the ack never reached the broker
		}
		return disp, err
	}
	if n, err := store.inboundQueueFor(c).Consume(ctx, 10, lossy); err != nil || n != 1 {
		t.Fatalf("first Consume = (%d, %v), want (1, nil)", n, err)
	}
	if got := countRuns(t, pool, org, project); got != 1 {
		t.Fatalf("runs after the first delivery = %d, want 1", got)
	}

	// The un-acked message's visibility lease expires: a crashed consumer's message redelivers.
	expireLease(t, pool, connID)
	if err := bridge.Tick(ctx); err != nil {
		t.Fatalf("bridge Tick error = %v", err)
	}

	if got := countRuns(t, pool, org, project); got != 1 {
		t.Fatalf("runs after the redelivery = %d, want 1 — the redelivery must REPLAY, not admit a second run", got)
	}
	if got := queueMessageState(t, pool, org, project, connID, "msg-1"); got != "acked" {
		t.Fatalf("message state after the redelivery = %q, want acked", got)
	}
	if got := countReceipts(t, pool, org, project, connID, "msg-1"); got != 1 {
		t.Fatalf("idempotency receipts = %d, want 1 (append-only, one per key)", got)
	}
}

// TestQueueBridgeFloodBackpressureNoDropOneRunEach is the AUT-010 WIRED leg: at capacity the producer is
// told to back off (ErrQueueFull) instead of the queue silently growing or dropping, the depth gauge
// reports the backlog an operator would read, and every message that WAS accepted becomes exactly one run.
// Backpressure applied, nothing lost.
func TestQueueBridgeFloodBackpressureNoDropOneRunEach(t *testing.T) {
	bridge, store, _, pool := wiredQueueBridge(t, QueueBridgeConfig{Batch: 4})
	ctx := context.Background()
	org, project, _ := seedSession(t, pool)
	principal := seedPrincipal(t, pool, org, project)
	revision := seedQueuePublishedRevision(t, pool, org, project)
	const capacity = 5
	connID := mustCreateQueueConn(t, store, org, project, QueueConnectionInput{
		Name: "flood", Direction: "inbound", Capacity: capacity, Config: inboundConnConfig(revision, principal)})

	q, err := store.InboundQueue(ctx, org, project, connID)
	if err != nil {
		t.Fatalf("InboundQueue error = %v", err)
	}
	accepted, shed := 0, 0
	for i := 0; i < capacity*3; i++ {
		err := q.Publish(ctx, fmt.Sprintf("flood-%d", i), queueEnvelope("flood.v1", "", fmt.Sprintf("evt-%d", i), `{}`))
		switch {
		case err == nil:
			accepted++
		case err == queue.ErrQueueFull:
			shed++
		default:
			t.Fatalf("Publish error = %v", err)
		}
	}
	if shed == 0 {
		t.Fatal("no publish was shed: the bounded buffer never applied backpressure")
	}
	if accepted > capacity {
		t.Fatalf("accepted %d messages over a capacity of %d — the ceiling did not hold", accepted, capacity)
	}
	depth, err := q.Depth(ctx)
	if err != nil {
		t.Fatalf("Depth error = %v", err)
	}
	if depth.Ready != accepted {
		t.Fatalf("depth gauge reports %d ready, want the %d accepted (nothing dropped)", depth.Ready, accepted)
	}

	// Drain: Batch is 4 and capacity 5, so this deliberately takes more than one tick — the bounded batch
	// is the second half of "bounded buffer", and a bridge that only drained one batch would leave a
	// backlog that never clears.
	for i := 0; i < 5; i++ {
		if err := bridge.Tick(ctx); err != nil {
			t.Fatalf("bridge Tick error = %v", err)
		}
	}
	if got := countRuns(t, pool, org, project); got != accepted {
		t.Fatalf("runs = %d, want %d (one per accepted message: none dropped, none duplicated)", got, accepted)
	}
	depth, err = q.Depth(ctx)
	if err != nil {
		t.Fatalf("Depth error = %v", err)
	}
	if depth.Ready != 0 || depth.InFlight != 0 || depth.Dead != 0 {
		t.Fatalf("depth after the drain = %+v, want empty", depth)
	}
}

// TestQueueBridgeDeadLettersPoison is the §34.3 leg: a body that can never normalize is retired to the
// dead-letter view on its FIRST delivery — not redelivered until it exhausts the bound, and not silently
// dropped — while the healthy message behind it still becomes a run. A poison message must not block the
// stream.
func TestQueueBridgeDeadLettersPoison(t *testing.T) {
	bridge, store, _, pool := wiredQueueBridge(t, QueueBridgeConfig{})
	ctx := context.Background()
	org, project, _ := seedSession(t, pool)
	principal := seedPrincipal(t, pool, org, project)
	revision := seedQueuePublishedRevision(t, pool, org, project)
	connID := mustCreateQueueConn(t, store, org, project, QueueConnectionInput{
		Name: "poison", Direction: "inbound", Config: inboundConnConfig(revision, principal)})

	q, err := store.InboundQueue(ctx, org, project, connID)
	if err != nil {
		t.Fatalf("InboundQueue error = %v", err)
	}
	if err := q.Publish(ctx, "poison-1", []byte(`not json at all`)); err != nil {
		t.Fatalf("Publish error = %v", err)
	}
	// An envelope-shaped body with no source is poison too: it is syntactically JSON but unroutable.
	if err := q.Publish(ctx, "poison-2", []byte(`{"data":{"x":1}}`)); err != nil {
		t.Fatalf("Publish error = %v", err)
	}
	if err := q.Publish(ctx, "healthy-1", queueEnvelope("orders.v1", "", "evt-ok", `{"ok":true}`)); err != nil {
		t.Fatalf("Publish error = %v", err)
	}

	if err := bridge.Tick(ctx); err != nil {
		t.Fatalf("bridge Tick error = %v", err)
	}
	for _, key := range []string{"poison-1", "poison-2"} {
		if got := queueMessageState(t, pool, org, project, connID, key); got != "dead" {
			t.Fatalf("%s state = %q, want dead (retired on the first delivery, not looped)", key, got)
		}
	}
	if got := queueMessageState(t, pool, org, project, connID, "healthy-1"); got != "acked" {
		t.Fatalf("healthy message state = %q, want acked — poison must not block the stream", got)
	}
	if got := countRuns(t, pool, org, project); got != 1 {
		t.Fatalf("runs = %d, want 1 (only the healthy message admits)", got)
	}
}

// TestQueueBridgeDisabledConnectionAdmitsNothing is the §2 leg: disabling a binding stops admission
// completely and IMMEDIATELY. The message is not consumed, not acked and not dead-lettered — it stays
// exactly where it was, so re-enabling the connection resumes rather than having silently burned the
// backlog. The check is in the catalogue SQL, so it cannot be forgotten downstream.
func TestQueueBridgeDisabledConnectionAdmitsNothing(t *testing.T) {
	bridge, store, _, pool := wiredQueueBridge(t, QueueBridgeConfig{})
	ctx := context.Background()
	org, project, _ := seedSession(t, pool)
	principal := seedPrincipal(t, pool, org, project)
	revision := seedQueuePublishedRevision(t, pool, org, project)
	connID := mustCreateQueueConn(t, store, org, project, QueueConnectionInput{
		Name: "disabled", Direction: "inbound", Config: inboundConnConfig(revision, principal)})

	q, err := store.InboundQueue(ctx, org, project, connID)
	if err != nil {
		t.Fatalf("InboundQueue error = %v", err)
	}
	if err := q.Publish(ctx, "msg-disabled", queueEnvelope("orders.v1", "", "evt-d", `{}`)); err != nil {
		t.Fatalf("Publish error = %v", err)
	}
	mustExec(t, pool, `UPDATE queue_connections SET enabled = false WHERE id = $1`, connID)

	if _, ok := sweptConn(t, store, "inbound", connID); ok {
		t.Fatal("a disabled connection is still returned by the bridge sweep — the enabled check is not in the catalogue query")
	}
	for i := 0; i < 3; i++ {
		if err := bridge.Tick(ctx); err != nil {
			t.Fatalf("bridge Tick error = %v", err)
		}
	}
	if got := countRuns(t, pool, org, project); got != 0 {
		t.Fatalf("runs from a disabled connection = %d, want 0", got)
	}
	if got := queueMessageState(t, pool, org, project, connID, "msg-disabled"); got != "ready" {
		t.Fatalf("message state under a disabled connection = %q, want ready (untouched, resumable)", got)
	}
}

// TestQueueBridgePayloadCannotSelectTenantOrTarget is the §38.6 leg, and it is the reason this bridge is a
// separate admission entrypoint rather than IngestInbound with the signature check turned off. A hostile
// producer publishes an envelope whose source_tenant names ANOTHER organization and whose data carries an
// agent_revision_id belonging to that other tenant. Both are inert: the run is born in the CONNECTION's
// tenant, pinned to the CONNECTION's revision, and the victim tenant gets nothing at all.
func TestQueueBridgePayloadCannotSelectTenantOrTarget(t *testing.T) {
	bridge, store, _, pool := wiredQueueBridge(t, QueueBridgeConfig{})
	ctx := context.Background()
	org, project, _ := seedSession(t, pool)
	principal := seedPrincipal(t, pool, org, project)
	revision := seedQueuePublishedRevision(t, pool, org, project)
	// The victim: a completely separate tenant with its own published revision.
	victimOrg, victimProject, _ := seedSession(t, pool)
	victimRevision := seedQueuePublishedRevision(t, pool, victimOrg, victimProject)

	connID := mustCreateQueueConn(t, store, org, project, QueueConnectionInput{
		Name: "hostile", Direction: "inbound", Config: inboundConnConfig(revision, principal)})
	q, err := store.InboundQueue(ctx, org, project, connID)
	if err != nil {
		t.Fatalf("InboundQueue error = %v", err)
	}
	hostile, _ := json.Marshal(map[string]any{
		"source":          "orders.v1",
		"source_tenant":   victimOrg, // the escape attempt: name another tenant
		"source_event_id": "evt-hostile",
		"data": map[string]any{
			"organization_id":   victimOrg,
			"project_id":        victimProject,
			"agent_revision_id": victimRevision, // the second escape attempt: pin another tenant's revision
			"principal_id":      "prin_whatever",
		},
	})
	if err := q.Publish(ctx, "hostile-1", hostile); err != nil {
		t.Fatalf("Publish error = %v", err)
	}
	if err := bridge.Tick(ctx); err != nil {
		t.Fatalf("bridge Tick error = %v", err)
	}

	if got := countRuns(t, pool, org, project); got != 1 {
		t.Fatalf("runs in the connection's own tenant = %d, want 1", got)
	}
	if got := countRuns(t, pool, victimOrg, victimProject); got != 0 {
		t.Fatalf("runs in the tenant the PAYLOAD named = %d, want 0 — a payload field selected a tenant", got)
	}
	if got := runPinnedRevision(t, pool, org, project); got != revision {
		t.Fatalf("run pinned revision = %q, want the CONNECTION's %q (the payload's pin must be inert)", got, revision)
	}
}

// TestQueueBridgeSkipsConnectionWithNoRunTarget pins the fail-closed half: a binding whose config names no
// revision (or a principal from another tenant) admits nothing AND does not burn its backlog — the
// messages stay ready for the moment an operator fixes the configuration.
func TestQueueBridgeSkipsConnectionWithNoRunTarget(t *testing.T) {
	bridge, store, _, pool := wiredQueueBridge(t, QueueBridgeConfig{})
	ctx := context.Background()
	org, project, _ := seedSession(t, pool)
	revision := seedQueuePublishedRevision(t, pool, org, project)
	// A principal that belongs to a DIFFERENT tenant: the confused-deputy attempt.
	foreignOrg, foreignProject, _ := seedSession(t, pool)
	foreignPrincipal := seedPrincipal(t, pool, foreignOrg, foreignProject)

	for _, tc := range []struct {
		name   string
		config []byte
	}{
		{"no config at all", nil},
		{"no revision", []byte(fmt.Sprintf(`{"principal_id":%q}`, seedPrincipal(t, pool, org, project)))},
		{"no principal", []byte(fmt.Sprintf(`{"agent_revision_id":%q}`, revision))},
		{"foreign principal", inboundConnConfig(revision, foreignPrincipal)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			connID := mustCreateQueueConn(t, store, org, project, QueueConnectionInput{
				Name: randID("badtarget"), Direction: "inbound", Config: tc.config})
			q, err := store.InboundQueue(ctx, org, project, connID)
			if err != nil {
				t.Fatalf("InboundQueue error = %v", err)
			}
			if err := q.Publish(ctx, "k", queueEnvelope("orders.v1", "", "e", `{}`)); err != nil {
				t.Fatalf("Publish error = %v", err)
			}
			before := countRuns(t, pool, org, project)
			if err := bridge.Tick(ctx); err != nil {
				t.Fatalf("bridge Tick error = %v", err)
			}
			if got := countRuns(t, pool, org, project); got != before {
				t.Fatalf("runs went %d -> %d, want no admission from a target-less connection", before, got)
			}
			if got := queueMessageState(t, pool, org, project, connID, "k"); got != "ready" {
				t.Fatalf("message state = %q, want ready (a misconfiguration must not burn the backlog)", got)
			}
			if got := countRuns(t, pool, foreignOrg, foreignProject); got != 0 {
				t.Fatalf("runs in the foreign principal's tenant = %d, want 0", got)
			}
		})
	}
}

// TestQueueTerminalEnqueuesOutboundLosslessExactlyOnce is the §34.5 WIRED leg. A run's TERMINAL
// TRANSACTION commits the outbound delivery row, so:
//
//	the publisher is down          -> the row is already durable; nothing is lost
//	the pump restarts              -> a fresh outbox picks the same row up
//	the publisher recovers         -> exactly ONE logical delivery (the destination key collapses retries)
//	a second terminal write        -> still ONE row (ON CONFLICT on the run-keyed destination)
//
// The point of asserting the row BEFORE any pump tick is that durability comes from the terminal
// transaction, not from the pump having run.
func TestQueueTerminalEnqueuesOutboundLosslessExactlyOnce(t *testing.T) {
	_, store, spine, pool := wiredQueueBridge(t, QueueBridgeConfig{})
	ctx := context.Background()
	org, project, _ := seedSession(t, pool)
	principal := seedPrincipal(t, pool, org, project)
	connID := mustCreateQueueConn(t, store, org, project, QueueConnectionInput{
		Name: "results", Direction: "outbound", MaxDeliveries: 5,
		Config: []byte(`{"destination_url":"https://sink.example.test/queue"}`)})

	// A real run through the real admission path, then a real terminal transition.
	responseID, runID, sessionID := newID("resp"), newID("run"), newID("ses")
	if _, err := spine.AdmitResponse(ctx, coordinator.Tenant{Organization: org, Project: project}, coordinator.AdmissionInput{
		Principal: principal, IdempotencyKey: "outbound-1", Method: "POST", Route: queueAdmitRoute,
		RequestHash: "hash-outbound-1", ResponseID: responseID, RunID: runID, SessionID: sessionID,
		Input: []byte(`{}`), Body: []byte(`{"id":"` + responseID + `"}`), Store: true,
	}); err != nil {
		t.Fatalf("AdmitResponse error = %v", err)
	}
	if _, err := spine.ApplyRunTransition(ctx, coordinator.Tenant{Organization: org, Project: project},
		runID, statemachines.RunCmdCancel); err != nil {
		t.Fatalf("ApplyRunTransition error = %v", err)
	}

	// The row exists BEFORE any pump ran: durability is the terminal transaction's, not the pump's.
	if got := queueDeliveryState(t, pool, org, project, runID, connID); got != "pending" {
		t.Fatalf("delivery state right after the terminal transition = %q, want pending", got)
	}

	pump := NewQueueOutboxPump(store, func([]byte) (queue.Sink, error) { return nil, nil }, QueueBridgeConfig{}, t.Logf)
	sink := &recordingSink{down: true}
	pump.sinks = func([]byte) (queue.Sink, error) { return sink, nil }
	pump.cfg.DeliveryBackoff = -time.Second // due immediately on the next tick

	if err := pump.Tick(ctx); err != nil {
		t.Fatalf("pump Tick (publisher down) error = %v", err)
	}
	if got := queueDeliveryState(t, pool, org, project, runID, connID); got != "pending" {
		t.Fatalf("delivery state after a failed attempt = %q, want pending (durable, not lost)", got)
	}

	sink.down = false
	if err := pump.Tick(ctx); err != nil {
		t.Fatalf("pump Tick (publisher recovered) error = %v", err)
	}
	if got := queueDeliveryState(t, pool, org, project, runID, connID); got != "delivered" {
		t.Fatalf("delivery state after recovery = %q, want delivered", got)
	}
	if sink.unique() != 1 {
		t.Fatalf("unique destination keys = %d, want 1 (exactly one logical delivery)", sink.unique())
	}
	if err := pump.Tick(ctx); err != nil {
		t.Fatalf("third pump Tick error = %v", err)
	}
	if sink.unique() != 1 || sink.total() > 2 {
		t.Fatalf("sink saw unique=%d total=%d after a delivered result — a delivered row must not re-send", sink.unique(), sink.total())
	}

	// The payload names the run's canonical coordinates and nothing else — a queue subscriber learns what
	// to fetch, not the run's content.
	var payload []byte
	if err := pool.QueryRow(storage.ScopeToTenant(ctx, org, project),
		`SELECT payload FROM queue_deliveries WHERE queue_connection_id = $1 AND destination_key = $2`,
		connID, runID).Scan(&payload); err != nil {
		t.Fatalf("read delivery payload error = %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal(payload, &got); err != nil {
		t.Fatalf("delivery payload is not JSON: %v (%q)", err, payload)
	}
	if got["run_id"] != runID || got["response_id"] != responseID || got["state"] != string(statemachines.RunCanceled) {
		t.Fatalf("delivery payload = %v, want the run's canonical terminal coordinates", got)
	}
}

// TestQueueTerminalEnqueuesNothingWithoutAnOutboundConnection pins the other half of the same hot path: a
// project that registered no outbound binding (every deployment that wires no queue) writes nothing on a
// run terminal — the bridge is opt-in per project, not a tax on the spine.
func TestQueueTerminalEnqueuesNothingWithoutAnOutboundConnection(t *testing.T) {
	_, store, spine, pool := wiredQueueBridge(t, QueueBridgeConfig{})
	ctx := context.Background()
	org, project, _ := seedSession(t, pool)
	principal := seedPrincipal(t, pool, org, project)
	// An INBOUND connection exists — the direction predicate, not merely "a connection exists", is what
	// decides. A bug that ignored direction would enqueue results onto a consumer binding.
	revision := seedQueuePublishedRevision(t, pool, org, project)
	mustCreateQueueConn(t, store, org, project, QueueConnectionInput{
		Name: "consumer-only", Direction: "inbound", Config: inboundConnConfig(revision, principal)})

	responseID, runID, sessionID := newID("resp"), newID("run"), newID("ses")
	if _, err := spine.AdmitResponse(ctx, coordinator.Tenant{Organization: org, Project: project}, coordinator.AdmissionInput{
		Principal: principal, IdempotencyKey: "no-outbound-1", Method: "POST", Route: queueAdmitRoute,
		RequestHash: "hash-no-outbound", ResponseID: responseID, RunID: runID, SessionID: sessionID,
		Input: []byte(`{}`), Body: []byte(`{"id":"` + responseID + `"}`), Store: true,
	}); err != nil {
		t.Fatalf("AdmitResponse error = %v", err)
	}
	if _, err := spine.ApplyRunTransition(ctx, coordinator.Tenant{Organization: org, Project: project},
		runID, statemachines.RunCmdCancel); err != nil {
		t.Fatalf("ApplyRunTransition error = %v", err)
	}

	var n int
	if err := pool.QueryRow(storage.ScopeToTenant(ctx, org, project),
		`SELECT count(*) FROM queue_deliveries WHERE organization_id = $1 AND project_id = $2`, org, project).Scan(&n); err != nil {
		t.Fatalf("count deliveries error = %v", err)
	}
	if n != 0 {
		t.Fatalf("queue deliveries with no OUTBOUND connection = %d, want 0", n)
	}
}
