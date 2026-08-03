package stack

import (
	"os"
	"testing"
	"time"
)

// The knobs a bring-up must actually export. Both of these were measured wrong on a live stack before
// they were tested: a value an operator set in .env.local that never reached the container is
// indistinguishable from a value they never set.

// CONCURRENCY IS TWO NUMBERS AND A STACK IS AS PARALLEL AS THE SMALLER ONE. PALAI_DISPATCH_WORKERS is how
// many runs the control plane dispatches at once; PALAI_RUNNER_CONCURRENCY is how many engine leases one
// runner parks at once, and at the default of 1 a second concurrent dial BLOCKS. `palai up` exported the
// first and not the second, so a .env.local asking for four concurrent runs got four dispatch workers
// queueing behind one engine — measured on the live stack 2026-07-28, dispatch=4 and runner=1.
func TestBothConcurrencyKnobsReachTheStack(t *testing.T) {
	for _, tc := range []struct{ name, dispatch, runner, wantRunner string }{
		{"explicit runner value wins", "4", "2", "2"},
		{"unset runner follows the dispatch count", "4", "", "4"},
		{"a lone runner value is honoured", "", "3", "3"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("PALAI_DISPATCH_WORKERS", "")
			t.Setenv("PALAI_RUNNER_CONCURRENCY", "")
			env := map[string]string{}
			if tc.dispatch != "" {
				env["PALAI_DISPATCH_WORKERS"] = tc.dispatch
			}
			if tc.runner != "" {
				env["PALAI_RUNNER_CONCURRENCY"] = tc.runner
			}
			if err := applyConcurrencyEnv(envGetter(env)); err != nil {
				t.Fatalf("applyConcurrencyEnv: %v", err)
			}
			if got := os.Getenv("PALAI_RUNNER_CONCURRENCY"); got != tc.wantRunner {
				t.Fatalf("PALAI_RUNNER_CONCURRENCY = %q, want %q — compose reads this from the environment, so "+
					"an unexported value leaves the runner at 1 and the stack serial", got, tc.wantRunner)
			}
		})
	}
}

// A BRING-UP ON A LOADED MAC NEEDS LONGER THAN 30s, and that number used to be a constant. The native
// control plane took 34s to serve /v1/capabilities on a 12-core Mac under load (migrations against a
// containerised Postgres, plus Slack socket init), so `palai up --native` failed and never started the
// runner — a stack that looked broken and was merely slow. The default is now 90s and tunable, and a
// malformed value REFUSES rather than silently falling back to the default: an operator who set this
// meant to change it, and a typo that quietly restores 90s is the bug they would never find.
func TestReadyTimeoutIsTunableAndRefusesGarbage(t *testing.T) {
	for _, tc := range []struct {
		name, env string
		want      time.Duration
		wantErr   bool
	}{
		{name: "unset is the generous default", env: "", want: 90 * time.Second},
		{name: "an explicit duration wins", env: "3m", want: 3 * time.Minute},
		{name: "a bare number is not a duration", env: "90", wantErr: true},
		{name: "nonsense refuses", env: "soon", wantErr: true},
		{name: "zero refuses", env: "0s", wantErr: true},
		{name: "negative refuses", env: "-5s", wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("PALAI_STACK_READY_TIMEOUT", tc.env)
			got, err := readyTimeout()
			if tc.wantErr {
				if err == nil {
					t.Fatalf("readyTimeout() = %s, want an error for %q", got, tc.env)
				}
				return
			}
			if err != nil {
				t.Fatalf("readyTimeout(): %v", err)
			}
			if got != tc.want {
				t.Fatalf("readyTimeout() = %s, want %s", got, tc.want)
			}
		})
	}
}
