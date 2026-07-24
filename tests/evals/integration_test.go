package evals

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"testing"
	"time"

	"github.com/palgroup/palai/adapters/integrations/a2a"
	"github.com/palgroup/palai/adapters/integrations/queue"
	"github.com/palgroup/palai/adapters/integrations/slack"
)

// The integration benchmark (§57.10) runs the SAME six canonical-mapping scenarios — duplicate,
// out-of-order, identity, attachment, rate-limit, one-terminal-response — PARAMETRICALLY against the three
// on-main integration adapters (T1 Slack, T2 A2A, T7 queue). Each cell asserts the scenario's invariant
// against that adapter's OWN canonical-mapping surface (no new interface is forced onto the adapters). A
// cell a given adapter's surface genuinely does not model is an HONEST skip with a recorded reason, never a
// silent pass. The benchmark fails iff any applicable cell fails.
//
// T3 (A2A client) lands in parallel; its remote-agent client surface is an EXTENSION POINT for these same
// scenarios (a remote duplicate/identity/attachment leg) and is noted where relevant, not covered here.

type cellResult struct {
	ok      bool
	skipped bool
	detail  string
}

func pass() cellResult                  { return cellResult{ok: true} }
func skipCell(reason string) cellResult { return cellResult{skipped: true, detail: reason} }
func failCell(detail string) cellResult { return cellResult{detail: detail} }

func TestIntegrationBenchmark(t *testing.T) {
	scenarios := []string{"duplicate", "out-of-order", "identity", "attachment", "rate-limit", "one-terminal-response"}
	adapters := []string{"slack", "a2a", "queue"}

	cell := map[string]map[string]func(*testing.T) cellResult{
		"slack": {
			"duplicate":             slackDuplicate,
			"out-of-order":          slackOutOfOrder,
			"identity":              slackIdentity,
			"attachment":            slackAttachment,
			"rate-limit":            slackRateLimit,
			"one-terminal-response": slackOneTerminal,
		},
		"a2a": {
			"duplicate":             a2aDuplicate,
			"out-of-order":          a2aOutOfOrder,
			"identity":              a2aIdentity,
			"attachment":            a2aAttachment,
			"rate-limit":            a2aRateLimit,
			"one-terminal-response": a2aOneTerminal,
		},
		"queue": {
			"duplicate":             queueDuplicate,
			"out-of-order":          queueOutOfOrder,
			"identity":              queueIdentity,
			"attachment":            queueAttachment,
			"rate-limit":            queueRateLimit,
			"one-terminal-response": queueOneTerminal,
		},
	}

	covered := 0
	for _, sc := range scenarios {
		for _, ad := range adapters {
			fn := cell[ad][sc]
			if fn == nil {
				t.Fatalf("benchmark matrix hole: no cell for %s/%s", ad, sc)
			}
			t.Run(ad+"/"+sc, func(t *testing.T) {
				r := fn(t)
				switch {
				case r.skipped:
					t.Logf("N/A: %s", r.detail)
				case r.ok:
					covered++
				default:
					t.Fatalf("cell failed: %s", r.detail)
				}
			})
		}
	}
	// The benchmark must genuinely exercise the adapters, not skip its way to green. The skips are
	// compile-time hardcoded (queue/attachment + a2a/rate-limit), so the covered count is EXACT: 18 cells − 2
	// skips = 16. Pinning it exactly catches a silent skip regression (a cell that starts skipping instead of
	// asserting) that a `covered == 0` guard would wave through at 1/16.
	const wantCovered = 16 // 6 scenarios × 3 adapters − 2 compile-time N/A cells
	if covered != wantCovered {
		t.Fatalf("benchmark covered %d cells; want exactly %d (18 − 2 compile-time skips) — a silent skip regression", covered, wantCovered)
	}
}

// ---------- queue (T7) ----------

func queueDuplicate(t *testing.T) cellResult {
	// A redelivery repeats the idempotency key; the idempotent handler runs the effect ONCE (§34.2).
	q := queue.NewMemory(queue.MemoryConfig{}, time.Now)
	ctx := context.Background()
	_ = q.Publish(ctx, "k1", []byte("a"))
	_ = q.Publish(ctx, "k1", []byte("a")) // same logical message redelivered
	effects := runIdempotent(t, q)
	if effects["k1"] != 1 {
		return failCell("duplicate produced multiple effects")
	}
	return pass()
}

func queueIdentity(t *testing.T) cellResult {
	// The canonical identity is the connection-scoped idempotency key, NOT the payload: two DIFFERENT
	// bodies under the same key collapse to one effect (a payload cannot fork identity — §34.1).
	q := queue.NewMemory(queue.MemoryConfig{}, time.Now)
	ctx := context.Background()
	_ = q.Publish(ctx, "k1", []byte("body-A"))
	_ = q.Publish(ctx, "k1", []byte("body-B"))
	effects := runIdempotent(t, q)
	if effects["k1"] != 1 {
		return failCell("payload content forked the canonical identity")
	}
	return pass()
}

func queueOutOfOrder(t *testing.T) cellResult {
	// Distinct keys keep distinct identities regardless of consume order — ordering never merges them.
	q := queue.NewMemory(queue.MemoryConfig{}, time.Now)
	ctx := context.Background()
	_ = q.Publish(ctx, "k2", []byte("b"))
	_ = q.Publish(ctx, "k1", []byte("a"))
	effects := runIdempotent(t, q)
	if effects["k1"] != 1 || effects["k2"] != 1 {
		return failCell("out-of-order delivery merged or dropped a distinct identity")
	}
	return pass()
}

func queueRateLimit(t *testing.T) cellResult {
	// Bounded buffer: at capacity Publish returns ErrQueueFull (backpressure, never a silent drop) and
	// Depth reports the backlog (§34.4).
	q := queue.NewMemory(queue.MemoryConfig{Capacity: 2}, time.Now)
	ctx := context.Background()
	_ = q.Publish(ctx, "k1", []byte("a"))
	_ = q.Publish(ctx, "k2", []byte("b"))
	if err := q.Publish(ctx, "k3", []byte("c")); err != queue.ErrQueueFull {
		return failCell("over-capacity Publish did not signal backpressure")
	}
	d, _ := q.Depth(ctx)
	if d.Ready != 2 {
		return failCell("Depth did not report the bounded backlog")
	}
	return pass()
}

func queueOneTerminal(t *testing.T) cellResult {
	// One Ack per message: after the effect Acks, the message is gone — a second Consume yields nothing.
	q := queue.NewMemory(queue.MemoryConfig{}, time.Now)
	ctx := context.Background()
	_ = q.Publish(ctx, "k1", []byte("a"))
	h := func(ctx context.Context, m queue.Message) (queue.Disposition, error) { return queue.Ack, nil }
	n1, _ := q.Consume(ctx, 10, h)
	n2, _ := q.Consume(ctx, 10, h)
	if n1 != 1 || n2 != 0 {
		return failCell("a delivery did not terminate exactly once")
	}
	return pass()
}

func queueAttachment(t *testing.T) cellResult {
	return skipCell("queue payloads are opaque bytes; attachment fetch/scan is the consumer's ingest step, not a queue-adapter concern (§34.1)")
}

func runIdempotent(t *testing.T, q *queue.Memory) map[string]int {
	t.Helper()
	effects := map[string]int{}
	seen := map[string]bool{}
	h := func(ctx context.Context, m queue.Message) (queue.Disposition, error) {
		if seen[m.IdempotencyKey] {
			return queue.Ack, nil // idempotent: the effect already ran for this key
		}
		seen[m.IdempotencyKey] = true
		effects[m.IdempotencyKey]++
		return queue.Ack, nil
	}
	if _, err := q.Consume(context.Background(), 10, h); err != nil {
		t.Fatalf("consume: %v", err)
	}
	return effects
}

// ---------- slack (T1) ----------

func slackDuplicate(t *testing.T) cellResult {
	body := []byte(`{"type":"event_callback","team_id":"T1","event_id":"Ev42","event":{"type":"message","user":"U1","channel":"C1","ts":"5.5"}}`)
	first, err := slack.MapEvent(body, "Ubot", false)
	if err != nil {
		return failCell(err.Error())
	}
	retry, err := slack.MapEvent(body, "Ubot", true) // Slack's redelivery
	if err != nil {
		return failCell(err.Error())
	}
	if first.SourceEventID == "" || first.SourceEventID != retry.SourceEventID {
		return failCell("redelivery did not repeat the canonical dedupe key")
	}
	return pass()
}

func slackOutOfOrder(t *testing.T) cellResult {
	root := []byte(`{"type":"event_callback","team_id":"T1","event_id":"Ev1","event":{"type":"message","user":"U1","channel":"C1","ts":"100.0","thread_ts":"100.0"}}`)
	reply := []byte(`{"type":"event_callback","team_id":"T1","event_id":"Ev2","event":{"type":"message","user":"U2","channel":"C1","ts":"101.0","thread_ts":"100.0"}}`)
	a, err1 := slack.MapEvent(reply, "Ubot", false) // arrives first, out of order
	b, err2 := slack.MapEvent(root, "Ubot", false)
	if err1 != nil || err2 != nil {
		return failCell("map error")
	}
	if a.ThreadTS != b.ThreadTS || a.ThreadTS != "100.0" {
		return failCell("thread correlation not stable across arrival order")
	}
	return pass()
}

func slackIdentity(t *testing.T) cellResult {
	// The tenant is the workspace (team_id), never a user-supplied field: two events from different users
	// in the same workspace resolve the same SourceTenant (a message cannot self-declare a tenant).
	e1, _ := slack.MapEvent([]byte(`{"type":"event_callback","team_id":"T1","event_id":"Ev1","event":{"type":"message","user":"U1","channel":"C1","ts":"1.1"}}`), "Ubot", false)
	e2, _ := slack.MapEvent([]byte(`{"type":"event_callback","team_id":"T1","event_id":"Ev2","event":{"type":"message","user":"U2","channel":"C1","ts":"2.2"}}`), "Ubot", false)
	if e1.SourceTenant != e2.SourceTenant || e1.SourceTenant != "T1" {
		return failCell("tenant identity was not workspace-scoped")
	}
	return pass()
}

func slackAttachment(t *testing.T) cellResult {
	// A shared file classifies to KindFileShare — the control plane does a scoped fetch+scan (SLK-005);
	// the adapter does not auto-trust the file content.
	ev, err := slack.MapEvent([]byte(`{"type":"event_callback","team_id":"T1","event_id":"EvF","event":{"type":"message","subtype":"file_share","user":"U1","channel":"C1","ts":"1.1"}}`), "Ubot", false)
	if err != nil {
		return failCell(err.Error())
	}
	if ev.Kind != slack.KindFileShare {
		return failCell("file share not classified for scoped fetch+scan")
	}
	return pass()
}

func slackRateLimit(t *testing.T) cellResult {
	// A 429 is repaired ONCE (bounded, Retry-After honored) then the visible message posts — no retry storm.
	doer := &sequenceDoer{responses: []fakeResp{
		{status: http.StatusTooManyRequests, retryAfter: "1"},
		{status: http.StatusOK, body: `{"ok":true,"ts":"123.45"}`},
	}}
	res, err := slack.PostMessage(context.Background(), doer,
		slack.PostRequest{MethodURL: "https://slack.test/chat.postMessage", Body: []byte(`{}`)},
		slack.PostOptions{Wait: func(context.Context, time.Duration) error { return nil }})
	if err != nil {
		return failCell(err.Error())
	}
	if !res.Repaired || res.MessageTS != "123.45" || res.Attempts != 2 {
		return failCell("rate-limit repair did not converge to one delivery")
	}
	return pass()
}

func slackOneTerminal(t *testing.T) cellResult {
	// One delivery => one terminal post (one ts, one round trip) — the coalesced terminal summary (SLK-006).
	doer := &sequenceDoer{responses: []fakeResp{{status: http.StatusOK, body: `{"ok":true,"ts":"9.9"}`}}}
	res, err := slack.PostMessage(context.Background(), doer,
		slack.PostRequest{MethodURL: "https://slack.test/chat.postMessage", Body: []byte(`{}`)},
		slack.PostOptions{})
	if err != nil {
		return failCell(err.Error())
	}
	if res.MessageTS != "9.9" || res.Attempts != 1 {
		return failCell("a single delivery did not yield exactly one terminal post")
	}
	return pass()
}

// ---------- a2a (T2) ----------

func a2aDuplicate(t *testing.T) cellResult {
	// The A2A task id is an EXTERNAL ref projected stably; the canonical run id is never replaced (§38.2).
	st := a2a.TaskStatus{State: a2a.TaskStateWorking}
	one := a2a.BuildTask("task-1", "ctx-1", st, nil)
	two := a2a.BuildTask("task-1", "ctx-1", st, nil)
	if one.ID != two.ID || one.ID != "task-1" {
		return failCell("external task id not projected stably")
	}
	return pass()
}

func a2aOutOfOrder(t *testing.T) cellResult {
	// Terminal classification is order-independent: a completed state is terminal regardless of a later
	// working update (A2A-002 terminal consistency), and MapRunState is deterministic.
	if !a2a.TaskStateCompleted.Terminal() || a2a.TaskStateWorking.Terminal() {
		return failCell("terminal classification is not stable")
	}
	if a2a.MapRunState("completed") != a2a.TaskStateCompleted {
		return failCell("run-state mapping not deterministic")
	}
	return pass()
}

func a2aIdentity(t *testing.T) cellResult {
	// A forged tenant in message metadata is IGNORED — the authenticated bearer scope governs (§38.6).
	msg := a2a.Message{Metadata: map[string]any{"organization": "orgEVIL", "project": "projEVIL"}}
	org, proj := a2a.GovernIdentity("orgA", "projA", msg)
	if org != "orgA" || proj != "projA" {
		return failCell("A2A metadata overrode the governing tenant identity")
	}
	return pass()
}

func a2aAttachment(t *testing.T) cellResult {
	// An inbound file part is surfaced as a scannable unit (the A2A-004 ingest target); it never becomes a
	// privileged instruction.
	msg := a2a.Message{Parts: []a2a.Part{
		{Kind: "text", Text: "hello"},
		{Kind: "file", File: &a2a.FilePart{Name: "x.bin", Bytes: "AAAA"}},
	}}
	if got := a2a.FileParts(msg); len(got) != 1 || got[0].Name != "x.bin" {
		return failCell("inbound file part not surfaced for ingest+scan")
	}
	return pass()
}

func a2aRateLimit(t *testing.T) cellResult {
	return skipCell("A2A rate-limit/backpressure is transport-layer; the queue bounded-buffer and slack coalesced-repair cells own that invariant")
}

func a2aOneTerminal(t *testing.T) cellResult {
	// A direct Message is returned ONLY for a genuinely-complete non-durable response — one terminal; every
	// other outcome returns a Task tracked to a single terminal status (§38.2, A2A-002).
	if !a2a.DecideDirectMessage(a2a.TaskStateCompleted, false) {
		return failCell("a complete non-durable response should be one direct terminal")
	}
	if a2a.DecideDirectMessage(a2a.TaskStateWorking, false) {
		return failCell("a still-working response must be a tracked Task, not a terminal message")
	}
	if a2a.DecideDirectMessage(a2a.TaskStateCompleted, true) {
		return failCell("a durable run must always return a trackable Task")
	}
	return pass()
}

// ---------- test doubles ----------

type fakeResp struct {
	status     int
	body       string
	retryAfter string
}

// sequenceDoer returns a scripted sequence of responses, one per Do call — a fake Slack Web API for the
// rate-limit/one-terminal cells (no network).
type sequenceDoer struct {
	responses []fakeResp
	i         int
}

func (d *sequenceDoer) Do(*http.Request) (*http.Response, error) {
	r := d.responses[d.i]
	if d.i < len(d.responses)-1 {
		d.i++
	}
	h := http.Header{}
	if r.retryAfter != "" {
		h.Set("Retry-After", r.retryAfter)
	}
	return &http.Response{
		StatusCode: r.status,
		Header:     h,
		Body:       io.NopCloser(bytes.NewReader([]byte(r.body))),
	}, nil
}
