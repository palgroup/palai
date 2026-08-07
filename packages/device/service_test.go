package device

import (
	"os"
	"strconv"
	"strings"
	"testing"
)

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
			got := GUIDomainUID(func(string) string { return tc.sudoUID }, root)
			if got != tc.want {
				t.Fatalf("GUIDomainUID(SUDO_UID=%q) = %d, want %d", tc.sudoUID, got, tc.want)
			}
		})
	}
}

// TestRootReachesTheUsersSessionThroughAsuser — ROOT IS IN NO GUI SESSION, AND launchd SAYS SO WITH AN
// I/O ERROR.
//
// Enrolment must be root to install the account daemon, and the agent must load into the human's GUI
// domain to reach xcodebuild. Those two are only compatible through `launchctl asuser <uid> launchctl
// …`, which joins that user's bootstrap namespace first. Without it a root bootstrap into gui/501
// answers "Bootstrap failed: 5: Input/output error" — measured on a real Mac on 2026-08-07, one fix
// after the domain itself was corrected from gui/0.
func TestRootReachesTheUsersSessionThroughAsuser(t *testing.T) {
	// A process already inside the session calls launchctl directly; anything else has to ask.
	if got := launchctlFor(os.Geteuid()); len(got) != 1 || got[0] != "launchctl" {
		t.Fatalf("launchctlFor(self) = %v, want the plain form — a process in its own session must not "+
			"round-trip through asuser", got)
	}
	other := os.Geteuid() + 1
	got := launchctlFor(other)
	want := []string{"launchctl", "asuser", strconv.Itoa(other), "launchctl"}
	if len(got) != len(want) {
		t.Fatalf("launchctlFor(%d) = %v, want %v", other, got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("launchctlFor(%d) = %v, want %v", other, got, want)
		}
	}
}

// TestTheAgentPlistUsesTheONLYBooleanFormLaunchdAccepts — `plutil` SAYS BOTH ARE VALID AND launchd
// ACCEPTS ONE.
//
// encoding/xml writes an empty element as `<true></true>`. launchd refuses that file with
// `Bootstrap failed: 5: Input/output error` — an errno, not a sentence, so nothing in the message says
// "your plist is malformed". Every agent this package rendered was therefore unloadable on macOS, and
// it survived because `plutil -lint` calls the long form valid: the file passes every check an author
// would run and is rejected by the only reader that matters.
//
// It cost three fixes to reach: the domain was wrong (gui/0), then the namespace was wrong (no asuser),
// and the SAME errno came back both times. deploy/macos's hand-written daemon plist uses `<true/>`,
// which is exactly why the daemon installed while the agent did not.
func TestTheAgentPlistUsesTheONLYBooleanFormLaunchdAccepts(t *testing.T) {
	body, err := RenderLaunchAgent(ServiceSpec{
		Executable: "/usr/local/bin/palai",
		ConfigFile: "/tmp/agent.json",
		LogFile:    "/tmp/agent.log",
	})
	if err != nil {
		t.Fatalf("RenderLaunchAgent: %v", err)
	}
	rendered := string(body)
	for _, refused := range []string{"<true></true>", "<false></false>"} {
		if strings.Contains(rendered, refused) {
			t.Errorf("the rendered agent carries %s — launchd answers errno 5 and the machine finishes "+
				"enrolled with no agent running:\n%s", refused, rendered)
		}
	}
	// NON-VACUITY: a renderer that stopped emitting booleans at all would pass the loop above while
	// dropping RunAtLoad and KeepAlive, which is how an agent stops coming back after a reboot.
	for _, needed := range []string{"<key>RunAtLoad</key>", "<key>KeepAlive</key>", "<true/>"} {
		if !strings.Contains(rendered, needed) {
			t.Errorf("the rendered agent is missing %s — it would load once and never return:\n%s", needed, rendered)
		}
	}
}
