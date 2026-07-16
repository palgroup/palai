package statemachines

import (
	"errors"
	"fmt"
)

// ErrStaleFence is returned when an offered fencing token is not newer than the
// one currently held. Its stable Problem code is lease_conflict.
var ErrStaleFence = errors.New("lease_conflict")

// AcceptFence reports whether an offered fencing token may take over from the
// current one. The token must strictly increase; an equal or lower offer is
// stale (spec §22.3: stale attempts cannot append authoritative state).
func AcceptFence(current, offered uint64) error {
	if offered <= current {
		return fmt.Errorf("%w: offered fence %d is not newer than %d", ErrStaleFence, offered, current)
	}
	return nil
}
