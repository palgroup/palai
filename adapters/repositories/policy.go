package repositories

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// Policy is the untrusted-repo policy a binding carries into preparation (spec §30.1/§30.4). Its
// zero value is the SAFE default: hooks disabled, submodules/LFS not materialized, commit identity
// not required. A binding opts into more only deliberately.
type Policy struct {
	// AllowSubmodules materializes .gitmodules submodules whose URLs pass validateSubmoduleURL.
	// Default false: submodules are recorded as findings, not fetched (§30.4).
	AllowSubmodules bool
	// AllowLFS is the binding's LFS policy knob (§30.1). ponytail: NOT yet wired — LFS smudge is
	// ALWAYS skipped (hardenedEnv sets GIT_LFS_SKIP_SMUDGE=1 unconditionally), which is fail-closed
	// (a hostile repo cannot force an unbounded LFS pull). Making AllowLFS=true actually materialize
	// LFS is deferred to the changeset/artifact task (E09 Task 5) where the size-limit lives.
	AllowLFS bool
	// RequireSignedCommits verifies commit identity before checkout (§30.3 step 5). Default false.
	RequireSignedCommits bool
	// AllowedSubmoduleSchemes overrides the default transport allowlist for submodule URLs.
	AllowedSubmoduleSchemes []string
}

// Finding is an untrusted-repo containment record (spec §30.4): a defense that fired during
// preparation — a rejected submodule URL, an escaping symlink, a case collision. It is data, never
// an executed action; the preparation records it and continues (or fails, for a hard reject).
type Finding struct {
	Kind   string // "submodule_url_rejected", "symlink_escape", "case_collision"
	Path   string // the .gitmodules path or worktree path the finding concerns
	Detail string // safe, credential-free description
}

// defaultSubmoduleSchemes is the transport allowlist a submodule URL must use unless the binding
// widens it. It excludes ext:: (arbitrary-command RCE) and file:// (local-path escape) by omission
// — the two classic untrusted-submodule vectors (§30.4).
var defaultSubmoduleSchemes = []string{"https", "ssh", "git"}

// defaultProtectedBranches are the branches direct agent work is denied on by default (spec §30.5):
// the common default branches. A binding's branch policy may widen the protected set.
var defaultProtectedBranches = []string{"main", "master"}

// DirectWorkAllowed reports whether an agent may do direct mutable work on branch (spec §30.5).
// Direct work on a protected or default branch is denied by default — mutable work uses a generated
// agent/<session>/<run> branch instead (ChildBranch). protected widens the default set from policy.
func DirectWorkAllowed(branch string, protected []string) bool {
	if strings.TrimSpace(branch) == "" {
		return false
	}
	for _, p := range defaultProtectedBranches {
		if branch == p {
			return false
		}
	}
	for _, p := range protected {
		if branch == p {
			return false
		}
	}
	return true
}

// hardenedConfig is the `-c` override list every Git invocation in a preparation carries. Together
// with hardenedEnv they neutralize the untrusted-repo attack surface (spec §30.4): hooks are
// disabled, arbitrary-command transports/filters are refused, and neither repository nor ambient
// configuration can redirect a credential helper or execute a command. hooksDir is an empty
// directory the preparation owns, so no repository-supplied hook can run.
func hardenedConfig(hooksDir string) []string {
	return []string{
		"-c", "core.hooksPath=" + hooksDir, // hooks disabled — no repo/ambient hook runs (§30.4)
		"-c", "core.fsmonitor=false", // no fsmonitor command execution
		"-c", "protocol.ext.allow=never", // block ext:: arbitrary-command transport (submodule/remote RCE)
		"-c", "protocol.file.allow=always", // local file remotes (tests/local repos) — never ext::
		"-c", "credential.helper=", // clear any inherited helper before the brokered one is added
		"-c", "core.sshCommand=/bin/false", // no attacker-chosen ssh command
		"-c", "core.pager=cat", // no pager command execution
		"-c", "core.askPass=", // no askpass program
		"-c", "fetch.recurseSubmodules=false", // submodules materialized explicitly + policy-gated (§30.4)
		"-c", "transfer.fsckObjects=true", // validate fetched objects
		"-c", "gc.auto=0", // no background gc forking during preparation
	}
}

// hardenedEnv is the environment every preparation Git invocation runs under. It REPLACES the
// ambient environment (the preparation builds cmd.Env from exactly this, plus the credential-helper
// PATH), so a malicious inherited GIT_CONFIG_* / HOME / netrc cannot inject a credential helper or a
// command: system and global config are ignored, the terminal never prompts, and the transport
// allowlist excludes ext::. homeDir is an empty directory the preparation owns.
func hardenedEnv(homeDir string) []string {
	return []string{
		"GIT_CONFIG_NOSYSTEM=1",                      // ignore /etc/gitconfig
		"GIT_CONFIG_GLOBAL=/dev/null",                // ignore ~/.gitconfig even if HOME leaks
		"GIT_CONFIG_SYSTEM=/dev/null",                // belt-and-suspenders for GIT_CONFIG_NOSYSTEM
		"HOME=" + homeDir,                            // controlled empty HOME: no ~/.gitconfig, ~/.netrc
		"GIT_TERMINAL_PROMPT=0",                      // never prompt for credentials
		"GIT_ASKPASS=",                               // no askpass program
		"GIT_ALLOW_PROTOCOL=file:git:http:https:ssh", // allowlist transports; ext:: excluded
		"GIT_LFS_SKIP_SMUDGE=1",                      // LFS smudge always skipped (fail-closed; AllowLFS wiring deferred, §30.4)
		"PATH=" + os.Getenv("PATH"),                  // git must find itself and its credential helper
	}
}

// validateSubmoduleURL rejects a submodule URL whose transport is not on the policy allowlist. The
// default allowlist excludes ext:: (arbitrary-command RCE) and file:// (local-path escape), the two
// classic untrusted-submodule vectors (§30.4 "submodule URLs are validated against policy"). It is
// a pure check: given the same URL and policy it always decides the same way.
func (p Policy) validateSubmoduleURL(rawURL string) error {
	trimmed := strings.TrimSpace(rawURL)
	if trimmed == "" {
		return fmt.Errorf("empty submodule url")
	}
	// Relative submodule URLs (./ or ../) resolve against the parent remote's transport, which is
	// already the policy-approved clone URL — they carry no new transport, so they are allowed.
	if strings.HasPrefix(trimmed, "./") || strings.HasPrefix(trimmed, "../") {
		return nil
	}
	scheme := submoduleScheme(trimmed)
	if scheme == "" {
		return fmt.Errorf("submodule url has no recognizable transport: %s", trimmed)
	}
	allowed := p.AllowedSubmoduleSchemes
	if len(allowed) == 0 {
		allowed = defaultSubmoduleSchemes
	}
	for _, a := range allowed {
		if scheme == a {
			return nil
		}
	}
	return fmt.Errorf("submodule transport %q not permitted by policy", scheme)
}

// submoduleScheme extracts the transport of a submodule URL, handling the scp-like SSH shorthand
// (user@host:path) Git accepts. It lower-cases the scheme so "EXT::" cannot slip past a case check.
func submoduleScheme(raw string) string {
	if i := strings.Index(raw, "::"); i >= 0 {
		// transport helper form, e.g. ext::sh -c "...". The scheme is everything before "::".
		return strings.ToLower(raw[:i])
	}
	if i := strings.Index(raw, "://"); i >= 0 {
		return strings.ToLower(raw[:i])
	}
	// scp-like shorthand git@github.com:org/repo — SSH, no scheme prefix. Require a user@host:path
	// shape so a bare "foo:bar" is not mistaken for it.
	if at := strings.Index(raw, "@"); at > 0 {
		if colon := strings.Index(raw[at:], ":"); colon > 0 {
			return "ssh"
		}
	}
	return ""
}

// checkWorktreeContainment walks the prepared worktree and records a Finding for every symlink that
// escapes the repository root and every case-insensitive path collision (spec §30.4 "symlink and
// case-collision behavior is checked for the target filesystem"). It never follows a link; it only
// resolves the target lexically against root. It is defense-in-depth beside core.symlinks handling:
// a symlink whose target leaves the tree is recorded, so a later file tool cannot be tricked into
// writing outside the workspace through it.
func checkWorktreeContainment(root string) ([]Finding, error) {
	var findings []Finding
	seen := map[string]string{} // lower-cased rel path -> first real rel path, for collision detection
	walkErr := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, relErr := filepath.Rel(root, path)
		if relErr != nil || rel == "." {
			return nil
		}
		if strings.HasPrefix(rel, ".git"+string(os.PathSeparator)) || rel == ".git" {
			return nil // the repo's own metadata is not untrusted worktree content
		}
		lower := strings.ToLower(rel)
		if first, ok := seen[lower]; ok && first != rel {
			findings = append(findings, Finding{Kind: "case_collision", Path: rel, Detail: "collides with " + first})
		} else {
			seen[lower] = rel
		}
		if d.Type()&fs.ModeSymlink != 0 {
			if escapes, target := symlinkEscapes(root, path); escapes {
				findings = append(findings, Finding{Kind: "symlink_escape", Path: rel, Detail: "target leaves workspace: " + target})
			}
		}
		return nil
	})
	if walkErr != nil {
		return findings, fmt.Errorf("scan worktree containment: %w", walkErr)
	}
	return findings, nil
}

// symlinkEscapes reports whether the symlink at linkPath resolves outside root. A relative target is
// resolved against the link's directory; an absolute target escapes unless it stays under root.
func symlinkEscapes(root, linkPath string) (bool, string) {
	target, err := os.Readlink(linkPath)
	if err != nil {
		return false, ""
	}
	resolved := target
	if !filepath.IsAbs(resolved) {
		resolved = filepath.Join(filepath.Dir(linkPath), target)
	}
	resolved = filepath.Clean(resolved)
	rootClean := filepath.Clean(root)
	if resolved == rootClean || strings.HasPrefix(resolved, rootClean+string(os.PathSeparator)) {
		return false, ""
	}
	return true, target
}
