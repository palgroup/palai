package execution

import (
	"testing"
	"time"
)

// THE ENGINE'S WALL TIME IS A MEASURED DEFAULT AND AN OPERATOR'S KNOB.
//
// ‼️ IT WAS 60s, FIXED, AND THAT IS BELOW THE WORK IT BOUNDS. Measured on a live plane 2026-08-07:
// successful runs that clone a repository, call a model several times, write a file and commit it took
// 94.7s, 80.0s, 30.5s, 19.0s and 13.9s, against 9-15s for a plain answer. Two of the five were over the
// bound and finished only because the retry ladder gave them a second attempt after the first was killed
// mid-work — the intermittent coding failure, seen from outside.
func TestTheEngineWallTimeClearsTheSlowestMeasuredRun(t *testing.T) {
	// The slowest honest coding run measured on the plane this default was chosen against.
	const slowestMeasured = 95 * time.Second
	if defaultEngineWallTime < 3*slowestMeasured {
		t.Fatalf("the default engine wall time is %s, under 3x the slowest measured honest run (%s): a "+
			"bound below the work it bounds kills legitimate runs and hides behind the retry ladder",
			defaultEngineWallTime, slowestMeasured)
	}
}

func TestAnUnparseableWallTimeIsTheDefaultAndNeverZero(t *testing.T) {
	// ‼️ THE HALF THAT MATTERS MORE THAN THE PARSE. runner.Limits refuses a non-positive wall time, so a
	// value coerced to zero would take a deployment from "runs are slow" to "nothing runs at all". The
	// shell posture records the same trap for `10min`, which is a duration to a human and nothing to Go.
	for _, raw := range []string{"10min", "", "0", "-5m", "banana"} {
		t.Setenv("PALAI_ENGINE_WALL_TIME", raw)
		if got := EngineWallTime(); got != defaultEngineWallTime {
			t.Fatalf("PALAI_ENGINE_WALL_TIME=%q resolved to %s, want the default %s", raw, got, defaultEngineWallTime)
		}
		if attemptLimits().WallTimeMS <= 0 {
			t.Fatalf("PALAI_ENGINE_WALL_TIME=%q produced a non-positive bound, which runner.Limits refuses", raw)
		}
	}
}

func TestAValidWallTimeIsHonoured(t *testing.T) {
	t.Setenv("PALAI_ENGINE_WALL_TIME", "12m")
	if got := EngineWallTime(); got != 12*time.Minute {
		t.Fatalf("EngineWallTime() = %s, want 12m — an operator's bound that the code ignores is a knob "+
			"with no handle", got)
	}
	if got := attemptLimits().WallTimeMS; got != (12 * time.Minute).Milliseconds() {
		t.Fatalf("the leased limits carry %dms, want %dms: the value has to reach the lease, not just the reader",
			got, (12 * time.Minute).Milliseconds())
	}
}
