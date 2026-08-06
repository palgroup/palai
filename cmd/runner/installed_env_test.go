package main

// DoD 17'S OWN GATE: an INSTALLED agent reads ZERO `PALAI_` names.
//
// ‼️ THE ITEM SAYS "and the count is the gate", AND ITS LAST LINE SAID THE COUNT WAS "verified by
// INSPECTING the `installed != nil` branch of every device-facing helper". Inspection is what this
// repository keeps finding wrong six months later, and the thing being inspected is a branch somebody
// will add a line to. A device is installed by an autoscaler onto a machine nobody logs into, so an
// environment read that creeps back is not a bug an operator notices — it is a machine that silently
// takes its configuration from whatever the provisioner's shell happened to export.
//
// ‼️ THE POISON IS THE METHOD, AND THE POSITIVE CONTROL IS WHAT MAKES IT MEAN ANYTHING. Every `PALAI_`
// name the device tree mentions is set to a value that cannot occur naturally; the installed path must
// return none of it. Run that alone and a poison nothing reads would report the same clean result, so the
// SAME poison is run through the UNINSTALLED path first, which must be full of it.

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/palgroup/palai/packages/device"
)

const envPoison = "POISONED-BY-THE-ENVIRONMENT"

var palaiName = regexp.MustCompile(`"(PALAI_[A-Z0-9_]+)"`)

// deviceEnvNames is every PALAI_ name the device's own source mentions — read from the source rather than
// listed here, so a name added tomorrow is poisoned tomorrow.
func deviceEnvNames(t *testing.T) []string {
	t.Helper()
	cmd := exec.Command("git", "grep", "-h", "-oE", `"PALAI_[A-Z0-9_]+"`,
		"--", "cmd/runner", "packages/runner", "packages/device")
	// ‼️ FROM THE REPO ROOT. `git grep` searches the CURRENT DIRECTORY's subtree and this test runs in
	// cmd/runner, so two of the three pathspecs name nothing and it exits 1. The same mistake was made in
	// apps/control-plane/api's catalogue guard the same day; a sweep pointed at the wrong tree returns what
	// a sweep that found nothing returns.
	cmd.Dir = repoRootOf(t)
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("git grep: %v", err)
	}
	seen := map[string]bool{}
	var names []string
	for _, m := range palaiName.FindAllStringSubmatch(string(out), -1) {
		if !seen[m[1]] {
			seen[m[1]] = true
			names = append(names, m[1])
		}
	}
	if len(names) < 10 {
		t.Fatalf("only %d PALAI_ names were found in the device tree — the scan is reading the wrong files "+
			"and a poison nobody sets proves nothing", len(names))
	}
	return names
}

// fatalOnAPoisonedValue are the names whose reader treats the value as a FILE and calls log.Fatal when it
// cannot be read or parsed.
//
// ‼️ THEY ARE EXCLUDED BECAUSE POISONING THEM MEASURES THE LOADER, NOT THE READER. The first run of this
// test set PALAI_CONTROLLER_CA to the poison, and the uninstalled path did exactly what it should — read
// it as a path, fail to open it, and log.Fatal — which kills the test process before any assertion. A
// positive control that cannot survive its own control is not a control. The installed path's refusal to
// build a CA pool from the environment is asserted directly instead, below.
var fatalOnAPoisonedValue = map[string]bool{
	"PALAI_CONTROLLER_CA":         true,
	"PALAI_ENROLLMENT_TOKEN_FILE": true,
	"PALAI_RUNNER_PRIVATE_KEY":    true,
}

// poisonTheEnvironment sets every device-facing name to a value that cannot come from anywhere else.
func poisonTheEnvironment(t *testing.T) {
	t.Helper()
	poisoned := 0
	for _, name := range deviceEnvNames(t) {
		if fatalOnAPoisonedValue[name] {
			continue
		}
		t.Setenv(name, envPoison)
		poisoned++
	}
	if poisoned < 10 {
		t.Fatalf("only %d names were poisoned — the exclusion list has swallowed the corpus", poisoned)
	}
}

// installedFixture is a machine that has been through `enroll`: a config on disk, a key file, and the
// paths the agent derives everything else from.
func installedFixture(t *testing.T) *device.Installation {
	t.Helper()
	home := t.TempDir()
	paths := device.DefaultPaths("linux", home, func(string) string { return "" })
	for _, dir := range []string{filepath.Dir(paths.ConfigFile), filepath.Dir(paths.DeviceKeyFile), paths.WorkspaceRoot} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	keyFile := filepath.Join(home, "pool-key")
	if err := os.WriteFile(keyFile, []byte("rpk_live_fixture"), 0o600); err != nil {
		t.Fatal(err)
	}
	return &device.Installation{
		Paths: paths,
		Config: device.Config{
			ControllerURL:     "https://controller.fixture.internal:8443",
			EnrollmentKeyFile: keyFile,
		},
	}
}

// answers collects every value the installed path hands back, as one string to scan.
func answers(installed *device.Installation) string {
	bootstrap, tokenFile, sessionURL, renewURL, settingsURL, controllerDNS, _ := loadConfig(installed)
	return strings.Join([]string{
		bootstrap.RunnerID, bootstrap.RunnerDNS, bootstrap.EnrollmentToken, bootstrap.EnrollmentURL,
		bootstrap.ControllerDNS, bootstrap.PoolID, bootstrap.Posture,
		tokenFile, sessionURL, renewURL, settingsURL, controllerDNS,
		workspaceRoot(installed), settingsInterval(installed).String(),
	}, "\n")
}

// TestAnInstalledAgentReadsNOTHINGFromItsEnvironment is DoD 17, measured.
func TestAnInstalledAgentReadsNOTHINGFromItsEnvironment(t *testing.T) {
	poisonTheEnvironment(t)

	// THE POSITIVE CONTROL FIRST. With no installation the compose path reads the environment, so the
	// poison MUST come back — otherwise the assertion below is about a poison nothing consumes.
	if got := answers(nil); !strings.Contains(got, envPoison) {
		t.Fatalf("the UNINSTALLED path returned no poisoned value, so the poison reaches no reader and this "+
			"test proves nothing about the installed one:\n%s", got)
	}

	installed := installedFixture(t)
	if got := answers(installed); strings.Contains(got, envPoison) {
		t.Errorf("an INSTALLED agent took a value from its environment:\n%s\n\nDoD 17 is that it reads zero "+
			"PALAI_ names — a device is installed by an autoscaler onto a machine nobody logs into, so a "+
			"reader that creeps back here is a machine configured by whatever the provisioner exported", got)
	}
	// ‼️ A BOOLEAN READER IS INVISIBLE TO A POISON THAT IS NOT ITS ON-VALUE, and this test passed a leak
	// because of it: re-adding `os.Getenv("PALAI_WORKSPACE_UNSAFE_BIND") == "1"` to the installed branch
	// left it GREEN, since the poison string is not "1". The property HAD broken — an installed device
	// would have honoured the box's own opt-in — so the test was the guilty one. A boolean is poisoned with
	// the value that turns it ON.
	for _, name := range deviceEnvNames(t) {
		if !fatalOnAPoisonedValue[name] {
			t.Setenv(name, "1")
		}
	}
	if allowUnsafeBind(installed) {
		t.Error("an installed device opted into a lease's unsafe local bind from PALAI_WORKSPACE_UNSAFE_BIND — " +
			"that yes exists so a control plane ALONE cannot mount an arbitrary host path, and on a device it " +
			"could only be typed as an environment variable on the box")
	}
	if _, _, _, _, _, _, pool := loadConfig(installed); pool != nil && installed.CAs == nil {
		t.Error("an installed device built a CA pool from the environment: the trust anchor is the one its " +
			"config named, or the host's root store")
	}
}

// TestTheInstalledPathAnswersFromTheInstallationRatherThanADefault keeps the guard above from passing
// because the installed path returns EMPTY for everything — which would also contain no poison.
func TestTheInstalledPathAnswersFromTheInstallationRatherThanADefault(t *testing.T) {
	poisonTheEnvironment(t)
	installed := installedFixture(t)

	if got := workspaceRoot(installed); got != installed.Paths.WorkspaceRoot {
		t.Errorf("workspaceRoot = %q, want the installation's own %q", got, installed.Paths.WorkspaceRoot)
	}
	_, _, sessionURL, _, _, controllerDNS, _ := loadConfig(installed)
	if !strings.HasPrefix(sessionURL, "wss://controller.fixture.internal:8443") {
		t.Errorf("session URL = %q, want it derived from the installation's controller_url", sessionURL)
	}
	if controllerDNS != "controller.fixture.internal" {
		t.Errorf("controller identity = %q, want the host the config named", controllerDNS)
	}
	if d := settingsInterval(installed); d != 0 {
		t.Errorf("settingsInterval = %v, want 0 so packages/runner applies the package default", d)
	}
}
