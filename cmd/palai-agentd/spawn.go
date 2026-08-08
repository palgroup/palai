package main

import (
	"context"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"time"

	"github.com/palgroup/palai/packages/macagent"
)

// sessionWorker is everything about one spawned process, and it is a struct so that the exec boundary
// takes ONE value every field of which this daemon derived.
//
// ‼️ READ THE FIELD LIST AS A CLAIM ABOUT WHAT A CALLER DID NOT CHOOSE. Path comes from a constant, UID
// and GID from arithmetic on the slot, Dir from the slot, and Env from [workerEnv] below. A request
// carries one integer, and every one of these is downstream of it. This type exists at all so that the
// claim is checkable in one place instead of being spread across an argv built at a call site.
type sessionWorker struct {
	// Path is macagent.InstalledWorkerPath. There is no Argv field, and its absence is deliberate: a
	// worker that needed a flag would be a worker whose behaviour a caller could steer by asking for a
	// different one, and this daemon has no way to tell a legitimate flag from a hostile one.
	Path string
	// UID and GID are the account's, from macagent.AccountUID and macagent.SlotGID.
	UID, GID int
	// Dir is the SLOT DIRECTORY: the directory the control plane opens this slot's allocations in, and
	// the one place both principals can reach — the plane by ownership, the worker by its group. It is
	// also where the worker binds its socket.
	Dir string
	// Env is the complete environment, built rather than inherited. See workerEnv.
	Env []string
}

// workerEnvNames is what a session worker's environment consists of, and the list is COMPLETE: nothing
// is inherited from the daemon.
//
// ‼️ IT IS BUILT, NOT FILTERED, and on this daemon that matters more than anywhere else in the tree.
// palai-agentd is started by launchd as root; adapters/sandboxes/host makes the same argument about the
// operator's environment and it is the weaker case, because a control plane's environment holds the
// operator's own secrets while a ROOT LaunchDaemon's holds whatever a machine's plist and launchd
// decided root should have. A deny-list would have to be right about a set nobody here enumerated. So
// the worker gets four variables this function writes and nothing else — a variable nobody named cannot
// arrive.
//
// It was measured on 2026-08-04 that `ps -E` on Darwin 25.3.0 shows a live process's environment to the
// same uid, so a session worker's environment is readable by the tenant that worker belongs to. That is
// the second reason nothing is inherited: an inherited value would not merely be present, it would be
// PUBLISHED to the tenant.
var workerEnvNames = []string{"PATH", "LANG"}

// workerEnv builds the complete environment for one worker: the account's own HOME and TMPDIR, plus the
// few inherited names above. HOME and TMPDIR are written from the account rather than inherited because
// root's are root's, and a tenant process writing into root's TMPDIR is the residue this phase exists
// to remove.
func workerEnv(home string) []string {
	env := []string{"HOME=" + home, "TMPDIR=" + home + "/tmp"}
	for _, name := range workerEnvNames {
		if value, ok := os.LookupEnv(name); ok {
			env = append(env, name+"="+value)
		}
	}
	return env
}

// Spawn starts the session worker as slot N's account.
//
// ‼️ THIS IS THE ONLY PLACE IN THIS SYSTEM WHERE A PRIVILEGE DROP CAN ACTUALLY FIRE, and the reason is
// arithmetic rather than policy: only uid 0 may become another uid, and this daemon is the one Palai
// process that is uid 0. adapters/sandboxes/host has carried a syscall.Credential since 85b62252 and it
// has never once been spent — that executor runs as the operator, so procAttrFor takes its refusing
// branch on every machine that exists. Moving the drop HERE is what makes a tenant's work run as the
// tenant, and it is why neither that Credential nor a chown is on the path any more.
//
// THE GUARD IS THE SAME ONE DELETE USES, and sharing it is the point. Both verbs answer the same
// question — is this record one we created on this Mac? — and a second copy of that question is a
// second place for it to be answered differently. What the two do with a `yes` differs; what makes a
// `yes` does not.
func (a *SysadminctlAccounts) Spawn(ctx context.Context, slot int) (string, int, error) {
	if err := a.requireMac(); err != nil {
		return "", 0, err
	}
	name, err := macagent.AccountName(slot)
	if err != nil {
		return "", 0, err
	}
	uid, err := macagent.AccountUID(slot)
	if err != nil {
		return "", 0, err
	}
	home, err := macagent.HomeDir(slot)
	if err != nil {
		return "", 0, err
	}

	rec, err := a.record(ctx, name)
	if err != nil {
		return "", 0, macagent.Errorf(macagent.ClassInternal, "reading the record for %s failed: %v", name, err)
	}
	if !rec.exists {
		return "", 0, macagent.Errorf(macagent.ClassNotFound, "%s has no account, so there is no uid to start a worker as; create the slot first", name)
	}
	huuid, err := a.hostUUID(ctx)
	if err != nil {
		return "", 0, macagent.Errorf(macagent.ClassInternal, "this host has no IOPlatformUUID, so no marker can be matched against it: %v", err)
	}
	// ‼️ REFUSING TO SPAWN AS A RECORD WE DID NOT CREATE IS NOT THE SAME REFUSAL AS DECLINING TO DELETE
	// ONE, EVEN THOUGH IT IS THE SAME CHECK. Delete's failure mode is erasing an account that is not
	// ours; this one's is starting a process AS a principal that is not ours — root's, the console
	// user's, or an account somebody renamed into our namespace. protectedUIDs is what keeps uid 0 and
	// the operator's own uid out of the second list, and it is exactly as load-bearing here.
	if why := notOurAccountBecause(rec, huuid, a.protectedUIDs()); why != "" {
		return "", 0, macagent.Errorf(macagent.ClassRefused, "%s", why)
	}
	// AND THE UID SPENT IS THE ONE THE SLOT NAMES, not the one the record happens to carry. The guard
	// above already refuses a record whose uid is not this one, so the two agree wherever this line is
	// reached — which is precisely why it costs nothing to spend the derived value and never the read
	// one. A uid that arrived from directory services is a uid something outside this process wrote.
	if rec.uid != uid {
		return "", 0, macagent.Errorf(macagent.ClassInternal,
			"the record for %s carries uid %d and this daemon allocates %d; refusing to start a process as either", name, rec.uid, uid)
	}

	gid, err := macagent.SlotGID(slot)
	if err != nil {
		return "", 0, err
	}

	if err := a.requireSafeWorker(); err != nil {
		return "", 0, err
	}

	// ‼️ ALREADY RUNNING IS THE OUTCOME THIS CALL WANTED, and it is MEASURED rather than remembered. A
	// control plane that restarted holds no memory of which sessions have workers, so its next Acquire
	// asks for one that is already there; a second worker would race the first for the same socket and
	// the loser would take the account's commands with it. A pid table on this side would answer from
	// memory the daemon might also have lost — so the question is put to the socket, which is the only
	// thing that knows.
	if live, lerr := a.workerAnswering(slot); lerr == nil && live {
		return name, 0, nil
	}

	// ‼️ THE WORKER STARTS IN THE SLOT DIRECTORY AND NOT IN THE ACCOUNT'S HOME, because that is where its
	// socket has to live: the control plane OWNS the slot directory and reaches it by ownership, which no
	// group list can take away, while a running plane can never join the account's own group. See
	// macagent.WorkerSocketName for the measurement.
	if a.allocationRoot == "" {
		return "", 0, macagent.Errorf(macagent.ClassUnsupported,
			"this daemon was installed with no allocation root, so a session worker has nowhere to listen — reinstall it with -allocation-root")
	}
	slotDir := filepath.Join(a.allocationRoot, SlotDirName(slot))
	// ‼️ THE DIRECTORY IS PREPARED HERE AND NOT LEFT TO adopt, BECAUSE OF WHEN EACH ONE RUNS. Spawn
	// happens at Acquire — the moment the session gets its account — and adopt happens later, when a
	// workspace has been planned. A worker started before adopt found the slot directory at the mode
	// whoever created it left, and `fork/exec` reports `permission denied` when the CHILD cannot enter
	// its own working directory: measured on a real Mac on 2026-08-08 against a 0710 directory whose
	// group the account holds but whose x-only bit is not enough to bind a socket in.
	//
	// Both verbs now leave the same three facts — owner untouched, group the slot's, mode slotRootMode —
	// so whichever runs first is enough and the second is a no-op. The owner is deliberately NOT set: the
	// control plane created this directory and reaches it by ownership, which is the half no group list
	// can take away.
	// ‼️ AND THE PATH TO IT MUST BE WALKABLE BY THE ACCOUNT, WHICH IS NOT A PROPERTY OF THIS DIRECTORY AT
	// ALL. Measured on a real Mac on 2026-08-08:
	//
	//	drwxr-x---  salih:staff  /Users/salih
	//	drwxrwx---  salih:palai-s02-grp  /Users/salih/palai-work/slot-02
	//
	// The slot directory was perfect and the session could not reach it, because the allocation root sat
	// inside the OPERATOR'S HOME and a session account is deliberately not in `staff` — putting it there
	// is the defect this whole namespace exists to avoid, since `staff` reads every project under that
	// home. So the account cannot traverse `/Users/salih`, and `fork/exec` answers `permission denied`
	// naming the worker binary, which is present, executable and world-readable.
	//
	// The refusal is here rather than in a doc because the failure it prevents names the wrong file.
	if err := requireWalkable(a.allocationRoot); err != nil {
		return "", 0, err
	}
	if err := os.MkdirAll(slotDir, slotRootMode); err != nil {
		return "", 0, macagent.Errorf(macagent.ClassInternal, "create %s: %v", slotDir, err)
	}
	// ‼️ THE OWNER IS THE CONTROL PLANE AND NOT root, WHICH IS THE HALF THIS GOT WRONG FIRST. A directory
	// MkdirAll'd by this daemon belongs to uid 0, and the control plane then cannot write in it — the
	// runner's next `mkdir <slot>/alloc_…` answers `permission denied` and the session fails at workspace
	// planning. Measured on a real Mac on 2026-08-08: `drwxrwx--- 0 801 /Users/Shared/palai-work/slot-01`.
	//
	// Who the control plane IS comes from the allocation root's owner, the same derivation
	// joinControlPlane uses and the only statement about its identity that does not arrive on a wire.
	cpUID, err := a.ownerOf(a.allocationRoot)
	if err != nil {
		return "", 0, macagent.Errorf(macagent.ClassInternal,
			"reading who owns %s, so this daemon cannot tell which user the slot directory belongs to: %v", a.allocationRoot, err)
	}
	// Through the same seam Create's handover uses: only uid 0 may do this, so it is the one filesystem
	// call here a test cannot make.
	if err := a.chown(slotDir, cpUID, gid); err != nil {
		return "", 0, macagent.Errorf(macagent.ClassInternal, "give %s to uid %d gid %d: %v", slotDir, cpUID, gid, err)
	}
	if err := os.Chmod(slotDir, slotRootMode); err != nil {
		return "", 0, macagent.Errorf(macagent.ClassInternal, "set the mode of %s: %v", slotDir, err)
	}
	pid, err := a.spawn(ctx, sessionWorker{
		Path: a.workerPath,
		UID:  uid,
		GID:  gid,
		Dir:  slotDir,
		Env:  workerEnv(home),
	})
	if err != nil {
		return "", 0, macagent.Errorf(macagent.ClassInternal, "starting a session worker as %s (uid %d) failed: %v", name, uid, err)
	}
	return name, pid, nil
}

// requireSafeWorker refuses to execute a worker binary that is not there, or that the accounts this
// daemon manages could have written.
//
// ‼️ THE WRITABILITY CHECK IS ABOUT ONE SLOT ATTACKING ANOTHER, which is easy to miss because the
// process is started AS the tenant and running the tenant's own code as the tenant would cost nothing.
// Each account now has its own group, so no group bit joins two slots — but a worker binary that were
// group- or WORLD-writable would still let slot 07 replace the program slot 08's session is about to
// run. Every slot on the machine is one principal's, and a file all of them can write is not one of them.
//
// It is checked at SPAWN rather than at startup on purpose: an upgrade replaces this file while the
// daemon is running, and a posture read once at boot is a posture that describes a file that is gone.
// The socket's own posture is checked the same way and for the same reason (see requireSocketPosture).
func (a *SysadminctlAccounts) requireSafeWorker() error {
	fi, err := os.Stat(a.workerPath)
	if err != nil {
		return macagent.Errorf(macagent.ClassInternal,
			"there is no session worker at %s, so this daemon has nothing to start: %v", a.workerPath, err)
	}
	if fi.IsDir() || !fi.Mode().IsRegular() {
		return macagent.Errorf(macagent.ClassInternal, "%s is not a regular file (mode %s)", a.workerPath, fi.Mode())
	}
	if fi.Mode().Perm()&0o111 == 0 {
		return macagent.Errorf(macagent.ClassInternal, "%s is not executable (mode %04o)", a.workerPath, fi.Mode().Perm())
	}
	if fi.Mode().Perm()&0o022 != 0 {
		return macagent.Errorf(macagent.ClassInternal,
			"%s is mode %04o, so a session account could replace the program another session is about to run; it must not be group- or world-writable",
			a.workerPath, fi.Mode().Perm())
	}
	return nil
}

// startWorker is the production spawn boundary: it starts the worker and returns its pid WITHOUT
// waiting for it.
//
// ‼️ NoSetGroups IS LEFT FALSE, AND THAT IS THE SAME DECISION adapters/sandboxes/host RECORDS, HERE FOR
// THE FIRST TIME ON A PARENT THAT CAN ACTUALLY SPEND IT. A root parent's group list on a Mac includes
// wheel and admin. NoSetGroups=true would make the child KEEP them, so a process that had just been
// given a tenant's uid would carry root's group memberships and the drop would give back most of what
// it took. False makes Go call setgroups(0, NULL) with a nil Groups, and the child ends with its uid,
// its gid, and nothing else.
//
// Setsid puts the worker in its own session, so it is not in the daemon's process group and a signal
// aimed at launchd's job does not reach a tenant's work sideways.
//
// AND IT IS REAPED. A root daemon that started processes and never waited on them would accumulate
// zombies for the machine's uptime; the goroutine below waits and discards, which is the whole of what
// this daemon wants to know about a worker's exit — the control plane learns a session ended from the
// session, not from a pid.
//
// ‼️ THE CONTEXT IS DELIBERATELY NOT SPENT, AND exec.CommandContext HERE WOULD BE A BUG WITH NO
// SYMPTOM UNTIL PRODUCTION. The ctx that reaches this function is the REQUEST's — Server.handle puts a
// ten-second deadline on the connection — so a CommandContext worker would be killed ten seconds after
// the reply that announced it, and every test that only checks the pid would stay green. A worker
// outlives the request that asked for it by design; that is what makes it a session and not a command.
func startWorker(_ context.Context, w sessionWorker) (int, error) {
	cmd := exec.Command(w.Path)
	cmd.Dir = w.Dir
	cmd.Env = w.Env
	cmd.SysProcAttr = &syscall.SysProcAttr{
		Setsid:     true,
		Credential: &syscall.Credential{Uid: uint32(w.UID), Gid: uint32(w.GID)},
	}
	if err := cmd.Start(); err != nil {
		return 0, fmt.Errorf("start %s: %w", w.Path, err)
	}
	go func() { _ = cmd.Wait() }()
	return cmd.Process.Pid, nil
}

// workerAnswering reports whether a worker is listening for this slot. A dial that connects is proof a
// listener exists, which is precisely the claim this needs — unlike a probe of the daemon itself, where
// the tree insists on a round trip, the question here is only "would a second worker collide".
func (a *SysadminctlAccounts) workerAnswering(slot int) (bool, error) {
	if a.allocationRoot == "" {
		return false, fmt.Errorf("this daemon has no allocation root, so a slot has no directory to hold a worker socket")
	}
	socket := macagent.WorkerSocketIn(filepath.Join(a.allocationRoot, SlotDirName(slot)))
	conn, err := net.DialTimeout("unix", socket, workerDialProbe)
	if err != nil {
		return false, nil
	}
	_ = conn.Close()
	return true, nil
}

// workerDialProbe bounds the collision check. It is short because the socket is on this machine's own
// filesystem: anything slower than this is not a worker that is busy, it is one that is gone.
const workerDialProbe = 500 * time.Millisecond

// requireWalkable refuses an allocation root a session account could never reach.
//
// EVERY ANCESTOR MUST BE WORLD-TRAVERSABLE, and the reason it is `other` rather than a group is that
// these directories are SHARED by every slot: a group that admitted one slot would have to admit all of
// them, which is the fleet-wide group this daemon deleted. x-without-r is what makes that safe — a
// session can walk THROUGH /Users/salih/palai-work and cannot list it.
func requireWalkable(root string) error {
	for dir := filepath.Clean(root); ; dir = filepath.Dir(dir) {
		fi, err := os.Stat(dir)
		if err != nil {
			// A missing ancestor is not this check's business: MkdirAll below reports it, and reporting it
			// twice in two vocabularies is how an operator ends up fixing the wrong thing.
			if os.IsNotExist(err) {
				if parent := filepath.Dir(dir); parent != dir {
					continue
				}
			}
			return macagent.Errorf(macagent.ClassInternal, "reading %s: %v", dir, err)
		}
		if fi.Mode().Perm()&0o001 == 0 {
			return macagent.Errorf(macagent.ClassRefused,
				"%s is mode %04o, so a session account cannot walk through it to reach %s — every directory on the "+
					"way to the allocation root must be traversable by others (x, and r is not needed). This is "+
					"usually an allocation root inside the operator's home: a session account is deliberately not in "+
					"`staff`, because an account that were could read every project under that home. Move the "+
					"workspace root somewhere outside it, e.g. /Users/Shared/palai-work",
				dir, fi.Mode().Perm(), root)
		}
		if parent := filepath.Dir(dir); parent == dir {
			return nil
		}
	}
}
