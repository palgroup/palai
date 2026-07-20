package repositories

import (
	"fmt"
	"regexp"
	"strings"
)

// refPattern is the strict allowlist for a caller-supplied Git ref/branch (spec §30.4 untrusted
// input): letters, digits, and . _ / - only. rejectFlagShaped handles the leading-"-" case;
// validateRef adds the no-".." rule (a refspec escape) on top.
var refPattern = regexp.MustCompile(`^[A-Za-z0-9._/-]+$`)

// rejectFlagShaped refuses a caller-supplied Git POSITIONAL that begins with "-" — the argument-
// injection vector where git reparses e.g. "--upload-pack=<cmd>" as an option (arbitrary command)
// or "--config=..." as a config override (spec §30.4 untrusted-input territory). Every positional
// reaching git from an untrusted source (clone URL, ref, branch, base, worktree path) passes here,
// and the git invocations additionally carry an explicit "--" end-of-options separator where git
// accepts one (fetch/worktree-add/merge). This is the single shared guard for prepare and worktree.
func rejectFlagShaped(kind, value string) error {
	if strings.HasPrefix(value, "-") {
		return fmt.Errorf("%s %q is flag-shaped (leading '-'): refused to prevent git argument injection", kind, value)
	}
	return nil
}

// rejectGitPositionals refuses any of the named positionals that is flag-shaped (leading "-"). It is
// the multi-value form of rejectFlagShaped for a call site that passes several positionals to git.
func rejectGitPositionals(byKind map[string]string) error {
	for kind, value := range byKind {
		if err := rejectFlagShaped(kind, value); err != nil {
			return err
		}
	}
	return nil
}

// validateRef enforces the strict ref allowlist (spec §30.4): non-flag-shaped, no "..", allowlisted
// characters only. Empty is rejected — the caller resolves the default branch before validating.
func validateRef(ref string) error {
	if err := rejectFlagShaped("ref", ref); err != nil {
		return err
	}
	if ref == "" || strings.Contains(ref, "..") || !refPattern.MatchString(ref) {
		return fmt.Errorf("ref %q is not an allowed git ref (allowlist [A-Za-z0-9._/-], no '..')", ref)
	}
	return nil
}
