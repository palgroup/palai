//go:build component

package repository

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/palgroup/palai/adapters/sandboxes/oci/workspace"
	"github.com/palgroup/palai/packages/contracts"
	"github.com/palgroup/palai/packages/runner"
)

// resolvedTempRoot is the machine's allocation root, symlink-resolved. macOS puts t.TempDir() behind
// /var -> /private/var, and the server's own §24 check resolves both sides; an unresolved root here
// would make every operation below compare two spellings of one directory and refuse.
func resolvedTempRoot(t *testing.T) string {
	t.Helper()
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatalf("resolve temp root: %v", err)
	}
	return dir
}

// THE SAME EMPTY STRING MEANT TWO OPPOSITE THINGS ON TWO SIDES OF ONE WIRE, AND THE MACHINE'S READING
// WON EVERY CLONE THAT REACHED A MACHINE.
//
// An empty token is how this package's brokers say "offer no credential" ON PURPOSE:
// credentialVault.Secret and writeHelper both document it in those words, and AnonymousBroker.Mint was
// changed to mint exactly it on 2026-08-02, because a fabricated `palai-local-…` token BROKE clones
// that work anonymously. The control plane honours that on its own disk. The runner's clone read the
// same empty string as "the credential path is broken" and refused.
//
// A NATIVE MAC TAKES THE MACHINE BRANCH FOR EVERY RUN, so on the one shipped configuration the
// 2026-08-02 fix was unreachable. Measured against the live stack 2026-08-05: a fresh run against a
// PUBLIC binding with no connection_ref (octocat/Hello-World) failed with
// `workspace_provisioning_failed` — "clone request carries no read credential" — and left its
// workspace in `preparing`, where 22 others already sat.
//
// THESE TESTS DRIVE `Handle`, NOT `clone`, and the JSON round trip below is not ceremony. The struct's
// own doc says it is exported "so the two ends of this wire cannot drift into two spellings of the
// same field"; a test that handed Go values straight to the unexported function would prove the
// branch and prove nothing about `allow_anonymous`, which is the field the branch depends on.

// cloneRequestOverTheWire marshals the request exactly as RemoteWorkspace.Clone does — through JSON —
// so the server decodes the same field names a real control plane sends.
func cloneRequestOverTheWire(t *testing.T, in runner.WorkspaceCloneRequest) map[string]any {
	t.Helper()
	raw, err := json.Marshal(in)
	if err != nil {
		t.Fatalf("marshal clone request: %v", err)
	}
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("unmarshal clone request: %v", err)
	}
	return out
}

// openAllocationOnTheMachine performs the `open` the runner does when it admits the lease, so the clone
// below writes into a real §29.9 layout rather than into a directory this test invented.
func openAllocationOnTheMachine(t *testing.T, srv *runner.WorkspaceServer, root, alloc string) string {
	t.Helper()
	answer := srv.Handle(context.Background(), contracts.RunnerMessage{
		Type: runner.WorkspaceRequestType,
		Data: runner.WorkspaceRequestData("ws_open", runner.WorkspaceOpOpen, filepath.Join(root, alloc), nil),
	})
	if errText, _ := answer.Data["error"].(string); errText != "" {
		t.Fatalf("the machine refused to open the allocation: %s", errText)
	}
	dir, _ := answer.Data["root"].(string)
	if dir == "" {
		t.Fatalf("the machine opened the allocation but named no root: %#v", answer.Data)
	}
	return dir
}

// TestADeclaredAnonymousCloneIsServedByTheMachine is the fix: a control plane that says it MEANT to
// offer no credential gets the unauthenticated clone it asked for, and the receipt proves the bytes
// actually arrived rather than the refusal merely changing shape.
func TestADeclaredAnonymousCloneIsServedByTheMachine(t *testing.T) {
	requireGit(t)
	remote := newLocalRemote(t)
	root := resolvedTempRoot(t)
	srv := runner.NewWorkspaceServer(root)
	alloc := openAllocationOnTheMachine(t, srv, root, "alloc_anon")

	answer := srv.Handle(context.Background(), contracts.RunnerMessage{
		Type: runner.WorkspaceRequestType,
		Data: runner.WorkspaceRequestData("ws_clone", runner.WorkspaceOpClone, alloc, map[string]any{
			"clone": cloneRequestOverTheWire(t, runner.WorkspaceCloneRequest{
				CloneURL:       remote.url,
				RequestedRef:   remote.head,
				DefaultBranch:  "main",
				WorkBranch:     "agent/ws_anon/run_anon",
				SecretsDir:     filepath.Join(alloc, "secrets"),
				Token:          "",   // the anonymous broker's own answer — see AnonymousBroker.Mint
				AllowAnonymous: true, // …and the control plane saying it meant it
			}),
		}),
	})

	if errText, _ := answer.Data["error"].(string); errText != "" {
		t.Fatalf("a DECLARED anonymous clone was refused: %s\n"+
			"This is the defect measured live on 2026-08-05: a public repository with no connection_ref "+
			"clones on the control plane's own disk and was refused on every machine.", errText)
	}
	// THE RECEIPT, NOT THE ABSENCE OF AN ERROR. "no error" would also be true of a clone that fetched
	// nothing, and a run whose workspace is empty fails later and elsewhere.
	//
	// It is a contracts.PreparationReceipt rather than a decoded map because Handle is called in-process
	// here: the transport marshals the answer, and this test is the server, not the socket. The REQUEST
	// still crosses JSON (cloneRequestOverTheWire) because that is the direction the field spelling this
	// fix depends on travels in.
	receipt, ok := answer.Data["receipt"].(contracts.PreparationReceipt)
	if !ok {
		t.Fatalf("the clone answered no receipt: %#v", answer.Data)
	}
	if receipt.BaseCommit != remote.head {
		t.Fatalf("receipt base commit = %q, want the exact requested commit %q", receipt.BaseCommit, remote.head)
	}
	// And the bytes: the committed file is on the machine's disk, under the repo the tools confine to.
	body, err := os.ReadFile(filepath.Join(workspace.RepoPath(alloc), "README.md"))
	if err != nil {
		t.Fatalf("the clone reported a receipt but wrote no worktree: %v", err)
	}
	if strings.TrimSpace(string(body)) != "hello world" {
		t.Fatalf("the anonymously cloned worktree holds %q, want the committed content", body)
	}
}

// TestAnUndeclaredEmptyTokenIsStillRefused is the half that must NOT move. The refusal exists because
// an empty token clones anonymously — which succeeds for a public repository and fails for every
// private one — so a broken credential path would look like a working deployment until the first
// private binding. Only the DECLARED case above was opened; a silence is answered exactly as before.
func TestAnUndeclaredEmptyTokenIsStillRefused(t *testing.T) {
	requireGit(t)
	remote := newLocalRemote(t)
	root := resolvedTempRoot(t)
	srv := runner.NewWorkspaceServer(root)
	alloc := openAllocationOnTheMachine(t, srv, root, "alloc_silent")

	answer := srv.Handle(context.Background(), contracts.RunnerMessage{
		Type: runner.WorkspaceRequestType,
		Data: runner.WorkspaceRequestData("ws_clone", runner.WorkspaceOpClone, alloc, map[string]any{
			"clone": cloneRequestOverTheWire(t, runner.WorkspaceCloneRequest{
				CloneURL:      remote.url,
				RequestedRef:  remote.head,
				DefaultBranch: "main",
				WorkBranch:    "agent/ws_silent/run_silent",
				SecretsDir:    filepath.Join(alloc, "secrets"),
				Token:         "", // no token AND no declaration: the machine cannot tell why
			}),
		}),
	})

	errText, _ := answer.Data["error"].(string)
	if !strings.Contains(errText, "no read credential") {
		t.Fatalf("an UNDECLARED empty token answered %q, want the fail-closed refusal.\n"+
			"The refusal is the property: a control plane that drops the token in marshalling, or one "+
			"that predates allow_anonymous, must not be handed an anonymous clone it never asked for.", errText)
	}
	// It refused BEFORE cloning, which is the difference between a refusal and a late failure: nothing
	// of the remote reached the allocation.
	if _, err := os.ReadFile(filepath.Join(workspace.RepoPath(alloc), "README.md")); err == nil {
		t.Fatal("the refusal still cloned: README.md is in the allocation, so the guard reported a refusal it did not perform")
	}
}
