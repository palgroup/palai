// The compose-level half of A.3. Until this epic the control plane held a process-wide executor and
// ran every shell command itself, so the shell posture only ever had to reach the CONTROL-PLANE
// service. A run's command now executes on the machine holding the attempt's lease — the runner — and
// no shipped file gave the runner service either variable:
//
//	grep -rn "PALAI_SHELL_NATIVE\|PALAI_SANDBOX_IMAGE" deploy/     (2026-08-03)
//	  compose.yaml:86             PALAI_SANDBOX_IMAGE -> the CONTROL-PLANE service
//	  native-control-plane.yml:8  PALAI_SHELL_NATIVE  -> a comment
//
// So ServeConfig.Shell was nil on every deployment: a shell tool call would have been answered with
// "this runner was not wired with an executor" forever, and the compose walk could not have found the
// gap because a walk only sees what compose sets.
//
// Static, Docker-free asserts in the spirit of slack_wiring_test.go. What they pin is "the wiring
// exists AND is dormant by default": a non-empty default would give every stack that never declared a
// posture a shell tool it did not ask for, and on the container posture that is a boundary decision.
package compose

import (
	"testing"
)

// shellPostureVars are the three settings that decide where a command runs and how long it may run.
// They are a LIST rather than a walk of the file for the reason this tree keeps re-learning: a walk
// finds what exists, and the defect here was a variable that existed nowhere.
var shellPostureVars = []string{
	"PALAI_SANDBOX_IMAGE",
	"PALAI_SHELL_NATIVE",
	"PALAI_SANDBOX_WALL_TIME",
}

// TestComposeGivesTheRunnerTheShellPosture is the fix for that gap. The runner is the process that
// executes a command now, so its service must carry all three — and carry them INTERPOLATED, so an
// operator's value reaches the container rather than being a key nobody can set.
func TestComposeGivesTheRunnerTheShellPosture(t *testing.T) {
	wiring := loadWiring(t)
	runner, ok := wiring.Services["runner"]
	if !ok {
		t.Fatal("compose.yaml has no runner service, so this guard has checked nothing")
	}
	for _, name := range shellPostureVars {
		value, present := runner.Environment[name]
		if !present {
			t.Errorf("the runner service does not carry %s. It is the process that RUNS a shell command "+
				"since A.3, so without this its executor is nil and every call is answered with a refusal", name)
			continue
		}
		if value != "${"+name+":-}" {
			t.Errorf("the runner's %s = %q, want ${%s:-}: an operator sets this variable, and the "+
				"control-plane service spells the same three the same way", name, value, name)
		}
	}
}

// TestTheShellPostureIsDormantByDefault is the other half, and it is the half that protects every
// existing stack. A default value here would hand a shell tool to a deployment that declared no
// posture — on the native posture that means commands with no container boundary at all, which is a
// security decision and must be the operator's.
//
// It asserts BOTH services, because the control plane still reads its copy: since A.3 that copy
// governs only detached background tasks, which is a narrower job and the same default.
func TestTheShellPostureIsDormantByDefault(t *testing.T) {
	wiring := loadWiring(t)
	for _, service := range []string{"runner", "control-plane"} {
		env := wiring.Services[service].Environment
		for _, name := range shellPostureVars {
			value, present := env[name]
			if !present {
				continue // the runner guard above reports a missing one; the control plane may legitimately differ
			}
			// `${VAR:-}` and `${VAR:-<default>}` are the two shapes; only the first is dormant.
			if value != "${"+name+":-}" {
				t.Errorf("%s carries %s=%q, which is not empty by default: a stack that declared no shell "+
					"posture would get one", service, name, value)
			}
		}
	}
}
