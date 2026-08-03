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
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/palgroup/palai/adapters/sandboxes/oci"
	"github.com/palgroup/palai/adapters/sandboxes/posture"
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

	bootstrap, tokenFile, sessionURL, renewURL, settingsURL, controllerDNS, controllerCAs := loadConfig()

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
		Now:        time.Now,
		Log:        log.Printf,
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
		Settings: settingsPoll(settingsURL, controllerCAs, controllerDNS),
		// Unset uses packages/runner's own default (30s). It is read here rather than baked so an operator
		// running a large fleet can lengthen it, and a demo can shorten it, without a new build.
		SettingsInterval: envDurationDefault("PALAI_SETTINGS_INTERVAL", 0),
		// The managed allocation root every leased workspace path must sit under before this runner
		// bind-mounts it (spec §30.13, carry (b)). Unset disables the check (pre-E09 behaviour).
		WorkspaceRoot: os.Getenv("PALAI_WORKSPACE_ROOT"),
		// Opt this runner into honouring a lease's §30.13 unsafe local bind (REP-012). Default off so a
		// control plane alone cannot make the runner mount an arbitrary host path (§24 trust boundary).
		AllowUnsafeBind: os.Getenv("PALAI_WORKSPACE_UNSAFE_BIND") == "1",
		// The executor an exec.request runs on. nil on every deployment that has not declared a posture,
		// which is all of them today — see shellRunnerFromEnv.
		Shell: shellRunnerFromEnv(),
	}.Serve(ctx)
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
func loadConfig() (bootstrap runner.BootstrapConfig, tokenFile, sessionURL, renewURL, settingsURL, controllerDNS string, controllerCAs *x509.CertPool) {
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

	caPEM, err := os.ReadFile(mustEnv("PALAI_CONTROLLER_CA"))
	if err != nil {
		log.Fatalf("read controller CA: %v", err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(caPEM) {
		log.Fatal("controller CA file contained no certificates")
	}

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
	}
	return bootstrap, tokenFile,
		derivedEnv("PALAI_SESSION_URL", controllerURL, joinPath("/v1/runner/connect")),
		derivedEnv("PALAI_RENEW_URL", controllerURL, joinPath("/v1/runner/renew")),
		derivedEnv("PALAI_SETTINGS_URL", controllerURL, joinPath("/v1/runner/settings")),
		controllerDNS, pool
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

func mustEnv(name string) string {
	value := os.Getenv(name)
	if value == "" {
		log.Fatalf("%s is required", name)
	}
	return value
}

// envIntDefault reads a positive integer env var, falling back to def when unset or unparseable.
func envIntDefault(name string, def int) int {
	if n, err := strconv.Atoi(os.Getenv(name)); err == nil && n > 0 {
		return n
	}
	return def
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
