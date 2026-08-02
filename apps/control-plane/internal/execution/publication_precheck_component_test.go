//go:build component

package execution

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/palgroup/palai/apps/control-plane/internal/execution/tools"
	"github.com/palgroup/palai/packages/coordinator"
	toolbroker "github.com/palgroup/palai/packages/tool-broker"
)

// A PUBLICATION THAT CANNOT BE PUBLISHED IS REFUSED BEFORE A HUMAN IS ASKED, against REAL PostgreSQL.
//
// The failure this refuses is the one with a person inside it. Before the publisher was un-gated, a
// deployment with no GitHub App built no publisher and the approval pump was a no-op: the push tool
// answered `pending_approval`, the row was written, an operator read the destination and pressed Approve,
// the durable command applied, the parked run woke — and nothing was pushed. Every surface reported
// success. The row is still sitting at `approved`.
//
// So the question "does this deployment have a credential path for this binding" is asked at the EARLIEST
// point it can be answered — the moment RunPublicationTarget resolves the binding, inside the tool call —
// and a publication with no path never becomes a pending approval at all.
//
// THE ASSERTIONS ARE IN THE ORDER OF THE DANGER: the tool must refuse, AND the ledger must hold no
// publication for the run. A refusal that still wrote the row would leave the same approvable question
// behind, with an error message in front of it.

// prechecked builds the production seam: an Orchestrator holding a publisher, its canPublish method, and
// the real publicationRegistry over the real spine. It is assembled the way NewOrchestrator assembles it,
// through SetPublisher — a hand-written closure here would be a second answer to the question this test
// exists to ask.
func prechecked(h *publishHarness, publisher Publisher) toolbroker.PublicationRegistry {
	orch := &Orchestrator{spine: h.spine}
	orch.SetPublisher(publisher)
	return newPublicationRegistry(h.spine, orch.canPublish)
}

// appLessPublisher is what main.repositoryPublisher builds on a deployment with no GitHub App: the
// connection resolver wired, the deployment-global broker nil.
func appLessPublisher(t *testing.T) *RepositoryPublisher {
	return &RepositoryPublisher{
		ConnectionSecrets: func(string, string) ([]byte, error) {
			return []byte("tenant-provisioned-token"), nil
		},
	}
}

// publicationCount reads how many publication rows exist for the run, which is the durable half of the
// claim: an approvable question is a ROW, and the assertion has to be about the row rather than about the
// tool's answer.
func publicationCount(t *testing.T, h *publishHarness) int {
	t.Helper()
	pubs, err := h.spine.ApprovedPublicationsForRun(context.Background(), h.tenant, h.runID)
	if err != nil {
		t.Fatalf("read the run's approved publications: %v", err)
	}
	pending, err := h.spine.PendingPublicationApprovals(context.Background(), h.tenant, coordinator.ToolApprovalWindow{Limit: 50})
	if err != nil {
		t.Fatalf("read the pending publication approvals: %v", err)
	}
	n := len(pubs)
	for _, p := range pending {
		if p.RunID == h.runID {
			n++
		}
	}
	return n
}

// TestPublicationRefusedBeforeAHumanIsAsked is the App-less, ref-less case: the binding names no
// credential of its own and the deployment has no App, so there is nothing to publish under.
func TestPublicationRefusedBeforeAHumanIsAsked(t *testing.T) {
	h := newPublishHarness(t)
	env := toolbroker.ExecEnv{
		WorkspaceRoot: h.root,
		Publications:  prechecked(h, appLessPublisher(t)),
		Scope: toolbroker.TaskScope{
			Org: h.tenant.Organization, Project: h.tenant.Project,
			SessionID: h.sessionID, RunID: h.runID, ResponseID: h.respID,
		},
	}

	out, err := tools.PushTool().Exec(context.Background(), env, map[string]any{})
	if err == nil {
		t.Fatalf("the push tool answered %v on a deployment with no credential for this binding: a human is "+
			"now going to be asked to approve a write that cannot happen", out)
	}
	// IT MUST BE AN ANSWER, NOT A FAULT, and this leg is here because the first version of the check was
	// a fault and the live stack said so: three attempt.recovering.v1 from one checkpoint and
	// run.failed.v1, because an unclassified Exec error kills the attempt and the retry ladder spends
	// itself reproducing a refusal that will never change. A guard that wedges the run is worse than the
	// silent skip it replaced.
	if !errors.Is(err, toolbroker.ErrToolAnswer) {
		t.Fatalf("the refusal is a FAULT rather than an answer (%v): the attempt dies with the tool_call row "+
			"still executing, the ledger classifies it uncertain, and the run is retried into the same "+
			"refusal three times before it dead-letters", err)
	}
	for _, want := range []string{"PALAI_GITHUB_APP_ID", "connection_ref"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("the refusal does not name %q, so neither the model nor the operator learns what to do "+
				"about it: %v", want, err)
		}
	}
	if n := publicationCount(t, h); n != 0 {
		t.Fatalf("the refusal still left %d publication row(s) on the run: an approvable question with an "+
			"error message in front of it is the same question", n)
	}
}

// TestPublicationAcceptedWhenTheBindingCarriesItsOwnCredential is the leg that makes the one above mean
// something. SAME App-less deployment, SAME tool — a binding whose connection_ref names a server-side
// credential is accepted and records its pending approval, because that is the configuration the whole
// per-binding credential path exists to serve.
//
// Without this, the refusal above is satisfied by a deployment that refuses every publication.
func TestPublicationAcceptedWhenTheBindingCarriesItsOwnCredential(t *testing.T) {
	h := newPublishHarness(t)
	// A SECOND binding for the same run, carrying its own credential. RunPublicationTarget orders by the
	// preparation receipt, so re-pointing the receipt's binding is what makes the run resolve to this one.
	ownBinding := redeliveryID("bnd")
	ctx := context.Background()
	if err := h.spine.CreateRepositoryBinding(ctx, h.tenant, coordinator.RepositoryBindingInput{
		BindingID: ownBinding, Provider: "github", RepositoryIdentity: "acme/widgets",
		CloneURL: h.remote, DefaultBranch: publishBaseBranch, ConnectionRef: "rcon_tenant_pat",
	}); err != nil {
		t.Fatalf("create the connection-ref binding: %v", err)
	}
	if err := h.spine.RecordPreparationReceipt(ctx, h.tenant, coordinator.PreparationReceiptInput{
		ReceiptID: redeliveryID("prep"), BindingID: ownBinding, RunID: h.runID,
		RequestedRef: publishBaseBranch, BaseCommit: "0000000", TreeHash: "0000000", Branch: h.branch,
	}); err != nil {
		t.Fatalf("record the preparation receipt: %v", err)
	}
	target, found, err := h.spine.RunPublicationTarget(ctx, h.tenant, h.runID)
	if err != nil || !found {
		t.Fatalf("RunPublicationTarget: (%v,%v)", found, err)
	}
	if target.ConnectionRef != "rcon_tenant_pat" {
		t.Fatalf("the run resolves to connection_ref %q, want the binding's own — this test would then be "+
			"measuring the ref-less case again", target.ConnectionRef)
	}

	env := toolbroker.ExecEnv{
		WorkspaceRoot: h.root,
		Publications:  prechecked(h, appLessPublisher(t)),
		Scope: toolbroker.TaskScope{
			Org: h.tenant.Organization, Project: h.tenant.Project,
			SessionID: h.sessionID, RunID: h.runID, ResponseID: h.respID,
		},
	}
	out, err := tools.PushTool().Exec(ctx, env, map[string]any{})
	if err != nil {
		t.Fatalf("a binding carrying its OWN credential was refused on an App-less deployment: %v — this is "+
			"the configuration the connection_ref path exists for", err)
	}
	if status, _ := out["status"].(string); status != "pending_approval" {
		t.Fatalf("the push tool answered status %q, want pending_approval", status)
	}
	if n := publicationCount(t, h); n != 1 {
		t.Fatalf("the accepted publication left %d row(s) on the run, want exactly 1", n)
	}
}
