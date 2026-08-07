package host

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"syscall"
	"testing"

	"github.com/palgroup/palai/packages/macagent"
	toolbroker "github.com/palgroup/palai/packages/tool-broker"
)

// This file is A.5 Task 3's evidence that the uid a session account names REACHES THE KERNEL ARGUMENT
// the child is spawned with — and, where it cannot, that nothing is spawned at all.
//
// ‼️ WHAT IT DOES NOT PROVE, STATED FIRST SO NOTHING BELOW READS AS MORE THAN IT IS. No test in this
// file runs a process as palai-sNN. Measured on this machine 2026-08-05, Darwin 25.3.0, from uid 501:
// `Credential{Uid:701}` returns `fork/exec: operation not permitted` and `os.Chown(f, 701, 20)` returns
// `operation not permitted`, because only uid 0 may become another uid. Every machine this repository
// is developed on is unprivileged, so the FIRST REAL DROP HAS NOT HAPPENED YET AND MAY SURPRISE US —
// the plan's Task 4 is where a privileged execer arrives and where `stat -f '%u'` on a produced file
// can finally answer `palai-sNN`. What is measured here is everything up to that syscall: the bound on
// the uid, the exact Credential built for it, both branches of the privilege check, and the refusal the
// shipped surface returns on a machine that cannot honour the request.

// TestTheCredentialACommandIsSpawnedWithCarriesTheSessionUid is the assertion the whole task exists for.
// It drives procAttrFor at euid 0 — the parameter exists precisely so this branch is reachable from an
// unprivileged machine — and reads back the struct exec.Cmd.SysProcAttr is assigned.
func TestTheCredentialACommandIsSpawnedWithCarriesTheSessionUid(t *testing.T) {
	uid, err := macagent.AccountUID(7)
	if err != nil {
		t.Fatalf("AccountUID(7): %v", err)
	}
	attr, err := procAttrFor(&toolbroker.RunAs{UID: uid, GID: macagent.GIDBase + 7}, 0)
	if err != nil {
		t.Fatalf("procAttrFor at euid 0: %v", err)
	}
	if attr.Credential == nil {
		t.Fatal("the command would be spawned with NO credential: it runs as whoever the executor is, " +
			"which is the operator's uid on every Mac measured — the account would be minted, the home " +
			"directory created, and every tenant would still be one principal")
	}
	if got := int(attr.Credential.Uid); got != uid {
		t.Errorf("Credential.Uid = %d, want %d (palai-s07)", got, uid)
	}
	if got := int(attr.Credential.Gid); got != macagent.GIDBase+7 {
		t.Errorf("Credential.Gid = %d, want %d — slot 7's OWN group, the one sysadminctl -addUser -GID creates the account in",
			got, macagent.GIDBase+7)
	}
	// NoSetGroups FALSE is the secure value and the counter-intuitive one. Go calls setgroups() only when
	// it is false; with a nil Groups that is setgroups(0, NULL), so the child ends with NO supplementary
	// groups. True would make it KEEP the parent's — and the parent has to be root for any of this, whose
	// group list on a Mac includes wheel and admin. The drop would then hand back most of what it took.
	if attr.Credential.NoSetGroups {
		t.Error("NoSetGroups is true: the child would inherit the root supervisor's supplementary groups " +
			"(wheel, admin) along with the tenant's uid")
	}
	// The process group is not a casualty of the new field. It is what makes a reaped xcodebuild take its
	// compilers with it, and it predates this task by two epics.
	if !attr.Setpgid {
		t.Error("Setpgid was lost: a wall-time kill would reap the shell and leave the build tree running")
	}
}

// TestACommandWithNoSessionAccountIsSpawnedExactlyAsItWasBefore is the no-regression half, and it is the
// state a machine is in before `sudo palai agentd install`: nothing mints an account, so RunAs is nil
// on every command in the tree.
func TestACommandWithNoSessionAccountIsSpawnedExactlyAsItWasBefore(t *testing.T) {
	for _, euid := range []int{0, 501} {
		attr, err := procAttrFor(nil, euid)
		if err != nil {
			t.Fatalf("procAttrFor(nil, %d): %v", euid, err)
		}
		if attr.Credential != nil {
			t.Errorf("euid %d: a command that named no session account was given a credential %+v",
				euid, attr.Credential)
		}
		if !attr.Setpgid {
			t.Errorf("euid %d: Setpgid lost", euid)
		}
	}
}

// TestAUidOutsideTheSessionNamespaceIsRefusedBeforeAnythingIsSpentOnIt is the bound on a value that
// crossed a wire. toolbroker.RunAs is two integers, and unlike macagent.Request — which has no
// string-kinded field at all, so `delete salih` is unwriteable rather than refused — a struct of
// integers cannot make a bad integer unwriteable. So this one is a check, and it is here because the
// far side of that wire spends ROOT on the number.
func TestAUidOutsideTheSessionNamespaceIsRefusedBeforeAnythingIsSpentOnIt(t *testing.T) {
	for _, tc := range []struct {
		name  string
		runAs toolbroker.RunAs
	}{
		{"root", toolbroker.RunAs{UID: 0, GID: macagent.GIDBase}},
		{"the operator", toolbroker.RunAs{UID: 501, GID: macagent.GIDBase}},
		{"the base itself, which is slot 0 and is not allocated", toolbroker.RunAs{UID: macagent.UIDBase, GID: macagent.GIDBase}},
		{"one past the last slot", toolbroker.RunAs{UID: macagent.UIDBase + macagent.MaxSlot + 1, GID: macagent.GIDBase + macagent.MaxSlot + 1}},
		{"negative", toolbroker.RunAs{UID: -1, GID: macagent.GIDBase}},
		{"a session uid in the wheel group", toolbroker.RunAs{UID: macagent.UIDBase + 7, GID: 0}},
		// ‼️ THE ROW THE PER-SLOT GROUP EXISTS FOR. Slot 7's uid carrying slot 8's gid is well-formed on
		// both sides and passed every check this guard had while the group was fleet-wide: it is one
		// tenant asking for the group bit on ANOTHER tenant's workspace.
		{"a session uid carrying a DIFFERENT slot's group", toolbroker.RunAs{UID: macagent.UIDBase + 7, GID: macagent.GIDBase + 8}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// euid 0 — the PRIVILEGED branch, because a refusal that only happens on a machine which
			// could not have run the command anyway proves nothing about the bound.
			attr, err := procAttrFor(&tc.runAs, 0)
			if !errors.Is(err, ErrRunAsOutsideNamespace) {
				t.Fatalf("procAttrFor(%+v, euid 0) = (%+v, %v), want ErrRunAsOutsideNamespace", tc.runAs, attr, err)
			}
		})
	}
}

// TestAnUnprivilegedExecutorRefusesRatherThanRunningAsItself is the fail-closed leg, and it is the one
// that would have caught the defect this task was written for. The alternative behaviour — run the
// command as whoever the executor already is — is not a weaker boundary, it is a boundary that was
// reported and does not exist, and docs/measurements/faz-a5-residue.md §2 measured exactly that.
func TestAnUnprivilegedExecutorRefusesRatherThanRunningAsItself(t *testing.T) {
	uid, _ := macagent.AccountUID(7)
	attr, err := procAttrFor(&toolbroker.RunAs{UID: uid, GID: macagent.GIDBase + 7}, 501)
	if !errors.Is(err, ErrCannotDropPrivilege) {
		t.Fatalf("procAttrFor(uid %d, euid 501) = (%+v, %v), want ErrCannotDropPrivilege", uid, attr, err)
	}
	if attr != nil {
		t.Fatalf("a refusal still produced a SysProcAttr %+v — a caller that ignored the error would spawn "+
			"the tenant's command as the operator", attr)
	}
}

// TestTheShippedRunSurfaceRefusesAPrivilegeDropItCannotMake drives Executor.Run, not procAttrFor. The
// distinction is this tree's most expensive recurring one: proving a mechanism is not proving the
// surface a caller reaches. Run is what packages/runner's ToolServer calls for every tool call on a Mac.
//
// It is also the ONE leg of this file that runs against the real euid, and on any machine an agent works
// on that euid is not 0 — so this test asserts the refusal. On a privileged execer it would assert the
// opposite, which is why the branch is written out rather than skipped.
func TestTheShippedRunSurfaceRefusesAPrivilegeDropItCannotMake(t *testing.T) {
	root := t.TempDir()
	witness := filepath.Join(root, "the-command-ran")
	uid, _ := macagent.AccountUID(7)

	result, err := NewExecutor(0).Run(context.Background(), toolbroker.ShellCommand{
		Argv:          []string{"/usr/bin/touch", witness},
		WorkspaceRoot: root,
		RunAs:         &toolbroker.RunAs{UID: uid, GID: macagent.GIDBase + 7},
	})

	if os.Geteuid() == 0 {
		// A privileged execer: the drop is real and the command runs. Assert the FILE, because that is
		// the only evidence that does not come from this process's own bookkeeping.
		if err != nil {
			t.Fatalf("Run() as root: %v", err)
		}
		info, serr := os.Stat(witness)
		if serr != nil {
			t.Fatalf("the command did not run: %v", serr)
		}
		owner := info.Sys().(*syscall.Stat_t).Uid
		if int(owner) != uid {
			t.Fatalf("the file the command produced is owned by uid %d, want %d — the command ran as "+
				"somebody else", owner, uid)
		}
		return
	}

	if !errors.Is(err, ErrCannotDropPrivilege) {
		t.Fatalf("Run() at euid %d = (%+v, %v), want ErrCannotDropPrivilege", os.Geteuid(), result, err)
	}
	// AND NOTHING RAN. A refusal that arrives after the process started is not a refusal.
	if _, serr := os.Stat(witness); !os.IsNotExist(serr) {
		t.Fatalf("the command ran anyway (stat %s: %v) — it ran as uid %d under a request for uid %d",
			witness, serr, os.Geteuid(), uid)
	}
}

// TestABackgroundStartRefusesTheSameDropTheSynchronousPathDoes keeps the two postures' identities in
// step. A detached process is the one nobody is waiting on: if it ran as the wrong uid, the first
// evidence would be files on disk owned by the operator, long after the attempt that spawned it ended.
func TestABackgroundStartRefusesTheSameDropTheSynchronousPathDoes(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("this leg asserts the unprivileged refusal; the privileged branch is covered by Run's test")
	}
	root := t.TempDir()
	witness := filepath.Join(root, "the-task-ran")
	uid, _ := macagent.AccountUID(7)

	handle, err := NewExecutor(0).Start(context.Background(), toolbroker.ShellCommand{
		Argv:          []string{"/usr/bin/touch", witness},
		WorkspaceRoot: root,
		RunAs:         &toolbroker.RunAs{UID: uid, GID: macagent.GIDBase + 7},
	}, toolbroker.BackgroundSpec{TaskID: "bgt_privdrop", OutputPath: ".palai-session/bg/bgt_privdrop.log"})
	if !errors.Is(err, ErrCannotDropPrivilege) {
		t.Fatalf("Start() = (%+v, %v), want ErrCannotDropPrivilege", handle, err)
	}
	if _, serr := os.Stat(witness); !os.IsNotExist(serr) {
		t.Fatalf("the background task was spawned anyway (stat %s: %v)", witness, serr)
	}
}
