package stack

import (
	"bufio"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"sort"
	"strings"
	"time"
)

// up.go is `palai up`: ONE command that ends in a stack PROVEN to be talking to a real model
// provider — or a named failure with its fix. It composes what already exists (Init, AddProvider,
// Up, doctor's runChecks, the public API) and adds exactly one thing none of them do: it REFUSES
// to report success on a stack it could not demonstrate a live round-trip against.
//
// The trap it closes is real and was hit by hand an hour before this file existed: a stack brought
// up with a valid credential but a mis-spelled selector runs the DETERMINISTIC FAKE adapter, the
// run reports `status: completed`, and the only tell is `"model": "fake"` in the response body.
// Everything below exists because a silent downgrade to fake is indistinguishable from success at
// every layer except the one this command asserts.

// liveSelector is the ONLY PALAI_MODEL_PROVIDER value main.modelBrokerFromEnv treats as live
// (apps/control-plane/cmd/palai-control-plane/main.go). EVERY other value — "openai", "anthropic",
// a typo, or unset — falls through to the fake adapter with no warning, so `palai up` refuses
// anything else instead of inheriting that fallback.
const liveSelector = "provider-one"

// fakeModel is the model id main.go scripts the fake adapter with. It is the needle the live proof
// looks for: a terminal projection carrying it means the run never left the process.
const fakeModel = "fake"

// credentialEnv is the .env.local variable the provider-one (OpenAI) adapter's credential comes
// from. NOTE what it is NOT: the control-plane never reads it — the value is written to the 0600
// $PALAI_HOME/secrets/provider-one file secret, which deploy/compose/control-plane-entrypoint.sh
// bridges into the container as PALAI_SECRET_PROVIDER_ONE at start. Setting OPENAI_API_KEY in the
// environment of a running stack does nothing at all, which is the second half of the same trap.
const credentialEnv = "OPENAI_API_KEY"

// Bootstrap is `palai up`. Steps 1-3 run BEFORE any Docker work so a refusal costs nothing.
func Bootstrap(envFile string) error {
	fmt.Fprintln(os.Stderr, "palai up — bring the stack up and PROVE it is live")

	// [1/6] The dotenv file. Only key NAMES are ever printed, and only the PALAI_* knobs below are
	// exported to the child `docker compose` environment — a credential read here reaches exactly
	// one destination: the 0600 file secret AddProvider writes.
	fileEnv, found, err := loadEnvFile(envFile)
	if err != nil {
		return err
	}
	if found {
		fmt.Fprintf(os.Stderr, "[1/6] env       %s: %d values (%s)\n", envFile, len(fileEnv), strings.Join(sortedKeys(fileEnv), ", "))
	} else {
		fmt.Fprintf(os.Stderr, "[1/6] env       %s absent — running on the process environment alone\n", envFile)
	}
	get := lookup(fileEnv)

	// [2/6] The provider decision comes BEFORE `init`, so a refusal leaves no .palai behind and
	// costs nothing. secretSlotFilled is honest either way: an absent .palai has no slot, and
	// `init` would only ever create an EMPTY one.
	p, err := resolvePaths()
	if err != nil {
		return err
	}
	choice, err := resolveProvider(get, secretSlotFilled(p))
	// Warnings print on BOTH paths: a mis-spelled variable name is most worth saying out loud in
	// the message that is about to tell the operator they have no usable credential.
	for _, w := range choice.warnings {
		fmt.Fprintf(os.Stderr, "        WARNING %s\n", w)
	}
	if err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "[2/6] provider  selector %s — %s\n", liveSelector, choice.source)
	if err := Init(); err != nil {
		return err
	}
	if choice.credential != "" {
		if err := addProvider(liveSelector, strings.NewReader(choice.credential)); err != nil {
			return err
		}
	}

	// [3/6] The compose interpolation contract. PALAI_MODEL_PROVIDER is what selects the live
	// adapter; PALAI_DISPATCH_WORKERS defaults to 0 (queued-only) in compose.yaml, which would
	// leave every run parked in `queued` and the proof unprovable.
	if err := os.Setenv("PALAI_MODEL_PROVIDER", liveSelector); err != nil {
		return err
	}
	if m := get("PALAI_MODEL"); m != "" {
		if err := os.Setenv("PALAI_MODEL", m); err != nil {
			return err
		}
	}
	switch w := strings.TrimSpace(get("PALAI_DISPATCH_WORKERS")); w {
	case "":
		if err := os.Setenv("PALAI_DISPATCH_WORKERS", "1"); err != nil {
			return err
		}
	case "0":
		return errors.New("PALAI_DISPATCH_WORKERS=0 selects a queued-only stack: nothing executes, so no round-trip can be proven — unset it or set it to 1")
	default:
		if err := os.Setenv("PALAI_DISPATCH_WORKERS", w); err != nil {
			return err
		}
	}
	// The Slack knobs the CONTAINER needs must be exported before compose creates it: a running
	// control-plane never re-reads its environment, so a socket switched on after the fact stays off
	// until the next bring-up. Failures here are reported and carried — an unusable master key must
	// not fail a bring-up whose model round-trip is about to be proven.
	for _, w := range applySlackEnv(p, get) {
		fmt.Fprintf(os.Stderr, "        WARNING %s\n", w)
	}
	fmt.Fprintln(os.Stderr, "[3/6] stack     docker compose up (this builds on a first run)")
	if err := Up(); err != nil {
		return err
	}

	cfg, p, err := loadConfig()
	if err != nil {
		return err
	}
	key, err := readTrimmed(p.apiKey)
	if err != nil {
		return fmt.Errorf("read api key: %w", err)
	}
	api := &apiClient{baseURL: cfg.BaseURL, key: key, http: &http.Client{Timeout: 30 * time.Second}}

	// [4/6] Health, from doctor's OWN checks — no second set of thresholds. A still-red check is
	// reported and CARRIED, not swallowed and not fatal: the live proof below is the real verdict,
	// and a red check that actually blocks the stack will surface there with its detail on screen.
	report := waitHealthy(cfg, p, 90*time.Second)
	red := redChecks(report)
	if len(red) == 0 {
		fmt.Fprintf(os.Stderr, "[4/6] health    doctor: %d/%d green\n", len(report.Checks), len(report.Checks))
	} else {
		fmt.Fprintf(os.Stderr, "[4/6] health    doctor: %d/%d green — NOT green: %s\n",
			len(report.Checks)-len(red), len(report.Checks), strings.Join(red, "; "))
	}

	// [5/6] The point of the whole command.
	fmt.Fprintln(os.Stderr, "[5/6] proof     one real single-step run...")
	rt, err := roundTripProof(api)
	if err != nil {
		if len(red) > 0 {
			return fmt.Errorf("%w\n        doctor checks still red, one of them may be the cause: %s", err, strings.Join(red, "; "))
		}
		return err
	}

	// [6/6] Slack, if and only if the workspace values are present. Never fatal.
	slackFact, slackLine, slackWarn := wireSlack(api, cfg, p, get)
	fmt.Fprintf(os.Stderr, "[6/6] slack     %s\n", slackLine)

	caps, err := api.capabilities()
	if err != nil {
		return fmt.Errorf("read /v1/capabilities for the status table: %w", err)
	}
	printReport(cfg, rt, caps, observedFacts(rt, slackFact), red, slackWarn)
	return nil
}

// --- provider selection -------------------------------------------------------------------

// providerChoice is the resolved live provider decision. source is operator-facing prose; it names
// where the credential came from and NEVER carries a value.
type providerChoice struct {
	credential string
	source     string
	warnings   []string
}

// resolveProvider decides whether this bring-up can run live, and refuses rather than falling back.
// The three refusals are the three ways an operator silently ends up on the fake adapter:
//
//  1. a selector that is not `provider-one` (main.modelBrokerFromEnv reads ANY other value as fake),
//  2. no credential anywhere, and
//  3. only an Anthropic credential — which the compose deployment default cannot use at all.
//
// storedSecret reports whether $PALAI_HOME/secrets/provider-one is already non-empty, so an
// operator who ran `palai provider add` once is not asked for the value again.
func resolveProvider(get func(string) string, storedSecret bool) (providerChoice, error) {
	var c providerChoice
	switch sel := strings.TrimSpace(get("PALAI_MODEL_PROVIDER")); sel {
	case "", liveSelector:
	case fakeModel:
		return c, fmt.Errorf("PALAI_MODEL_PROVIDER=%q selects the deterministic fake adapter, and `palai up` exists to prove a LIVE round-trip — "+
			"fix: unset it (or set it to %q) for a live stack, or run `palai local up` for the fake one", sel, liveSelector)
	default:
		return c, fmt.Errorf("PALAI_MODEL_PROVIDER=%q is not a recognised selector: the control-plane reads every value other than %q as the "+
			"deterministic FAKE adapter and says nothing (main.modelBrokerFromEnv) — fix: set PALAI_MODEL_PROVIDER=%s", sel, liveSelector, liveSelector)
	}

	// The mis-spelling is this repo's own: .env.local and the whole tree spell it ANTROPHIC_API_KEY
	// (`grep -r ANTROPHIC`). Both spellings are read HERE — and only here, to make the refusal below
	// accurate — but nothing is renamed in the tree: the mis-spelling is consistent, so a rename is a
	// cross-cutting change with no bearing on whether this command can prove a live round-trip.
	anthropic := firstNonEmpty(get("ANTHROPIC_API_KEY"), get("ANTROPHIC_API_KEY"))
	if get("ANTROPHIC_API_KEY") != "" && get("ANTHROPIC_API_KEY") == "" {
		c.warnings = append(c.warnings, "ANTROPHIC_API_KEY is spelled the way this repo spells it, but it is a mis-spelling of ANTHROPIC_API_KEY (accepted here, nothing else in the tree reads either)")
	}

	if cred := strings.TrimSpace(get(credentialEnv)); cred != "" {
		c.credential = cred
		c.source = fmt.Sprintf("credential from %s, written to the 0600 file secret (never argv, env or log)", credentialEnv)
		return c, nil
	}
	if storedSecret {
		c.source = "credential already stored by a previous `palai provider add provider-one` (0600 file secret, not re-read here)"
		return c, nil
	}
	if anthropic != "" {
		return c, fmt.Errorf("an Anthropic credential is set but the compose deployment default cannot use it: the entrypoint bridges only "+
			"provider_one_key and modelBrokerFromEnv's env route is %s (OpenAI). The Anthropic adapter (provider-two) is reachable only through a "+
			"published per-project model route — fix: set %s in .env.local for this bring-up", liveSelector, credentialEnv)
	}
	return c, fmt.Errorf("no provider credential: %s is unset and $PALAI_HOME/secrets/provider-one is empty, so the stack would come up on the "+
		"deterministic FAKE adapter — fix: put %s=<key> in .env.local, or pipe it once with "+
		"`printf %%s \"$%s\" | palai provider add provider-one`", credentialEnv, credentialEnv, credentialEnv)
}

// secretSlotFilled reports whether the provider-one file secret already holds a value. `palai init`
// creates the slot EMPTY (compose bind-mounts it and the mount fails on a missing source), so
// existence proves nothing and only a non-zero size does.
func secretSlotFilled(p paths) bool {
	info, err := os.Stat(p.secretPath(liveSelector))
	return err == nil && info.Size() > 0
}

// --- the live proof -----------------------------------------------------------------------

// roundTrip is the evidence one real single-step run leaves behind.
type roundTrip struct {
	ResponseID   string
	Status       string
	Model        string
	InputTokens  int
	OutputTokens int
	TotalTokens  int
}

// roundTripProof issues one trivial run and returns it only if proveLive accepts it.
func roundTripProof(api *apiClient) (roundTrip, error) {
	id, err := api.createResponse("Reply with the single word: ok")
	if err != nil {
		return roundTrip{}, err
	}
	rt, err := api.awaitTerminal(id, 180*time.Second)
	if err != nil {
		return roundTrip{}, err
	}
	return rt, proveLive(rt)
}

// proveLive is the crown assertion: it accepts a terminal response ONLY as evidence that a real
// provider served it. All four conditions are load-bearing, because each one alone is satisfiable
// by a stack that never left the process:
//
//   - a fake run also reaches `completed`, so status alone proves nothing;
//   - an empty model would sail past a bare `model != "fake"` check (the vacuous-assertion trap);
//   - `fake` is the model id main.go scripts the fake adapter with; and
//   - the fake script declares NO usage, so a zero token count is the second tell — and no real
//     provider bills a completed call zero in AND zero out.
func proveLive(rt roundTrip) error {
	switch {
	case rt.Status != "completed":
		return fmt.Errorf("NOT PROVEN: response %s ended %q, not completed — the stack is up but it did not complete a run. "+
			"Fix: `palai local doctor` for the failing surface, then `docker compose logs control-plane`", rt.ResponseID, rt.Status)
	case rt.Model == "":
		return fmt.Errorf("NOT PROVEN: response %s completed but its terminal projection carries NO model, so nothing here says which provider "+
			"served it. Fix: this is a control-plane finalize bug, not a configuration one — report it with the response id", rt.ResponseID)
	case rt.Model == fakeModel:
		return fmt.Errorf("NOT PROVEN — THE STACK IS FAKE: response %s completed on model %q, the deterministic in-process adapter. No provider "+
			"was called and no token was spent. Fix: PALAI_MODEL_PROVIDER must be exactly %q when the control-plane starts (any other value selects "+
			"fake in silence), and the credential must be in $PALAI_HOME/secrets/provider-one BEFORE `local up` — a running container never re-reads it",
			rt.ResponseID, rt.Model, liveSelector)
	case rt.InputTokens == 0 && rt.OutputTokens == 0:
		return fmt.Errorf("NOT PROVEN: response %s completed on model %q but reported ZERO tokens in and out. A real provider call is metered; a "+
			"zero-usage completion is the fake adapter's signature under a different model id. Fix: check the control-plane logs for a model route "+
			"override on this project", rt.ResponseID, rt.Model)
	}
	return nil
}

// --- slack ---------------------------------------------------------------------------------

// slackSecretSlots is the ONE table behind every Slack credential this command touches: the
// registration field that names the handle, the handle's prefix, and the .env.local variable
// holding the value stored under it.
//
// It is one table because the failure it closes is a PAIRING failure. `palai up` used to register
// `signing_secret_ref: slack-signing-<team>` — correctly a handle, never a secret — while nothing
// stored a value under that name anywhere the control-plane could redeem it. The handle resolved to
// nothing, and every consumer of it fails SILENTLY by design: an unresolvable signing secret is a
// verification refusal (no config oracle), an unresolvable app token is a dial that never happens.
// Deriving the registered handle and the stored secret's NAME from the same row is what makes them
// incapable of drifting apart.
//
// Where each value comes from is docs/superpowers/plans/phase-19-integration-wiring.md §0.1.
var slackSecretSlots = []struct{ field, prefix, env, purpose string }{
	{"signing_secret_ref", "slack-signing-", "SLACK_SIGNING_SECRET", "verifies the v0 signature on the HTTP Events/interactivity callbacks"},
	{"bot_token_ref", "slack-bot-", "SLACK_BOT_TOKEN", "posts and updates the bot's own messages in the thread"},
	{"app_token_ref", "slack-app-", "SLACK_APP_TOKEN", "opens the Socket Mode connection (apps.connections.open)"},
}

// slackAppTokenSlot is the row Socket Mode's only authentication lives on — named once so
// slackSocketTeam cannot drift from the table.
const slackAppTokenSlot = "app_token_ref"

// containerMasterKeyPath is where compose mounts the master-key file secret. A PATH, never a value,
// so it rides the compose environment like every other knob.
const containerMasterKeyPath = "/run/secrets/master_key"

// slackSocketTeam returns the workspace Socket Mode should dial, or "" to leave the connect loop
// dormant. It follows the APP-LEVEL TOKEN rather than the team id, because the app-level token is
// Socket Mode's only authentication (adapters/integrations/slack, D7): switching the loop on
// without one produces a control-plane that dials, fails and logs on a timer, which reads as a
// broken stack rather than an unconfigured one.
func slackSocketTeam(get func(string) string) string {
	team := strings.TrimSpace(get("SLACK_TEAM_ID"))
	if team == "" || strings.TrimSpace(get("SLACK_APP_TOKEN")) == "" {
		return ""
	}
	return team
}

// slackSecretValues maps each secret-ref NAME to the value that must be stored under it. Values
// present in .env.local only; a slot with no value is left out entirely, so a handle is never
// registered for a credential the operator does not have.
func slackSecretValues(get func(string) string) map[string]string {
	team := strings.TrimSpace(get("SLACK_TEAM_ID"))
	if team == "" {
		return nil
	}
	out := map[string]string{}
	for _, slot := range slackSecretSlots {
		if v := strings.TrimSpace(get(slot.env)); v != "" {
			out[slot.prefix+team] = v
		}
	}
	return out
}

// applySlackEnv exports the compose interpolation the CONTAINER needs for Slack, and returns any
// operator warnings. It runs BEFORE `docker compose up` because a running control-plane never
// re-reads its environment.
//
// ONE variable now, optional and empty by default in compose.yaml, so a deployment with no Slack app
// is byte-for-byte unchanged: PALAI_SLACK_SOCKET_TEAM_ID switches on main.startSlackSocket.
//
// PALAI_SECRET_MASTER_KEY_FILE USED TO BE EXPORTED HERE and no longer is (E21 T2, §3.6 D5). That was
// the bug: the secret store is not a Slack feature — every *_ref handle on the stack redeems through
// it — but it was mounted only when Slack credentials happened to be present, so a stack without them
// resolved every handle nowhere. compose.yaml now writes the path literally and ensureSecretSlots
// mints the key it names, on every bring-up, Slack or not.
//
// p stays in the signature deliberately: an env-shaping step that cannot see the stack's own paths is
// where the next "only when Slack is configured" special case gets added back.
func applySlackEnv(p paths, get func(string) string) []string {
	values := slackSecretValues(get)
	if len(values) == 0 {
		return nil
	}
	var warnings []string
	switch team := slackSocketTeam(get); team {
	case "":
		warnings = append(warnings, "SLACK_APP_TOKEN is unset, so Socket Mode stays off: an app-level token (xapp-…) is its only "+
			"authentication, and without a public callback URL nothing can reach this stack from Slack "+
			"(§0.1 — App → Basic Information → App-Level Tokens → Generate, scope connections:write)")
	default:
		if err := os.Setenv("PALAI_SLACK_SOCKET_TEAM_ID", team); err != nil {
			warnings = append(warnings, fmt.Sprintf("could not export PALAI_SLACK_SOCKET_TEAM_ID: %v", err))
		}
	}
	return warnings
}

// ensureMasterKey makes $PALAI_HOME/secrets/master-key hold a key identity.ParseMasterKey accepts
// (32 bytes, hex). An EXISTING valid key is never replaced: it seals every secret already in the
// store, and dbSecret fails CLOSED on a decrypt failure rather than falling back — so re-minting
// would not degrade the stack, it would break it.
//
// An existing file holding something else (the `REPLACE_WITH_…` placeholder, a truncated paste) is
// a refusal, not an overwrite: the control-plane treats an unparseable key as a startup error, so
// booting on it would take the whole stack down, and clobbering it would destroy whatever it was.
func ensureMasterKey(p paths) error {
	raw, err := os.ReadFile(p.masterKey)
	if err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("read %s: %w", p.masterKey, err)
	}
	switch key := strings.TrimSpace(string(raw)); {
	case len(key) == 64 && isHex(key):
		return nil
	case key != "":
		return fmt.Errorf("%s does not hold a 32-byte hex key, so the control-plane would refuse to boot on it "+
			"(it is a startup error by design) — fix: replace it with `openssl rand -hex 32`, or delete it to have one minted", p.masterKey)
	}
	if err := os.MkdirAll(p.secretsDir, 0o700); err != nil {
		return fmt.Errorf("create %s: %w", p.secretsDir, err)
	}
	// randomHex(32) is 32 random BYTES hex-encoded — the 64-char form ParseMasterKey requires.
	if err := os.WriteFile(p.masterKey, []byte(randomHex(32)), 0o600); err != nil {
		return fmt.Errorf("mint %s: %w", p.masterKey, err)
	}
	return nil
}

func isHex(s string) bool {
	_, err := hex.DecodeString(s)
	return err == nil
}

// slackOutcome is what a bring-up actually OBSERVED about Slack — the input to the report, kept
// separate from the printing so the "what earns the word live" rule is a testable function rather
// than a format string.
type slackOutcome struct {
	team         string
	connectionID string
	stored       int    // secret refs whose VALUE is now redeemable on this stack
	existing     int    // workspaces already registered (the skip path)
	connected    bool   // Slack sent the Socket Mode `hello` frame on this boot
	detail       string // the control-plane's own last word about the socket
	skip         string // why the step did nothing at all
	problem      string // a registration/storage failure, already operator-readable
}

// slackReport turns an observation into (fact, line). fact is what the capability table shows, and
// a NON-EMPTY fact renders the row as `live` — which is the whole point of this function.
//
// THE RULE: `slack` is live only when the control-plane observed Slack's own `hello` frame, the
// first message on an authenticated Socket Mode connection. Everything short of that is dormant.
// A 201 from POST /v1/slack-connections proves a row was written; it proves nothing about whether
// anything can arrive, which is exactly what the previous `slack live — workspace T… registered`
// claimed. This repo has found nine surfaces reporting more than they proved; this is not a tenth.
func slackReport(o slackOutcome) (fact string, line string) {
	switch {
	case o.skip != "":
		if o.connected {
			return fmt.Sprintf("Socket Mode connected — %s", o.detail),
				fmt.Sprintf("SKIPPED — %s (%d workspace(s) already registered, and Socket Mode IS connected on this stack)", o.skip, o.existing)
		}
		if o.existing > 0 {
			return "", fmt.Sprintf("SKIPPED — %s (%d workspace(s) already registered, but Socket Mode is NOT connected: %s)", o.skip, o.existing, o.detail)
		}
		return "", "SKIPPED — " + o.skip
	case o.problem != "":
		return "", o.problem
	case o.connected:
		return fmt.Sprintf("Socket Mode connected for workspace %s — %s", o.team, o.detail),
			fmt.Sprintf("registered %s for team %s; %d secret ref(s) stored and redeemable; Socket Mode CONNECTED — %s",
				o.connectionID, o.team, o.stored, o.detail)
	default:
		return "", fmt.Sprintf("registered %s for team %s; %d secret ref(s) stored — but NOT CONNECTED: %s. "+
			"Nothing will arrive from Slack until that line changes, so `slack` is reported dormant.",
			o.connectionID, o.team, o.stored, o.detail)
	}
}

// wireSlack does the whole last mile after the stack is up: store the credential VALUES where the
// control-plane can redeem them, register the workspace against those handles, and then OBSERVE
// whether Slack actually opened a socket. It never fails the bring-up — an absent Slack app is a
// normal state, and a Slack hiccup does not un-prove the live round-trip above.
//
// It returns (fact, line, warn). fact is the observed-state string the capability table shows for
// `slack`, line is the operator-facing step result, and warn is a non-fatal condition the final
// report must repeat because the operator would otherwise only meet it as silence.
//
// warn is a SEPARATE return rather than more prose on line, and that separation is the point: the
// two carry different truths about the same connection — whether anything can ARRIVE from Slack
// (the socket, on line) and whether anything can be APPROVED once it does (the allow-list, on
// warn). Folding one into the other would let a connected socket read as a working integration
// while every Approve click is silently refused, which is the same over-claiming in a new place.
func wireSlack(api *apiClient, cfg Config, p paths, get func(string) string) (fact string, line string, warn string) {
	body, skip := slackRegistration(get)
	if skip != "" {
		o := slackOutcome{skip: skip}
		if n, err := api.slackConnectionCount(); err == nil && n > 0 {
			// A workspace registered by an earlier run may well be live; look before saying so.
			o.existing = n
			o.connected, o.detail = observeSlackSocket(cfg, p, 0)
		}
		// No body was sent, so nothing here knows the STORED connection's approver list — and
		// guessing at it is exactly what slackApproverWarning exists to replace. The skip itself IS
		// carried out, though: a half-configured install that silently does nothing is what item 3
		// of this task exists to end.
		fact, line = slackReport(o)
		return fact, line, slackSkipWarning(get, skip)
	}
	team, _ := body["team_id"].(string)
	o := slackOutcome{team: team}

	// WHAT to run and AS WHOM. Resolved against the running stack (explicit config wins, otherwise
	// provisioned) rather than demanded from .env.local — see resolveRunTarget. It runs AFTER the
	// team/signing checks above so a stack with no Slack app provisions nothing at all.
	target, err := resolveRunTarget(api, get)
	if err != nil {
		o.problem = "NOT registered: " + err.Error()
		fact, line = slackReport(o)
		return fact, line, ""
	}
	body["default_policy"] = map[string]any{"agent_revision_id": target.revision, "principal_id": target.principal}
	if target.resolved != "" {
		fmt.Fprintf(os.Stderr, "        %s\n", target.resolved)
	}

	// The VALUES first: a connection registered against handles that resolve nowhere is precisely
	// the state this task exists to end, so the redemption path is filled before the row names it.
	for name, value := range slackSecretValues(get) {
		if err := api.putSecretRef(name, value); err != nil {
			// No connection row was created, so there is no allow-list to warn about yet.
			o.problem = "NOT wired: " + err.Error()
			fact, line = slackReport(o)
			return fact, line, ""
		}
		o.stored++
	}

	id, status, err := api.createSlackConnection(body)
	switch {
	case err != nil:
		o.problem = "NOT registered: " + err.Error()
		fact, line = slackReport(o)
		return fact, line, ""
	case status == http.StatusConflict:
		// The workspace is already bound (possibly from an earlier run). The refs it was bound with
		// are the same handles — derived from the team id — so the values just stored still apply.
		// The approver list, though, is the STORED connection's and not this body's: nothing here
		// changed it and nothing here read it, so no warning is claimed about it either way.
		o.connectionID = "(already bound)"
	default:
		o.connectionID = id
		// A row was written from THIS body, so what it does and does not authorize is known. Derived
		// from the body actually sent, never from an assumption about what `palai up` usually does.
		warn = slackApproverWarning(body)
	}

	// The connect loop resolves its workspace ONCE, at boot (extensions.SlackSocket's documented
	// ceiling, and the remedy its own log line names). A workspace registered by THIS bring-up
	// therefore cannot be seen by the control-plane that was already running, so look first and
	// recreate only if the socket is not already up. --force-recreate rather than restart, so the
	// log read afterwards carries this boot alone.
	if o.connected, o.detail = observeSlackSocket(cfg, p, 0); !o.connected {
		fmt.Fprintln(os.Stderr, "        the control-plane is restarting to pick up the workspace (Socket Mode resolves it at boot)")
		if err := recreateControlPlane(cfg, p); err != nil {
			o.detail = fmt.Sprintf("the control-plane could not be recreated to pick up the registration: %v", err)
			fact, line = slackReport(o)
			return fact, line, warn
		}
		o.connected, o.detail = observeSlackSocket(cfg, p, 45*time.Second)
	}
	// warn rides alongside whatever the socket turned out to be. A connected socket does not retire
	// it — that is the case it matters MOST in, because the events now arriving are the ones whose
	// Approve buttons will be refused.
	fact, line = slackReport(o)
	return fact, line, warn
}

// recreateControlPlane replaces the control-plane container in place, leaving Postgres, the object
// store and the runner alone. The engine digest is re-resolved rather than defaulted to the tag:
// the control-plane hands PALAI_ENGINE_IMAGE to the runner's lease, which requires an immutable
// sha256, so recreating with the mutable tag would break every subsequent run.
func recreateControlPlane(cfg Config, p paths) error {
	digest, err := imageID(engineImage)
	if err != nil {
		return err
	}
	if err := runVisible(cfg.composeEnv(p.home, digest), "docker", "compose", "-p", cfg.Project, "-f", composeFile(),
		"up", "-d", "--force-recreate", "--no-deps", "--wait", "control-plane"); err != nil {
		return fmt.Errorf("recreate the control-plane: %w", err)
	}
	return waitForAPI(cfg, p)
}

// observeSlackSocket reads the control-plane's OWN log until it says Slack sent `hello`, or the
// window elapses. within == 0 is a single look (the "is it already up?" probe).
//
// It is a log read because the `hello` frame is the only evidence that exists: it is Slack's first
// message on an authenticated Socket Mode connection, so seeing it means apps.connections.open
// accepted the app-level token AND the WebSocket is open AND the workspace resolved to a real
// connection row. No endpoint exposes that, and inventing one to report it would be a bigger change
// than the wiring it reports on. The lines it reads carry no credential by construction
// (TestSlackSocketNeverLogsTheTokenOrTheTicket is the guard on that).
func observeSlackSocket(cfg Config, p paths, within time.Duration) (bool, string) {
	deadline := time.Now().Add(within)
	for {
		out, _ := runCaptured(cfg.composeEnv(p.home, engineImage), "docker", "compose", "-p", cfg.Project,
			"-f", composeFile(), "logs", "--no-color", "control-plane")
		connected, detail := readSlackSocketLog(out)
		if connected || !time.Now().Before(deadline) {
			return connected, detail
		}
		time.Sleep(2 * time.Second)
	}
}

// The three shapes the control-plane's log carries about Socket Mode.
const (
	// slackSocketConnected is what extensions.SlackSocket writes when Slack's `hello` arrives.
	slackSocketConnected = "slack socket: connected"
	// slackSocketPrefix marks every line the connect loop writes itself.
	slackSocketPrefix = "slack socket: "
	// slackSocketEnabled is main.startSlackSocket announcing the loop was mounted at all.
	slackSocketEnabled = "Slack Socket Mode enabled"
	// slackSocketSupervised is the coordinator restarting the loop after a DIAL failure. This one is
	// easy to miss and is the most actionable of the three: a dial that fails returns to the
	// supervisor, so `apps.connections.open refused: "invalid_auth"` — the difference between a wrong
	// app-level token and a stack still waiting — appears ONLY here, never on a `slack socket:` line.
	// A probe against a real stack with a deliberately bogus token is what surfaced that.
	slackSocketSupervised = `supervised "slack-socket" failed`
)

// readSlackSocketLog reads the control-plane's log for the socket's LATEST state and reports it in
// the operator's terms. The outcomes are distinguished on purpose — reporting a bare "not connected"
// for all of them would send someone hunting for a Slack app problem when the real fault is a
// variable that never reached the container, or hand them "still waiting" when Slack has in fact
// been refusing their token once a second.
func readSlackSocketLog(logs string) (bool, string) {
	last, started := "", false
	for _, line := range strings.Split(logs, "\n") {
		switch i, j := strings.Index(line, slackSocketPrefix), strings.Index(line, slackSocketSupervised); {
		case i >= 0:
			last = strings.TrimSpace(line[i:])
		case j >= 0:
			// The supervisor's own line, which carries the dial reason after "restarting after backoff:".
			last, started = strings.TrimSpace(line[j:]), true
		case strings.Contains(line, slackSocketEnabled):
			started = true
		}
	}
	switch {
	case strings.HasPrefix(last, slackSocketConnected):
		return true, last
	case last != "":
		return false, last
	case started:
		return false, "the connect loop started but Slack has not sent `hello` on this boot"
	default:
		return false, "the control-plane logged nothing about Socket Mode at all: PALAI_SLACK_SOCKET_TEAM_ID reached no container, " +
			"so the connect loop never started (set SLACK_TEAM_ID and SLACK_APP_TOKEN in .env.local and re-run `palai up`)"
	}
}

// slackRegistration builds the POST /v1/slack-connections body from the .env.local values, or
// returns the reason the step is skipped. It NEVER sends a credential: the endpoint refuses inline
// values and accepts only *_ref handles, so a value is read here ONLY to decide whether its handle
// can honestly be registered at all — a handle with no value behind it is worse than an absent one,
// because the row then claims a capability that silently does nothing.
func slackRegistration(get func(string) string) (map[string]any, string) {
	team := strings.TrimSpace(get("SLACK_TEAM_ID"))
	if team == "" {
		return nil, "no SLACK_TEAM_ID in the environment. A Slack workspace needs an app at https://api.slack.com/apps; " +
			"the values and where each is found are in docs/superpowers/plans/phase-19-integration-wiring.md §0.1"
	}
	// The run target (default_policy.agent_revision_id + principal_id) is NOT checked here any more,
	// and its absence is no longer a skip (E21 T2). It used to be both: a missing SLACK_AGENT_REVISION_ID
	// or SLACK_PRINCIPAL_ID made the whole registration skip and `palai up` still exit green — an
	// operator watching a successful install that could not run anything. wireSlack resolves the target
	// against the running stack instead (resolveRunTarget: explicit config wins, otherwise provisioned
	// over the existing API) and fills default_policy in. Nothing here may guess at it.
	//
	// signing_secret_ref is the one handle the API itself requires, and this command no longer
	// registers a handle it cannot store a value for — so an absent signing secret is a skip with
	// its source named, not a 400 the operator has to decode.
	if strings.TrimSpace(get("SLACK_SIGNING_SECRET")) == "" {
		return nil, "SLACK_TEAM_ID is set but SLACK_SIGNING_SECRET is missing. POST /v1/slack-connections requires signing_secret_ref, " +
			"and a handle with no value behind it resolves nowhere (§0.1 — App → Basic Information → App Credentials → Signing Secret)"
	}
	body := map[string]any{"team_id": team}
	// HANDLES, never secrets — and only for the credentials this bring-up can actually store a value
	// for, off the same table slackSecretValues writes from.
	for _, slot := range slackSecretSlots {
		if strings.TrimSpace(get(slot.env)) != "" {
			body[slot.field] = slot.prefix + team
		}
	}
	if v := strings.TrimSpace(get("SLACK_BOT_USER_ID")); v != "" {
		body["bot_user_id"] = v
	}
	// The two SCOPES, and both are absent-by-default on purpose.
	//
	// SLACK_ALLOWED_CHANNELS, not SLACK_TEST_CHANNEL. The latter belongs to the live test harness
	// (tests/live/slack) and means "a channel the bot was invited to so a test can post there"; it used
	// to be written here as allowed_channels, which turned a variable an operator set to make the tests
	// run into a production security scope confining their bot to their test channel. Unset ⇒ the field
	// is not sent at all ⇒ NO channel restriction, which is the production default.
	if v := splitList(get("SLACK_ALLOWED_CHANNELS")); len(v) > 0 {
		body["allowed_channels"] = v
	}
	// allowed_users is deny-by-default server-side, so an unset value really does mean nobody can approve.
	// That is the correct posture and it is not softened here — it is SAID OUT LOUD instead, by
	// slackApproverWarning, in the final report.
	if v := splitList(get("SLACK_APPROVER_IDS")); len(v) > 0 {
		body["allowed_users"] = v
	}
	return body, ""
}

// --- the run target (E21 T2, plan §4 T2 item 2) ----------------------------------------------------

const (
	// bootstrapKeyID is the fixed id of a stack's first API key (identity.firstKey, seeded by
	// ProvisionFirstOrg). It is the key `palai up` itself authenticates with, so its principal is the
	// identity this operator already acts as. install_backup.go pins the same four bootstrap ids.
	bootstrapKeyID = "key_local"
	// slackAgentProfileName is the profile lineage a bring-up provisions when the operator named no
	// SLACK_AGENT_REVISION_ID. Looked up by name so a re-run reuses it instead of piling up lineages.
	slackAgentProfileName = "slack"
)

// slackRunTarget is what a Slack binding must be told before it can admit anything: WHAT to run
// (a published agent revision) and AS WHOM (a principal). resolved is operator-facing prose naming
// the target and where each half came from; it is empty only when the operator configured both.
type slackRunTarget struct {
	revision  string
	principal string
	resolved  string
}

// resolveRunTarget answers the two questions POST /v1/slack-connections refuses a binding without.
//
// It exists because the alternative was worse than an error: a missing SLACK_AGENT_REVISION_ID or
// SLACK_PRINCIPAL_ID used to SKIP the whole registration while `palai up` still exited green, so an
// operator watched a successful install that could never run anything. Neither id is a Slack value —
// they are Palai-side ids no Slack app page will ever hand you — so demanding them from .env.local
// asked the operator for something only the running stack could tell them.
//
// EXPLICIT CONFIGURATION ALWAYS WINS, per id: anything the operator named is used verbatim and
// nothing is provisioned for it. It opens NO new path — both halves come off the existing
// provisioning API (§4 T2), and neither writes a credential.
func resolveRunTarget(api *apiClient, get func(string) string) (slackRunTarget, error) {
	t := slackRunTarget{
		revision:  strings.TrimSpace(get("SLACK_AGENT_REVISION_ID")),
		principal: strings.TrimSpace(get("SLACK_PRINCIPAL_ID")),
	}
	if t.revision != "" && t.principal != "" {
		return t, nil
	}
	var how []string
	if t.principal == "" {
		// The bootstrap key's OWN principal, read back — not a freshly minted one. The only API that
		// creates a principal is POST /v1/api-keys, which also mints a full-scope key whose plaintext is
		// returned once and would then be discarded: a live admin credential nobody holds, left in the
		// database forever, to obtain an id that differs from this one only in its characters. Slack runs
		// therefore admit as the same principal an operator's own API calls do, which is the truth of a
		// single-operator self-host stack. Set SLACK_PRINCIPAL_ID to attribute them to a different one.
		p, err := api.bootstrapPrincipal()
		if err != nil {
			return slackRunTarget{}, err
		}
		t.principal = p
		how = append(how, fmt.Sprintf("as principal %s (this stack's bootstrap-key principal — set SLACK_PRINCIPAL_ID to bind a different one)", p))
	}
	if t.revision == "" {
		tools := slackAgentTools(get)
		rev, minted, err := api.ensureSlackAgentRevision(tools)
		if err != nil {
			return slackRunTarget{}, err
		}
		t.revision = rev
		// What the agent can DO is the part an operator most needs to see, so it is named rather than
		// counted: "2 tools" tells nobody whether they just granted a web fetch or a shell.
		grant := "NO tools — it can answer and nothing else, so its runs are single-step (set SLACK_AGENT_TOOLS to widen)"
		if len(tools) > 0 {
			grant = "tools: " + strings.Join(tools, ", ")
		}
		switch {
		case minted:
			how = append(how, fmt.Sprintf("running agent revision %s, created and published by this bring-up — %s", rev, grant))
		default:
			how = append(how, fmt.Sprintf("running agent revision %s, reused from the %q profile an earlier bring-up created — %s", rev, slackAgentProfileName, grant))
		}
	}
	t.resolved = "Slack run target: " + strings.Join(how, ", ")
	return t, nil
}

// slackSkipWarning turns a registration skip into the report's WARNING section — but only when the
// operator was evidently trying to wire Slack (E21 T2 item 3). A skip inside a green bring-up is how
// an install that does nothing reads as an install that worked.
//
// A stack with no SLACK_TEAM_ID at all is NOT warned about: that operator did not ask for Slack, and
// warning them on every single bring-up is precisely the crying-wolf this same task removes from the
// orphan sweep. The step line still says SKIPPED either way — the warning is the escalation.
func slackSkipWarning(get func(string) string, skip string) string {
	if strings.TrimSpace(get("SLACK_TEAM_ID")) == "" {
		return ""
	}
	return "Slack is configured in this environment but was NOT wired: " + skip +
		". The bring-up above still succeeded, so nothing else will tell you — but no message will reach the agent until this is fixed."
}

// splitList parses a comma-separated operator value: trimmed, empties dropped. nil when nothing is left,
// so a caller can distinguish "not configured" from "configured empty".
func splitList(raw string) []string {
	var out []string
	for _, part := range strings.Split(raw, ",") {
		if p := strings.TrimSpace(part); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// slackApproverWarning is the line an operator would otherwise only learn from a silently refused Approve
// click. ApproverAuthorized is deny-by-default — correctly: a connection that has not been told who may
// approve must not let any member of the workspace authorize a privileged operation. But a `palai up` that
// registers no approver and says nothing leaves a real operator mentioning the bot, clicking Approve, and
// getting NOTHING back, with no surface anywhere that explains it.
//
// Derived from the body actually sent, never from an assumption about what `palai up` usually does.
func slackApproverWarning(body map[string]any) string {
	if users, ok := body["allowed_users"].([]string); ok && len(users) > 0 {
		return ""
	}
	return "slack: registered, but NO approver is allow-listed, so every approve/deny click will be refused; " +
		"set SLACK_APPROVER_IDS=U… (comma-separated Slack user ids) and re-run"
}

// --- the final report ------------------------------------------------------------------------

// capRow is one line of the capability table.
type capRow struct{ name, state, reason string }

// observedFacts collects what THIS bring-up actually observed, keyed by capability name. Only a
// capability with a fact here is called live; everything else is dormant with the tier the
// deployment served as its reason.
func observedFacts(rt roundTrip, slackFact string) map[string]string {
	facts := map[string]string{
		"responses": fmt.Sprintf("real round-trip proven: %s, %d in / %d out", rt.Model, rt.InputTokens, rt.OutputTokens),
	}
	if slackFact != "" {
		facts["slack"] = slackFact
	}
	return facts
}

// capabilityRows derives the status table from the RUNNING stack: the row set is exactly what
// GET /v1/capabilities served, so a capability this binary has never heard of still gets a row and
// one this deployment does not mount gets none.
//
// ponytail: a dormant row's reason names the TIER THE DEPLOYMENT SERVED, not the server-side knob
// that produced it (`workspaces` is "unavailable" because PALAI_WORKSPACE_ROOT is unset — see
// api.workspacesCapability). Copying that name→knob mapping into the CLI would be the hardcoded
// list this table is supposed to avoid, and it is exactly the kind of copy that goes stale when the
// server changes. Upgrade path: have /v1/capabilities serve the reason alongside the tier.
func capabilityRows(caps map[string]string, facts map[string]string) []capRow {
	rows := make([]capRow, 0, len(caps))
	for _, name := range sortedKeys(caps) {
		tier := caps[name]
		if fact, ok := facts[name]; ok && fact != "" {
			rows = append(rows, capRow{name, "live", fmt.Sprintf("%s — %s", tier, fact)})
			continue
		}
		reason := fmt.Sprintf("%s — this bring-up exercised nothing on it", tier)
		if tier == "unavailable" || tier == "disabled" {
			reason = fmt.Sprintf("%s — the deployment itself reports it does not serve this", tier)
		}
		rows = append(rows, capRow{name, "dormant", reason})
	}
	return rows
}

// printReport writes the operator-facing result to stdout. Every line states what was PROVEN, not
// what was attempted — "stack up" is not a proof.
func printReport(cfg Config, rt roundTrip, caps map[string]string, facts map[string]string, red []string, warnings ...string) {
	out := os.Stdout
	fmt.Fprintln(out, "\nPROVEN LIVE")
	fmt.Fprintf(out, "  round-trip   %s -> %s\n", rt.ResponseID, rt.Status)
	fmt.Fprintf(out, "  model        %s   (selector %s — NOT the fake adapter)\n", rt.Model, liveSelector)
	fmt.Fprintf(out, "  usage        %d in / %d out / %d total tokens\n", rt.InputTokens, rt.OutputTokens, rt.TotalTokens)
	fmt.Fprintf(out, "  api          %s\n", cfg.BaseURL)

	fmt.Fprintln(out, "\nCAPABILITIES (from GET /v1/capabilities on this running stack)")
	for _, r := range capabilityRows(caps, facts) {
		fmt.Fprintf(out, "  %-19s %-8s %s\n", r.name, r.state, r.reason)
	}
	if len(red) > 0 {
		fmt.Fprintf(out, "\nNOT green (doctor, not fatal — the round-trip above still ran): %s\n", strings.Join(red, "; "))
	}
	// Registered-but-unusable conditions. They are not failures — the thing was created — but they are
	// exactly the states an operator discovers as unexplained silence, so they are said here in full.
	for _, w := range warnings {
		if w != "" {
			fmt.Fprintf(out, "\nWARNING %s\n", w)
		}
	}
	fmt.Fprintln(out, "\n  palai local doctor      the full health surface")
	fmt.Fprintln(out, "  palai local down        stop the stack, keeping its data")
}

// --- plumbing ------------------------------------------------------------------------------

// apiClient speaks the public API with the bootstrap key. It is the only HTTP in this file; the
// four calls below (create, retrieve, capabilities, slack) all route through it.
type apiClient struct {
	baseURL string
	key     string
	http    *http.Client
}

func (c *apiClient) do(method, path string, body any, out any) (int, error) {
	var payload io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return 0, err
		}
		payload = strings.NewReader(string(raw))
	}
	req, err := http.NewRequest(method, c.baseURL+path, payload)
	if err != nil {
		return 0, err
	}
	req.Header.Set("Authorization", "Bearer "+c.key)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	// POST /v1/responses is idempotency-gated: without this header it answers 400, which reads to a
	// newcomer as "the stack is broken".
	if method == http.MethodPost {
		req.Header.Set("Idempotency-Key", "palai-up-"+randomHex(12))
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return 0, fmt.Errorf("%s %s: %w", method, path, err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if out != nil && len(raw) > 0 {
		if err := json.Unmarshal(raw, out); err != nil {
			return resp.StatusCode, fmt.Errorf("%s %s: decode %d body: %w", method, path, resp.StatusCode, err)
		}
	}
	if resp.StatusCode >= 500 {
		return resp.StatusCode, fmt.Errorf("%s %s = %d: %s", method, path, resp.StatusCode, strings.TrimSpace(string(raw)))
	}
	return resp.StatusCode, nil
}

func (c *apiClient) createResponse(input string) (string, error) {
	var created struct {
		ID string `json:"id"`
	}
	status, err := c.do(http.MethodPost, "/v1/responses", map[string]any{"input": input}, &created)
	if err != nil {
		return "", err
	}
	if status != http.StatusAccepted {
		return "", fmt.Errorf("POST /v1/responses = %d, want 202 (a 400 here is usually the missing Idempotency-Key header)", status)
	}
	return created.ID, nil
}

// awaitTerminal polls the response until it is terminal. A run that never leaves `queued` means the
// stack is dispatch-off, which the message names rather than leaving as a bare timeout.
func (c *apiClient) awaitTerminal(id string, within time.Duration) (roundTrip, error) {
	deadline := time.Now().Add(within)
	var last struct {
		Status string `json:"status"`
		Model  string `json:"model"`
		Usage  struct {
			InputTokens  int `json:"input_tokens"`
			OutputTokens int `json:"output_tokens"`
			TotalTokens  int `json:"total_tokens"`
		} `json:"usage"`
	}
	for {
		// A non-200 must not be polled over: an expired key or a purged response would otherwise
		// read as "still queued" until the deadline and blame dispatch for an auth fault.
		switch status, err := c.do(http.MethodGet, "/v1/responses/"+id, nil, &last); {
		case err != nil:
			return roundTrip{}, err
		case status != http.StatusOK:
			return roundTrip{}, fmt.Errorf("GET /v1/responses/%s = %d, want 200", id, status)
		}
		switch last.Status {
		case "completed", "failed", "canceled":
			return roundTrip{
				ResponseID: id, Status: last.Status, Model: last.Model,
				InputTokens: last.Usage.InputTokens, OutputTokens: last.Usage.OutputTokens, TotalTokens: last.Usage.TotalTokens,
			}, nil
		}
		if time.Now().After(deadline) {
			return roundTrip{}, fmt.Errorf("NOT PROVEN: response %s was still %q after %s. A run stuck in `queued` means the control-plane is "+
				"dispatch-off — fix: PALAI_DISPATCH_WORKERS must be >= 1 when the container starts", id, last.Status, within)
		}
		time.Sleep(time.Second)
	}
}

func (c *apiClient) capabilities() (map[string]string, error) {
	var body struct {
		Capabilities map[string]string `json:"capabilities"`
	}
	status, err := c.do(http.MethodGet, "/v1/capabilities", nil, &body)
	if err != nil {
		return nil, err
	}
	if status != http.StatusOK {
		return nil, fmt.Errorf("GET /v1/capabilities = %d, want 200", status)
	}
	return body.Capabilities, nil
}

func (c *apiClient) slackConnectionCount() (int, error) {
	var body struct {
		Data []json.RawMessage `json:"data"`
	}
	status, err := c.do(http.MethodGet, "/v1/slack-connections", nil, &body)
	if err != nil {
		return 0, err
	}
	if status != http.StatusOK {
		return 0, fmt.Errorf("GET /v1/slack-connections = %d", status)
	}
	return len(body.Data), nil
}

// bootstrapPrincipal reads back the principal behind the stack's bootstrap API key — the identity
// this command is already authenticated as. Metadata only: GET /v1/api-keys/{id} never renders a key.
func (c *apiClient) bootstrapPrincipal() (string, error) {
	var body struct {
		PrincipalID string `json:"principal_id"`
	}
	status, err := c.do(http.MethodGet, "/v1/api-keys/"+bootstrapKeyID, nil, &body)
	switch {
	case err != nil:
		return "", fmt.Errorf("read this stack's own principal: %w", err)
	case status != http.StatusOK:
		return "", fmt.Errorf("GET /v1/api-keys/%s = %d: this stack has no bootstrap key row to take a principal from, "+
			"so a Slack binding cannot be told who to run as — set SLACK_PRINCIPAL_ID to name one", bootstrapKeyID, status)
	case body.PrincipalID == "":
		return "", fmt.Errorf("GET /v1/api-keys/%s returned no principal_id", bootstrapKeyID)
	}
	return body.PrincipalID, nil
}

// ensureSlackAgentRevision returns a PUBLISHED agent revision for the Slack binding to pin, reusing
// the one an earlier bring-up made where possible. minted reports whether THIS call created it.
//
// Published is not a nicety: coordinator's admission verifies published_at before pinning a run, so a
// draft revision would register a binding that admits nothing — today's silent failure with one more
// step in front of it. The reuse lookup is by profile NAME because `palai up` is re-run constantly,
// and a bring-up that minted a fresh lineage each time would move the workspace's binding underneath
// the operator and leave a pile of orphan revisions behind.
func (c *apiClient) ensureSlackAgentRevision(tools []string) (id string, minted bool, err error) {
	profileID, err := c.findAgentProfile(slackAgentProfileName)
	if err != nil {
		return "", false, err
	}
	if profileID != "" {
		if rev, err := c.publishedAgentRevision(profileID, tools); err != nil {
			return "", false, err
		} else if rev != "" {
			return rev, false, nil
		}
	} else {
		var created struct {
			ID string `json:"id"`
		}
		status, err := c.do(http.MethodPost, "/v1/agents", map[string]any{"name": slackAgentProfileName}, &created)
		if err != nil {
			return "", false, fmt.Errorf("create the %q agent profile: %w", slackAgentProfileName, err)
		}
		if status != http.StatusCreated || created.ID == "" {
			return "", false, fmt.Errorf("POST /v1/agents = %d, want 201 with an id", status)
		}
		profileID = created.ID
	}

	// The model and instructions stay EMPTY on purpose: the revision inherits the deployment's model, and
	// guessing an instruction here would bake a `palai up` opinion into every Slack run, invisibly, with the
	// operator having no idea where it came from.
	//
	// TOOLS ARE DIFFERENT, and E21 T4 is where they arrive. A revision with an empty effective tool set is
	// single-step — it can answer, and it can do nothing else — so leaving the list empty was not neutrality,
	// it was a ceiling nobody had chosen either. What IS a real choice is which tools, and the default is
	// deliberately the narrow one:
	//
	//   READ-ONLY BY DEFAULT. A Slack DM is the lowest-friction surface this platform has — anyone in the
	//   workspace can reach it, and E20 granted im:history so the panel conversation works at all. Binding
	//   the workspace and publish tools here would hand every one of those people a shell and a push, as a
	//   standing capability nobody opted into. That is the same class of decision as im:history itself, and
	//   it belongs to the workspace owner, at install time, in the open.
	//
	//   SLACK_AGENT_TOOLS widens it, explicitly and by name. An operator who wants the agent to write code
	//   from Slack sets it — and can see in one line what they granted. Explicit configuration always wins,
	//   the same rule the revision and principal ids follow above.
	//
	// mcp_connections is NOT set, and its absence is the fail-closed default for EXTERNAL tools: a Slack run
	// reaches no MCP server until the operator registers one and names it on the revision. The rider is the
	// capability ceiling (extensions/lookup.go), so an empty one is a run that cannot reach outward at all.
	var rev struct {
		ID string `json:"id"`
	}
	status, err := c.do(http.MethodPost, "/v1/agents/"+profileID+"/revisions", map[string]any{"tools": tools}, &rev)
	if err != nil {
		return "", false, fmt.Errorf("create an agent revision under %s: %w", profileID, err)
	}
	if status != http.StatusCreated || rev.ID == "" {
		return "", false, fmt.Errorf("POST /v1/agents/%s/revisions = %d, want 201 with an id", profileID, status)
	}
	status, err = c.do(http.MethodPost, "/v1/agents/"+profileID+"/revisions/"+rev.ID+"/publish", map[string]any{}, nil)
	if err != nil {
		return "", false, fmt.Errorf("publish agent revision %s: %w", rev.ID, err)
	}
	if status != http.StatusOK {
		return "", false, fmt.Errorf("publish agent revision %s = %d, want 200: a DRAFT revision pins no run, so the "+
			"binding would admit nothing", rev.ID, status)
	}
	return rev.ID, true, nil
}

// findAgentProfile returns the id of the profile lineage with this name, or "" when there is none.
func (c *apiClient) findAgentProfile(name string) (string, error) {
	var page struct {
		Data []struct {
			ID   string `json:"id"`
			Name string `json:"name"`
		} `json:"data"`
	}
	status, err := c.do(http.MethodGet, "/v1/agents", nil, &page)
	if err != nil {
		return "", fmt.Errorf("list agent profiles: %w", err)
	}
	if status != http.StatusOK {
		return "", fmt.Errorf("GET /v1/agents = %d, want 200", status)
	}
	for _, p := range page.Data {
		if p.Name == name {
			return p.ID, nil
		}
	}
	return "", nil
}

// publishedAgentRevision returns a published revision of this profile, or "" when it has none.
// It reuses a published revision ONLY when that revision already carries the wanted tool list. Reusing one
// with a different list would make SLACK_AGENT_TOOLS silently inert on every bring-up after the first — the
// operator would set it, see a green `palai up`, and get the old toolless revision anyway. That is the same
// silent-skip shape E21 T2 removed from this file, so it is not reintroduced one function down.
func (c *apiClient) publishedAgentRevision(profileID string, wantTools []string) (string, error) {
	var page struct {
		Data []struct {
			ID     string   `json:"id"`
			Status string   `json:"status"`
			Tools  []string `json:"tools"`
		} `json:"data"`
	}
	status, err := c.do(http.MethodGet, "/v1/agents/"+profileID+"/revisions", nil, &page)
	if err != nil {
		return "", fmt.Errorf("list revisions of %s: %w", profileID, err)
	}
	if status != http.StatusOK {
		return "", fmt.Errorf("GET /v1/agents/%s/revisions = %d, want 200", profileID, status)
	}
	for _, r := range page.Data {
		if r.Status == "published" && sameTools(r.Tools, wantTools) {
			return r.ID, nil
		}
	}
	return "", nil
}

// sameTools compares two tool lists as SETS: the API's order is not a promise, and an operator who reorders
// SLACK_AGENT_TOOLS has not asked for a new revision.
func sameTools(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	seen := make(map[string]int, len(a))
	for _, t := range a {
		seen[t]++
	}
	for _, t := range b {
		seen[t]--
		if seen[t] < 0 {
			return false
		}
	}
	return true
}

// slackAgentTools is the tool list bound to the revision `palai up` creates. The default is READ-ONLY on
// purpose (see ensureSlackAgentRevision); SLACK_AGENT_TOOLS replaces it wholesale, by name, so what an
// operator granted is legible in one line rather than assembled from a default plus a delta.
//
// "Give this agent nothing" is a real posture and needs to stay reachable, so it has an explicit spelling:
// SLACK_AGENT_TOOLS=none. Blank is treated as unset, because a variable that got emptied by a stray line in
// .env.local should not silently disarm the agent.
func slackAgentTools(get func(string) string) []string {
	raw := strings.TrimSpace(get("SLACK_AGENT_TOOLS"))
	if raw == "" {
		return slackDefaultTools
	}
	if strings.EqualFold(raw, "none") {
		return nil
	}
	var out []string
	for _, name := range strings.Split(raw, ",") {
		if name = strings.TrimSpace(name); name != "" {
			out = append(out, name)
		}
	}
	return out
}

// slackDefaultTools: read-only, side-effect-free, and functional on a plain compose stack — no workspace
// root, no repository, no sandbox driver required. palai.research.fetch carries its own egress guards;
// palai.knowledge.retrieve is ACL-first and fail-closed to KB-wide sources.
var slackDefaultTools = []string{"palai.research.fetch", "palai.knowledge.retrieve"}

// putSecretRef stores one credential VALUE under its handle through the E13 T3 secret store — the
// SAME write-path a production tenant uses, envelope-encrypted at rest and resolved fresh, so the
// resolver chain that fronts every consumer (dbSecret) redeems the handle with no restart.
//
// The value travels in a POST body over loopback and nowhere else: not argv (which `ps` shows), not
// an env var (which `docker inspect` shows), not a log, not a file this command leaves behind. It
// is write-only at the far end too — every projection the API serves is metadata, so nothing can
// read it back out.
func (c *apiClient) putSecretRef(name, value string) error {
	status, err := c.do(http.MethodPost, "/v1/secret-refs", map[string]any{"name": name, "value": value}, nil)
	switch {
	case err != nil:
		// c.do builds its error from the method, path and response body — never from the request.
		return fmt.Errorf("store the secret ref %q: %w", name, err)
	case status == http.StatusCreated:
		return nil
	case status == http.StatusNotFound:
		return fmt.Errorf("POST /v1/secret-refs = 404: the control-plane booted with no secret store, so the handle %q could resolve nowhere. "+
			"Fix: PALAI_SECRET_MASTER_KEY_FILE must name a 32-byte hex key file when the container starts (a running one never re-reads it)", name)
	case status == http.StatusForbidden:
		return fmt.Errorf("POST /v1/secret-refs = 403: this API key lacks the `provision` capability, so it cannot store the handle %q", name)
	default:
		return fmt.Errorf("POST /v1/secret-refs = %d storing the handle %q, want 201", status, name)
	}
}

func (c *apiClient) createSlackConnection(body map[string]any) (string, int, error) {
	var created struct {
		ID     string `json:"id"`
		Detail string `json:"detail"`
	}
	status, err := c.do(http.MethodPost, "/v1/slack-connections", body, &created)
	if err != nil {
		return "", status, err
	}
	switch status {
	case http.StatusCreated, http.StatusConflict:
		return created.ID, status, nil
	default:
		return "", status, fmt.Errorf("POST /v1/slack-connections = %d: %s", status, created.Detail)
	}
}

// waitHealthy re-runs DOCTOR'S OWN checks until they are all green or they stop improving. It
// invents no check and no threshold — runChecks is the same function `palai local doctor` calls.
//
// It gives up early when a round changes nothing, because some checks are never going to go green
// by waiting: the disk check fails on a host under 10% free, and blocking the live proof behind a
// full timeout for a condition that is not about readiness would tax every bring-up on such a host.
func waitHealthy(cfg Config, p paths, within time.Duration) Report {
	deadline := time.Now().Add(within)
	previous, stable := "", 0
	for {
		report := runChecks(cfg, p)
		red := strings.Join(redChecks(report), "|")
		if red == previous {
			stable++
		} else {
			previous, stable = red, 0
		}
		// Three identical rounds (~6s): long enough for a runner still finishing its compose-mTLS
		// enrolment to turn green, short enough that a permanently-red check costs seconds.
		if report.OK || stable >= 3 || time.Now().After(deadline) {
			return report
		}
		time.Sleep(2 * time.Second)
	}
}

// redChecks names every check that is not green, with its detail — a name alone is not a fix.
func redChecks(r Report) []string {
	var out []string
	for _, name := range sortedKeys(r.Checks) {
		if c := r.Checks[name]; c.Status != "ok" {
			out = append(out, fmt.Sprintf("%s: %s", name, c.Detail))
		}
	}
	return out
}

// loadEnvFile reads a dotenv-style file. A missing file is NOT an error — the command then runs on
// the process environment alone, which is what `set -a; . ./.env.local` already gives it.
func loadEnvFile(path string) (map[string]string, bool, error) {
	f, err := os.Open(path)
	if os.IsNotExist(err) {
		return map[string]string{}, false, nil
	}
	if err != nil {
		return nil, false, fmt.Errorf("open %s: %w", path, err)
	}
	defer f.Close()
	env, err := parseEnv(f)
	return env, true, err
}

// parseEnv handles the shape `.env.local` actually has: KEY=VALUE, optional `export`, # comments,
// optional surrounding quotes.
//
// ponytail: no interpolation, no multi-line values, no escape sequences. The documented way to load
// this file is `set -a; . ./.env.local`, and every value it holds is a flat opaque credential.
func parseEnv(r io.Reader) (map[string]string, error) {
	out := map[string]string{}
	sc := bufio.NewScanner(r)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		line = strings.TrimPrefix(line, "export ")
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		k, v = strings.TrimSpace(k), strings.TrimSpace(v)
		if len(v) > 1 && (v[0] == '"' && v[len(v)-1] == '"' || v[0] == '\'' && v[len(v)-1] == '\'') {
			v = v[1 : len(v)-1]
		}
		if k != "" {
			out[k] = v
		}
	}
	return out, sc.Err()
}

// lookup resolves a key from the process environment FIRST, then the dotenv file — an explicitly
// exported value always beats a file the operator may have forgotten about.
func lookup(fileEnv map[string]string) func(string) string {
	return func(k string) string {
		if v := os.Getenv(k); v != "" {
			return v
		}
		return fileEnv[k]
	}
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

// sortedKeys keeps every rendered map (env key names, doctor checks, capabilities) in a stable order.
func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
