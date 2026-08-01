package api

import (
	"encoding/json"
	"net/http"
	"net/url"
	"os"
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

const (
	warnDispatchOff = "dispatch_workers_zero"
	warnModelFake   = "model_provider_fake"

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
}

const (
	cpMain    = "apps/control-plane/cmd/palai-control-plane/main.go"
	changeCP  = "recreate the control-plane with the new value (`palai up`, or `docker compose up -d --force-recreate control-plane`)"
	changeAll = "recreate the whole stack with the new value (`palai up`)"
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
		Mutability: mutabilityBringUp, ChangeWith: changeCP,
		// The reader is this file's own DispatchWorkers, and main.dispatchWorkerCount delegates to it, so
		// the number this surface reports and the number startDispatch gates on are the same number.
		ReaderFile: "apps/control-plane/api/deployment.go", ReaderFunc: "DispatchWorkers",
	},
	{
		Name: "PALAI_MODEL_PROVIDER", Group: "model", Kind: kindValue, Default: "fake",
		Effect:     "The DEPLOYMENT-DEFAULT model route. Exactly one value — `provider-one` — selects a live provider; every other value, including a provider name this binary can otherwise speak to and including a typo, falls through to the deterministic fake adapter, whose answers are fabricated and render exactly like real ones.",
		Mutability: mutabilityDefaultOnly,
		ChangeWith: "for ONE project, publish a model route (POST /v1/model-routes) — it is resolved per attempt and overrides this with no restart; to change the fallback every project without a route gets, " + changeCP,
		ReaderFile: cpMain, ReaderFunc: "modelBrokerFromEnv",
	},
	{
		Name: "PALAI_MODEL", Group: "model", Kind: kindValue, Default: "gpt-4o-mini (only when PALAI_MODEL_PROVIDER=provider-one; otherwise the fake adapter's `fake`)",
		Effect:     "The model id the deployment-default route names. Read only on the `provider-one` branch — with any other provider it is inert.",
		Mutability: mutabilityDefaultOnly,
		ChangeWith: "a published project model route names its own model; otherwise " + changeCP,
		ReaderFile: cpMain, ReaderFunc: "modelBrokerFromEnv",
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
		Mutability: mutabilityBringUp, ChangeWith: changeCP,
		ReaderFile: cpMain, ReaderFunc: "startDispatch",
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
		Mutability: mutabilityBringUp, ChangeWith: changeCP,
		ReaderFile: cpMain, ReaderFunc: "sandboxWallTime",
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
		Mutability: mutabilityBringUp, ChangeWith: changeCP,
		ReaderFile: cpMain, ReaderFunc: "startRetention",
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
		Effect: "Sustained request rate the API edge admits per key.", Mutability: mutabilityBringUp, ChangeWith: changeCP,
		ReaderFile: cpMain, ReaderFunc: "edgeLimitsFromEnv",
	},
	{
		Name: "PALAI_REQUEST_BURST", Group: "edge", Kind: kindValue, Default: "unbounded",
		Effect: "Burst allowance above the sustained rate.", Mutability: mutabilityBringUp, ChangeWith: changeCP,
		ReaderFile: cpMain, ReaderFunc: "edgeLimitsFromEnv",
	},
	{
		Name: "PALAI_MAX_CONCURRENT_RUNS", Group: "edge", Kind: kindValue, Default: "unbounded",
		Effect: "How many runs one tenant may have in flight before admission refuses.", Mutability: mutabilityBringUp, ChangeWith: changeCP,
		ReaderFile: cpMain, ReaderFunc: "edgeLimitsFromEnv",
	},
	{
		Name: "PALAI_MAX_QUEUED_RUNS", Group: "edge", Kind: kindValue, Default: "unbounded",
		Effect: "How many runs one tenant may have queued before admission refuses.", Mutability: mutabilityBringUp, ChangeWith: changeCP,
		ReaderFile: cpMain, ReaderFunc: "edgeLimitsFromEnv",
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
		Effect: "The lifetime of an issued runner certificate; a short value makes a runner renew mid-session.", Mutability: mutabilityBringUp, ChangeWith: changeCP,
		ReaderFile: cpMain, ReaderFunc: "startRunnerGateway",
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
func deployment(w http.ResponseWriter, r *http.Request) {
	scope, ok := middleware.ScopeFrom(r.Context())
	if !ok {
		middleware.WriteProblem(w, r, http.StatusUnauthorized, "authentication_required", "a bearer API key is required")
		return
	}
	if !scope.HasScope(provisionScope) {
		middleware.WriteProblem(w, r, http.StatusForbidden, "insufficient_scope", "this API key lacks the provision capability")
		return
	}

	body := deploymentBody{Object: "deployment", Settings: make([]deploymentSetting, 0, len(deploymentCatalogue))}
	for _, entry := range deploymentCatalogue {
		raw, set := os.LookupEnv(entry.Name)
		body.Settings = append(body.Settings, deploymentSetting{
			Name: entry.Name, Group: entry.Group, Value: reportedValue(raw), Set: set && raw != "",
			Default: entry.Default, Kind: entry.Kind, Effect: entry.Effect,
			Mutability: entry.Mutability, ChangeWith: entry.ChangeWith,
			ReaderFile: entry.ReaderFile, ReaderFunc: entry.ReaderFunc,
		})
	}
	body.Warnings = deploymentWarnings()

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(body)
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
