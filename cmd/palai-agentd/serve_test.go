package main

import (
	"testing"

	"github.com/palgroup/palai/packages/macagent"
)

// TestTheServerBoundMatchesTheClientBound — TWO BOUNDS THAT DISAGREE ARE A CONNECTION ONE SIDE CLOSES
// WHILE THE OTHER IS STILL WAITING.
//
// The client waits macagent.AccountExchangeTimeout for create and delete because `sysadminctl -addUser`
// writes a user record and a home directory. The server had one ten-second connection deadline for
// every verb, so it closed the socket mid-create and the caller read EOF while the account was being
// made underneath — measured on a real Mac on 2026-08-07, on the SECOND session, because the first
// found its account already there and never took the path.
//
// The short bound is KEPT for everything else, and that is the half worth asserting: version and list
// answer from memory, so a slow answer from either is the wedge the ten seconds exist to catch.
func TestTheServerBoundMatchesTheClientBound(t *testing.T) {
	if accountConnDeadline != macagent.AccountExchangeTimeout {
		t.Errorf("server bound %s != client bound %s — whichever is shorter closes first, and the other "+
			"side reports a failure for an operation that succeeded", accountConnDeadline, macagent.AccountExchangeTimeout)
	}
	if connDeadline >= accountConnDeadline {
		t.Fatalf("the silence bound (%s) is not shorter than the account bound (%s), so separating them "+
			"bought nothing", connDeadline, accountConnDeadline)
	}
}
