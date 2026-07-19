// Command runner is the Palai private execution host. It enrolls once with a one-use
// bootstrap token, opens an outbound mutually authenticated session to the control-plane
// runner gateway to obtain a lease, and supervises the leased engine inside a hardened
// OCI sandbox. It opens no inbound port and retains no bootstrap token on disk.
package main

import (
	"context"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"errors"
	"log"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/palgroup/palai/adapters/sandboxes/oci"
	"github.com/palgroup/palai/packages/contracts"
	"github.com/palgroup/palai/packages/runner"
)

func main() {
	log.SetFlags(0)
	// The runner is a long-lived execution host: it serves leases until it is signalled to stop
	// (SIGTERM on container teardown) or its enrolled certificate expires (runnerCertTTL).
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	bootstrap, sessionURL, controllerDNS, controllerCAs := loadConfig()

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
	supervisor := runner.NewStreamSupervisor(driver)

	// Serve leases for the runner's lifetime. OpenLease re-dials with the enrolled identity
	// and blocks until the gateway offers the next lease — the enrollment token is spent once
	// (above); every reconnect rides the client certificate — so this is a clean
	// park -> lease -> supervise -> repeat loop with no gateway change. A one-shot runner (the
	// prior behaviour) served exactly one lease then exited, leaving every subsequent
	// response's Dial blocked on the gateway's empty available channel.
	// ponytail: runnerCertTTL (5m) bounds a runner's serving window — ample for one tier;
	// cert renewal for a longer-lived runner is a separate follow-up.
	for {
		leaseSession, err := session.OpenLease(ctx)
		if err != nil {
			if ctx.Err() != nil {
				return // signalled to stop between leases — clean exit, no spin
			}
			// A transient dial error must not end the runner (that reintroduces the one-shot
			// stall); log it and back off so a persistent failure stays visible + rate-limited.
			log.Printf("open lease: %v; retrying", err)
			select {
			case <-ctx.Done():
				return
			case <-time.After(time.Second):
			}
			continue
		}
		serveLease(ctx, supervisor, leaseSession)
	}
}

// serveLease supervises one leased engine to a terminal outcome: it relays controller frames
// into the engine and engine frames back to the control plane, then reports lease completion.
// A lease-scoped context stops the inbound relay goroutine so it never outlives the lease. A
// failed lease is logged, not fatal, so one bad engine does not end the runner's service.
func serveLease(ctx context.Context, supervisor *runner.StreamSupervisor, leaseSession *runner.LeaseSession) {
	defer leaseSession.Close()
	lease := leaseSession.Lease()
	log.Printf("received lease %s for run %s (fence %d)", lease.LeaseID, lease.RunID, lease.Fence)

	leaseCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	// Controller frames relayed over the lease feed the supervisor's stdin injection; the
	// supervisor's forwarded engine frames are relayed back to the controller by the sink.
	inbound := make(chan contracts.EngineFrame)
	go func() {
		defer close(inbound)
		for {
			frame, err := leaseSession.ReceiveControllerFrame(leaseCtx)
			if err != nil {
				return
			}
			select {
			case inbound <- frame:
			case <-leaseCtx.Done():
				return
			}
		}
	}()

	sink := func(ctx context.Context, frame contracts.EngineFrame) error {
		return leaseSession.SendEngineFrame(ctx, frame)
	}

	result, streamErr := supervisor.Stream(leaseCtx, runner.EngineRequest{
		ImageDigest: lease.ImageDigest,
		RunID:       lease.RunID,
		AttemptID:   lease.AttemptID,
		Fence:       lease.Fence,
		Limits:      lease.Limits,
	}, inbound, sink)

	if err := leaseSession.Complete(ctx, outcomeClass(streamErr), stderrDigest(result.Stderr)); err != nil {
		log.Printf("report lease completion for run %s: %v", lease.RunID, err)
		return
	}
	if streamErr != nil {
		log.Printf("supervise engine for run %s (exit %d, %d stderr bytes): %v", lease.RunID, result.ExitCode, result.StderrBytes, streamErr)
		return
	}
	log.Printf("engine completed for run %s: %d stdout bytes", lease.RunID, result.StdoutBytes)
}

// outcomeClass maps a supervised streaming outcome to the lease.complete outcome class the
// control plane records: a wall-time kill is lost, any other failure is failed, and a
// clean run is succeeded.
func outcomeClass(err error) string {
	switch {
	case err == nil:
		return "succeeded"
	case errors.Is(err, runner.ErrEngineTimeout):
		return "lost"
	default:
		return "failed"
	}
}

// stderrDigest is the content digest of the already-redacted stderr the runner reports on
// completion, so the controller can correlate logs without the runner shipping raw stderr.
func stderrDigest(redacted []byte) string {
	sum := sha256.Sum256(redacted)
	return "sha256:" + hex.EncodeToString(sum[:])
}

// loadConfig reads the bootstrap input from the environment and immediately clears the
// one-use token so it is neither inherited by child processes nor left in the runner's
// environment after enrollment.
func loadConfig() (runner.BootstrapConfig, string, string, *x509.CertPool) {
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

	controllerDNS := mustEnv("PALAI_CONTROLLER_DNS")
	bootstrap := runner.BootstrapConfig{
		RunnerID:        mustEnv("PALAI_RUNNER_ID"),
		RunnerDNS:       mustEnv("PALAI_RUNNER_DNS"),
		EnrollmentToken: token,
		EnrollmentURL:   mustEnv("PALAI_ENROLLMENT_URL"),
		ControllerCAs:   pool,
		ControllerDNS:   controllerDNS,
		Now:             time.Now,
	}
	return bootstrap, mustEnv("PALAI_SESSION_URL"), controllerDNS, pool
}

func mustEnv(name string) string {
	value := os.Getenv(name)
	if value == "" {
		log.Fatalf("%s is required", name)
	}
	return value
}
