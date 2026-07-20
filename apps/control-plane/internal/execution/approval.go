package execution

import (
	"context"
	"fmt"
	"log"
	"os"
	"path/filepath"

	"github.com/palgroup/palai/adapters/repositories"
	"github.com/palgroup/palai/adapters/sandboxes/oci/workspace"
	"github.com/palgroup/palai/packages/coordinator"
)

// This file is the approval PUMP — the approval equivalent of command_pump.go (spec §30.9-30.12,
// APV-001). Once an approve command has driven a publication to the DURABLE approved state, the pump
// publishes it at the next live-run boundary: it pushes the branch or opens the draft PR through
// publish.go, then records the external receipt. Every publish is idempotent + reconciliation-safe, so
// a lost ack or E10's detached execution re-drives the SAME code with zero rework — the honest E09
// ceiling is that an approve arriving after the run terminated leaves the row approved for E10.

// Publisher executes one approved publication against the external repository and returns the receipt to
// record (spec §30.9-30.10). RepositoryPublisher is the real implementation (push via the broker, PR via
// the provider client); a fake proves the pump deterministically. A nil publisher disables the pump —
// a stack with no repository publication wired (every existing orchestrator test) simply skips it, the
// SetShellRunner discipline.
type Publisher interface {
	Publish(ctx context.Context, target PublishTarget) (map[string]any, error)
}

// PublishTarget is one approved publication plus the per-attempt context needed to execute it.
type PublishTarget struct {
	Publication   coordinator.Publication
	WorkspaceRoot string // the attempt's workspace allocation root (the repo lives at WorkspaceRoot/repo)
	Org           string
	Project       string
}

// SetPublisher injects the repository publisher the approval pump publishes through. Left unset, an
// approved publication simply waits (the pump is a no-op) — no push happens without a wired publisher.
func (o *Orchestrator) SetPublisher(p Publisher) { o.publisher = p }

// pumpApprovedPublications publishes a run's approved-but-unpublished publications at a safe boundary
// (spec §30.9-30.10, APV-001). It reads the durable approved set, publishes each through the injected
// publisher, and records the receipt (MarkPublicationPublished, single-winner). A publish error leaves
// the row approved and is logged, not fatal: the operation is idempotent, so the next boundary (or E10)
// re-drives it — a transient remote/network failure must never crash the model loop. A nil publisher
// (no repository publication wired) is a clean no-op.
func (o *Orchestrator) pumpApprovedPublications(ctx context.Context, st *attemptState) error {
	if o.publisher == nil {
		return nil
	}
	approved, err := o.spine.ApprovedPublicationsForRun(ctx, st.tenant, string(st.attempt.RunID))
	if err != nil {
		return err
	}
	for _, pub := range approved {
		receipt, err := o.publisher.Publish(ctx, PublishTarget{
			Publication: pub, WorkspaceRoot: st.attempt.WorkspaceHostPath,
			Org: st.tenant.Organization, Project: st.tenant.Project,
		})
		if err != nil {
			// Retry-safe: the row stays approved, so the next boundary (or E10's detached execution)
			// re-drives the idempotent publish. Log it; never fail the run over a publish error.
			log.Printf("approval pump: publish %s (%s) failed: %v", pub.ID, pub.Operation, err)
			continue
		}
		if err := o.spine.MarkPublicationPublished(ctx, st.tenant, st.sessionID, st.responseID, pub.ID, pub.Operation, receipt); err != nil {
			return err
		}
	}
	return nil
}

// RepositoryPublisher publishes through the adapters/repositories push/PR path (spec §30.9-30.10): a
// push mints a write-scoped broker credential and pushes the exact approved head; a pull request finds
// or opens a draft through the provider client. It holds only the broker + PR client + branch policy —
// everything else comes from the publication row + the attempt's workspace, so the model never supplies
// a destination.
type RepositoryPublisher struct {
	Broker    repositories.Broker
	PRClient  repositories.PullRequestClient // nil disables PR publication (push-only stacks)
	Protected []string                       // policy-widened protected branches (default main/master always)
}

// Publish executes one approved publication (spec §30.9-30.10). The credential rides only the broker
// helper file; a per-publish temp secrets dir holds it and is removed after (defence in depth on top of
// the broker's own revoke).
func (p *RepositoryPublisher) Publish(ctx context.Context, target PublishTarget) (map[string]any, error) {
	pub := target.Publication
	audience := repositories.Audience{Organization: target.Org, Project: target.Project, Run: pub.RunID, ToolCall: pub.ID}
	switch pub.Operation {
	case "push_branch":
		if target.WorkspaceRoot == "" {
			return nil, fmt.Errorf("publish push %s: no workspace bound for the run", pub.ID)
		}
		secrets, err := os.MkdirTemp("", "palai-publish-")
		if err != nil {
			return nil, fmt.Errorf("publish push %s: secrets dir: %w", pub.ID, err)
		}
		defer os.RemoveAll(secrets)
		receipt, err := repositories.PushBranch(ctx, p.Broker, repositories.PushRequest{
			Remote: pub.Remote, RepoDir: filepath.Join(target.WorkspaceRoot, workspace.RepoDir),
			Branch: pub.Branch, HeadSHA: pub.HeadSHA, Protected: p.Protected, SecretsDir: secrets, Audience: audience,
		})
		if err != nil {
			return nil, err
		}
		return map[string]any{
			"remote": receipt.Remote, "branch": receipt.Branch,
			"remote_sha": receipt.RemoteSHA, "reconciled": receipt.Reconciled,
		}, nil
	case "open_pull_request":
		if p.PRClient == nil {
			return nil, fmt.Errorf("publish pull request %s: no pull-request client wired", pub.ID)
		}
		pr, err := repositories.OpenPullRequest(ctx, p.PRClient, repositories.OpenPRInput{
			HeadBranch: pub.Branch, Base: pub.Base, Title: "Agent changes: " + pub.Branch, Body: pub.Display,
		})
		if err != nil {
			return nil, err
		}
		return map[string]any{"pull_request_id": pr.ID, "url": pr.URL, "number": pr.Number, "draft": pr.Draft}, nil
	default:
		return nil, fmt.Errorf("publish %s: unknown operation %q", pub.ID, pub.Operation)
	}
}
