package execution

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/palgroup/palai/adapters/repositories"
	"github.com/palgroup/palai/adapters/sandboxes/oci/workspace"
	"github.com/palgroup/palai/packages/coordinator"
)

// This file is the approval PUMP — the approval equivalent of command_pump.go (spec §30.9-30.12,
// APV-001). Once an approve command has driven a publication to the DURABLE approved state, the pump
// publishes it at the next live-run boundary: it pushes the branch or opens the draft PR through
// publish.go, then records the external receipt. Every publish is idempotent + reconciliation-safe, so a
// lost ack or E10's detached execution re-drives the SAME code with zero rework — the honest E09 ceiling
// is that an approve arriving after the run terminated leaves the row approved for E10.
//
// ponytail: post-terminal (E10) execution depends on workspace snapshot/restore preserving the
// workspace repo's .git so a re-drive can still push the approved head — recorded as an E10 concern.

// publishTimeout bounds one boundary publish so a hanging remote (git push has no inherent deadline)
// cannot block the model-loop boundary indefinitely.
const publishTimeout = 60 * time.Second

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
	AttemptFence  uint64 // binds the minted push credential to this attempt (§28.11)
}

// PublicationPump is the store seam the approval pump reads approved publications from and records
// receipts / warnings through (the ReconcileStore idiom). *coordinator.Store implements it; a fake in
// the pump test proves the boundary-pump against a real bare remote without a database.
type PublicationPump interface {
	ApprovedPublicationsForRun(ctx context.Context, tenant coordinator.Tenant, runID string) ([]coordinator.Publication, error)
	MarkPublicationPublished(ctx context.Context, tenant coordinator.Tenant, sessionID, responseID, publicationID, operation string, receipt map[string]any) error
	RecordPublicationWarning(ctx context.Context, tenant coordinator.Tenant, sessionID, responseID, publicationID, detail string) error
}

// SetPublisher injects the repository publisher the approval pump publishes through. Left unset, an
// approved publication simply waits (the pump is a no-op) — no push happens without a wired publisher.
func (o *Orchestrator) SetPublisher(p Publisher) { o.publisher = p }

// pumpApprovedPublications publishes a run's approved publications at a safe boundary (spec §30.9-30.10,
// APV-001). A nil publisher (no repository publication wired) is a clean no-op; otherwise it drives the
// pure publishApproved body against the durable spine.
func (o *Orchestrator) pumpApprovedPublications(ctx context.Context, st *attemptState) error {
	if o.publisher == nil {
		return nil
	}
	return publishApproved(ctx, o.spine, o.publisher, st.tenant, string(st.attempt.RunID), st.sessionID, st.responseID, st.attempt.WorkspaceHostPath, st.attempt.Fence)
}

// publishApproved is the pure boundary-pump body: it reads the run's DURABLE approved-but-unpublished
// publications, publishes each through the publisher under a per-publish timeout, and records the
// receipt single-winner. A publish error leaves the row approved and journals a VISIBLE warning
// (REP-010) rather than a silent server-log loop: the operation is idempotent, so the next boundary (or
// E10) re-drives it, and the model/user sees the failure to choose rebase/merge/wait. It takes the store
// as a seam so the pump is provable with a fake store + a real bare remote, no database.
func publishApproved(ctx context.Context, spine PublicationPump, publisher Publisher, tenant coordinator.Tenant, runID, sessionID, responseID, workspaceRoot string, fence uint64) error {
	approved, err := spine.ApprovedPublicationsForRun(ctx, tenant, runID)
	if err != nil {
		return err
	}
	for _, pub := range approved {
		pubCtx, cancel := context.WithTimeout(ctx, publishTimeout)
		receipt, perr := publisher.Publish(pubCtx, PublishTarget{
			Publication: pub, WorkspaceRoot: workspaceRoot,
			Org: tenant.Organization, Project: tenant.Project, AttemptFence: fence,
		})
		cancel()
		if perr != nil {
			// ponytail: a persistent hard error (e.g. a diverged remote) re-warns each boundary; a
			// dedupe/terminal-fail transition is the E10 refinement. The warning is the visible surface.
			if werr := spine.RecordPublicationWarning(ctx, tenant, sessionID, responseID, pub.ID, perr.Error()); werr != nil {
				return werr
			}
			continue
		}
		if err := spine.MarkPublicationPublished(ctx, tenant, sessionID, responseID, pub.ID, pub.Operation, receipt); err != nil {
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
// helper file and binds to the attempt fence; a per-publish temp secrets dir holds it and is removed
// after (defence in depth on top of the broker's own revoke).
func (p *RepositoryPublisher) Publish(ctx context.Context, target PublishTarget) (map[string]any, error) {
	pub := target.Publication
	audience := repositories.Audience{
		Organization: target.Org, Project: target.Project,
		Run: pub.RunID, AttemptFence: target.AttemptFence, ToolCall: pub.ID,
	}
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
		// ponytail: E09 opens with a deterministic default title/body; the model's proposed title/body
		// are recorded on the publication (args) for a later policy-filtered pass, not yet applied.
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
