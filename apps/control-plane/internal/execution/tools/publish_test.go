package tools

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/palgroup/palai/adapters/sandboxes/oci/workspace"
	toolbroker "github.com/palgroup/palai/packages/tool-broker"
)

// fakePublicationRegistry records the last publication op a tool requested, so a tool test can assert
// the tool computed the workspace head and asked for the right operation without a database.
type fakePublicationRegistry struct {
	lastScope toolbroker.TaskScope
	lastOp    map[string]any
}

func (f *fakePublicationRegistry) RequestPublication(_ context.Context, scope toolbroker.TaskScope, op map[string]any) (map[string]any, error) {
	f.lastScope, f.lastOp = scope, op
	return map[string]any{"status": "pending_approval", "operation": op["operation"]}, nil
}

// TestPushToolRecordsPendingPublicationAtWorkspaceHead proves the push tool (spec §30.9): it does NOT
// push — it computes the workspace repo's current head and records a PENDING push publication through
// the registry (returning pending_approval), so the push is gated behind an approval. The model never
// supplies a head or destination.
func TestPushToolRecordsPendingPublicationAtWorkspaceHead(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skipf("git not found: %v", err)
	}
	root := realTempDir(t)
	repoDir := filepath.Join(root, workspace.RepoDir)
	initRepo(t, repoDir)
	if err := os.WriteFile(filepath.Join(repoDir, "edit.txt"), []byte("agent edit\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	run := repoGit(t, repoDir)
	run("add", "edit.txt")
	run("commit", "-q", "-m", "agent edit")
	head := run("rev-parse", "HEAD")

	reg := &fakePublicationRegistry{}
	out, err := pushExec(context.Background(), toolbroker.ExecEnv{WorkspaceRoot: root, Publications: reg}, nil)
	if err != nil {
		t.Fatalf("pushExec() error = %v", err)
	}
	if status, _ := out["status"].(string); status != "pending_approval" {
		t.Fatalf("push tool result status = %q, want pending_approval (a push is gated, not immediate)", status)
	}
	if op, _ := reg.lastOp["operation"].(string); op != "push_branch" {
		t.Fatalf("recorded operation = %q, want push_branch", op)
	}
	if got, _ := reg.lastOp["head_sha"].(string); got != head {
		t.Fatalf("recorded head_sha = %q, want the workspace repo head %q", got, head)
	}
}

// TestPushToolFailsCleanlyWithoutRegistry proves the push tool fails cleanly rather than acting when no
// publication registry is wired (the SetShellRunner-nil discipline).
func TestPushToolFailsCleanlyWithoutRegistry(t *testing.T) {
	if _, err := pushExec(context.Background(), toolbroker.ExecEnv{WorkspaceRoot: t.TempDir()}, nil); err == nil {
		t.Fatal("pushExec with no registry = nil error, want a clean failure")
	}
}

// destinationFields are the words a publication destination is spelled with. None may ever appear in either
// publish tool's input schema.
// E23 T6 adds four spellings, and three of them are the merge tool's own destination: the pull request
// NUMBER, the repository, and the merge METHOD. The fourth ("pull_request") catches the whole family in one
// go, since `pull_request_number` and `pr` are the same field asked for differently.
var destinationFields = []string{"base", "head", "branch", "remote", "repo", "repository", "ref", "url",
	"target", "clone_url", "origin", "upstream", "into", "onto", "number", "pull_request", "merge_method", "method"}

// TestNoPublishToolLetsTheModelNameTheDestination is E22 T4's structural anchor (plan §3.5 X17), and it is
// the reason "open the PR against dev" needed NO code change: the base comes from the binding's
// default_branch through RunPublicationTarget, so `dev` is a value an operator set in .env.local and not a
// constant anybody typed. That only holds while the model cannot name a destination.
//
// A `base` property on the pull-request tool would break it in the quietest possible way. Nothing would
// fail: the schema would accept it, pullRequestExec would ignore it today, and the next person to wire it
// through would be implementing a field the tool already advertised. By then the approval message a human
// pressed — "open draft pull request X -> dev" — would be showing a base the model chose, which is the one
// thing an approval cannot survive. So the refusal lives here, at the schema, where it is one line to check
// and impossible to add by accident.
//
// It sweeps the whole property set rather than looking for "base" alone, because `target_branch`,
// `head_ref` and `remote` are the same field with different spellings.
func TestNoPublishToolLetsTheModelNameTheDestination(t *testing.T) {
	for _, tool := range []toolbroker.Tool{PushTool(), PullRequestTool(), MergeTool()} {
		props, ok := tool.InputSchema["properties"].(map[string]any)
		if !ok {
			t.Fatalf("%s has no `properties` object in its input schema: %#v — a schema this guard cannot read "+
				"is a schema it cannot hold", tool.Name, tool.InputSchema)
		}
		if extra, _ := tool.InputSchema["additionalProperties"].(bool); extra {
			t.Fatalf("%s sets additionalProperties=true, so a destination can be smuggled past the property "+
				"sweep below by simply not being declared", tool.Name)
		}
		for field := range props {
			for _, banned := range destinationFields {
				if strings.Contains(strings.ToLower(field), banned) {
					t.Fatalf("%s accepts %q from the model. The destination is resolved from the run's binding "+
						"(RunPublicationTarget: clone_url, the preparation receipt's branch, default_branch) — a "+
						"base the model can choose is a base the approver did not approve", tool.Name, field)
				}
			}
		}
	}
	// The push tool takes NOTHING at all, and that is stronger than "no destination": there is no per-call
	// argument for a policy pass to have to filter later.
	if props, _ := PushTool().InputSchema["properties"].(map[string]any); len(props) != 0 {
		t.Fatalf("the push tool's input schema grew properties %v; it takes no arguments — what to push is the "+
			"run's own committed head, and where to push it is the binding's", props)
	}
	// The pull-request tool takes exactly the two the model is allowed to PROPOSE. They are recorded on the
	// publication's args for a later policy-filtered pass and are not the destination (E09 publishes with a
	// deterministic title/body), so their presence is not a hole — but a THIRD field would need a reason.
	props, _ := PullRequestTool().InputSchema["properties"].(map[string]any)
	if len(props) != 2 || props["title"] == nil || props["body"] == nil {
		t.Fatalf("the pull-request tool's input schema is %v, want exactly title + body — prose the model may "+
			"propose, and nothing that decides where the change lands", props)
	}
	// E23 T6's RED #3, and it is the strictest of the three because a merge has the least to negotiate. The
	// merge tool takes NOTHING: which pull request comes from the run's OWN published receipt
	// (RunPublishedPullRequest), the head from that publication's row, and merge_method from the binding's
	// policy. A `pull_request_number` here would let a model aim an approved merge at somebody else's pull
	// request while the approval message still read like this run's; a `merge_method` would let it choose
	// squash over the operator's rebase. Neither exists, and this is where a future one dies.
	if props, _ := MergeTool().InputSchema["properties"].(map[string]any); len(props) != 0 {
		t.Fatalf("the merge tool's input schema grew properties %v; it takes no arguments at all — WHAT is "+
			"merged is the pull request this run published, AT which commit is the head a human approved, and "+
			"HOW is the binding's policy", props)
	}
}
