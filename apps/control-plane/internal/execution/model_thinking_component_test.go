//go:build component

package execution

import (
	"context"
	"strings"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/palgroup/palai/packages/contracts"
	modelbroker "github.com/palgroup/palai/packages/model-broker"

	"github.com/palgroup/palai/storage"
)

// reasoningAdapter emits a REASONING stream and then an answer stream, in that order, because that is the
// order the real provider produces them: Anthropic closes the thinking content block before it opens the
// text one (measured 2026-08-04 — content_block_start {"type":"thinking"}, thinking_delta*, signature_delta,
// then the text block). An adapter that interleaved them would be testing a shape the wire does not have.
//
// Both streams are chunked so the sink is forced to write more than one window of each: a single-window
// result cannot tell a lossless coalescer from one that keeps only the last window.
type reasoningAdapter struct {
	mu        sync.Mutex
	reasoning []string
	answer    []string
}

func (a *reasoningAdapter) Execute(_ context.Context, req modelbroker.Request, _ string, onDelta func(modelbroker.Delta)) (modelbroker.Result, error) {
	a.mu.Lock()
	reasoning := append([]string(nil), a.reasoning...)
	answer := append([]string(nil), a.answer...)
	a.mu.Unlock()
	if onDelta != nil {
		for _, c := range reasoning {
			onDelta(modelbroker.Delta{Thinking: c})
		}
		for _, c := range answer {
			onDelta(modelbroker.Delta{Text: c})
		}
	}
	return modelbroker.Result{
		ModelRequestID: req.ModelRequestID, ProviderRequestID: "reasoning_1", Model: req.Model,
		Output: strings.Join(answer, ""), Thinking: strings.Join(reasoning, ""),
		FinishReason: "stop", Attempts: 1,
	}, nil
}

// TestDispatchJournalsReasoningSeparatelyFromTheAnswer drives dispatchModel — NOT AppendModelStepThinking —
// for the reason the delta test next door states: a test that called the store method directly would prove
// the mechanism and say nothing about whether anything CALLS it, which is the shape every absence defect in
// this tree has had.
//
// THE PROPERTY THAT COSTS SOMETHING IS THE SEPARATION, not the presence. Reasoning could have ridden as a
// field on model_step.delta.v1, and every assertion about it being journalled would still pass. What that
// would break is downstream and silent: the Slack relay writes a delta's `data.text` straight into the
// reply body (apps/slack-bot/internal/relay/relay.go), so a single reasoning byte reaching the delta stream
// puts the model's private working into the answer a person reads. This test refuses that in both
// directions — no reasoning byte in any delta, no answer byte in any thinking event — so the two streams
// cannot be merged later without turning this red.
//
// It also pins the ordering bracket and losslessness, matching the delta test's contract:
//
//	(1) reasoning is journalled at all, as its own type;
//	(2) concatenating the windows in seq order reproduces the reasoning EXACTLY;
//	(3) every reasoning event sits STRICTLY BETWEEN its step's created and completed events;
//	(4) neither stream carries a byte of the other.
func TestDispatchJournalsReasoningSeparatelyFromTheAnswer(t *testing.T) {
	cs, tenant, exec := openPinnedSpine(t)
	ctx := context.Background()
	sessionID, responseID, runID := pinnedID("ses"), pinnedID("resp"), pinnedID("run")
	exec(`INSERT INTO sessions (id, project_id) VALUES ($1,$2)`, sessionID, tenant.Project)
	exec(`INSERT INTO responses (id, project_id, session_id, state) VALUES ($1,$2,$3,'queued')`,
		responseID, tenant.Project, sessionID)
	exec(`INSERT INTO runs (id, project_id, session_id, response_id, state) VALUES ($1,$2,$3,$4,'running')`,
		runID, tenant.Project, sessionID, responseID)

	// The two streams are built from DISJOINT alphabets so the separation assertion is exact rather than
	// approximate: any byte from one appearing in the other is detectable by character class alone, with no
	// substring search that could miss a partial leak at a window boundary.
	reasoningChunks := []string{
		strings.Repeat("r", 4*1024),
		strings.Repeat("s", 4*1024),
		strings.Repeat("t", 4*1024),
	}
	answerChunks := []string{
		strings.Repeat("x", 4*1024),
		strings.Repeat("y", 4*1024),
		strings.Repeat("z", 4*1024),
	}
	wantReasoning := strings.Join(reasoningChunks, "")
	wantAnswer := strings.Join(answerChunks, "")

	adapter := &reasoningAdapter{reasoning: reasoningChunks, answer: answerChunks}
	broker := modelbroker.New(modelbroker.Config{
		Adapters: map[string]modelbroker.ModelAdapter{"reasoning": adapter},
		Secrets:  modelbroker.StaticResolver{"reasoning": "unused"},
	})
	orch := &Orchestrator{spine: cs, models: broker, route: ModelRoute{
		Provider: "reasoning", Model: "reasoning-model", Secret: "reasoning",
		Thinking: modelbroker.ThinkingVisible,
	}}
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
			map[string]any{"role": "user", "content": "reason about it"},
		},
	}}

	if _, err := orch.dispatchModel(ctx, st, frame); err != nil {
		t.Fatalf("dispatchModel: %v", err)
	}

	sctx := storage.WithSystemScope(ctx)
	pool := cs.Pool()

	// (1) and (2): the reasoning windows, read back in the order a client would replay them.
	gotReasoning, thinkingSeqs := collectWindows(t, sctx, pool, sessionID, "model_step.thinking.v1", "thinking", requestID)
	if len(thinkingSeqs) == 0 {
		t.Fatal("no model_step.thinking.v1 was journalled: the adapter streamed reasoning and the registry " +
			"declares the event, so a reader saw the answer with no working behind it — the absence this test exists for")
	}
	if len(thinkingSeqs) < 2 {
		t.Fatalf("journalled %d reasoning window(s): 12 KiB across a %d-byte flush threshold must produce more "+
			"than one, and a single window cannot distinguish a lossless coalescer from one that keeps only the last",
			len(thinkingSeqs), deltaFlushBytes)
	}
	gotAnswer, deltaSeqs := collectWindows(t, sctx, pool, sessionID, "model_step.delta.v1", "text", requestID)
	if len(deltaSeqs) == 0 {
		t.Fatal("no model_step.delta.v1 was journalled: a step that streamed reasoning must still stream its " +
			"answer, and a reasoning sink that swallowed the answer would pass every assertion above")
	}

	// (4) THE SEPARATION, in both directions — CHECKED BEFORE THE EQUALITIES BELOW, ON PURPOSE.
	//
	// Ordering is load-bearing here, and it was measured rather than assumed: with the equality assertions
	// first, a perturbation that leaked reasoning into the answer stream failed as
	// "concatenated answer deltas = 24576 bytes, want 12288" — a BYTE-COUNT problem, when what had actually
	// happened was the model's private working being published into the text a person reads. This tree
	// records that exact defect class (an assertion whose failure points at the wrong file), so the sharper
	// diagnosis has to run first: a leak must report as a leak, not as arithmetic.
	if i := strings.IndexAny(gotAnswer, "rst"); i >= 0 {
		t.Fatalf("a reasoning byte (%q at offset %d) reached the ANSWER delta stream: the Slack relay writes "+
			"delta text straight into the reply body, so this is the model's private working shown to a person",
			gotAnswer[i], i)
	}
	if i := strings.IndexAny(gotReasoning, "xyz"); i >= 0 {
		t.Fatalf("an answer byte (%q at offset %d) reached the REASONING stream: the two streams are not "+
			"separable and a consumer coalescing by event type would rebuild neither correctly",
			gotReasoning[i], i)
	}

	// Losslessness, once the two streams are known not to be contaminated by each other.
	if gotReasoning != wantReasoning {
		t.Fatalf("concatenated reasoning = %d bytes, want %d — the coalescer dropped, duplicated or reordered a window",
			len(gotReasoning), len(wantReasoning))
	}
	if gotAnswer != wantAnswer {
		t.Fatalf("concatenated answer deltas = %d bytes, want %d", len(gotAnswer), len(wantAnswer))
	}

	// (3) the ordering bracket: strictly inside the step's own created/completed pair.
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
	for _, seq := range thinkingSeqs {
		if seq <= createdSeq || seq >= completedSeq {
			t.Fatalf("reasoning event at seq %d is outside its step's bracket (created=%d, completed=%d): "+
				"reasoning journalled after its own terminal event reaches a client out of order",
				seq, createdSeq, completedSeq)
		}
	}
}

// collectWindows reads one event type's windows for a session in seq order and returns them concatenated
// plus their sequences, failing if any window cannot be attributed to the step that produced it.
//
// The type and payload field are parameters so the two streams are read by the SAME code: a helper written
// once per stream could drift, and a divergence between how the answer and the reasoning are read would
// weaken exactly the comparison this test is for.
func collectWindows(t *testing.T, ctx context.Context, pool *pgxpool.Pool, sessionID, eventType, field, requestID string) (string, []int64) {
	t.Helper()
	rows, err := pool.Query(ctx,
		`SELECT seq, payload->>$2, payload->>'model_request_id' FROM events
		  WHERE session_id=$1 AND type=$3 ORDER BY seq`, sessionID, field, eventType)
	if err != nil {
		t.Fatalf("query %s: %v", eventType, err)
	}
	defer rows.Close()
	var (
		got  strings.Builder
		seqs []int64
	)
	for rows.Next() {
		var seq int64
		var text, mreq string
		if err := rows.Scan(&seq, &text, &mreq); err != nil {
			t.Fatalf("scan %s: %v", eventType, err)
		}
		if mreq != requestID {
			t.Fatalf("%s at seq %d carries model_request_id %q, want %q — a window that cannot be attributed "+
				"to its step is not correlatable by a client", eventType, seq, mreq, requestID)
		}
		seqs = append(seqs, seq)
		got.WriteString(text)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate %s: %v", eventType, err)
	}
	return got.String(), seqs
}
