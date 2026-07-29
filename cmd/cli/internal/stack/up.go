package stack

import (
	"bufio"
	"bytes"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
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
//
// native selects the E22 T5 posture: the control plane runs as a PROCESS ON THIS MACHINE and only
// Postgres, the object store and the runner stay in Docker (native.go). Everything after the
// bring-up is identical — the same doctor checks, the same live round-trip, the same report — which
// is the point: a Mac deployment is a deployment, not a second product.
func Bootstrap(envFile string, native bool) error {
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
	if err := applyConcurrencyEnv(get); err != nil {
		return err
	}

	// The Slack knobs the CONTAINER needs must be exported before compose creates it: a running
	// control-plane never re-reads its environment, so a socket switched on after the fact stays off
	// until the next bring-up. Failures here are reported and carried — an unusable master key must
	// not fail a bring-up whose model round-trip is about to be proven.
	for _, w := range applySlackEnv(p, get) {
		fmt.Fprintf(os.Stderr, "        WARNING %s\n", w)
	}
	// The publication destination's CREDENTIAL, for the same reason and at the same moment: an approved
	// push publishes through a GitHub App the control-plane resolves at boot, so a stack brought up without
	// one has an approval chain that ends in silence (see applyGitHubAppEnv).
	for _, w := range applyGitHubAppEnv(p, get) {
		fmt.Fprintf(os.Stderr, "        WARNING %s\n", w)
	}
	posture := "container control plane (docker compose)"
	if native {
		fmt.Fprintln(os.Stderr, "[3/6] stack     NATIVE: postgres + object-store + runner in docker, control plane on this machine")
		if posture, err = UpNative(get); err != nil {
			return err
		}
	} else {
		fmt.Fprintln(os.Stderr, "[3/6] stack     docker compose up (this builds on a first run)")
		if err := Up(); err != nil {
			return err
		}
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
	slackFact, slackLine, slackWarns := wireSlack(api, cfg, p, get)
	fmt.Fprintf(os.Stderr, "[6/6] slack     %s\n", slackLine)

	caps, err := api.capabilities()
	if err != nil {
		return fmt.Errorf("read /v1/capabilities for the status table: %w", err)
	}
	if native {
		// A control plane on a Mac with no declared shell posture has a nil shell runner, so the
		// whole reason for running natively — the host's own toolchain — is silently absent.
		slackWarns = appendWarn(slackWarns, nativeShellWarning(get))
	}
	// WHO MAY APPROVE (E23 T2). Unconditional, not Slack-conditional: the HTTP approve surface exists on
	// every stack whether or not a workspace was ever wired.
	slackWarns = appendWarn(slackWarns, approverListWarning(api))
	// HOW MANY RUNS AT ONCE (§3.6 D11). Unconditional for the same reason: a second machine that nothing
	// can reach is the shape of a fleet that was bought and is not being used.
	slackWarns = appendWarn(slackWarns, dispatchWorkerFleetWarning(api, get))
	printReport(cfg, posture, rt, caps, observedFacts(rt, slackFact), red, slackWarns...)
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

// containerGitHubAppKeyPath is where compose mounts the GitHub App private key file secret. compose.yaml
// writes it LITERALLY, the way it writes the master key's path: the value an operator puts in .env.local is
// a HOST path, and interpolating a host path into a container is how you get a control-plane looking for a
// key at a name that means something else on the other side of the mount.
const containerGitHubAppKeyPath = "/run/secrets/github_app_key"

// gitHubAppKeySlot is the file-secret source compose bind-mounts the key from.
const gitHubAppKeySlot = "github-app-key"

// applyGitHubAppEnv wires the GitHub App the approval pump publishes THROUGH (§0.2), and removes the
// silence around it.
//
// THE SILENCE IT REMOVES, because it is the whole reason this function exists rather than three more lines
// in compose: repositoryPublisherFromEnv (main.go) returns nil when any of the three variables is missing,
// and a nil publisher makes pumpApprovedPublications a no-op. So an operator could grant the publish tools,
// watch the agent propose a push, press Approve, see the message repaired to "Approved: push agent/… -> …"
// — and nothing would ever happen. Not an error, not a log line the operator sees, not a retry: an approved
// row sitting in the database forever. That is E21 T2's silent-skip in its most expensive form, because
// this one has a human believing they authorized something.
//
// WHAT RIDES WHERE. The App id and the installation id are identifiers, so they ride the compose
// environment like every other knob. The PRIVATE KEY does not: its bytes are copied into the 0600
// $PALAI_HOME/secrets slot compose mounts as a file secret, and what the control-plane reads from the
// environment is a PATH. The key value therefore appears in no `docker inspect`, no `compose config`, no
// argv and no log — the same posture the provider credential and the master key already have, and the
// reason main.go can say it "only mints short-lived scoped tokens against it".
//
// It runs BEFORE `docker compose up` for the reason applySlackEnv gives: a running control-plane never
// re-reads its environment.
func applyGitHubAppEnv(p paths, get func(string) string) []string {
	appID := strings.TrimSpace(get("PALAI_GITHUB_APP_ID"))
	installID := strings.TrimSpace(get("PALAI_GITHUB_APP_INSTALLATION_ID"))
	keyFile := strings.TrimSpace(get("PALAI_GITHUB_APP_PRIVATE_KEY_FILE"))

	if appID == "" && installID == "" && keyFile == "" {
		// Nothing configured. Warn ONLY when this stack is about to grant the publish tools — an operator who
		// bound no repository did not ask to publish anything, and warning them on every bring-up is the
		// crying-wolf `palai up` already refuses elsewhere.
		if strings.TrimSpace(get("PALAI_GIT_CLONE_URL")) == "" || strings.TrimSpace(get("PALAI_GIT_BASE_BRANCH")) == "" {
			return nil
		}
		return []string{"a repository is bound but NO GitHub App is configured, so the agent can propose a push " +
			"and a human can approve it and NOTHING WILL HAPPEN: the approved publication waits forever with no " +
			"error anywhere. Fix: set PALAI_GITHUB_APP_ID, PALAI_GITHUB_APP_INSTALLATION_ID and " +
			"PALAI_GITHUB_APP_PRIVATE_KEY_FILE in .env.local (§0.2) and re-run `palai up`"}
	}
	if missing := missingNames(map[string]string{
		"PALAI_GITHUB_APP_ID": appID, "PALAI_GITHUB_APP_INSTALLATION_ID": installID,
		"PALAI_GITHUB_APP_PRIVATE_KEY_FILE": keyFile,
	}); len(missing) > 0 {
		return []string{"the GitHub App is HALF configured (" + strings.Join(missing, " and ") + " unset), so no " +
			"publisher is wired at all and an approved push would wait forever. All three are required together"}
	}

	// The key's BYTES, copied into the slot compose mounts. Read failures are warnings rather than fatal: a
	// bring-up whose model round-trip is about to be proven must not die on a publication path.
	keyPEM, err := os.ReadFile(keyFile)
	if err != nil {
		return []string{fmt.Sprintf("PALAI_GITHUB_APP_PRIVATE_KEY_FILE=%s could not be read (%v), so no publisher "+
			"is wired and an approved push would wait forever", keyFile, err)}
	}
	if len(bytes.TrimSpace(keyPEM)) == 0 {
		return []string{"PALAI_GITHUB_APP_PRIVATE_KEY_FILE=" + keyFile + " is empty, so no publisher is wired and " +
			"an approved push would wait forever"}
	}
	if err := os.WriteFile(p.secretPath(gitHubAppKeySlot), keyPEM, 0o600); err != nil {
		return []string{fmt.Sprintf("could not stage the GitHub App key into %s (%v), so no publisher is wired",
			p.secretPath(gitHubAppKeySlot), err)}
	}

	var warnings []string
	exports := map[string]string{
		"PALAI_GITHUB_APP_ID":               appID,
		"PALAI_GITHUB_APP_INSTALLATION_ID":  installID,
		"PALAI_GITHUB_APP_PRIVATE_KEY_FILE": containerGitHubAppKeyPath,
	}
	// owner/repo is what the PULL REQUEST half needs (main.go builds the PR client from it); without it a
	// push still publishes and every pull request answers "no pull-request client wired". §0.2 asks the
	// operator for PALAI_GIT_REPO, which is also what the repository binding uses, so the two cannot drift.
	if slug := repositorySlug(get); slug != "" {
		exports["PALAI_GITHUB_REPO"] = slug
	} else {
		warnings = append(warnings, "the GitHub App is configured but owner/repo is not, so a push will publish and "+
			"every pull request will answer 'no pull-request client wired'. Fix: set PALAI_GIT_REPO=owner/repo")
	}
	for name, value := range exports {
		if err := os.Setenv(name, value); err != nil {
			warnings = append(warnings, fmt.Sprintf("could not export %s: %v", name, err))
		}
	}
	return warnings
}

// missingNames returns the names whose value is empty, sorted so the warning reads the same every run.
func missingNames(values map[string]string) []string {
	var missing []string
	for name, v := range values {
		if v == "" {
			missing = append(missing, name)
		}
	}
	sort.Strings(missing)
	return missing
}

// repositorySlug is owner/repo for the PR client: PALAI_GIT_REPO if the operator set it (§0.2), otherwise
// derived from the clone URL the SAME way the repository binding derives its identity — one value, one
// derivation, so the App and the binding can never point at two different repositories.
func repositorySlug(get func(string) string) string {
	if slug := strings.TrimSpace(get("PALAI_GIT_REPO")); strings.Contains(slug, "/") {
		return slug
	}
	if cloneURL := strings.TrimSpace(get("PALAI_GIT_CLONE_URL")); cloneURL != "" {
		if slug := repositoryIdentityFrom(cloneURL); strings.Contains(slug, "/") {
			return slug
		}
	}
	return ""
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
// It returns (fact, line, warns). fact is the observed-state string the capability table shows for
// `slack`, line is the operator-facing step result, and warns are non-fatal conditions the final
// report must repeat because the operator would otherwise only meet them as silence.
//
// warns is a SEPARATE return rather than more prose on line, and that separation is the point: they
// carry different truths about the same connection — whether anything can ARRIVE from Slack (the
// socket, on line), whether anything can be APPROVED once it does (the allow-list), and whether the
// agent can touch CODE at all (the repository binding, E22 T3). Folding one into the other would let
// a connected socket read as a working integration while every Approve click is silently refused,
// which is the same over-claiming in a new place.
func wireSlack(api *apiClient, cfg Config, p paths, get func(string) string) (fact string, line string, warns []string) {
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
		return fact, line, appendWarn(nil, slackSkipWarning(get, skip))
	}
	team, _ := body["team_id"].(string)
	o := slackOutcome{team: team}

	// AGAINST WHAT REPOSITORY (E22 T3). It runs BEFORE resolveRunTarget because the answer decides the tool
	// list: the workspace tools are bound only when a repository actually exists to use them on. Absent
	// configuration is a WARNING rather than a silent skip — see resolveRepository.
	repo := resolveRepository(api, get)
	if repo.resolved != "" {
		fmt.Fprintf(os.Stderr, "        %s\n", repo.resolved)
	}
	warns = appendWarn(warns, repo.warn)

	// WHAT to run and AS WHOM. Resolved against the running stack (explicit config wins, otherwise
	// provisioned) rather than demanded from .env.local — see resolveRunTarget. It runs AFTER the
	// team/signing checks above so a stack with no Slack app provisions nothing at all.
	target, err := resolveRunTarget(api, get, repo.id != "")
	if err != nil {
		o.problem = "NOT registered: " + err.Error()
		fact, line = slackReport(o)
		return fact, line, warns
	}
	policy := map[string]any{"agent_revision_id": target.revision, "principal_id": target.principal}
	if repo.id != "" {
		// The repository rides the CONNECTION, exactly like the revision and the principal beside it: every
		// event on this workspace admits against this binding, and no Slack payload can name a different one.
		policy["repository_binding_id"] = repo.id
	}
	body["default_policy"] = policy
	if target.resolved != "" {
		fmt.Fprintf(os.Stderr, "        %s\n", target.resolved)
	}

	// The VALUES first: a connection registered against handles that resolve nowhere is precisely
	// the state this task exists to end, so the redemption path is filled before the row names it.
	for name, value := range slackSecretValues(get) {
		if err := api.putSecretRef(name, value); err != nil {
			// No connection row was created, so there is no allow-list to warn about yet. The repository
			// warning still rides: it is about what this stack CAN do, not about a row that got written.
			o.problem = "NOT wired: " + err.Error()
			fact, line = slackReport(o)
			return fact, line, warns
		}
		o.stored++
	}

	id, status, err := api.createSlackConnection(body)
	switch {
	case err != nil:
		o.problem = "NOT registered: " + err.Error()
		fact, line = slackReport(o)
		return fact, line, warns
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
		warns = appendWarn(warns, slackApproverWarning(body))
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
			return fact, line, warns
		}
		o.connected, o.detail = observeSlackSocket(cfg, p, 45*time.Second)
	}
	// warns ride alongside whatever the socket turned out to be. A connected socket does not retire
	// them — that is the case they matter MOST in, because the events now arriving are the ones whose
	// Approve buttons will be refused, or whose agent has no repository to work in.
	fact, line = slackReport(o)
	return fact, line, warns
}

// appendWarn adds a non-empty warning to the list. Empty warnings are dropped here rather than at every
// call site, because a "" in the report renders as a blank WARNING block.
func appendWarn(warns []string, w string) []string {
	if w == "" {
		return warns
	}
	return append(warns, w)
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
	if err := runVisible(cfg.composeEnv(p.home, digest), "docker", "compose", "-p", cfg.Project, "-f", p.compose,
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
			"-f", p.compose, "logs", "--no-color", "control-plane")
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
	// bootstrapProjectID is the project that same bootstrap key is scoped to (identity.firstProject).
	// It is the project whose approver list `palai up` reports on, because it is the only one this
	// command's own credential can read.
	bootstrapProjectID = "prj_local"
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
func resolveRunTarget(api *apiClient, get func(string) string, hasRepository bool) (slackRunTarget, error) {
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
		tools := slackAgentTools(get, hasRepository)
		mcp := slackAgentMCP(get)
		rev, minted, err := api.ensureSlackAgentRevision(tools, mcp)
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
		// The MCP rider is said in the SAME line and with the same discipline, because reaching a
		// third-party server is a grant of exactly the kind an operator must be able to read off the
		// bring-up. The empty case is stated rather than omitted: "the agent cannot reach Jira" is
		// otherwise discovered as an agent that answers as if Jira does not exist.
		grant += "; " + slackMCPGrant(mcp)
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

// slackRepository is the repository this bring-up bound to the Slack connection, or the reason it bound
// none. id empty means the agent can talk and search and cannot touch code — a legitimate posture, and the
// one every stack had before E22.
type slackRepository struct {
	id       string
	resolved string // operator-facing prose: what was created or reused, printed as it happens
	warn     string // a condition the final report must repeat, because its only other symptom is silence
}

// resolveRepository creates (or reuses) the repository binding a Slack thread codes against, from the two
// values §0.2 asks the operator for.
//
// IT WARNS RATHER THAN SKIPPING SILENTLY, and that is E21 T2's lesson arriving in the same file for the
// second time: a bring-up that quietly does nothing is indistinguishable from one that worked. An operator
// who wired Slack and expected the agent to write code would otherwise meet the gap only as an agent that
// keeps answering "no workspace bound for this run", with no line anywhere connecting that to a variable
// they never set.
//
// ponytail: no PALAI_REPOSITORY_BINDING_ID override. Every other id in this file has one because the
// operator may already hold it; a binding is created BY this command, so an operator holding one can pass
// its clone_url and get it reused by the lookup below. Add the override when someone has a binding they
// cannot describe.
func resolveRepository(api *apiClient, get func(string) string) slackRepository {
	cloneURL := strings.TrimSpace(get("PALAI_GIT_CLONE_URL"))
	branch := strings.TrimSpace(get("PALAI_GIT_BASE_BRANCH"))
	switch {
	case cloneURL == "" && branch == "":
		return slackRepository{warn: "no repository is bound to this Slack workspace: PALAI_GIT_CLONE_URL and " +
			"PALAI_GIT_BASE_BRANCH are unset, so the agent can read and search but CANNOT clone, edit or commit " +
			"anything. Its tool list is the read-only three; set both in .env.local and re-run `palai up` to bind one."}
	case cloneURL == "":
		return slackRepository{warn: "PALAI_GIT_BASE_BRANCH is set but PALAI_GIT_CLONE_URL is not: a binding needs " +
			"BOTH — the clone URL says WHICH repository, the base branch says which branch a pull request will " +
			"target. No repository was bound, and the agent keeps the read-only tool list."}
	case branch == "":
		return slackRepository{warn: "PALAI_GIT_CLONE_URL is set but PALAI_GIT_BASE_BRANCH is not: a binding needs " +
			"BOTH, and the base branch is not a default worth guessing — it is the branch every pull request from " +
			"this workspace will target. No repository was bound."}
	}
	// The provider repository identity is the authoritative name (spec §30.1); the clone URL is not trusted as
	// identity. PALAI_GIT_REPO is what §0.2 asks for; deriving it from the URL is the fallback so an operator
	// who set only the URL still gets a binding rather than a 400.
	identity := strings.TrimSpace(get("PALAI_GIT_REPO"))
	if identity == "" {
		identity = repositoryIdentityFrom(cloneURL)
	}
	id, reused, err := api.ensureRepositoryBinding(identity, cloneURL, branch)
	if err != nil {
		return slackRepository{warn: "no repository was bound: " + err.Error() +
			". The Slack agent keeps the read-only tool list until this is fixed."}
	}
	verb := "registered"
	if reused {
		verb = "reused"
	}
	return slackRepository{
		id: id,
		// The BASE BRANCH is named because it is the whole of "open the PR against dev": the publication
		// destination is resolved from this binding's default_branch and is never asked of the model.
		resolved: fmt.Sprintf("Slack repository: %s %s for %s (clone %s, base branch %s — a pull request from this "+
			"workspace targets %s, and no model chooses that)", verb, id, identity, cloneURL, branch, branch),
	}
}

// repositoryIdentityFrom derives owner/repo from a clone URL. A best-effort fallback for the operator who
// set only PALAI_GIT_CLONE_URL; PALAI_GIT_REPO overrides it. Returns the trimmed path, or the URL itself
// when it parses to nothing — the store requires a non-empty identity and an honest echo beats an empty one.
func repositoryIdentityFrom(cloneURL string) string {
	u, err := url.Parse(cloneURL)
	if err != nil {
		return cloneURL
	}
	if path := strings.TrimSuffix(strings.Trim(u.Path, "/"), ".git"); path != "" {
		return path
	}
	return cloneURL
}

// ensureRepositoryBinding returns the id of a binding for this clone URL + base branch, reusing one this
// project already has. Reuse is by (clone_url, default_branch) because those two ARE the binding an operator
// described in .env.local: `palai up` is re-run constantly, and a fresh binding each time would leave a pile
// of duplicates and — worse — move the connection's repository underneath a thread mid-conversation.
//
// It matches on default_branch too rather than clone_url alone, so an operator who retargets
// PALAI_GIT_BASE_BRANCH from `main` to `dev` gets a binding that says `dev` instead of a silently inert
// change. Same discipline publishedAgentRevision applies to the tool list, one function up.
func (c *apiClient) ensureRepositoryBinding(identity, cloneURL, branch string) (id string, reused bool, err error) {
	var page struct {
		Data []struct {
			ID            string `json:"id"`
			CloneURL      string `json:"clone_url"`
			DefaultBranch string `json:"default_branch"`
		} `json:"data"`
	}
	status, err := c.do(http.MethodGet, "/v1/repository-bindings", nil, &page)
	switch {
	case err != nil:
		return "", false, fmt.Errorf("list repository bindings: %w", err)
	case status == http.StatusNotFound:
		return "", false, fmt.Errorf("GET /v1/repository-bindings = 404: this control-plane mounts no repository " +
			"binding surface, so a repository cannot be bound to a Slack workspace on this stack")
	case status != http.StatusOK:
		return "", false, fmt.Errorf("GET /v1/repository-bindings = %d, want 200", status)
	}
	for _, b := range page.Data {
		if b.CloneURL == cloneURL && b.DefaultBranch == branch {
			return b.ID, true, nil
		}
	}
	var created struct {
		ID     string `json:"id"`
		Detail string `json:"detail"`
	}
	// allowed_operations is left empty on purpose and it is NOT a permission grant this omits: nothing in the
	// tree reads that column today (it is stored and projected, never evaluated), so writing a list here would
	// be a security-shaped string that enforces nothing. What actually bounds this agent is the tool list.
	status, err = c.do(http.MethodPost, "/v1/repository-bindings", map[string]any{
		"provider":            "github",
		"repository_identity": identity,
		"clone_url":           cloneURL,
		"default_branch":      branch,
	}, &created)
	switch {
	case err != nil:
		return "", false, fmt.Errorf("register the repository binding: %w", err)
	case status != http.StatusCreated || created.ID == "":
		return "", false, fmt.Errorf("POST /v1/repository-bindings = %d, want 201 with an id: %s", status, created.Detail)
	}
	return created.ID, false, nil
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

// approverListWarning is the platform-level sibling of slackApproverWarning below, and it warns about the
// OPPOSITE failure. That one exists because a connection with no allowed_users refuses everything in
// silence; this one exists because a project with no approvers list refuses NOTHING — config_policy.approvers
// is unrestricted when absent, deliberately (see coordinator.ConfigPolicy.ApproverAllowed: a control that
// changes behaviour for deployments that never asked for it is an outage). So the open door is the default,
// and the only honest thing to do is say so out loud rather than let an operator discover it.
//
// It names what is actually open, not a generality: the HTTP approve surface. Slack clicks are still gated
// by the connection's own allowed_users, which is why that half is not claimed here.
//
// Derived from what the running stack ACTUALLY serves — GET /v1/projects/{id} — never from an assumption
// about what `palai up` usually writes. A read that fails warns about the read, because "I could not tell"
// and "there is no list" are different facts and only one of them is about approvals.
func approverListWarning(api *apiClient) string {
	approvers, err := api.projectApprovers(bootstrapProjectID)
	if err != nil {
		return fmt.Sprintf("could not read %s's approver list, so nothing here can tell you whether the approve surfaces are gated: %v",
			bootstrapProjectID, err)
	}
	if len(approvers) > 0 {
		return ""
	}
	return "no project approver list is configured; the HTTP approve surface is gated only by tenant-scoped key possession — " +
		"set one with `palai admin project set-policy " + bootstrapProjectID + " --approvers key:<api_key_id>,slack:<team_id>:<user_id>` " +
		"(see docs/operations/approvals.md). Leaving it unset is a supported posture, not a bug — it is just not a gate"
}

// dispatchWorkerFleetWarning is §3.6 D11 said out loud, and it is the correction of a MEASURED LIE
// rather than a code change.
//
// Concurrency here is min(PALAI_DISPATCH_WORKERS, the fleet's lease slots), and the FIRST of those
// defaults to 1. So an operator who enrols a second Mac adds a machine that parks and is never reached,
// sees no speed-up, and has nowhere to read why: the bound is in the control plane, not in the fleet.
// That matters more now than when it was measured, because it has since been measured that two sessions
// DO run concurrently on one Mac once both knobs are raised — the ceiling is real and it is liftable.
//
// It reads the live registry rather than counting containers, because "enrolled" is a durable fact about
// machines that may not be on this host at all, which is the entire point of a fleet. Read failures are
// reported as such: a warning that silently becomes "no warning" when a read fails is worse than none.
func dispatchWorkerFleetWarning(api *apiClient, get func(string) string) string {
	// Empty follows applyConcurrencyEnv, which exports 1 when the operator set nothing — so the default
	// stack is exactly the case this warning is for.
	workers := strings.TrimSpace(get("PALAI_DISPATCH_WORKERS"))
	if workers == "" {
		workers = "1"
	}
	if workers != "1" {
		return ""
	}
	runners, err := api.enrolledRunnerCount()
	if err != nil {
		return fmt.Sprintf("could not read the enrolled runner count, so nothing here can tell you whether your fleet is reachable: %v", err)
	}
	if runners < 2 {
		return ""
	}
	return fmt.Sprintf("%d runners are enrolled but PALAI_DISPATCH_WORKERS=1 — concurrent runs are bounded by the control plane, not the fleet; "+
		"raise PALAI_DISPATCH_WORKERS (and PALAI_RUNNER_CONCURRENCY, which `palai up` keeps in step with it) or the extra machines park and are never reached", runners)
}

// enrolledRunnerCount reads how many machines are enrolled in the caller's tenant. The page limit is
// deliberately small: the question is "more than one", not "how many exactly", and a fleet of a hundred
// would answer it on the first page.
func (c *apiClient) enrolledRunnerCount() (int, error) {
	var body struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	status, err := c.do(http.MethodGet, "/v1/runners?limit=10", nil, &body)
	switch {
	case err != nil:
		return 0, err
	case status == http.StatusNotFound:
		// A stack with no runner gateway does not mount the route. That is a supported posture (a
		// queued-only stack), and it has no fleet to warn about.
		return 0, nil
	case status != http.StatusOK:
		return 0, fmt.Errorf("GET /v1/runners = %d", status)
	}
	return len(body.Data), nil
}

// projectApprovers reads a project's configured approver principals off the running stack.
func (c *apiClient) projectApprovers(projectID string) ([]string, error) {
	var body struct {
		ConfigPolicy struct {
			Approvers []string `json:"approvers"`
		} `json:"config_policy"`
	}
	status, err := c.do(http.MethodGet, "/v1/projects/"+projectID, nil, &body)
	switch {
	case err != nil:
		return nil, err
	case status != http.StatusOK:
		return nil, fmt.Errorf("GET /v1/projects/%s = %d", projectID, status)
	}
	return body.ConfigPolicy.Approvers, nil
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
func printReport(cfg Config, posture string, rt roundTrip, caps map[string]string, facts map[string]string, red []string, warnings ...string) {
	out := os.Stdout
	fmt.Fprintln(out, "\nPROVEN LIVE")
	fmt.Fprintf(out, "  round-trip   %s -> %s\n", rt.ResponseID, rt.Status)
	fmt.Fprintf(out, "  model        %s   (selector %s — NOT the fake adapter)\n", rt.Model, liveSelector)
	fmt.Fprintf(out, "  usage        %d in / %d out / %d total tokens\n", rt.InputTokens, rt.OutputTokens, rt.TotalTokens)
	fmt.Fprintf(out, "  api          %s\n", cfg.BaseURL)
	// WHICH POSTURE THIS BRING-UP ACTUALLY BROUGHT UP. Both postures serve the same API on the same
	// port, so nothing else on this report distinguishes a control plane in a container from one on
	// this machine — and the difference is the whole of whether `xcodebuild` is reachable.
	fmt.Fprintf(out, "  posture      %s\n", posture)

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
func (c *apiClient) ensureSlackAgentRevision(tools, mcp []string) (id string, minted bool, err error) {
	profileID, err := c.findAgentProfile(slackAgentProfileName)
	if err != nil {
		return "", false, err
	}
	if profileID != "" {
		if rev, err := c.publishedAgentRevision(profileID, tools, mcp); err != nil {
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
	// mcp_connections stays EMPTY unless the operator named connections, and its absence is still the
	// fail-closed default for EXTERNAL tools: a Slack run reaches no MCP server until somebody registers one
	// and names it here. The rider is the capability ceiling (extensions/lookup.go), so an empty one is a
	// run that cannot reach outward at all. E22 T6 gives that decision a spelling — SLACK_AGENT_MCP — rather
	// than changing the default; a bring-up that guessed a connection would hand every workspace member a
	// path out to a third party's server, and a Jira ticket's body is written by whoever can file a ticket.
	var rev struct {
		ID string `json:"id"`
	}
	status, err := c.do(http.MethodPost, "/v1/agents/"+profileID+"/revisions",
		map[string]any{"tools": tools, "mcp_connections": mcp}, &rev)
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
// It reuses a published revision ONLY when that revision already carries the wanted tool list AND the
// wanted MCP rider. Reusing one with a different config would make SLACK_AGENT_TOOLS silently inert on
// every bring-up after the first — the operator would set it, see a green `palai up`, and get the old
// toolless revision anyway. That is the same silent-skip shape E21 T2 removed from this file, so it is not
// reintroduced one function down.
//
// THE RIDER IS CHECKED FOR THE SAME REASON AND WAS ADDED SECOND (E22 T6), which is worth saying plainly:
// the tool half of this comparison already existed, and adding SLACK_AGENT_MCP without extending it would
// have shipped the identical defect a third time — a Jira connection an operator named, on a stack that had
// been brought up once before, resolving to ErrUnknownTool forever with a green install behind it.
func (c *apiClient) publishedAgentRevision(profileID string, wantTools, wantMCP []string) (string, error) {
	var page struct {
		Data []struct {
			ID             string   `json:"id"`
			Status         string   `json:"status"`
			Tools          []string `json:"tools"`
			MCPConnections []string `json:"mcp_connections"`
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
		if r.Status == "published" && sameTools(r.Tools, wantTools) && sameTools(r.MCPConnections, wantMCP) {
			return r.ID, nil
		}
	}
	return "", nil
}

// sameTools compares two name lists as SETS: the API's order is not a promise, and an operator who reorders
// SLACK_AGENT_TOOLS or SLACK_AGENT_MCP has not asked for a new revision.
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
//
// hasRepository is E22 T3's ONE conditional, and the condition is what makes it honest: the workspace tools
// — and, since T4, the publish tools — are added only when this bring-up actually bound a repository. A tool
// whose every call ends in "no workspace bound for this run" is worse than an absent tool — it is an
// advertised capability that cannot work, which is the shape E21 T5 already paid for from the other
// direction (a real tool no list named).
func slackAgentTools(get func(string) string, hasRepository bool) []string {
	raw := strings.TrimSpace(get("SLACK_AGENT_TOOLS"))
	if raw == "" {
		if hasRepository {
			out := append(append([]string{}, slackDefaultTools...), slackRepositoryTools...)
			return append(out, slackPublishTools...)
		}
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
// palai.knowledge.retrieve is ACL-first and fail-closed to KB-wide sources; palai.slack.search reads this
// workspace's PUBLIC channels and stores nothing (its own two gates are elsewhere and both must hold — the
// workspace must have granted search:read.public, and the RUN must carry an action_token, or the broker
// lookup never offers the tool at all).
//
// A NAME MISSING FROM THIS LIST IS A TOOL THAT CANNOT EXIST, and that is not obvious from either side:
// advertisedTools iterates the run's EFFECTIVE SET and only asks the broker about names it already holds,
// so a tool absent here is never resolved no matter how completely it is wired. palai.slack.search was
// missing for exactly that reason and the whole search feature was unreachable — mounted, tested, and dead.
// TestEverySlackDefaultToolResolves is the guard, and it lives control-plane-side because that is the only
// place that can see both this list and the real broker.
var slackDefaultTools = []string{"palai.research.fetch", "palai.knowledge.retrieve", "palai.slack.search"}

// slackRepositoryTools are added to the list above ONLY when this bring-up bound a repository to the Slack
// connection (E22 T3). They are the coding half — read a file, run a command, commit — and every one of them
// refuses cleanly without a workspace root, which is precisely why they are conditional rather than default:
// binding them to a connection with no repository would advertise three tools whose every call answers "no
// workspace bound for this run".
//
// The publish half is the SEPARATE list below, and the two stay separate after E22 T4 opened it: this one
// is what an agent may do to a workspace nobody else can see, and that one is what leaves the machine.
var slackRepositoryTools = []string{"palai.workspace.file", "palai.workspace.shell", "palai.workspace.commit"}

// slackPublishTools are the publish half (E22 T4), added under the SAME condition as the coding half and
// for the same reason: without a binding RunPublicationTarget answers "the run prepared no repository", so
// offering them would advertise two tools whose every call fails.
//
// GRANTING THEM IS NOT GRANTING A PUSH, and that is the whole shape of this task. Neither tool acts: each
// records a PENDING publication and returns pending_approval (push.go:19, pull_request.go:20), and the
// operation happens only after a human presses Approve on a message only SLACK_APPROVER_IDS may press. The
// destination is never in the list either — remote, branch and base come from the binding
// (RunPublicationTarget), so "open the PR against dev" is the operator's PALAI_GIT_BASE_BRANCH and not
// something a model can name. What this list decides is whether the agent may ASK.
//
// A stack that granted these and configured no GitHub App gets an approved publication that waits forever;
// resolveGitHubApp warns about exactly that, because silence is the failure mode this file has now paid for
// three times.
// E23 T6 adds the third and it belongs on THIS list rather than a fourth: merging is what leaves the
// machine, exactly like the other two, and it asks the same human the same way. What it does NOT do is
// widen who decides or what may be named — the pull request it merges is the one this run opened.
var slackPublishTools = []string{"palai.publish.push", "palai.publish.pull_request", "palai.publish.merge_pull_request"}

// slackAgentMCP is the mcp_connections rider bound to the revision `palai up` creates (E22 T6): the names
// of the MCP connections a Slack run may reach. It is the EXTERNAL capability ceiling — a run resolves a
// connection's tools only when its revision names it (extensions/lookup.go), so an empty rider is a run
// that cannot reach outward at all.
//
// EMPTY IS THE DEFAULT AND STAYS THE DEFAULT. What changes here is only that the operator now has a
// spelling: SLACK_AGENT_MCP=jira, alongside the tool set that carries the Jira tools. Registering the
// connection and approving its tools is still a separate, deliberate ceremony
// (docs/operations/jira-mcp-connection.md) — this variable decides only whether the Slack agent is one of
// the things allowed to use it.
//
// It takes CONNECTION NAMES rather than ids for the same reason SLACK_AGENT_TOOLS takes tool names: an
// operator writing .env.local has the name they chose at registration, not an `mcpc_…` id they would have
// to go and look up.
//
// `none` and blank both leave the rider empty, which makes them equivalent TODAY — the default is already
// empty. The word is kept anyway: it means the same thing it means on SLACK_AGENT_TOOLS, and "deliberately
// nothing" should be sayable without deleting a line.
func slackAgentMCP(get func(string) string) []string {
	raw := strings.TrimSpace(get("SLACK_AGENT_MCP"))
	if raw == "" || strings.EqualFold(raw, "none") {
		return nil
	}
	return splitList(raw)
}

// slackMCPGrant is the operator-facing sentence for the rider. The empty case is a sentence rather than
// silence: a Slack agent that cannot reach Jira looks exactly like one whose Jira connection is broken, and
// the difference is a variable nobody would think to look for.
func slackMCPGrant(mcp []string) string {
	if len(mcp) == 0 {
		return "NO MCP connection — it cannot reach Jira or any other external server (set SLACK_AGENT_MCP to name one)"
	}
	return "MCP connections: " + strings.Join(mcp, ", ") +
		" (their tools must also be approved and pinned into the tool set — see docs/operations/jira-mcp-connection.md)"
}

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

// applyConcurrencyEnv exports BOTH concurrency knobs, because a stack is only as parallel as the smaller of
// them and compose reads both from the environment. Extracted from Bootstrap so the pairing is testable —
// the defect it fixes was invisible precisely because nothing could assert it.
func applyConcurrencyEnv(get func(string) string) error {
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
	// THE RUNNER'S CONCURRENCY MUST BE EXPORTED TOO, and forgetting it is why a stack could be told to run
	// four runs at once and still run them one at a time. PALAI_DISPATCH_WORKERS is how many runs the control
	// plane will dispatch; PALAI_RUNNER_CONCURRENCY is how many engine leases ONE runner identity parks at
	// once (runner_concurrency_test.go: at the default of 1 "a second concurrent Dial blocks"). Compose reads
	// the second from the environment and `palai up` never put it there, so it was always 1 whatever
	// .env.local said — three of four dispatch workers queueing behind one engine, with nothing said about it.
	//
	// UNSET FOLLOWS THE DISPATCH COUNT, because the two numbers answer the same question — "how many runs at
	// once" — and letting them disagree silently is the defect above. An explicit value always wins.
	runners := strings.TrimSpace(get("PALAI_RUNNER_CONCURRENCY"))
	if runners == "" {
		runners = os.Getenv("PALAI_DISPATCH_WORKERS")
	}
	if runners != "" {
		if err := os.Setenv("PALAI_RUNNER_CONCURRENCY", runners); err != nil {
			return err
		}
	}
	return nil
}
