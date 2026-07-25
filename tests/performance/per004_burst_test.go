//go:build performance

// PER-004 — burst trigger/queue: backpressure and fairness (spec §64.14, §34.4).
//
// Two real seams, one profiled run:
//
//   - E11 trigger admission (over real HTTP against the running binary): a burst of deliveries across
//     several correlation keys hits the `queue` concurrency policy. The BOUNDED BUFFER is the per-key
//     admission gate — at most one admitted run per key, every other delivery DEFERRED durably, none
//     dropped — and the DEPTH REPORT is the deferred backlog per key. Fairness is per-key: one key's
//     burst must not starve another key's first delivery.
//   - E17 T7 queue adapter (in-process, the public reference impl): a burst past the configured
//     capacity must SHED with ErrQueueFull, never silently drop, never grow past capacity; the Depth
//     gauge must report the backlog; and every accepted message must eventually be served (no producer
//     starves behind another).
//
// The RED fixture turns the REAL knob off: concurrency_policy `allow` is backpressure disabled, and the
// same burst then admits every delivery at once — the bounded-buffer gate MUST refuse that run.
//
// HONEST CEILING: the queue leg exercises the in-process reference adapter and the durable per-key gate
// behind the real API; a real broker (SQS/PubSub/Kafka) is the E18 §6 operator leg and is not measured
// here. Dispatch is disabled in this tier, so an admitted run stays queued — which is exactly what keeps
// the gate deterministic, and means these numbers say nothing about run execution time.
package performance

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/palgroup/palai/adapters/integrations/queue"
)

const (
	per004Keys        = 4  // correlation keys in the burst (the fairness dimension)
	per004PerKey      = 12 // deliveries per key
	per004QueueCap    = 64 // E17 T7 bounded buffer under test
	per004QueueBurst  = 256
	per004QueueMakers = 4
)

const per004Ceiling = "macOS + Docker Desktop profile; NO SLO and NO reference-hardware claim (§54 targets " +
	"are product goals, measured for real in E18 §6 leg 3). The E11 leg is the durable per-key admission " +
	"gate behind the real API; the queue leg is the E17 T7 in-process reference adapter — a real broker is " +
	"the §6 operator leg. Dispatch is off (E08 rule), so no number here is run-execution time."

func TestPER004BurstTriggerQueue(t *testing.T) {
	s := requireStack(t)
	run := mustRun(t, "PER-004", fmt.Sprintf(
		"burst: %d correlation keys x %d concurrent trigger deliveries (%d total) under concurrency_policy=queue; "+
			"queue adapter: %d messages from %d concurrent producers into a capacity-%d bounded buffer",
		per004Keys, per004PerKey, per004Keys*per004PerKey, per004QueueBurst, per004QueueMakers, per004QueueCap),
		per004Ceiling)

	// A BURST budget this harness chose — deliberately NOT §54.3's 500ms accepted-mutation target, which
	// is stated "under admitted healthy capacity". A 48-way simultaneous burst against one per-key gate on
	// a shared laptop is contention on purpose; measuring it against the healthy-capacity target would
	// either fail honest work or quietly redefine the spec's number. PER-001 owns the §54.3 mutation gate.
	// Observed on this profile: p95 ~0.4s for the 48-way burst; 2s is ~5x headroom for a shared laptop
	// and still refuses a real stall.
	run.Gate("trigger_delivery_accept", 95, float64(2*time.Second), "ns",
		"case budget (burst contention) — NOT the §54.3 healthy-capacity mutation target; PER-001 owns that")
	// THE bounded-buffer gate: never more than one admitted (running) delivery per correlation key,
	// whatever the burst. p100 = the worst key.
	run.Gate("trigger_runs_admitted_per_key", 100, 1, "runs",
		"invariant (E11 queue policy: one admitted run per correlation key)")
	// Nothing may be lost: every delivery the API accepted must have a durable row.
	run.Gate("trigger_deliveries_lost", 100, 0, "deliveries", "invariant (durable-record-before-ack, §34.2)")
	// E17 T7: the ready+in-flight backlog may never exceed the configured capacity.
	run.Gate("queue_depth", 100, per004QueueCap, "messages", "invariant (§34.4 bounded buffer = configured capacity)")
	// Fairness: no producer's accepted message goes unserved behind another producer's burst.
	run.Gate("queue_unserved_per_producer", 100, 0, "messages", "invariant (no producer starves)")

	burstTriggerDeliveries(t, s, run, "queue")
	burstQueueAdapter(t, run)
	complete(t, run, "PER-004")
}

// TestPER004BackpressureDisabledFailsTheBoundedBufferGate is the RED fixture, and it turns the REAL
// knob: concurrency_policy `allow` is the E11 gate DISABLED. The identical burst then admits every
// delivery concurrently, and the bounded-buffer gate must refuse the run.
func TestPER004BackpressureDisabledFailsTheBoundedBufferGate(t *testing.T) {
	s := requireStack(t)
	run := mustRun(t, "PER-004-negative-backpressure-off", fmt.Sprintf(
		"RED fixture: the SAME %dx%d burst with concurrency_policy=allow (backpressure disabled)",
		per004Keys, per004PerKey), per004Ceiling+" RED fixture: produces a deliberately failing run.")
	run.Gate("trigger_runs_admitted_per_key", 100, 1, "runs",
		"invariant (E11 queue policy: one admitted run per correlation key)")

	admitted := burstTriggerDeliveries(t, s, run, "allow")
	if admitted <= per004Keys {
		t.Fatalf("with backpressure disabled only %d deliveries were admitted; the fixture did not "+
			"actually turn the gate off, so its failure would prove nothing", admitted)
	}
	mustFailGate(t, run, "PER-004-negative-backpressure-off", "trigger_runs_admitted_per_key")
}

// burstTriggerDeliveries fires the burst at one trigger under the given concurrency policy and records
// the admission latencies, the per-key admitted count (the bounded buffer), the per-key deferred backlog
// (the depth report) and the lost-delivery count. It returns the total admitted count.
func burstTriggerDeliveries(t *testing.T, s *stack, run *Run, policy string) int {
	t.Helper()
	trigger := s.mustPost("/v1/triggers", jsonBody(map[string]string{"name": newID("perf-burst")}), 201, nil)
	triggerID, _ := trigger["id"].(string)
	if triggerID == "" {
		t.Fatalf("createTrigger returned no id: %v", trigger)
	}
	s.mustPost("/v1/triggers/"+triggerID+"/revisions", jsonBody(map[string]any{
		"concurrency_policy":   policy,
		"correlation_key_expr": map[string]string{"select": "key"},
		"input_mapping":        map[string]any{"fields": map[string]any{"input": map[string]string{"const": "burst"}}},
	}), 201, nil)

	type outcome struct {
		key     string
		state   string
		id      string
		elapsed time.Duration
		status  int
	}
	results := make([]outcome, per004Keys*per004PerKey)
	var wg sync.WaitGroup
	for k := 0; k < per004Keys; k++ {
		for i := 0; i < per004PerKey; i++ {
			idx, key := k*per004PerKey+i, fmt.Sprintf("k%d", k)
			wg.Add(1)
			go func() {
				defer wg.Done()
				status, body, elapsed := s.do("POST", "/v1/triggers/"+triggerID+"/deliveries",
					jsonBody(map[string]string{"key": key}),
					map[string]string{"Idempotency-Key": newID("idem")})
				state, _ := body["state"].(string)
				id, _ := body["id"].(string)
				results[idx] = outcome{key: key, state: state, id: id, elapsed: elapsed, status: status}
			}()
		}
	}
	wg.Wait()

	admittedByKey := map[string]int{}
	deferredByKey := map[string]int{}
	accepted, admittedTotal := 0, 0
	for _, r := range results {
		ok := r.status == 202
		run.Latency("trigger_delivery_accept", policy, r.elapsed, ok)
		if !ok {
			continue
		}
		accepted++
		switch r.state {
		case "run_created":
			admittedByKey[r.key]++
			admittedTotal++
		case "deferred":
			deferredByKey[r.key]++
		default:
			t.Fatalf("delivery reached unexpected state %q under policy %q", r.state, policy)
		}
	}

	// The bounded buffer and the depth report, per key. Every key is recorded (including a zero) so a
	// key that never got in is visible as starvation rather than as a missing sample.
	for k := 0; k < per004Keys; k++ {
		key := fmt.Sprintf("k%d", k)
		run.Observe("trigger_runs_admitted_per_key", key, float64(admittedByKey[key]), "runs", true)
		run.Observe("trigger_queue_depth_per_key", key, float64(deferredByKey[key]), "deliveries", true)
		if policy == "queue" && admittedByKey[key] != 1 {
			t.Fatalf("key %s admitted %d runs under the queue policy, want exactly 1 "+
				"(fairness: every key gets in, and no key gets in twice)", key, admittedByKey[key])
		}
	}

	// Nothing dropped: every accepted delivery is a durable row.
	durable := s.queryInt(`SELECT count(*) FROM trigger_deliveries WHERE trigger_id=$1`, triggerID)
	lost := int64(accepted) - durable
	if lost < 0 {
		lost = 0
	}
	run.Observe("trigger_deliveries_lost", policy, float64(lost), "deliveries", true)
	if durable != int64(accepted) {
		t.Fatalf("%d deliveries were accepted but %d rows are durable: the buffer dropped work", accepted, durable)
	}

	if policy == "queue" {
		drainOneKey(t, s, run, triggerID, results[0].key)
	}
	return admittedTotal
}

// drainOneKey proves the bounded buffer DRAINS rather than merely blocking: terminating the admitted run
// opens the key's gate, the supervised delivery-reconciler admits the FIFO head, and the key is back at
// exactly one admitted run. Without this, "at most one" could be satisfied by a wedged queue.
func drainOneKey(t *testing.T, s *stack, run *Run, triggerID, key string) {
	t.Helper()
	before := s.queryInt(`SELECT count(*) FROM trigger_deliveries WHERE trigger_id=$1 AND state='run_created'`, triggerID)
	s.exec(`UPDATE runs SET state='completed' WHERE id IN (
	          SELECT run_id FROM trigger_deliveries
	          WHERE trigger_id=$1 AND state='run_created' AND run_id IS NOT NULL LIMIT 1)`, triggerID)

	start := time.Now()
	deadline := start.Add(30 * time.Second)
	for {
		if s.queryInt(`SELECT count(*) FROM trigger_deliveries WHERE trigger_id=$1 AND state='run_created'`, triggerID) > before {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the deferred backlog for %s never drained after its gate opened; a bounded buffer "+
				"that does not drain is a stall, not backpressure", key)
		}
		time.Sleep(50 * time.Millisecond)
	}
	run.Latency("trigger_queue_drain", "reconciler", time.Since(start), true)
}

// burstQueueAdapter drives the E17 T7 reference queue: concurrent producers overrun a bounded buffer,
// the excess is SHED (ErrQueueFull) rather than dropped, the Depth gauge reports the backlog, and every
// accepted message is eventually served.
func burstQueueAdapter(t *testing.T, run *Run) {
	t.Helper()
	q := queue.NewMemory(queue.MemoryConfig{Capacity: per004QueueCap, Visibility: 5 * time.Second, MaxDeliveries: 3}, nil)
	ctx := context.Background()

	accepted := make([]int, per004QueueMakers)
	shed := make([]int, per004QueueMakers)
	var mu sync.Mutex
	var wg sync.WaitGroup
	for p := 0; p < per004QueueMakers; p++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < per004QueueBurst/per004QueueMakers; i++ {
				body := []byte(fmt.Sprintf(`{"producer":%d,"n":%d}`, p, i))
				start := time.Now()
				err := q.Publish(ctx, fmt.Sprintf("p%d-%d", p, i), body)
				elapsed := time.Since(start)

				mu.Lock()
				// A shed publish is backpressure working, not an error: it is recorded ok=true so it
				// cannot inflate the error rate, and counted separately so nothing goes missing.
				switch {
				case err == nil:
					accepted[p]++
				case errors.Is(err, queue.ErrQueueFull):
					shed[p]++
				default:
					t.Errorf("Publish returned an unexpected error: %v", err)
				}
				mu.Unlock()
				run.Latency("queue_publish", fmt.Sprintf("p%d", p), elapsed, err == nil || errors.Is(err, queue.ErrQueueFull))

				depth, derr := q.Depth(ctx)
				if derr != nil {
					t.Errorf("Depth error: %v", derr)
					return
				}
				run.Observe("queue_depth", "burst", float64(depth.Ready+depth.InFlight), "messages", true)
			}
		}()
	}
	wg.Wait()

	totalAccepted, totalShed := 0, 0
	for p := range accepted {
		totalAccepted += accepted[p]
		totalShed += shed[p]
	}
	if totalAccepted+totalShed != per004QueueBurst {
		t.Fatalf("%d accepted + %d shed != %d published: the queue lost messages instead of applying "+
			"backpressure", totalAccepted, totalShed, per004QueueBurst)
	}
	if totalShed == 0 {
		t.Fatalf("a %d-message burst into a capacity-%d buffer shed nothing; the fixture is not "+
			"actually overrunning the buffer, so the bounded-buffer claim would be vacuous",
			per004QueueBurst, per004QueueCap)
	}

	// Drain: every accepted message must be served. The consumer counts per producer, so a starved
	// producer shows up as a non-zero unserved count rather than as a good-looking average.
	served := make([]int, per004QueueMakers)
	for {
		handled, err := q.Consume(ctx, 32, func(_ context.Context, m queue.Message) (queue.Disposition, error) {
			var p, n int
			if _, serr := fmt.Sscanf(string(m.Body), `{"producer":%d,"n":%d}`, &p, &n); serr != nil {
				return queue.DeadLetter, nil
			}
			served[p]++
			return queue.Ack, nil
		})
		if err != nil {
			t.Fatalf("Consume error = %v", err)
		}
		if handled == 0 {
			break
		}
	}
	for p := range served {
		run.Observe("queue_unserved_per_producer", fmt.Sprintf("p%d", p), float64(accepted[p]-served[p]), "messages", true)
	}
	final, err := q.Depth(ctx)
	if err != nil {
		t.Fatalf("final Depth error = %v", err)
	}
	if final.Ready != 0 || final.InFlight != 0 {
		t.Fatalf("after draining, depth = %+v, want an empty ready/in-flight backlog", final)
	}
	t.Logf("PER-004 queue leg: %d accepted, %d shed by backpressure, %d dead-lettered, all accepted served",
		totalAccepted, totalShed, final.Dead)
}

// mustRun opens a profiled run or fails the case. The refusal is the point: no profile, no numbers.
func mustRun(t *testing.T, caseID, loadShape, ceiling string) *Run {
	t.Helper()
	run, err := NewRun(caseID, loadShape, ceiling)
	if err != nil {
		t.Fatalf("NewRun(%s) error = %v", caseID, err)
	}
	return run
}
