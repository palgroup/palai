//go:build live

// Package subagents_test is the E09 Task 6 live smoke. It runs only under the `live` build tag, in
// `make test-live-provider PROVIDER=provider-one CASE=subagents-isolated`, which loads the real
// provider credential from .env.local. It proves, in one run: a real provider round-trip for a
// PARENT and a distinct-model CHILD (two real chat-completion ids — the SUB-002 config-driven
// delegation shape at the provider layer), and that T6's isolated-worktree + conflict-aware merge
// mechanics apply on a REAL git repo (SUB-006 + REP-011). The credential is only an opaque needle
// for the leak scan and is never printed.
//
// HONEST CEILING (team-lead approved): workspace provisioning is not wired into the run path yet, so
// the real provider calls and the real worktree are NOT causally joined — the child does not edit in
// a live-provisioned worktree during its model call. This proves the provider round-trip live AND
// the worktree/merge mechanics live, with workspace_mode enforcement proven deterministically
// (child_dispatch_test.go). Model-driven spawn is T7; provisioning-into-live-worktree is T9.
package subagents_test

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	providerone "github.com/palgroup/palai/adapters/models/provider_one"
	"github.com/palgroup/palai/adapters/repositories"
	"github.com/palgroup/palai/packages/contracts"
	modelbroker "github.com/palgroup/palai/packages/model-broker"
)

const credentialEnv = "OPENAI_API_KEY"

func liveModel() string {
	if m := os.Getenv("PALAI_LIVE_MODEL"); m != "" {
		return m
	}
	return "gpt-4o-mini"
}

// liveChildModel is the child's routed model — the SUB-002 "cheap route" (a distinct model id, same
// provider). Defaults to the parent model when unset; the two calls still yield two distinct
// chat-completion ids.
func liveChildModel() string {
	if m := os.Getenv("PALAI_LIVE_CHILD_MODEL"); m != "" {
		return m
	}
	return liveModel()
}

func TestLiveSubagentIsolatedWorktreeRealProvider(t *testing.T) {
	secret := os.Getenv(credentialEnv)
	if secret == "" {
		t.Fatalf("%s is unset; the live tier loads it from .env.local at runtime", credentialEnv)
	}
	if _, err := exec.LookPath("git"); err != nil {
		t.Skipf("git not found: %v", err)
	}
	ctx := context.Background()

	broker := modelbroker.New(modelbroker.Config{
		Adapters: map[string]modelbroker.ModelAdapter{"provider-one": providerone.Adapter{}},
		Secrets:  modelbroker.EnvResolver{"provider-one": credentialEnv},
	})

	// Two real provider round-trips: a parent and a distinct-model child — the config-driven
	// delegation shape at the provider layer (SUB-002: two real chat-completion ids).
	parent := liveRoute(t, broker, "mreq_live_parent", liveModel())
	child := liveRoute(t, broker, "mreq_live_child", liveChildModel())
	if parent.ProviderRequestID == child.ProviderRequestID {
		t.Fatalf("parent and child share a provider request id %q — want two distinct real calls", parent.ProviderRequestID)
	}
	for who, res := range map[string]modelbroker.Result{"parent": parent, "child": child} {
		if !strings.HasPrefix(res.ProviderRequestID, "chatcmpl") {
			t.Errorf("%s provider request id %q is not a real chat completion id", who, res.ProviderRequestID)
		}
	}

	// T6 mechanics on a REAL repo: the isolated child worktree + conflict-aware merge back to parent.
	repo, base := liveRepo(t)
	wt, err := repositories.AddIsolatedWorktree(ctx, repo, filepath.Join(t.TempDir(), "child"), "ses_live", "run_live", base)
	if err != nil {
		t.Fatalf("AddIsolatedWorktree() error = %v", err)
	}
	if wt.Branch != "agent/ses_live/run_live" || !wt.Writable {
		t.Fatalf("isolated worktree = %+v, want writable branch agent/ses_live/run_live (SUB-006)", wt)
	}
	// The child edits in its worktree, on its branch; the parent worktree is untouched.
	if err := os.WriteFile(filepath.Join(wt.Path, "README.md"), []byte("child edit\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	childGit(t, wt.Path)("add", "README.md")
	childGit(t, wt.Path)("commit", "-q", "-m", "child change")
	if got, _ := os.ReadFile(filepath.Join(repo, "README.md")); strings.TrimSpace(string(got)) != "base" {
		t.Fatalf("parent worktree mutated by the child: %q, want unchanged 'base'", got)
	}
	// The explicit merge applies the child's branch conflict-aware.
	res, err := repositories.MergeBranch(ctx, repo, "agent/ses_live/run_live")
	if err != nil || !res.Merged {
		t.Fatalf("MergeBranch() = %+v err=%v, want a clean merge (REP-011)", res, err)
	}
	if got, _ := os.ReadFile(filepath.Join(repo, "README.md")); strings.TrimSpace(string(got)) != "child edit" {
		t.Fatalf("merge did not apply the child's edit to the parent: %q", got)
	}

	// Leak scan by construction: the credential must appear on no captured surface.
	for _, res := range []modelbroker.Result{parent, child} {
		blob, err := json.Marshal(res)
		if err != nil {
			t.Fatalf("marshal result: %v", err)
		}
		if bytes.Contains(blob, []byte(secret)) {
			t.Fatal("the provider credential leaked into a captured result")
		}
	}
	t.Logf("T6 live smoke: parent=%s child=%s; isolated worktree + merge applied on a real repo (ceiling: provisioning unwired, T7/T9)", parent.ProviderRequestID, child.ProviderRequestID)
}

func liveRoute(t *testing.T, broker *modelbroker.Broker, reqID, model string) modelbroker.Result {
	t.Helper()
	res, err := broker.Route(context.Background(), "provider-one", modelbroker.Request{
		ModelRequestID: contracts.ModelRequestID(reqID),
		RouteRevision:  1,
		ModelStepID:    "step-1",
		Model:          model,
		Messages:       []modelbroker.Message{{Role: "user", Content: "Reply with the single word: ok."}},
		Deadline:       time.Now().Add(60 * time.Second),
		Reservation:    modelbroker.Reservation{MaxTotalTokens: 2000},
		Secret:         modelbroker.SecretRef("provider-one"),
	}, func(modelbroker.Delta) {})
	if err != nil {
		t.Fatalf("route %s: %v", model, err)
	}
	if res.Error != nil {
		t.Fatalf("provider returned a sanitized error: code=%s status=%d", res.Error.Code, res.Error.Status)
	}
	if err := res.Validate(); err != nil {
		t.Fatalf("live result is not canonical: %v", err)
	}
	return res
}

func liveRepo(t *testing.T) (string, string) {
	t.Helper()
	dir := t.TempDir()
	run := childGit(t, dir)
	run("init", "-q", "-b", "main")
	if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte("base\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	run("add", "README.md")
	run("commit", "-q", "-m", "base")
	return dir, run("rev-parse", "HEAD")
}

func childGit(t *testing.T, dir string) func(args ...string) string {
	t.Helper()
	return func(args ...string) string {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@e.test", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@e.test")
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
}
