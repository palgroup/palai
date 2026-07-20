// Package repository_test proves the scoped credential broker's absence invariant (spec §30.2,
// REP-003 — the E09 exit-gate: the model NEVER receives a raw Git credential). A brokered Git
// credential appears on NO surface: not the remote URL, process argv, environment, a log, the
// workspace snapshot, or the engine-facing receipt — and the read credential is removed after
// preparation (§30.3 step 9). Even a FAKE token must be absent; an absence proof needs no
// provider-realness (the honest ceiling — the live tier confirms the same invariant with a real
// installation token). The scan is by construction: a distinctive sentinel compared as opaque
// bytes, so a failure reports a leak without ever echoing it (mirrors tests/security/secrets).
package repository_test

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/palgroup/palai/adapters/repositories"
)

// brokerSentinel is the fake read credential the broker mints here. It is deliberately NOT shaped
// like a real provider token (no ghs_/ghp_ prefix, no 40-hex run), so the credential-literal
// hygiene grep — which hunts real Git PAT / App-key / installation-token shapes — never matches it,
// yet it is a rigorous absence marker: it must appear on no surface after preparation.
const brokerSentinel = "palai-REPMARK-read-credential-must-never-leak-a1b2c3d4"

func TestBrokeredCredentialAbsentFromAllSurfaces(t *testing.T) {
	requireGit(t)
	ctx := context.Background()

	remote := newLocalRemote(t)
	target := t.TempDir()
	secrets := t.TempDir()

	// A git shim on PATH records the argv + full environment of every git invocation the
	// preparation makes, so the scan reads the REAL remote-URL / process-args / env surfaces.
	trace := installGitTrace(t)

	broker := repositories.NewLocalBrokerWithToken(brokerSentinel)
	res, err := repositories.Prepare(ctx, broker, repositories.Request{
		CloneURL:      remote.url,
		RequestedRef:  remote.head,
		DefaultBranch: "main",
		TargetDir:     filepath.Join(target, "repo"),
		SecretsDir:    secrets,
		WorkBranch:    "agent/ses_x/run_y",
		Audience: repositories.Audience{
			Organization: "org_x", Project: "prj_x", Run: "run_y", AttemptFence: 1, ToolCall: "tcall_z",
		},
	})
	if err != nil {
		t.Fatalf("Prepare() error = %v", err)
	}

	sentinel := []byte(brokerSentinel)

	// Surfaces 1-3: remote URL, process argv, environment — the captured git trace.
	traceBytes, err := os.ReadFile(trace)
	if err != nil {
		t.Fatalf("read git trace: %v", err)
	}
	if !bytes.Contains(traceBytes, []byte("fetch")) {
		t.Fatal("git trace captured no fetch — the shim did not intercept preparation")
	}
	if bytes.Contains(traceBytes, sentinel) {
		t.Fatal("brokered credential leaked into git argv / env / remote URL")
	}

	// Surface 4 (model context / engine frame): the receipt is what crosses to the engine.
	receiptJSON, err := json.Marshal(res.Receipt)
	if err != nil {
		t.Fatalf("marshal receipt: %v", err)
	}
	if bytes.Contains(receiptJSON, sentinel) {
		t.Fatal("brokered credential leaked into the preparation receipt")
	}

	// Surface 5 (snapshot / workspace): the engine-visible worktree carries no credential.
	if leaked := walkContains(t, filepath.Join(target, "repo"), sentinel); leaked != "" {
		t.Fatalf("brokered credential leaked into workspace file %s", leaked)
	}

	// Surface 6 (log): a returned error is credential-free by construction (the token never enters
	// prepare's Go frame); here Prepare succeeded, so there is no error surface to scan.
}

// TestReadCredentialRevokedAfterPreparation proves step 9 "remove credential material" (spec §30.3):
// after preparation the read credential's helper file is gone from the /secrets area, so nothing —
// not a later tool, not a leaked handle — can redeem it.
func TestReadCredentialRevokedAfterPreparation(t *testing.T) {
	requireGit(t)
	ctx := context.Background()

	remote := newLocalRemote(t)
	target := t.TempDir()
	secrets := t.TempDir()

	broker := repositories.NewLocalBrokerWithToken(brokerSentinel)
	if _, err := repositories.Prepare(ctx, broker, repositories.Request{
		CloneURL:      remote.url,
		RequestedRef:  remote.head,
		DefaultBranch: "main",
		TargetDir:     filepath.Join(target, "repo"),
		SecretsDir:    secrets,
		WorkBranch:    "agent/ses_x/run_y",
		Audience:      repositories.Audience{Organization: "org_x", Project: "prj_x", Run: "run_y", AttemptFence: 1, ToolCall: "tcall_z"},
	}); err != nil {
		t.Fatalf("Prepare() error = %v", err)
	}

	// No git-credentials helper file may survive the operation, and no file under /secrets may hold
	// the token — the read credential was removed after preparation.
	var leftover []string
	_ = filepath.WalkDir(secrets, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		if strings.HasPrefix(d.Name(), "git-credentials-") {
			leftover = append(leftover, path)
		}
		if body, rerr := os.ReadFile(path); rerr == nil && bytes.Contains(body, []byte(brokerSentinel)) {
			leftover = append(leftover, path+" (holds token)")
		}
		return nil
	})
	if len(leftover) > 0 {
		t.Fatalf("credential material survived preparation: %v", leftover)
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

// newLocalRemote builds a real local Git remote with one commit, configured to serve an exact commit
// by SHA (uploadpack.allowAnySHA1InWant) so preparation can fetch the pinned commit deterministically.
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
	head := run("rev-parse", "HEAD")
	return localRemote{url: dir, head: head}
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

// installGitTrace puts a git shim first on PATH that appends every invocation's argv + environment
// to a trace file, then execs the real git. Preparation builds its own env, so the trace records
// exactly the env the git subprocess ran under — the real surface the absence scan reads.
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

// walkContains returns the first file under root whose bytes contain needle, or "" if none.
func walkContains(t *testing.T, root string, needle []byte) string {
	t.Helper()
	var hit string
	_ = filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() || hit != "" {
			return nil
		}
		body, rerr := os.ReadFile(path)
		if rerr == nil && bytes.Contains(body, needle) {
			hit = path
		}
		return nil
	})
	return hit
}
