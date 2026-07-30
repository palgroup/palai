package execution

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/palgroup/palai/adapters/repositories"
	"github.com/palgroup/palai/adapters/sandboxes/oci/workspace"
	"github.com/palgroup/palai/packages/coordinator"
	statemachines "github.com/palgroup/palai/packages/state-machines"
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

// ---------------------------------------------------------------------------------------------------
// E23 T1 — THE GENERIC HALF: a tool call parks its run on a human. Everything above this line is the
// publication pump; everything below gates ANY tool call and knows nothing about repositories.
// ---------------------------------------------------------------------------------------------------

// errRunParked ends an attempt cleanly when the run has been released to WAITING at a boundary it will
// be woken back to (spec §22.4, §26.7): the attempt ends with no engine process held, and whatever the
// run is waiting on — a human's decision, the approval expiry reaper, or a background task's exit —
// opens a fresh attempt through coordinator.wakeParkedRunTx. Like errRunPaused and errRunReleased it is
// NOT a failure: ExecuteAttempt returns nil on it, so the worker is freed even in a single-runner stack
// and a parked run costs no compute while whatever it waits for happens.
//
// It was named for the approval gate until E26 T3 gave it a second producer. The name is the only thing
// that changed: renaming rather than adding a sibling sentinel is the same rule as parkRun below — a second
// value would mean a second arm in the run loop, and an arm somebody forgets is a run that fails instead
// of parking.
var errRunParked = errors.New("run_parked")

// parkRun releases the run's compute while something outside this attempt happens (spec §22.4, §26.5).
// It follows the pause/detach choreography: capture a durable checkpoint of this boundary if a sink is
// wired, drive the run running→waiting, move the response to the state that says WHY it is waiting, and
// hand back errRunParked so the attempt ends without sending a tool.result — the model is given NOTHING
// to continue on, which is the whole difference between this and a tool that answers "pending" and lets
// the run finish.
//
// IT DOES NOT REFUSE A CHECKPOINT-LESS PARK, and that is a deliberate departure from detach.go, which
// does. §26.5 forbids releasing with no recoverable boundary, and a detached child HAS no other boundary
// to come back to: its result must be folded at exactly the point the parent left. A parked approval
// does: the woken attempt replays the committed transcript, reaches the SAME tool.request, and consults
// the SAME ledger row — recovery ladder rung 2, which is always available. Requiring a checkpoint sink
// would make the approval gate unavailable on every deployment without object storage, which is most of
// them, and an unavailable gate is a gate nobody has. E26 T3's background park inherits that reasoning
// VERBATIM and needed no second version of it: the woken attempt replays the transcript, reaches the same
// terminal frame, and re-reads the same task state.
//
// respCmd IS OPTIONAL AND EMPTY MEANS "SAY NOTHING", which is what E23's and E08's parks pass. Their
// response states (waiting_for_approval, waiting_for_input) are declared in ResponseTable and have never
// been written either; binding them is those paths' owners' change, and doing it here would move two
// surfaces this task cannot test through their own flows. What it must not do is bind the WRONG one, so
// the parameter is explicit rather than defaulted.
func (o *Orchestrator) parkRun(ctx context.Context, st *attemptState, respCmd statemachines.ResponseCommand) error {
	if o.checkpoints != nil {
		if err := o.checkpointBeforePause(ctx, st); err != nil {
			return err
		}
	}
	if _, err := o.spine.ApplyRunTransition(ctx, st.tenant, string(st.attempt.RunID), statemachines.RunCmdWait); err != nil {
		return err
	}
	if respCmd != "" {
		if err := o.advanceResponse(ctx, st.tenant, st.responseID, respCmd); err != nil {
			return err
		}
	}
	return errRunParked
}

// parkOnPendingPublication is E23 T3, and it builds nothing: it makes E22's closing claim true. E22 said
// the agent could write code and publish it behind a human's button. The publication was recorded and the
// decision chain was production — but the tool ANSWERED the model with "pending_approval" and the run
// carried on to `completed`, so by the time anybody looked at Slack the click hit guardRunActive, Decide
// returned an error, the route answered 503, and the publication stayed pending forever. E22 measured
// exactly that and wrote it down (TestPublicationFromSlackCeiling…). The button existed and could not be
// pressed in the flow that needed it.
//
// So the run parks on its own pending publication, through the SAME path a gated tool call parks on
// (parkRun): running→waiting, no tool.result, no engine process held. The fix is the ROOT one and
// lives in one place — the dispatcher, after any tool commits — rather than in each publication tool, so
// merge (T6) and whatever the fourth publication operation turns out to be inherit it without a line.
//
// THE AUTHORITY IS THE DATABASE, NEVER THE RESULT BYTES. The park is decided by a pending `approvals` row
// this RUN owns, not by a tool answering `{"status":"pending_approval"}` — an MCP server can write that
// string, and a tool result is untrusted data (§2). What a remote server says about its own call can
// therefore never park a run, and the one indexed read this costs is publications_session_pending.
//
// INHERITED CEILING, unchanged by this epic: PendingApprovalForSession takes the session's OLDEST pending
// approval, so a second concurrent one is not decidable until the first clears. The RunID comparison is
// what keeps that ceiling honest here — a run never parks on a DIFFERENT run's question, because nothing
// would ever wake it if it did.
func (o *Orchestrator) parkOnPendingPublication(ctx context.Context, st *attemptState) (bool, error) {
	pub, found, err := o.spine.PendingApprovalForSession(ctx, st.tenant, st.sessionID)
	if err != nil || !found || pub.RunID != string(st.attempt.RunID) {
		return false, err
	}
	// No response command: waiting_for_approval is ResponseTable's, not this epic's (see parkRun).
	return true, o.parkRun(ctx, st, "")
}

// WHY THE PUBLICATION'S APPROVAL DISPLAY STAYS SEPARATE FROM THE GENERIC ONE (E23 T3, and this is the
// note the plan asks for rather than a merge nobody wrote down):
//
// publicationDisplay (publication_registry.go, untouched by this epic) builds its sentence from the
// RESOLVED DESTINATION — the remote, branch and base the binding and the preparation receipt decided,
// plus the exact head that will be pushed. Not one character of it is reachable by the model, which is
// why the pull-request tool's InputSchema has no destination field at all.
//
// DeriveToolApprovalDisplay (E23 T1) builds ITS screen from the CALL: the identity
// resolved through the executing lookup, the operator's registration-time label, and the full argument
// object — and those arguments are the model's, defused by being labelled as the model's rather than by
// being infrastructure-derived.
//
// The two are the same shape and DIFFERENT guarantees. Folding them into one "generic display" would not
// average them; it would drag the stronger down to the weaker, because the only signature both fit is the
// one that carries a name and a bag of arguments — and a push whose destination arrived as an argument is
// precisely the push this whole path exists to prevent. Two functions is the cost of keeping the stronger
// claim true.

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
	// ExpireApprovalIfElapsed is the consume-time expiry guard (spec §22.4, E10 T7): before a publish, an
	// approved publication whose one-shot approval elapsed is expired + journaled and reported true, so
	// the pump skips it. A live approval reports false and publishes unchanged.
	ExpireApprovalIfElapsed(ctx context.Context, tenant coordinator.Tenant, sessionID, responseID, publicationID string) (bool, error)
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
		// Consume-time expiry guard (spec §22.4, E10 T7): an approval that elapsed between approval and
		// this boundary is expired + journaled here, and the publish is skipped — an expired approval
		// never pushes. A live approval reports false and publishes exactly as before (bit-unchanged).
		switch expired, err := spine.ExpireApprovalIfElapsed(ctx, tenant, sessionID, responseID, pub.ID); {
		case err != nil:
			return err
		case expired:
			continue
		}
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
	case "merge_pull_request":
		if p.PRClient == nil {
			return nil, fmt.Errorf("publish merge %s: no pull-request client wired", pub.ID)
		}
		// The local protected-branch policy is checked for a merge too — on the HEAD, the same branch and the
		// same predicate a push checks (DirectWorkAllowed is "may an agent do direct mutable work on this
		// branch"). It is NOT applied to the base, and that omission is the design rather than an oversight:
		// the base of a pull request is main by construction in most deployments, and refusing every merge
		// into a protected branch would not protect it — it would delete the feature and leave the protection
		// to be turned off. WHAT GUARDS THE BASE IS GITHUB'S OWN BRANCH PROTECTION, which E23 TRUSTS and does
		// not reimplement: no required-checks query, no review request, no CI wait. Every one of those
		// refusals arrives as the documented 405 and becomes a visible warning below.
		if !repositories.DirectWorkAllowed(pub.Branch, p.Protected) {
			return nil, fmt.Errorf("merge from %q: %w", pub.Branch, repositories.ErrProtectedBranch)
		}
		number, _ := pub.Args["pull_request_number"].(float64) // JSONB round-trip: numbers decode as float64
		method, _ := pub.Args["merge_method"].(string)
		receipt, err := repositories.MergePullRequest(ctx, p.PRClient, repositories.MergeInput{
			// The head is the APPROVED publication's own, sent as `sha`. GitHub marks the field optional;
			// making it mandatory is what buys the race guard for free (P16, 409 on a moved head).
			Number: int(number), HeadSHA: pub.HeadSHA, Method: method,
		})
		if err != nil {
			return nil, err
		}
		return map[string]any{"merged": receipt.Merged, "sha": receipt.SHA, "message": receipt.Message,
			"number": int(number), "merge_method": method}, nil
	default:
		return nil, fmt.Errorf("publish %s: unknown operation %q", pub.ID, pub.Operation)
	}
}
