package runner

// THE VERSION STAMP IN A PACKAGED AGENT, and the refusal that keeps it there.
//
// ‼️ THESE EXIST BECAUSE THE STAMP WAS MISSING AND NOTHING WENT RED. Until 2026-08-06 this packager
// compiled cmd/runner with neither -X version.Stamp nor build VCS info, so every installed agent
// resolved its own version to "dev". version.Supported is FAIL-OPEN for an unstamped build, which means
// the §48.2 support window — the one cmd/runner's own comment says it advertises its version for — could
// not fire on the artifact a fleet installs, no matter how far behind the machine was. The panel read one
// version for every Mac and a desired-version rollout had nothing to compare. Nothing was red: the gate
// that would have caught it is the one below, and it did not exist.
//
// The rule lives in two places by necessity — a shell pattern in build.sh, because the refusal must
// happen before a compile, and packages/version.IsRelease, because that is what the running system
// judges by. TestTheStampGateAgreesWithIsRelease is what binds them: it feeds the same table to both.

import (
	"archive/tar"
	"compress/gzip"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/palgroup/palai/packages/version"
)

// buildStamped runs the real packager with an explicit STAMP and reports whether it produced the
// archive. It returns the combined output so a refusal can be read rather than guessed at.
func buildStamped(t *testing.T, out, stamp string) (tarball string, output string, ok bool) {
	t.Helper()
	cmd := exec.Command("/usr/bin/env", "bash", "build.sh")
	cmd.Env = append(os.Environ(), "OUT="+out, "ARCH=amd64", "VERSION=18.0.0", "STAMP="+stamp)
	combined, err := cmd.CombinedOutput()
	// The tarball name is on stdout; CombinedOutput mixes it with the log, so locate the artifact on
	// disk instead — which is also the assertion a refusal needs.
	matches, _ := filepath.Glob(filepath.Join(out, "palai-*.tar.gz"))
	if len(matches) > 0 {
		tarball = matches[0]
	}
	return tarball, string(combined), err == nil
}

// extractMember returns one member's bytes from a .tar.gz — what an operator gets, rather than what the
// packager staged.
func extractMember(t *testing.T, path, member string) []byte {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		t.Fatalf("gzip open %s: %v", path, err)
	}
	tr := tar.NewReader(gz)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("tar read %s: %v", path, err)
		}
		if hdr.Name == member {
			b, err := io.ReadAll(tr)
			if err != nil {
				t.Fatalf("read member %s: %v", member, err)
			}
			return b
		}
	}
	t.Fatalf("%s has no member %q", path, member)
	return nil
}

// TestAPackagedAgentCarriesTheStampItWasBuiltWith is the property the fix restored: the stamp string
// reaches the shipped bytes. It uses a marker no other part of the binary could contain by accident —
// a coincidental match is what makes a substring assertion vacuous, and this tree has shipped one.
//
// The archive member, not the staging directory: what is measured is what an operator extracts.
func TestAPackagedAgentCarriesTheStampItWasBuiltWith(t *testing.T) {
	const marker = "18.0.0+gstamp-reaches-the-bytes"
	out := t.TempDir()
	tarball, output, ok := buildStamped(t, out, marker)
	if !ok {
		t.Fatalf("packager refused a release stamp %q:\n%s", marker, output)
	}

	binary := extractMember(t, tarball, "palai")
	if !strings.Contains(string(binary), marker) {
		t.Fatalf("the packaged `palai` does not contain its build stamp %q — it will report \"dev\", "+
			"and version.Supported skips the §48.2 window for an unstamped build on EVERY machine this "+
			"archive is installed on", marker)
	}
}

// TestTheStampGateAgreesWithIsRelease is the anti-drift binding. Two rules judge "is this a release
// version": a grep in build.sh and packages/version.IsRelease. If they diverge, either the packager
// refuses a stamp the window would have honoured, or — the direction that costs something — it ships an
// archive whose version the window cannot compare, which is the exact hole this file was written for.
func TestTheStampGateAgreesWithIsRelease(t *testing.T) {
	// Two accepted forms (each pays for a compile, so the accepted set is deliberately small) and the
	// unstamped shapes version.Resolve actually falls back to: "dev", a bare VCS revision, a dirty one,
	// and numbers too short to carry a minor.
	for _, stamp := range []string{"18.0.0", "v1.2.3-4-gabcdef", "dev", "a1b2c3d4e5f6", "a1b2c3d4-dirty", "18", "v"} {
		t.Run(stamp, func(t *testing.T) {
			out := t.TempDir()
			tarball, output, built := buildStamped(t, out, stamp)
			want := version.IsRelease(stamp)
			if built != want {
				t.Fatalf("packager built=%v for stamp %q, but version.IsRelease says %v — the two rules "+
					"have drifted apart:\n%s", built, stamp, want, output)
			}
			if !want && tarball != "" {
				t.Fatalf("the packager refused stamp %q yet left %s behind: a refusal that still writes "+
					"the artifact is not a refusal", stamp, tarball)
			}
		})
	}
}

// TestAnEmptyStampIsUnreachableRatherThanAccepted records why "" is absent from the table above: both
// VERSION and STAMP default when empty, so no invocation can reach the gate with one. Asserting it
// would measure the defaulting, not the gate.
func TestAnEmptyStampIsUnreachableRatherThanAccepted(t *testing.T) {
	out := t.TempDir()
	cmd := exec.Command("/usr/bin/env", "bash", "build.sh")
	cmd.Env = append(os.Environ(), "OUT="+out, "ARCH=amd64", "VERSION=", "STAMP=")
	combined, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("an empty STAMP must fall back to the default version, not refuse:\n%s", combined)
	}
	if !strings.Contains(string(combined), "0.1.0") {
		t.Fatalf("an empty VERSION did not fall back to the packager's default:\n%s", combined)
	}
}

// TestTheDarwinArchiveCarriesTheAccountsDaemonAndLinuxDoesNot is §3.3's sentence, measured: "The same
// package carries `palai-agentd`, but enabling it is one explicit administrator action."
//
// ‼️ IT DID NOT. Until 2026-08-06 the archive held `palai` alone, so a Mac with no checkout and no Go
// toolchain could not turn `accounts` isolation on at all: `agentd install` BUILDS the daemon from source
// and refuses without a source tree — the same defect §3.7 already deleted for the runner, still standing
// one binary over. A fleet Mac is exactly the machine that has neither.
//
// The two halves are asserted together because either alone is the wrong package: darwin must carry it,
// and linux must not — palai-agentd owns macOS session accounts and has no meaning on Linux, where it
// would be dead weight in every archive and a file somebody eventually wonders about.
func TestTheDarwinArchiveCarriesTheAccountsDaemonAndLinuxDoesNot(t *testing.T) {
	for _, tc := range []struct {
		os    string
		wants bool
	}{{"darwin", true}, {"linux", false}} {
		t.Run(tc.os, func(t *testing.T) {
			out := t.TempDir()
			cmd := exec.Command("/usr/bin/env", "bash", "build.sh")
			cmd.Env = append(os.Environ(), "OUT="+out, "ARCH=arm64", "VERSION=18.0.0", "STAMP=18.0.0", "OS="+tc.os)
			if combined, err := cmd.CombinedOutput(); err != nil {
				t.Fatalf("build.sh OS=%s: %v\n%s", tc.os, err, combined)
			}
			members := tarMembers(t, filepath.Join(out, "palai-18.0.0-"+tc.os+"-arm64.tar.gz"))
			if got := members["palai-agentd"]; got != tc.wants {
				if tc.wants {
					t.Fatalf("the %s archive carries no palai-agentd — a Mac with no source tree cannot enable "+
						"accounts isolation, which is the one administrator action the product has", tc.os)
				}
				t.Fatalf("the %s archive carries palai-agentd, which owns macOS session accounts and does "+
					"nothing here", tc.os)
			}
			// The device binary is in both, always: that is what install.sh places.
			if !members["palai"] {
				t.Fatalf("the %s archive has no `palai` member", tc.os)
			}
		})
	}
}

// TestTheInstallerPlacesThePayloadAndLoadsNoService is the other half of the same sentence, and the
// line it draws MOVED on 2026-08-07.
//
// It used to be "install.sh must not name palai-agentd at all", on the design that enabling accounts
// mode was a separate deliberate administrator action. That action is now `palai enroll` itself: one
// command enrols, installs the service and installs the daemon, because a rented Mac is provisioned
// unattended and a second privileged step nobody is there to type is a step that never happens.
//
// So the installer must PLACE the daemon binary — enrolment looks for it beside the agent and a clean
// install could not finish without it — and it must still LOAD nothing. Placing a file asks for no
// privilege; `launchctl bootstrap` and writing under /Library/LaunchDaemons are what turn a rootless
// install into a privileged one, and those stay in enrolment where an administrator decided.
func TestTheInstallerPlacesThePayloadAndLoadsNoService(t *testing.T) {
	body, err := os.ReadFile(filepath.Join("..", "..", "install", "install.sh"))
	if err != nil {
		t.Fatal(err)
	}
	script := string(body)
	if !strings.Contains(script, `install_binary "$work/palai" "$dest"`) {
		t.Fatal("install.sh no longer installs the `palai` member by name — this guard is reading a script " +
			"that has changed shape, and everything it concludes below is about a file it does not understand")
	}
	// The payload has to land, or enrolment cannot finish on a machine nobody logs into.
	if !strings.Contains(script, `install_binary "$work/palai-agentd"`) {
		t.Error("install.sh does not place palai-agentd beside the agent: `palai enroll` looks for it there, " +
			"so an unattended provision enrols a machine that cannot open a session account")
	}
	// And nothing here may load it. These are the verbs that would.
	for _, privileged := range []string{"launchctl", "LaunchDaemons", "sudo "} {
		if strings.Contains(script, privileged) {
			t.Errorf("install.sh contains %q — the default install stays rootless, and loading the daemon is "+
				"enrolment's one privileged step rather than something a `curl | sh` does silently", privileged)
		}
	}
}
