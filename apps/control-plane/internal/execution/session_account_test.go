package execution

import (
	"context"
	"errors"
	"testing"
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
		name:  func(slot int) string { return "palai-s" + string(rune('0'+slot/10)) + string(rune('0'+slot%10)) },
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
		if first == second {
			t.Fatalf("both allocations got %q — two sessions sharing a uid is the collision the accounts "+
				"exist to prevent", first)
		}
		if len(*calls) != 2 {
			t.Fatalf("privileged calls = %v, want one create each", *calls)
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
			t.Fatalf("re-acquire gave %q, want the same %q", again, first)
		}
		if len(*calls) != 1 {
			t.Fatalf("privileged calls = %v, want the second acquire to make none", *calls)
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
			t.Fatalf("next allocation got %q, want the freed %q — a machine that never reuses a slot runs "+
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
			t.Fatalf("the next allocation was handed %q, the slot whose account may still exist — two "+
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
