package coordinator

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/palgroup/palai/storage"
)

// THE SESSION'S STANDING AUTHORIZATION (E30 T1, migration 000056, spec §22.4).
//
// The owner's ask was "auto-approve yapabilmek istiyorum session'u" — let me auto-approve the session —
// and this file is the durable half of it. What it is NOT is a bypass: an armed session still creates
// every approval row, still passes the project's approver policy, and still journals a decision with a
// name on it. What changes is WHO answers and WHEN — a human answered in advance, for this sitting,
// instead of being interrupted per call.
//
// WHY THE TWO FAMILIES DO NOT SHARE A FLAG. This is the whole design and it is worth stating where the
// type is declared rather than in a plan nobody reads at the call site:
//
//   - A gated TOOL call is `xcodebuild`, `simctl`, a shell command. Its effects land in the run's own
//     workspace while a human watches, and the operator armed it precisely because a build asks the
//     same question forty times.
//   - A PUBLICATION is a push, a pull request, a merge. Its effects land in somebody else's repository,
//     they OUTLIVE the session, and no amount of watching the chat undoes them.
//
// Collapsing them would mean the click that stopped `xcodebuild -list` from asking also stopped every
// push from asking. Nobody who wants the first wants the second by implication, so they are two
// columns, two fields, and two separate sentences on the screen.
//
// AND ONE MEASURED REASON THE PUBLICATION HALF IS DIFFERENT IN KIND, not merely in degree. A publish can
// fail after the decision — a diverged remote, a revoked tenant credential, a 405 from branch protection —
// and each of those lands as a warning on the publication row that somebody has to read. With a human in
// the loop somebody is at least watching for the receipt. Armed, there is nobody left to notice, so this
// half is the one that defaults off and stays off unless it is asked for by name.
//
// The sharpest instance of that used to be a MISSING GitHub App: `repositoryPublisherFromEnv` returned nil,
// the pump was a silent no-op, and the row sat at `approved` forever. That one is closed — the publisher is
// built independently of the App and a publication with no credential path is refused at the tool, before
// any decision — which narrows the hazard without removing it.

// AutoApprove is a session's standing authorization, as the two gates read it.
//
// The zero value — both halves off, no principal — is what every session created before migration
// 000056 reads, and what every session created by a client that has never heard of these fields reads.
// Both gates behave bit-for-bit as they did on that value, which is the property that makes this
// feature additive rather than a change to everyone's deployment.
type AutoApprove struct {
	// Tools arms the gated-tool family: a tool whose revision declares RequiresApproval is decided by
	// the standing authorization instead of parking the run for a human.
	Tools bool
	// Publications arms the publication family: a push/PR/merge is decided by the standing
	// authorization instead of parking the run for a human.
	Publications bool
	// SetBy is THE PRINCIPAL THE AUTO-DECISION IS MADE AS, and it is the reason this struct is not two
	// booleans. An armed session does not approve as a machine or as "the system": it approves as the
	// human who armed it, who is standing behind these calls in advance. Two things follow, and both
	// are the point:
	//
	//   1. The approvals screen and the journal say "decided by <them>", which is TRUE. Nothing is
	//      skipped and nothing is anonymous.
	//   2. The decision still passes ApproverAllowed(SetBy). A project whose config_policy names an
	//      `approvers` list auto-approves NOTHING for a principal that list does not name — so arming a
	//      session can never grant more than clicking would have, and cannot be used to escalate.
	//
	// Empty means unidentified, and ApproverAllowed already refuses an empty principal wherever a list
	// exists. On a project with no list it is permitted, which is exactly the authority a click from
	// that same surface would have carried.
	SetBy string
}

// Armed reports whether either family is armed — the cheap read a caller uses to decide whether the
// principal is worth resolving at all.
func (a AutoApprove) Armed() bool { return a.Tools || a.Publications }

// AutoApproveView is the projection an operator's screen renders: the state plus WHEN it was armed.
// SetAt is nil when nothing is armed, so "armed at" never outlives the arming.
type AutoApproveView struct {
	AutoApprove
	SetAt *time.Time
	Found bool
}

// SessionAutoApprove reads a session's standing authorization within the tenant scope. An unknown or
// foreign session reads the zero value — both halves off — which is the fail-closed answer: a gate that
// cannot find the session it is asked about asks a human.
func (s *Store) SessionAutoApprove(ctx context.Context, tenant Tenant, sessionID string) (AutoApprove, error) {
	ctx = storage.ScopeToTenant(ctx, tenant.Organization, tenant.Project)
	var a AutoApprove
	err := s.pool.QueryRow(ctx, storage.Query("SessionAutoApprove"), sessionID, tenant.Organization, tenant.Project).
		Scan(&a.Tools, &a.Publications, &a.SetBy)
	if errors.Is(err, pgx.ErrNoRows) {
		return AutoApprove{}, nil
	}
	if err != nil {
		return AutoApprove{}, fmt.Errorf("read session auto-approve for %s: %w", sessionID, err)
	}
	return a, nil
}

// autoApproveTx is SessionAutoApprove inside an open transaction, for a gate that must read the
// standing authorization in the SAME transaction it acts on — so a disarm racing a dispatch cannot be
// read as armed after it committed.
func autoApproveTx(ctx context.Context, tx pgx.Tx, tenant Tenant, sessionID string) (AutoApprove, error) {
	var a AutoApprove
	err := tx.QueryRow(ctx, storage.Query("SessionAutoApprove"), sessionID, tenant.Organization, tenant.Project).
		Scan(&a.Tools, &a.Publications, &a.SetBy)
	if errors.Is(err, pgx.ErrNoRows) {
		return AutoApprove{}, nil
	}
	if err != nil {
		return AutoApprove{}, fmt.Errorf("read session auto-approve for %s: %w", sessionID, err)
	}
	return a, nil
}

// SetSessionAutoApprove arms or disarms a session's two approval families as `principal`.
//
// THE PRINCIPAL IS THE CALLER'S VERIFIED IDENTITY AND NEVER A BODY FIELD — the same rule
// api.ApprovalDecision states for a click ("the principal is stamped server-side from the verified
// key"). A request that could name its own approver would let anyone arm a session as anyone.
//
// Found is false for an unknown or foreign session, so the caller renders a 404 that discloses no
// cross-tenant existence.
func (s *Store) SetSessionAutoApprove(ctx context.Context, tenant Tenant, sessionID, principal string, tools, publications bool) (AutoApproveView, error) {
	ctx = storage.ScopeToTenant(ctx, tenant.Organization, tenant.Project)
	var v AutoApproveView
	err := s.pool.QueryRow(ctx, storage.Query("SetSessionAutoApprove"),
		sessionID, tenant.Organization, tenant.Project, tools, publications, principal).
		Scan(&v.Tools, &v.Publications, &v.SetBy, &v.SetAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return AutoApproveView{}, nil
	}
	if err != nil {
		return AutoApproveView{}, fmt.Errorf("set session auto-approve for %s: %w", sessionID, err)
	}
	v.Found = true
	return v, nil
}
