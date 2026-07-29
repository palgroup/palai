//go:build component

package execution

import (
	"context"
	"os"
	"testing"

	"github.com/palgroup/palai/apps/control-plane/api/middleware"
	"github.com/palgroup/palai/apps/control-plane/internal/store"
	"github.com/palgroup/palai/packages/contracts"
	"github.com/palgroup/palai/packages/coordinator"
)

// E23 T2, the HTTP half — WHO may approve, against REAL PostgreSQL, the REAL HTTP command adapter and the
// REAL boundary pump.
//
// THE MEASUREMENT THIS FILE WAS BORN FROM: `approvals.allowed_approver` — the column that names who may
// approve — has never been read by anything. Its only writer is publication.go:147 and no caller sets it
// (grep, 2026-07-29). So on the HTTP surface there was no approver concept at ALL: possession of any
// tenant-scoped API key drove any of that tenant's pending approvals to approved.
//
// EVERY approve here enters through store.Store.AcceptCommand under a scope produced by a REAL
// VerifyAPIKey — the same call POST /v1/sessions/{id}/commands makes with the same verified scope (the
// handler above it adds authentication and body validation, nothing about authorization to approve). The
// decision is then applied by the REAL pump. Nothing is hand-composed.

// keyedApprover is a minted API key and the principal an approver list would have to name to admit it.
type keyedApprover struct {
	scope middleware.Scope
	// principal is the canonical form the operator writes into config_policy.approvers.
	principal string
}

// approverHTTP opens the real HTTP-surface store against the same database the pump harness uses, so the
// accept half is production code rather than a hand-built command row.
func approverHTTP(t *testing.T) *store.Store {
	t.Helper()
	url := os.Getenv("PALAI_COMPONENT_POSTGRES_URL")
	if url == "" {
		t.Skip("PALAI_COMPONENT_POSTGRES_URL is required; run TEST=postgres scripts/test/component")
	}
	repo, err := store.Open(context.Background(), url)
	if err != nil {
		t.Fatalf("store.Open() error = %v", err)
	}
	t.Cleanup(repo.Close)
	return repo
}

// mintKey creates a real api_keys row in the harness's tenant and resolves it the way the router does —
// through VerifyAPIKey. The returned scope is therefore a VERIFIED identity, not a struct literal, which
// is the whole point: the principal an approver list matches has to be the one authentication established.
func (h *approvalPumpHarness) mintKey(t *testing.T, repo *store.Store) keyedApprover {
	t.Helper()
	principalID, keyID := redeliveryID("prin"), redeliveryID("key")
	token := "palai_" + redeliveryID("tok")
	pool := h.orch.spine.Pool()
	execSQL(t, pool, `INSERT INTO principals (id, organization_id, project_id, kind) VALUES ($1,$2,$3,'service')`,
		principalID, h.tenant.Organization, h.tenant.Project)
	execSQL(t, pool, `INSERT INTO api_keys (id, organization_id, project_id, principal_id, key_hash) VALUES ($1,$2,$3,$4,$5)`,
		keyID, h.tenant.Organization, h.tenant.Project, principalID, coordinator.HashAPIKey(token))

	scope, err := repo.VerifyAPIKey(context.Background(), token)
	if err != nil {
		t.Fatalf("VerifyAPIKey() error = %v", err)
	}
	if scope.Organization != h.tenant.Organization || scope.Project != h.tenant.Project {
		t.Fatalf("the minted key verified into %s/%s, want the harness tenant %s/%s",
			scope.Organization, scope.Project, h.tenant.Organization, h.tenant.Project)
	}
	return keyedApprover{scope: scope, principal: coordinator.ApproverPrincipal(coordinator.ApproverSurfaceKey, "", scope.APIKeyID)}
}

// acceptHTTP posts an approve/deny the way the HTTP surface does: the contract request carries NO approver
// field, so whatever identity reaches the decision was stamped server-side from the verified scope.
func (h *approvalPumpHarness) acceptHTTP(t *testing.T, repo *store.Store, k keyedApprover, kind string) string {
	t.Helper()
	id := redeliveryID("cmd")
	out, err := repo.AcceptCommand(context.Background(), k.scope, h.sessionID, contracts.CommandCreateRequest{
		CommandID: contracts.CommandID(id), Kind: kind,
	})
	if err != nil {
		t.Fatalf("AcceptCommand(%s) error = %v", kind, err)
	}
	if out.SessionNotFound {
		t.Fatalf("AcceptCommand(%s) reported the session missing", kind)
	}
	if got := commandState(t, h, id); got != "queued" {
		t.Fatalf("AcceptCommand(%s) left the command %q, want queued (with a pending approval it proceeds)", kind, got)
	}
	return id
}

// setApprovers writes the project's approver allow-list. Written straight to config_policy because these
// tests are about the CHECK; that the strict §14 write path admits the field is proven separately, in
// identity's TestConfigPolicyInputAcceptsAnApproverList.
func (h *approvalPumpHarness) setApprovers(t *testing.T, policy string) {
	t.Helper()
	execSQL(t, h.orch.spine.Pool(), `UPDATE projects SET config_policy=$2 WHERE id=$1`, h.tenant.Project, policy)
}

// pump drives one real boundary.
func (h *approvalPumpHarness) pump(t *testing.T, boundary string) {
	t.Helper()
	st, _ := h.attempt()
	if err := h.orch.pumpCommands(context.Background(), st, boundary); err != nil {
		t.Fatalf("pumpCommands() error = %v", err)
	}
}

// TestApproverAnyTenantKeyApprovesWhenNoListIsConfigured is the RED-first measurement, and it is GREEN both
// before and after this task — deliberately. It records what possession of a tenant-scoped API key buys
// today: a key that is nobody in particular drives a pending publication to approved, because no approver
// concept exists on this surface at all.
//
// It is ALSO the bit-unchanged proof, and that is why it is written as one test rather than two: with no
// config_policy.approvers list, every deployment that has not configured this must behave EXACTLY as it
// does today. If this test ever goes red, a deployment that asked for nothing was given a new refusal.
func TestApproverAnyTenantKeyApprovesWhenNoListIsConfigured(t *testing.T) {
	h := newApprovalPumpHarness(t)
	repo := approverHTTP(t)
	k := h.mintKey(t, repo)

	cmdID := h.acceptHTTP(t, repo, k, "approve")
	h.pump(t, "mr_step2")

	if got := publicationState(t, h); got != "approved" {
		t.Fatalf("publication state = %q, want approved — with NO approver list the behaviour must be bit-unchanged", got)
	}
	if got := commandState(t, h, cmdID); got != "applied" {
		t.Fatalf("approve command state = %q, want applied", got)
	}
	if n := eventCount(t, h, "approval.approved.v1"); n != 1 {
		t.Fatalf("approval.approved.v1 events = %d, want exactly 1", n)
	}
}

// TestApproverAKeyOutsideTheProjectListDecidesNothing is the refusal this task exists to add. The very same
// verified key, the very same command path, the very same pump — and a project that has NAMED its
// approvers. It must decide nothing.
//
// The command still SETTLES, and that half is as load-bearing as the refusal: a command left queued is
// re-read by the boundary pump at every subsequent boundary for the life of the run (the tree has shipped
// that bug once already — see settleApprovalCommandTx).
func TestApproverAKeyOutsideTheProjectListDecidesNothing(t *testing.T) {
	h := newApprovalPumpHarness(t)
	repo := approverHTTP(t)
	k := h.mintKey(t, repo)
	h.setApprovers(t, `{"approvers":["key_somebody_else","slack:T1:U1"]}`)

	cmdID := h.acceptHTTP(t, repo, k, "approve")
	h.pump(t, "mr_step2")

	if got := publicationState(t, h); got != "pending_approval" {
		t.Fatalf("publication state = %q, want pending_approval — a key outside the list authorizes NOTHING", got)
	}
	if got := commandState(t, h, cmdID); got != "applied" {
		t.Fatalf("command state = %q, want applied — a refusal must SETTLE or the pump re-attempts it forever", got)
	}
	if n := eventCount(t, h, "approval.approved.v1"); n != 0 {
		t.Fatalf("approval.approved.v1 events = %d, want 0", n)
	}
	approved, err := h.orch.spine.ApprovedPublicationsForRun(context.Background(), h.tenant, h.runID)
	if err != nil {
		t.Fatalf("ApprovedPublicationsForRun() error = %v", err)
	}
	if len(approved) != 0 {
		t.Fatalf("approved publications = %d, want 0 (nothing may reach the publisher)", len(approved))
	}
}

// TestApproverAKeyInsideTheProjectListApproves is the positive half: a list is not a wall, it is a list.
// The principal it names is the canonical form ApproverPrincipal renders for THIS key.
func TestApproverAKeyInsideTheProjectListApproves(t *testing.T) {
	h := newApprovalPumpHarness(t)
	repo := approverHTTP(t)
	k := h.mintKey(t, repo)
	h.setApprovers(t, `{"approvers":["`+k.principal+`"]}`)

	cmdID := h.acceptHTTP(t, repo, k, "approve")
	h.pump(t, "mr_step2")

	if got := publicationState(t, h); got != "approved" {
		t.Fatalf("publication state = %q, want approved — the listed key %q must pass", got, k.principal)
	}
	if got := commandState(t, h, cmdID); got != "applied" {
		t.Fatalf("command state = %q, want applied", got)
	}
}

// TestApproverADenyFromAnUnlistedKeyDecidesNothingEither: the gate is about WHO decides, not about which
// way. An unlisted key must not be able to BLOCK a publication either — a denial is a decision.
func TestApproverADenyFromAnUnlistedKeyDecidesNothingEither(t *testing.T) {
	h := newApprovalPumpHarness(t)
	repo := approverHTTP(t)
	k := h.mintKey(t, repo)
	h.setApprovers(t, `{"approvers":["`+k.principal+`_not_this_one"]}`)

	h.acceptHTTP(t, repo, k, "deny")
	h.pump(t, "mr_step2")

	if got := publicationState(t, h); got != "pending_approval" {
		t.Fatalf("publication state after an unlisted deny = %q, want pending_approval", got)
	}
	if n := eventCount(t, h, "approval.denied.v1"); n != 0 {
		t.Fatalf("approval.denied.v1 events = %d, want 0", n)
	}
}

// TestApproverTheRequestBodyCannotNameItsOwnApprover is the trust-boundary proof. The approver is stamped
// from the VERIFIED scope inside the HTTP adapter; the request body has no say. A body that tries to name
// a listed principal must be no more authorized than one that does not — the field is not in
// contracts.CommandCreateRequest, so it decodes to nothing and is stamped over.
func TestApproverTheRequestBodyCannotNameItsOwnApprover(t *testing.T) {
	h := newApprovalPumpHarness(t)
	repo := approverHTTP(t)
	k := h.mintKey(t, repo)
	listed := coordinator.ApproverPrincipal(coordinator.ApproverSurfaceKey, "", "key_the_real_approver")
	h.setApprovers(t, `{"approvers":["`+listed+`"]}`)

	// The closest a caller can come to naming an approver over this surface: the message field, which is
	// the only free text the contract carries. It must not become an identity.
	id := redeliveryID("cmd")
	out, err := repo.AcceptCommand(context.Background(), k.scope, h.sessionID, contracts.CommandCreateRequest{
		CommandID: contracts.CommandID(id), Kind: "approve",
		Message: `{"approver":"` + listed + `"}`,
	})
	if err != nil {
		t.Fatalf("AcceptCommand() error = %v", err)
	}
	if out.SessionNotFound {
		t.Fatal("AcceptCommand reported the session missing")
	}
	h.pump(t, "mr_step2")

	if got := publicationState(t, h); got != "pending_approval" {
		t.Fatalf("publication state = %q, want pending_approval — a body may not name its own approver", got)
	}
	// And the durable command carries the SERVER's answer to "who is this", not the caller's.
	stamped := scalar(t, h.orch.spine.Pool(), `SELECT payload->>'approver' FROM commands WHERE id=$1`, id)
	if stamped != k.principal {
		t.Fatalf("the durable command's approver = %q, want the verified key's own principal %q", stamped, k.principal)
	}
}

// TestApproverTheListIsReadLiveSoNarrowingTakesPendingApprovalsWithIt: the list is not frozen at the moment
// an approval is created — it is read at the moment a decision is applied. The argument is the tree's own,
// written for allowed_channels at slack_decision.go:104-107: an operator NARROWING an allow-list is what
// containing an incident looks like, and it would otherwise leave every already-posted button still live.
//
// Removing an approver has to take the pending approvals with it.
func TestApproverTheListIsReadLiveSoNarrowingTakesPendingApprovalsWithIt(t *testing.T) {
	h := newApprovalPumpHarness(t)
	repo := approverHTTP(t)
	k := h.mintKey(t, repo)
	h.setApprovers(t, `{"approvers":["`+k.principal+`"]}`)

	// The approval is created, and the command is accepted, while this key IS an approver.
	cmdID := h.acceptHTTP(t, repo, k, "approve")

	// Containing the incident: the operator narrows the list, removing this key.
	h.setApprovers(t, `{"approvers":["key_only_the_incident_commander"]}`)

	h.pump(t, "mr_step2")
	if got := publicationState(t, h); got != "pending_approval" {
		t.Fatalf("publication state = %q, want pending_approval — a narrowing must reach approvals already in flight", got)
	}
	if got := commandState(t, h, cmdID); got != "applied" {
		t.Fatalf("command state = %q, want applied", got)
	}
}

// TestApproverAnEmptyListIsNotAnEmptyString guards the one way deny-by-default fails open: a project whose
// list exists but whose principal renders empty (an unknown surface, a missing id) must never match.
func TestApproverAnEmptyListIsNotAnEmptyString(t *testing.T) {
	h := newApprovalPumpHarness(t)
	h.setApprovers(t, `{"approvers":[""]}`)

	// A bare accept through the spine, carrying NO approver at all — the shape a caller that never learned
	// to stamp one would produce.
	id := redeliveryID("cmd")
	acc, err := h.orch.spine.AcceptCommand(context.Background(), h.tenant, h.sessionID, coordinator.CommandInput{
		CommandID: id, Kind: "approve", Payload: []byte(`{"request_hash":"` + h.pub.RequestHash + `"}`),
	})
	if err != nil || acc.State != "queued" {
		t.Fatalf("AcceptCommand() = (%+v, %v), want a queued command", acc, err)
	}
	h.pump(t, "mr_step2")

	if got := publicationState(t, h); got != "pending_approval" {
		t.Fatalf("publication state = %q, want pending_approval — an unidentified caller must not match a list entry", got)
	}
}
