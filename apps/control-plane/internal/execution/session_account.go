package execution

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sync"

	"github.com/palgroup/palai/packages/macagent"
)

// SESSION ACCOUNTS: the uid a workspace's tools run under, created with the allocation and destroyed
// with it.
//
// WHY A uid AND NOT A DIRECTORY. On macOS the uid is the only boundary measured to survive `xcrun simctl
// spawn`, which walks through Apple's own App Sandbox and through TCC because the spawned process is
// parented to launchd rather than to the caller (docs/research/macos-isolation-without-accounts.md, T14
// and T17). Per-session directories and `simctl --set` stop sessions clobbering each other — that is
// accident prevention, and it is not a boundary: any process of the same uid points --set somewhere else
// (T22). So an allocation that must be isolated from the next one needs an account, or it needs its own
// Mac.
//
// WHAT THIS IS NOT. It is not a boundary between CUSTOMERS. sudo and any local-root escalation defeat a
// uid — three such were patched for macOS in 2026 alone — so this packs ONE customer's sessions densely
// onto one machine and nothing more. docs/operations/palai-on-a-mac.md says so at the top and this file
// does not quietly claim otherwise.

// SessionAccounts creates and destroys the account an allocation's tools run under.
//
// THE KEY IS THE SESSION, not the allocation, because that is what the lifecycle is: a session gets an
// account when it starts doing work and loses it when it closes. A session that provisions a second
// allocation — a later run in the same conversation — keeps the same uid, which is what makes its earlier
// files readable to it and unreadable to everybody else.
//
// Acquire is idempotent per session for the same reason: every run of a session asks, and only the first
// one creates.
type SessionAccounts interface {
	Acquire(ctx context.Context, sessionID string) (SessionAccount, error)
	// Lookup answers what THIS process already minted for a session, without minting anything. It is
	// how the exec path reaches the uid the provision path acquired.
	//
	// IT MINTS NOTHING, AND THAT IS THE WHOLE DIFFERENCE FROM Acquire. A tool call arriving for a
	// session this process does not know must not become a new account: the session's tree is owned by
	// the uid of the account it already has, and a fresh slot would hand the run a directory it cannot
	// write. The `false` is therefore the answer, and execEnv turns it into a refusal rather than into
	// a command that quietly runs as the control plane's own uid.
	Lookup(sessionID string) (SessionAccount, bool)
	// Adopt gives this session's allocation directory to this session's account. It is on the interface
	// rather than only on the concrete type because provisioning depends on it and a deployment with no
	// accounts must still compile — the same shape Acquire and Release already have.
	Adopt(ctx context.Context, sessionID string) error
	Release(ctx context.Context, sessionID string) error
}

// SessionAccount is one session's identity on this machine: the account name, and the uid/gid pair a
// privilege drop actually spends.
//
// THE NUMBERS ARE THE FIELD THAT MATTERS AND THE NAME IS NOT LOAD-BEARING. Nothing acts on Name — it is
// carried for the operator reading a refusal — because the kernel reads integers and because this tree
// has already decided once that a privileged path must not take a caller-supplied account name
// (packages/macagent's package comment). The uid is ARITHMETIC from the slot (macagent.AccountUID), so
// it cannot disagree with the account the daemon created unless the arithmetic itself changes, in one
// place, for both.
type SessionAccount struct {
	Name string
	UID  int
	GID  int
}

// Bound reports whether this value names an account at all. A zero SessionAccount is what a
// no-session-account deployment carries, and it must never be mistaken for uid 0.
func (a SessionAccount) Bound() bool { return a.Name != "" && a.UID > 0 }

// ErrCannotHandOverTree is the refusal a tree hand-over returns when this process cannot perform it.
// It is a named error because the caller's answer to it is FAIL, not continue: an allocation whose
// bytes still belong to the control plane's uid, handed to a session that was told it owns them, is a
// run that cannot write its own workspace and a boundary that was reported and not built.
var ErrCannotHandOverTree = fmt.Errorf("this process cannot give a workspace tree to another uid")

// HandTreeTo gives an allocation's tree to the account its commands run as. It is called where BYTES
// LAND — after the clone in provisionFreshAllocation, after the restore in ResumeReleasedWorkspace —
// rather than at Acquire, because a chown before the write owns an empty directory and the writer is
// somebody else.
//
// ‼️ Lchown, NEVER Chown, AND THAT IS THE ONLY REASON THIS IS A FUNCTION AND NOT THREE LINES INLINE.
// os.Chown FOLLOWS a symlink and chowns its TARGET. The tree being walked is a repository this run's
// own model may already have written into, so `ln -s /etc/passwd x` inside it would make a privileged
// walk hand /etc/passwd to the tenant. os.Lchown changes the link itself. filepath.WalkDir does not
// descend through symlinks either, so the walk stays inside the allocation.
//
// A FAILURE STOPS THE WALK. Half a tree owned by the session is worse than none of it: the run fails
// somewhere in the middle of a build with a permission error nobody can attribute, rather than at the
// provision that could say what is missing. On a control plane that is not root every call fails at
// the first entry, which is the honest report that this machine has no privilege drop — see
// docs/plans/2026-08-05-faz-a5-mac-izolasyon.md Task 3.
func HandTreeTo(root string, account SessionAccount) error {
	if root == "" {
		return fmt.Errorf("hand over tree: no allocation path")
	}
	if !account.Bound() {
		return fmt.Errorf("hand over tree %s: no session account to give it to", root)
	}
	return filepath.WalkDir(root, func(path string, _ os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if lerr := os.Lchown(path, account.UID, account.GID); lerr != nil {
			return fmt.Errorf("%w: giving %s to %s (uid %d): %v",
				ErrCannotHandOverTree, path, account.Name, account.UID, lerr)
		}
		return nil
	})
}

// maxSessionSlots is the index range palai-agentd allocates and the privileged wrapper
// accepts: 01..99, two digits, enforced identically on both sides of the sudoers entry.
const maxSessionSlots = 99

// SlotAccounts maps allocations onto the session slots the operator tooling names.
//
// THE SLOT IS THE UNIT BECAUSE THE TOOLING'S IS. palai-session-account takes a two-digit index and
// nothing else — that narrowness is the whole reason its sudoers entry is safe to grant — so the control
// plane owns the allocation→slot mapping rather than passing an allocation id into a privileged call
// that would then have to parse one.
//
// ‼️ THE MAP IS IN PROCESS, AND THAT IS A STATED LIMIT RATHER THAN AN OVERSIGHT. A control plane that
// restarts forgets which slots it held, so accounts created before the restart are not released by it.
// This is the same shape as BGT-P4 — "the machine a task is on and the machine the control plane is on
// are the same sentence" — and it has the same operator-level reconciliation: `palai agentd reconciliation
// --mode accounts --apply` removes every account this tooling created and recorded, and refuses every
// account it did not. Making it survive a restart means a durable slot table, which is a schema change
// and belongs with the decision about whether slots are per-machine or per-fleet. Writing that down is
// the difference between a limit and a bug.
type SlotAccounts struct {
	mu    sync.Mutex
	slots map[string]int // session id -> slot index
	held  map[int]bool

	// run performs one privileged verb. It is a field so a test can drive every path of this type
	// without a sudoers entry, which is also why the type is worth having: the mapping and the
	// idempotence are the parts with behaviour, and neither needs root to be tested.
	run func(ctx context.Context, verb string, slot int) error
}

// NewDaemonSessionAccounts wires the real privileged path: one line over palai-agentd's unix socket.
//
// IT IS A DAEMON AND NOT sudo, and the daemon's own header is where that argument lives: there are a
// hundred of these machines and nobody types a password on them, so sudo is wrong three ways — it wants
// a password, it wants a TTY a control plane does not have, and it hands over everything the named
// binary can be argued into doing. Membership in the socket's group is the entire credential, and the
// verb set on the other side is closed: create, delete, list, spawn, version. There is no pass-through.
//
// THE CONTROL PLANE HAD NO CALL TO IT UNTIL 2026-08-07. palai-agentd shipped, explained itself, and was
// reachable by nothing — the only isolation path here was a sudo wrapper, so a Mac either wanted the
// sudoers entry the design exists to avoid or ran every session as the operator's own uid. It ran as the
// operator's own uid.
func NewDaemonSessionAccounts(socketPath string) *SlotAccounts {
	dial := macagent.UnixDialer(socketPath)
	return &SlotAccounts{
		slots: map[string]int{},
		held:  map[int]bool{},
		run: func(ctx context.Context, verb string, slot int) error {
			// The two verbs this type spends are the two the daemon admits for an account. A third
			// spelling would be a request the daemon refuses on shape, which is the right answer but a
			// late one — so the mapping is total and explicit here.
			var v macagent.Verb
			switch verb {
			case "create":
				v = macagent.VerbCreate
			case "destroy":
				v = macagent.VerbDelete
			case "adopt":
				v = macagent.VerbAdopt
			default:
				return fmt.Errorf("session account: %q is not a verb this daemon is asked for", verb)
			}
			resp, err := macagent.Ask(ctx, dial, macagent.Request{Verb: v, Slot: slot})
			if err != nil {
				return fmt.Errorf("session account %s slot %02d: %w", verb, slot, err)
			}
			// ‼️ "IT ALREADY EXISTS" IS THE OUTCOME THIS CALL WANTED, NOT A REFUSAL, and treating it as one
			// poisons a slot permanently. The daemon's create writes a user record and a home directory,
			// which takes seconds; a caller that timed out mid-create leaves the account behind, and every
			// later attempt on that slot then failed with `daemon refused (exists)` while the account it
			// needed was sitting there. Measured on a real Mac on 2026-08-07, one fix after the timeout
			// that produced it.
			//
			// The same is true in the other direction: deleting an account that is already gone is the
			// state the caller asked for (ClassNotFound). Both verbs are idempotent in EFFECT, and this is the layer that
			// says so — the daemon still reports precisely what it found, because a daemon that lied about
			// which of the two happened would make an operator's `list` unreadable.
			switch {
			case resp.OK:
			case verb == "create" && resp.Class == macagent.ClassExists:
			case verb == "destroy" && resp.Class == macagent.ClassNotFound:
			default:
				return fmt.Errorf("session account %s slot %02d: daemon refused (%s): %s", verb, slot, resp.Class, resp.Message)
			}
			return nil
		},
	}
}

// accountFor builds the identity of a slot. The name comes from the formatter this type was built with
// (production and test spell it the same way); the uid and gid are ARITHMETIC, from the one place that
// owns the namespace, so the pair cannot drift from the account palai-agentd actually created.
func (a *SlotAccounts) accountFor(slot int) (SessionAccount, error) {
	uid, err := macagent.AccountUID(slot)
	if err != nil {
		return SessionAccount{}, fmt.Errorf("session account: slot %d has no uid: %w", slot, err)
	}
	name, err := macagent.AccountName(slot)
	if err != nil {
		return SessionAccount{}, fmt.Errorf("session account: slot %d has no name: %w", slot, err)
	}
	gid, err := macagent.SlotGID(slot)
	if err != nil {
		return SessionAccount{}, fmt.Errorf("session account: slot %d has no gid: %w", slot, err)
	}
	return SessionAccount{Name: name, UID: uid, GID: gid}, nil
}

// Lookup answers what this process already holds for a session. See the interface comment for why it
// mints nothing.
func (a *SlotAccounts) Lookup(sessionID string) (SessionAccount, bool) {
	a.mu.Lock()
	slot, ok := a.slots[sessionID]
	a.mu.Unlock()
	if !ok {
		return SessionAccount{}, false
	}
	account, err := a.accountFor(slot)
	if err != nil {
		return SessionAccount{}, false
	}
	return account, true
}

// Acquire returns the account for this allocation, creating it on first call.
func (a *SlotAccounts) Acquire(ctx context.Context, sessionID string) (SessionAccount, error) {
	if sessionID == "" {
		return SessionAccount{}, fmt.Errorf("session account: a session id is required")
	}
	a.mu.Lock()
	if slot, ok := a.slots[sessionID]; ok {
		a.mu.Unlock()
		return a.accountFor(slot) // idempotent: a re-entered provision reuses its own slot
	}
	slot := 0
	for i := 1; i <= maxSessionSlots; i++ {
		if !a.held[i] {
			slot = i
			break
		}
	}
	if slot == 0 {
		a.mu.Unlock()
		return SessionAccount{}, fmt.Errorf("session account: all %d slots are held; this machine cannot isolate another session", maxSessionSlots)
	}
	// RESERVED BEFORE THE PRIVILEGED CALL, so two concurrent provisions cannot pick the same slot while
	// the first one is still creating its account. Released again on failure below — a slot held by an
	// account that was never created is a slot leaked for the life of the process.
	a.held[slot] = true
	a.slots[sessionID] = slot
	a.mu.Unlock()

	if err := a.run(ctx, "create", slot); err != nil {
		a.mu.Lock()
		delete(a.held, slot)
		delete(a.slots, sessionID)
		a.mu.Unlock()
		return SessionAccount{}, err
	}
	return a.accountFor(slot)
}

// Release destroys the allocation's account. An allocation this instance never acquired is not an error:
// a destroy runs on paths where provisioning failed before it reached Acquire, and on a control plane
// that restarted since (see the type comment).
func (a *SlotAccounts) Release(ctx context.Context, sessionID string) error {
	a.mu.Lock()
	slot, ok := a.slots[sessionID]
	a.mu.Unlock()
	if !ok {
		return nil
	}
	if err := a.run(ctx, "destroy", slot); err != nil {
		// THE SLOT IS NOT FREED ON A FAILED DESTROY. An account that may still exist must not have its
		// index handed to the next allocation, which would create a second session on top of the first —
		// exactly the collision the accounts exist to prevent. The slot is lost until an operator
		// reconciles, and losing a slot is cheaper than sharing one.
		return err
	}
	a.mu.Lock()
	delete(a.held, slot)
	delete(a.slots, sessionID)
	a.mu.Unlock()
	return nil
}

// Adopt asks the daemon to give this session's allocation directory to this session's account.
//
// IT REPLACED A WALK THIS PROCESS COULD NOT PERFORM. HandTreeTo below lchowns the tree itself, and only
// uid 0 may give a tree to another uid — so on every deployment where the control plane is not root
// (which is every deployment) it failed at the first entry with "operation not permitted". The daemon
// is the root this installation has, and it derives the directory from the slot rather than being told
// one, so a caller cannot point it anywhere.
func (a *SlotAccounts) Adopt(ctx context.Context, sessionID string) error {
	account, ok := a.Lookup(sessionID)
	if !ok || !account.Bound() {
		return fmt.Errorf("adopt: session %s holds no account", sessionID)
	}
	slot, ok := macagent.SlotFromUID(account.UID)
	if !ok {
		return fmt.Errorf("adopt: uid %d is not a session slot", account.UID)
	}
	return a.run(ctx, "adopt", slot)
}

// SlotDirFor is where one session's allocations live under the machine's workspace root.
//
// BOTH SIDES DERIVE IT FROM THE SLOT and neither tells the other a path: the control plane places an
// allocation here, the daemon adopts here, and the integer is the only thing that crosses between them.
func SlotDirFor(root string, account SessionAccount) (string, error) {
	slot, ok := macagent.SlotFromUID(account.UID)
	if !ok {
		return "", fmt.Errorf("slot directory: uid %d is not a session slot", account.UID)
	}
	return filepath.Join(root, fmt.Sprintf("slot-%02d", slot)), nil
}
