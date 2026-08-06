package execution

// White-box guard for the gateway's own presence bookkeeping — the half the projection tests cannot see.
//
// ‼️ IT EXISTS BECAUSE ITS ABSENCE WAS MEASURED. The Fleet presence guards drive a STUB gateway: they
// prove that whatever the gateway answers reaches the panel, and nothing about whether the gateway
// counts. Deleting `pr.life.sessions.Add(1)` from addSession — which makes every machine read `offline`
// forever, the feature's total failure — reddened ZERO tests across the whole component tier on
// 2026-08-06. A projection guard and a mechanism guard are different guards, and this tree keeps
// finding the second one missing.
//
// No websocket is needed: addSession/removeSession are the two points that change the count, and they
// take the session record directly.

import "testing"

func presenceGateway() *RunnerGateway {
	return &RunnerGateway{
		machines: map[string]*runnerLifecycle{},
		sessions: map[*pendingRunner]struct{}{},
	}
}

// session builds the record addSession brackets, bound to this gateway's lifecycle for that machine —
// the same binding the connect handshake makes.
func (g *RunnerGateway) presenceSession(runnerID string) *pendingRunner {
	return &pendingRunner{runnerID: runnerID, life: g.lifecycle(runnerID)}
}

// TestASessionMakesONLYItsOwnMachineConnected is the property last_seen_at cannot carry and the stub
// cannot check: opening a session moves THAT machine's count and no other's.
func TestASessionMakesONLYItsOwnMachineConnected(t *testing.T) {
	g := presenceGateway()
	const mine, other = "rnr_mine", "rnr_other"
	// The other machine is known to the gateway before the test acts, so "0" below is a machine the
	// gateway has a record for and not merely one it has never heard of — those are different zeros.
	g.lifecycle(other)

	if n := g.RunnerConnections(mine); n != 0 {
		t.Fatalf("a machine with no session reports %d connections, want 0", n)
	}

	pr := g.presenceSession(mine)
	g.addSession(pr)
	if n := g.RunnerConnections(mine); n != 1 {
		t.Fatalf("an open session reports %d connections for its own machine, want 1", n)
	}
	if n := g.RunnerConnections(other); n != 0 {
		t.Fatalf("a session on %s made %s report %d connections: one machine's presence must not answer for another", mine, other, n)
	}

	g.removeSession(pr)
	if n := g.RunnerConnections(mine); n != 0 {
		t.Fatalf("after the session closed the machine still reports %d connections, want 0 — a Mac that was unplugged would keep looking online", n)
	}
}

// TestTwoParkedLoopsAreOneMachineHoldingTwo is the counting rule at the source. A runner keeps one
// session per concurrent lease, so the gateway must add them up per machine rather than treating a set
// of connections as a set of machines.
func TestTwoParkedLoopsAreOneMachineHoldingTwo(t *testing.T) {
	g := presenceGateway()
	const id = "rnr_two_loops"

	first, second := g.presenceSession(id), g.presenceSession(id)
	g.addSession(first)
	g.addSession(second)
	if n := g.RunnerConnections(id); n != 2 {
		t.Fatalf("two parked loops report %d, want 2", n)
	}

	// Closing ONE leaves the machine online. A count that dropped to zero here would take a Mac out of
	// the panel while it was still serving its other lease.
	g.removeSession(first)
	if n := g.RunnerConnections(id); n != 1 {
		t.Fatalf("closing one of two loops reports %d, want 1: the machine is still connected", n)
	}
	g.removeSession(second)
	if n := g.RunnerConnections(id); n != 0 {
		t.Fatalf("closing both loops reports %d, want 0", n)
	}
}

// TestAMachineTheGatewayNeverSawReportsZero guards the lookup's miss path. It must answer 0 rather than
// creating a lifecycle record for an id nobody enrolled — a read that writes is a read an unauthenticated
// caller could grow the map with.
func TestAMachineTheGatewayNeverSawReportsZero(t *testing.T) {
	g := presenceGateway()
	if n := g.RunnerConnections("rnr_never_seen"); n != 0 {
		t.Fatalf("an unknown machine reports %d connections, want 0", n)
	}
	if _, created := g.machines["rnr_never_seen"]; created {
		t.Fatal("reading an unknown machine's presence CREATED a lifecycle record: a read that writes lets a caller grow this map")
	}
	if n := g.RunnerConnections(""); n != 0 {
		t.Fatalf("an empty runner id reports %d connections, want 0", n)
	}
}
