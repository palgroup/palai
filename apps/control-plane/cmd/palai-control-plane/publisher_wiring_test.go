package main

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
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
	var askedRef string
	repo.ConnectionSecrets = func(ref string) ([]byte, error) {
		askedRef = ref
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
		WorkspaceRoot: root, Project: "prj_local", AttemptFence: 7,
		ConnectionRef: "demo-local-token", Identity: "local/demo-target",
	})
	if err != nil {
		t.Fatalf("an approved push under the binding's own credential failed on an App-less deployment: %v", err)
	}
	if askedRef != "demo-local-token" {
		t.Fatalf("the publisher resolved ref=%q; want the BINDING's own ref", askedRef)
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

// TestAnAppLessDeploymentRefusesABindingWithNoCredential is the other half, and it is the SECURITY half.
//
// A binding with no connection_ref has always meant "publish under the deployment's App". On a deployment
// with no App there is nothing for that to mean, and both wrong answers are worse than a refusal:
// publishing anonymously, or publishing under whatever credential happens to be reachable. The refusal is
// the publisher's own, so it reaches the operator — the publication tool refuses the request before a
// human is asked, and the publish boundary would warn on the row.
func TestAnAppLessDeploymentRefusesABindingWithNoCredential(t *testing.T) {
	appLessEnv(t)
	repo, ok := repositoryPublisher().(*execution.RepositoryPublisher)
	if !ok {
		t.Fatal("repositoryPublisher() built no *execution.RepositoryPublisher")
	}
	err := repo.CanPublish("")
	if err == nil {
		t.Fatal("a binding with NO connection_ref was accepted on a deployment with no GitHub App: there is " +
			"no credential this publication could be made under, so approving it authorizes nothing")
	}
	// The refusal has to be actionable. It names both routes out, because an operator on a single-tenant
	// stack wants the connection_ref one and an operator running a fleet wants the App one.
	for _, want := range []string{"PALAI_GITHUB_APP_ID", "connection_ref"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("the refusal does not name %q, so it does not say what to do about it: %v", want, err)
		}
	}
	// ...and the binding that DOES name its own credential is accepted on the SAME deployment. Without this
	// leg the assertion above is satisfied by a publisher that refuses everything, which would be the
	// original defect wearing an error message.
	if err := repo.CanPublish("demo-local-token"); err != nil {
		t.Fatalf("a binding naming its own credential was refused on an App-less deployment: %v — this is the "+
			"configuration the connection_ref path exists for", err)
	}
}

// TestTheAppPathIsUnchangedWhenAnAppIsConfigured pins the direction this change must NOT alter. The App
// path is the right answer for a fleet, where handing a run a tenant PAT would hand it that account's
// whole reach, and it stays exactly as it was for the bindings that name no credential of their own.
func TestTheAppPathIsUnchangedWhenAnAppIsConfigured(t *testing.T) {
	appLessEnv(t)
	key := filepath.Join(t.TempDir(), "app.pem")
	if err := os.WriteFile(key, testAppPEM(t), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PALAI_GITHUB_APP_ID", "123456")
	t.Setenv("PALAI_GITHUB_APP_INSTALLATION_ID", "7891011")
	t.Setenv("PALAI_GITHUB_APP_PRIVATE_KEY_FILE", key)
	t.Setenv("PALAI_GIT_REPO", "acme/widgets")

	repo, ok := repositoryPublisher().(*execution.RepositoryPublisher)
	if !ok {
		t.Fatal("repositoryPublisher() built no *execution.RepositoryPublisher")
	}
	if repo.Broker == nil {
		t.Fatal("a configured GitHub App built no deployment-global broker: every binding without its own " +
			"credential now has none")
	}
	if repo.PRClient == nil {
		t.Fatal("a configured App with PALAI_GIT_REPO=owner/repo built no pull-request client: an approved " +
			"pull request would answer 'no pull-request client wired'")
	}
	if err := repo.CanPublish(""); err != nil {
		t.Fatalf("a ref-less binding was refused on a deployment WITH an App: %v", err)
	}
	if err := repo.CanPublish("rcon_tenant_pat"); err != nil {
		t.Fatalf("a ref-carrying binding was refused on a deployment with an App: %v", err)
	}
	// AND THE CONNECTION HALF SURVIVES THE APP BEING PRESENT. The two paths are independent in BOTH
	// directions: this is the assertion that would have caught the fix being written as a second gate.
	if repo.ConnectionSecrets == nil || repo.PRClientFor == nil {
		t.Fatal("configuring a GitHub App removed the per-binding credential path: a tenant that provisioned " +
			"its own credential would silently publish as the deployment App")
	}
}

// TestAHalfConfiguredAppBuildsNoDeploymentCredential pins the state an operator reaches by editing
// .env.local and being interrupted. Two of three must not read as configured — a broker built from a
// partial triple would be an App identity nobody chose.
func TestAHalfConfiguredAppBuildsNoDeploymentCredential(t *testing.T) {
	key := filepath.Join(t.TempDir(), "app.pem")
	if err := os.WriteFile(key, testAppPEM(t), 0o600); err != nil {
		t.Fatal(err)
	}
	full := map[string]string{
		"PALAI_GITHUB_APP_ID":               "123456",
		"PALAI_GITHUB_APP_INSTALLATION_ID":  "7891011",
		"PALAI_GITHUB_APP_PRIVATE_KEY_FILE": key,
	}
	for missing := range full {
		t.Run(missing, func(t *testing.T) {
			appLessEnv(t)
			for name, value := range full {
				if name != missing {
					t.Setenv(name, value)
				}
			}
			repo, ok := repositoryPublisher().(*execution.RepositoryPublisher)
			if !ok {
				t.Fatal("repositoryPublisher() built no *execution.RepositoryPublisher")
			}
			if repo.Broker != nil {
				t.Fatalf("a half-configured App (%s unset) built a deployment-global broker anyway", missing)
			}
			// It is still a publisher, and a binding with its own credential still publishes through it. That
			// is the difference between this and the old behaviour, where a half-configured App returned nil
			// and took the tenant path down with it.
			if err := repo.CanPublish("demo-local-token"); err != nil {
				t.Fatalf("a half-configured App refused a binding carrying its OWN credential: %v", err)
			}
			if err := repo.CanPublish(""); err == nil {
				t.Fatal("a half-configured App accepted a ref-less binding: there is no credential for it")
			}
		})
	}
}

// testAppPEM is a real RSA private key, generated for this test. NewGitHubAppBroker parses it, so a
// placeholder string would fail the parse and make every "an App was configured" assertion below pass for
// the wrong reason — a nil broker, which is exactly the state these tests exist to tell apart. It signs
// nothing that leaves the process.
func testAppPEM(t *testing.T) []byte {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate an App key: %v", err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)})
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
