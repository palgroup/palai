// Command palai-control-plane serves the LP-0 HTTP surface over the durable spine.
package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"log"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/palgroup/palai/apps/control-plane/api"
	"github.com/palgroup/palai/apps/control-plane/internal/execution"
	"github.com/palgroup/palai/apps/control-plane/internal/store"
	"github.com/palgroup/palai/packages/coordinator"
)

func main() {
	ctx := context.Background()

	databaseURL := os.Getenv("PALAI_DATABASE_URL")
	if databaseURL == "" {
		log.Fatal("PALAI_DATABASE_URL is required")
	}
	repo, err := store.Open(ctx, databaseURL)
	if err != nil {
		log.Fatalf("open store: %v", err)
	}
	defer repo.Close()
	if err := repo.Migrate(ctx); err != nil {
		log.Fatalf("apply migration: %v", err)
	}
	if err := repo.Bootstrap(ctx, readFileEnv("PALAI_BOOTSTRAP_API_KEY_FILE")); err != nil {
		log.Fatalf("seed bootstrap identity: %v", err)
	}

	startRunnerGateway(os.Getenv("PALAI_RUNNER_LISTEN_ADDR"))

	addr := os.Getenv("PALAI_LISTEN_ADDR")
	if addr == "" {
		addr = ":8080"
	}
	srv := &http.Server{
		Addr: addr,
		// The runner gateway is served over a separate mutually-authenticated listener
		// (Task 12 binds the local CA and that listener); the public API server carries no
		// runner routes, so it is passed nil here.
		Handler:           api.NewRouter(repo, repo, repo, sseConfigFromEnv(), nil),
		ReadHeaderTimeout: 10 * time.Second,
	}

	startDispatch(ctx, repo.Spine())
	startRetention(ctx, repo)

	log.Printf("palai control-plane listening on %s", addr)
	log.Fatal(srv.ListenAndServe())
}

// startDispatch launches the durable dispatch workers and the reconciler that turn
// admitted response.run jobs into assigned runs (spec §24.4). A killed worker's lease
// lapses and its job is reclaimed at a higher fence, so no graceful shutdown is
// needed. PALAI_DISPATCH_WORKERS sets the worker count (default 1); 0 disables
// dispatch — the read-path SSE e2e drives runs by hand and runs the server without a
// dispatcher racing it.
func startDispatch(ctx context.Context, spine *coordinator.Store) {
	workers := envIntDefault("PALAI_DISPATCH_WORKERS", 1)
	if workers <= 0 {
		return
	}
	retry := coordinator.RetryPolicy{MaxAttempts: 5, BaseBackoff: 100 * time.Millisecond, MaxBackoff: 30 * time.Second}
	handler := execution.AdvanceRun(spine)
	for i := 0; i < workers; i++ {
		w := coordinator.NewWorker(spine, coordinator.WorkerConfig{
			Owner:        fmt.Sprintf("control-plane-%d-%d", os.Getpid(), i),
			Lease:        30 * time.Second,
			Heartbeat:    10 * time.Second,
			PollInterval: 500 * time.Millisecond,
			Retry:        retry,
		}, handler)
		go func() { _ = w.Run(ctx) }()
	}
	reconciler := execution.NewReconciler(spine, 30*time.Second, retry.MaxAttempts)
	go func() { _ = reconciler.Run(ctx) }()
}

// startRetention launches the store:false retention reaper when a TTL is configured
// (PALAI_RETENTION_STORE_FALSE_TTL). Unset disables it, so no arbitrary production
// default is imposed here; UAT and operators set a short TTL to activate reaping (spec
// §8.3, §20.9). A killed process just misses ticks; the next run resumes the sweep.
func startRetention(ctx context.Context, repo *store.Store) {
	ttl := envDuration("PALAI_RETENTION_STORE_FALSE_TTL")
	if ttl <= 0 {
		return
	}
	reaper := execution.NewReaper(repo, ttl)
	go func() { _ = reaper.Run(ctx, 30*time.Second) }()
}

// startRunnerGateway serves the runner enrollment + mutually-authenticated session
// endpoints on a SEPARATE listener from the public API. The server TLS accepts a certless
// handshake (VerifyClientCertIfGiven) so the enrollment endpoint can bootstrap a runner
// that has no certificate yet; the connect handler asserts the verified client chain
// itself. addr empty disables the gateway — the public router still carries a nil runner
// handler, and this task binds the listener but does NOT wire the gateway as an
// EngineDialer to any worker (that live exec-path is Task 15).
func startRunnerGateway(addr string) {
	if strings.TrimSpace(addr) == "" {
		return
	}
	caCertPath := mustGatewayEnv("PALAI_RUNNER_CA_CERT")
	caKeyPath := mustGatewayEnv("PALAI_RUNNER_CA_KEY")
	serverCert, err := tls.LoadX509KeyPair(mustGatewayEnv("PALAI_RUNNER_SERVER_CERT"), mustGatewayEnv("PALAI_RUNNER_SERVER_KEY"))
	if err != nil {
		log.Fatalf("load runner server certificate: %v", err)
	}
	caPEM, err := os.ReadFile(caCertPath)
	if err != nil {
		log.Fatalf("read runner CA certificate: %v", err)
	}
	caPool := x509.NewCertPool()
	if !caPool.AppendCertsFromPEM(caPEM) {
		log.Fatal("runner CA certificate file held no certificates")
	}
	issuer, err := execution.NewFileCertIssuer(caCertPath, caKeyPath)
	if err != nil {
		log.Fatalf("bind runner CA issuer: %v", err)
	}
	tokens := execution.NewFileEnrollmentTokens(mustGatewayEnv("PALAI_ENROLLMENT_TOKEN_FILE"))
	gateway := execution.NewRunnerGateway(issuer, tokens)

	srv := &http.Server{
		Addr:              addr,
		Handler:           gateway.Routes(),
		ReadHeaderTimeout: 10 * time.Second,
		TLSConfig: &tls.Config{
			MinVersion:   tls.VersionTLS13,
			Certificates: []tls.Certificate{serverCert},
			ClientCAs:    caPool,
			ClientAuth:   tls.VerifyClientCertIfGiven,
		},
	}
	// Bind synchronously so a bind failure fails fast and the gateway port is listening
	// before main starts the public server. The control-plane healthcheck gates on the
	// public /healthz, so a runner that waits for service_healthy is guaranteed a bound
	// gateway to enroll against.
	ln, err := tls.Listen("tcp", addr, srv.TLSConfig)
	if err != nil {
		log.Fatalf("bind runner gateway listener: %v", err)
	}
	log.Printf("palai runner gateway listening on %s", addr)
	go func() { log.Fatal(srv.Serve(ln)) }()
}

// mustGatewayEnv reads a required gateway env var, failing fast when the runner listener
// is enabled but misconfigured.
func mustGatewayEnv(name string) string {
	value := os.Getenv(name)
	if value == "" {
		log.Fatalf("%s is required when PALAI_RUNNER_LISTEN_ADDR is set", name)
	}
	return value
}

// readFileEnv reads the file named by env var name and returns its trimmed contents, or
// "" when the var is unset or the file is unreadable (the bootstrap seed treats an empty
// key as a no-op).
func readFileEnv(name string) string {
	path := os.Getenv(name)
	if path == "" {
		return ""
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(data))
}

// envIntDefault reads an integer env var, returning def when unset or unparseable.
func envIntDefault(name string, def int) int {
	v := os.Getenv(name)
	if v == "" {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return def
	}
	return n
}

// sseConfigFromEnv reads the event-stream timers from the environment. Unset values
// stay zero and take production defaults in api.NewRouter; operators (and the e2e
// tier) shorten them without a rebuild.
func sseConfigFromEnv() api.SSEConfig {
	return api.SSEConfig{
		Heartbeat:    envDuration("PALAI_SSE_HEARTBEAT"),
		PollInterval: envDuration("PALAI_SSE_POLL_INTERVAL"),
		WriteTimeout: envDuration("PALAI_SSE_WRITE_TIMEOUT"),
		BatchLimit:   envInt("PALAI_SSE_BATCH_LIMIT"),
	}
}

func envDuration(name string) time.Duration {
	d, err := time.ParseDuration(os.Getenv(name))
	if err != nil {
		return 0
	}
	return d
}

func envInt(name string) int {
	n, err := strconv.Atoi(os.Getenv(name))
	if err != nil {
		return 0
	}
	return n
}
