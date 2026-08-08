package main

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/palgroup/palai/packages/macagent"
)

// Accounts is the privileged surface: everything palai-agentd can do as root.
//
// ‼️ EVERY METHOD TAKES A SLOT AND NO METHOD TAKES A NAME. That is not a convention, it is the
// security property, and TestTheProtocolCannotExpressAnAccountNameAtAll reads these signatures out of
// the AST to keep it. Add a `name string` parameter here and a compromised caller regains the sentence
// `delete salih`, which the wire protocol went out of its way to make unwriteable.
type Accounts interface {
	Create(ctx context.Context, slot int) (name, home string, err error)
	Delete(ctx context.Context, slot int) (name, home string, err error)
	List(ctx context.Context) ([]int, error)
	// Spawn starts the session worker as slot N's account and answers its pid. It takes a slot and
	// NOTHING ELSE — no program, no argv, no environment — which is the property that makes a `run`
	// verb on a root daemon defensible; see macagent.InstalledWorkerPath.
	Spawn(ctx context.Context, slot int) (name string, pid int, err error)
}

// markerAttr, markerMagic and markerVersion are this daemon's own, and they have to.
//
// THE MARKER IS THE AUTHORITY, NOT THE NAME. Only root can write a dsAttrTypeNative attribute, so a
// record carrying this marker is one root created here — where an account merely CALLED palai-s07 could
// be anybody's. Sharing the format with the shell tooling is what lets an operator's
// `palai agentd` reconciliation clean up after this daemon, and lets this daemon
// delete what the operator's script created. Two namespaces that cannot see each other's accounts would
// leave orphans that neither side is willing to touch.
const (
	markerAttr    = "dsAttrTypeNative:palai_mac_session"
	markerMagic   = "palai-mac-sessions"
	markerVersion = "1"
)

// darwinFoldersRoot is where per-uid caches live. `delete` removes the ones belonging to the uid it is
// retiring, because measurement (palai-cloud docs/measurements/faz-a5-residue.md §7) found uid 701's
// bucket still on disk with its `0/ C/ T/ X/` subdirectories while `id 701` answered `no such user`.
// Deleting the account does not delete this, and the shader cache and simulator residue that matter
// most live here rather than in the home directory.
const darwinFoldersRoot = "/private/var/folders"

// mdsMessagesRoot is the metadata server's per-uid message directory, and it is here because ONE
// SWEEP LOOKS IN ONE DIRECTION. Walking /private/var/folders finds a bucket that exists and has no
// owner; it cannot find a family that lives somewhere else entirely. The other direction is a list —
// "what does a uid own outside its home?" — and asking it on 2026-08-05 turned up SEVEN paths for the
// orphan uid 701, six of them the darwin bucket above and the seventh this:
//
//	/usr/bin/find /private/var/db /private/var/folders /Library /Users/Shared /private/tmp -xdev -uid 701
//	  -> /private/var/db/mds/messages/701/se_SecurityMessages   (32768 B, -rw-------, Aug 3 20:34)
//	     /private/var/folders/dk/m9lnxdpx6c1fp1s43s2g75n40000nx  (+ its 0/ C/ T/ X/)
//	id 701 -> no such user
//
// It accumulates the same way: `ls /private/var/db/mds/messages/` answered 501, 503, 504 and 701 —
// four uids on a machine with three accounts.
//
// ‼️ WHAT IT CONTAINS WAS NOT MEASURED. The file is mode 600 owned by a uid that no longer exists, so
// this process could not read it (`cannot open: Permission denied`). It is removed because it is
// per-uid state that outlives its uid, which is enough; a claim about what is inside it would be a
// claim nobody here is in a position to make.
//
// THE SHAPE DIFFERS FROM THE BUCKET SWEEP AND THE TWO ARE KEPT SEPARATE FOR THAT REASON. A bucket is
// found by OWNERSHIP because its name is a hash; this directory is found by NAME because its name IS
// the uid — and the directory itself is root-owned, so an ownership check here would refuse the very
// thing it must remove. Conflating them would hide that.
const mdsMessagesRoot = "/private/var/db/mds/messages"

// SysadminctlAccounts is the real implementation: it drives macOS directory services directly.
//
// NO SHELL, EVER. Every call below is an argv, not a command line, so there is no word splitting, no
// glob, and no `;`. That is the second half of the argument in scripts/ops/palai-session-account: that
// wrapper narrowed a sudoers entry down to one verb and one two-digit index precisely because a binary
// reachable through sudo is only as narrow as the arguments it will accept. Here the caller supplies an
// integer and nothing else reaches an exec.
type SysadminctlAccounts struct {
	// run is the exec boundary and a field so tests can drive every path without root. It takes an
	// argv, never a command string.
	run func(ctx context.Context, name string, args ...string) (string, error)
	// spawn is the OTHER exec boundary, and it is separate from run for two reasons that are both about
	// what it does differently rather than about testing: it STARTS a process and does not wait for its
	// output, and it is the only place in this daemon that spends a uid other than root's. A field for
	// the same reason run is one — every machine an agent works on is unprivileged, and a boundary
	// reachable only with root is a boundary no test ever sees.
	spawn func(ctx context.Context, w sessionWorker) (int, error)
	// workerPath is macagent.InstalledWorkerPath in production, and a temporary file in tests. It is a
	// field and NOT a caller-reachable input: see that constant for why the difference is the whole
	// safety argument for the spawn verb.
	workerPath string
	// foldersRoot is darwinFoldersRoot in production, and a temporary directory in tests. A test that
	// pointed the real path at itself would be a test that can delete the operator's caches.
	foldersRoot string
	// messagesRoot is mdsMessagesRoot in production, and a temporary directory in tests, for the same
	// reason foldersRoot is a field: this one removes a directory named after a uid, and the operator's
	// own uid names one too.
	messagesRoot string
	// hostUUID identifies this Mac, binding a marker to it.
	hostUUID func(ctx context.Context) (string, error)
	now      func() time.Time
	// usersRoot is "/Users" in production and a temporary directory in tests. It is a field for the same
	// reason foldersRoot is: the alternative is a test that creates and deletes real home directories on
	// the machine running it. macagent.HomeDir stays the ONE producer of the path directory services is
	// told about; this only relocates the filesystem side, and TestTheHomeOnDiskIsTheHomeInTheDirectory
	// pins that the two agree in production.
	usersRoot string
	// chown is os.Lchown in production — NEVER os.Chown, which follows a symlink and would chown its
	// TARGET; the trees this daemon walks are ones a model may already have written into. Only uid 0 may give a file to another uid, so this is the one
	// filesystem call in this file a test cannot make — a field, for the same reason `run` is.
	chown func(path string, uid, gid int) error
	// sleep is time.Sleep in production. It is a field so a test can drive the shipped settle without
	// spending it, which is the same reason macagent.Prober carries one.
	sleep func(time.Duration)
	// goos is runtime.GOOS in production. It is a field because the refusal on a non-Mac is behaviour
	// worth testing, and CI runs on Linux.
	goos string
	// allocationRoot is the directory this machine opens session workspaces in, and it is here for ONE
	// purpose: its owner is the control plane, and that is the only honest way this daemon has to learn
	// who the control plane is. Nothing arrives on the wire that could say so — the socket carries a
	// slot and nothing else — and a name in a flag would be a name an operator could get wrong. The
	// directory the control plane made is the control plane's own statement about its identity.
	allocationRoot string
	// ownerOf answers which uid owns a path, and lookupUser turns a uid into a name. Both are fields so
	// that a test can pin what the group ends up containing without a real account existing.
	ownerOf    func(path string) (int, error)
	lookupUser func(uid int) (string, error)
}

// NewSysadminctlAccounts wires the production implementation.
func NewSysadminctlAccounts(allocationRoot string) *SysadminctlAccounts {
	return &SysadminctlAccounts{
		allocationRoot: allocationRoot,
		run:            runArgv,
		spawn:          startWorker,
		workerPath:     macagent.InstalledWorkerPath,
		foldersRoot:    darwinFoldersRoot,
		messagesRoot:   mdsMessagesRoot,
		hostUUID:       hostUUID,
		now:            time.Now,
		goos:           runtime.GOOS,
		ownerOf:        ownerOf,
		lookupUser:     lookupUserName,
		usersRoot:      darwinUsersRoot,
		chown:          os.Lchown,
		sleep:          time.Sleep,
	}
}

// ownerOf reads the uid that owns a path.
func ownerOf(path string) (int, error) {
	fi, err := os.Stat(path)
	if err != nil {
		return 0, err
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, fmt.Errorf("%s carries no unix ownership", path)
	}
	return int(st.Uid), nil
}

// lookupUserName turns a uid into the name directory services knows it by, which is what dseditgroup
// takes. The uid is what a directory's ownership carries; the name is what group membership is written
// in, and the two are only the same fact on a machine that agrees with itself.
func lookupUserName(uid int) (string, error) {
	u, err := user.LookupId(strconv.Itoa(uid))
	if err != nil {
		return "", err
	}
	return u.Username, nil
}

// darwinUsersRoot is where macOS keeps local home directories, and macagent.HomeDir formats a path
// under it. The two are pinned equal by a test rather than derived from each other, because one is what
// directory services is TOLD and the other is what this process WRITES.
const darwinUsersRoot = "/Users"

// accountReapSettle is how long a delete waits between signalling an account's processes and removing
// its record. Short, because the only process there by construction is a session worker, and a worker
// that has been asked to stop closes a listener and returns.
const accountReapSettle = 500 * time.Millisecond

func runArgv(ctx context.Context, name string, args ...string) (string, error) {
	out, err := exec.CommandContext(ctx, name, args...).CombinedOutput()
	if err != nil {
		return string(out), fmt.Errorf("%s: %w", name, err)
	}
	return string(out), nil
}

// hostUUID reads this Mac's IOPlatformUUID, the same identity the marker mints markers against.
func hostUUID(ctx context.Context) (string, error) {
	out, err := runArgv(ctx, "ioreg", "-rd1", "-c", "IOPlatformExpertDevice")
	if err != nil {
		return "", err
	}
	for _, line := range strings.Split(out, "\n") {
		if !strings.Contains(line, "IOPlatformUUID") {
			continue
		}
		parts := strings.Split(line, `"`)
		if len(parts) >= 4 {
			return parts[len(parts)-2], nil
		}
	}
	return "", fmt.Errorf("no IOPlatformUUID in ioreg output")
}

func (a *SysadminctlAccounts) requireMac() error {
	if a.goos != "darwin" {
		return macagent.Errorf(macagent.ClassUnsupported, "session accounts exist only on macOS, this is %s", a.goos)
	}
	return nil
}

// ensureSlotGroup makes SLOT N's OWN group, idempotently, and puts nothing in it but that slot.
//
// ‼️ THIS GROUP IS THE BOUNDARY, NOT BOOKKEEPING, AND THERE IS ONE PER SLOT. A macOS home directory is
// `drwxr-x---` owned by `<operator>:staff` and a local account created without `-GID` lands in `staff`,
// so a session account in the default group reads the operator's entire home by the group bit. A single
// fleet-wide group fixed that and created the mirror image: the workspace a session must reach through
// its group becomes reachable by every OTHER session's account too. Two customers on one Mac is a case
// this fleet serves, so the group is exactly as narrow as the thing it protects. See
// macagent.SlotGroupName.
//
// IT REFUSES RATHER THAN ADAPTS, on both collisions:
//
//   - the NAME exists with a different gid — that group is somebody else's, and creating an account in
//     it would hand them whatever it can reach;
//   - the GID is held under a different name — same hazard from the other direction, and `sysadminctl
//     -GID` names the number, not the record.
//
// Either way the honest answer is to stop: an account created into the wrong group is a boundary that
// reports success and does not exist.
//
// AN EXISTING GROUP OF OUR OWN IS NOT A COLLISION. Slots are reused — an account is destroyed when a
// session goes idle and slot 07 is minted again for the next one — so finding palai-s07-grp at gid 807
// is the ordinary second create, and refusing it would make a machine usable exactly once per slot.
func (a *SysadminctlAccounts) ensureSlotGroup(ctx context.Context, slot int) error {
	group, err := macagent.SlotGroupName(slot)
	if err != nil {
		return err
	}
	gid, err := macagent.SlotGID(slot)
	if err != nil {
		return err
	}
	path := "/Groups/" + group

	if out, err := a.run(ctx, "dscl", ".", "-read", path, "PrimaryGroupID"); err == nil {
		got := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(out), "PrimaryGroupID:"))
		if got == strconv.Itoa(gid) {
			return nil
		}
		return macagent.Errorf(macagent.ClassInternal,
			"group %s already exists with gid %s, not %d: it is not the one this daemon owns, and creating "+
				"a session account in it would give that session whatever that group can reach",
			group, got, gid)
	}

	// The gid may be held under another name even when the name is free. `-search` answers the records
	// that hold it; anything at all here is a collision.
	if out, err := a.run(ctx, "dscl", ".", "-search", "/Groups", "PrimaryGroupID", strconv.Itoa(gid)); err == nil {
		if holder := strings.TrimSpace(out); holder != "" {
			return macagent.Errorf(macagent.ClassInternal,
				"gid %d is already held on this machine (%s), so it cannot be %s: this slot would share a "+
					"group with whatever holds it",
				gid, strings.Fields(holder)[0], group)
		}
	}

	// THREE ATTRIBUTES AND NO PASSWORD. A group password authenticates nothing on modern macOS, and the
	// `*` placeholder Apple's own service groups carry is a shell metacharacter — which this daemon's
	// TestCreateRunsArgvAndNeverACommandLine forbids in ANY argument, and rightly: the rule is that no
	// privileged call may contain something a shell would read, and an exception for a harmless one is how
	// the rule stops being checkable. The attribute is not needed, so it is not written.
	for _, args := range [][]string{
		{".", "-create", path},
		{".", "-create", path, "PrimaryGroupID", strconv.Itoa(gid)},
		{".", "-create", path, "RealName", fmt.Sprintf("Palai session slot %02d", slot)},
	} {
		if out, err := a.run(ctx, "dscl", args...); err != nil {
			return macagent.Errorf(macagent.ClassInternal,
				"creating group %s failed: %v: %s", group, err, strings.TrimSpace(out))
		}
	}
	return a.joinControlPlane(ctx, group)
}

// joinControlPlane puts the control plane's own user in the slot's group.
//
// ‼️ THE GROUP HAS TO CARRY BOTH PRINCIPALS OR IT ONLY WORKS IN ONE DIRECTION, and the direction it
// would work in is the one that matters least. The control plane clones the repository and runs the file
// tools, so it owns what it writes and reaches it as the owner — the group is what lets the SESSION read
// it. But the session's own commands write files too, and those are owned by the session account with
// the session's group; a control plane outside that group would then be unable to read back the work the
// tenant just did, which is the diff a human approves. Both write, so both are members.
//
// IT IS THE OWNER OF THE ALLOCATION ROOT, NOT A CONFIGURED NAME. See the field comment on
// allocationRoot: a directory the control plane created is the control plane's own statement about who
// it is, and it is the only one this daemon can read without being told.
//
// A DAEMON WITH NO ALLOCATION ROOT SKIPS THIS RATHER THAN FAILING. Such a daemon cannot adopt anything
// either — adopt refuses outright — so there is no tree for the two principals to share and no claim
// being quietly weakened.
func (a *SysadminctlAccounts) joinControlPlane(ctx context.Context, group string) error {
	if a.allocationRoot == "" {
		return nil
	}
	uid, err := a.ownerOf(a.allocationRoot)
	if err != nil {
		return macagent.Errorf(macagent.ClassInternal,
			"reading who owns %s failed, so this daemon cannot tell which user is the control plane: %v", a.allocationRoot, err)
	}
	// ‼️ ROOT IS NOT PUT IN A TENANT'S GROUP, and reaching this branch means the allocation root is
	// root-owned — which is a real misinstall rather than a state to paper over. root already reaches
	// every path on the machine, so adding it would be a no-op that hides the misinstall; refusing says
	// the control plane is not the process that made this directory.
	if uid == 0 {
		return macagent.Errorf(macagent.ClassInternal,
			"%s is owned by root, so the control plane cannot be derived from it; it must be created by the "+
				"user the control plane runs as", a.allocationRoot)
	}
	name, err := a.lookupUser(uid)
	if err != nil {
		return macagent.Errorf(macagent.ClassInternal, "uid %d owns %s but has no name in directory services: %v", uid, a.allocationRoot, err)
	}
	if out, err := a.run(ctx, "dseditgroup", "-o", "edit", "-a", name, "-t", "user", group); err != nil {
		return macagent.Errorf(macagent.ClassInternal,
			"adding %s to %s failed, so the control plane could not read back what the session writes: %v: %s",
			name, group, err, strings.TrimSpace(out))
	}
	return nil
}

// removeSlotGroup takes the group away with the account, so a destroyed session leaves no record that a
// later, unrelated account could be created into. It is best-effort in exactly one direction: a group
// that is already gone is the state the caller asked for.
func (a *SysadminctlAccounts) removeSlotGroup(ctx context.Context, slot int) error {
	group, err := macagent.SlotGroupName(slot)
	if err != nil {
		return err
	}
	path := "/Groups/" + group
	if _, err := a.run(ctx, "dscl", ".", "-read", path, "PrimaryGroupID"); err != nil {
		return nil
	}
	if out, err := a.run(ctx, "dscl", ".", "-delete", path); err != nil {
		return macagent.Errorf(macagent.ClassInternal, "deleting group %s failed: %v: %s", group, err, strings.TrimSpace(out))
	}
	return nil
}

// readAttr reads one attribute off one directory-services record and returns its value with the label
// stripped. A missing record is an error, which is what dscl itself answers.
func (a *SysadminctlAccounts) readAttr(ctx context.Context, path, attr string) (string, error) {
	out, err := a.run(ctx, "dscl", ".", "-read", path, attr)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(out), attr+":")), nil
}

// ensureHome leaves slot N's home in the state a fresh create would: a directory the account owns, a
// TMPDIR inside it, and mode 0710.
//
// ‼️ IT RUNS ON THE "ALREADY EXISTS" PATH TOO, AND NOT RUNNING THERE COST THE WHOLE BOUNDARY. Measured
// on this Mac on 2026-08-08: palai-s01's record survived while its home did not, so create took its
// idempotent branch, aligned the group, and reported ClassExists — which the caller treats as success.
// The spawn that followed answered
//
//	fork/exec /usr/local/libexec/palai-session-worker: no such file or directory
//
// naming a file that was present, executable and root-owned. fork/exec reports ENOENT for a missing
// WORKING DIRECTORY too, and the worker's is the account's home. Every session then ran its commands as
// the control plane's own uid with every layer above reporting success; the live chain said `ran as
// salih` while a session account sat there unused.
//
// orphanStale is the one thing the two paths do differently. A fresh create must never inherit a
// directory left by a previous tenant of the same slot; the exists path is the SAME account still
// holding the slot — a control plane that restarted mid-session — and renaming its home there would
// take the running session's own files away from it.
func (a *SysadminctlAccounts) ensureHome(name string, uid, gid int, orphanStale bool) error {
	// ‼️ THE HOME IS MADE HERE AND `createhomedir` IS NOT USED, AND THE REASON WAS MEASURED ON THIS MAC
	// ON 2026-08-08. createhomedir populates from the user template — Music, Pictures, Desktop,
	// Library/Containers — and macOS puts those exact paths behind TCC. `sudo rm -rf /Users/palai-s05`
	// answered `Operation not permitted` on eight of them AS ROOT, and the home survived. An account
	// whose home cannot be removed is an account that cannot be DESTROYED, which is the entire idle half
	// of this lifecycle: created when a session starts, destroyed when it goes quiet.
	//
	// A SESSION ACCOUNT NEEDS NONE OF THAT TEMPLATE. It never logs in, never opens a GUI app and never
	// has a Desktop. It needs a directory it owns and a TMPDIR, so those are the two things made — and
	// what is never created is never protected.
	diskHome := filepath.Join(a.usersRoot, name)

	// ‼️ A HOME THAT IS ALREADY THERE IS MOVED ASIDE AND NEVER INHERITED. Slots are reused and a
	// directory is not: one left by a crashed destroy, by a machine that outlived a scheme change, or by
	// the template homes this daemon used to create and could not remove, would arrive owned by the NEW
	// account with the OLD session's files in it. That is a cross-tenant read with every layer reporting
	// success, which is the one outcome per-session accounts exist to prevent.
	//
	// ‼️ RENAME FIRST, THEN DELETE, AND THE ORDER IS A CORRECTION OF A CEILING THIS FILE OVERSTATED. The
	// sentence here said a rename "is a write to /Users, which root may always do". Measured the same
	// night, as root:
	//
	//	rename /Users/palai-s03 /Users/palai-s03.orphaned-…: operation not permitted
	//
	// macOS protects a home directory against being MOVED as well as against having its template
	// subdirectories removed — the template homes this daemon used to create are behind TCC on both
	// verbs. Overstating a ceiling is as wrong as understating one: it left a slot that could never be
	// given a clean home, and the refusal took the whole session down with it.
	//
	// So both are tried. A rename is preferred because it keeps the previous tenant's bytes where an
	// operator can see them; a delete is the fallback for the homes this daemon makes itself, which
	// carry no protected subdirectory and remove cleanly.
	if _, err := os.Lstat(diskHome); err == nil && orphanStale {
		orphan := fmt.Sprintf("%s.orphaned-%d", diskHome, a.now().UTC().Unix())
		renameErr := os.Rename(diskHome, orphan)
		if renameErr != nil {
			if rmErr := os.RemoveAll(diskHome); rmErr != nil {
				return macagent.Errorf(macagent.ClassRefused,
					"%s is a directory this daemon can neither move aside (%v) nor remove (%v), so this slot cannot be "+
						"given a clean home and must not be handed to a session; it is a macOS-protected home left by an "+
						"earlier install — remove it with Full Disk Access granted, or leave this slot out of service",
					diskHome, renameErr, rmErr)
			}
		}
	}
	if err := os.MkdirAll(diskHome, 0o710); err != nil {
		return macagent.Errorf(macagent.ClassInternal, "creating %s failed: %v", diskHome, err)
	}
	if err := a.chown(diskHome, uid, gid); err != nil {
		return macagent.Errorf(macagent.ClassInternal, "giving %s to %s: %v", diskHome, name, err)
	}
	// TMPDIR, because the session worker's environment names $HOME/tmp and a TMPDIR that does not exist
	// is a toolchain that fails somewhere far from here.
	tmp := filepath.Join(diskHome, "tmp")
	if err := os.MkdirAll(tmp, 0o700); err != nil {
		return macagent.Errorf(macagent.ClassInternal, "creating %s failed: %v", tmp, err)
	}
	if err := a.chown(tmp, uid, gid); err != nil {
		return macagent.Errorf(macagent.ClassInternal, "giving %s to %s: %v", tmp, name, err)
	}
	// ‼️ x WITHOUT r, AND THE ONE BIT IS LOAD-BEARING. The group is this slot's own and its other member
	// is the control plane, which has to reach exactly ONE path inside: the session worker's socket
	// (macagent.WorkerSocket). 0700 makes that socket unreachable, which is a machine that mints
	// accounts and cannot run a single command as one. x lets it traverse to a path it already knows; no
	// r means it cannot list the home, and no other tenant is in the group at all. MkdirAll applies the
	// process umask, so the mode is SET rather than assumed.
	if err := os.Chmod(diskHome, 0o710); err != nil {
		return macagent.Errorf(macagent.ClassInternal, "tightening %s to 0710 failed: %v", diskHome, err)
	}
	return nil
}

// alignPrimaryGroup makes an EXISTING account's primary group the one this daemon allocates for its
// slot, and it is a repair rather than a check because the alternative is a refusal a session cannot act
// on.
//
// THE CASE IS A MACHINE THAT OUTLIVED A SCHEME CHANGE. Accounts on this Mac were created in one shared
// group until 2026-08-08; the same records, with the same names and uids, now belong in a group of their
// own. An upgrade that only changed what NEW accounts get would leave every existing slot in the old
// group while adopt sets its workspace to the new one — a session that owns nothing it can read, with
// every layer reporting success.
func (a *SysadminctlAccounts) alignPrimaryGroup(ctx context.Context, name string, slot int) error {
	gid, err := macagent.SlotGID(slot)
	if err != nil {
		return err
	}
	path := "/Users/" + name
	got, err := a.readAttr(ctx, path, "PrimaryGroupID")
	if err != nil {
		return macagent.Errorf(macagent.ClassInternal, "reading the primary group of %s failed: %v", name, err)
	}
	if got == strconv.Itoa(gid) {
		return nil
	}
	if out, err := a.run(ctx, "dscl", ".", "-create", path, "PrimaryGroupID", strconv.Itoa(gid)); err != nil {
		return macagent.Errorf(macagent.ClassInternal,
			"%s exists in another group and could not be moved to %d, so its session would not be able to read its own workspace: %v: %s",
			name, gid, err, strings.TrimSpace(out))
	}
	return nil
}

// Create makes slot N's account, its home, and the marker that makes it deletable later.
func (a *SysadminctlAccounts) Create(ctx context.Context, slot int) (string, string, error) {
	if err := a.requireMac(); err != nil {
		return "", "", err
	}
	// Derived, never received. AccountName is the only producer of a name in this system.
	name, err := macagent.AccountName(slot)
	if err != nil {
		return "", "", err
	}
	uid, err := macagent.AccountUID(slot)
	if err != nil {
		return "", "", err
	}
	home, err := macagent.HomeDir(slot)
	if err != nil {
		return "", "", err
	}

	if rec, err := a.record(ctx, name); err == nil && rec.exists {
		// ‼️ "ALREADY EXISTS" IS SUCCESS TO THE CALLER, so this branch has to leave the account in the
		// state a fresh create would have. execution.SlotAccounts treats ClassExists as the outcome it
		// wanted — deliberately, because a caller that timed out mid-create must not poison the slot — so
		// an account left over from a previous run is one a session is about to USE. Returning here
		// without re-asserting the group is how a slot whose group scheme changed under it, or whose
		// group an operator removed, gets handed to a session that then cannot read its own workspace.
		if err := a.ensureSlotGroup(ctx, slot); err != nil {
			return "", "", err
		}
		if err := a.alignPrimaryGroup(ctx, name, slot); err != nil {
			return "", "", err
		}
		// AND THE HOME, which the group alignment above says nothing about. See ensureHome: a record that
		// outlived its directory is what made every session's commands run as the control plane's uid.
		euid, herr := macagent.AccountUID(slot)
		if herr != nil {
			return "", "", herr
		}
		egid, herr := macagent.SlotGID(slot)
		if herr != nil {
			return "", "", herr
		}
		if err := a.ensureHome(name, euid, egid, false); err != nil {
			return "", "", err
		}
		return "", "", macagent.Errorf(macagent.ClassExists, "%s already exists (uid %d)", name, rec.uid)
	}

	huuid, err := a.hostUUID(ctx)
	if err != nil {
		return "", "", macagent.Errorf(macagent.ClassInternal, "this host has no IOPlatformUUID, so a marker cannot be bound to it: %v", err)
	}

	// THE GROUP IS MADE BEFORE THE ACCOUNT, because `-GID` on a group that does not exist produces a
	// user whose primary group resolves to nothing — a login that cannot open its own home, reported as
	// a successful create. See ensureSlotGroup for why the group is a boundary rather than bookkeeping,
	// and why there is one of them per slot.
	if err := a.ensureSlotGroup(ctx, slot); err != nil {
		return "", "", err
	}
	gid, err := macagent.SlotGID(slot)
	if err != nil {
		return "", "", err
	}

	// NO -password. The account is created without one, so it is reachable by root and by no login
	// path. Passing one would put it in argv, where every user on the box reads it out of `ps` — and
	// on this platform `ps -E` was measured on 2026-08-04 to show a live process's environment to the
	// same uid as well, so neither argv nor the environment is a place for a secret.
	// ‼️ dscl AND NOT `sysadminctl -addUser`, AND THIS WAS MEASURED RATHER THAN PREFERRED. On this Mac on
	// 2026-08-08, five of six accounts carried a uid this daemon never allocated: `-UID 702` through
	// `-UID 706` produced 505, 506, 507, 508 and 509 — macOS's own next-free sequence — while 702..706
	// were all FREE (`dscl . -search /Users UniqueID 702` answered nothing for every one of them).
	// sysadminctl took the flag, ignored it, and exited 0.
	//
	// WHAT THAT COSTS IS NOT COSMETIC. The uid is the boundary: the control plane hands the DERIVED uid
	// to whatever drops privilege, so a session would have dropped to a uid no account holds. And the
	// deletion guard refuses any record whose uid is not the derived one — correctly — so every one of
	// those five accounts became undeletable and its slot was poisoned for the machine's lifetime. The
	// live plane said so in as many words: `destroy slot 05: uid 508 is not the uid 705 this daemon
	// allocates`.
	//
	// dscl writes exactly the attributes it is given and cannot substitute another value. The one thing
	// sysadminctl did for us that this does not is create the home directory, and it did not do that
	// either — see createhomedir below, which was always the thing that made the home.
	for _, attr := range [][]string{
		{"UniqueID", strconv.Itoa(uid)},
		{"PrimaryGroupID", strconv.Itoa(gid)},
		{"UserShell", "/bin/zsh"},
		{"NFSHomeDirectory", home},
		{"RealName", fmt.Sprintf("Palai session s%02d", slot)},
	} {
		args := append([]string{".", "-create", "/Users/" + name}, attr...)
		if out, err := a.run(ctx, "dscl", args...); err != nil {
			return "", "", macagent.Errorf(macagent.ClassInternal, "creating %s failed at %s: %v: %s", name, attr[0], err, strings.TrimSpace(out))
		}
	}

	// AND READ IT BACK, because the whole reason the line above is dscl is that a creator reported success
	// and wrote something else. A verification that trusts the creator it just stopped trusting would be
	// the same defect one layer up.

	if got, rerr := a.readAttr(ctx, "/Users/"+name, "UniqueID"); rerr != nil || got != strconv.Itoa(uid) {
		return "", "", macagent.Errorf(macagent.ClassInternal,
			"created %s but directory services reports UniqueID %q rather than the %d this daemon allocates; the account "+
				"is not the one that was asked for and must not be handed to a session (%v)", name, got, uid, rerr)
	}

	marker := strings.Join([]string{markerMagic, markerVersion, huuid, name, a.now().UTC().Format(time.RFC3339)}, ":")
	if out, err := a.run(ctx, "dscl", ".", "-create", "/Users/"+name, markerAttr, marker); err != nil {
		return "", "", macagent.Errorf(macagent.ClassInternal, "marking %s failed, so it could never be deleted by this daemon: %v: %s", name, err, strings.TrimSpace(out))
	}

	if err := a.ensureHome(name, uid, gid, true); err != nil {
		return "", "", err
	}
	return name, home, nil
}

// Delete removes slot N's account, its home, and the darwin cache bucket its uid owns.
func (a *SysadminctlAccounts) Delete(ctx context.Context, slot int) (string, string, error) {
	if err := a.requireMac(); err != nil {
		return "", "", err
	}
	name, err := macagent.AccountName(slot)
	if err != nil {
		return "", "", err
	}
	home, err := macagent.HomeDir(slot)
	if err != nil {
		return "", "", err
	}
	rec, err := a.record(ctx, name)
	if err != nil {
		return "", "", macagent.Errorf(macagent.ClassInternal, "reading the record for %s failed: %v", name, err)
	}
	if !rec.exists {
		return "", "", macagent.Errorf(macagent.ClassNotFound, "%s has no account", name)
	}
	huuid, err := a.hostUUID(ctx)
	if err != nil {
		return "", "", macagent.Errorf(macagent.ClassInternal, "this host has no IOPlatformUUID, so no marker can be matched against it: %v", err)
	}
	if why := notOurAccountBecause(rec, huuid, a.protectedUIDs()); why != "" {
		return "", "", macagent.Errorf(macagent.ClassRefused, "%s", why)
	}

	// ‼️ dscl AND NOT `sysadminctl -deleteUser -secure`, FOR THE REASON THE CREATOR IS dscl. On this Mac
	// on 2026-08-08 the live plane logged `destroy slot 01: sysadminctl reported success but the record
	// for palai-s01 still exists` — the verification below firing, on a deletion that exited 0 and did
	// nothing. sysadminctl has now been measured lying in both directions on the same machine, and a
	// privileged surface cannot be built on a tool that reports outcomes it did not produce.
	//
	// THE TWO STEPS ARE SEPARATE BECAUSE THEY FAIL SEPARATELY. `-secure` erased the home as part of the
	// same call, so a home that could not be removed and a record that could not be deleted arrived as
	// one opaque exit code. Deleting the record first is also the safer order: a record that is gone
	// cannot be logged into while its home is being removed.
	// ‼️ THE ACCOUNT'S PROCESSES GO FIRST, AND THE SESSION WORKER IS ALWAYS ONE OF THEM. A worker lives
	// exactly as long as the account it is — there is no idle exit, deliberately, because nothing on the
	// control-plane side could start a second one — so at this point there is a process holding this uid
	// by construction. Deleting a record out from under a running process leaves a live uid with no
	// name, which is both a leak and the most likely reading of what was measured on this Mac on
	// 2026-08-08: `sysadminctl reported success but the record for palai-s01 still exists`.
	//
	// SIGTERM AND THEN SIGKILL, because a worker that is mid-command should be allowed to finish writing
	// what it has; and `pkill -u` rather than a pid table for the reason the group check is a dial —
	// the kernel knows which processes hold this uid and this daemon does not.
	// ‼️ THE LAUNCHD DOMAIN GOES FIRST, AND NOT KNOWING THIS COST THE WHOLE DELETE PATH. Measured on this
	// Mac on 2026-08-08: `dscl . -delete /Users/palai-s01` answered eDSPermissionError as ROOT, and
	// `ps -u 701` explained why —
	//
	//	5359  1  /usr/libexec/lsd
	//	5422  1  /usr/sbin/distnoted agent
	//	5426  1  /usr/sbin/cfprefsd agent
	//	5477  1  /usr/libexec/trustd --agent
	//	6079  1  /usr/libexec/secd
	//	…
	//
	// seven processes, every one of them PARENTED BY launchd. The moment ANY process runs as a uid, macOS
	// materialises a per-user launchd domain and starts these agents in it. They are not ours, killing
	// them accomplishes nothing because launchd starts them again, and directory services refuses to
	// remove a record whose domain is live.
	//
	// `bootout` is what tears the domain down. It is spelled `user/<uid>` — the per-user domain, not
	// `gui/<uid>`, which is the one a LOGIN creates and a session account never has. A domain that is
	// already gone answers non-zero, which is the state this call wants, so its exit is not checked.
	a.run(ctx, "launchctl", "bootout", "user/"+strconv.Itoa(rec.uid))

	// AND THEN ANYTHING LEFT. bootout takes the domain's jobs with it; this is for a process the account
	// started outside it — a build the session spawned, a worker mid-command.
	killed := false
	for _, signal := range []string{"-TERM", "-KILL"} {
		if _, err := a.run(ctx, "pkill", signal, "-u", strconv.Itoa(rec.uid)); err == nil {
			killed = true
		}
	}
	if killed {
		// One settle, because a signal is a request and `dscl -delete` on a uid whose processes are still
		// exiting is the race this whole paragraph is about.
		a.sleep(accountReapSettle)
	}

	// ‼️ BOTH TOOLS, BECAUSE BOTH HAVE BEEN MEASURED FAILING — IN OPPOSITE WAYS, ON THIS MACHINE, ON THE
	// SAME NIGHT. `sysadminctl -deleteUser` exited 0 and left the record standing. `dscl . -delete`
	// answered `eDSPermissionError (-14120)` as ROOT, which is the local node refusing an operation root
	// is otherwise entitled to. Neither is trustworthy alone and neither is trustworthy about ITSELF.
	//
	// THE AUTHORITY IS THE READ-BACK BELOW, and that is the whole design: try the deterministic one,
	// then the platform one, then ask directory services what is actually there. A refusal is reported
	// only when the record survived BOTH — which is the one state an operator has to act on.
	dsclOut, dsclErr := a.run(ctx, "dscl", ".", "-delete", "/Users/"+name)
	if dsclErr != nil {
		if out, err := a.run(ctx, "sysadminctl", "-deleteUser", name); err != nil {
			return "", "", macagent.Errorf(macagent.ClassInternal,
				"deleting the record for %s failed with both tools: dscl said %v (%s); sysadminctl said %v (%s)",
				name, dsclErr, strings.TrimSpace(dsclOut), err, strings.TrimSpace(out))
		}
	}
	if err := os.RemoveAll(filepath.Join(a.usersRoot, name)); err != nil {
		return "", "", macagent.Errorf(macagent.ClassInternal, "the record for %s is gone but %s could not be removed: %v", name, home, err)
	}
	// Verified rather than trusted, the same way a reconciler checks both halves: the record can go
	// while the home stays, and a home left behind is the residue this whole phase exists to remove.
	if after, err := a.record(ctx, name); err == nil && after.exists {
		return "", "", macagent.Errorf(macagent.ClassInternal, "the deletion reported success but the record for %s still exists", name)
	}
	if _, err := os.Lstat(home); err == nil {
		return "", "", macagent.Errorf(macagent.ClassInternal, "the record for %s went but %s did not", name, home)
	}

	// AND THE PER-UID STATE OUTSIDE THE HOME, which the account deletion does not touch. Measured: uid
	// 701's bucket AND its mds message directory both outlived its account entirely.
	if _, err := removeDarwinBuckets(a.foldersRoot, rec.uid); err != nil {
		return "", "", macagent.Errorf(macagent.ClassInternal,
			"%s is gone but its darwin cache bucket is not, so its shader cache and simulator residue remain: %v", name, err)
	}
	if err := removeMessagesDir(a.messagesRoot, rec.uid); err != nil {
		return "", "", macagent.Errorf(macagent.ClassInternal,
			"%s is gone but its metadata-server message directory is not: %v", name, err)
	}
	// AND THE GROUP GOES WITH THE ACCOUNT, because the group IS this slot's boundary and a boundary that
	// outlives what it protected is worse than none: the next thing created into palai-s07-grp inherits
	// group access to whatever the last session left behind. Slots are reused — an account is destroyed
	// the moment a session goes idle and minted again for the next one — so this is the ordinary path,
	// not the teardown path.
	if err := a.removeSlotGroup(ctx, slot); err != nil {
		return "", "", err
	}
	return name, home, nil
}

// List reports the slots that currently have an account. It reads names back out of directory services
// and keeps only those inside this namespace whose uid is the one the namespace allocates — the same
// two conditions the deletion guard applies, because a listing that included records this daemon cannot
// delete would be a listing that lies about what it can do.
func (a *SysadminctlAccounts) List(ctx context.Context) ([]int, error) {
	if err := a.requireMac(); err != nil {
		return nil, err
	}
	out, err := a.run(ctx, "dscl", ".", "-list", "/Users", "UniqueID")
	if err != nil {
		return nil, macagent.Errorf(macagent.ClassInternal, "listing users failed: %v: %s", err, strings.TrimSpace(out))
	}
	slots := []int{}
	for _, line := range strings.Split(out, "\n") {
		fields := strings.Fields(line)
		if len(fields) != 2 {
			continue
		}
		slot, ok := macagent.IsAccountName(fields[0])
		if !ok {
			continue
		}
		uid, err := strconv.Atoi(fields[1])
		if err != nil {
			continue
		}
		if want, err := macagent.AccountUID(slot); err != nil || uid != want {
			continue
		}
		slots = append(slots, slot)
	}
	return slots, nil
}

// dsRecord is what directory services says about one account.
type dsRecord struct {
	exists  bool
	name    string
	uid     int
	marker  string
	inAdmin bool
}

func (a *SysadminctlAccounts) record(ctx context.Context, name string) (dsRecord, error) {
	rec := dsRecord{name: name}
	out, err := a.run(ctx, "dscl", ".", "-read", "/Users/"+name, "UniqueID")
	if err != nil {
		// dscl exits non-zero for a record that does not exist, which is the common case on create.
		return rec, nil
	}
	fields := strings.Fields(out)
	if len(fields) < 2 {
		return rec, nil
	}
	uid, err := strconv.Atoi(fields[len(fields)-1])
	if err != nil {
		return rec, fmt.Errorf("uid %q for %s is not a number", fields[len(fields)-1], name)
	}
	rec.exists = true
	rec.uid = uid

	if out, err := a.run(ctx, "dscl", ".", "-read", "/Users/"+name, markerAttr); err == nil {
		if fields := strings.Fields(out); len(fields) >= 2 {
			rec.marker = fields[len(fields)-1]
		}
	}
	if out, err := a.run(ctx, "dsmemberutil", "checkmembership", "-U", name, "-G", "admin"); err == nil {
		rec.inAdmin = strings.Contains(out, "is a member")
	}
	return rec, nil
}

// protectedUIDs lists everything delete must never touch: root, whoever this process runs as, and
// whoever is at the console.
func (a *SysadminctlAccounts) protectedUIDs() map[int]bool {
	protected := map[int]bool{0: true, os.Getuid(): true}
	if fi, err := os.Stat("/dev/console"); err == nil {
		if st, ok := fi.Sys().(*syscall.Stat_t); ok {
			protected[int(st.Uid)] = true
		}
	}
	return protected
}

// notOurAccountBecause is the deletion guard, and it returns prose only when it REFUSES: empty means
// permitted. It is a free function over a record so its whole table of cases can be exercised without a
// Mac and without root, which is the only reason the destructive path has any test coverage at all.
//
// A NAME IS NOT AUTHORITY. Every condition below is one way an account can be named like ours and not
// be ours.
func notOurAccountBecause(rec dsRecord, thisHost string, protected map[int]bool) string {
	slot, ok := macagent.IsAccountName(rec.name)
	if !ok {
		return fmt.Sprintf("%q is not an account this daemon names", rec.name)
	}
	// The uid must be the one this namespace allocates for that exact name. A record whose name and
	// uid disagree was edited after creation and we do not know by whom.
	wantUID, err := macagent.AccountUID(slot)
	if err != nil {
		return err.Error()
	}
	if rec.uid != wantUID {
		return fmt.Sprintf("uid %d is not the uid %d this daemon allocates for %s", rec.uid, wantUID, rec.name)
	}
	if thisHost == "" {
		return "this host has no identity, so no marker can be matched against it"
	}
	parts := strings.Split(rec.marker, ":")
	if len(parts) < 4 || parts[0] != markerMagic || parts[1] != markerVersion {
		return fmt.Sprintf("the record carries no %s marker, so root did not create it here", markerMagic)
	}
	if parts[2] != thisHost {
		return fmt.Sprintf("the marker was minted on host %q and this host is %q", parts[2], thisHost)
	}
	if parts[3] != rec.name {
		return fmt.Sprintf("the marker names %q but the record is %q", parts[3], rec.name)
	}
	if rec.inAdmin {
		return "the account is a member of admin, and this daemon never creates one"
	}
	if protected[rec.uid] {
		return fmt.Sprintf("uid %d is root, the console user, or this process", rec.uid)
	}
	return ""
}

// removeDarwinBuckets deletes every /private/var/folders bucket owned by uid, and returns what it
// removed.
//
// ‼️ THE RANGE CHECK IS THE WHOLE SAFETY ARGUMENT. This deletes directories as root, so it will only
// ever consider a uid this namespace allocates. Called with 0, or 501, or anything outside 701..799 it
// removes nothing and says why — because the difference between "delete the tenant's cache" and
// "delete the operator's cache" is one integer.
func removeDarwinBuckets(root string, uid int) ([]string, error) {
	if _, ok := macagent.SlotFromUID(uid); !ok {
		return nil, fmt.Errorf("uid %d is outside the session range %d..%d, refusing to remove any cache bucket",
			uid, macagent.UIDBase+1, macagent.UIDBase+macagent.MaxSlot)
	}
	buckets, err := darwinBucketsOwnedBy(root, uid)
	if err != nil {
		return nil, err
	}
	removed := make([]string, 0, len(buckets))
	for _, path := range buckets {
		if err := os.RemoveAll(path); err != nil {
			return removed, fmt.Errorf("removing %s: %w", path, err)
		}
		removed = append(removed, path)
	}
	return removed, nil
}

// removeMessagesDir deletes the metadata server's message directory for uid, if there is one.
//
// ‼️ THE SAME RANGE CHECK, AND IT CARRIES MORE WEIGHT HERE THAN IT DOES ABOVE. The bucket sweep has two
// independent filters — the range AND the ownership of what it found. This function has ONE, because
// the directory it removes is root-owned and named after the uid, so ownership says nothing. Called
// with 0 or 501 it would name a real directory belonging to root or to the operator, and the only
// thing standing in front of that is the line below.
//
// An absent directory is SUCCESS, not a failure: `delete` runs for slots that never got far enough to
// have one, and mds creates this lazily. Lstat rather than Stat, and a refusal on a symlink, for the
// reason darwinBucketsOwnedBy gives: a root process removing directories is not the place to follow a
// link somebody planted.
func removeMessagesDir(root string, uid int) error {
	if _, ok := macagent.SlotFromUID(uid); !ok {
		return fmt.Errorf("uid %d is outside the session range %d..%d, refusing to remove any message directory",
			uid, macagent.UIDBase+1, macagent.UIDBase+macagent.MaxSlot)
	}
	if root == "" {
		return fmt.Errorf("no metadata-server message root configured")
	}
	path := filepath.Join(root, strconv.Itoa(uid))
	fi, err := os.Lstat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	if fi.Mode()&os.ModeSymlink != 0 || !fi.IsDir() {
		return fmt.Errorf("%s is not a directory, refusing to remove it", path)
	}
	return os.RemoveAll(path)
}

// darwinBucketsOwnedBy finds every bucket under root that uid owns.
//
// EVERY, NOT THE FIRST. The residue measurement found `_palagenttest` (uid 504) holding TWO buckets, so
// a search that stopped at the first match would leave the second — and a leftover bucket is exactly
// the failure this closes.
//
// It looks exactly two levels down, because that is the shape of the tree
// (/private/var/folders/<a>/<b>), and it uses Lstat so a symlink planted in it is something this
// declines to follow rather than something it deletes through. This tree has already shipped four
// defeated path comparisons inside verifiers; a root process removing directories is not the place for
// a fifth.
func darwinBucketsOwnedBy(root string, uid int) ([]string, error) {
	outer, err := os.ReadDir(root)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	found := []string{}
	for _, a := range outer {
		if !a.IsDir() {
			continue
		}
		inner, err := os.ReadDir(filepath.Join(root, a.Name()))
		if err != nil {
			continue
		}
		for _, b := range inner {
			path := filepath.Join(root, a.Name(), b.Name())
			fi, err := os.Lstat(path)
			if err != nil || fi.Mode()&os.ModeSymlink != 0 || !fi.IsDir() {
				continue
			}
			st, ok := fi.Sys().(*syscall.Stat_t)
			if !ok || int(st.Uid) != uid {
				continue
			}
			found = append(found, path)
		}
	}
	return found, nil
}
