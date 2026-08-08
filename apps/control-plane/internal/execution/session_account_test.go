package execution

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"syscall"
	"testing"

	"github.com/palgroup/palai/packages/macagent"
)

// fakeSlots drives SlotAccounts without a sudoers entry. The privileged call is one exec; the parts with
// BEHAVIOUR are the slot mapping, the idempotence and what happens to a slot when a verb fails, and none
// of those need root to be wrong.
func fakeSlots() (*SlotAccounts, *[]string, *error) {
	var calls []string
	var failWith error
	a := &SlotAccounts{
		slots: map[string]int{},
		held:  map[int]bool{},
		run: func(_ context.Context, verb string, slot int) error {
			calls = append(calls, verb)
			return failWith
		},
	}
	return a, &calls, &failWith
}

func TestSessionAccountsMapAllocationsOntoSlots(t *testing.T) {
	ctx := context.Background()

	t.Run("two allocations get two slots", func(t *testing.T) {
		a, calls, _ := fakeSlots()
		first, err := a.Acquire(ctx, "alloc_a")
		if err != nil {
			t.Fatalf("acquire: %v", err)
		}
		second, err := a.Acquire(ctx, "alloc_b")
		if err != nil {
			t.Fatalf("acquire: %v", err)
		}
		if first.UID == second.UID {
			t.Fatalf("both allocations got uid %d — two sessions sharing a uid is the collision the accounts "+
				"exist to prevent", first.UID)
		}
		// COUNTED BY VERB AND NOT BY LENGTH. An acquire is a create AND a spawn — the account and the
		// process that IS it — so a raw count says nothing about the property, which is that each session
		// consumes exactly one slot.
		if got := countVerb(*calls, "create"); got != 2 {
			t.Fatalf("privileged calls = %v: %d creates, want one per session", *calls, got)
		}
	})

	// A PROVISION THAT RE-ENTERS IS THE NORMAL RECOVERY PATH, not an error: provisionFreshAllocation is
	// driven idempotently so a failed clone can be retried. A second slot per retry would exhaust a
	// machine's 99 in a loop nobody would think to look at.
	t.Run("a re-entered provision reuses its own slot", func(t *testing.T) {
		a, calls, _ := fakeSlots()
		first, _ := a.Acquire(ctx, "alloc_a")
		again, err := a.Acquire(ctx, "alloc_a")
		if err != nil {
			t.Fatalf("second acquire: %v", err)
		}
		if again != first {
			t.Fatalf("re-acquire gave %+v, want the same %+v", again, first)
		}
		if got := countVerb(*calls, "create"); got != 1 {
			t.Fatalf("privileged calls = %v: %d creates, want the second acquire to make none", *calls, got)
		}
		if got := countVerb(*calls, "spawn"); got != 1 {
			t.Fatalf("privileged calls = %v: %d spawns, want the second acquire to start no second worker — "+
				"two workers race for one socket and the loser takes the session's commands with it", *calls, got)
		}
	})

	t.Run("a released slot is reused", func(t *testing.T) {
		a, _, _ := fakeSlots()
		first, _ := a.Acquire(ctx, "alloc_a")
		if err := a.Release(ctx, "alloc_a"); err != nil {
			t.Fatalf("release: %v", err)
		}
		next, _ := a.Acquire(ctx, "alloc_b")
		if next != first {
			t.Fatalf("next allocation got %+v, want the freed %+v — a machine that never reuses a slot runs "+
				"out after 99 sessions", next, first)
		}
	})

	// A SLOT RESERVED FOR AN ACCOUNT THAT WAS NEVER CREATED IS A SLOT LEAKED FOR THE PROCESS'S LIFE.
	t.Run("a failed create frees its reservation", func(t *testing.T) {
		a, _, fail := fakeSlots()
		*fail = errors.New("sudo: no tty present")
		if _, err := a.Acquire(ctx, "alloc_a"); err == nil {
			t.Fatal("acquire reported success while the privileged call failed")
		}
		*fail = nil
		if _, err := a.Acquire(ctx, "alloc_b"); err != nil {
			t.Fatalf("the next acquire failed too: %v — the failed one kept its slot", err)
		}
	})

	// AND THE OPPOSITE ON DESTROY. An account that may still exist must not have its index handed to the
	// next allocation: that creates a second session on top of the first, which is exactly what the
	// accounts prevent. Losing a slot is cheaper than sharing one.
	t.Run("a failed destroy keeps the slot held", func(t *testing.T) {
		a, _, fail := fakeSlots()
		first, _ := a.Acquire(ctx, "alloc_a")
		*fail = errors.New("account busy")
		if err := a.Release(ctx, "alloc_a"); err == nil {
			t.Fatal("release reported success while the privileged call failed")
		}
		*fail = nil
		next, _ := a.Acquire(ctx, "alloc_b")
		if next == first {
			t.Fatalf("the next allocation was handed %+v, the slot whose account may still exist — two "+
				"sessions would share a uid", next)
		}
	})

	// Destroy runs on paths where provisioning failed before Acquire, and on a control plane that
	// restarted since (the in-process map is a stated limit, see the type comment). Neither is an error.
	t.Run("releasing an unknown allocation is not an error", func(t *testing.T) {
		a, calls, _ := fakeSlots()
		if err := a.Release(ctx, "alloc_never_seen"); err != nil {
			t.Fatalf("release: %v", err)
		}
		if len(*calls) != 0 {
			t.Fatalf("privileged calls = %v, want none — a destroy for an allocation this process never "+
				"acquired must not aim a privileged deletion at somebody else's slot", *calls)
		}
	})

	t.Run("an empty allocation id is refused", func(t *testing.T) {
		a, _, _ := fakeSlots()
		if _, err := a.Acquire(ctx, ""); err == nil {
			t.Fatal("acquired a slot for an allocation with no id")
		}
	})
}

// TestAnAcquiredAccountCarriesTheUidTheDaemonWouldHaveCreated is A.5 Task 3's first assertion, and it
// is about a RETURN VALUE rather than about behaviour. Until this task Acquire answered with a name and
// both call sites passed it to log.Printf; docs/measurements/faz-a5-residue.md §2 measured the result —
// an account created, a home directory populated, and every tenant still running as the operator's uid.
// A name has nothing anyone can spend. A uid does.
func TestAnAcquiredAccountCarriesTheUidTheDaemonWouldHaveCreated(t *testing.T) {
	a, _, _ := fakeSlots()
	account, err := a.Acquire(context.Background(), "sess_a")
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	// SLOT 1 IS THE FIRST FREE ONE, so the uid is the one macagent computes for it — spelled through the
	// same function palai-agentd creates the account with, never as a literal beside it.
	want, err := macagent.AccountUID(1)
	if err != nil {
		t.Fatalf("AccountUID(1): %v", err)
	}
	if account.UID != want {
		t.Errorf("UID = %d, want %d — a uid that disagrees with the account the daemon created is a "+
			"privilege drop into a user that does not exist", account.UID, want)
	}
	// AND THE GID IS SLOT 1'S OWN, spelled through the same function for the same reason. A gid that is
	// merely "in the session range" would be satisfied by ANOTHER slot's group, which is exactly the
	// cross-tenant grant the per-slot group exists to make inexpressible.
	wantGID, err := macagent.SlotGID(1)
	if err != nil {
		t.Fatalf("SlotGID(1): %v", err)
	}
	if account.GID != wantGID {
		t.Errorf("GID = %d, want %d — slot 1's own group, the one sysadminctl -addUser -GID puts the account in",
			account.GID, wantGID)
	}
	if !account.Bound() {
		t.Error("Bound() is false for an account that was just created")
	}
	// A ZERO UID IS ROOT. The whole point of Bound is that the zero value of this struct can never be
	// mistaken for one, because a caller that forgot to check would hand root to a tenant.
	if (SessionAccount{}).Bound() {
		t.Error("a zero SessionAccount reports itself bound: uid 0 is root")
	}
}

// TestLookupNamesOnlyASessionThisProcessAcquired is the seam the EXEC path reads. It mints nothing, and
// the `false` it returns for an unknown session is what execEnv turns into a refusal rather than into a
// command that quietly runs as the control plane.
func TestLookupNamesOnlyASessionThisProcessAcquired(t *testing.T) {
	ctx := context.Background()
	a, calls, _ := fakeSlots()

	if _, ok := a.Lookup("sess_never_seen"); ok {
		t.Fatal("Lookup answered for a session this process never acquired")
	}
	if len(*calls) != 0 {
		t.Fatalf("privileged calls = %v — Lookup created an account", *calls)
	}

	acquired, err := a.Acquire(ctx, "sess_a")
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	found, ok := a.Lookup("sess_a")
	if !ok {
		t.Fatal("Lookup did not find the session that was just acquired: every tool call it makes would " +
			"be refused for want of a uid this process is holding")
	}
	if found != acquired {
		t.Errorf("Lookup gave %+v, Acquire gave %+v — the exec path and the provision path would name two "+
			"different users for one session", found, acquired)
	}

	// AND IT STOPS ANSWERING WHEN THE SLOT GOES BACK. A released slot is handed to the next session, so a
	// Lookup that kept answering would aim one session's commands at another session's uid.
	if err := a.Release(ctx, "sess_a"); err != nil {
		t.Fatalf("release: %v", err)
	}
	if _, ok := a.Lookup("sess_a"); ok {
		t.Fatal("Lookup still answers for a released session")
	}
}

// TestHandingOverATreeChangesTheLinkAndNeverItsTarget is the sharpest test in this file, and the reason
// HandTreeTo is a function rather than three lines inline.
//
// os.Chown FOLLOWS a symlink. The tree being walked is a repository this session's own model may
// already have written into, so `ln -s /etc/passwd anything` inside it would make a ROOT walk hand
// /etc/passwd to the tenant. os.Lchown changes the link itself.
//
// ‼️ THE DISCRIMINATOR IS A DANGLING LINK, AND IT WAS CHOSEN BECAUSE IT NEEDS NO PRIVILEGE. Ownership
// cannot be observed changing on an unprivileged machine — the only uid this process may write is its
// own — so a test that chowned to a real target and read the owner back would measure nothing. But
// os.Chown on a link with no target fails ENOENT while os.Lchown succeeds, so the two functions are
// distinguishable here by their ERROR at any privilege level. Perturbing Lchown to Chown reddens this.
func TestHandingOverATreeChangesTheLinkAndNeverItsTarget(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "repo", "src"), 0o755); err != nil {
		t.Fatalf("seed tree: %v", err)
	}
	if err := os.WriteFile(filepath.Join(root, "repo", "src", "main.swift"), []byte("import Foundation\n"), 0o644); err != nil {
		t.Fatalf("seed file: %v", err)
	}
	if err := os.Symlink(filepath.Join(root, "nothing-is-here"), filepath.Join(root, "repo", "dangling")); err != nil {
		t.Fatalf("seed dangling symlink: %v", err)
	}

	// The current uid is the only one an unprivileged process may give anything to, and giving a tree to
	// the uid that already owns it is a no-op the kernel still validates — which is exactly what makes it
	// a usable stand-in for the walk itself.
	self := SessionAccount{Name: "palai-s01", UID: os.Getuid(), GID: os.Getgid()}
	if err := HandTreeTo(root, self); err != nil {
		t.Fatalf("HandTreeTo over a tree containing a dangling symlink: %v\n"+
			"an ENOENT here means the walk used os.Chown, which resolves a link to its TARGET — and a "+
			"privileged walk doing that gives away whatever the tenant pointed at", err)
	}
}

// TestHandingOverATreeFailsClosedWhenTheUidIsUnreachable is the other half, and the branch every
// unprivileged machine takes. An allocation whose bytes still belong to the control plane, handed to a
// session that was told it owns them, is a permission error arriving in the middle of a build and
// attributed to the build.
//
// BOTH BRANCHES ASSERT. On a privileged execer the hand-over succeeds and the OWNER IS READ BACK OFF
// THE DISK, which is the evidence the plan asks for and which no unprivileged machine can produce.
func TestHandingOverATreeFailsClosedWhenTheUidIsUnreachable(t *testing.T) {
	root := t.TempDir()
	file := filepath.Join(root, "cloned.swift")
	if err := os.WriteFile(file, []byte("// tenant bytes\n"), 0o644); err != nil {
		t.Fatalf("seed: %v", err)
	}
	uid, err := macagent.AccountUID(7)
	if err != nil {
		t.Fatalf("AccountUID(7): %v", err)
	}
	account := SessionAccount{Name: "palai-s07", UID: uid, GID: macagent.GIDBase + 7}

	err = HandTreeTo(root, account)

	if os.Geteuid() == 0 {
		if err != nil {
			t.Fatalf("HandTreeTo as root: %v", err)
		}
		info, serr := os.Lstat(file)
		if serr != nil {
			t.Fatalf("stat after hand-over: %v", serr)
		}
		if owner := int(info.Sys().(*syscall.Stat_t).Uid); owner != uid {
			t.Fatalf("the cloned file is owned by uid %d, want %d", owner, uid)
		}
		return
	}

	if !errors.Is(err, ErrCannotHandOverTree) {
		t.Fatalf("HandTreeTo at euid %d = %v, want ErrCannotHandOverTree — a tree that was not handed "+
			"over while the provision reported success is a boundary that exists only in the log",
			os.Geteuid(), err)
	}
}

// TestATreeIsNeverHandedToNobody keeps the zero value out of the privileged path. uid 0 is root, and a
// SessionAccount nobody filled in is uid 0.
func TestATreeIsNeverHandedToNobody(t *testing.T) {
	root := t.TempDir()
	if err := HandTreeTo(root, SessionAccount{}); err == nil {
		t.Fatal("a zero account was accepted: every entry in the allocation would be chowned to uid 0")
	}
	if err := HandTreeTo("", SessionAccount{Name: "palai-s01", UID: 701, GID: 20}); err == nil {
		t.Fatal("an empty root was accepted")
	}
}

// countVerb is how these assertions read the privileged calls: by VERB, because an acquire makes more
// than one and a length is not a claim about any of them.
func countVerb(calls []string, verb string) int {
	n := 0
	for _, call := range calls {
		if call == verb {
			n++
		}
	}
	return n
}

// TestEveryVerbThisTypeSPENDSIsOneTheMappingKNOWS — THE MAPPING SAID IT WAS TOTAL WHILE BEING
// INCOMPLETE, AND THE COST WAS THE WHOLE BOUNDARY.
//
// `spawn` was added to Acquire on 2026-08-08 and never added to the verb switch in
// NewDaemonSessionAccounts. Nothing failed to compile. The daemon's refusal was correct and useless —
// it named a verb the caller never sent it — and the log line that carried it said "slot 01 has an
// account but no worker", which reads as a machine missing an install. Every shell command then ran as
// the control plane's own uid, and the live chain reported `ran as salih` while every layer above it
// reported success.
//
// So this reads the SOURCE of the two halves rather than driving them: the verbs Acquire/Release/Adopt
// hand to a.run are string literals, and the switch that translates them is a list of string literals,
// and the only way they can disagree is if one grew without the other.
func TestEveryVerbThisTypeSPENDSIsOneTheMappingKNOWS(t *testing.T) {
	body, err := os.ReadFile("session_account.go")
	if err != nil {
		t.Fatalf("read the source: %v", err)
	}
	src := string(body)

	spends := regexp.MustCompile(`a\.run\(ctx, "([a-z]+)"`).FindAllStringSubmatch(src, -1)
	if len(spends) == 0 {
		t.Fatal("no `a.run(ctx, \"...\"` call was found: this guard is reading a file that has changed " +
			"shape, and everything it concludes is about code it does not understand")
	}
	knows := map[string]bool{}
	for _, m := range regexp.MustCompile(`case "([a-z]+)":\s*\n\s*v = macagent\.Verb`).FindAllStringSubmatch(src, -1) {
		knows[m[1]] = true
	}
	if len(knows) == 0 {
		t.Fatal("the verb switch did not parse; the anchor is gone and this guard is vacuous")
	}
	for _, m := range spends {
		if !knows[m[1]] {
			t.Errorf("this type spends %q and the mapping does not know it: the daemon answers "+
				"%q is not a verb this daemon is asked for, which names a verb the caller never sent — and the "+
				"boundary that verb exists for silently is not there", m[1], m[1])
		}
	}
}
