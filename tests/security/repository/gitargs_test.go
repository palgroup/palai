// Argument-injection defense (spec §30.4): a flag-shaped clone URL / ref / branch / base would be
// reparsed by git as an option (the classic --upload-pack=<cmd> command-execution vector). Every
// caller-supplied git positional is refused before git runs, and the invocations carry an explicit
// "--" end-of-options separator. These prove the refusal happens BEFORE git and the separator is present.
package repository_test

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/palgroup/palai/adapters/repositories"
)

func TestPrepareRefusesFlagShapedGitPositionals(t *testing.T) {
	requireGit(t)
	ctx := context.Background()
	remote := newLocalRemote(t)
	pwn := filepath.Join(t.TempDir(), "PWNED")

	base := repositories.Request{
		CloneURL: remote.url, RequestedRef: remote.head, DefaultBranch: "main",
		TargetDir: filepath.Join(t.TempDir(), "repo"), SecretsDir: t.TempDir(), WorkBranch: "agent/s/r",
	}
	cases := map[string]func(r repositories.Request) repositories.Request{
		"flag-shaped clone url": func(r repositories.Request) repositories.Request {
			r.CloneURL = "--upload-pack=touch " + pwn
			return r
		},
		"flag-shaped ref":         func(r repositories.Request) repositories.Request { r.RequestedRef = "--upload-pack=x"; return r },
		"flag-shaped work branch": func(r repositories.Request) repositories.Request { r.WorkBranch = "-x"; return r },
		"ref with dot-dot escape": func(r repositories.Request) repositories.Request { r.RequestedRef = "refs/../evil"; return r },
	}
	for name, mut := range cases {
		req := mut(base)
		req.TargetDir = filepath.Join(t.TempDir(), "repo")
		req.SecretsDir = t.TempDir()
		if _, err := repositories.Prepare(ctx, repositories.NewLocalBroker(), req); err == nil {
			t.Fatalf("%s: Prepare returned nil error, want refused before git", name)
		}
	}
	// The injection never executed: refusal happens before git, so no sentinel was created.
	if _, err := os.Stat(pwn); err == nil {
		t.Fatal("the --upload-pack injection executed — refusal did not happen before git")
	}
}

// TestPrepareFetchCarriesEndOfOptionsSeparator proves the "--" separator is present on the fetch, so
// even a positional that slipped the allowlist could not be reparsed as an option.
func TestPrepareFetchCarriesEndOfOptionsSeparator(t *testing.T) {
	requireGit(t)
	ctx := context.Background()
	remote := newLocalRemote(t)
	trace := installGitTrace(t)

	if _, err := repositories.Prepare(ctx, repositories.NewLocalBroker(), repositories.Request{
		CloneURL: remote.url, RequestedRef: remote.head, DefaultBranch: "main",
		TargetDir: filepath.Join(t.TempDir(), "repo"), SecretsDir: t.TempDir(), WorkBranch: "agent/s/r",
	}); err != nil {
		t.Fatalf("Prepare() error = %v", err)
	}
	traceBytes, _ := os.ReadFile(trace)
	if !bytes.Contains(traceBytes, []byte("fetch --depth=1 -- ")) {
		t.Fatalf("fetch invocation is missing the '--' end-of-options separator:\n%s", firstLineWith(traceBytes, "fetch"))
	}
}

// TestWorktreeAndMergeRefuseFlagShaped proves the same guard on the worktree/merge mechanics (shared
// gitargs helper): a flag-shaped base or merge branch is refused before git.
func TestWorktreeAndMergeRefuseFlagShaped(t *testing.T) {
	requireGit(t)
	ctx := context.Background()
	remote := newLocalRemote(t)

	if _, err := repositories.AddIsolatedWorktree(ctx, remote.url, filepath.Join(t.TempDir(), "wt"), "s", "r", "--upload-pack=x"); err == nil {
		t.Fatal("AddIsolatedWorktree accepted a flag-shaped base, want refused")
	}
	if _, err := repositories.MergeBranch(ctx, remote.url, "-x"); err == nil {
		t.Fatal("MergeBranch accepted a flag-shaped branch, want refused")
	}
}

func firstLineWith(b []byte, needle string) string {
	for _, line := range strings.Split(string(b), "\n") {
		if strings.Contains(line, needle) {
			return line
		}
	}
	return "(no line with " + needle + ")"
}
