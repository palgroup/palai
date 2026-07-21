//go:build component

package execution

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"io"
	"os"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/palgroup/palai/packages/contracts"
	"github.com/palgroup/palai/packages/coordinator"
)

// The E10 Task 2 redelivery-wiring proof (spec §26.9): drive the boundary pump against a REAL spine
// and assert a message delivered by a prior attempt is redelivered on a fresh attempt at exactly its
// original boundary, exactly once across reclaims, without the original attempt double-delivering.
// The crash is simulated at the seam — a fresh attemptState with a fresh channel, the same durable
// rows — so it is deterministic; the real-engine kill-mid-fold + resume is the gated fault-live half.

// recordingChannel captures the controller frames the pump sends (message.deliver), so the test can
// assert what the engine would fold. Receive is never used by the pump.
type recordingChannel struct{ sent []contracts.EngineFrame }

func (c *recordingChannel) Send(_ context.Context, f contracts.EngineFrame) error {
	c.sent = append(c.sent, f)
	return nil
}
func (c *recordingChannel) Receive(context.Context) (contracts.EngineFrame, error) {
	return contracts.EngineFrame{}, io.EOF
}
func (c *recordingChannel) Close() error { return nil }

func (c *recordingChannel) delivers(commandID string) []string {
	var msgs []string
	for _, f := range c.sent {
		if f.Type != "message.deliver" {
			continue
		}
		if id, _ := f.Data["command_id"].(string); id == commandID {
			msgs = append(msgs, asString(f.Data["message"]))
		}
	}
	return msgs
}

// deliverOrder is the ordered command_ids of the message.deliver frames sent — the fold order the
// engine reconstructs from.
func (c *recordingChannel) deliverOrder() []string {
	var ids []string
	for _, f := range c.sent {
		if f.Type != "message.deliver" {
			continue
		}
		if id, _ := f.Data["command_id"].(string); id != "" {
			ids = append(ids, id)
		}
	}
	return ids
}

func asString(v any) string {
	s, _ := v.(string)
	return s
}

func redeliveryID(prefix string) string {
	var raw [8]byte
	_, _ = rand.Read(raw[:])
	return prefix + "_" + hex.EncodeToString(raw[:])
}

// redeliveryHarness is a real spine + a seeded active run + one queued send_message command.
type redeliveryHarness struct {
	orch      *Orchestrator
	tenant    coordinator.Tenant
	sessionID string
	runID     string
	commandID string
	message   string
}

func newRedeliveryHarness(t *testing.T, delivery string) *redeliveryHarness {
	t.Helper()
	url := os.Getenv("PALAI_COMPONENT_POSTGRES_URL")
	if url == "" {
		t.Skip("PALAI_COMPONENT_POSTGRES_URL is required; run make test-component TEST=postgres")
	}
	ctx := context.Background()
	cs, err := coordinator.Open(ctx, url)
	if err != nil {
		t.Fatalf("coordinator.Open() error = %v", err)
	}
	t.Cleanup(cs.Close)
	if err := cs.Migrate(ctx); err != nil {
		t.Fatalf("Migrate() error = %v", err)
	}

	h := &redeliveryHarness{
		orch:      &Orchestrator{spine: cs},
		tenant:    coordinator.Tenant{Organization: redeliveryID("org"), Project: redeliveryID("prj")},
		sessionID: redeliveryID("ses"),
		runID:     redeliveryID("run"),
		commandID: redeliveryID("cmd"),
		message:   "also do Y",
	}
	pool := cs.Pool()
	execSQL(t, pool, `INSERT INTO organizations (id) VALUES ($1)`, h.tenant.Organization)
	execSQL(t, pool, `INSERT INTO projects (id, organization_id) VALUES ($1, $2)`, h.tenant.Project, h.tenant.Organization)
	execSQL(t, pool, `INSERT INTO sessions (id, organization_id, project_id) VALUES ($1, $2, $3)`, h.sessionID, h.tenant.Organization, h.tenant.Project)
	execSQL(t, pool, `INSERT INTO runs (id, organization_id, project_id, session_id, state) VALUES ($1, $2, $3, $4, 'running')`, h.runID, h.tenant.Organization, h.tenant.Project, h.sessionID)
	h.commandID = h.enqueue(t, delivery, h.message)
	return h
}

// enqueue inserts another queued send_message command for the run and returns its id — a message
// accepted at this boundary (the first at harness build, more during a simulated outage).
func (h *redeliveryHarness) enqueue(t *testing.T, delivery, message string) string {
	t.Helper()
	id := redeliveryID("cmd")
	execSQL(t, h.orch.spine.Pool(),
		`INSERT INTO commands (id, organization_id, project_id, session_id, run_id, kind, delivery, payload, state)
		 VALUES ($1, $2, $3, $4, $5, 'send_message', $6, jsonb_build_object('message', $7::text), 'queued')`,
		id, h.tenant.Organization, h.tenant.Project, h.sessionID, h.runID, delivery, message)
	return id
}

func execSQL(t *testing.T, pool *pgxpool.Pool, sql string, args ...any) {
	t.Helper()
	if _, err := pool.Exec(context.Background(), sql, args...); err != nil {
		t.Fatalf("exec %q error = %v", sql, err)
	}
}

// attemptAt builds a fresh per-attempt state with its own channel — a new attempt reconstructs from
// scratch, so each gets a clean recordingChannel.
func (h *redeliveryHarness) attemptAt() (*attemptState, *recordingChannel) {
	ch := &recordingChannel{}
	st := &attemptState{
		attempt:    AttemptDescriptor{RunID: contracts.RunID(h.runID), AttemptID: contracts.AttemptID(redeliveryID("att"))},
		tenant:     h.tenant,
		sessionID:  h.sessionID,
		responseID: "",
		ch:         ch,
	}
	return st, ch
}

const foldBoundary = "mr_step2"

// TestAppliedMessageSurvivesCrashBeforeFoldCommit (REC-002): a message applied at a boundary, then a
// crash BEFORE the folding step commits, is redelivered EXACTLY ONCE on the fresh attempt at the same
// boundary — and the original attempt that delivered it from the queue does NOT also redeliver it.
func TestAppliedMessageSurvivesCrashBeforeFoldCommit(t *testing.T) {
	ctx := context.Background()
	h := newRedeliveryHarness(t, "steer")

	// Prior attempt: the queued command is applied and delivered once. The durable row is written;
	// the fold is NOT committed (the crash), so the row stays 'delivered'.
	stA, chA := h.attemptAt()
	if err := h.orch.pumpCommands(ctx, stA, foldBoundary); err != nil {
		t.Fatalf("prior attempt pumpCommands() error = %v", err)
	}
	if got := chA.delivers(h.commandID); len(got) != 1 || got[0] != h.message {
		t.Fatalf("prior attempt delivered %v, want exactly one %q (queue path, no self-redelivery)", got, h.message)
	}

	// Fresh attempt (reclaim): the command is already applied (drained), so the queue read is empty;
	// only the durable row redelivers it — exactly once, at the same boundary.
	stB, chB := h.attemptAt()
	if err := h.orch.pumpCommands(ctx, stB, foldBoundary); err != nil {
		t.Fatalf("reclaim pumpCommands() error = %v", err)
	}
	if got := chB.delivers(h.commandID); len(got) != 1 || got[0] != h.message {
		t.Fatalf("reclaim redelivered %v, want exactly one %q from the durable row", got, h.message)
	}
}

// TestRedeliveryIsExactlyOnceAcrossTwoReclaims: two successive crashes each reconstruct the
// conversation with the turn once — never accumulating duplicates.
func TestRedeliveryIsExactlyOnceAcrossTwoReclaims(t *testing.T) {
	ctx := context.Background()
	h := newRedeliveryHarness(t, "queue")

	stA, _ := h.attemptAt()
	if err := h.orch.pumpCommands(ctx, stA, foldBoundary); err != nil {
		t.Fatalf("prior attempt pumpCommands() error = %v", err)
	}
	for i := 0; i < 2; i++ {
		st, ch := h.attemptAt()
		if err := h.orch.pumpCommands(ctx, st, foldBoundary); err != nil {
			t.Fatalf("reclaim %d pumpCommands() error = %v", i, err)
		}
		if got := ch.delivers(h.commandID); len(got) != 1 {
			t.Fatalf("reclaim %d redelivered %v, want exactly one", i, got)
		}
	}
}

// TestAppliedFoldedTurnPresentInPostResumeHistory (REC-003/R1): a message applied+folded+committed,
// then resumed, is still present in the reconstructed conversation at its canonical boundary — the
// folded row redelivers too, so the model sees the turn on every post-resume step.
func TestAppliedFoldedTurnPresentInPostResumeHistory(t *testing.T) {
	ctx := context.Background()
	h := newRedeliveryHarness(t, "queue")

	// Prior attempt delivers, then the following step commits: the row transitions to 'folded'.
	stA, _ := h.attemptAt()
	if err := h.orch.pumpCommands(ctx, stA, foldBoundary); err != nil {
		t.Fatalf("prior attempt pumpCommands() error = %v", err)
	}
	if err := h.orch.spine.CommitModelRequest(ctx, h.tenant, h.sessionID, "", h.runID, "mr_step3", "model_step.created.v1", []byte(`{}`)); err != nil {
		t.Fatalf("CommitModelRequest() error = %v", err)
	}
	if _, err := h.orch.spine.CommitModelResult(ctx, h.tenant, h.sessionID, "", h.runID, "mr_step3", []byte(`{"output":"ok"}`), "model_step.completed.v1", []byte(`{}`)); err != nil {
		t.Fatalf("CommitModelResult() error = %v", err)
	}
	if got := foldStateOf(t, h, h.commandID); got != "folded" {
		t.Fatalf("after commit fold_state = %q, want folded", got)
	}

	// Resume: the folded turn is redelivered at its original boundary (present in post-resume history).
	st, ch := h.attemptAt()
	if err := h.orch.pumpCommands(ctx, st, foldBoundary); err != nil {
		t.Fatalf("resume pumpCommands() error = %v", err)
	}
	if got := ch.delivers(h.commandID); len(got) != 1 || got[0] != h.message {
		t.Fatalf("resume redelivered %v, want the folded turn %q present once", got, h.message)
	}
}

// TestRedeliveryNeverInjectsIntoReconstructedStep (§26.9 negative): a message is redelivered ONLY at
// its own recorded boundary. Pumping a DIFFERENT boundary emits nothing, so the turn is never spliced
// into some other reconstructed step — it folds at its own input boundary or not at all.
func TestRedeliveryNeverInjectsIntoReconstructedStep(t *testing.T) {
	ctx := context.Background()
	h := newRedeliveryHarness(t, "queue")

	stA, _ := h.attemptAt()
	if err := h.orch.pumpCommands(ctx, stA, foldBoundary); err != nil {
		t.Fatalf("prior attempt pumpCommands() error = %v", err)
	}

	// A different boundary on the fresh attempt must not redeliver this message.
	stB, chB := h.attemptAt()
	if err := h.orch.pumpCommands(ctx, stB, "mr_step7"); err != nil {
		t.Fatalf("wrong-boundary pumpCommands() error = %v", err)
	}
	if got := chB.delivers(h.commandID); len(got) != 0 {
		t.Fatalf("wrong-boundary redelivered %v, want none (never injected into another step)", got)
	}

	// Its own boundary still redelivers it, at the input boundary.
	stC, chC := h.attemptAt()
	if err := h.orch.pumpCommands(ctx, stC, foldBoundary); err != nil {
		t.Fatalf("own-boundary pumpCommands() error = %v", err)
	}
	if got := chC.delivers(h.commandID); len(got) != 1 {
		t.Fatalf("own-boundary redelivered %v, want exactly one", got)
	}
}

// TestRedeliveryOrderStableAcrossReclaimWithFreshMessage: when a boundary carries BOTH a prior
// message and a message queued during the outage, the fold order must be IDENTICAL across every
// reclaim — a flipped order rebuilds a different request for a committed step (LookupModelResult
// replays by id without hash-checking the request), a silent divergence. Redelivery reads before the
// fresh drain, so a prior message always folds before a message applied later at the same boundary.
func TestRedeliveryOrderStableAcrossReclaimWithFreshMessage(t *testing.T) {
	ctx := context.Background()
	h := newRedeliveryHarness(t, "queue") // command M, applied first (lower applied_sequence)

	// Original attempt applies M and delivers it once.
	stA, _ := h.attemptAt()
	if err := h.orch.pumpCommands(ctx, stA, foldBoundary); err != nil {
		t.Fatalf("prior attempt pumpCommands() error = %v", err)
	}
	// A fresh message N is accepted at the SAME boundary during the outage.
	nID := h.enqueue(t, "steer", "then do Z")

	st1, ch1 := h.attemptAt()
	if err := h.orch.pumpCommands(ctx, st1, foldBoundary); err != nil {
		t.Fatalf("reclaim 1 pumpCommands() error = %v", err)
	}
	st2, ch2 := h.attemptAt()
	if err := h.orch.pumpCommands(ctx, st2, foldBoundary); err != nil {
		t.Fatalf("reclaim 2 pumpCommands() error = %v", err)
	}

	order1, order2 := ch1.deliverOrder(), ch2.deliverOrder()
	if !slicesEqual(order1, order2) {
		t.Fatalf("fold order diverged across reclaims: reclaim1=%v reclaim2=%v", order1, order2)
	}
	if want := []string{h.commandID, nID}; !slicesEqual(order1, want) {
		t.Fatalf("reclaim fold order = %v, want canonical applied_sequence order %v (prior before fresh)", order1, want)
	}
}

// TestRedeliveryFailsClosedOnEmptyBoundary: an empty boundary id would write a NULL boundary_request_id
// the redelivery read (= $4) can never match — an unreachable row that turns command.applied.v1 back
// into a lie. The pump fails closed instead: it errors and writes no row. (The engine always sets the
// id; this guards the malformed frame.)
func TestRedeliveryFailsClosedOnEmptyBoundary(t *testing.T) {
	ctx := context.Background()
	h := newRedeliveryHarness(t, "queue")

	st, ch := h.attemptAt()
	if err := h.orch.pumpCommands(ctx, st, ""); err == nil {
		t.Fatal("pumpCommands with empty boundary = nil error, want fail-closed error")
	}
	if got := ch.deliverOrder(); len(got) != 0 {
		t.Fatalf("delivered %v under empty boundary, want none", got)
	}
	var rows int
	if err := h.orch.spine.Pool().QueryRow(ctx,
		`SELECT count(*) FROM delivered_messages WHERE run_id = $1`, h.runID).Scan(&rows); err != nil {
		t.Fatalf("count delivered rows error = %v", err)
	}
	if rows != 0 {
		t.Fatalf("wrote %d delivered rows under empty boundary, want 0 (no unreachable NULL-boundary row)", rows)
	}
}

func slicesEqual(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func foldStateOf(t *testing.T, h *redeliveryHarness, commandID string) string {
	t.Helper()
	var fold string
	if err := h.orch.spine.Pool().QueryRow(context.Background(),
		`SELECT fold_state FROM delivered_messages WHERE command_id = $1`, commandID).Scan(&fold); err != nil {
		t.Fatalf("read fold_state error = %v", err)
	}
	return fold
}
