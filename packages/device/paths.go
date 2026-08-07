// Package device holds the one-time INSTALL half of a Palai agent: where a machine keeps its
// configuration and its durable identity, how both are written so no other account can read them, and
// how the service that runs the agent is registered with the platform.
//
// ‼️ THE RUNNING AGENT READS NOTHING FROM THE ENVIRONMENT, AND THIS PACKAGE IS WHY. Before it, a device
// was configured by twenty-six `PALAI_` names that had to be present on every start — measured
// 2026-08-05, `grep -rhoE 'PALAI_[A-Z_]+' cmd/runner/ packages/runner/ | sort -u | wc -l` → 26 — which
// makes the machine, not the admin plane, the thing an operator edits. `enroll` runs once, writes what
// is here, and the agent reads it from disk on every later start.
//
// ‼️ AND THE SECRET IS NEVER COPIED. Config records the PATH to the pool key, never its bytes: a key
// that exists in one file an operator placed is a key an operator can rotate by replacing that file,
// while a key copied into a config is a second copy nobody remembers.
package device

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
)

// Paths is where one installed device keeps its four durable things. It is a struct rather than four
// functions because a test has to be able to point all of them at one temporary directory, and a
// package that reads $HOME at call time cannot be driven that way.
type Paths struct {
	// ConfigFile holds the controller address and the path to the pool key. Mode 0600.
	ConfigFile string
	// DeviceKeyFile holds the private half of the device keypair — THE identity. It is written exactly
	// once, at first enrolment, and every later certificate is issued to the same key. Mode 0600.
	DeviceKeyFile string
	// IdentityFile holds the runner id the control plane minted and the certificate it issued. Unlike
	// DeviceKeyFile it is rewritten on every renewal, and an EXPIRED one is still useful: it names the
	// id whose recovery the device asks for. Mode 0600.
	IdentityFile string
	// ServiceFile is the platform's unit/plist for the agent. It is not a secret and is not 0600.
	ServiceFile string
	// LogFile is where the service manager is told to send the agent's output.
	LogFile string
	// WorkspaceRoot is the directory every leased session's workspace is allocated under.
	//
	// ‼️ IT IS THE DEVICE'S OWN PROPERTY AND NOT THE ADMIN PLANE'S, and the control plane says so in the
	// refusal it gives when someone tries: "naming the host directory every coding workspace is allocated
	// under, from a web form, is a filesystem write primitive wearing a settings control." That refusal is
	// right, which is why this is here: the machine knows where its own disk is, and an installed device
	// has no operator to set an environment variable for it.
	WorkspaceRoot string
}

// StateDir is the directory the three 0600 files live in. It is derived rather than stored so there is
// one answer to "where does the identity live" and not two that can disagree.
func (p Paths) StateDir() string { return filepath.Dir(p.DeviceKeyFile) }

// ConfigDir is the directory the config lives in. On macOS it is the same directory as StateDir; on
// Linux the XDG split puts them apart, which is the reason this is a method and not an assumption.
func (p Paths) ConfigDir() string { return filepath.Dir(p.ConfigFile) }

// ServiceLabel is the name the platform knows the agent by. One constant, used by the plist's Label, the
// systemd unit's file name and every `launchctl`/`systemctl` argument — because a service installed
// under one name and addressed under another is a service that looks installed and is unreachable.
const ServiceLabel = "net.palai.agent"

// SystemdUnitName is the systemd counterpart of ServiceLabel. systemd units are addressed by file name,
// and a name with dots in it reads as a template to a hurried operator, so it is spelled out separately
// rather than derived from the label.
const SystemdUnitName = "palai-agent.service"

// DefaultPaths answers where this platform's files go. `goos` and `home` are parameters so a test can
// measure the macOS layout on Linux and back — the layouts are the product decision (plan §3.2) and a
// function that could only be checked on the platform it describes would be checked on neither.
//
// THE LINUX SPLIT IS XDG'S AND IS READ FROM THE ENVIRONMENT, because a distribution that sets
// XDG_STATE_HOME and a distribution that does not must both end up with one directory the agent can
// find — and `env` is a parameter for the same reason `home` is.
func DefaultPaths(goos, home string, env func(string) string) Paths {
	if env == nil {
		env = os.Getenv
	}
	if goos == "darwin" {
		support := filepath.Join(home, "Library", "Application Support", "Palai")
		return Paths{
			ConfigFile:    filepath.Join(support, "agent.json"),
			DeviceKeyFile: filepath.Join(support, "device.key"),
			IdentityFile:  filepath.Join(support, "identity.json"),
			ServiceFile:   filepath.Join(home, "Library", "LaunchAgents", ServiceLabel+".plist"),
			LogFile:       filepath.Join(home, "Library", "Logs", "Palai", "agent.log"),
			WorkspaceRoot: filepath.Join(support, "workspaces"),
		}
	}
	configHome := env("XDG_CONFIG_HOME")
	if configHome == "" {
		configHome = filepath.Join(home, ".config")
	}
	stateHome := env("XDG_STATE_HOME")
	if stateHome == "" {
		stateHome = filepath.Join(home, ".local", "state")
	}
	state := filepath.Join(stateHome, "palai")
	return Paths{
		ConfigFile:    filepath.Join(configHome, "palai", "agent.json"),
		DeviceKeyFile: filepath.Join(state, "device.key"),
		IdentityFile:  filepath.Join(state, "identity.json"),
		ServiceFile:   filepath.Join(configHome, "systemd", "user", SystemdUnitName),
		LogFile:       filepath.Join(state, "agent.log"),
		WorkspaceRoot: filepath.Join(state, "workspaces"),
	}
}

// HostPaths is DefaultPaths for the machine this process is on. It fails rather than guessing a home
// directory: a device that wrote its identity to a directory nobody chose would come back after a
// reboot as a machine with no identity, which is the exact failure this package exists to remove.
func HostPaths() (Paths, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return Paths{}, err
	}
	return DefaultPaths(runtime.GOOS, home, os.Getenv), nil
}

// HandOverTo gives every file this enrolment wrote to the account that will READ them.
//
// ENROLMENT RUNS AS root — it must, to install the account daemon — and the agent runs as the human in
// their GUI session. Root-owned files in that human's Application Support directory are not a
// permissions nicety: the agent loads, cannot stat its own config, and crash-loops with
// "read device configuration: permission denied" while `launchctl` reports it started. Measured on a
// real Mac on 2026-08-07.
//
// It walks rather than naming the four files, because the next file added under this directory would
// otherwise be the next one root keeps. A failure on any single entry is reported with its path: a
// partial hand-over leaves the agent unable to start, which is the state this exists to prevent, and
// silently ignoring it would produce exactly the crash-loop above one file later.
func HandOverTo(paths Paths, uid, gid int) error {
	roots := map[string]struct{}{}
	for _, f := range []string{paths.ConfigFile, paths.IdentityFile, paths.DeviceKeyFile, paths.LogFile, paths.WorkspaceRoot} {
		if f != "" {
			roots[filepath.Dir(f)] = struct{}{}
		}
	}
	for dir := range roots {
		if err := filepath.WalkDir(dir, func(path string, _ fs.DirEntry, err error) error {
			if err != nil {
				if os.IsNotExist(err) {
					return nil
				}
				return err
			}
			if err := os.Chown(path, uid, gid); err != nil {
				return fmt.Errorf("hand %s to uid %d: %w", path, uid, err)
			}
			return nil
		}); err != nil {
			return err
		}
	}
	return nil
}
