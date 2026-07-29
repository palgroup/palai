package runner

// The enrolment side of §2's bit-unchanged rule (E24 T2). The gateway half is proved in
// apps/control-plane/internal/execution (the lease offer's bytes); this is the other direction — what
// a machine SENDS — and it belongs here because this package owns the encoder.

import (
	"encoding/base64"
	"encoding/json"
	"testing"
)

// TestAnUndeclaredPostureSendsTheRequestBodyThisPackageAlwaysSent is the whole of "a runner built
// before E24 T2 is unchanged", stated on bytes rather than on behaviour. A `posture` key present and
// empty would be a new field on the enrolment wire for every deployment in existence, and the reason
// that matters is not tidiness: a control plane too old to know the field would still accept it, but
// the tree has no rule that says so, and asserting the bytes means nobody has to rely on one.
func TestAnUndeclaredPostureSendsTheRequestBodyThisPackageAlwaysSent(t *testing.T) {
	publicKey := base64.StdEncoding.EncodeToString([]byte("public-key-der"))
	body, err := json.Marshal(enrollmentRequest{RunnerID: "runner-local", PublicKey: publicKey})
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	want := `{"runner_id":"runner-local","public_key":"` + publicKey + `"}`
	if string(body) != want {
		t.Fatalf("an undeclared posture changed the enrolment request body.\n got: %s\nwant: %s", body, want)
	}
}

// TestADeclaredPostureIsOnTheWire is the other half: the field has to actually travel, or the control
// plane's comparison is a comparison against nothing. It is a separate test from the one above because
// the two would otherwise be one assertion that can only fail in one direction.
func TestADeclaredPostureIsOnTheWire(t *testing.T) {
	body, err := json.Marshal(enrollmentRequest{RunnerID: "mac-01", PublicKey: "cGs=", Posture: "unsandboxed-host"})
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(body, &decoded); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if decoded["posture"] != "unsandboxed-host" {
		t.Fatalf("enrolment request carries posture %v, want unsandboxed-host", decoded["posture"])
	}
}
