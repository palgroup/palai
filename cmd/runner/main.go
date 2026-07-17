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
	"crypto/x509"
	"log"
	"os"
	"time"

	"github.com/palgroup/palai/adapters/sandboxes/oci"
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
	lease, err := session.ReceiveLease(ctx)
	if err != nil {
		log.Fatalf("receive lease: %v", err)
	}
	log.Printf("received lease %s for run %s (fence %d)", lease.LeaseID, lease.RunID, lease.Fence)

	driver, err := oci.NewDockerDriver()
	if err != nil {
		log.Fatalf("create sandbox driver: %v", err)
	}
	defer driver.Close()

	result, err := runner.NewSupervisor(driver).Run(ctx, runner.EngineRequest{
		ImageDigest: lease.ImageDigest,
		RunID:       lease.RunID,
		AttemptID:   lease.AttemptID,
		Limits:      lease.Limits,
	})
	if err != nil {
		log.Fatalf("supervise engine (exit %d, %d stderr bytes): %v", result.ExitCode, result.StderrBytes, err)
	}
	log.Printf("engine completed: %d frames, %d stdout bytes", len(result.Frames), result.StdoutBytes)
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
