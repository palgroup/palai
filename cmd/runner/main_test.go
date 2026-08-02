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
