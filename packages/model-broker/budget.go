package modelbroker

import (
	"errors"
	"fmt"

	"github.com/palgroup/palai/packages/contracts"
)

// ErrBudgetExceeded is returned when settled usage runs past its reservation. Its
// stable Problem code is budget_exceeded (spec §25.8 terminal outcomes).
var ErrBudgetExceeded = errors.New("budget_exceeded")

// Reservation bounds a single call's spend. A zero MaxTotalTokens is unbounded, so
// a caller that does not budget is not blocked.
type Reservation struct {
	MaxTotalTokens int `json:"max_total_tokens,omitempty"`
}

// Admit reports whether one call's usage fits the reservation.
func (r Reservation) Admit(u contracts.Usage) error {
	if r.MaxTotalTokens > 0 && u.TotalTokens > r.MaxTotalTokens {
		return fmt.Errorf("%w: %d tokens over the %d reserved", ErrBudgetExceeded, u.TotalTokens, r.MaxTotalTokens)
	}
	return nil
}

// Budget accumulates usage against a reservation across calls. It is the accounting
// ledger a coordinator holds for a run: each settled call adds to Consumed and is
// rejected once the running total would pass the reservation.
type Budget struct {
	reservation Reservation
	consumed    contracts.Usage
}

// NewBudget opens a ledger against a reservation.
func NewBudget(r Reservation) *Budget { return &Budget{reservation: r} }

// Settle adds one call's usage to the ledger, rejecting it with ErrBudgetExceeded
// if the running total would exceed the reservation. On rejection the ledger is
// left unchanged, so a rejected call spends nothing.
func (b *Budget) Settle(u contracts.Usage) error {
	next := contracts.Usage{
		InputTokens:  b.consumed.InputTokens + u.InputTokens,
		OutputTokens: b.consumed.OutputTokens + u.OutputTokens,
		TotalTokens:  b.consumed.TotalTokens + u.TotalTokens,
		ToolCalls:    b.consumed.ToolCalls + u.ToolCalls,
	}
	if err := b.reservation.Admit(next); err != nil {
		return err
	}
	b.consumed = next
	return nil
}

// Consumed reports the running total settled so far.
func (b *Budget) Consumed() contracts.Usage { return b.consumed }
