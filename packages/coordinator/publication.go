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
	warningRaisedEvent     = "warning.raised.v1"
)

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
}

// RunPublicationTarget resolves a run's publication destination — the remote/branch/base a push or PR
// targets — from the run's latest preparation receipt joined to its binding (spec §30.9). It is
// infrastructure-owned: the model never supplies a remote, so an agent cannot redirect a publication.
func (s *Store) RunPublicationTarget(ctx context.Context, tenant Tenant, runID string) (PublicationTarget, bool, error) {
	var t PublicationTarget
	err := s.pool.QueryRow(ctx, storage.Query("RunPublicationTarget"), runID, tenant.Organization, tenant.Project).
		Scan(&t.Remote, &t.Branch, &t.Base)
	if errors.Is(err, pgx.ErrNoRows) {
		return PublicationTarget{}, false, nil
	}
	if err != nil {
		return PublicationTarget{}, false, fmt.Errorf("resolve run publication target: %w", err)
	}
	return t, true, nil
}

// RequestPublication records a pending publication + its one-shot approval binding idempotently (spec
// §30.8, §22.4). A duplicate idempotency_key returns the ORIGINAL publication (Replayed) — the tool
// re-proposing the same operation resolves to the existing pending approval, never a second. It journals
// approval.requested.v1 on the first insert only, so a replay does not re-journal.
func (s *Store) RequestPublication(ctx context.Context, tenant Tenant, in PublicationRequest) (Publication, error) {
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
		in.PublicationID, tenant.Organization, tenant.Project, in.SessionID, in.RunID, nullableText(in.ResponseID),
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

	if _, err := tx.Exec(ctx, storage.Query("InsertApproval"),
		in.ApprovalID, in.PublicationID, tenant.Organization, tenant.Project, in.RequestHash, in.AllowedApprover, nil); err != nil {
		return Publication{}, fmt.Errorf("insert approval: %w", err)
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
	row := s.pool.QueryRow(ctx, storage.Query("PendingApprovalForSession"), sessionID, tenant.Organization, tenant.Project)
	pub, err := scanPublication(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return Publication{}, false, nil
	}
	if err != nil {
		return Publication{}, false, fmt.Errorf("read pending approval: %w", err)
	}
	return pub, true, nil
}

// ApplyApprovalDecision applies a queued approve/deny command at a safe boundary (spec §22.4-22.5,
// APV-001). In one transaction it transitions the session's pending publication (approve ->
// approved, deny -> denied), records who decided, journals approval.approved/denied.v1, and marks the
// command applied — so the approved (durable) publication and the applied command commit together. It
// runs under guardRunActive (the pump's fence discipline). requestHash is the one-shot binding from the
// approve command: a mismatch (a stale approve for a head that moved, or an edited request) authorizes
// nothing but still settles the command. A missing pending approval is a no-op that settles the command.
func (s *Store) ApplyApprovalDecision(ctx context.Context, tenant Tenant, sessionID, responseID, runID, commandID, kind, requestHash string) (int64, error) {
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
	switch err := tx.QueryRow(ctx, storage.Query("LockPendingApprovalForSession"), sessionID, tenant.Organization, tenant.Project).
		Scan(&pubID, &pendingHash, &expiresAt); {
	case errors.Is(err, pgx.ErrNoRows):
		// No pending approval (already resolved by a racing path): settle the command, transition nothing.
		return applyCommandInTx(ctx, tx, tenant, sessionID, responseID, commandID)
	case err != nil:
		return 0, fmt.Errorf("lock pending approval: %w", err)
	}

	// Consume-time expiry guard (spec §22.4, E10 T7): an approve/deny that arrives after the one-shot
	// approval passed its minutes-scale expiry authorizes NOTHING. Expire the publication (pending ->
	// expired) + journal approval.expired.v1, then settle the command — the same "authorizes nothing but
	// settles the command" shape the stale-hash branch uses. Checked before the hash so an expired
	// approval never approves regardless of the token.
	if expiresAt != nil && expiresAt.Before(time.Now()) {
		if err := expirePublicationTx(ctx, tx, tenant, sessionID, responseID, pubID, "pending_approval"); err != nil {
			return 0, err
		}
		seq, err := applyCommandInTx(ctx, tx, tenant, sessionID, responseID, commandID)
		if err != nil {
			return 0, err
		}
		if err := tx.Commit(ctx); err != nil {
			return 0, fmt.Errorf("commit expired approval: %w", err)
		}
		return seq, nil
	}

	// A stale one-shot token (the head moved -> a new pending approval carries a new hash, or the args
	// were edited) authorizes nothing: settle the command without transitioning the publication.
	if requestHash != "" && requestHash != pendingHash {
		return applyCommandInTx(ctx, tx, tenant, sessionID, responseID, commandID)
	}

	newState, event := "approved", approvalApprovedEvent
	if kind == "deny" {
		newState, event = "denied", approvalDeniedEvent
	}
	if _, err := tx.Exec(ctx, storage.Query("SetPublicationState"),
		pubID, tenant.Organization, tenant.Project, newState, "pending_approval"); err != nil {
		return 0, fmt.Errorf("transition publication: %w", err)
	}
	if _, err := tx.Exec(ctx, storage.Query("SetApprovalDecision"), pubID, commandID); err != nil {
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
	if err := tx.Commit(ctx); err != nil {
		return 0, fmt.Errorf("commit apply approval: %w", err)
	}
	return seq, nil
}

// expirePublicationTx drives a publication fromState -> expired single-winner and journals
// approval.expired.v1 (spec §22.4, E10 T7). It is the shared enforcement body: the consume-time guards
// (ApplyApprovalDecision, ExpireApprovalIfElapsed) and the reconcile sweep all expire through it, so an
// expired approval is journaled identically no matter which path observes the elapsed deadline. The
// UPDATE is conditional on fromState, so a racing publish/deny that already moved the row wins and this
// is a no-op that journals nothing.
func expirePublicationTx(ctx context.Context, tx pgx.Tx, tenant Tenant, sessionID, responseID, pubID, fromState string) error {
	tag, err := tx.Exec(ctx, storage.Query("SetPublicationState"),
		pubID, tenant.Organization, tenant.Project, "expired", fromState)
	if err != nil {
		return fmt.Errorf("expire publication: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return nil // a racing transition already moved it; nothing to journal
	}
	if _, err := appendEvent(ctx, tx, tenant, sessionID, responseID, approvalExpiredEvent,
		mustMarshal(map[string]any{"publication_id": pubID})); err != nil {
		return err
	}
	return nil
}

// ExpireApprovalIfElapsed is the approval pump's consume-time expiry guard (spec §22.4, §30.9-30.10,
// E10 T7): before publishing an APPROVED publication, the pump checks whether its one-shot approval
// elapsed between approval and publish. If so it expires the row (approved -> expired) + journals
// approval.expired.v1 and reports expired=true, so the pump SKIPS the publish — an expired approval
// never pushes. A row with no expiry, or one still within it, reports false and is published unchanged
// (bit-identical). The lock serializes it against a concurrent publish so the transition is
// single-winner.
func (s *Store) ExpireApprovalIfElapsed(ctx context.Context, tenant Tenant, sessionID, responseID, publicationID string) (bool, error) {
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return false, fmt.Errorf("begin expire-if-elapsed: %w", err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()

	var state string
	var expiresAt *time.Time
	switch err := tx.QueryRow(ctx, storage.Query("LockPublicationApprovalExpiry"), publicationID, tenant.Organization, tenant.Project).
		Scan(&state, &expiresAt); {
	case errors.Is(err, pgx.ErrNoRows):
		return false, nil
	case err != nil:
		return false, fmt.Errorf("lock publication expiry: %w", err)
	}
	if state != "approved" || expiresAt == nil || !expiresAt.Before(time.Now()) {
		return false, nil
	}
	if err := expirePublicationTx(ctx, tx, tenant, sessionID, responseID, publicationID, "approved"); err != nil {
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
		pubID, sessionID, responseID, fromState string
		tenant                                  Tenant
	}
	var candidates []expired
	for rows.Next() {
		var e expired
		if err := rows.Scan(&e.pubID, &e.tenant.Organization, &e.tenant.Project, &e.sessionID, &e.responseID, &e.fromState); err != nil {
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
		// single-winner expiry guards on. A row a concurrent consume already moved is a no-op that
		// journals nothing (RowsAffected 0 inside expirePublicationTx).
		if err := expirePublicationTx(ctx, tx, e.tenant, e.sessionID, e.responseID, e.pubID, e.fromState); err != nil {
			return 0, err
		}
		swept++
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
	rows, err := s.pool.Query(ctx, storage.Query("ApprovedPublicationsForRun"), runID, tenant.Organization, tenant.Project)
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
		publicationID, tenant.Organization, tenant.Project, receiptJSON)
	if err != nil {
		return fmt.Errorf("mark publication published: %w", err)
	}
	// The event rides the FIRST publish only; a re-drive updates 0 rows and journals nothing.
	if tag.RowsAffected() > 0 {
		event := pushCompletedEvent
		if operation == "open_pull_request" {
			event = pullRequestOpenedEvent
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
	row := tx.QueryRow(ctx, storage.Query("GetPublicationByKey"), tenant.Organization, tenant.Project, idempotencyKey)
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
		pub     Publication
		receipt string
	)
	if err := row.Scan(&pub.ID, &pub.SessionID, &pub.RunID, &pub.ResponseID, &pub.Operation, &pub.Remote,
		&pub.Branch, &pub.Base, &pub.HeadSHA, &pub.IdempotencyKey, &pub.Display, &pub.State, &receipt, &pub.RequestHash); err != nil {
		return Publication{}, err
	}
	if receipt != "" {
		pub.Receipt = []byte(receipt)
	}
	return pub, nil
}
