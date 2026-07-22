// Command palai-control-plane serves the LP-0 HTTP surface over the durable spine.
package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	// time/tzdata embeds the IANA zoneinfo database in the binary so schedule timezones resolve even in a
	// container without /usr/share/zoneinfo (spec §33.1; time.LoadLocation's documented final fallback).
	_ "time/tzdata"

	mcpclient "github.com/palgroup/palai/adapters/integrations/mcp"
	"github.com/palgroup/palai/adapters/integrations/webhook"
	fake "github.com/palgroup/palai/adapters/models/fake"
	providerone "github.com/palgroup/palai/adapters/models/provider_one"
	"github.com/palgroup/palai/adapters/repositories"
	"github.com/palgroup/palai/adapters/sandboxes/oci"
	"github.com/palgroup/palai/adapters/sandboxes/oci/workspace"
	remotehttp "github.com/palgroup/palai/adapters/tools/http"
	"github.com/palgroup/palai/apps/control-plane/api"
	"github.com/palgroup/palai/apps/control-plane/internal/artifacts"
	"github.com/palgroup/palai/apps/control-plane/internal/automation"
	"github.com/palgroup/palai/apps/control-plane/internal/execution"
	tools "github.com/palgroup/palai/apps/control-plane/internal/execution/tools"
	"github.com/palgroup/palai/apps/control-plane/internal/extensions"
	"github.com/palgroup/palai/apps/control-plane/internal/identity"
	"github.com/palgroup/palai/apps/control-plane/internal/metering"
	"github.com/palgroup/palai/apps/control-plane/internal/store"
	"github.com/palgroup/palai/packages/coordinator"
	"github.com/palgroup/palai/packages/coordinator/recovery"
	modelbroker "github.com/palgroup/palai/packages/model-broker"
	toolbroker "github.com/palgroup/palai/packages/tool-broker"
	"github.com/palgroup/palai/storage"
)

func main() {
	// The process-lifetime context is the control plane's SYSTEM scope: it drives migration, the
	// bootstrap seed, and the background loops that are cross-tenant by construction (the job claim
	// loop, the reconciler, the retention reaper, the outbox/webhook/schedule pumps). Nothing serving
	// an HTTP request inherits it — a request's scope is published by the auth middleware from the
	// verified API key, and a claimed job's work is re-scoped to that job's tenant by the worker
	// (migration 000029).
	ctx := storage.WithSystemScope(context.Background())

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

	// The tenancy provisioning store backs the /v1/organizations, /v1/projects, and /v1/api-keys surface
	// (E13 Task 2). It rides the durable spine's pool; organization creation opens a new tenant with no
	// restart, and the config_policy PATCH makes the §14 resolver's project layer API-reachable.
	identityStore := identity.New(repo.Spine().Pool())

	// The DB-backed secret store (E13 Task 3, SEC-002/MCI-002) fronts the env-file secret bridge: a secret
	// provisioned over POST /v1/secret-refs is envelope-encrypted at rest (single master-key AES-256-GCM) and
	// resolved fresh, so a rotation takes effect with NO restart. It is wired ONLY when
	// PALAI_SECRET_MASTER_KEY_FILE names a file holding a 32-byte hex key; without it dbSecretStore stays nil,
	// the four resolvers stay env-file-only, and the secret-ref routes stay unmounted — every pre-T3 stack is
	// unchanged. Set here (before any component that resolves a secret is built) so the front-door is live.
	var secretStore *identity.SecretStore
	if keyFile := os.Getenv("PALAI_SECRET_MASTER_KEY_FILE"); keyFile != "" {
		// A SET-but-unreadable key file is FATAL, the same posture as bad hex/length below: a broken key-file
		// permission on redeploy must not boot "healthy" with the secret store silently disabled (which would
		// serve superseded env secrets). Only an UNSET env var leaves the store nil — the documented opt-out.
		// readFileEnv is NOT used here because it swallows the read error into an empty string.
		raw, err := os.ReadFile(keyFile)
		if err != nil {
			log.Fatalf("secret master key: read %s: %v", keyFile, err) // path only — never the contents
		}
		key, err := identity.ParseMasterKey(string(raw))
		if err != nil {
			log.Fatalf("secret master key: %v", err)
		}
		secretStore = identity.NewSecretStore(repo.Spine().Pool(), key)
		dbSecretStore = secretStore
	}

	gateway := startRunnerGateway(os.Getenv("PALAI_RUNNER_LISTEN_ADDR"))

	// The outbound-webhook store is shared by the HTTP surface (endpoint registration + the delivery
	// view) and the delivery pump (spec §21.4-21.6). It rides the durable spine's pool.
	webhookStore := automation.NewWebhookStore(repo.Spine().Pool())

	// The trigger store is shared by the HTTP surface (trigger management + manual/API delivery + the signed
	// inbound-webhook receiver) and the delivery-reconciler (spec §20.2.2, E11 Task 2/5). It admits a
	// triggered run through the durable spine — the SAME §20.9 admission path a POST /v1/responses takes. The
	// inbound receiver verifies against the org-scoped secret bridge, audits rejects (log-only ceiling — E13/
	// E15 durable store), and bounds a flood (in-flight semaphore default 256, per-trigger backlog opt-in).
	triggerStore := automation.NewTriggerStore(repo.Spine().Pool()).WithAdmitter(repo.Spine()).
		WithInboundSecrets(inboundSecretResolver).
		WithInboundGate(log.Printf, envDuration("PALAI_INBOUND_TOLERANCE"),
			envIntDefault("PALAI_INBOUND_MAX_INFLIGHT", 256), envIntDefault("PALAI_INBOUND_BACKLOG_MAX", 0))

	// The schedule store is shared by the HTTP surface (schedule management + occurrence log) and the
	// schedule-ticker (spec §33, E11 Task 3). It fires schedules through the SAME trigger-delivery pipeline
	// the manual/API path uses — a scheduled firing admits its run via triggerStore.
	scheduleStore := automation.NewScheduleStore(repo.Spine().Pool(), triggerStore)

	// One supervisor keeps the dispatch workers, reconciler, and retention reaper alive: a
	// background loop that returns a transient error is logged, counted, and restarted rather
	// than silently dying and stalling dispatch (H2; LP-15 — no restart cap).
	supervisor := coordinator.NewSupervisor(log.Printf, time.Second)

	// The S3 artifact store is a single main-level instance shared by its consumers (spec §24 — the
	// credential lives only here). Today the retention reaper's byte-deleter, the changeset write-path, and
	// the E13 T5 retrieval read-path share it. nil when no PALAI_S3_ENDPOINT is set.
	artStore := artifactStoreFromEnv(ctx)

	// The artifact retrieval read-path (spec §22.6, E13 Task 5): the never-opened READ half of the E09
	// write-path, mounted on the public API. It streams bytes from the same control-plane-only object store
	// (the credential never leaves) and reads the tenant-scoped rows over the durable spine's pool. Left nil
	// when no object store is configured, so the retrieval routes stay unmounted rather than 500 on an
	// absent store (the nil-seam guard NewRouter honours for every optional surface).
	var artifactReader api.ArtifactAPI
	if artStore != nil {
		artifactReader = artifacts.NewReader(artStore, repo.Spine().Pool())
	}

	addr := os.Getenv("PALAI_LISTEN_ADDR")
	if addr == "" {
		addr = ":8080"
	}
	// WithSecretRefs is passed ONLY when a store exists: passing a typed-nil *identity.SecretStore through
	// api.WithSecretRefs would set a non-nil interface wrapping a nil pointer and mount routes over a nil
	// store (the classic Go nil-interface trap), so the option is appended conditionally.
	// The metering surface (spec §43, E13 T6): the durable budget/quota limits and the tenant's view of
	// what has been settled, over the same spine pool. It is always wired — unlike secret-refs it needs no
	// external key material — and mounting it only opens the MANAGEMENT routes: a limit already stored is
	// enforced inside the admission transaction whether or not this option is passed.
	routerOpts := []api.RouterOption{
		api.WithEdgeLimits(edgeLimitsFromEnv()),
		api.WithUsage(metering.New(repo.Spine().Pool())),
	}
	if secretStore != nil {
		routerOpts = append(routerOpts, api.WithSecretRefs(secretStore))
	}
	srv := &http.Server{
		Addr: addr,
		// The runner gateway is served over a separate mutually-authenticated listener
		// (Task 12 binds the local CA and that listener); the public API server carries no
		// runner routes, so it is passed nil here. The handler is wrapped so `palai doctor`
		// can surface the supervisor's restart counters over /healthz/supervisor.
		// The signed remote-tool result callback endpoint (spec §28.24, E12 T4): its auth IS the per-operation
		// HMAC signature + one-use token, so it rides the top mux unauthenticated (like the inbound receiver).
		// The SAME org-scoped secret bridge signs the outbound invoke and verifies the inbound callback.
		// The §20.12 edge admission control (E13 T7): the per-API-key request-rate limiter and the
		// per-project concurrent/queued run caps, read from the environment. Every value defaults to
		// zero = disabled, so a stack that configures none admits exactly as before.
		Handler: withSupervisorStatus(api.NewRouter(repo, repo, repo, repo, repo, repo, webhookStore, triggerStore, scheduleStore, repo, repo, repo, repo, identityStore, artifactReader, sseConfigFromEnv(), nil,
			api.NewToolCallbackHandler(remotehttp.NewOperations(repo.Spine().Pool()), remoteToolSecretResolver),
			routerOpts...), supervisor),
		ReadHeaderTimeout: 10 * time.Second,
	}

	startDispatch(ctx, repo, gateway, supervisor, artStore)
	startWebhookPump(ctx, webhookStore, supervisor)
	startDeliveryReconciler(ctx, triggerStore, supervisor)
	startScheduleTicker(ctx, scheduleStore, supervisor)
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
			tools.ResearchFetchTool(), // web-research fetch + citations (E12 T3); code-defined, no registry seed
		)
		// Wire the E12 per-tenant registry lookup: a tool absent from the static set above is resolved
		// through the run's pinned tool_sets (control_plane echo binder in T2) and runs the SAME fenced
		// path. ExecEnv.Scope carries tenant + RunID, so resolution is tenant-scoped; a registered tool
		// never enters the static map (no cross-tenant leak).
		toolRegistry := extensions.New(spine.Pool())
		// Wire the E12 T4 remote_http executor: a registered remote-tool revision resolves to a signed HTTP
		// invoke over the shared egress layer, opening a durable async operation the signed callback resolves
		// under a live fence. The signing secret is resolved fresh per invoke from the org-scoped file bridge
		// (never held). PALAI_TOOL_CALLBACK_BASE_URL is this CP's public base the 202 result is posted back to;
		// unset leaves the async callback URL empty (a remote tool can then only answer synchronously).
		toolRegistry.SetRemoteInvoker(
			remotehttp.NewExecutor(remotehttp.NewOperations(spine.Pool()),
				remotehttp.WithCallbackBaseURL(os.Getenv("PALAI_TOOL_CALLBACK_BASE_URL"))),
			remoteToolSecretResolver,
		)
		toolBroker.SetLookup(func(ctx context.Context, env toolbroker.ExecEnv, name string) (toolbroker.Tool, bool, error) {
			return toolRegistry.LookupTool(ctx, env.Scope.Org, env.Scope.Project, env.Scope.RunID, name)
		})
		// Wire the MCP client (E12 T5): a discovered MCP tool resolves through its run's connection rider and
		// runs in a per-call, network-less OCI sandbox (stdio) or a vetted HTTP transport. The SAME manager
		// backs the dispatch lookup (Call) and the admin discover API (repo.SetMCP), and a label-scoped orphan
		// sweep reclaims any container a crash left behind. Absent a Docker driver, stdio MCP fails cleanly;
		// HTTP MCP still works.
		mcpManager := mcpManagerFromEnv(spine, broker, route)
		toolRegistry.SetMCP(mcpManager)
		repo.SetMCP(mcpManager)
		startMCPOrphanSweep(ctx, supervisor)
		// Wire the E12 T8 hooks (spec §28.17): the registry fires a run's registered hooks at the five pinned
		// dispatch points. platform_inline hooks dispatch to the code-defined handler table (deny-all is the
		// deny-visible fixture); remote_http hooks reuse the SAME T4 signed transport + org-scoped secret
		// resolver wired above. The orchestrator fires through the registry (SetHookFirer); no hook fires unless
		// an admin registers one, so a hook-less run is bit-unchanged.
		toolRegistry.SetHookHandlers(extensions.PlatformHookHandlers())
		orch := execution.NewOrchestrator(repo, gateway, broker, toolBroker)
		orch.SetModelRoute(route)
		// §20.12 queue-deadline: a run that waited in the admission queue past PALAI_QUEUE_DEADLINE is
		// timed out at dispatch, before any billable compute. Unset ⇒ disabled (runs never expire on
		// queue age), so the deterministic tiers are bit-unchanged.
		orch.SetQueueDeadline(envDuration("PALAI_QUEUE_DEADLINE"))
		orch.SetHookFirer(toolRegistry)
		// Wire the repository publisher the approval pump publishes through (spec §30.9-30.10), gated on
		// the GitHub App environment. Absent it, an approved publication waits (the pump is a no-op) — no
		// push happens without a configured destination. ponytail: the live wave sets the env; the
		// deterministic tier proves the pump with a fake publisher.
		if publisher := repositoryPublisherFromEnv(); publisher != nil {
			orch.SetPublisher(publisher)
		}
		// Wire the checkpoint + snapshot sinks whenever an object store exists (spec §26.1-26.2, §29.10).
		// Unlike the changeset writer, neither is gated on a coding workspace — a checkpoint boundary
		// applies to any run, and the snapshot sink is a no-op for a run with no workspace. Absent an
		// object store, a checkpoint offer is dropped and a pause cuts no snapshot (no durable boundary).
		// The snapshot sink shares artStore (the same Put/Get shape) so a pause boundary cuts + links a
		// workspace snapshot (SES-009); without it workspaceRestorable is vacuously true and the snapshot
		// half is inert.
		if artStore != nil {
			orch.SetCheckpointSink(execution.NewCheckpointSink(artStore, recovery.New(spine.Pool())))
			orch.SetSnapshotSink(execution.NewSnapshotSink(artStore, spine))
			// The changeset writer doubles as the research tool's body-artifact seam, so it is wired on the
			// object store — NOT the coding workspace. The changeset compile still only runs for a
			// workspace-bound run (it needs a base to diff), so hoisting it here is behavior-preserving for
			// the changeset while letting a workspace-less research run persist its full fetched body.
			orch.SetChangesetWriter(artifacts.NewWriter(artStore, spine.Pool()))
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
			// The changeset writer is wired above on the object store (it doubles as the research
			// body-artifact seam); a workspace-bound run reuses that same writer for its changeset compile.
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
	// Uncertain-tool reconciliation loop (spec §26.7, E10 T7): resolves tool_calls stuck `uncertain` by a
	// kill-between-execute-and-commit. The RemoteToolProber (E12 T4) is the FIRST real destination prober:
	// for an uncertain remote_http call it reads the durable remote-operation ledger, so a LATE signed
	// callback (which wrote late_result there, never touching the tool ledger) resolves the call to
	// reconciled_completed. A non-remote uncertain call has no operation row, so it still escalates to
	// manual_resolution — the pre-T4 behaviour, unchanged.
	toolReconciler := execution.NewUncertainReconciler(spine,
		execution.NewRemoteToolProber(remotehttp.NewOperations(spine.Pool())), 30*time.Second, 100)
	go supervisor.Supervise(ctx, "tool-reconciler", toolReconciler.Run)
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

// mcpManagerFromEnv builds the MCP client the discovered-tool dispatch + admin discover paths share (spec
// §28.13-28.14, E12 T5). The stdio transport needs a Docker interactive driver (a per-call, network-less,
// mount-less sandbox); absent it, stdio MCP fails cleanly while HTTP MCP still works. The bearer for an HTTP
// connection is resolved from its secret_ref at request time via the org-scoped file bridge (never inline),
// and progress notifications journal advisory tool_call.progress.v1 events through the spine.
//
// E12 T6: a sampling-enabled connection routes a server sampling/createMessage as a SEPARATE budgeted model
// step through the SAME broker + route the engine's model steps use (the platform's own model credential,
// control-plane-side), journalled as model_step.created/completed.v1 events tagged source:"mcp_sampling". A
// connection that does not enable sampling (the default) stays default-deny regardless.
func mcpManagerFromEnv(spine *coordinator.Store, broker *modelbroker.Broker, route execution.ModelRoute) *mcpclient.Manager {
	driver, err := oci.NewDockerInteractiveDriver()
	if err != nil {
		log.Printf("mcp: bind docker interactive driver: %v (stdio MCP disabled; http MCP still available)", err)
		driver = nil
	}
	sampling := execution.NewMCPSamplingRouter(broker, route,
		func(ctx context.Context, scope mcpclient.CallScope, eventType string, payload []byte) error {
			return spine.AppendModelStep(ctx,
				coordinator.Tenant{Organization: scope.Org, Project: scope.Project},
				scope.SessionID, scope.ResponseID, scope.RunID, eventType, payload)
		})
	return mcpclient.NewManager(mcpclient.Config{
		Driver:         driver,
		Secrets:        mcpSecretResolver,
		Sink:           execution.NewMCPProgressSink(spine),
		Sampling:       sampling,
		DefaultTimeout: envDurationOr("PALAI_MCP_TIMEOUT", 30*time.Second),
	})
}

// dbSecretStore is the DB-backed secret store (E13 Task 3), set once at boot when a master key is configured
// (nil otherwise). It is the single front-door the four env resolvers share via dbSecret, so a secret
// provisioned over the API wins and an absent ref falls through to the env-file bridge.
// ponytail: a boot-set composition-root singleton — the resolvers are themselves package funcs by design; it
// is written once before any goroutine starts, so no synchronization is needed.
var dbSecretStore *identity.SecretStore

// secretResolveTimeout bounds a DB-backed secret resolve. It now runs on live request paths (MCP connect,
// webhook/remote-tool delivery) that previously did only local file reads, so a hung/partitioned Postgres
// must not block them indefinitely — a timeout degrades to the env bridge.
// ponytail: fixed 2s; make it an env knob (envDurationOr, off the hot path) only if operators ask.
const secretResolveTimeout = 2 * time.Second

// dbSecret consults the DB-backed store (when configured) before the env-file fallback, returning
// (value, hit, err). The error is load-bearing:
//   - a DECRYPT failure (the row exists but the master key is wrong/corrupt) FAILS CLOSED — the caller must
//     NOT serve the superseded env secret, or a rotation is silently defeated (the SEC-002 failure);
//   - a timeout / DB-unavailable error (bounded by secretResolveTimeout) or a genuine miss degrades to the
//     env bridge (the allowed fallback), so a store hiccup does not fail an env-satisfiable lookup.
//
// The org is server-minted, so the store scopes the read to it and RLS denies any foreign row.
func dbSecret(org, ref string) ([]byte, bool, error) {
	if dbSecretStore == nil {
		return nil, false, nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), secretResolveTimeout)
	defer cancel()
	v, ok, err := dbSecretStore.Resolve(ctx, org, ref)
	if err != nil {
		if errors.Is(err, identity.ErrSecretDecrypt) {
			return nil, false, err // fail closed: never fall back to a superseded env secret
		}
		log.Printf("secret store: resolve ref %q under org %q: %v (falling back to env bridge)", ref, org, err)
		return nil, false, nil
	}
	return v, ok, nil
}

// mcpSecretResolver bridges an MCP connection's secret_ref handle to the bearer bytes at request time (the
// webhookSecretResolver twin): the DB-backed store (E13 T3) is consulted first, then
// PALAI_MCP_SECRET_FILE_<ORG>__<REF> holds a FILE PATH, never the secret inline, read only here and never
// logged. The org prefix is a server-minted hard tenant boundary, so a tenant's ref can only name a secret
// provisioned under its OWN org.
func mcpSecretResolver(org, ref string) ([]byte, error) {
	if org == "" || ref == "" {
		return nil, errors.New("empty mcp secret org/ref")
	}
	if v, ok, err := dbSecret(org, ref); err != nil {
		return nil, err
	} else if ok {
		return v, nil
	}
	if strings.Contains(secretEnvKey(org), "__") {
		return nil, fmt.Errorf("ambiguous mcp secret org key %q", org)
	}
	path := os.Getenv("PALAI_MCP_SECRET_FILE_" + secretEnvKey(org) + "__" + secretEnvKey(ref))
	if path == "" {
		return nil, fmt.Errorf("no secret bridge configured for mcp ref under org %q", org)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	// Trim trailing whitespace/newline: a secret file written with a trailing \n would otherwise corrupt
	// the Authorization header (an opaque upstream 401).
	return []byte(strings.TrimSpace(string(b))), nil
}

// startMCPOrphanSweep launches the label-scoped MCP orphan-container sweep (spec §28.13 named gap, E12 T5):
// a crash between a per-call container's Start and its teardown leaves an orphan, which this reclaims. It is
// STRICTLY io.palai.sandbox=mcp — an engine/shell container is never touched (and the engine reaper never
// touches an MCP one). It runs like the artifact-orphan-gc sweep: unconditionally supervised, a killed
// process just misses ticks. Grace/interval are env-tunable.
func startMCPOrphanSweep(ctx context.Context, supervisor *coordinator.Supervisor) {
	grace := envDurationOr("PALAI_MCP_SWEEP_GRACE", 2*time.Minute)
	interval := envDurationOr("PALAI_MCP_SWEEP_INTERVAL", time.Minute)
	sweeper, err := mcpclient.NewSweeper(grace)
	if err != nil {
		log.Printf("mcp orphan-sweep: %v (disabled)", err)
		return
	}
	go supervisor.Supervise(ctx, "mcp-orphan-sweep", func(ctx context.Context) error { return sweeper.Run(ctx, interval) })
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

// startWebhookPump launches the supervised outbound-webhook delivery pump (spec §21.4-21.6). It is a
// system loop that serves every project's endpoints and is inert until an endpoint is registered, so
// it runs unconditionally (like the retention/GC sweeps) — a killed process just misses ticks; the
// next run resumes from each endpoint's durable cursor. A delivery never blocks a run (AUT-011).
func startWebhookPump(ctx context.Context, store *automation.WebhookStore, supervisor *coordinator.Supervisor) {
	pump := automation.NewWebhookPump(store, webhook.NewSender(), webhookSecretResolver, automation.PumpConfig{
		Tick:        envDurationOr("PALAI_WEBHOOK_TICK", time.Second),
		BaseBackoff: envDurationOr("PALAI_WEBHOOK_BACKOFF_BASE", 30*time.Second),
		MaxBackoff:  envDurationOr("PALAI_WEBHOOK_BACKOFF_MAX", time.Hour),
	}, log.Printf)
	go supervisor.Supervise(ctx, "webhook-pump", pump.Run)
}

// startDeliveryReconciler launches the supervised trigger delivery-reconciler (spec §20.2.2, E11 Task 2).
// It is a system loop that serves every project's deferred deliveries and is inert until one is deferred,
// so it runs unconditionally (like the webhook pump): it admits the FIFO head of each gate-opened
// correlation-key group and re-decides crash remnants stranded in `mapped`. The loop name is pinned
// "delivery-reconciler" — T5 folds inbound-source sweeps into the same loop. A killed process just misses
// ticks; the next run resumes from the durable delivery rows.
func startDeliveryReconciler(ctx context.Context, store *automation.TriggerStore, supervisor *coordinator.Supervisor) {
	rec := automation.NewDeliveryReconciler(store,
		envDurationOr("PALAI_TRIGGER_RECONCILE_TICK", time.Second),
		envDurationOr("PALAI_TRIGGER_MAPPED_GRACE", time.Minute),
		envIntDefault("PALAI_TRIGGER_RECONCILE_BATCH", 100),
		log.Printf).
		// Short-retention scrub of terminal inbound raw payloads (0 ⇒ disabled, the operator opt-in shape of
		// PALAI_RETENTION_STORE_FALSE_TTL; encryption-at-rest is E13, no "encrypted" claim here).
		WithInboundRawTTL(envDuration("PALAI_INBOUND_RAW_TTL"))
	go supervisor.Supervise(ctx, "delivery-reconciler", rec.Run)
}

// startScheduleTicker launches the supervised schedule-ticker (spec §33, E11 Task 3). It is a SIBLING of
// the delivery-reconciler, not an extension: the reconciler sweeps trigger_deliveries remnants, the ticker
// sweeps schedules/occurrences — the due-scan (claim durable occurrences) and the pending-occurrence
// handoff sweep, both inside its Run. It is a system loop that serves every project's schedules and is
// inert until one is due, so it runs unconditionally (like the webhook pump / delivery-reconciler). A
// killed process just misses ticks; the next run resumes from the durable schedule + occurrence rows.
func startScheduleTicker(ctx context.Context, store *automation.ScheduleStore, supervisor *coordinator.Supervisor) {
	ticker := automation.NewScheduleTicker(store,
		envDurationOr("PALAI_SCHEDULE_TICK", time.Second),
		envIntDefault("PALAI_SCHEDULE_BATCH", 100),
		log.Printf)
	go supervisor.Supervise(ctx, "schedule-ticker", ticker.Run)
}

// webhookSecretResolver bridges an endpoint's SecretRef handle to the signing-secret bytes at delivery
// time (the E09 credential-broker hand-off pattern): PALAI_WEBHOOK_SECRET_FILE_<ORG>__<REF> holds a
// FILE PATH, never the secret inline, and the bytes are read only here and never logged (E13 seals the
// file at rest). The env key is scoped by the endpoint's ORG so a tenant's SigningSecretRef can only
// name a secret provisioned under its OWN org — a foreign ref resolves to no env var (F2). The org is
// server-minted (never tenant-forgeable), so the org prefix is a hard tenant boundary. An unresolved
// ref fails the attempt (a retry), never an unsigned delivery.
func webhookSecretResolver(org, ref string) ([]byte, error) {
	if org == "" || ref == "" {
		return nil, errors.New("empty webhook secret org/ref")
	}
	if v, ok, err := dbSecret(org, ref); err != nil {
		return nil, err
	} else if ok {
		return v, nil
	}
	// Belt-and-braces: "__" is the org/ref delimiter, so an org whose normalized key form already contains
	// it would make the env key ambiguous with a different split. The org is server-minted (never
	// tenant-forgeable), so this is defence-in-depth, not the primary tenant boundary.
	if strings.Contains(secretEnvKey(org), "__") {
		return nil, fmt.Errorf("ambiguous webhook secret org key %q", org)
	}
	path := os.Getenv("PALAI_WEBHOOK_SECRET_FILE_" + secretEnvKey(org) + "__" + secretEnvKey(ref))
	if path == "" {
		return nil, fmt.Errorf("no secret bridge configured for webhook ref under org %q", org)
	}
	return os.ReadFile(path)
}

// inboundSecretResolver is the receiver-side sibling of webhookSecretResolver (E11 Task 5): it bridges a
// trigger's inbound source-secret ref to bytes via PALAI_INBOUND_SECRET_FILE_<ORG>__<REF> (a FILE PATH,
// never inline; E13 seals the file at rest). The org prefix is a server-minted hard tenant boundary, so a
// tenant's ref can only name a secret provisioned under its OWN org — and the inbound namespace is
// DISTINCT from the outbound PALAI_WEBHOOK_SECRET_FILE_ one, so the two secret sets are non-interchangeable.
// An unresolved ref fails verification (a generic 404 upstream — no config oracle), never an unsigned accept.
func inboundSecretResolver(org, ref string) ([]byte, error) {
	if org == "" || ref == "" {
		return nil, errors.New("empty inbound secret org/ref")
	}
	if v, ok, err := dbSecret(org, ref); err != nil {
		return nil, err
	} else if ok {
		return v, nil
	}
	// Belt-and-braces, as in webhookSecretResolver: a normalized org key carrying the "__" delimiter is
	// ambiguous; reject it rather than resolve a colliding key. The org is server-minted, so this is
	// defence-in-depth on top of the org-scoped namespace.
	if strings.Contains(secretEnvKey(org), "__") {
		return nil, fmt.Errorf("ambiguous inbound secret org key %q", org)
	}
	path := os.Getenv("PALAI_INBOUND_SECRET_FILE_" + secretEnvKey(org) + "__" + secretEnvKey(ref))
	if path == "" {
		return nil, fmt.Errorf("no secret bridge configured for inbound ref under org %q", org)
	}
	return os.ReadFile(path)
}

// remoteToolSecretResolver is the third sibling of webhook/inboundSecretResolver (E12 Task 4): it bridges
// a tool_revision.secret_ref handle to the HMAC signing-secret bytes via PALAI_REMOTE_TOOL_SECRET_FILE_
// <ORG>__<REF> (a FILE PATH, never inline; E13 seals the file at rest). The SAME secret signs the outbound
// invoke and verifies the inbound callback. The org prefix is a server-minted hard tenant boundary, so a
// tenant's ref can only name a secret provisioned under its OWN org — and the remote-tool namespace is
// DISTINCT from the webhook/inbound ones, so the three secret sets are non-interchangeable. An unresolved
// ref fails the invoke (a retry) / a generic-404 callback, never an unsigned request or accept.
func remoteToolSecretResolver(org, ref string) ([]byte, error) {
	if org == "" || ref == "" {
		return nil, errors.New("empty remote tool secret org/ref")
	}
	if v, ok, err := dbSecret(org, ref); err != nil {
		return nil, err
	} else if ok {
		return v, nil
	}
	// Belt-and-braces, as in the sibling resolvers: a normalized org key carrying the "__" delimiter is
	// ambiguous; reject it rather than resolve a colliding key. The org is server-minted, so this is
	// defence-in-depth on top of the org-scoped namespace.
	if strings.Contains(secretEnvKey(org), "__") {
		return nil, fmt.Errorf("ambiguous remote tool secret org key %q", org)
	}
	path := os.Getenv("PALAI_REMOTE_TOOL_SECRET_FILE_" + secretEnvKey(org) + "__" + secretEnvKey(ref))
	if path == "" {
		return nil, fmt.Errorf("no secret bridge configured for remote tool ref under org %q", org)
	}
	return os.ReadFile(path)
}

// secretEnvKey normalizes a SecretRef into an env-var suffix (upper alphanumerics, others to '_').
func secretEnvKey(ref string) string {
	var b strings.Builder
	for _, r := range strings.ToUpper(ref) {
		if (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
		} else {
			b.WriteByte('_')
		}
	}
	return b.String()
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

// edgeLimitsFromEnv reads the §20.12 basic-tier edge admission control (E13 T7). Every value
// defaults to zero = disabled, so a stack that sets none keeps the pre-E13-T7 behaviour (no
// request-rate limiter, no per-project run caps). Operators (and the live smoke) enable them
// without a rebuild.
func edgeLimitsFromEnv() api.EdgeLimits {
	return api.EdgeLimits{
		RequestRatePerSec: envFloat("PALAI_REQUEST_RATE_PER_SEC"),
		RequestBurst:      envInt("PALAI_REQUEST_BURST"),
		MaxConcurrentRuns: envInt("PALAI_MAX_CONCURRENT_RUNS"),
		MaxQueuedRuns:     envInt("PALAI_MAX_QUEUED_RUNS"),
	}
}

func envFloat(name string) float64 {
	f, err := strconv.ParseFloat(os.Getenv(name), 64)
	if err != nil {
		return 0
	}
	return f
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
