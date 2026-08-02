//go:build component

package execution

import (
	"context"
	"strings"
	"sync"
	"testing"

	"github.com/palgroup/palai/packages/contracts"
	modelbroker "github.com/palgroup/palai/packages/model-broker"

	"github.com/palgroup/palai/storage"
)

// streamingAdapter emits the provider's answer as SEVERAL deltas before returning it whole, which is what a
// real streaming adapter does and what the deterministic fake does not. The chunk size is chosen against
// deltaFlushBytes rather than picked: three 4 KiB chunks cross the 8 KiB size trigger exactly once, so the
// sink is forced to write MORE THAN ONE window without this test sleeping for a 500ms flush tick. A
// single-window test could not tell a lossless coalescer from one that drops every window but the last.
type streamingAdapter struct {
	mu     sync.Mutex
	chunks []string
}

func (a *streamingAdapter) Execute(_ context.Context, req modelbroker.Request, _ string, onDelta func(modelbroker.Delta)) (modelbroker.Result, error) {
	a.mu.Lock()
	chunks := append([]string(nil), a.chunks...)
	a.mu.Unlock()
	if onDelta != nil {
		for _, c := range chunks {
			onDelta(modelbroker.Delta{Text: c})
		}
	}
	return modelbroker.Result{
		ModelRequestID: req.ModelRequestID, ProviderRequestID: "stream_1", Model: req.Model,
		Output: strings.Join(chunks, ""), FinishReason: "stop", Attempts: 1,
	}, nil
}

// TestDispatchJournalsStreamedTextAsDeltasBetweenCreatedAndCompleted is the guard for a defect whose whole
// shape was ABSENCE: model_step.delta.v1 was in the canonical registry and in the AsyncAPI x-event-types
// list from the beginning, and nothing in production ever wrote it — dispatch accumulated streamed text into
// a local strings.Builder so an interrupt could record a partial (spec §25.16), and journalled none of it.
// Every client therefore saw model_step.created.v1 followed by model_step.completed.v1 with the entire
// answer arriving at once. Nothing was broken; a writer was missing.
//
// THIS TEST DRIVES dispatchModel, NOT AppendModelStepDelta, and that is the point. A test that called the
// store method directly would pass against the tree as it stood BEFORE the writer was wired — it would
// prove the mechanism and say nothing about the defect, which lived in whether anything CALLS it. The rule
// this follows is the tree's own: proving a mechanism is not proving the surface that reaches it.
//
// Three properties, and the third is the one that costs something to get right:
//
//	(1) deltas are journalled at all;
//	(2) concatenating them in seq order reproduces the model's output EXACTLY — a coalescer that drops,
//	    duplicates or reorders a window fails here, and asserting on a substring instead would not catch it;
//	(3) every delta sits STRICTLY BETWEEN its step's created and completed events. A delta landing after
//	    its own terminal event puts a client's transcript out of order, which is worse than one that never
//	    arrived — it is what deltaSink.close() being synchronous exists to prevent.
func TestDispatchJournalsStreamedTextAsDeltasBetweenCreatedAndCompleted(t *testing.T) {
	cs, tenant, exec := openPinnedSpine(t)
	ctx := context.Background()
	sessionID, responseID, runID := pinnedID("ses"), pinnedID("resp"), pinnedID("run")
	exec(`INSERT INTO sessions (id, organization_id, project_id) VALUES ($1,$2,$3)`, sessionID, tenant.Organization, tenant.Project)
	exec(`INSERT INTO responses (id, organization_id, project_id, session_id, state) VALUES ($1,$2,$3,$4,'queued')`,
		responseID, tenant.Organization, tenant.Project, sessionID)
	exec(`INSERT INTO runs (id, organization_id, project_id, session_id, response_id, state) VALUES ($1,$2,$3,$4,$5,'running')`,
		runID, tenant.Organization, tenant.Project, sessionID, responseID)

	// Distinct chunk bodies so a dropped or duplicated window changes the concatenation rather than
	// hiding inside a run of identical bytes.
	chunks := []string{
		strings.Repeat("a", 4*1024),
		strings.Repeat("b", 4*1024),
		strings.Repeat("c", 4*1024),
	}
	want := strings.Join(chunks, "")

	adapter := &streamingAdapter{chunks: chunks}
	broker := modelbroker.New(modelbroker.Config{
		Adapters: map[string]modelbroker.ModelAdapter{"stream": adapter},
		Secrets:  modelbroker.StaticResolver{"stream": "unused"},
	})
	orch := &Orchestrator{spine: cs, models: broker, route: ModelRoute{Provider: "stream", Model: "stream-model", Secret: "stream"}}
	st := &attemptState{
		attempt:    AttemptDescriptor{RunID: contracts.RunID(runID), AttemptID: contracts.AttemptID(pinnedID("att")), Fence: 1},
		tenant:     tenant,
		sessionID:  sessionID,
		responseID: responseID,
		ch:         &recordingChannel{},
	}

	requestID := pinnedID("mreq")
	frame := contracts.EngineFrame{Type: "model.request", Data: map[string]any{
		"model_request_id": requestID,
		"messages": []any{
			map[string]any{"role": "user", "content": "stream it"},
		},
	}}

	if _, err := orch.dispatchModel(ctx, st, frame); err != nil {
		t.Fatalf("dispatchModel: %v", err)
	}

	sctx := storage.WithSystemScope(ctx)
	pool := cs.Pool()

	// (1) and (2): the deltas, read back in the order a client would replay them.
	rows, err := pool.Query(sctx,
		`SELECT seq, payload->>'text', payload->>'model_request_id' FROM events
		  WHERE session_id=$1 AND type='model_step.delta.v1' ORDER BY seq`, sessionID)
	if err != nil {
		t.Fatalf("query deltas: %v", err)
	}
	defer rows.Close()
	var (
		got       strings.Builder
		deltaSeqs []int64
	)
	for rows.Next() {
		var seq int64
		var text, mreq string
		if err := rows.Scan(&seq, &text, &mreq); err != nil {
			t.Fatalf("scan delta: %v", err)
		}
		if mreq != requestID {
			t.Fatalf("delta at seq %d carries model_request_id %q, want %q — a delta that cannot be "+
				"attributed to its step is not correlatable by a client", seq, mreq, requestID)
		}
		deltaSeqs = append(deltaSeqs, seq)
		got.WriteString(text)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate deltas: %v", err)
	}

	if len(deltaSeqs) == 0 {
		t.Fatal("no model_step.delta.v1 was journalled: the registry declares the event and dispatch " +
			"streamed text, so a client saw created -> completed with nothing in between — the defect this " +
			"test exists for")
	}
	if len(deltaSeqs) < 2 {
		t.Fatalf("journalled %d delta(s): 12 KiB across a %d-byte flush threshold must produce more than "+
			"one window, and a single-window result cannot distinguish a lossless coalescer from one that "+
			"keeps only the last window", len(deltaSeqs), deltaFlushBytes)
	}
	if got.String() != want {
		t.Fatalf("concatenated deltas = %d bytes, want %d — the coalescer dropped, duplicated or reordered "+
			"a window (first divergence is what to look at, not the length alone)", got.Len(), len(want))
	}

	// (3) the ordering property: strictly inside the step's own bracket.
	var createdSeq, completedSeq int64
	if err := pool.QueryRow(sctx,
		`SELECT seq FROM events WHERE session_id=$1 AND type='model_step.created.v1' ORDER BY seq LIMIT 1`,
		sessionID).Scan(&createdSeq); err != nil {
		t.Fatalf("read model_step.created.v1 seq: %v", err)
	}
	if err := pool.QueryRow(sctx,
		`SELECT seq FROM events WHERE session_id=$1 AND type='model_step.completed.v1' ORDER BY seq DESC LIMIT 1`,
		sessionID).Scan(&completedSeq); err != nil {
		t.Fatalf("read model_step.completed.v1 seq: %v", err)
	}
	for _, seq := range deltaSeqs {
		if seq <= createdSeq || seq >= completedSeq {
			t.Fatalf("delta at seq %d is outside its step's bracket (created=%d, completed=%d): a delta "+
				"journalled after its own terminal event reaches a client out of order",
				seq, createdSeq, completedSeq)
		}
	}
}
