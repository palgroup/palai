package stack

// THE ORDER OF TWO STEPS IN `up`, and why it is worth a test of its own.
//
// ‼️ THE TOOL BASELINE USED TO BE WRITTEN AFTER THE ROUND-TRIP PROOF, under a comment arguing that the
// grant "is configuration and not a precondition of the proof". That was true about the PROOF and was the
// wrong thing to reason about: `roundTripProof` can FAIL, and a failed proof RETURNS from `up`. So on
// every stack whose round trip did not pass — which is every stack with no machine enrolled yet, the
// ordinary state of a fresh Mac fleet — the baseline was never written at all.
//
// What that cost, measured on a native bring-up 2026-08-06: `up` exited at [5/5] with "NOT PROVEN",
// `prj_local.config_policy` stayed `{}`, and because execution/config.go intersects an agent revision's
// tool list with that baseline LAST, every agent on the stack resolved to the EMPTY set. The iOS demo's
// agent answered "I'll start by listing the Swift files in the repository" and called nothing. Nine legs
// of that smoke passed the moment the baseline was granted by hand.
//
// WHY THIS GUARD READS SOURCE RATHER THAN DRIVING `up`. There is no harness that runs `up` end to end —
// it builds images and stands up compose — so the only behavioural leg available (up_e2e_test.go) drives
// `roundTripProof` alone. A source-order assertion is weaker than a behavioural one and is written here
// with that named, rather than left as an untested comment: what it CAN catch is the exact regression
// that happened, somebody moving the grant back down beside the other post-proof status lines because
// that is where the other `warns` live.

import (
	"os"
	"strings"
	"testing"
)

// TestTheToolBaselineIsGrantedBeforeTheProofCanReturn pins the order of the two call sites.
func TestTheToolBaselineIsGrantedBeforeTheProofCanReturn(t *testing.T) {
	body, err := os.ReadFile("up.go")
	if err != nil {
		t.Fatal(err)
	}
	source := string(body)

	const grant = "api.grantDefaultToolBaseline()"
	const proof = "roundTripProof(api)"
	for _, call := range []string{grant, proof} {
		if n := strings.Count(source, call); n != 1 {
			t.Fatalf("up.go carries %d call(s) to %s, want exactly 1 — this guard locates the two steps by "+
				"their call text, and a second copy means it is measuring the wrong one", n, call)
		}
	}
	if strings.Index(source, grant) > strings.Index(source, proof) {
		t.Fatal("the default-tool baseline is granted AFTER the round-trip proof. A failed proof returns " +
			"from `up`, so on every stack whose round trip does not pass — every fleet with no machine " +
			"enrolled yet — the baseline is never written, and every agent on that stack resolves to the " +
			"EMPTY tool set: runs complete, answer in one step, and call nothing while no layer says why")
	}
}

// TestTheProofItselfStillRunsIsNotWhatThisAsserts records what this file deliberately does NOT claim, so
// a later reader does not take the guard above for more than it is.
//
// It says nothing about whether the grant SUCCEEDED, whether the proof ran, or whether either reached the
// server. grantDefaultToolBaseline stands down silently in three cases (read failed, PATCH failed, policy
// already grants tools) and up.go's own emptyToolBaselineWarning is what speaks then — that warning has
// its own tests in up_default_tools_test.go. This one asserts ORDER and nothing else, which is the single
// property those tests cannot see.
func TestTheProofItselfStillRunsIsNotWhatThisAsserts(t *testing.T) {
	body, err := os.ReadFile("up.go")
	if err != nil {
		t.Fatal(err)
	}
	// The warning must stay AFTER the proof: it re-reads the server and is the only thing that speaks
	// when the grant did not land, so moving it above the grant would make it report a baseline that had
	// not been written yet.
	source := string(body)
	if strings.Index(source, "api.emptyToolBaselineWarning()") < strings.Index(source, "api.grantDefaultToolBaseline()") {
		t.Fatal("the empty-baseline warning is read BEFORE the grant writes it: it would report every " +
			"successful bring-up as ungranted")
	}
}
