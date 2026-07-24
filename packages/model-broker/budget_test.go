package modelbroker

import (
	"errors"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/palgroup/palai/packages/contracts"
)

func TestReservationAdmitsWithinAndRejectsOver(t *testing.T) {
	r := Reservation{MaxTotalTokens: 100}
	if err := r.Admit(contracts.Usage{TotalTokens: 100}); err != nil {
		t.Errorf("usage at the reservation must be admitted: %v", err)
	}
	if err := r.Admit(contracts.Usage{TotalTokens: 101}); !errors.Is(err, ErrBudgetExceeded) {
		t.Errorf("usage over the reservation: got %v, want ErrBudgetExceeded", err)
	}
	// A zero reservation is unbounded.
	if err := (Reservation{}).Admit(contracts.Usage{TotalTokens: 1 << 30}); err != nil {
		t.Errorf("an unbounded reservation must admit any usage: %v", err)
	}
}

// TestBudgetSettlesUntilExhausted proves the ledger accumulates usage and, once a
// call would pass the reservation, rejects it and spends nothing.
func TestBudgetSettlesUntilExhausted(t *testing.T) {
	b := NewBudget(Reservation{MaxTotalTokens: 30})
	if err := b.Settle(contracts.Usage{InputTokens: 10, OutputTokens: 8, TotalTokens: 18}); err != nil {
		t.Fatalf("first settle within budget: %v", err)
	}
	if got := b.Consumed().TotalTokens; got != 18 {
		t.Fatalf("consumed = %d, want 18", got)
	}
	if err := b.Settle(contracts.Usage{TotalTokens: 20}); !errors.Is(err, ErrBudgetExceeded) {
		t.Fatalf("settling past the reservation: got %v, want ErrBudgetExceeded", err)
	}
	// The rejected call spent nothing.
	if got := b.Consumed().TotalTokens; got != 18 {
		t.Errorf("consumed after rejection = %d, want the unchanged 18", got)
	}
}

// TestBudgetConcurrentSettlementNeverOverspends is MOD-011 (E16 T6): many concurrent steps settle
// against ONE reservation; the ledger admits usage until the reservation is reached and rejects the
// rest, so the consumed total NEVER exceeds the reservation regardless of interleaving (run under
// -race). This is the local counterpart of the E13 T6 usage_ledger reservation->settlement driven
// concurrently: overspend is impossible.
//
// Honest ceiling: this proves the LEDGER never overspends across concurrent settlements. The live
// budget-admission gate reads SETTLED usage, so an in-flight run can still overshoot a budget by its
// own usage (the BIL-003 documented estimate variance) — proven by CASE=budget-admission, not here.
func TestBudgetConcurrentSettlementNeverOverspends(t *testing.T) {
	const reservation = 1000
	const perCall = 7
	const goroutines = 200
	b := NewBudget(Reservation{MaxTotalTokens: reservation})

	var wg sync.WaitGroup
	var admitted int64
	for i := 0; i < goroutines; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			switch err := b.Settle(contracts.Usage{InputTokens: perCall, TotalTokens: perCall}); {
			case err == nil:
				atomic.AddInt64(&admitted, 1)
			case errors.Is(err, ErrBudgetExceeded):
				// expected once the reservation is reached
			default:
				t.Errorf("unexpected settle error: %v", err)
			}
		}()
	}
	wg.Wait()

	consumed := b.Consumed().TotalTokens
	if consumed > reservation {
		t.Fatalf("consumed = %d, want <= reservation %d (overspend must be impossible)", consumed, reservation)
	}
	// Every admitted call is accounted in the consumed total — nothing settled that was not counted.
	if int(admitted)*perCall != consumed {
		t.Errorf("admitted=%d * %d = %d, want the consumed total %d", admitted, perCall, int(admitted)*perCall, consumed)
	}
	// The ledger admitted as many whole calls as fit — the remaining headroom is less than one call.
	if reservation-consumed >= perCall {
		t.Errorf("consumed=%d leaves headroom %d >= a whole call %d — the ledger under-admitted", consumed, reservation-consumed, perCall)
	}
}
