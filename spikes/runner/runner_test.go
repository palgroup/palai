package runner

import (
	"context"
	"crypto/tls"
	"net"
	"reflect"
	"strings"
	"testing"
	"time"
)

const (
	controllerDNS = "controller.runner.spike.palai.test"
	runnerDNS     = "runner-01.runner.spike.palai.test"
	runnerID      = "runner-01"
)

func TestRunnerWithTrustedShortLivedCertificateReceivesLease(t *testing.T) {
	now := time.Date(2026, time.July, 16, 12, 0, 0, 0, time.UTC)
	ca := mustCertificateAuthority(t, now)
	serverCertificate := mustCertificate(t, ca, CertificateRequest{
		DNSName:   controllerDNS,
		Usage:     CertificateUsageServer,
		NotBefore: now.Add(-time.Second),
		NotAfter:  now.Add(time.Minute),
	})
	clientCertificate := mustCertificate(t, ca, CertificateRequest{
		DNSName:   runnerDNS,
		Usage:     CertificateUsageClient,
		NotBefore: now.Add(-time.Second),
		NotAfter:  now.Add(59 * time.Second),
	})
	if lifetime := clientCertificate.Leaf.NotAfter.Sub(clientCertificate.Leaf.NotBefore); lifetime > time.Minute {
		t.Fatalf("client certificate lifetime = %s, want <= 1m", lifetime)
	}

	controllerTLS := mustControllerTLS(t, serverCertificate, ca, runnerDNS, now)
	controller := mustStartController(t, controllerTLS, fixtureLease(now))
	runnerTLS := mustRunnerTLS(t, &clientCertificate, ca, controllerDNS, now)
	client := Runner{ID: runnerID, ControllerURL: controller.URL(), TLSConfig: runnerTLS}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	lease, err := client.ReceiveLease(ctx)
	if err != nil {
		t.Fatalf("receive lease: %v", err)
	}
	if err := lease.Validate(now); err != nil {
		t.Fatalf("validate received lease: %v", err)
	}
	if lease.RunnerID != runnerID || lease.Fence != 7 {
		t.Fatalf("unexpected lease identity: runner=%q fence=%d", lease.RunnerID, lease.Fence)
	}
	if lease.Image.ID == "" || lease.Image.Digest == "" || lease.Limits.WallTimeMS == 0 || lease.Limits.MaxStdoutBytes == 0 {
		t.Fatalf("lease omitted immutable image or explicit bounds: %#v", lease)
	}
}

func TestControllerRejectsInvalidRunnerCertificates(t *testing.T) {
	now := time.Date(2026, time.July, 16, 12, 0, 0, 0, time.UTC)
	ca := mustCertificateAuthority(t, now)
	otherCA := mustCertificateAuthority(t, now)
	serverCertificate := mustCertificate(t, ca, CertificateRequest{
		DNSName:   controllerDNS,
		Usage:     CertificateUsageServer,
		NotBefore: now.Add(-time.Minute),
		NotAfter:  now.Add(time.Hour),
	})
	controller := mustStartController(t, mustControllerTLS(t, serverCertificate, ca, runnerDNS, now), fixtureLease(now))

	expired := mustCertificate(t, ca, CertificateRequest{
		DNSName:   runnerDNS,
		Usage:     CertificateUsageClient,
		NotBefore: now.Add(-2 * time.Minute),
		NotAfter:  now.Add(-time.Minute),
	})
	wrongCA := mustCertificate(t, otherCA, CertificateRequest{
		DNSName:   runnerDNS,
		Usage:     CertificateUsageClient,
		NotBefore: now.Add(-30 * time.Second),
		NotAfter:  now.Add(30 * time.Second),
	})
	wrongSAN := mustCertificate(t, ca, CertificateRequest{
		DNSName:   "other-runner.runner.spike.palai.test",
		Usage:     CertificateUsageClient,
		NotBefore: now.Add(-30 * time.Second),
		NotAfter:  now.Add(30 * time.Second),
	})
	wildcardSAN := mustCertificate(t, ca, CertificateRequest{
		DNSName:   "*.runner.spike.palai.test",
		Usage:     CertificateUsageClient,
		NotBefore: now.Add(-30 * time.Second),
		NotAfter:  now.Add(30 * time.Second),
	})

	tests := []struct {
		name        string
		certificate *tls.Certificate
	}{
		{name: "missing", certificate: nil},
		{name: "expired", certificate: &expired},
		{name: "wrong CA", certificate: &wrongCA},
		{name: "wrong SAN", certificate: &wrongSAN},
		{name: "wildcard SAN", certificate: &wildcardSAN},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			clientTLS := mustRunnerTLS(t, test.certificate, ca, controllerDNS, now)
			client := Runner{ID: runnerID, ControllerURL: controller.URL(), TLSConfig: clientTLS}
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			if _, err := client.ReceiveLease(ctx); err == nil {
				t.Fatal("controller accepted invalid runner certificate")
			}
		})
	}
}

func TestRunnerRejectsControllerHostnameMismatch(t *testing.T) {
	now := time.Date(2026, time.July, 16, 12, 0, 0, 0, time.UTC)
	ca := mustCertificateAuthority(t, now)
	serverCertificate := mustCertificate(t, ca, CertificateRequest{
		DNSName:   controllerDNS,
		Usage:     CertificateUsageServer,
		NotBefore: now.Add(-time.Minute),
		NotAfter:  now.Add(time.Hour),
	})
	clientCertificate := mustCertificate(t, ca, CertificateRequest{
		DNSName:   runnerDNS,
		Usage:     CertificateUsageClient,
		NotBefore: now.Add(-30 * time.Second),
		NotAfter:  now.Add(30 * time.Second),
	})
	controller := mustStartController(t, mustControllerTLS(t, serverCertificate, ca, runnerDNS, now), fixtureLease(now))
	clientTLS := mustRunnerTLS(t, &clientCertificate, ca, "wrong-controller.runner.spike.palai.test", now)
	client := Runner{ID: runnerID, ControllerURL: controller.URL(), TLSConfig: clientTLS}

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if _, err := client.ReceiveLease(ctx); err == nil {
		t.Fatal("runner accepted a controller certificate with the wrong hostname")
	}
}

func TestRunnerRejectsWildcardControllerSAN(t *testing.T) {
	now := time.Date(2026, time.July, 16, 12, 0, 0, 0, time.UTC)
	ca := mustCertificateAuthority(t, now)
	serverCertificate := mustCertificate(t, ca, CertificateRequest{
		DNSName:   "*.runner.spike.palai.test",
		Usage:     CertificateUsageServer,
		NotBefore: now.Add(-time.Minute),
		NotAfter:  now.Add(time.Hour),
	})
	clientCertificate := mustCertificate(t, ca, CertificateRequest{
		DNSName:   runnerDNS,
		Usage:     CertificateUsageClient,
		NotBefore: now.Add(-30 * time.Second),
		NotAfter:  now.Add(30 * time.Second),
	})
	controller := mustStartController(t, mustControllerTLS(t, serverCertificate, ca, runnerDNS, now), fixtureLease(now))
	clientTLS := mustRunnerTLS(t, &clientCertificate, ca, controllerDNS, now)
	client := Runner{ID: runnerID, ControllerURL: controller.URL(), TLSConfig: clientTLS}

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if _, err := client.ReceiveLease(ctx); err == nil {
		t.Fatal("runner accepted wildcard controller SAN instead of exact identity")
	}
}

func TestRunnerHasNoInboundListenerConfiguration(t *testing.T) {
	runnerType := reflect.TypeOf(Runner{})
	listenerType := reflect.TypeOf((*net.Listener)(nil)).Elem()
	for index := 0; index < runnerType.NumField(); index++ {
		field := runnerType.Field(index)
		if strings.Contains(strings.ToLower(field.Name), "listen") || field.Type.Implements(listenerType) {
			t.Fatalf("Runner exposes inbound listener field %q", field.Name)
		}
	}
}

func TestRunnerRejectsLeaseWithoutImmutableImageAndBounds(t *testing.T) {
	now := time.Date(2026, time.July, 16, 12, 0, 0, 0, time.UTC)
	lease := fixtureLease(now)
	lease.Image.Digest = "palai/engine:latest"
	lease.Limits.MaxStdoutBytes = 0
	if err := lease.Validate(now); err == nil {
		t.Fatal("lease accepted mutable image identity and missing stdout bound")
	}
}

func mustCertificateAuthority(t *testing.T, now time.Time) *CertificateAuthority {
	t.Helper()
	ca, err := NewCertificateAuthority(now)
	if err != nil {
		t.Fatalf("create certificate authority: %v", err)
	}
	return ca
}

func mustCertificate(t *testing.T, ca *CertificateAuthority, request CertificateRequest) tls.Certificate {
	t.Helper()
	certificate, err := ca.Issue(request)
	if err != nil {
		t.Fatalf("issue certificate: %v", err)
	}
	return certificate
}

func mustControllerTLS(
	t *testing.T,
	certificate tls.Certificate,
	ca *CertificateAuthority,
	expectedRunnerDNS string,
	now time.Time,
) *tls.Config {
	t.Helper()
	configuration, err := NewControllerTLSConfig(certificate, ca, expectedRunnerDNS, now)
	if err != nil {
		t.Fatalf("create controller TLS config: %v", err)
	}
	return configuration
}

func mustRunnerTLS(
	t *testing.T,
	certificate *tls.Certificate,
	ca *CertificateAuthority,
	expectedControllerDNS string,
	now time.Time,
) *tls.Config {
	t.Helper()
	configuration, err := NewRunnerTLSConfig(certificate, ca, expectedControllerDNS, now)
	if err != nil {
		t.Fatalf("create runner TLS config: %v", err)
	}
	return configuration
}

func mustStartController(t *testing.T, tlsConfig *tls.Config, lease Lease) *Controller {
	t.Helper()
	controller, err := StartController(ControllerConfig{
		TLSConfig:        tlsConfig,
		ExpectedRunner:   runnerID,
		ExpectedProtocol: RunnerProtocolV1,
		Lease:            lease,
	})
	if err != nil {
		t.Fatalf("start controller: %v", err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		if err := controller.Close(ctx); err != nil {
			t.Errorf("close controller: %v", err)
		}
	})
	return controller
}

func fixtureLease(now time.Time) Lease {
	digest := "sha256:" + strings.Repeat("a", 64)
	return Lease{
		Protocol:  RunnerProtocolV1,
		Type:      "lease.offer",
		RunnerID:  runnerID,
		RunID:     "run-contract-fixture",
		AttemptID: "attempt-contract-fixture",
		Fence:     7,
		Image: ImageIdentity{
			Repository: "palai/spike-engine",
			ID:         digest,
			Digest:     digest,
			Platform:   "linux/arm64",
		},
		Deadline: now.Add(30 * time.Second),
		Limits: LeaseLimits{
			WallTimeMS:      5_000,
			MaxStdoutBytes:  64 * 1024,
			MaxStderrBytes:  16 * 1024,
			MaxFrameBytes:   8 * 1024,
			MaxMemoryBytes:  64 * 1024 * 1024,
			MaxProcessCount: 16,
		},
	}
}
