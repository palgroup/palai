package repositories

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/palgroup/palai/packages/contracts"
)

// Request is the infrastructure-owned input to a deterministic preparation (spec §30.3). It is NOT
// model-controlled: the URL, ref, policy, and audience come from the resolved binding and the run,
// never from model output, so the recorded provenance does not depend on model behavior (§30.3 line
// 3273, REP-001).
type Request struct {
	CloneURL      string   // the binding's clone/fetch URL (clean — no credential, §30.2)
	RequestedRef  string   // branch/tag/commit to fetch; empty falls back to DefaultBranch
	DefaultBranch string   // the binding's default branch
	TargetDir     string   // the workspace repo dir the engine will see (e.g. <allocation>/repo)
	SecretsDir    string   // the snapshot-excluded /secrets area for the credential helper + git home (§29.10)
	WorkBranch    string   // the generated agent/<...> work branch to check out; empty = detached read-only (§30.3 step 6)
	Policy        Policy   // untrusted-repo policy (submodule/LFS/commit-signing, §30.1/§30.4)
	Audience      Audience // binds the minted read credential to this operation (§28.11)
}

// Result is the outcome of a preparation: the model-independent receipt (§30.3 step 10) and any
// untrusted-repo containment findings recorded along the way (§30.4). Findings are data — a rejected
// submodule URL, an escaping symlink — never executed actions.
type Result struct {
	Receipt  contracts.PreparationReceipt
	Findings []Finding
}

// now is overridable so a test can pin prepared_at; production uses the wall clock.
var now = time.Now

// Prepare runs the infrastructure-owned 11-step deterministic preparation (spec §30.3): it fills
// TargetDir with the exact requested commit under an untrusted-repo-hardened Git, then records the
// model-independent receipt. The credential is minted just in time, feeds only a Git credential
// helper, and is revoked on EVERY return path (the deferred Revoke is step 9 "remove credential
// material" — a late error can never leave a live credential behind). The engine MAY run ordinary
// Git afterward, but this initial provenance does not depend on model behavior (REP-001).
func Prepare(ctx context.Context, broker Broker, req Request) (Result, error) {
	ref := req.RequestedRef
	if ref == "" {
		ref = req.DefaultBranch // step 1: resolve binding + requested ref
	}
	if ref == "" {
		return Result{}, fmt.Errorf("prepare: no ref requested and binding has no default branch")
	}
	if req.CloneURL == "" {
		return Result{}, fmt.Errorf("prepare: clone URL is required")
	}

	// The hooks/home/helper dirs live in the snapshot-excluded /secrets area, NEVER inside TargetDir
	// (which the engine sees). An empty hooksDir means no repository-supplied hook can run (§30.4).
	hooksDir := filepath.Join(req.SecretsDir, "githooks-empty")
	homeDir := filepath.Join(req.SecretsDir, "githome")
	for _, d := range []string{req.TargetDir, req.SecretsDir, hooksDir, homeDir} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			return Result{}, fmt.Errorf("prepare: create %s: %w", d, err)
		}
	}
	git := func(extraConfig []string, args ...string) (string, error) {
		return runGit(ctx, req.TargetDir, homeDir, hooksDir, extraConfig, args...)
	}

	// Step 3: clean repository init with safe Git configuration (the hardened -c/env carry it).
	if _, err := git(nil, "init", "-q"); err != nil {
		return Result{}, err
	}

	// Step 2: mint the read credential just in time. Step 9 is the deferred Revoke below: the
	// credential is removed on every path, so a late error cannot leave it live (§30.2).
	cred, err := broker.Mint(ctx, ScopeRead, req.Audience)
	if err != nil {
		return Result{}, fmt.Errorf("prepare: mint read credential: %w", err)
	}
	defer func() { _ = broker.Revoke(ctx, cred.Handle) }()
	helperConfig, err := broker.writeHelper(cred.Handle, req.CloneURL, req.SecretsDir)
	if err != nil {
		return Result{}, fmt.Errorf("prepare: %w", err)
	}
	// The brokered helper is added AFTER the hardened `credential.helper=` reset, so it is the only
	// active helper: the token reaches Git only here, never in argv or the remote URL (§30.2, REP-003).
	fetchConfig := []string{"-c", "credential.helper=" + helperConfig}

	// Step 4: fetch the exact commit/ref with bounded history (--depth=1). The remote URL is clean.
	if _, err := git(fetchConfig, "fetch", "--depth=1", req.CloneURL, ref); err != nil {
		return Result{}, err
	}

	// Step 5: verify commit identity if policy requires it (default: not required).
	if req.Policy.RequireSignedCommits {
		if _, err := git(nil, "verify-commit", "FETCH_HEAD"); err != nil {
			return Result{}, fmt.Errorf("prepare: commit identity verification failed: %w", err)
		}
	}

	// Step 6: check out the generated work branch, or a detached read-only state.
	if req.WorkBranch != "" {
		if _, err := git(nil, "checkout", "-q", "-b", req.WorkBranch, "FETCH_HEAD"); err != nil {
			return Result{}, err
		}
	} else {
		if _, err := git(nil, "checkout", "-q", "--detach", "FETCH_HEAD"); err != nil {
			return Result{}, err
		}
	}

	// Step 7 + 8: materialize allowed submodules/LFS (default: none), recording containment findings
	// for anything the policy refuses. Hooks + unsafe filters are already disabled by the hardened env.
	findings, err := materializeSubmodules(git, req.TargetDir, req.Policy)
	if err != nil {
		return Result{}, err
	}
	worktreeFindings, err := checkWorktreeContainment(req.TargetDir)
	if err != nil {
		return Result{}, err
	}
	findings = append(findings, worktreeFindings...)

	// Step 10: record base commit, tree hash, branch, and the preparation receipt — computed by
	// infrastructure from the checked-out tree, before any model behavior (REP-001).
	baseCommit, err := git(nil, "rev-parse", "HEAD")
	if err != nil {
		return Result{}, err
	}
	treeHash, err := git(nil, "rev-parse", "HEAD^{tree}")
	if err != nil {
		return Result{}, err
	}
	receipt := contracts.PreparationReceipt{
		RequestedRef: req.RequestedRef,
		BaseCommit:   baseCommit,
		TreeHash:     treeHash,
		Branch:       req.WorkBranch,
		PreparedAt:   now().UTC().Format(time.RFC3339),
	}
	// Step 11: expose the prepared workspace to the engine — the caller mounts TargetDir; the receipt
	// is its model-independent provenance.
	return Result{Receipt: receipt, Findings: findings}, nil
}

// materializeSubmodules validates every .gitmodules URL against policy (spec §30.4) and, only when
// the policy allows submodules AND the URL passes, updates it. A rejected URL is a Finding and the
// submodule is NOT fetched — a hostile ext:: / file:// URL never reaches a Git transport. The
// default policy materializes nothing.
func materializeSubmodules(git func([]string, ...string) (string, error), targetDir string, policy Policy) ([]Finding, error) {
	gitmodules := filepath.Join(targetDir, ".gitmodules")
	raw, err := os.ReadFile(gitmodules)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil // no submodules
		}
		return nil, fmt.Errorf("read .gitmodules: %w", err)
	}
	var findings []Finding
	var safe []string
	for _, u := range submoduleURLs(string(raw)) {
		if verr := policy.validateSubmoduleURL(u); verr != nil {
			findings = append(findings, Finding{Kind: "submodule_url_rejected", Path: ".gitmodules", Detail: verr.Error()})
			continue
		}
		safe = append(safe, u)
	}
	// A hostile URL was rejected: refuse to run `submodule update` at all, since it would re-read
	// .gitmodules and could still act on the rejected entry. The submodules stay unmaterialized.
	if len(findings) > 0 || !policy.AllowSubmodules {
		return findings, nil
	}
	if len(safe) > 0 {
		if _, err := git(nil, "submodule", "update", "--init", "--depth=1"); err != nil {
			return findings, fmt.Errorf("materialize submodules: %w", err)
		}
	}
	return findings, nil
}

// submoduleURLs extracts the `url = ...` values from a .gitmodules file. It is a minimal INI scan:
// enough to validate transports before any Git command reads the file.
func submoduleURLs(gitmodules string) []string {
	var urls []string
	for _, line := range strings.Split(gitmodules, "\n") {
		trimmed := strings.TrimSpace(line)
		if rest, ok := strings.CutPrefix(trimmed, "url"); ok {
			if eq := strings.IndexByte(rest, '='); eq >= 0 {
				urls = append(urls, strings.TrimSpace(rest[eq+1:]))
			}
		}
	}
	return urls
}

// runGit runs one Git command under the untrusted-repo-hardened config + environment (spec §30.4).
// cmd.Env is built from exactly hardenedEnv, so no ambient GIT_CONFIG_* / HOME / netrc can inject a
// credential helper or a command. Output is captured (never streamed to a terminal), and the token
// is structurally absent from argv — the credential enters only via the store helper file.
func runGit(ctx context.Context, dir, homeDir, hooksDir string, extraConfig []string, args ...string) (string, error) {
	full := append([]string{}, hardenedConfig(hooksDir)...)
	full = append(full, extraConfig...)
	full = append(full, args...)
	cmd := exec.CommandContext(ctx, "git", full...)
	cmd.Dir = dir
	cmd.Env = hardenedEnv(homeDir)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(stderr.String())
		if len(msg) > 2048 {
			msg = msg[:2048]
		}
		return "", fmt.Errorf("git %s: %w: %s", args[0], err, msg)
	}
	return strings.TrimSpace(stdout.String()), nil
}
