package main

import "testing"

// TestThePlaneWinsOverTheBoxsOwnEnvironment is the runner half of the desired-configuration round trip, and
// it exists because the control-plane binary's own round-trip guard structurally cannot cover it: that guard
// pushes a value through THIS setting's reader, and this setting's reader is in this binary, not that one.
// Narrowing it there (DesiredWritableSettingsFor) without writing this here would have moved a setting out
// of one guard's coverage and into nobody's — which is the shape this tree keeps finding.
//
// THE PRECEDENCE IS THE PRODUCT DECISION. The plane wins because the point of the runner_pool document is
// that an operator configures a fleet from one screen instead of editing a file on every box. The
// environment is still read second, and that second read is the migration path rather than a fallback worth
// keeping forever: the compose file, the Helm chart and the systemd unit all set this variable today, so a
// runner that ignored them would come up misconfigured against a control plane nobody had given a document.
func TestThePlaneWinsOverTheBoxsOwnEnvironment(t *testing.T) {
	const name = "PALAI_RUNNER_CONCURRENCY"

	t.Run("the plane's value is used even when the box disagrees", func(t *testing.T) {
		t.Setenv(name, "1")
		if got := planeIntDefault(map[string]string{name: "4"}, name, 1); got != 4 {
			t.Fatalf("concurrency = %d, want 4 — the box's own environment overrode the admin plane, so an "+
				"operator's fleet-wide decision would not reach a machine that had the variable set locally", got)
		}
	})

	t.Run("no document leaves the box's environment in charge", func(t *testing.T) {
		t.Setenv(name, "3")
		if got := planeIntDefault(nil, name, 1); got != 3 {
			t.Fatalf("concurrency = %d, want 3 — a pool nobody has configured must behave exactly as it did "+
				"before the document existed, or this change breaks every deployment built before it", got)
		}
	})

	t.Run("neither means the built-in default", func(t *testing.T) {
		t.Setenv(name, "")
		if got := planeIntDefault(nil, name, 1); got != 1 {
			t.Fatalf("concurrency = %d, want the built-in 1", got)
		}
	})

	// A VALUE THE PLANE SENT THAT DOES NOT PARSE FALLS TO THE ENVIRONMENT, NOT TO THE DEFAULT, and the
	// difference is what an operator sees. Falling to the default would make a typo in the panel
	// indistinguishable from an unconfigured pool: the machine would quietly serve one lease and the screen
	// would show four. Falling to the environment keeps the machine on whatever it was already doing, which
	// is the answer that does not invent a number nobody chose.
	t.Run("an unparseable plane value falls through to the environment", func(t *testing.T) {
		t.Setenv(name, "3")
		for _, bad := range []string{"four", "", "0", "-2", " 4"} {
			if got := planeIntDefault(map[string]string{name: bad}, name, 1); got != 3 {
				t.Errorf("plane value %q gave %d, want the environment's 3 — a typo in the panel must not be "+
					"silently indistinguishable from a pool nobody configured", bad, got)
			}
		}
	})
}

// TestOneAddressDerivesWhatFourVariablesUsedToCarry is the "one CLI" half of the install: a machine
// joining a fleet knows WHERE its control plane is, and should not separately be told three URL paths and
// a DNS name on that same host. Asking for them is how a one-command install becomes a file to edit on
// every box — the thing the runner-plane document exists to remove, applied to the bootstrap itself.
//
// EXPLICIT ALWAYS WINS, and that is not politeness: compose, Helm and the systemd unit all set these four
// today, so a derivation that overrode them would silently repoint every existing deployment at whatever
// PALAI_CONTROLLER_URL happened to say.
func TestOneAddressDerivesWhatFourVariablesUsedToCarry(t *testing.T) {
	const base = "https://cp.example.internal:8443"

	t.Run("the paths are the gateway's own routes", func(t *testing.T) {
		for _, tc := range []struct{ name, path, want string }{
			{"PALAI_ENROLLMENT_URL", "/v1/runner/enroll", base + "/v1/runner/enroll"},
			{"PALAI_SESSION_URL", "/v1/runner/connect", base + "/v1/runner/connect"},
			{"PALAI_RENEW_URL", "/v1/runner/renew", base + "/v1/runner/renew"},
		} {
			t.Setenv(tc.name, "")
			if got := derivedEnv(tc.name, base, joinPath(tc.path)); got != tc.want {
				t.Errorf("%s derived %q, want %q", tc.name, got, tc.want)
			}
		}
	})

	// A PORT IN A ServerName IS A HANDSHAKE THAT NEVER MATCHES. The DNS name is what the runner pins its
	// TLS to and what the certificate's SAN carries, so the host must come out without the port.
	t.Run("the DNS name drops the port", func(t *testing.T) {
		t.Setenv("PALAI_CONTROLLER_DNS", "")
		if got := derivedEnv("PALAI_CONTROLLER_DNS", base, hostOf); got != "cp.example.internal" {
			t.Fatalf("controller DNS = %q, want the host with no port — a ServerName carrying a port never "+
				"matches the certificate's SAN", got)
		}
	})

	t.Run("an explicit value beats the derivation", func(t *testing.T) {
		t.Setenv("PALAI_SESSION_URL", "https://old.internal/v1/runner/connect")
		got := derivedEnv("PALAI_SESSION_URL", base, joinPath("/v1/runner/connect"))
		if got != "https://old.internal/v1/runner/connect" {
			t.Fatalf("derived %q over an explicit value; every deployment that sets these four today would be "+
				"silently repointed at whatever PALAI_CONTROLLER_URL says", got)
		}
	})

	t.Run("a trailing slash does not double up", func(t *testing.T) {
		if got := joinPath("/v1/runner/enroll")(base + "/"); got != base+"/v1/runner/enroll" {
			t.Fatalf("joined to %q; an operator pasting an address with a trailing slash is not an error", got)
		}
	})

	// The id a machine SENDS is a label — the control plane mints the real one — so defaulting it costs a
	// confusing row at worst, never a wrong identity. Requiring it costs an operator at a terminal.
	t.Run("the machine names itself when nobody named it", func(t *testing.T) {
		t.Setenv("PALAI_RUNNER_ID", "")
		if got := defaultEnv("PALAI_RUNNER_ID", machineName); got == "" {
			t.Fatal("a machine with no configured id has no default name, so enrolment fails on a label")
		}
		t.Setenv("PALAI_RUNNER_ID", "explicit-name")
		if got := defaultEnv("PALAI_RUNNER_ID", machineName); got != "explicit-name" {
			t.Fatalf("id = %q, want the explicit value", got)
		}
	})
}
