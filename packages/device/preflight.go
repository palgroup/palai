package device

import (
	"fmt"
	"os"
	"path/filepath"
)

// PreflightWorkspaceRoot reports whether this machine can actually hold a session's files, and names the
// reason when it cannot.
//
// ‼️ IT EXISTS BECAUSE `user` MODE WAS CLAIMED UNCONDITIONALLY ON DARWIN. Measure's own comment gives the
// reasoning — "`user` mode needs nothing INSTALLED — it IS the login account this process is already
// running as" — and that is true and incomplete: it needs nothing installed, but it needs somewhere to
// WRITE. A Mac whose workspace root cannot be created (a home the agent does not own, a path under a
// volume that is not mounted, a directory an operator made read-only) measured `user`, enrolled, and
// became ready capacity. Every session placed on it then failed at its first file write — after the
// placement, after the lease, on a machine the panel showed as healthy. DoD 9's sentence is that a
// machine which cannot execute never appears as ready capacity, and this is the half of it that no
// installed component can answer for.
//
// It CREATES the directory rather than only testing it. That is the operation the agent will perform
// anyway on its first lease, so testing anything weaker — os.Stat, a permission bit — would be testing a
// different question than the one that matters, and permission bits do not decide the answer on their own
// (an ACL, a read-only mount and a full disk all defeat a mode check that says yes).
func PreflightWorkspaceRoot(root string) error {
	if root == "" {
		return fmt.Errorf("this machine has no workspace root, so a leased session would have nowhere to put its files")
	}
	if err := os.MkdirAll(root, 0o700); err != nil {
		return fmt.Errorf("workspace root %s cannot be created: %w", root, err)
	}
	// A directory that exists is not a directory that accepts bytes. The probe is written and removed, so
	// nothing survives a preflight — a leftover file is a file some later sweep has to reason about.
	probe := filepath.Join(root, ".palai-preflight")
	if err := os.WriteFile(probe, []byte("preflight"), 0o600); err != nil {
		return fmt.Errorf("workspace root %s is not writable by this agent: %w", root, err)
	}
	if err := os.Remove(probe); err != nil {
		return fmt.Errorf("workspace root %s would not release a file this agent had just written: %w", root, err)
	}
	return nil
}
