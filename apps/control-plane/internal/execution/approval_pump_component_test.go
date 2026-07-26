//go:build component

package execution

import (
	"context"
	"errors"
	"os"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/palgroup/palai/adapters/repositories"
	"github.com/palgroup/palai/packages/contracts"
	"github.com/palgroup/palai/packages/coordinator"
	statemachines "github.com/palgroup/palai/packages/state-machines"

	"github.com/palgroup/palai/storage"
)

// The APV-001 boundary-pump wiring proof (spec §22.4-22.5). Everything here drives the REAL pump against
// a REAL spine, and every approve enters through AcceptCommand — the SAME entry POST
// /v1/sessions/{id}/commands takes. That is the point: applyBoundaryApproval was fully written, carried
// the one-shot hash check and the ErrCommandNotPending race semantics, and was unreachable, because the
// pump's own boundary read (PendingBoundaryCommands) never returned an approve. Every existing approval
// test called ApplyApprovalDecision DIRECTLY, so none of them could see it.

// approvalPumpHarness is a real spine + an active session + a running root run + one pending publication.
type approvalPumpHarness struct {
	orch      *Orchestrator
	tenant    coordinator.Tenant
	sessionID string
	runID     string
	respID    string
	pub       coordinator.Publication
}

func newApprovalPumpHarness(t *testing.T) *approvalPumpHarness {
	t.Helper()
	url := os.Getenv("PALAI_COMPONENT_POSTGRES_URL")
	if url == "" {
		t.Skip("PALAI_COMPONENT_POSTGRES_URL is required; run TEST=postgres scripts/test/component")
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

	h := &approvalPumpHarness{
		orch:      &Orchestrator{spine: cs},
		tenant:    coordinator.Tenant{Organization: redeliveryID("org"), Project: redeliveryID("prj")},
		sessionID: redeliveryID("ses"),
		runID:     redeliveryID("run"),
		respID:    redeliveryID("resp"),
	}
	pool := cs.Pool()
	execSQL(t, pool, `INSERT INTO organizations (id) VALUES ($1)`, h.tenant.Organization)
	execSQL(t, pool, `INSERT INTO projects (id, organization_id) VALUES ($1, $2)`, h.tenant.Project, h.tenant.Organization)
	execSQL(t, pool, `INSERT INTO sessions (id, organization_id, project_id, state) VALUES ($1, $2, $3, 'active')`,
		h.sessionID, h.tenant.Organization, h.tenant.Project)
	execSQL(t, pool, `INSERT INTO responses (id, organization_id, project_id, session_id, state) VALUES ($1,$2,$3,$4,'in_progress')`,
		h.respID, h.tenant.Organization, h.tenant.Project, h.sessionID)
	execSQL(t, pool, `INSERT INTO runs (id, organization_id, project_id, session_id, response_id, state) VALUES ($1,$2,$3,$4,$5,'running')`,
		h.runID, h.tenant.Organization, h.tenant.Project, h.sessionID, h.respID)
	h.pub = h.requestPush(t, "abc123")
	return h
}

// requestPush records a pending publication the way the push tool does, so the approve has a real target
// carrying a real one-shot request hash.
func (h *approvalPumpHarness) requestPush(t *testing.T, head string) coordinator.Publication {
	t.Helper()
	remote, branch, base := "git@h:o/r", "agent/s/r", "main"
	pub, err := h.orch.spine.RequestPublication(context.Background(), h.tenant, coordinator.PublicationRequest{
		PublicationID: redeliveryID("pub"), ApprovalID: redeliveryID("apr"), SessionID: h.sessionID, RunID: h.runID,
		ResponseID: h.respID, Operation: "push_branch", Remote: remote, Branch: branch, Base: base, HeadSHA: head,
		IdempotencyKey: repositories.IdempotencyKey(h.tenant.Organization, h.tenant.Project, h.runID, repositories.OpPushBranch, remote, branch, base, head),
		RequestHash:    repositories.RequestHash(h.tenant.Organization, h.tenant.Project, h.runID, repositories.OpPushBranch, remote, branch, base, head),
		Display:        "push " + branch,
	})
	if err != nil {
		t.Fatalf("RequestPublication() error = %v", err)
	}
	return pub
}

// accept posts an approve/deny through the ordinary command path — the entry the HTTP surface uses. It
// fails the test unless the command was durably QUEUED: a rejected command proves nothing about the pump.
func (h *approvalPumpHarness) accept(t *testing.T, kind, requestHash string) string {
	t.Helper()
	id := redeliveryID("cmd")
	acc, err := h.orch.spine.AcceptCommand(context.Background(), h.tenant, h.sessionID, coordinator.CommandInput{
		CommandID: id, Kind: kind, Payload: []byte(`{"request_hash":"` + requestHash + `"}`),
	})
	if err != nil {
		t.Fatalf("AcceptCommand(%s) error = %v", kind, err)
	}
	if acc.State != "queued" {
		t.Fatalf("AcceptCommand(%s) state = %q, want queued (with a pending approval it proceeds)", kind, acc.State)
	}
	return id
}

// attempt builds the per-attempt state the pump drives, with its own recording channel.
func (h *approvalPumpHarness) attempt() (*attemptState, *recordingChannel) {
	ch := &recordingChannel{}
	return &attemptState{
		attempt:    AttemptDescriptor{RunID: contracts.RunID(h.runID), AttemptID: contracts.AttemptID(redeliveryID("att"))},
		tenant:     h.tenant,
		sessionID:  h.sessionID,
		responseID: h.respID,
		ch:         ch,
	}, ch
}

func scalar(t *testing.T, pool *pgxpool.Pool, sql string, args ...any) string {
	t.Helper()
	var v string
	if err := pool.QueryRow(storage.WithSystemScope(context.Background()), sql, args...).Scan(&v); err != nil {
		t.Fatalf("query %q error = %v", sql, err)
	}
	return v
}

func publicationState(t *testing.T, h *approvalPumpHarness) string {
	t.Helper()
	return scalar(t, h.orch.spine.Pool(), `SELECT state FROM publications WHERE id=$1`, h.pub.ID)
}

func commandState(t *testing.T, h *approvalPumpHarness, commandID string) string {
	t.Helper()
	return scalar(t, h.orch.spine.Pool(), `SELECT state FROM commands WHERE id=$1`, commandID)
}

func eventCount(t *testing.T, h *approvalPumpHarness, eventType string) int {
	t.Helper()
	var n int
	if err := h.orch.spine.Pool().QueryRow(storage.WithSystemScope(context.Background()),
		`SELECT count(*) FROM events WHERE session_id=$1 AND type=$2`, h.sessionID, eventType).Scan(&n); err != nil {
		t.Fatalf("count %s error = %v", eventType, err)
	}
	return n
}

// TestBoundaryApprovalAppliesAQueuedApprove is the wiring proof: an approve accepted through the ordinary
// command path is applied by the boundary pump — the publication reaches the DURABLE approved state the
// approval pump publishes from, and the command settles. Before the fix the pump's boundary read filtered
// the approve out, so this run left the publication pending_approval and the command queued forever.
func TestBoundaryApprovalAppliesAQueuedApprove(t *testing.T) {
	ctx := context.Background()
	h := newApprovalPumpHarness(t)
	cmdID := h.accept(t, "approve", h.pub.RequestHash)

	st, ch := h.attempt()
	if err := h.orch.pumpCommands(ctx, st, "mr_step2"); err != nil {
		t.Fatalf("pumpCommands() error = %v", err)
	}

	if got := publicationState(t, h); got != "approved" {
		t.Fatalf("publication state after the boundary = %q, want approved (the HTTP approve path is dead code otherwise)", got)
	}
	if got := commandState(t, h, cmdID); got != "applied" {
		t.Fatalf("approve command state = %q, want applied", got)
	}
	approved, err := h.orch.spine.ApprovedPublicationsForRun(ctx, h.tenant, h.runID)
	if err != nil {
		t.Fatalf("ApprovedPublicationsForRun() error = %v", err)
	}
	if len(approved) != 1 || approved[0].ID != h.pub.ID {
		t.Fatalf("approved publications = %+v, want exactly %s (what pumpApprovedPublications publishes)", approved, h.pub.ID)
	}
	if n := eventCount(t, h, "approval.approved.v1"); n != 1 {
		t.Fatalf("approval.approved.v1 events = %d, want exactly 1", n)
	}
	// An approve is not a message: it emits no engine frame.
	if len(ch.sent) != 0 {
		t.Fatalf("the pump sent %d engine frames for an approve, want none: %+v", len(ch.sent), ch.sent)
	}

	// Deliver-once: the command left 'queued', so a second boundary re-reads a drained set and applies
	// nothing twice — no second approval event, no second transition.
	st2, _ := h.attempt()
	if err := h.orch.pumpCommands(ctx, st2, "mr_step3"); err != nil {
		t.Fatalf("second pumpCommands() error = %v", err)
	}
	if n := eventCount(t, h, "approval.approved.v1"); n != 1 {
		t.Fatalf("approval.approved.v1 events after a second boundary = %d, want still 1 (deliver-once)", n)
	}
}

// TestBoundaryApprovalAppliesAQueuedDeny is the same wiring for the deny half: the publication is blocked and
// never joins the publishable set.
func TestBoundaryApprovalAppliesAQueuedDeny(t *testing.T) {
	ctx := context.Background()
	h := newApprovalPumpHarness(t)
	cmdID := h.accept(t, "deny", h.pub.RequestHash)

	st, _ := h.attempt()
	if err := h.orch.pumpCommands(ctx, st, "mr_step2"); err != nil {
		t.Fatalf("pumpCommands() error = %v", err)
	}
	if got := publicationState(t, h); got != "denied" {
		t.Fatalf("publication state after the boundary = %q, want denied", got)
	}
	if got := commandState(t, h, cmdID); got != "applied" {
		t.Fatalf("deny command state = %q, want applied", got)
	}
	approved, _ := h.orch.spine.ApprovedPublicationsForRun(ctx, h.tenant, h.runID)
	if len(approved) != 0 {
		t.Fatalf("approved publications after a deny = %d, want 0", len(approved))
	}
}

// TestBoundaryApprovalStaleHashSettlesWithoutApproving: an approve carrying a request hash that does not
// match the pending approval (the head moved, the request was edited) authorizes NOTHING — and still
// SETTLES, or the pump re-attempts it at every boundary for the life of the run. Both halves are load
// bearing; the store's own comment claimed the second one while the transaction rolled it back.
func TestBoundaryApprovalStaleHashSettlesWithoutApproving(t *testing.T) {
	ctx := context.Background()
	h := newApprovalPumpHarness(t)
	cmdID := h.accept(t, "approve", "req_stale_mismatch")

	st, _ := h.attempt()
	if err := h.orch.pumpCommands(ctx, st, "mr_step2"); err != nil {
		t.Fatalf("pumpCommands() error = %v", err)
	}
	if got := publicationState(t, h); got != "pending_approval" {
		t.Fatalf("publication state after a stale approve = %q, want pending_approval (REP-009: it authorizes nothing)", got)
	}
	if got := commandState(t, h, cmdID); got != "applied" {
		t.Fatalf("stale approve command state = %q, want applied (it must SETTLE, not sit queued and re-attempt)", got)
	}
	if n := eventCount(t, h, "approval.approved.v1"); n != 0 {
		t.Fatalf("approval.approved.v1 events after a stale approve = %d, want 0", n)
	}
}

// TestBoundaryApprovalWithNoPendingApprovalSettles: the race the pump's ErrCommandNotPending mapping is
// written for, from the other side — by the time the boundary applies it, another path already resolved
// the publication. The decision transitions nothing and the command still settles.
func TestBoundaryApprovalWithNoPendingApprovalSettles(t *testing.T) {
	ctx := context.Background()
	h := newApprovalPumpHarness(t)
	cmdID := h.accept(t, "approve", h.pub.RequestHash)
	// Something else resolved the publication between accept and the boundary.
	execSQL(t, h.orch.spine.Pool(), `UPDATE publications SET state='denied' WHERE id=$1`, h.pub.ID)

	st, _ := h.attempt()
	if err := h.orch.pumpCommands(ctx, st, "mr_step2"); err != nil {
		t.Fatalf("pumpCommands() error = %v", err)
	}
	if got := commandState(t, h, cmdID); got != "applied" {
		t.Fatalf("command state with no pending approval = %q, want applied (settled, transitioning nothing)", got)
	}
	if got := publicationState(t, h); got != "denied" {
		t.Fatalf("publication state = %q, want denied (untouched by the settle)", got)
	}
}

// TestBoundaryApprovalRacesTheDirectDecisionPath drives BOTH approval paths at one pending
// publication concurrently: the Slack path calls ApplyApprovalDecision directly on the command it
// accepted, and the boundary pump drains that same queued command. The comment on applyBoundaryApproval
// asserts ErrCommandNotPending makes the loser a no-op; this runs it rather than trusting it. Exactly one
// approval.approved.v1, one approved publication, one settled command, and no error from either side.
func TestBoundaryApprovalRacesTheDirectDecisionPath(t *testing.T) {
	ctx := context.Background()
	h := newApprovalPumpHarness(t)
	cmdID := h.accept(t, "approve", h.pub.RequestHash)

	st, _ := h.attempt()
	var wg sync.WaitGroup
	var pumpErr, directErr error
	start := make(chan struct{})
	wg.Add(2)
	go func() {
		defer wg.Done()
		<-start
		pumpErr = h.orch.pumpCommands(ctx, st, "mr_step2")
	}()
	go func() {
		defer wg.Done()
		<-start
		_, directErr = h.orch.spine.ApplyApprovalDecision(ctx, h.tenant, h.sessionID, h.respID, h.runID, cmdID, "approve", h.pub.RequestHash)
	}()
	close(start)
	wg.Wait()

	if pumpErr != nil {
		t.Fatalf("the boundary pump lost the race with an error = %v, want a clean no-op", pumpErr)
	}
	// The direct path may legitimately report ErrCommandNotPending when it loses — its caller decides.
	if directErr != nil && !errors.Is(directErr, coordinator.ErrCommandNotPending) {
		t.Fatalf("the direct decision path error = %v, want nil or ErrCommandNotPending", directErr)
	}
	// Which side won is scheduling, so it is LOGGED, never asserted — the invariants below hold either
	// way, and -count=N over a real database is what actually walks both orderings.
	if directErr != nil {
		t.Log("the boundary pump won this ordering; the direct path found the command already settled")
	} else {
		t.Log("the direct path won this ordering (or ran uncontended); the pump's read/apply was the no-op")
	}
	if got := publicationState(t, h); got != "approved" {
		t.Fatalf("publication state after the race = %q, want approved exactly once", got)
	}
	if got := commandState(t, h, cmdID); got != "applied" {
		t.Fatalf("command state after the race = %q, want applied", got)
	}
	if n := eventCount(t, h, "approval.approved.v1"); n != 1 {
		t.Fatalf("approval.approved.v1 events after the race = %d, want exactly 1 (single-winner)", n)
	}
	if n := eventCount(t, h, "command.applied.v1"); n != 1 {
		t.Fatalf("command.applied.v1 events after the race = %d, want exactly 1", n)
	}
}

// TestBoundaryApprovalPumpWinsAndTheDirectPathSeesASettledCommand pins the ordering the concurrent test
// above cannot force (the pump does three queries before it contends, so the direct path wins every real
// scheduling): the pump applies FIRST, and the direct caller — the Slack decision path, a moment behind —
// gets ErrCommandNotPending for a decision that IS durable. That ambiguity is the whole reason the Slack
// path re-reads the publication instead of treating the error as a failure: an EXPIRY sweep settles a
// queued command exactly the same way (the second half below) having decided NOTHING.
func TestBoundaryApprovalPumpWinsAndTheDirectPathSeesASettledCommand(t *testing.T) {
	ctx := context.Background()
	h := newApprovalPumpHarness(t)
	cmdID := h.accept(t, "approve", h.pub.RequestHash)

	st, _ := h.attempt()
	if err := h.orch.pumpCommands(ctx, st, "mr_step2"); err != nil {
		t.Fatalf("pumpCommands() error = %v", err)
	}
	_, err := h.orch.spine.ApplyApprovalDecision(ctx, h.tenant, h.sessionID, h.respID, h.runID, cmdID, "approve", h.pub.RequestHash)
	if !errors.Is(err, coordinator.ErrCommandNotPending) {
		t.Fatalf("the direct path after the pump won = %v, want ErrCommandNotPending", err)
	}
	// The decision landed — the caller that saw that error must NOT report a failure.
	state, found, err := h.orch.spine.PublicationState(ctx, h.tenant, h.pub.ID)
	if err != nil || !found || state != "approved" {
		t.Fatalf("PublicationState = (%q,%v,%v), want approved — the pump applied the same command", state, found, err)
	}

	// The other settler of a queued approve, on a SECOND publication: the run terminalizes and the expiry
	// sweep takes the command. Same ErrCommandNotPending, and the publication has NOT moved.
	h.pub = h.requestPush(t, "def456")
	stranded := h.accept(t, "approve", h.pub.RequestHash)
	if _, err := h.orch.spine.ApplyRunTransition(ctx, h.tenant, h.runID, statemachines.RunCmdComplete); err != nil {
		t.Fatalf("ApplyRunTransition(complete) error = %v", err)
	}
	_, err = h.orch.spine.ApplyApprovalDecision(ctx, h.tenant, h.sessionID, h.respID, h.runID, stranded, "approve", h.pub.RequestHash)
	if err == nil {
		t.Fatal("the direct path on an EXPIRED command returned nil, want an error — nothing was decided")
	}
	state, found, perr := h.orch.spine.PublicationState(ctx, h.tenant, h.pub.ID)
	if perr != nil || !found || state != "pending_approval" {
		t.Fatalf("PublicationState = (%q,%v,%v), want pending_approval — an expired command authorizes nothing", state, found, perr)
	}
}

// TestBoundaryApprovalExpiresWhenTheRunTerminalizes: an approve that never reached a boundary must NOT
// survive its run. An approval is bound to one publication, in one run, against that run's workspace —
// letting it sit queued would apply a human's decision to a later run they never saw. It expires on a
// CLEAN completion too (the case where send_message deliberately carries): the run whose operation was
// being authorized is over either way.
func TestBoundaryApprovalExpiresWhenTheRunTerminalizes(t *testing.T) {
	ctx := context.Background()
	h := newApprovalPumpHarness(t)
	cmdID := h.accept(t, "approve", h.pub.RequestHash)

	if _, err := h.orch.spine.ApplyRunTransition(ctx, h.tenant, h.runID, statemachines.RunCmdComplete); err != nil {
		t.Fatalf("ApplyRunTransition(complete) error = %v", err)
	}
	if got := commandState(t, h, cmdID); got != "expired" {
		t.Fatalf("approve command state after a clean run terminal = %q, want expired (an approval never carries to another run)", got)
	}
	if got := publicationState(t, h); got != "pending_approval" {
		t.Fatalf("publication state = %q, want pending_approval (an expired approve authorized nothing)", got)
	}
}
