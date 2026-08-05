package macagent

import (
	"context"
	"fmt"
	"runtime"
)

// Admission is the question a machine answers about ITSELF before it joins a pool: is the mechanism
// that isolates one tenant from the next actually here?
//
// ‼️ IT IS ASKED ON THE MACHINE BECAUSE ONLY THE MACHINE CAN SEE THE ANSWER. The control plane already
// records a posture, a pool and a capacity, and every one of those is a DECLARATION it cannot verify —
// packages/runner says so in the field comments. This is the same kind of statement with one difference
// that matters: the machine can measure it before it makes it, so a machine that cannot measure it
// refuses to make it.
type Admission struct {
	// Native is whether this machine will run a tenant's shell commands ON ITSELF, with no container
	// around them. It is the answer adapters/sandboxes/posture already derives, passed in rather than
	// re-derived, because a second reading of PALAI_SHELL_NATIVE is a second thing to keep in step with
	// the first and this tree has already paid for one of those.
	Native bool
	// GOOS is runtime.GOOS in production. A field because the refusal has to be testable on the Linux
	// box CI runs on, where the whole mechanism does not exist.
	GOOS string
	// Probe is how the daemon is measured. Nil means nothing was measured, which is not admission — see
	// Admit.
	Probe *Prober
}

// ErrNoIsolation is what a machine is refused with when it would run tenant commands as itself and has
// no daemon to make each tenant a different uid.
var ErrNoIsolation = fmt.Errorf("this machine cannot isolate a session")

// Admit refuses a machine that would run tenant work unsandboxed and has no palai-agentd behind it.
//
// ‼️ TODAY THE OPPOSITE IS TRUE AND IT WAS MEASURED (2026-08-05, this machine): the socket does not
// exist, no plist is installed, the `palai` group does not exist — and a native bring-up runs anyway,
// notices none of it, and looks like it is working. That is the defect this whole phase exists to close.
// A machine in that state runs every tenant as uid 501, which is the operator's own uid, so the 0700
// modes on each home directory protect nothing: there is one principal, and permissions between one
// principal and itself are not a boundary.
//
// FAIL-CLOSED MEANS THE THIRD STATE IS GONE. Of a hundred Macs, each is either provably isolated or not
// in the pool at all. The expensive state is the one in between — a machine serving tenant work that
// nobody can say either way about — and it is expensive precisely because it looks identical to a
// healthy one from every surface except this one.
//
// THE SANDBOXED POSTURE IS NOT GATED, and that is a decision rather than an oversight: there the
// boundary is the container, the session account is not what carries it, and requiring a macOS daemon
// of a Linux container runner would refuse every machine in every deployment that exists today for a
// mechanism none of them use.
//
// CEILING, NAMED RATHER THAN HIDDEN: on a non-Mac the native posture is NOT gated here, because there
// is no session-account mechanism on that platform for this to check — palai-agentd is `sysadminctl`
// and `dscl`. A Linux host running PALAI_SHELL_NATIVE therefore still runs tenant commands as one uid
// with nothing between them, and this gate does not say otherwise. It is not the machine this phase is
// about, and a refusal here would be a refusal with no fix behind it.
func Admit(ctx context.Context, a Admission) (Health, error) {
	if !a.Native {
		return Health{}, nil
	}
	goos := a.GOOS
	if goos == "" {
		goos = runtime.GOOS
	}
	if goos != "darwin" {
		return Health{}, nil
	}
	if a.Probe == nil {
		return Health{}, fmt.Errorf("%w: it declares the unsandboxed-host posture and nothing measured whether palai-agentd is here", ErrNoIsolation)
	}
	health, err := a.Probe.Probe(ctx)
	if err != nil {
		return health, fmt.Errorf("%w: it declares the unsandboxed-host posture, where a tenant's commands run on this machine as this uid "+
			"and the ONLY boundary between one tenant and the next is a per-session account — and palai-agentd, which owns those accounts, "+
			"did not answer: %w", ErrNoIsolation, err)
	}
	return health, nil
}
