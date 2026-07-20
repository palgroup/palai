// Command palai-control-plane serves the LP-0 HTTP surface over the durable spine.
package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	fake "github.com/palgroup/palai/adapters/models/fake"
	providerone "github.com/palgroup/palai/adapters/models/provider_one"
	"github.com/palgroup/palai/adapters/repositories"
	"github.com/palgroup/palai/adapters/sandboxes/oci"
	"github.com/palgroup/palai/adapters/sandboxes/oci/workspace"
	"github.com/palgroup/palai/apps/control-plane/api"
	"github.com/palgroup/palai/apps/control-plane/internal/artifacts"
	"github.com/palgroup/palai/apps/control-plane/internal/execution"
	tools "github.com/palgroup/palai/apps/control-plane/internal/execution/tools"
	"github.com/palgroup/palai/apps/control-plane/internal/store"
	"github.com/palgroup/palai/packages/coordinator"
	modelbroker "github.com/palgroup/palai/packages/model-broker"
	toolbroker "github.com/palgroup/palai/packages/tool-broker"
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

	gateway := startRunnerGateway(os.Getenv("PALAI_RUNNER_LISTEN_ADDR"))

	// One supervisor keeps the dispatch workers, reconciler, and retention reaper alive: a
	// background loop that returns a transient error is logged, counted, and restarted rather
	// than silently dying and stalling dispatch (H2; LP-15 — no restart cap).
	supervisor := coordinator.NewSupervisor(log.Printf, time.Second)

	addr := os.Getenv("PALAI_LISTEN_ADDR")
	if addr == "" {
		addr = ":8080"
	}
	srv := &http.Server{
		Addr: addr,
		// The runner gateway is served over a separate mutually-authenticated listener
		// (Task 12 binds the local CA and that listener); the public API server carries no
		// runner routes, so it is passed nil here. The handler is wrapped so `palai doctor`
		// can surface the supervisor's restart counters over /healthz/supervisor.
		Handler:           withSupervisorStatus(api.NewRouter(repo, repo, repo, repo, repo, sseConfigFromEnv(), nil), supervisor),
		ReadHeaderTimeout: 10 * time.Second,
	}

	// The S3 artifact store is a single main-level instance shared by its consumers (spec §24 — the
	// credential lives only here). Today the retention reaper's byte-deleter is the in-binary consumer;
	// the changeset write-path (spec §30.6) is a composed step the live smoke + coding journey drive
	// with their own Writer over this same store, and the exact consumer the finalize gate wires once
	// workspace provisioning lands (repository.go deferral). nil when no PALAI_S3_ENDPOINT is set.
	artStore := artifactStoreFromEnv(ctx)

	startDispatch(ctx, repo, gateway, supervisor, artStore)
	startRetention(ctx, repo, supervisor, artStore)
	startOrphanGC(ctx, repo, supervisor, artStore)

	log.Printf("palai control-plane listening on %s", addr)
	log.Fatal(srv.ListenAndServe())
}

// startDispatch launches the durable dispatch workers and the reconciler that drive
// admitted response.run jobs. With a runner listener bound (gateway != nil) the worker
// runs the full production exec-path: the orchestrator drives each claimed run through the
// model broker, the conformance tool broker, and a live engine dialed over the gateway, to
// a committed terminal response. Without it, the binary keeps the assignment-only behavior
// the read-path SSE e2e drives (no broker/engine racing it). A killed worker's lease lapses
// and its job is reclaimed at a higher fence, so no graceful shutdown is needed.
// PALAI_DISPATCH_WORKERS sets the worker count (default 1); 0 disables dispatch.
func startDispatch(ctx context.Context, repo *store.Store, gateway *execution.RunnerGateway, supervisor *coordinator.Supervisor, artStore *artifacts.Store) {
	workers := envIntDefault("PALAI_DISPATCH_WORKERS", 1)
	if workers <= 0 {
		return
	}
	spine := repo.Spine()
	retry := coordinator.RetryPolicy{MaxAttempts: 5, BaseBackoff: 100 * time.Millisecond, MaxBackoff: 30 * time.Second}

	handler := execution.AdvanceRun(spine)
	if gateway != nil {
		broker, route := modelBrokerFromEnv()
		// Register the real coding tools alongside the conformance math tool: the workspace file and
		// shell tools (spec §28.7-28.8) that E09's real tool round-trip dispatches. The file tool
		// confines to the attempt's workspace; the shell tool runs behind the sandbox shell runner
		// (injected where a sandbox driver is wired — SetShellRunner; nil fails a shell call cleanly).
		toolBroker := toolbroker.New(
			toolbroker.ConformanceMathAdd(),
			tools.FileTool(),
			tools.ShellTool(),
			tools.CommitTool(),
			tools.PushTool(),
			tools.PullRequestTool(),
		)
		orch := execution.NewOrchestrator(repo, gateway, broker, toolBroker)
		orch.SetModelRoute(route)
		// Wire the repository publisher the approval pump publishes through (spec §30.9-30.10), gated on
		// the GitHub App environment. Absent it, an approved publication waits (the pump is a no-op) — no
		// push happens without a configured destination. ponytail: the live wave sets the env; the
		// deterministic tier proves the pump with a fake publisher.
		if publisher := repositoryPublisherFromEnv(); publisher != nil {
			orch.SetPublisher(publisher)
		}
		// Wire the root run's workspace auto-provisioning + coding-tool sandbox, gated on
		// PALAI_WORKSPACE_ROOT (spec §29.7-30.3, E09 Task 10). This is what makes a coding session
		// reachable from a plain HTTP request: the root run clones @ the attached ref under a brokered
		// credential (CP-side — the model/sandbox never see it, §30.2), the shell tool runs in a
		// credential-free OCI sandbox, and finalize compiles the changeset into the object store.
		// Unset ⇒ no coding workspace (a run with a binding gets no mount, the tools fail clean).
		//
		// §24 ceiling: the E09 collapsed compose co-locates CP + runner on a SHARED PALAI_WORKSPACE_ROOT,
		// so the tools run CP-side against the same host allocation the runner bind-mounts. A split
		// CP≠runner deploy (control plane and runner on different hosts, not sharing a filesystem) needs a
		// runner-relay seam — the CP-side tool dispatch would ship the file/shell op to the runner that
		// holds the mount — a NAMED FUTURE split-deploy hardening, not built here.
		if root := os.Getenv("PALAI_WORKSPACE_ROOT"); root != "" {
			orch.SetWorkspaceProvisioner(root, repositoryBrokerFromEnv())
			if artStore != nil {
				orch.SetChangesetWriter(artifacts.NewWriter(artStore, spine.Pool()))
			}
			if shell := shellRunnerFromEnv(); shell != nil {
				orch.SetShellRunner(shell)
			}
		}
		handler = execution.ExecuteRun(spine, repo, orch)
	}

	// Each worker, and the reconciler, run under the supervisor: a transient error restarts
	// the loop (logged + counted) instead of ending it, so the queue keeps draining. A worker
	// cancelled mid-job still leaves its lease to lapse and be reclaimed at a higher fence.
	for i := 0; i < workers; i++ {
		w := coordinator.NewWorker(spine, coordinator.WorkerConfig{
			Owner:        fmt.Sprintf("control-plane-%d-%d", os.Getpid(), i),
			Lease:        30 * time.Second,
			Heartbeat:    10 * time.Second,
			PollInterval: 500 * time.Millisecond,
			Retry:        retry,
		}, handler)
		go supervisor.Supervise(ctx, fmt.Sprintf("dispatch-worker-%d", i), w.Run)
	}
	reconciler := execution.NewReconciler(spine, 30*time.Second, retry.MaxAttempts)
	go supervisor.Supervise(ctx, "reconciler", reconciler.Run)
}

// modelBrokerFromEnv builds the model broker and route the exec-path uses, selected by
// PALAI_MODEL_PROVIDER. "provider-one" is the live OpenAI adapter: the model id comes from
// PALAI_MODEL (default gpt-4o-mini) and the credential is redeemed only at call time from
// PALAI_SECRET_PROVIDER_ONE (the compose file-secret bridge) — never on a request, argument,
// or log. Any other value (including unset) selects the deterministic fake adapter: no
// network, no credential, a fixed scripted completion for the shipped-binary wiring proof.
// ponytail: env selection, not a DB model_routes lookup — that routing is the deferred
// E-series carve-out.
func modelBrokerFromEnv() (*modelbroker.Broker, execution.ModelRoute) {
	if os.Getenv("PALAI_MODEL_PROVIDER") == "provider-one" {
		model := os.Getenv("PALAI_MODEL")
		if model == "" {
			model = "gpt-4o-mini"
		}
		broker := modelbroker.New(modelbroker.Config{
			Adapters: map[string]modelbroker.ModelAdapter{"provider-one": providerone.Adapter{}},
			Secrets:  modelbroker.EnvResolver{modelbroker.SecretRef("provider-one"): "PALAI_SECRET_PROVIDER_ONE"},
		})
		return broker, execution.ModelRoute{Provider: "provider-one", Model: model, Secret: modelbroker.SecretRef("provider-one")}
	}
	broker := modelbroker.New(modelbroker.Config{
		Adapters: map[string]modelbroker.ModelAdapter{"fake": fake.Adapter{Script: fake.Script{
			ProviderRequestID: "fake-local", Model: "fake", Output: "ok",
		}}},
		Secrets: modelbroker.StaticResolver{modelbroker.SecretRef("fake"): "unused"},
	})
	return broker, execution.ModelRoute{Provider: "fake", Model: "fake", Secret: modelbroker.SecretRef("fake")}
}

// repositoryBrokerFromEnv builds the credential broker the root-run clone runs behind (spec §30.2-30.3):
// the GitHub App broker when the App environment is configured (private repos), else the local broker —
// filesystem credential helpers for a local/dev Git remote or a public repo. The broker stays CP-side;
// the minted read credential feeds only a Git credential helper and is revoked after the fetch, so the
// model and the sandbox never see it. A misconfigured App falls back to the local broker rather than
// disabling provisioning, so a dev/compose stack still clones its local double.
func repositoryBrokerFromEnv() repositories.Broker {
	appID := os.Getenv("PALAI_GITHUB_APP_ID")
	installID := os.Getenv("PALAI_GITHUB_APP_INSTALLATION_ID")
	keyFile := os.Getenv("PALAI_GITHUB_APP_PRIVATE_KEY_FILE")
	if appID == "" || installID == "" || keyFile == "" {
		return repositories.NewLocalBroker()
	}
	keyPEM, err := os.ReadFile(keyFile)
	if err != nil {
		log.Printf("repository broker: read app key file: %v (using local broker)", err)
		return repositories.NewLocalBroker()
	}
	cfg := repositories.GitHubAppConfig{AppID: appID, InstallationID: installID, PrivateKeyPEM: keyPEM}
	if slug := os.Getenv("PALAI_GITHUB_REPO"); strings.IndexByte(slug, '/') > 0 {
		cfg.Repositories = []string{slug[strings.IndexByte(slug, '/')+1:]}
	}
	broker, err := repositories.NewGitHubAppBroker(cfg)
	if err != nil {
		log.Printf("repository broker: app broker: %v (using local broker)", err)
		return repositories.NewLocalBroker()
	}
	return broker
}

// shellRunnerFromEnv builds the credential-free OCI shell sandbox the workspace shell tool runs through
// (spec §28.8, SAN-002/003/004), gated on PALAI_SANDBOX_IMAGE (the pinned command image) and a working
// Docker driver. Absent either it returns nil, so a shell tool call fails cleanly (no runner) rather
// than escaping — the SetShellRunner discipline. The sandbox mounts no credential/DB/S3: the credential
// broker stays CP-side (§24), so the engine and the sandbox never see cred/DB/S3.
func shellRunnerFromEnv() toolbroker.ShellRunner {
	image := os.Getenv("PALAI_SANDBOX_IMAGE")
	if image == "" {
		return nil
	}
	driver, err := oci.NewDockerDriver()
	if err != nil {
		log.Printf("shell sandbox: bind docker driver: %v (shell tool disabled)", err)
		return nil
	}
	limits := oci.Limits{
		WallTime:        envDuration("PALAI_SANDBOX_WALL_TIME"),
		MaxMemoryBytes:  int64(envIntDefault("PALAI_SANDBOX_MAX_MEMORY_BYTES", 1<<30)),
		MaxProcessCount: int64(envIntDefault("PALAI_SANDBOX_MAX_PROCS", 128)),
		NanoCPUs:        int64(envIntDefault("PALAI_SANDBOX_NANO_CPUS", 1_000_000_000)),
	}
	return workspace.NewShellExecutor(driver, image, limits)
}

// repositoryPublisherFromEnv builds the repository publisher the approval pump publishes through (spec
// §30.9-30.10), gated on the GitHub App environment. The App private key arrives via the LP-0
// file-secret bridge (PALAI_GITHUB_APP_PRIVATE_KEY_FILE — a PATH, never inline), sealed at rest by E13;
// this process only mints short-lived scoped tokens against it and never logs it. Absent any required
// var it returns nil, so an approved publication simply waits — no push without a configured
// destination. ponytail: env gating like modelBrokerFromEnv; the live wave sets these, the deterministic
// tier proves the pump with a fake publisher.
func repositoryPublisherFromEnv() execution.Publisher {
	appID := os.Getenv("PALAI_GITHUB_APP_ID")
	installID := os.Getenv("PALAI_GITHUB_APP_INSTALLATION_ID")
	keyFile := os.Getenv("PALAI_GITHUB_APP_PRIVATE_KEY_FILE")
	if appID == "" || installID == "" || keyFile == "" {
		return nil
	}
	keyPEM, err := os.ReadFile(keyFile)
	if err != nil {
		log.Printf("repository publisher: read app key file: %v (publication disabled)", err)
		return nil
	}
	owner, repo := "", ""
	if slug := os.Getenv("PALAI_GITHUB_REPO"); strings.IndexByte(slug, '/') > 0 {
		i := strings.IndexByte(slug, '/')
		owner, repo = slug[:i], slug[i+1:]
	}
	cfg := repositories.GitHubAppConfig{AppID: appID, InstallationID: installID, PrivateKeyPEM: keyPEM}
	if repo != "" {
		cfg.Repositories = []string{repo}
	}
	broker, err := repositories.NewGitHubAppBroker(cfg)
	if err != nil {
		log.Printf("repository publisher: app broker: %v (publication disabled)", err)
		return nil
	}
	publisher := &execution.RepositoryPublisher{Broker: broker}
	if owner != "" && repo != "" {
		if prClient, err := repositories.NewGitHubPullRequestClient(cfg, owner, repo); err != nil {
			log.Printf("repository publisher: pr client: %v (pull requests disabled)", err)
		} else {
			publisher.PRClient = prClient
		}
	}
	return publisher
}

// startRetention launches the store:false retention reaper when a TTL is configured
// (PALAI_RETENTION_STORE_FALSE_TTL). Unset disables it, so no arbitrary production
// default is imposed here; UAT and operators set a short TTL to activate reaping (spec
// §8.3, §20.9). A killed process just misses ticks; the next run resumes the sweep.
func startRetention(ctx context.Context, repo *store.Store, supervisor *coordinator.Supervisor, artStore *artifacts.Store) {
	ttl := envDuration("PALAI_RETENTION_STORE_FALSE_TTL")
	if ttl <= 0 {
		return
	}
	reaper := execution.NewReaper(repo, ttl)
	if artStore != nil {
		reaper = reaper.WithArtifactStore(artStore)
	}
	go supervisor.Supervise(ctx, "retention-reaper", func(ctx context.Context) error { return reaper.Run(ctx, 30*time.Second) })
}

// startOrphanGC launches the artifact orphan garbage-collector when an object store is
// configured — the SAME gate as the retention reaper's byte-deleter, because the two write-path
// gaps it closes (an object whose row insert never committed, and a retention delete that failed
// after the row was tombstoned) only exist when there is an object store. It reconciles the bucket
// against the artifacts index on an interval, reclaiming objects no live row references, so the
// store cannot grow unbounded. A referenced object is never deleted, and the grace window —
// comfortably wider than the write path's PUT→row-insert gap — spares an object whose row may still
// be committing. Grace and interval are env-tunable (PALAI_ARTIFACT_GC_GRACE / _INTERVAL); the
// defaults are safe (a wide grace, an hourly pass, since a full bucket-list is heavier than the
// reaper's bounded DB purge). A killed process just misses ticks; the next run resumes the sweep.
func startOrphanGC(ctx context.Context, repo *store.Store, supervisor *coordinator.Supervisor, artStore *artifacts.Store) {
	if artStore == nil {
		return // no object store: retention scrubs only the DB row, so there are no orphan bytes
	}
	configured := envDurationOr("PALAI_ARTIFACT_GC_GRACE", time.Hour)
	grace := artifactGCGrace(configured)
	if grace != configured {
		log.Printf("PALAI_ARTIFACT_GC_GRACE=%s is below the %s floor; flooring it to protect in-flight writes", configured, grace)
	}
	interval := envDurationOr("PALAI_ARTIFACT_GC_INTERVAL", time.Hour)
	gc := artifacts.NewCollector(artStore, repo.Spine().Pool(), grace)
	go supervisor.Supervise(ctx, "artifact-orphan-gc", func(ctx context.Context) error { return gc.Run(ctx, interval) })
}

// minArtifactGCGrace floors PALAI_ARTIFACT_GC_GRACE: a typo'd sub-floor value (e.g. "1s")
// would collapse the GC's primary write-safety guard and let a live in-flight write be
// reclaimed before its row commits. envDurationOr rejects negative/zero but not a small
// positive, so the floor is enforced here.
const minArtifactGCGrace = 5 * time.Minute

// artifactGCGrace clamps a configured grace window up to minArtifactGCGrace.
func artifactGCGrace(configured time.Duration) time.Duration {
	if configured < minArtifactGCGrace {
		return minArtifactGCGrace
	}
	return configured
}

// envDurationOr reads a Go duration env var, returning def when unset or unparseable.
func envDurationOr(name string, def time.Duration) time.Duration {
	if d := envDuration(name); d > 0 {
		return d
	}
	return def
}

// artifactStoreFromEnv builds the control-plane's S3 artifact store from PALAI_S3_* when an
// endpoint is configured, ensuring its bucket exists; it returns nil when no endpoint is set,
// so retention then scrubs only the DB row (the object store is optional in deployments and
// tests that do not run one). The S3 credential is read here and never leaves the control
// plane (spec §24): it is redeemed for the object-store client, rides no request the engine
// or runner sees, and is never logged. Called once from main so the store is a single shared
// instance (the T5 hoist — the retention deleter and the changeset write-path share it).
func artifactStoreFromEnv(ctx context.Context) *artifacts.Store {
	endpoint := os.Getenv("PALAI_S3_ENDPOINT")
	if endpoint == "" {
		return nil
	}
	artStore, err := artifacts.NewStore(artifacts.Config{
		Endpoint:  endpoint,
		Bucket:    envDefault("PALAI_S3_BUCKET", "palai-artifacts"),
		Region:    os.Getenv("PALAI_S3_REGION"),
		AccessKey: os.Getenv("PALAI_S3_ACCESS_KEY"),
		SecretKey: os.Getenv("PALAI_S3_SECRET_KEY"),
	})
	if err != nil {
		log.Fatalf("bind artifact store: %v", err)
	}
	if err := artStore.EnsureBucket(ctx); err != nil {
		log.Fatalf("ensure artifact bucket: %v", err)
	}
	return artStore
}

// envDefault reads a string env var, returning def when unset.
func envDefault(name, def string) string {
	if v := os.Getenv(name); v != "" {
		return v
	}
	return def
}

// withSupervisorStatus serves GET /healthz/supervisor as the JSON restart-counter snapshot
// `palai doctor` surfaces, delegating every other request to next. It rides alongside
// /healthz (unauthenticated liveness) and carries no sensitive data — only the per-loop
// restart counts, so an operator can see a background loop that is silently restarting.
func withSupervisorStatus(next http.Handler, supervisor *coordinator.Supervisor) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && r.URL.Path == "/healthz/supervisor" {
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{"restarts": supervisor.Restarts()})
			return
		}
		next.ServeHTTP(w, r)
	})
}

// startRunnerGateway serves the runner enrollment + mutually-authenticated session
// endpoints on a SEPARATE listener from the public API. The server TLS accepts a certless
// handshake (VerifyClientCertIfGiven) so the enrollment endpoint can bootstrap a runner
// that has no certificate yet; the connect handler asserts the verified client chain
// itself. It returns the gateway so startDispatch can drive the production exec-path over it
// as the orchestrator's EngineDialer. addr empty disables the gateway (returns nil) — the
// public router carries a nil runner handler and dispatch stays assignment-only.
func startRunnerGateway(addr string) *execution.RunnerGateway {
	if strings.TrimSpace(addr) == "" {
		return nil
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
	// PALAI_RUNNER_CERT_TTL bounds an issued runner certificate; unset takes the production
	// default (5m). The fault-live renewal proof injects a short TTL to make rollover provable
	// in seconds. The runner renews over the cert-authenticated renew endpoint before expiry.
	issuer, err := execution.NewFileCertIssuer(caCertPath, caKeyPath, envDuration("PALAI_RUNNER_CERT_TTL"))
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
	return gateway
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
