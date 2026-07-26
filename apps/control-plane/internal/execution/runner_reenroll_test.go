package execution_test

// The expired-identity recovery proof (the counterpart of
// TestRunnerRenewsCertificateAcrossLifetimesWithoutReenrolling, which proves the NORMAL path).
//
// Renewal runs over mTLS with the very certificate that is expiring, so it can only serve a
// runner that is still inside its validity window. Miss that window — a laptop sleeps, Docker
// Desktop stalls, the host is loaded past the renewal point — and the identity is dead: the
// renew endpoint rejects it at the TLS handshake for exactly the reason renewal was needed, and
// every lease dial fails the same way. Before this proof the only cure was `palai local down &&
// palai local up`, which nothing told the operator; the runner retried, forever, at 1/s.
//
// These tests drive the REAL serve loop against the REAL gateway over REAL TLS with the
// PRODUCTION token store (execution.FileEnrollmentTokens), and prove recovery happens inside
// the already-running process — no restart, no operator.

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/palgroup/palai/apps/control-plane/internal/execution"
	"github.com/palgroup/palai/packages/runner"
)

// tokenFileFixture writes a one-line bootstrap token file (the .palai/runner-token both compose
// services mount) and returns the production token store bound to it.
func tokenFileFixture(t *testing.T, token string, minInterval time.Duration) *execution.FileEnrollmentTokens {
	t.Helper()
	path := filepath.Join(t.TempDir(), "runner-token")
	if err := os.WriteFile(path, []byte(token+"\n"), 0o600); err != nil {
		t.Fatalf("write token file: %v", err)
	}
	return execution.NewFileEnrollmentTokens(path, minInterval)
}

// TestRunnerRecoversFromAnExpiredIdentityWithoutARestart is the availability proof: a runner
// whose certificate has ALREADY expired — renewal-over-mTLS is no longer possible — gets itself
// a working identity again from inside the running process, and serves a lease.
//
// The proof is deliberately end-to-end rather than a unit assertion on the fallback: it starts
// the serve loop on a certificate that is already dead (first asserting that renewal and the
// lease dial both fail on it, so the deadlock is real and not assumed) and then requires
// gateway.Dial — the production EngineDialer the orchestrator drives — to hand that same
// process a lease. Nothing in the test restarts anything.
func TestRunnerRecoversFromAnExpiredIdentityWithoutARestart(t *testing.T) {
	const certTTL = 2 * time.Second
	tokens := tokenFileFixture(t, "gw-token-1", certTTL)
	f := newGatewayFixture(t, tokens)
	f.ca.ttl = certTTL

	identity, err := runner.Enroll(context.Background(), f.bootstrap("gw-token-1"))
	if err != nil {
		t.Fatalf("enroll: %v", err)
	}

	// Miss the renewal window the way a sleeping laptop does: nothing renews until the
	// certificate is past NotAfter.
	time.Sleep(time.Until(identity.NotAfter) + 500*time.Millisecond)

	// The deadlock is real, not assumed: with this identity the runner can neither renew nor
	// dial, and re-dialing the same certificate can never clear either.
	renewCtx, cancelRenew := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancelRenew()
	if _, err := runner.Renew(renewCtx, identity, f.renewConfig()); err == nil {
		t.Fatal("the renew endpoint accepted an EXPIRED client certificate; the expiry premise no longer holds")
	}
	if _, err := f.session(identity).OpenLease(renewCtx); err == nil {
		t.Fatal("the connect endpoint accepted an EXPIRED client certificate; the expiry premise no longer holds")
	}

	// Start the real serve loop on the dead identity. This is the SAME process the runner
	// container already runs — it is never restarted below.
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		runner.ServeConfig{
			Session: f.session(identity),
			Renew: func(ctx context.Context, current runner.Identity) (runner.Identity, error) {
				return runner.Renew(ctx, current, f.renewConfig())
			},
			// The recovery path: re-present the file-mounted bootstrap credential, and only
			// when the runner holds no usable identity. Re-read from the file at call time —
			// the runner keeps no token in memory.
			Reenroll: func(ctx context.Context) (runner.Identity, error) {
				return runner.Enroll(ctx, f.bootstrap(readTokenFile(t, tokens)))
			},
			Now:     time.Now,
			Backoff: 100 * time.Millisecond,
		}.Serve(ctx)
	}()
	defer func() { cancel(); <-done }()

	// Recovery, observed where it matters: the production dialer offers this runner a lease.
	dialCtx, cancelDial := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancelDial()
	channel, err := f.gateway.Dial(dialCtx, f.attempt("run_reenroll", "att_reenroll", 1))
	if err != nil {
		t.Fatalf("the runner never recovered from an expired identity: %v", err)
	}
	_ = channel.Close()
}

// readTokenFile reads the bootstrap token back out of the fixture's file, the way the runner
// re-reads PALAI_ENROLLMENT_TOKEN_FILE rather than retaining the token in memory.
func readTokenFile(t *testing.T, tokens *execution.FileEnrollmentTokens) string {
	t.Helper()
	raw, err := os.ReadFile(tokens.Path())
	if err != nil {
		t.Fatalf("read token file: %v", err)
	}
	return trimToken(string(raw))
}

func trimToken(s string) string {
	for len(s) > 0 && (s[len(s)-1] == '\n' || s[len(s)-1] == '\r' || s[len(s)-1] == ' ') {
		s = s[:len(s)-1]
	}
	return s
}
