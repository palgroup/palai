package statemachines

import (
	"errors"
	"fmt"
)

// ErrNonMonotonicSequence is returned when a sequence number does not strictly
// increase.
var ErrNonMonotonicSequence = errors.New("non_monotonic_sequence")

// NextSequence reports whether next may follow prev in a strictly increasing
// sequence.
func NextSequence(prev, next int64) error {
	if next <= prev {
		return fmt.Errorf("%w: next %d is not greater than %d", ErrNonMonotonicSequence, next, prev)
	}
	return nil
}
