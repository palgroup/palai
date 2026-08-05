package device

import (
	"crypto/x509"
	"encoding/xml"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// parseAndCheck is the verification the GATEWAY performs, restated here from outside the production
// path. It is deliberately not a call into a helper this package exports: the check that matters happens
// in the control plane, and a test that called the same function the server calls would prove the two
// agree without proving either is right. What it pins is that the bytes this package produces satisfy
// the standard-library check the server runs on them.
func parseAndCheck(der []byte) (*x509.CertificateRequest, error) {
	csr, err := x509.ParseCertificateRequest(der)
	if err != nil {
		return nil, err
	}
	if err := csr.CheckSignature(); err != nil {
		return nil, err
	}
	return csr, nil
}

// TestAnUnsafeFileIsRefusedBeforeTheNetwork is plan §T2's fourth RED, and the word that carries it is
// BEFORE. A key checked after enrolment is a key that has already been presented from a file every
// account on the machine could read; the check is only worth having at the moment it can still prevent
// that, so what is asserted here is the refusal itself and the ORDER is asserted where the network call
// is — cmd/runner's enroll test.
//
// ALL THREE FILES, not just the key. A device key another account can read is an identity another
// account can BE, and a config another account can WRITE is a machine that can be repointed at another
// control plane by anyone with a shell on it.
func TestAnUnsafeFileIsRefusedBeforeTheNetwork(t *testing.T) {
	dir := t.TempDir()
	for _, mode := range []os.FileMode{0o644, 0o640, 0o604, 0o660, 0o666, 0o777, 0o601} {
		path := filepath.Join(dir, "key")
		if err := os.WriteFile(path, []byte("rpk_secret"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(path, mode); err != nil {
			t.Fatal(err)
		}
		if _, err := ReadEnrollmentKey(path); err == nil {
			t.Fatalf("mode %04o was accepted: a pool key readable by another account is a pool key that account holds", mode)
		}
		if err := RequireOwnerOnly(path); err == nil {
			t.Fatalf("mode %04o passed RequireOwnerOnly", mode)
		}
	}

	// AND THE NON-VACUITY HALF. Without this leg a check that refused EVERYTHING would satisfy the loop
	// above while making the product uninstallable — the shape this tree keeps finding, applied to a
	// permission guard.
	good := filepath.Join(dir, "good-key")
	if err := os.WriteFile(good, []byte("  rpk_live\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	key, err := ReadEnrollmentKey(good)
	if err != nil {
		t.Fatalf("a 0600 key file was refused: %v", err)
	}
	if key != "rpk_live" {
		t.Fatalf("key = %q, want the trimmed value: a trailing newline from `echo` is not part of a credential", key)
	}
}

// TestEveryWrittenFileIsOwnerOnlyAndAtomic covers the two properties the identity path rests on.
//
// ATOMICITY IS NOT A NICETY HERE. The device key is written once and read on every later start; a
// process killed mid-write would leave a truncated file that parses as no key, and the machine would
// then enrol as a NEW device forever after — silently, because "no key" and "first boot" look identical.
// What is asserted is the observable consequence: no partial file is ever visible under the final name,
// which is what rename-into-place buys.
func TestEveryWrittenFileIsOwnerOnlyAndAtomic(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "nested", "deeper", "identity.json")
	if err := WriteOwnerOnly(path, []byte("first")); err != nil {
		t.Fatalf("write: %v", err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("file mode %04o, want 0600", info.Mode().Perm())
	}
	// The directories it created are 0700: a 0600 file inside a 0755 directory is still LISTABLE by
	// every account, which names the identity file to anyone who asks.
	for _, d := range []string{filepath.Dir(path), filepath.Dir(filepath.Dir(path))} {
		di, err := os.Stat(d)
		if err != nil {
			t.Fatal(err)
		}
		if di.Mode().Perm()&0o077 != 0 {
			t.Fatalf("directory %s has mode %04o: a 0600 file in a group-readable directory is still enumerable", d, di.Mode().Perm())
		}
	}

	// The overwrite leaves no temporary behind and no partial content. The directory listing is the
	// assertion: a rename-based writer that failed to clean up would leave a `.palai-*` sibling holding
	// the same bytes at the same mode, which is a second copy of a secret nobody knows about.
	if err := WriteOwnerOnly(path, []byte("second")); err != nil {
		t.Fatalf("rewrite: %v", err)
	}
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(body) != "second" {
		t.Fatalf("content = %q, want the rewritten value", body)
	}
	entries, err := os.ReadDir(filepath.Dir(path))
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), ".palai-") {
			t.Fatalf("a temporary %q survived the write: it holds the same bytes at the same mode, so it is a second copy of the file", entry.Name())
		}
	}
	if len(entries) != 1 {
		t.Fatalf("directory holds %d entries, want exactly the target file", len(entries))
	}
}

// TestTheDeviceKeyIsGeneratedOnceAndNeverAgain is the property the whole task turns on: a restart is a
// restart because the KEY is the same.
//
// ‼️ THE MEASUREMENT THAT MADE THIS NECESSARY. Before packages/device, packages/runner.Enroll called
// ecdsa.GenerateKey at the top of every enrolment and cmd/runner called Enroll once per PROCESS START.
// So a Mac that rebooted presented a key the registry had never seen, took a new `rnr_` id, and — in a
// strict pool — waited for a human to approve a machine that had already been approved.
func TestTheDeviceKeyIsGeneratedOnceAndNeverAgain(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "device.key")

	first, err := LoadOrCreateDeviceKey(path)
	if err != nil {
		t.Fatalf("first: %v", err)
	}
	if first.Fingerprint() == "" {
		t.Fatal("a generated device key has no fingerprint, so nothing can key a machine on it")
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("device key mode %04o, want 0600 — a private key another account can read is a shared identity", info.Mode().Perm())
	}

	// Three more loads, which is plan §T2's "kill and restart the agent three times".
	for i := 0; i < 3; i++ {
		again, err := LoadOrCreateDeviceKey(path)
		if err != nil {
			t.Fatalf("reload %d: %v", i, err)
		}
		if again.Fingerprint() != first.Fingerprint() {
			t.Fatalf("reload %d produced fingerprint %s, want %s — every restart would be a new machine",
				i, again.Fingerprint(), first.Fingerprint())
		}
	}

	// A DIFFERENT DIRECTORY IS A DIFFERENT MACHINE, which is the other half of the same rule: re-imaging
	// creates a new key and therefore a new machine, rather than inheriting an identity.
	other, err := LoadOrCreateDeviceKey(filepath.Join(t.TempDir(), "device.key"))
	if err != nil {
		t.Fatal(err)
	}
	if other.Fingerprint() == first.Fingerprint() {
		t.Fatal("two independently created device keys share a fingerprint")
	}
}

// TestASignedRequestProvesPossessionAndACorruptedOneDoesNot is the local half of the CSR proof. The
// gateway's half — that an unverifiable CSR is REFUSED rather than falling back to the public key inside
// it — is in the execution package, against the real handler.
func TestASignedRequestProvesPossessionAndACorruptedOneDoesNot(t *testing.T) {
	key, err := LoadOrCreateDeviceKey(filepath.Join(t.TempDir(), "device.key"))
	if err != nil {
		t.Fatal(err)
	}
	der, err := key.CertificateRequest("some-mac")
	if err != nil {
		t.Fatalf("build CSR: %v", err)
	}
	csr, err := parseAndCheck(der)
	if err != nil {
		t.Fatalf("a freshly signed CSR does not verify: %v", err)
	}
	if csr.Subject.CommonName != "some-mac" {
		t.Fatalf("CSR common name = %q", csr.Subject.CommonName)
	}

	// THE NON-VACUITY HALF, and it is the one that matters: a checker that returned nil for everything
	// would pass the leg above. One byte of the signature is flipped — the LAST byte, which is inside the
	// signature and not in the encoded body, so what fails is the signature check rather than the parse.
	corrupted := append([]byte(nil), der...)
	corrupted[len(corrupted)-1] ^= 0xff
	if _, err := parseAndCheck(corrupted); err == nil {
		t.Fatal("a CSR with a corrupted signature verified: proof of possession that accepts a broken signature is not proof")
	}
}

// TestAnIdentityWhoseKeyIsGoneIsDiscardedRatherThanPresented is the local half of the re-image case.
//
// A DISK THAT KEPT identity.json AND LOST device.key is a real shape: a restored backup, a re-imaged
// machine whose state directory was partially preserved. The persisted id is one this machine can no
// longer prove, and presenting it would earn a refusal from the control plane forever. Discarding it
// locally makes the machine enrol as the NEW machine it is — which is plan §3.4's "re-imaging creates a
// new key and therefore a new machine/approval subject".
func TestAnIdentityWhoseKeyIsGoneIsDiscardedRatherThanPresented(t *testing.T) {
	dir := t.TempDir()
	keyPath := filepath.Join(dir, "device.key")
	identityPath := filepath.Join(dir, "identity.json")

	original, err := LoadOrCreateDeviceKey(keyPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := SaveIdentity(identityPath, DeviceIdentity{
		RunnerID: "rnr_original", Fingerprint: original.Fingerprint(), NotAfter: time.Now().Add(time.Hour),
	}); err != nil {
		t.Fatal(err)
	}
	held, err := LoadIdentity(identityPath, original)
	if err != nil {
		t.Fatal(err)
	}
	if held.RunnerID != "rnr_original" {
		t.Fatalf("the machine did not recover its own id: %q", held.RunnerID)
	}

	// The key is lost and a new one is minted — the re-image.
	if err := os.Remove(keyPath); err != nil {
		t.Fatal(err)
	}
	reimaged, err := LoadOrCreateDeviceKey(keyPath)
	if err != nil {
		t.Fatal(err)
	}
	stale, err := LoadIdentity(identityPath, reimaged)
	if err != nil {
		t.Fatalf("a stale identity file made the loader fail rather than answering 'none': %v", err)
	}
	if stale.RunnerID != "" {
		t.Fatalf("the machine still claims id %q under a key that cannot prove it: it would present a claim the "+
			"control plane must refuse, instead of enrolling as the new machine it is", stale.RunnerID)
	}
}

// TestAMeasuredMachineClaimsOnlyWhatItCanDo covers DoD 9's half that lives on the device: the modes a
// machine reports are what it MEASURED, and a Mac with no palai-agentd does not claim `accounts`.
//
// EVERY INPUT IS A PARAMETER, which is why this runs on any platform. A Measure that read runtime.GOOS
// directly could only ever be checked on the platform CI happens to build for — and the case that
// matters most (a Mac without the daemon) is the one no CI box is.
func TestAMeasuredMachineClaimsOnlyWhatItCanDo(t *testing.T) {
	for _, tc := range []struct {
		name                     string
		goos                     string
		agentd, docker           bool
		want                     []string
		mustNotSupport           []string
		alsoMustSupportWhenAsked []string
	}{
		{
			name: "a Mac with no daemon offers user isolation and not accounts",
			goos: "darwin", agentd: false, docker: false,
			want: []string{IsolationUser}, mustNotSupport: []string{IsolationAccounts, IsolationContainer},
		},
		{
			name: "a Mac with palai-agentd offers both",
			goos: "darwin", agentd: true, docker: false,
			want: []string{IsolationAccounts, IsolationUser},
		},
		{
			name: "a Linux host with Docker offers container isolation and neither macOS mode",
			goos: "linux", agentd: false, docker: true,
			want: []string{IsolationContainer}, mustNotSupport: []string{IsolationUser, IsolationAccounts},
		},
		{
			// The DoD 9 case: a machine that cannot execute at all claims nothing, and a pool that
			// requires anything will refuse it.
			name: "a Linux host with no Docker claims nothing",
			goos: "linux", agentd: false, docker: false,
			want: nil, mustNotSupport: []string{IsolationUser, IsolationAccounts, IsolationContainer},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			facts := Measure(tc.goos, "arm64", "9.9.9", tc.agentd, tc.docker)
			if strings.Join(facts.IsolationModes, ",") != strings.Join(tc.want, ",") {
				t.Fatalf("modes = %v, want %v", facts.IsolationModes, tc.want)
			}
			if facts.OS != tc.goos || facts.Arch != "arm64" || facts.Version != "9.9.9" {
				t.Fatalf("facts = %+v, want the measured platform verbatim", facts)
			}
			for _, mode := range tc.mustNotSupport {
				if facts.Supports(mode) {
					t.Errorf("claims %q it cannot provide", mode)
				}
			}
			// A pool that requires nothing admits everybody, including the machine that measured nothing.
			if !facts.Supports("") {
				t.Error("a pool with no isolation requirement refused a machine")
			}
		})
	}
}

// TestModesRoundTripThroughTheOneTextColumn covers the storage shape. The distinction it pins is
// "declared nothing" versus "declared a mode with no name": an empty column must come back as no modes
// rather than as a one-element slice holding the empty string, which would `Supports("")`-match nothing
// and silently pass an isolation check that should have been skipped entirely.
func TestModesRoundTripThroughTheOneTextColumn(t *testing.T) {
	for _, modes := range [][]string{nil, {IsolationUser}, {IsolationAccounts, IsolationUser}} {
		got := SplitModes(JoinModes(modes))
		if strings.Join(got, ",") != strings.Join(modes, ",") {
			t.Fatalf("round trip of %v gave %v", modes, got)
		}
	}
	for _, stored := range []string{"", "   ", ",", ",,"} {
		if got := SplitModes(stored); len(got) != 0 {
			t.Fatalf("stored %q split to %v, want none — an empty column is 'declared nothing'", stored, got)
		}
	}
}

// TestTheServiceCarriesNoCredentialAndParsesWithAwkwardPaths is the unit half of the install.
//
// ‼️ THE PLIST IS GENERATED THROUGH encoding/xml AND THIS TEST IS WHY. A directory named `R&D` — an
// ordinary thing to have — produces an unparseable plist under a fmt.Sprintf template, and the failure
// surfaces as a machine that silently never starts. The assertion is that the document round-trips
// through a parser, not that it contains a substring.
func TestTheServiceCarriesNoCredentialAndParsesWithAwkwardPaths(t *testing.T) {
	spec := ServiceSpec{
		Executable: "/Users/R&D <lab>/bin/palai",
		ConfigFile: "/Users/R&D <lab>/Library/Application Support/Palai/agent.json",
		LogFile:    "/Users/R&D <lab>/Library/Logs/Palai/agent.log",
	}
	plist, err := RenderLaunchAgent(spec)
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	var parsed any
	if err := xml.Unmarshal(plist, &parsed); err != nil {
		t.Fatalf("the generated plist does not parse, so launchd would refuse it and the machine would "+
			"never start: %v\n%s", err, plist)
	}
	for _, key := range []string{"RunAtLoad", "KeepAlive", ServiceLabel} {
		if !strings.Contains(string(plist), key) {
			t.Errorf("plist does not carry %q", key)
		}
	}

	unit := string(RenderSystemdUnit(spec))
	for _, want := range []string{"Restart=always", "WantedBy=default.target", spec.ConfigFile} {
		if !strings.Contains(unit, want) {
			t.Errorf("systemd unit does not carry %q:\n%s", want, unit)
		}
	}

	// ‼️ NEITHER FILE MAY CARRY A CREDENTIAL OR A `PALAI_` NAME. A service file is world-readable by
	// design, and a unit that exported a secret would put it in the process environment — which this tree
	// measured on 2026-08-04 to be readable by `ps -E -p <pid>` WITH VALUES, and not hidden by
	// os.Unsetenv, because macOS serves ps from the kernel's start-time copy.
	for name, body := range map[string]string{"plist": string(plist), "systemd unit": unit} {
		if strings.Contains(body, "PALAI_") {
			t.Errorf("the %s names a PALAI_ variable: the installed agent's contract is its config file, not its environment", name)
		}
		if strings.Contains(strings.ToLower(body), "rpk_") {
			t.Errorf("the %s carries something shaped like a pool key", name)
		}
	}
}

// TestTheInstallDrivesTheServiceManagerInOneOrder asserts the argument vectors and their ORDER without a
// launchd or a systemd to talk to, which is the only way this can be checked on either platform from the
// other. The order is load-bearing on Linux: without linger, `enable --now` leaves a unit that is enabled
// under a manager that stops at logout.
func TestTheInstallDrivesTheServiceManagerInOneOrder(t *testing.T) {
	for _, tc := range []struct {
		goos string
		want []string
	}{
		{goos: "darwin", want: []string{"launchctl bootout", "launchctl bootstrap", "launchctl kickstart"}},
		{goos: "linux", want: []string{"systemctl --user daemon-reload", "loginctl enable-linger", "systemctl --user enable --now " + SystemdUnitName}},
	} {
		t.Run(tc.goos, func(t *testing.T) {
			home := t.TempDir()
			paths := DefaultPaths(tc.goos, home, func(string) string { return "" })
			var ran []string
			result, err := install(tc.goos, paths, ServiceSpec{
				Executable: "/opt/palai/palai", ConfigFile: paths.ConfigFile, LogFile: paths.LogFile,
			}, func(name string, args ...string) error {
				ran = append(ran, strings.TrimSpace(name+" "+strings.Join(args, " ")))
				return nil
			})
			if err != nil {
				t.Fatalf("install: %v", err)
			}
			if !result.Loaded || !result.Started {
				t.Fatalf("install reported loaded=%t started=%t", result.Loaded, result.Started)
			}
			if _, err := os.Stat(paths.ServiceFile); err != nil {
				t.Fatalf("no service file at %s: %v", paths.ServiceFile, err)
			}
			at := 0
			for _, want := range tc.want {
				found := false
				for ; at < len(ran); at++ {
					if strings.HasPrefix(ran[at], want) {
						found, at = true, at+1
						break
					}
				}
				if !found {
					t.Fatalf("%q did not run, or ran out of order; commands were %v", want, ran)
				}
			}
		})
	}

	// A PLATFORM WITH NO SERVICE MANAGER REFUSES RATHER THAN REPORTING SUCCESS. `enroll` has already
	// spent a pool key by the time Install runs, so a silent no-op would leave an enrolled machine that
	// nothing will ever start and nobody was told about.
	home := t.TempDir()
	paths := DefaultPaths("windows", home, func(string) string { return "" })
	if _, err := install("windows", paths, ServiceSpec{ConfigFile: paths.ConfigFile}, func(string, ...string) error { return nil }); err == nil {
		t.Fatal("an unsupported platform reported a successful install")
	}
}

// TestTheStandardPathsAreThePlansPaths pins the two layouts from plan §3.2's table. It runs both on both
// platforms — that is what the goos/home parameters are for — because a layout only checkable on the
// platform it describes is a layout checked on neither in a Linux CI.
func TestTheStandardPathsAreThePlansPaths(t *testing.T) {
	mac := DefaultPaths("darwin", "/Users/op", func(string) string { return "" })
	if mac.ConfigFile != "/Users/op/Library/Application Support/Palai/agent.json" {
		t.Errorf("macOS config = %q", mac.ConfigFile)
	}
	if mac.ServiceFile != "/Users/op/Library/LaunchAgents/"+ServiceLabel+".plist" {
		t.Errorf("macOS LaunchAgent = %q", mac.ServiceFile)
	}
	if mac.LogFile != "/Users/op/Library/Logs/Palai/agent.log" {
		t.Errorf("macOS log = %q", mac.LogFile)
	}

	// XDG unset falls back to the documented defaults.
	linux := DefaultPaths("linux", "/home/op", func(string) string { return "" })
	if linux.ConfigFile != "/home/op/.config/palai/agent.json" {
		t.Errorf("linux config = %q", linux.ConfigFile)
	}
	if linux.DeviceKeyFile != "/home/op/.local/state/palai/device.key" {
		t.Errorf("linux device key = %q", linux.DeviceKeyFile)
	}
	if linux.ServiceFile != "/home/op/.config/systemd/user/"+SystemdUnitName {
		t.Errorf("linux unit = %q", linux.ServiceFile)
	}

	// XDG set is HONOURED, which is the half a hardcoded default silently breaks: a distribution that
	// sets XDG_STATE_HOME would otherwise get an agent writing its identity somewhere the operator's
	// backups do not cover.
	env := map[string]string{"XDG_CONFIG_HOME": "/cfg", "XDG_STATE_HOME": "/state"}
	xdg := DefaultPaths("linux", "/home/op", func(name string) string { return env[name] })
	if xdg.ConfigFile != "/cfg/palai/agent.json" || xdg.DeviceKeyFile != "/state/palai/device.key" {
		t.Fatalf("XDG variables were ignored: config=%q key=%q", xdg.ConfigFile, xdg.DeviceKeyFile)
	}
}

// TestAnInstallationIsLoadedFromDiskAndNothingElse is the zero-variable contract stated as a
// measurement: a loaded installation carries an address, a key PATH and an identity, and the config
// file on disk contains no key value.
//
// THE ABSENT-CONFIG CASE IS THE COMPATIBILITY HINGE and it is asserted rather than assumed: every
// Compose, Helm and systemd deployment alive today runs this binary with no config file, and a loader
// that errored there would take all of them down on upgrade.
func TestAnInstallationIsLoadedFromDiskAndNothingElse(t *testing.T) {
	home := t.TempDir()
	paths := DefaultPaths(runtime.GOOS, home, os.Getenv)

	if installed, err := Load(paths); err != nil || installed != nil {
		t.Fatalf("a machine with no config reported installed=%v err=%v; every deployment that exists today "+
			"runs with no config file", installed, err)
	}

	keyPath := filepath.Join(home, "pool.key")
	if err := os.WriteFile(keyPath, []byte("rpk_do_not_copy_me"), 0o600); err != nil {
		t.Fatal(err)
	}
	config := Config{ControllerURL: "https://runner.example.test:8443", EnrollmentKeyFile: keyPath}
	if err := config.Save(paths.ConfigFile); err != nil {
		t.Fatal(err)
	}

	// ‼️ THE CONFIG HOLDS THE PATH, NEVER THE KEY. Read the bytes on disk and look for the value: a
	// rotation must mean "replace one file", and a copied key is a second copy nobody remembers.
	onDisk, err := os.ReadFile(paths.ConfigFile)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(onDisk), "rpk_do_not_copy_me") {
		t.Fatalf("the agent config contains the pool key value:\n%s", onDisk)
	}

	installed, err := Load(paths)
	if err != nil || installed == nil {
		t.Fatalf("load: installed=%v err=%v", installed, err)
	}
	if installed.CAs != nil {
		t.Fatal("a config naming no CA file produced a non-nil pool: nil is what packages/runner reads as " +
			"'the host's own root store', and a non-nil empty pool trusts nothing at all")
	}
	if installed.Key.Fingerprint() == "" {
		t.Fatal("loading an installation did not produce a device key")
	}
	key, err := installed.EnrollmentKey()
	if err != nil {
		t.Fatalf("read enrolment key through the installation: %v", err)
	}
	if key != "rpk_do_not_copy_me" {
		t.Fatalf("enrolment key = %q", key)
	}

	// A config an unknown field was hand-edited into is refused rather than silently ignored: a value
	// that will never take effect is worse than an error, because the operator believes it did.
	if err := os.WriteFile(paths.ConfigFile, []byte(`{"controller_url":"https://x.test","pool_id":"pool_x"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadConfig(paths.ConfigFile); err == nil {
		t.Fatal("a config carrying an unknown field loaded: a machine-level pool_id is exactly the input this design removes, " +
			"and silently ignoring it tells the operator it took effect")
	}
}
