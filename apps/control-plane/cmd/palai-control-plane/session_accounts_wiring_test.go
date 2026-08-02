package main

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

// TestSessionAccountsAreOneInstanceAcrossBothHalves guards a defect that was written and caught in the
// same sitting, and it is worth stating plainly because the broken version COMPILED, VETTED and read like
// working code.
//
// The lifecycle has two halves in two scopes: the acquire is wired onto the orchestrator inside
// startDispatch, and the release onto the command surface beside routerOpts in main. Each was gated on
// the same environment variable, so each constructed its own SlotAccounts. They share a map — which
// session holds which slot — so the releaser's copy was empty: every close would have found nothing to
// destroy AND REPORTED SUCCESS, leaking one account per session on a resource bounded at 99 per machine,
// produced by the code whose whole job is isolating sessions.
//
// Nothing about that fails a build. The property is "constructed once", so that is what this asserts, by
// reading the composition root the way this tree reads a CLI's tool list rather than duplicating it.
func TestSessionAccountsAreOneInstanceAcrossBothHalves(t *testing.T) {
	src, err := os.ReadFile("main.go")
	if err != nil {
		t.Fatalf("read main.go: %v — if the composition root moved, this guard must follow it rather than be deleted: %v", err, err)
	}
	body := string(src)

	constructions := regexp.MustCompile(`execution\.NewSudoSessionAccounts\(`).FindAllString(body, -1)
	if len(constructions) != 1 {
		t.Fatalf("execution.NewSudoSessionAccounts is called %d times; it must be called exactly ONCE. Two "+
			"instances do not share the map of which session holds which slot, so the releasing half would "+
			"find an empty map, destroy nothing, and report success — one leaked account per session",
			len(constructions))
	}

	// AND BOTH HALVES MUST STILL BE REACHED. "Constructed once" is satisfied by constructing it once and
	// wiring it nowhere, which would be a quieter version of the same bug.
	for _, want := range []struct{ call, why string }{
		{"orch.SetSessionAccounts(", "the acquire half: without it a session never gets its own uid"},
		{"api.WithSessionAccounts(", "the release half: without it an account is minted and never destroyed"},
	} {
		if !strings.Contains(body, want.call) {
			t.Errorf("main.go does not call %s — %s", want.call, want.why)
		}
	}

	// The single value has to REACH the other scope, which for startDispatch means being a parameter. A
	// re-read of the environment variable there would be a second construction wearing a different name.
	if !strings.Contains(body, "sessionAccounts *execution.SlotAccounts") {
		t.Error("startDispatch does not take the accounts as a parameter, so the acquire half is reading " +
			"something other than the value the release half holds")
	}
	if n := strings.Count(body, `os.Getenv("PALAI_SESSION_ACCOUNT_HELPER")`); n != 1 {
		t.Errorf("PALAI_SESSION_ACCOUNT_HELPER is read %d times; it must be read ONCE. A second read is how "+
			"two instances get built without either call site looking wrong", n)
	}
}
