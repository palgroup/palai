package api

import (
	"encoding/json"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"

	"github.com/palgroup/palai/apps/control-plane/api/middleware"
)

// GET /v1/deployment — THE MACHINE'S OWN CONFIGURATION.
//
// Measured on main at cf0efd63 (2026-08-01):
//
//	grep -oE 'PALAI_[A-Z_]+' deploy/compose/compose.yaml | sort -u   -> 24 settings
//	of those, readable from any /v1 route                            -> 0
//
// Every knob that decides what this deployment DOES lived in container environment, and no client — not
// the console, not the SDK, not `palai` — could read one of them. The cost is not hypothetical: a stack
// brought up with `make local-up` takes PALAI_DISPATCH_WORKERS=0 from compose.yaml:82, accepts runs
// through POST /v1/responses, and executes none of them. The console shows the run parked at
// run.queued.v1 and says nothing, because nothing on the wire could have told it. The value is currently
// discoverable with `docker inspect`, which is not an operator interface.
//
// THIS IS NOT config_policy AND MUST NOT BE. `config_policy` is a JSONB column on the PROJECTS table
// (storage/migrations/000005_config_revisions.up.sql:16), written per project through
// PATCH /v1/projects/{id} and resolved as the §14 project layer. A dispatch worker count is not a
// property of a project — it is a property of the process, shared by every project on it, and putting a
// machine-wide switch behind a project picker would make it look per-project to the one reader who most
// needs to know it is not. The two surfaces answer different questions and neither can answer the other's.
//
// WHAT THIS SURFACE IS ALLOWED TO CLAIM. It reports THIS PROCESS's environment and nothing else. That is
// a real ceiling and it is why PALAI_RUNNER_CONCURRENCY is absent: the variable lives on the RUNNER
// container, and cmd/cli/internal/stack/upgrade.go says what happens to a reader that forgets —
// "reading a runner-scoped var off the control-plane container always misses it". A control plane
// reporting its own (unset) copy and labelling it the runner's would be a confident wrong answer, which
// is worse than the silence this surface replaces.

// The mutability vocabulary. It answers ONE question — what does an operator have to do for a new value
// to take effect — and it has exactly two answers because the code has exactly two.
//
// There is deliberately no "live" word. Every variable below is read from the PROCESS ENVIRONMENT, which
// is fixed at exec: even the handful read per-job rather than at boot (execute_run.go's ExecuteRun closes
// over PALAI_ENGINE_IMAGE when the handler is built) are reading a value nothing in this tree can change
// without replacing the process. A surface offering an "edit" control for any of them would be the exact
// defect this repository keeps finding — declared, and nothing happens.
const (
	// mutabilityBringUp: the value is read once from this process's environment; a new value needs the
	// process replaced with it (a compose recreate, `palai up`, or a native relaunch).
	mutabilityBringUp = "bring_up"
	// mutabilityDefaultOnly: the same, EXCEPT that the behaviour it governs has a shipped runtime
	// write-path, so the environment value only decides the fallback. Naming these apart from
	// mutabilityBringUp is the point of the distinction: an operator told "restart the stack" when a POST
	// would have done has been sent on an outage they did not need.
	mutabilityDefaultOnly = "bring_up_default_only"
)

// The value vocabulary. `path` exists so a reader can tell a filesystem handle from a setting: the
// credential rule in this tree is that a *_FILE variable's VALUE is a path and a path is not a secret.
const (
	kindValue = "value"
	kindPath  = "path"
)

// The DESIRED-value grammar. Every writable setting declares one, and it names the STANDARD LIBRARY CALL
// this binary's own reader makes — not a shape somebody thought looked right.
//
// THAT IS THE WHOLE POINT OF THE GRAMMAR AND IT IS NOT A STYLE CHOICE. Every reader of every catalogued
// setting COERCES SILENTLY on a value it cannot parse, and each one coerces to something different:
//
//	DispatchWorkers        (deployment.go)  strconv.Atoi        -> 1  on error
//	envInt                 (main.go:1994)   strconv.Atoi        -> 0  on error
//	envFloat               (main.go:1978)   strconv.ParseFloat  -> 0  on error
//	envDuration            (main.go:1986)   time.ParseDuration  -> 0  on error
//	envDurationOr          (main.go)        time.ParseDuration  -> the named default on error
//
// So `PALAI_SANDBOX_WALL_TIME=10min` is 10 minutes to a human, an unparseable duration to Go, and the
// DEFAULT to this binary — main.go:996 already writes that down. A write surface that stored "10min"
// would show an operator a wall time the process is not running, which is the defect this whole file was
// built to expose, shipped INTO it. So a value the reader would coerce is REFUSED at the door, and
// TestEveryDesiredValueThisBinaryAcceptsIsParsedByItsOwnReader (in the composition root's test, where the
// real helpers live) drives every accepted value through the real reader to prove the two agree.
const (
	desiredInt      = "integer"  // strconv.Atoi, and non-negative
	desiredRate     = "rate"     // strconv.ParseFloat, finite and non-negative
	desiredDuration = "duration" // time.ParseDuration, non-negative
	desiredToken    = "token"    // a bare identifier: see desiredTokenPattern
)

const (
	warnDispatchOff = "dispatch_workers_zero"
	warnModelFake   = "model_provider_fake"
	// warnDesiredPending: an operator saved a configuration this process is not running. ADVISORY rather
	// than blocking, and the distinction is the honest one — the machine is working, it is working with the
	// PREVIOUS configuration, and the remedy is a bring-up somebody has to choose to run. Calling it
	// blocking would put a red banner on a healthy deployment for as long as one setting was pending.
	warnDesiredPending = "desired_config_pending"

	severityBlocking = "blocking"
	severityAdvisory = "advisory"
)

// deploymentSetting is one row of the effective configuration.
type deploymentSetting struct {
	Name  string `json:"name"`
	Group string `json:"group"`
	// Value is what this process holds, empty when the variable is unset. A URL's userinfo is stripped
	// (see reportedValue) — no catalogued variable is supposed to carry one, and "supposed to" is not a
	// property a response body should depend on.
	Value string `json:"value"`
	Set   bool   `json:"set"`
	// Default is what the process uses when the variable is unset, in the words of the code that applies
	// it. "unset" on its own tells a reader nothing about what is running.
	Default    string `json:"default"`
	Kind       string `json:"kind"`
	Effect     string `json:"effect"`
	Mutability string `json:"mutability"`
	ChangeWith string `json:"change_with"`
	// ReaderFile/ReaderFunc name the code that reads this variable. They are a FUNCTION rather than a
	// line so an unrelated edit above the reader does not redden a citation nobody re-read, and
	// TestEveryCatalogueCitationResolvesToARealReader parses the file and asserts that function's source
	// actually mentions the variable — so the mutability claim above is anchored to code rather than to
	// somebody's recollection of it.
	ReaderFile string `json:"reader_file"`
	ReaderFunc string `json:"reader_func"`
	// Writable reports whether the panel may write a DESIRED value for this setting. It is served rather
	// than derived by each client, so a console and a CLI cannot disagree about which of thirty-five
	// controls exist — and a client written against an older deployment gets `false` and renders nothing,
	// which is the safe direction.
	Writable bool `json:"writable"`
	// NotWritableBecause is the sentence from nonDesiredReason, empty when Writable. It is served for the
	// same reason the effect prose is: an operator who cannot change something on this screen is entitled
	// to the reason without reading Go source, and twenty-four of the thirty-five are in that position.
	NotWritableBecause string `json:"not_writable_because,omitempty"`
	// Desired is what the desired document asks for, and DesiredSet distinguishes "the operator decided
	// this" from "no opinion" — an empty Desired with DesiredSet false is the second, and the two are
	// different facts about the same machine.
	Desired    string `json:"desired"`
	DesiredSet bool   `json:"desired_set"`
	// Drift is DesiredSet && Desired != Value: this process is not running what the document asks for, so
	// a bring-up is pending. It compares the raw environment STRINGS, which is what the next bring-up will
	// export — see desiredDrift for why comparing behaviour instead gets PALAI_DISPATCH_WORKERS backwards.
	Drift bool `json:"drift"`
}

// deploymentWarning is a configured value that changes what the product DOES, in a way a screen showing
// the product would otherwise misrepresent.
//
// IT NAMES NO CONSOLE PATH, on purpose. Which screen would lie is the console's own question — it is the
// only thing that knows what its screens claim — and a control plane carrying a list of `/runs` and
// `/history` would be a server with an opinion about a client it never serves. The `code` is the join.
type deploymentWarning struct {
	Code     string `json:"code"`
	Severity string `json:"severity"`
	Headline string `json:"headline"`
	Detail   string `json:"detail"`
	Remedy   string `json:"remedy"`
	// Settings names the rows this warning was derived from, so a screen can link the banner to the table.
	Settings []string `json:"settings"`
}

type deploymentBody struct {
	Object   string              `json:"object"`
	Settings []deploymentSetting `json:"settings"`
	Warnings []deploymentWarning `json:"warnings"`
	// Desired is the desired document's own metadata, or NULL when no operator has ever written one — which
	// is a fact a screen must be able to state plainly. A machine with no desired document is running on its
	// compose file's defaults, and rendering an empty object here would let a screen imply the panel is in
	// control when the compose file still is.
	Desired *desiredView `json:"desired"`
}

// catalogueEntry is one declared setting. The catalogue is an ALLOW-LIST and that is the whole of the
// credential rule: a variable nobody has decided about is INVISIBLE rather than published, so the failure
// mode of adding PALAI_SECRET_PROVIDER_THREE tomorrow is a missing row, not a leaked key.
type catalogueEntry struct {
	Name       string
	Group      string
	Kind       string
	Default    string
	Effect     string
	Mutability string
	ChangeWith string
	ReaderFile string
	ReaderFunc string
	// DesiredValue names the grammar a written value must satisfy, and its EMPTINESS is what makes the
	// setting read-only: desiredWritable() admits a name only if this is non-empty AND Kind != kindPath.
	// There is no separate boolean, because two fields that must agree are a pair that can disagree.
	DesiredValue string
}

const (
	cpMain    = "apps/control-plane/cmd/palai-control-plane/main.go"
	changeCP  = "recreate the control-plane with the new value (`palai up`, or `docker compose up -d --force-recreate control-plane`)"
	changeAll = "recreate the whole stack with the new value (`palai up`)"
	// changeDesired is what a WRITABLE setting's remedy became, and the second sentence is a CORRECTION to
	// what this catalogue used to tell every operator.
	//
	// changeCP names two commands as equivalent and for a desired-configuration setting they are not.
	// `palai up` reads the desired document off the machine and exports it (cmd/cli/internal/stack/desired.go);
	// `docker compose up -d --force-recreate control-plane` interpolates ${PALAI_X} from the SHELL THAT
	// INVOKES IT, which knows nothing about the document. An operator who saved a value in the panel and then
	// ran the compose command would recreate the process with the value it already had and see the pending
	// banner survive a restart, with nothing on any screen able to say why.
	changeDesired = "save it here (it is written to this machine's desired configuration) and then run `palai up` on the machine — " +
		"the bring-up reads that document and exports it into the environment it starts the control plane with. " +
		"`docker compose up -d --force-recreate control-plane` does NOT apply it: compose interpolates from the shell that runs it, not from the document"
)

// deploymentCatalogue is the ordered, declared configuration of this process. ORDER IS THE SCREEN'S: the
// two settings that silently change what the product does come first, because an alphabetical dump puts
// PALAI_DISPATCH_WORKERS between the CA paths and the engine image, which is where it was already hiding.
//
// Every ReaderFile/ReaderFunc pair below was measured by parsing the cited file and reading which
// function's body mentions the variable; the guard re-runs that parse on every test run.
var deploymentCatalogue = []catalogueEntry{
	// --- what this deployment actually DOES with a submitted run -------------------------------------
	{
		Name: "PALAI_DISPATCH_WORKERS", Group: "execution", Kind: kindValue, Default: "1",
		Effect:     "How many durable dispatch workers run. ZERO IS NOT 'SLOWER', IT IS OFF: startDispatch returns before it builds anything, so the deployment admits runs through POST /v1/responses and executes none — and with it go the reconciler, the dead-letter sweep, approval expiry, capacity-park expiry and the background-exit notification.",
		Mutability: mutabilityBringUp, ChangeWith: changeDesired,
		// The reader is this file's own DispatchWorkers, and main.dispatchWorkerCount delegates to it, so
		// the number this surface reports and the number startDispatch gates on are the same number.
		ReaderFile: "apps/control-plane/api/deployment.go", ReaderFunc: "DispatchWorkers",
		DesiredValue: desiredInt,
	},
	{
		Name: "PALAI_MODEL_PROVIDER", Group: "model", Kind: kindValue, Default: "fake",
		Effect:     "The DEPLOYMENT-DEFAULT model route. Exactly one value — `provider-one` — selects a live provider; every other value, including a provider name this binary can otherwise speak to and including a typo, falls through to the deterministic fake adapter, whose answers are fabricated and render exactly like real ones.",
		Mutability: mutabilityDefaultOnly,
		ChangeWith: "for ONE project, publish a model route (POST /v1/model-routes) — it is resolved per attempt and overrides this with no restart; to change the fallback every project without a route gets, " + changeDesired,
		ReaderFile: cpMain, ReaderFunc: "modelBrokerFromEnv",
		DesiredValue: desiredToken,
	},
	{
		Name: "PALAI_MODEL", Group: "model", Kind: kindValue, Default: "gpt-4o-mini (only when PALAI_MODEL_PROVIDER=provider-one; otherwise the fake adapter's `fake`)",
		Effect:     "The model id the deployment-default route names. Read only on the `provider-one` branch — with any other provider it is inert.",
		Mutability: mutabilityDefaultOnly,
		ChangeWith: "a published project model route names its own model; otherwise " + changeDesired,
		ReaderFile: cpMain, ReaderFunc: "modelBrokerFromEnv",
		DesiredValue: desiredToken,
	},
	{
		Name: "PALAI_OPENAI_COMPATIBLE_BASE_URL", Group: "model", Kind: kindValue, Default: "none — the custom family then has no deployment-wide endpoint",
		Effect:     "The deployment-wide endpoint for the OpenAI-compatible provider family. A model connection carrying its own base URL (migration 000049) wins over it, per request.",
		Mutability: mutabilityDefaultOnly,
		ChangeWith: "give the model connection its own endpoint (POST /v1/model-connections); otherwise " + changeCP,
		ReaderFile: cpMain, ReaderFunc: "modelBrokerFromEnv",
	},
	{
		Name: "PALAI_ENGINE_IMAGE", Group: "execution", Kind: kindValue, Default: "none — a lease pins no image and a dispatched run cannot start an engine",
		Effect:     "The engine image digest the control plane pins into every lease. The runner refuses a mutable tag, so this is an immutable sha256 reference.",
		Mutability: mutabilityBringUp, ChangeWith: changeAll,
		ReaderFile: "apps/control-plane/internal/execution/execute_run.go", ReaderFunc: "ExecuteRun",
	},
	{
		Name: "PALAI_QUEUE_DEADLINE", Group: "execution", Kind: kindValue, Default: "disabled — a run never expires on queue age",
		Effect:     "How long an admitted run may wait in the queue before dispatch times it out, ahead of any billable compute (§20.12).",
		Mutability: mutabilityBringUp, ChangeWith: changeDesired,
		ReaderFile: cpMain, ReaderFunc: "startDispatch",
		DesiredValue: desiredDuration,
	},
	{
		Name: "PALAI_WORKSPACE_ROOT", Group: "execution", Kind: kindPath, Default: "none — no workspace is provisioned and GET /v1/capabilities reports workspaces `unavailable`",
		Effect:     "The host directory coding workspaces are allocated under. Its presence is what makes this deployment advertise the `workspaces` capability at all.",
		Mutability: mutabilityBringUp, ChangeWith: changeCP,
		ReaderFile: cpMain, ReaderFunc: "startDispatch",
	},

	// --- where a shell command runs, which is a security posture rather than a feature ---------------
	{
		Name: "PALAI_SANDBOX_IMAGE", Group: "shell", Kind: kindValue, Default: "none — there is no shell tool; a shell call fails cleanly rather than escaping",
		Effect:     "The pinned command image the workspace shell tool runs inside. Mutually exclusive with PALAI_SHELL_NATIVE — a stack runs its shell tool in the sandbox or on the host, never both.",
		Mutability: mutabilityBringUp, ChangeWith: changeCP,
		ReaderFile: cpMain, ReaderFunc: "shellRunnerFromEnv",
	},
	{
		Name: "PALAI_SHELL_NATIVE", Group: "shell", Kind: kindValue, Default: "unset — the sandboxed posture, which is how every existing deployment runs",
		Effect:     "Set to the exact words `unsandboxed-host`, shell commands run on this machine as this uid with NO container boundary, no network denial and no resource bound. The value is a sentence rather than a `1` because deleting a security boundary should not be reachable by the reflex that switches a feature on.",
		Mutability: mutabilityBringUp, ChangeWith: changeCP,
		ReaderFile: cpMain, ReaderFunc: "resolveShellPosture",
	},
	{
		Name: "PALAI_SANDBOX_WALL_TIME", Group: "shell", Kind: kindValue, Default: "10m",
		Effect:     "The wall time ONE shell call runs under. The container posture refuses a non-positive bound.",
		Mutability: mutabilityBringUp, ChangeWith: changeDesired,
		ReaderFile: cpMain, ReaderFunc: "sandboxWallTime",
		DesiredValue: desiredDuration,
	},

	// --- what the deployment keeps, and for how long -------------------------------------------------
	{
		Name: "PALAI_S3_ENDPOINT", Group: "storage", Kind: kindValue, Default: "none — NO object store: artifact retrieval, the checkpoint and snapshot sinks, retention's byte deletion and the orphan collector are all off",
		Effect:     "The object store the control plane writes artifacts to. Its credential is a separate variable this surface never reports.",
		Mutability: mutabilityBringUp, ChangeWith: changeCP,
		ReaderFile: cpMain, ReaderFunc: "artifactStoreFromEnv",
	},
	{
		Name: "PALAI_S3_BUCKET", Group: "storage", Kind: kindValue, Default: "palai-artifacts",
		Effect:     "The bucket artifacts are written to.",
		Mutability: mutabilityBringUp, ChangeWith: changeCP,
		ReaderFile: cpMain, ReaderFunc: "artifactStoreFromEnv",
	},
	{
		Name: "PALAI_RETENTION_STORE_FALSE_TTL", Group: "storage", Kind: kindValue, Default: "disabled — nothing is reaped, and GET /v1/capabilities advertises a TTL of 0",
		Effect:     "How long a `store:false` response survives before the retention reaper deletes it. Unset starts no reaper at all, so no arbitrary production default is imposed.",
		Mutability: mutabilityBringUp, ChangeWith: changeDesired,
		ReaderFile: cpMain, ReaderFunc: "startRetention",
		DesiredValue: desiredDuration,
	},

	// --- how this process is reached ------------------------------------------------------------------
	{
		Name: "PALAI_LISTEN_ADDR", Group: "network", Kind: kindValue, Default: ":8080",
		Effect:     "The public API listener.",
		Mutability: mutabilityBringUp, ChangeWith: changeCP,
		ReaderFile: cpMain, ReaderFunc: "main",
	},
	{
		Name: "PALAI_RUNNER_LISTEN_ADDR", Group: "network", Kind: kindValue, Default: "none — the runner gateway is NOT bound, so no machine can enrol and no run reaches an engine",
		Effect:     "The mutually-authenticated listener enrolled runners dial. Setting it makes the CA and server-certificate variables below required.",
		Mutability: mutabilityBringUp, ChangeWith: changeCP,
		ReaderFile: cpMain, ReaderFunc: "main",
	},
	{
		Name: "PALAI_PUBLIC_BASE_URL", Group: "network", Kind: kindValue, Default: "none — an A2A push target is not addressable from outside",
		Effect:     "This deployment's externally reachable base URL, used where a peer must be handed an address to call back on.",
		Mutability: mutabilityBringUp, ChangeWith: changeCP,
		ReaderFile: cpMain, ReaderFunc: "main",
	},
	{
		Name: "PALAI_TOOL_CALLBACK_BASE_URL", Group: "network", Kind: kindValue, Default: "none — a remote tool can then only answer synchronously",
		Effect:     "The base URL a remote tool's asynchronous 202 result is posted back to.",
		Mutability: mutabilityBringUp, ChangeWith: changeCP,
		ReaderFile: cpMain, ReaderFunc: "startDispatch",
	},

	// --- the edge's bounds ----------------------------------------------------------------------------
	{
		Name: "PALAI_REQUEST_RATE_PER_SEC", Group: "edge", Kind: kindValue, Default: "unbounded",
		Effect: "Sustained request rate the API edge admits per key.", Mutability: mutabilityBringUp, ChangeWith: changeDesired,
		ReaderFile: cpMain, ReaderFunc: "edgeLimitsFromEnv",
		DesiredValue: desiredRate,
	},
	{
		Name: "PALAI_REQUEST_BURST", Group: "edge", Kind: kindValue, Default: "unbounded",
		Effect: "Burst allowance above the sustained rate.", Mutability: mutabilityBringUp, ChangeWith: changeDesired,
		ReaderFile: cpMain, ReaderFunc: "edgeLimitsFromEnv",
		DesiredValue: desiredInt,
	},
	{
		Name: "PALAI_MAX_CONCURRENT_RUNS", Group: "edge", Kind: kindValue, Default: "unbounded",
		Effect: "How many runs one tenant may have in flight before admission refuses.", Mutability: mutabilityBringUp, ChangeWith: changeDesired,
		ReaderFile: cpMain, ReaderFunc: "edgeLimitsFromEnv",
		DesiredValue: desiredInt,
	},
	{
		Name: "PALAI_MAX_QUEUED_RUNS", Group: "edge", Kind: kindValue, Default: "unbounded",
		Effect: "How many runs one tenant may have queued before admission refuses.", Mutability: mutabilityBringUp, ChangeWith: changeDesired,
		ReaderFile: cpMain, ReaderFunc: "edgeLimitsFromEnv",
		DesiredValue: desiredInt,
	},

	// --- the handles this process holds. PATHS, NEVER VALUES ------------------------------------------
	{
		Name: "PALAI_SECRET_MASTER_KEY_FILE", Group: "identity", Kind: kindPath, Default: "unset — the DB-backed secret store is DISABLED, the secret-ref routes stay unmounted, and every resolver is env-file-only",
		Effect:     "The file holding the 32-byte hex master key the envelope-encrypted secret store redeems through. A set-but-unreadable file is fatal at boot rather than a silent downgrade. This surface reports the PATH; the key never leaves the process.",
		Mutability: mutabilityBringUp,
		ChangeWith: "a stored secret is rotated live through POST /v1/secret-refs and needs no restart; changing the MASTER KEY's location needs " + changeCP,
		ReaderFile: cpMain, ReaderFunc: "main",
	},
	{
		Name: "PALAI_BOOTSTRAP_API_KEY_FILE", Group: "identity", Kind: kindPath, Default: "unset — no bootstrap identity is seeded",
		Effect:     "The file holding the first admin API key, seeded once at boot. The value is a path; the key itself is never read back by any route.",
		Mutability: mutabilityBringUp, ChangeWith: changeCP,
		ReaderFile: cpMain, ReaderFunc: "main",
	},
	{
		Name: "PALAI_RUNNER_CA_CERT", Group: "fleet", Kind: kindPath, Default: "required once the runner listener is bound; the process refuses to start without it",
		Effect: "The CA certificate the runner gateway presents and verifies enrolled machines against.", Mutability: mutabilityBringUp, ChangeWith: changeCP,
		ReaderFile: cpMain, ReaderFunc: "startRunnerGateway",
	},
	{
		Name: "PALAI_RUNNER_CA_KEY", Group: "fleet", Kind: kindPath, Default: "required once the runner listener is bound; the process refuses to start without it",
		Effect: "The CA private key runner certificates are issued from. A PATH — the key material is never returned by any route.", Mutability: mutabilityBringUp, ChangeWith: changeCP,
		ReaderFile: cpMain, ReaderFunc: "startRunnerGateway",
	},
	{
		Name: "PALAI_RUNNER_SERVER_CERT", Group: "fleet", Kind: kindPath, Default: "required once the runner listener is bound",
		Effect: "The gateway listener's own certificate. Its SANs decide which addresses a runner may dial.", Mutability: mutabilityBringUp, ChangeWith: changeCP,
		ReaderFile: cpMain, ReaderFunc: "startRunnerGateway",
	},
	{
		Name: "PALAI_RUNNER_SERVER_KEY", Group: "fleet", Kind: kindPath, Default: "required once the runner listener is bound",
		Effect: "The gateway listener's private key. A PATH, never the key.", Mutability: mutabilityBringUp, ChangeWith: changeCP,
		ReaderFile: cpMain, ReaderFunc: "startRunnerGateway",
	},
	{
		Name: "PALAI_ENROLLMENT_TOKEN_FILE", Group: "fleet", Kind: kindPath, Default: "required once the runner listener is bound",
		Effect:     "The bootstrap enrolment token a machine spends to join, and re-presents only when its identity expired before renewal could roll it forward.",
		Mutability: mutabilityBringUp, ChangeWith: changeCP,
		ReaderFile: cpMain, ReaderFunc: "startRunnerGateway",
	},
	{
		Name: "PALAI_RUNNER_CERT_TTL", Group: "fleet", Kind: kindValue, Default: "5m",
		Effect: "The lifetime of an issued runner certificate; a short value makes a runner renew mid-session.", Mutability: mutabilityBringUp, ChangeWith: changeDesired,
		ReaderFile: cpMain, ReaderFunc: "startRunnerGateway",
		DesiredValue: desiredDuration,
	},

	// --- what this deployment is connected to ---------------------------------------------------------
	{
		Name: "PALAI_SLACK_SOCKET_TEAM_ID", Group: "integration", Kind: kindValue, Default: "unset — Socket Mode is DORMANT, so a registered workspace can never receive an @mention",
		Effect:     "The Slack workspace this deployment holds an outbound Socket Mode connection to. A workspace ID, not a credential.",
		Mutability: mutabilityBringUp, ChangeWith: changeCP,
		ReaderFile: cpMain, ReaderFunc: "startSlackSocket",
	},
	{
		Name: "PALAI_GITHUB_APP_ID", Group: "integration", Kind: kindValue, Default: "unset — the LOCAL repository broker is used, which reaches public repos and local remotes only",
		Effect:     "The GitHub App this deployment mints repository read credentials from. All three App variables must be set together; any one missing falls back to the local broker.",
		Mutability: mutabilityBringUp, ChangeWith: changeCP,
		ReaderFile: cpMain, ReaderFunc: "repositoryBrokerFromEnv",
	},
	{
		Name: "PALAI_GITHUB_APP_INSTALLATION_ID", Group: "integration", Kind: kindValue, Default: "unset — see PALAI_GITHUB_APP_ID",
		Effect: "The App installation the credential is minted for.", Mutability: mutabilityBringUp, ChangeWith: changeCP,
		ReaderFile: cpMain, ReaderFunc: "repositoryBrokerFromEnv",
	},
	{
		Name: "PALAI_GITHUB_APP_PRIVATE_KEY_FILE", Group: "integration", Kind: kindPath, Default: "unset — see PALAI_GITHUB_APP_ID",
		Effect: "The file holding the App's private key. A PATH; the PEM is never returned.", Mutability: mutabilityBringUp, ChangeWith: changeCP,
		ReaderFile: cpMain, ReaderFunc: "repositoryBrokerFromEnv",
	},
	{
		Name: "PALAI_GITHUB_REPO", Group: "integration", Kind: kindValue, Default: "unset — the App credential is not narrowed to one repository",
		Effect: "The owner/name slug the minted App credential is scoped to.", Mutability: mutabilityBringUp, ChangeWith: changeCP,
		ReaderFile: cpMain, ReaderFunc: "repositoryBrokerFromEnv",
	},

	// --- what this process says it is -----------------------------------------------------------------
	{
		Name: "PALAI_VERSION", Group: "version", Kind: kindValue, Default: "the version stamp baked into this image at build time",
		Effect:     "Overrides the baked release stamp, which decides the §48.2 support window and what an upgrade records as applied_by.",
		Mutability: mutabilityBringUp, ChangeWith: changeAll,
		ReaderFile: "packages/version/version.go", ReaderFunc: "Resolve",
	},
}

// unreportedSettings names every variable deploy/compose/compose.yaml SETS that this surface deliberately
// does not report, with the reason. Adding a name here is the auditable act; the DEFAULT for a new compose
// variable is that TestEveryComposeSettingIsCataloguedOrDeclaredUnreported fails until somebody decides.
//
// The two reasons are the only two there are: the value is a credential, or the value is not this
// process's to report.
var unreportedSettings = map[string]string{
	"PALAI_RUNNER_CONCURRENCY": "runner-scoped: it is set on the RUNNER container and this process's copy is unset. " +
		"cmd/cli/internal/stack/upgrade.go already records what happens to a reader that forgets — \"reading a runner-scoped var off " +
		"the control-plane container always misses it\" — and a confident wrong answer is worse than the absence. " +
		"The fleet's live lease count (GET /v1/runners) is what this deployment can observe about a machine's capacity.",
	"PALAI_CONTROLLER_URL":  "runner-scoped: the address the RUNNER dials this control plane on. This process holds no copy.",
	"PALAI_CONTROLLER_DNS":  "runner-scoped: the SAN the RUNNER pins its gateway connection to. This process holds no copy.",
	"PALAI_COMPOSE_PROJECT": "runner-scoped: the compose project label the RUNNER tags engine sandboxes with. This process holds no copy.",
}

// nonDesiredReason names every catalogued setting the panel may NOT write, with the reason. It is the
// mirror of unreportedSettings and it exists for the same reason: adding a name is the auditable act, and
// TestEverySettingIsEitherWritableOrHasAStatedRefusal fails until somebody decides.
//
// THE RULE THE TWENTY-FOUR REFUSALS ARE INSTANCES OF: the panel writes OPERATIONAL BOUNDS and ROUTING
// DEFAULTS. It does not write IDENTITY, IMAGES, the ADDRESS this process is reached on, or a DESTINATION a
// credential is sent to. Every entry below is one of those four, plus the paths — which are refused
// STRUCTURALLY rather than by this map (desiredWritable drops Kind == kindPath before it ever reads a name)
// and are listed anyway, because a reader asking "why can I not set the master key path here" deserves the
// sentence rather than an inference from a missing row.
var nonDesiredReason = map[string]string{
	// --- filesystem handles. desiredWritable REFUSES THESE BY KIND; the sentences are for the reader. ----
	"PALAI_WORKSPACE_ROOT": "a path. Naming the host directory every coding workspace is allocated under, from a web form, " +
		"is a filesystem write primitive wearing a settings control.",
	"PALAI_SECRET_MASTER_KEY_FILE": "a path, and the sharpest one: it names the file the ENTIRE secret store redeems through. " +
		"Moving it from a form points the store at a file the operator chose and the process reads at boot with no further question.",
	"PALAI_BOOTSTRAP_API_KEY_FILE": "a path, and the file it names holds the first admin key. It is seeded once at boot; " +
		"re-pointing it is minting an identity, not configuring a machine.",
	"PALAI_RUNNER_CA_CERT":              "a path to the fleet's trust root.",
	"PALAI_RUNNER_CA_KEY":               "a path to the private key every runner certificate is issued from.",
	"PALAI_RUNNER_SERVER_CERT":          "a path to the gateway listener's certificate; its SANs decide which addresses a runner may dial.",
	"PALAI_RUNNER_SERVER_KEY":           "a path to the gateway listener's private key.",
	"PALAI_ENROLLMENT_TOKEN_FILE":       "a path to the credential a machine spends to join the fleet.",
	"PALAI_GITHUB_APP_PRIVATE_KEY_FILE": "a path to the App's PEM.",

	// --- images: what CODE runs on this machine ------------------------------------------------------
	"PALAI_ENGINE_IMAGE": "an IMAGE REFERENCE. The control plane pins it into every lease and the runner starts it — so a form " +
		"that wrote it would be arbitrary container execution on the fleet, reached through a settings screen. It is pinned by the " +
		"release the operator installed and it changes when they upgrade.",
	"PALAI_SANDBOX_IMAGE": "an IMAGE REFERENCE, for the same reason: it is the container every workspace shell call runs inside, " +
		"with the workspace mounted. Choosing it is a supply-chain decision made at install time, not a setting.",

	// --- a security boundary -------------------------------------------------------------------------
	"PALAI_SHELL_NATIVE": "it DELETES a security boundary — the exact words `unsandboxed-host` run every shell command on this " +
		"machine as this uid with no container, no network denial and no resource bound. deployment.go's own entry says the value is a " +
		"sentence rather than a `1` so that removing the boundary is not reachable by the reflex that switches a feature on; putting it " +
		"behind a form would hand that reflex back.",

	// --- destinations a credential is sent to --------------------------------------------------------
	"PALAI_S3_ENDPOINT": "a destination this process sends its OBJECT-STORE CREDENTIAL to. PALAI_S3_ACCESS_KEY/SECRET_KEY are " +
		"variables this surface never reports and never writes, but they are sent to whatever this names — so a written endpoint is " +
		"credential exfiltration with the artifacts as a bonus.",
	"PALAI_S3_BUCKET": "the identity of the store every artifact already written lives in. Changing it does not move them; it makes " +
		"them unreachable while the surface reports success.",

	// --- the address this process is reached on ------------------------------------------------------
	"PALAI_LISTEN_ADDR": "the address this API is reached on — including by the panel doing the writing. A machine that changes " +
		"where it listens, from the surface that reaches it, cannot be reached to change it back.",
	"PALAI_RUNNER_LISTEN_ADDR": "the same, for the fleet: unbinding it strands every enrolled machine, and binding it makes four " +
		"path-valued variables suddenly required — a coupling a single-field form cannot express.",

	// --- an address handed to a peer to call back on --------------------------------------------------
	"PALAI_PUBLIC_BASE_URL": "the address peers are handed to call this deployment back on. A written value redirects a callback " +
		"leg rather than configuring one.",
	"PALAI_TOOL_CALLBACK_BASE_URL": "the address a remote tool POSTs its asynchronous result to. Same shape: writing it moves " +
		"where results land.",

	// --- half of a credential triple -----------------------------------------------------------------
	"PALAI_GITHUB_APP_ID": "one of THREE variables that must be set together, and the third is a path this surface will never write. " +
		"Writing two of three falls back to the local repository broker with nothing on any screen able to say why.",
	"PALAI_GITHUB_APP_INSTALLATION_ID": "see PALAI_GITHUB_APP_ID.",
	"PALAI_GITHUB_REPO":                "it narrows a credential this surface cannot mint; on its own it configures nothing.",

	// --- a binding whose other half is a credential ---------------------------------------------------
	"PALAI_SLACK_SOCKET_TEAM_ID": "the workspace half of a pair whose other half is an app-level TOKEN in the secret store. Written " +
		"alone it points Socket Mode at a workspace this deployment holds no token for, and the failure is a connect loop that never " +
		"says which of the two is wrong. A workspace is registered through POST /v1/slack-connections, which takes both.",

	// --- it has a strictly better live write-path ------------------------------------------------------
	"PALAI_OPENAI_COMPATIBLE_BASE_URL": "superseded by a LIVE write-path that is strictly better: migration 000051 gave " +
		"model_connections its own base_url, vetted through packages/egress and resolved per request, so an endpoint is a property of a " +
		"connection rather than of the machine. Writing the deployment-wide one here would re-create the limitation 000051 removed — one " +
		"custom endpoint per stack — and cost a bring-up to change.",

	// --- what this process says it is ------------------------------------------------------------------
	"PALAI_VERSION": "the release stamp, which decides the §48.2 support window and what an upgrade records as applied_by. It is " +
		"baked into the image at build time; a form that overrode it would let a machine misreport which build is running, and the " +
		"support window is computed from the claim.",
}

// desiredTokenPattern is the accepted shape of a `token`-grammar value: the characters a provider selector
// or a model id is made of, and NOTHING that could act on the way to the process.
//
// IT IS AN ALLOW-LIST OF CHARACTERS, and that is deliberate rather than a denylist of dangerous ones. The
// value's journey is: JSON body -> a JSONB document -> `palai up` -> os.Setenv -> a `docker compose`
// process environment -> ${VAR} interpolation into a YAML scalar in compose.yaml. A newline in that last
// step is a YAML structure edit; a `$` is a second round of interpolation. Neither is expressible below,
// and neither is expressible in an integer or a duration either — which is why nine of the eleven writable
// settings have no free text in them at all.
var desiredTokenPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:/-]{0,199}$`)

// desiredWritable is THE allow-list, computed from the catalogue, and it is the structural half of
// requirement 3.
//
// TWO CONDITIONS, AND THE SECOND ONE IS NOT REDUNDANT. A setting is writable when it declares a value
// grammar AND its kind is not a path. The kind test cannot be satisfied by editing an entry: flipping
// PALAI_SECRET_MASTER_KEY_FILE to `DesiredValue: desiredToken` still yields a read-only setting, because the
// filter drops it by KIND and never consults the grammar. That is what "structural rather than a validation
// string" means here — the refusal is in the code that builds the set, not in a message printed after
// something reached the set.
//
// The list is rebuilt on every call rather than cached in a package var, because a cached copy is a second
// source of truth for exactly the question this function exists to answer.
func desiredWritable() map[string]catalogueEntry {
	out := map[string]catalogueEntry{}
	for _, entry := range deploymentCatalogue {
		if entry.Kind == kindPath || entry.DesiredValue == "" {
			continue
		}
		out[entry.Name] = entry
	}
	return out
}

// DesiredWritableSettings is the writable set's NAMES, for the guards that live outside this package —
// deploy/compose's "does the shipped compose file actually pass this variable?" walk, and the composition
// root's round-trip through the real readers. It is exported for those two and has no production caller:
// production reads desiredWritable() above, which carries the entries.
func DesiredWritableSettings() []string {
	names := make([]string, 0, len(deploymentCatalogue))
	for _, entry := range deploymentCatalogue {
		if _, ok := desiredWritable()[entry.Name]; ok {
			names = append(names, entry.Name)
		}
	}
	return names
}

// DispatchWorkers is the ONE reading of PALAI_DISPATCH_WORKERS in this binary.
//
// main.dispatchWorkerCount delegates to it, and startDispatch's early return is `<= 0`, so the number
// reported by GET /v1/deployment and the number that decides whether anything executes cannot drift apart.
// A second copy of the parsing rules here would be the defect that made this surface necessary, one layer
// up: a screen confidently reporting a default the code does not use.
func DispatchWorkers() int {
	raw := os.Getenv("PALAI_DISPATCH_WORKERS")
	if raw == "" {
		return 1
	}
	n, err := strconv.Atoi(raw)
	if err != nil {
		return 1
	}
	return n
}

// deployment serves the effective configuration. It is gated on the `provision` capability — the same
// authority the tenancy surface requires — because this is an OPERATOR read: it names the object store,
// the sandbox image, the GitHub App and every path the process holds open. A narrow project-scoped key
// handed to a run has no business enumerating the machine it is running on.
func deployment(desired DesiredConfigAPI) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		scope, ok := middleware.ScopeFrom(r.Context())
		if !ok {
			middleware.WriteProblem(w, r, http.StatusUnauthorized, "authentication_required", "a bearer API key is required")
			return
		}
		if !scope.HasScope(provisionScope) {
			middleware.WriteProblem(w, r, http.StatusForbidden, "insufficient_scope", "this API key lacks the provision capability")
			return
		}

		// The desired document, when this deployment has somewhere to keep one. A read failure is FATAL to
		// the response rather than degraded to "no document": the difference between "nobody has written
		// one" and "we could not read the one that exists" is the difference between a machine running on
		// its compose defaults and a machine whose operator's decision is invisible, and a screen shown the
		// first when the second is true would report no pending bring-up while one is pending.
		var doc *DesiredDocument
		if desired != nil {
			var err error
			if doc, err = desired.GetDesiredConfig(r.Context(), scope); err != nil {
				middleware.WriteProblem(w, r, http.StatusInternalServerError, "internal_error",
					"the effective configuration was read but the desired configuration could not be, so this response cannot say whether a bring-up is pending")
				return
			}
		}
		writable := desiredWritable()

		body := deploymentBody{Object: "deployment", Settings: make([]deploymentSetting, 0, len(deploymentCatalogue))}
		for _, entry := range deploymentCatalogue {
			raw, set := os.LookupEnv(entry.Name)
			_, isWritable := writable[entry.Name]
			row := deploymentSetting{
				Name: entry.Name, Group: entry.Group, Value: reportedValue(raw), Set: set && raw != "",
				Default: entry.Default, Kind: entry.Kind, Effect: entry.Effect,
				Mutability: entry.Mutability, ChangeWith: entry.ChangeWith,
				ReaderFile: entry.ReaderFile, ReaderFunc: entry.ReaderFunc,
				Writable: isWritable,
			}
			if !isWritable {
				row.NotWritableBecause = nonDesiredReason[entry.Name]
			}
			if doc != nil {
				// The desired value is compared against the RAW environment, not against `row.Value` — which
				// is reportedValue()'s redacted rendering. Comparing the redaction would report drift on any
				// value carrying userinfo forever, because the redacted form can never equal what was written.
				row.Desired, row.DesiredSet = doc.Settings[entry.Name]
				row.Drift = row.DesiredSet && row.Desired != raw
			}
			body.Settings = append(body.Settings, row)
		}
		body.Warnings = deploymentWarnings()
		if doc != nil {
			drifted := desiredDrift(doc, os.Getenv)
			body.Desired = &desiredView{
				Revision: doc.Revision, WrittenAt: doc.WrittenAt, WrittenBy: doc.WrittenBy,
				Pending: len(drifted) > 0, Drifted: drifted,
			}
			if len(drifted) > 0 {
				body.Warnings = append(body.Warnings, deploymentWarning{
					Code: warnDesiredPending, Severity: severityAdvisory,
					Headline: "This machine is not running the configuration that was saved for it.",
					Detail: "Desired revision " + strconv.FormatInt(doc.Revision, 10) + " differs from what this process holds for " +
						strconv.Itoa(len(drifted)) + " setting(s): " + strings.Join(drifted, ", ") + ". Every one of them is read from the " +
						"process environment, which is fixed at exec, so the saved value takes effect when the process is replaced with it — not before.",
					Remedy:   "Run `palai up` on the machine. The bring-up reads this document and exports it into the environment it starts the control plane with.",
					Settings: drifted,
				})
			}
		}

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(body)
	}
}

// deploymentWarnings reports the configured values that change what the product DOES in a way a screen
// showing the product would otherwise misrepresent. It is derived from the SAME readers production uses,
// never from the catalogue's prose.
func deploymentWarnings() []deploymentWarning {
	out := []deploymentWarning{}
	if DispatchWorkers() <= 0 {
		out = append(out, deploymentWarning{
			Code: warnDispatchOff, Severity: severityBlocking,
			Headline: "This deployment has no dispatcher — submitted runs are accepted and never executed.",
			Detail: "PALAI_DISPATCH_WORKERS is " + strconv.Itoa(DispatchWorkers()) + ", so startDispatch returns before it builds a worker. " +
				"POST /v1/responses still returns 201 and the run stays at run.queued.v1 forever. The reconciler, the dead-letter sweep, " +
				"approval expiry and capacity-park expiry are off with it, so nothing will time the run out either.",
			Remedy: "Recreate the control-plane with PALAI_DISPATCH_WORKERS=1 or higher. `make local-up` and deploy/compose/compose.yaml " +
				"default it to 0; the production overlay defaults it to 1.",
			Settings: []string{"PALAI_DISPATCH_WORKERS"},
		})
	}
	if !liveModelProviderConfigured() {
		out = append(out, deploymentWarning{
			Code: warnModelFake, Severity: severityAdvisory,
			Headline: "Every run without a published model route is answered by the deterministic fake adapter.",
			Detail: "The deployment default route is chosen by an exact match on `provider-one`; PALAI_MODEL_PROVIDER is " +
				quotedOrUnset(os.Getenv("PALAI_MODEL_PROVIDER")) + ", so the fallback is the fake adapter. Its output is fabricated and " +
				"renders exactly like a real answer — a misspelled provider name lands here silently.",
			Remedy: "A project that publishes a model route (POST /v1/model-routes) dispatches through that instead, with no restart. " +
				"To change the fallback, recreate the control-plane with PALAI_MODEL_PROVIDER=provider-one and a credential configured.",
			Settings: []string{"PALAI_MODEL_PROVIDER", "PALAI_MODEL"},
		})
	}
	return out
}

// liveModelProviderConfigured mirrors modelBrokerFromEnv's branch EXACTLY, including the fact that it is an
// equality rather than a non-emptiness test: any value other than `provider-one` returns the fake route.
func liveModelProviderConfigured() bool {
	return os.Getenv("PALAI_MODEL_PROVIDER") == "provider-one"
}

func quotedOrUnset(v string) string {
	if v == "" {
		return "unset"
	}
	return strconv.Quote(v)
}

// reportedValue strips a URL's userinfo before the value is published. No catalogued variable is supposed
// to carry a credential — PALAI_S3_ENDPOINT's key is a separate variable this surface never reports — but
// "supposed to" is not a property a response body should rest on, and an operator who pasted
// `https://user:pass@…` into an endpoint should not learn about it from a console screenshot.
func reportedValue(raw string) string {
	if !strings.Contains(raw, "://") {
		return raw
	}
	u, err := url.Parse(raw)
	if err != nil || u.User == nil {
		return raw
	}
	u.User = url.User("redacted")
	return u.String()
}
