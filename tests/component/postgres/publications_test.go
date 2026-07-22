//go:build component

package postgres

import (
	"context"
	"testing"

	"github.com/palgroup/palai/adapters/repositories"
	"github.com/palgroup/palai/packages/coordinator"

	"github.com/palgroup/palai/storage"
)

// TestApprovalsPublicationsMigration proves 000013 adds its two tables idempotently and reverses
// cleanly (spec §30.8; the 000010/000012 re-run-safety pattern).
func TestApprovalsPublicationsMigration(t *testing.T) {
	cs := openHarness(t)
	ctx := context.Background()
	pool := cs.Pool()

	if err := cs.Migrate(ctx); err != nil {
		t.Fatalf("re-Migrate() error = %v", err)
	}
	for _, name := range []string{"publications", "approvals"} {
		if !tableExists(t, pool, name) {
			t.Fatalf("after apply, %s is missing", name)
		}
	}
	if err := cs.Rollback(ctx); err != nil {
		t.Fatalf("Rollback() error = %v", err)
	}
	for _, name := range []string{"publications", "approvals"} {
		if tableExists(t, pool, name) {
			t.Fatalf("after rollback, %s still exists", name)
		}
	}
	if err := cs.Migrate(ctx); err != nil {
		t.Fatalf("re-Migrate() error = %v", err)
	}
	for _, name := range []string{"publications", "approvals"} {
		if !tableExists(t, pool, name) {
			t.Fatalf("after reapply, %s is missing", name)
		}
	}
}

// TestRequestPublicationIdempotent proves the operation-level idempotency (spec §30.8, decision (b)): a
// duplicate request (same idempotency key) resolves to the ORIGINAL pending publication rather than
// stacking a second approval.
func TestRequestPublicationIdempotent(t *testing.T) {
	cs := openHarness(t)
	ctx := context.Background()
	pool := cs.Pool()
	tenant, sessionID, runID := seedRun(t, pool)

	key := repositories.IdempotencyKey(tenant.Organization, tenant.Project, runID, repositories.OpPushBranch, "git@h:o/r", "agent/s/r", "main", "abc123")
	in := coordinator.PublicationRequest{
		PublicationID: newID("pub"), ApprovalID: newID("apr"), SessionID: sessionID, RunID: runID,
		Operation: "push_branch", Remote: "git@h:o/r", Branch: "agent/s/r", Base: "main", HeadSHA: "abc123",
		IdempotencyKey: key, RequestHash: "req_1", Display: "push agent/s/r",
	}
	first, err := cs.RequestPublication(ctx, tenant, in)
	if err != nil {
		t.Fatalf("first RequestPublication error = %v", err)
	}
	if first.Replayed {
		t.Fatal("a first request must not be a replay")
	}
	dup := in
	dup.PublicationID = newID("pub") // a different row id, but the SAME idempotency key
	dup.ApprovalID = newID("apr")
	second, err := cs.RequestPublication(ctx, tenant, dup)
	if err != nil {
		t.Fatalf("duplicate RequestPublication error = %v", err)
	}
	if !second.Replayed || second.ID != first.ID {
		t.Fatalf("duplicate request = {id:%s replayed:%v}, want the original id %s replayed", second.ID, second.Replayed, first.ID)
	}
	var count int
	if err := pool.QueryRow(storage.WithSystemScope(ctx), `SELECT count(*) FROM publications WHERE run_id=$1`, runID).Scan(&count); err != nil {
		t.Fatalf("count publications error = %v", err)
	}
	if count != 1 {
		t.Fatalf("publications for run = %d, want 1 (idempotent request records no second)", count)
	}
}

// TestPendingApprovalApproveProceedsDenyBlocks proves APV-001: a side-effect tool's pending publication
// makes an approve/deny PROCEED (queued for the boundary) instead of the E08 rejection; approve drives
// the publication to a durable approved state (the pump's drain sees it); deny blocks it; and with NO
// pending approval the E08 no_pending_approval rejection is preserved.
func TestPendingApprovalApproveProceedsDenyBlocks(t *testing.T) {
	cs := openHarness(t)
	ctx := context.Background()
	pool := cs.Pool()
	tenant, sessionID, runID := seedRun(t, pool)
	// A live session + running root run with a response the approve/deny journal scopes to.
	exec(t, pool, `UPDATE sessions SET state='active' WHERE id=$1`, sessionID)
	respID := newID("resp")
	exec(t, pool, `INSERT INTO responses (id, organization_id, project_id, session_id, state) VALUES ($1,$2,$3,$4,'in_progress')`,
		respID, tenant.Organization, tenant.Project, sessionID)
	exec(t, pool, `UPDATE runs SET state='running', response_id=$2 WHERE id=$1`, runID, respID)

	// 1. No pending approval: an approve is accepted-but-rejected (E08 preserved).
	rej, err := cs.AcceptCommand(ctx, tenant, sessionID, coordinator.CommandInput{CommandID: newID("cmd"), Kind: "approve", Payload: []byte(`{}`)})
	if err != nil {
		t.Fatalf("AcceptCommand(approve, no pending) error = %v", err)
	}
	if rej.State != "rejected" {
		t.Fatalf("approve with no pending = %q, want rejected (E08 preserved)", rej.State)
	}

	// 2. A side-effect tool records a pending publication.
	pub := requestPushPublication(t, cs, tenant, sessionID, runID, "abc123")

	// 3. The approve now PROCEEDS: it is queued for the boundary, not rejected.
	approveCmd := coordinator.CommandInput{CommandID: newID("cmd"), Kind: "approve", Payload: approvePayload(pub.RequestHash)}
	acc, err := cs.AcceptCommand(ctx, tenant, sessionID, approveCmd)
	if err != nil {
		t.Fatalf("AcceptCommand(approve, pending) error = %v", err)
	}
	if acc.State != "queued" {
		t.Fatalf("approve with a pending approval = %q, want queued (proceeds)", acc.State)
	}

	// 4. Applied at the boundary: the publication reaches the durable approved state the pump drains.
	if _, err := cs.ApplyApprovalDecision(ctx, tenant, sessionID, respID, runID, approveCmd.CommandID, "approve", pub.RequestHash); err != nil {
		t.Fatalf("ApplyApprovalDecision(approve) error = %v", err)
	}
	approved, err := cs.ApprovedPublicationsForRun(ctx, tenant, runID)
	if err != nil {
		t.Fatalf("ApprovedPublicationsForRun error = %v", err)
	}
	if len(approved) != 1 || approved[0].ID != pub.ID {
		t.Fatalf("approved publications = %+v, want exactly the approved publication %s", approved, pub.ID)
	}

	// 5. deny BLOCKS a fresh pending publication: it never enters the approved (publishable) set.
	pub2 := requestPushPublication(t, cs, tenant, sessionID, runID, "def456")
	denyCmd := coordinator.CommandInput{CommandID: newID("cmd"), Kind: "deny", Payload: approvePayload(pub2.RequestHash)}
	if _, err := cs.AcceptCommand(ctx, tenant, sessionID, denyCmd); err != nil {
		t.Fatalf("AcceptCommand(deny) error = %v", err)
	}
	if _, err := cs.ApplyApprovalDecision(ctx, tenant, sessionID, respID, runID, denyCmd.CommandID, "deny", pub2.RequestHash); err != nil {
		t.Fatalf("ApplyApprovalDecision(deny) error = %v", err)
	}
	var state string
	if err := pool.QueryRow(storage.WithSystemScope(ctx), `SELECT state FROM publications WHERE id=$1`, pub2.ID).Scan(&state); err != nil {
		t.Fatalf("read denied publication error = %v", err)
	}
	if state != "denied" {
		t.Fatalf("denied publication state = %q, want denied (deny blocks)", state)
	}
	// The approved set still holds only the first (approved) publication — the denied one never joined.
	approvedAfter, _ := cs.ApprovedPublicationsForRun(ctx, tenant, runID)
	if len(approvedAfter) != 1 {
		t.Fatalf("approved publications after deny = %d, want 1 (deny did not publish)", len(approvedAfter))
	}
}

// TestMarkPublicationPublishedIdempotent proves the approval pump's mark step (spec §30.9-30.10): an
// approved publication reaches published with its external receipt and one push.completed.v1 event, and
// a re-drive (a lost-ack retry that re-reconciled the remote) updates 0 rows and re-journals nothing —
// so E10's detached execution re-driving the SAME publish never double-journals.
func TestMarkPublicationPublishedIdempotent(t *testing.T) {
	cs := openHarness(t)
	ctx := context.Background()
	pool := cs.Pool()
	tenant, sessionID, runID := seedRun(t, pool)
	respID := newID("resp")
	exec(t, pool, `INSERT INTO responses (id, organization_id, project_id, session_id, state) VALUES ($1,$2,$3,$4,'in_progress')`,
		respID, tenant.Organization, tenant.Project, sessionID)

	pub := requestPushPublication(t, cs, tenant, sessionID, runID, "abc123")
	exec(t, pool, `UPDATE publications SET state='approved' WHERE id=$1`, pub.ID)

	receipt := map[string]any{"remote_sha": "abc123", "branch": "agent/s/r"}
	if err := cs.MarkPublicationPublished(ctx, tenant, sessionID, respID, pub.ID, "push_branch", receipt); err != nil {
		t.Fatalf("MarkPublicationPublished error = %v", err)
	}
	var state string
	if err := pool.QueryRow(storage.WithSystemScope(ctx), `SELECT state FROM publications WHERE id=$1`, pub.ID).Scan(&state); err != nil {
		t.Fatalf("read published state error = %v", err)
	}
	if state != "published" {
		t.Fatalf("publication state = %q, want published", state)
	}
	eventCount := func() int {
		var n int
		if err := pool.QueryRow(storage.WithSystemScope(ctx), `SELECT count(*) FROM events WHERE session_id=$1 AND type='push.completed.v1'`, sessionID).Scan(&n); err != nil {
			t.Fatalf("count push.completed events error = %v", err)
		}
		return n
	}
	if eventCount() != 1 {
		t.Fatalf("push.completed.v1 events = %d, want 1", eventCount())
	}
	// Re-drive: idempotent, no second event, still published.
	if err := cs.MarkPublicationPublished(ctx, tenant, sessionID, respID, pub.ID, "push_branch", receipt); err != nil {
		t.Fatalf("re-drive MarkPublicationPublished error = %v", err)
	}
	if eventCount() != 1 {
		t.Fatalf("push.completed.v1 events after re-drive = %d, want 1 (idempotent)", eventCount())
	}
}

// TestStaleApprovalHashLeavesPublicationPending proves REP-009 at the store level: an approve whose
// one-shot request hash does NOT match the pending approval (a head that moved, or an edited request)
// authorizes nothing — the publication stays pending_approval, never approved. The command still
// settles, but no stale operation is published.
func TestStaleApprovalHashLeavesPublicationPending(t *testing.T) {
	cs := openHarness(t)
	ctx := context.Background()
	pool := cs.Pool()
	tenant, sessionID, runID := seedRun(t, pool)
	exec(t, pool, `UPDATE sessions SET state='active' WHERE id=$1`, sessionID)
	respID := newID("resp")
	exec(t, pool, `INSERT INTO responses (id, organization_id, project_id, session_id, state) VALUES ($1,$2,$3,$4,'in_progress')`,
		respID, tenant.Organization, tenant.Project, sessionID)
	exec(t, pool, `UPDATE runs SET state='running', response_id=$2 WHERE id=$1`, runID, respID)

	pub := requestPushPublication(t, cs, tenant, sessionID, runID, "abc123")
	// The approve command is legitimately queued (a pending approval exists), but it carries a STALE
	// request hash — the head moved after the request, so a new tool call would carry a new hash.
	approveCmd := coordinator.CommandInput{CommandID: newID("cmd"), Kind: "approve", Payload: approvePayload("req_stale_mismatch")}
	if _, err := cs.AcceptCommand(ctx, tenant, sessionID, approveCmd); err != nil {
		t.Fatalf("AcceptCommand(approve) error = %v", err)
	}
	if _, err := cs.ApplyApprovalDecision(ctx, tenant, sessionID, respID, runID, approveCmd.CommandID, "approve", "req_stale_mismatch"); err != nil {
		t.Fatalf("ApplyApprovalDecision(stale hash) error = %v", err)
	}

	var state string
	if err := pool.QueryRow(storage.WithSystemScope(ctx), `SELECT state FROM publications WHERE id=$1`, pub.ID).Scan(&state); err != nil {
		t.Fatalf("read publication state error = %v", err)
	}
	if state != "pending_approval" {
		t.Fatalf("publication state after stale approve = %q, want pending_approval (a stale hash authorizes nothing, REP-009)", state)
	}
	approved, _ := cs.ApprovedPublicationsForRun(ctx, tenant, runID)
	if len(approved) != 0 {
		t.Fatalf("approved publications after stale approve = %d, want 0", len(approved))
	}
}

// TestExpiredApprovalNeverPublishesAndEmitsExpiredEvent proves the approval-expiry enforcement rider
// (spec §22.4, E10 T7): the schema (expires_at + the 'expired' publication state) was forward-declared in
// 000013 and unreachable — T7 adds only the ENFORCEMENT. An APPROVED publication whose one-shot approval
// passed its expiry is expired by the pump's consume-time guard (ExpireApprovalIfElapsed): it never
// publishes, its state becomes 'expired', and exactly one approval.expired.v1 is journaled — while a
// concurrent non-expired approved row is bit-unchanged (still approved, still publishable).
func TestExpiredApprovalNeverPublishesAndEmitsExpiredEvent(t *testing.T) {
	cs := openHarness(t)
	ctx := context.Background()
	pool := cs.Pool()
	tenant, sessionID, runID := seedRun(t, pool)
	respID := newID("resp")
	exec(t, pool, `INSERT INTO responses (id, organization_id, project_id, session_id, state) VALUES ($1,$2,$3,$4,'in_progress')`,
		respID, tenant.Organization, tenant.Project, sessionID)

	// Two approved publications; only the first's approval has elapsed.
	stale := requestPushPublication(t, cs, tenant, sessionID, runID, "aaa111")
	live := requestPushPublication(t, cs, tenant, sessionID, runID, "bbb222")
	exec(t, pool, `UPDATE publications SET state='approved' WHERE id IN ($1,$2)`, stale.ID, live.ID)
	exec(t, pool, `UPDATE approvals SET expires_at = clock_timestamp() - interval '1 minute' WHERE publication_id=$1`, stale.ID)

	// The pump's consume-time guard expires the elapsed one and reports it, so the pump skips its publish.
	expired, err := cs.ExpireApprovalIfElapsed(ctx, tenant, sessionID, respID, stale.ID)
	if err != nil {
		t.Fatalf("ExpireApprovalIfElapsed(stale) error = %v", err)
	}
	if !expired {
		t.Fatal("an elapsed approval must report expired=true (the pump skips the publish)")
	}
	// The live one is untouched — reported not-expired, state unchanged.
	liveExpired, err := cs.ExpireApprovalIfElapsed(ctx, tenant, sessionID, respID, live.ID)
	if err != nil {
		t.Fatalf("ExpireApprovalIfElapsed(live) error = %v", err)
	}
	if liveExpired {
		t.Fatal("a non-elapsed approval must report expired=false (bit-unchanged, still publishable)")
	}

	var staleState, liveState string
	if err := pool.QueryRow(storage.WithSystemScope(ctx), `SELECT state FROM publications WHERE id=$1`, stale.ID).Scan(&staleState); err != nil {
		t.Fatalf("read stale state error = %v", err)
	}
	if err := pool.QueryRow(storage.WithSystemScope(ctx), `SELECT state FROM publications WHERE id=$1`, live.ID).Scan(&liveState); err != nil {
		t.Fatalf("read live state error = %v", err)
	}
	if staleState != "expired" {
		t.Fatalf("stale publication state = %q, want expired", staleState)
	}
	if liveState != "approved" {
		t.Fatalf("live publication state = %q, want approved (bit-unchanged)", liveState)
	}
	// Exactly one approval.expired.v1, for the stale publication only.
	var expiredEvents int
	if err := pool.QueryRow(storage.WithSystemScope(ctx), `SELECT count(*) FROM events WHERE session_id=$1 AND type='approval.expired.v1'`, sessionID).Scan(&expiredEvents); err != nil {
		t.Fatalf("count approval.expired events error = %v", err)
	}
	if expiredEvents != 1 {
		t.Fatalf("approval.expired.v1 events = %d, want 1 (only the stale one)", expiredEvents)
	}
	// The pump's drain no longer offers the expired publication; only the live one remains publishable.
	approved, err := cs.ApprovedPublicationsForRun(ctx, tenant, runID)
	if err != nil {
		t.Fatalf("ApprovedPublicationsForRun error = %v", err)
	}
	if len(approved) != 1 || approved[0].ID != live.ID {
		t.Fatalf("approved set after expiry = %+v, want only the live publication %s", approved, live.ID)
	}
	// A second guard call on the already-expired row is an idempotent no-op — no second event.
	if again, err := cs.ExpireApprovalIfElapsed(ctx, tenant, sessionID, respID, stale.ID); err != nil || again {
		t.Fatalf("re-expire = (%v, %v), want (false, nil) — idempotent no-op", again, err)
	}
	if err := pool.QueryRow(storage.WithSystemScope(ctx), `SELECT count(*) FROM events WHERE session_id=$1 AND type='approval.expired.v1'`, sessionID).Scan(&expiredEvents); err != nil {
		t.Fatalf("recount approval.expired events error = %v", err)
	}
	if expiredEvents != 1 {
		t.Fatalf("approval.expired.v1 events after re-expire = %d, want 1 (idempotent)", expiredEvents)
	}
}

// TestExpiredApprovalSweepAndConsumeGuard proves the two other enforcement points (spec §22.4, E10 T7):
// the reconcile SWEEP expires an approval that elapsed while idle (no consume observed it), and the
// consume-time guard in ApplyApprovalDecision rejects an approve that arrives after expiry — an expired
// approval authorizes nothing but still settles the command.
func TestExpiredApprovalSweepAndConsumeGuard(t *testing.T) {
	cs := openHarness(t)
	ctx := context.Background()
	pool := cs.Pool()
	tenant, sessionID, runID := seedRun(t, pool)
	exec(t, pool, `UPDATE sessions SET state='active' WHERE id=$1`, sessionID)
	respID := newID("resp")
	exec(t, pool, `INSERT INTO responses (id, organization_id, project_id, session_id, state) VALUES ($1,$2,$3,$4,'in_progress')`,
		respID, tenant.Organization, tenant.Project, sessionID)
	exec(t, pool, `UPDATE runs SET state='running', response_id=$2 WHERE id=$1`, runID, respID)

	// (a) Idle sweep: a pending approval elapses with no approve/publish observing it.
	idle := requestPushPublication(t, cs, tenant, sessionID, runID, "ccc333")
	exec(t, pool, `UPDATE approvals SET expires_at = clock_timestamp() - interval '1 minute' WHERE publication_id=$1`, idle.ID)
	swept, err := cs.SweepExpiredApprovals(ctx)
	if err != nil {
		t.Fatalf("SweepExpiredApprovals error = %v", err)
	}
	if swept != 1 {
		t.Fatalf("swept expired approvals = %d, want 1", swept)
	}
	var idleState string
	if err := pool.QueryRow(storage.WithSystemScope(ctx), `SELECT state FROM publications WHERE id=$1`, idle.ID).Scan(&idleState); err != nil {
		t.Fatalf("read idle state error = %v", err)
	}
	if idleState != "expired" {
		t.Fatalf("idle-swept publication state = %q, want expired", idleState)
	}
	// A second sweep is an idempotent no-op (the row already left the pending/approved set).
	if swept, err := cs.SweepExpiredApprovals(ctx); err != nil || swept != 0 {
		t.Fatalf("re-sweep = (%d, %v), want (0, nil)", swept, err)
	}

	// (b) Consume-time guard: an approve command arrives after the pending approval expired. It settles
	// the command but authorizes nothing — the publication goes to expired, never approved.
	late := requestPushPublication(t, cs, tenant, sessionID, runID, "ddd444")
	exec(t, pool, `UPDATE approvals SET expires_at = clock_timestamp() - interval '1 minute' WHERE publication_id=$1`, late.ID)
	approveCmd := coordinator.CommandInput{CommandID: newID("cmd"), Kind: "approve", Payload: approvePayload(late.RequestHash)}
	if _, err := cs.AcceptCommand(ctx, tenant, sessionID, approveCmd); err != nil {
		t.Fatalf("AcceptCommand(approve) error = %v", err)
	}
	if _, err := cs.ApplyApprovalDecision(ctx, tenant, sessionID, respID, runID, approveCmd.CommandID, "approve", late.RequestHash); err != nil {
		t.Fatalf("ApplyApprovalDecision(expired) error = %v", err)
	}
	var lateState, cmdState string
	if err := pool.QueryRow(storage.WithSystemScope(ctx), `SELECT state FROM publications WHERE id=$1`, late.ID).Scan(&lateState); err != nil {
		t.Fatalf("read late state error = %v", err)
	}
	if err := pool.QueryRow(storage.WithSystemScope(ctx), `SELECT state FROM commands WHERE id=$1`, approveCmd.CommandID).Scan(&cmdState); err != nil {
		t.Fatalf("read command state error = %v", err)
	}
	if lateState != "expired" {
		t.Fatalf("publication after expired approve = %q, want expired (authorizes nothing)", lateState)
	}
	if cmdState != "applied" {
		t.Fatalf("expired approve command state = %q, want applied (still settles)", cmdState)
	}
	// It never joined the publishable set.
	approved, _ := cs.ApprovedPublicationsForRun(ctx, tenant, runID)
	if len(approved) != 0 {
		t.Fatalf("approved publications after expired approve = %d, want 0", len(approved))
	}
}

// requestPushPublication records a pending push publication for the run and returns its projection.
func requestPushPublication(t *testing.T, cs *coordinator.Store, tenant coordinator.Tenant, sessionID, runID, head string) coordinator.Publication {
	t.Helper()
	remote, branch, base := "git@h:o/r", "agent/s/r", "main"
	pub, err := cs.RequestPublication(context.Background(), tenant, coordinator.PublicationRequest{
		PublicationID: newID("pub"), ApprovalID: newID("apr"), SessionID: sessionID, RunID: runID,
		Operation: "push_branch", Remote: remote, Branch: branch, Base: base, HeadSHA: head,
		IdempotencyKey: repositories.IdempotencyKey(tenant.Organization, tenant.Project, runID, repositories.OpPushBranch, remote, branch, base, head),
		RequestHash:    repositories.RequestHash(tenant.Organization, tenant.Project, runID, repositories.OpPushBranch, remote, branch, base, head),
		Display:        "push " + branch,
	})
	if err != nil {
		t.Fatalf("requestPushPublication error = %v", err)
	}
	return pub
}

// approvePayload builds an approve/deny command payload carrying the one-shot request hash.
func approvePayload(requestHash string) []byte {
	return []byte(`{"request_hash":"` + requestHash + `"}`)
}
