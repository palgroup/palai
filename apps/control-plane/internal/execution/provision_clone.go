package execution

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"

	"github.com/palgroup/palai/adapters/repositories"
	"github.com/palgroup/palai/adapters/sandboxes/oci/workspace"
	"github.com/palgroup/palai/packages/coordinator"
	"github.com/palgroup/palai/packages/runner"
	toolbroker "github.com/palgroup/palai/packages/tool-broker"
)

// THE CLONE RUNS WHERE THE REPOSITORY WILL LIVE (A.3 T5), AND THE CREDENTIAL IS STILL THIS SIDE'S.
//
// PrepareRepository (repository.go) is the same eleven steps against THIS host's filesystem, and it
// stays: the deterministic e2e tier, the coding journey and the live smoke all drive it, and a
// control plane that realizes a workspace on its own disk should prepare it on its own disk. This is
// its sibling for the case where the allocation is on a machine.
//
// THE SPLIT OF DUTIES IS THE POINT. The control plane resolves the binding, chooses the broker,
// MINTS the read credential and REVOKES it on every return path — none of which the machine could do,
// because none of the key material behind them is there. The machine spends the credential into a
// credential helper next to the repository it is cloning, which is the one place git can read it
// from, and returns the receipt. What crosses is one five-minute, audience-bound token; what never
// crosses is the App key, the tenant's stored credential, or the resolver that reaches them.

// cloneOnMachine performs the §30.3 deterministic preparation for a fresh allocation through ops, and
// records the model-independent receipt. It is the exact composition PrepareRepository performs, with
// repositories.Prepare's own mint/defer-Revoke hoisted out here — the machine gets a token, not a
// broker, so the revoke has to be on this side of the wire.
func (o *Orchestrator) cloneOnMachine(ctx context.Context, ops toolbroker.WorkspaceOps, tenant coordinator.Tenant, plan workspacePlan, runID string, fence uint64) error {
	remote, ok := ops.(*RemoteWorkspace)
	if !ok {
		// A control-plane-realized allocation: prepare it here, exactly as before this task. This is not
		// a fallback for a machine that failed to answer — realizeRootWorkspace chose these ops because
		// the attempt's channel reaches no machine at all, so there is no elsewhere for the bytes to be.
		_, err := PrepareRepository(ctx, o.spine, o.provisionBroker, tenant, PrepareRepositoryInput{
			BindingID:    plan.ws.BindingID,
			RunID:        runID,
			RequestedRef: plan.ws.RequestedRef,
			WorkBranch:   rootWorkBranch(plan.ws.WorkspaceID, runID),
			TargetDir:    workspace.RepoPath(plan.hostPath),
			SecretsDir:   filepath.Join(plan.hostPath, provisionSecretsDir),
			AttemptFence: fence,
			ToolCall:     "provision",
			// A binding that names a connection_ref clones under its OWN tenant's credential (E13 T9).
			ConnectionSecrets: o.provisionSecrets,
		})
		if err != nil {
			return err
		}
		// Defense-in-depth (§24): the credential helper + git home the preparation wrote into
		// <alloc>/secrets are useless now (the read credential is already revoked), but the shell sandbox
		// rw-mounts the allocation ROOT, so remove the staging area rather than leave even revoked-token
		// residue in the sandbox-visible tree. It is snapshot-excluded regardless, so removing it changes
		// no snapshot. The machine does the same for its own allocation, inside the clone it performed —
		// the party that wrote the file is the party that removes it.
		return os.RemoveAll(filepath.Join(plan.hostPath, provisionSecretsDir))
	}

	binding, found, err := o.spine.GetRepositoryBinding(ctx, tenant, plan.ws.BindingID)
	if err != nil {
		return err
	}
	if !found {
		return fmt.Errorf("prepare repository: binding %s not found in scope", plan.ws.BindingID)
	}
	broker, err := bindingBroker(o.provisionBroker, tenant, binding, o.provisionSecrets)
	if err != nil {
		return err
	}
	audience := repositories.Audience{
		Project: tenant.Project, Run: runID, AttemptFence: fence, ToolCall: "provision",
	}
	cred, err := broker.Mint(ctx, repositories.ScopeRead, audience)
	if err != nil {
		return fmt.Errorf("prepare repository: mint read credential: %w", err)
	}
	// STEP 9 ("remove credential material") ON EVERY RETURN PATH, which is where repositories.Prepare
	// puts it and why it is a defer here too: a late error must never leave a live credential behind,
	// and after this task the token also exists in a helper file on somebody else's disk. Revoking kills
	// the secret at the provider (App broker) and in this vault; what the machine keeps is a dead copy,
	// and the machine removes its own staging area either way (removeSecretsStaging below).
	defer func() {
		if err := broker.Revoke(ctx, cred.Handle); err != nil {
			// A failed revoke leaves a real installation token valid until its ~1h expiry — log it so the
			// failure is visible (the local broker's revoke is filesystem-only and effectively never fails).
			log.Printf("repository preparation: revoke read credential %s failed: %v", cred.Handle, err)
		}
	}()

	spendable, ok := broker.(repositories.HostSpendable)
	if !ok {
		// FAIL CLOSED. A broker whose secret cannot be spent on another host cannot clone onto one, and
		// carrying on would mint a credential, send nothing, and let the machine clone anonymously —
		// which SUCCEEDS for a public repository and fails for every private one, so the deployment would
		// look healthy until the first private binding.
		return errors.New("prepare repository: this broker cannot hand its credential to the machine that holds the workspace")
	}
	username, token, err := spendable.Secret(cred.Handle)
	if err != nil {
		return fmt.Errorf("prepare repository: %w", err)
	}
	if username != repositories.HelperUsername {
		// The machine builds its helper with repositories.NewTokenBroker, which mints
		// repositories.HelperUsername. A broker that minted something else would authenticate wrongly on
		// the far side and report it as a clone failure; refusing here names the real cause.
		return fmt.Errorf("prepare repository: credential username %q is not the one the machine will use", username)
	}

	receipt, findings, err := remote.Clone(ctx, runner.WorkspaceCloneRequest{
		CloneURL:      binding.CloneUrl,
		RequestedRef:  plan.ws.RequestedRef,
		DefaultBranch: binding.DefaultBranch,
		WorkBranch:    rootWorkBranch(plan.ws.WorkspaceID, runID),
		SecretsDir:    filepath.Join(plan.hostPath, provisionSecretsDir),
		Policy:        policyFromBinding(binding.Policy),
		Audience:      audience,
		Token:         token,
	})
	if err != nil {
		return err
	}
	for _, finding := range findings {
		// Findings are DATA — a rejected submodule URL, an escaping symlink — never executed actions
		// (§30.4). They were returned to a caller that dropped them when the clone ran here; on the far
		// side of a wire they would be dropped silently, which is the difference between "this repository
		// tried something" and nothing at all.
		log.Printf("workspace %s: repository finding %s at %s: %s",
			plan.ws.WorkspaceID, finding.Kind, finding.Path, finding.Detail)
	}
	if err := o.spine.RecordPreparationReceipt(ctx, tenant, coordinator.PreparationReceiptInput{
		ReceiptID:    "prep_" + randHex16(),
		BindingID:    plan.ws.BindingID,
		RunID:        runID,
		RequestedRef: receipt.RequestedRef,
		BaseCommit:   receipt.BaseCommit,
		TreeHash:     receipt.TreeHash,
		Branch:       receipt.Branch,
	}); err != nil {
		return err
	}
	return nil
}
