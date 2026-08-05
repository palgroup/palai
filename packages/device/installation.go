package device

import (
	"crypto/x509"
	"fmt"
	"os"
)

// Installation is everything an ENROLLED machine knows about itself, read from its own disk.
//
// ‼️ THIS TYPE IS THE ZERO-VARIABLE CONTRACT. An agent holding one reads no environment: the address is
// in the config, the credential is behind a path in the config, the identity is on disk beside a key
// that was generated once, and everything else — pool, posture, concurrency, capacity — comes from the
// admin plane after the machine connects. Measured 2026-08-05, the alternative was 26 `PALAI_` names.
type Installation struct {
	Paths    Paths
	Config   Config
	Key      DeviceKey
	Identity DeviceIdentity
	// CAs is the private trust anchor the config named, or NIL for a publicly trusted gateway. Nil is the
	// normal value rather than a degraded one — it means "the host's root store verifies the server",
	// which is DoD item 2.
	CAs *x509.CertPool
}

// Load reads an installation, or reports that this machine has none.
//
// ‼️ "NONE" IS NOT AN ERROR AND THAT IS THE COMPATIBILITY HINGE. Every Compose, Helm and systemd
// deployment alive today runs this binary with no config file at all, and the agent falls back to its
// environment for exactly those. A loader that failed here would take every existing deployment down on
// upgrade; a loader that invented a config would point them somewhere nobody chose.
//
// A config that EXISTS and cannot be read IS an error, and the distinction is the whole of the function:
// a machine an operator enrolled and whose config has the wrong mode must say so rather than quietly
// falling back to an environment the service manager did not set — which would be a machine that looks
// configured, starts, and reaches nothing.
func Load(paths Paths) (*Installation, error) {
	if _, err := os.Stat(paths.ConfigFile); err != nil {
		if isNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	config, err := LoadConfig(paths.ConfigFile)
	if err != nil {
		return nil, err
	}
	key, err := LoadOrCreateDeviceKey(paths.DeviceKeyFile)
	if err != nil {
		return nil, fmt.Errorf("device key: %w", err)
	}
	identity, err := LoadIdentity(paths.IdentityFile, key)
	if err != nil {
		return nil, fmt.Errorf("device identity: %w", err)
	}
	installation := &Installation{Paths: paths, Config: config, Key: key, Identity: identity}
	if config.ControllerCAFile != "" {
		pem, err := os.ReadFile(config.ControllerCAFile)
		if err != nil {
			return nil, fmt.Errorf("read %s: %w", config.ControllerCAFile, err)
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(pem) {
			return nil, fmt.Errorf("%s contained no certificates", config.ControllerCAFile)
		}
		installation.CAs = pool
	}
	return installation, nil
}

// EnrollmentKey reads the pool key this installation was configured with, checking its mode first.
//
// IT IS READ AT THE MOMENT IT IS NEEDED AND NEVER HELD. The agent needs it twice — once at start-up and
// once on the expired-certificate recovery path, which may be days later — and a credential resident in
// a long-lived process's memory between those two moments is a credential in a core dump.
//
// An installation whose config names no key file returns empty and no error: a device enrolled from
// stdin has no file to re-read, which is a fact about that install rather than a failure, and the caller
// (which has an identity and a certificate) is the one that decides whether it matters.
func (i *Installation) EnrollmentKey() (string, error) {
	if i.Config.EnrollmentKeyFile == "" {
		return "", nil
	}
	return ReadEnrollmentKey(i.Config.EnrollmentKeyFile)
}
