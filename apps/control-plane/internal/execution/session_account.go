package execution

import (
	"context"
	"fmt"
	"os/exec"
	"sync"
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
// onto one machine and nothing more. docs/operations/mac-sessions.md says so at the top and this file
// does not quietly claim otherwise.

// SessionAccounts creates and destroys the account an allocation's tools run under.
//
// Acquire is idempotent per allocation: a retry after a failed provision must not consume a second slot,
// because a provision that re-enters is the normal recovery path rather than an error.
type SessionAccounts interface {
	Acquire(ctx context.Context, allocationID string) (account string, err error)
	Release(ctx context.Context, allocationID string) error
}

// maxSessionSlots is the index range scripts/ops/mac-sessions.sh allocates and the privileged wrapper
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
// are the same sentence" — and it has the same operator-level reconciliation: `mac-sessions.sh down
// --mode accounts --apply` removes every account this tooling created and recorded, and refuses every
// account it did not. Making it survive a restart means a durable slot table, which is a schema change
// and belongs with the decision about whether slots are per-machine or per-fleet. Writing that down is
// the difference between a limit and a bug.
type SlotAccounts struct {
	mu    sync.Mutex
	slots map[string]int // allocation id -> slot index
	held  map[int]bool

	// run performs one privileged verb. It is a field so a test can drive every path of this type
	// without a sudoers entry, which is also why the type is worth having: the mapping and the
	// idempotence are the parts with behaviour, and neither needs root to be tested.
	run func(ctx context.Context, verb string, slot int) error
	// name formats the account for a slot, mirroring mac-sessions.sh's session_name.
	name func(slot int) string
}

// NewSudoSessionAccounts wires the real privileged path: `sudo -n <wrapper> {create|destroy} NN`.
//
// -n IS NOT OPTIONAL. Without it a missing sudoers entry makes sudo PROMPT, and a control plane has no
// terminal — the call would hang instead of failing, and a hang inside allocation provisioning is a run
// that never starts and never says why. With it, a missing entry is an immediate error naming what is
// not installed.
func NewSudoSessionAccounts(wrapper string) *SlotAccounts {
	return &SlotAccounts{
		slots: map[string]int{},
		held:  map[int]bool{},
		name:  func(slot int) string { return fmt.Sprintf("palai-s%02d", slot) },
		run: func(ctx context.Context, verb string, slot int) error {
			out, err := exec.CommandContext(ctx, "sudo", "-n", wrapper, verb, fmt.Sprintf("%02d", slot)).CombinedOutput()
			if err != nil {
				return fmt.Errorf("session account %s slot %02d: %w: %s", verb, slot, err, out)
			}
			return nil
		},
	}
}

// Acquire returns the account for this allocation, creating it on first call.
func (a *SlotAccounts) Acquire(ctx context.Context, allocationID string) (string, error) {
	if allocationID == "" {
		return "", fmt.Errorf("session account: an allocation id is required")
	}
	a.mu.Lock()
	if slot, ok := a.slots[allocationID]; ok {
		a.mu.Unlock()
		return a.name(slot), nil // idempotent: a re-entered provision reuses its own slot
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
		return "", fmt.Errorf("session account: all %d slots are held; this machine cannot isolate another session", maxSessionSlots)
	}
	// RESERVED BEFORE THE PRIVILEGED CALL, so two concurrent provisions cannot pick the same slot while
	// the first one is still creating its account. Released again on failure below — a slot held by an
	// account that was never created is a slot leaked for the life of the process.
	a.held[slot] = true
	a.slots[allocationID] = slot
	a.mu.Unlock()

	if err := a.run(ctx, "create", slot); err != nil {
		a.mu.Lock()
		delete(a.held, slot)
		delete(a.slots, allocationID)
		a.mu.Unlock()
		return "", err
	}
	return a.name(slot), nil
}

// Release destroys the allocation's account. An allocation this instance never acquired is not an error:
// a destroy runs on paths where provisioning failed before it reached Acquire, and on a control plane
// that restarted since (see the type comment).
func (a *SlotAccounts) Release(ctx context.Context, allocationID string) error {
	a.mu.Lock()
	slot, ok := a.slots[allocationID]
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
	delete(a.slots, allocationID)
	a.mu.Unlock()
	return nil
}
