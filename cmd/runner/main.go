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
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/palgroup/palai/adapters/sandboxes/oci"
	"github.com/palgroup/palai/packages/runner"
	"github.com/palgroup/palai/packages/version"
)

func main() {
	log.SetFlags(0)
	// The runner is a long-lived execution host: it serves leases until it is signalled to
	// stop (SIGTERM on container teardown), renewing its certificate as it nears expiry.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	bootstrap, tokenFile, sessionURL, renewURL, controllerDNS, controllerCAs := loadConfig()

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

	// Renewal rolls the client certificate forward over the runner's existing identity as it
	// nears expiry; the one-use bootstrap token is spent once (above) and never presented
	// again. A runner with no renew URL configured serves a single certificate lifetime.
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
		Concurrency: envIntDefault("PALAI_RUNNER_CONCURRENCY", 1),
		// The managed allocation root every leased workspace path must sit under before this runner
		// bind-mounts it (spec §30.13, carry (b)). Unset disables the check (pre-E09 behaviour).
		WorkspaceRoot: os.Getenv("PALAI_WORKSPACE_ROOT"),
		// Opt this runner into honouring a lease's §30.13 unsafe local bind (REP-012). Default off so a
		// control plane alone cannot make the runner mount an arbitrary host path (§24 trust boundary).
		AllowUnsafeBind: os.Getenv("PALAI_WORKSPACE_UNSAFE_BIND") == "1",
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
func loadConfig() (bootstrap runner.BootstrapConfig, tokenFile, sessionURL, renewURL, controllerDNS string, controllerCAs *x509.CertPool) {
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

	controllerDNS = mustEnv("PALAI_CONTROLLER_DNS")
	bootstrap = runner.BootstrapConfig{
		RunnerID:        mustEnv("PALAI_RUNNER_ID"),
		RunnerDNS:       mustEnv("PALAI_RUNNER_DNS"),
		EnrollmentToken: token,
		EnrollmentURL:   mustEnv("PALAI_ENROLLMENT_URL"),
		ControllerCAs:   pool,
		ControllerDNS:   controllerDNS,
		Now:             time.Now,
	}
	return bootstrap, tokenFile, mustEnv("PALAI_SESSION_URL"), os.Getenv("PALAI_RENEW_URL"), controllerDNS, pool
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
