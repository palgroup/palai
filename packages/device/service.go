package device

import (
	"encoding/xml"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
)

// ServiceSpec is everything a platform needs to run the agent unattended: which binary, with which
// config, writing where.
//
// ‼️ THE UNIT NAMES A CONFIG PATH AND NO CREDENTIAL. Nothing here carries the pool key, and nothing here
// sets a `PALAI_` variable — a unit file that exported a secret would put it in the process environment,
// which this tree measured on 2026-08-04 to be readable by `ps -E -p <pid>` with values, and NOT hidden
// by os.Unsetenv because macOS serves ps from the kernel's start-time copy. A service file is also
// world-readable by design, so a secret in one is a secret in a file every account can open.
type ServiceSpec struct {
	// Executable is the agent binary the service manager runs. `enroll` fills it from os.Executable so a
	// device that was installed from a tarball and one installed from a package both point at themselves.
	Executable string
	// ConfigFile is passed as an argument rather than an environment variable, for the reason above and
	// for one more: an argument is visible in `launchctl print` and `systemctl cat`, so an operator
	// debugging a machine can see which config the running agent is on.
	ConfigFile string
	LogFile    string
}

// InstalledService is what Install wrote and what it did with it, returned so the caller can report
// facts rather than intentions.
//
// ‼️ `Started` IS "THE SERVICE MANAGER ACCEPTED THE UNIT AND THE AGENT IS RUNNING", AND IT IS NOT
// "IT WILL COME BACK AFTER A REBOOT". The reboot property is what the unit's RunAtLoad/WantedBy is FOR,
// but this package has not measured it and neither has any test in this task — plan Milestone A0 owns
// that leg and it needs a real machine to power-cycle. A field called `SurvivesReboot` would be a claim
// nobody made.
type InstalledService struct {
	Path    string
	Loaded  bool
	Started bool
}

// launchAgentPlist is the macOS user-session unit. Two keys carry the behaviour:
//
//	RunAtLoad  — start when the agent is loaded, which for a LaunchAgent is at graphical login.
//	KeepAlive  — restart the process if it exits, so a crashed agent does not leave a machine parked.
//
// It is generated through encoding/xml rather than a fmt.Sprintf template, because a path containing an
// `&` or a `<` — a directory named `R&D`, which is an ordinary thing to have — produces a plist that
// launchd refuses to parse, and the failure surfaces as a machine that silently never starts.
type launchAgentPlist struct {
	XMLName xml.Name `xml:"plist"`
	Version string   `xml:"version,attr"`
	Dict    plistDict
}

type plistDict struct {
	XMLName xml.Name `xml:"dict"`
	Entries []any
}

type plistKey struct {
	XMLName xml.Name `xml:"key"`
	Value   string   `xml:",chardata"`
}

type plistString struct {
	XMLName xml.Name `xml:"string"`
	Value   string   `xml:",chardata"`
}

type plistTrue struct {
	XMLName xml.Name `xml:"true"`
}

type plistArray struct {
	XMLName xml.Name `xml:"array"`
	Items   []plistString
}

// RenderLaunchAgent returns the plist bytes for a macOS user-session agent.
func RenderLaunchAgent(spec ServiceSpec) ([]byte, error) {
	document := launchAgentPlist{Version: "1.0", Dict: plistDict{Entries: []any{
		plistKey{Value: "Label"}, plistString{Value: ServiceLabel},
		plistKey{Value: "ProgramArguments"}, plistArray{Items: []plistString{
			{Value: spec.Executable}, {Value: "--config"}, {Value: spec.ConfigFile},
		}},
		plistKey{Value: "RunAtLoad"}, plistTrue{},
		plistKey{Value: "KeepAlive"}, plistTrue{},
		plistKey{Value: "StandardOutPath"}, plistString{Value: spec.LogFile},
		plistKey{Value: "StandardErrorPath"}, plistString{Value: spec.LogFile},
	}}}
	body, err := xml.MarshalIndent(document, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("render launch agent: %w", err)
	}
	header := `<?xml version="1.0" encoding="UTF-8"?>` + "\n" +
		`<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">` + "\n"
	return append([]byte(header), append(body, '\n')...), nil
}

// RenderSystemdUnit returns the systemd USER unit for the agent.
//
// USER RATHER THAN SYSTEM, which is plan §3.2's row and not a convenience: the device package must
// install without root, and a system unit needs it. The cost is that a user unit stops when the user
// logs out, which `loginctl enable-linger` is exactly the fix for — Install runs it, and a machine where
// it fails is reported rather than assumed.
//
// Restart=always with a delay for the reason KeepAlive exists on the Mac: an agent that exits because the
// control plane was briefly unreachable must come back, and a machine that retries instantly against a
// plane that is down is a machine hammering it.
func RenderSystemdUnit(spec ServiceSpec) []byte {
	return []byte(strings.Join([]string{
		"[Unit]",
		"Description=Palai device agent",
		"After=network-online.target",
		"Wants=network-online.target",
		"",
		"[Service]",
		"Type=simple",
		"ExecStart=" + spec.Executable + " --config " + spec.ConfigFile,
		"Restart=always",
		"RestartSec=5",
		"",
		"[Install]",
		"WantedBy=default.target",
		"",
	}, "\n"))
}

// runner is how Install reaches `launchctl` / `systemctl`. It is a field rather than a direct call to
// os/exec so a test can drive the whole install — the same file writing, the same argument vectors, the
// same ordering — on a machine that has neither, and assert on what WOULD have been run.
type runner func(name string, args ...string) error

func execRunner(name string, args ...string) error {
	command := exec.Command(name, args...)
	output, err := command.CombinedOutput()
	if err != nil {
		return fmt.Errorf("%s %s: %w: %s", name, strings.Join(args, " "), err, strings.TrimSpace(string(output)))
	}
	return nil
}

// Install writes the platform's service file and asks the service manager to load and start it.
//
// ‼️ WHAT IT PROVES IS "WRITTEN, LOADED, RUNNING". The reboot behaviour these units declare is not
// measured here and is not claimed anywhere in this package — plan Milestone A0 owns it and it needs a
// machine that can be powered off.
//
// A FAILED LOAD IS REPORTED, NOT SWALLOWED. `enroll` has already spent a pool key and written an
// identity by the time this runs, so a machine whose service manager refused the unit is a machine that
// IS enrolled and will not come back by itself — an operator has to read that sentence, and a silent
// success would be the "declared but never fires" shape this repository keeps finding.
func Install(goos string, paths Paths, spec ServiceSpec) (InstalledService, error) {
	return install(goos, paths, spec, execRunner)
}

func install(goos string, paths Paths, spec ServiceSpec, run runner) (InstalledService, error) {
	result := InstalledService{Path: paths.ServiceFile}
	if err := os.MkdirAll(filepath.Dir(paths.ServiceFile), 0o755); err != nil {
		return result, fmt.Errorf("create service directory: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(paths.LogFile), 0o755); err != nil {
		return result, fmt.Errorf("create log directory: %w", err)
	}

	switch goos {
	case "darwin":
		body, err := RenderLaunchAgent(spec)
		if err != nil {
			return result, err
		}
		if err := os.WriteFile(paths.ServiceFile, body, 0o644); err != nil {
			return result, fmt.Errorf("write %s: %w", paths.ServiceFile, err)
		}
		// A PREVIOUS LOAD IS EVICTED FIRST AND ITS FAILURE IS IGNORED, which is the one ignored error in
		// this function and the reason is that "there was nothing loaded" and "the eviction failed" are
		// the same exit status from launchctl. Not evicting is worse: bootstrap onto an already-bootstrapped
		// label fails with "service already loaded" and a re-run of `enroll` would refuse on a machine
		// that is simply already installed.
		domain := fmt.Sprintf("gui/%d", guiDomainUID(os.Getenv, os.Getuid))
		_ = run("launchctl", "bootout", domain+"/"+ServiceLabel)
		if err := run("launchctl", "bootstrap", domain, paths.ServiceFile); err != nil {
			return result, fmt.Errorf("load launch agent: %w", err)
		}
		result.Loaded = true
		if err := run("launchctl", "kickstart", "-k", domain+"/"+ServiceLabel); err != nil {
			return result, fmt.Errorf("start launch agent: %w", err)
		}
		result.Started = true
		return result, nil
	case "linux":
		if err := os.WriteFile(paths.ServiceFile, RenderSystemdUnit(spec), 0o644); err != nil {
			return result, fmt.Errorf("write %s: %w", paths.ServiceFile, err)
		}
		if err := run("systemctl", "--user", "daemon-reload"); err != nil {
			return result, fmt.Errorf("reload systemd user manager: %w", err)
		}
		result.Loaded = true
		// LINGER BEFORE ENABLE, and the order is the property: without linger the user manager stops at
		// logout, so `enable --now` on a machine that then logs out leaves a unit that is enabled and a
		// manager that is gone. A failure here is FATAL rather than a warning, because a device that only
		// runs while somebody is logged in is not a fleet machine.
		if err := run("loginctl", "enable-linger"); err != nil {
			return result, fmt.Errorf("enable systemd linger: %w", err)
		}
		if err := run("systemctl", "--user", "enable", "--now", SystemdUnitName); err != nil {
			return result, fmt.Errorf("start systemd user unit: %w", err)
		}
		result.Started = true
		return result, nil
	default:
		return result, fmt.Errorf("no service manager is implemented for %s; the agent can still be run in the foreground with --config %s", goos, spec.ConfigFile)
	}
}

// guiDomainUID is the uid whose GUI domain this machine's LaunchAgent belongs in.
//
// UNDER sudo IT IS NOT THE CALLER'S uid, and that difference is a whole enrolment. `palai enroll` runs
// as root — it must, to install the account daemon — and root has no GUI session, so bootstrapping into
// `gui/0` answers "Bootstrap failed: 125: Domain does not support specified action" and the machine
// finishes enrolled with no agent running. The agent belongs to the human who ran sudo, and that is the
// same session the toolchain needs: an agent outside a GUI domain cannot reach xcodebuild or simctl.
//
// SUDO_UID is the only thing that carries that identity into a root process. It is read defensively —
// a non-numeric or zero value falls back to the real uid rather than guessing — because this decides
// which domain a service is loaded into and a wrong answer is silent until somebody looks for the agent.
func guiDomainUID(getenv func(string) string, getuid func() int) int {
	if uid, err := strconv.Atoi(strings.TrimSpace(getenv("SUDO_UID"))); err == nil && uid > 0 {
		return uid
	}
	return getuid()
}
