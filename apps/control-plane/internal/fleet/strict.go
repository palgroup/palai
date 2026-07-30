package fleet

// STRICT ENROLMENT — THE WAITING ROOM, AND WHY ITS APPROVAL IS NOT E23'S THROAT (E24 T6).
//
// A pool with `strict_enrollment` records an enrolling machine as `pending`: it gets its certificate and
// it never enters the rendezvous, so a `Dial` cannot reach it however the placement rules are later
// changed (RunnerGateway.awaitApproval). This file is the other end — the human's answer.
//
// WHY A SEPARATE APPROVAL PATH IS CORRECT HERE, written down because the next person will otherwise
// either wire it into the wrong throat or quietly duplicate the rule. E23's rule is that a decision about
// a gated operation goes through ONE function, `coordinator.DecideToolApproval`, and that function's
// subject is a TOOL CALL or a PUBLICATION: it is keyed by `tool_calls.id`, it applies the one-shot
// `request_hash` binding so that arguments changed after an approval leave no approval, it transitions the
// call, and it wakes the run that parked on it. A MACHINE ENROLMENT HAS NONE OF THOSE. There is no tool
// call, no arguments, no parked run, and — the load-bearing one — NO REQUEST HASH TO BIND TO: the hash
// exists so that what a human saw is what runs, and an enrolment's subject is a certificate that was
// already issued. Routing it through that throat would mean inventing a synthetic tool call and a
// synthetic hash for every Mac that boots, and the binding would then bind nothing.
//
// So this is a second PATH and not a second POLICY, and that distinction is what keeps E23's rule intact:
// WHO MAY DECIDE is still `config_policy.approvers` evaluated by the ONE function that evaluates it,
// coordinator.ConfigPolicy.ApproverAllowed. If that rule ever changes — an empty list stops meaning
// "everybody", a principal form changes — it changes for tool calls, publications and machines together,
// because all three ask the same function. Nothing about the approver policy is reimplemented below.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/palgroup/palai/packages/coordinator"
	"github.com/palgroup/palai/storage"
)

// Admission is what one approval did to one machine.
//
// Found and Admitted are DIFFERENT answers and both are needed. Found=false is an id that is not this
// tenant's, is not there, or is a machine an approval must not move (cordoned, revoked) — rendered as one
// non-disclosing 404. Admitted=false with Found=true is the repeat: the row was already `active`, so this
// call moved nothing and recorded nothing, and the caller still gets a 200 because an operator retrying a
// request that timed out must not read "the id was wrong".
type Admission struct {
	Runner   Runner
	Found    bool
	Admitted bool
}

// Approve admits ONE machine out of a strict pool's waiting room, in one transaction: the approver policy
// is applied, the row moves `pending` -> `active`, and the decision is journalled.
//
// principal is the CANONICAL approver principal (coordinator.ApproverPrincipal) and is derived server-side
// from the verified credential by the caller — never taken from a request body, which is the rule every
// other decide surface in this tree follows. An empty principal is refused against any configured list.
//
// THE LIST IS THE RUNNER'S OWN PROJECT'S, resolved from the row rather than from the caller's scope, and
// that is not a detail: an org-scoped key carries no project, so a policy read keyed on the caller's scope
// would find no project row, decode an empty policy, and an empty list means EVERYTHING is permitted — the
// approver list would have been bypassed by holding a wider key. The row names the project the machine
// belongs to, which is the project whose policy governs it.
//
// THE MACHINE IS RESOLVED BEFORE THE PRINCIPAL IS CHECKED, so an unauthorized caller learns 403 for a
// machine that exists and 404 for one that does not. That is deliberate rather than overlooked: the same
// key can already page `GET /v1/runners`, which needs no capability beyond a verified scope, so there is
// no fact here it could not read anyway — and the alternative (checking first) cannot resolve the project
// whose list to check.
func (s *Store) Approve(ctx context.Context, org, project, id, principal string) (Admission, error) {
	ctx = storage.WithTenant(ctx, org, project)
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return Admission{}, fmt.Errorf("begin runner approval: %w", err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()

	current, err := scanRunner(tx.QueryRow(ctx, storage.Query("GetRunner"), id, org, project), true)
	if errors.Is(err, pgx.ErrNoRows) {
		return Admission{}, nil
	}
	if err != nil {
		return Admission{}, fmt.Errorf("read the runner to approve: %w", err)
	}
	allowed, err := approverAllowed(ctx, tx, current.Organization, current.Project, principal)
	if err != nil {
		return Admission{}, err
	}
	if !allowed {
		return Admission{}, coordinator.ErrApproverNotAuthorized
	}

	row, err := scanRunner(tx.QueryRow(ctx, storage.Query("ApproveRunner"), id, org, project), true)
	if errors.Is(err, pgx.ErrNoRows) {
		// The machine is cordoned or revoked: `state IN ('pending','active')` is the statement's whole
		// admission set, so an approval can neither resurrect a decommissioned identity nor un-cordon a Mac.
		return Admission{}, nil
	}
	if err != nil {
		return Admission{}, fmt.Errorf("approve runner: %w", err)
	}
	admitted := current.State == "pending"
	if admitted {
		// WHO admitted it, which is the entry's reason for existing: `state` already says the machine is
		// active, and the question an audit asks afterwards is who said so. The principal is `key:<api_key_id>`
		// or `slack:<team>:<user>` — an identifier, never a credential, and there is no field here one could
		// be put in.
		detail, err := json.Marshal(map[string]string{
			"runner_dns": row.DNS, "label": row.Label, "approved_by": principal,
		})
		if err != nil {
			return Admission{}, fmt.Errorf("encode admission detail: %w", err)
		}
		if _, err := tx.Exec(ctx, storage.Query("AppendRunnerDecision"),
			s.mintID("renr"), row.Organization, row.Project, row.ID, row.PoolID, "approved", detail); err != nil {
			return Admission{}, fmt.Errorf("append admission entry: %w", err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return Admission{}, fmt.Errorf("commit runner approval: %w", err)
	}
	return Admission{Runner: row, Found: true, Admitted: admitted}, nil
}

// approverAllowed applies the PROJECT's approver list to one principal, inside the caller's transaction.
//
// The decode is `coordinator.ConfigPolicy` and the decision is its own ApproverAllowed — the single
// definition of the rule E23 T2 shipped, including the asymmetry that carries every existing deployment:
// NO list means everything is permitted, bit-for-bit as before the check existed, and a list means deny by
// default with an unidentified principal deciding nothing. A project with a NULL config_policy (every
// project alive today) decodes to the zero value, which is the no-list case.
func approverAllowed(ctx context.Context, tx pgx.Tx, org, project, principal string) (bool, error) {
	var raw []byte
	err := tx.QueryRow(ctx, storage.Query("GetProjectConfig"), org, project).Scan(&raw)
	if errors.Is(err, pgx.ErrNoRows) {
		return coordinator.ConfigPolicy{}.ApproverAllowed(principal), nil
	}
	if err != nil {
		return false, fmt.Errorf("read the project approver list: %w", err)
	}
	var policy coordinator.ConfigPolicy
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &policy); err != nil {
			// A policy document that will not decode must not read as "no list configured": that would turn one
			// malformed write into an open door. It is the one case here that refuses rather than defaults.
			return false, fmt.Errorf("decode the project approver list: %w", err)
		}
	}
	return policy.ApproverAllowed(principal), nil
}
