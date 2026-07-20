//go:build component

// Package repository holds the real-Git component proofs for deterministic preparation (spec §30.3)
// and untrusted-repo containment (§30.4). They run under `make test-component TEST=repository`
// against the host's git (no container), and are kept out of the credential-free unit tier only to
// group them with the other component suites. REP-001 proves the exact-commit receipt is recorded
// by infrastructure, independent of any model; REP-002 proves a hostile repository is contained.
package repository

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/palgroup/palai/adapters/repositories"
)

// TestPrepareYieldsExactCommitWithReceipt proves REP-001: preparation fetches the EXACT requested
// commit and records a model-independent receipt (base commit, tree hash, work branch). The receipt
// is computed from Git plumbing over the checked-out tree — no model runs — so its provenance holds
// before any model behavior (§30.3 line 3273).
func TestPrepareYieldsExactCommitWithReceipt(t *testing.T) {
	requireGit(t)
	ctx := context.Background()

	remote := newLocalRemote(t)
	// The tree hash the remote's exact commit points at — computed independently, to compare against.
	wantTree := gitRunner(t, remote.url)("rev-parse", remote.head+"^{tree}")

	target := t.TempDir()
	res, err := repositories.Prepare(ctx, repositories.NewLocalBroker(), repositories.Request{
		CloneURL:      remote.url,
		RequestedRef:  remote.head, // an exact commit SHA
		DefaultBranch: "main",
		TargetDir:     filepath.Join(target, "repo"),
		SecretsDir:    t.TempDir(),
		WorkBranch:    "agent/ses_abc/run_def",
	})
	if err != nil {
		t.Fatalf("Prepare() error = %v", err)
	}

	if res.Receipt.BaseCommit != remote.head {
		t.Fatalf("receipt base commit = %q, want the exact requested commit %q", res.Receipt.BaseCommit, remote.head)
	}
	if res.Receipt.TreeHash != wantTree {
		t.Fatalf("receipt tree hash = %q, want %q (the exact commit's tree)", res.Receipt.TreeHash, wantTree)
	}
	if res.Receipt.Branch != "agent/ses_abc/run_def" {
		t.Fatalf("receipt branch = %q, want the generated work branch", res.Receipt.Branch)
	}
	if res.Receipt.RequestedRef != remote.head {
		t.Fatalf("receipt requested ref = %q, want %q", res.Receipt.RequestedRef, remote.head)
	}
	// The exact tree materialized: the committed file is present with its committed content.
	body, err := os.ReadFile(filepath.Join(target, "repo", "README.md"))
	if err != nil {
		t.Fatalf("prepared worktree is missing the committed file: %v", err)
	}
	if strings.TrimSpace(string(body)) != "hello world" {
		t.Fatalf("prepared worktree content = %q, want the exact committed content", body)
	}
}

// TestHostileRepoConfigHooksSubmoduleContained proves REP-002: a hostile repository cannot escape
// preparation. A poisoned ambient git config that redirects hooks does NOT run its hook (hooks are
// disabled AND ambient config is stripped, §30.4), and a hostile ext:: submodule URL is rejected
// and never materialized — its arbitrary command never runs.
func TestHostileRepoConfigHooksSubmoduleContained(t *testing.T) {
	requireGit(t)
	ctx := context.Background()

	pwn := t.TempDir()
	hookSentinel := filepath.Join(pwn, "HOOK_RAN")
	submoduleSentinel := filepath.Join(pwn, "SUBMODULE_RAN")

	// A remote whose .gitmodules points a submodule at an ext:: transport — arbitrary-command RCE if
	// git ever acts on it (spec §30.4 submodule-URL validation).
	remote := newLocalRemote(t)
	run := gitRunner(t, remote.url)
	gitmodules := "[submodule \"evil\"]\n\tpath = evil\n\turl = ext::sh -c \"touch " + submoduleSentinel + "\"\n"
	if err := os.WriteFile(filepath.Join(remote.url, ".gitmodules"), []byte(gitmodules), 0o644); err != nil {
		t.Fatal(err)
	}
	run("add", ".gitmodules")
	run("commit", "-q", "-m", "add hostile submodule")
	head := run("rev-parse", "HEAD")

	// A poisoned ambient GLOBAL git config that redirects hooks to an attacker dir — the shared-host
	// config-override vector (§30.4). WITHOUT hardening, the checkout during preparation would run
	// the post-checkout hook.
	maliciousHooks := t.TempDir()
	postCheckout := filepath.Join(maliciousHooks, "post-checkout")
	if err := os.WriteFile(postCheckout, []byte("#!/bin/sh\ntouch "+hookSentinel+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	globalCfg := filepath.Join(t.TempDir(), "gitconfig")
	if err := os.WriteFile(globalCfg, []byte("[core]\n\thooksPath = "+maliciousHooks+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GIT_CONFIG_GLOBAL", globalCfg)

	res, err := repositories.Prepare(ctx, repositories.NewLocalBroker(), repositories.Request{
		CloneURL:      remote.url,
		RequestedRef:  head,
		DefaultBranch: "main",
		TargetDir:     filepath.Join(t.TempDir(), "repo"),
		SecretsDir:    t.TempDir(),
		WorkBranch:    "agent/ses_x/run_y",
	})
	if err != nil {
		t.Fatalf("Prepare() error = %v, want success with containment findings", err)
	}

	// Hooks disabled + ambient config stripped: the redirected post-checkout hook never ran.
	if _, err := os.Stat(hookSentinel); err == nil {
		t.Fatal("the redirected post-checkout hook executed — hooks/ambient-config were NOT contained")
	}
	// The hostile submodule URL was rejected and never materialized: its ext:: command never ran.
	if _, err := os.Stat(submoduleSentinel); err == nil {
		t.Fatal("the ext:: submodule command executed — the hostile submodule was NOT contained")
	}
	var rejected bool
	for _, f := range res.Findings {
		if f.Kind == "submodule_url_rejected" {
			rejected = true
		}
	}
	if !rejected {
		t.Fatalf("no submodule_url_rejected finding recorded; findings = %+v", res.Findings)
	}
}

// --- fixtures -------------------------------------------------------------------------------------

func requireGit(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skipf("git not found on PATH: %v", err)
	}
}

type localRemote struct{ url, head string }

// newLocalRemote builds a real local Git remote with one commit, serving exact commits by SHA so
// preparation can fetch a pinned commit deterministically.
func newLocalRemote(t *testing.T) localRemote {
	t.Helper()
	dir := t.TempDir()
	run := gitRunner(t, dir)
	run("init", "-q", "-b", "main")
	run("config", "uploadpack.allowAnySHA1InWant", "true")
	run("config", "uploadpack.allowReachableSHA1InWant", "true")
	if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte("hello world\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	run("add", "README.md")
	run("commit", "-q", "-m", "initial commit")
	return localRemote{url: dir, head: run("rev-parse", "HEAD")}
}

func gitRunner(t *testing.T, dir string) func(args ...string) string {
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
