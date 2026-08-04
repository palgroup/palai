package coordinator

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/palgroup/palai/storage"
)

// Publication lifecycle events (spec §30.8-30.12, §22.4-22.5). approval.requested is a genesis event
// (the pending approval is born, not a table transition — like command.accepted.v1); the rest mark the
// single-winner transitions the command spine and the approval pump drive. They live in
// protocols/schemas/execution/event-types.json + the AsyncAPI x-event-types (drift-checked).
const (
	approvalRequestedEvent = "approval.requested.v1"
	approvalApprovedEvent  = "approval.approved.v1"
	approvalDeniedEvent    = "approval.denied.v1"
	// approvalExpiredEvent marks a one-shot approval that passed its minutes-scale expiry (spec §22.4)
	// before it was consumed — forward-declared in 000013 (the 'expired' publication state + expires_at)
	// and enforced in E10 T7. It is already in the canonical registry (event-types.json + AsyncAPI); the
	// consume-time guard + reconcile sweep are the code half, no schema.
	approvalExpiredEvent   = "approval.expired.v1"
	pushCompletedEvent     = "push.completed.v1"
	pullRequestOpenedEvent = "pull_request.opened.v1"
	// pullRequestMergedEvent is E23 T6's receipt event. A merge journalling push.completed.v1 would have
	// been the smallest possible edit and a lie in the audit stream — the one place a lie is permanent.
	pullRequestMergedEvent = "pull_request.merged.v1"
	warningRaisedEvent     = "warning.raised.v1"
)

// The approver surfaces (E23 T2). A surface is WHERE an identity was authenticated, and it is part of the
// principal rather than a separate field because the two id spaces do not overlap and must never be
// allowed to: nothing links a Slack account to an API key, so a principal that did not say which one it
// came from would be an invitation to match one against the other.
const (
	ApproverSurfaceSlack = "slack"
	ApproverSurfaceKey   = "key"
)

// ApproverPrincipal renders one surface's identity as the canonical approver principal — the only string
// form config_policy.approvers ever names:
//
//	Slack → slack:<team_id>:<user_id>
//	HTTP  → key:<api_key_id>            (workspace is empty)
//
// EVERY surface maps its identity HERE, and that is the point rather than a tidiness preference. The Slack
// form is WORKSPACE-QUALIFIED because a Slack user id is unique only within its workspace: an unqualified
// id in an operator's list would admit a stranger from another workspace who happens to share it. The HTTP
// form names the API KEY, not the principal_id behind it — api_keys.principal_id carries no UNIQUE
// constraint (000001_core.up.sql:34), so several keys may share one principal, and an operator who revokes
// a key expects to have revoked what that key could approve.
//
// An identity that is not one renders "", which no list can name: an unknown surface, a Slack user with no
// workspace, an empty id. Combined with ApproverAllowed's refusal to match an empty principal, a caller
// this function cannot describe decides nothing whenever a list exists.
//
// HONEST CEILING, and it is the ceiling of the whole feature: PALAI HAS NO USER IDENTITY. A principal is a
// Slack account or an API key — it is NOT a person. Nothing here links the same human's two identities
// across surfaces, and this epic does not build that (TEN-004, end-user token exchange, is still post-1.0).
// Two-person approval does not exist on this path either: one approval is one principal.
func ApproverPrincipal(surface, workspace, id string) string {
	if id == "" {
		return ""
	}
	switch surface {
	case ApproverSurfaceSlack:
		if workspace == "" {
			return ""
		}
		return ApproverSurfaceSlack + ":" + workspace + ":" + id
	case ApproverSurfaceKey:
		return ApproverSurfaceKey + ":" + id
	default:
		return ""
	}
}

// PublicationRequest records one decomposed side-effect operation awaiting approval (spec §30.8). The
// remote/branch/base/head come from the resolved binding + preparation receipt, NOT model output, so the
// approved operation is exactly what infrastructure computed. IdempotencyKey/RequestHash are formed by
// the adapter (repositories.IdempotencyKey/RequestHash) so the dedupe + one-shot binding are one
// definition.
type PublicationRequest struct {
	PublicationID   string
	ApprovalID      string
	SessionID       string
	RunID           string
	ResponseID      string
	Operation       string // push_branch | open_pull_request
	Remote          string
	Branch          string
	Base            string
	HeadSHA         string
	IdempotencyKey  string
	RequestHash     string
	Display         string
	Args            map[string]any
	AllowedApprover string
}

// Publication is the durable projection of a publication row + its approval binding (spec §30.8). It is
// the pending-approval the command spine gates on, the approved operation the pump publishes, and the
// receipt once published.
type Publication struct {
	ID             string
	SessionID      string
	RunID          string
	ResponseID     string
	Operation      string
	Remote         string
	Branch         string
	Base           string
	HeadSHA        string
	IdempotencyKey string
	Display        string
	State          string
	Receipt        []byte
	RequestHash    string
	// Args is the row's own recorded argument object, and E23 T6 is the first thing to READ it. It carries
	// what a merge needs and the publication columns have no home for — the pull request number the
	// publisher resolved and the merge method the binding declared. None of it is the model's: the merge
	// tool takes no arguments at all, and the two values are written by the registry from a store read.
	Args map[string]any
	// Replayed marks a duplicate idempotency_key that returned the ORIGINAL row rather than a second
	// pending approval — the model re-proposing the same push does not stack approvals (spec §30.8).
	Replayed bool
}

// PublicationTarget is a run's publication destination, resolved from its preparation receipt + binding
// (spec §30.9): the clean remote URL, the work branch to push, and the base branch. found is false when
// the run prepared no repository — then there is nothing to publish.
type PublicationTarget struct {
	Remote string
	Branch string
	Base   string
	// MergeMethod is the binding policy's merge_method (E23 T6): merge | squash | rebase, empty meaning
	// the `merge` default. It is deployment policy rather than a per-PR choice — the honest ceiling — and
	// it lives beside the base for the same reason: both decide how work lands, and neither is the
	// model's to name.
	MergeMethod string
	// ConnectionRef is the binding's OWN credential handle (repository_bindings.connection_ref), empty
	// when the binding takes the deployment-global App. It is an OPAQUE HANDLE and never a token: the
	// bytes live in the secret-ref store and are resolved server-side at publish time, exactly as the
	// clone half already resolves them (main.repositoryConnectionSecret, E13 T9).
	//
	// It rides THIS read because the publish half had no other way to learn it. Clone was
	// panel-provisioned and tenant-scoped; push and pull request were deployment-global and blind to the
	// binding — so a tenant who provisioned a credential in the admin panel had it honoured on the way IN
	// and ignored on the way OUT.
	ConnectionRef string
	// Identity is the binding's `owner/repo` (repository_bindings.repository_identity). The pull-request
	// client needs it, and until now it came from PALAI_GITHUB_REPO / PALAI_GIT_REPO — a DEPLOYMENT-WIDE
	// env var, so one stack could open pull requests against exactly one repository no matter how many
	// bindings it had. publish.go's own comment already claimed "Owner/repo come from the binding, not the
	// model"; this is the read that makes that sentence true.
	Identity string
}

// RunPublicationTarget resolves a run's publication destination — the remote/branch/base a push or PR
// targets — from the run's latest preparation receipt joined to its binding (spec §30.9). It is
// infrastructure-owned: the model never supplies a remote, so an agent cannot redirect a publication.
func (s *Store) RunPublicationTarget(ctx context.Context, tenant Tenant, runID string) (PublicationTarget, bool, error) {
	ctx = storage.ScopeToTenant(ctx, tenant.Project)
	var t PublicationTarget
	err := s.pool.QueryRow(ctx, storage.Query("RunPublicationTarget"), runID, tenant.Project).
		Scan(&t.Remote, &t.Branch, &t.Base, &t.MergeMethod, &t.ConnectionRef, &t.Identity)
	if errors.Is(err, pgx.ErrNoRows) {
		return PublicationTarget{}, false, nil
	}
	if err != nil {
		return PublicationTarget{}, false, fmt.Errorf("resolve run publication target: %w", err)
	}
	return t, true, nil
}

// RunPublishedPullRequest resolves the pull request THIS RUN opened — its number and the head it was
// opened at — from the run's own published open_pull_request receipt (E23 T6). found is false when the run
// has published no pull request, which is the honest refusal for "merge" before there is anything to merge.
//
// It is the whole of the destination guarantee for a merge. The number is the provider's own answer,
// written by the publisher into the receipt of a publication a human approved; the head is the one that
// publication carried. A model that wants a different pull request merged has no field to say so in.
func (s *Store) RunPublishedPullRequest(ctx context.Context, tenant Tenant, runID string) (number int, headSHA string, found bool, err error) {
	ctx = storage.ScopeToTenant(ctx, tenant.Project)
	switch err := s.pool.QueryRow(ctx, storage.Query("RunPublishedPullRequest"), runID, tenant.Project).
		Scan(&number, &headSHA); {
	case errors.Is(err, pgx.ErrNoRows):
		return 0, "", false, nil
	case err != nil:
		return 0, "", false, fmt.Errorf("resolve run published pull request: %w", err)
	}
	if number <= 0 {
		// A published PR whose receipt carries no number is a receipt this code cannot merge from. Refuse
		// rather than merge "#0" — an operation aimed at nothing is worse than one that did not happen.
		return 0, "", false, nil
	}
	return number, headSHA, true, nil
}

// RequestPublication records a pending publication + its one-shot approval binding idempotently (spec
// §30.8, §22.4). A duplicate idempotency_key returns the ORIGINAL publication (Replayed) — the tool
// re-proposing the same operation resolves to the existing pending approval, never a second. It journals
// approval.requested.v1 on the first insert only, so a replay does not re-journal.
func (s *Store) RequestPublication(ctx context.Context, tenant Tenant, in PublicationRequest) (Publication, error) {
	ctx = storage.ScopeToTenant(ctx, tenant.Project)
	args := in.Args
	if args == nil {
		args = map[string]any{}
	}
	argsJSON, err := json.Marshal(args)
	if err != nil {
		return Publication{}, fmt.Errorf("marshal publication args: %w", err)
	}
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return Publication{}, fmt.Errorf("begin request publication: %w", err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()

	// Reserve the publication. ON CONFLICT on the idempotency key returns no row for a duplicate, so we
	// read and replay the original (idempotent request — no second pending approval).
	err = tx.QueryRow(ctx, storage.Query("InsertPublication"),
		in.PublicationID, tenant.Project, in.SessionID, in.RunID, nullableText(in.ResponseID),
		in.Operation, in.Remote, in.Branch, in.Base, in.HeadSHA, in.IdempotencyKey, in.Display, argsJSON).
		Scan(new(string))
	if errors.Is(err, pgx.ErrNoRows) {
		pub, err := publicationByKey(ctx, tx, tenant, in.IdempotencyKey)
		if err != nil {
			return Publication{}, err
		}
		if err := tx.Commit(ctx); err != nil {
			return Publication{}, fmt.Errorf("commit publication replay: %w", err)
		}
		pub.Replayed = true
		return pub, nil
	}
	if err != nil {
		return Publication{}, fmt.Errorf("insert publication: %w", err)
	}

	// THE DEADLINE, and until E23 T3 nothing ever wrote one. 000013 declared expires_at in 2023 and E10 T7
	// built both consume-time guards plus the idle sweep around it, all of which were dead code because
	// every publication approval was born with a NULL deadline. It was harmless while a publication tool
	// answered the model and the run carried on; it stops being harmless the moment the run PARKS on the
	// answer, because then "nobody clicked" is a run that waits forever. Same clock as a gated tool call
	// (ApprovalTTL), because a human reading two kinds of question has one attention span.
	expiresAt := time.Now().Add(ApprovalTTL())
	if _, err := tx.Exec(ctx, storage.Query("InsertApproval"),
		in.ApprovalID, in.PublicationID, tenant.Project, in.RequestHash, in.AllowedApprover, expiresAt); err != nil {
		return Publication{}, fmt.Errorf("insert approval: %w", err)
	}
	// THE ORDER TO POST (E23 T3, 000044 R4), committed in the SAME transaction as the approval it
	// announces — 000041's discipline verbatim. Until now nothing in this tree posted an approval message,
	// so a human's path to the button existed only in tests: `ApprovalMessage` had Approve/Deny buttons and
	// no production caller. Writing the order here rather than from the poster is the loss-lessness claim:
	// "a human owes an answer" and "the question is recorded for delivery" become durable together, so a
	// poster that is down, restarting or absent delays the question and can never lose it.
	//
	// A session with no Slack thread inserts ZERO rows, which is every non-Slack run in the deployment.
	if _, err := tx.Exec(ctx, storage.Query("EnqueueApprovalMessage"),
		tenant.Project, in.ApprovalID, in.RunID, in.ResponseID, in.SessionID); err != nil {
		return Publication{}, fmt.Errorf("enqueue approval message: %w", err)
	}
	payload := mustMarshal(map[string]any{
		"publication_id": in.PublicationID, "operation": in.Operation, "branch": in.Branch,
		"request_hash": in.RequestHash, "display": in.Display,
	})
	if _, err := appendEvent(ctx, tx, tenant, in.SessionID, in.ResponseID, approvalRequestedEvent, payload); err != nil {
		return Publication{}, err
	}
	pub, err := publicationByKey(ctx, tx, tenant, in.IdempotencyKey)
	if err != nil {
		return Publication{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return Publication{}, fmt.Errorf("commit request publication: %w", err)
	}
	return pub, nil
}

// PendingApprovalForSession returns the session's oldest publication still awaiting approval — the
// command spine's read to decide whether an approve/deny has a target (spec §22.4). found=false → the
// E08 no_pending_approval rejection is preserved (TestApproveWithoutPendingApprovalRejected).
func (s *Store) PendingApprovalForSession(ctx context.Context, tenant Tenant, sessionID string) (Publication, bool, error) {
	ctx = storage.ScopeToTenant(ctx, tenant.Project)
	row := s.pool.QueryRow(ctx, storage.Query("PendingApprovalForSession"), sessionID, tenant.Project)
	pub, err := scanPublication(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return Publication{}, false, nil
	}
	if err != nil {
		return Publication{}, false, fmt.Errorf("read pending approval: %w", err)
	}
	return pub, true, nil
}

// PublicationState reads one publication's current state (found=false for an unknown or foreign id — a
// cross-tenant id leaks nothing). It is the "did my decision land?" read for a caller whose approve/deny
// command was settled by another path: the boundary pump applies the SAME command under the SAME one-shot
// hash, so the decision is durable and the caller must report it — but an expiry sweep settles a queued
// command having decided nothing, and only the publication's own state tells those apart.
func (s *Store) PublicationState(ctx context.Context, tenant Tenant, publicationID string) (string, bool, error) {
	ctx = storage.ScopeToTenant(ctx, tenant.Project)
	var state string
	switch err := s.pool.QueryRow(ctx, storage.Query("PublicationState"), publicationID, tenant.Project).
		Scan(&state); {
	case errors.Is(err, pgx.ErrNoRows):
		return "", false, nil
	case err != nil:
		return "", false, fmt.Errorf("read publication state: %w", err)
	}
	return state, true, nil
}

// ErrApproverNotAuthorized reports that a decision arrived from a principal the project's approver
// allow-list does not name (E23 T2). The command is SETTLED before this is returned — a refusal that left
// the command queued would be re-read by the boundary pump at every boundary for the life of the run —
// so callers treat it as "the receiver worked and the click/POST authorized nothing", never as a failure.
var ErrApproverNotAuthorized = errors.New("approver_not_authorized")

// ApplyApprovalDecision applies a queued approve/deny command at a safe boundary (spec §22.4-22.5,
// APV-001). In one transaction it transitions the session's pending publication (approve ->
// approved, deny -> denied), records who decided, journals approval.approved/denied.v1, and marks the
// command applied — so the approved (durable) publication and the applied command commit together. It
// runs under guardRunActive (the pump's fence discipline). requestHash is the one-shot binding from the
// approve command: a mismatch (a stale approve for a head that moved, or an edited request) authorizes
// nothing but still settles the command. A missing pending approval is a no-op that settles the command.
//
// approver is the canonical principal of whoever is deciding (ApproverPrincipal), and THIS is where the
// project's approver allow-list is enforced — for BOTH surfaces, because this is the one function both
// pass through. Slack's Decide calls it directly; the HTTP surface's queued command is applied here by the
// boundary pump. Putting the check in each caller instead is how the next caller forgets it, and there is
// an untagged guard (TestApproverAllowedHasExactlyOneProductionCallSite) that counts.
//
// The list is read INSIDE this transaction, so it is live at the moment of decision rather than frozen
// when the approval was created. That is deliberate and the argument is the tree's own, written for
// allowed_channels at slack_decision.go: an operator NARROWING an allow-list is what containing an
// incident looks like, and it would otherwise leave every already-posted button still live. Removing an
// approver takes the pending approvals with it.
func (s *Store) ApplyApprovalDecision(ctx context.Context, tenant Tenant, sessionID, responseID, runID, commandID, kind, requestHash, approver string) (int64, error) {
	ctx = storage.ScopeToTenant(ctx, tenant.Project)
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return 0, fmt.Errorf("begin apply approval: %w", err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()

	if err := guardRunActive(ctx, tx, tenant, runID); err != nil {
		return 0, err
	}

	// Lock the session's pending publication so the transition is single-winner.
	var pubID, pendingHash string
	var expiresAt *time.Time
	switch err := tx.QueryRow(ctx, storage.Query("LockPendingApprovalForSession"), sessionID, tenant.Project).
		Scan(&pubID, &pendingHash, &expiresAt); {
	case errors.Is(err, pgx.ErrNoRows):
		// No pending approval (already resolved by a racing path): settle the command, transition nothing.
		return settleApprovalCommandTx(ctx, tx, tenant, sessionID, responseID, commandID)
	case err != nil:
		return 0, fmt.Errorf("lock pending approval: %w", err)
	}

	// WHO IS DECIDING (E23 T2), checked BEFORE the expiry and hash guards below, and the order matters
	// twice over. First: an unauthorized principal must cause NO state transition at all — not even the
	// expiry one — so nothing about this session moves because a stranger pressed a button. Second: it
	// closes an oracle. Checked after the hash, a stranger could tell a valid request hash from an invalid
	// one by which refusal came back; checked here, every unauthorized caller gets the same answer
	// whatever they present.
	//
	// The command is settled anyway (see settleApprovalCommandTx) and the error is returned so a caller
	// can draw a refusal rather than a decision.
	allowed, err := approverAuthorizedTx(ctx, tx, tenant, approver)
	if err != nil {
		return 0, err
	}
	if !allowed {
		if _, err := settleApprovalCommandTx(ctx, tx, tenant, sessionID, responseID, commandID); err != nil {
			return 0, err
		}
		return 0, ErrApproverNotAuthorized
	}

	// Consume-time expiry guard (spec §22.4, E10 T7): an approve/deny that arrives after the one-shot
	// approval passed its minutes-scale expiry authorizes NOTHING. Expire the publication (pending ->
	// expired) + journal approval.expired.v1, then settle the command — the same "authorizes nothing but
	// settles the command" shape the stale-hash branch uses. Checked before the hash so an expired
	// approval never approves regardless of the token.
	if expiresAt != nil && expiresAt.Before(time.Now()) {
		if _, err := expirePublicationTx(ctx, tx, tenant, sessionID, responseID, pubID, "pending_approval"); err != nil {
			return 0, err
		}
		// A click that arrives one second late authorizes nothing — and must not be the reason a run parked
		// on this approval waits forever. Same pairing the gated-call path makes: whatever closes the
		// question releases what was waiting on it.
		if err := wakeParkedRunTx(ctx, tx, tenant, runID); err != nil {
			return 0, err
		}
		return settleApprovalCommandTx(ctx, tx, tenant, sessionID, responseID, commandID)
	}

	// A stale one-shot token (the head moved -> a new pending approval carries a new hash, or the args
	// were edited) authorizes nothing: settle the command without transitioning the publication.
	if requestHash != "" && requestHash != pendingHash {
		return settleApprovalCommandTx(ctx, tx, tenant, sessionID, responseID, commandID)
	}

	newState, event := "approved", approvalApprovedEvent
	if kind == "deny" {
		newState, event = "denied", approvalDeniedEvent
	}
	if _, err := tx.Exec(ctx, storage.Query("SetPublicationState"),
		pubID, tenant.Project, newState, "pending_approval"); err != nil {
		return 0, fmt.Errorf("transition publication: %w", err)
	}
	// WHO DECIDED, AND THIS COLUMN USED TO HOLD THE WRONG THING. It was passed `commandID`, so every
	// publication approval this system has ever recorded answered "who authorized this write" with a
	// `cmd_…` id — while the query's own comment said "records who decided an approval (audit)" and
	// slack_decision.go:258 said "recorded durably on the approval's decided_by, which is where an
	// identity belongs". Three sentences describing a principal, over a column holding a command id.
	//
	// `approver` was in scope the whole time: approverAuthorizedTx above already decides authorization on
	// it. The gated-tool family has always written the principal here (SetToolApprovalDecision), so the
	// two families disagreed about what their shared column meant.
	//
	// Nothing read it as a command id — the command's own identity is on the journalled event below and
	// on the commands table — so this narrows the value to the one the name promises.
	if _, err := tx.Exec(ctx, storage.Query("SetApprovalDecision"), pubID, approver); err != nil {
		return 0, fmt.Errorf("record approval decision: %w", err)
	}
	if _, err := appendEvent(ctx, tx, tenant, sessionID, responseID, event,
		mustMarshal(map[string]any{"publication_id": pubID, "command_id": commandID})); err != nil {
		return 0, err
	}
	seq, err := applyCommandInTx(ctx, tx, tenant, sessionID, responseID, commandID)
	if err != nil {
		return 0, err
	}
	// THE WAKE (E23 T3), and it belongs HERE rather than in Slack's Decide for the reason the approver check
	// is here: this is the ONE function both surfaces pass through, so a wake written in each caller is a
	// wake the next caller forgets. It is a no-op for a run that is not parked — which is exactly what the
	// boundary pump's own application of this command produces, since a run at a boundary is running.
	//
	// It is also what turns E22's measured 503 into a 200: the run no longer races the human to a terminal
	// state, so the decision lands on an ACTIVE run and re-enters it in the same transaction that approved.
	if err := wakeParkedRunTx(ctx, tx, tenant, runID); err != nil {
		return 0, err
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, fmt.Errorf("commit apply approval: %w", err)
	}
	return seq, nil
}

// approverAuthorizedTx is THE approver check (E23 T2), and it is a function rather than two copies of two
// lines for a reason the tree ENFORCES: TestApproverAllowedHasExactlyOneProductionCallSite walks every
// non-test .go file in the module and requires ConfigPolicy.ApproverAllowed to be called from exactly one
// place. That guard is not decoration — "put the check in each caller" is how the next caller forgets it.
//
// E23 T8 IS THE SECOND CALLER THE GUARD WAS WRITTEN FOR, and it arrived as the guard predicted: wiring the
// generic decision surface meant DecideToolApproval had to refuse an unlisted principal too, and the
// obvious way to write that would have been a second `policy.ApproverAllowed(...)` in approvals.go —
// turning one throat into two and reddening the guard. Extracting the body instead keeps the call site
// singular AND the property real: both kinds of approval are refused by the same six lines, so a project's
// approver list cannot come to mean one thing for a publication and another for a tool call.
func approverAuthorizedTx(ctx context.Context, tx pgx.Tx, tenant Tenant, approver string) (bool, error) {
	policy, err := projectConfigTx(ctx, tx, tenant)
	if err != nil {
		return false, err
	}
	return policy.ApproverAllowed(approver), nil
}

// settleApprovalCommandTx settles an approve/deny that authorizes NOTHING — no pending approval left, a
// stale one-shot hash, an elapsed approval — and commits. Settling matters as much as not approving: a
// command that stays queued is re-read by the boundary pump at every subsequent boundary for the life of
// the run. Two of these three exits shipped as a bare `return applyCommandInTx(...)`, which the deferred
// rollback undid, so the settle the comments promised never reached the database.
func settleApprovalCommandTx(ctx context.Context, tx pgx.Tx, tenant Tenant, sessionID, responseID, commandID string) (int64, error) {
	seq, err := applyCommandInTx(ctx, tx, tenant, sessionID, responseID, commandID)
	if err != nil {
		return 0, err
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, fmt.Errorf("commit settled approval command: %w", err)
	}
	return seq, nil
}

// expirePublicationTx drives a publication fromState -> expired single-winner and journals
// approval.expired.v1 (spec §22.4, E10 T7). It is the shared enforcement body: the consume-time guards
// (ApplyApprovalDecision, ExpireApprovalIfElapsed) and the reconcile sweep all expire through it, so an
// expired approval is journaled identically no matter which path observes the elapsed deadline. The
// UPDATE is conditional on fromState, so a racing publish/deny that already moved the row wins and this
// is a no-op that journals nothing. It returns whether it actually expired a row, so a sweep counts only
// the ones it moved (not a no-op a concurrent consume already handled).
func expirePublicationTx(ctx context.Context, tx pgx.Tx, tenant Tenant, sessionID, responseID, pubID, fromState string) (bool, error) {
	tag, err := tx.Exec(ctx, storage.Query("SetPublicationState"),
		pubID, tenant.Project, "expired", fromState)
	if err != nil {
		return false, fmt.Errorf("expire publication: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return false, nil // a racing transition already moved it; nothing to journal
	}
	if _, err := appendEvent(ctx, tx, tenant, sessionID, responseID, approvalExpiredEvent,
		mustMarshal(map[string]any{"publication_id": pubID})); err != nil {
		return false, err
	}
	return true, nil
}

// ExpireApprovalIfElapsed is the approval pump's consume-time expiry guard (spec §22.4, §30.9-30.10,
// E10 T7): before publishing an APPROVED publication, the pump checks whether its one-shot approval
// elapsed between approval and publish. If so it expires the row (approved -> expired) + journals
// approval.expired.v1 and reports expired=true, so the pump SKIPS the publish — an expired approval
// never pushes. A row with no expiry, or one still within it, reports false and is published unchanged
// (bit-identical). The lock serializes it against a concurrent publish so the transition is
// single-winner.
func (s *Store) ExpireApprovalIfElapsed(ctx context.Context, tenant Tenant, sessionID, responseID, publicationID string) (bool, error) {
	ctx = storage.ScopeToTenant(ctx, tenant.Project)
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return false, fmt.Errorf("begin expire-if-elapsed: %w", err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()

	var state string
	var expiresAt *time.Time
	switch err := tx.QueryRow(ctx, storage.Query("LockPublicationApprovalExpiry"), publicationID, tenant.Project).
		Scan(&state, &expiresAt); {
	case errors.Is(err, pgx.ErrNoRows):
		return false, nil
	case err != nil:
		return false, fmt.Errorf("lock publication expiry: %w", err)
	}
	if state != "approved" || expiresAt == nil || !expiresAt.Before(time.Now()) {
		return false, nil
	}
	if _, err := expirePublicationTx(ctx, tx, tenant, sessionID, responseID, publicationID, "approved"); err != nil {
		return false, err
	}
	if err := tx.Commit(ctx); err != nil {
		return false, fmt.Errorf("commit expire-if-elapsed: %w", err)
	}
	return true, nil
}

// SweepExpiredApprovals expires every still-open publication (pending_approval or approved) whose
// one-shot approval passed its minutes-scale expiry, in one transaction, journaling approval.expired.v1
// per row (spec §22.4, E10 T7). It is the reconcile-loop half of expiry enforcement: the consume-time
// guards catch an expiry observed at approve/publish, this catches one that elapsed while idle (no
// consume). Not a timer-scheduled sweep (that is E11) — one supervised reconcile pass, the retention/GC
// pattern. Returns the number expired this pass.
func (s *Store) SweepExpiredApprovals(ctx context.Context) (int, error) {
	ctx = storage.WithSystemScope(ctx) // reconcile sweep: spans every tenant by construction
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return 0, fmt.Errorf("begin sweep expired approvals: %w", err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()

	rows, err := tx.Query(ctx, storage.Query("SelectExpiredApprovals"))
	if err != nil {
		return 0, fmt.Errorf("select expired approvals: %w", err)
	}
	type expired struct {
		pubID, sessionID, responseID, runID, fromState string
		tenant                                         Tenant
	}
	var candidates []expired
	for rows.Next() {
		var e expired
		if err := rows.Scan(&e.pubID, &e.tenant.Project, &e.sessionID, &e.responseID,
			&e.runID, &e.fromState); err != nil {
			rows.Close()
			return 0, fmt.Errorf("scan expired approval: %w", err)
		}
		candidates = append(candidates, e)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, err
	}

	swept := 0
	for _, e := range candidates {
		// The row's own read state (pending_approval or approved) is the fromState the shared
		// single-winner expiry guards on. Count only rows this sweep actually moved — a row a concurrent
		// consume already expired is a no-op (RowsAffected 0), not a swept one.
		expired, err := expirePublicationTx(ctx, tx, e.tenant, e.sessionID, e.responseID, e.pubID, e.fromState)
		if err != nil {
			return 0, err
		}
		if expired {
			// E23 T3 — THE HALF THAT DID NOT EXIST. Expiring the publication was always here; releasing what
			// was waiting on it was not, because until this epic nothing waited. Now the run PARKS on its own
			// pending approval, so an unanswered question is a stopped run, and a deadline that cancels the
			// question without re-entering the run would be a gate that keeps shut forever what it closed.
			// A run that is not parked is a no-op, which is every `approved`-row expiry (the run was woken by
			// the approve long before this sweep saw the deadline).
			if err := wakeParkedRunTx(ctx, tx, e.tenant, e.runID); err != nil {
				return 0, err
			}
			swept++
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, fmt.Errorf("commit sweep expired approvals: %w", err)
	}
	return swept, nil
}

// ApprovedPublicationsForRun returns a run's approved-but-unpublished publications — the approval pump's
// drain (spec §30.9-30.10). The rows are DURABLE: an approve survives run termination, so a row lingers
// here for E10's detached execution if the run ended before the pump published it (the honest E09
// ceiling — E09 publishes at a live-run boundary).
func (s *Store) ApprovedPublicationsForRun(ctx context.Context, tenant Tenant, runID string) ([]Publication, error) {
	ctx = storage.ScopeToTenant(ctx, tenant.Project)
	rows, err := s.pool.Query(ctx, storage.Query("ApprovedPublicationsForRun"), runID, tenant.Project)
	if err != nil {
		return nil, fmt.Errorf("read approved publications: %w", err)
	}
	defer rows.Close()
	var out []Publication
	for rows.Next() {
		pub, err := scanPublication(rows)
		if err != nil {
			return nil, fmt.Errorf("scan approved publication: %w", err)
		}
		out = append(out, pub)
	}
	return out, rows.Err()
}

// MarkPublicationPublished records the external receipt and drives approved -> published single-winner
// (spec §30.9-30.10), journaling push.completed.v1 or pull_request.opened.v1 by operation. It is
// idempotent: a re-driven publish whose row is already published updates 0 rows and re-journals nothing,
// so a lost-ack retry that re-reconciled the remote settles cleanly. sessionID/responseID scope the
// event.
func (s *Store) MarkPublicationPublished(ctx context.Context, tenant Tenant, sessionID, responseID, publicationID, operation string, receipt map[string]any) error {
	ctx = storage.ScopeToTenant(ctx, tenant.Project)
	receiptJSON, err := json.Marshal(receipt)
	if err != nil {
		return fmt.Errorf("marshal publication receipt: %w", err)
	}
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return fmt.Errorf("begin mark published: %w", err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()

	tag, err := tx.Exec(ctx, storage.Query("MarkPublicationPublished"),
		publicationID, tenant.Project, receiptJSON)
	if err != nil {
		return fmt.Errorf("mark publication published: %w", err)
	}
	// The event rides the FIRST publish only; a re-drive updates 0 rows and journals nothing.
	if tag.RowsAffected() > 0 {
		event := pushCompletedEvent
		switch operation {
		case "open_pull_request":
			event = pullRequestOpenedEvent
		case "merge_pull_request":
			event = pullRequestMergedEvent
		}
		payload := mustMarshal(map[string]any{"publication_id": publicationID, "receipt": receipt})
		if _, err := appendEvent(ctx, tx, tenant, sessionID, responseID, event, payload); err != nil {
			return err
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit mark published: %w", err)
	}
	return nil
}

// RecordPublicationWarning journals a warning.raised.v1 for a publication the approval pump could not
// publish (spec §30.12, REP-010): a diverged remote or a policy denial the model/user must SEE, rather
// than an invisible server-log retry. The publication row stays approved (retry-safe), so the warning
// surfaces the choice (rebase/merge/wait) without silently dropping the operation. detail carries the
// error text — a brokered credential never reaches git output, so it carries no secret.
func (s *Store) RecordPublicationWarning(ctx context.Context, tenant Tenant, sessionID, responseID, publicationID, detail string) error {
	ctx = storage.ScopeToTenant(ctx, tenant.Project)
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return fmt.Errorf("begin publication warning: %w", err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	payload := mustMarshal(map[string]any{"publication_id": publicationID, "detail": detail, "kind": "publication_failed"})
	if _, err := appendEvent(ctx, tx, tenant, sessionID, responseID, warningRaisedEvent, payload); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit publication warning: %w", err)
	}
	return nil
}

// publicationByKey reads a publication projection by idempotency key within tx.
func publicationByKey(ctx context.Context, tx pgx.Tx, tenant Tenant, idempotencyKey string) (Publication, error) {
	row := tx.QueryRow(ctx, storage.Query("GetPublicationByKey"), tenant.Project, idempotencyKey)
	pub, err := scanPublication(row)
	if err != nil {
		return Publication{}, fmt.Errorf("read publication: %w", err)
	}
	return pub, nil
}

// scanRow is the pgx row surface scanPublication reads from (a QueryRow or a Query row both satisfy it).
type scanRow interface {
	Scan(dest ...any) error
}

// scanPublication scans the shared publication projection column list.
func scanPublication(row scanRow) (Publication, error) {
	var (
		pub           Publication
		receipt, args string
	)
	if err := row.Scan(&pub.ID, &pub.SessionID, &pub.RunID, &pub.ResponseID, &pub.Operation, &pub.Remote,
		&pub.Branch, &pub.Base, &pub.HeadSHA, &pub.IdempotencyKey, &pub.Display, &pub.State, &receipt,
		&pub.RequestHash, &args); err != nil {
		return Publication{}, err
	}
	if receipt != "" {
		pub.Receipt = []byte(receipt)
	}
	if args != "" {
		_ = json.Unmarshal([]byte(args), &pub.Args)
	}
	return pub, nil
}
