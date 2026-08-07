package main

import (
	"context"

	"github.com/palgroup/palai/apps/control-plane/api"
	"github.com/palgroup/palai/apps/control-plane/internal/execution"
	"github.com/palgroup/palai/apps/control-plane/internal/fleet"
)

// runnerLogBridge carries one store to the two ends that need it.
//
// THREE TYPES SAY "A LOG LINE" AND THAT IS DELIBERATE, not duplication somebody forgot to collapse.
// execution.RunnerLogLine is a WIRE contract every deployed agent may send; fleet.RunnerLogLine is what
// the table holds; api.RunnerLogEntry is what an operator reads. Collapsing them would mean a column
// added for the reader becomes a field agents are expected to send, which is how a wire contract stops
// being one. The conversion is here, in the composition root, where the three meet by design.
type runnerLogBridge struct{ store *fleet.RunnerLogs }

// Append is the gateway's half: what a machine shipped.
func (b runnerLogBridge) Append(ctx context.Context, project, runnerID string, lines []execution.RunnerLogLine) error {
	out := make([]fleet.RunnerLogLine, 0, len(lines))
	for _, l := range lines {
		out = append(out, fleet.RunnerLogLine{
			At: l.At, Level: l.Level, SessionID: l.SessionID, Message: l.Message,
		})
	}
	return b.store.Append(ctx, project, runnerID, out)
}

// Page is the admin plane's half: what the machine has said.
func (b runnerLogBridge) Page(ctx context.Context, runnerID, sessionID string, limit int) ([]api.RunnerLogEntry, error) {
	lines, err := b.store.Page(ctx, runnerID, sessionID, limit)
	if err != nil {
		return nil, err
	}
	out := make([]api.RunnerLogEntry, 0, len(lines))
	for _, l := range lines {
		out = append(out, api.RunnerLogEntry{
			ID: l.ID, RunnerID: l.RunnerID, At: l.At, ReceivedAt: l.ReceivedAt,
			Level: l.Level, SessionID: l.SessionID, Message: l.Message,
		})
	}
	return out, nil
}
