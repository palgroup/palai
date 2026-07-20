package workspace

import (
	"strings"
	"testing"
)

// TestUnsafeLocalBindRequiresFlagWarnsDisablesPublication proves REP-012 (spec §30.13): a direct
// mutable local bind happens only behind the explicit unsafe flag, and when it does it records the
// exact host scope, disables publication, refuses the isolation claim, and warns prominently. The
// default stays a safe, isolated, publishable snapshot with no host path exposed.
func TestUnsafeLocalBindRequiresFlagWarnsDisablesPublication(t *testing.T) {
	// Default: no unsafe flag → safe snapshot, isolation holds, publication allowed, no host path.
	safe, err := ResolveBind(false, "/host/dev/repo")
	if err != nil {
		t.Fatalf("ResolveBind(safe) error = %v", err)
	}
	if safe.Mode != BindSnapshot || !safe.IsolationClaimable || safe.PublicationDisabled {
		t.Fatalf("default bind = %#v, want a safe isolated publishable snapshot", safe)
	}
	if safe.HostPath != "" || safe.Warning != "" {
		t.Fatalf("default bind leaked a host path/warning: %#v", safe)
	}

	// Unsafe flag set: direct mutable bind, exact host scope recorded, publication disabled,
	// isolation not claimable, prominent warning naming the exact path.
	const host = "/host/dev/my-repo"
	unsafe, err := ResolveBind(true, host)
	if err != nil {
		t.Fatalf("ResolveBind(unsafe) error = %v", err)
	}
	if unsafe.Mode != BindUnsafeLocal {
		t.Fatalf("unsafe bind mode = %q, want %q", unsafe.Mode, BindUnsafeLocal)
	}
	if unsafe.HostPath != host {
		t.Fatalf("unsafe bind host scope = %q, want the exact %q", unsafe.HostPath, host)
	}
	if !unsafe.PublicationDisabled {
		t.Fatal("unsafe bind must disable publication")
	}
	if unsafe.IsolationClaimable {
		t.Fatal("unsafe bind must not claim sandbox isolation")
	}
	if !strings.Contains(unsafe.Warning, host) || !strings.Contains(unsafe.Warning, "isolation") {
		t.Fatalf("unsafe bind warning is not prominent/scoped: %q", unsafe.Warning)
	}

	// An unsafe bind must name what it exposes: an empty host path is a caller error.
	if _, err := ResolveBind(true, ""); err == nil {
		t.Fatal("ResolveBind(unsafe, \"\") = nil error, want a rejection")
	}
}
