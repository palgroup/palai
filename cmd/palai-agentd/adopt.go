package main

import (
	"context"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/palgroup/palai/packages/macagent"
)

// slotRootMode is what one slot's directory is left as: the control plane that CREATED it keeps it, the
// slot's own group may READ AND WRITE, and nobody else has anything at all.
//
// ‼️ THE GROUP NEEDS w, AND THAT IS THE SOCKET. The session worker binds macagent.WorkerSocketName in
// this directory — it has to be here, because the plane reaches it by OWNERSHIP and can never join the
// account's group (a process's groups are fixed at exec). Binding is a write, so 0710 produced a worker
// that could not listen. Everything under this directory belongs to the one session holding the slot,
// so there is nothing here the group may see that is not already its own. The `1` that used to sit in the
// last position was world-traversable, which on a Mac holding two tenants means tenant B can walk into
// tenant A's slot and reach any path it can guess; the per-slot group makes 0 expressible there.
const slotRootMode = 0o770

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
	gid, err := macagent.SlotGID(slot)
	if err != nil {
		return responseFor(err)
	}
	dir := filepath.Join(s.AllocationRoot, SlotDirName(slot))
	// The directory is created when it is missing rather than refused. A slot whose first session has
	// not provisioned anything yet has no directory, and "adopt what is not there" is the state the
	// caller asked for — the same idempotence create and delete already answer with.
	if err := os.MkdirAll(dir, slotRootMode); err != nil {
		return macagent.Err(macagent.ClassInternal, fmt.Sprintf("create %s: %v", dir, err))
	}
	if err := filepath.WalkDir(dir, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		// ‼️ THE SLOT ROOT ITSELF IS NOT GIVEN AWAY, and that is the boundary this whole verb draws.
		// The RUNNER creates each allocation directory inside it — that is the process that opens a
		// workspace on this machine — so a root handed to the session account locks the runner out of
		// its own container on the very next attempt: `mkdir …/slot-02/alloc_…: permission denied`,
		// measured on a real Mac on 2026-08-08, on a retry of the run that had just adopted.
		//
		// So the root stays with whoever made it, traversable, and everything INSIDE it becomes the
		// account's. The container is the runner's; the contents are the tenant's.
		if path == dir {
			// The root's GROUP still moves — that is what lets this slot's account traverse into it —
			// but its OWNER does not, which is the distinction the paragraph above is about.
			if err := os.Lchown(path, -1, gid); err != nil {
				return err
			}
			return os.Chmod(path, slotRootMode)
		}
		// ‼️ THE OWNER IS LEFT ALONE AND THE GROUP IS THE BOUNDARY. This tree's tools run CONTROL-PLANE
		// side against the same host allocation, so the control plane's uid must keep read and write on
		// the tree — an allocation owned by the session account answered "permission denied" to the file
		// tool, with the agent reporting it in its own words on 2026-08-08.
		//
		// So: owner unchanged, group = THIS SLOT'S OWN group, mode 0770. Both halves work and no other
		// tenant is in that group — which is why the group is per-slot and not the shared AccountGID
		// every session account carries.
		if err := os.Lchown(path, -1, gid); err != nil {
			return err
		}
		// ‼️ THE MODES ARE SET HERE TOO, AND OWNERSHIP ALONE WOULD NOT HAVE BEEN A BOUNDARY. The control
		// plane clones as itself under whatever umask it happens to have, so a tree that was only
		// chowned keeps 0755 directories and 0644 files — readable by every other session account on
		// this Mac, which is the one thing per-session uids exist to prevent.
		//
		// The SLOT ROOT is the exception and it is 0711: the runner has to resolve a path through it to
		// open the allocation, and traverse is all that takes. It cannot LIST what is inside, and what
		// is inside is owner-only, so the runner learns nothing by walking through.
		if entry != nil && entry.IsDir() {
			return os.Chmod(path, 0o770)
		}
		// A symlink has no mode of its own worth setting, and following it to chmod the target is the
		// same escape Lchown above refuses to make.
		if entry != nil && entry.Type()&fs.ModeSymlink != 0 {
			return nil
		}
		return os.Chmod(path, 0o660)
	}); err != nil {
		return macagent.Err(macagent.ClassInternal, fmt.Sprintf("give %s to %s: %v", dir, name, err))
	}
	return macagent.OKAccount(macagent.VerbAdopt, name, dir)
}
