// Package admin is the `palai org|project|apikey|secret|poolkey|runner` admin CLI: a thin authenticated
// HTTP client over the E13 provisioning + secret-ref APIs (router.go:152-187), the E24 runner-pool key
// surface and the E24 T5 machine lifecycle. It adds NO server surface — every subcommand maps to one
// existing endpoint. Until the E17 console it is the only human interface for tenancy admin (spec §47.6),
// and for the fleet it is the only one there will be for now: the console has no authentication at all,
// so minting a pool key or decommissioning a Mac behind it would undo what E24 builds (§0.2).
//
// CREDENTIAL HYGIENE (the load-bearing rule): the admin API key and any secret VALUE never ride argv. The
// key comes from --api-key-file (a path, not the value), $PALAI_API_KEY, or the initialised .palai stack;
// a secret value comes only from stdin. TWO places write a secret to stdout, both of them a create-only
// field the API returns exactly once and the CLI prints without retaining: an `apikey create` response's
// plaintext key, and a `poolkey create` response's runner-pool enrolment key.
package admin

import (
	"bytes"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/palgroup/palai/cmd/cli/internal/stack"
)

// Client speaks the admin API over one resolved base URL + bearer key. Out/In are the IO streams (os.Stdout
// and os.Stdin in main, buffers in tests); JSON toggles the raw-vs-human rendering.
type Client struct {
	BaseURL string
	APIKey  string
	HTTP    *http.Client
	Out     io.Writer
	In      io.Reader
	JSON    bool
}

// Run parses a resource's subcommand + flags, resolves the connection over the flag → env → .palai chain,
// and executes the request against Out/In. It is the entry point main dispatches the four admin cases to.
func Run(resource string, args []string, out io.Writer, in io.Reader) error {
	sub, f, pos, err := parse(resource, args)
	if err != nil {
		return err
	}
	baseURL, apiKey, err := resolve(f.baseURL, f.apiKeyFile)
	if err != nil {
		return err
	}
	httpClient, err := newHTTPClient(f.ca)
	if err != nil {
		return err
	}
	c := &Client{
		BaseURL: baseURL,
		APIKey:  apiKey,
		HTTP:    httpClient,
		Out:     out,
		In:      in,
		JSON:    f.json,
	}
	return c.execute(resource, sub, pos, f)
}

// flags holds every flag the four resources may register. Registration is per-resource, so an irrelevant
// flag on the wrong resource is a parse error (a typo guard), while the value fields stay in one struct.
type flags struct {
	baseURL       string
	apiKeyFile    string
	ca            string
	json          bool
	displayName   string
	project       string
	scopes        multiFlag
	expiresAt     string
	allowedModels string
	allowedTools  string
	defaultTools  string
	approvers     string
	name          string
	pool          string
	posture       string
	osName        string
	arch          string
	strict        bool
	// The E29 model-wiring flags. Every one is a PUBLIC fact — a family name, a secret REF name, a URL, a
	// model id. None can carry a credential, and admin_test.go's TestModelFlagsCarryNoCredential is what
	// keeps the next one from doing so.
	provider    string
	secretRef   string
	connBaseURL string
	connection  string
	model       string
	routeName   string
}

// multiFlag collects a repeatable string flag (--scope run --scope provision).
type multiFlag []string

func (m *multiFlag) String() string     { return strings.Join(*m, ",") }
func (m *multiFlag) Set(v string) error { *m = append(*m, v); return nil }

// register defines the global flags plus the resource-specific ones. NOTE: there is deliberately no flag
// carrying a secret value — a secret value only ever comes from stdin.
func (f *flags) register(fs *flag.FlagSet, resource string) {
	fs.StringVar(&f.baseURL, "base-url", "", "control-plane base URL (else $PALAI_BASE_URL, then .palai)")
	fs.StringVar(&f.apiKeyFile, "api-key-file", "", "file holding the admin API key (else $PALAI_API_KEY, then .palai)")
	fs.StringVar(&f.ca, "ca", "", "PEM CA file to trust for an https base URL (else $PALAI_CA_FILE) — e.g. the self-signed edge's ${PALAI_HOME}/ca/ca.crt")
	fs.BoolVar(&f.json, "json", false, "emit the raw JSON response instead of a human render")
	switch resource {
	case "project":
		fs.StringVar(&f.displayName, "display-name", "", "project display name (create)")
		fs.StringVar(&f.allowedModels, "allowed-models", "", "comma-separated allowed models (set-policy)")
		fs.StringVar(&f.allowedTools, "allowed-tools", "", "comma-separated allowed tools (set-policy)")
		fs.StringVar(&f.defaultTools, "default-tools", "", "comma-separated default tools (set-policy)")
		fs.StringVar(&f.approvers, "approvers", "", "comma-separated approver principals, slack:<team>:<user> or key:<api_key_id> (set-policy)")
		// The runner pool this project's runs are placed in (E24 T2). MEASURED IN T6 WHILE WRITING THE
		// OPERATOR PAGE: the field shipped on the API and there was no flag for it, so the epic's central
		// knob could only be set with a raw PATCH — the same half-shipped shape T2 found on the endpoint
		// itself, one layer up. `palai admin runner list` prints the pool ids to choose from.
		fs.StringVar(&f.pool, "pool", "", "runner pool id this project's runs are placed in (set-policy)")
	case "apikey":
		fs.StringVar(&f.project, "project", "", "project id the key belongs to (create)")
		fs.Var(&f.scopes, "scope", "capability scope (repeatable; omit for a full-capability admin key)")
		fs.StringVar(&f.expiresAt, "expires-at", "", "RFC3339 expiry, optional (create)")
	case "secret":
		fs.StringVar(&f.name, "name", "", "secret name (create) — the VALUE is read from stdin")
	case "pool":
		// THE POOL ITSELF (E28 T1), and every flag here is a public fact about a machine's shape. There is
		// nothing a credential could ride: a pool has no secret — its enrolment key is a separate row and the
		// `poolkey` resource below.
		fs.StringVar(&f.name, "name", "", "pool name, unique within the project (create)")
		fs.StringVar(&f.posture, "posture", "", "sandboxed-linux (a container runner) or unsandboxed-host (a rented Mac) — fixed at creation, because machines INHERIT it (create)")
		fs.StringVar(&f.osName, "os", "", "the OS machines in this pool report, e.g. darwin (create, optional)")
		fs.StringVar(&f.arch, "arch", "", "the architecture machines in this pool report, e.g. arm64 (create, optional)")
		fs.BoolVar(&f.strict, "strict", false, "enrolling into this pool needs a human approval (create, set-strict) — omit on set-strict to close the waiting room")
	case "poolkey":
		// There is deliberately no flag carrying a key value: a pool key only ever comes OUT of `create`,
		// and everything else names it by id. Nothing here can put a credential in argv.
		fs.StringVar(&f.pool, "pool", "", "runner pool id (required for create; narrows list)")
		fs.StringVar(&f.expiresAt, "expires-at", "", "RFC3339 expiry, optional (create) — omitted means the key never expires")
	case "runner":
		// No flags at all. Every subcommand either takes no argument (`list`) or names ONE machine by the
		// id the server minted, so there is nothing to configure and nothing that could carry a credential.
	case "model":
		// THE PROVIDER WIRING (E29). There is deliberately NO flag carrying a provider key, for exactly the
		// reason `secret` has none: a value in argv is a value in the shell history and in every `ps` on the
		// box. The key is written with `palai secret create` (stdin) and named here by its REF.
		fs.StringVar(&f.provider, "provider", "", "provider family: provider-one (OpenAI), provider-two (Anthropic) or openai-compatible (your own endpoint) — create")
		fs.StringVar(&f.secretRef, "secret-ref", "", "the name of a secret written with `palai secret create` (create) — the REF, never the value")
		// `--endpoint`, NOT `--base-url`: the global flag registered at the top of this function is already
		// called --base-url and it means the CONTROL PLANE's address. Two different addresses under one
		// spelling is how an operator points a connection at their own control plane; the flag package
		// happens to panic on the collision, which is a kinder failure than the one that was available.
		fs.StringVar(&f.connBaseURL, "endpoint", "", "the chat-completions URL for the openai-compatible family (create) — REQUIRED by it, refused on the others")
		fs.StringVar(&f.connection, "connection", "", "the connection id a route dispatches through (route)")
		// The example used to be a literal model id. It is a command instead, because the id an operator
		// should type is one their OWN credential can see — `palai model models <mconn_id>` prints exactly
		// that list, and a name suggested here is a name that goes stale in a help string.
		fs.StringVar(&f.model, "model", "", "the model id to send on the provider wire (route) — list the ids this connection can reach with `palai model models <mconn_id>`")
		fs.StringVar(&f.routeName, "route", "", "route alias (route) — defaults to `default`, which is the only alias a run resolves today")
	}
}

// parse builds the FlagSet for a resource and returns the subcommand, the parsed flags, and the positionals.
func parse(resource string, args []string) (sub string, f *flags, pos []string, err error) {
	if len(args) == 0 {
		return "", nil, nil, usageErr(resource)
	}
	sub = args[0]
	f = &flags{}
	fs := flag.NewFlagSet("palai "+resource+" "+sub, flag.ContinueOnError)
	// The FlagSet must not print a parse error itself; main prints the returned error once (NIT 10).
	fs.SetOutput(io.Discard)
	f.register(fs, resource)
	pos, err = parseInterleaved(fs, args[1:])
	return sub, f, pos, err
}

// parseInterleaved parses flags and positionals in any order — the stdlib flag package stops at the first
// non-flag token, so this loop resumes past each positional to keep flags after an id working
// (`palai apikey revoke <id> --json`).
func parseInterleaved(fs *flag.FlagSet, args []string) ([]string, error) {
	var positional []string
	for {
		if err := fs.Parse(args); err != nil {
			return nil, err
		}
		if fs.NArg() == 0 {
			return positional, nil
		}
		positional = append(positional, fs.Arg(0))
		args = fs.Args()[1:]
	}
}

// positionalArity is the exact number of positional arguments each subcommand takes. An unexpected extra
// positional is an error, not silently ignored — that catches a second id (`revoke key_1 key_2` would
// otherwise drop key_2) and, critically, a secret fat-fingered onto argv (`secret create --name x SECRET`),
// which must fail loudly since the value may now be in shell history.
var positionalArity = map[string]int{
	"project/create": 0, "project/list": 0, "project/get": 1, "project/set-policy": 1,
	"apikey/create": 0, "apikey/list": 0, "apikey/get": 1, "apikey/revoke": 1,
	"secret/create": 0, "secret/list": 0, "secret/get": 1, "secret/rotate": 1,
	// A pool is created with flags and its waiting room is switched on ONE pool named by id (E28 T1) — the
	// same one-subject-one-command shape `runner approve` has, and for the same reason: opening a waiting
	// room across several pools with one command is not a thing an operator should do by accident.
	"pool/create": 0, "pool/list": 0, "pool/set-strict": 1,
	// A pool key is minted with flags and revoked by ID. `create` takes no positional for the same reason
	// `secret create` takes none: the only string that could go there is a credential.
	"poolkey/create": 0, "poolkey/list": 0, "poolkey/revoke": 1,
	// A machine is named by ONE id (E24 T5); `list` takes none. An extra positional is refused for the
	// reason every other row here refuses one: `revoke rnr_a rnr_b` must not silently decommission only the
	// first while an operator believes both are down.
	// `approve` (E24 T6) names ONE machine for the same reason: admitting a fleet with one command is not a
	// thing an operator should be able to do by accident, and a human approval that could name several is a
	// human approval nobody read.
	"runner/list": 0, "runner/cordon": 1, "runner/resume": 1, "runner/revoke": 1, "runner/approve": 1,
	// The provider wiring (E29). `create` and `route` take flags only — the one string that could go in a
	// positional is a credential, which is the same reason `secret create` takes none. `verify` names ONE
	// connection: probing several with one command is a bill and a burst of outbound requests nobody asked
	// for, and an operator reading a single verdict knows which row it belongs to.
	// `models` names ONE connection for the same reason `verify` does: a list belongs to a credential, so
	// listing several at once would interleave four catalogues with no column saying whose is whose.
	"model/create": 0, "model/list": 0, "model/get": 1, "model/verify": 1, "model/models": 1, "model/route": 0, "model/routes": 0,
}

// execute maps (resource, subcommand) to the one E13 endpoint it fronts and dispatches it. It first enforces
// the positional arity, so every case below can index pos without a bounds or "extra argument" check.
func (c *Client) execute(resource, sub string, pos []string, f *flags) error {
	want, known := positionalArity[resource+"/"+sub]
	if !known {
		return usageErr(resource)
	}
	if len(pos) != want {
		// Never echo the positionals: an extra one may be a secret fat-fingered onto argv, and this error
		// could land in a log. The count is enough for the operator to see what they typed.
		return fmt.Errorf("palai %s %s takes %d positional argument(s), got %d", resource, sub, want, len(pos))
	}
	switch resource {
	// `org` WAS HERE, and it is removed rather than left to 404. A.2 Task 6 unmounted /v1/organizations
	// with the table behind it; a subcommand that reaches a route the server does not serve renders
	// "request failed (HTTP 404): 404 page not found", which reads to an operator as a broken stack rather
	// than as a command that no longer exists. `project` took over opening a tenant — it is what `org
	// create` used to mean.
	case "project":
		switch sub {
		case "create":
			return c.do(http.MethodPost, "/v1/projects", body(map[string]any{"display_name": f.displayName}))
		case "list":
			return c.do(http.MethodGet, "/v1/projects", nil)
		case "get":
			return c.do(http.MethodGet, "/v1/projects/"+esc(pos[0]), nil)
		case "set-policy":
			policy := map[string]any{}
			if f.allowedModels != "" {
				policy["allowed_models"] = csv(f.allowedModels)
			}
			if f.allowedTools != "" {
				policy["allowed_tools"] = csv(f.allowedTools)
			}
			if f.defaultTools != "" {
				policy["default_tools"] = csv(f.defaultTools)
			}
			// The approver allow-list (E23 T2). Setting it turns the approve surfaces deny-by-default;
			// leaving it unset keeps today's behaviour exactly. docs/operations/approvals.md.
			if f.approvers != "" {
				policy["approvers"] = csv(f.approvers)
			}
			// The runner pool (E24 T2). ONE id, not a list: a project's runs go to one posture.
			if f.pool != "" {
				policy["pool"] = strings.TrimSpace(f.pool)
			}
			if len(policy) == 0 {
				return errors.New("set-policy requires at least one of --allowed-models/--allowed-tools/--default-tools/--approvers/--pool")
			}
			// THE WRITE REPLACES THE WHOLE DOCUMENT — `UpdateProjectConfigPolicy` is an assignment, not a
			// merge — so a call that names one flag CLEARS every other key the policy carried. That is
			// pre-existing behaviour and it is stated in docs/operations/runner-fleet.md §1 where an operator
			// meets it, because the realistic accident is setting --pool and silently dropping --approvers.
			return c.do(http.MethodPatch, "/v1/projects/"+esc(pos[0]), body(map[string]any{"config_policy": policy}))
		}
	case "apikey":
		switch sub {
		case "create":
			b := map[string]any{"project_id": f.project}
			if len(f.scopes) > 0 {
				b["scopes"] = []string(f.scopes)
			}
			if f.expiresAt != "" {
				b["expires_at"] = f.expiresAt
			}
			return c.do(http.MethodPost, "/v1/api-keys", body(b))
		case "list":
			return c.do(http.MethodGet, "/v1/api-keys", nil)
		case "get":
			return c.do(http.MethodGet, "/v1/api-keys/"+esc(pos[0]), nil)
		case "revoke":
			return c.do(http.MethodPost, "/v1/api-keys/"+esc(pos[0])+"/revoke", nil)
		}
	// THE PROVIDER WIRING (E29) — `palai model create|list|get|verify|route|routes`.
	//
	// IT EXISTS BECAUSE THE CONSOLE SAID IT DID. apps/web-console/lib/routes.ts led the model-wiring screen
	// with "This is a read surface — every row here is created by the API or the CLI", and
	// `grep -rn 'model-connections' cmd/ | grep -v _test` returned NOTHING. The API half was true; an
	// operator who followed the CLI half found no verb.
	//
	// `route` is TWO calls behind one subcommand, and that is a judgement rather than an oversight: a
	// connection alone routes nothing — a run resolves an alias, through a PUBLISHED revision, to a
	// connection — so an operator who has created a connection and stopped has done nothing observable. The
	// three underlying verbs stay on the API for anyone who wants the revision lifecycle; this is the path
	// that ends with a run reaching the provider.
	case "model":
		switch sub {
		case "create":
			if f.provider == "" || f.secretRef == "" {
				return errors.New("model create requires --provider <provider-one|provider-two|openai-compatible> and --secret-ref <name> " +
					"(write the key first with `palai secret create --name <name>`, value on stdin)")
			}
			b := map[string]any{"provider": f.provider, "secret_ref": f.secretRef}
			if f.connBaseURL != "" {
				b["base_url"] = f.connBaseURL
			}
			return c.do(http.MethodPost, "/v1/model-connections", body(b))
		case "list":
			return c.do(http.MethodGet, "/v1/model-connections", nil)
		case "get":
			return c.do(http.MethodGet, "/v1/model-connections/"+esc(pos[0]), nil)
		case "verify":
			// A REAL probe: the control plane redeems this connection's credential and asks the endpoint
			// whether it accepts it. It answers 200 even when the credential is REJECTED — the request
			// succeeded and the verdict is in the body — so read `outcome`, and read `not_probed` as
			// "nothing was checked", never as a pass.
			return c.do(http.MethodPost, "/v1/model-connections/"+esc(pos[0])+"/verify", nil)
		case "models":
			// THE PROVIDER'S OWN CATALOGUE for this connection's credential (E29 provider models) — the
			// answer to "what do I put in --model", read from the provider instead of typed.
			//
			// A GET, unlike `verify` directly above, because it writes no stamp. Read `outcome` FIRST: the
			// response carries `data` only when the provider actually answered, so an absent `data` is
			// "we could not ask" and never "your key sees no models".
			return c.do(http.MethodGet, "/v1/model-connections/"+esc(pos[0])+"/models", nil)
		case "route":
			return c.routeThroughConnection(f)
		case "routes":
			return c.do(http.MethodGet, "/v1/model-routes", nil)
		}
	case "secret":
		switch sub {
		case "create":
			if f.name == "" {
				return errors.New("secret create requires --name (the VALUE is read from stdin)")
			}
			value, err := readSecret(c.In)
			if err != nil {
				return err
			}
			return c.do(http.MethodPost, "/v1/secret-refs", body(map[string]any{"name": f.name, "value": value}))
		case "list":
			return c.do(http.MethodGet, "/v1/secret-refs", nil)
		case "get":
			return c.do(http.MethodGet, "/v1/secret-refs/"+esc(pos[0]), nil)
		case "rotate":
			value, err := readSecret(c.In)
			if err != nil {
				return err
			}
			return c.do(http.MethodPost, "/v1/secret-refs/"+esc(pos[0])+"/rotate", body(map[string]any{"value": value}))
		}
	// THE POOL'S BIRTH PATH (E28 T1) — `palai pool create|list|set-strict`.
	//
	// It exists because a fleet whose pools cannot be created is a fleet with one pool, named `default`,
	// posture `sandboxed-linux`, forever: `InsertDefaultRunnerPool` wrote those as LITERALS and there was no
	// UPDATE anywhere in the tree. So `poolkey --pool <pool_id>` above could only ever name that one pool,
	// `config_policy.pool` could only ever place a project in it, and `palai admin runner approve` decided a
	// state — a machine in a STRICT pool's waiting room — that no operator could produce.
	//
	// `set-strict` is a subcommand rather than a flag on a general `update` because it is the only thing
	// about a pool that can change: a machine INHERITS its pool's posture at enrolment, so a `--posture` on
	// an existing pool would retroactively change what the machines in it ARE. The server refuses it; this
	// spelling means an operator never types it.
	case "pool":
		switch sub {
		case "create":
			if f.name == "" || f.posture == "" {
				return errors.New("pool create requires --name <n> and --posture <sandboxed-linux|unsandboxed-host> (a pool IS a posture, so there is no default)")
			}
			b := map[string]any{
				"name": f.name, "posture": f.posture, "strict_enrollment": f.strict,
			}
			if f.osName != "" {
				b["os"] = f.osName
			}
			if f.arch != "" {
				b["arch"] = f.arch
			}
			return c.do(http.MethodPost, "/v1/runner-pools", body(b))
		case "list":
			return c.do(http.MethodGet, "/v1/runner-pools", nil)
		case "set-strict":
			// The flag's ABSENCE closes the waiting room, which is why the body always names the field: a PATCH
			// that omitted it would be refused by the route, and an operator who wants it off has to be able to
			// say so with the same command that turned it on.
			return c.do(http.MethodPatch, "/v1/runner-pools/"+esc(pos[0]), body(map[string]any{"strict_enrollment": f.strict}))
		}
	// The runner-pool enrolment key (E24 T3). `create` prints the value ONCE — it is the same
	// create-only field `apikey create` returns, and the same rule applies: the CLI prints it and
	// retains nothing.
	case "poolkey":
		switch sub {
		case "create":
			if f.pool == "" {
				return errors.New("poolkey create requires --pool <pool_id> (a key admits into exactly one pool)")
			}
			b := map[string]any{}
			if f.expiresAt != "" {
				b["expires_at"] = f.expiresAt
			}
			return c.do(http.MethodPost, "/v1/runner-pools/"+esc(f.pool)+"/keys", body(b))
		case "list":
			if f.pool == "" {
				return errors.New("poolkey list requires --pool <pool_id>")
			}
			return c.do(http.MethodGet, "/v1/runner-pools/"+esc(f.pool)+"/keys", nil)
		case "revoke":
			// By id, never by value: a revoke that took the value would put a credential in argv and in
			// every access log between here and the control plane.
			return c.do(http.MethodPost, "/v1/runner-pool-keys/"+esc(pos[0])+"/revoke", nil)
		}
	// THE MACHINE LIFECYCLE (E24 T5) — `palai admin runner cordon|resume|revoke|list`.
	//
	// It exists because §3.6 D15 measured that `Revoke()` — SAN-011's hard stop, written for a compromised
	// runner — had NO PRODUCTION CALLER anywhere in the tree. There was no way for a human to say
	// "decommission that Mac". This is it, and it is a CLI rather than a console screen because the console
	// has no authentication at all (§0.2): a fleet-moving write behind an unauthenticated proxy would undo
	// what this epic builds.
	//
	// `revoke` IS IRREVERSIBLE and the CLI does not prompt, deliberately: neither `apikey revoke` nor
	// `poolkey revoke` does, and a confirmation on only one of the three teaches an operator the others are
	// safe.
	// AND `approve` (E24 T6): the human a strict pool waits for. It rides the same one-machine-one-verb shape
	// as the three above, and the difference is on the server — the route is gated on the `approve`
	// capability rather than on `provision`, so the key an operator uses here may be a key that can do
	// nothing else. That is the point of the separate capability rather than an accident of routing.
	case "runner":
		switch sub {
		case "list":
			return c.do(http.MethodGet, "/v1/runners", nil)
		case "cordon", "resume", "revoke", "approve":
			return c.do(http.MethodPost, "/v1/runners/"+esc(pos[0])+"/"+sub, nil)
		}
	}
	return usageErr(resource)
}

// do issues the authenticated request and renders the outcome. A body of nil sends no payload.
func (c *Client) do(method, path string, payload []byte) error {
	var r io.Reader
	if payload != nil {
		r = bytes.NewReader(payload)
	}
	req, err := http.NewRequest(method, c.BaseURL+path, r)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.APIKey)
	req.Header.Set("Accept", "application/json")
	if payload != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return fmt.Errorf("%s %s: %w", method, path, err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 400 {
		return c.renderProblem(resp.StatusCode, raw)
	}
	return c.renderSuccess(raw)
}

// createAndReadID POSTs a create and returns the server-minted id, rendering the response as `do` does so
// the operator still sees each step of a multi-call subcommand happen. It is the ONE place this CLI reads a
// field out of a response body: every other subcommand fronts exactly one endpoint and needs nothing from
// what came back.
//
// A create with no `id` is an ERROR rather than an empty string threaded into the next path — that is how a
// POST to /v1/model-routes//revisions gets built, and a 404 on a path with a doubled slash sends an
// operator looking for a route that was never the problem.
func (c *Client) createAndReadID(path string, payload map[string]any) (string, error) {
	req, err := http.NewRequest(http.MethodPost, c.BaseURL+path, bytes.NewReader(body(payload)))
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+c.APIKey)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return "", fmt.Errorf("POST %s: %w", path, err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 400 {
		return "", c.renderProblem(resp.StatusCode, raw)
	}
	var out struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(raw, &out); err != nil || out.ID == "" {
		return "", fmt.Errorf("POST %s returned %d with no `id`, so the next step has nothing to name", path, resp.StatusCode)
	}
	return out.ID, c.renderSuccess(raw)
}

// renderSuccess writes the response to Out: raw bytes in --json mode, indented JSON otherwise (a non-JSON
// body is passed through as-is).
func (c *Client) renderSuccess(raw []byte) error {
	if c.JSON || !json.Valid(raw) {
		return writeLine(c.Out, raw)
	}
	var buf bytes.Buffer
	if err := json.Indent(&buf, raw, "", "  "); err != nil {
		return writeLine(c.Out, raw)
	}
	return writeLine(c.Out, buf.Bytes())
}

// renderProblem turns a >=400 response into a returned error (main prints it to stderr, exit 1). In --json
// mode the raw RFC9457 problem is written to stdout and the error stays terse; otherwise the error carries a
// human line built from the problem's title/detail/code/request id.
func (c *Client) renderProblem(status int, raw []byte) error {
	if c.JSON {
		if err := writeLine(c.Out, raw); err != nil {
			return err
		}
		return fmt.Errorf("request failed (HTTP %d)", status)
	}
	var p struct {
		Title     string `json:"title"`
		Detail    string `json:"detail"`
		Code      string `json:"code"`
		RequestID string `json:"request_id"`
	}
	if err := json.Unmarshal(raw, &p); err != nil || p.Code == "" {
		// Not a problem document (e.g. a proxy's HTML error page) — trim and cap it so it can't flood the line.
		return fmt.Errorf("request failed (HTTP %d): %s", status, cap200(strings.TrimSpace(string(raw))))
	}
	msg := p.Title
	if msg == "" {
		msg = p.Code
	}
	if p.Detail != "" {
		msg += ": " + p.Detail
	}
	msg += " (" + p.Code
	if p.RequestID != "" {
		msg += ", request " + p.RequestID
	}
	msg += ")"
	return errors.New(msg)
}

// resolve applies the flag → env → .palai chain for the base URL and the API key. The key comes from a file
// (the --api-key-file path or the .palai file) or the env — never a flag value.
//
// SECURITY: an EXPLICIT --api-key-file that is empty after trim is a hard error — it never falls back to the
// .palai bootstrap admin key. A failed `vault read > /tmp/k` must not silently upgrade the command to full
// admin; a named source is honored or the command fails.
func resolve(baseURLFlag, apiKeyFileFlag string) (baseURL, apiKey string, err error) {
	baseURL = firstNonEmpty(baseURLFlag, os.Getenv("PALAI_BASE_URL"))
	if apiKeyFileFlag != "" {
		raw, err := os.ReadFile(apiKeyFileFlag)
		if err != nil {
			return "", "", fmt.Errorf("read --api-key-file: %w", err)
		}
		apiKey = strings.TrimSpace(string(raw))
		if apiKey == "" {
			return "", "", fmt.Errorf("--api-key-file %s is empty; refusing to fall back to the .palai bootstrap key", apiKeyFileFlag)
		}
	} else {
		apiKey = os.Getenv("PALAI_API_KEY")
	}
	// Fall back to the initialised .palai for whatever a flag/env did not supply. A .palai read error is only
	// fatal when it leaves a gap — if the key came from env and only the base URL is missing, .palai returning
	// its base URL (even alongside an api-key read error) is enough.
	if baseURL == "" || apiKey == "" {
		dURL, dKey, derr := stack.AdminDefaults()
		baseURL = firstNonEmpty(baseURL, dURL)
		apiKey = firstNonEmpty(apiKey, dKey)
		if derr != nil && (baseURL == "" || apiKey == "") {
			return "", "", fmt.Errorf("no base URL / API key from flags or env, and .palai unavailable: %w", derr)
		}
	}
	if baseURL == "" {
		return "", "", errors.New("no base URL: pass --base-url, set $PALAI_BASE_URL, or run `palai init`")
	}
	if apiKey == "" {
		return "", "", errors.New("no API key: pass --api-key-file, set $PALAI_API_KEY, or run `palai init`")
	}
	return baseURL, apiKey, nil
}

// newHTTPClient builds the admin client's HTTP client, adding a CA trust anchor when one is named (the --ca
// flag → $PALAI_CA_FILE). This is the edge-trust enabling change (E14 T7): the E14 production profile fronts
// the control-plane with a TLS edge that serves a self-signed local-CA certificate, so a plain client cannot
// reach it over https. Pointing --ca at ${PALAI_HOME}/ca/ca.crt adds that CA to the client's RootCAs so the
// admin CLI provisions THROUGH the edge with full verification (the cert's SAN is the controller DNS, so the
// base URL uses that name and resolves it to the edge). An operator with a real-domain certificate needs no
// --ca — the system trust store covers it. A named-but-unreadable/empty CA file is a hard error, never a
// silent fall-through to the system store.
func newHTTPClient(caFile string) (*http.Client, error) {
	caFile = firstNonEmpty(caFile, os.Getenv("PALAI_CA_FILE"))
	if caFile == "" {
		return &http.Client{Timeout: 30 * time.Second}, nil
	}
	pem, err := os.ReadFile(caFile)
	if err != nil {
		return nil, fmt.Errorf("read --ca file: %w", err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(pem) {
		return nil, fmt.Errorf("--ca file %s held no PEM certificates", caFile)
	}
	return &http.Client{
		Timeout:   30 * time.Second,
		Transport: &http.Transport{TLSClientConfig: &tls.Config{MinVersion: tls.VersionTLS12, RootCAs: pool}},
	}, nil
}

// maxSecretBytes bounds a secret value so an oversized read is a loud error, not a silent truncation.
const maxSecretBytes = 64 * 1024

// readSecret reads a secret value from stdin (never argv). It refuses a terminal (so a typed secret never
// echoes to the screen), errors on a value over 64KiB rather than truncating it, and errors on non-UTF-8
// bytes rather than letting JSON corrupt them to U+FFFD. The trailing newline a shell pipe adds is trimmed.
func readSecret(in io.Reader) (string, error) {
	if f, ok := in.(*os.File); ok {
		if fi, err := f.Stat(); err == nil && fi.Mode()&os.ModeCharDevice != 0 {
			return "", errors.New("refusing to read a secret from a terminal; pipe the value on stdin (e.g. `printf %s \"$VALUE\" | palai secret create --name db-url`)")
		}
	}
	// Read one extra byte beyond the max value + a possible CRLF so an over-limit value is detectable.
	raw, err := io.ReadAll(io.LimitReader(in, maxSecretBytes+2))
	if err != nil {
		return "", fmt.Errorf("read secret value from stdin: %w", err)
	}
	v := strings.TrimRight(string(raw), "\r\n")
	if len(v) > maxSecretBytes {
		return "", fmt.Errorf("secret value exceeds %d bytes; store an oversized value by reference, not inline", maxSecretBytes)
	}
	if v == "" {
		return "", errors.New("no secret value on stdin (pipe it, e.g. `printf %s \"$VALUE\" | palai secret create --name db-url`)")
	}
	if !utf8.ValidString(v) {
		return "", errors.New("secret value is not valid UTF-8; base64-encode binary values before storing them")
	}
	return v, nil
}

// body marshals a request body. The maps here are string/[]string only, so marshalling cannot fail.
func body(v any) []byte {
	b, _ := json.Marshal(v)
	return b
}

// esc percent-escapes a user-chosen path segment (a secret name may carry reserved characters; ids are
// minted but escaping is uniformly correct).
func esc(s string) string { return url.PathEscape(s) }

// csv splits a comma-separated flag into a trimmed, non-empty slice.
func csv(s string) []string {
	parts := strings.Split(s, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if t := strings.TrimSpace(p); t != "" {
			out = append(out, t)
		}
	}
	return out
}

// cap200 bounds an untrusted response body embedded in an error line.
func cap200(s string) string {
	const max = 200
	if len(s) > max {
		return s[:max] + "…"
	}
	return s
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

// writeLine writes b to w, ensuring exactly one trailing newline.
func writeLine(w io.Writer, b []byte) error {
	_, err := w.Write(append(bytes.TrimRight(b, "\n"), '\n'))
	return err
}

// routeThroughConnection points the project's runs at a connection: open the alias, add a revision naming
// the model and the connection, publish it. Three server calls, because that is what the revision lifecycle
// IS — a draft never steers a run, and publishing is the moment the routing changes.
//
// IT IS NOT ATOMIC, and the error says which step failed rather than pretending otherwise: a failure after
// the alias exists leaves an alias with no published revision, which routes nothing and is therefore safe —
// the project keeps running on the deployment default. Re-running the command is the repair (alias creation
// is get-or-create).
func (c *Client) routeThroughConnection(f *flags) error {
	if f.connection == "" || f.model == "" {
		return errors.New("model route requires --connection <mconn_id> and --model <model id> (`palai model list` prints the connection ids)")
	}
	alias := f.routeName
	if alias == "" {
		// The ONLY alias a run resolves today (coordinator.DefaultModelRouteAlias). A different one is
		// accepted because the API accepts it, and it will route nothing until a config layer can ask for it.
		alias = "default"
	}
	routeID, err := c.createAndReadID("/v1/model-routes", map[string]any{"name": alias})
	if err != nil {
		return fmt.Errorf("open the route alias %q: %w", alias, err)
	}
	revisionID, err := c.createAndReadID("/v1/model-routes/"+esc(routeID)+"/revisions",
		map[string]any{"model": f.model, "connection_id": f.connection})
	if err != nil {
		return fmt.Errorf("add a revision to route %s (the alias exists and routes nothing, which is safe — re-run to repair): %w", routeID, err)
	}
	if err := c.do(http.MethodPost, "/v1/model-routes/"+esc(routeID)+"/revisions/"+esc(revisionID)+"/publish", nil); err != nil {
		return fmt.Errorf("publish revision %s (it is a DRAFT and steers nothing until published): %w", revisionID, err)
	}
	return nil
}

// usageErr names the subcommands a resource accepts.
func usageErr(resource string) error {
	subs := map[string]string{
		"project": "create --display-name <n> | list | get <prj_id> | set-policy <prj_id> [--allowed-models <a,b>] [--approvers <p,q>] [--pool <pool_id>] (set-policy REPLACES the whole policy)",
		"apikey":  "create --project <prj_id> [--scope <s>]... | list | get <key_id> | revoke <key_id>",
		"secret":  "create --name <n> (value on stdin) | list | get <name> | rotate <name> (value on stdin)",
		"pool":    "create --name <n> --posture <sandboxed-linux|unsandboxed-host> [--os <o>] [--arch <a>] [--strict] | list | set-strict <pool_id> [--strict] (posture is fixed at creation: machines inherit it)",
		"poolkey": "create --pool <pool_id> [--expires-at <rfc3339>] (value printed once) | list --pool <pool_id> | revoke <key_id>",
		// Spelled with its prefix, because that is the only spelling that works: the lifecycle is reached as
		// `palai admin runner …` so it cannot be confused with the local runner container.
		"runner": "list | approve <runner_id> | cordon <runner_id> | resume <runner_id> | revoke <runner_id> (revoke is IRREVERSIBLE)",
		// The provider wiring (E29). `create` never takes a key: write it with `palai secret create` and
		// name the REF here.
		"model": "create --provider <provider-one|provider-two|openai-compatible> --secret-ref <name> [--endpoint <url>] | list | get <mconn_id> | verify <mconn_id> | models <mconn_id> | route --connection <mconn_id> --model <id> | routes",
	}
	return fmt.Errorf("usage: palai %s <%s>", resource, subs[resource])
}
