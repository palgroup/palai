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
	"net"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	// time/tzdata embeds the IANA zoneinfo database in the binary so schedule timezones resolve even in a
	// container without /usr/share/zoneinfo (spec §33.1; time.LoadLocation's documented final fallback).
	_ "time/tzdata"

	"github.com/palgroup/palai/adapters/integrations/a2a"
	mcpclient "github.com/palgroup/palai/adapters/integrations/mcp"
	"github.com/palgroup/palai/adapters/integrations/webhook"
	fake "github.com/palgroup/palai/adapters/models/fake"
	openaicompatible "github.com/palgroup/palai/adapters/models/openai_compatible"
	providerone "github.com/palgroup/palai/adapters/models/provider_one"
	providertwo "github.com/palgroup/palai/adapters/models/provider_two"
	"github.com/palgroup/palai/adapters/repositories"
	"github.com/palgroup/palai/adapters/sandboxes/oci"
	"github.com/palgroup/palai/adapters/sandboxes/oci/workspace"
	remotehttp "github.com/palgroup/palai/adapters/tools/http"
	"github.com/palgroup/palai/apps/control-plane/api"
	"github.com/palgroup/palai/apps/control-plane/api/middleware"
	"github.com/palgroup/palai/apps/control-plane/internal/artifacts"
	"github.com/palgroup/palai/apps/control-plane/internal/automation"
	"github.com/palgroup/palai/apps/control-plane/internal/execution"
	tools "github.com/palgroup/palai/apps/control-plane/internal/execution/tools"
	"github.com/palgroup/palai/apps/control-plane/internal/extensions"
	"github.com/palgroup/palai/apps/control-plane/internal/identity"
	"github.com/palgroup/palai/apps/control-plane/internal/knowledge"
	"github.com/palgroup/palai/apps/control-plane/internal/metering"
	"github.com/palgroup/palai/apps/control-plane/internal/metrics"
	"github.com/palgroup/palai/apps/control-plane/internal/store"
	"github.com/palgroup/palai/apps/control-plane/internal/workers"
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
	// --migrate-and-exit is the Kubernetes migration-Job mode (deploy/helm): apply the schema and
	// exit 0, without binding the runner gateway or serving. The Helm chart runs this as a
	// pre-install/pre-upgrade hook so migrations complete BEFORE the control-plane Deployment rolls,
	// rather than N racing pods each migrating at boot. The boot-time Migrate above is idempotent, so
	// a stack that does NOT run the Job (compose) is unchanged. Bootstrap/seed is left to the serving
	// process — the Job's job is migrations only.
	if migrateAndExit() {
		log.Printf("palai control-plane: migrations applied, exiting (--migrate-and-exit)")
		return
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

	// The capability-worker gateway (E17 T9, spec §31): the outbound-enrolled enroll/claim/redeem/result
	// surface an out-of-process worker dials. Built here so the secret store above (nil unless a master key
	// is configured) is the resolver a job-scoped secret handle redeems through.
	capabilityWorkers := startCapabilityWorkerGateway(os.Getenv("PALAI_CAPABILITY_WORKER_LISTEN_ADDR"), repo, secretStore)

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
	//
	// WithModelRoutes mounts the DB-backed model-routing write surface (E13 T8): the store is the same
	// non-nil repo the positional seams use, so it is unconditional.
	// WithKnowledge mounts the knowledge spine (E17 T4): the FTS ingestion/index/retrieval store over the
	// same spine pool. Unconditional like WithUsage — it needs no external key material, and a stack that
	// serves no knowledge simply gets no traffic on the routes. Its discovery TIER is not decided here: the
	// E17 T11 exit gate recomputes it from the KNO claim outcomes (it closed "stable"), and
	// apps/control-plane/api asserts the served map is bit-equal to that recompute.
	edge := edgeLimitsFromEnv()
	routerOpts := []api.RouterOption{
		api.WithEdgeLimits(edge),
		api.WithUsage(metering.New(repo.Spine().Pool())),
		api.WithModelRoutes(repo),
		api.WithKnowledge(knowledge.New(repo.Spine().Pool())),
	}
	if secretStore != nil {
		routerOpts = append(routerOpts, api.WithSecretRefs(secretStore))
	}
	// The A2A 1.0 server projection (E17 T2, spec §38): the DB-backed interface + task store over the same
	// spine pool, wired behind the api.Admitter so an inbound A2A message admits through the SAME §20.9 path a
	// POST /v1/responses takes (no invented run identity, §34.1) under the SAME per-project caps. Mounting it
	// is what makes discovery advertise `a2a` (capabilities derives the tier from the live mount, never a
	// static claim — §2). PALAI_PUBLIC_BASE_URL is the public origin the Agent Card advertises its exact
	// interface URLs under (empty ⇒ relative). Push DELIVERY and inbound file ingest stay unwired honest
	// ceilings (§5/§6): the card advertises push only when a Pusher exists.
	a2aPusher := a2aPusherFromEnv()
	a2aStore := a2a.NewStore(repo.Spine().Pool(), middleware.NewID)
	a2aServer := api.NewA2AServer(repo, a2aStore, a2aStore,
		api.AdmissionLimits{MaxConcurrentRuns: edge.MaxConcurrentRuns, MaxQueuedRuns: edge.MaxQueuedRuns},
		os.Getenv("PALAI_PUBLIC_BASE_URL"), a2aPusher)
	routerOpts = append(routerOpts, api.WithA2A(a2aServer, a2aServer.PublicCardHandler()))
	// The Slack Events API receiver (E19 T1, spec §36): the 000035 connection/thread store over the same spine
	// pool, wired behind the SAME api.Admitter the A2A surface and POST /v1/responses use, so a Slack event
	// admits through the one §20.9 path under the one set of per-project caps — no parallel admission. The
	// tenant comes ONLY from the resolved slack_connections row (no bearer exists on this route; the v0
	// signature is the auth), and the run target from that row's default_policy. Unconditional like WithUsage:
	// it needs no external key material, and a stack with no registered workspace simply gets no traffic —
	// mounting is also what lets discovery advertise `slack` at all, at the tier T11 recomputes (preview).
	//
	// The interactivity half (E19 T2) is wired onto the SAME bridge with WithDecisions: an interactive
	// approval resolves the same connection, verifies under the same signing secret, and then drives the
	// coordinator's one-shot approval primitive — SlackAuthorizationPolicyFor + ApproverAuthorized are
	// enforced there, so an unmapped clicker enqueues nothing. It needs the approval spine and an outbound
	// HTTP client; PALAI_SLACK_API_BASE_URL overrides Slack's own base for a staging/proxied deployment and
	// is empty in production (⇒ https://slack.com/api). The bot token is redeemed from bot_token_ref at call
	// time by the same org-scoped resolver.
	//
	// The REGISTRATION half (E19 T9) rides the same store through its own seam: until it existed,
	// CreateSlackConnection had zero non-test callers, so an operator who had just been handed a signing
	// secret and a bot token had NO way to register the workspace except hand-written SQL — which made the
	// phase's own promise ("supply the credentials, run the live legs unchanged") untrue at step one. It is
	// a separate seam from the admission bridge on purpose: registration is a bearer-scoped operator action,
	// the receivers are unauthenticated signature-verified callbacks.
	slackStore := extensions.New(repo.Spine().Pool())
	slackBridge := extensions.NewSlackAdmitter(
		slackStore, repo, slackSecretResolver,
		api.AdmissionLimits{MaxConcurrentRuns: edge.MaxConcurrentRuns, MaxQueuedRuns: edge.MaxQueuedRuns}).
		WithDecisions(repo.Spine(), http.DefaultClient, os.Getenv("PALAI_SLACK_API_BASE_URL")).
		// The RUN FOLLOWER (E20 T1): while a Slack-born run works, its thread shows a status and the message
		// appears when the first step lands instead of only after the terminal transaction. `repo` is the SAME
		// api.EventReader the SSE endpoint tails, so no second journal read path exists. Unconditional and
		// scope-free — assistant.threads.setStatus and the chat.*Stream family are all `chat:write`, which
		// this app already holds, so nothing here waits on a reinstall or on the agent panel.
		WithStreaming(repo, supervisor, 0)
	// The IMAGE leg (E20, slack_vision.go): a screenshot dropped in a thread is fetched with the bot token and
	// named by the run's input, so the model can see it. Gated on the object store because that is where the
	// bytes go — without one there is nothing to put an image in, and a shared file is admitted text-only
	// exactly as before. `files:read` is the only scope it needs and this app already holds it.
	if artStore != nil {
		slackBridge = slackBridge.WithFileFetch(http.DefaultClient, artifacts.NewWriter(artStore, repo.Spine().Pool()))
	}
	routerOpts = append(routerOpts, api.WithSlack(slackBridge), api.WithSlackInteractions(slackBridge),
		api.WithSlackConnections(extensions.NewSlackRegistry(slackStore)))
	// The queue bridges (E19 T6, spec §34.1-34.5). ONE store serves all three halves: the admin surface
	// mounted here, the supervised consume→admit bridge, and the outbound DeliverDue pump (both started
	// below). Unconditional like WithUsage — the reference adapter's broker is this deployment's own
	// Postgres, so it needs no external credential, and a stack with no registered binding does no work.
	//
	// The inbound bridge admits through `repo.Spine()` — the SAME coordinator POST /v1/responses and the
	// trigger pipeline admit through — but it does NOT go through IngestInbound: a broker's auth boundary is
	// the CONNECTION, not a per-message HMAC, so it is a second admission entrypoint rather than the signed
	// one bent by a flag. The full reasoning is at the top of automation/queue_bridge.go.
	//
	// Mounting is what would let discovery advertise `queues` at all; it does NOT raise the tier — no broker
	// PRODUCT exists in this tree (E17 §6 leg 5), so `queues` stays preview and only the T11 recompute writes
	// the word.
	queueStore := automation.NewQueueStore(repo.Spine().Pool())
	routerOpts = append(routerOpts, api.WithQueueConnections(queueStore, nil))
	// Discovery advertises `capability-workers` ONLY where the gateway above actually BOUND its listener —
	// the option is passed off the returned value, never off the env var, so the claim cannot outlive the
	// mount (§2; E19 T8a closed the static "stable" that a binary not importing internal/workers was serving).
	if capabilityWorkers != nil {
		routerOpts = append(routerOpts, api.WithCapabilityWorkers())
	}
	// The Prometheus /metrics exposition (E14 T6): installation-aggregate operational series over the
	// same spine pool, mounted unauthenticated on the internal top mux beside /healthz. The runner-session
	// gauge reads the gateway (nil in assignment-only tiers, reported as 0); the object-store up-probe reads
	// artStore (a typed-nil *artifacts.Store must NOT wrap a non-nil interface, so the pinger is built
	// conditionally — the same nil-interface guard WithSecretRefs uses). PALAI_METRICS_DISK_PATH names the
	// data volume to statfs; unset defaults to "/".
	var runnerSessions func() int64
	if gateway != nil {
		runnerSessions = gateway.Connected
	}
	var objStorePinger metrics.ObjectStorePinger
	if artStore != nil {
		objStorePinger = artStore
	}
	collector := metrics.New(repo.Spine().Pool(), runnerSessions, supervisor.Restarts, objStorePinger, os.Getenv("PALAI_METRICS_DISK_PATH"))
	routerOpts = append(routerOpts, api.WithMetrics(collector))
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
			routerOpts...), supervisor, gateway),
		ReadHeaderTimeout: 10 * time.Second,
	}

	startDispatch(ctx, repo, gateway, supervisor, artStore)
	startWebhookPump(ctx, webhookStore, supervisor)
	startQueueBridges(ctx, queueStore, repo.Spine(), edge, supervisor)
	startDeliveryReconciler(ctx, triggerStore, supervisor)
	startScheduleTicker(ctx, scheduleStore, supervisor)
	startRetention(ctx, repo, supervisor, artStore)
	startOrphanGC(ctx, repo, supervisor, artStore)
	// The Slack RETURN LEG's poster. Unconditional like the other pumps and for the same reason: it serves
	// every project and stays inert until a Slack-born run terminates, so there is nothing for an operator
	// to configure. Its work was committed inside each run's terminal transaction, which is why this loop
	// being down (or restarting) delays an answer and can never lose one.
	go supervisor.Supervise(ctx, "slack-reply-pump", extensions.NewSlackReplyPump(slackBridge).Run)
	drainSlackSocket := startSlackSocket(ctx, slackBridge, supervisor)

	log.Printf("palai control-plane listening on %s", addr)
	serveWithGracefulDrain(srv, gateway, drainSlackSocket)
}

// startSlackSocket launches the Slack Socket Mode connect loop (E19 T3, spec §36) and returns its drain, or
// nil when the deployment has not configured Socket Mode. It is the ONLY start* here that is conditional on
// an env var, and the reason is the transport itself: Socket Mode holds an OUTBOUND WebSocket to Slack for a
// SPECIFIC workspace, so unlike the pumps and reapers — which serve every project and sit inert until work
// exists — there is nothing for it to do until an operator names one.
//
// PALAI_SLACK_SOCKET_TEAM_ID is that workspace's Slack team id (§0.1). The connection it resolves to supplies
// everything else: the tenant, the run target, the bot user for the self-loop guard, and the app_token_ref
// whose xapp- token is Socket Mode's only authentication. Nothing about the transport is configured twice.
//
// WHY THIS IS WORTH MOUNTING AT ALL, in one line an operator can act on: Socket Mode needs NO PUBLIC URL — no
// tunnel, no DNS, no inbound firewall hole — so it is the cheapest way to put this deployment on a real Slack
// workspace (plan §0.1).
//
// It gets its OWN cancellable context rather than riding the process context every other loop uses, because
// it is the one loop with something to drain: an in-flight envelope has already been acknowledged to Slack,
// so it must finish rather than be killed. serveWithGracefulDrain calls the returned func on SIGTERM.
func startSlackSocket(ctx context.Context, bridge *extensions.SlackAdmitter, supervisor *coordinator.Supervisor) func(context.Context) error {
	socket := bridge.SocketMode(os.Getenv("PALAI_SLACK_SOCKET_TEAM_ID"))
	if socket == nil {
		return nil
	}
	sockCtx, stop := context.WithCancel(ctx)
	finished := make(chan struct{})
	go func() {
		defer close(finished)
		supervisor.Supervise(sockCtx, "slack-socket", socket.Run)
	}()
	log.Printf("palai control-plane: Slack Socket Mode enabled for the configured workspace")
	return func(drainCtx context.Context) error {
		stop()
		select {
		case <-finished:
			return nil
		case <-drainCtx.Done():
			return drainCtx.Err()
		}
	}
}

// serveWithGracefulDrain serves until SIGTERM/Interrupt, then DRAINS the runner gateway before exit:
// it stops offering NEW leases and waits (bounded by PALAI_DRAIN_TIMEOUT, default 20s) for the in-flight
// lease to finish, so a control-plane swap — compose sends SIGTERM to the OLD container during
// `up -d control-plane` — is the §48.4 "runner drain" step rather than a hard kill of an active run.
// Whatever does not finish inside the window is reclaimed and completed by the E10 recovery layer on the
// new control-plane, so a run always survives the swap on its pinned engine. A stack with no gateway
// (assignment-only tiers) or no in-flight lease drains instantly, so ordinary shutdowns are unchanged.
//
// drainSocket (nil unless Socket Mode is configured, E19 T3) is drained on the SAME budget and BEFORE the
// gateway, because it is upstream of it: the socket is what admits new Slack work, and letting it keep
// admitting while runner leases drain would mean draining against a source still filling the queue. Its own
// in-flight envelope is already ACKNOWLEDGED to Slack — Slack will not redeliver it — so it finishes rather
// than being killed.
func serveWithGracefulDrain(srv *http.Server, gateway *execution.RunnerGateway, drainSocket func(context.Context) error) {
	sigCtx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	serveErr := make(chan error, 1)
	go func() { serveErr <- srv.ListenAndServe() }()

	select {
	case err := <-serveErr:
		log.Fatalf("serve: %v", err) // the listener died on its own — fatal, as before
	case <-sigCtx.Done():
	}

	drainTimeout := envDuration("PALAI_DRAIN_TIMEOUT")
	if drainTimeout <= 0 {
		drainTimeout = 20 * time.Second
	}
	if drainSocket != nil {
		socketCtx, cancel := context.WithTimeout(context.Background(), drainTimeout)
		if err := drainSocket(socketCtx); err != nil {
			log.Printf("slack socket drain did not quiesce (%v); an in-flight envelope may have been abandoned after it was acknowledged", err)
		} else {
			log.Printf("slack socket drain complete")
		}
		cancel()
	}

	if gateway != nil {
		drainCtx, cancel := context.WithTimeout(context.Background(), drainTimeout)
		if err := gateway.Drain(drainCtx); err != nil {
			log.Printf("runner drain did not fully quiesce (%v); interrupted work recovers on the next control-plane", err)
		} else {
			log.Printf("runner drain complete: no leases in flight")
		}
		cancel()
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Printf("http shutdown: %v", err)
	}
}

// migrateAndExit reports whether the binary was invoked with --migrate-and-exit (the Kubernetes
// migration-Job mode). It is a bare arg scan rather than a flag.FlagSet so the serving path — which
// takes no flags — is untouched and no other invocation form changes.
func migrateAndExit() bool {
	for _, a := range os.Args[1:] {
		if a == "--migrate-and-exit" {
			return true
		}
	}
	return false
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
			tools.KnowledgeRetrievalTool(knowledge.New(repo.Spine().Pool())), // ACL-first, cited knowledge retrieval (E17 T5); untrusted result, server-derived scope, KB-wide (fail-closed)
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
		// Wire remote child-run dispatch (E19 T5, §38.5): a child.request naming a REGISTERED
		// a2a_remote_agents row is executed by that remote instead of a local engine. The lookup is the same
		// tenant-scoped a2a store the server half uses; the dialer is the E17 T3 client in PRODUCTION posture
		// — AllowPrivate stays false, so a remote agent registered at a loopback/RFC1918/metadata address is
		// refused by egress before any dial, and no Files sink is wired, so a remote that returns a file part
		// fails its child honestly rather than losing the file. The bearer is redeemed ONLY from the agent
		// row's auth_connection_ref by the org-scoped resolver — there is no parameter through which this
		// process could hand the remote the parent's or the platform's credential (A2A-005/SUB-007).
		//
		// Unconditional, like SetHookFirer: it needs no external key material and a project that registered
		// no remote agent simply never takes the branch. Registration itself has no HTTP surface yet — the
		// rows are created directly today — which is a NAMED gap, not a claim this task closes.
		orch.SetRemoteChildren(a2a.NewStore(spine.Pool(), middleware.NewID),
			a2a.NewClient(a2a.ClientConfig{Secrets: a2aRemoteSecretResolver}))
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
			artifactWriter := artifacts.NewWriter(artStore, spine.Pool())
			orch.SetChangesetWriter(artifactWriter)
			// The READ side of the same store, which is what lets a run see an image: an `image_ref` in the
			// run's input resolves to bytes here, control-plane-side (spec §24, §25.10). Same object store,
			// so a stack that can persist a changeset can also show a model a screenshot.
			orch.SetImageReader(artifactWriter)
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
			// A binding that names a connection_ref clones under its own tenant's credential (E13 T9);
			// the resolver is inert for the ref-less bindings that take the global broker above.
			orch.SetConnectionSecrets(repositoryConnectionSecret)
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

// modelBrokerFromEnv builds the model broker and the DEPLOYMENT-DEFAULT route, selected by
// PALAI_MODEL_PROVIDER. "provider-one" is the live OpenAI adapter: the model id comes from
// PALAI_MODEL (default gpt-4o-mini) and the credential is redeemed only at call time from
// PALAI_SECRET_PROVIDER_ONE (the compose file-secret bridge) — never on a request, argument,
// or log. Any other value (including unset) selects the deterministic fake adapter: no
// network, no credential, a fixed scripted completion for the shipped-binary wiring proof.
//
// Since E13 T8 this env selection is the FALLBACK, not the whole story: a project with a published
// model route dispatches through that route's model and its own connection credential, and only a
// project without one runs on what this function returns. The broker's resolver reflects that split —
// a tenant-qualified ref (minted by a DB route) redeems from the T3 secret store under that tenant's own
// organization, and an unqualified ref redeems from the env bridge below.
func modelBrokerFromEnv() (*modelbroker.Broker, execution.ModelRoute) {
	if os.Getenv("PALAI_MODEL_PROVIDER") == "provider-one" {
		model := os.Getenv("PALAI_MODEL")
		if model == "" {
			model = "gpt-4o-mini"
		}
		// The second family (E16 T5) rides the same registry so a DB-published route can
		// dispatch to provider-two (Anthropic) or the OpenAI-compatible adapter with its own
		// connection credential; the env deployment DEFAULT below stays provider-one. The
		// OpenAI-compatible base URL comes from env (empty → OpenAI's endpoint).
		broker := modelbroker.New(modelbroker.Config{
			Adapters: map[string]modelbroker.ModelAdapter{
				"provider-one":      providerone.Adapter{},
				"provider-two":      providertwo.Adapter{},
				"openai-compatible": openaicompatible.Adapter{Adapter: providerone.Adapter{BaseURL: os.Getenv("PALAI_OPENAI_COMPATIBLE_BASE_URL")}},
			},
			Secrets: execution.RouteSecretResolver{
				Lookup: dbSecret,
				Fallback: modelbroker.EnvResolver{
					modelbroker.SecretRef("provider-one"):      "PALAI_SECRET_PROVIDER_ONE",
					modelbroker.SecretRef("provider-two"):      "PALAI_SECRET_PROVIDER_TWO",
					modelbroker.SecretRef("openai-compatible"): "PALAI_SECRET_OPENAI_COMPATIBLE",
				},
			},
		})
		return broker, execution.ModelRoute{Provider: "provider-one", Model: model, Secret: modelbroker.SecretRef("provider-one")}
	}
	broker := modelbroker.New(modelbroker.Config{
		Adapters: map[string]modelbroker.ModelAdapter{"fake": fake.Adapter{Script: fake.Script{
			ProviderRequestID: "fake-local", Model: "fake", Output: "ok",
		}}},
		Secrets: execution.RouteSecretResolver{
			Lookup:   dbSecret,
			Fallback: modelbroker.StaticResolver{modelbroker.SecretRef("fake"): "unused"},
		},
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
		// Not every consumer HAS an env bridge (the repository connection resolver is DB-only), so the log
		// states what happened here rather than claiming a fallback the caller may not have.
		log.Printf("secret store: resolve ref %q under org %q: %v (treated as a miss)", ref, org, err)
		return nil, false, nil
	}
	return v, ok, nil
}

// repositoryConnectionSecret bridges a repository binding's connection_ref to the Git credential bytes at
// clone time (E13 Task 9). Unlike its four siblings it is DB-ONLY: connection_ref is a NEW consumer, so
// there is no pre-T3 env-file bridge to stay compatible with — a binding-scoped Git credential is
// provisioned over POST /v1/secret-refs and rotated there with no restart. A MISS is an error: a binding
// that deliberately names its own credential must never silently clone under the deployment-global App.
// The org is server-minted from the run, so the store scopes the read to it and RLS denies any foreign row.
//
// HONEST CEILING: there is no per-tenant GitHub App ONBOARDING surface (installing an App per tenant and
// capturing its installation credential is product/SaaS work). This resolves whatever token the tenant
// already provisioned under the ref — a PAT or an installation token it manages itself.
func repositoryConnectionSecret(org, ref string) ([]byte, error) {
	if org == "" || ref == "" {
		return nil, errors.New("empty repository connection org/ref")
	}
	v, ok, err := dbSecret(org, ref)
	if err != nil {
		return nil, err
	}
	if !ok {
		// dbSecret flattens a transient store failure into a miss (it is logged there), so this covers two
		// causes and must not assert the wrong one: an operator reading "unprovisioned" during a Postgres
		// blip would go hunting for a ref that is in fact present.
		return nil, fmt.Errorf("repository connection %q did not resolve under org %q: no such secret ref, or the secret store was unreachable (see the secret-store log)", ref, org)
	}
	return v, nil
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

// startQueueBridges launches the two supervised queue loops (E19 T6, spec §34.2/§34.5):
//
//   - the INBOUND bridge consumes each enabled inbound binding and admits every message through the
//     coordinator spine, acking only after the admission and the idempotency receipt have committed;
//   - the OUTBOUND pump drains queue_deliveries — rows that were already committed inside each run's
//     TERMINAL TRANSACTION, which is why a publisher-down window (or this pump simply not running) can
//     never lose a result. The pump delivers; the terminal transaction is what makes it durable.
//
// Both are system loops that serve every project and are inert until a binding is registered, so they run
// unconditionally like the webhook pump. The bridge passes the SAME per-project §20.12 caps the HTTP edge
// resolves, so a queue flood is shed by the same admission gate a POST flood is — not a second policy.
func startQueueBridges(ctx context.Context, store *automation.QueueStore, spine *coordinator.Store, edge api.EdgeLimits, supervisor *coordinator.Supervisor) {
	cfg := automation.QueueBridgeConfig{
		MaxConcurrentRuns: edge.MaxConcurrentRuns,
		MaxQueuedRuns:     edge.MaxQueuedRuns,
		Tick:              envDurationOr("PALAI_QUEUE_TICK", time.Second),
		DeliveryBackoff:   envDurationOr("PALAI_QUEUE_DELIVERY_BACKOFF", 30*time.Second),
	}
	go supervisor.Supervise(ctx, "queue-bridge", automation.NewQueueBridge(store, spine, cfg, log.Printf).Run)
	// The outbound sink is an egress-safe POST to the destination each connection names, over the SAME
	// vetted sender the webhook pump uses. A real broker client (an SQS/PubSub/Kafka SDK Publish) is the
	// operator leg (§6 leg 5) and plugs in behind the same queue.Sink interface.
	go supervisor.Supervise(ctx, "queue-outbox-pump",
		automation.NewQueueOutboxPump(store, automation.QueueHTTPSinks(webhook.NewSender()), cfg, log.Printf).Run)
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

// slackSecretResolver is the fourth sibling of webhook/inbound/remoteToolSecretResolver (E19 T1): it bridges
// a slack_connections.signing_secret_ref handle to the v0 signing-secret bytes via
// PALAI_SLACK_SECRET_FILE_<ORG>__<REF> (a FILE PATH, never inline). The org prefix is a server-minted hard
// tenant boundary, so a tenant's ref can only name a secret provisioned under its OWN org — and the Slack
// namespace is DISTINCT from the webhook/inbound/remote-tool ones, so the four secret sets are
// non-interchangeable. An unresolved ref fails VERIFICATION (a generic 401 upstream), never an unsigned
// accept: a receiver that cannot check a signature refuses. The same bridge will resolve bot_token_ref /
// app_token_ref when T2/T3 wire the outbound and Socket Mode legs.
func slackSecretResolver(org, ref string) ([]byte, error) {
	if org == "" || ref == "" {
		return nil, errors.New("empty slack secret org/ref")
	}
	if v, ok, err := dbSecret(org, ref); err != nil {
		return nil, err
	} else if ok {
		return v, nil
	}
	// Belt-and-braces, as in the sibling resolvers: a normalized org key carrying the "__" delimiter is
	// ambiguous; reject it rather than resolve a colliding key.
	if strings.Contains(secretEnvKey(org), "__") {
		return nil, fmt.Errorf("ambiguous slack secret org key %q", org)
	}
	path := os.Getenv("PALAI_SLACK_SECRET_FILE_" + secretEnvKey(org) + "__" + secretEnvKey(ref))
	if path == "" {
		return nil, fmt.Errorf("no secret bridge configured for slack ref under org %q", org)
	}
	return os.ReadFile(path)
}

// a2aRemoteSecretResolver is the fifth sibling of webhook/inbound/remoteTool/slackSecretResolver (E19 T5):
// it bridges an a2a_remote_agents.auth_connection_ref handle to the REMOTE CONNECTION'S OWN bearer via
// PALAI_A2A_REMOTE_SECRET_FILE_<ORG>__<REF> (a FILE PATH, never inline). The org prefix is a server-minted
// hard tenant boundary, so a tenant's ref can only name a secret provisioned under its OWN org — and the
// A2A-remote namespace is DISTINCT from the webhook/inbound/remote-tool/Slack ones, so the five secret sets
// are non-interchangeable. This is the ONLY bearer a remote child dial can carry: an unresolved ref FAILS
// the dispatch (an honest child failure), it never falls back to the platform's or the parent's credential.
func a2aRemoteSecretResolver(org, ref string) ([]byte, error) {
	if org == "" || ref == "" {
		return nil, errors.New("empty a2a remote secret org/ref")
	}
	if v, ok, err := dbSecret(org, ref); err != nil {
		return nil, err
	} else if ok {
		return v, nil
	}
	// Belt-and-braces, as in the sibling resolvers: a normalized org key carrying the "__" delimiter is
	// ambiguous; reject it rather than resolve a colliding key.
	if strings.Contains(secretEnvKey(org), "__") {
		return nil, fmt.Errorf("ambiguous a2a remote secret org key %q", org)
	}
	path := os.Getenv("PALAI_A2A_REMOTE_SECRET_FILE_" + secretEnvKey(org) + "__" + secretEnvKey(ref))
	if path == "" {
		return nil, fmt.Errorf("no secret bridge configured for a2a remote ref under org %q", org)
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

// withSupervisorStatus serves the two JSON health snapshots `palai doctor` surfaces, delegating
// every other request to next. Both ride alongside /healthz (unauthenticated liveness) and
// carry no sensitive data:
//
//   - /healthz/supervisor — the per-loop restart counts, so an operator can see a background
//     loop that is silently restarting.
//   - /healthz/runner — the number of live runner sessions and the EXPIRY of the client
//     certificate a runner last presented. That expiry is not observable anywhere else outside
//     the runner process, and it is the difference between "runs are failing" and "the runner's
//     identity lapsed": the runner refreshes it on every renewal, so a NotAfter in the past means
//     it stopped rolling the identity forward. A certificate's validity window and a runner DNS
//     name are public halves of a credential, not secrets — the private key never leaves the
//     runner and nothing here helps a caller obtain one.
func withSupervisorStatus(next http.Handler, supervisor *coordinator.Supervisor, gateway *execution.RunnerGateway) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && r.URL.Path == "/healthz/supervisor" {
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{"restarts": supervisor.Restarts()})
			return
		}
		if r.Method == http.MethodGet && r.URL.Path == "/healthz/runner" {
			body := map[string]any{"gateway": gateway != nil}
			if gateway != nil {
				body["sessions"] = gateway.Connected()
				if identity, seen := gateway.LastRunnerIdentity(); seen {
					body["identity"] = identity
				}
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(body)
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
	// The bootstrap token is the runner's RECOVERY path when its identity has already expired and
	// renewal-over-mTLS is therefore impossible (see FileEnrollmentTokens for the threat model). It
	// is rate-limited to one certificate per issued lifetime, so a leaked token mints no fleet.
	tokens := execution.NewFileEnrollmentTokens(mustGatewayEnv("PALAI_ENROLLMENT_TOKEN_FILE"), issuer.TTL())
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

// startCapabilityWorkerGateway serves the capability-worker enroll/claim/redeem/result surface (E17 T9,
// spec §31) on a SEPARATE listener from the public API, exactly as startRunnerGateway does, and returns nil
// when PALAI_CAPABILITY_WORKER_LISTEN_ADDR is unset — a deployment that configures no worker surface serves
// none, and discovery then advertises no `capability-workers`.
//
// WHY A SEPARATE LISTENER RATHER THAN THE /v1 EDGE. This is the same CLASS of surface as the runner gateway,
// and it is deliberately NOT an API surface:
//   - Its principal is a WORKER, not an API key. A worker presents a one-time enrollment token and then a
//     short-lived workload identity the /v1 bearer middleware knows nothing about; mounting /capability/* on
//     the authenticated router would put it behind a verifier that must reject every worker token, and
//     mounting it on the top mux would publish it wherever the public listener is published.
//   - The production edge path-matches `reverse_proxy /v1/*` (deploy/compose/production.yml), so the /v1 edge
//     is precisely the surface reachable from outside. Enrollment is an OPERATOR ceremony over a one-time
//     token, not a tenant-facing route; a separate listener is what lets an operator bind it to the network
//     the workers actually live on and nowhere else.
//   - E17 T9 built and proved this surface as outbound-enrolled with NO inbound port to the worker. Putting
//     it on the public edge would change the posture the WRK-001..007 proofs were taken under, which is
//     exactly what §2 forbids: wiring must go through the surface as built.
//
// HONEST CEILINGS — four of them, none introduced here, all worth the operator knowing:
//
//   - CLEARTEXT LISTENER, so this wiring REFUSES to bind it off-host (listenCapabilityWorker). Its sibling at
//     the same topology gets mTLS, and three things travel on this one in the clear: the one-time enrollment
//     token, the workload bearer on EVERY request — with no channel binding, unlike the runner's client cert
//     whose private key never leaves the runner, so one observed request is full worker impersonation for the
//     identity TTL — and the REDEEMED SECRET VALUE in the redeem response body. Loopback + TLS terminated in
//     front is the supported posture; full parity with startRunnerGateway (tls.Listen, TLS 1.3, ClientCAs,
//     reusing PALAI_RUNNER_SERVER_CERT/KEY) is the right answer once a fleet actually enrolls across a
//     network. Note the client is NOT the obstacle: cmd/palai-capability-worker dials c.base+path with a
//     stock http.Client, which handles an https:// base against a normally-trusted certificate fine — only a
//     SELF-SIGNED listener would need a CA flag the client does not have.
//
//   - DORMANT IN THREE WAYS. The surface is SERVED and enforces everything WRK-001..007 proved, but nothing
//     drives it yet, so `capability-workers` means "this binary can serve the surface", never "a worker ran a
//     job": (1) no production path mints an enrollment token — Gateway.IssueEnrollmentToken has no operator
//     caller (the runner's equivalent is PALAI_ENROLLMENT_TOKEN_FILE), so nobody can enroll; (2) no
//     production path dispatches a job — Store.DispatchJob has no caller, so even an enrolled worker polls
//     204 forever; (3) no production path advances worker health or reclaims an expired lease —
//     Store.SetWorkerHealth and Store.RedispatchForRetry have no callers, so the worker-health fence never
//     moves and there is no reaper. All three are missing OPERATOR/DRIVER paths, not missing capabilities,
//     and they are why this task mounts what exists instead of inventing routes on the way past. E19 T8b/T9
//     own them (docs/superpowers/plans/phase-19-integration-wiring.md, T8).
//
//   - The gateway's in-memory maps are now a LONG-LIVED daemon's, not a fixture's (E17 T9 marked them
//     "fixture scale" when the only caller was a test): Gateway.artifacts is never deleted (every output,
//     each ≤8MB, is retained for the process lifetime) and Gateway.sessions expiry is CHECKED on every
//     request but never swept. The durable object store (E09) is the reuse path.
//
//   - NO DRAIN. serveWithGracefulDrain drains only the runner gateway and workers.Gateway has no Drain, so a
//     control-plane swap 401s every worker (sessions and claims are in-process) and leaves leased jobs
//     `leased` in the journal for the reaper that (see above) does not exist yet. Recovery is T9's.
func startCapabilityWorkerGateway(addr string, repo *store.Store, secrets *identity.SecretStore) *workers.Gateway {
	if strings.TrimSpace(addr) == "" {
		return nil
	}
	// A typed-nil *identity.SecretStore must NOT wrap a non-nil interface (the nil-interface trap
	// WithSecretRefs guards): the store checks `secrets == nil` to refuse a redeem outright, so an absent
	// master key has to leave the seam a true nil rather than a live-looking resolver that panics.
	var resolver workers.SecretResolver
	if secrets != nil {
		resolver = secrets
	}
	gateway := workers.NewGateway(
		workers.NewStore(repo.Spine().Pool(), resolver, middleware.NewID, nil),
		envDuration("PALAI_CAPABILITY_WORKER_IDENTITY_TTL"), // 0 ⇒ the gateway's 10m default
	)
	srv := &http.Server{
		Addr:              addr,
		Handler:           gateway.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
	}
	// Bind synchronously so a bind failure fails fast, for the same reason the runner listener does: the
	// public /healthz must not report healthy while a configured worker surface silently never came up —
	// that would recreate the very lie this wiring closed.
	ln, err := listenCapabilityWorker(addr)
	if err != nil {
		log.Fatalf("bind capability worker gateway listener: %v", err)
	}
	log.Printf("palai capability worker gateway listening on %s", addr)
	go func() { log.Fatal(srv.Serve(ln)) }()
	return gateway
}

// listenCapabilityWorker binds the capability-worker gateway's listener, REFUSING any address that is not
// an explicit loopback host. That listener is cleartext (see startCapabilityWorkerGateway's first ceiling
// for what travels on it), and configvalidate.go's edge check inspects only host-PUBLISHED ports — so an
// operator copying compose.yaml's `PALAI_RUNNER_LISTEN_ADDR: ":8443"` shape into
// PALAI_CAPABILITY_WORKER_LISTEN_ADDR would otherwise get a wildcard-bound secret-redemption endpoint with
// no warning at all. Refusing the bind is what makes accidental remote exposure impossible rather than
// merely documented; TLS parity with startRunnerGateway is the upgrade path for a real off-host fleet.
func listenCapabilityWorker(addr string) (net.Listener, error) {
	host, _, err := net.SplitHostPort(strings.TrimSpace(addr))
	if err != nil {
		return nil, fmt.Errorf("PALAI_CAPABILITY_WORKER_LISTEN_ADDR %q is not a host:port address: %w", addr, err)
	}
	// An empty host is the WILDCARD bind (":8444") — the accident this guards, so it must not read as
	// loopback. "localhost" is the documented loopback name; a poisoned resolver is not the accident here.
	host = strings.Trim(strings.TrimSpace(host), "[]")
	loopback := host == "localhost"
	if ip := net.ParseIP(host); ip != nil {
		loopback = ip.IsLoopback()
	}
	if !loopback {
		return nil, fmt.Errorf("refusing to bind the CLEARTEXT capability-worker gateway to non-loopback address %q: "+
			"the enrollment token, the workload bearer on every request, and the redeemed secret VALUE would all "+
			"travel unencrypted. Bind loopback and terminate TLS in front of it, or give this listener the runner "+
			"gateway's mTLS (tls.Listen with PALAI_RUNNER_SERVER_CERT/KEY + ClientCAs) before it crosses a network", addr)
	}
	return net.Listen("tcp", addr)
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
// a2aPusherFromEnv builds the A2A push pusher, or nil to leave push UNMOUNTED (E19 T4, §3.5 D12/D13).
//
// Push POSTs to a URL the CLIENT registered, so it is off unless a deployment explicitly says where
// deliveries may go. PALAI_A2A_PUSH_ALLOWED_HOSTS is that single fail-closed switch:
//
//	unset/empty  -> no pusher. The card advertises pushNotifications:false and the CRUD 404s (D13).
//	"a.example,b.example" -> push on, restricted to those hosts (normalized whole-host equality).
//	"*"          -> push on with NO host allowlist: any public https destination a client names will be
//	                POSTed to, with only packages/egress standing between us and it. This is the weaker
//	                posture the A2A security guidance warns about ("SHOULD NOT blindly trust ... any URL
//	                provided by a client"), so it must be typed out on purpose — it is never the default.
//
// PALAI_A2A_PUSH_ALLOW_PRIVATE=1 additionally opens loopback/RFC1918 receivers for a self-host deployment;
// the metadata and special-use ranges stay denied even then (egress.VetIP).
//
// The pusher rides a webhook.Sender — the SAME egress-vetted, IP-pinned signed sender the §21.6 delivery
// pump uses.
func a2aPusherFromEnv() *a2a.WebhookPusher {
	raw := strings.TrimSpace(os.Getenv("PALAI_A2A_PUSH_ALLOWED_HOSTS"))
	if raw == "" {
		return nil
	}
	policy := a2a.PushPolicy{AllowPrivate: os.Getenv("PALAI_A2A_PUSH_ALLOW_PRIVATE") == "1"}
	if raw != "*" {
		for _, host := range strings.Split(raw, ",") {
			if h := strings.TrimSpace(host); h != "" {
				policy.AllowedHosts = append(policy.AllowedHosts, h)
			}
		}
		// A list that parsed to nothing (e.g. ",,") must not silently become the "*" posture.
		if len(policy.AllowedHosts) == 0 {
			return nil
		}
	}
	pusher := a2a.NewWebhookPusher(webhook.NewSender(), policy)
	pusher.DeadLetter = func(_ context.Context, f a2a.PushFailure) {
		// A dead-lettered push is an operator signal, never a run failure: the canonical task result is
		// already durable and untouched. PushFailure carries no destination URL and no token.
		log.Printf("a2a: %s", f)
	}
	return pusher
}

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
