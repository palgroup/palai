package execution

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/palgroup/palai/adapters/repositories"
	"github.com/palgroup/palai/apps/control-plane/internal/extensions"
	"github.com/palgroup/palai/packages/coordinator"
	toolbroker "github.com/palgroup/palai/packages/tool-broker"
)

// publicationRegistry adapts the durable-spine store to the broker's PublicationRegistry seam (spec
// §30.8): a push/PR tool records a PENDING publication through it, and it resolves the destination from
// the run's binding so the model never supplies a remote. The idempotency key + one-shot request hash
// are formed by the adapter (repositories.IdempotencyKey/RequestHash), so the dedupe and approval
// binding have a single definition shared with the publish path.
type publicationRegistry struct {
	store *coordinator.Store
	// hooks fires before_repository_publish (spec §28.17, E12 T8) once the destination is resolved. Nil ⇒ no
	// hook fires (bit-unchanged). The orchestrator propagates its firer here via SetHookFirer.
	hooks HookFirer
}

func newPublicationRegistry(store *coordinator.Store) *publicationRegistry {
	return &publicationRegistry{store: store}
}

// RequestPublication records a pending publication + approval and returns the pending-approval result
// the model sees (spec §30.8, §22.4). It resolves the run's remote/branch/base from the binding, forms
// the operation-specific idempotency key + head-bound request hash, and records the row idempotently — a
// duplicate request resolves to the existing pending approval (Replayed), never a second.
func (r *publicationRegistry) RequestPublication(ctx context.Context, scope toolbroker.TaskScope, op map[string]any) (map[string]any, error) {
	tenant := coordinator.Tenant{Organization: scope.Org, Project: scope.Project}
	operation, _ := op["operation"].(string)
	if operation != string(repositories.OpPushBranch) && operation != string(repositories.OpOpenPullRequest) {
		return nil, fmt.Errorf("publication tool: unsupported operation %q", operation)
	}
	headSHA, _ := op["head_sha"].(string)
	if headSHA == "" {
		return nil, fmt.Errorf("publication tool: the workspace has no head to publish")
	}

	target, found, err := r.store.RunPublicationTarget(ctx, tenant, scope.RunID)
	if err != nil {
		return nil, err
	}
	if !found {
		return nil, fmt.Errorf("publication tool: the run prepared no repository, nothing to publish")
	}

	// before_repository_publish hooks fire once the destination is RESOLVED (spec §28.17, E12 T8): a policy
	// hook sees the exact operation/branch/base/remote (not just the tool name a before_tool hook saw) and can
	// DENY the publication. A deny journals policy.denied.v1 and returns a denied result the model sees — the
	// publication is rejected, no pending approval recorded. No-op when no firer is wired.
	if r.hooks != nil {
		outcome, herr := r.hooks.Fire(ctx, extensions.HookEvent{
			Org: scope.Org, Project: scope.Project, SessionID: scope.SessionID, ResponseID: scope.ResponseID,
			RunID: scope.RunID, Point: extensions.HookPointBeforeRepositoryPublish,
			Payload: map[string]any{
				"operation": operation, "remote": target.Remote, "branch": target.Branch,
				"base": target.Base, "head_sha": headSHA,
			},
		})
		if herr != nil {
			return nil, herr
		}
		if outcome.Denied {
			payload, _ := json.Marshal(map[string]any{
				"run_id": scope.RunID, "hook_id": outcome.HookID, "point": extensions.HookPointBeforeRepositoryPublish,
				"reason": outcome.Reason, "operation": operation, "branch": target.Branch,
			})
			if jerr := r.store.JournalRunEvent(ctx, tenant, scope.SessionID, scope.ResponseID, scope.RunID, eventPolicyDenied, payload); jerr != nil {
				return nil, jerr
			}
			return map[string]any{"status": "denied", "reason": outcome.Reason, "hook_id": outcome.HookID}, nil
		}
	}

	pubOp := repositories.PublishOperation(operation)
	idemKey := repositories.IdempotencyKey(scope.Org, scope.Project, scope.RunID, pubOp, target.Remote, target.Branch, target.Base, headSHA)
	reqHash := repositories.RequestHash(scope.Org, scope.Project, scope.RunID, pubOp, target.Remote, target.Branch, target.Base, headSHA)

	args := map[string]any{}
	if title, ok := op["title"].(string); ok {
		args["title"] = title
	}
	if body, ok := op["body"].(string); ok {
		args["body"] = body
	}

	pub, err := r.store.RequestPublication(ctx, tenant, coordinator.PublicationRequest{
		PublicationID:  newExecID("pub"),
		ApprovalID:     newExecID("apr"),
		SessionID:      scope.SessionID,
		RunID:          scope.RunID,
		ResponseID:     scope.ResponseID,
		Operation:      operation,
		Remote:         target.Remote,
		Branch:         target.Branch,
		Base:           target.Base,
		HeadSHA:        headSHA,
		IdempotencyKey: idemKey,
		RequestHash:    reqHash,
		Display:        publicationDisplay(operation, target, headSHA),
		Args:           args,
	})
	if err != nil {
		return nil, err
	}
	return map[string]any{
		"status":         "pending_approval",
		"publication_id": pub.ID,
		"operation":      pub.Operation,
		"branch":         pub.Branch,
		"request_hash":   pub.RequestHash,
		"display":        pub.Display,
		"replayed":       pub.Replayed,
	}, nil
}

// publicationDisplay is the exact, credential-free operation display shown for approval (spec §22.4):
// the destination + head for a push, the branch->base for a PR. The model's prose never replaces this.
func publicationDisplay(operation string, target coordinator.PublicationTarget, headSHA string) string {
	if operation == string(repositories.OpOpenPullRequest) {
		return fmt.Sprintf("open draft pull request %s -> %s on %s", target.Branch, target.Base, target.Remote)
	}
	return fmt.Sprintf("push %s @ %s -> %s", target.Branch, headSHA, target.Remote)
}
