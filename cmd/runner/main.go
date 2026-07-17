// Command runner is the Palai private execution host. It enrolls once with a one-use
// bootstrap token, opens an outbound mutually authenticated session to obtain a lease,
// and supervises the leased engine inside a hardened OCI sandbox. It opens no inbound
// port and retains no bootstrap token on disk.
//
// The production control-plane runner gateway is a later task; until it exists this
// binary is the runner-side wiring, exercised by the conformance, fault, and security
// suites through packages/runner and adapters/sandboxes/oci.
package main

import (
	"context"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"errors"
	"log"
	"os"
	"time"

	"github.com/palgroup/palai/adapters/sandboxes/oci"
	"github.com/palgroup/palai/packages/contracts"
	"github.com/palgroup/palai/packages/runner"
)

func main() {
	log.SetFlags(0)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

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
	leaseSession, err := session.OpenLease(ctx)
	if err != nil {
		log.Fatalf("open lease: %v", err)
	}
	defer leaseSession.Close()
	lease := leaseSession.Lease()
	log.Printf("received lease %s for run %s (fence %d)", lease.LeaseID, lease.RunID, lease.Fence)

	driver, err := oci.NewDockerInteractiveDriver()
	if err != nil {
		log.Fatalf("create sandbox driver: %v", err)
	}
	if closer, ok := driver.(interface{ Close() error }); ok {
		defer func() { _ = closer.Close() }()
	}

	// Controller frames relayed over the lease feed the supervisor's stdin injection; the
	// supervisor's forwarded engine frames are relayed back to the controller by the sink.
	inbound := make(chan contracts.EngineFrame)
	go func() {
		defer close(inbound)
		for {
			frame, err := leaseSession.ReceiveControllerFrame(ctx)
			if err != nil {
				return
			}
			select {
			case inbound <- frame:
			case <-ctx.Done():
				return
			}
		}
	}()

	sink := func(ctx context.Context, frame contracts.EngineFrame) error {
		return leaseSession.SendEngineFrame(ctx, frame)
	}

	result, streamErr := runner.NewStreamSupervisor(driver).Stream(ctx, runner.EngineRequest{
		ImageDigest: lease.ImageDigest,
		RunID:       lease.RunID,
		AttemptID:   lease.AttemptID,
		Limits:      lease.Limits,
	}, inbound, sink)

	if err := leaseSession.Complete(ctx, outcomeClass(streamErr), stderrDigest(result.Stderr)); err != nil {
		log.Fatalf("report lease completion: %v", err)
	}
	if streamErr != nil {
		log.Fatalf("supervise engine (exit %d, %d stderr bytes): %v", result.ExitCode, result.StderrBytes, streamErr)
	}
	log.Printf("engine completed: %d stdout bytes", result.StdoutBytes)
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
