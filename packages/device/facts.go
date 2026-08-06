package device

import (
	"sort"
	"strings"
)

// The three isolation modes a machine can support, and they are the product's words rather than an
// implementation's (plan §3.5, DoD 7/8/19):
//
//	user       — one macOS login account, per-session directories and simulator sets, no root. This is
//	             same-customer ACCIDENT isolation and NOT a cross-customer security boundary, which is why
//	             DoD 19 makes multi-tenant pools accounts-only.
//	accounts   — one macOS account per slot, handed out by palai-agentd. It requires a one-time
//	             administrator installation and a machine that cannot prove the daemon is there does not
//	             claim it.
//	container  — the Linux posture: a hardened OCI sandbox per session on a supported Docker daemon.
const (
	IsolationUser      = "user"
	IsolationAccounts  = "accounts"
	IsolationContainer = "container"
)

// Facts are what the machine MEASURED about itself, sent with the enrolment so the gateway can refuse a
// machine that cannot execute before it becomes ready capacity (DoD 9).
//
// ‼️ THE DIFFERENCE FROM EVERY OTHER FIELD ON AN ENROLMENT IS THAT THESE ARE MEASURED. Posture, pool and
// capacity are DECLARATIONS the control plane records and cannot verify — packages/runner says exactly
// that in each field's comment. `runtime.GOOS` and `runtime.GOARCH` are compiled into the binary and a
// daemon that answers a socket answered it; a machine still cannot be forced to tell the truth, but
// these are the facts it cannot get wrong by accident, which is the realistic failure.
type Facts struct {
	OS      string
	Arch    string
	Version string
	// IsolationModes is what this machine can actually do, sorted and deduplicated. EMPTY means the
	// machine measured nothing it can offer — the gateway treats that as "declares nothing" and applies
	// the pool's requirement only if the pool has one, so every machine built before this field enrols
	// byte-unchanged.
	IsolationModes []string
}

// Measure derives the facts from values the caller has already established.
//
// EVERY INPUT IS A PARAMETER because the interesting cases are the ones this process is not in: a Linux
// CI box has to be able to measure what a Mac with no palai-agentd would report, and a function reading
// runtime.GOOS directly can only ever be checked on the platform it happens to be built for.
//
// `agentdReady` is the palai-agentd probe's verdict, which cmd/runner already performs BEFORE enrolment
// (admitMachine). It is passed in rather than re-probed for the reason admitMachine gives about
// PALAI_SHELL_NATIVE: two readings of one fact is two things to keep in step.
//
// `dockerReady` is whether an OCI driver could be constructed. A Linux machine without one cannot run a
// session at all, so it claims no container mode and a container pool refuses it — which is DoD 9's
// whole sentence: a machine that cannot execute never appears as ready capacity.
//
// `workspaceUsable` is PreflightWorkspaceRoot's verdict, and it gates EVERY mode rather than one of them.
// Until 2026-08-06 `user` was claimed on darwin unconditionally, on the reasoning below — which is true
// about installation and silent about writing. A machine that cannot hold a file cannot serve a session
// in any posture, so it claims nothing, and a pool with a requirement refuses it at enrolment.
func Measure(goos, goarch, version string, agentdReady, dockerReady, workspaceUsable bool) Facts {
	facts := Facts{OS: goos, Arch: goarch, Version: version}
	if !workspaceUsable {
		// Deliberately BEFORE the per-mode arms rather than subtracted after them: a machine with nowhere
		// to write has no mode to lose, and the next mode added here inherits the rule instead of having to
		// remember it.
		return facts
	}
	if goos == "darwin" {
		// `user` mode needs nothing installed — it IS the login account this process is already running
		// as. Claiming it is therefore a fact about the platform and not about the machine's setup.
		facts.IsolationModes = append(facts.IsolationModes, IsolationUser)
		if agentdReady {
			facts.IsolationModes = append(facts.IsolationModes, IsolationAccounts)
		}
	}
	if dockerReady {
		facts.IsolationModes = append(facts.IsolationModes, IsolationContainer)
	}
	sort.Strings(facts.IsolationModes)
	return facts
}

// Supports reports whether the machine measured the named mode. An EMPTY name is supported by every
// machine, because "the pool requires no particular mode" is the shipped state of every pool that exists
// today and a requirement nobody set must not refuse anybody.
func (f Facts) Supports(mode string) bool {
	if mode == "" {
		return true
	}
	for _, have := range f.IsolationModes {
		if have == mode {
			return true
		}
	}
	return false
}

// JoinModes renders the modes for storage in one text column. Comma-separated and already sorted, so two
// machines with the same capabilities produce the same string and a reader can compare them.
func JoinModes(modes []string) string { return strings.Join(modes, ",") }

// SplitModes is JoinModes reversed, for a row read back out of the registry. An empty column yields nil
// rather than a one-element slice holding "", which is the difference between "declared nothing" and
// "declared a mode with no name".
func SplitModes(joined string) []string {
	if strings.TrimSpace(joined) == "" {
		return nil
	}
	parts := strings.Split(joined, ",")
	modes := make([]string, 0, len(parts))
	for _, part := range parts {
		if trimmed := strings.TrimSpace(part); trimmed != "" {
			modes = append(modes, trimmed)
		}
	}
	return modes
}
