//go:build component

package execution

import (
	"context"
	"testing"

	"github.com/palgroup/palai/apps/control-plane/internal/extensions"
	"github.com/palgroup/palai/packages/contracts"
	"github.com/palgroup/palai/packages/coordinator"
	toolbroker "github.com/palgroup/palai/packages/tool-broker"
)

// denyingFirer is a fake HookFirer that DENIES at one point and passes every other through — enough to drive
// the before_model / before_repository_publish seams against a real spine without the full registry.
type denyingFirer struct {
	point  string
	hookID string
	reason string
	fired  int
}

func (f *denyingFirer) Fire(_ context.Context, ev extensions.HookEvent) (extensions.HookOutcome, error) {
	if ev.Point == f.point {
		f.fired++
		return extensions.HookOutcome{Denied: true, Reason: f.reason, HookID: f.hookID, Payload: ev.Payload}, nil
	}
	return extensions.HookOutcome{Payload: ev.Payload}, nil
}

// assertEventJournaled asserts the session's journal carries exactly `want` events of the given type (the
// reused policy.denied.v1 kind for a hook deny).
func assertEventJournaled(t *testing.T, cs *coordinator.Store, sessionID, eventType string, want int) {
	t.Helper()
	var got int
	if err := cs.Pool().QueryRow(context.Background(),
		`SELECT count(*) FROM events WHERE session_id = $1 AND type = $2`, sessionID, eventType).Scan(&got); err != nil {
		t.Fatalf("count %s events: %v", eventType, err)
	}
	if got != want {
		t.Fatalf("journaled %d %s events, want %d", got, eventType, want)
	}
}

// TestBeforeModelPolicyDenyFailsStep proves a before_model POLICY deny fails the model step VISIBLY (spec
// §28.17): dispatchModel journals policy.denied.v1 and returns an error BEFORE the provider is ever routed —
// the model call never happens. It drives dispatchModel against a real spine with a nil model broker, so a
// reached Route would panic (proving the deny short-circuits before it).
func TestBeforeModelPolicyDenyFailsStep(t *testing.T) {
	ctx := context.Background()
	cs, tenant, sessionID, runID := openLedgerSpine(t)
	firer := &denyingFirer{point: extensions.HookPointBeforeModel, hookID: "hook_m", reason: "model calls are frozen for this project"}

	orch := &Orchestrator{spine: cs, hooks: firer, route: defaultModelRoute}
	st := &attemptState{
		attempt:   AttemptDescriptor{RunID: contracts.RunID(runID), AttemptID: contracts.AttemptID(redeliveryID("att")), Fence: 1},
		tenant:    tenant,
		sessionID: sessionID,
		ch:        &recordingChannel{},
	}
	frame := contracts.EngineFrame{Type: "model.request", Data: map[string]any{"model_request_id": redeliveryID("mreq"), "messages": []any{}}}

	continues, err := orch.dispatchModel(ctx, st, frame)
	if err == nil {
		t.Fatal("before_model deny did not fail the step")
	}
	if continues {
		t.Fatal("a denied model step reported it continues")
	}
	if firer.fired != 1 {
		t.Fatalf("before_model hook fired %d times, want 1", firer.fired)
	}
	assertEventJournaled(t, cs, sessionID, "policy.denied.v1", 1)
}

// TestBeforeRepositoryPublishDenyRejects proves a before_repository_publish POLICY deny REJECTS the
// publication (spec §28.17): RequestPublication journals policy.denied.v1 and returns a denied result the
// model sees — no pending approval is recorded. The hook sees the RESOLVED destination (operation/branch/
// remote), not just the tool name.
func TestBeforeRepositoryPublishDenyRejects(t *testing.T) {
	ctx := context.Background()
	cs, tenant, sessionID, runID := openLedgerSpine(t)
	pool := cs.Pool()

	// Seed a binding + preparation receipt so RunPublicationTarget resolves a destination for the run.
	bindingID := redeliveryID("repo")
	execSQL(t, pool, `INSERT INTO repository_bindings (id, organization_id, project_id, provider, repository_identity, clone_url, default_branch)
		VALUES ($1,$2,$3,'github','o/r','git@h:o/r.git','main')`, bindingID, tenant.Organization, tenant.Project)
	execSQL(t, pool, `INSERT INTO preparation_receipts (id, repository_binding_id, organization_id, project_id, base_commit, tree_hash, branch, run_id)
		VALUES ($1,$2,$3,$4,'basecommit','treehash','agent/work',$5)`,
		redeliveryID("prcpt"), bindingID, tenant.Organization, tenant.Project, runID)

	firer := &denyingFirer{point: extensions.HookPointBeforeRepositoryPublish, hookID: "hook_pub", reason: "publishing is disabled in this project"}
	reg := &publicationRegistry{store: cs, hooks: firer}
	scope := toolbroker.TaskScope{Org: tenant.Organization, Project: tenant.Project, SessionID: sessionID, RunID: runID}

	result, err := reg.RequestPublication(ctx, scope, map[string]any{"operation": "push_branch", "head_sha": "deadbeef"})
	if err != nil {
		t.Fatalf("RequestPublication() error = %v", err)
	}
	if result["status"] != "denied" || result["reason"] != firer.reason {
		t.Fatalf("publication not rejected by the hook: %+v", result)
	}
	if firer.fired != 1 {
		t.Fatalf("before_repository_publish hook fired %d times, want 1", firer.fired)
	}
	assertEventJournaled(t, cs, sessionID, "policy.denied.v1", 1)
}
