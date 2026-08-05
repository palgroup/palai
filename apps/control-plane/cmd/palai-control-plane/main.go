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
	"github.com/palgroup/palai/adapters/models/registry"
	"github.com/palgroup/palai/adapters/repositories"
	"github.com/palgroup/palai/adapters/sandboxes/oci"
	"github.com/palgroup/palai/adapters/sandboxes/posture"
	remotehttp "github.com/palgroup/palai/adapters/tools/http"
	"github.com/palgroup/palai/apps/control-plane/api"
	"github.com/palgroup/palai/apps/control-plane/api/middleware"
	"github.com/palgroup/palai/apps/control-plane/internal/artifacts"
	"github.com/palgroup/palai/apps/control-plane/internal/automation"
	"github.com/palgroup/palai/apps/control-plane/internal/botcreds"
	"github.com/palgroup/palai/apps/control-plane/internal/bots"
	"github.com/palgroup/palai/apps/control-plane/internal/execution"
	tools "github.com/palgroup/palai/apps/control-plane/internal/execution/tools"
	"github.com/palgroup/palai/apps/control-plane/internal/extensions"
	"github.com/palgroup/palai/apps/control-plane/internal/fleet"
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

	// The shell posture is resolved BEFORE anything is opened, because a deployment that cannot say
	// where its shell commands run must not start at all (E22 plan §2). The declaration is printed
	// once, here, so it is the first thing in the log of a stack that has no sandbox — including a
	// stack with no workspace root, where the runner is never even bound.
	nativeShell, err := posture.Resolve(os.Getenv("PALAI_SANDBOX_IMAGE"), os.Getenv("PALAI_SHELL_NATIVE"))
	if err != nil {
		log.Fatalf("shell posture: %v", err)
	}
	if nativeShell {
		log.Print(shellPostureDeclaration)
	}

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

	// The tenancy provisioning store backs the /v1/projects and /v1/api-keys surface (E13 Task 2; the three
	// /v1/organizations routes were unmounted by A.2 Task 6). It rides the durable spine's pool; POST
	// /v1/projects is what opens a new tenant now, with no restart, and the config_policy PATCH makes the
	// §14 resolver's project layer API-reachable.
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

	// The runner registry (E24 T1, migration 000045): the durable inventory of enrolled machines. It
	// rides the durable spine's pool and is handed to the gateway, which records an enrollment and
	// advances a liveness stamp on connect/renew. It is ALSO the read surface below (WithRunners).
	runnerRegistry := fleet.NewStore(repo.Spine().Pool(), middleware.NewID, nil)
	// The pool enrolment keys (E24 T3): the credential a machine presents to enrol into ONE pool. It
	// rides the same pool and is handed to the gateway as the FIRST link of the credential chain — the
	// file bootstrap token stays as the second, because it is the only thing that can rescue a machine
	// whose certificate has already expired.
	runnerPoolKeys := fleet.NewPoolEnrollmentKeys(repo.Spine().Pool(), middleware.NewID, nil)

	gateway := startRunnerGateway(os.Getenv("PALAI_RUNNER_LISTEN_ADDR"), runnerRegistry, runnerPoolKeys, repo.Spine(), repo)

	// The kind-agnostic bot registry (2026-08-03 plan Task 4): a project's registered bots, one row per
	// relay process the console can create. It rides the same durable spine's pool as every other admin
	// registry above and needs no external credential, so it is wired unconditionally (WithBots, appended
	// to routerOpts below) — the WithKnowledge posture.
	botStore := bots.NewStore(repo.Spine().Pool(), middleware.NewID, nil)

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
		api.WithBots(botStore),
		// WithToolCalls mounts the tool-call read (E30 T2), over the SAME non-nil repo the positional
		// seams use, so it is unconditional like the two above. It is the only route from which a client
		// can learn what a tool call actually was: the journal frames carry the tool's NAME and
		// deliberately not its arguments or result, so a chat that renders an `xcodebuild` failure with
		// its file and line reads them here.
		api.WithToolCalls(repo),
	}

	// PER-SESSION ACCOUNTS (macOS), RELEASE HALF. It is HERE rather than in applyCloseSessionTx because
	// that runs inside a database transaction: a `sudo` there holds a session row lock for the length of an
	// exec, and a rollback after it cannot un-delete an account. So the release happens after the close has
	// committed, on the command surface.
	//
	// ONE ENV VALUE DRIVES BOTH HALVES — the acquire is wired in startDispatch — because a deployment that
	// wired one without the other leaks an account per session.
	//
	// ‼️ ONE INSTANCE, NOT TWO. The acquire and the release share a slot map — which session holds which
	// index — so constructing one here and another in startDispatch would give the releaser an empty map,
	// and every release would find nothing to destroy while reporting success. That is a leak that looks
	// exactly like working code, which is why the value is built once and threaded.
	var sessionAccounts *execution.SlotAccounts
	if wrapper := os.Getenv("PALAI_SESSION_ACCOUNT_HELPER"); wrapper != "" {
		sessionAccounts = execution.NewSudoSessionAccounts(wrapper)
		routerOpts = append(routerOpts, api.WithSessionAccounts(sessionAccounts))
	}
	if secretStore != nil {
		routerOpts = append(routerOpts, api.WithSecretRefs(secretStore))
		// THE VERIFY ACTION'S REAL WORK (E29), on the SAME condition and the SAME store as the secret-ref
		// surface, for the reason the environment surface rides it: a credential probe has to REDEEM the
		// credential, and without a master key there is nothing to redeem. It resolves through `dbSecret` —
		// byte-for-byte the resolver the model broker itself uses — so the probe checks what a run would
		// actually send rather than a second read path that could disagree with it. A stack with no master
		// key mounts the route and answers "nothing was checked", which is the honest answer and never a
		// green.
		repo.WithModelConnectionInspectors(connectionInspectors(), dbSecret)
		// The environment surface (E25 T3) rides the SAME condition and the SAME store, and the coupling is
		// the design: an environment value IS a secret_refs version, so an environment surface without a
		// master key could list names it can never fill. One `if`, two families, no way to mount half of it.
		routerOpts = append(routerOpts, api.WithEnvironments(secretStore))
		// Bot credential redemption (2026-08-03 plan Task 14.5) rides the same condition for the same
		// reason: it opens sealed values, so with no master key there is nothing to open. It is the ONE
		// route by which a relay process running outside this binary can turn the handles its own registry
		// row names into tokens — without it a bot is configurable and unstartable.
		routerOpts = append(routerOpts, api.WithBotCredentials(botcreds.New(botStore, secretStore)))
	}
	// The DESIRED configuration (E29, migration 000052): what this MACHINE should be running with, written
	// by the admin panel and applied by the next bring-up.
	//
	// UNCONDITIONAL on `repo`, unlike the two above, and the difference is real. Those need a master key
	// because they seal values; this stores no credential at all — its allow-list refuses every path-kind
	// setting structurally and every destination a credential is sent to by name — so the only thing it
	// needs is a durable spine, which this composition root always has by the time it gets here. Gating it
	// on the master key would leave the write path unmounted on exactly the deployments that have the most
	// configuration and the least tooling to change it.
	routerOpts = append(routerOpts, api.WithDesiredConfig(repo))
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
	// THE SLACK SEARCH AUTHORITIES (E21 T5), and after the 2026-08-05 cutover they are all that is left of
	// Slack inside this binary. Until then this block built an in-process Slack bridge: an Events API receiver,
	// an interactivity receiver, a workspace-registration surface, a Socket Mode connect loop, a reply pump, an
	// approval pump, a run follower and two file legs. Slack is now a SEPARATE PROCESS (apps/slack-bot) that
	// consumes this control plane over `/v1` exactly like any other client, so the bridge and everything that
	// fed it were deleted rather than left dormant.
	//
	// The authorities survive because they are NOT a transport: they are the per-turn grant that decides whether
	// the model is offered the Slack search tool at all. Nothing is persisted — an action_token's lifetime is
	// undocumented, so a grant lives exactly as long as the turn does.
	//
	// TWO ENDS, BORN AT OPPOSITE ENDS OF THIS FUNCTION: the route below writes a grant in, and the tool broker's
	// lookup (startDispatch) reads it out. The relay cannot key a grant by run id — it learns a run id only from
	// the response this control plane returns, by which time the run has already asked the broker what tools it
	// may have — so the grant is keyed on the SESSION it creates the response in.
	//
	// The route mounts only where WithBotCredentials did (router.go): it resolves the bot's own token rather
	// than accepting one on the wire, so with no master key there is nothing to authorise with.
	slackSearchAuthorities := tools.NewSearchAuthorities()
	routerOpts = append(routerOpts,
		api.WithSlackSearchGrants(slackSearchAuthorities, os.Getenv("PALAI_SLACK_API_BASE_URL")))
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
	// The Slack-less approval surface (E23 T9, spec §22.4). UNCONDITIONAL, and that is the whole point of
	// the task: E23 T8 gave the generic approval gate a decision surface and gave it only to Slack, so a
	// deployment that registers no workspace — the self-host story — parked runs on questions nobody could
	// answer and the expiry reaper released them half an hour later. Mounting it behind a flag would leave
	// the same hole for anyone who did not set the flag.
	//
	// It advertises NOTHING in discovery: /v1/capabilities' word list is the E17 T11 recompute's, and a new
	// key here would be a claim no proof had recomputed.
	routerOpts = append(routerOpts, api.WithApprovals(repo))
	// The runner registry READ surface (E24 T1). Mounted unconditionally beside the approval surface and
	// for the same reason: it needs only the durable spine, which this binary always has. It advertises
	// NOTHING in discovery — /v1/capabilities' word list is the E17 T11 recompute's, and exposing a table
	// is not a capability.
	// The read surface plus T3's key WRITE half, joined at the composition root because the router takes
	// one interface and the two stores are deliberately separate types (inventory / credential).
	//
	// AND T5'S MACHINE LIFECYCLE, whose live half is the GATEWAY: cordon/resume/revoke writes the row and
	// then reaches the sessions that machine is holding, because a row alone takes effect at its next
	// connect — hours away for a Mac mid-run. This one line is why `Revoke()` is reachable at all: §3.6 D15
	// found it implemented, tested, catalogued as SAN-011's hard stop and called by NOTHING, which is the
	// third time this repository has shipped that shape (E19 T9's CreateSlackConnection, E23's
	// DecideToolApproval). It is fenced by name in a test for exactly that reason. A stack with no runner
	// listener passes a nil gateway and the routes then write the row only, which is correct: there are no
	// sessions to cut.
	runnerAdmin := fleet.NewRegistryAPI(runnerRegistry, runnerPoolKeys)
	if gateway != nil {
		runnerAdmin = runnerAdmin.WithLifecycle(gateway)
	}
	routerOpts = append(routerOpts, api.WithRunners(runnerAdmin))
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

	startDispatch(ctx, repo, gateway, supervisor, artStore, slackSearchAuthorities, sessionAccounts)
	startWebhookPump(ctx, webhookStore, supervisor)
	startQueueBridges(ctx, queueStore, repo.Spine(), edge, supervisor)
	startDeliveryReconciler(ctx, triggerStore, supervisor)
	startScheduleTicker(ctx, scheduleStore, supervisor)
	startRetention(ctx, repo, supervisor, artStore)
	startOrphanGC(ctx, repo, supervisor, artStore)
	startIdleRelease(ctx, repo, supervisor, artStore, sessionAccounts)
	startLeaseReclaim(ctx, repo, supervisor)

	log.Printf("palai control-plane listening on %s", addr)
	serveWithGracefulDrain(srv, gateway)
}

// serveWithGracefulDrain serves until SIGTERM/Interrupt, then DRAINS the runner gateway before exit:
// it stops offering NEW leases and waits (bounded by PALAI_DRAIN_TIMEOUT, default 20s) for the in-flight
// lease to finish, so a control-plane swap — compose sends SIGTERM to the OLD container during
// `up -d control-plane` — is the §48.4 "runner drain" step rather than a hard kill of an active run.
// Whatever does not finish inside the window is reclaimed and completed by the E10 recovery layer on the
// new control-plane, so a run always survives the swap on its pinned engine. A stack with no gateway
// (assignment-only tiers) or no in-flight lease drains instantly, so ordinary shutdowns are unchanged.
//
// IT USED TO DRAIN A SECOND THING FIRST. Until the 2026-08-05 cutover this binary held Slack's Socket Mode
// connection itself, and that socket drained BEFORE the gateway because it was upstream of it — draining
// runner leases while a socket kept admitting new Slack work would have been draining against a source still
// filling the queue. Slack is now dialled by apps/slack-bot, a separate process with its own lifecycle, so
// this binary has exactly one thing to drain again.
func serveWithGracefulDrain(srv *http.Server, gateway *execution.RunnerGateway) {
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

// dispatchWorkerCount is production's reading of PALAI_DISPATCH_WORKERS, and startDispatch's early
// return is `dispatchWorkerCount() <= 0`. It is a named function for the reason backgroundRunnerFor is
// one: a test that builds its own configuration never sees the configuration production builds, so the
// only honest way to pin what a deployment gets is to ask the function production asks.
//
// WHAT THAT ZERO GATES IS EVERYTHING BELOW IT, and the list is longer than "dispatch": the reconciler,
// the fleet heartbeat and the uncertain-tool reconciler are all constructed after this return, so a
// stack at zero runs no dead-letter sweep, no approval expiry, no capacity-park expiry and — since
// E26 T4 — no background exit notification. deploy/compose/compose.yaml ships `:-0`; the production
// overlay ships `:-1`. See docs/operations/background-execution.md, first paragraph.
//
// IT DELEGATES rather than reading the variable itself, and that is the E29 machine-configuration change.
// GET /v1/deployment reports this posture to an operator, and a screen that read the environment with its
// own copy of the default would be a second answer to a question that must have one — the shape of every
// defect this tree's CLAUDE.md is about. api.DispatchWorkers is now the single reader; this function stays
// because the sentence above it is still true: production's reading is what a test must be able to ask.
func dispatchWorkerCount() int { return api.DispatchWorkers() }

// startDispatch launches the durable dispatch workers and the reconciler that drive
// admitted response.run jobs. It requires a runner listener: the worker runs the full production
// exec-path, driving each claimed run through the model broker, the conformance tool broker, and a
// live engine dialed over the gateway, to a committed terminal response. A killed worker's lease
// lapses and its job is reclaimed at a higher fence, so no graceful shutdown is needed.
// PALAI_DISPATCH_WORKERS sets the worker count (default 1); 0 disables dispatch.
//
// THE SENTENCE THAT USED TO BE HERE SAID "Without it, the binary keeps the assignment-only behavior
// the read-path SSE e2e drives", AND NO SUCH DRIVER EXISTS. scripts/test/e2e sets
// PALAI_DISPATCH_WORKERS=0 — its own comment says it runs "without a dispatcher so no background
// worker races those manual transitions" — so the early return fires and that branch is never built.
// What the branch did reach was an operator's stack with dispatch on and no listener, where it marked
// runs `running` and completed their jobs. See dispatchPosture.
// dispatchPosture decides whether dispatch may start, and when it may not, the sentence that says why.
//
// IT EXISTS BECAUSE THE ALTERNATIVE STRANDED RUNS SILENTLY. `startDispatch` built the assignment-only
// handler (`execution.AdvanceRun`) and replaced it with the real exec-path handler only inside
// `if gateway != nil`. AdvanceRun drives a run queued -> provisioning -> running and reports SUCCESS
// while opening no attempt and dialing nothing, so a stack with dispatch on and no runner listener
// answered every response.run job by marking its run `running` and completing the job. Measured on a
// live stack 2026-08-02: of 240 jobs, 227 carried result_hash `run:<id>:executed` and exactly one
// carried `run:<id>:assigned` — a run that had been `running` with no attempt row and no terminal.
//
// THE COMMENT THIS REPLACES NAMED A DRIVER THAT DOES NOT EXIST: it said the assignment-only behaviour
// is what the read-path SSE e2e drives. scripts/test/e2e sets PALAI_DISPATCH_WORKERS=0 and says so in
// its own comment, so dispatchWorkerCount()'s early return fires and AdvanceRun is never constructed.
// No shipped tier drives that branch, which is why refusing costs nothing.
//
// A refused stack leaves runs `queued`, which is the recoverable state — a properly-wired control
// plane picks them up on its next boot — rather than a `running` claim nothing can honour.
func dispatchPosture(workers int, gatewayBound bool) (start bool, refusal string) {
	switch {
	case workers <= 0:
		return false, "" // dispatch is off by choice; nothing to explain
	case !gatewayBound:
		// BOTH EXITS ARE NAMED, because the refusal has two legitimate resolutions and an operator told
		// only one of them will assume the other is unsupported. Set the listener if this control plane is
		// meant to execute runs; set PALAI_DISPATCH_WORKERS=0 if it is deliberately API-only — a real
		// posture the tree already ships (scripts/test/e2e drives the read path that way, and
		// deploy/compose/compose.env.example's default is 0). Naming only the listener would read as "you
		// must run a runner plane", which is false, and would send an operator to configure certificates
		// for a stack that has no runs to execute.
		return false, "dispatch is configured (PALAI_DISPATCH_WORKERS>0) but no runner listener is bound. " +
			"Refusing to dispatch: an assignment-only worker marks each run `running` and completes its job, " +
			"leaving a run with no attempt, no engine and no terminal. Two ways out — set " +
			"PALAI_RUNNER_LISTEN_ADDR if this control plane is meant to execute runs, or set " +
			"PALAI_DISPATCH_WORKERS=0 if it is deliberately API-only. Runs stay `queued` either way and are " +
			"picked up by the next control plane that can execute them."
	default:
		return true, ""
	}
}

func startDispatch(ctx context.Context, repo *store.Store, gateway *execution.RunnerGateway, supervisor *coordinator.Supervisor, artStore *artifacts.Store, slackSearchAuthorities *tools.SearchAuthorities, sessionAccounts *execution.SlotAccounts) {
	workers := dispatchWorkerCount()
	switch start, refusal := dispatchPosture(workers, gateway != nil); {
	case start:
	case refusal != "":
		log.Printf("control plane: %s", refusal)
		return
	default:
		return
	}
	spine := repo.Spine()
	retry := coordinator.RetryPolicy{MaxAttempts: 5, BaseBackoff: 100 * time.Millisecond, MaxBackoff: 30 * time.Second}

	// NO ASSIGNMENT-ONLY DEFAULT. This was `execution.AdvanceRun(spine)`, and it survived only when the
	// gateway branch below did not fire — which is precisely the stack that could not execute anything.
	// dispatchPosture has already refused that combination, so the handler is assigned exactly once,
	// inside the block, and a nil here would be a wiring mistake rather than a posture. It is checked
	// below rather than assumed: this file's job is to fail closed and name the wire.
	var handler coordinator.Handler
	// The observer the reconciler notices a finished background task through (E26 T4). It is declared
	// out here because the orchestrator that owns it is built inside the gateway branch below, and the
	// reconciler is built after it: a stack with no engine gateway starts no background task, so nil is
	// the right value and the sweep is then a no-op.
	var backgroundObserver coordinator.BackgroundObserver
	if gateway != nil {
		broker, route, err := modelBrokerFromEnv()
		if err != nil {
			log.Fatalf("model broker: %v", err) // a routed script that cannot be replayed stops the boot
		}
		// Register the real coding tools alongside the conformance math tool: the workspace file and
		// shell tools (spec §28.7-28.8) that E09's real tool round-trip dispatches. The file tool
		// confines to the attempt's workspace; the shell tool runs on the MACHINE holding the attempt's
		// lease (A.3, execution.shellFor) — there is no process-wide runner to inject here any more, and an
		// attempt whose connection reaches no machine gets no shell rather than this host's.
		toolBroker := toolbroker.New(
			toolbroker.ConformanceMathAdd(),
			tools.FileTool(),
			tools.TextEditorTool(), // Anthropic's editor: str_replace instead of whole-file rewriting
			tools.GlobTool(),       // find files by name before reading any of them
			tools.GrepTool(),       // and by content, without paying the shell's irreversible classification
			tools.ShellTool(),
			tools.BackgroundKillTool(), // E26 T2: stop a task the shell tool's `background` parameter started
			// The agent's own way to SHOW a human what it did — a simulator screenshot or a screen
			// recording. Registered beside the file tool because it is the same confinement and the same
			// workspace; what differs is where the bytes go, which is the artifact store rather than the
			// model's context.
			tools.MediaTool(),
			tools.CommitTool(),
			tools.PushTool(),
			tools.PullRequestTool(),
			tools.MergeTool(),         // E23 T6: the third publication operation, gated by the same human button
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
		// The Slack search tool resolves AHEAD of the registry, and through a lookup rather than the static
		// map for one reason: a run with no action_token must not be OFFERED it. A tool advertised but always
		// failing is worse than one that does not exist — the model spends a turn on it and then tells a human
		// the workspace holds no such information. The lookup can see the run; the static map cannot.
		toolBroker.SetLookup(tools.SlackSearchLookup(http.DefaultClient, slackSearchAuthorities,
			func(ctx context.Context, env toolbroker.ExecEnv, name string) (toolbroker.Tool, bool, error) {
				return toolRegistry.LookupTool(ctx, env.Scope.Project, env.Scope.RunID, name)
			}))
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
		// THE SURFACE BACKGROUND EXECUTION TRAVELS ON (A.3 T7). It is the same gateway the dialer is, and
		// it is wired separately because the two answer different questions: a dialer hands out whichever
		// machine a pool has free for an attempt, this addresses the machine a task ALREADY runs on — by
		// name, long after that attempt ended.
		orch.SetMachineCaller(gateway)
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
		// Wire the repository publisher the approval pump publishes through (spec §30.9-30.10).
		// UNCONDITIONALLY, and the `if publisher != nil` this replaces is the defect: the publisher was
		// built inside the GitHub App gate, so a deployment with no App wired none — and a nil publisher
		// makes pumpApprovedPublications a no-op, which is indistinguishable from success on every surface
		// a human looks at. A binding carrying its own connection_ref now publishes on a deployment with no
		// App at all, and a binding carrying none is REFUSED there with a sentence rather than ignored.
		orch.SetPublisher(repositoryPublisher())
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
		// The environment resolver (E25 T3). Wired UNCONDITIONALLY and outside the workspace branch, unlike
		// the connection resolver: an environment reaches the agent through the SHELL TOOL, and a shell tool
		// call can happen on any run whose revision names an environment. Left nil, such a run would fail its
		// tool call with "no secret resolver is wired" — the fail-closed direction, but a confusing message
		// for a deployment that has a master key. With no master key configured, dbSecretStore is nil and the
		// resolver reports an honest miss instead.
		orch.SetEnvironmentSecrets(environmentValueSecret)
		if root := os.Getenv("PALAI_WORKSPACE_ROOT"); root != "" {
			orch.SetWorkspaceProvisioner(root, repositoryBrokerFromEnv())
			// PER-SESSION ACCOUNTS (macOS), ACQUIRE HALF: the uid a session's tools run under, created when the
			// session first provisions a workspace. It is the SAME INSTANCE the release half holds — they share the
			// map of which session owns which slot, and two instances would give the releaser an empty one.
			if sessionAccounts != nil {
				orch.SetSessionAccounts(sessionAccounts)
			}
			// A binding that names a connection_ref clones under its own tenant's credential (E13 T9);
			// the resolver is inert for the ref-less bindings that take the global broker above.
			orch.SetConnectionSecrets(repositoryConnectionSecret)
			// The changeset writer is wired above on the object store (it doubles as the research
			// body-artifact seam); a workspace-bound run reuses that same writer for its changeset compile.
			// THE SYNCHRONOUS SHELL IS NO LONGER WIRED HERE, AND NOTHING REPLACED IT (A.3). A command
			// now runs on the machine that holds the attempt's lease, derived from that attempt's own
			// connection (execution.shellFor) — so there is nothing process-wide left to inject. This
			// posture still builds an executor for the DETACHED half below, and that asymmetry is real:
			// a background task outlives its attempt, so it stays on the control plane's own host until
			// the machine grows a task registry of its own.
			if shell := shellRunnerFromEnv(); shell != nil {
				// The DETACHED half of the same posture (E26 T1). Both shipped executors can start a command
				// that outlives the attempt, so the assertion holds for both today — but it is written as a
				// type assertion rather than assumed, because ShellRunner is the interface main.go builds
				// against and a posture that could only run synchronously must produce a nil background
				// runner here rather than a half-wired one. backgroundRunnerFor is where that decision lives,
				// so a composition-root test can ask it what production wires.
				if bg := backgroundRunnerFor(shell); bg != nil {
					orch.SetBackgroundRunner(bg)
					// WHOSE KERNEL THAT RUNNER REACHES (A.3 T6). A background task outlives the attempt
					// that started it, so its row is read later — possibly by a restarted process, and the
					// sweep is system-scoped, so possibly by a control plane on a different box. A probe
					// there cannot fail, it answers `exited` for a process it never started, so the row
					// has to say where it ran.
					//
					// The hostname is what identifies the kernel a pgid belongs to, and it behaves
					// correctly in both shipped shapes: on a Mac it survives a restart, so this plane
					// still settles its own tasks; in a container a recreate yields a new name AND a new
					// PID namespace, where the old handles genuinely are lost. A machine renamed under a
					// running task reads as lost too — the safe direction, since lost is never signalled.
					if host, herr := os.Hostname(); herr == nil {
						orch.SetBackgroundMachine(host)
					} else {
						log.Printf("background: this machine has no resolvable hostname (%v), so tasks it starts will not be probed again", herr)
					}
					// AND THE OTHER END OF THE SAME DECISION (E26 T4): the reconciler below notices a task
					// finishing through THIS orchestrator's observer. Taken from the orchestrator rather than
					// rebuilt here so the probe that decides a task is done is the same one the park gate and
					// the kill tool use — a second prober would be a second answer to "is it still running".
					backgroundObserver = orch.BackgroundObserver()
					// THE CANCELLATION KILLER TAKES NO LINE HERE AND THAT IS THE POINT (E26 T5):
					// SetBackgroundRunner above wired it, on this same spine, because a deployment that can
					// start a build it cannot stop is the orphan this epic exists to prevent — and E26 T2
					// found one instance of exactly that shape, granted by omission across two conditionals.
				}
			}
		}
		handler = execution.ExecuteRun(spine, repo, orch)
	}

	// FAIL CLOSED ON A HANDLER NOBODY ASSIGNED. dispatchPosture guarantees the branch above ran, so this
	// can only be reached by a future edit that moves the assignment — and a worker claiming jobs with a
	// nil handler would panic per job while the runs it claimed stayed `running`. Refusing leaves them
	// `queued` and says which wire is missing, exactly as the unbound-listener refusal does.
	if handler == nil {
		log.Print("control plane: refusing to dispatch — no response.run handler was wired (the runner gateway branch did not assign one)")
		return
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
	// PALAI_FLEET_PARK_TTL opts the deployment into expiring capacity parks (E24 T5). UNSET MEANS NEVER and
	// that is deliberate: a run parked because its pool holds no machine is waiting for a Mac, and AWS
	// documents a Mac host taking 6 to 20 minutes to start — so any default this binary chose would be a
	// guess about somebody else's fleet. Set it and a park that outlives it ends as a `timed_out` response
	// naming the reason, instead of waiting forever (T4's FLT-P7).
	// AND E26 T4's sweep, on the same loop. NOTE WHERE THIS LINE SITS: below `if workers <= 0 { return }`
	// at the top of this function, so a deployment that sets PALAI_DISPATCH_WORKERS=0 — which the shipped
	// deploy/compose/compose.yaml does — builds no reconciler and therefore delivers NO background exit
	// notification at all. That is a declaration, not a defect, and it is the first paragraph of
	// docs/operations/background-execution.md.
	// E26 T5's FOUR CEILINGS take no line here and that is deliberate, but they are NAMED here so an
	// operator reading the composition root finds them: PALAI_BACKGROUND_MAX_WALL_TIME (60m; `0` is
	// unbounded and must be written), PALAI_BACKGROUND_MAX_PER_RUN (5), PALAI_BACKGROUND_MAX_PER_HOST (20)
	// and PALAI_BACKGROUND_LOG_TTL (24h). Three of the four are read at SPAWN time inside the dispatcher
	// and the fourth on the sweep, each through the function production itself calls
	// (internal/execution/background.go) — capturing them here instead would have put production's own
	// defaults out of reach of every test that constructs its own Orchestrator, which is the exact trap
	// that let a shell wall time be unbounded on the host and refuse every call in the container while
	// every sandbox test was green. They are documented in docs/operations/background-execution.md.
	reconciler := execution.NewReconciler(spine, 30*time.Second, retry.MaxAttempts).
		WithCapacityParkTTL(envDuration("PALAI_FLEET_PARK_TTL")).
		WithBackgroundTasks(backgroundObserver)
	go supervisor.Supervise(ctx, "reconciler", reconciler.Run)
	// THE FLEET HEARTBEAT AND ITS REAPER (E24 T5), supervised beside the reconciler. It pings every live
	// runner session: an answer advances `runners.last_seen_at` — the stamp that froze the moment a machine
	// finished connecting before this — and no answer CUTS the session, which is the only thing that notices
	// a connection alive to the kernel and dead to the process (a suspended laptop, an unplugged Mac). Only
	// with a gateway: a stack that binds no runner listener has no sessions to ping.
	if gateway != nil {
		go supervisor.Supervise(ctx, "runner-heartbeat", gateway.HeartbeatLoop)
	}
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
// a tenant-qualified ref (minted by a DB route) redeems from the T3 secret store,
// and an unqualified ref redeems from the env bridge below.
//
// The fake adapter's EXCHANGE is a deployment setting too, and PALAI_FAKE_SCRIPT_FILE is it: unset — every
// deployment that exists — replays registry.FakeScript unchanged, and a file lets a stack with no
// credential drive a run that calls a tool and reads its result. It is the only reason this function can
// fail: see fakeScriptFromEnv.
func modelBrokerFromEnv() (*modelbroker.Broker, execution.ModelRoute, error) {
	// THE ADAPTER MAP IS BUILT UNCONDITIONALLY, AND THAT IS THE FIX E29 EXISTS FOR.
	//
	// It used to be built INSIDE the `PALAI_MODEL_PROVIDER == "provider-one"` branch below. The comment
	// above this function said the env selection was "the FALLBACK, not the whole story", and for the ROUTE
	// that was true — a published route did override the model and the credential. But the ADAPTER map was
	// not a fallback, it was a GATE: on any other value, including the unset one a fresh self-host has, the
	// broker knew exactly one adapter named `fake`. So on the only deployment the model-connections feature
	// exists for — an operator who has typed no key into .env.local and is wiring their provider through the
	// console — a connection created over POST /v1/model-connections resolved onto a route and then died at
	// Broker.Route with `unknown_provider`. A shipped, mounted, routed surface that nothing could dispatch
	// to.
	//
	// The env var now decides ONE thing: the deployment DEFAULT ROUTE, for a project that has published no
	// route of its own. WHICH PROVIDERS THIS BINARY CAN SPEAK TO IS NOT A DEPLOYMENT SETTING — it is a
	// property of the binary, and it is adapters/models/registry, where a test pins the family list and the
	// adapter map to each other in BOTH directions.
	//
	// PALAI_OPENAI_COMPATIBLE_BASE_URL survives as the deployment-wide fallback endpoint for the custom
	// family, so a deployment that set it keeps working unchanged; a connection carrying its own base URL
	// (migration 000049) wins over it, per request.
	live := os.Getenv("PALAI_MODEL_PROVIDER") == liveModelProvider
	// The deployment-routed exchange for the deterministic adapter, if this deployment routed one.
	// It is read BEFORE anything is built, because an unreadable one has to stop the boot.
	script, err := fakeScriptFromEnv(os.Getenv(fakeScriptFileEnv), live)
	if err != nil {
		return nil, execution.ModelRoute{}, err
	}
	adapters := registry.Adapters(registry.Options{
		OpenAICompatibleBaseURL: os.Getenv("PALAI_OPENAI_COMPATIBLE_BASE_URL"),
		FakeScript:              script,
	})
	// The credential resolver is the same in both branches: a tenant-qualified ref (minted by a DB route)
	// redeems from the T3 secret store, and an unqualified ref redeems from the env bridge. "Qualified" is
	// a ROUTING mark, not a tenant boundary — see execution.TenantSecretRef. The `fake` entry is
	// unconditional for the same reason the adapter map is — a
	// project may route to a family the deployment default is not.
	broker := modelbroker.New(modelbroker.Config{Adapters: adapters, Secrets: execution.RouteSecretResolver{
		Lookup: dbSecret,
		// THERE IS NO ENVIRONMENT FALLBACK, and its removal is the point rather than a simplification.
		//
		// Until 2026-08-04 this chain read PALAI_SECRET_PROVIDER_ONE/_TWO/_OPENAI_COMPATIBLE, so a
		// provider credential lived in the control plane's environment for the life of the process. That
		// is reachable by anything running as the same uid, which on the native posture includes the
		// agent's own shell — measured: `ps -E -p <pid>` served 62 variables with their values, and
		// `os.Unsetenv` does not remove them because macOS answers ps from the kernel's copy of the
		// initial environment. A value that ever enters the environment cannot be taken back out.
		//
		// The supported path was already built and the fallback was the way around it: the console seals
		// the credential through POST /v1/secret-refs and a model connection NAMES the ref, so what the
		// deployment stores is a handle and the value is redeemed at call time from the encrypted store.
		// Keeping a second, weaker path meant every deployment that skipped the console silently ran on
		// the weaker one — and no operator could tell which, because both produced a working stack.
		//
		// `fake` stays: it dials nothing, so its "credential" is a placeholder the broker still insists on
		// redeeming, and the shipped-binary wiring proof needs a route that reaches no network.
		Fallback: modelbroker.StaticResolver{modelbroker.SecretRef("fake"): "unused"},
	}})

	if live {
		model := os.Getenv("PALAI_MODEL")
		if model == "" {
			model = "gpt-4o-mini"
		}
		return broker, execution.ModelRoute{Provider: liveModelProvider, Model: model, Secret: modelbroker.SecretRef(liveModelProvider)}, nil
	}
	// No configured provider: the deployment default stays the deterministic fake adapter, exactly as
	// before, and every existing deployment's runs are bit-unchanged.
	return broker, execution.ModelRoute{Provider: registry.FakeFamily, Model: "fake", Secret: modelbroker.SecretRef("fake")}, nil
}

// liveModelProvider is the ONE value of PALAI_MODEL_PROVIDER that selects a live provider. Every other
// value, including the unset one, leaves the deployment default on the deterministic adapter.
const liveModelProvider = "provider-one"

// fakeScriptFileEnv names a JSON file holding the exchange the deterministic adapter replays
// (fake.LoadScript). Unset — which is every deployment that has ever existed — leaves registry.FakeScript
// in place and nothing about a run changes.
const fakeScriptFileEnv = "PALAI_FAKE_SCRIPT_FILE"

// fakeScriptFromEnv resolves that file into the script the adapter map is built with. nil means "the
// built-in one", and it is returned for exactly one input: an unset variable.
//
// A SCRIPT ROUTED INTO A LIVE DEPLOYMENT IS REFUSED, and that refusal is the whole reason this is a
// function rather than a call to fake.LoadScript. The deterministic adapter is reachable ONLY as the
// deployment default — no model connection can name the family (registry.FakeFamily), so with
// PALAI_MODEL_PROVIDER=provider-one the script would be loaded, installed, and replayed by nothing. An
// operator would run their proof, watch a live model answer with no tool call, and have no way to tell
// that the file they wrote was never consulted. That is the same "declared, and nothing happens" defect
// the seam exists to close, so it is named here instead.
func fakeScriptFromEnv(path string, live bool) (*fake.Script, error) {
	path = strings.TrimSpace(path)
	if path == "" {
		return nil, nil
	}
	if live {
		return nil, fmt.Errorf("%s names %s, but PALAI_MODEL_PROVIDER=%s routes this deployment to a live provider "+
			"and no run can reach the deterministic adapter the script is for: unset one of the two",
			fakeScriptFileEnv, path, liveModelProvider)
	}
	script, err := fake.LoadScript(path)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", fakeScriptFileEnv, err)
	}
	return &script, nil
}

// connectionProbers adapts the adapter registry's probers to the store's seam. It is a conversion rather
// than a direct pass because the store must not import the adapters — the store is what api.ModelRouteAPI
// is satisfied by, and a wire-format dependency there would put every provider adapter behind the
// management surface.
func connectionInspectors() map[string]store.ConnectionInspector {
	out := map[string]store.ConnectionInspector{}
	for family, prober := range registry.Inspectors() {
		out[family] = prober
	}
	return out
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
		return repositories.NewAnonymousBroker()
	}
	keyPEM, err := os.ReadFile(keyFile)
	if err != nil {
		log.Printf("repository broker: read app key file: %v (using local broker)", err)
		return repositories.NewAnonymousBroker()
	}
	cfg := repositories.GitHubAppConfig{AppID: appID, InstallationID: installID, PrivateKeyPEM: keyPEM}
	if slug := os.Getenv("PALAI_GITHUB_REPO"); strings.IndexByte(slug, '/') > 0 {
		cfg.Repositories = []string{slug[strings.IndexByte(slug, '/')+1:]}
	}
	broker, err := repositories.NewGitHubAppBroker(cfg)
	if err != nil {
		log.Printf("repository broker: app broker: %v (using local broker)", err)
		return repositories.NewAnonymousBroker()
	}
	return broker
}

// shellPostureNative is the ONLY accepted value of PALAI_SHELL_NATIVE. It is posture's constant since
// A.3, not a copy: the RUNNER compares against the same word now that the runner is what executes the
// command, and two spellings of one posture name is how a machine ends up in a posture nobody declared.
const shellPostureNative = posture.Native

// shellPostureDeclaration is the one line the control plane prints at boot when it runs its shell
// tool on the host. It names the posture, what it costs, and the operating rule that replaces the
// boundary — because that rule is the only thing standing between two customers on one Mac, and it
// is enforced by an operator, not by this binary.
const shellPostureDeclaration = "shell posture: UNSANDBOXED HOST — commands run as this uid with no container boundary, " +
	"no network denial and no resource bound; different customers MUST use different Macs " +
	"(docs/research/macos-isolation-without-accounts.md §6, docs/operations/palai-on-a-mac.md)"

// shellRunnerFromEnv is the control plane's read of the posture this deployment declared. Since A.3 the
// derivation itself lives in adapters/sandboxes/posture, because the RUNNER makes the same decision and
// a second writing of four sandbox bounds is how two machines end up disagreeing about what contained a
// command.
//
// WHAT IT BUILDS NO LONGER RUNS A TOOL CALL, and that is the A.3 change: a synchronous shell command
// runs on the machine that holds the attempt's lease (execution.shellFor). This posture survives here
// for the DETACHED half — backgroundRunnerFor below — which is still process-wide because a background
// task outlives its attempt.
//
// A refused posture is logged and yields nil rather than falling back to the other one; main() already
// fail-fasts on it at boot, so this is the second of two doors, not the only one.
func shellRunnerFromEnv() toolbroker.ShellRunner {
	shell, err := posture.RunnerFromEnv()
	if err != nil {
		log.Printf("shell posture: %v (background tasks disabled)", err)
		return nil
	}
	return shell
}

// backgroundRunnerFor reports the DETACHED runner for a wired shell posture, or nil when that posture
// cannot start a command that outlives the attempt (E26 T1).
//
// It is a named function for the same reason sandboxLimitsFromEnv is one: so a test can assert what the
// COMPOSITION ROOT actually builds. Every sandbox test in this tree constructs its own executor, which
// is exactly how a wall time that was unbounded on one posture and refused every call on the other
// stayed invisible to all of them.
//
// NIL IS THE HONEST ANSWER AND NOT A DEGRADED ONE. A posture that cannot detach must leave this seam
// unwired so the tool refuses; falling back to a synchronous run would block the model in precisely the
// way background execution exists to prevent.
func backgroundRunnerFor(shell toolbroker.ShellRunner) toolbroker.BackgroundRunner {
	background := posture.BackgroundRunnerFor(shell)
	if background == nil {
		log.Printf("shell posture %T cannot start background tasks (the background parameter will be refused)", shell)
	}
	return background
}

// defaultSandboxWallTime bounds ONE shell tool call in BOTH postures. It is a default rather than a
// required setting because the variable was set in no shipped file, and unset meant two opposite
// broken things: the host executor ran UNBOUNDED (zero = unbounded, adapters/sandboxes/host), and the
// OCI driver REFUSED every call before creating a container (Limits.Validate rejects a non-positive
// bound). Neither is a posture anyone chose.
//
// THE NUMBER IS MEASURED, NOT PICKED. On the owner's own Mac (Xcode 26.6, 2026-07-30): `xcodebuild
// -version` takes 3.15s; merely LISTING the schemes of a real iOS project with its packages already
// cached takes 12.3s; a clean `swift build` of one trivial SwiftPM target takes 59s; and a clean
// `xcodebuild build` of a single iOS framework scheme with SPM dependencies, code signing off, takes
// 3m32s at 19% CPU — meaning contention, not compute, dominates, so a loaded machine is slower still.
// Ten minutes clears the largest of those by roughly 3x and leaves room for a full app plus its test
// suite; anything in the tens of seconds would fail on a command that only PRINTS A VERSION.
//
// It is deliberately an order of magnitude above the tree's other timeouts (PALAI_STACK_READY_TIMEOUT
// 90s, PALAI_DRAIN_TIMEOUT 20s, the live MCP suites 60s) because it bounds a different kind of work:
// those bound operations PALAI performs, whose cost Palai controls and can estimate. This one bounds
// whatever toolchain the operator's argv names, whose cost Palai does not control and cannot know.
//
// It is a BACKSTOP AGAINST A HANG, not a schedule — a build wedged on a stuck CoreSimulatorService is
// what it exists to reap. A deployment whose real builds run longer sets the variable, which is now
// documented in deploy/compose/compose.yaml and docs/operations/palai-on-a-mac.md; before this change
// there was nothing to discover, because there was no default to be wrong about.
//
// It also stays well below the 60m ceiling E26 chose for BACKGROUND tasks
// (docs/superpowers/plans/phase-26-background-execution.md §0.2), keeping the two ordered: work that
// needs an hour belongs in the background, which is that plan's own argument.
// defaultSandboxWallTime is posture's constant, not a copy of it. It had been written down twice —
// once here and once in cmd/runner — with a comment in the second saying it MUST match the first,
// which is the shape of a number that eventually will not.
const defaultSandboxWallTime = posture.DefaultWallTime

// sandboxWallTime is the PALAI_SANDBOX_WALL_TIME bound BOTH postures run under, on BOTH planes since
// A.3. One derivation rather than several: the postures had drifted apart through a shared unset
// variable once already, and a bound that means different things on different machines is the defect
// this replaces.
func sandboxWallTime() time.Duration { return posture.WallTime() }

// sandboxLimitsFromEnv builds the OCI posture's resource bounds — PALAI_SANDBOX_MAX_MEMORY_BYTES,
// PALAI_SANDBOX_MAX_PROCS, PALAI_SANDBOX_NANO_CPUS — over that wall time. It is a named function so a
// test can assert what the composition root ACTUALLY builds: every sandbox test in this tree constructs
// its own oci.Limits with explicit values, which is exactly why an unset variable that refused every
// containerised shell call was invisible to all of them.
func sandboxLimitsFromEnv() oci.Limits { return posture.Limits() }

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
				coordinator.Tenant{Project: scope.Project},
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
// A.2 Task 6 removed the org argument: secret_refs carries no tenant column and migration 000066 keys it
// on the INSTALLATION, so the argument selected nothing — see identity.SecretStore.Resolve.
func dbSecret(ref string) ([]byte, bool, error) {
	if dbSecretStore == nil {
		return nil, false, nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), secretResolveTimeout)
	defer cancel()
	v, ok, err := dbSecretStore.Resolve(ctx, ref)
	if err != nil {
		if errors.Is(err, identity.ErrSecretDecrypt) {
			return nil, false, err // fail closed: never fall back to a superseded env secret
		}
		// Not every consumer HAS an env bridge (the repository connection resolver is DB-only), so the log
		// states what happened here rather than claiming a fallback the caller may not have.
		log.Printf("secret store: resolve ref %q: %v (treated as a miss)", ref, err)
		return nil, false, nil
	}
	return v, ok, nil
}

// repositoryConnectionSecret bridges a repository binding's connection_ref to the Git credential bytes at
// clone time (E13 Task 9). Unlike its four siblings it is DB-ONLY: connection_ref is a NEW consumer, so
// there is no pre-T3 env-file bridge to stay compatible with — a binding-scoped Git credential is
// provisioned over POST /v1/secret-refs and rotated there with no restart. A MISS is an error: a binding
// that deliberately names its own credential must never silently clone under the deployment-global App.
//
// THERE IS NO TENANT SCOPING ON THIS READ, and saying so is the point: this line claimed "the org is
// server-minted from the run, so the store scopes the read to it and RLS denies any foreign row". dbSecret
// resolves under WithInstallationScope (000066 keys secret_refs on the installation), so a connection_ref
// names the same row for every project. What still holds is that the ref comes from the BINDING row and
// never from tenant request input — a caller cannot ask for an arbitrary name here.
//
// HONEST CEILING: there is no per-tenant GitHub App ONBOARDING surface (installing an App per tenant and
// capturing its installation credential is product/SaaS work). This resolves whatever token the tenant
// already provisioned under the ref — a PAT or an installation token it manages itself.
func repositoryConnectionSecret(ref string) ([]byte, error) {
	if ref == "" {
		return nil, errors.New("empty repository connection ref")
	}
	v, ok, err := dbSecret(ref)
	if err != nil {
		return nil, err
	}
	if !ok {
		// dbSecret flattens a transient store failure into a miss (it is logged there), so this covers two
		// causes and must not assert the wrong one: an operator reading "unprovisioned" during a Postgres
		// blip would go hunting for a ref that is in fact present.
		return nil, fmt.Errorf("repository connection %q did not resolve: no such secret ref, or the secret store was unreachable (see the secret-store log)", ref)
	}
	return v, nil
}

// environmentValueSecret bridges an ENVIRONMENT key's derived secret name to its value at exec time (E25
// T3). It is the fifth sibling of the webhook/inbound/remote-tool/repository resolvers and, like the
// repository one, DB-ONLY: the derived name `env:<environment_id>:<key>` is minted by this feature and has
// no pre-existing env-file bridge to stay compatible with.
//
// A MISS IS AN ERROR. The write path lands the membership row and the sealed version in ONE transaction,
// so a named key with no resolvable value means the row was hand-edited or the master key changed. The
// alternative — an empty string — would run the agent's command anonymously and report success, which is
// the failure this resolver exists to prevent.
//
// The derived name is server-minted from the RUN, never from anything the model or the engine said — that
// is the property, and it is the one this resolver actually has. It used to add "so the store scopes the
// read to it and RLS denies any foreign row"; dbSecret runs under WithInstallationScope, so there is no
// per-tenant scoping to lean on and the minting is doing all the work. The error names the ref, never the
// value.
func environmentValueSecret(ref string) ([]byte, error) {
	if ref == "" {
		return nil, errors.New("empty environment ref")
	}
	v, ok, err := dbSecret(ref)
	if err != nil {
		return nil, err
	}
	if !ok {
		// dbSecret flattens a transient store failure into a miss (logged there), so this covers two causes
		// and must not assert the wrong one — the repositoryConnectionSecret precedent, verbatim.
		return nil, fmt.Errorf("environment value %q did not resolve: no such secret ref, or the secret store was unreachable (see the secret-store log)", ref)
	}
	return v, nil
}

// mcpSecretResolver bridges an MCP connection's secret_ref handle to the bearer bytes at request time (the
// webhookSecretResolver twin): the DB-backed store (E13 T3) is consulted first, then
// PALAI_MCP_SECRET_FILE_<REF> holds a FILE PATH, never the secret inline, read only here and never logged.
//
// <REF> is secretEnvKey(ref) — the ref upper-cased with every non-alphanumeric byte replaced by `_`, and
// NOTHING ELSE. It said <ORG>__<REF> until A.2 Task 6, and that was an operator-facing env var name this
// binary could not produce. The org segment is gone from the key AND from the property: a ref name is
// single-occupancy across the installation (000066), so there is no per-tenant namespace here to enforce.
// See identity.SecretStore.Resolve, which records the same correction for the DB half.
func mcpSecretResolver(ref string) ([]byte, error) {
	if ref == "" {
		return nil, errors.New("empty mcp secret ref")
	}
	if v, ok, err := dbSecret(ref); err != nil {
		return nil, err
	} else if ok {
		return v, nil
	}
	path := os.Getenv("PALAI_MCP_SECRET_FILE_" + secretEnvKey(ref))
	if path == "" {
		return nil, fmt.Errorf("no secret bridge configured for mcp ref %q", ref)
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
// touches an MCP one). Grace/interval are env-tunable.
//
// It is no longer started UNCONDITIONALLY (§3.6 D8). client.New only builds a client — it dials nothing — so
// on a deployment with no Docker socket the failure surfaced once a minute, forever, from the loop. The first
// pass runs HERE instead, synchronously, and doubles as the reachability probe: it reclaims whatever the
// previous process left behind, and if the daemon cannot be reached the sweep is disabled with ONE line
// naming the reason. There is no MCP stdio transport without a Docker socket, so there is nothing for the
// loop to reclaim either.
//
// ponytail: a transient daemon hiccup exactly at boot disables the sweep for that process's lifetime; the
// orphans are reclaimed by the next boot's probe. Promote this to a bounded retry if a real deployment is
// ever seen to lose the socket only momentarily at startup.
func startMCPOrphanSweep(ctx context.Context, supervisor *coordinator.Supervisor) {
	grace := envDurationOr("PALAI_MCP_SWEEP_GRACE", 2*time.Minute)
	interval := envDurationOr("PALAI_MCP_SWEEP_INTERVAL", time.Minute)
	sweeper, err := mcpclient.NewSweeper(grace)
	if err != nil {
		log.Printf("mcp orphan-sweep: %v (disabled)", err)
		return
	}
	switch reclaimed, err := sweeper.Sweep(ctx); {
	case err != nil:
		log.Printf("mcp orphan-sweep: docker is not reachable from this process (%v) — the sweep is DISABLED for this "+
			"boot rather than retried every %s. MCP stdio servers need the same socket, so nothing can be orphaned "+
			"while it is absent.", err, interval)
		return
	case reclaimed > 0:
		log.Printf("mcp orphan-sweep reclaimed %d orphan container(s) left by an earlier process", reclaimed)
	}
	go supervisor.Supervise(ctx, "mcp-orphan-sweep", func(ctx context.Context) error { return sweeper.Run(ctx, interval) })
}

// repositoryPublisher builds the repository publisher the approval pump publishes through (spec
// §30.9-30.10). IT HAS TWO CREDENTIAL PATHS AND THEY ARE CONSTRUCTED INDEPENDENTLY — which is the whole
// shape of this function rather than a stylistic preference, so the reason is here and not in a plan.
//
// THE DEFECT THIS SHAPE REPLACES. Every field below, the connection_ref resolver included, used to be
// built INSIDE `if appID == "" || installID == "" || keyFile == "" { return nil }`. So the object that
// knows how to publish under a TENANT's own credential was constructed only on a deployment that had
// configured the DEPLOYMENT-GLOBAL App — the thing that path exists to replace. On a stack with no App,
// main.go wired nothing, pumpApprovedPublications became a no-op, and an APPROVED push waited forever:
// the model got its pending_approval, a human pressed Approve, the run woke, and nothing was pushed.
// Measured on the live native stack, 2026-08-02: no App configured, a binding carrying connection_ref
// `demo-local-token`, and no push possible.
//
// SO THE PUBLISHER IS ALWAYS BUILT AND main.go ALWAYS WIRES IT. The App half is an OPTIONAL EXTRA
// (gitHubAppPublisherFromEnv, below) rather than a gate around the rest, and a deployment that configured
// no App gets a publisher whose Broker is nil. A publication whose binding names no connection_ref is
// then REFUSED by RepositoryPublisher.CanPublish — with a sentence naming the three variables — instead of
// being silently not-attempted. A refusal reaches the operator (the publication row's warning, and the
// publication tool refuses it before a human is ever asked); a no-op reached nobody.
//
// The App private key arrives via the LP-0 file-secret bridge (PALAI_GITHUB_APP_PRIVATE_KEY_FILE — a PATH,
// never inline), sealed at rest by E13; this process only mints short-lived scoped tokens against it and
// never logs it.
func repositoryPublisher() execution.Publisher {
	publisher := &execution.RepositoryPublisher{
		// THE BINDING'S OWN CREDENTIAL, on the way OUT, and it is wired UNCONDITIONALLY. The clone half has
		// resolved this since E13 T9 (SetConnectionSecrets, above). It is the SAME function, passed rather
		// than re-implemented, so one place in this tree turns a connection_ref into a credential.
		ConnectionSecrets: repositoryConnectionSecret,
		// And the pull-request client per binding, over that credential and the binding's own owner/repo —
		// also unconditional, because a binding's identity is a property of the binding and not of the App.
		PRClientFor: func(token, owner, repo string) (repositories.PullRequestClient, error) {
			return repositories.NewTokenPullRequestClient(token, "", owner, repo)
		},
	}
	publisher.Broker, publisher.PRClient = gitHubAppPublisherFromEnv()
	return publisher
}

// gitHubAppPublisherFromEnv builds the DEPLOYMENT-GLOBAL half — the credential a binding that names no
// connection_ref of its own publishes under. Both returns are nil when no App is configured, which is a
// supported deployment and not a failure: see repositoryPublisher above.
//
// The App path stays exactly as it was for the fleet case it is right for, where handing a run a tenant
// PAT would hand it that account's whole reach. What changed is only that its absence no longer removes
// the other path.
//
// "AN APPROVED PUSH WILL WAIT FOREVER" NO LONGER BELONGS AT THIS LOG LINE, and dropping it is a
// correction rather than a tidy-up: after this change nothing waits silently — a ref-less publication on
// an App-less deployment is refused at the tool, before a human is asked, and GET /v1/deployment carries
// the warning. What the half-configured case still deserves is its own line, because it is the one state
// an operator reaches BELIEVING they finished.
func gitHubAppPublisherFromEnv() (repositories.Broker, repositories.PullRequestClient) {
	appID := os.Getenv("PALAI_GITHUB_APP_ID")
	installID := os.Getenv("PALAI_GITHUB_APP_INSTALLATION_ID")
	keyFile := os.Getenv("PALAI_GITHUB_APP_PRIVATE_KEY_FILE")
	if !api.GitHubAppConfigured() {
		// THE CONDITION IS THE TWO IDENTIFIERS, NOT THE KEY PATH, and it was measured: every bring-up sets
		// PALAI_GITHUB_APP_PRIVATE_KEY_FILE (native.go's env map and compose.yaml both write it
		// unconditionally, at a file-secret slot that exists EMPTY until an App is configured), so a line
		// conditioned on it printed on a stack that had configured NOTHING — seen on the live native stack
		// 2026-08-02 at boot. `palai up` calls that crying wolf and removed it once already.
		if appID != "" || installID != "" {
			log.Printf("repository publisher: PALAI_GITHUB_APP_ID/PALAI_GITHUB_APP_INSTALLATION_ID/" +
				"PALAI_GITHUB_APP_PRIVATE_KEY_FILE are required TOGETHER; at least one is missing, so the " +
				"deployment-global App is OFF — only a binding carrying its own connection_ref can publish")
		}
		return nil, nil
	}
	keyPEM, err := os.ReadFile(keyFile)
	if err != nil {
		log.Printf("repository publisher: read app key file: %v (the deployment-global App is OFF)", err)
		return nil, nil
	}
	// owner/repo for the App's pull-request client. PALAI_GITHUB_REPO is this binary's own name for it;
	// PALAI_GIT_REPO is the one §0.2 asks the operator for and the repository binding already uses, so both
	// are read rather than letting a stack be configured correctly and publish nothing. It narrows ONLY the
	// App path: a binding with its own credential brings its own owner/repo (target.Identity), which is why
	// one stack no longer opens pull requests against exactly one repository however many bindings it serves.
	owner, repo := "", ""
	slug := os.Getenv("PALAI_GITHUB_REPO")
	if slug == "" {
		slug = os.Getenv("PALAI_GIT_REPO")
	}
	if strings.IndexByte(slug, '/') > 0 {
		i := strings.IndexByte(slug, '/')
		owner, repo = slug[:i], slug[i+1:]
	}
	cfg := repositories.GitHubAppConfig{AppID: appID, InstallationID: installID, PrivateKeyPEM: keyPEM}
	if repo != "" {
		cfg.Repositories = []string{repo}
	}
	broker, err := repositories.NewGitHubAppBroker(cfg)
	if err != nil {
		log.Printf("repository publisher: app broker: %v (the deployment-global App is OFF)", err)
		return nil, nil
	}
	if owner == "" || repo == "" {
		// A push publishes without this (its remote comes from the binding); a pull request cannot, and
		// answers "no pull-request client wired" at the pump. Said out loud, because the operator's only
		// other symptom is an approved PR that never opens.
		log.Printf("repository publisher: no owner/repo (set PALAI_GIT_REPO=owner/repo) — App pushes publish, " +
			"App pull requests do NOT")
		return broker, nil
	}
	prClient, err := repositories.NewGitHubPullRequestClient(cfg, owner, repo)
	if err != nil {
		log.Printf("repository publisher: pr client: %v (App pull requests disabled)", err)
		return broker, nil
	}
	return broker, prClient
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
// time (the E09 credential-broker hand-off pattern): PALAI_WEBHOOK_SECRET_FILE_<REF> holds a FILE PATH,
// never the secret inline, and the bytes are read only here and never logged (E13 seals the file at rest).
//
// THE ENV KEY IS NO LONGER SCOPED BY ORG, and the claim goes with the segment rather than being softened.
// It read "<ORG>__<REF> … a tenant's SigningSecretRef can only name a secret provisioned under its OWN
// org — a foreign ref resolves to no env var (F2)". secretEnvKey takes the ref ALONE, so that env var name
// never existed after A.2 Task 6, and the boundary it described does not either: a ref name is
// installation-wide (000066). An unresolved ref still fails the attempt (a retry), never an unsigned
// delivery — that half was always about resolution, not about tenancy, and it still holds.
func webhookSecretResolver(ref string) ([]byte, error) {
	if ref == "" {
		return nil, errors.New("empty webhook secret ref")
	}
	if v, ok, err := dbSecret(ref); err != nil {
		return nil, err
	} else if ok {
		return v, nil
	}
	path := os.Getenv("PALAI_WEBHOOK_SECRET_FILE_" + secretEnvKey(ref))
	if path == "" {
		return nil, fmt.Errorf("no secret bridge configured for webhook ref %q", ref)
	}
	return os.ReadFile(path)
}

// inboundSecretResolver is the receiver-side sibling of webhookSecretResolver (E11 Task 5): it bridges a
// trigger's inbound source-secret ref to bytes via PALAI_INBOUND_SECRET_FILE_<REF> (a FILE PATH, never
// inline; E13 seals the file at rest). The <ORG>__ segment left with A.2 Task 6 along with the boundary it
// claimed — secretEnvKey keys on the ref alone and a ref name is installation-wide (000066). What DOES
// still hold is the other half: the inbound namespace is DISTINCT from the outbound
// PALAI_WEBHOOK_SECRET_FILE_ one, so the two secret sets remain non-interchangeable — that separation is
// carried by the PREFIX, which no tenant ever supplied.
// An unresolved ref fails verification (a generic 404 upstream — no config oracle), never an unsigned accept.
func inboundSecretResolver(ref string) ([]byte, error) {
	if ref == "" {
		return nil, errors.New("empty inbound secret ref")
	}
	if v, ok, err := dbSecret(ref); err != nil {
		return nil, err
	} else if ok {
		return v, nil
	}
	path := os.Getenv("PALAI_INBOUND_SECRET_FILE_" + secretEnvKey(ref))
	if path == "" {
		return nil, fmt.Errorf("no secret bridge configured for inbound ref %q", ref)
	}
	return os.ReadFile(path)
}

// remoteToolSecretResolver is the third sibling of webhook/inboundSecretResolver (E12 Task 4): it bridges
// a tool_revision.secret_ref handle to the HMAC signing-secret bytes via
// PALAI_REMOTE_TOOL_SECRET_FILE_<REF> (a FILE PATH, never inline; E13 seals the file at rest). The SAME
// secret signs the outbound invoke and verifies the inbound callback. The <ORG>__ segment and the
// per-tenant boundary it claimed both left with A.2 Task 6: secretEnvKey keys on the ref alone and a ref
// name is installation-wide (000066). The remote-tool namespace is still DISTINCT from the
// webhook/inbound ones — three PREFIXES, so the three secret sets stay non-interchangeable. An unresolved
// ref fails the invoke (a retry) / a generic-404 callback, never an unsigned request or accept.
func remoteToolSecretResolver(ref string) ([]byte, error) {
	if ref == "" {
		return nil, errors.New("empty remote tool secret ref")
	}
	if v, ok, err := dbSecret(ref); err != nil {
		return nil, err
	} else if ok {
		return v, nil
	}
	path := os.Getenv("PALAI_REMOTE_TOOL_SECRET_FILE_" + secretEnvKey(ref))
	if path == "" {
		return nil, fmt.Errorf("no secret bridge configured for remote tool ref %q", ref)
	}
	return os.ReadFile(path)
}

// a2aRemoteSecretResolver is the fourth sibling of webhook/inbound/remoteToolSecretResolver (E19 T5).
// It was the FIFTH until 2026-08-05, when slackSecretResolver went with the Slack bridge that was its only
// caller: a bot's credentials are resolved through GET /v1/bots/{id}/credentials now, and PALAI_SLACK_SECRET_FILE_*
// has no reader left in this binary.
// it bridges an a2a_remote_agents.auth_connection_ref handle to the REMOTE CONNECTION'S OWN bearer via
// PALAI_A2A_REMOTE_SECRET_FILE_<REF> (a FILE PATH, never inline). The <ORG>__ segment and the per-tenant
// boundary it claimed both left with A.2 Task 6: secretEnvKey keys on the ref alone and a ref name is
// installation-wide (000066). The A2A-remote namespace is still DISTINCT from the
// webhook/inbound/remote-tool/Slack ones — five PREFIXES, so the five secret sets stay
// non-interchangeable. This is the ONLY bearer a remote child dial can carry: an unresolved ref FAILS
// the dispatch (an honest child failure), it never falls back to the platform's or the parent's credential.
func a2aRemoteSecretResolver(ref string) ([]byte, error) {
	if ref == "" {
		return nil, errors.New("empty a2a remote secret ref")
	}
	if v, ok, err := dbSecret(ref); err != nil {
		return nil, err
	} else if ok {
		return v, nil
	}
	path := os.Getenv("PALAI_A2A_REMOTE_SECRET_FILE_" + secretEnvKey(ref))
	if path == "" {
		return nil, fmt.Errorf("no secret bridge configured for a2a remote ref %q", ref)
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

// startIdleRelease launches the workspace idle releaser: a session that stops using its machine has the
// machine handed back — the allocation archived and its directory reclaimed — WITHOUT the session closing,
// so the next message in that thread restores it and carries on where it left off.
//
// TWO GATES, AND ONLY ONE OF THEM IS A PREFERENCE.
//
// PALAI_WORKSPACE_ROOT is the honest one: a deployment that provisions no workspaces has no allocations to
// release, so the sweep would find nothing on every tick forever.
//
// artStore is a CORRECTNESS gate and refusing on it is the whole safety argument. The release deletes a
// directory holding a session's uncommitted work; the only thing that makes that survivable is the archive
// written first. With no object store there is no archive, so a releaser here would not be a weaker
// releaser — it would be data loss on a timer. It refuses loudly rather than degrading.
//
// PALAI_WORKSPACE_IDLE_TTL is a knob, not a switch: a deployment that wants workspaces held longer says how
// much longer. There is deliberately no value that turns the sweep off, because "off" is the state this
// stack was already in and it is the defect being fixed — 107 allocation directories and 97 uid slots held
// by sessions that had not run anything for days (measured 2026-08-05).
func startIdleRelease(ctx context.Context, repo *store.Store, supervisor *coordinator.Supervisor, artStore *artifacts.Store, sessionAccounts *execution.SlotAccounts) {
	if os.Getenv("PALAI_WORKSPACE_ROOT") == "" {
		return // no workspaces provisioned here, so no machine to hand back
	}
	if artStore == nil {
		log.Printf("workspace idle release DISABLED: no object store is configured, and releasing a workspace whose bytes were never archived would delete a session's uncommitted work")
		return
	}
	ttl := envDurationOr("PALAI_WORKSPACE_IDLE_TTL", execution.DefaultIdleWorkspaceTTL)
	releaser := execution.NewIdleReleaser(repo.Spine(), execution.NewSnapshotSink(artStore, repo.Spine()), ttl)
	// THE SAME INSTANCE THE ACQUIRE HALF HOLDS. SlotAccounts keeps the session→slot map in process, so a
	// second instance would hold an empty one and release nothing while reporting success — the release
	// half of the leak this whole function exists to close.
	if sessionAccounts != nil {
		releaser = releaser.WithSessionAccounts(sessionAccounts)
	}
	log.Printf("workspace idle release enabled: a session's machine is handed back after %s idle, and restored from its archive on the next message", ttl)
	go supervisor.Supervise(ctx, "workspace-idle-release", func(ctx context.Context) error { return releaser.Run(ctx, 30*time.Second) })
}

// startLeaseReclaim launches the abandoned-writer-lease sweep, which returns a workspace whose holder run
// reached terminal from `leased` back to `ready`.
//
// IT IS THE IDLE RELEASER'S FEEDER, NOT ITS RIVAL, and wiring the two together is the point. The idle
// releaser only ever considers a `ready` workspace with no active lease; a lease nobody releases from the
// inside leaves the workspace `leased` forever, which is invisible to it. So without this the sweep above
// simply never sees a whole class of held machine — the class produced by the ordinary end of a thread.
//
// TWO WAYS A LEASE OUTLIVES ITS RUN, and only one of them was ever repaired. A crash mid-attempt is
// covered, because the next attempt on that allocation reclaims the dangling lease (acquireWriterLease).
// A thread that simply ENDS is not: there is no next attempt, so there is no reclaim, and until this
// existed the only cure was a human typing into that thread again.
//
// NO artStore GATE, unlike startIdleRelease, and the asymmetry is deliberate rather than an oversight.
// That gate is a correctness gate because the release DELETES a directory and only an archive makes that
// survivable. This sweep deletes nothing: it releases a lease row and moves a state to `ready`, both
// reversible by the next attempt that wants the workspace. What it hands the workspace to — the idle
// releaser — keeps its own refusal, so an object-store-less deployment reclaims leases and never deletes
// bytes, which is the correct behaviour for it rather than a degraded one.
//
// PALAI_WORKSPACE_ROOT gates it for the same honest reason it gates the releaser: a deployment that
// provisions no workspaces has no leases to reclaim.
func startLeaseReclaim(ctx context.Context, repo *store.Store, supervisor *coordinator.Supervisor) {
	if os.Getenv("PALAI_WORKSPACE_ROOT") == "" {
		return // no workspaces provisioned here, so no lease to reclaim
	}
	grace := envDurationOr("PALAI_ABANDONED_LEASE_GRACE", execution.DefaultAbandonedLeaseGrace)
	reclaimer := execution.NewLeaseReclaimer(repo.Spine(), grace)
	log.Printf("abandoned writer-lease reclaim enabled: a workspace whose holder run has been terminal for %s returns to ready, where the idle sweep can hand its machine back", grace)
	go supervisor.Supervise(ctx, "workspace-lease-reclaim", func(ctx context.Context) error { return reclaimer.Run(ctx, 30*time.Second) })
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
func startRunnerGateway(addr string, registry fleet.Registry, poolKeys execution.PoolEnrollment, waker execution.CapacityWaker, poolSettings execution.PoolSettings) *execution.RunnerGateway {
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
	// The pool keys go FIRST and the file token stays SECOND (E24 T3). A machine that presents a pool
	// key enrols into that key's pool and is recorded against it; a machine that presents the file token
	// lands in the default pool exactly as it always has. Nothing is removed: the file token is the only
	// credential that can rescue an identity that has already expired.
	gateway.SetPoolKeys(poolKeys)
	// The registry is what makes a SECOND runner nameable: without it the gateway's only record is the
	// last certificate it saw (one slot, last writer wins), so `--scale runner=3` was three machines and
	// one observable identity. Wired here rather than passed to the constructor so every existing caller
	// — the conformance tier, every wire proof — compiles and behaves unchanged.
	gateway.SetRegistry(registry)
	// The capacity waker (E24 T4): when a machine joins a pool, the pool's oldest run parked for want of
	// a machine is re-entered and its response.run job enqueued, in one transaction. Without this line a
	// run placed in an empty pool waits forever instead of ~2.5 minutes, which is a worse bug than the
	// one it replaces — so it is fenced by name in a test, for the reason T3 fenced three call sites.
	gateway.SetCapacityWaker(waker)
	// The runner-plane desired-configuration reader (E29's second plane). Until this line the plane existed
	// in the document's key and in migration 000053's CHECK, and the write path refused it BY NAME because
	// "a row written here would be a setting no machine ever sees" — the reader was a second binary and
	// nothing handed it a document. This is that hand-off: a machine's pool document rides the answer to its
	// own enrolment, so an operator configures a fleet from one screen instead of editing an env file on
	// every box.
	gateway.SetPoolSettings(poolSettings)

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
