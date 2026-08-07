package main

import (
	"context"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/palgroup/palai/packages/macagent"
)

// SlotDirName is the directory one slot's allocations live in, under the allocation root. It is
// derived from the integer on BOTH sides — the control plane places allocations here and the daemon
// adopts here — so the two agree without either telling the other a path.
func SlotDirName(slot int) string { return fmt.Sprintf("slot-%02d", slot) }

// adopt gives one slot's allocation directory to that slot's account.
//
// ONLY uid 0 MAY GIVE A TREE TO ANOTHER uid, which is the whole reason this lives in a root daemon. The
// control plane runs as the operator and clones the repository as itself; the account that must own the
// result is not the one that wrote it, and `lchown` from a non-root process answers "operation not
// permitted".
//
// THE PATH IS DERIVED, NEVER RECEIVED. The caller sends a slot; this function joins it onto the
// allocation root the daemon was INSTALLED with. A path on the wire would let any member of the
// socket's group hand /etc to a session account.
//
// Lchown, NEVER Chown: os.Chown follows a symlink and chowns its TARGET, and the tree being walked is a
// repository a model may already have written into — `ln -s /etc/passwd x` inside it would otherwise
// make this privileged walk hand /etc/passwd to a tenant. filepath.WalkDir does not descend through
// symlinks either, so the walk stays inside the allocation.
func (s *Server) adopt(ctx context.Context, slot int) macagent.Response {
	if err := macagent.ValidSlot(slot); err != nil {
		return responseFor(err)
	}
	if s.AllocationRoot == "" {
		return macagent.Err(macagent.ClassUnsupported,
			"this daemon was installed with no allocation root, so it can adopt nothing — reinstall it with -allocation-root")
	}
	name, err := macagent.AccountName(slot)
	if err != nil {
		return responseFor(err)
	}
	uid, err := macagent.AccountUID(slot)
	if err != nil {
		return responseFor(err)
	}
	dir := filepath.Join(s.AllocationRoot, SlotDirName(slot))
	// The directory is created when it is missing rather than refused. A slot whose first session has
	// not provisioned anything yet has no directory, and "adopt what is not there" is the state the
	// caller asked for — the same idempotence create and delete already answer with.
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return macagent.Err(macagent.ClassInternal, fmt.Sprintf("create %s: %v", dir, err))
	}
	if err := filepath.WalkDir(dir, func(path string, _ fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		return os.Lchown(path, uid, macagent.AccountGID)
	}); err != nil {
		return macagent.Err(macagent.ClassInternal, fmt.Sprintf("give %s to %s: %v", dir, name, err))
	}
	return macagent.OKAccount(macagent.VerbAdopt, name, dir)
}
