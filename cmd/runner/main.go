// Command runner is the Palai private execution host. It enrolls with a file-mounted
// bootstrap token, opens an outbound mutually authenticated session to the control-plane
// runner gateway to obtain a lease, and supervises the leased engine inside a hardened
// OCI sandbox. It opens no inbound port and writes no credential to disk. As its enrolled
// certificate nears expiry it renews over that certificate — never the bootstrap token — so
// a long-lived host serves leases across many certificate lifetimes. If it ever MISSES that
// window and the certificate expires, renewal is no longer possible (it authenticates with
// the expired certificate), so the runner re-presents the bootstrap token to get a new
// identity: the one path back that does not need an operator to restart the stack.
package main

import (
	"context"
	"crypto/x509"
	"fmt"
	"log"
	"net/url"
	"os"
	"os/signal"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/palgroup/palai/adapters/sandboxes/oci"
	"github.com/palgroup/palai/adapters/sandboxes/posture"
	"github.com/palgroup/palai/packages/device"
	"github.com/palgroup/palai/packages/macagent"
	"github.com/palgroup/palai/packages/runner"
	toolbroker "github.com/palgroup/palai/packages/tool-broker"
	"github.com/palgroup/palai/packages/version"
)

func main() {
	log.SetFlags(0)
	// The runner is a long-lived execution host: it serves leases until it is signalled to
	// stop (SIGTERM on container teardown), renewing its certificate as it nears expiry.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// ‼️ `enroll` IS THE ONE-TIME INSTALL AND IT IS THE ONLY SUBCOMMAND. It writes this machine's config
	// and durable identity, installs the service, and returns; everything below is the AGENT, which is
	// what the service then runs. A device artifact carries exactly these two behaviours — no `up`, no
	// admin verb, no stack (plan §3.7).
	// ‼️ `--version` ANSWERS AND EXITS, and it has to, because install.sh RUNS THIS BINARY to decide
	// whether it already has the build it is about to write:
	//
	//   if [ -x "$dest" ] && [ "$("$dest" --version …)" = "$version" ]; then … already installed …
	//
	// Everything below this dispatch is the AGENT, which runs until it is stopped. So a binary that fell
	// through on `--version` did not print a version — it STARTED A SECOND AGENT and never returned, and
	// the installer hung with a partial line on screen. The first install on a machine worked (nothing to
	// probe) and every re-install after it hung, which is the shape a provisioner meets on its second
	// boot rather than its first.
	if len(os.Args) > 1 && (os.Args[1] == "--version" || os.Args[1] == "-version" || os.Args[1] == "version") {
		fmt.Println(version.Resolve())
		return
	}
	if len(os.Args) > 1 && os.Args[1] == "enroll" {
		if err := runEnroll(ctx, os.Args[2:], os.Stdout); err != nil {
			log.Fatalf("%v", err)
		}
		return
	}

	// ‼️ `agentd` IS THE SECOND AND LAST VERB, and it is here rather than in the operator CLI because the
	// machine that needs it is a DEVICE. Turning on `accounts` isolation — one macOS account per session,
	// the only isolation that is a cross-customer boundary — used to require the operator CLI on the box,
	// which is precisely the thing §3.7 removes. It installs the daemon the device archive already carries;
	// it never builds one, so a Mac with no checkout and no Go toolchain can do it.
	if len(os.Args) > 1 && os.Args[1] == "agentd" {
		if err := runAgentd(ctx, os.Args[2:], os.Stdout); err != nil {
			log.Fatalf("%v", err)
		}
		return
	}

	// ‼️ BEFORE ENROLMENT, NOT AFTER. What this machine can isolate with is measured here so it travels
	// WITH the enrolment request; a fact that arrived after the certificate was issued would describe a
	// machine the control plane had already counted.
	reportIsolation(ctx, macagent.NewProber(macagent.DefaultSocketPath))

	installed := installedDevice()
	bootstrap, tokenFile, sessionURL, renewURL, settingsURL, controllerDNS, controllerCAs := loadConfig(installed)

	identity, err := runner.Enroll(ctx, bootstrap)
	if err != nil {
		log.Fatalf("enroll: %v", err)
	}
	// The token is spent. Drop it from memory: the recovery path below re-reads the mounted file
	// at the moment it needs it, so nothing keeps a bootstrap credential resident for the
	// runner's lifetime.
	bootstrap.EnrollmentToken = ""
	log.Printf("enrolled runner %s; identity valid until %s", identity.RunnerID, identity.NotAfter.Format(time.RFC3339))

	session := runner.Session{
		Identity:      identity,
		URL:           sessionURL,
		ControllerCAs: controllerCAs,
		ControllerDNS: controllerDNS,
		Now:           time.Now,
		// The bg.* answers this machine gives WHILE PARKED (A.3 T7). It is on the session rather than on
		// the lease because that is when the question arrives: a background task outlives the attempt
		// that started it, so the sweep that probes it finds this machine between leases. The executor is
		// the detached half of the same posture the shell tool runs on, so a machine that cannot detach
		// answers a refusal rather than falling silent.
		Background: runner.NewBackgroundServer(posture.BackgroundRunnerFor(shellRunner(installed))),
		// Advertise this runner build's stamp so the control-plane enforces the §48.2 support window
		// (OPS-008): a runner more than two minors behind is rejected at connect with the hop message.
		Version: version.Resolve(),
	}

	driver, err := oci.NewDockerInteractiveDriver()
	if err != nil {
		log.Fatalf("create sandbox driver: %v", err)
	}
	if closer, ok := driver.(interface{ Close() error }); ok {
		defer func() { _ = closer.Close() }()
	}

	// Renewal rolls the client certificate forward over the runner's existing identity as it nears
	// expiry, and it never presents the bootstrap credential — that is the property that makes revoking
	// an enrolment key safe for machines already running (E24 T3).
	//
	// THE BOOTSTRAP CREDENTIAL IS NOT ONE-USE, AND THIS COMMENT USED TO SAY IT WAS (§3.6 D4, corrected
	// in E24 T3) — twenty lines above the re-enrolment fallback in this same function, which exists
	// precisely because it can be presented again. A runner with no renew URL configured serves a single
	// certificate lifetime and then recovers through that fallback.
	var renew func(context.Context, runner.Identity) (runner.Identity, error)
	if renewURL != "" {
		renewConfig := runner.RenewConfig{RenewURL: renewURL, ControllerCAs: controllerCAs, ControllerDNS: controllerDNS, Now: time.Now}
		renew = func(ctx context.Context, current runner.Identity) (runner.Identity, error) {
			return runner.Renew(ctx, current, renewConfig)
		}
	}

	// The recovery path renewal cannot serve. Renewal authenticates with the certificate that is
	// expiring, so a runner that MISSED its renewal window — a sleeping laptop, a stalled Docker
	// Desktop, a loaded host — holds an identity that can neither renew nor connect, and before
	// this fallback it retried that forever. Re-presenting the file-mounted bootstrap credential
	// is the only thing that can mint a new identity, so the runner does it itself rather than
	// waiting for an operator who was never told. It runs ONLY when the current certificate has
	// already expired (packages/runner enforces that), and the token is read from disk at that
	// moment — never held in memory. The control plane rate-limits redemption to one certificate
	// per issued lifetime; see execution.FileEnrollmentTokens for the threat model.
	var reenroll func(context.Context) (runner.Identity, error)
	if tokenFile != "" {
		reenroll = func(ctx context.Context) (runner.Identity, error) {
			token, err := os.ReadFile(tokenFile)
			if err != nil {
				return runner.Identity{}, fmt.Errorf("read bootstrap token file: %w", err)
			}
			config := bootstrap
			config.EnrollmentToken = strings.TrimSpace(string(token))
			return runner.Enroll(ctx, config)
		}
	}

	// Serve leases for the runner's lifetime: park -> lease -> supervise -> renew. A one-shot
	// runner (the prior behaviour) served exactly one lease then exited, leaving every
	// subsequent response's Dial blocked on the gateway's empty available channel.
	runner.ServeConfig{
		Session:    session,
		Supervisor: runner.NewStreamSupervisor(driver),
		Renew:      renew,
		Reenroll:   reenroll,
		// Keep the disk in step with the identity this loop holds. nil on the environment path, which has
		// no disk identity to keep in step — see persistIdentity for what an unpersisted renewal costs.
		PersistIdentity: persistIdentity(installed),
		Now:             time.Now,
		Log:             log.Printf,
		// Default 1 (LP-0 + existing stacks unchanged); the delegation-capable stack sets 2 so a
		// run's parent and its inline child each hold an engine on this runner (spec §25.18).
		//
		// THIS IS THE STARTING VALUE AND NO LONGER THE FINAL ONE. It is what the machine serves until its
		// first settings poll answers, which matters on exactly one path: a control plane that is
		// unreachable at poll time leaves the machine on the configuration it enrolled with, rather than on
		// a default it never chose.
		Concurrency: planeIntDefault(identity.Settings, "PALAI_RUNNER_CONCURRENCY", 1),
		// THE CHANGE-PROPAGATION HALF. Enrolment answers a machine its configuration ONCE; this asks again
		// on a timer, so an operator editing the panel reaches a machine that is already running instead of
		// having to restart it. It authenticates with the certificate this machine already holds — no new
		// credential, and no inbound port, which is the property cmd/runner's header states and this must
		// not cost.
		Settings:         settingsPoll(settingsURL, controllerCAs, controllerDNS),
		SettingsInterval: settingsInterval(installed),
		// The managed allocation root every leased workspace path must sit under before this runner
		// bind-mounts it (spec §30.13, carry (b)). Unset disables the check (pre-E09 behaviour).
		//
		// ‼️ AN INSTALLED DEVICE ANSWERS THIS ITSELF — see workspaceRoot.
		WorkspaceRoot:   workspaceRoot(installed),
		AllowUnsafeBind: allowUnsafeBind(installed),
		// The executor an exec.request runs on. nil on every deployment that has not declared a posture,
		// which is all of them today — see shellRunnerFromEnv.
		Shell: shellRunner(installed),
	}.Serve(ctx)
}

// reportIsolation logs what this machine can isolate a session with. The MEASUREMENT that decides
// admission travels with the enrolment (device.Measure -> Facts.IsolationModes); this is the operator's
// copy of it, on the machine, at the moment it mattered.
//
// ‼️ THE REFUSAL MOVED, IT DID NOT DISAPPEAR, and where it moved to is the point. This function used to
// be `admitMachine` and it killed the process whenever a native Mac had no palai-agentd. That was
// fail-closed in the wrong place: it made a daemon — and therefore one administrator action — a
// precondition for a machine to join ANY pool, including a single-customer one where the login account
// IS the boundary the operator intended. A hundred cloud Macs cannot each wait for a person.
//
// The machine now states what it measured and the GATEWAY refuses, because only the gateway knows what
// the pool requires (fleet.Store.Register -> isolationSatisfied). A pool that requires `accounts` still
// refuses a Mac with no daemon, before any certificate is issued; a pool that requires nothing still
// admits it. THE HOLE THAT ARGUMENT LEAVES IS REAL AND IS CLOSED SOMEWHERE ELSE: a multi-tenant pool
// must declare `isolation_mode=accounts` (plan DoD 19), because on a `user`-mode Mac every session runs
// as the login uid and permissions between one principal and itself are not a boundary.
//
// ‼️ IT RETRIES ONLY WHEN THERE IS SOMETHING TO WAIT FOR, and that is a measurement rather than a knob.
// macagent.ColdBootProbe dials twelve times over tens of seconds because a runner started by a first-boot
// hook can beat launchd, and a machine refused for being EARLY is refused for the wrong cause. But a
// machine with no daemon INSTALLED has nothing arriving: the plist is absent, so every one of those
// attempts is spent waiting for a process that will never bind. Measured 2026-08-06, paying it
// unconditionally added ~38 s to each start on a `user`-mode Mac — which is every Mac in the default
// posture, on every boot.
//
// The installed plist is the discriminator because it is what launchd reads: present means a daemon is
// coming, absent means one was never installed.
func reportIsolation(ctx context.Context, probe *macagent.Prober) {
	if runtime.GOOS != "darwin" {
		return
	}
	if _, err := os.Stat(macagent.LaunchDaemonPlistPath); err != nil {
		probe.Policy = macagent.ProbePolicy{Attempts: 1}
	}
	health, err := probe.Probe(ctx)
	if err != nil {
		log.Printf("session isolation: palai-agentd did not answer on %s, so this machine claims %q only and a pool requiring %q will refuse it: %v",
			probe.SocketPath, device.IsolationUser, device.IsolationAccounts, err)
		return
	}
	log.Printf("session isolation: palai-agentd answering on %s, %d session account(s) open — this machine claims %q and %q",
		probe.SocketPath, len(health.Slots), device.IsolationUser, device.IsolationAccounts)
}

// loadConfig reads the bootstrap input from the environment and immediately clears the
// token so it is neither inherited by child processes nor left in the runner's environment
// after enrollment. PALAI_RENEW_URL is optional: unset disables renewal (a one-shot
// certificate lifetime); the compose entrypoint derives it from the controller URL.
// PALAI_ENROLLMENT_TOKEN_FILE is the mounted bootstrap credential (compose, Helm and the
// systemd unit all set it): it seeds the initial token when the entrypoint did not bridge one
// into the environment, and it is the file the expired-identity recovery path re-reads. Unset
// leaves the runner with no recovery path — an expired identity is then terminal, and the
// serve loop says so.
func loadConfig(installed *device.Installation) (bootstrap runner.BootstrapConfig, tokenFile, sessionURL, renewURL, settingsURL, controllerDNS string, controllerCAs *x509.CertPool) {
	// ‼️ AN INSTALLED DEVICE READS NO ENVIRONMENT AT ALL. This branch is the whole point of `enroll`: its
	// inputs are the config on disk and this machine's own identity, so the running agent reads zero
	// `PALAI_` names. Every line below it is the COMPOSE path — the deployments that exist today, which
	// §3.7 keeps for one release and which no packaged device takes.
	if installed != nil {
		return installedBootstrap(installed)
	}

	token := os.Getenv("PALAI_ENROLLMENT_TOKEN")
	_ = os.Unsetenv("PALAI_ENROLLMENT_TOKEN")
	tokenFile = os.Getenv("PALAI_ENROLLMENT_TOKEN_FILE")
	if token == "" && tokenFile != "" {
		raw, err := os.ReadFile(tokenFile)
		if err != nil {
			log.Fatalf("read bootstrap token file: %v", err)
		}
		token = strings.TrimSpace(string(raw))
	}

	// ‼️ PALAI_CONTROLLER_CA IS NO LONGER REQUIRED — it was `mustEnv` here, and that single line made a CA
	// file a mandatory input on every device in every deployment, including the ones whose gateway carries
	// a publicly trusted certificate (DoD item 2). Unset now means the host's own root store verifies the
	// gateway. Nothing is skipped: packages/runner still verifies a chain, and still refuses a certificate
	// that vouches for anyone but the configured host — every DNS name must BE that host and every IP must
	// be loopback (verifyControllerIdentity). Both branches, unchanged by this.
	pool := caPoolFromFile(os.Getenv("PALAI_CONTROLLER_CA"))

	// ONE ADDRESS INSTEAD OF FOUR. A machine joining a fleet knows where its control plane is; it does not
	// separately know three URL paths and a DNS name on that same host, and asking an operator for them is
	// how a one-command install becomes a file to edit on every box. Each stays overridable and an explicit
	// value always wins, so every deployment built before this — compose, Helm, the systemd unit — is
	// byte-unchanged.
	controllerURL := os.Getenv("PALAI_CONTROLLER_URL")
	controllerDNS = derivedEnv("PALAI_CONTROLLER_DNS", controllerURL, hostOf)
	bootstrap = runner.BootstrapConfig{
		// The id this machine SENDS is a label — the control plane mints the real one and the runner adopts
		// it (see enrollmentResponse.RunnerID). So the hostname is a better default than a required
		// variable: it is what an operator would have typed, and being wrong costs a label rather than an
		// identity.
		RunnerID:        defaultEnv("PALAI_RUNNER_ID", machineName),
		RunnerDNS:       defaultEnv("PALAI_RUNNER_DNS", machineName+".runner.palai.internal"),
		EnrollmentToken: token,
		EnrollmentURL:   derivedEnv("PALAI_ENROLLMENT_URL", controllerURL, joinPath("/v1/runner/enroll")),
		ControllerCAs:   pool,
		ControllerDNS:   controllerDNS,
		Now:             time.Now,
		// PALAI_RUNNER_POSTURE is what this machine DECLARES itself to be (E24 T2): "sandboxed-linux"
		// or "unsandboxed-host". The control plane compares it with the pool's and refuses the
		// enrolment on a disagreement — it cannot verify it, so what this catches is an operator
		// pointing a Mac at the Linux pool's credential, not a machine that lies.
		//
		// Unset declares nothing, which is what every deployment does today and is what keeps a
		// single-runner install bit-unchanged: the request body is byte-identical without it. It is
		// read with os.Getenv rather than mustEnv for exactly that reason.
		Posture: os.Getenv("PALAI_RUNNER_POSTURE"),
		// PALAI_RUNNER_POOL is the pool this machine was CONFIGURED to join (E24 T3). It authorises
		// nothing — the pool comes from the enrolment KEY — and exists so that pasting the wrong pool's
		// key onto a machine is refused at the door instead of quietly joining another fleet, which
		// posture comparison above cannot catch when two pools share a posture.
		//
		// Unset declares nothing and inherits the key's pool, which keeps the single-runner install
		// bit-unchanged for the same reason PALAI_RUNNER_POSTURE does: the body is byte-identical without it.
		PoolID: os.Getenv("PALAI_RUNNER_POOL"),
		// PALAI_RUNNER_CAPACITY is how many sessions this machine says it can hold AT ONCE, and reading it
		// here is what makes the ceiling exist at all. Every other link had shipped — the field, the wire,
		// the handler, the store, and AcquireLease's refusal — and this one had not, so every machine in
		// every deployment enrolled with capacity 0 and ErrMachineAtCapacity could never fire.
		//
		// It is read at ENROLMENT and not from the admin plane's settings document, unlike
		// PALAI_RUNNER_CONCURRENCY above: the document arrives in the enrolment RESPONSE, so a ceiling
		// delivered by it could not bound the enrolment that carried it.
		Capacity: declaredCapacity(),
	}
	return bootstrap, tokenFile,
		// ‼️ THE SESSION URL IS wss, AND THE OTHER THREE ARE https — one derivation, not four. Deriving
		// this one with joinPath produced `https://…/v1/runner/connect`, which the session refuses
		// ("session URL must be outbound wss"), so every deployment had to be handed a pre-built value by
		// a shell bridge. That is why three scripts spelled the swap and no Go code did.
		derivedEnv("PALAI_SESSION_URL", controllerURL, outboundSessionURL),
		derivedEnv("PALAI_RENEW_URL", controllerURL, joinPath("/v1/runner/renew")),
		derivedEnv("PALAI_SETTINGS_URL", controllerURL, joinPath("/v1/runner/settings")),
		controllerDNS, pool
}

// installedDevice reads this machine's installation, or nil when it has none.
//
// ‼️ `--config <path>` IS AN ARGUMENT AND NOT A VARIABLE, and that is the same decision `enroll` makes
// about the key: the service file names the path, so `launchctl print` and `systemctl cat` show an
// operator which config the running agent is on. An environment variable would be one more `PALAI_` name
// on the very count this task exists to drive to zero.
//
// A CONFIG THAT EXISTS AND CANNOT BE READ IS FATAL, and a machine with no config falls through to the
// environment. The asymmetry is deliberate: "no config" is every Compose and Helm deployment alive today,
// while "a config with the wrong mode" is a machine somebody enrolled — and letting that one fall back to
// an environment the service manager did not set would produce an agent that starts and reaches nothing.
func installedDevice() *device.Installation {
	paths, err := device.HostPaths()
	if err != nil {
		// A machine that cannot name its own home directory has no standard config path to look in. That
		// is not a failure to start — it is the pre-device state — so the environment path takes over.
		return nil
	}
	if explicit := configFlag(os.Args[1:]); explicit != "" {
		paths.ConfigFile = explicit
	}
	installed, err := device.Load(paths)
	if err != nil {
		log.Fatalf("read device configuration: %v", err)
	}
	return installed
}

// configFlag reads `--config <path>` (or `--config=<path>`, or the single-dash spellings) out of the
// agent's arguments. It is hand-parsed rather than a flag.FlagSet because this binary's argument list is
// two shapes — `enroll ...` and the agent — and installing a package-level FlagSet would make an
// unrecognised argument fatal for a service manager that passes one.
func configFlag(args []string) string {
	for i, arg := range args {
		switch {
		case arg == "--config" || arg == "-config":
			if i+1 < len(args) {
				return args[i+1]
			}
		case strings.HasPrefix(arg, "--config="):
			return strings.TrimPrefix(arg, "--config=")
		case strings.HasPrefix(arg, "-config="):
			return strings.TrimPrefix(arg, "-config=")
		}
	}
	return ""
}

// installedBootstrap is the ZERO-VARIABLE branch of loadConfig: every value below comes from this
// machine's own disk or is derived from the one address in its config.
//
// ‼️ THE DEVICE KEY AND THE HELD ID ARE WHAT MAKE A RESTART A RESTART. The key is the same one every
// previous start used, so the registry recovers the row it already has; the id is presented so the
// control plane reissues THAT identity rather than minting a second. Neither is an authorisation — the
// server checks the claim against the fingerprint and refuses a disagreement.
//
// WHAT IS ABSENT IS THE CONTRACT. No pool, no posture, no capacity, no runner id, no DNS name, no four
// derived URLs, no workspace root, no concurrency. Those are the admin plane's or the binary's, and a
// value added here is a value an operator has to set on every machine in the fleet.
func installedBootstrap(installed *device.Installation) (bootstrap runner.BootstrapConfig, tokenFile, sessionURL, renewURL, settingsURL, controllerDNS string, controllerCAs *x509.CertPool) {
	key, err := installed.EnrollmentKey()
	if err != nil {
		log.Fatalf("read enrolment key: %v", err)
	}
	base := strings.TrimRight(installed.Config.ControllerURL, "/")
	// The identity the certificate must carry, which is not always the address dialled — see
	// device.Config.ServerName. Derived by the same function `enroll` used, so a machine cannot verify
	// against one name at install and another at every start after it.
	controllerDNS = controllerIdentity(base, installed.Config.ServerName)
	if controllerDNS == "" {
		log.Fatalf("%s names controller_url %q, which has no host", installed.Paths.ConfigFile, installed.Config.ControllerURL)
	}
	// The preflight is re-run on every start, not trusted from enrolment: a workspace root that was
	// writable when the operator enrolled can be gone after a reboot mounted one volume less, and a machine
	// that reports its modes from a stale measurement is the "declared, and nothing happens" defect with a
	// day's delay. A failure claims NO mode, so a pool with a requirement stops placing sessions here.
	workspaceUsable := device.PreflightWorkspaceRoot(installed.Paths.WorkspaceRoot) == nil
	if !workspaceUsable {
		log.Printf("preflight FAILED: workspace root %s is unusable — this machine claims no isolation mode "+
			"and a pool that requires one will not place sessions on it", installed.Paths.WorkspaceRoot)
	}
	facts := device.Measure(runtime.GOOS, runtime.GOARCH, version.Resolve(), agentdReady(context.Background()), dockerReady(context.Background()), workspaceUsable)
	return runner.BootstrapConfig{
			RunnerID:  machineName,
			RunnerDNS: machineName + ".runner.palai.internal",
			// The key is passed for THIS enrolment and dropped by main immediately after, exactly as the
			// environment path's is. The file it came from is named in the config, so the recovery path
			// re-reads it at the moment it needs it rather than holding it for the process's lifetime.
			EnrollmentToken: key,
			EnrollmentURL:   base + "/v1/runner/enroll",
			ControllerCAs:   installed.CAs,
			ControllerDNS:   controllerDNS,
			Now:             time.Now,
			Posture:         declaredPosture(),
			DeviceKey:       installed.Key.Signer(),
			RecoverRunnerID: installed.Identity.RunnerID,
			OS:              facts.OS,
			Arch:            facts.Arch,
			Version:         facts.Version,
			IsolationModes:  facts.IsolationModes,
		},
		installed.Config.EnrollmentKeyFile,
		outboundSessionURL(base),
		base + "/v1/runner/renew",
		base + "/v1/runner/settings",
		controllerDNS, installed.CAs
}

// outboundSessionURL turns the one controller address into the websocket address the lease session
// dials. The session refuses anything that is not `wss://` (packages/runner.Session.openConnection),
// because an outbound-only agent that could be pointed at `ws://` would be an agent whose lease traffic
// can be read.
//
// ‼️ IT IS HERE BECAUSE IT WAS NOWHERE. The swap lived in three shell bridges — the compose entrypoint,
// the host package's launcher and the native runner's env map — and in no Go code at all, so the device
// path built `https://…/v1/runner/connect` and every lease attempt answered "session URL must be
// outbound wss; retrying", forever, on a machine that had enrolled successfully and appeared healthy in
// Fleet. Measured 2026-08-06 on the real agent. A derivation copied into three scripts is a derivation
// the fourth caller does not have.
func outboundSessionURL(base string) string {
	if rest, ok := strings.CutPrefix(base, "https://"); ok {
		return "wss://" + rest + "/v1/runner/connect"
	}
	// Left as-is so the session's own guard produces the refusal, naming the URL it was given. Rewriting
	// an http:// base to wss:// here would hide an operator's mistake behind a connection error.
	return base + "/v1/runner/connect"
}

// persistIdentity writes each renewed or recovered certificate beside this device's durable key, or nil
// for a machine that has no installation.
//
// ‼️ WHAT AN UNPERSISTED RENEWAL COSTS, which is the reason the hook exists at all: the runner rolls its
// certificate forward in memory, so without this the disk keeps the certificate the machine ENROLLED
// with. A restart after that one expires then takes the recovery path and spends a pool key to be issued
// something the machine had already been issued — and on a box whose provisioner deleted the key file
// after installation, there is no recovery path and the machine is simply out of the fleet.
//
// THE FINGERPRINT IS RE-WRITTEN EVERY TIME AND IT IS ALWAYS THE SAME ONE, because the device key never
// changes. Writing it is what lets LoadIdentity detect a disk whose identity file survived a re-image and
// whose key did not, and discard the claim locally instead of presenting an id the machine cannot prove.
func persistIdentity(installed *device.Installation) func(runner.Identity) error {
	if installed == nil {
		return nil
	}
	return func(identity runner.Identity) error {
		var leaf []byte
		if len(identity.Certificate.Certificate) > 0 {
			leaf = identity.Certificate.Certificate[0]
		}
		return device.SaveIdentity(installed.Paths.IdentityFile, device.DeviceIdentity{
			RunnerID:    identity.RunnerID,
			Certificate: leaf,
			NotAfter:    identity.NotAfter,
			Fingerprint: installed.Key.Fingerprint(),
		})
	}
}

// caPoolFromFile reads an optional trust anchor. EMPTY RETURNS NIL, and nil is what packages/runner reads
// as "the host's own root store" — not "trust nothing" and not "trust anything". It is the line that
// stopped a CA file from being a mandatory input on every device (DoD item 2).
func caPoolFromFile(path string) *x509.CertPool {
	if path == "" {
		return nil
	}
	pem, err := os.ReadFile(path)
	if err != nil {
		log.Fatalf("read controller CA: %v", err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(pem) {
		log.Fatal("controller CA file contained no certificates")
	}
	return pool
}

// machineName is the default identity a machine gives itself: its hostname, or a fixed fallback when the
// host cannot answer. It is only ever a LABEL — the control plane mints the id that names this machine
// everywhere else — so a collision costs a confusing row and never a wrong identity.
var machineName = func() string {
	if h, err := os.Hostname(); err == nil && h != "" {
		return strings.TrimSuffix(h, ".local")
	}
	return "palai-runner"
}()

// defaultEnv is os.Getenv with a fallback, for a variable a machine can answer about itself.
func defaultEnv(name, def string) string {
	if v := os.Getenv(name); v != "" {
		return v
	}
	return def
}

// derivedEnv is the collapse: an explicit variable wins, otherwise the value is DERIVED from the one
// address the machine was given, and only if neither exists does it fail by name.
//
// IT FAILS RATHER THAN GUESSES. A runner with no controller URL and no explicit variable has nowhere to
// go, and a default here would be a machine that starts and silently reaches nothing — the failure this
// tree keeps finding. The message names both ways out, because an operator hitting it has either forgotten
// the one address or is running a deployment that still sets the four.
func derivedEnv(name, controllerURL string, derive func(string) string) string {
	if v := os.Getenv(name); v != "" {
		return v
	}
	if controllerURL == "" {
		log.Fatalf("%s is required, or set PALAI_CONTROLLER_URL once and this is derived from it", name)
	}
	derived := derive(controllerURL)
	if derived == "" {
		log.Fatalf("%s could not be derived from PALAI_CONTROLLER_URL=%q — set %s explicitly", name, controllerURL, name)
	}
	return derived
}

// joinPath derives a runner endpoint from the controller address. The gateway's routes are fixed
// (runner_gateway.go Routes), so these are not a guess about a deployment — they are the same three
// constants the server registers, read from the one place a machine already had to know.
func joinPath(path string) func(string) string {
	return func(base string) string { return strings.TrimRight(base, "/") + path }
}

// hostOf is the DNS name the runner pins its TLS to: the controller address's host, without the port. It
// is the SAN the certificate carries, and a port in a ServerName is a handshake that never matches.
func hostOf(base string) string {
	u, err := url.Parse(base)
	if err != nil || u.Host == "" {
		return ""
	}
	return u.Hostname()
}

// mustEnv IS DELETED RATHER THAN LEFT UNUSED. It had exactly one caller — the PALAI_CONTROLLER_CA read
// in loadConfig — and that single call was what made a CA file a mandatory input on every device in
// every deployment (DoD item 2). Keeping the helper with no caller would leave the next person a
// ready-made way to make another variable required, which is the direction this task exists to reverse.

// envIntDefault reads a positive integer env var, falling back to def when unset or unparseable.
func envIntDefault(name string, def int) int {
	if n, err := strconv.Atoi(os.Getenv(name)); err == nil && n > 0 {
		return n
	}
	return def
}

// declaredCapacity reads this machine's declared session ceiling, and it is deliberately not
// envIntDefault: that helper clamps anything non-positive to its default, and both ends of the range
// matter here.
//
// ZERO IS THE SHIPPED POSTURE, NOT A FALLBACK. Unset declares nothing, `omitempty` keeps the field off
// the wire, and the store records a 0 that AcquireLease's `capacity > 0` guard reads as "no ceiling" —
// so a machine nobody configured enrols byte-for-byte as it always did.
//
// A NEGATIVE IS PASSED THROUGH RATHER THAN CLAMPED, because the control plane already refuses it by name
// (fleet.ErrCapacityNotDeclarable → 400 "declared capacity cannot be negative") and clamping here would
// leave that shipped refusal with no caller — the same "declared but nothing reaches it" shape this
// reader exists to close, one layer down.
//
// An unparseable value declares nothing, which is the position every other reader in this file takes.
func declaredCapacity() int {
	n, err := strconv.Atoi(strings.TrimSpace(os.Getenv("PALAI_RUNNER_CAPACITY")))
	if err != nil {
		return 0
	}
	return n
}

// planeIntDefault reads a positive-integer setting the ADMIN PLANE sent with this machine's identity,
// falling back to the machine's own environment and then to the built-in default.
//
// THE ORDER IS THE PRODUCT DECISION AND THE FALLBACK IS THE MIGRATION PATH. The plane wins because the
// whole point of the runner_pool document is that an operator configures a fleet from one screen instead
// of editing a file on every box. The environment is still read second, and deleting that second read
// would break every deployment built before this field existed — the compose file, the Helm chart and the
// systemd unit all set this variable today, and a runner that ignored them would come up misconfigured
// against a control plane nobody had given a document to.
//
// A value the plane sent that does not parse falls through to the environment rather than to the default,
// so a typo in the panel is not silently indistinguishable from an unconfigured pool.
// shellRunnerFromEnv builds the executor this machine runs an exec.request on, or nil when the
// deployment has declared no posture for it.
//
// BOTH POSTURES ARE REACHABLE HERE NOW, which is the half of A.3 that makes the epic's exit criterion
// attainable: a Linux pool's commands run in the sandbox image ON THIS MACHINE rather than beside the
// control plane. The derivation is adapters/sandboxes/posture's, shared with the control plane's own
// root, because the OCI posture carries four bounds (image, wall time, max memory, max process count)
// and a second writing of them is how two composition roots end up disagreeing about what contained a
// command — a containment no operator can then reason about. Until A.3 exactly that copy was the reason
// this function wired only the host posture.
//
// A REFUSED POSTURE IS FATAL HERE, and that asymmetry with the control plane is deliberate. The control
// plane logs and carries on because it has a whole deployment to serve with the shell tool simply
// absent; a runner that cannot decide where its commands run has nothing else to be, and a machine
// silently in the OTHER posture is the outcome worth crashing to avoid.
//
// A nil executor is still not silence: the tool server answers with a refusal
// (packages/runner/toolserver.go), so a control plane learns it will get no result rather than blocking
// a tool call forever.
func shellRunnerFromEnv() toolbroker.ShellRunner {
	shell, err := posture.RunnerFromEnv()
	if err != nil {
		log.Fatalf("shell posture: %v", err)
	}
	return shell
}

// shellRunner is the executor a leased command runs on: the posture package answers it, from what the
// machine measured on an installed device and from the environment everywhere else. This function only
// chooses which question to ask — building an executor here is what
// posture.TestNeitherCompositionRootBuildsItsOwnExecutor forbids, and it caught the first draft.
func shellRunner(installed *device.Installation) toolbroker.ShellRunner {
	if installed != nil {
		shell, err := posture.RunnerForInstalledDevice(runtime.GOOS)
		if err != nil {
			log.Fatalf("shell posture: %v", err)
		}
		return shell
	}
	return shellRunnerFromEnv()
}

// settingsInterval is how often the agent re-asks the admin plane for its configuration. An INSTALLED
// device takes the package default; the environment answers for compose, Helm and the systemd unit,
// where an operator running a large fleet can lengthen it without a new build.
//
// A DEVICE HAS NOWHERE TO PUT IT, which is the whole reason it is not read there: the value would have to
// arrive as a per-machine environment variable, and a fleet where the poll interval differs per box for
// reasons nobody recorded is a fleet whose configuration lag cannot be reasoned about. If it ever needs
// to vary, its home is the desired document the agent already polls — not the machine.
func settingsInterval(installed *device.Installation) time.Duration {
	if installed != nil {
		return 0 // packages/runner's own default
	}
	return envDurationDefault("PALAI_SETTINGS_INTERVAL", 0)
}

// allowUnsafeBind opts this runner into honouring a lease's §30.13 unsafe local bind (REP-012).
//
// ‼️ AN INSTALLED DEVICE NEVER OPTS IN, and this is a narrowing rather than a default. The flag exists so
// a control plane ALONE cannot make a runner mount an arbitrary host path — the machine has to have said
// yes too (§24 trust boundary). On a device that yes could only be typed as an environment variable on
// the box, which is precisely the surface the device contract removes; and a fleet machine that mounts
// host paths on request is the one shape the isolation work exists to prevent.
//
// Compose, Helm and the systemd unit keep it: they are deployments where an operator edits a file they
// own, and REP-012 is theirs.
func allowUnsafeBind(installed *device.Installation) bool {
	if installed != nil {
		return false
	}
	return os.Getenv("PALAI_WORKSPACE_UNSAFE_BIND") == "1"
}

// workspaceRoot is the directory every leased session's workspace is allocated under.
//
// ‼️ IT IS THE DEVICE'S OWN, NOT THE ADMIN PLANE'S, and the control plane is what settled that: writing
// PALAI_WORKSPACE_ROOT through the desired-configuration surface is REFUSED, with the reason "naming the
// host directory every coding workspace is allocated under, from a web form, is a filesystem write
// primitive wearing a settings control." That refusal is correct, so the machine answers instead — it is
// the party that knows where its own disk is.
//
// Measured 2026-08-06, before this existed: an installed device has no PALAI_WORKSPACE_ROOT, so a
// correctly enrolled Mac renewed its certificate forever, took no lease, and the only thing the operator
// saw was a model saying "the shell tool is unavailable due to workspace constraints".
//
// The environment still answers for the compose and systemd deployments that set it today; an installed
// device never reads it (plan §3.7 — the compatibility window is theirs, not the packaged agent's).
// workspaceRoot answers where this machine opens session workspaces, and the ORDER is the whole point:
// what the deployment CHOSE beats what this platform would have picked.
//
// The control plane hands out allocation paths under its own PALAI_WORKSPACE_ROOT and this agent
// refuses anything outside its root, so the two must name one directory. An enrolled device used to
// return its platform default unconditionally — env could not correct it, because a LaunchAgent does
// not inherit the shell that enrolled it — which made every co-located native deployment fail with
// "workspace path is outside the runner allocation root" the moment enrolment stopped being part of
// the bring-up.
func workspaceRoot(installed *device.Installation) string {
	// AN INSTALLED AGENT READS NOTHING FROM ITS ENVIRONMENT — that is this tree's rule and two guards
	// enforce it, so the deployment's choice arrives in the CONFIG rather than in a variable. It could
	// not arrive any other way regardless: a LaunchAgent inherits no shell.
	if installed != nil {
		if root := strings.TrimSpace(installed.Config.WorkspaceRoot); root != "" {
			return root
		}
		return installed.Paths.WorkspaceRoot
	}
	return os.Getenv("PALAI_WORKSPACE_ROOT")
}

func planeIntDefault(settings map[string]string, name string, def int) int {
	if v, ok := settings[name]; ok {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			return n
		}
	}
	return envIntDefault(name, def)
}

// settingsPoll builds the settings-poll function ServeConfig drives on its timer, or nil when this machine
// has no settings URL — which disables the poll entirely and leaves the machine on the configuration it
// enrolled with, the behaviour of every runner built before the endpoint existed.
//
// IT IS BUILT HERE RATHER THAN IN packages/runner FOR THE REASON Renew IS: the package owns the protocol
// and the composition root owns the wiring, so a deployment that wants to point the poll somewhere else —
// or switch it off — does so without the package knowing there is such a thing as an environment variable.
func settingsPoll(settingsURL string, cas *x509.CertPool, dns string) func(context.Context, runner.Identity, runner.Settings) (runner.Settings, error) {
	if settingsURL == "" {
		return nil
	}
	config := runner.SettingsConfig{SettingsURL: settingsURL, ControllerCAs: cas, ControllerDNS: dns, Now: time.Now}
	return func(ctx context.Context, current runner.Identity, report runner.Settings) (runner.Settings, error) {
		return runner.FetchSettings(ctx, current, report, config)
	}
}

// envDurationDefault reads a Go duration env var, falling back to def when unset or unparseable.
//
// AN UNPARSEABLE VALUE FALLS BACK RATHER THAN FAILING THE BOOT, which is the same position every other
// reader in this file takes, but it is worth naming what that costs: `10min` is ten minutes to a human and
// nothing to time.ParseDuration, so a machine given one polls on the default and says nothing. That is
// acceptable HERE and would not be for a security parameter — this variable decides only how stale a
// machine's configuration may get, and the control plane's own write path refuses the same malformed value
// before it could ever reach a document.
func envDurationDefault(name string, def time.Duration) time.Duration {
	if d, err := time.ParseDuration(os.Getenv(name)); err == nil && d > 0 {
		return d
	}
	return def
}
