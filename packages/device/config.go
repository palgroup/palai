package device

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// Config is everything the RUNNING agent is told by the machine it is installed on. Three fields, and
// two of them are paths — that is the whole point of the type: the device's runtime input is an address
// and a pointer to a file, not a credential and not a fleet's worth of tuning.
//
// WHAT IS NOT HERE IS THE CONTRACT. There is no pool id, no posture, no capacity, no runner id, no DNS
// name and no four derived URLs. The key chooses the pool, the binary measures the machine, the admin
// plane sets concurrency, and the control plane mints the identity (plan §3.3, §3.4, §3.6). A field
// added here is an operator input added to every machine in the fleet, so each one has to earn it.
type Config struct {
	// ControllerURL is the ONE address. Every runner-plane route is derived from it.
	ControllerURL string `json:"controller_url"`
	// EnrollmentKeyFile is the PATH to the pool key, never the key. The distinction is the reason this
	// file can be 0600-and-readable-by-the-agent while the key stays whatever the provisioner made it:
	// a rotation replaces one file, and no stale copy survives in a config nobody re-reads.
	EnrollmentKeyFile string `json:"enrollment_key_file"`
	// ServerName is the identity on the controller's certificate, when it differs from the address.
	//
	// ‼️ THE ADDRESS AND THE IDENTITY ARE DIFFERENT QUESTIONS, and a self-hosted plane is where they come
	// apart: the device is reachable at 127.0.0.1 or at an internal IP, while the certificate names the
	// service. Verifying against the name lets the runner keep its strictest rule — the certificate names
	// exactly this controller and nobody else — instead of admitting a second identity to make an address
	// fit, which is a rule that can no longer tell a sibling name from an attacker's.
	//
	// EMPTY IS THE NORMAL CASE. A publicly trusted gateway is reached at the name on its certificate, so
	// there is nothing to say and nothing to keep in step.
	ServerName string `json:"server_name,omitempty"`
	// ControllerCAFile is the additional trust anchor for a PRIVATE deployment, and empty is the normal
	// case rather than a degraded one. A publicly trusted runner gateway needs nothing here: the system
	// roots verify it, which is DoD item 2 and the reason PALAI_CONTROLLER_CA stopped being required.
	ControllerCAFile string `json:"controller_ca_file,omitempty"`
	// WorkspaceRoot is the directory this machine opens session workspaces under, when the DEPLOYMENT
	// chose one. Empty means the per-platform default in Paths.
	//
	// ‼️ IT EXISTS BECAUSE THE CONTROL PLANE AND THIS AGENT MUST NAME THE SAME DIRECTORY. The
	// co-located posture (one Mac running both) has the control plane hand out allocation paths under
	// its own PALAI_WORKSPACE_ROOT, and the agent refuses any path outside its allocation root — so two
	// answers to one question is a run that fails with "workspace path is outside the runner allocation
	// root". It was two answers the moment enrolment became independent of the bring-up: an enrolled
	// device used its platform default and could not be told otherwise, because the env var it would
	// have read is not inherited by a LaunchAgent.
	WorkspaceRoot string `json:"workspace_root,omitempty"`
	// ReleaseBaseURL is where this machine fetches a new version from when the plane names one. Empty
	// means this machine refuses to update rather than guessing a host — an air-gapped fleet mirrors its
	// releases, and a machine that reached out to the internet because nobody told it not to is the
	// opposite of what an air gap is for.
	ReleaseBaseURL string `json:"release_base_url,omitempty"`
}

// ErrUnsafePermissions is what a file readable by anyone but its owner is refused with. It is a distinct
// error rather than a string because the ORDER matters and a test has to be able to assert on it: the
// refusal happens before any network call, so a machine whose key is world-readable never presents that
// key at all (plan §T2 RED, item 4).
var ErrUnsafePermissions = errors.New("file is readable by accounts other than its owner")

// RequireOwnerOnly refuses a file any group or other account can read, write or execute.
//
// ‼️ IT IS CALLED BEFORE THE NETWORK, NOT AFTER. A key checked after enrolment is a key that has already
// been spent from a file the whole machine could read; the check is only worth having at the moment it
// can still prevent that. The same function guards the config and the device key, because an identity
// another account can copy is an identity another account can BE.
//
// A SYMLINK IS RESOLVED FIRST. os.Stat follows the link, so what is measured is the mode of the bytes
// that will be read rather than the 0777 every symlink carries — a check on Lstat would pass every
// symlink in existence and refuse nothing.
func RequireOwnerOnly(path string) error {
	info, err := os.Stat(path)
	if err != nil {
		return err
	}
	if mode := info.Mode().Perm(); mode&0o077 != 0 {
		return fmt.Errorf("%s: %w (mode %04o)", path, ErrUnsafePermissions, mode)
	}
	return nil
}

// WriteOwnerOnly writes bytes atomically at mode 0600.
//
// ATOMIC MEANS THE READER NEVER SEES A HALF FILE, and on this path that is not a nicety. The device key
// is written once and read on every later start; a process killed mid-write would leave a machine that
// enrols as a NEW device forever after, silently, because a truncated key parses as no key. Write to a
// sibling temporary and rename: rename within a directory is atomic on both platforms this ships to.
//
// THE TEMPORARY IS CREATED 0600 RATHER THAN CHMODDED, so there is no instant at which the bytes exist
// under a wider mode. os.CreateTemp makes 0600 already; the explicit Chmod after it is for a umask that
// could not have widened it and costs nothing to keep honest.
func WriteOwnerOnly(path string, data []byte) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create %s: %w", dir, err)
	}
	tmp, err := os.CreateTemp(dir, ".palai-*")
	if err != nil {
		return fmt.Errorf("create temporary in %s: %w", dir, err)
	}
	name := tmp.Name()
	defer func() { _ = os.Remove(name) }()
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("restrict %s: %w", name, err)
	}
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("write %s: %w", name, err)
	}
	// Sync BEFORE rename. A rename that lands before the bytes do is exactly the half-file this function
	// exists to prevent, and on a power cut that is not a hypothetical ordering.
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("sync %s: %w", name, err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close %s: %w", name, err)
	}
	if err := os.Rename(name, path); err != nil {
		return fmt.Errorf("install %s: %w", path, err)
	}
	return nil
}

// Save writes the config atomically at 0600.
func (c Config) Save(path string) error {
	encoded, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return fmt.Errorf("encode agent config: %w", err)
	}
	return WriteOwnerOnly(path, append(encoded, '\n'))
}

// LoadConfig reads the config and refuses it if any other account can read it.
//
// THE PERMISSION CHECK IS PART OF LOADING RATHER THAN A SEPARATE CALL A CALLER MIGHT FORGET. This tree
// has a standing record of guards that exist and are never reached; a loader that returns a config
// without having checked its mode is that shape, one layer down.
func LoadConfig(path string) (Config, error) {
	if err := RequireOwnerOnly(path); err != nil {
		return Config{}, err
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return Config{}, err
	}
	decoder := json.NewDecoder(strings.NewReader(string(raw)))
	// An unknown field is a REFUSAL and not a shrug: this file is written by `enroll` and read by the
	// agent, so a field neither of them knows is either a hand edit that will not take effect or a config
	// from a newer agent than this binary. Both are worth a message rather than a silent default.
	decoder.DisallowUnknownFields()
	var config Config
	if err := decoder.Decode(&config); err != nil {
		return Config{}, fmt.Errorf("decode %s: %w", path, err)
	}
	if config.ControllerURL == "" {
		return Config{}, fmt.Errorf("%s names no controller_url; re-run `palai enroll`", path)
	}
	return config, nil
}

// ReadEnrollmentKey reads the pool key, refusing a file other accounts can read BEFORE reading it.
//
// The returned value is the caller's to spend and drop. Nothing in this package retains it, nothing
// writes it to the config, and the only copy on the machine stays the file the provisioner placed —
// which is what makes "rotate the key" mean "replace one file" rather than "find every copy".
func ReadEnrollmentKey(path string) (string, error) {
	if err := RequireOwnerOnly(path); err != nil {
		return "", err
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	key := strings.TrimSpace(string(raw))
	if key == "" {
		return "", fmt.Errorf("%s is empty", path)
	}
	return key, nil
}

// ensureDir creates a directory at 0700, and TIGHTENS one that already exists.
//
// The tightening is the half that matters: MkdirAll leaves an existing directory's mode alone, so a
// state directory that was 0755 before this package existed would stay 0755 and every 0600 file in it
// would still be LISTABLE by other accounts. Listing is not reading, but it names the identity file to
// anyone who asks, and the fix is one syscall.
func ensureDir(path string) error {
	if err := os.MkdirAll(path, 0o700); err != nil {
		return err
	}
	info, err := os.Stat(path)
	if err != nil {
		return err
	}
	if info.Mode().Perm()&0o077 != 0 {
		return os.Chmod(path, 0o700)
	}
	return nil
}

// isNotExist is errors.Is(err, fs.ErrNotExist), named so the identity loader can say "there is no
// identity yet" without a caller having to know which package's sentinel that is.
func isNotExist(err error) bool { return errors.Is(err, fs.ErrNotExist) }
