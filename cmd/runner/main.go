// Command runner is the Palai private execution host. It enrolls once with a one-use
// bootstrap token, opens an outbound mutually authenticated session to the control-plane
// runner gateway to obtain a lease, and supervises the leased engine inside a hardened
// OCI sandbox. It opens no inbound port and retains no bootstrap token on disk. As its
// enrolled certificate nears expiry it renews over that certificate — never the spent
// bootstrap token — so a long-lived host serves leases across many certificate lifetimes.
package main

import (
	"context"
	"crypto/x509"
	"log"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"github.com/palgroup/palai/adapters/sandboxes/oci"
	"github.com/palgroup/palai/packages/runner"
)

func main() {
	log.SetFlags(0)
	// The runner is a long-lived execution host: it serves leases until it is signalled to
	// stop (SIGTERM on container teardown), renewing its certificate as it nears expiry.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	bootstrap, sessionURL, renewURL, controllerDNS, controllerCAs := loadConfig()

	identity, err := runner.Enroll(ctx, bootstrap)
	if err != nil {
		log.Fatalf("enroll: %v", err)
	}
	log.Printf("enrolled runner %s; identity valid until %s", identity.RunnerID, identity.NotAfter.Format(time.RFC3339))

	session := runner.Session{
		Identity:      identity,
		URL:           sessionURL,
		ControllerCAs: controllerCAs,
		ControllerDNS: controllerDNS,
		Now:           time.Now,
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

	// Serve leases for the runner's lifetime: park -> lease -> supervise -> renew. A one-shot
	// runner (the prior behaviour) served exactly one lease then exited, leaving every
	// subsequent response's Dial blocked on the gateway's empty available channel.
	runner.ServeConfig{
		Session:    session,
		Supervisor: runner.NewStreamSupervisor(driver),
		Renew:      renew,
		Now:        time.Now,
		Log:        log.Printf,
		// Default 1 (LP-0 + existing stacks unchanged); the delegation-capable stack sets 2 so a
		// run's parent and its inline child each hold an engine on this runner (spec §25.18).
		Concurrency: envIntDefault("PALAI_RUNNER_CONCURRENCY", 1),
	}.Serve(ctx)
}

// loadConfig reads the bootstrap input from the environment and immediately clears the
// one-use token so it is neither inherited by child processes nor left in the runner's
// environment after enrollment. PALAI_RENEW_URL is optional: unset disables renewal (a
// one-shot certificate lifetime); the compose entrypoint derives it from the controller URL.
func loadConfig() (bootstrap runner.BootstrapConfig, sessionURL, renewURL, controllerDNS string, controllerCAs *x509.CertPool) {
	token := os.Getenv("PALAI_ENROLLMENT_TOKEN")
	_ = os.Unsetenv("PALAI_ENROLLMENT_TOKEN")

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
	return bootstrap, mustEnv("PALAI_SESSION_URL"), os.Getenv("PALAI_RENEW_URL"), controllerDNS, pool
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
