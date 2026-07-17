package modelbroker

import (
	"errors"
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
