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
	// canPublish answers whether this deployment has a credential path for a publication carrying the
	// binding's connection_ref, BEFORE a pending approval is recorded. Nil ⇒ no precheck (the fakes in this
	// package, which record approvals nothing will ever pump). Orchestrator.canPublish is what production
	// passes; it delegates to the wired publisher's own CanPublish, so this is not a second copy of the rule.
	canPublish func(connectionRef string) error
}

func newPublicationRegistry(store *coordinator.Store, canPublish func(connectionRef string) error) *publicationRegistry {
	return &publicationRegistry{store: store, canPublish: canPublish}
}

// RequestPublication records a pending publication + approval and returns the pending-approval result
// the model sees (spec §30.8, §22.4). It resolves the run's remote/branch/base from the binding, forms
// the operation-specific idempotency key + head-bound request hash, and records the row idempotently — a
// duplicate request resolves to the existing pending approval (Replayed), never a second.
func (r *publicationRegistry) RequestPublication(ctx context.Context, scope toolbroker.TaskScope, op map[string]any) (map[string]any, error) {
	tenant := coordinator.Tenant{Organization: scope.Org, Project: scope.Project}
	operation, _ := op["operation"].(string)
	switch operation {
	case string(repositories.OpPushBranch), string(repositories.OpOpenPullRequest), string(repositories.OpMergePullRequest):
	default:
		return nil, fmt.Errorf("publication tool: unsupported operation %q", operation)
	}

	target, found, err := r.store.RunPublicationTarget(ctx, tenant, scope.RunID)
	if err != nil {
		return nil, err
	}
	if !found {
		return nil, fmt.Errorf("publication tool: the run prepared no repository, nothing to publish")
	}

	// REFUSED BEFORE A HUMAN IS ASKED, and this is the earliest point at which the question can be
	// answered: target.ConnectionRef is the binding's own credential handle and it has just been resolved.
	//
	// WHY IT IS HERE RATHER THAN AT THE PUMP. The pump's refusal already exists and lands as a warning on
	// the row — but by then a human has read a push, pressed Approve, and been told the run woke. An
	// approval that authorizes nothing is worse than a refusal, because it is indistinguishable from one
	// that worked. So a publication with no credential path never becomes a pending approval at all.
	//
	// IT IS AN ERROR, WHICH IS AN ANSWER. A tool error is delivered to the model as a RESULT, so the agent
	// reports "this deployment cannot publish this binding" in the conversation instead of the run wedging
	// or the operator being handed a button that does nothing.
	if r.canPublish != nil {
		if err := r.canPublish(target.ConnectionRef); err != nil {
			return nil, fmt.Errorf("publication tool: %w", err)
		}
	}

	// THE MERGE DESTINATION IS RESOLVED HERE, and it is resolved from a row the model never touched (E23
	// T6). The pull request is the one THIS RUN published — its number came back from the provider into an
	// approved publication's receipt — and the head is the one that publication carried, which is what makes
	// `sha` a race guard rather than a formality: the commit a human read about is the only commit that can
	// be merged, and the merge tool has no field to say otherwise in.
	prNumber := 0
	headSHA, _ := op["head_sha"].(string) // push/PR: the workspace head the tool computed
	if operation == string(repositories.OpMergePullRequest) {
		number, prHead, ok, perr := r.store.RunPublishedPullRequest(ctx, tenant, scope.RunID)
		if perr != nil {
			return nil, perr
		}
		if !ok {
			return nil, fmt.Errorf("merge tool: this run has published no pull request, so there is nothing to merge")
		}
		// The merge tool supplies no head, so it is OVERWRITTEN rather than defaulted: even if a future caller
		// passed one, the head that gets approved is the published pull request's, never the caller's.
		prNumber, headSHA = number, prHead
	}
	if headSHA == "" {
		return nil, fmt.Errorf("publication tool: the workspace has no head to publish")
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
	if operation == string(repositories.OpMergePullRequest) {
		// The two server-resolved values the publisher will need, carried on the row a human approves rather
		// than re-resolved at the boundary. Re-reading them later would reopen the window this whole path
		// closes: what is merged has to be what the approval screen described, and the row is the only copy
		// of that both the button and the pump agree on.
		args["pull_request_number"], args["merge_method"] = prNumber, target.MergeMethod
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
		Display:        publicationDisplay(operation, target, headSHA, prNumber),
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
// the destination + head for a push, the branch->base for a PR, and since E23 T6 the pull request, its
// branches and its exact head for a merge. The model's prose never replaces this.
//
// The merge sentence names the HEAD as well as the pull request, and that is the sentence doing work rather
// than decorating: the sha it names is the sha sent to GitHub, so what the human read is what is merged or
// nothing is. A merge display that stopped at "#12" would describe a moving target.
func publicationDisplay(operation string, target coordinator.PublicationTarget, headSHA string, prNumber int) string {
	switch operation {
	case string(repositories.OpOpenPullRequest):
		return fmt.Sprintf("open draft pull request %s -> %s on %s", target.Branch, target.Base, target.Remote)
	case string(repositories.OpMergePullRequest):
		return fmt.Sprintf("merge pull request #%d (%s -> %s) on %s at %s",
			prNumber, target.Branch, target.Base, target.Remote, headSHA)
	}
	return fmt.Sprintf("push %s @ %s -> %s", target.Branch, headSHA, target.Remote)
}
