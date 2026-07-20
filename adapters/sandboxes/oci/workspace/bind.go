package workspace

import "fmt"

// BindMode is how a workspace allocation is realised for a session (spec §30.13).
type BindMode string

const (
	// BindSnapshot is the default: the workspace is a snapshot/copy, fully sandbox-isolated, and
	// publication is permitted. No host path is exposed to the model.
	BindSnapshot BindMode = "snapshot"
	// BindUnsafeLocal is a direct mutable host bind mount — opt-in for local development only. It
	// cannot claim sandbox isolation and disables publication (spec §30.13, REP-012).
	BindUnsafeLocal BindMode = "unsafe_local"
)

// BindDecision is the resolved binding for a workspace (spec §30.13, REP-012). For the safe
// default it exposes no host path and leaves publication enabled; for an unsafe local bind it
// records the exact host scope, disables publication, refuses the isolation claim, and carries a
// prominent operator warning.
type BindDecision struct {
	Mode                BindMode
	HostPath            string
	PublicationDisabled bool
	IsolationClaimable  bool
	Warning             string
}

// ResolveBind decides how to bind a workspace (spec §30.13). Default (unsafe=false) is a safe
// snapshot/copy: sandbox isolation holds, publication is allowed, and no host path is bound. A
// direct mutable bind mount is taken ONLY when the caller passes the explicit unsafe-development
// flag (REP-012); it then records the exact host scope, disables publication, cannot claim sandbox
// isolation, and returns a prominent warning naming the exact host path. An unsafe request with an
// empty host path is a caller error — an unsafe bind must name what it exposes.
func ResolveBind(unsafe bool, hostPath string) (BindDecision, error) {
	if !unsafe {
		return BindDecision{Mode: BindSnapshot, IsolationClaimable: true}, nil
	}
	if hostPath == "" {
		return BindDecision{}, fmt.Errorf("unsafe local bind requires an explicit host path to expose")
	}
	return BindDecision{
		Mode:                BindUnsafeLocal,
		HostPath:            hostPath,
		PublicationDisabled: true,
		IsolationClaimable:  false,
		Warning: fmt.Sprintf(
			"UNSAFE LOCAL BIND: %s is mounted read-write into the sandbox for local development. "+
				"Sandbox isolation cannot be claimed and publication is disabled.", hostPath),
	}, nil
}
