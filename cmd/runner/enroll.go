package main

import (
	"context"
	"crypto/x509"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/palgroup/palai/adapters/sandboxes/oci"
	"github.com/palgroup/palai/adapters/sandboxes/posture"
	"github.com/palgroup/palai/packages/device"
	"github.com/palgroup/palai/packages/macagent"
	"github.com/palgroup/palai/packages/runner"
	"github.com/palgroup/palai/packages/version"
)

// runEnroll is `palai enroll` — the ONE-TIME INSTALL, and everything about this function follows from
// that word.
//
// ‼️ IT IS NOT A RUNTIME CONTRACT. It runs once per machine, writes the config and the durable device
// identity, installs the service, and returns. Nothing is typed on that machine again, and the agent it
// installs reads ZERO `PALAI_` variables — measured 2026-08-05, `cmd/runner` + `packages/runner` read 26
// of them before this existed, each one an operator input on every box in the fleet.
//
// ‼️ THE ORDER IS THE SECURITY PROPERTY, and it is written out here because a reader checking it will
// check it at this call site:
//
//  1. permissions   — the key file, and the config/identity if they already exist. BEFORE the network,
//     so a key every account on the machine can read is never presented at all.
//  2. isolation     — admitMachine, the same gate the agent runs, so a Mac that cannot isolate a session
//     is refused before it holds an identity rather than after.
//  3. measurement   — GOOS, GOARCH, version and the isolation modes this machine can actually provide.
//  4. device key    — loaded, or generated and persisted atomically at 0600 on the first run.
//  5. enrolment     — a CSR signed by that key, plus the measured facts, plus the id this machine
//     already holds if it has one.
//  6. persistence   — config and identity written atomically at 0600.
//  7. service       — written, loaded, started.
//
// ‼️ AND THE KEY IS NEVER ON ARGV. There is no `--key`, only `--key-file` (and `-` for stdin). Measured
// on this tree 2026-08-04: `ps -E -p <pid>` listed 62 environment variables WITH THEIR VALUES, and
// os.Unsetenv did not hide them because macOS serves ps from the kernel's start-time copy
// (KERN_PROCARGS2). argv is at least as exposed as the environment. The key is read once, spent, and
// never copied into the config — the config records the PATH.
func runEnroll(ctx context.Context, args []string, out io.Writer) error {
	return enrol(ctx, args, out, enrolSeams{})
}

// enrolSeams are the two boundaries a test substitutes so it drives THIS function rather than a copy of
// it: where the device's files go, and what installs the service.
//
// ‼️ THEY ARE FIELDS AND NOT FLAGS, and that is the whole point of the shape. `--config` and
// `--no-service` used to exist for exactly this, and each had ONE consumer: the test. A product surface
// that exists for a test is a product surface an operator can reach, and `--no-service` in particular
// could leave a machine enrolled with nothing to bring it back after a power cycle — the state DoD 20
// forbids. The seam belongs where every other boundary in this tree lives: injected, defaulted to the
// real thing, invisible from the command line.
type enrolSeams struct {
	// Paths overrides where the config, key, identity and service unit go. Zero means this host's own.
	Paths *device.Paths
	// InstallService replaces the platform service installation. Zero means device.Install, which shells
	// out to launchctl or systemctl — the thing a test must not do to the machine running it.
	InstallService func(device.Paths, device.ServiceSpec) (device.InstalledService, error)
	// InstallAccounts replaces the palai-agentd installation. Zero means installAgentd, which needs
	// root — so a test drives this seam for the same reason it drives InstallService, and for a sharper
	// one: without it every enrolment test on a Mac would fail for want of a privilege the test has no
	// business holding, which is how a step ends up excluded from enrolment to keep the suite green.
	InstallAccounts func(context.Context) (macagent.Health, error)
}

func enrol(ctx context.Context, args []string, out io.Writer, seams enrolSeams) error {
	flags := flag.NewFlagSet("palai enroll", flag.ContinueOnError)
	flags.SetOutput(out)
	url := flags.String("url", "", "runner gateway address, e.g. https://runner.example.com:8443")
	keyFile := flags.String("key-file", "", "path to the file holding the pool enrolment key (rpk_...), or - to read it from stdin")
	caFile := flags.String("ca-file", "", "additional trust anchor for a private deployment; omit for a publicly trusted gateway")
	serverName := flags.String("server-name", "", "identity on the controller's certificate when it differs from the address; omit when the URL host IS the certified name")
	flags.Usage = func() {
		fmt.Fprint(out, "usage: palai enroll --url <controller> --key-file <path> [--ca-file <path>] [--server-name <name>]\n\n"+
			"Runs once per machine. Writes this device's configuration and identity, installs the\n"+
			"service that runs the agent, and returns. There is no --key: a secret on argv is readable\n"+
			"by every process on the machine.\n\n")
		flags.PrintDefaults()
	}
	if err := flags.Parse(args); err != nil {
		return err
	}
	if *url == "" || *keyFile == "" {
		flags.Usage()
		return errors.New("enroll requires --url and --key-file")
	}
	if !strings.HasPrefix(*url, "https://") {
		return fmt.Errorf("--url must be https; a pool key presented over %q is a pool key on the wire", *url)
	}

	paths, err := device.HostPaths()
	if err != nil {
		return fmt.Errorf("resolve device paths: %w", err)
	}
	if seams.Paths != nil {
		paths = *seams.Paths
	}

	// STEP 1 — PERMISSIONS, BEFORE THE NETWORK. ReadEnrollmentKey checks the mode before it reads the
	// bytes, so a world-readable key is refused rather than spent.
	key, keyPathForConfig, err := readEnrollmentKey(*keyFile)
	if err != nil {
		return err
	}
	// The identity file is checked here rather than only inside LoadIdentity because a second `enroll` on
	// an installed machine must refuse an identity another account could have written, and LoadIdentity's
	// own check answers "absent" for a file that is not there — which is the right answer there and would
	// swallow the mode failure here.
	for _, path := range []string{paths.ConfigFile, paths.DeviceKeyFile, paths.IdentityFile} {
		if err := device.RequireOwnerOnly(path); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("refusing to enrol: %w", err)
		}
	}

	var cas *x509.CertPool
	if *caFile != "" {
		pem, err := os.ReadFile(*caFile)
		if err != nil {
			return fmt.Errorf("read %s: %w", *caFile, err)
		}
		cas = x509.NewCertPool()
		if !cas.AppendCertsFromPEM(pem) {
			return fmt.Errorf("%s contained no certificates", *caFile)
		}
	}

	// STEP 2 — THE SAME ISOLATION MEASUREMENT THE AGENT REPORTS, said out loud here because the person
	// running `enroll` is standing at the machine. If this Mac can only offer `user`, they learn it now
	// rather than from a gateway refusal whose reason arrives as an exit code.
	reportIsolation(ctx, macagent.NewProber(macagent.DefaultSocketPath))

	// STEP 3 — MEASUREMENT. Every probe is an answer this machine can give about ITSELF, which is the
	// difference between these and the posture/pool/capacity declarations the control plane records and
	// cannot verify.
	//
	// THE WORKSPACE PREFLIGHT REFUSES HERE RATHER THAN REPORTING ZERO MODES, and the difference matters:
	// a pool with no isolation requirement admits a machine that declares nothing, so a machine with
	// nowhere to write would still have joined and still have taken sessions. It also refuses BEFORE the
	// gateway is called — the same shape as the unsafe-key refusal above — because the operator standing
	// at this machine is the one who can fix a directory, and a gateway rejection reaches them as an exit
	// code.
	if err := device.PreflightWorkspaceRoot(paths.WorkspaceRoot); err != nil {
		return fmt.Errorf("preflight: %w\n  this machine cannot hold a session's files, so enrolling it would "+
			"add capacity that fails on its first lease", err)
	}
	facts := device.Measure(runtime.GOOS, runtime.GOARCH, version.Resolve(),
		agentdReady(ctx), dockerReady(ctx), true)

	// STEP 4 — THE DURABLE KEY. Generated and persisted on the first run; LOADED on every later one, which
	// is what makes a second `enroll` recover this machine's identity instead of creating a second machine.
	deviceKey, err := device.LoadOrCreateDeviceKey(paths.DeviceKeyFile)
	if err != nil {
		return fmt.Errorf("device key: %w", err)
	}
	held, err := device.LoadIdentity(paths.IdentityFile, deviceKey)
	if err != nil {
		return fmt.Errorf("device identity: %w", err)
	}

	// STEP 5 — ENROLMENT. The CSR is signed by the durable key, so the control plane can verify the
	// machine holds the private half; the facts ride the same request so the gateway can refuse a machine
	// that cannot execute BEFORE it issues an identity.
	identity, err := runner.Enroll(ctx, runner.BootstrapConfig{
		RunnerID:        machineName,
		RunnerDNS:       machineName + ".runner.palai.internal",
		EnrollmentToken: key,
		EnrollmentURL:   strings.TrimRight(*url, "/") + "/v1/runner/enroll",
		ControllerCAs:   cas,
		ControllerDNS:   controllerIdentity(*url, *serverName),
		Now:             time.Now,
		Posture:         declaredPosture(),
		DeviceKey:       deviceKey.Signer(),
		RecoverRunnerID: held.RunnerID,
		OS:              facts.OS,
		Arch:            facts.Arch,
		Version:         facts.Version,
		IsolationModes:  facts.IsolationModes,
	})
	// The key is spent. Drop it here rather than at the end of the function: everything below writes
	// files, and a credential still in a local while a file is being written is a credential that can end
	// up in one by a later edit.
	key = ""
	if err != nil {
		return fmt.Errorf("enroll: %w", err)
	}

	// STEP 6 — PERSISTENCE. The config records the PATH to the key and never the key.
	config := device.Config{
		ControllerURL:     strings.TrimRight(*url, "/"),
		EnrollmentKeyFile: keyPathForConfig,
		ControllerCAFile:  *caFile,
		ServerName:        *serverName,
	}
	if err := config.Save(paths.ConfigFile); err != nil {
		return fmt.Errorf("write agent config: %w", err)
	}
	if err := device.SaveIdentity(paths.IdentityFile, device.DeviceIdentity{
		RunnerID:    identity.RunnerID,
		Certificate: identity.Certificate.Certificate[0],
		NotAfter:    identity.NotAfter,
		Fingerprint: deviceKey.Fingerprint(),
	}); err != nil {
		return fmt.Errorf("write device identity: %w", err)
	}

	fmt.Fprintf(out, "enrolled as %s (device key %s…)\n", identity.RunnerID, shortFingerprint(deviceKey.Fingerprint()))
	fmt.Fprintf(out, "config    %s\n", paths.ConfigFile)
	fmt.Fprintf(out, "identity  %s\n", paths.IdentityFile)

	// STEP 7 — THE SERVICE. What is claimed on success is exactly "written, loaded, running". Whether it
	// comes back after a power cycle is what the unit's RunAtLoad/WantedBy is FOR and it is NOT measured
	// here — plan Milestone A0 owns that leg and it needs a machine that can be powered off.
	installService := seams.InstallService
	if installService == nil {
		installService = func(p device.Paths, spec device.ServiceSpec) (device.InstalledService, error) {
			return device.Install(runtime.GOOS, p, spec)
		}
	}
	installed, err := installService(paths, device.ServiceSpec{
		Executable: executablePath(),
		ConfigFile: paths.ConfigFile,
		LogFile:    paths.LogFile,
	})
	if err != nil {
		// NOT SWALLOWED. The pool key has been spent and the identity written by the time this runs, so a
		// machine whose service manager refused the unit IS enrolled and will not come back by itself.
		// An operator has to read that sentence.
		return fmt.Errorf("the machine is enrolled as %s but its service was not installed at %s: %w",
			identity.RunnerID, installed.Path, err)
	}
	fmt.Fprintf(out, "service   %s (loaded=%t started=%t)\n", installed.Path, installed.Loaded, installed.Started)
	fmt.Fprintf(out, "logs      %s\n", paths.LogFile)

	// STEP 8 — THE ACCOUNT DAEMON, ON THE SAME COMMAND AND NOT A SECOND ONE.
	//
	// A session on a Mac is isolated by its uid and by nothing else, and only palai-agentd can open one.
	// Until 2026-08-07 enrolment installed the RUNNER service and stopped, so a freshly enrolled Mac
	// accepted sessions and ran every customer's work as one uid — and the fix an operator needed was a
	// separate `sudo palai agentd install` that scripts/install/install.sh already claimed enrolment did.
	// The claim was right and the code was not.
	//
	// IT MATTERS MOST ON A MACHINE NOBODY LOGS INTO. A rented Mac is provisioned by user-data that runs
	// as root exactly once; a second privileged step nobody is there to type is a step that does not
	// happen, and the machine joins the fleet unisolated. One command at machine birth is the only shape
	// that survives an unattended install.
	installAccounts := seams.InstallAccounts
	if installAccounts == nil {
		installAccounts = installAgentd
	}
	if runtime.GOOS == "darwin" {
		health, agentErr := installAccounts(ctx)
		switch {
		case agentErr != nil:
			// NOT SWALLOWED, for the reason the service failure above is not: this machine is enrolled and
			// will take work. Reporting success here would hand the fleet a Mac that cannot isolate anything
			// while its operator believes the opposite.
			return fmt.Errorf("the machine is enrolled as %s and its runner service is installed, but the "+
				"session-account daemon is not: %w\n\nUNTIL IT IS, EVERY SESSION ON THIS MAC RUNS AS ONE UID. "+
				"Re-run this enrolment as root, or install it alone with `palai agentd install`", identity.RunnerID, agentErr)
		default:
			fmt.Fprintf(out, "accounts  palai-agentd on %s (version %s, %d open)\n",
				macagent.DefaultSocketPath, stampOrUnknown(health.Version), len(health.Slots))
		}
	}
	return nil
}

// readEnrollmentKey reads the pool key from a file or from stdin, and returns the path to record in the
// config — empty for stdin, because there is no file for the agent to re-read.
//
// ‼️ STDIN EXISTS FOR THE PROVISIONER, and it carries a cost this function states rather than hides: a
// key piped in has no file, so the expired-certificate recovery path has nothing to re-read and the
// machine's only way back after a missed renewal is another `enroll`. A provisioner that wants the
// machine to recover itself writes the key to a 0600 file and passes --key-file.
func readEnrollmentKey(path string) (key, configPath string, err error) {
	if path == "-" {
		raw, err := io.ReadAll(io.LimitReader(os.Stdin, 64*1024))
		if err != nil {
			return "", "", fmt.Errorf("read enrolment key from stdin: %w", err)
		}
		value := strings.TrimSpace(string(raw))
		if value == "" {
			return "", "", errors.New("stdin carried no enrolment key")
		}
		return value, "", nil
	}
	value, err := device.ReadEnrollmentKey(path)
	if err != nil {
		return "", "", fmt.Errorf("refusing to enrol: %w", err)
	}
	absolute, err := absPath(path)
	if err != nil {
		return "", "", err
	}
	return value, absolute, nil
}

// agentdReady is the palai-agentd probe reduced to the one bit device.Measure needs. It is a separate
// call from admitMachine deliberately: admitMachine REFUSES a machine that declares the native posture
// and has no daemon, while this asks the different question of whether `accounts` mode is available —
// a container-posture Mac answers "no" here and is admitted there, and collapsing the two would make a
// Linux pool's Mac claim a mode it cannot serve.
// ‼️ IT IS SINGLE-SHOT, AND THE COLD-BOOT RETRY WINDOW IS DELIBERATELY NOT USED HERE. macagent's shipped
// policy is 12 attempts with a backoff that tops out at 5 s — about 37 seconds against a socket that
// never answers, measured 2026-08-05 when this function first used it and turned every `enroll` on a Mac
// without palai-agentd into a 37-second command. That window exists for the ADMISSION gate, where the
// question is "is a machine that declares the native posture racing a cold boot"; admitMachine has
// already asked it with the full policy by the time this runs. The question HERE is different — "can this
// machine offer `accounts` mode" — and on a Mac where no administrator ever installed the daemon, waiting
// 37 seconds does not change the answer.
func agentdReady(ctx context.Context) bool {
	if runtime.GOOS != "darwin" {
		return false
	}
	probe := macagent.NewProber(macagent.DefaultSocketPath)
	probe.Policy = macagent.ProbePolicy{Attempts: 1}
	_, err := probe.Probe(ctx)
	return err == nil
}

// dockerReady is whether a Docker daemon ANSWERS on this machine. A machine without one cannot run a
// containerised session at all, so it claims no `container` mode and a container pool refuses it — which
// is DoD 9's sentence: a machine that cannot execute never appears as ready capacity.
//
// ‼️ IT PINGS RATHER THAN CONSTRUCTING A DRIVER, and the difference is the whole value of the field.
// `oci.NewDockerInteractiveDriver()` returns a nil error on a machine with no Docker installed at all —
// the API-version negotiation is deferred to the first request — so measuring the constructor would have
// made every machine claim `container` and reintroduced exactly the "declared but never true" shape this
// task exists to remove.
func dockerReady(ctx context.Context) bool { return oci.DaemonAvailable(ctx) }

// declaredPosture is what this machine says it is, derived from the SAME reading admitMachine and the
// executor use. It is not measured — see the posture field comments in packages/runner — and it is here
// so `enroll` sends what the agent would have sent.
func declaredPosture() string {
	native, err := posture.Resolve(os.Getenv("PALAI_SANDBOX_IMAGE"), os.Getenv("PALAI_SHELL_NATIVE"))
	if err != nil || !native {
		return ""
	}
	return "unsandboxed-host"
}

// executablePath is the binary a service manager should run. os.Executable's error is answered with the
// plain name rather than a failure: a machine that cannot name its own binary can still be given one on
// PATH, and refusing the whole install for it would be a refusal with no fix behind it.
func executablePath() string {
	if path, err := os.Executable(); err == nil && path != "" {
		return path
	}
	return "palai"
}

// absPath makes the key path absolute before it is recorded, because the config is read by a SERVICE
// whose working directory is not the one the operator typed the command in. A relative path here is a
// machine that enrols by hand and then cannot find its own key at boot.
func absPath(path string) (string, error) {
	absolute, err := filepath.Abs(path)
	if err != nil {
		return "", fmt.Errorf("resolve %s: %w", path, err)
	}
	return absolute, nil
}

func shortFingerprint(fingerprint string) string {
	if len(fingerprint) > 12 {
		return fingerprint[:12]
	}
	return fingerprint
}

// controllerIdentity is the name the controller's certificate must carry: the operator's --server-name
// when the address is not the certified name, and the URL host otherwise. One function so the enrol path
// and the agent path cannot disagree about which of the two a machine verifies against.
func controllerIdentity(url, serverName string) string {
	if serverName != "" {
		return serverName
	}
	return hostOf(url)
}
