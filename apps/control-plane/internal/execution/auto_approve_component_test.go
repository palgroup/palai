//go:build component

package execution

import (
	"context"
	"errors"
	"testing"

	"github.com/palgroup/palai/adapters/repositories"
	"github.com/palgroup/palai/packages/coordinator"
	statemachines "github.com/palgroup/palai/packages/state-machines"

	"github.com/palgroup/palai/storage"
)

// THE SESSION'S STANDING AUTHORIZATION, against a REAL spine (E30 T1, migration 000056, spec §22.4).
//
// The owner asked to be able to auto-approve a session so they could watch an agent drive `xcodebuild`
// and the simulator without answering the same question forty times. These tests are written first and
// RED first, because the feature is a GATE BEING OPENED and every one of them is the reason it is safe:
//
//  1. An armed session runs a gated tool with no human, and the run does NOT park.
//  2. A session that armed NOTHING behaves bit-for-bit as before. This is the non-vacuity half — without
//     it, a build that auto-approved EVERY tool would satisfy every other assertion here while removing
//     the gate from every deployment that never asked for it.
//  3. THE SPLIT. Arming the tool half does not arm the publication half. This is the test the whole
//     design exists for: an operator who stopped confirming `xcodebuild` has not thereby stopped
//     confirming a push to somebody's repository.
//  4. An auto-approval leaves a DECIDED ROW, not a skipped one. The approvals surface must show what
//     happened and whose standing authorization it happened under — a gate that silently omits rows is
//     indistinguishable, from every screen a human looks at, from a gate that was never there.
//  5. NO ESCALATION. A project whose `approvers` list does not name the arming principal auto-approves
//     nothing, and the run parks for a human exactly as it would have. Arming a session grants what
//     clicking would have granted and not one thing more.

// armSession writes the standing authorization the two gates read. It goes through the STORE METHOD the
// HTTP surface calls, not a hand-written UPDATE, so a test cannot pass against a column the production
// write path never sets.
func armSession(t *testing.T, h *approvalHarness, principal string, tools, publications bool) {
	t.Helper()
	view, err := h.spine.SetSessionAutoApprove(context.Background(), h.tenant, h.sessionID, principal, tools, publications)
	if err != nil {
		t.Fatalf("SetSessionAutoApprove() error = %v", err)
	}
	if !view.Found {
		t.Fatalf("SetSessionAutoApprove() found no session %s", h.sessionID)
	}
	if view.Tools != tools || view.Publications != publications {
		t.Fatalf("armed (tools=%v, publications=%v), the row reads (tools=%v, publications=%v)",
			tools, publications, view.Tools, view.Publications)
	}
}

// setApprovers writes the project's config_policy approver list — the E23 T2 gate the auto-decision must
// still pass.
func setApprovers(t *testing.T, h *approvalHarness, approvers ...string) {
	t.Helper()
	policy := `{"approvers":[`
	for i, a := range approvers {
		if i > 0 {
			policy += ","
		}
		policy += `"` + a + `"`
	}
	policy += `]}`
	execSQL(t, h.spine.Pool(), `UPDATE projects SET config_policy = $1::jsonb WHERE id = $2`, policy, h.tenant.Project)
}

// approvalRow reads the approvals row for the parked call: its state and who decided it. The state lives
// on tool_calls; `decided_by` is what makes an auto-approval auditable rather than anonymous.
func approvalRow(t *testing.T, h *approvalHarness) (callState, decidedBy string) {
	t.Helper()
	ctx := storage.WithSystemScope(context.Background())
	if err := h.spine.Pool().QueryRow(ctx,
		`SELECT state FROM tool_calls WHERE id = $1`, h.callID).Scan(&callState); err != nil {
		t.Fatalf("read tool_call state: %v", err)
	}
	if err := h.spine.Pool().QueryRow(ctx,
		`SELECT COALESCE(decided_by, '') FROM approvals WHERE tool_call_id = $1`, h.callID).Scan(&decidedBy); err != nil {
		t.Fatalf("read approvals.decided_by: %v", err)
	}
	return callState, decidedBy
}

// TestAutoApproveArmedSessionRunsAGatedToolWithoutParkingTheRun is RED #1 and the feature itself. The
// same gated tool that parks a run in TestToolApprovalGateNeverReachesExecuteWithoutAHumanDecision runs
// here, on the first dispatch, because a human authorized it in advance for this session.
func TestAutoApproveArmedSessionRunsAGatedToolWithoutParkingTheRun(t *testing.T) {
	h := newApprovalHarness(t, gatedTool)
	armSession(t, h, "key:operator-anna", true, false)

	args := map[string]any{"issue": "PAL-42", "status": "Done"}
	ch, err := h.dispatch(t, 1, args)
	if err != nil {
		t.Fatalf("dispatchTool on an armed session error = %v, want the call to run", err)
	}
	if n := h.ran(); n != 1 {
		t.Fatalf("the gated tool ran %d time(s) on an armed session, want exactly 1", n)
	}
	// The model was ANSWERED. A run that auto-approves and then still hands the engine nothing has
	// swapped one stall for another.
	if got := toolResults(ch); len(got) != 1 {
		t.Fatalf("the engine was sent %d tool.result frame(s), want exactly 1: %+v", len(got), got)
	}
	if got := h.callState(t); got != "completed" {
		t.Fatalf("tool_call state = %q, want completed", got)
	}
	if got := h.runState(t); got != "running" {
		t.Fatalf("run state = %q on an armed session, want running — an armed session must not park", got)
	}
}

// TestAutoApproveDisarmedSessionIsBitUnchangedAndStillParks is the NON-VACUITY half, and it is not
// ceremony: it is the one assertion that fails if the gate is removed rather than made conditional. A
// session that armed nothing is every session in every deployment alive today.
func TestAutoApproveDisarmedSessionIsBitUnchangedAndStillParks(t *testing.T) {
	h := newApprovalHarness(t, gatedTool)
	// Deliberately NOT armed. No SetSessionAutoApprove call at all — the state a session is born in.

	if _, err := h.dispatch(t, 1, map[string]any{"issue": "PAL-42"}); !errors.Is(err, errRunParked) {
		t.Fatalf("dispatchTool on an UNARMED session error = %v, want errRunParked", err)
	}
	if n := h.ran(); n != 0 {
		t.Fatalf("the gated tool ran %d time(s) with nothing armed and nobody asked, want 0", n)
	}
	if got := h.callState(t); got != "approval_pending" {
		t.Fatalf("tool_call state = %q on an unarmed session, want approval_pending", got)
	}
	if got := h.runState(t); got != string(statemachines.RunWaiting) {
		t.Fatalf("run state = %q on an unarmed session, want waiting", got)
	}
}

// TestAutoApproveDisarmingASessionRestoresTheGate closes the half the pair above leaves open: arming is
// reversible, and a session disarmed mid-sitting parks the very next gated call. Without this, "auto
// approve" could be a one-way door and the screen's toggle would be a lie in one direction.
func TestAutoApproveDisarmingASessionRestoresTheGate(t *testing.T) {
	h := newApprovalHarness(t, gatedTool)
	armSession(t, h, "key:operator-anna", true, false)
	armSession(t, h, "key:operator-anna", false, false)

	if _, err := h.dispatch(t, 1, map[string]any{"issue": "PAL-42"}); !errors.Is(err, errRunParked) {
		t.Fatalf("dispatchTool after DISARMING error = %v, want errRunParked", err)
	}
	if n := h.ran(); n != 0 {
		t.Fatalf("the gated tool ran %d time(s) after the session was disarmed, want 0", n)
	}
}

// TestAutoApproveToolsArmedDoesNotArmPublications IS THE SPLIT, and it is the test this whole design
// exists to make passable.
//
// The session is armed for TOOLS and not for publications. A gated tool runs with no human — and the
// publication family's standing authorization is still off, so anything that reaches the publication
// gate still owes a human an answer. One flag for both would make this test unwritable.
func TestAutoApproveToolsArmedDoesNotArmPublications(t *testing.T) {
	h := newApprovalHarness(t, gatedTool)
	armSession(t, h, "key:operator-anna", true, false)

	if _, err := h.dispatch(t, 1, map[string]any{"issue": "PAL-42"}); err != nil {
		t.Fatalf("the TOOL half is armed and the call still did not run: %v", err)
	}
	if n := h.ran(); n != 1 {
		t.Fatalf("the gated tool ran %d time(s) with the tool half armed, want 1", n)
	}

	// The publication half must be OFF in the durable row the publication gate reads. Asserting on the
	// row rather than on a Go value is deliberate: the two gates read this through separate code paths
	// and the property has to hold in the database they share, not in one function's local.
	a, err := h.spine.SessionAutoApprove(context.Background(), h.tenant, h.sessionID)
	if err != nil {
		t.Fatalf("SessionAutoApprove() error = %v", err)
	}
	if !a.Tools {
		t.Fatal("the tool half is not armed in the row the gates read")
	}
	if a.Publications {
		t.Fatal("ARMING THE TOOL HALF ARMED THE PUBLICATION HALF: an operator who stopped confirming " +
			"xcodebuild has silently stopped confirming every push to their repository")
	}
}

// TestAutoApproveLeavesADecidedApprovalRowRatherThanSkippingIt. An auto-approval must be VISIBLE on the
// approvals surface, carrying the name of the human whose standing authorization decided it. A gate that
// skips the row is, from every screen a human looks at, identical to a gate that was never there.
func TestAutoApproveLeavesADecidedApprovalRowRatherThanSkippingIt(t *testing.T) {
	h := newApprovalHarness(t, gatedTool)
	armSession(t, h, "key:operator-anna", true, false)

	if _, err := h.dispatch(t, 1, map[string]any{"issue": "PAL-42"}); err != nil {
		t.Fatalf("dispatchTool error = %v", err)
	}

	// The row EXISTS. A missing row would make this call indistinguishable from an ungated one.
	var rows int
	if err := h.spine.Pool().QueryRow(storage.WithSystemScope(context.Background()),
		`SELECT count(*) FROM approvals WHERE tool_call_id = $1`, h.callID).Scan(&rows); err != nil {
		t.Fatalf("count approvals: %v", err)
	}
	if rows != 1 {
		t.Fatalf("an auto-approved gated call left %d approvals row(s), want exactly 1 — an approval "+
			"nobody can see is an approval nobody can audit", rows)
	}

	state, decidedBy := approvalRow(t, h)
	if state != "completed" {
		t.Fatalf("tool_call state = %q after an auto-approval, want completed", state)
	}
	if decidedBy != "key:operator-anna" {
		t.Fatalf("approvals.decided_by = %q, want the ARMING PRINCIPAL %q — an auto-approval decided by "+
			"nobody is an audit trail that cannot answer who authorized this", decidedBy, "key:operator-anna")
	}
}

// TestAutoApproveRefusesWhenTheApproverPolicyDoesNotNameTheArmingPrincipal is the NO-ESCALATION claim,
// and it is the reason AutoApprove carries a principal rather than a pair of booleans.
//
// The project names an approver list that does NOT include the person who armed the session. Arming must
// therefore authorize nothing: the run parks and a human is asked, exactly as it would have been. If this
// went the other way, the switch would be a way for anyone who can PATCH a session to grant themselves
// an approval authority the project deliberately withheld.
func TestAutoApproveRefusesWhenTheApproverPolicyDoesNotNameTheArmingPrincipal(t *testing.T) {
	h := newApprovalHarness(t, gatedTool)
	setApprovers(t, h, "key:release-manager")
	armSession(t, h, "key:operator-anna", true, false)

	if _, err := h.dispatch(t, 1, map[string]any{"issue": "PAL-42"}); !errors.Is(err, errRunParked) {
		t.Fatalf("dispatchTool error = %v, want errRunParked — an arming principal the project's "+
			"approver list does not name must authorize NOTHING", err)
	}
	if n := h.ran(); n != 0 {
		t.Fatalf("the gated tool ran %d time(s) under a principal the approver list excludes, want 0", n)
	}
	if got := h.callState(t); got != "approval_pending" {
		t.Fatalf("tool_call state = %q, want approval_pending (a human must still be asked)", got)
	}
}

// TestAutoApproveHonoursAnApproverPolicyThatDoesNameThePrincipal is the other direction, and without it
// the test above is satisfiable by a build where auto-approve never works at all. A project WITH a list
// that names the arming principal auto-approves normally: the standing authorization is exactly as
// strong as a click from that same person, no weaker.
func TestAutoApproveHonoursAnApproverPolicyThatDoesNameThePrincipal(t *testing.T) {
	h := newApprovalHarness(t, gatedTool)
	setApprovers(t, h, "key:release-manager", "key:operator-anna")
	armSession(t, h, "key:operator-anna", true, false)

	if _, err := h.dispatch(t, 1, map[string]any{"issue": "PAL-42"}); err != nil {
		t.Fatalf("dispatchTool error = %v — the approver list NAMES the arming principal, so the "+
			"standing authorization must be honoured", err)
	}
	if n := h.ran(); n != 1 {
		t.Fatalf("the gated tool ran %d time(s) under a named approver, want 1", n)
	}
}

// TestAutoApproveArmingWithNoPrincipalAuthorizesNothingUnderAnApproverList closes the empty-principal
// hole ApproverAllowed already refuses at its own layer, at THIS layer, where it would otherwise be a
// silent widening: an unidentified caller who could arm a session must not thereby decide approvals in a
// project that restricts who may decide.
func TestAutoApproveArmingWithNoPrincipalAuthorizesNothingUnderAnApproverList(t *testing.T) {
	h := newApprovalHarness(t, gatedTool)
	setApprovers(t, h, "key:release-manager")
	armSession(t, h, "", true, false)

	if _, err := h.dispatch(t, 1, map[string]any{"issue": "PAL-42"}); !errors.Is(err, errRunParked) {
		t.Fatalf("dispatchTool error = %v, want errRunParked for an UNIDENTIFIED arming principal", err)
	}
	if n := h.ran(); n != 0 {
		t.Fatalf("the gated tool ran %d time(s) armed by nobody, want 0", n)
	}
}

var _ = coordinator.AutoApprove{}

// ---------------------------------------------------------------------------------------------------
// THE PUBLICATION HALF, and the split proven from the OTHER side.
// ---------------------------------------------------------------------------------------------------
//
// The two tests above prove the tool half runs and that arming it leaves `auto_approve_publications`
// false in the row. These two prove what that false actually DOES at the publication gate, and what a
// true does — because a column nothing reads is a column that proves nothing, and this tree has shipped
// that defect often enough to name it (the /runs `Agent (optional)` selector sent nothing at all).

// pendingPublication records a pending publication the way the push tool does — a real one-shot request
// hash against a real row, so the gate has a real target to park on.
// A publication decision is a durable COMMAND on the run, and AcceptCommand resolves the session's
// active root run — which must therefore carry a response. openLedgerSpine's run does not (the tool
// gate never needed one), so this seeds it here rather than widening the shared harness for one family
// of tests. Returning the response id keeps the publication row pointing at the same one.
func seedResponseForRun(t *testing.T, h *approvalHarness) string {
	t.Helper()
	respID := redeliveryID("resp")
	execSQL(t, h.spine.Pool(), `INSERT INTO responses (id, project_id, session_id, state) VALUES ($1,$2,$3,'in_progress')`,
		respID, h.tenant.Project, h.sessionID)
	execSQL(t, h.spine.Pool(), `UPDATE runs SET response_id = $1 WHERE id = $2`, respID, h.runID)
	return respID
}

func pendingPublication(t *testing.T, h *approvalHarness, responseID string) coordinator.Publication {
	t.Helper()
	remote, branch, base, head := "git@h:o/r", "agent/s/r", "main", "abc123"
	pub, err := h.spine.RequestPublication(context.Background(), h.tenant, coordinator.PublicationRequest{
		PublicationID: redeliveryID("pub"), ApprovalID: redeliveryID("apr"), SessionID: h.sessionID,
		RunID: h.runID, ResponseID: responseID, Operation: "push_branch",
		Remote: remote, Branch: branch, Base: base, HeadSHA: head,
		IdempotencyKey: repositories.IdempotencyKey(h.tenant.Project, h.runID, repositories.OpPushBranch, remote, branch, base, head),
		RequestHash:    repositories.RequestHash(h.tenant.Project, h.runID, repositories.OpPushBranch, remote, branch, base, head),
		Display:        "push " + branch,
	})
	if err != nil {
		t.Fatalf("RequestPublication() error = %v", err)
	}
	return pub
}

func armedPublicationState(t *testing.T, h *approvalHarness, pubID string) string {
	t.Helper()
	var state string
	if err := h.spine.Pool().QueryRow(storage.WithSystemScope(context.Background()),
		`SELECT state FROM publications WHERE id = $1`, pubID).Scan(&state); err != nil {
		t.Fatalf("read publication state: %v", err)
	}
	return state
}

// TestAutoApproveASessionArmedForToolsStillParksOnAPublication IS THE SPLIT, measured at the gate that
// matters rather than at the column.
//
// The session is armed for TOOLS. A push reaches the publication gate. It must PARK — the operator who
// stopped confirming `xcodebuild` has not authorized a write to anybody's repository, and this is the
// assertion that fails if the two halves are ever collapsed into one flag.
func TestAutoApproveASessionArmedForToolsStillParksOnAPublication(t *testing.T) {
	h := newApprovalHarness(t, gatedTool)
	armSession(t, h, "key:operator-anna", true, false)
	pub := pendingPublication(t, h, seedResponseForRun(t, h))

	orch, st, _ := ledgerAttempt(h.spine, h.broker(), h.tenant, h.sessionID, h.runID, 1)
	// parkRun signals the park through its OWN return (errRunParked), which is what the production
	// caller reads; anything else here is a real failure.
	parked, err := orch.parkOnPendingPublication(context.Background(), st)
	if err != nil && !errors.Is(err, errRunParked) {
		t.Fatalf("parkOnPendingPublication() error = %v", err)
	}
	if !parked {
		t.Fatal("A SESSION ARMED FOR TOOLS AUTO-APPROVED A PUBLICATION: the operator who stopped " +
			"confirming xcodebuild has silently authorized a write to somebody's repository")
	}
	if got := armedPublicationState(t, h, pub.ID); got != "pending_approval" {
		t.Fatalf("publication state = %q with only the tool half armed, want pending_approval", got)
	}
	if got := h.runState(t); got != string(statemachines.RunWaiting) {
		t.Fatalf("run state = %q, want waiting — a publication nobody authorized must park the run", got)
	}
}

// TestAutoApproveASessionArmedForPublicationsDoesNotParkOnOne is the other direction, and without it the
// test above is satisfiable by a publication half that never works at all — the shipped-but-uncalled
// shape this tree keeps finding. An operator who explicitly armed the publication family gets what they
// asked for: the push is approved, by name, and the run carries on.
func TestAutoApproveASessionArmedForPublicationsDoesNotParkOnOne(t *testing.T) {
	h := newApprovalHarness(t, gatedTool)
	armSession(t, h, "key:operator-anna", false, true)
	pub := pendingPublication(t, h, seedResponseForRun(t, h))

	orch, st, _ := ledgerAttempt(h.spine, h.broker(), h.tenant, h.sessionID, h.runID, 1)
	parked, err := orch.parkOnPendingPublication(context.Background(), st)
	if err != nil {
		t.Fatalf("parkOnPendingPublication() error = %v — an armed publication must not park", err)
	}
	if parked {
		t.Fatal("the publication half is armed and the run parked anyway — the toggle does nothing")
	}
	if got := armedPublicationState(t, h, pub.ID); got != "approved" {
		t.Fatalf("publication state = %q with the publication half armed, want approved", got)
	}
	// And it is auditable: the decision names the human who armed the session, exactly as the tool half's
	// does. An approval decided by nobody cannot answer "who authorized this write".
	var decidedBy string
	if err := h.spine.Pool().QueryRow(storage.WithSystemScope(context.Background()),
		`SELECT COALESCE(decided_by,'') FROM approvals WHERE publication_id = $1`, pub.ID).Scan(&decidedBy); err != nil {
		t.Fatalf("read approvals.decided_by: %v", err)
	}
	if decidedBy != "key:operator-anna" {
		t.Fatalf("approvals.decided_by = %q, want the arming principal %q", decidedBy, "key:operator-anna")
	}
}

// TestAutoApprovePublicationsRefusesWhenTheApproverPolicyExcludesThePrincipal is the no-escalation claim
// on the half where escalation would be worst: a write to somebody else's repository.
func TestAutoApprovePublicationsRefusesWhenTheApproverPolicyExcludesThePrincipal(t *testing.T) {
	h := newApprovalHarness(t, gatedTool)
	setApprovers(t, h, "key:release-manager")
	armSession(t, h, "key:operator-anna", false, true)
	pub := pendingPublication(t, h, seedResponseForRun(t, h))

	orch, st, _ := ledgerAttempt(h.spine, h.broker(), h.tenant, h.sessionID, h.runID, 1)
	parked, err := orch.parkOnPendingPublication(context.Background(), st)
	if err != nil && !errors.Is(err, errRunParked) {
		t.Fatalf("parkOnPendingPublication() error = %v", err)
	}
	if !parked {
		t.Fatal("a principal the project's approver list EXCLUDES auto-approved a push")
	}
	if got := armedPublicationState(t, h, pub.ID); got != "pending_approval" {
		t.Fatalf("publication state = %q, want pending_approval", got)
	}
}
