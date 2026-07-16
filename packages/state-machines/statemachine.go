// Package statemachines holds the pure, in-memory state transition tables and
// guards for Palai execution resources. It imports only the Go standard library.
package statemachines

import (
	"errors"
	"fmt"
)

// ErrInvalidState is returned when a command has no transition from the current
// state. Its stable Problem code is invalid_state.
var ErrInvalidState = errors.New("invalid_state")

// Transition is one row of a state machine table: applying Command while in
// state From moves to state To and emits Event.
type Transition[S comparable, C comparable] struct {
	From    S
	Command C
	To      S
	Event   string
}

// Apply returns the destination state and event for (current, command) in table,
// or ErrInvalidState when no row matches.
func Apply[S comparable, C comparable](current S, command C, table []Transition[S, C]) (S, string, error) {
	for _, tr := range table {
		if tr.From == current && tr.Command == command {
			return tr.To, tr.Event, nil
		}
	}
	var zero S
	return zero, "", fmt.Errorf("%w: no transition from %v via %v", ErrInvalidState, current, command)
}

// TerminalStates reports every state in table, marking a state terminal (true)
// when it appears as a destination but never as a source.
func TerminalStates[S comparable, C comparable](table []Transition[S, C]) map[S]bool {
	out := map[S]bool{}
	for _, tr := range table {
		if _, ok := out[tr.To]; !ok {
			out[tr.To] = true
		}
	}
	for _, tr := range table {
		out[tr.From] = false
	}
	return out
}
