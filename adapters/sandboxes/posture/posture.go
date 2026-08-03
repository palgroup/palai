// Package posture answers one question from the environment a process was started with: WHERE does a
// shell command run on this machine, and under WHAT BOUNDS.
//
// IT EXISTS BECAUSE TWO COMPOSITION ROOTS NOW ASK IT. Before A.3 only the control plane did, so the
// answer lived in its main.go; a command dispatched by any deployment ran beside the control plane
// whatever machine had taken the lease. Now the runner runs the command, so the runner must answer the
// same question — and the second answer must not be a second WRITING of it.
//
// A copy would not stay a copy. The OCI posture needs four bounds (image, wall time, max memory, max
// process count) and a wall time that means UNBOUNDED when it is zero (adapters/sandboxes/host), so
// two independently maintained derivations would eventually disagree — and two disagreeing bounds are
// a containment no operator can reason about, because neither `docker inspect` nor the control plane's
// own settings surface would name the one that applied. That is not hypothetical here: the wall-time
// default had already been written down twice, once in each root, with a comment in the second saying
// it MUST match the first.
package posture

import (
	"fmt"
	"os"
	"strconv"
	"time"

	"github.com/palgroup/palai/adapters/sandboxes/host"
	"github.com/palgroup/palai/adapters/sandboxes/oci"
	"github.com/palgroup/palai/adapters/sandboxes/oci/workspace"
	toolbroker "github.com/palgroup/palai/packages/tool-broker"
)

// Native is the ONLY accepted value of PALAI_SHELL_NATIVE, and it is a sentence rather than a boolean
// because it deletes a security boundary: shell commands then run on this machine as this uid, with no
// container, no network denial and no resource bound. Setting that must not be reachable by the reflex
// that switches a feature on, and a misspelling must be REFUSED rather than silently leaving the
// machine in the other posture.
//
// It is toolbroker's constant rather than a fourth literal: the pool row, the enrolment, the background
// handle and this all compare against the same word.
const Native = string(toolbroker.PostureUnsandboxedHost)

// DefaultWallTime bounds ONE shell call in BOTH postures, and it is a default rather than a required
// setting because the variable was set in no shipped file. Unset used to mean two opposite broken
// things: the host executor ran UNBOUNDED (zero is not neutral there — adapters/sandboxes/host/exec.go)
// and the container posture refused every call.
//
// The postures get the SAME number deliberately. A wall time bounds the WORK, and nothing about a
// container makes a compile faster; these two executors differ only where the machine forces it
// (ReadOnly has no host equivalent, OOMKilled needs a cgroup), and this is not such a place.
const DefaultWallTime = 10 * time.Minute

// Resolve reports whether the deployment declared the native posture, reading PALAI_SHELL_NATIVE and
// PALAI_SANDBOX_IMAGE. An undeclared posture is (false, nil) — no shell at all, not a default to
// either one.
func Resolve(image, native string) (bool, error) {
	if native == "" {
		return false, nil
	}
	if image != "" {
		return false, fmt.Errorf("PALAI_SANDBOX_IMAGE and PALAI_SHELL_NATIVE are mutually exclusive: "+
			"a machine runs its shell commands in the sandbox image or on the host, never both (image=%q, native=%q)", image, native)
	}
	if native != Native {
		return false, fmt.Errorf("PALAI_SHELL_NATIVE must be exactly %q (got %q): the value states what the "+
			"posture IS, because it deletes the sandbox boundary", Native, native)
	}
	return true, nil
}

// WallTime is the PALAI_SANDBOX_WALL_TIME bound both postures run under.
//
// ponytail: an UNPARSEABLE value is treated as unset, so `PALAI_SANDBOX_WALL_TIME=10min` (not a Go
// duration) silently gets the default. That is the behaviour both roots already had.
func WallTime() time.Duration {
	if d, err := time.ParseDuration(os.Getenv("PALAI_SANDBOX_WALL_TIME")); err == nil && d > 0 {
		return d
	}
	return DefaultWallTime
}

// Limits builds the OCI posture's resource bounds: PALAI_SANDBOX_MAX_MEMORY_BYTES,
// PALAI_SANDBOX_MAX_PROCS and PALAI_SANDBOX_NANO_CPUS, over the shared WallTime.
//
// It is a named function so a test can assert what a composition root ACTUALLY builds: every sandbox
// test in this tree constructs its own oci.Limits with explicit values, which is exactly why an unset
// variable that refused every containerised shell call was invisible to all of them.
func Limits() oci.Limits {
	return oci.Limits{
		WallTime:        WallTime(),
		MaxMemoryBytes:  int64(envInt("PALAI_SANDBOX_MAX_MEMORY_BYTES", 1<<30)),
		MaxProcessCount: int64(envInt("PALAI_SANDBOX_MAX_PROCS", 128)),
		NanoCPUs:        int64(envInt("PALAI_SANDBOX_NANO_CPUS", 1_000_000_000)),
	}
}

// RunnerFromEnv builds the executor this process runs shell commands on, in whichever posture the
// environment DECLARED (Resolve):
//
//   - the credential-free OCI sandbox (spec §28.8, SAN-002/003/004), gated on PALAI_SANDBOX_IMAGE (the
//     pinned command image) and a working Docker driver. The sandbox mounts no credential/DB/S3: the
//     credential broker stays control-plane-side (§24), so neither the engine nor the sandbox sees them;
//   - the HOST executor (E22 T1), gated on PALAI_SHELL_NATIVE, which runs the argv on this machine as
//     this uid — no container, no network denial, no resource bound. That is how a machine that is a Mac
//     reaches the Mac's own tools (`xcodebuild`, `simctl`, `axe`) without Palai typing a single one.
//
// THREE OUTCOMES, AND THEY ARE DELIBERATELY DISTINCT. (nil, nil) is "this deployment declares no shell
// posture", which is a legitimate configuration and the one every pre-E22 stack ran. A non-nil error is
// a REFUSAL — a contradictory or misspelled declaration, or a driver that would not bind — and a caller
// must never read it as the first case, because falling back to the other posture is precisely how a
// deleted security boundary would arrive by accident.
func RunnerFromEnv() (toolbroker.ShellRunner, error) {
	image := os.Getenv("PALAI_SANDBOX_IMAGE")
	native, err := Resolve(image, os.Getenv("PALAI_SHELL_NATIVE"))
	if err != nil {
		return nil, err
	}
	if native {
		return host.NewExecutor(WallTime()), nil
	}
	if image == "" {
		return nil, nil
	}
	driver, err := oci.NewDockerDriver()
	if err != nil {
		return nil, fmt.Errorf("bind docker driver for PALAI_SANDBOX_IMAGE=%q: %w", image, err)
	}
	return workspace.NewShellExecutor(driver, image, Limits()), nil
}

// BackgroundRunnerFor reports the DETACHED runner for a wired posture, or nil when that posture cannot
// start a command that outlives the attempt (E26 T1).
//
// NIL IS THE HONEST ANSWER AND NOT A DEGRADED ONE. A posture that cannot detach must leave that seam
// unwired so the tool refuses; falling back to a synchronous run would block the model in precisely the
// way background execution exists to prevent. It is written as a type assertion rather than assumed —
// both shipped executors satisfy it today — because ShellRunner is the interface a composition root
// builds against, and a posture that could only run synchronously must produce nil here rather than a
// half-wired seam.
func BackgroundRunnerFor(shell toolbroker.ShellRunner) toolbroker.BackgroundRunner {
	background, ok := shell.(toolbroker.BackgroundRunner)
	if !ok {
		return nil
	}
	return background
}

func envInt(name string, def int) int {
	n, err := strconv.Atoi(os.Getenv(name))
	if err != nil {
		return def
	}
	return n
}
