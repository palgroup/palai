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
	if out, err := a.run(ctx, "sysadminctl",
		"-addUser", name,
		"-fullName", fmt.Sprintf("Palai session s%02d", slot),
		"-UID", strconv.Itoa(uid),
		"-GID", strconv.Itoa(gid),
		"-shell", "/bin/zsh",
		"-home", home,
	); err != nil {
		return "", "", macagent.Errorf(macagent.ClassInternal, "creating %s failed: %v: %s", name, err, strings.TrimSpace(out))
	}

	marker := strings.Join([]string{markerMagic, markerVersion, huuid, name, a.now().UTC().Format(time.RFC3339)}, ":")
	if out, err := a.run(ctx, "dscl", ".", "-create", "/Users/"+name, markerAttr, marker); err != nil {
		return "", "", macagent.Errorf(macagent.ClassInternal, "marking %s failed, so it could never be deleted by this daemon: %v: %s", name, err, strings.TrimSpace(out))
	}

	// -home ASSIGNS a home directory; it does not CREATE one. sysadminctl says so in its own output
	// ("Home directory is assigned (not created!)") and an earlier shell creator shipped a defect by reading
	// straight past it. createhomedir is the supported way to populate one from the user template;
	// -c means this machine, -u names the account.
	if out, err := a.run(ctx, "createhomedir", "-c", "-u", name); err != nil {
		return "", "", macagent.Errorf(macagent.ClassInternal, "populating %s failed: %v: %s", home, err, strings.TrimSpace(out))
	}
	// AND THEN VERIFY IT, because createhomedir exits 0 on paths it did not create — a mobile-account
	// variant, a full disk, a /Users that is not writable. A directory about to be chmod 700 and handed
	// a session is one this daemon must have SEEN.
	if fi, err := os.Stat(home); err != nil || !fi.IsDir() {
		return "", "", macagent.Errorf(macagent.ClassInternal,
			"created %s but %s is not a directory, so this session has nowhere to run; delete the slot before retrying", name, home)
	}
	// The default is drwxr-x--- with group staff, which lets every other session traverse the top level
	// of this one. 700 is the first thing to fix on a shared Mac.
	if err := os.Chmod(home, 0o700); err != nil {
		return "", "", macagent.Errorf(macagent.ClassInternal, "tightening %s to 0700 failed: %v", home, err)
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

	// -secure erases the home directory rather than unlinking it. Spelled with ONE dash: `sysadminctl`
	// on Darwin 25.3.0 prints `-deleteUser <user name> [-secure || -keepHome]`, and the plan's
	// `--secure` is not a flag this binary has.
	if out, err := a.run(ctx, "sysadminctl", "-deleteUser", name, "-secure"); err != nil {
		return "", "", macagent.Errorf(macagent.ClassInternal, "deleting %s failed: %v: %s", name, err, strings.TrimSpace(out))
	}
	// Verified rather than trusted, the same way a reconciler checks both halves: the record can go
	// while the home stays, and a home left behind is the residue this whole phase exists to remove.
	if after, err := a.record(ctx, name); err == nil && after.exists {
		return "", "", macagent.Errorf(macagent.ClassInternal, "sysadminctl reported success but the record for %s still exists", name)
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
