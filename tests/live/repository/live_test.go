//go:build live

// Package repository_test is the E09 Task 3 live repository-clone smoke. It runs only under the
// `live` build tag, in `make test-live-provider PROVIDER=provider-one CASE=repository-clone`, which
// loads credentials from the git-ignored .env.local. It clones a REAL Git repository at an exact
// commit through the infrastructure-owned preparation and proves the brokered Git credential is
// absent from every surface — the live confirmation of the REP-003 exit-gate invariant.
//
// HONEST CEILING: this is the LIVE half of REP-003. The deterministic tier
// (tests/security/repository) already proves credential absence with a fake token — an absence proof
// needs no provider-realness. This tier confirms it with a REAL credential: when the GitHub App
// env is present it mints a REAL installation token (installation token > user PAT, §30.2) and clones
// a private repo; otherwise it clones PALAI_GIT_REPO with the local broker. Because a minted token is
// opaque (the broker never reveals it), absence is proven by SHAPE — no gh[pso]_ token and no
// credential-in-URL appears on any captured surface — plus the clean fetch URL and the removed helper.
package repository_test

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/palgroup/palai/adapters/repositories"
)

// credentialShapes match a real Git credential on any captured surface: a GitHub token (gh[pso]_...)
// or a credential embedded in a URL (user:secret@host / x-access-token:...@). None may appear.
var credentialShapes = []*regexp.Regexp{
	regexp.MustCompile(`gh[psou]_[A-Za-z0-9]{20,}`),
	regexp.MustCompile(`x-access-token:[^@\s]+@`),
	regexp.MustCompile(`://[^/@\s:]+:[^@\s/]+@`),
}

func TestLiveRepositoryCloneCredentialAbsent(t *testing.T) {
	repoURL := os.Getenv("PALAI_GIT_REPO")
	if repoURL == "" {
		t.Skip("PALAI_GIT_REPO is required for the live repository-clone smoke")
	}
	if _, err := exec.LookPath("git"); err != nil {
		t.Skipf("git not found: %v", err)
	}
	ref := os.Getenv("PALAI_GIT_COMMIT")
	if ref == "" {
		ref = os.Getenv("PALAI_GIT_REF")
	}
	ctx := context.Background()

	broker, tier := liveBroker(t, repoURL)
	t.Logf("live repository-clone tier: %s", tier)

	target := t.TempDir()
	secrets := t.TempDir()
	trace := installGitTrace(t)

	res, err := repositories.Prepare(ctx, broker, repositories.Request{
		CloneURL:      repoURL,
		RequestedRef:  ref,
		DefaultBranch: envOr("PALAI_GIT_DEFAULT_BRANCH", "main"),
		TargetDir:     filepath.Join(target, "repo"),
		SecretsDir:    secrets,
		WorkBranch:    "agent/live_ses/live_run",
		Audience: repositories.Audience{
			Organization: "org_live", Project: "prj_live", Run: "run_live", AttemptFence: 1, ToolCall: "tcall_live",
		},
	})
	if err != nil {
		t.Fatalf("Prepare() error = %v", err)
	}
	if res.Receipt.BaseCommit == "" || res.Receipt.TreeHash == "" {
		t.Fatalf("receipt is missing exact-commit provenance: %+v", res.Receipt)
	}
	if ref != "" && !strings.HasPrefix(res.Receipt.BaseCommit, strings.TrimSpace(ref)) && res.Receipt.BaseCommit != ref {
		// A full-SHA ref must resolve to that exact commit; a branch/tag ref is not asserted here.
		if len(ref) == 40 {
			t.Fatalf("receipt base commit = %q, want the exact requested commit %q", res.Receipt.BaseCommit, ref)
		}
	}

	// Surface scan: no credential shape on the git trace (argv/env/remote URL), the receipt, or the
	// engine-visible worktree — the REAL token is absent by shape.
	traceBytes, err := os.ReadFile(trace)
	if err != nil {
		t.Fatalf("read git trace: %v", err)
	}
	if !bytes.Contains(traceBytes, []byte("fetch")) {
		t.Fatal("git trace captured no fetch — the shim did not intercept preparation")
	}
	scanForCredentialShape(t, "git trace (argv/env/remote URL)", traceBytes)
	// The fetch URL argv must be exactly the clean repo URL — no credential spliced in.
	if !bytes.Contains(traceBytes, []byte(repoURL)) {
		t.Fatalf("git trace does not show the clean repo URL %q as fetched", repoURL)
	}
	receiptJSON, _ := json.Marshal(res.Receipt)
	scanForCredentialShape(t, "preparation receipt", receiptJSON)
	scanWorktree(t, filepath.Join(target, "repo"))

	// The read credential's helper file was removed after preparation (§30.3 step 9).
	_ = filepath.WalkDir(secrets, func(path string, d os.DirEntry, err error) error {
		if err == nil && !d.IsDir() && strings.HasPrefix(d.Name(), "git-credentials-") {
			t.Fatalf("credential helper file survived preparation: %s", path)
		}
		return nil
	})
}

// liveBroker builds the credential broker for the run: a real GitHub App installation-token minter
// when the App env is present (the strongest tier), else the local broker for a public/local repo.
func liveBroker(t *testing.T, repoURL string) (repositories.Broker, string) {
	t.Helper()
	appID := os.Getenv("PALAI_GITHUB_APP_ID")
	installID := os.Getenv("PALAI_GITHUB_APP_INSTALLATION_ID")
	keyFile := os.Getenv("PALAI_GITHUB_APP_PRIVATE_KEY_FILE")
	if appID != "" && installID != "" && keyFile != "" {
		pem, err := os.ReadFile(keyFile)
		if err != nil {
			t.Fatalf("read GitHub App private key: %v", err)
		}
		var repos []string
		if r := os.Getenv("PALAI_GITHUB_APP_REPO"); r != "" {
			repos = []string{r}
		}
		broker, err := repositories.NewGitHubAppBroker(repositories.GitHubAppConfig{
			AppID: appID, InstallationID: installID, PrivateKeyPEM: pem, Repositories: repos,
		})
		if err != nil {
			t.Fatalf("NewGitHubAppBroker() error = %v", err)
		}
		return broker, "github-app-installation-token (real)"
	}
	return repositories.NewLocalBroker(), "local-broker (PALAI_GIT_REPO, no App env)"
}

func scanForCredentialShape(t *testing.T, surface string, data []byte) {
	t.Helper()
	for _, re := range credentialShapes {
		if re.Match(data) {
			t.Fatalf("a Git credential shape (%s) leaked into %s", re.String(), surface)
		}
	}
}

func scanWorktree(t *testing.T, root string) {
	t.Helper()
	_ = filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		if strings.Contains(path, string(os.PathSeparator)+".git"+string(os.PathSeparator)) {
			return nil // git's own object store is not a model-visible surface
		}
		if body, rerr := os.ReadFile(path); rerr == nil {
			scanForCredentialShape(t, "worktree file "+path, body)
		}
		return nil
	})
}

func installGitTrace(t *testing.T) string {
	t.Helper()
	realGit, err := exec.LookPath("git")
	if err != nil {
		t.Skipf("git not found: %v", err)
	}
	binDir := t.TempDir()
	trace := filepath.Join(t.TempDir(), "git-trace.log")
	shim := "#!/bin/sh\n{ echo \"ARGV: $*\"; env; } >> " + shellQuote(trace) + "\nexec " + shellQuote(realGit) + " \"$@\"\n"
	if err := os.WriteFile(filepath.Join(binDir, "git"), []byte(shim), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return trace
}

func shellQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
