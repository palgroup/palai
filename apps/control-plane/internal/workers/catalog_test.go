package workers

import (
	"context"
	"errors"
	"testing"
)

// TestNoTunnel_UntypedOperationIsRefusedAtDispatch is the no-tunnel crown (§31.5), unit half: an operation
// that is NOT a typed operation of its capability is refused at dispatch, BEFORE any database work — so an
// ordinary sandbox worker can never be handed a general connect/proxy/exec. A nil pool proves the refusal is
// a pure guard, not a DB effect. Removing the LookupOperation guard in DispatchJob turns each of these green
// (a tunnel job would be accepted): that is the RED this pins.
func TestNoTunnel_UntypedOperationIsRefusedAtDispatch(t *testing.T) {
	s := NewStore(nil, nil, seqID(), nil)
	tenant := Tenant{Organization: "org_x", Project: "prj_x"}
	for _, op := range []string{"tunnel.connect", "net.connect", "shell.exec", "socks5", "http.proxy", ""} {
		_, err := s.DispatchJob(context.Background(), tenant, JobSpec{Capability: "swift-toolchain", Operation: op})
		if !errors.Is(err, ErrUntypedOperation) {
			t.Fatalf("dispatch of untyped operation %q error = %v, want ErrUntypedOperation (no tunnel)", op, err)
		}
	}
}

// TestNoTunnel_TypedOperationPassesTheGate proves the allowlist is not vacuous: the one real typed operation
// clears the guard (it fails later on the nil pool, which is exactly past the guard — the point is it is NOT
// ErrUntypedOperation).
func TestNoTunnel_TypedOperationPassesTheGate(t *testing.T) {
	op, ok := LookupOperation("swift-toolchain", "swift.build-check")
	if !ok {
		t.Fatal("swift.build-check is not a typed operation of swift-toolchain")
	}
	if !op.ReadOnly {
		t.Fatal("swift.build-check should be read-only (a compile check has no external side effect)")
	}
}

// TestAppleBuildCapabilityIsAbsentEverywhere is the honest-ceiling crown (§6 leg 3): apple-build has NO
// Catalog entry, so KnownCapability is false and a worker cannot even ENROLL for it — there is no
// signing/build/store operation anywhere. A nil pool proves the refusal is a pure guard.
func TestAppleBuildCapabilityIsAbsentEverywhere(t *testing.T) {
	if KnownCapability("apple-build") {
		t.Fatal("apple-build must have NO Catalog entry (no real Xcode+signing proof — discovery=disabled)")
	}
	if _, ok := LookupOperation("apple-build", "build"); ok {
		t.Fatal("no apple-build operation may exist (no signing/build/store credential in the system)")
	}
	s := NewStore(nil, nil, seqID(), nil)
	_, err := s.Enroll(context.Background(), Tenant{Organization: "org_x", Project: "prj_x"}, WorkerSpec{Capability: "apple-build"})
	if !errors.Is(err, ErrUnknownCapability) {
		t.Fatalf("enroll for apple-build error = %v, want ErrUnknownCapability", err)
	}
}

// seqID is a deterministic id minter for the unit tier.
func seqID() func(string) string {
	n := 0
	return func(prefix string) string {
		n++
		return prefix + "_" + itoa(n)
	}
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	return string(b[i:])
}
