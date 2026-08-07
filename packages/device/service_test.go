package device

import "testing"

// TestTheLaunchAgentGoesIntoTheHUMANSGuiDomain — `gui/0` IS NOT A DOMAIN, AND ROOT IS WHO ENROLS.
//
// Enrolment runs as root because it installs the account daemon. Root has no GUI session, so a
// LaunchAgent bootstrapped into gui/0 fails with "Bootstrap failed: 125: Domain does not support
// specified action" and the machine finishes ENROLLED WITH NO AGENT — measured on a real Mac on
// 2026-08-07. The agent belongs to the human who ran sudo, and that is also the only session where
// xcodebuild and simctl exist.
func TestTheLaunchAgentGoesIntoTheHUMANSGuiDomain(t *testing.T) {
	root := func() int { return 0 }
	for _, tc := range []struct {
		name    string
		sudoUID string
		want    int
	}{
		{"under sudo the human's uid wins", "501", 501},
		{"no sudo means the caller is the human", "", 0},
		{"a non-numeric value is not trusted", "nobody", 0},
		{"zero is not a GUI domain either", "0", 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := guiDomainUID(func(string) string { return tc.sudoUID }, root)
			if got != tc.want {
				t.Fatalf("guiDomainUID(SUDO_UID=%q) = %d, want %d", tc.sudoUID, got, tc.want)
			}
		})
	}
}
