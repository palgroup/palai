package execution

import (
	"context"
	"fmt"

	"github.com/palgroup/palai/adapters/repositories"
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
