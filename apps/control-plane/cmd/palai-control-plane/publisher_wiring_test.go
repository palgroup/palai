package main

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/palgroup/palai/adapters/sandboxes/oci/workspace"
	"github.com/palgroup/palai/apps/control-plane/internal/execution"
	"github.com/palgroup/palai/packages/coordinator"
)

// THE PUBLISHER'S COMPOSITION, and why it is tested HERE rather than in the execution package.
//
// RepositoryPublisher has honoured a binding's own connection_ref since the publish-credential change:
// approval.go resolves target.ConnectionRef to a token broker and refuses to fall back to the deployment
// App. Every one of those tests passes with `Broker: nil`, because the ref branch never reads it.
//
// The defect was one layer up and no test in that package could see it. repositoryPublisherFromEnv built
// the WHOLE publisher — the connection_ref path included — inside `if appID == "" || installID == "" ||
// keyFile == "" { return nil }`, so on a deployment with no GitHub App the object that knows how to use a
// tenant's own credential was never constructed. main.go then wired it only `if publisher != nil`, and a
// nil publisher makes pumpApprovedPublications a no-op: the row reaches `approved`, the run wakes, and
// nothing is pushed.
//
// That is the feature defeating its own purpose. The connection_ref path exists so an operator does not
// have to put a token on every machine — and it was gated on the deployment-global App it exists to
// replace. Measured on the live native stack (2026-08-02, GET /v1/deployment): PALAI_GITHUB_APP_ID and
// PALAI_GITHUB_APP_INSTALLATION_ID unset, one repository binding carrying connection_ref
// `demo-local-token`. So these tests drive THE CONSTRUCTOR PRODUCTION CALLS, with the environment
// production actually has.

// appLessEnv clears the three GitHub App variables AND restores them after the test, which is what lets
// these read back what the constructor built without leaking into their neighbours.
//
// It OWNS the condition it asserts about rather than inheriting it: an empty string is passed for each
// name explicitly, so the test measures an App-less deployment on a machine that has an App configured in
// its shell exactly as it does on one that does not. A test that inherited the absence would be measuring
// its harness (CLAUDE.md: a test claiming a refusal must own the condition being refused).
func appLessEnv(t *testing.T) {
	t.Helper()
	for _, name := range []string{"PALAI_GITHUB_APP_ID", "PALAI_GITHUB_APP_INSTALLATION_ID",
		"PALAI_GITHUB_APP_PRIVATE_KEY_FILE", "PALAI_GITHUB_REPO", "PALAI_GIT_REPO"} {
		t.Setenv(name, "")
	}
}

// TestABindingsOwnCredentialPublishesWithNoGitHubApp is the owner's transcript, in a test.
//
// A binding names its own credential. The deployment has NO GitHub App. A push is approved. The assertion
// is that it PUBLISHED — the remote's ref moves to the approved head — because "the approval landed and
// nothing happened" is the failure this whole path exists to prevent, and it is the one the operator saw.
func TestABindingsOwnCredentialPublishesWithNoGitHubApp(t *testing.T) {
	requireGit(t)
	appLessEnv(t)

	publisher := repositoryPublisher()
	if publisher == nil {
		t.Fatal("a deployment with no GitHub App built NO publisher, so a binding carrying its own " +
			"connection_ref can never publish: an APPROVED push waits forever")
	}
	repo, ok := publisher.(*execution.RepositoryPublisher)
	if !ok {
		t.Fatalf("repositoryPublisher() returned %T, want *execution.RepositoryPublisher", publisher)
	}

	// THE APP HALF IS ABSENT AND THE CONNECTION HALF IS PRESENT. Asserting both is what makes this a test
	// about INDEPENDENCE rather than about a publisher existing: an object built with an App broker on an
	// App-less stack would be a worse defect than the nil one.
	if repo.Broker != nil {
		t.Fatal("an App-less deployment built a deployment-global broker: with no App id, no installation " +
			"id and no key, there is no credential for one to hold")
	}
	if repo.ConnectionSecrets == nil {
		t.Fatal("the connection_ref resolver is NOT wired on an App-less deployment — the publisher exists " +
			"and still cannot use a tenant's own credential")
	}
	// It is PRODUCTION's resolver, not some resolver. The substitution below is only legitimate because of
	// this line: what a unit test cannot provide is the DB read, and this proves the field it replaces is
	// the one main.go put there.
	if reflect.ValueOf(repo.ConnectionSecrets).Pointer() != reflect.ValueOf(repositoryConnectionSecret).Pointer() {
		t.Fatal("the wired resolver is not main.repositoryConnectionSecret: exactly one place in this tree " +
			"may turn a connection_ref into a credential, and this is a second one")
	}

	// The one thing a unit test cannot provide is the secret store's bytes (repositoryConnectionSecret is
	// DB-only by design). Everything else below is what production built.
	const token = "demo-local-token-value"
	var askedOrg, askedRef string
	repo.ConnectionSecrets = func(org, ref string) ([]byte, error) {
		askedOrg, askedRef = org, ref
		return []byte(token), nil
	}

	root := t.TempDir()
	head := seedWorkspaceRepoAt(t, root)
	bare := seedBareRemoteAt(t)
	receipt, err := repo.Publish(context.Background(), execution.PublishTarget{
		Publication: coordinator.Publication{
			ID: "pub_appless", RunID: "run_1", Operation: "push_branch",
			Remote: bare, Branch: "agent/ses/appless", HeadSHA: head, State: "approved",
		},
		WorkspaceRoot: root, Org: "org_local", Project: "prj_local", AttemptFence: 7,
		ConnectionRef: "demo-local-token", Identity: "local/demo-target",
	})
	if err != nil {
		t.Fatalf("an approved push under the binding's own credential failed on an App-less deployment: %v", err)
	}
	if askedRef != "demo-local-token" || askedOrg != "org_local" {
		t.Fatalf("the publisher resolved (org=%q ref=%q); want the BINDING's own ref under the run's org",
			askedOrg, askedRef)
	}
	if receipt["remote_sha"] != head {
		t.Fatalf("receipt remote_sha = %v, want the approved head %q", receipt["remote_sha"], head)
	}
	// THE REMOTE'S REF, not the receipt's opinion of it.
	if got := remoteRefAt(t, bare, "agent/ses/appless"); got != head {
		t.Fatalf("the remote's ref is %q, want the approved head %q — the receipt claimed a push that did "+
			"not land", got, head)
	}
}

// --- git fixtures. Local to this package: the execution package's copies are unexported test helpers, and
// importing a test helper across a package boundary is not a thing Go does. ---------------------------

func requireGit(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skipf("git not found: %v", err)
	}
}

func seedWorkspaceRepoAt(t *testing.T, root string) string {
	t.Helper()
	repoDir := filepath.Join(root, workspace.RepoDir)
	if err := os.MkdirAll(repoDir, 0o755); err != nil {
		t.Fatal(err)
	}
	run := gitIn(t, repoDir)
	run("init", "-q", "-b", "main")
	if err := os.WriteFile(filepath.Join(repoDir, "f.txt"), []byte("work\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	run("add", "f.txt")
	run("commit", "-q", "-m", "work")
	return run("rev-parse", "HEAD")
}

func seedBareRemoteAt(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	gitIn(t, dir)("init", "-q", "--bare", "-b", "main")
	return dir
}

func remoteRefAt(t *testing.T, remote, branch string) string {
	t.Helper()
	return gitIn(t, remote)("rev-parse", "refs/heads/"+branch)
}

func gitIn(t *testing.T, dir string) func(args ...string) string {
	t.Helper()
	return func(args ...string) string {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.test",
			"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@example.test")
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
}
